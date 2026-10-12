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
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/internal/config"
	"github.com/aenix-io/site-router-operator/internal/controller"
	"github.com/aenix-io/site-router-operator/internal/denyset"
	"github.com/aenix-io/site-router-operator/internal/gateway"
	"github.com/aenix-io/site-router-operator/internal/testchart"
	"github.com/aenix-io/site-router-operator/internal/testenv"
)

// asTheOperator installs the rights the chart gives the operator, for a release in
// a namespace of its own, and returns the configuration of a client that acts as
// the service account of the chart, and the namespace. The apiserver of the tests
// enforces RBAC, so what the chart does not grant is refused as it is in a
// cluster.
func asTheOperator(t *testing.T) (*rest.Config, string) {
	t.Helper()
	ns := &corev1.Namespace{GenerateName: "operator-"}
	mustCreate(t, ns)
	// The release is named after its namespace, so that the cluster-wide objects of
	// one test are not those of another.
	objs := testchart.Render(t, ns.Name, ns.Name, "--values", testchart.Values("minimal-values.yaml"))
	sa := objs.One(t, "ServiceAccount").GetName()
	for _, kind := range []string{"ServiceAccount", "ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding"} {
		for _, u := range objs.Kind(kind) {
			mustCreate(t, u)
			if u.GetNamespace() == "" {
				t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), u) })
			}
		}
	}

	cfg := rest.CopyConfig(testenv.Config)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:" + ns.Name + ":" + sa}
	op, err := client.New(cfg, client.Options{Scheme: testenv.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	// The authorizer learns of new bindings from a watch.
	waitFor(t, "the rights of the chart are in force", func() bool {
		return op.List(t.Context(), &v1alpha1.SiteRouterList{}) == nil
	})
	return cfg, ns.Name
}

// runAs has the operator act as a client: everything it writes and reads, the
// networks of the cluster included.
func (g *rig) runAs(c client.Client) {
	g.t.Helper()
	clock := func() time.Time { return g.now }
	g.disc = &flaky{Reader: c}
	source, err := denyset.NewSource(g.cfg.ClusterNetworks, denyset.NewDiscoverer(g.disc, g.cfg.Timing.Discovery.Duration, clock))
	if err != nil {
		g.t.Fatal(err)
	}
	g.reg = prometheus.NewRegistry()
	g.writes = &counting{Client: c}
	g.r, err = controller.New(controller.Options{
		Client:   g.writes,
		Reader:   c,
		Recorder: g.rec,
		Config:   g.cfg,
		Engine:   g.spy,
		Networks: source,
		Metrics:  controller.NewMetrics(g.reg),
		Now:      clock,
	})
	if err != nil {
		g.t.Fatal(err)
	}
}

// The passes of a gateway, from its making with its Cilium policies through a
// move to another image to its deletion, are run as the service account of the
// chart: a verb the chart does not grant fails the pass that needs it.
func TestTheOperatorDoesItsWorkWithWhatTheChartGrants(t *testing.T) {
	cfg, operatorNamespace := asTheOperator(t)
	op, err := client.New(cfg, client.Options{Scheme: testenv.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	g := newRig(t, onFirstVersion, func(c *config.Config, _ *v1alpha1.SiteRouter) {
		c.Gateway.Isolation = gateway.IsolationConfig{
			Mode:       gateway.IsolationCilium,
			Controller: gateway.ControllerIdentity{Namespace: operatorNamespace, Labels: map[string]string{"app.kubernetes.io/name": "site-router-operator"}},
		}
	})
	g.runAs(op)
	g.ready()
	policies := &unstructured.UnstructuredList{}
	policies.SetGroupVersionKind(gateway.CiliumNetworkPolicyGVK.GroupVersion().WithKind("CiliumNetworkPolicyList"))
	if err := k8sClient.List(t.Context(), policies, client.InNamespace(g.ns)); err != nil {
		t.Fatal(err)
	}
	if len(policies.Items) != 2 {
		t.Errorf("the operator made %d Cilium policies, want 2", len(policies.Items))
	}

	g.latest()
	g.runAs(op)
	g.reconcile()
	g.setDV(diskName(imageV2), "Succeeded")
	g.reconcile()
	if g.exists(gateway.VirtualMachineInstanceGVK, g.names().VM) {
		t.Fatal("the instance of the gateway was not deleted to boot it from the new disk")
	}
	g.reboot()
	g.reconcile()
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionTrue, "Running")
	oldDisk, oldSeed := diskOf(name, imageV1)
	if g.exists(gateway.DataVolumeGVK, oldDisk) || g.secretExists(oldSeed) {
		t.Error("the disk and the seed of the old image were not deleted")
	}

	if err := k8sClient.Delete(t.Context(), g.sr()); err != nil {
		t.Fatal(err)
	}
	g.reconcile()
	g.sync()
	g.reconcile()
	if g.sr() != nil {
		t.Error("the SiteRouter is held after the routes are gone")
	}
}

// leaseID is the Lease the manager takes turns at, as cmd/main.go names it.
const leaseID = "642d6065.aenix.io"

// The manager runs as the service account of the chart: it becomes the leader with
// the Lease and the events the chart grants in its namespace, and its cache lists
// and watches what it does, or the controller would never start.
func TestTheManagerRunsWithWhatTheChartGrants(t *testing.T) {
	// The first operator of an installation makes the Lease.
	t.Run("a new Lease", func(t *testing.T) { runAsTheOperator(t, false) })
	// A pod that follows one that is gone finds its Lease and takes it over by
	// updating it.
	t.Run("the Lease of an operator that is gone", func(t *testing.T) { runAsTheOperator(t, true) })
}

func runAsTheOperator(t *testing.T, takeOver bool) {
	cfg, operatorNamespace := asTheOperator(t)
	g := newRig(t)
	gone := "an-operator-that-is-gone"
	if takeOver {
		mustCreate(t, &coordinationv1.Lease{
			Name:      leaseID,
			Namespace: operatorNamespace,
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &gone,
				LeaseDurationSeconds: new(int32(1)),
				RenewTime:            &metav1.MicroTime{Time: time.Now().Add(-time.Hour)},
			},
		})
	}

	// A watch the operator may not make fails only in its informer: the first list
	// fills the cache, the controller starts, and the objects of that kind no longer
	// wake it. What the informers are refused is kept to be checked.
	var mu sync.Mutex
	var refused []string
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: testenv.Scheme,
		Cache: cache.Options{
			ByObject: controller.CacheObjects(),
			DefaultWatchErrorHandler: func(ctx context.Context, r *toolscache.Reflector, err error) {
				mu.Lock()
				refused = append(refused, err.Error())
				mu.Unlock()
				toolscache.DefaultWatchErrorHandler(ctx, r, err)
			},
		},
		Metrics:                 metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:  "0",
		LeaderElection:          true,
		LeaderElectionNamespace: operatorNamespace,
		LeaderElectionID:        leaseID,
		Controller:              ctrlconfig.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	source, err := denyset.NewSource(g.cfg.ClusterNetworks, denyset.NewDiscoverer(mgr.GetAPIReader(), time.Minute, nil))
	if err != nil {
		t.Fatal(err)
	}
	rec, err := controller.New(controller.Options{
		Client:   mgr.GetClient(),
		Reader:   mgr.GetAPIReader(),
		Recorder: mgr.GetEventRecorder("site-router-operator"),
		Config:   g.cfg,
		Engine:   g.spy,
		Networks: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = mgr.Start(ctx)
	}()
	// The connector acts on its own, as it does in a cluster.
	go func() {
		for ctx.Err() == nil {
			_ = g.conn.Sync(ctx)
			time.Sleep(100 * time.Millisecond)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	select {
	case <-mgr.Elected():
	case <-time.After(30 * time.Second):
		t.Fatal("the manager did not become the leader")
	}
	lease := &coordinationv1.Lease{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: operatorNamespace, Name: leaseID}, lease); err != nil {
		t.Fatal(err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == gone {
		t.Fatalf("the Lease is held by %v, not by the manager", lease.Spec.HolderIdentity)
	}
	waitFor(t, "the SiteRouter is given a finalizer and its VM is made", func() bool {
		sr := g.sr()
		vm := &unstructured.Unstructured{}
		vm.SetGroupVersionKind(gateway.VirtualMachineGVK)
		return sr != nil && slices.Contains(sr.Finalizers, controller.Finalizer) &&
			k8sClient.Get(ctx, client.ObjectKey{Namespace: g.ns, Name: g.names().VM}, vm) == nil
	})
	waitFor(t, "leader election says who took the Lease", func() bool {
		events := &corev1.EventList{}
		return k8sClient.List(ctx, events, client.InNamespace(operatorNamespace)) == nil &&
			slices.ContainsFunc(events.Items, func(e corev1.Event) bool { return e.Reason == "LeaderElection" })
	})

	// The controller records an event when the gateway is ready, and not while it
	// starts.
	g.tenantPod()
	g.boot()
	waitFor(t, "the controller says with an event that the gateway is ready", func() bool {
		events := &eventsv1.EventList{}
		return k8sClient.List(ctx, events, client.InNamespace(g.ns)) == nil &&
			slices.ContainsFunc(events.Items, func(e eventsv1.Event) bool {
				return e.ReportingController == "site-router-operator" && e.Regarding.Name == name && e.Reason == "Ready"
			})
	})

	mu.Lock()
	defer mu.Unlock()
	if len(refused) != 0 {
		t.Errorf("the informers of the manager were refused: %v", refused)
	}
}
