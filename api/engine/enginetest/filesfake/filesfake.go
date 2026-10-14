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

// Package filesfake simulates a plain-distribution appliance: connection files
// plus a firewall ruleset applied in two separate steps (no single commit),
// tunnels keyed by connection name, and the ruleset read back for
// confirmation. It is deliberately unlike package cmdfake and exists to
// exercise the engine seam, not to be used for real.
package filesfake

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

const schema = "files1"

var versions = []string{"3.1", "3.0"}

// Device is the simulated appliance, shared across Connect calls like a real VM.
type Device struct {
	mu      sync.Mutex
	ruleset string
	conns   string
	sas     map[string]bool // connection name == tunnel ID
	minted  int

	// FailApply, FailConfirm and FailObserve make the matching call fail.
	FailApply, FailConfirm, FailObserve error
}

// NewDevice returns a device with nothing configured.
func NewDevice() *Device { return &Device{sas: map[string]bool{}} }

// Dial marks the connection for tunnel id as established.
func (d *Device) Dial(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sas[id] = true
}

// BreakFilter simulates the accept rules vanishing from the guest.
func (d *Device) BreakFilter() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ruleset = strings.ReplaceAll(d.ruleset, "accept", "")
}

// mint returns credentials that differ on every call, as freshly generated ones
// do, so that a test can tell minted credentials from preserved ones.
func (d *Device) mint() map[string][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.minted++
	return map[string][]byte{"controller-key": []byte(fmt.Sprintf("minted-key-%d", d.minted))}
}

// Engine is the engine for Dev.
type Engine struct{ Dev *Device }

// Info implements engine.Engine.
func (Engine) Info() engine.Info {
	return engine.Info{Name: "fake-files", ContractVersion: engine.ContractVersion, Versions: slices.Clone(versions)}
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
		DiskGiB:        8,
		SeedFiles:      map[string][]byte{engine.SeedUserData: []byte("#cloud-config\n# " + r.Instance + " " + r.Version)},
		Credentials:    creds,
		ManagementPort: 22,
	}, nil
}

// Connect implements engine.Engine.
func (e Engine) Connect(engine.Endpoint) (engine.Session, error) { return &session{dev: e.Dev}, nil }

type session struct{ dev *Device }

func (s *session) Apply(_ context.Context, d engine.Desired, prev string) (engine.Applied, error) {
	s.dev.mu.Lock()
	defer s.dev.mu.Unlock()
	if s.dev.FailApply != nil {
		return engine.Applied{}, fmt.Errorf("%w (loading connections failed for %v)", s.dev.FailApply, d.Secrets())
	}
	var rules, conns []string
	for _, dst := range d.Guards.TunnelIngress.Destinations {
		rules = append(rules, "ip daddr "+dst+" accept")
	}
	for _, t := range d.Tunnels {
		conns = append(conns, fmt.Sprintf("conn %s { remote_addrs=%s psk=%s }", t.ID, t.PeerAddress, t.PSK))
	}
	slices.Sort(rules)
	slices.Sort(conns)
	rs, cs := "chain ingress-allow {"+strings.Join(rules, ";")+"}", strings.Join(conns, "\n")
	sum := sha256.Sum256([]byte(rs + "|" + cs))
	rev := schema + ":" + hex.EncodeToString(sum[:8])
	if rev == prev && rs == s.dev.ruleset && cs == s.dev.conns {
		return engine.Applied{Revision: rev, Changed: false}, nil
	}
	// The guard goes in before the tunnels so there is never a tunnel without it.
	s.dev.ruleset = rs
	s.dev.conns = cs
	return engine.Applied{Revision: rev, Changed: true}, nil
}

func (s *session) Confirm(context.Context) (engine.GuardReport, error) {
	s.dev.mu.Lock()
	defer s.dev.mu.Unlock()
	if s.dev.FailConfirm != nil {
		return engine.GuardReport{}, s.dev.FailConfirm
	}
	if strings.Contains(s.dev.ruleset, "accept") {
		return engine.GuardReport{TunnelIngress: engine.GuardActive}, nil
	}
	return engine.GuardReport{TunnelIngress: engine.GuardDown, Detail: "ingress-allow has no accept rule"}, nil
}

func (s *session) Observe(context.Context) (engine.Observed, error) {
	s.dev.mu.Lock()
	defer s.dev.mu.Unlock()
	if s.dev.FailObserve != nil {
		return engine.Observed{}, s.dev.FailObserve
	}
	out := engine.Observed{Tunnels: map[string]engine.TunnelState{}, BGPEstablished: map[string]bool{}}
	for id, up := range s.dev.sas {
		if up {
			out.Tunnels[id] = engine.TunnelUp
		}
	}
	return out, nil
}
