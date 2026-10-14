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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/internal/controller"
	"github.com/aenix-io/site-router-operator/internal/denyset"
	"github.com/aenix-io/site-router-operator/internal/gateway"
	"github.com/aenix-io/site-router-operator/internal/testenv"
)

// wakeUp is how long a gateway may take to follow a change to what it is made of.
// A gateway that is not ready is looked at again every ten seconds whatever
// happens, so a wait that is shorter than that is one only a watch can meet.
const wakeUp = 7 * time.Second

// waitFor polls until the condition holds or wakeUp has passed.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(wakeUp)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %v: %s", wakeUp, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The controller runs in a manager with the cache the operator uses, and follows
// the objects its gateway is made of without waiting to be asked again: the VM
// that starts, the Service that is given an address and the Forwarder the
// connector answers each wake it.
func TestTheControllerFollowsWhatItsGatewayIsMadeOf(t *testing.T) {
	g := newRig(t)

	mgr, err := ctrl.NewManager(testenv.Config, ctrl.Options{
		Scheme:                 testenv.Scheme,
		Cache:                  cache.Options{ByObject: controller.CacheObjects()},
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             ctrlconfig.Controller{SkipNameValidation: new(true)},
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
		Recorder: mgr.GetEventRecorder("site-router-operator-test"),
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

	waitFor(t, "the SiteRouter is given a finalizer and its VM is made", func() bool {
		sr := g.sr()
		vm := &unstructured.Unstructured{}
		vm.SetGroupVersionKind(gateway.VirtualMachineGVK)
		return sr != nil && slices.Contains(sr.Finalizers, controller.Finalizer) &&
			k8sClient.Get(ctx, client.ObjectKey{Namespace: g.ns, Name: g.names().VM}, vm) == nil
	})

	g.tenantPod()
	g.boot()
	waitFor(t, "the gateway is configured and routed once KubeVirt and the load balancer have done their part", func() bool {
		sr := g.sr()
		return sr != nil && meta.IsStatusConditionTrue(sr.Status.Conditions, v1alpha1.ConditionReady)
	})
	if got := g.asked(); !slices.Equal(got, []string{"192.0.2.0/24"}) {
		t.Errorf("the Forwarder asks for %v", got)
	}

	// An edit is carried on as well.
	g.edit(func(sr *v1alpha1.SiteRouter) { sr.Spec.RemoteCIDRs = []v1alpha1.IPv4CIDR{"198.18.0.0/24"} })
	waitFor(t, "the new network is routed", func() bool {
		sr := g.sr()
		c := meta.FindStatusCondition(sr.Status.Conditions, v1alpha1.ConditionRoutesProgrammed)
		return slices.Equal(g.asked(), []string{"198.18.0.0/24"}) && c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == sr.Generation
	})

	// A SiteRouter that is deleted goes once the connector has taken the routes.
	if err := k8sClient.Delete(ctx, g.sr()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the SiteRouter is released", func() bool { return g.sr() == nil })
}
