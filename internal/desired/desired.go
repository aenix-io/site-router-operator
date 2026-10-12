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

// Package desired turns a SiteRouter and what is known about its gateway into
// the state the engine is asked to put the gateway in. It is a pure function:
// what the cluster says is read by the caller and handed in.
package desired

import (
	"cmp"
	"slices"

	"github.com/aenix-io/site-router-operator/api/engine"
	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/internal/denyset"
)

// TunnelID is the identity of the tunnel of a SiteRouter in the engine, which
// reports the state of a tunnel under it. There is one tunnel to a peer.
const TunnelID = "tunnel-0"

// anyAddress is the local selector of the tunnel.
const anyAddress = "0.0.0.0/0"

// Input is what the gateway is made to be from.
type Input struct {
	Instance *v1alpha1.SiteRouter
	// TunnelAddress is the address of the Service of the tunnel, which the remote
	// end calls and which the gateway presents as its identity. Empty until the
	// load balancer has given one.
	TunnelAddress string
	// PSK is the pre-shared key of the tunnel. Empty until it can be read.
	PSK string
	// ManagementCIDR is where the management API of the gateway accepts calls from.
	ManagementCIDR string
	// Scope is the addresses the connector reports as the tenant's, which the
	// source filter lets decrypted traffic through to. They are not taken on
	// trust: Networks decides which of them the gateway is opened to.
	Scope []string
	// Networks are those of the cluster, as the deny set knows them.
	Networks denyset.Networks
	// NICMACs are the MAC addresses of the network cards of the gateway VM, from
	// which the engine finds the one that carries the tunnel.
	NICMACs []string
}

// Output is the state asked for, and what is missing from it.
type Output struct {
	Desired engine.Desired
	// RemoteCIDRs are the networks behind the remote end, in order.
	RemoteCIDRs []string
	// TunnelConfigured is false when the tunnel is not in Desired: with no peer
	// address, no key or no tunnel address there is nothing to call. The gateway
	// is configured all the same, without it, and routes nothing the tunnel
	// would have carried.
	TunnelConfigured bool
	// DroppedDestinations is how many of the addresses reported as the tenant's
	// the gateway is not opened to, for the log.
	DroppedDestinations int
}

// Build returns the state the gateway is asked to be in. Two inputs that mean the
// same give the same Output, whatever order their lists came in, so that the
// engine can tell a change from a reordering.
//
// The error is for a network of the cluster that does not parse.
func Build(in Input) (Output, error) {
	spec := in.Instance.Spec

	remote := make([]string, len(spec.RemoteCIDRs))
	for i, c := range spec.RemoteCIDRs {
		remote[i] = string(c)
	}
	slices.Sort(remote)

	destinations, dropped, err := denyset.Destinations(in.Scope, in.Networks)
	if err != nil {
		return Output{}, err
	}

	d := engine.Desired{
		Guards: engine.Guards{
			ManagementAllow: in.ManagementCIDR,
			TunnelIngress:   engine.TunnelIngress{Sources: remote, Destinations: destinations},
		},
		TunnelNICMACs: slices.Sorted(slices.Values(in.NICMACs)),
	}

	out := Output{RemoteCIDRs: remote, DroppedDestinations: dropped}
	if spec.Peer.Address != "" && in.PSK != "" && in.TunnelAddress != "" {
		d.Tunnels = []engine.Tunnel{{
			ID:              TunnelID,
			PeerAddress:     spec.Peer.Address,
			PSK:             in.PSK,
			LocalID:         in.TunnelAddress,
			LocalSelectors:  []string{anyAddress},
			RemoteSelectors: remote,
		}}
		out.TunnelConfigured = true
	}

	for _, r := range spec.StaticRoutes {
		d.Routes = append(d.Routes, engine.Route{Dest: string(r.Destination), NextHop: string(r.NextHop)})
	}
	slices.SortFunc(d.Routes, func(a, b engine.Route) int {
		if c := cmp.Compare(a.Dest, b.Dest); c != 0 {
			return c
		}
		return cmp.Compare(a.NextHop, b.NextHop)
	})

	// The schema refuses BGP with no local AS, so one is there; what is not is
	// not guessed.
	if spec.BGP.Enabled && spec.BGP.LocalASN != nil {
		bgp := &engine.BGP{LocalASN: uint32(*spec.BGP.LocalASN)}
		for _, n := range spec.BGP.Neighbors {
			bgp.Neighbors = append(bgp.Neighbors, engine.Neighbor{Address: string(n.Address), RemoteASN: uint32(n.RemoteASN)})
		}
		slices.SortFunc(bgp.Neighbors, func(a, b engine.Neighbor) int { return cmp.Compare(a.Address, b.Address) })
		d.BGP = bgp
	}

	out.Desired = d
	return out, nil
}
