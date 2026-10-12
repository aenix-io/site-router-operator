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

// Package controller_test runs the reconcile loop against a real apiserver, with
// the engine and the connector as doubles and KubeVirt played by the test: what a
// pass makes of the objects it finds is only worth what the apiserver makes of
// the objects it writes.
package controller_test

import (
	"context"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/aenix-io/site-router-operator/api/engine"
	"github.com/aenix-io/site-router-operator/api/engine/enginetest/cmdfake"
	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/internal/config"
	"github.com/aenix-io/site-router-operator/internal/controller"
	"github.com/aenix-io/site-router-operator/internal/denyset"
	"github.com/aenix-io/site-router-operator/internal/forwarder/fakeconnector"
	"github.com/aenix-io/site-router-operator/internal/gateway"
	"github.com/aenix-io/site-router-operator/internal/testenv"
)

var k8sClient client.Client

func TestMain(m *testing.M) {
	// The reconciler logs through the logger of the context, which a test does not set.
	ctrl.SetLogger(zap.New(zap.WriteTo(io.Discard)))
	testenv.Main(m, func(c client.Client) { k8sClient = c }, "../gateway/crds")
}

var (
	// The clock of the tests starts at the time of the machine, since the apiserver
	// stamps what it makes with its own, and a disk is as old as that stamp says.
	epoch = time.Now().UTC().Truncate(time.Second)

	imageV1  = "registry.example/appliances/gateway-disk:1.0.0@sha256:" + strings.Repeat("a1", 32)
	imageV2  = "registry.example/appliances/gateway-disk:2.0.0@sha256:" + strings.Repeat("b2", 32)
	digestV1 = "sha256:" + strings.Repeat("a1", 32)
	digestV2 = "sha256:" + strings.Repeat("b2", 32)
)

const (
	// The address of the gateway pod, of the tunnel, and of the pods of the tenant.
	gatewayIP = "10.244.1.9"
	tunnelIP  = "198.51.100.1"
	webIP     = "10.244.1.5"

	name = "branch-office"
)

// testConfig is the configuration of an operator with two versions of the image,
// the second of them the latest.
func testConfig() config.Config {
	cfg := config.Default()
	cfg.ManagementCIDR = "10.244.0.0/16"
	// The service network of the apiserver of envtest.
	cfg.ClusterNetworks = denyset.Config{PodCIDR: "10.244.0.0/16", ServiceCIDR: "10.0.0.0/24"}
	cfg.Gateway.Isolation = gateway.IsolationConfig{Mode: gateway.IsolationExternal}
	cfg.Engine = config.Engine{
		Name:     "fake-cmd",
		Versions: []config.Version{{Name: "1.0", Image: imageV1}, {Name: "2.0", Image: imageV2}},
		Latest:   "2.0",
	}
	cfg.Upgrade.Policy = config.UpgradeAuto
	return cfg
}

// spy is an engine that records what it is asked, and whose guest says what the
// test has it say.
type spy struct {
	engine.Engine

	mu         sync.Mutex
	bootstraps []engine.BootstrapRequest
	// failBootstrap makes the engine refuse to make a seed, with the error it gives.
	failBootstrap func(engine.BootstrapRequest) error
	applied       []engine.Desired
	// prevs are the revisions the engine was told the guest had when it was asked to apply.
	prevs       []string
	endpoints   []engine.Endpoint
	forceGuard  *engine.GuardState
	failConfirm error
}

func (s *spy) Bootstrap(r engine.BootstrapRequest) (engine.BootstrapPlan, error) {
	s.mu.Lock()
	s.bootstraps = append(s.bootstraps, r)
	fail := s.failBootstrap
	s.mu.Unlock()
	if fail != nil {
		return engine.BootstrapPlan{}, fail(r)
	}
	return s.Engine.Bootstrap(r)
}

func (s *spy) bootstrapped() []engine.BootstrapRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.bootstraps)
}

func (s *spy) Connect(ep engine.Endpoint) (engine.Session, error) {
	s.mu.Lock()
	s.endpoints = append(s.endpoints, ep)
	s.mu.Unlock()
	sess, err := s.Engine.Connect(ep)
	return spySession{Session: sess, s: s}, err
}

type spySession struct {
	engine.Session
	s *spy
}

func (x spySession) Apply(ctx context.Context, d engine.Desired, prev string) (engine.Applied, error) {
	x.s.mu.Lock()
	x.s.prevs = append(x.s.prevs, prev)
	x.s.applied = append(x.s.applied, d)
	x.s.mu.Unlock()
	return x.Session.Apply(ctx, d, prev)
}

func (x spySession) Confirm(ctx context.Context) (engine.GuardReport, error) {
	x.s.mu.Lock()
	force, fail := x.s.forceGuard, x.s.failConfirm
	x.s.mu.Unlock()
	switch {
	case fail != nil:
		return engine.GuardReport{}, fail
	case force != nil:
		return engine.GuardReport{TunnelIngress: *force, Detail: "forced"}, nil
	}
	return x.Session.Confirm(ctx)
}

// last is the state the engine was last asked for, and how many times it was.
func (s *spy) last() (engine.Desired, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.applied) == 0 {
		return engine.Desired{}, 0
	}
	return s.applied[len(s.applied)-1], len(s.applied)
}

// revisions are the revisions the engine was told the guest had, in the order it was asked.
func (s *spy) revisions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.prevs)
}

func (s *spy) lastEndpoint() engine.Endpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.endpoints) == 0 {
		return engine.Endpoint{}
	}
	return s.endpoints[len(s.endpoints)-1]
}

// counting is a client that counts the writes of a status, which a pass that
// finds nothing different must not make, and that reads as the cache of the manager
// does.
type counting struct {
	client.Client
	statusWrites atomic.Int32

	mu sync.Mutex
	// failCreate makes the creation of an object fail with the error it returns,
	// or not fail when it returns nil.
	failCreate func(client.Object) error
}

func (c *counting) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.mu.Lock()
	fail := c.failCreate
	c.mu.Unlock()
	if fail != nil {
		if err := fail(obj); err != nil {
			return err
		}
	}
	return c.Client.Create(ctx, obj, opts...)
}

// Get reads as the cache of the manager does: a Forwarder that does not carry the
// label of the operator is not there. The operator's cache holds only what it made,
// and what it makes nothing of is not shown to it.
func (c *counting) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if _, ok := obj.(*v1alpha1.Forwarder); ok && obj.GetLabels()[v1alpha1.LabelManagedBy] != v1alpha1.ManagedByValue {
		return apierrors.NewNotFound(v1alpha1.GroupVersion.WithResource("forwarders").GroupResource(), key.Name)
	}
	return nil
}

func (c *counting) setFailCreate(f func(client.Object) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failCreate = f
}

func (c *counting) Status() client.SubResourceWriter {
	return countingStatus{SubResourceWriter: c.Client.Status(), n: &c.statusWrites}
}

type countingStatus struct {
	client.SubResourceWriter
	n *atomic.Int32
}

func (c countingStatus) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	c.n.Add(1)
	return c.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

// flaky is a reader of the cluster that can be made to fail.
type flaky struct {
	client.Reader
	mu   sync.Mutex
	down bool
}

func (f *flaky) set(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

func (f *flaky) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	f.mu.Lock()
	down := f.down
	f.mu.Unlock()
	if down {
		return &downError{}
	}
	return f.Reader.List(ctx, list, opts...)
}

type downError struct{}

func (*downError) Error() string {
	return "Get https://10.96.0.1:443/api/v1/nodes: connection refused"
}

// rig is a namespace with a SiteRouter in it and a reconciler for it, with a
// clock that the test moves.
type rig struct {
	t    *testing.T
	ns   string
	r    *controller.Reconciler
	spy  *spy
	dev  *cmdfake.Device
	conn *fakeconnector.Connector
	rec  *events.FakeRecorder
	reg  *prometheus.Registry
	disc *flaky
	now  time.Time
	cfg  config.Config
	// name is the SiteRouter the rig acts on, which is the one it made unless it is a sibling.
	name string
	// writes is the client of the reconciler, which counts what it writes.
	writes *counting
	// vmiDisk is the disk the next instance boots from when it is not the one the VM
	// says, as it is when the VM was pointed at another disk and the instance was
	// not made again.
	vmiDisk string
}

// newRig makes a namespace with a SiteRouter, which has a peer and one remote
// network, and a reconciler. The mutations change the configuration and the
// SiteRouter before either is used.
func newRig(t *testing.T, mutate ...func(*config.Config, *v1alpha1.SiteRouter)) *rig {
	t.Helper()
	ns := &corev1.Namespace{GenerateName: "ctl-"}
	mustCreate(t, ns)
	// Nothing makes the default ServiceAccount of a namespace here, and a pod
	// cannot be made without it.
	mustCreate(t, &corev1.ServiceAccount{Name: "default", Namespace: ns.Name})

	cfg := testConfig()
	sr := &v1alpha1.SiteRouter{
		Name:      name,
		Namespace: ns.Name,
		Spec: v1alpha1.SiteRouterSpec{
			Peer:        v1alpha1.Peer{Address: "203.0.113.10"},
			RemoteCIDRs: []v1alpha1.IPv4CIDR{"192.0.2.0/24"},
		},
	}
	for _, m := range mutate {
		m(&cfg, sr)
	}
	mustCreate(t, sr)

	g := &rig{t: t, ns: ns.Name, name: name, now: epoch, dev: cmdfake.NewDevice(), rec: events.NewFakeRecorder(1000)}
	g.spy = &spy{Engine: cmdfake.Engine{Dev: g.dev}}
	g.conn = &fakeconnector.Connector{Client: k8sClient}
	g.disc = &flaky{Reader: k8sClient}
	g.build(cfg)
	t.Cleanup(g.forgetSiteRouters)
	return g
}

// forgetSiteRouters deletes the SiteRouters of the namespace without the operator,
// which is not there to let them go. The apiserver of the tests is shared by every
// test, and a SiteRouter that was left moving would be read by the next operator as
// a place that is taken.
func (g *rig) forgetSiteRouters() {
	ctx := context.Background()
	list := &v1alpha1.SiteRouterList{}
	if err := k8sClient.List(ctx, list, client.InNamespace(g.ns)); err != nil {
		return
	}
	for i := range list.Items {
		sr := &list.Items[i]
		sr.Finalizers = nil
		_ = k8sClient.Update(ctx, sr)
		_ = k8sClient.Delete(ctx, sr)
	}
}

// build makes the reconciler for a configuration.
func (g *rig) build(cfg config.Config) {
	g.t.Helper()
	clock := func() time.Time { return g.now }
	source, err := denyset.NewSource(cfg.ClusterNetworks, denyset.NewDiscoverer(g.disc, cfg.Timing.Discovery.Duration, clock))
	if err != nil {
		g.t.Fatal(err)
	}
	g.cfg = cfg
	// A registry of its own: the series of an operator that started again are those
	// of the new one.
	g.reg = prometheus.NewRegistry()
	g.writes = &counting{Client: k8sClient}
	g.r, err = controller.New(controller.Options{
		Client:   g.writes,
		Reader:   k8sClient,
		Recorder: g.rec,
		Config:   cfg,
		Engine:   g.spy,
		Networks: source,
		Metrics:  controller.NewMetrics(g.reg),
		Now:      clock,
	})
	if err != nil {
		g.t.Fatal(err)
	}
}

// restart is the operator starting again with another configuration, with
// nothing of what the last one kept in memory.
func (g *rig) restart(mutate func(*config.Config)) {
	g.t.Helper()
	cfg := g.cfg
	mutate(&cfg)
	g.build(cfg)
}

func mustCreate(t *testing.T, o client.Object) {
	t.Helper()
	if err := k8sClient.Create(t.Context(), o); err != nil {
		t.Fatal(err)
	}
}

func (g *rig) key() client.ObjectKey { return client.ObjectKey{Namespace: g.ns, Name: g.name} }

func (g *rig) names() gateway.Names { return gateway.NamesFor(g.name) }

// reconcileErr runs a pass, and returns what it returns.
func (g *rig) reconcileErr() (ctrl.Result, error) {
	g.t.Helper()
	return g.r.Reconcile(g.t.Context(), ctrl.Request{NamespacedName: g.key()})
}

// reconcile runs a pass that is expected not to fail.
func (g *rig) reconcile() ctrl.Result {
	g.t.Helper()
	res, err := g.reconcileErr()
	if err != nil {
		g.t.Fatalf("Reconcile: %v", err)
	}
	return res
}

// sr returns the SiteRouter as it is in the cluster, or nil when it is gone.
func (g *rig) sr() *v1alpha1.SiteRouter {
	g.t.Helper()
	sr := &v1alpha1.SiteRouter{}
	if err := k8sClient.Get(g.t.Context(), g.key(), sr); err != nil {
		if client.IgnoreNotFound(err) != nil {
			g.t.Fatal(err)
		}
		return nil
	}
	return sr
}

// edit changes the SiteRouter.
func (g *rig) edit(mutate func(*v1alpha1.SiteRouter)) {
	g.t.Helper()
	sr := g.sr()
	mutate(sr)
	if err := k8sClient.Update(g.t.Context(), sr); err != nil {
		g.t.Fatal(err)
	}
}

// cond returns a condition of the SiteRouter, which must be there.
func (g *rig) cond(typ string) metav1.Condition {
	g.t.Helper()
	sr := g.sr()
	if sr == nil {
		g.t.Fatal("the SiteRouter is gone")
	}
	c := meta.FindStatusCondition(sr.Status.Conditions, typ)
	if c == nil {
		g.t.Fatalf("no %s condition among %v", typ, sr.Status.Conditions)
	}
	return *c
}

// expect fails the test unless the condition has the status and the reason.
func (g *rig) expect(typ string, status metav1.ConditionStatus, reason string) {
	g.t.Helper()
	if c := g.cond(typ); c.Status != status || c.Reason != reason {
		g.t.Errorf("%s = %s/%s (%s), want %s/%s", typ, c.Status, c.Reason, c.Message, status, reason)
	}
}

// pod makes a pod with an address.
func (g *rig) pod(podName string, labels map[string]string, ip string) {
	g.t.Helper()
	p := &corev1.Pod{
		Name:      podName,
		Namespace: g.ns,
		Labels:    labels,
		Spec:      corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "registry.example/app"}}},
	}
	mustCreate(g.t, p)
	p.Status.PodIP = ip
	if err := k8sClient.Status().Update(g.t.Context(), p); err != nil {
		g.t.Fatal(err)
	}
}

// tenantPod makes a pod of the tenant.
func (g *rig) tenantPod() {
	g.t.Helper()
	if err := k8sClient.Get(g.t.Context(), client.ObjectKey{Namespace: g.ns, Name: "web-0"}, &corev1.Pod{}); err == nil {
		return
	}
	g.pod("web-0", map[string]string{"app": "web"}, webIP)
}

// boot plays KubeVirt and the load balancer for the gateway the controller made:
// the instance of the VM is running, its pod has an address, and the Service of
// the tunnel has one too.
func (g *rig) boot() { g.bootAs("Running", true) }

// bootAs is boot with the phase the instance is in, and with the load balancer
// giving the Service an address or not.
func (g *rig) bootAs(phase string, address bool) { g.bootOwnedBy(phase, address, "") }

// bootOwnedBy is bootAs with the UID the instance names as the VM that controls it:
// that of the VM when it is empty, and what is given when it is not, as one
// that someone else made would.
func (g *rig) bootOwnedBy(phase string, address bool, ownerUID types.UID) {
	g.t.Helper()
	ctx := g.t.Context()

	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(gateway.VirtualMachineGVK)
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: g.ns, Name: g.names().VM}, vm); err != nil {
		g.t.Fatalf("the VM of the gateway: %v", err)
	}
	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	if g.vmiDisk != "" {
		for _, v := range volumes {
			if m, ok := v.(map[string]any); ok && m["name"] == "boot" {
				m["dataVolume"] = map[string]any{"name": g.vmiDisk}
			}
		}
	}

	yes := true
	if ownerUID == "" {
		ownerUID = vm.GetUID()
	}
	vmi := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"volumes": volumes}}}
	vmi.SetGroupVersionKind(gateway.VirtualMachineInstanceGVK)
	vmi.SetName(g.names().VM)
	vmi.SetNamespace(g.ns)
	vmi.SetLabels(gateway.Labels(g.name))
	vmi.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "kubevirt.io/v1", Kind: "VirtualMachine", Name: vm.GetName(), UID: ownerUID, Controller: &yes,
	}})
	if err := k8sClient.Create(ctx, vmi); err != nil {
		g.t.Fatal(err)
	}
	if err := unstructured.SetNestedField(vmi.Object, phase, "status", "phase"); err != nil {
		g.t.Fatal(err)
	}
	if err := unstructured.SetNestedSlice(vmi.Object, []any{
		map[string]any{"name": "default", "ipAddress": gatewayIP, "mac": "02:00:00:00:00:01"},
	}, "status", "interfaces"); err != nil {
		g.t.Fatal(err)
	}
	if err := k8sClient.Status().Update(ctx, vmi); err != nil {
		g.t.Fatal(err)
	}

	g.pod("virt-launcher-"+g.names().VM, gateway.Labels(g.name), gatewayIP)

	if !address {
		return
	}
	svc := &corev1.Service{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: g.ns, Name: g.names().Tunnel}, svc); err != nil {
		g.t.Fatalf("the Service of the tunnel: %v", err)
	}
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: tunnelIP}}
	if err := k8sClient.Status().Update(ctx, svc); err != nil {
		g.t.Fatal(err)
	}
}

// sync has the connector act on the Forwarders.
func (g *rig) sync() {
	g.t.Helper()
	if err := g.conn.Sync(g.t.Context()); err != nil {
		g.t.Fatal(err)
	}
}

// forwarder returns the Forwarder of the SiteRouter, or nil when there is none.
func (g *rig) forwarder() *v1alpha1.Forwarder {
	g.t.Helper()
	f := &v1alpha1.Forwarder{}
	err := k8sClient.Get(g.t.Context(), client.ObjectKey{Namespace: g.ns, Name: g.names().Forwarder}, f)
	if client.IgnoreNotFound(err) != nil {
		g.t.Fatal(err)
	}
	if err != nil {
		return nil
	}
	return f
}

// asked is the networks the Forwarder asks the connector for.
func (g *rig) asked() []string {
	g.t.Helper()
	f := g.forwarder()
	if f == nil {
		g.t.Fatal("no Forwarder")
	}
	out := make([]string, len(f.Spec.CIDRs))
	for i, c := range f.Spec.CIDRs {
		out[i] = string(c)
	}
	return out
}

// ready brings the SiteRouter to where it is Ready: the gateway runs, the
// connector has acted and the routes are in.
func (g *rig) ready() {
	g.t.Helper()
	g.tenantPod()
	g.reconcile()
	// The connector reports the pods of the tenant while the VM is still starting,
	// as it does in a cluster, where the gateway takes minutes to boot.
	g.sync()
	g.boot()
	g.sync()
	g.reconcile()
	g.sync()
	g.reconcile()
	g.expect(v1alpha1.ConditionReady, metav1.ConditionTrue, "AllConditionsMet")
}

// events drains what the recorder has been told.
func (g *rig) events() []string {
	var out []string
	for {
		select {
		case e := <-g.rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// sibling is another SiteRouter in the same namespace, which the same reconciler
// looks after. Its networks are its own, since the connector gives a network to
// the first Forwarder that asks for it.
func (g *rig) sibling(siblingName, remote string) *rig {
	g.t.Helper()
	sr := &v1alpha1.SiteRouter{
		Name:      siblingName,
		Namespace: g.ns,
		Spec: v1alpha1.SiteRouterSpec{
			Peer:        v1alpha1.Peer{Address: "203.0.113.20"},
			RemoteCIDRs: []v1alpha1.IPv4CIDR{v1alpha1.IPv4CIDR(remote)},
		},
	}
	mustCreate(g.t, sr)
	s := *g
	s.name = siblingName
	return &s
}

// setDV says what phase the importer has a disk in.
func (g *rig) setDV(diskName, phase string) {
	g.t.Helper()
	dv := &unstructured.Unstructured{}
	dv.SetGroupVersionKind(gateway.DataVolumeGVK)
	if err := k8sClient.Get(g.t.Context(), client.ObjectKey{Namespace: g.ns, Name: diskName}, dv); err != nil {
		g.t.Fatalf("the disk %s: %v", diskName, err)
	}
	if err := unstructured.SetNestedField(dv.Object, phase, "status", "phase"); err != nil {
		g.t.Fatal(err)
	}
	if err := k8sClient.Status().Update(g.t.Context(), dv); err != nil {
		g.t.Fatal(err)
	}
}

// exists says whether an object of a kind with no Go type is in the cluster.
func (g *rig) exists(gvk schema.GroupVersionKind, objectName string) bool {
	g.t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	err := k8sClient.Get(g.t.Context(), client.ObjectKey{Namespace: g.ns, Name: objectName}, u)
	if client.IgnoreNotFound(err) != nil {
		g.t.Fatal(err)
	}
	return err == nil
}

// secretExists says whether a Secret is in the cluster.
func (g *rig) secretExists(secretName string) bool {
	g.t.Helper()
	err := k8sClient.Get(g.t.Context(), client.ObjectKey{Namespace: g.ns, Name: secretName}, &corev1.Secret{})
	if client.IgnoreNotFound(err) != nil {
		g.t.Fatal(err)
	}
	return err == nil
}

// bootsFrom is the disk the VM is pointed at.
func (g *rig) bootsFrom() string {
	g.t.Helper()
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(gateway.VirtualMachineGVK)
	if err := k8sClient.Get(g.t.Context(), client.ObjectKey{Namespace: g.ns, Name: g.names().VM}, vm); err != nil {
		g.t.Fatal(err)
	}
	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	for _, v := range volumes {
		if m, ok := v.(map[string]any); ok && m["name"] == "boot" {
			n, _, _ := unstructured.NestedString(m, "dataVolume", "name")
			return n
		}
	}
	return ""
}

// reboot plays KubeVirt booting the VM again, after the operator has deleted its
// instance: the instance and its pod are made anew from what the VM says.
func (g *rig) reboot() {
	g.t.Helper()
	pod := &corev1.Pod{Name: "virt-launcher-" + g.names().VM, Namespace: g.ns}
	if err := client.IgnoreNotFound(k8sClient.Delete(g.t.Context(), pod)); err != nil {
		g.t.Fatal(err)
	}
	g.bootAs("Running", false)
}

// upgrades reads how many moves of a gateway the operator has counted with a result.
func (g *rig) upgrades(result string) float64 {
	g.t.Helper()
	families, err := g.reg.Gather()
	if err != nil {
		g.t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "site_router_upgrades_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "result" && l.GetValue() == result {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// diskOf names the boot disk and the seed of the image of a version.
func diskOf(siteRouter, image string) (disk, seed string) {
	n := gateway.NamesFor(siteRouter).ForImage(image)
	return n.BootDisk, n.CloudInit
}

// follow has a sibling use the reconciler that the rig has now, which is another
// after the operator has started again.
func (g *rig) follow(of *rig) { g.r, g.reg = of.r, of.reg }

// wipeStatus forgets what the SiteRouter remembers of the gateway, as a status
// that was lost would.
func (g *rig) wipeStatus() {
	g.t.Helper()
	sr := g.sr()
	sr.Status = v1alpha1.SiteRouterStatus{}
	if err := k8sClient.Status().Update(g.t.Context(), sr); err != nil {
		g.t.Fatal(err)
	}
}

// deleteObject deletes an object of a kind that has no Go type, if it is there.
func (g *rig) deleteObject(gvk schema.GroupVersionKind, objectName string) {
	g.t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName(objectName)
	u.SetNamespace(g.ns)
	if err := client.IgnoreNotFound(k8sClient.Delete(g.t.Context(), u)); err != nil {
		g.t.Fatal(err)
	}
}
