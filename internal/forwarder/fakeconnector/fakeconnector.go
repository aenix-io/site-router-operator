/*
Copyright 2026 The Aenix Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package fakeconnector stands in for the connector of a cluster network, so
// that the operator's side of the Forwarder API can be tested without a network.
// It does what the connector for kube-ovn is meant to do and no more: it finds
// the pods by the selectors, reports their addresses, gives the gateway the right
// to forward and the routes to the networks, refuses what two Forwarders both
// claim, and counts the pods that were left behind. It exists to exercise the
// operator, not to be used for real.
package fakeconnector

import (
	"cmp"
	"context"
	"fmt"
	"net/netip"
	"slices"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
)

// Connector serves every Forwarder it can see. A pass is one call to Sync, so
// that a test decides when the connector acts.
type Connector struct {
	Client client.Client
	// RoutesAtPodCreation makes the network one that programs a route only when a
	// pod is created, as kube-ovn does with an annotation of the namespace: the
	// pods of the tenant that already exist when the routes come stay without them
	// and are reported as pending.
	RoutesAtPodCreation bool

	mu      sync.Mutex
	granted map[types.UID]Grant
	// left are the pods of the tenant that existed when the routes of a Forwarder
	// were first programmed.
	left map[types.UID][]string
}

// Grant is what the network does for one Forwarder: the pods that may forward,
// and the networks that are routed to them.
type Grant struct {
	Gateways []string
	Routes   []string
}

// Granted returns what the network does for the Forwarder now.
func (c *Connector) Granted(uid types.UID) Grant {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.granted[uid]
}

// Sync serves every Forwarder in the cluster once.
func (c *Connector) Sync(ctx context.Context) error {
	list := &v1alpha1.ForwarderList{}
	if err := c.Client.List(ctx, list); err != nil {
		return fmt.Errorf("listing the Forwarders: %w", err)
	}
	forwarders := list.Items
	// An older Forwarder keeps a network that a newer one also claims.
	slices.SortFunc(forwarders, func(a, b v1alpha1.Forwarder) int {
		if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	for i := range forwarders {
		if err := c.serve(ctx, &forwarders[i], forwarders[:i]); err != nil {
			return err
		}
	}
	return nil
}

func (c *Connector) serve(ctx context.Context, f *v1alpha1.Forwarder, older []v1alpha1.Forwarder) error {
	gateways, scope, scopePods, err := c.resolve(ctx, f)
	if err != nil {
		return err
	}

	want := make([]string, len(f.Spec.CIDRs))
	for i, cidr := range f.Spec.CIDRs {
		want[i] = string(cidr)
	}
	conflict := claimedByOlder(f, want, older)

	resolved := len(gateways) > 0
	granted := resolved && len(want) > 0 && !conflict

	c.mu.Lock()
	if c.granted == nil {
		c.granted, c.left = map[types.UID]Grant{}, map[types.UID][]string{}
	}
	if granted {
		c.granted[f.UID] = Grant{Gateways: slices.Clone(gateways), Routes: slices.Clone(want)}
		if _, ok := c.left[f.UID]; !ok {
			names := make([]string, 0, len(scopePods))
			for _, p := range scopePods {
				names = append(names, p.Name)
			}
			c.left[f.UID] = names
		}
	} else {
		delete(c.granted, f.UID)
		delete(c.left, f.UID)
	}
	left := slices.Clone(c.left[f.UID])
	c.mu.Unlock()

	var pending []string
	if c.RoutesAtPodCreation {
		for _, p := range scopePods {
			if slices.Contains(left, p.Name) {
				pending = append(pending, p.Name)
			}
		}
		slices.Sort(pending)
	}

	status := v1alpha1.ForwarderStatus{
		ObservedGeneration: f.Generation,
		Forwarder:          v1alpha1.AddressSet{Addresses: gateways},
		Scope:              v1alpha1.AddressSet{Addresses: scope},
		PendingWorkloads:   pending,
		Conditions:         slices.Clone(f.Status.Conditions),
	}
	set := func(typ string, ok bool, reason, msg string) {
		st := metav1.ConditionFalse
		if ok {
			st = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: typ, Status: st, Reason: reason, Message: msg, ObservedGeneration: f.Generation,
		})
	}
	set(v1alpha1.ForwarderAccepted, true, "Served", "")
	set(v1alpha1.ForwarderResolved, resolved, reasonIf(resolved, "Resolved", "NoGateway"),
		fmt.Sprintf("gateway pods behind %v: %v", f.Spec.WorkloadSelector.MatchLabels, gateways))
	set(v1alpha1.ForwarderGranted, granted, grantReason(resolved, len(want), conflict), "")
	set(v1alpha1.ForwarderRoutesProgrammed, granted, grantReason(resolved, len(want), conflict), "")
	set(v1alpha1.ForwarderConflict, conflict, reasonIf(conflict, "Overlap", "NoOverlap"),
		fmt.Sprintf("a network of %s/%s is claimed by an older Forwarder in the namespace", f.Namespace, f.Name))

	if equality.Semantic.DeepEqual(f.Status, status) {
		return nil
	}
	f.Status = status
	if err := c.Client.Status().Update(ctx, f); err != nil {
		return fmt.Errorf("writing the status of %s/%s: %w", f.Namespace, f.Name, err)
	}
	return nil
}

func reasonIf(ok bool, yes, no string) string {
	if ok {
		return yes
	}
	return no
}

func grantReason(resolved bool, cidrs int, conflict bool) string {
	switch {
	case conflict:
		return "Conflict"
	case !resolved:
		return "NoGateway"
	case cidrs == 0:
		return "NoNetworks"
	}
	return "Granted"
}

// resolve finds the addresses of the gateway and of the tenant. A pod that
// matches the workload selector is a gateway, and is not in the scope.
func (c *Connector) resolve(ctx context.Context, f *v1alpha1.Forwarder) (gateways, scope []string, scopePods []corev1.Pod, err error) {
	workload, err := metav1.LabelSelectorAsSelector(&f.Spec.WorkloadSelector)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("the workload selector of %s/%s: %w", f.Namespace, f.Name, err)
	}
	tenant, err := metav1.LabelSelectorAsSelector(&f.Spec.ScopeSelector)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("the scope selector of %s/%s: %w", f.Namespace, f.Name, err)
	}
	pods := &corev1.PodList{}
	if err := c.Client.List(ctx, pods, client.InNamespace(f.Namespace)); err != nil {
		return nil, nil, nil, fmt.Errorf("listing the pods of %s: %w", f.Namespace, err)
	}
	for _, p := range pods.Items {
		ip, ok := address(p)
		if !ok || p.DeletionTimestamp != nil {
			continue
		}
		set := labels.Set(p.Labels)
		switch {
		case workload.Matches(set):
			gateways = append(gateways, ip)
		case tenant.Matches(set) && reachable(p):
			scope = append(scope, ip)
			scopePods = append(scopePods, p)
		}
	}

	// The cluster IP of a Service of the tenant is as much theirs as a pod is.
	services := &corev1.ServiceList{}
	if err := c.Client.List(ctx, services, client.InNamespace(f.Namespace)); err != nil {
		return nil, nil, nil, fmt.Errorf("listing the services of %s: %w", f.Namespace, err)
	}
	for _, svc := range services.Items {
		for _, raw := range svc.Spec.ClusterIPs {
			if a, err := netip.ParseAddr(raw); err == nil && a.Is4() {
				scope = append(scope, a.String())
			}
		}
	}

	slices.Sort(gateways)
	slices.Sort(scope)
	return gateways, slices.Compact(scope), scopePods, nil
}

// reachable is false for a pod whose address is not the tenant's to reach: one
// that has finished, whose address is on its way back to the pool, and one on the
// network of its node, whose address is the node's. (A pod that is being deleted
// is left out by the caller.)
func reachable(p corev1.Pod) bool {
	return !p.Spec.HostNetwork && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed
}

func address(p corev1.Pod) (string, bool) {
	a, err := netip.ParseAddr(p.Status.PodIP)
	if err != nil {
		return "", false
	}
	return a.String(), true
}

// claimedByOlder is true when one of the networks overlaps one that an older
// Forwarder of the namespace asks for.
func claimedByOlder(f *v1alpha1.Forwarder, want []string, older []v1alpha1.Forwarder) bool {
	for _, o := range older {
		if o.Namespace != f.Namespace {
			continue
		}
		for _, theirs := range o.Spec.CIDRs {
			p, err := netip.ParsePrefix(string(theirs))
			if err != nil {
				continue
			}
			for _, mine := range want {
				if q, err := netip.ParsePrefix(mine); err == nil && p.Overlaps(q) {
					return true
				}
			}
		}
	}
	return false
}
