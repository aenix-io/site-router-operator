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

// Package forwarder_test runs the operator's side of the Forwarder API against a
// real apiserver: what the manager does with an object that exists, and what the
// apiserver does with what the manager writes, is only worth what the apiserver
// makes of it.
package forwarder_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/internal/forwarder"
	"github.com/aenix-io/site-router-operator/internal/gateway"
	"github.com/aenix-io/site-router-operator/internal/reconcile"
	"github.com/aenix-io/site-router-operator/internal/testenv"
)

var k8sClient client.Client

func TestMain(m *testing.M) {
	testenv.Main(m, func(c client.Client) { k8sClient = c })
}

var (
	cidrs   = []string{"192.0.2.0/24", "198.51.100.0/24"}
	epoch   = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	timeout = 90 * time.Second
)

// world is a namespace with a SiteRouter in it, and a manager with a clock that
// the test moves.
type world struct {
	t   *testing.T
	ns  string
	sr  *v1alpha1.SiteRouter
	now time.Time
	m   *forwarder.Manager
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ns := &corev1.Namespace{GenerateName: "fw-"}
	mustCreate(t, ns)
	sr := &v1alpha1.SiteRouter{
		Name:      "branch-office",
		Namespace: ns.Name,
		Spec:      v1alpha1.SiteRouterSpec{Peer: v1alpha1.Peer{Address: "203.0.113.10"}},
	}
	mustCreate(t, sr)
	w := &world{t: t, ns: ns.Name, sr: sr, now: epoch}
	w.m = &forwarder.Manager{Client: k8sClient, NotRespondingAfter: timeout, Now: func() time.Time { return w.now }}
	return w
}

func mustCreate(t *testing.T, o client.Object) {
	t.Helper()
	if err := k8sClient.Create(t.Context(), o); err != nil {
		t.Fatal(err)
	}
}

func (w *world) routes() *forwarder.Routes { return w.m.For(w.sr) }

func (w *world) key() client.ObjectKey {
	return client.ObjectKey{Namespace: w.ns, Name: gateway.NamesFor(w.sr.Name).Forwarder}
}

// forwarder returns the Forwarder, or nil if there is none.
func (w *world) forwarder() *v1alpha1.Forwarder {
	w.t.Helper()
	f := &v1alpha1.Forwarder{}
	err := k8sClient.Get(w.t.Context(), w.key(), f)
	if client.IgnoreNotFound(err) != nil {
		w.t.Fatal(err)
	}
	if err != nil {
		return nil
	}
	return f
}

// answer writes the status a connector would, for the generation of the spec.
func (w *world) answer(mutate func(*v1alpha1.ForwarderStatus)) {
	w.t.Helper()
	f := w.forwarder()
	if f == nil {
		w.t.Fatal("no Forwarder to answer")
	}
	f.Status.ObservedGeneration = f.Generation
	for _, typ := range []string{v1alpha1.ForwarderAccepted, v1alpha1.ForwarderResolved, v1alpha1.ForwarderGranted, v1alpha1.ForwarderRoutesProgrammed} {
		meta.SetStatusCondition(&f.Status.Conditions, metav1.Condition{
			Type: typ, Status: metav1.ConditionTrue, Reason: "Done", ObservedGeneration: f.Generation,
		})
	}
	f.Status.Forwarder.Addresses = []string{"10.244.1.9"}
	f.Status.Scope.Addresses = []string{"10.244.1.5", "10.244.1.6"}
	if mutate != nil {
		mutate(&f.Status)
	}
	if err := k8sClient.Status().Update(w.t.Context(), f); err != nil {
		w.t.Fatal(err)
	}
}

func cidrsOf(f *v1alpha1.Forwarder) []string {
	out := make([]string, 0, len(f.Spec.CIDRs))
	for _, c := range f.Spec.CIDRs {
		out = append(out, string(c))
	}
	return out
}

func TestEnsureMakesAForwarderThatAsksForNothing(t *testing.T) {
	w := newWorld(t)
	if err := w.routes().Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	f := w.forwarder()
	if f == nil {
		t.Fatal("no Forwarder was made")
	}
	if len(f.Spec.CIDRs) != 0 {
		t.Errorf("cidrs = %v, want none: nothing is routed before the filter is confirmed", f.Spec.CIDRs)
	}
	if got := f.Spec.WorkloadSelector.MatchLabels; len(got) != 1 || got[v1alpha1.LabelInstance] != w.sr.Name {
		t.Errorf("workloadSelector = %v, want the gateway of the SiteRouter alone", got)
	}
	// The workloads of the tenant are the pods that are not gateways.
	exprs := f.Spec.ScopeSelector.MatchExpressions
	if len(exprs) != 1 || exprs[0].Key != v1alpha1.LabelInstance || exprs[0].Operator != metav1.LabelSelectorOpDoesNotExist {
		t.Errorf("scopeSelector = %+v, want every pod that does not carry the label of a gateway", f.Spec.ScopeSelector)
	}
	if !metav1.IsControlledBy(f, w.sr) {
		t.Error("the Forwarder is not controlled by the SiteRouter")
	}
	if f.Labels[v1alpha1.LabelInstance] != w.sr.Name || f.Labels[v1alpha1.LabelManagedBy] != v1alpha1.ManagedByValue {
		t.Errorf("labels = %v", f.Labels)
	}
	if got := f.Annotations[forwarder.RequestedAtAnnotation]; got != epoch.Format(time.RFC3339) {
		t.Errorf("requested-at = %q, want %q", got, epoch.Format(time.RFC3339))
	}
}

func TestEnsureTwiceChangesNothing(t *testing.T) {
	w := newWorld(t)
	if err := w.routes().Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := w.forwarder().ResourceVersion
	w.now = w.now.Add(time.Hour)
	if err := w.routes().Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	if after := w.forwarder().ResourceVersion; after != before {
		t.Errorf("the Forwarder was written again: resourceVersion %s, then %s", before, after)
	}
}

func TestEnsureKeepsTheNetworksThatAreAsked(t *testing.T) {
	w := newWorld(t)
	if _, err := w.routes().Grant(t.Context(), cidrs); err != nil {
		t.Fatal(err)
	}
	if err := w.routes().Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := cidrsOf(w.forwarder()); !slices.Equal(got, cidrs) {
		t.Errorf("cidrs = %v after Ensure, want %v", got, cidrs)
	}
}

func TestEnsurePutsTheSelectorsBack(t *testing.T) {
	w := newWorld(t)
	if err := w.routes().Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Someone points the Forwarder at every pod of the namespace, which would have
	// the connector grant forwarding to all of them.
	f := w.forwarder()
	f.Spec.WorkloadSelector = metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}
	if err := k8sClient.Update(t.Context(), f, client.FieldOwner("someone-else")); err != nil {
		t.Fatal(err)
	}

	if err := w.routes().Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := w.forwarder().Spec.WorkloadSelector.MatchLabels; got[v1alpha1.LabelInstance] != w.sr.Name || got["app"] != "" {
		t.Errorf("workloadSelector = %v, want the gateway of the SiteRouter alone", got)
	}
}

func TestGrantAsksForTheNetworksAndSaysWhereItStands(t *testing.T) {
	w := newWorld(t)
	state, err := w.routes().Grant(t.Context(), cidrs)
	if err != nil {
		t.Fatal(err)
	}
	if got := cidrsOf(w.forwarder()); !slices.Equal(got, cidrs) {
		t.Fatalf("cidrs = %v, want %v", got, cidrs)
	}
	if state.Programmed || state.Reason != v1alpha1.ReasonRoutesPending {
		t.Errorf("state = %+v before any connector answers, want RoutesPending", state)
	}

	w.answer(nil)
	state, err = w.routes().Grant(t.Context(), cidrs)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Programmed || state.Reason != reconcile.ReasonProgrammed {
		t.Errorf("state = %+v after the connector answers, want Programmed", state)
	}
}

// Grant shows what the connector has made of the request, for each way it can
// go, as the state the reconcile step puts in RoutesProgrammed.
func TestGrantSaysWhatTheConnectorHasMadeOfTheRequest(t *testing.T) {
	setFalse := func(typ string) func(*v1alpha1.ForwarderStatus) {
		return func(s *v1alpha1.ForwarderStatus) {
			meta.SetStatusCondition(&s.Conditions, metav1.Condition{Type: typ, Status: metav1.ConditionFalse, Reason: "No"})
		}
	}
	cases := []struct {
		name       string
		answer     func(*v1alpha1.ForwarderStatus)
		programmed bool
		reason     string
	}{
		{"all in place", nil, true, reconcile.ReasonProgrammed},
		{"the gateway is not found", setFalse(v1alpha1.ForwarderResolved), false, v1alpha1.ReasonRoutesPending},
		{"the grant is not given", setFalse(v1alpha1.ForwarderGranted), false, v1alpha1.ReasonRoutesPending},
		{"the routes are not in", setFalse(v1alpha1.ForwarderRoutesProgrammed), false, v1alpha1.ReasonRoutesPending},
		{"the request is refused", setFalse(v1alpha1.ForwarderAccepted), false, v1alpha1.ReasonForwarderRejected},
		{"a neighbor claims a network", func(s *v1alpha1.ForwarderStatus) {
			meta.SetStatusCondition(&s.Conditions, metav1.Condition{Type: v1alpha1.ForwarderConflict, Status: metav1.ConditionTrue, Reason: "Overlap"})
		}, false, v1alpha1.ReasonRouteConflict},
		{"workloads were left behind", func(s *v1alpha1.ForwarderStatus) { s.PendingWorkloads = []string{"web-0", "web-1"} }, false, v1alpha1.ReasonPendingWorkloads},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			if _, err := w.routes().Grant(t.Context(), cidrs); err != nil {
				t.Fatal(err)
			}
			w.answer(c.answer)
			state, err := w.routes().Grant(t.Context(), cidrs)
			if err != nil {
				t.Fatal(err)
			}
			if state.Programmed != c.programmed || state.Reason != c.reason {
				t.Errorf("state = %+v, want programmed=%v reason=%s", state, c.programmed, c.reason)
			}
		})
	}
}

func TestAnAnswerToAnOlderRequestIsNotAnAnswerToThisOne(t *testing.T) {
	w := newWorld(t)
	if _, err := w.routes().Grant(t.Context(), cidrs[:1]); err != nil {
		t.Fatal(err)
	}
	w.answer(nil)

	// The networks change: the status is for the generation before.
	state, err := w.routes().Grant(t.Context(), cidrs)
	if err != nil {
		t.Fatal(err)
	}
	if state.Programmed || state.Reason != v1alpha1.ReasonRoutesPending {
		t.Errorf("state = %+v, want RoutesPending until the connector has seen the new request", state)
	}
}

func TestAnUnansweredRequestBecomesNotResponding(t *testing.T) {
	w := newWorld(t)
	if _, err := w.routes().Grant(t.Context(), cidrs); err != nil {
		t.Fatal(err)
	}

	// Asking again with the same networks does not start the wait over.
	w.now = epoch.Add(timeout / 2)
	state, err := w.routes().Grant(t.Context(), slices.Clone(cidrs))
	if err != nil || state.Reason != v1alpha1.ReasonRoutesPending {
		t.Fatalf("after half the wait: %+v, %v; want RoutesPending", state, err)
	}
	if got := w.forwarder().Annotations[forwarder.RequestedAtAnnotation]; got != epoch.Format(time.RFC3339) {
		t.Errorf("requested-at moved to %q although the request did not change", got)
	}

	w.now = epoch.Add(timeout + time.Second)
	state, err = w.routes().Grant(t.Context(), cidrs)
	if err != nil || state.Reason != v1alpha1.ReasonForwarderNotResponding {
		t.Errorf("after the wait: %+v, %v; want ForwarderNotResponding", state, err)
	}
}

func TestAChangedRequestStartsTheWaitOver(t *testing.T) {
	w := newWorld(t)
	if _, err := w.routes().Grant(t.Context(), cidrs[:1]); err != nil {
		t.Fatal(err)
	}
	w.now = epoch.Add(timeout + time.Minute)
	state, err := w.routes().Grant(t.Context(), cidrs)
	if err != nil {
		t.Fatal(err)
	}
	if state.Reason != v1alpha1.ReasonRoutesPending {
		t.Errorf("state = %+v: a request that was just changed is owed an answer from now, not from before", state)
	}
	if got := w.forwarder().Annotations[forwarder.RequestedAtAnnotation]; got != w.now.Format(time.RFC3339) {
		t.Errorf("requested-at = %q, want %q", got, w.now.Format(time.RFC3339))
	}
}

func TestTheSameNetworksInAnotherOrderAreTheSameRequest(t *testing.T) {
	w := newWorld(t)
	if _, err := w.routes().Grant(t.Context(), cidrs); err != nil {
		t.Fatal(err)
	}
	before := w.forwarder()
	w.now = epoch.Add(time.Hour)
	if _, err := w.routes().Grant(t.Context(), []string{cidrs[1], cidrs[0]}); err != nil {
		t.Fatal(err)
	}
	after := w.forwarder()
	if after.Annotations[forwarder.RequestedAtAnnotation] != before.Annotations[forwarder.RequestedAtAnnotation] {
		t.Error("a reordered list restarted the wait")
	}
}

func TestWhatTheConnectorWrotePassesThroughAGrant(t *testing.T) {
	w := newWorld(t)
	if _, err := w.routes().Grant(t.Context(), cidrs); err != nil {
		t.Fatal(err)
	}
	w.answer(nil)
	before := w.forwarder()

	if _, err := w.routes().Grant(t.Context(), cidrs); err != nil {
		t.Fatal(err)
	}
	after := w.forwarder()
	if len(after.Status.Conditions) != len(before.Status.Conditions) || !slices.Equal(after.Status.Scope.Addresses, before.Status.Scope.Addresses) {
		t.Errorf("the status of the connector was changed by the operator: %+v", after.Status)
	}
}

func TestWithdrawTakesTheNetworksAway(t *testing.T) {
	w := newWorld(t)
	if _, err := w.routes().Grant(t.Context(), cidrs); err != nil {
		t.Fatal(err)
	}
	if err := w.routes().Withdraw(t.Context()); err != nil {
		t.Fatal(err)
	}
	f := w.forwarder()
	if f == nil {
		t.Fatal("the Forwarder was removed: the connector needs it to take the grant back")
	}
	if len(f.Spec.CIDRs) != 0 {
		t.Errorf("cidrs = %v after Withdraw, want none", f.Spec.CIDRs)
	}
}

func TestWithdrawWhenThereIsNothingToWithdrawWritesNothing(t *testing.T) {
	w := newWorld(t)
	// No Forwarder at all: none is made to be emptied.
	if err := w.routes().Withdraw(t.Context()); err != nil {
		t.Fatal(err)
	}
	if w.forwarder() != nil {
		t.Error("Withdraw made a Forwarder")
	}

	// One that asks for nothing is left as it is.
	if err := w.routes().Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := w.forwarder().ResourceVersion
	w.now = w.now.Add(time.Hour)
	if err := w.routes().Withdraw(t.Context()); err != nil {
		t.Fatal(err)
	}
	if after := w.forwarder().ResourceVersion; after != before {
		t.Error("Withdraw wrote a Forwarder that asked for nothing")
	}
}

func TestReportSaysWhereTheGatewayAndTheTenantAre(t *testing.T) {
	w := newWorld(t)

	if got, err := w.routes().Report(t.Context()); err != nil || got.Resolved || len(got.Gateway) != 0 || len(got.Scope) != 0 {
		t.Errorf("Report with no Forwarder = %+v, %v; want nothing", got, err)
	}

	if err := w.routes().Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.routes().Report(t.Context()); got.Resolved {
		t.Errorf("Report = %+v before the connector answers, want it unresolved", got)
	}

	w.answer(nil)
	got, err := w.routes().Report(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Resolved || !slices.Equal(got.Gateway, []string{"10.244.1.9"}) || !slices.Equal(got.Scope, []string{"10.244.1.5", "10.244.1.6"}) {
		t.Errorf("Report = %+v", got)
	}

	// The gateway is not found: the addresses are not to be relied on.
	w.answer(func(s *v1alpha1.ForwarderStatus) {
		meta.SetStatusCondition(&s.Conditions, metav1.Condition{Type: v1alpha1.ForwarderResolved, Status: metav1.ConditionFalse, Reason: "NoGateway"})
	})
	if got, _ := w.routes().Report(t.Context()); got.Resolved {
		t.Errorf("Report = %+v with the gateway not found, want it unresolved", got)
	}
}

func TestAReportOfAnOlderGenerationIsNotResolved(t *testing.T) {
	w := newWorld(t)
	if _, err := w.routes().Grant(t.Context(), cidrs[:1]); err != nil {
		t.Fatal(err)
	}
	w.answer(nil)
	if _, err := w.routes().Grant(t.Context(), cidrs); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.routes().Report(t.Context()); got.Resolved {
		t.Errorf("Report = %+v for a spec the connector has not seen, want it unresolved", got)
	}
}

// A Forwarder the operator owns but that carries no time, because it was made by
// an older version or by hand, is taken to have been asked for when it was made.
func TestAForwarderWithNoTimeIsTakenToHaveBeenAskedForWhenItWasMade(t *testing.T) {
	w := newWorld(t)
	yes := true
	mustCreate(t, &v1alpha1.Forwarder{
		Name:      gateway.NamesFor(w.sr.Name).Forwarder,
		Namespace: w.ns,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: v1alpha1.GroupVersion.String(), Kind: "SiteRouter", Name: w.sr.Name, UID: w.sr.UID, Controller: &yes,
		}},
		Spec: v1alpha1.ForwarderSpec{
			WorkloadSelector: metav1.LabelSelector{MatchLabels: map[string]string{v1alpha1.LabelInstance: w.sr.Name}},
			CIDRs:            []v1alpha1.IPv4CIDR{"192.0.2.0/24", "198.51.100.0/24"},
		},
	})

	// The apiserver stamped it with the real time, so the manager's clock is real
	// here: it was made a moment ago, so it is not late yet.
	w.now = time.Now()
	state, err := w.routes().Grant(t.Context(), cidrs)
	if err != nil {
		t.Fatal(err)
	}
	if state.Reason != v1alpha1.ReasonRoutesPending {
		t.Errorf("state = %+v, want RoutesPending: the Forwarder was made a moment ago", state)
	}

	w.now = time.Now().Add(timeout + time.Minute)
	state, err = w.routes().Grant(t.Context(), cidrs)
	if err != nil {
		t.Fatal(err)
	}
	if state.Reason != v1alpha1.ReasonForwarderNotResponding {
		t.Errorf("state = %+v, want ForwarderNotResponding: it was made long enough ago", state)
	}
}

func TestAForwarderThatIsNotTheOperatorsIsNotTouched(t *testing.T) {
	w := newWorld(t)
	foreign := &v1alpha1.Forwarder{
		Name:      gateway.NamesFor(w.sr.Name).Forwarder,
		Namespace: w.ns,
		Spec: v1alpha1.ForwarderSpec{
			WorkloadSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			CIDRs:            []v1alpha1.IPv4CIDR{"203.0.113.0/24"},
		},
	}
	mustCreate(t, foreign)
	before := w.forwarder().ResourceVersion

	calls := map[string]func() error{
		"Ensure":   func() error { return w.routes().Ensure(t.Context()) },
		"Grant":    func() error { _, err := w.routes().Grant(t.Context(), cidrs); return err },
		"Withdraw": func() error { return w.routes().Withdraw(t.Context()) },
		"Report":   func() error { _, err := w.routes().Report(t.Context()); return err },
	}
	for name, call := range calls {
		var notOwned *gateway.NotOwnedError
		if err := call(); !errors.As(err, &notOwned) || notOwned.Kind != "Forwarder" {
			t.Errorf("%s: error = %v, want a NotOwnedError for the Forwarder", name, err)
		}
	}
	if after := w.forwarder().ResourceVersion; after != before {
		t.Error("a Forwarder of someone else was written")
	}
}

func TestForwardersOfTwoSiteRoutersAreApart(t *testing.T) {
	w := newWorld(t)
	other := &v1alpha1.SiteRouter{Name: "other", Namespace: w.ns, Spec: v1alpha1.SiteRouterSpec{Peer: v1alpha1.Peer{Address: "203.0.113.20"}}}
	mustCreate(t, other)

	if _, err := w.routes().Grant(t.Context(), cidrs[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := w.m.For(other).Grant(t.Context(), cidrs[1:]); err != nil {
		t.Fatal(err)
	}

	mine, theirs := &v1alpha1.Forwarder{}, &v1alpha1.Forwarder{}
	if err := k8sClient.Get(t.Context(), w.key(), mine); err != nil {
		t.Fatal(err)
	}
	if err := k8sClient.Get(t.Context(), client.ObjectKey{Namespace: w.ns, Name: gateway.NamesFor("other").Forwarder}, theirs); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cidrsOf(mine), cidrs[:1]) || !slices.Equal(cidrsOf(theirs), cidrs[1:]) {
		t.Errorf("cidrs: %v and %v", cidrsOf(mine), cidrsOf(theirs))
	}
	if mine.Spec.WorkloadSelector.MatchLabels[v1alpha1.LabelInstance] == theirs.Spec.WorkloadSelector.MatchLabels[v1alpha1.LabelInstance] {
		t.Error("two SiteRouters select the same gateway")
	}
}
