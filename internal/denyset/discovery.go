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
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"golang.org/x/sync/singleflight"
)

// Snapshot is the addresses of the cluster that only the cluster can say: the
// nodes and the load balancers. Each is an IPv4 address in the notation of a
// network, its /32, so that a value that holds one is refused even when it does
// not hold the rest of its network.
type Snapshot struct {
	Nodes         []string
	LoadBalancers []string
}

// Discoverer lists the addresses of the nodes and of the load balancers.
//
// What it finds is the same for every gateway of the cluster, so it is listed
// once for all of them and kept for a short time. Without that, every gateway
// would list every Node and every Service on every pass. Passes that arrive
// while a listing is under way wait for it instead of starting another.
//
// A listing that fails is an error and is not kept. The validator treats it as
// "cannot judge now" and leaves the routes as they are; guessing from an older
// listing would leave the deny set out of date for as long as the API is down.
type Discoverer struct {
	reader  client.Reader
	ttl     time.Duration
	timeout time.Duration
	now     func() time.Time

	mu    sync.Mutex
	taken time.Time
	snap  Snapshot

	flight singleflight.Group
}

// NewDiscoverer returns a Discoverer that keeps a listing for ttl. The now
// function is the clock, and is time.Now when nil.
func NewDiscoverer(reader client.Reader, ttl time.Duration, now func() time.Time) *Discoverer {
	if now == nil {
		now = time.Now
	}
	return &Discoverer{reader: reader, ttl: ttl, timeout: 30 * time.Second, now: now}
}

// Discover returns the addresses, from the cache when it is younger than the
// ttl. The caller can give up while a listing is under way without cancelling it
// for the others that wait for it.
func (d *Discoverer) Discover(ctx context.Context) (Snapshot, error) {
	if s, ok := d.cached(); ok {
		return s, nil
	}
	call := d.flight.DoChan("discover", func() (any, error) {
		// Another pass may have finished a listing while this one waited.
		if s, ok := d.cached(); ok {
			return s, nil
		}
		// The listing belongs to everyone who waits for it, so it does not end
		// when the one who started it gives up.
		lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.timeout)
		defer cancel()
		s, err := d.list(lctx)
		if err != nil {
			return Snapshot{}, err
		}
		d.mu.Lock()
		d.snap, d.taken = s, d.now()
		d.mu.Unlock()
		return s, nil
	})
	select {
	case r := <-call:
		if r.Err != nil {
			return Snapshot{}, r.Err
		}
		return r.Val.(Snapshot).clone(), nil
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	}
}

func (d *Discoverer) cached() (Snapshot, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.taken.IsZero() || d.now().Sub(d.taken) >= d.ttl {
		return Snapshot{}, false
	}
	return d.snap.clone(), true
}

func (s Snapshot) clone() Snapshot {
	return Snapshot{Nodes: slices.Clone(s.Nodes), LoadBalancers: slices.Clone(s.LoadBalancers)}
}

func (d *Discoverer) list(ctx context.Context) (Snapshot, error) {
	var s Snapshot

	nodes := &corev1.NodeList{}
	if err := d.reader.List(ctx, nodes); err != nil {
		return Snapshot{}, fmt.Errorf("listing the nodes: %w", err)
	}
	for _, n := range nodes.Items {
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP || a.Type == corev1.NodeExternalIP {
				s.Nodes = appendHost(s.Nodes, a.Address)
			}
		}
	}

	services := &corev1.ServiceList{}
	if err := d.reader.List(ctx, services); err != nil {
		return Snapshot{}, fmt.Errorf("listing the services: %w", err)
	}
	for _, svc := range services.Items {
		for _, ip := range svc.Spec.ExternalIPs {
			s.LoadBalancers = appendHost(s.LoadBalancers, ip)
		}
		if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
			continue
		}
		s.LoadBalancers = appendHost(s.LoadBalancers, svc.Spec.LoadBalancerIP)
		for _, in := range svc.Status.LoadBalancer.Ingress {
			s.LoadBalancers = appendHost(s.LoadBalancers, in.IP)
		}
	}

	s.Nodes = sortedUnique(s.Nodes)
	s.LoadBalancers = sortedUnique(s.LoadBalancers)
	return s, nil
}

// appendHost adds the /32 of an IPv4 address. Anything else is left out: the
// deny set judges IPv4 only, and an address that does not parse is not one.
func appendHost(dst []string, raw string) []string {
	a, err := netip.ParseAddr(raw)
	if err != nil || !a.Is4() {
		return dst
	}
	return append(dst, netip.PrefixFrom(a, a.BitLen()).String())
}

func sortedUnique(values []string) []string {
	return slices.Compact(slices.Sorted(slices.Values(values)))
}
