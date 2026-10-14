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
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
)

const (
	gwIP   = "10.244.1.9"
	gwIP2  = "10.244.1.10"
	webIP  = "10.244.1.5"
	remote = "192.0.2.0/24"
	// foreignGW is the next hop of a route of the namespace that no Forwarder owns:
	// the pod of somebody else.
	foreignGW = "10.244.0.77"
	foreign   = "198.51.100.0/24"
)

// webApp is the name of the workload of the tenant, and of its Service.
const webApp = "web"

var web = map[string]string{"app": webApp}

// Run runs every scenario of the contract against the connector of s. The
// scenarios share one cluster and each makes a namespace of its own.
func Run(t *testing.T, s Setup) {
	t.Helper()
	for _, sc := range []struct {
		name string
		run  func(*testing.T, *env)
	}{
		{"AddressesAreReportedBeforeAnythingIsRouted", addressesAreReported},
		{"TheTunnelServiceOfAGatewayIsNotInTheScope", theTunnelServiceIsNotInTheScope},
		{"NetworksAreGrantedAndRouted", networksAreGrantedAndRouted},
		{"NoNetworksTakesTheGrantAndTheRoutesBack", noNetworksTakesBack},
		{"TheForwarderMadeFirstKeepsANetwork", theForwarderMadeFirstKeeps},
		{"ALabelAloneEarnsNoGrant", aLabelAloneEarnsNoGrant},
		{"APodWaitsForKubeVirtToListIt", aPodWaitsForKubeVirt},
		{"APodThatIsNoLongerTheGatewayLosesTheGrant", aPodNoLongerTheGatewayLosesTheGrant},
		{"DeletionTakesEverythingBack", deletionTakesEverythingBack},
		{"ThePodsMadeBeforeTheRoutesArePending", thePodsMadeBeforeTheRoutesArePending},
		{"TheStatusNamesNoAddressOfSomebodyElse", theStatusNamesNoAddressOfSomebodyElse},
		{"ARestartDoesNotDisturbTheGrant", aRestartDoesNotDisturbTheGrant},
	} {
		t.Run(sc.name, func(t *testing.T) { sc.run(t, newEnv(t, s)) })
	}
	t.Run("AConnectorThatCannotReadChangesNothing", func(t *testing.T) {
		if s.StartUnreadable == nil {
			t.Skip("the connector cannot be given a client that fails")
		}
		aConnectorThatCannotReadChangesNothing(t, newEnv(t, s))
	})
}

func sortAddresses(addrs []string) []string {
	out := slices.Clone(addrs)
	slices.SortFunc(out, func(a, b string) int { return netip.MustParseAddr(a).Compare(netip.MustParseAddr(b)) })
	return out
}

// Contract 3 and 4: the gateway and the scope are reported before anything is
// routed, and with no networks nothing is granted or programmed.
func addressesAreReported(t *testing.T, e *env) {
	e.start()
	g := e.newGateway("branch")
	g.launcher(launcherPod, gwIP, true)
	e.pod("web-0", web, webIP)
	svc := &corev1.Service{
		Name: webApp, Namespace: e.ns,
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
	}
	e.must(svc)
	g.forwarder()

	f := g.condition(v1alpha1.ForwarderResolved, metav1.ConditionTrue)
	if !slices.Equal(f.Status.Forwarder.Addresses, []string{gwIP}) {
		t.Errorf("forwarder addresses = %v, want [%s]", f.Status.Forwarder.Addresses, gwIP)
	}
	got := sortAddresses(f.Status.Scope.Addresses)
	if want := sortAddresses([]string{webIP, svc.Spec.ClusterIP}); !slices.Equal(got, want) {
		t.Errorf("scope addresses = %v, want %v", got, want)
	}
	g.condition(v1alpha1.ForwarderAccepted, metav1.ConditionTrue)
	g.condition(v1alpha1.ForwarderGranted, metav1.ConditionFalse)
	g.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionFalse)
	consistently(t, "nothing is granted or routed for no networks", window, func() error {
		if r := e.routes(); len(r) != 0 {
			return fmt.Errorf("the namespace has routes %v", r)
		}
		if e.forwards(launcherPod) {
			return fmt.Errorf("the gateway forwards")
		}
		return nil
	})
}

// Contract 4: the Service in front of a gateway takes the tunnel, and is not an
// address the tenant's traffic goes to through it.
func theTunnelServiceIsNotInTheScope(t *testing.T, e *env) {
	e.start()
	g := e.newGateway("branch")
	g.launcher(launcherPod, gwIP, true)
	tunnel := &corev1.Service{
		Name: "site-router-branch-tunnel", Namespace: e.ns, Labels: gatewayLabels("branch"),
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 500, Protocol: corev1.ProtocolUDP}}},
	}
	e.must(tunnel)
	other := &corev1.Service{
		Name: webApp, Namespace: e.ns,
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
	}
	e.must(other)
	g.forwarder()

	f := g.condition(v1alpha1.ForwarderResolved, metav1.ConditionTrue)
	if tunnel.Spec.ClusterIP == "" || other.Spec.ClusterIP == "" {
		t.Skip("the cluster gives no cluster IP to a Service")
	}
	if slices.Contains(f.Status.Scope.Addresses, tunnel.Spec.ClusterIP) {
		t.Errorf("the scope %v has the cluster IP %s of the Service of the gateway",
			f.Status.Scope.Addresses, tunnel.Spec.ClusterIP)
	}
	if !slices.Contains(f.Status.Scope.Addresses, other.Spec.ClusterIP) {
		t.Errorf("the scope %v lacks the cluster IP %s of a Service of the tenant",
			f.Status.Scope.Addresses, other.Spec.ClusterIP)
	}
}

// Contract 7: with networks, a gateway that is found is let forward and the
// networks are routed to it.
func networksAreGrantedAndRouted(t *testing.T, e *env) {
	e.start()
	g := e.newGateway("branch")
	g.launcher(launcherPod, gwIP, true)
	g.forwarder(remote)

	g.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionTrue)
	g.condition(v1alpha1.ForwarderGranted, metav1.ConditionTrue)
	g.condition(v1alpha1.ForwarderConflict, metav1.ConditionFalse)
	if !e.forwards(launcherPod) {
		t.Error("the gateway does not forward")
	}
	if got := e.routes(); !sameRoutes(got, map[string]string{remote: gwIP}) {
		t.Errorf("routes = %v, want %s to %s", got, remote, gwIP)
	}
}

// Contract 6, 8 and 9: no networks takes the grant and the routes of the
// Forwarder back, and leaves a route that is not its own.
func noNetworksTakesBack(t *testing.T, e *env) {
	e.addRoute(foreign, foreignGW)
	e.start()
	g := e.newGateway("branch")
	g.launcher(launcherPod, gwIP, true)
	g.forwarder(remote)
	g.programmed()

	g.ask()
	g.condition(v1alpha1.ForwarderGranted, metav1.ConditionFalse)
	g.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionFalse)
	eventually(t, "the grant and the routes of the Forwarder are gone", func() error {
		if e.forwards(launcherPod) {
			return fmt.Errorf("the gateway still forwards")
		}
		if got := e.routes(); !sameRoutes(got, map[string]string{foreign: foreignGW}) {
			return fmt.Errorf("routes = %v, want only the route that is not the Forwarder's", got)
		}
		return nil
	})
}

// Contract 10: of two Forwarders that claim the same network, the one made first
// keeps it, whatever their names say.
func theForwarderMadeFirstKeeps(t *testing.T, e *env) {
	e.start()
	first := e.newGateway("bravo")
	first.launcher("virt-launcher-bravo", gwIP, true)
	first.forwarder(remote)
	first.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionTrue)

	// The apiserver keeps the time an object is made to the second.
	time.Sleep(1100 * time.Millisecond)
	second := e.newGateway("alpha")
	second.launcher("virt-launcher-alpha", gwIP2, true)
	second.forwarder("192.0.2.0/25")

	second.condition(v1alpha1.ForwarderConflict, metav1.ConditionTrue)
	second.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionFalse)
	second.condition(v1alpha1.ForwarderGranted, metav1.ConditionFalse)
	consistently(t, "the second gateway gets nothing", window, func() error {
		if got := e.routes(); !sameRoutes(got, map[string]string{remote: gwIP}) {
			return fmt.Errorf("routes = %v, want only %s to %s", got, remote, gwIP)
		}
		if e.forwards("virt-launcher-alpha") {
			return fmt.Errorf("the second gateway forwards")
		}
		return nil
	})
	first.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionTrue)
	first.condition(v1alpha1.ForwarderConflict, metav1.ConditionFalse)
}

// Contract 2: a label is written by whoever makes the pod. Only the pod KubeVirt
// runs the instance of the gateway in is let forward.
func aLabelAloneEarnsNoGrant(t *testing.T, e *env) {
	e.start()
	g := e.newGateway("branch")
	g.launcher(launcherPod, gwIP, true)
	labels := gatewayLabels("branch")
	e.pod("labelled", labels, "10.244.1.61")
	e.pod("forged-owner", labels, "10.244.1.62", func(p *corev1.Pod) {
		p.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "kubevirt.io/v1", Kind: vmiKind, Name: g.name, UID: "forged", Controller: new(true),
		}}
	})
	// Controlled by the instance, by its UID, and not one KubeVirt runs it in.
	g.launcher("not-active", "10.244.1.63", false)
	g.forwarder(remote)

	f := g.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionTrue)
	if !e.forwards(launcherPod) {
		t.Error("the gateway does not forward")
	}
	for _, name := range []string{"labelled", "forged-owner", "not-active"} {
		if e.forwards(name) {
			t.Errorf("the pod %s forwards", name)
		}
	}
	if !slices.Equal(f.Status.Forwarder.Addresses, []string{gwIP}) {
		t.Errorf("forwarder addresses = %v, want only %s", f.Status.Forwarder.Addresses, gwIP)
	}
	if len(f.Status.Scope.Addresses) != 0 {
		t.Errorf("scope = %v: a pod with the label of a gateway is no workload of the tenant", f.Status.Scope.Addresses)
	}
}

// Contract 2: a pod the instance controls is not let forward until KubeVirt lists
// it among the active pods of the instance.
func aPodWaitsForKubeVirt(t *testing.T, e *env) {
	e.start()
	g := e.newGateway("branch")
	p := g.launcher(launcherPod, gwIP, false)
	g.forwarder(remote)

	g.condition(v1alpha1.ForwarderResolved, metav1.ConditionFalse)
	consistently(t, "an unproven pod gets nothing", window, func() error {
		if r := e.routes(); len(r) != 0 {
			return fmt.Errorf("the namespace has routes %v", r)
		}
		if e.forwards(launcherPod) {
			return fmt.Errorf("the unproven pod forwards")
		}
		return nil
	})

	g.setActive(p)
	g.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionTrue)
	if !e.forwards(launcherPod) {
		t.Error("the pod does not forward once KubeVirt lists it")
	}
}

// Contract 8: a pod KubeVirt no longer runs the instance in, or that is no longer
// selected, loses the grant, and the pod that replaces it gets it.
func aPodNoLongerTheGatewayLosesTheGrant(t *testing.T, e *env) {
	e.start()
	g := e.newGateway("branch")
	p := g.launcher(launcherPod, gwIP, true)
	g.forwarder(remote)
	g.condition(v1alpha1.ForwarderGranted, metav1.ConditionTrue)
	if !e.forwards(p.Name) {
		t.Fatal("the gateway does not forward")
	}

	other := g.launcher("virt-launcher-other", gwIP2, false)
	g.setActive(other)
	eventually(t, "the pod KubeVirt left loses the grant", func() error {
		if e.forwards(p.Name) {
			return fmt.Errorf("it forwards")
		}
		return nil
	})
	eventually(t, "the pod KubeVirt runs it in now gets it", func() error {
		if !e.forwards(other.Name) {
			return fmt.Errorf("it does not forward")
		}
		return nil
	})
	eventually(t, "the routes go to the new pod", func() error {
		if got := e.routes(); !sameRoutes(got, map[string]string{remote: gwIP2}) {
			return fmt.Errorf("routes = %v, want %s to %s", got, remote, gwIP2)
		}
		return nil
	})

	// The label goes: the pod is no longer selected.
	o := e.getPod(other.Name)
	o.Labels = nil
	if err := e.c.Update(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the pod that is no longer selected loses the grant", func() error {
		if e.forwards(other.Name) {
			return fmt.Errorf("it forwards")
		}
		return nil
	})
	g.condition(v1alpha1.ForwarderResolved, metav1.ConditionFalse)
}

// Contract 8 and 9: deleting the Forwarder takes back the grant and the routes of
// it and leaves a route that is not its own, and the Forwarder goes once it has.
func deletionTakesEverythingBack(t *testing.T, e *env) {
	e.addRoute(foreign, foreignGW)
	e.start()
	g := e.newGateway("branch")
	g.launcher(launcherPod, gwIP, true)
	f := g.forwarder(remote)
	g.programmed()

	if err := e.c.Delete(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the Forwarder goes", func() error {
		err := e.c.Get(t.Context(), client.ObjectKeyFromObject(f), &v1alpha1.Forwarder{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("it is still there: %v", err)
	})
	if e.forwards(launcherPod) {
		t.Error("the gateway still forwards")
	}
	if got := e.routes(); !sameRoutes(got, map[string]string{foreign: foreignGW}) {
		t.Errorf("routes = %v, want only the route that is not the Forwarder's", got)
	}
}

// Contract 11: the pods made before the routes do not have them, and are named; a
// pod made after gets them, and is not.
func thePodsMadeBeforeTheRoutesArePending(t *testing.T, e *env) {
	e.start()
	g := e.newGateway("branch")
	g.launcher(launcherPod, gwIP, true)
	e.pod("web-0", web, webIP)
	g.forwarder(remote)
	g.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionTrue)

	pending := func(want ...string) func() error {
		return func() error {
			if got := g.get().Status.PendingWorkloads; !slices.Equal(got, want) {
				return fmt.Errorf("pending = %v, want %v", got, want)
			}
			return nil
		}
	}
	eventually(t, "the pod made before the routes is pending", pending("web-0"))

	fresh := e.pod("web-1", web, "10.244.1.6")
	if got := e.s.Network.PodRoutes(e.getPod(fresh.Name)); !sameRoutes(got, map[string]string{remote: gwIP}) {
		t.Fatalf("the network gave the new pod the routes %v", got)
	}
	consistently(t, "the pod made after the routes is not pending", window, pending("web-0"))

	if err := e.c.Delete(t.Context(), e.getPod("web-0")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "nothing is pending once the old pod is gone", pending())
}

// addressRE finds what reads as an IPv4 address, with a prefix length or not.
var addressRE = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?:/\d{1,2})?`)

// Contract 5: no condition names an address that is not the tenant's, and the
// scope has none either: not the gateway of somebody else that a route of the
// namespace goes to, nor a node.
func theStatusNamesNoAddressOfSomebodyElse(t *testing.T, e *env) {
	// The route of somebody else is to the network the tenant asks for.
	e.addRoute(remote, foreignGW)
	e.start()
	g := e.newGateway("branch")
	g.launcher(launcherPod, gwIP, true)
	nodeIP := "192.168.10.5"
	e.pod("agent", web, nodeIP, func(p *corev1.Pod) { p.Spec.HostNetwork = true })
	g.forwarder(remote)

	f := g.condition(v1alpha1.ForwarderConflict, metav1.ConditionTrue)
	g.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionFalse)
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
	if err := e.c.List(t.Context(), events, client.InNamespace(e.ns)); err != nil {
		t.Fatal(err)
	}
	for _, ev := range events.Items {
		if addressRE.MatchString(ev.Message) {
			t.Errorf("the event %s names an address: %q", ev.Reason, ev.Message)
		}
	}
	if got := e.routes(); !sameRoutes(got, map[string]string{remote: foreignGW}) {
		t.Errorf("the route of somebody else was changed: %v", got)
	}
}

// Contract 12: the connector stops and comes back while KubeVirt has not written
// the active pods of the instance yet. The gateway keeps the grant and the
// routes, and nothing is written to the pod.
func aRestartDoesNotDisturbTheGrant(t *testing.T, e *env) {
	stop := e.start()
	g := e.newGateway("branch")
	p := g.launcher(launcherPod, gwIP, true)
	g.forwarder(remote)
	g.programmed()
	podVersion := e.getPod(p.Name).ResourceVersion
	routesBefore := e.routes()

	stop()
	g.setActive()
	e.start()
	g.condition(v1alpha1.ForwarderResolved, metav1.ConditionFalse)
	unchanged := func() error {
		if v := e.getPod(p.Name).ResourceVersion; v != podVersion {
			return fmt.Errorf("the pod was written: version %s, was %s", v, podVersion)
		}
		if got := e.routes(); !sameRoutes(got, routesBefore) {
			return fmt.Errorf("routes = %v, were %v", got, routesBefore)
		}
		return nil
	}
	consistently(t, "the connector that came back leaves the gateway as it was", 2*window, unchanged)

	g.setActive(p)
	g.condition(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionTrue)
	consistently(t, "nothing is written again once KubeVirt lists the pod", window, unchanged)
}

// Contract 13: a connector that cannot read the cluster changes nothing until it
// can.
func aConnectorThatCannotReadChangesNothing(t *testing.T, e *env) {
	stop := e.start()
	g := e.newGateway("branch")
	p := g.launcher(launcherPod, gwIP, true)
	g.forwarder(remote)
	g.programmed()
	stop()

	down := &atomic.Bool{}
	down.Store(true)
	t.Cleanup(e.s.StartUnreadable(t, down))
	// What wakes the connector: the gateway is asked for nothing.
	g.ask()
	consistently(t, "a connector that cannot read takes nothing back", 2*window, func() error {
		if !e.forwards(p.Name) {
			return fmt.Errorf("the grant was taken back")
		}
		if got := e.routes(); !sameRoutes(got, map[string]string{remote: gwIP}) {
			return fmt.Errorf("routes = %v, want %s to %s", got, remote, gwIP)
		}
		return nil
	})

	down.Store(false)
	eventually(t, "the grant is taken back once the connector reads again", func() error {
		if e.forwards(p.Name) {
			return fmt.Errorf("it still forwards")
		}
		return nil
	})
}
