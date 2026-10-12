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

package forwarder

import (
	"context"
	"fmt"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/internal/gateway"
	"github.com/aenix-io/site-router-operator/internal/reconcile"
)

// RequestedAtAnnotation is when the operator last changed the networks it asks
// the connector for. The connector owes an answer from then on, and the time is
// what a Forwarder that nobody answers is measured against. The API server does
// not keep a time for a change of the spec, so the operator does.
const RequestedAtAnnotation = "siterouter.aenix.io/requested-at"

// DefaultNotRespondingAfter is how long a request can go unanswered before the
// condition says that no connector serves it.
const DefaultNotRespondingAfter = time.Minute

// Manager keeps the Forwarders of the SiteRouters of a cluster.
type Manager struct {
	Client client.Client
	// NotRespondingAfter is DefaultNotRespondingAfter when zero.
	NotRespondingAfter time.Duration
	// Now is the clock, and is time.Now when nil.
	Now func() time.Time
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) notResponding() time.Duration {
	if m.NotRespondingAfter > 0 {
		return m.NotRespondingAfter
	}
	return DefaultNotRespondingAfter
}

// For returns the Routes of one SiteRouter: its Forwarder, as the reconcile step
// uses it.
func (m *Manager) For(sr *v1alpha1.SiteRouter) *Routes { return &Routes{m: m, sr: sr} }

// Routes is the Forwarder of one SiteRouter. It implements reconcile.Routes.
type Routes struct {
	m  *Manager
	sr *v1alpha1.SiteRouter
}

var _ reconcile.Routes = (*Routes)(nil)

// Report is what the connector says about the addresses.
type Report struct {
	// Resolved is true when the connector has seen the current request and has
	// found the gateway.
	Resolved bool
	// Gateway holds the addresses of the pods of the gateway, in the addressing
	// of the cluster network: the next hop of the routes.
	Gateway []string
	// Scope holds the addresses of the workloads of the tenant that the gateway
	// lets decrypted traffic through to.
	Scope []string
}

// Ensure makes the Forwarder exist, so that the connector can find the gateway
// and report the addresses of the tenant before anything is routed. A new one is
// asked for no networks, and an existing one keeps the networks it has.
func (r *Routes) Ensure(ctx context.Context) error {
	existing, err := r.get(ctx)
	if err != nil {
		return err
	}
	var cidrs []string
	requested := r.m.now()
	if existing != nil {
		cidrs, requested = cidrsOf(existing), r.requestedAt(existing)
	}
	return r.apply(ctx, cidrs, requested)
}

// Report returns what the connector says about the addresses, or the zero Report
// when there is no Forwarder.
func (r *Routes) Report(ctx context.Context) (Report, error) {
	f, err := r.get(ctx)
	if err != nil || f == nil {
		return Report{}, err
	}
	return Report{
		Resolved: seen(f) && meta.IsStatusConditionTrue(f.Status.Conditions, v1alpha1.ForwarderResolved),
		Gateway:  slices.Clone(f.Status.Forwarder.Addresses),
		Scope:    slices.Clone(f.Status.Scope.Addresses),
	}, nil
}

// Grant asks for cidrs to be routed to the gateway, and says how that stands. It
// implements reconcile.Routes.
func (r *Routes) Grant(ctx context.Context, cidrs []string) (reconcile.RouteState, error) {
	existing, err := r.get(ctx)
	if err != nil {
		return reconcile.RouteState{}, err
	}
	now := r.m.now()
	requested := now
	if existing != nil && sameSet(cidrsOf(existing), cidrs) {
		requested = r.requestedAt(existing)
	}
	if err := r.apply(ctx, cidrs, requested); err != nil {
		return reconcile.RouteState{}, err
	}
	f, err := r.get(ctx)
	if err != nil {
		return reconcile.RouteState{}, err
	}
	if f == nil {
		return reconcile.RouteState{}, fmt.Errorf("the Forwarder %s is gone", gateway.NamesFor(r.sr.Name).Forwarder)
	}
	return Assess(f, cidrs, requested, now, r.m.notResponding()), nil
}

// Withdraw asks for no networks at all, which takes the grant and the routes
// away. It implements reconcile.Routes.
func (r *Routes) Withdraw(ctx context.Context) error {
	existing, err := r.get(ctx)
	if err != nil || existing == nil || len(existing.Spec.CIDRs) == 0 {
		return err
	}
	return r.apply(ctx, nil, r.m.now())
}

// Withdrawn says whether the networks have been taken away and the connector has
// acted on it, so that nothing is routed to a gateway that is about to go. It is
// true when there is no Forwarder, when the connector has seen the empty request
// and answers that nothing is granted or programmed, and when no connector has
// answered for as long as one is waited for, since a cluster that has none must
// not hold a SiteRouter in deletion for ever. It is false while the Forwarder
// still asks for networks, so it does not withdraw them itself.
func (r *Routes) Withdrawn(ctx context.Context) (bool, error) {
	f, err := r.get(ctx)
	if err != nil || f == nil {
		return true, err
	}
	if len(f.Spec.CIDRs) > 0 {
		return false, nil
	}
	if seen(f) {
		return !meta.IsStatusConditionTrue(f.Status.Conditions, v1alpha1.ForwarderGranted) &&
			!meta.IsStatusConditionTrue(f.Status.Conditions, v1alpha1.ForwarderRoutesProgrammed), nil
	}
	return r.m.now().Sub(r.requestedAt(f)) > r.m.notResponding(), nil
}

// get returns the Forwarder of the SiteRouter, or nil when there is none. One
// that the SiteRouter does not control is not its own and is refused.
func (r *Routes) get(ctx context.Context) (*v1alpha1.Forwarder, error) {
	name := gateway.NamesFor(r.sr.Name).Forwarder
	f := &v1alpha1.Forwarder{}
	if err := r.m.Client.Get(ctx, client.ObjectKey{Namespace: r.sr.Namespace, Name: name}, f); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if !metav1.IsControlledBy(f, r.sr) {
		return nil, &gateway.NotOwnedError{Kind: "Forwarder", Name: name}
	}
	return f, nil
}

// apply makes the Forwarder what the operator asks it to be, with cidrs. The
// selectors are the operator's and are put back if someone changes them.
//
// The gateway is found by the label of the operator. The workloads of the tenant
// that need a route are every other pod of the namespace: those that carry the
// label are gateways, this one's or another's, and they are not routed to.
func (r *Routes) apply(ctx context.Context, cidrs []string, requested time.Time) error {
	names := gateway.NamesFor(r.sr.Name)
	f := &v1alpha1.Forwarder{
		APIVersion:      v1alpha1.GroupVersion.String(),
		Kind:            "Forwarder",
		Name:            names.Forwarder,
		Namespace:       r.sr.Namespace,
		Labels:          gateway.Labels(r.sr.Name),
		Annotations:     map[string]string{RequestedAtAnnotation: requested.UTC().Format(time.RFC3339)},
		OwnerReferences: []metav1.OwnerReference{gateway.OwnerReference(r.sr)},
		Spec: v1alpha1.ForwarderSpec{
			WorkloadSelector: metav1.LabelSelector{MatchLabels: map[string]string{v1alpha1.LabelInstance: r.sr.Name}},
			ScopeSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: v1alpha1.LabelInstance, Operator: metav1.LabelSelectorOpDoesNotExist},
			}},
		},
	}
	for _, c := range cidrs {
		f.Spec.CIDRs = append(f.Spec.CIDRs, v1alpha1.IPv4CIDR(c))
	}
	ac, err := gateway.ApplyConfiguration(f)
	if err != nil {
		return err
	}
	return r.m.Client.Apply(ctx, ac, client.FieldOwner(gateway.FieldOwner), client.ForceOwnership)
}

// requestedAt is when the networks of the Forwarder were asked for. A Forwarder
// that carries no time, because it was not made by the operator's own code, is
// taken to have been asked for when it was made.
func (r *Routes) requestedAt(f *v1alpha1.Forwarder) time.Time {
	if t, err := time.Parse(time.RFC3339, f.Annotations[RequestedAtAnnotation]); err == nil {
		return t
	}
	return f.CreationTimestamp.Time
}

// seen is true when the connector has written the status for the current
// generation of the spec. What it wrote is then in the conditions: one that is
// not there was not reported, and does not count as met.
func seen(f *v1alpha1.Forwarder) bool {
	return f.Status.ObservedGeneration >= f.Generation
}

func cidrsOf(f *v1alpha1.Forwarder) []string {
	out := make([]string, len(f.Spec.CIDRs))
	for i, c := range f.Spec.CIDRs {
		out[i] = string(c)
	}
	return out
}

func sameSet(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}
