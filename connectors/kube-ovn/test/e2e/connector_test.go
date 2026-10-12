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

package e2e

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/controller"
	"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/grant"
	"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/routes"
)

const (
	gwIP   = "10.244.1.9"
	gwIP2  = "10.244.1.10"
	webIP  = "10.244.1.5"
	remote = "192.0.2.0/24"
	// humanRoute is a route of the namespace that no Forwarder owns, to the pod
	// of somebody else.
	foreignGW  = "10.244.0.77"
	humanRoute = `{"dst":"198.51.100.0/24","gw":"` + foreignGW + `"}`
)

var web = map[string]string{"app": "web"}

// granted says whether the pod forwards: its check of addresses is off.
func (g *gateway) granted(name string) error {
	p := g.w.getPod(name)
	if p.Annotations[grant.PortSecurityAnnotation] != "false" {
		return fmt.Errorf("the pod %s does not forward: %v", name, p.Annotations)
	}
	return nil
}

// programmed waits for the routes of the gateway to be programmed, and checks
// that the pod forwards: a test that the grant is taken back, or kept, is
// worth nothing if there was none.
func (g *gateway) programmed(pod string) {
	g.w.t.Helper()
	g.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionTrue, controller.ReasonProgrammed)
	if err := g.granted(pod); err != nil {
		g.w.t.Fatal(err)
	}
}

// checked says whether the check of addresses of the pod is on.
func (w *world) checked(name string) error {
	p := w.getPod(name)
	if v := p.Annotations[grant.PortSecurityAnnotation]; v != "true" {
		return fmt.Errorf("the check of the pod %s is %q", name, v)
	}
	return nil
}

// untouched says whether the pod is as the webhook made it: checked, and named
// by no Forwarder.
func (w *world) untouched(name string) error {
	if err := w.checked(name); err != nil {
		return err
	}
	if v, ok := w.getPod(name).Annotations[grant.GrantedByAnnotation]; ok {
		return fmt.Errorf("the pod %s names %q", name, v)
	}
	return nil
}

func (w *world) routes() string {
	w.t.Helper()
	return w.namespace().Annotations[routes.Annotation]
}

func (w *world) setRoutes(value string) {
	w.t.Helper()
	ns := w.namespace()
	if ns.Annotations == nil {
		ns.Annotations = map[string]string{}
	}
	ns.Annotations[routes.Annotation] = value
	if err := k8sClient.Update(w.t.Context(), ns); err != nil {
		w.t.Fatal(err)
	}
}

func route(dst, gw string) string { return `{"dst":"` + dst + `","gw":"` + gw + `"}` }

func TestAddressesAreReportedBeforeAnythingIsRouted(t *testing.T) {
	w := newWorld(t)
	startConnector(t, nil)
	g := w.newGateway("branch")
	g.launcher("virt-launcher-branch", gwIP, true)
	w.pod("web-0", web, webIP)
	svc := &corev1.Service{
		Name: "web", Namespace: w.ns,
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
	}
	mustCreate(t, svc)
	g.forwarder()

	f := g.condition(v1alpha1.ForwarderResolved, metav1.ConditionTrue, controller.ReasonGatewayFound)
	if !slices.Equal(f.Status.Forwarder.Addresses, []string{gwIP}) {
		t.Errorf("forwarder addresses = %v, want [%s]", f.Status.Forwarder.Addresses, gwIP)
	}
	want := []string{webIP, svc.Spec.ClusterIP}
	slices.SortFunc(want, func(a, b string) int { return netip.MustParseAddr(a).Compare(netip.MustParseAddr(b)) })
	if !slices.Equal(f.Status.Scope.Addresses, want) {
		t.Errorf("scope addresses = %v, want %v", f.Status.Scope.Addresses, want)
	}
	g.condition(v1alpha1.ForwarderGranted, metav1.ConditionFalse, controller.ReasonNoNetworks)
	g.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionFalse, controller.ReasonNoNetworks)
	g.condition(v1alpha1.ForwarderAccepted, metav1.ConditionTrue, controller.ReasonServed)
	if !slices.Contains(f.Finalizers, controller.Finalizer) {
		t.Errorf("finalizers = %v, want the connector's", f.Finalizers)
	}
	consistently(t, "nothing is granted or routed for no networks", time.Second, func() error {
		if v := w.routes(); v != "" {
			return fmt.Errorf("the namespace has routes %s", v)
		}
		return w.untouched("virt-launcher-branch")
	})
}

func TestNetworksAreGrantedAndRouted(t *testing.T) {
	w := newWorld(t)
	startConnector(t, nil)
	g := w.newGateway("branch")
	g.launcher("virt-launcher-branch", gwIP, true)
	g.forwarder(remote)

	f := g.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionTrue, controller.ReasonProgrammed)
	g.condition(v1alpha1.ForwarderGranted, metav1.ConditionTrue, controller.ReasonGranted)
	g.condition(v1alpha1.ForwarderConflict, metav1.ConditionFalse, controller.ReasonNoConflict)
	if f.Status.ObservedGeneration != f.Generation {
		t.Errorf("observed generation %d of %d", f.Status.ObservedGeneration, f.Generation)
	}
	if err := g.granted("virt-launcher-branch"); err != nil {
		t.Error(err)
	}
	if got := w.getPod("virt-launcher-branch").Annotations[grant.GrantedByAnnotation]; got != g.name {
		t.Errorf("the pod names %q as the Forwarder that let it forward, want %s", got, g.name)
	}
	if got, want := w.routes(), "["+route(remote, gwIP)+"]"; got != want {
		t.Errorf("routes = %s, want %s", got, want)
	}
	if got, want := w.namespace().Annotations[routes.OwnersAnnotation], `{"`+g.name+`":["`+gwIP+`"]}`; got != want {
		t.Errorf("owners = %s, want %s", got, want)
	}
}

func TestNoNetworksTakesTheGrantAndTheRoutesBack(t *testing.T) {
	w := newWorld(t)
	w.setRoutes("[" + humanRoute + "]")
	startConnector(t, nil)
	g := w.newGateway("branch")
	g.launcher("virt-launcher-branch", gwIP, true)
	g.forwarder(remote)
	g.programmed("virt-launcher-branch")

	g.ask()
	g.condition(v1alpha1.ForwarderGranted, metav1.ConditionFalse, controller.ReasonNoNetworks)
	g.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionFalse, controller.ReasonNoNetworks)
	if err := w.untouched("virt-launcher-branch"); err != nil {
		t.Error(err)
	}
	ns := w.namespace()
	if got, want := ns.Annotations[routes.Annotation], "["+humanRoute+"]"; got != want {
		t.Errorf("routes = %s, want only the route that is not the Forwarder's: %s", got, want)
	}
	if v, ok := ns.Annotations[routes.OwnersAnnotation]; ok {
		t.Errorf("owners = %s, want none", v)
	}
}

// Of two Forwarders that claim the same network, the one made first keeps it,
// whatever their names say.
func TestTheForwarderMadeFirstKeepsANetwork(t *testing.T) {
	w := newWorld(t)
	startConnector(t, nil)
	first := w.newGateway("bravo")
	first.launcher("virt-launcher-bravo", gwIP, true)
	first.forwarder(remote)
	first.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionTrue, controller.ReasonProgrammed)

	// The apiserver keeps the time an object is made to the second.
	time.Sleep(1100 * time.Millisecond)
	second := w.newGateway("alpha")
	second.launcher("virt-launcher-alpha", gwIP2, true)
	second.forwarder("192.0.2.0/25")

	f := second.condition(v1alpha1.ForwarderConflict, metav1.ConditionTrue, controller.ReasonNetworkClaimed)
	if c := conditionOf(f, v1alpha1.ForwarderConflict); !strings.Contains(c.Message, first.name) {
		t.Errorf("the conflict does not name the Forwarder that holds the network: %q", c.Message)
	}
	second.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionFalse, controller.ReasonConflict)
	second.condition(v1alpha1.ForwarderGranted, metav1.ConditionFalse, controller.ReasonConflict)
	consistently(t, "the second gateway gets nothing", time.Second, func() error {
		if got, want := w.routes(), "["+route(remote, gwIP)+"]"; got != want {
			return fmt.Errorf("routes = %s, want %s", got, want)
		}
		return w.untouched("virt-launcher-alpha")
	})
	first.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionTrue, controller.ReasonProgrammed)
	first.condition(v1alpha1.ForwarderConflict, metav1.ConditionFalse, controller.ReasonNoConflict)
}

// A Forwarder made first that asks for a network later takes it over from one
// that came after it; the other keeps the grant it has, and is in conflict.
func TestTheForwarderMadeFirstTakesANetworkOver(t *testing.T) {
	w := newWorld(t)
	startConnector(t, nil)
	first := w.newGateway("bravo")
	first.launcher("virt-launcher-bravo", gwIP, true)
	first.forwarder()
	first.condition(v1alpha1.ForwarderResolved, metav1.ConditionTrue, controller.ReasonGatewayFound)

	time.Sleep(1100 * time.Millisecond)
	second := w.newGateway("alpha")
	second.launcher("virt-launcher-alpha", gwIP2, true)
	second.forwarder(remote, "203.0.113.0/24")
	second.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionTrue, controller.ReasonProgrammed)

	first.ask("192.0.2.0/25")
	first.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionTrue, controller.ReasonProgrammed)
	second.condition(v1alpha1.ForwarderConflict, metav1.ConditionTrue, controller.ReasonNetworkClaimed)
	eventually(t, "the network is routed to the gateway made first", func() error {
		want := "[" + route("203.0.113.0/24", gwIP2) + "," + route("192.0.2.0/25", gwIP) + "]"
		if got := w.routes(); got != want {
			return fmt.Errorf("routes = %s, want %s", got, want)
		}
		return nil
	})
	if err := second.granted("virt-launcher-alpha"); err != nil {
		t.Errorf("a conflict took the grant of a gateway that had it: %v", err)
	}
}

// A label is written by whoever makes the pod. Only the pod KubeVirt runs the
// instance of the gateway in is let forward.
func TestALabelAloneEarnsNoGrant(t *testing.T) {
	w := newWorld(t)
	startConnector(t, nil)
	g := w.newGateway("branch")
	g.launcher("virt-launcher-branch", gwIP, true)
	labels := gatewayLabels("branch")
	w.pod("labelled", labels, "10.244.1.61")
	w.pod("forged-owner", labels, "10.244.1.62", func(p *corev1.Pod) {
		p.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "kubevirt.io/v1", Kind: "VirtualMachineInstance", Name: g.name, UID: "forged", Controller: new(true),
		}}
	})
	// Controlled by the instance, by its UID, and not one KubeVirt runs it in.
	g.launcher("not-active", "10.244.1.63", false)
	g.forwarder(remote)

	f := g.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionTrue, controller.ReasonProgrammed)
	if err := g.granted("virt-launcher-branch"); err != nil {
		t.Error(err)
	}
	for _, name := range []string{"labelled", "forged-owner", "not-active"} {
		if err := w.untouched(name); err != nil {
			t.Error(err)
		}
	}
	if !slices.Equal(f.Status.Forwarder.Addresses, []string{gwIP}) {
		t.Errorf("forwarder addresses = %v, want only %s", f.Status.Forwarder.Addresses, gwIP)
	}
	if len(f.Status.Scope.Addresses) != 0 {
		t.Errorf("scope = %v: a pod with the label of a gateway is no workload of the tenant", f.Status.Scope.Addresses)
	}
}

// A pod the instance controls is not let forward until KubeVirt lists it among
// the active pods of the instance.
func TestAPodWaitsForKubeVirtToListIt(t *testing.T) {
	w := newWorld(t)
	startConnector(t, nil)
	g := w.newGateway("branch")
	p := g.launcher("virt-launcher-branch", gwIP, false)
	g.forwarder(remote)

	g.condition(v1alpha1.ForwarderResolved, metav1.ConditionFalse, controller.ReasonGatewayUnproven)
	consistently(t, "an unproven pod gets nothing", time.Second, func() error {
		if v := w.routes(); v != "" {
			return fmt.Errorf("the namespace has routes %s", v)
		}
		return w.untouched("virt-launcher-branch")
	})

	g.setActive(p)
	g.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionTrue, controller.ReasonProgrammed)
	if err := g.granted("virt-launcher-branch"); err != nil {
		t.Error(err)
	}
}

// KubeVirt makes another pod for the gateway, at another address: the new pod is
// let forward and the routes move to it.
func TestANewPodOfTheGatewayIsGrantedAgain(t *testing.T) {
	w := newWorld(t)
	startConnector(t, nil)
	g := w.newGateway("branch")
	old := g.launcher("virt-launcher-branch-1", gwIP, true)
	g.forwarder(remote)
	g.programmed("virt-launcher-branch-1")

	if err := k8sClient.Delete(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	g.launcher("virt-launcher-branch-2", gwIP2, true)

	eventually(t, "the new pod is let forward", func() error { return g.granted("virt-launcher-branch-2") })
	eventually(t, "the routes go to the new pod", func() error {
		if got, want := w.routes(), "["+route(remote, gwIP2)+"]"; got != want {
			return fmt.Errorf("routes = %s, want %s", got, want)
		}
		if got, want := w.namespace().Annotations[routes.OwnersAnnotation], `{"`+g.name+`":["`+gwIP2+`"]}`; got != want {
			return fmt.Errorf("owners = %s, want %s", got, want)
		}
		return nil
	})
	f := g.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionTrue, controller.ReasonProgrammed)
	if !slices.Equal(f.Status.Forwarder.Addresses, []string{gwIP2}) {
		t.Errorf("forwarder addresses = %v, want [%s]", f.Status.Forwarder.Addresses, gwIP2)
	}
}

// A pod KubeVirt no longer runs the instance in, or that is no longer selected,
// loses the grant.
func TestAPodThatIsNoLongerTheGatewayLosesTheGrant(t *testing.T) {
	w := newWorld(t)
	startConnector(t, nil)
	g := w.newGateway("branch")
	p := g.launcher("virt-launcher-branch", gwIP, true)
	g.forwarder(remote)
	g.condition(v1alpha1.ForwarderGranted, metav1.ConditionTrue, controller.ReasonGranted)
	if err := g.granted(p.Name); err != nil {
		t.Fatal(err)
	}

	other := g.launcher("virt-launcher-other", gwIP2, false)
	g.setActive(other)
	eventually(t, "the pod KubeVirt left loses the grant", func() error { return w.untouched(p.Name) })
	eventually(t, "the pod KubeVirt runs it in now gets it", func() error { return g.granted(other.Name) })

	// The label goes: the pod is no longer selected.
	o := w.getPod(other.Name)
	o.Labels = nil
	if err := k8sClient.Update(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the pod that is no longer selected loses the grant", func() error { return w.untouched(other.Name) })
	g.condition(v1alpha1.ForwarderResolved, metav1.ConditionFalse, controller.ReasonGatewayNotFound)
}

func TestDeletionTakesEverythingBack(t *testing.T) {
	w := newWorld(t)
	w.setRoutes("[" + humanRoute + "]")
	startConnector(t, nil)
	g := w.newGateway("branch")
	g.launcher("virt-launcher-branch", gwIP, true)
	f := g.forwarder(remote)
	g.programmed("virt-launcher-branch")

	if err := k8sClient.Delete(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the Forwarder goes", func() error {
		err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(f), &v1alpha1.Forwarder{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("it is still there: %v", err)
	})
	if err := w.untouched("virt-launcher-branch"); err != nil {
		t.Error(err)
	}
	ns := w.namespace()
	if got, want := ns.Annotations[routes.Annotation], "["+humanRoute+"]"; got != want {
		t.Errorf("routes = %s, want %s", got, want)
	}
	if v, ok := ns.Annotations[routes.OwnersAnnotation]; ok {
		t.Errorf("owners = %s, want none", v)
	}
}

// The pods made before the routes do not have them, and are named; a pod made
// after gets them from the webhook, and is not.
func TestThePodsMadeBeforeTheRoutesArePending(t *testing.T) {
	w := newWorld(t)
	startConnector(t, nil)
	g := w.newGateway("branch")
	g.launcher("virt-launcher-branch", gwIP, true)
	w.pod("web-0", web, webIP)
	g.forwarder(remote)
	g.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionTrue, controller.ReasonProgrammed)

	pending := func(want ...string) func() error {
		return func() error {
			if got := g.get().Status.PendingWorkloads; !slices.Equal(got, want) {
				return fmt.Errorf("pending = %v, want %v", got, want)
			}
			return nil
		}
	}
	eventually(t, "the pod made before the routes is pending", pending("web-0"))

	w.pod("web-1", web, "10.244.1.6")
	if got := w.getPod("web-1").Annotations[routes.Annotation]; got != "["+route(remote, gwIP)+"]" {
		t.Fatalf("the webhook gave the new pod %q", got)
	}
	consistently(t, "the pod made after the routes is not pending", time.Second, pending("web-0"))

	if err := k8sClient.Delete(t.Context(), w.getPod("web-0")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "nothing is pending once the old pod is gone", pending())
	g.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionTrue, controller.ReasonProgrammed)
}

// addressRE finds what reads as an IPv4 address, with a prefix length or not.
var addressRE = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?:/\d{1,2})?`)

// No condition names an address that is not the tenant's, and the scope has
// none either: not the gateway of somebody else that a route of the namespace
// goes to, nor a node.
func TestTheStatusNamesNoAddressOfSomebodyElse(t *testing.T) {
	w := newWorld(t)
	// The route of somebody else is to the network the tenant asks for.
	w.setRoutes("[" + route(remote, foreignGW) + "]")
	startConnector(t, nil)
	g := w.newGateway("branch")
	g.launcher("virt-launcher-branch", gwIP, true)
	nodeIP := "192.168.10.5"
	w.pod("agent", web, nodeIP, func(p *corev1.Pod) { p.Spec.HostNetwork = true })
	g.forwarder(remote)

	f := g.condition(v1alpha1.ForwarderConflict, metav1.ConditionTrue, controller.ReasonNetworkClaimed)
	g.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionFalse, controller.ReasonConflict)
	for _, c := range f.Status.Conditions {
		for _, a := range addressRE.FindAllString(c.Message, -1) {
			if a != remote {
				t.Errorf("the condition %s names %s: %q", c.Type, a, c.Message)
			}
		}
	}
	if slices.Contains(f.Status.Scope.Addresses, nodeIP) {
		t.Errorf("the scope has the address of a node: %v", f.Status.Scope.Addresses)
	}
	if slices.Contains(f.Status.Scope.Addresses, foreignGW) || slices.Contains(f.Status.Forwarder.Addresses, foreignGW) {
		t.Errorf("the status has the address of somebody else: %+v", f.Status)
	}
	events := &corev1.EventList{}
	if err := k8sClient.List(t.Context(), events, client.InNamespace(w.ns)); err != nil {
		t.Fatal(err)
	}
	for _, e := range events.Items {
		if addressRE.MatchString(e.Message) {
			t.Errorf("the event %s names an address: %q", e.Reason, e.Message)
		}
	}
	if got := w.routes(); got != "["+route(remote, foreignGW)+"]" {
		t.Errorf("the route of somebody else was changed: %s", got)
	}
}

// The connector stops and comes back while KubeVirt has not written the active
// pods of the instance yet. The gateway keeps the grant and the routes, and
// nothing is written to the pod or the namespace.
func TestARestartDoesNotDisturbTheGrant(t *testing.T) {
	w := newWorld(t)
	conn := startConnector(t, nil)
	g := w.newGateway("branch")
	p := g.launcher("virt-launcher-branch", gwIP, true)
	g.forwarder(remote)
	g.programmed("virt-launcher-branch")
	podVersion := w.getPod(p.Name).ResourceVersion
	routesBefore := w.routes()

	conn.stop()
	g.setActive()
	startConnector(t, nil)
	g.condition(v1alpha1.ForwarderResolved, metav1.ConditionFalse, controller.ReasonGatewayUnproven)
	unchanged := func() error {
		if v := w.getPod(p.Name).ResourceVersion; v != podVersion {
			return fmt.Errorf("the pod was written: version %s, was %s", v, podVersion)
		}
		if v := w.routes(); v != routesBefore {
			return fmt.Errorf("routes = %s, were %s", v, routesBefore)
		}
		return nil
	}
	consistently(t, "the connector that came back leaves the gateway as it was", 2*time.Second, unchanged)

	g.setActive(p)
	g.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionTrue, controller.ReasonProgrammed)
	consistently(t, "nothing is written again once KubeVirt lists the pod", time.Second, unchanged)
}

// flaky is a client whose reads can be made to fail.
type flaky struct {
	client.Client
	down *atomic.Bool
}

var errDown = errors.New("the apiserver does not answer")

func (f flaky) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if f.down.Load() {
		return errDown
	}
	return f.Client.Get(ctx, key, obj, opts...)
}

func (f flaky) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if f.down.Load() {
		return errDown
	}
	return f.Client.List(ctx, list, opts...)
}

// A connector that cannot read the cluster changes nothing until it can.
func TestAConnectorThatCannotReadChangesNothing(t *testing.T) {
	w := newWorld(t)
	conn := startConnector(t, nil)
	g := w.newGateway("branch")
	p := g.launcher("virt-launcher-branch", gwIP, true)
	g.forwarder(remote)
	g.programmed("virt-launcher-branch")
	conn.stop()

	down := &atomic.Bool{}
	down.Store(true)
	startConnector(t, func(c client.Client) client.Client { return flaky{Client: c, down: down} })
	// What wakes the connector: the gateway is asked for nothing.
	g.ask()
	consistently(t, "a connector that cannot read takes nothing back", 2*time.Second, func() error {
		if err := g.granted(p.Name); err != nil {
			return err
		}
		if got, want := w.routes(), "["+route(remote, gwIP)+"]"; got != want {
			return fmt.Errorf("routes = %s, want %s", got, want)
		}
		return nil
	})

	down.Store(false)
	eventually(t, "the grant is taken back once the connector reads again", func() error { return w.untouched(p.Name) })
}

func conditionOf(f *v1alpha1.Forwarder, typ string) metav1.Condition {
	for _, c := range f.Status.Conditions {
		if c.Type == typ {
			return c
		}
	}
	return metav1.Condition{}
}
