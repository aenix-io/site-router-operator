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

// Package enginetest holds the conformance suite that every engine must pass.
// An engine implementation runs it from its own tests against a simulated
// guest; the operator's tests run it against the fakes in the subpackages.
package enginetest

import (
	"testing"

	"github.com/aenix-io/site-router-operator/api/engine"
)

// Rig is an engine together with the means to act on the guest behind it. The
// engine under test cannot do these things itself, they stand for what happens
// to a real appliance.
type Rig struct {
	Engine   engine.Engine
	Endpoint engine.Endpoint
	// Dial makes the remote peer of the tunnel with this ID bring its SA up.
	Dial func(id string)
	// BreakFilter removes the source filter from the guest behind the engine's
	// back, as a reboot or a manual change would.
	BreakFilter func()
}

const (
	tunnelID = "t1"
	version  = "conformance"
)

func desired(destinations ...string) engine.Desired {
	return engine.Desired{
		Tunnels: []engine.Tunnel{{
			ID:              tunnelID,
			PeerAddress:     "203.0.113.10",
			PSK:             "conformance-psk",
			LocalID:         "198.51.100.1",
			RemoteSelectors: []string{"192.0.2.0/24"},
		}},
		Guards: engine.Guards{TunnelIngress: engine.TunnelIngress{
			Sources:      []string{"192.0.2.0/24"},
			Destinations: destinations,
		}},
	}
}

// Run runs the conformance suite. newRig is called once per subtest and must
// return a rig on a fresh guest.
func Run(t *testing.T, newRig func(t *testing.T) Rig) {
	t.Helper()
	for name, test := range map[string]func(*testing.T, Rig){
		"InfoDeclaresVersions":            testInfo,
		"BootstrapPerVersion":             testBootstrapPerVersion,
		"BootstrapRejectsUnknownVersion":  testBootstrapUnknownVersion,
		"BootstrapKeepsCredentials":       testBootstrapKeepsCredentials,
		"ApplyIsIdempotent":               testApplyIdempotent,
		"ApplyHealsDrift":                 testApplyHealsDrift,
		"ConfirmReadsLiveState":           testConfirmLive,
		"EmptyDestinationsFailClosed":     testFailClosed,
		"ObserveIsKeyedByOperatorTunnels": testObserveKeys,
	} {
		t.Run(name, func(t *testing.T) { test(t, newRig(t)) })
	}
}

func connect(t *testing.T, r Rig) engine.Session {
	t.Helper()
	s, err := r.Engine.Connect(r.Endpoint)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return s
}

func apply(t *testing.T, s engine.Session, d engine.Desired, prev string) engine.Applied {
	t.Helper()
	a, err := s.Apply(t.Context(), d, prev)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return a
}

// confirmed reports whether the engine vouches for the source filter. An error
// from Confirm is "not confirmed", the same as the operator treats it.
func confirmed(t *testing.T, s engine.Session) bool {
	t.Helper()
	rep, err := s.Confirm(t.Context())
	return err == nil && rep.TunnelIngress == engine.GuardActive
}

func testInfo(t *testing.T, r Rig) {
	info := r.Engine.Info()
	if info.Name == "" {
		t.Error("Info().Name is empty")
	}
	if info.ContractVersion != engine.ContractVersion {
		t.Errorf("Info().ContractVersion = %d, want %d", info.ContractVersion, engine.ContractVersion)
	}
	if len(info.Versions) == 0 {
		t.Fatal("Info().Versions is empty: the engine drives no image")
	}
	seen := map[string]bool{}
	for _, v := range info.Versions {
		if v == "" || seen[v] {
			t.Errorf("Info().Versions has an empty or repeated entry %q: %v", v, info.Versions)
		}
		seen[v] = true
	}
}

func testBootstrapPerVersion(t *testing.T, r Rig) {
	for _, v := range r.Engine.Info().Versions {
		p, err := r.Engine.Bootstrap(engine.BootstrapRequest{Instance: "a", Namespace: "ns", Version: v})
		if err != nil {
			t.Fatalf("Bootstrap(%s): %v", v, err)
		}
		if p.Arch == "" || p.DiskGiB <= 0 || p.ManagementPort <= 0 || len(p.SeedFiles) == 0 || len(p.Credentials) == 0 {
			t.Errorf("%s: incomplete plan: %+v", v, p)
		}
	}
}

func testBootstrapUnknownVersion(t *testing.T, r Rig) {
	if _, err := r.Engine.Bootstrap(engine.BootstrapRequest{Instance: "a", Namespace: "ns", Version: version}); err == nil {
		t.Errorf("Bootstrap accepted the version %q that Info() does not list", version)
	}
	if _, err := r.Engine.Bootstrap(engine.BootstrapRequest{Instance: "a", Namespace: "ns"}); err == nil {
		t.Error("Bootstrap accepted a request without a version")
	}
}

func testBootstrapKeepsCredentials(t *testing.T, r Rig) {
	v := r.Engine.Info().Versions[0]
	first, err := r.Engine.Bootstrap(engine.BootstrapRequest{Instance: "a", Namespace: "ns", Version: v})
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.Engine.Bootstrap(engine.BootstrapRequest{Instance: "a", Namespace: "ns", Version: v, Existing: first.Credentials})
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range first.Credentials {
		if got := again.Credentials[k]; string(got) != string(want) {
			t.Errorf("credential %q was not preserved across Bootstrap", k)
		}
	}
}

func testApplyIdempotent(t *testing.T, r Rig) {
	s := connect(t, r)
	first := apply(t, s, desired("10.244.1.5/32"), "")
	if first.Revision == "" || !first.Changed {
		t.Fatalf("first Apply = %+v, want a revision and Changed", first)
	}
	again := apply(t, s, desired("10.244.1.5/32"), first.Revision)
	if again.Changed || again.Revision != first.Revision {
		t.Errorf("identical Apply = %+v, want Changed=false and revision %q", again, first.Revision)
	}
	next := apply(t, s, desired("10.244.1.5/32", "10.244.1.6/32"), again.Revision)
	if !next.Changed || next.Revision == first.Revision {
		t.Errorf("Apply of a changed desired state = %+v, want Changed and a new revision", next)
	}
}

func testApplyHealsDrift(t *testing.T, r Rig) {
	s := connect(t, r)
	first := apply(t, s, desired("10.244.1.5/32"), "")
	r.BreakFilter()
	healed := apply(t, s, desired("10.244.1.5/32"), first.Revision)
	if !healed.Changed {
		t.Error("Apply with a matching revision on a drifted guest reported Changed=false: the engine trusts its stored state")
	}
	if !confirmed(t, s) {
		t.Error("the filter is not active after Apply healed the drift")
	}
}

func testConfirmLive(t *testing.T, r Rig) {
	s := connect(t, r)
	apply(t, s, desired("10.244.1.5/32"), "")
	if !confirmed(t, s) {
		t.Fatal("the filter is not active right after Apply")
	}
	r.BreakFilter()
	if confirmed(t, s) {
		t.Error("Confirm still vouches for the filter after it was removed from the guest: it reads stored state, not the live guest")
	}
}

func testFailClosed(t *testing.T, r Rig) {
	s := connect(t, r)
	apply(t, s, desired(), "")
	if confirmed(t, s) {
		t.Error("with no destinations the engine confirmed a filter that admits flows: it must fail closed")
	}
}

func testObserveKeys(t *testing.T, r Rig) {
	s := connect(t, r)
	apply(t, s, desired("10.244.1.5/32"), "")
	before, err := s.Observe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if before.Tunnels[tunnelID] == engine.TunnelUp {
		t.Error("the tunnel is Up before the peer dialed")
	}
	r.Dial(tunnelID)
	after, err := s.Observe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after.Tunnels[tunnelID] != engine.TunnelUp {
		t.Errorf("tunnel %q = %q after the peer dialed, want Up", tunnelID, after.Tunnels[tunnelID])
	}
	for id := range after.Tunnels {
		if id != tunnelID {
			t.Errorf("Observe reports %q, which is not an operator tunnel ID: vendor naming leaked out", id)
		}
	}
}
