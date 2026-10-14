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

// Package selection finds what a Forwarder is about among the pods and Services
// of its namespace: the pods of its gateway, which are let forward and routed
// to, and the workloads of the tenant, which the gateway lets traffic through to
// and which need the routes.
package selection

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/routes"
)

// InstanceGVK is the kind of the VirtualMachineInstance of a gateway.
var InstanceGVK = schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstance"}

// Instance is what the VirtualMachineInstance of a gateway says about its pods.
type Instance struct {
	Name string
	UID  types.UID
	// ActivePods are the pods KubeVirt says the instance runs in, by UID. It is
	// empty until KubeVirt has written them.
	ActivePods map[types.UID]bool
}

// InstanceOf reads an Instance from a VirtualMachineInstance.
func InstanceOf(u *unstructured.Unstructured) *Instance {
	in := &Instance{Name: u.GetName(), UID: u.GetUID(), ActivePods: map[types.UID]bool{}}
	active, _, _ := unstructured.NestedMap(u.Object, "status", "activePods")
	for uid := range active {
		in.ActivePods[types.UID(uid)] = true
	}
	return in
}

// Verdict is what a pod is to a Forwarder.
type Verdict int

const (
	// NotGateway: the pod is not the gateway of the Forwarder, and there is
	// something that shows it.
	NotGateway Verdict = iota
	// Unproven: the pod may be the gateway, but KubeVirt has not said that its
	// instance runs in it. It is given nothing, and nothing it has is taken.
	Unproven
	// Gateway: the pod is the gateway of the Forwarder.
	Gateway
)

func (v Verdict) String() string {
	switch v {
	case Gateway:
		return "Gateway"
	case Unproven:
		return "Unproven"
	}
	return "NotGateway"
}

// Judge says whether pod is the gateway of the Forwarder f, whose workload
// selector is workload and whose VirtualMachineInstance is vmi, nil when there
// is none.
//
// A label is written by whoever makes the pod, so a pod the selector selects is
// the gateway only when the instance named like the Forwarder controls it, by
// UID. An owner reference is written by whoever makes the pod too; the active
// pods in the status of the instance are written by KubeVirt alone, so the pod
// has to be one of them as well. A pod that is being deleted or has finished is
// not the gateway, nor is a pod on the network of its node, whose address is the
// node's.
func Judge(pod *corev1.Pod, f *v1alpha1.Forwarder, workload labels.Selector, vmi *Instance) Verdict {
	switch {
	case pod.Namespace != f.Namespace, !Live(pod), pod.Spec.HostNetwork:
		return NotGateway
	case !workload.Matches(labels.Set(pod.Labels)):
		return NotGateway
	}
	if !ControlledByInstanceNamed(pod, f.Name) {
		return NotGateway
	}
	if vmi == nil || vmi.Name != f.Name || metav1.GetControllerOf(pod).UID != vmi.UID {
		return NotGateway
	}
	if len(vmi.ActivePods) == 0 {
		return Unproven
	}
	if !vmi.ActivePods[pod.UID] {
		return NotGateway
	}
	return Gateway
}

// ControlledByInstanceNamed says whether the controller of the pod, as the pod
// says, is a VirtualMachineInstance called name. It is what the pod claims, and
// nothing is given on it alone.
func ControlledByInstanceNamed(pod *corev1.Pod, name string) bool {
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.Kind != InstanceGVK.Kind || owner.Name != name {
		return false
	}
	gv, err := schema.ParseGroupVersion(owner.APIVersion)
	return err == nil && gv.Group == InstanceGVK.Group
}

// Live is false for a pod that is being deleted, whose address goes back to the
// pool, and for one that has finished.
func Live(pod *corev1.Pod) bool {
	return pod.DeletionTimestamp == nil && pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
}

// Address is the IPv4 address of a pod on the cluster network. On kube-ovn the
// address a KubeVirt guest has on a bridge to the pod network is that of its
// pod, so for a gateway it is the address the guest sees and the next hop of
// the routes.
func Address(pod *corev1.Pod) (netip.Addr, bool) {
	ips := make([]string, 0, len(pod.Status.PodIPs)+1)
	for _, ip := range pod.Status.PodIPs {
		ips = append(ips, ip.IP)
	}
	ips = append(ips, pod.Status.PodIP)
	for _, ip := range ips {
		if a, err := netip.ParseAddr(ip); err == nil && a.Is4() {
			return a, true
		}
	}
	return netip.Addr{}, false
}

// GatewayPod is a pod of the gateway that has an address.
type GatewayPod struct {
	Pod     *corev1.Pod
	Address netip.Addr
}

// View is what a Forwarder finds in its namespace.
type View struct {
	// Gateways are the pods of its gateway that have an address, the one made
	// first first: while KubeVirt moves the instance to another node there are
	// two, and the one it runs in until the move is done is the older.
	Gateways []GatewayPod
	// Scope are the addresses the gateway lets traffic through to.
	Scope []string
	// ScopePods are the pods of the tenant that need the routes.
	ScopePods []*corev1.Pod
}

// Addresses are the addresses of the gateway, sorted and without repeats.
func (v View) Addresses() []string {
	out := make([]netip.Addr, 0, len(v.Gateways))
	for _, g := range v.Gateways {
		out = append(out, g.Address)
	}
	return sortedStrings(out)
}

func sortedStrings(addrs []netip.Addr) []string {
	slices.SortFunc(addrs, netip.Addr.Compare)
	addrs = slices.Compact(addrs)
	out := make([]string, len(addrs))
	for i, a := range addrs {
		out[i] = a.String()
	}
	return out
}

// Hops are the addresses of the gateway, the next hop first.
func (v View) Hops() []netip.Addr {
	out := make([]netip.Addr, 0, len(v.Gateways))
	for _, g := range v.Gateways {
		if !slices.Contains(out, g.Address) {
			out = append(out, g.Address)
		}
	}
	return out
}

// Resolve finds the gateway and the scope of f. siblings are the other
// Forwarders of the namespace, which say what else is a gateway.
//
// The scope is what scopeSelector selects, without the gateways of every
// Forwarder of the namespace, as their selectors say, and without the pods that
// are not the tenant's to reach: those being deleted, whose address goes back to
// the pool and may be given to a pod of another namespace, those that have
// finished, and those on the network of their node. The cluster IP of each
// Service of the namespace is the tenant's as much as a pod is.
func Resolve(f *v1alpha1.Forwarder, siblings []v1alpha1.Forwarder, pods []corev1.Pod, services []corev1.Service, vmi *Instance) (View, error) {
	workload, err := metav1.LabelSelectorAsSelector(&f.Spec.WorkloadSelector)
	if err != nil {
		return View{}, fmt.Errorf("the workload selector: %w", err)
	}
	tenant, err := metav1.LabelSelectorAsSelector(&f.Spec.ScopeSelector)
	if err != nil {
		return View{}, fmt.Errorf("the scope selector: %w", err)
	}
	gateways := []labels.Selector{workload}
	for i := range siblings {
		s := &siblings[i]
		if s.Namespace != f.Namespace || s.UID == f.UID {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(&s.Spec.WorkloadSelector)
		if err != nil {
			// A selector that does not parse selects nothing for its own Forwarder
			// either, so it makes nothing a gateway.
			continue
		}
		gateways = append(gateways, sel)
	}

	var v View
	var scope []netip.Addr
	for i := range pods {
		p := &pods[i]
		if p.Namespace != f.Namespace {
			continue
		}
		if Judge(p, f, workload, vmi) == Gateway {
			if a, ok := Address(p); ok {
				v.Gateways = append(v.Gateways, GatewayPod{Pod: p, Address: a})
			}
			continue
		}
		set := labels.Set(p.Labels)
		if !Live(p) || p.Spec.HostNetwork || !tenant.Matches(set) ||
			slices.ContainsFunc(gateways, func(s labels.Selector) bool { return s.Matches(set) }) {
			continue
		}
		v.ScopePods = append(v.ScopePods, p)
		if a, ok := Address(p); ok {
			scope = append(scope, a)
		}
	}
	for i := range services {
		svc := &services[i]
		if svc.Namespace != f.Namespace {
			continue
		}
		// The Service in front of a gateway takes the tunnel. A workload has no
		// need of it through the gateway, and the filter would open to one more
		// address.
		if _, ok := svc.Labels[v1alpha1.LabelInstance]; ok {
			continue
		}
		for _, ip := range svc.Spec.ClusterIPs {
			if a, err := netip.ParseAddr(ip); err == nil && a.Is4() {
				scope = append(scope, a)
			}
		}
	}
	v.Scope = sortedStrings(scope)

	slices.SortFunc(v.Gateways, func(a, b GatewayPod) int {
		if c := a.Pod.CreationTimestamp.Compare(b.Pod.CreationTimestamp.Time); c != 0 {
			return c
		}
		return cmp.Compare(a.Pod.Name, b.Pod.Name)
	})
	slices.SortFunc(v.ScopePods, func(a, b *corev1.Pod) int { return cmp.Compare(a.Name, b.Name) })
	return v, nil
}

// Pending are the names of the pods of the scope that were made before the
// routes in mine were in place, and do not have them: kube-ovn gives a pod its
// routes only when it is made. A pod whose routes cannot be read is not counted,
// since what it has is not known.
func Pending(scope []*corev1.Pod, mine []routes.Entry) []string {
	if len(mine) == 0 {
		return nil
	}
	var out []string
	for _, p := range scope {
		ok, err := routes.Covers(p.Annotations[routes.Annotation], mine)
		if err == nil && !ok {
			out = append(out, p.Name)
		}
	}
	slices.Sort(out)
	return out
}
