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

package conformance

import (
	"errors"
	"fmt"
	"maps"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
)

// timeout is how long the suite waits for a connector to act, and window is how
// long it watches one that is meant not to.
const (
	timeout = 30 * time.Second
	window  = 2 * time.Second
)

// vmiKind is the kind of the instance of KubeVirt that a gateway runs as.
const vmiKind = "VirtualMachineInstance"

var vmiGVK = schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: vmiKind}

// env is a namespace of a tenant, and the connector that serves it.
type env struct {
	t  *testing.T
	s  Setup
	c  client.Client
	ns string
}

func newEnv(t *testing.T, s Setup) *env {
	t.Helper()
	ns := &corev1.Namespace{GenerateName: "tenant-"}
	if err := s.Client.Create(t.Context(), ns); err != nil {
		t.Fatalf("making a namespace: %v", err)
	}
	// Nothing makes the default ServiceAccount of a namespace in a cluster that has
	// no controller manager, and a pod cannot be made without it.
	sa := &corev1.ServiceAccount{Name: "default", Namespace: ns.Name}
	if err := s.Client.Create(t.Context(), sa); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("making the service account: %v", err)
	}
	return &env{t: t, s: s, c: s.Client, ns: ns.Name}
}

// start runs the connector, and stops it when the scenario ends.
func (e *env) start() func() {
	e.t.Helper()
	stop := e.s.Start(e.t)
	e.t.Cleanup(stop)
	return stop
}

func (e *env) must(o client.Object) {
	e.t.Helper()
	if err := e.c.Create(e.t.Context(), o); err != nil {
		e.t.Fatalf("making %T %s: %v", o, o.GetName(), err)
	}
}

// pod makes a running pod with the address ip, as the network makes one.
func (e *env) pod(name string, labels map[string]string, ip string, opts ...func(*corev1.Pod)) *corev1.Pod {
	e.t.Helper()
	p := &corev1.Pod{
		Name: name, Namespace: e.ns, Labels: labels,
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "registry.example/app"}}},
	}
	for _, o := range opts {
		o(p)
	}
	if err := e.s.Network.Admit(e.t.Context(), e.c, p); err != nil {
		e.t.Fatalf("the network admitting %s: %v", name, err)
	}
	e.must(p)
	// Making a pod drops its status, so it is written after.
	p.Status.Phase = corev1.PodRunning
	p.Status.PodIP = ip
	if ip != "" {
		p.Status.PodIPs = []corev1.PodIP{{IP: ip}}
	}
	if err := e.c.Status().Update(e.t.Context(), p); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) getPod(name string) *corev1.Pod {
	e.t.Helper()
	p := &corev1.Pod{}
	if err := e.c.Get(e.t.Context(), types.NamespacedName{Namespace: e.ns, Name: name}, p); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// forwards says whether the network lets the pod forward.
func (e *env) forwards(name string) bool {
	e.t.Helper()
	return e.s.Network.Forwards(e.getPod(name))
}

// routes are the routes of the namespace.
func (e *env) routes() map[string]string {
	e.t.Helper()
	r, err := e.s.Network.Routes(e.t.Context(), e.c, e.ns)
	if err != nil {
		e.t.Fatalf("reading the routes: %v", err)
	}
	return r
}

// addRoute makes a route that no Forwarder owns.
func (e *env) addRoute(destination, nextHop string) {
	e.t.Helper()
	if err := e.s.Network.AddRoute(e.t.Context(), e.c, e.ns, destination, nextHop); err != nil {
		e.t.Fatalf("making a route: %v", err)
	}
}

// gateway is the instance of a gateway, and its Forwarder.
type gateway struct {
	e        *env
	instance string
	name     string
	vmi      *unstructured.Unstructured
}

func gatewayLabels(instance string) map[string]string {
	return map[string]string{v1alpha1.LabelInstance: instance}
}

// newGateway makes the VirtualMachineInstance of the instance, named
// site-router-<instance> as the operator names it.
func (e *env) newGateway(instance string) *gateway {
	e.t.Helper()
	g := &gateway{e: e, instance: instance, name: "site-router-" + instance}
	g.vmi = &unstructured.Unstructured{}
	g.vmi.SetGroupVersionKind(vmiGVK)
	g.vmi.SetName(g.name)
	g.vmi.SetNamespace(e.ns)
	g.vmi.SetLabels(gatewayLabels(instance))
	e.must(g.vmi)
	return g
}

// launcher makes a pod the instance controls, as KubeVirt does, and lists it
// among the active pods of the instance when active is true.
func (g *gateway) launcher(name, ip string, active bool) *corev1.Pod {
	g.e.t.Helper()
	p := g.e.pod(name, gatewayLabels(g.instance), ip, func(p *corev1.Pod) {
		p.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "kubevirt.io/v1", Kind: vmiKind, Name: g.name, UID: g.vmi.GetUID(), Controller: new(true),
		}}
	})
	if active {
		g.setActive(p)
	}
	return p
}

// setActive writes the active pods of the instance, as KubeVirt does.
func (g *gateway) setActive(pods ...*corev1.Pod) {
	g.e.t.Helper()
	active := map[string]any{}
	for _, p := range pods {
		active[string(p.UID)] = "node-1"
	}
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(vmiGVK)
	if err := g.e.c.Get(g.e.t.Context(), client.ObjectKeyFromObject(g.vmi), vmi); err != nil {
		g.e.t.Fatal(err)
	}
	if err := unstructured.SetNestedField(vmi.Object, active, "status", "activePods"); err != nil {
		g.e.t.Fatal(err)
	}
	if err := g.e.c.Status().Update(g.e.t.Context(), vmi); err != nil {
		g.e.t.Fatal(err)
	}
}

// forwarder makes the Forwarder of the gateway as the operator does.
func (g *gateway) forwarder(cidrs ...string) *v1alpha1.Forwarder {
	g.e.t.Helper()
	f := &v1alpha1.Forwarder{
		Name: g.name, Namespace: g.e.ns,
		Spec: v1alpha1.ForwarderSpec{
			WorkloadSelector: metav1.LabelSelector{MatchLabels: gatewayLabels(g.instance)},
			ScopeSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: v1alpha1.LabelInstance, Operator: metav1.LabelSelectorOpDoesNotExist},
			}},
		},
	}
	for _, c := range cidrs {
		f.Spec.CIDRs = append(f.Spec.CIDRs, v1alpha1.IPv4CIDR(c))
	}
	g.e.must(f)
	return f
}

// ask changes the networks the Forwarder asks for.
func (g *gateway) ask(cidrs ...string) {
	g.e.t.Helper()
	f := g.get()
	f.Spec.CIDRs = nil
	for _, c := range cidrs {
		f.Spec.CIDRs = append(f.Spec.CIDRs, v1alpha1.IPv4CIDR(c))
	}
	if err := g.e.c.Update(g.e.t.Context(), f); err != nil {
		g.e.t.Fatal(err)
	}
}

func (g *gateway) get() *v1alpha1.Forwarder {
	g.e.t.Helper()
	f := &v1alpha1.Forwarder{}
	if err := g.e.c.Get(g.e.t.Context(), types.NamespacedName{Namespace: g.e.ns, Name: g.name}, f); err != nil {
		g.e.t.Fatal(err)
	}
	return f
}

// condition waits for a condition of the Forwarder to have the status, for the
// generation of the Forwarder, and returns the Forwarder.
func (g *gateway) condition(typ string, status metav1.ConditionStatus) *v1alpha1.Forwarder {
	g.e.t.Helper()
	var f *v1alpha1.Forwarder
	eventually(g.e.t, fmt.Sprintf("%s of %s is %s", typ, g.name, status), func() error {
		f = g.get()
		c := conditionOf(f, typ)
		switch {
		case c == nil:
			return errors.New("there is no such condition")
		case c.ObservedGeneration != f.Generation:
			return fmt.Errorf("it is for generation %d of %d", c.ObservedGeneration, f.Generation)
		case c.Status != status:
			return fmt.Errorf("it is %s (%s): %s", c.Status, c.Reason, c.Message)
		}
		return nil
	})
	return f
}

func conditionOf(f *v1alpha1.Forwarder, typ string) *metav1.Condition {
	for i := range f.Status.Conditions {
		if f.Status.Conditions[i].Type == typ {
			return &f.Status.Conditions[i]
		}
	}
	return nil
}

// launcherPod is the pod of the gateway that the scenarios make.
const launcherPod = "virt-launcher-branch"

// programmed waits for the routes of the gateway to be programmed, and checks
// that the pod forwards: a scenario that the grant is taken back, or kept, is
// worth nothing if there was none.
func (g *gateway) programmed() {
	g.e.t.Helper()
	g.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionTrue)
	if !g.e.forwards(launcherPod) {
		g.e.t.Fatalf("the pod %s does not forward", launcherPod)
	}
}

// eventually waits for check to pass, and fails the test with what it said last
// when it does not within the timeout.
func eventually(t *testing.T, what string, check func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var err error
	for time.Now().Before(deadline) {
		if err = check(); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s: %v", what, err)
}

// consistently checks that check keeps passing for a while: what a connector
// that is not meant to act must show.
func consistently(t *testing.T, what string, d time.Duration, check func() error) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if err := check(); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// sameRoutes says whether two sets of routes are one.
func sameRoutes(a, b map[string]string) bool { return maps.Equal(a, b) }
