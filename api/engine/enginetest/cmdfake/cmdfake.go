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

// Package cmdfake simulates an appliance driven by a transactional
// command-path configuration, with a named-set source filter and peer names
// that carry a "-tunnel-N" suffix in its output. All of that stays inside this
// package. It exists to exercise the engine seam, not to be used for real.
package cmdfake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/aenix-io/site-router-operator/api/engine"
)

const schema = "v1"

var versions = []string{"2.0", "1.0"}

// Device is the simulated appliance, shared across Connect calls like a real VM.
type Device struct {
	mu           sync.Mutex
	applied      []string
	filterActive bool
	dialed       map[string]bool // raw peer name -> SA up
	minted       int

	// FailApply, FailConfirm and FailObserve make the matching call fail.
	FailApply, FailConfirm, FailObserve error
}

// NewDevice returns a device with nothing configured.
func NewDevice() *Device { return &Device{dialed: map[string]bool{}} }

// Dial marks the peer for tunnel id as having brought its SA up.
func (d *Device) Dial(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dialed[id+"-tunnel-0"] = true
}

// BreakFilter simulates the source filter vanishing from the guest.
func (d *Device) BreakFilter() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.filterActive = false
}

// mint returns credentials that differ on every call, as freshly generated ones
// do, so that a test can tell minted credentials from preserved ones.
func (d *Device) mint() map[string][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.minted++
	return map[string][]byte{"token": []byte(fmt.Sprintf("minted-token-%d", d.minted))}
}

// Engine is the engine for Dev.
type Engine struct{ Dev *Device }

// Info implements engine.Engine.
func (Engine) Info() engine.Info {
	return engine.Info{Name: "fake-cmd", ContractVersion: engine.ContractVersion, Versions: slices.Clone(versions)}
}

// Bootstrap implements engine.Engine.
func (e Engine) Bootstrap(r engine.BootstrapRequest) (engine.BootstrapPlan, error) {
	if !slices.Contains(versions, r.Version) {
		return engine.BootstrapPlan{}, fmt.Errorf("version %q is not supported (supported: %v)", r.Version, versions)
	}
	creds := r.Existing
	if creds == nil {
		creds = e.Dev.mint()
	}
	return engine.BootstrapPlan{
		Arch:           "amd64",
		DiskGiB:        12,
		SeedFiles:      map[string][]byte{engine.SeedUserData: []byte("/* config for " + r.Instance + " " + r.Version + " */")},
		Credentials:    creds,
		ManagementPort: 443,
	}, nil
}

// Connect implements engine.Engine.
func (e Engine) Connect(engine.Endpoint) (engine.Session, error) { return &session{dev: e.Dev}, nil }

type session struct{ dev *Device }

func render(d engine.Desired) []string {
	ops := make([]string, 0, 2*len(d.Tunnels)+len(d.Guards.TunnelIngress.Destinations))
	for _, t := range d.Tunnels {
		ops = append(ops, fmt.Sprintf("set vpn site-to-site peer %s-tunnel-0 remote-address %s", t.ID, t.PeerAddress))
		ops = append(ops, fmt.Sprintf("set vpn authentication psk %s secret %s", t.ID, t.PSK))
	}
	for _, dst := range d.Guards.TunnelIngress.Destinations {
		ops = append(ops, "set firewall name TUNNEL-INGRESS rule accept destination "+dst)
	}
	slices.Sort(ops)
	return ops
}

func (s *session) Apply(_ context.Context, d engine.Desired, prev string) (engine.Applied, error) {
	s.dev.mu.Lock()
	defer s.dev.mu.Unlock()
	if s.dev.FailApply != nil {
		// Real engines leak secrets in errors; the operator must redact.
		return engine.Applied{}, fmt.Errorf("%w (psk=%s)", s.dev.FailApply, firstPSK(d))
	}
	ops := render(d)
	sum := sha256.Sum256([]byte(strings.Join(ops, "\n")))
	rev := schema + ":" + hex.EncodeToString(sum[:8])
	if rev == prev && slices.Equal(s.dev.applied, ops) && s.dev.filterActive {
		return engine.Applied{Revision: rev, Changed: false}, nil
	}
	s.dev.applied = ops
	s.dev.filterActive = len(d.Guards.TunnelIngress.Destinations) > 0
	return engine.Applied{Revision: rev, Changed: true}, nil
}

func (s *session) Confirm(context.Context) (engine.GuardReport, error) {
	s.dev.mu.Lock()
	defer s.dev.mu.Unlock()
	if s.dev.FailConfirm != nil {
		return engine.GuardReport{}, s.dev.FailConfirm
	}
	if s.dev.filterActive {
		return engine.GuardReport{TunnelIngress: engine.GuardActive}, nil
	}
	return engine.GuardReport{TunnelIngress: engine.GuardDown, Detail: "named set empty"}, nil
}

func (s *session) Observe(context.Context) (engine.Observed, error) {
	s.dev.mu.Lock()
	defer s.dev.mu.Unlock()
	if s.dev.FailObserve != nil {
		return engine.Observed{}, s.dev.FailObserve
	}
	out := engine.Observed{Tunnels: map[string]engine.TunnelState{}, BGPEstablished: map[string]bool{}}
	for raw, up := range s.dev.dialed {
		id := strings.TrimSuffix(raw, "-tunnel-0") // vendor quirk stays here
		if up {
			out.Tunnels[id] = engine.TunnelUp
		} else {
			out.Tunnels[id] = engine.TunnelConnecting
		}
	}
	return out, nil
}

func firstPSK(d engine.Desired) string {
	if s := d.Secrets(); len(s) > 0 {
		return s[0]
	}
	return ""
}
