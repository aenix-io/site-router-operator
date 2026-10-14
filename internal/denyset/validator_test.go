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

package denyset

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aenix-io/site-router-operator/api/engine"
	"github.com/aenix-io/site-router-operator/internal/reconcile"
)

var testConfig = Config{PodCIDR: "10.244.0.0/16", ServiceCIDR: "10.96.0.0/16", JoinCIDR: "100.64.0.0/16"}

func newValidator(t *testing.T, objs ...client.Object) reconcile.Validator {
	t.Helper()
	r := readerOf(t, objs...)
	v, err := NewValidator(testConfig, NewDiscoverer(r, time.Minute, nil))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// A gateway as the operator would configure one for a site that is fine.
func goodInstance() reconcile.Instance {
	return reconcile.Instance{
		Name:        "a",
		RemoteCIDRs: []string{"192.0.2.0/24"},
		Desired: engine.Desired{
			Tunnels: []engine.Tunnel{{ID: "t1", PeerAddress: "203.0.113.10", RemoteSelectors: []string{"192.0.2.0/24"}, LocalSelectors: []string{"10.244.0.0/16"}}},
			Routes:  []engine.Route{{Dest: "198.51.100.0/24", NextHop: "10.244.1.1"}},
			BGP:     &engine.BGP{LocalASN: 65001, Neighbors: []engine.Neighbor{{Address: "198.51.100.1", RemoteASN: 65002}}},
			Guards: engine.Guards{TunnelIngress: engine.TunnelIngress{
				Sources:      []string{"192.0.2.0/24"},
				Destinations: []string{"10.244.1.5/32", "10.96.0.10/32"},
			}},
		},
	}
}

func rejectedWith(t *testing.T, err error) *RejectedError {
	t.Helper()
	rej, ok := errors.AsType[*RejectedError](err)
	if !ok {
		t.Fatalf("error = %v, want a *RejectedError", err)
	}
	return rej
}

func TestValidatorAcceptsAnInstanceThatIsFine(t *testing.T) {
	v := newValidator(t)
	if err := v(t.Context(), goodInstance()); err != nil {
		t.Fatal(err)
	}
}

// The destinations of the filter are pods and services of the tenant, so they
// are inside the networks of the cluster; the local selectors, the next hop and
// the address of the peer are not networks the gateway routes to or accepts
// traffic from. None of them is the deny set's to refuse.
func TestValidatorDoesNotJudgeWhatIsNotAForeignNetwork(t *testing.T) {
	v := newValidator(t)
	in := goodInstance()
	in.Desired.Guards.TunnelIngress.Destinations = []string{"10.244.1.5/32", "10.96.0.10/32", "169.254.1.1/32"}
	in.Desired.Tunnels[0].LocalSelectors = []string{"10.244.0.0/16", "10.96.0.0/16"}
	in.Desired.Tunnels[0].PeerAddress = "10.244.9.9"
	in.Desired.Routes[0].NextHop = "10.244.1.1"
	if err := v(t.Context(), in); err != nil {
		t.Fatalf("refused: %v", err)
	}
}

func TestValidatorJudgesEveryPlaceTheNetworkIsDeclaredIn(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*reconcile.Instance)
		field  Field
		value  string
	}{
		{"the remote networks", func(in *reconcile.Instance) { in.RemoteCIDRs = []string{"10.244.5.0/24"} }, FieldRemoteCIDRs, "10.244.5.0/24"},
		{"a selector of a tunnel", func(in *reconcile.Instance) { in.Desired.Tunnels[0].RemoteSelectors = []string{"10.96.1.0/24"} }, FieldRemoteCIDRs, "10.96.1.0/24"},
		{"a source of the filter", func(in *reconcile.Instance) { in.Desired.Guards.TunnelIngress.Sources = []string{"100.64.0.0/24"} }, FieldRemoteCIDRs, "100.64.0.0/24"},
		{"a static route", func(in *reconcile.Instance) {
			in.Desired.Routes = []engine.Route{{Dest: "10.244.0.0/17", NextHop: "10.0.0.1"}}
		}, FieldStaticRoute, "10.244.0.0/17"},
		{"a BGP neighbor", func(in *reconcile.Instance) { in.Desired.BGP.Neighbors[0].Address = "10.244.0.7" }, FieldBGPNeighbor, "10.244.0.7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := newValidator(t)
			in := goodInstance()
			c.mutate(&in)
			rej := rejectedWith(t, v(t.Context(), in))
			if len(rej.Rejections) != 1 || rej.Rejections[0].Field != c.field || rej.Rejections[0].Value != c.value {
				t.Errorf("rejections = %+v, want %s %q alone", rej.Rejections, c.field, c.value)
			}
		})
	}
}

func TestANetworkDeclaredInSeveralPlacesIsJudgedOnce(t *testing.T) {
	v := newValidator(t)
	in := goodInstance()
	in.RemoteCIDRs = []string{"10.244.5.0/24"}
	in.Desired.Tunnels[0].RemoteSelectors = []string{"10.244.5.0/24"}
	in.Desired.Guards.TunnelIngress.Sources = []string{"10.244.5.0/24"}
	if got := len(rejectedWith(t, v(t.Context(), in)).Rejections); got != 1 {
		t.Errorf("%d rejections for one network declared three times, want one", got)
	}
}

func TestValidatorJudgesAgainstTheNodesAndTheLoadBalancersOfTheCluster(t *testing.T) {
	node := &corev1.Node{Name: "n1", Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.0.2.77"}}}}
	lb := service("lb", corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer}, "192.0.2.88")
	for _, c := range []struct {
		name    string
		obj     client.Object
		class   Class
		address string
	}{
		{"a node", node, ClassNode, "192.0.2.77"},
		{"a load balancer", lb, ClassLoadBalancer, "192.0.2.88"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := newValidator(t, c.obj)
			// The remote network is fine until it holds the address of one.
			err := v(t.Context(), goodInstance())
			rej := rejectedWith(t, err)
			if len(rej.Rejections) == 0 || rej.Rejections[0].Class != c.class {
				t.Fatalf("rejections = %+v, want one of class %s", rej.Rejections, c.class)
			}
			// And the tenant is told the class, not which one or what address.
			if strings.Contains(err.Error(), c.address) {
				t.Errorf("the error carries the address of %s: %v", c.name, err)
			}
		})
	}
}

func TestValidatorCannotJudgeWhenTheClusterDoesNotAnswer(t *testing.T) {
	r := readerOf(t)
	boom := errors.New("the API is down")
	r.fail.Store(&boom)
	v, err := NewValidator(testConfig, NewDiscoverer(r, time.Minute, nil))
	if err != nil {
		t.Fatal(err)
	}
	got := v(t.Context(), goodInstance())
	if !errors.Is(got, reconcile.ErrValidationUnavailable) {
		t.Fatalf("error = %v, want one that wraps ErrValidationUnavailable", got)
	}
	if _, isRejection := errors.AsType[*RejectedError](got); isRejection {
		t.Error("a failure to look was reported as a rejection of the instance")
	}
	if !errors.Is(got, boom) {
		t.Error("the cause is lost: it is what the log needs")
	}
}

// A pass whose context ends while the cluster is listed cannot judge either, and
// must not be taken for a verdict.
func TestValidatorCannotJudgeWhenTheCallerIsGone(t *testing.T) {
	r := readerOf(t)
	r.gate = make(chan struct{})
	v, err := NewValidator(testConfig, NewDiscoverer(r, time.Minute, nil))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got := v(ctx, goodInstance()); !errors.Is(got, reconcile.ErrValidationUnavailable) {
		t.Errorf("error = %v, want ErrValidationUnavailable", got)
	}
}

func TestNewValidatorRefusesWhatItCannotWorkWith(t *testing.T) {
	d := NewDiscoverer(readerOf(t), time.Minute, nil)
	if _, err := NewValidator(Config{}, d); err == nil || !strings.Contains(err.Error(), "podCIDR") {
		t.Errorf("a configuration with no pod network: error = %v", err)
	}
	if _, err := NewValidator(testConfig, nil); err == nil {
		t.Error("a validator with no Discoverer was made")
	}
}

func TestSourceGivesWhatIsConfiguredAndWhatIsFound(t *testing.T) {
	node := &corev1.Node{Name: "n1", Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.0.2.77"}}}}
	lb := service("lb", corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer}, "192.0.2.88")
	s, err := NewSource(testConfig, NewDiscoverer(readerOf(t, node, lb), time.Minute, nil))
	if err != nil {
		t.Fatal(err)
	}
	nets, err := s.Networks(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(nets.Pod) != 1 || len(nets.Service) != 1 || len(nets.Join) != 1 ||
		len(nets.Nodes) != 1 || nets.Nodes[0] != "192.0.2.77/32" || len(nets.LoadBalancers) != 1 || nets.LoadBalancers[0] != "192.0.2.88/32" {
		t.Errorf("Networks = %+v", nets)
	}

	// Asked twice, it gives the same, and a caller that changes what it got does
	// not change what the next one gets.
	nets.Nodes[0] = "tampered"
	again, err := s.Networks(t.Context())
	if err != nil || again.Nodes[0] != "192.0.2.77/32" {
		t.Errorf("Networks again = %+v, %v", again, err)
	}
}

func TestSourceCannotSayWhenTheClusterDoesNotAnswer(t *testing.T) {
	r := readerOf(t)
	boom := errors.New("the API is down")
	r.fail.Store(&boom)
	s, err := NewSource(testConfig, NewDiscoverer(r, time.Minute, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Networks(t.Context()); !errors.Is(err, reconcile.ErrValidationUnavailable) || !errors.Is(err, boom) {
		t.Errorf("error = %v, want one that wraps ErrValidationUnavailable and the cause", err)
	}
}

func TestSourceRefusesWhatItCannotWorkWith(t *testing.T) {
	if _, err := NewSource(Config{}, NewDiscoverer(readerOf(t), time.Minute, nil)); err == nil {
		t.Error("a Source was made of a configuration that does not hold together")
	}
	if _, err := NewSource(testConfig, nil); err == nil {
		t.Error("a Source was made with no Discoverer")
	}
}

// A network of the cluster that cannot be read is a fault of the configuration,
// which NewSource checks, and cannot get here by that road. If one does, the
// validator cannot judge, and says so instead of refusing the tenant for it.
func TestAValidatorThatCannotReadANetworkOfTheClusterCannotJudge(t *testing.T) {
	s := &Source{configured: Networks{Pod: []string{"nonsense"}}, d: NewDiscoverer(readerOf(t), time.Minute, nil)}
	err := s.Validator()(t.Context(), goodInstance())
	if !errors.Is(err, reconcile.ErrValidationUnavailable) {
		t.Fatalf("error = %v, want one that wraps ErrValidationUnavailable", err)
	}
	if _, isRejection := errors.AsType[*RejectedError](err); isRejection {
		t.Error("a fault of the configuration was reported as a rejection of the tenant")
	}
}
