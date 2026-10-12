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
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// countingReader counts the lists, and can hold them back or make them fail.
type countingReader struct {
	client.Reader
	lists atomic.Int64
	// gate, when set, is waited on before each list answers.
	gate chan struct{}
	// fail, when set, is returned by lists instead of an answer.
	fail atomic.Pointer[error]
}

func (r *countingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	r.lists.Add(1)
	if r.gate != nil {
		select {
		case <-r.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := r.fail.Load(); err != nil {
		return *err
	}
	return r.Reader.List(ctx, list, opts...)
}

func node(name string, addrs ...corev1.NodeAddress) *corev1.Node {
	return &corev1.Node{Name: name, Status: corev1.NodeStatus{Addresses: addrs}}
}

func service(name string, spec corev1.ServiceSpec, ingress ...string) *corev1.Service {
	s := &corev1.Service{Name: name, Namespace: "default", Spec: spec}
	for _, ip := range ingress {
		s.Status.LoadBalancer.Ingress = append(s.Status.LoadBalancer.Ingress, corev1.LoadBalancerIngress{IP: ip})
	}
	return s
}

func readerOf(t *testing.T, objs ...client.Object) *countingReader {
	t.Helper()
	return &countingReader{Reader: fake.NewClientBuilder().WithObjects(objs...).Build()}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestDiscoverFindsTheAddressesOfNodesAndLoadBalancers(t *testing.T) {
	r := readerOf(t,
		node("a",
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "192.168.100.5"},
			corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: "198.51.100.5"},
			corev1.NodeAddress{Type: corev1.NodeHostName, Address: "a.example"},
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "fd00::5"},
		),
		node("b", corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "192.168.100.6"}),
		// The same address twice, on two nodes.
		node("c", corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "192.168.100.5"}),
		service("lb", corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, LoadBalancerIP: "203.0.113.20"}, "203.0.113.10", "2001:db8::10"),
		service("ext", corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.96.0.5", ExternalIPs: []string{"203.0.113.30"}}),
		service("plain", corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.96.0.6"}),
		// A ClusterIP service with a stray status is not a load balancer.
		service("stray", corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP}, "203.0.113.99"),
		service("junk", corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer}, "not-an-address"),
	)
	got, err := NewDiscoverer(r, time.Minute, nil).Discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantNodes := []string{"192.168.100.5/32", "192.168.100.6/32", "198.51.100.5/32"}
	wantLBs := []string{"203.0.113.10/32", "203.0.113.20/32", "203.0.113.30/32"}
	if !slices.Equal(got.Nodes, wantNodes) {
		t.Errorf("nodes = %v, want %v", got.Nodes, wantNodes)
	}
	if !slices.Equal(got.LoadBalancers, wantLBs) {
		t.Errorf("load balancers = %v, want %v", got.LoadBalancers, wantLBs)
	}
}

func TestDiscoverOfAnEmptyClusterIsEmpty(t *testing.T) {
	got, err := NewDiscoverer(readerOf(t), time.Minute, nil).Discover(t.Context())
	if err != nil || len(got.Nodes) != 0 || len(got.LoadBalancers) != 0 {
		t.Errorf("got %+v, %v; want nothing", got, err)
	}
}

func TestDiscoverIsKeptForTheTTLAndListedAgainAfterIt(t *testing.T) {
	r := readerOf(t, node("a", corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "192.168.100.5"}))
	clk := &clock{t: time.Unix(1_000_000, 0)}
	d := NewDiscoverer(r, time.Minute, clk.now)

	if _, err := d.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := r.lists.Load(); got != 2 {
		t.Fatalf("the first discovery made %d lists, want one of nodes and one of services", got)
	}

	clk.advance(59 * time.Second)
	if _, err := d.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := r.lists.Load(); got != 2 {
		t.Errorf("a discovery within the ttl made %d lists in all, want the cache to answer", got)
	}

	clk.advance(2 * time.Second)
	if _, err := d.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := r.lists.Load(); got != 4 {
		t.Errorf("a discovery after the ttl made %d lists in all, want it listed again", got)
	}
}

func TestPassesThatArriveTogetherShareOneListing(t *testing.T) {
	r := readerOf(t, node("a", corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "192.168.100.5"}))
	r.gate = make(chan struct{})
	d := NewDiscoverer(r, time.Minute, nil)

	const callers = 20
	var wg sync.WaitGroup
	results := make([]Snapshot, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Go(func() { results[i], errs[i] = d.Discover(t.Context()) })
	}
	// Let them all get to the wait, then answer the listing.
	deadline := time.Now().Add(5 * time.Second)
	for r.lists.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(r.gate)
	wg.Wait()

	if got := r.lists.Load(); got != 2 {
		t.Errorf("%d lists for %d passes, want one listing of two lists", got, callers)
	}
	for i := range callers {
		if errs[i] != nil || !slices.Equal(results[i].Nodes, []string{"192.168.100.5/32"}) {
			t.Errorf("pass %d got %+v, %v", i, results[i], errs[i])
		}
	}
}

func TestAPassThatGivesUpDoesNotFailTheOthers(t *testing.T) {
	r := readerOf(t, node("a", corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "192.168.100.5"}))
	r.gate = make(chan struct{})
	d := NewDiscoverer(r, time.Minute, nil)

	impatient, cancel := context.WithCancel(t.Context())
	first := make(chan error, 1)
	go func() { _, err := d.Discover(impatient); first <- err }()
	for r.lists.Load() == 0 {
		time.Sleep(time.Millisecond)
	}

	patient := make(chan error, 1)
	var got Snapshot
	go func() {
		var err error
		got, err = d.Discover(t.Context())
		patient <- err
	}()
	time.Sleep(50 * time.Millisecond)

	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Errorf("the pass that gave up got %v, want context.Canceled", err)
	}
	close(r.gate)
	if err := <-patient; err != nil {
		t.Fatalf("the pass that waited got %v", err)
	}
	if !slices.Equal(got.Nodes, []string{"192.168.100.5/32"}) {
		t.Errorf("the pass that waited got %+v", got)
	}
}

func TestAFailedListingIsAnErrorAndIsNotKept(t *testing.T) {
	r := readerOf(t, node("a", corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "192.168.100.5"}))
	boom := errors.New("the API is down")
	r.fail.Store(&boom)
	d := NewDiscoverer(r, time.Minute, nil)

	if _, err := d.Discover(t.Context()); !errors.Is(err, boom) {
		t.Fatalf("error = %v, want one that wraps %v", err, boom)
	}

	r.fail.Store(nil)
	got, err := d.Discover(t.Context())
	if err != nil {
		t.Fatalf("the failure was kept: %v", err)
	}
	if !slices.Equal(got.Nodes, []string{"192.168.100.5/32"}) {
		t.Errorf("got %+v", got)
	}
}

func TestAFailureToListServicesFailsTheWholeListing(t *testing.T) {
	// The listing is of nodes and services together: half of it would leave the
	// deny set without the load balancers and say nothing about it.
	r := &failingOn{Reader: fake.NewClientBuilder().Build(), kind: "ServiceList"}
	if _, err := NewDiscoverer(r, time.Minute, nil).Discover(t.Context()); err == nil {
		t.Fatal("a listing without the services was returned")
	}
}

type failingOn struct {
	client.Reader
	kind string
}

func (f *failingOn) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*corev1.ServiceList); ok && f.kind == "ServiceList" {
		return errors.New("no services for you")
	}
	return f.Reader.List(ctx, list, opts...)
}

func TestWhatDiscoverReturnsIsNotTheCache(t *testing.T) {
	r := readerOf(t, node("a", corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "192.168.100.5"}))
	d := NewDiscoverer(r, time.Minute, nil)

	// The first answer comes from a listing, the second from the cache, and
	// neither may let its caller change what the next one gets.
	for i := range 3 {
		got, err := d.Discover(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got.Nodes, []string{"192.168.100.5/32"}) {
			t.Fatalf("answer %d: a caller changed what the next one got: %v", i, got.Nodes)
		}
		got.Nodes[0] = "tampered"
	}
}

func TestDiscoverReturnsWhenTheCallerIsAlreadyGone(t *testing.T) {
	r := readerOf(t)
	r.gate = make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := NewDiscoverer(r, time.Minute, nil).Discover(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}
