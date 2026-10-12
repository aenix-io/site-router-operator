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

package controller_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aenix-io/site-router-operator/api/engine"
	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/internal/config"
	"github.com/aenix-io/site-router-operator/internal/controller"
	"github.com/aenix-io/site-router-operator/internal/desired"
	"github.com/aenix-io/site-router-operator/internal/gateway"
)

// gauge reads a series of the operator, or says that there is none.
func (g *rig) gauge(metric string, labels map[string]string) (float64, bool) {
	g.t.Helper()
	families, err := g.reg.Gather()
	if err != nil {
		g.t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != metric {
			continue
		}
	series:
		for _, m := range f.GetMetric() {
			have := map[string]string{}
			for _, l := range m.GetLabel() {
				have[l.GetName()] = l.GetValue()
			}
			for k, v := range labels {
				if have[k] != v {
					continue series
				}
			}
			return m.GetGauge().GetValue(), true
		}
	}
	return 0, false
}

// expectTheObjectsOfTheGateway checks that the gateway is made of what the engine
// and the configuration say.
func (g *rig) expectTheObjectsOfTheGateway() {
	g.t.Helper()
	t, names := g.t, g.names()
	for _, kind := range []struct {
		obj  client.Object
		name string
	}{
		{&corev1.Secret{}, names.APIKey},
		{&corev1.Secret{}, names.PSK},
		{&corev1.Secret{}, "site-router-branch-office-seed-b2b2"},
		{&corev1.Service{}, names.Tunnel},
		{&v1alpha1.Forwarder{}, names.Forwarder},
	} {
		if err := k8sClient.Get(t.Context(), client.ObjectKey{Namespace: g.ns, Name: kind.name}, kind.obj); err != nil {
			t.Errorf("%s: %v", kind.name, err)
		}
	}
	for _, o := range []struct {
		gvk  schema.GroupVersionKind
		name string
	}{
		{gateway.VirtualMachineGVK, names.VM},
		{gateway.DataVolumeGVK, "site-router-branch-office-boot-b2b2"},
	} {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(o.gvk)
		if err := k8sClient.Get(t.Context(), client.ObjectKey{Namespace: g.ns, Name: o.name}, u); err != nil {
			t.Errorf("%s %s: %v", o.gvk.Kind, o.name, err)
		}
	}
}

// expectWhatTheEngineWasTold checks the state the engine was last asked for, and
// where.
func (g *rig) expectWhatTheEngineWasTold() {
	g.t.Helper()
	t, names := g.t, g.names()
	d, _ := g.spy.last()
	if ep := g.spy.lastEndpoint(); ep.Address != gatewayIP+":443" || string(ep.Credentials["token"]) == "" {
		t.Errorf("endpoint = %v, want the management port of the gateway pod, with the credentials of the engine", ep.Address)
	}
	if len(d.Tunnels) != 1 || d.Tunnels[0].ID != desired.TunnelID || d.Tunnels[0].PeerAddress != "203.0.113.10" || d.Tunnels[0].LocalID != tunnelIP {
		t.Errorf("tunnels = %+v", d.Tunnels)
	}
	// The credentials are those of the Secret the operator made, which the guest was
	// given: a pass that asked the engine for new ones would lock itself out.
	key := &corev1.Secret{}
	if err := k8sClient.Get(t.Context(), client.ObjectKey{Namespace: g.ns, Name: names.APIKey}, key); err != nil {
		t.Fatal(err)
	}
	if got, want := string(g.spy.lastEndpoint().Credentials["token"]), string(key.Data["token"]); got == "" || got != want {
		t.Errorf("the engine was reached with the token %q, want the one in the Secret, %q", got, want)
	}
	psk := &corev1.Secret{}
	if err := k8sClient.Get(t.Context(), client.ObjectKey{Namespace: g.ns, Name: names.PSK}, psk); err != nil {
		t.Fatal(err)
	}
	if len(d.Tunnels) == 1 && d.Tunnels[0].PSK != string(psk.Data["psk"]) {
		t.Error("the engine was given a key that is not the one in the Secret")
	}
	// The connector's scope has the cluster IPs of the Services of the namespace as
	// well, among them that of the tunnel; what matters is that the pod of the
	// tenant is in and the pod of the gateway is not.
	if dests := d.Guards.TunnelIngress.Destinations; !slices.Contains(dests, webIP+"/32") || slices.Contains(dests, gatewayIP+"/32") {
		t.Errorf("destinations = %v, want the pod of the tenant and not the gateway", dests)
	}
	if !slices.Equal(d.TunnelNICMACs, []string{"02:00:00:00:00:01"}) {
		t.Errorf("MACs = %v", d.TunnelNICMACs)
	}
	if d.Guards.ManagementAllow != "10.244.0.0/16" {
		t.Errorf("management = %q", d.Guards.ManagementAllow)
	}
}

// The first pass makes everything the gateway is made of, and says that it is not
// up yet; once KubeVirt and the load balancer have done their part the gateway is
// configured, and once the connector has done its part it is routed.
func TestAGatewayIsMadeAndBroughtUp(t *testing.T) {
	g := newRig(t)
	g.tenantPod()

	res := g.reconcile()
	sr := g.sr()
	if !slices.Contains(sr.Finalizers, controller.Finalizer) {
		t.Errorf("finalizers = %v, want the finalizer of the operator", sr.Finalizers)
	}
	g.expect(v1alpha1.ConditionValidated, metav1.ConditionTrue, "Accepted")
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionFalse, v1alpha1.ReasonGatewayPending)
	g.expect(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonGatewayPending)
	if res.RequeueAfter <= 0 || res.RequeueAfter > 10*time.Second {
		t.Errorf("RequeueAfter = %v, want a short wait for a gateway that is not up", res.RequeueAfter)
	}

	g.expectTheObjectsOfTheGateway()
	names := g.names()
	if got := sr.Status.Engine; got.Name != "fake-cmd" || got.Version != "2.0" || got.ImageDigest != digestV2 {
		t.Errorf("engine = %+v, want the latest version of the engine", got)
	}
	if got := sr.Status.PSKSecretRef; got == nil || got.Name != names.PSK {
		t.Errorf("pskSecretRef = %v, want %s", got, names.PSK)
	}
	if got := g.asked(); len(got) != 0 {
		t.Errorf("the Forwarder asks for %v before the gateway is configured, want nothing", got)
	}
	if _, n := g.spy.last(); n != 0 {
		t.Errorf("the engine was asked %d times before the gateway was up", n)
	}

	// The connector reports the tenant while the VM starts, and the gateway runs
	// and has an address.
	g.sync()
	g.boot()
	g.sync()
	g.reconcile()
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionTrue, "Running")
	g.expect(v1alpha1.ConditionConfigured, metav1.ConditionTrue, "ConfigApplied")
	g.expect(v1alpha1.ConditionRoutesProgrammed, metav1.ConditionFalse, v1alpha1.ReasonRoutesPending)
	g.expect(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonRoutesPending)
	if got := g.sr().Status.TunnelAddress; got != tunnelIP {
		t.Errorf("tunnelAddress = %q, want %q", got, tunnelIP)
	}

	g.expectWhatTheEngineWasTold()

	// The connector acts.
	g.sync()
	res = g.reconcile()
	g.expect(v1alpha1.ConditionRoutesProgrammed, metav1.ConditionTrue, "Programmed")
	g.expect(v1alpha1.ConditionReady, metav1.ConditionTrue, "AllConditionsMet")
	g.expect(v1alpha1.ConditionTunnelEstablished, metav1.ConditionFalse, "Observed")
	if got := g.asked(); !slices.Equal(got, []string{"192.0.2.0/24"}) {
		t.Errorf("the Forwarder asks for %v", got)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Errorf("RequeueAfter = %v, want the poll time", res.RequeueAfter)
	}

	// That the remote end has not called yet is not a fault, and is not said to be.
	for _, e := range g.events() {
		if strings.HasPrefix(e, "Warning") {
			t.Errorf("a gateway that is waiting for the remote end said %q", e)
		}
	}

	// The remote end calls.
	g.dev.Dial(desired.TunnelID)
	g.reconcile()
	g.expect(v1alpha1.ConditionTunnelEstablished, metav1.ConditionTrue, "Observed")

	labels := map[string]string{"namespace": g.ns, "name": name}
	if v, ok := g.gauge("site_router_ready", labels); !ok || v != 1 {
		t.Errorf("site_router_ready = %v (%v), want 1", v, ok)
	}
	if v, ok := g.gauge("site_router_tunnel_up", labels); !ok || v != 1 {
		t.Errorf("site_router_tunnel_up = %v (%v), want 1", v, ok)
	}
	if v, ok := g.gauge("site_router_gateway_image_info", map[string]string{"namespace": g.ns, "version": "2.0", "digest": digestV2}); !ok || v != 1 {
		t.Errorf("site_router_gateway_image_info = %v (%v), want 1", v, ok)
	}
}

// A pass that finds nothing different writes nothing: not the status, and not
// the Forwarder, whose spec the connector answers.
func TestAPassThatChangesNothingWritesNothing(t *testing.T) {
	g := newRig(t)
	g.ready()

	before, f := g.sr(), g.forwarder()
	g.events()
	writes := g.writes.statusWrites.Load()
	g.reconcile()
	after, g2 := g.sr(), g.forwarder()
	if n := g.writes.statusWrites.Load(); n != writes {
		t.Errorf("the status was written %d times by a pass that found nothing different", n-writes)
	}
	if events := g.events(); len(events) != 0 {
		t.Errorf("a pass that found nothing different said %v", events)
	}
	if before.ResourceVersion != after.ResourceVersion {
		t.Errorf("the SiteRouter was written: %s -> %s", before.ResourceVersion, after.ResourceVersion)
	}
	if f.ResourceVersion != g2.ResourceVersion {
		t.Errorf("the Forwarder was written: %s -> %s", f.ResourceVersion, g2.ResourceVersion)
	}
}

func TestEditingTheSpecIsCarriedToTheGateway(t *testing.T) {
	g := newRig(t)
	g.ready()
	_, n := g.spy.last()

	g.edit(func(sr *v1alpha1.SiteRouter) {
		sr.Spec.Peer.Address = "203.0.113.77"
		sr.Spec.RemoteCIDRs = []v1alpha1.IPv4CIDR{"192.0.2.0/24", "198.18.0.0/24"}
	})
	g.reconcile()

	d, n2 := g.spy.last()
	if n2 == n {
		t.Fatal("the engine was not asked again")
	}
	if d.Tunnels[0].PeerAddress != "203.0.113.77" {
		t.Errorf("peer = %q", d.Tunnels[0].PeerAddress)
	}
	if want := []string{"192.0.2.0/24", "198.18.0.0/24"}; !slices.Equal(d.Guards.TunnelIngress.Sources, want) {
		t.Errorf("sources = %v, want %v", d.Guards.TunnelIngress.Sources, want)
	}
	if got := g.asked(); !slices.Equal(got, []string{"192.0.2.0/24", "198.18.0.0/24"}) {
		t.Errorf("the Forwarder asks for %v", got)
	}
	// The connector has not seen the new request.
	g.expect(v1alpha1.ConditionRoutesProgrammed, metav1.ConditionFalse, v1alpha1.ReasonRoutesPending)
	g.sync()
	g.reconcile()
	g.expect(v1alpha1.ConditionReady, metav1.ConditionTrue, "AllConditionsMet")
	if got := g.sr().Status.ObservedGeneration; got != g.sr().Generation {
		t.Errorf("observedGeneration = %d, want %d", got, g.sr().Generation)
	}
}

// A tenant whose networks the cluster cannot take is told so, the routes they had
// are taken away, and nothing reaches the engine. The message says what is wrong
// and not what the cluster is made of.
func TestARefusedInstanceIsNotRouted(t *testing.T) {
	g := newRig(t)
	g.ready()
	_, n := g.spy.last()
	g.events()

	g.edit(func(sr *v1alpha1.SiteRouter) { sr.Spec.RemoteCIDRs = []v1alpha1.IPv4CIDR{"10.244.5.0/24"} })
	g.reconcile()

	g.expect(v1alpha1.ConditionValidated, metav1.ConditionFalse, v1alpha1.ReasonInvalidRemoteCIDR)
	g.expect(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonInvalidRemoteCIDR)
	if msg := g.cond(v1alpha1.ConditionValidated).Message; strings.Contains(msg, "10.244.0.0/16") {
		t.Errorf("the condition names a network of the cluster: %q", msg)
	}
	if got := g.asked(); len(got) != 0 {
		t.Errorf("the Forwarder asks for %v, want the routes taken away", got)
	}
	if _, n2 := g.spy.last(); n2 != n {
		t.Errorf("the engine was asked %d times for a refused instance", n2-n)
	}
	var warned bool
	for _, e := range g.events() {
		warned = warned || (strings.HasPrefix(e, "Warning "+v1alpha1.ReasonInvalidRemoteCIDR+" "))
	}
	if !warned {
		t.Error("no warning event for the refusal")
	}
}

// A cluster that does not answer is not a reason to break a tunnel: the pass says
// it could not judge, leaves the routes where they are and asks to be run again.
func TestAClusterThatDoesNotAnswerLeavesTheRoutes(t *testing.T) {
	g := newRig(t)
	g.ready()
	_, n := g.spy.last()

	g.disc.set(true)
	g.now = g.now.Add(2 * time.Minute) // the listing it kept is stale
	_, err := g.reconcileErr()
	if err == nil {
		t.Error("the pass did not ask to be run again")
	}
	g.expect(v1alpha1.ConditionValidated, metav1.ConditionFalse, v1alpha1.ReasonValidationUnavailable)
	if got := g.asked(); !slices.Equal(got, []string{"192.0.2.0/24"}) {
		t.Errorf("the Forwarder asks for %v, want the routes kept", got)
	}
	if _, n2 := g.spy.last(); n2 != n {
		t.Error("the engine was asked while the cluster could not be judged")
	}
	if msg := g.cond(v1alpha1.ConditionValidated).Message; strings.Contains(msg, "10.96.0.1") {
		t.Errorf("the condition carries the cause: %q", msg)
	}

	g.disc.set(false)
	g.reconcile()
	g.expect(v1alpha1.ConditionReady, metav1.ConditionTrue, "AllConditionsMet")
}

// What the guest says about its source filter decides what happens to the routes:
// a filter that is down takes them away, one that cannot be read leaves them.
func TestTheRoutesFollowTheSourceFilter(t *testing.T) {
	g := newRig(t)
	g.ready()

	g.spy.mu.Lock()
	g.spy.failConfirm = errors.New("the management API is down")
	g.spy.mu.Unlock()
	g.reconcile()
	g.expect(v1alpha1.ConditionConfigured, metav1.ConditionFalse, v1alpha1.ReasonSourceFilterPending)
	if got := g.asked(); len(got) == 0 {
		t.Error("the routes were taken away because the management API did not answer")
	}

	down := engine.GuardDown
	g.spy.mu.Lock()
	g.spy.failConfirm, g.spy.forceGuard = nil, &down
	g.spy.mu.Unlock()
	g.reconcile()
	g.expect(v1alpha1.ConditionRoutesProgrammed, metav1.ConditionFalse, v1alpha1.ReasonSourceFilterPending)
	if got := g.asked(); len(got) != 0 {
		t.Errorf("the Forwarder asks for %v while the filter is down", got)
	}

	g.spy.mu.Lock()
	g.spy.forceGuard = nil
	g.spy.mu.Unlock()
	g.reconcile()
	g.sync()
	g.reconcile()
	g.expect(v1alpha1.ConditionReady, metav1.ConditionTrue, "AllConditionsMet")
}

// The engine leaks the key in its errors, as engines do. Nothing the tenant can
// read has it.
func TestTheKeyIsNotInWhatTheTenantReads(t *testing.T) {
	g := newRig(t)
	g.ready()
	g.dev.FailApply = errors.New("apply refused")
	g.edit(func(sr *v1alpha1.SiteRouter) { sr.Spec.Peer.Address = "203.0.113.99" })
	g.events()
	g.reconcile()

	g.expect(v1alpha1.ConditionConfigured, metav1.ConditionFalse, v1alpha1.ReasonConfigureFailed)
	psk := &corev1.Secret{}
	if err := k8sClient.Get(t.Context(), client.ObjectKey{Namespace: g.ns, Name: g.names().PSK}, psk); err != nil {
		t.Fatal(err)
	}
	key := string(psk.Data["psk"])
	if key == "" {
		t.Fatal("no key")
	}
	for _, c := range g.sr().Status.Conditions {
		if strings.Contains(c.Message, key) {
			t.Errorf("%s carries the key: %q", c.Type, c.Message)
		}
	}
	for _, e := range g.events() {
		if strings.Contains(e, key) {
			t.Errorf("an event carries the key: %q", e)
		}
	}
	if !strings.Contains(g.cond(v1alpha1.ConditionConfigured).Message, "***") {
		t.Errorf("Configured = %q, want the key redacted", g.cond(v1alpha1.ConditionConfigured).Message)
	}
}

// The key of the tunnel is made once. A pass that finds the Secret uses what is
// in it.
func TestTheKeyOfTheTunnelIsMadeOnce(t *testing.T) {
	g := newRig(t)
	g.reconcile()
	read := func() string {
		s := &corev1.Secret{}
		if err := k8sClient.Get(t.Context(), client.ObjectKey{Namespace: g.ns, Name: g.names().PSK}, s); err != nil {
			t.Fatal(err)
		}
		return string(s.Data["psk"])
	}
	first := read()
	if len(first) < 32 {
		t.Errorf("the key is %d characters", len(first))
	}
	g.reconcile()
	g.reconcile()
	if second := read(); second != first {
		t.Error("the key changed")
	}
}

// A SiteRouter that names its own Secret is not given one, and waits until the
// Secret is there.
func TestANamedKeyIsWaitedFor(t *testing.T) {
	g := newRig(t, func(_ *config.Config, sr *v1alpha1.SiteRouter) {
		sr.Spec.Peer.Auth = &v1alpha1.PeerAuth{SecretRef: &v1alpha1.LocalSecretRef{Name: "branch-key"}}
	})
	g.tenantPod()
	g.reconcile()
	g.sync()
	g.boot()
	g.reconcile()

	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionFalse, v1alpha1.ReasonPSKSecretPending)
	if got := g.sr().Status.PSKSecretRef; got == nil || got.Name != "branch-key" {
		t.Errorf("pskSecretRef = %v", got)
	}
	err := k8sClient.Get(t.Context(), client.ObjectKey{Namespace: g.ns, Name: g.names().PSK}, &corev1.Secret{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("the operator made a Secret for a key that the SiteRouter names: %v", err)
	}

	mustCreate(t, &corev1.Secret{Name: "branch-key", Namespace: g.ns, Data: map[string][]byte{"psk": []byte("named-key-of-the-tunnel")}})
	g.reconcile()
	g.sync()
	g.reconcile()
	g.expect(v1alpha1.ConditionReady, metav1.ConditionTrue, "AllConditionsMet")
	if d, _ := g.spy.last(); d.Tunnels[0].PSK != "named-key-of-the-tunnel" {
		t.Errorf("the engine was given %q", d.Tunnels[0].PSK)
	}
}

// With no address for the peer there is no tunnel to bring up. That is said, and
// is not a failure.
func TestAGatewayWithNoPeerIsReadyAndSaysSo(t *testing.T) {
	g := newRig(t, func(_ *config.Config, sr *v1alpha1.SiteRouter) { sr.Spec.Peer.Address = "" })
	g.ready()

	g.expect(v1alpha1.ConditionValidated, metav1.ConditionTrue, v1alpha1.ReasonPeerNotConfigured)
	if d, _ := g.spy.last(); len(d.Tunnels) != 0 {
		t.Errorf("tunnels = %+v, want none", d.Tunnels)
	}
	// There is no tunnel that is up, and none that is asked for and down.
	if v, ok := g.gauge("site_router_tunnel_up", map[string]string{"namespace": g.ns, "name": name}); !ok || v != 0 {
		t.Errorf("site_router_tunnel_up = %v (%v), want 0", v, ok)
	}
}

// An object under the name of one of the gateway that the operator did not make
// is left as it is, and said to be in the way.
func TestAnObjectThatIsNotTheOperatorsIsLeftAlone(t *testing.T) {
	g := newRig(t)
	mustCreate(t, &corev1.Secret{Name: g.names().APIKey, Namespace: g.ns, Data: map[string][]byte{"token": []byte("not-ours")}})

	if _, err := g.reconcileErr(); err != nil {
		t.Errorf("a conflict needs a person, and is not tried again with a back-off: %v", err)
	}
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionFalse, v1alpha1.ReasonObjectConflict)
	for _, b := range g.spy.bootstrapped() {
		if len(b.Existing) != 0 {
			t.Errorf("the engine was given credentials that are not the operator's: %v", b.Existing)
		}
	}
	s := &corev1.Secret{}
	if err := k8sClient.Get(t.Context(), client.ObjectKey{Namespace: g.ns, Name: g.names().APIKey}, s); err != nil {
		t.Fatal(err)
	}
	if string(s.Data["token"]) != "not-ours" {
		t.Error("the Secret was changed")
	}
	// The tenant is told what is wrong with what they declared all the same.
	g.expect(v1alpha1.ConditionValidated, metav1.ConditionTrue, "Accepted")
}

// A SiteRouter that is deleted lets go once the routes to its gateway are gone,
// which the connector says.
func TestADeletedSiteRouterWaitsForTheRoutesToGo(t *testing.T) {
	g := newRig(t)
	g.ready()

	if err := k8sClient.Delete(t.Context(), g.sr()); err != nil {
		t.Fatal(err)
	}
	res := g.reconcile()
	if g.sr() == nil {
		t.Fatal("the SiteRouter went before the connector took the routes away")
	}
	if got := g.asked(); len(got) != 0 {
		t.Errorf("the Forwarder asks for %v, want the routes withdrawn", got)
	}
	if res.RequeueAfter <= 0 {
		t.Error("the SiteRouter is not looked at again")
	}

	g.sync()
	g.reconcile()
	if g.sr() != nil {
		t.Error("the SiteRouter is held after the routes are gone")
	}
	if _, ok := g.gauge("site_router_ready", map[string]string{"namespace": g.ns, "name": name}); ok {
		t.Error("the series of a SiteRouter that is gone are still reported")
	}
}

// A cluster with no connector must not keep a SiteRouter for ever.
func TestADeletedSiteRouterIsNotHeldByAConnectorThatNeverAnswers(t *testing.T) {
	g := newRig(t)
	g.ready()

	if err := k8sClient.Delete(t.Context(), g.sr()); err != nil {
		t.Fatal(err)
	}
	g.reconcile()
	if g.sr() == nil {
		t.Fatal("the SiteRouter went at once")
	}
	g.now = g.now.Add(2 * time.Minute)
	g.reconcile()
	if g.sr() != nil {
		t.Error("the SiteRouter is held although no connector has answered for longer than one is waited for")
	}
}

// A Secret under the name of the key that the operator did not make is not
// trusted with the tunnel: whoever made it chose the key.
func TestAKeyThatIsNotTheOperatorsIsNotUsed(t *testing.T) {
	g := newRig(t)
	mustCreate(t, &corev1.Secret{Name: g.names().PSK, Namespace: g.ns, Data: map[string][]byte{"psk": []byte("chosen-by-someone-else")}})

	if _, err := g.reconcileErr(); err != nil {
		t.Errorf("a conflict needs a person, and is not tried again with a back-off: %v", err)
	}
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionFalse, v1alpha1.ReasonObjectConflict)
	if _, n := g.spy.last(); n != 0 {
		t.Error("the engine was asked with a key that the operator did not make")
	}
	// Nothing that is someone else's goes any further than the pass that finds it.
	if got := g.spy.bootstrapped(); len(got) != 0 {
		t.Errorf("the engine was asked for a seed %d times with a key that is someone else's", len(got))
	}
}

// A Forwarder under the name of the gateway that the SiteRouter does not control
// is not its to take routes from, and does not keep it from being deleted.
func TestAForwarderThatIsNotTheSiteRoutersDoesNotHoldItsDeletion(t *testing.T) {
	g := newRig(t)
	foreign := &v1alpha1.Forwarder{
		Name: g.names().Forwarder, Namespace: g.ns,
		Spec: v1alpha1.ForwarderSpec{
			CIDRs:            []v1alpha1.IPv4CIDR{"198.18.0.0/24"},
			WorkloadSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "other-gateway"}},
			ScopeSelector:    metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
		},
	}
	mustCreate(t, foreign)
	g.reconcile() // adds the finalizer; the Forwarder is said to be in the way

	if err := k8sClient.Delete(t.Context(), g.sr()); err != nil {
		t.Fatal(err)
	}
	g.reconcile()
	if g.sr() != nil {
		t.Error("the SiteRouter is held by a Forwarder that is not its own")
	}
	got := &v1alpha1.Forwarder{}
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(foreign), got); err != nil {
		t.Fatal(err)
	}
	if len(got.Spec.CIDRs) != 1 {
		t.Errorf("the Forwarder of someone else was changed: %v", got.Spec.CIDRs)
	}
}

// A gateway that goes away is not said to be routed on what was true of it before:
// what the pass could not look at, it says it does not know.
func TestAGatewayThatGoesAwayIsNotSaidToBeRouted(t *testing.T) {
	g := newRig(t)
	g.ready()

	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(gateway.VirtualMachineInstanceGVK)
	vmi.SetName(g.names().VM)
	vmi.SetNamespace(g.ns)
	if err := k8sClient.Delete(t.Context(), vmi); err != nil {
		t.Fatal(err)
	}
	g.reconcile()

	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionFalse, v1alpha1.ReasonGatewayPending)
	for _, typ := range []string{v1alpha1.ConditionConfigured, v1alpha1.ConditionRoutesProgrammed, v1alpha1.ConditionTunnelEstablished} {
		if c := g.cond(typ); c.Status != metav1.ConditionUnknown {
			t.Errorf("%s = %s/%s, want Unknown for what the pass could not look at", typ, c.Status, c.Reason)
		}
	}
	// The routes stay: a gateway that is restarting is not a reason to take them.
	if got := g.asked(); !slices.Equal(got, []string{"192.0.2.0/24"}) {
		t.Errorf("the Forwarder asks for %v, want the routes kept", got)
	}
}

// A condition is one of the three statuses, the series of the metrics say which,
// and one only.
func TestTheConditionsAreOneHotInTheMetrics(t *testing.T) {
	g := newRig(t)
	g.tenantPod()
	g.reconcile()
	labels := func(status string) map[string]string {
		return map[string]string{"namespace": g.ns, "name": name, "type": "Ready", "status": status}
	}
	if v, ok := g.gauge("site_router_condition", labels("False")); !ok || v != 1 {
		t.Errorf("Ready=False = %v (%v), want 1", v, ok)
	}
	g.sync()
	g.boot()
	g.sync()
	g.reconcile()
	g.sync()
	g.reconcile()
	if v, ok := g.gauge("site_router_condition", labels("True")); !ok || v != 1 {
		t.Errorf("Ready=True = %v (%v), want 1", v, ok)
	}
	if _, ok := g.gauge("site_router_condition", labels("False")); ok {
		t.Error("Ready=False is still reported after the SiteRouter is Ready")
	}
}

// The class of the load balancer cannot be changed on a Service that exists. An
// operator that starts with another class says so, and leaves the Service as it is.
func TestAChangedLoadBalancerClassIsNamed(t *testing.T) {
	g := newRig(t)
	g.reconcile()
	g.restart(func(cfg *config.Config) { cfg.Gateway.Service.LoadBalancerClass = "example.com/metallb" })

	if _, err := g.reconcileErr(); err != nil {
		t.Errorf("a class that needs a person is not tried again with a back-off: %v", err)
	}
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionFalse, v1alpha1.ReasonLoadBalancerClassChanged)
	svc := &corev1.Service{}
	if err := k8sClient.Get(t.Context(), client.ObjectKey{Namespace: g.ns, Name: g.names().Tunnel}, svc); err != nil {
		t.Fatal(err)
	}
	if svc.Spec.LoadBalancerClass != nil {
		t.Errorf("the class of the Service became %q", *svc.Spec.LoadBalancerClass)
	}
}

// A platform that has the gateway run with no isolation, and does not say it
// means to, gets no gateway.
func TestAGatewayWithNoIsolationIsNotMadeUnlessItIsAcknowledged(t *testing.T) {
	g := newRig(t, func(cfg *config.Config, _ *v1alpha1.SiteRouter) {
		cfg.Gateway.Isolation = gateway.IsolationConfig{Mode: gateway.IsolationNone}
	})
	if _, err := g.reconcileErr(); err != nil {
		t.Errorf("a configuration that needs a person is not tried again with a back-off: %v", err)
	}
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionFalse, v1alpha1.ReasonGatewayIsolationUnavailable)
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(gateway.VirtualMachineGVK)
	if err := k8sClient.Get(t.Context(), client.ObjectKey{Namespace: g.ns, Name: g.names().VM}, vm); !apierrors.IsNotFound(err) {
		t.Errorf("a VM was made with no isolation: %v", err)
	}
}

// Each thing a gateway waits for is said when it is the one that is missing, and
// the engine is not asked before they are all there.
func TestAGatewayWaitsForEachThingItNeeds(t *testing.T) {
	t.Run("a VM that is not running", func(t *testing.T) {
		g := newRig(t)
		g.tenantPod()
		g.reconcile()
		g.sync()
		g.bootAs("Scheduling", true)
		g.reconcile()
		g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionFalse, v1alpha1.ReasonGatewayPending)
		if _, n := g.spy.last(); n != 0 {
			t.Error("the engine was asked while the VM was not running")
		}
	})

	t.Run("an address for the tunnel", func(t *testing.T) {
		g := newRig(t)
		g.tenantPod()
		g.reconcile()
		g.sync()
		g.bootAs("Running", false)
		g.reconcile()
		g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionFalse, v1alpha1.ReasonTunnelAddressPending)
		if _, n := g.spy.last(); n != 0 {
			t.Error("the engine was asked while the tunnel had no address")
		}
		if got := g.sr().Status.TunnelAddress; got != "" {
			t.Errorf("tunnelAddress = %q, want none", got)
		}
	})
}

// An instance that someone else made under the name of the VM, and that names the
// VM as its owner, is not the gateway: the management credentials would go to
// whatever address it reports.
func TestAnInstanceThatIsNotTheVMsIsNotTheGateway(t *testing.T) {
	g := newRig(t)
	g.tenantPod()
	g.reconcile()
	g.sync()
	g.bootOwnedBy("Running", true, "a-uid-that-is-not-the-vms")
	g.reconcile()

	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionFalse, v1alpha1.ReasonGatewayPending)
	if _, n := g.spy.last(); n != 0 {
		t.Error("the engine was reached at the address of an instance that is not the gateway's")
	}
	if ep := g.spy.lastEndpoint(); ep.Address != "" {
		t.Errorf("the credentials of the gateway were sent to %s", ep.Address)
	}
}

// The engine puts what it was given into its errors, as engines do. What comes of
// that is not shown to the tenant, written to an event, or returned to be logged.
func TestAFailureOfTheEngineKeepsTheCredentialsOut(t *testing.T) {
	g := newRig(t)
	g.ready()
	token := string(g.spy.lastEndpoint().Credentials["token"])
	if token == "" {
		t.Fatal("no token")
	}
	g.spy.mu.Lock()
	g.spy.failBootstrap = func(r engine.BootstrapRequest) error {
		return errors.New("cannot render a seed for the token " + string(r.Existing["token"]))
	}
	g.spy.mu.Unlock()
	g.events()

	_, err := g.reconcileErr()
	if err == nil {
		t.Fatal("a failure of the engine was not tried again")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("the error that is logged carries the token: %v", err)
	}
	c := g.cond(v1alpha1.ConditionGatewayReady)
	if c.Reason != v1alpha1.ReasonReconcileFailed || strings.Contains(c.Message, token) || !strings.Contains(c.Message, "***") {
		t.Errorf("GatewayReady = %s (%s), want the failure with the token redacted", statusOf(c), c.Message)
	}
	for _, e := range g.events() {
		if strings.Contains(e, token) {
			t.Errorf("an event carries the token: %q", e)
		}
	}
}

func statusOf(c metav1.Condition) string { return string(c.Status) + "/" + c.Reason }

// What the operator made itself, the credentials the engine minted and the key it
// generated, is as secret as what it was given. A client that refuses to write
// the Secret and says what it was given is a failure the tenant must not read it
// in.
func TestWhatTheOperatorMadeItselfIsKeptOutOfAFailureToo(t *testing.T) {
	names := gateway.NamesFor(name)
	for what, secretName := range map[string]string{
		"the credentials minted by the engine": names.APIKey,
		"the key generated for the tunnel":     names.PSK,
	} {
		t.Run(what, func(t *testing.T) {
			g := newRig(t)
			var leaked string
			g.writes.setFailCreate(func(o client.Object) error {
				s, ok := o.(*corev1.Secret)
				if !ok || s.Name != secretName {
					return nil
				}
				// The refusal repeats everything the Secret holds.
				for _, v := range s.Data {
					leaked = string(v)
				}
				return errors.New("the Secret was refused: " + leaked)
			})
			g.events()

			_, err := g.reconcileErr()
			if err == nil || leaked == "" {
				t.Fatalf("the failure was not tried again, or nothing was written (error %v, secret %q)", err, leaked)
			}
			c := g.cond(v1alpha1.ConditionGatewayReady)
			if c.Reason != v1alpha1.ReasonReconcileFailed || !strings.Contains(c.Message, "***") {
				t.Errorf("GatewayReady = %s (%s), want the failure with the secret redacted", statusOf(c), c.Message)
			}
			if strings.Contains(c.Message, leaked) || strings.Contains(err.Error(), leaked) {
				t.Errorf("the failure carries the secret: %s / %v", c.Message, err)
			}
			for _, e := range g.events() {
				if strings.Contains(e, leaked) {
					t.Errorf("an event carries the secret: %q", e)
				}
			}
		})
	}
}
