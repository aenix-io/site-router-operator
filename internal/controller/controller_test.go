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

package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/internal/config"
	"github.com/aenix-io/site-router-operator/internal/gateway"
	"github.com/aenix-io/site-router-operator/internal/reconcile"
	"github.com/aenix-io/site-router-operator/internal/rollout"
)

func statusOf(c metav1.Condition) string { return string(c.Status) + "/" + c.Reason }

func find(t *testing.T, list []metav1.Condition, typ string) metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(list, typ)
	if c == nil {
		t.Fatalf("no %s among %v", typ, list)
	}
	return *c
}

// said runs a pass over the conditions with the verdicts a pass would give.
func said(c *conditions, ok ...string) {
	for _, typ := range ok {
		c.set(typ, true, "Fine", "")
	}
}

func TestReadyIsTheAggregateOfFourConditions(t *testing.T) {
	all := []string{
		v1alpha1.ConditionValidated, v1alpha1.ConditionGatewayReady,
		v1alpha1.ConditionConfigured, v1alpha1.ConditionRoutesProgrammed,
	}

	t.Run("all four met, the tunnel not yet", func(t *testing.T) {
		c := newConditions(nil, 3)
		said(c, all...)
		c.set(v1alpha1.ConditionTunnelEstablished, false, "Observed", "tunnel-0=Down")
		got := c.finish("Unsaid", "")
		if r := find(t, got, v1alpha1.ConditionReady); r.Status != metav1.ConditionTrue || r.Reason != "AllConditionsMet" {
			t.Errorf("Ready = %s, want True: the tunnel is not part of it", statusOf(r))
		}
	})

	for _, missing := range all {
		t.Run("without "+missing, func(t *testing.T) {
			c := newConditions(nil, 3)
			for _, typ := range all {
				if typ != missing {
					said(c, typ)
				}
			}
			c.set(missing, false, "Because", "it is not")
			got := c.finish("Unsaid", "")
			r := find(t, got, v1alpha1.ConditionReady)
			if r.Status != metav1.ConditionFalse || r.Reason != "Because" || !strings.Contains(r.Message, missing) || !strings.Contains(r.Message, "it is not") {
				t.Errorf("Ready = %s (%s), want False with the reason and the message of %s", statusOf(r), r.Message, missing)
			}
		})
	}

	t.Run("a condition that is not known is not met", func(t *testing.T) {
		c := newConditions(nil, 3)
		said(c, v1alpha1.ConditionValidated, v1alpha1.ConditionGatewayReady, v1alpha1.ConditionConfigured)
		got := c.finish("Unsaid", "not looked at")
		if r := find(t, got, v1alpha1.ConditionReady); r.Status != metav1.ConditionFalse || r.Reason != "Unsaid" {
			t.Errorf("Ready = %s, want False for what the pass did not look at", statusOf(r))
		}
	})
}

// A condition that a pass says nothing about is not left as an earlier pass had
// it: it is no longer known.
func TestWhatAPassDoesNotSayIsUnknown(t *testing.T) {
	earlier := newConditions(nil, 1)
	said(earlier, v1alpha1.ConditionValidated, v1alpha1.ConditionGatewayReady, v1alpha1.ConditionConfigured, v1alpha1.ConditionRoutesProgrammed)
	before := earlier.finish("Unsaid", "")

	c := newConditions(before, 1)
	said(c, v1alpha1.ConditionValidated)
	c.set(v1alpha1.ConditionGatewayReady, false, "GatewayPending", "not running")
	got := c.finish("GatewayNotReady", "the gateway is not ready")

	for _, typ := range []string{v1alpha1.ConditionConfigured, v1alpha1.ConditionRoutesProgrammed, v1alpha1.ConditionTunnelEstablished} {
		if cur := find(t, got, typ); cur.Status != metav1.ConditionUnknown || cur.Reason != "GatewayNotReady" {
			t.Errorf("%s = %s, want Unknown/GatewayNotReady", typ, statusOf(cur))
		}
	}
	if cur := find(t, got, v1alpha1.ConditionValidated); cur.Status != metav1.ConditionTrue {
		t.Errorf("Validated = %s: what the pass said was changed", statusOf(cur))
	}
}

// The time a condition changed at is kept while it does not change, and moves when
// it does; what it says moves with the pass.
func TestAConditionKeepsItsTimeWhileItDoesNotChange(t *testing.T) {
	then := metav1.NewTime(time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC))
	before := []metav1.Condition{
		{Type: v1alpha1.ConditionValidated, Status: metav1.ConditionTrue, Reason: "Accepted", LastTransitionTime: then, ObservedGeneration: 1},
		{Type: v1alpha1.ConditionConfigured, Status: metav1.ConditionFalse, Reason: "ConfigureFailed", LastTransitionTime: then, ObservedGeneration: 1},
	}
	c := newConditions(before, 2)
	c.set(v1alpha1.ConditionValidated, true, "Accepted", "")
	c.set(v1alpha1.ConditionConfigured, true, "ConfigApplied", "")
	got := c.finish("Unsaid", "")

	v := find(t, got, v1alpha1.ConditionValidated)
	if !v.LastTransitionTime.Equal(&then) {
		t.Errorf("Validated moved from %v to %v with no change", then, v.LastTransitionTime)
	}
	if v.ObservedGeneration != 2 {
		t.Errorf("observedGeneration = %d, want the generation of the pass", v.ObservedGeneration)
	}
	cf := find(t, got, v1alpha1.ConditionConfigured)
	if cf.LastTransitionTime.Equal(&then) {
		t.Error("Configured kept its time although it changed")
	}
}

func TestAMessageIsCutShort(t *testing.T) {
	c := newConditions(nil, 1)
	c.set(v1alpha1.ConditionConfigured, false, "ConfigureFailed", strings.Repeat("x", 5000))
	got := find(t, c.finish("Unsaid", ""), v1alpha1.ConditionConfigured)
	if len(got.Message) != maxMessage || !strings.HasSuffix(got.Message, "...") {
		t.Errorf("the message is %d characters long and ends %q", len(got.Message), got.Message[len(got.Message)-5:])
	}
	short := newConditions(nil, 1)
	short.set(v1alpha1.ConditionConfigured, false, "ConfigureFailed", strings.Repeat("y", maxMessage))
	if m := find(t, short.finish("Unsaid", ""), v1alpha1.ConditionConfigured).Message; len(m) != maxMessage || strings.HasSuffix(m, "...") {
		t.Error("a message that fits was cut")
	}
}

func TestConditionsAreInTheOrderTheyAreShown(t *testing.T) {
	c := newConditions(nil, 1)
	said(c, v1alpha1.ConditionRoutesProgrammed, v1alpha1.ConditionValidated)
	finished := c.finish("Unsaid", "")
	got := make([]string, 0, len(finished))
	for _, cond := range finished {
		got = append(got, cond.Type)
	}
	if !slices.Equal(got, allConditions) {
		t.Errorf("order = %v, want %v", got, allConditions)
	}
}

func TestOnlyWhatAStepSaysIsTakenFromIt(t *testing.T) {
	c := newConditions(nil, 1)
	c.fromStep(reconcile.Result{Conditions: map[string]reconcile.Condition{
		reconcile.CondValidated:         {OK: true, Reason: "Accepted"},
		reconcile.CondRoutesProgrammed:  {OK: false, Reason: "RoutesPending", Message: "waiting"},
		reconcile.CondTunnelEstablished: {OK: true, Reason: "Observed"},
	}})
	got := c.finish("Unsaid", "")
	if cur := find(t, got, v1alpha1.ConditionRoutesProgrammed); cur.Status != metav1.ConditionFalse || cur.Reason != "RoutesPending" || cur.Message != "waiting" {
		t.Errorf("RoutesProgrammed = %s (%s)", statusOf(cur), cur.Message)
	}
	if cur := find(t, got, v1alpha1.ConditionTunnelEstablished); cur.Status != metav1.ConditionTrue {
		t.Errorf("TunnelEstablished = %s", statusOf(cur))
	}
	if cur := find(t, got, v1alpha1.ConditionConfigured); cur.Status != metav1.ConditionUnknown {
		t.Errorf("Configured = %s, want Unknown: the step said nothing about it", statusOf(cur))
	}
}

func vmiOf(obj map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: obj}
	u.SetGroupVersionKind(gateway.VirtualMachineInstanceGVK)
	return u
}

// ownedBy makes the instance be owned by an object, as KubeVirt writes it.
func ownedBy(u *unstructured.Unstructured, apiVersion, kind, name string, uid types.UID, controller bool) *unstructured.Unstructured {
	u.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: apiVersion, Kind: kind, Name: name, UID: uid, Controller: &controller}})
	return u
}

func TestWhatTheInstanceOfTheVMSays(t *testing.T) {
	iface := func(name, ip, mac string) map[string]any {
		return map[string]any{"name": name, "ipAddress": ip, "mac": mac}
	}
	status := func(phase string, ifaces ...any) map[string]any {
		return map[string]any{"phase": phase, "interfaces": ifaces}
	}
	volumes := []any{
		map[string]any{"name": "cloudinitdisk", "cloudInitNoCloud": map[string]any{"secretRef": map[string]any{"name": "seed"}}},
		map[string]any{"name": "boot", "dataVolume": map[string]any{"name": "disk-b2b2"}},
	}

	got := vmiFactsOf(vmiOf(map[string]any{
		"status": status("Running", iface("default", "10.244.1.9", "02:00:00:00:00:01")),
		"spec":   map[string]any{"volumes": volumes},
	}))
	if got.Phase != "Running" || got.IP != "10.244.1.9" || !slices.Equal(got.MACs, []string{"02:00:00:00:00:01"}) || got.BootDisk != "disk-b2b2" {
		t.Errorf("facts = %+v", got)
	}
	if !got.running() {
		t.Error("a running instance with an address is not running")
	}

	for name, f := range map[string]struct {
		obj  map[string]any
		want vmiFacts
	}{
		"an address that is not IPv4": {
			map[string]any{"status": status("Running", iface("default", "fd00::9", "02:00:00:00:00:01"))},
			vmiFacts{Phase: "Running", MACs: []string{"02:00:00:00:00:01"}},
		},
		"an address that is not an address": {
			map[string]any{"status": status("Running", iface("default", "10.244.1.9:443/../x", "02:00:00:00:00:01"))},
			vmiFacts{Phase: "Running", MACs: []string{"02:00:00:00:00:01"}},
		},
		"a card on another network": {
			map[string]any{"status": status("Running", iface("storage", "10.9.9.9", "02:00:00:00:00:02"), iface("default", "10.244.1.9", "02:00:00:00:00:01"))},
			vmiFacts{Phase: "Running", IP: "10.244.1.9", MACs: []string{"02:00:00:00:00:02", "02:00:00:00:00:01"}},
		},
		"no status": {map[string]any{}, vmiFacts{}},
	} {
		got := vmiFactsOf(vmiOf(f.obj))
		if got.Phase != f.want.Phase || got.IP != f.want.IP || !slices.Equal(got.MACs, f.want.MACs) {
			t.Errorf("%s: facts = %+v, want %+v", name, got, f.want)
		}
		if got.running() && got.IP == "" {
			t.Errorf("%s: running without an address", name)
		}
	}

	if (vmiFacts{Phase: "Scheduling", IP: "10.244.1.9"}).running() {
		t.Error("an instance that is scheduling is running")
	}
}

// The management credentials go to the address the instance reports, so an
// instance is the gateway's only when the VM of the gateway controls it, and the
// VM is told by its UID: a kind and a name are for anyone to write.
func TestOnlyTheInstanceTheVMMadeIsTheGateways(t *testing.T) {
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(gateway.VirtualMachineGVK)
	vm.SetName("site-router-x")
	vm.SetUID("the-uid-of-the-vm")

	const kubevirt = "kubevirt.io/v1"
	for name, c := range map[string]struct {
		u    *unstructured.Unstructured
		want bool
	}{
		"controlled by the VM":              {ownedBy(vmiOf(nil), kubevirt, "VirtualMachine", "site-router-x", "the-uid-of-the-vm", true), true},
		"the right name, another UID":       {ownedBy(vmiOf(nil), kubevirt, "VirtualMachine", "site-router-x", "a-forged-uid", false), false},
		"the right name, a UID guessed":     {ownedBy(vmiOf(nil), kubevirt, "VirtualMachine", "site-router-x", "", true), false},
		"owned but not controlled":          {ownedBy(vmiOf(nil), kubevirt, "VirtualMachine", "site-router-x", "the-uid-of-the-vm", false), false},
		"controlled by another VM":          {ownedBy(vmiOf(nil), kubevirt, "VirtualMachine", "site-router-y", "the-uid-of-the-vm", true), false},
		"controlled by a kind of that name": {ownedBy(vmiOf(nil), kubevirt, "Deployment", "site-router-x", "the-uid-of-the-vm", true), false},
		"controlled by another group":       {ownedBy(vmiOf(nil), "example.com/v1", "VirtualMachine", "site-router-x", "the-uid-of-the-vm", true), false},
		"an owner that is not a version":    {ownedBy(vmiOf(nil), "kubevirt.io/v1/x/y", "VirtualMachine", "site-router-x", "the-uid-of-the-vm", true), false},
		"no owner":                          {vmiOf(nil), false},
	} {
		if got := madeByVM(c.u, vm); got != c.want {
			t.Errorf("%s: madeByVM = %v, want %v", name, got, c.want)
		}
	}
}

func TestTheAddressOfTheTunnelIsAnIPv4Address(t *testing.T) {
	svc := func(in ...corev1.LoadBalancerIngress) *corev1.Service {
		return &corev1.Service{Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: in}}}
	}
	for name, c := range map[string]struct {
		svc  *corev1.Service
		want string
	}{
		"none yet":                 {svc(), ""},
		"an address":               {svc(corev1.LoadBalancerIngress{IP: "198.51.100.1"}), "198.51.100.1"},
		"a name":                   {svc(corev1.LoadBalancerIngress{Hostname: "lb.example.com"}), ""},
		"an IPv6 address":          {svc(corev1.LoadBalancerIngress{IP: "2001:db8::1"}), ""},
		"the first that is usable": {svc(corev1.LoadBalancerIngress{Hostname: "lb.example.com"}, corev1.LoadBalancerIngress{IP: "198.51.100.2"}), "198.51.100.2"},
	} {
		if got := tunnelAddress(c.svc); got != c.want {
			t.Errorf("%s: tunnelAddress = %q, want %q", name, got, c.want)
		}
	}
}

func TestTheBootDiskIsTheVolumeCalledBoot(t *testing.T) {
	if got := bootDiskOf(nil); got != "" {
		t.Errorf("bootDiskOf(nil) = %q", got)
	}
	vols := []any{
		"not a volume",
		map[string]any{"name": "other", "dataVolume": map[string]any{"name": "wrong"}},
		map[string]any{"name": "boot", "dataVolume": map[string]any{"name": "right"}},
	}
	if got := bootDiskOf(vols); got != "right" {
		t.Errorf("bootDiskOf = %q", got)
	}
	if got := bootDiskOf([]any{map[string]any{"name": "boot", "containerDisk": map[string]any{}}}); got != "" {
		t.Errorf("a boot volume that is no DataVolume gave %q", got)
	}
}

// A SiteRouter made again under a name that was used has nothing of the last one:
// what the engine applied to a gateway that is gone says nothing of the new.
func TestTheRevisionBelongsToTheSiteRouterItWasAppliedFor(t *testing.T) {
	r := &revisions{m: map[types.NamespacedName]revision{}}
	sr := func(uid string) *v1alpha1.SiteRouter {
		return &v1alpha1.SiteRouter{Name: "a", Namespace: "ns", UID: types.UID(uid)}
	}

	st := r.of(sr("one"))
	st.Revision = "rev-1"
	if got := r.of(sr("one")); got != st || got.Revision != "rev-1" {
		t.Errorf("the state of a SiteRouter changed between passes: %+v", got)
	}
	if got := r.of(sr("two")); got.Revision != "" {
		t.Errorf("a SiteRouter made again was given the revision %q of the last", got.Revision)
	}

	r.of(sr("two")).Revision = "rev-2"
	r.forget(client.ObjectKey{Namespace: "ns", Name: "a"})
	if got := r.of(sr("two")); got.Revision != "" {
		t.Errorf("forgotten, the revision is %q", got.Revision)
	}
	if got := r.of(&v1alpha1.SiteRouter{Name: "a", Namespace: "other", UID: "two"}); got.Revision != "" {
		t.Errorf("another namespace was given %q", got.Revision)
	}
}

// A key is not taken from the Secret of another gateway, which a SiteRouter
// controls. A Secret that something else owns, such as the object a secret store
// writes it from, is one a person has meant to be used.
func TestOnlyASecretASiteRouterControlsIsAnotherGateways(t *testing.T) {
	owned := func(apiVersion, kind string, controller bool) *corev1.Secret {
		return &corev1.Secret{OwnerReferences: []metav1.OwnerReference{
			{APIVersion: apiVersion, Kind: kind, Name: "x", UID: "u", Controller: &controller},
		}}
	}
	for name, c := range map[string]struct {
		secret *corev1.Secret
		want   bool
	}{
		"made for a gateway":             {owned("siterouter.aenix.io/v1alpha1", "SiteRouter", true), true},
		"another version of the API":     {owned("siterouter.aenix.io/v1beta1", "SiteRouter", true), true},
		"owned, not controlled":          {owned("siterouter.aenix.io/v1alpha1", "SiteRouter", false), false},
		"controlled by another kind":     {owned("siterouter.aenix.io/v1alpha1", "Forwarder", true), false},
		"controlled by another group":    {owned("example.com/v1", "SiteRouter", true), false},
		"controlled by a secret store":   {owned("external-secrets.io/v1", "ExternalSecret", true), false},
		"an owner that is not a version": {owned("siterouter.aenix.io/v1/x/y", "SiteRouter", true), false},
		"no owner":                       {&corev1.Secret{}, false},
	} {
		if got := controlledBySiteRouter(c.secret); got != c.want {
			t.Errorf("%s: controlledBySiteRouter = %v, want %v", name, got, c.want)
		}
	}
}

const (
	testDigestA = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testDigestB = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testDigestC = "sha256:" + "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// The name of a version is what the status says when it is of the same image, else
// what the configuration lists for the digest, else what the status last knew; an
// image no one lists is not given a name it could be mistaken for.
func TestTheVersionOfAnImage(t *testing.T) {
	r := &Reconciler{config: config.Config{Engine: config.Engine{Versions: []config.Version{
		{Name: "stable", Image: "registry.example/disk:2.0@" + testDigestB},
		{Name: "2.0", Image: "registry.example/disk:2.0@" + testDigestB},
		{Name: "1.0", Image: "registry.example/disk:1.0@" + testDigestA},
	}}}}
	at := func(version, digest string) *v1alpha1.SiteRouter {
		return &v1alpha1.SiteRouter{Status: v1alpha1.SiteRouterStatus{Engine: v1alpha1.EngineStatus{Version: version, ImageDigest: digest}}}
	}
	for name, c := range map[string]struct {
		sr     *v1alpha1.SiteRouter
		digest string
		want   string
	}{
		"the status knows the image":                  {at("2.0", testDigestB), testDigestB, "2.0"},
		"another name of the same image":              {at("stable", testDigestB), testDigestB, "stable"},
		"the status is of another image":              {at("1.0", testDigestA), testDigestB, "stable"},
		"the status was lost":                         {at("", ""), testDigestA, "1.0"},
		"an image that is not listed, the last known": {at("1.0", testDigestA), testDigestC, "1.0"},
		"an image that is not listed, nothing known":  {at("", ""), testDigestC, "unlisted"},
		"the status has a version and no digest":      {at("1.0", ""), testDigestB, "stable"},
	} {
		if got := r.versionOf(c.sr, c.digest); got != c.want {
			t.Errorf("%s: versionOf = %q, want %q", name, got, c.want)
		}
	}
}

// listFails makes the listing of SiteRouters fail while it is set.
type listFails struct {
	client.Client
	fail bool
}

func (l *listFails) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if l.fail {
		return errors.New("the apiserver does not answer")
	}
	return l.Client.List(ctx, list, opts...)
}

// The places of the moves that were under way are read once, before the first pass:
// from the statuses, not counting a SiteRouter that is being deleted, and tried
// again until they could be read.
func TestThePlacesOfMovesAreReadOnceBeforeTheFirstPass(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	siteRouter := func(name string, reason string, deleting bool) *v1alpha1.SiteRouter {
		sr := &v1alpha1.SiteRouter{Name: name, Namespace: "ns"}
		if reason != "" {
			sr.Status.Conditions = []metav1.Condition{{
				Type: v1alpha1.ConditionGatewayReady, Status: metav1.ConditionTrue, Reason: reason,
				LastTransitionTime: metav1.Now(),
			}}
		}
		if deleting {
			now := metav1.Now()
			sr.DeletionTimestamp, sr.Finalizers = &now, []string{Finalizer}
		}
		return sr
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		siteRouter("moving", v1alpha1.ReasonUpgrading, false),
		siteRouter("running", "Running", false),
		siteRouter("none", "", false),
		siteRouter("deleted", v1alpha1.ReasonUpgrading, true),
	).Build()
	c := &listFails{Client: fakeClient, fail: true}
	r := &Reconciler{client: c, gate: rollout.NewGate(5)}

	if err := r.restoreGate(t.Context()); err == nil {
		t.Fatal("a failure to read was not reported, so the pass would run with no places taken")
	}
	if n := r.gate.Moving(); n != 0 {
		t.Errorf("%d places taken before they could be read", n)
	}

	c.fail = false
	if err := r.restoreGate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := r.gate.Moving(); n != 1 {
		t.Errorf("%d places taken, want the one gateway that was moving", n)
	}

	// It is not read again: a place that was given back is not taken again from a
	// status that has not caught up.
	r.gate.Release(types.NamespacedName{Namespace: "ns", Name: "moving"})
	if err := r.restoreGate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := r.gate.Moving(); n != 0 {
		t.Errorf("%d places taken after the second call, want it to read nothing", n)
	}
}
