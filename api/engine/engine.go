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

// Package engine is the seam between the operator and whatever runs inside the
// gateway VM. The operator imports only this package; engine implementations
// import it too and make themselves available with Register.
package engine

import (
	"context"
	"maps"
	"slices"
)

// ContractVersion is the version of the contract the types and interfaces of
// this package describe. An engine reports the version it implements in Info.
const ContractVersion = 1

// Info describes an engine.
type Info struct {
	Name            string
	ContractVersion int
	// Versions lists the appliance image versions the engine can drive. The
	// operator checks the versions it is configured with against this list.
	Versions []string
}

// Tunnel is one IPsec tunnel to a remote site.
type Tunnel struct {
	ID              string // operator-chosen, stable across renders
	PeerAddress     string
	PSK             string
	LocalID         string
	LocalSelectors  []string
	RemoteSelectors []string
}

// Route is a static route inside the gateway.
type Route struct{ Dest, NextHop string }

// Neighbor is a BGP neighbor.
type Neighbor struct {
	Address   string
	RemoteASN uint32
}

// BGP is the BGP configuration of the gateway.
type BGP struct {
	LocalASN  uint32
	Neighbors []Neighbor
}

// TunnelIngress is the source-and-destination allow list for decrypted traffic.
// Empty Destinations means no new flows are admitted (fail closed).
type TunnelIngress struct{ Sources, Destinations []string }

// Guards holds the filters that protect the gateway and the tenant network.
type Guards struct {
	ManagementAllow string
	TunnelIngress   TunnelIngress
}

// Desired is the state the operator wants the gateway to be in.
type Desired struct {
	Tunnels       []Tunnel
	Routes        []Route
	BGP           *BGP
	Guards        Guards
	TunnelNICMACs []string
}

// Secrets lists values that must never reach logs, events or conditions.
func (d Desired) Secrets() []string {
	var out []string
	for _, t := range d.Tunnels {
		if t.PSK != "" {
			out = append(out, t.PSK)
		}
	}
	return out
}

// Applied is the outcome of Apply.
type Applied struct {
	// Revision is opaque to the operator: a hash over the engine's own resolved
	// render plus its schema version. The operator stores it and hands it back.
	Revision string
	Changed  bool
}

// GuardState is the state of a filter as read from the guest.
type GuardState string

// Guard states. Anything but GuardActive counts as "not confirmed".
const (
	GuardUnknown GuardState = "Unknown"
	GuardActive  GuardState = "Active"
	GuardDown    GuardState = "Down"
)

// GuardReport is the outcome of Confirm.
type GuardReport struct {
	TunnelIngress GuardState
	Detail        string
}

// TunnelState is the state of a tunnel as read from the guest.
type TunnelState string

// Tunnel states.
const (
	TunnelDown       TunnelState = "Down"
	TunnelConnecting TunnelState = "Connecting"
	TunnelUp         TunnelState = "Up"
)

// Observed is what the guest reports about itself.
type Observed struct {
	Tunnels        map[string]TunnelState // keyed by Tunnel.ID
	BGPEstablished map[string]bool        // keyed by neighbor address
}

// Endpoint is where and how to reach the management API of a gateway.
type Endpoint struct {
	Address string
	// Credentials are opaque to the operator. Every value is treated as a
	// secret and redacted from anything that leaves the operator, so an engine
	// puts only what must stay private in here.
	Credentials map[string][]byte
}

// Secrets lists the credential values, in key order.
func (e Endpoint) Secrets() []string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(e.Credentials)) {
		if v := e.Credentials[k]; len(v) > 0 {
			out = append(out, string(v))
		}
	}
	return out
}

// BootstrapRequest asks for the artifacts needed to create a gateway.
type BootstrapRequest struct {
	Instance        string
	Namespace       string
	ManagementAllow string
	// Version is one of Info.Versions. Image and seed are returned as a pair for
	// this version.
	Version string
	// Existing holds credentials minted earlier; Bootstrap must keep them.
	Existing map[string][]byte
}

// BootstrapPlan is what the operator needs to create the gateway VM.
type BootstrapPlan struct {
	Image          string // digest pinned
	Arch           string
	DiskGiB        int
	SeedFiles      map[string][]byte
	Credentials    map[string][]byte
	ManagementPort int
}

// Engine knows one kind of appliance.
type Engine interface {
	Info() Info
	// Bootstrap returns the image, seed and credentials for the version in the
	// request. It mints credentials only when the request carries none; the
	// ones in BootstrapRequest.Existing come back unchanged.
	Bootstrap(BootstrapRequest) (BootstrapPlan, error)
	Connect(Endpoint) (Session, error)
}

// Session talks to one gateway.
type Session interface {
	// Apply atomically replaces the managed parts of the guest configuration.
	// When prevRevision equals the revision of d and the guest still matches
	// it, Applied.Changed is false; when the guest has drifted, Apply applies
	// again and reports Changed.
	Apply(ctx context.Context, d Desired, prevRevision string) (Applied, error)
	// Confirm checks the source filter on the live guest, not on stored state.
	Confirm(ctx context.Context) (GuardReport, error)
	// Observe reports tunnels keyed by Tunnel.ID; vendor naming stays inside
	// the engine.
	Observe(ctx context.Context) (Observed, error)
}
