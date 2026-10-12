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

package forwarder_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aenix-io/site-router-operator/api/engine"
	"github.com/aenix-io/site-router-operator/api/engine/enginetest/cmdfake"
	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/internal/denyset"
	"github.com/aenix-io/site-router-operator/internal/forwarder/fakeconnector"
	"github.com/aenix-io/site-router-operator/internal/gateway"
	"github.com/aenix-io/site-router-operator/internal/reconcile"
)

const psk = "s3cr3t-psk-of-the-tunnel"

var remote = []string{"192.0.2.0/24"}

// flaky is a reader of the cluster that can be made to fail.
type flaky struct {
	client.Reader
	down atomic.Bool
}

func (f *flaky) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if f.down.Load() {
		return errors.New("Get https://10.96.0.1:443/api/v1/nodes: connection refused, token " + psk)
	}
	return f.Reader.List(ctx, list, opts...)
}

// guarded is an engine whose guest says what the test has the site force.
type guarded struct {
	engine.Engine
	s *site
}

func (g guarded) Connect(ep engine.Endpoint) (engine.Session, error) {
	sess, err := g.Engine.Connect(ep)
	return guardedSession{sess, g.s}, err
}

type guardedSession struct {
	engine.Session
	s *site
}

func (x guardedSession) Confirm(ctx context.Context) (engine.GuardReport, error) {
	switch {
	case x.s.failConfirm != nil:
		return engine.GuardReport{}, x.s.failConfirm
	case x.s.forceGuard != nil:
		return engine.GuardReport{TunnelIngress: *x.s.forceGuard, Detail: "forced"}, nil
	}
	return x.Session.Confirm(ctx)
}

// site is a gateway with the whole operator side around it: the deny set, the
// Forwarder, a connector double and an engine, over a real apiserver.
type site struct {
	*world
	reader  *flaky
	conn    *fakeconnector.Connector
	stepper *reconcile.Stepper
	state   reconcile.State

	// What the guest says when asked to confirm its source filter: this, when
	// set, or that it cannot say, when failConfirm is.
	forceGuard  *engine.GuardState
	failConfirm error
}

func newSite(t *testing.T, w *world, connectorAtPodCreation bool) *site {
	t.Helper()
	s := &site{world: w, reader: &flaky{Reader: k8sClient}}
	s.conn = &fakeconnector.Connector{Client: k8sClient, RoutesAtPodCreation: connectorAtPodCreation}

	discoverer := denyset.NewDiscoverer(s.reader, time.Minute, func() time.Time { return w.now })
	validate, err := denyset.NewValidator(denyset.Config{PodCIDR: "10.244.0.0/16", ServiceCIDR: "10.96.0.0/16"}, discoverer)
	if err != nil {
		t.Fatal(err)
	}
	s.stepper, err = reconcile.New(guarded{cmdfake.Engine{Dev: cmdfake.NewDevice()}, s}, validate)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (w *world) pod(name string, labels map[string]string, ip string, opts ...func(*corev1.Pod)) {
	w.t.Helper()
	p := &corev1.Pod{
		Name:      name,
		Namespace: w.ns,
		Labels:    labels,
		Spec:      corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "registry.example/app"}}},
	}
	for _, o := range opts {
		o(p)
	}
	// Creating a pod drops its status, so it is kept and written after.
	status := p.Status
	status.PodIP = ip
	mustCreate(w.t, p)
	p.Status = status
	if err := k8sClient.Status().Update(w.t.Context(), p); err != nil {
		w.t.Fatal(err)
	}
}

// gatewayPod makes the pod of the gateway of a SiteRouter, which carries the
// label of the operator.
func (w *world) gatewayPod(instance, ip string) {
	w.pod("gw-"+instance, gateway.Labels(instance), ip)
}

func (s *site) forwarderObject() *v1alpha1.Forwarder {
	s.t.Helper()
	f := &v1alpha1.Forwarder{}
	err := k8sClient.Get(s.t.Context(), client.ObjectKey{Namespace: s.ns, Name: gateway.NamesFor(s.sr.Name).Forwarder}, f)
	if client.IgnoreNotFound(err) != nil {
		s.t.Fatal(err)
	}
	if err != nil {
		return nil
	}
	return f
}

// pass is one pass of the reconciler for the gateway, with the connector
// acting before it when sync is true: what a controller does when it is woken
// by the Forwarder, which the connector has just written.
func (s *site) pass(sync bool, remoteCIDRs []string) reconcile.Result {
	s.t.Helper()
	routes := s.m.For(s.sr)
	if err := routes.Ensure(s.t.Context()); err != nil {
		s.t.Fatal(err)
	}
	if sync {
		if err := s.conn.Sync(s.t.Context()); err != nil {
			s.t.Fatal(err)
		}
	}
	rep, err := routes.Report(s.t.Context())
	if err != nil {
		s.t.Fatal(err)
	}
	dests := make([]string, len(rep.Scope))
	for i, a := range rep.Scope {
		dests[i] = a + "/32"
	}
	in := reconcile.Instance{
		Name:        s.sr.Name,
		RemoteCIDRs: remoteCIDRs,
		Desired: engine.Desired{
			Tunnels: []engine.Tunnel{{ID: "t1", PeerAddress: "203.0.113.10", PSK: psk, LocalID: "198.51.100.1", RemoteSelectors: remoteCIDRs}},
			Guards:  engine.Guards{TunnelIngress: engine.TunnelIngress{Sources: remoteCIDRs, Destinations: dests}},
		},
		Routes: routes,
	}
	ep := engine.Endpoint{Address: "10.244.1.9", Credentials: map[string][]byte{"token": []byte("api-token")}}
	return s.stepper.Step(s.t.Context(), ep, in, &s.state)
}

func (s *site) grantedRoutes() []string {
	f := s.forwarderObject()
	if f == nil {
		return nil
	}
	return s.conn.Granted(f.UID).Routes
}

func cond(t *testing.T, res reconcile.Result, typ string) reconcile.Condition {
	t.Helper()
	c, ok := res.Conditions[typ]
	if !ok {
		t.Fatalf("no %s condition among %v", typ, res.Conditions)
	}
	return c
}

// readyWorld is a namespace with a gateway pod and two pods of the tenant, and
// the gateway routed once.
func readyWorld(t *testing.T, atPodCreation bool) *site {
	t.Helper()
	w := newWorld(t)
	w.gatewayPod(w.sr.Name, "10.244.1.9")
	w.pod("web-0", map[string]string{"app": "web"}, "10.244.1.5")
	w.pod("web-1", map[string]string{"app": "web"}, "10.244.1.6")
	return newSite(t, w, atPodCreation)
}

func TestAGatewayIsRoutedOnceTheFilterIsConfirmed(t *testing.T) {
	s := readyWorld(t, false)

	// The first pass asks for the routes. The connector has not seen the request,
	// so they are not in yet, and that is not a failure.
	res := s.pass(true, remote)
	if c := cond(t, res, reconcile.CondValidated); !c.OK {
		t.Errorf("Validated = %+v", c)
	}
	if c := cond(t, res, reconcile.CondConfigured); !c.OK {
		t.Errorf("Configured = %+v", c)
	}
	if c := cond(t, res, reconcile.CondRoutesProgrammed); c.OK || c.Reason != v1alpha1.ReasonRoutesPending {
		t.Errorf("RoutesProgrammed = %+v on the first pass, want RoutesPending", c)
	}

	// The connector acts, and the next pass says so.
	res = s.pass(true, remote)
	if c := cond(t, res, reconcile.CondRoutesProgrammed); !c.OK || c.Reason != reconcile.ReasonProgrammed {
		t.Errorf("RoutesProgrammed = %+v, want Programmed", c)
	}
	if got := s.grantedRoutes(); !slices.Equal(got, remote) {
		t.Errorf("the network routes %v, want %v", got, remote)
	}
	g := s.conn.Granted(s.forwarderObject().UID)
	if !slices.Equal(g.Gateways, []string{"10.244.1.9"}) {
		t.Errorf("forwarding is granted to %v, want the pod of the gateway alone", g.Gateways)
	}
}

// The destinations of the filter are what the gateway lets decrypted traffic
// through to, so an address that is not the tenant's to reach must not be there.
func TestTheScopeLeavesOutWhatIsNotTheTenantsToReach(t *testing.T) {
	s := readyWorld(t, false)
	s.pod("job-0", map[string]string{"app": "job"}, "10.244.1.20", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded })
	s.pod("crashed-0", map[string]string{"app": "job"}, "10.244.1.21", func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed })
	s.pod("host-0", map[string]string{"app": "agent"}, "192.168.100.5", func(p *corev1.Pod) { p.Spec.HostNetwork = true })
	svc := &corev1.Service{
		Name: "web", Namespace: s.ns,
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}, Selector: map[string]string{"app": "web"}},
	}
	mustCreate(t, svc)
	headless := &corev1.Service{
		Name: "headless", Namespace: s.ns,
		Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone, Ports: []corev1.ServicePort{{Port: 80}}},
	}
	mustCreate(t, headless)

	s.pass(true, remote)
	rep, err := s.m.For(s.sr).Report(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.244.1.5", "10.244.1.6", svc.Spec.ClusterIP}
	slices.Sort(want)
	if !slices.Equal(rep.Scope, want) {
		t.Errorf("scope = %v, want the two pods of the tenant and the cluster IP of the Service: %v", rep.Scope, want)
	}
}

func TestTheDestinationsOfTheFilterAreThePodsOfTheTenant(t *testing.T) {
	s := readyWorld(t, false)
	// The gateway itself, and the gateway of another SiteRouter, are not pods of
	// the tenant, and the filter must not let traffic through to them.
	s.gatewayPod("other", "10.244.1.99")
	s.pass(true, remote)

	rep, err := s.m.For(s.sr).Report(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Scope, []string{"10.244.1.5", "10.244.1.6"}) {
		t.Errorf("scope = %v, want the two pods of the tenant", rep.Scope)
	}
	if !slices.Equal(rep.Gateway, []string{"10.244.1.9"}) {
		t.Errorf("gateway = %v, want this gateway alone", rep.Gateway)
	}
}

func TestWhenTheConnectorStopsAnsweringTheTenantIsToldWhy(t *testing.T) {
	s := readyWorld(t, false)
	// The connector served the Forwarder once, so the addresses of the tenant are
	// known, and then it is gone: nothing answers any request that follows.
	s.pass(true, remote)
	s.pass(true, remote)

	changed := []string{"198.51.100.0/24"}
	res := s.pass(false, changed)
	if c := cond(t, res, reconcile.CondRoutesProgrammed); c.OK || c.Reason != v1alpha1.ReasonRoutesPending {
		t.Fatalf("RoutesProgrammed = %+v early on, want RoutesPending", c)
	}

	s.now = s.now.Add(timeout + time.Second)
	res = s.pass(false, changed)
	c := cond(t, res, reconcile.CondRoutesProgrammed)
	if c.OK || c.Reason != v1alpha1.ReasonForwarderNotResponding {
		t.Errorf("RoutesProgrammed = %+v, want ForwarderNotResponding", c)
	}
	if !strings.Contains(c.Message, "connector") {
		t.Errorf("the message does not say what to look at: %q", c.Message)
	}
	// The gateway itself is configured all the same.
	if c := cond(t, res, reconcile.CondConfigured); !c.OK {
		t.Errorf("Configured = %+v: a missing connector is not a failure of the gateway", c)
	}
}

func TestARemoteNetworkInsideThePodNetworkIsRefusedAndTheRoutesAreTakenAway(t *testing.T) {
	s := readyWorld(t, false)
	s.pass(true, remote)
	s.pass(true, remote)
	if len(s.grantedRoutes()) == 0 {
		t.Fatal("the routes were not in before the instance went wrong")
	}

	// The tenant changes the remote network to one inside the pod network.
	res := s.pass(true, []string{"10.244.5.0/24"})
	c := cond(t, res, reconcile.CondValidated)
	if c.OK || c.Reason != reconcile.ReasonInvalidRemoteCIDR || !strings.Contains(c.Message, "pod network") {
		t.Errorf("Validated = %+v, want a refusal that names the pod network", c)
	}
	if _, ok := res.Conditions[reconcile.CondConfigured]; ok {
		t.Error("the gateway was configured for an instance that was refused")
	}

	// And the connector is told to take the routes back.
	if err := s.conn.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := s.grantedRoutes(); len(got) != 0 {
		t.Errorf("the network still routes %v after the instance was refused", got)
	}
}

func TestANodeOrALoadBalancerOfTheClusterIsRefusedWithoutSayingWhichOne(t *testing.T) {
	cases := []struct {
		name    string
		make    func(t *testing.T, w *world)
		address string
		network string
	}{
		{"a node", func(t *testing.T, w *world) {
			n := &corev1.Node{Name: "fw-node-" + w.ns}
			mustCreate(t, n)
			t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), n) })
			n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "203.0.113.50"}}
			if err := k8sClient.Status().Update(t.Context(), n); err != nil {
				t.Fatal(err)
			}
		}, "203.0.113.50", "203.0.113.0/24"},
		{"a load balancer", func(t *testing.T, w *world) {
			svc := &corev1.Service{
				Name: "lb", Namespace: w.ns,
				Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, Ports: []corev1.ServicePort{{Port: 80}}},
			}
			mustCreate(t, svc)
			svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "198.51.100.77"}}
			if err := k8sClient.Status().Update(t.Context(), svc); err != nil {
				t.Fatal(err)
			}
		}, "198.51.100.77", "198.51.100.0/24"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := readyWorld(t, false)
			c.make(t, s.world)

			res := s.pass(true, []string{c.network})
			v := cond(t, res, reconcile.CondValidated)
			if v.OK || v.Reason != reconcile.ReasonInvalidRemoteCIDR {
				t.Fatalf("Validated = %+v, want a refusal", v)
			}
			if strings.Contains(v.Message, c.address) {
				t.Errorf("the refusal carries the address of %s: %s", c.name, v.Message)
			}
		})
	}
}

func TestWhenTheClusterCannotBeListedTheRoutesStayAsTheyAre(t *testing.T) {
	s := readyWorld(t, false)
	s.pass(true, remote)
	s.pass(true, remote)
	if len(s.grantedRoutes()) == 0 {
		t.Fatal("the routes were not in before the cluster stopped answering")
	}

	// A minute later what was listed is too old to use, and the cluster does not
	// answer.
	s.now = s.now.Add(2 * time.Minute)
	s.reader.down.Store(true)
	res := s.pass(true, remote)

	v := cond(t, res, reconcile.CondValidated)
	if v.OK || v.Reason != reconcile.ReasonValidationUnavailable {
		t.Errorf("Validated = %+v, want ValidationUnavailable", v)
	}
	if strings.Contains(v.Message, "10.96.0.1") || strings.Contains(v.Message, psk) {
		t.Errorf("the condition carries what the API said: %q", v.Message)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "connection refused") {
		t.Errorf("Err = %v, want the cause for the log", res.Err)
	} else if strings.Contains(res.Err.Error(), psk) {
		t.Errorf("Err leaks a secret: %v", res.Err)
	}
	if got := s.grantedRoutes(); !slices.Equal(got, remote) {
		t.Errorf("the network routes %v, want %v kept: a failure to look is no reason to break the tunnel", got, remote)
	}

	// The cluster answers again, and the instance is judged as before.
	s.reader.down.Store(false)
	res = s.pass(true, remote)
	if v := cond(t, res, reconcile.CondValidated); !v.OK {
		t.Errorf("Validated = %+v after the cluster answered again", v)
	}
}

func TestTwoGatewaysThatClaimTheSameNetworkConflict(t *testing.T) {
	first := readyWorld(t, false)
	w := first.world

	other := &v1alpha1.SiteRouter{Name: "other", Namespace: w.ns, Spec: v1alpha1.SiteRouterSpec{Peer: v1alpha1.Peer{Address: "203.0.113.20"}}}
	mustCreate(t, other)
	w.gatewayPod("other", "10.244.1.99")
	second := newSite(t, &world{t: t, ns: w.ns, sr: other, now: w.now, m: w.m}, false)
	second.conn = first.conn

	first.pass(true, remote)
	first.pass(true, remote)
	second.pass(true, remote)
	second.pass(true, []string{"192.0.2.0/25"})
	res := second.pass(true, []string{"192.0.2.0/25"})

	c := cond(t, res, reconcile.CondRoutesProgrammed)
	if c.OK || c.Reason != v1alpha1.ReasonRouteConflict {
		t.Errorf("RoutesProgrammed of the second gateway = %+v, want RouteConflict", c)
	}
	if got := first.pass(true, remote); !cond(t, got, reconcile.CondRoutesProgrammed).OK {
		t.Error("the first gateway lost its routes to a neighbor that came later")
	}
}

func TestWorkloadsThatExistedBeforeTheRoutesAreToldToRestart(t *testing.T) {
	s := readyWorld(t, true)
	s.pass(true, remote)
	res := s.pass(true, remote)

	c := cond(t, res, reconcile.CondRoutesProgrammed)
	if c.OK || c.Reason != v1alpha1.ReasonPendingWorkloads {
		t.Fatalf("RoutesProgrammed = %+v, want PendingWorkloads", c)
	}
	if !strings.Contains(c.Message, "web-0") || !strings.Contains(c.Message, "web-1") {
		t.Errorf("the message does not name the workloads: %q", c.Message)
	}

	// A pod made after the routes got them with it, and is not left behind.
	s.pod("web-2", map[string]string{"app": "web"}, "10.244.1.7")
	res = s.pass(true, remote)
	if c := cond(t, res, reconcile.CondRoutesProgrammed); strings.Contains(c.Message, "web-2") {
		t.Errorf("a pod that was made after the routes is reported as left behind: %q", c.Message)
	}
}

func TestWhenTheSourceFilterIsDownTheRoutesAreWithdrawnAndWhenItIsUnknownTheyStay(t *testing.T) {
	s := readyWorld(t, false)
	s.pass(true, remote)
	s.pass(true, remote)
	if len(s.grantedRoutes()) == 0 {
		t.Fatal("the routes were not in before the filter went down")
	}

	// The engine cannot say: the routes stay.
	s.failConfirm = errors.New("the management API does not answer")
	res := s.pass(true, remote)
	if c := cond(t, res, reconcile.CondConfigured); c.OK || c.Reason != reconcile.ReasonSourceFilterPending {
		t.Errorf("Configured = %+v, want SourceFilterPending", c)
	}
	if got := s.grantedRoutes(); !slices.Equal(got, remote) {
		t.Errorf("the network routes %v when the filter is unknown, want %v kept", got, remote)
	}

	// The engine says the filter is down: the routes go.
	s.failConfirm = nil
	down := engine.GuardDown
	s.forceGuard = &down
	res = s.pass(true, remote)
	if c := cond(t, res, reconcile.CondRoutesProgrammed); c.OK || c.Reason != v1alpha1.ReasonSourceFilterPending {
		t.Errorf("RoutesProgrammed = %+v, want SourceFilterPending", c)
	}
	if err := s.conn.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := s.grantedRoutes(); len(got) != 0 {
		t.Errorf("the network still routes %v with the filter down", got)
	}
}
