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

// Package reconcile holds the part of the reconcile loop that talks to the
// gateway: the order of the steps, the gating between them and the redaction of
// what comes back. It knows the engine only through package engine.
package reconcile

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/aenix-io/site-router-operator/api/engine"
	"github.com/aenix-io/site-router-operator/api/v1alpha1"
)

// Condition types set by Step: the ones of the SiteRouter API.
const (
	CondValidated         = v1alpha1.ConditionValidated
	CondConfigured        = v1alpha1.ConditionConfigured
	CondRoutesProgrammed  = v1alpha1.ConditionRoutesProgrammed
	CondTunnelEstablished = v1alpha1.ConditionTunnelEstablished
)

// Condition reasons set by Step. Those that are False reasons of the API are
// taken from it; the rest are what Step says while everything is fine, or a
// stand-in for what the Forwarder API will report.
const (
	ReasonInvalidRemoteCIDR     = v1alpha1.ReasonInvalidRemoteCIDR
	ReasonValidationUnavailable = v1alpha1.ReasonValidationUnavailable
	ReasonConfigureFailed       = v1alpha1.ReasonConfigureFailed
	ReasonSourceFilterPending   = v1alpha1.ReasonSourceFilterPending

	ReasonAccepted      = "Accepted"
	ReasonConfigApplied = "ConfigApplied"
	ReasonProgrammed    = "Programmed"
	ReasonRoutesFailed  = "RoutesFailed"
	ReasonObserved      = "Observed"
	ReasonObserveFailed = "ObserveFailed"
)

// Forwarder is what Step needs from the network seam: routes for the remote
// CIDRs to the gateway, and their withdrawal. It is a narrow stand-in; the
// Forwarder API replaces it.
type Forwarder interface {
	ApplyRoutes(ctx context.Context, instance string, cidrs []string, gateway string) error
	WithdrawRoutes(ctx context.Context, instance string) error
}

// Validator rejects an instance before anything reaches the engine or the
// network. Its error becomes the Validated condition, and is for a tenant to
// read. Step applies in.Desired to the engine and in.RemoteCIDRs to the network
// without checking them against each other, so a validator has to vet both, not
// only the CIDR list.
//
// An error that wraps ErrValidationUnavailable says the validator could not
// judge, not that the instance is wrong.
type Validator func(context.Context, Instance) error

// ErrValidationUnavailable is wrapped by a Validator that cannot judge now, for
// instance because the cluster does not answer. The instance is not refused: the
// routes it has stay as they are, and nothing reaches the engine, until the
// validator can judge again. A failure to look is no reason to break a tunnel.
var ErrValidationUnavailable = errors.New("validation is not available")

// Instance is the input of one pass for one gateway.
type Instance struct {
	Name        string
	GatewayIP   string
	RemoteCIDRs []string
	Desired     engine.Desired
}

// State is what the operator remembers between passes of one instance.
// Revision is opaque: it is whatever the engine returned from Apply.
type State struct{ Revision string }

// Condition is the outcome of one stage of a pass.
type Condition struct {
	OK      bool
	Reason  string
	Message string
}

// Result is the outcome of a pass.
type Result struct {
	Conditions map[string]Condition
	Tunnels    map[string]engine.TunnelState
	// Err is why the pass could not go on, for the log, where a condition says it
	// in words for a tenant. It is redacted like a condition, and set only when
	// the two differ.
	Err error
}

// Stepper runs passes.
type Stepper struct {
	engine    engine.Engine
	forwarder Forwarder
	validate  Validator
}

// New returns a Stepper. All three arguments are required: a missing validator
// would silently accept anything, so it is refused instead.
func New(eng engine.Engine, fwd Forwarder, validate Validator) (*Stepper, error) {
	switch {
	case eng == nil:
		return nil, errors.New("reconcile: engine is required")
	case fwd == nil:
		return nil, errors.New("reconcile: forwarder is required")
	case validate == nil:
		return nil, errors.New("reconcile: validator is required")
	}
	return &Stepper{engine: eng, forwarder: fwd, validate: validate}, nil
}

func set(r *Result, typ string, ok bool, reason, msg string) {
	r.Conditions[typ] = Condition{OK: ok, Reason: reason, Message: msg}
}

// redact removes every known secret from text that came back from an engine,
// the network or the validator. It runs on every such call: none of them can be
// trusted to keep secrets out of its errors. Longer secrets go first, so one
// that contains another is not left half exposed.
func redact(text string, secrets []string) string {
	byLength := slices.SortedFunc(slices.Values(secrets), func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	for _, s := range byLength {
		if s != "" {
			text = strings.ReplaceAll(text, s, "***")
		}
	}
	return text
}

// Step runs one pass in the order the design needs:
//
//	validate -> connect -> apply -> confirm -> routes -> observe
//
// Routes are programmed only after the engine confirms the source filter, so no
// route is added in front of an unconfirmed guard. A confirmation that fails
// also drops the stored revision, so the next pass applies the configuration
// again, and what happens to the routes that already exist depends on what the
// engine said: if it reports the filter down, the routes are withdrawn, because
// a route in front of no filter must not stay; if it cannot be reached or cannot
// tell, they stay, so that a failure of the management API does not blackhole
// the tenant's traffic.
func (s *Stepper) Step(ctx context.Context, ep engine.Endpoint, in Instance, st *State) Result {
	res := Result{Conditions: map[string]Condition{}}
	secrets := append(in.Desired.Secrets(), ep.Secrets()...)
	safe := func(err error) string { return redact(err.Error(), secrets) }
	// withdraw takes the routes away and records a failure to do so.
	withdraw := func() bool {
		if werr := s.forwarder.WithdrawRoutes(ctx, in.Name); werr != nil {
			set(&res, CondRoutesProgrammed, false, ReasonRoutesFailed, "withdrawing routes: "+safe(werr))
			return false
		}
		return true
	}

	if err := s.validate(ctx, in); err != nil {
		if errors.Is(err, ErrValidationUnavailable) {
			// The text of the cause is for the log, not for a tenant: it can
			// name the API server or what it failed on.
			res.Err = errors.New(safe(err))
			set(&res, CondValidated, false, ReasonValidationUnavailable, "the networks of the cluster could not be checked now; trying again")
			return res
		}
		set(&res, CondValidated, false, ReasonInvalidRemoteCIDR, safe(err))
		_ = withdraw()
		return res
	}
	set(&res, CondValidated, true, ReasonAccepted, "")

	sess, err := s.engine.Connect(ep)
	if err != nil {
		set(&res, CondConfigured, false, ReasonConfigureFailed, "connect: "+safe(err))
		return res
	}

	applied, err := sess.Apply(ctx, in.Desired, st.Revision)
	if err != nil {
		set(&res, CondConfigured, false, ReasonConfigureFailed, "apply: "+safe(err))
		return res
	}
	st.Revision = applied.Revision

	rep, err := sess.Confirm(ctx)
	if err != nil || rep.TunnelIngress != engine.GuardActive {
		st.Revision = ""
		msg := redact(rep.Detail, secrets)
		if err != nil {
			msg = safe(err)
		}
		set(&res, CondConfigured, false, ReasonSourceFilterPending, msg)
		if err == nil && rep.TunnelIngress == engine.GuardDown && withdraw() {
			set(&res, CondRoutesProgrammed, false, ReasonSourceFilterPending, "routes withdrawn while the source filter is down")
		}
		return res
	}
	set(&res, CondConfigured, true, ReasonConfigApplied, "")

	if err := s.forwarder.ApplyRoutes(ctx, in.Name, in.RemoteCIDRs, in.GatewayIP); err != nil {
		set(&res, CondRoutesProgrammed, false, ReasonRoutesFailed, safe(err))
		return res
	}
	set(&res, CondRoutesProgrammed, true, ReasonProgrammed, "")

	obs, err := sess.Observe(ctx)
	if err != nil {
		// Not fatal: failing to observe must not undo a good configuration.
		set(&res, CondTunnelEstablished, false, ReasonObserveFailed, safe(err))
		return res
	}
	res.Tunnels = obs.Tunnels
	up := true
	for _, t := range in.Desired.Tunnels {
		up = up && obs.Tunnels[t.ID] == engine.TunnelUp
	}
	set(&res, CondTunnelEstablished, up, ReasonObserved, observedMessage(in.Desired.Tunnels, obs.Tunnels))
	return res
}

// observedMessage lists the tunnels that are not up, by operator ID.
func observedMessage(want []engine.Tunnel, got map[string]engine.TunnelState) string {
	var down []string
	for _, t := range want {
		if got[t.ID] != engine.TunnelUp {
			down = append(down, fmt.Sprintf("%s=%s", t.ID, stateOrDown(got[t.ID])))
		}
	}
	return strings.Join(down, ", ")
}

func stateOrDown(s engine.TunnelState) engine.TunnelState {
	if s == "" {
		return engine.TunnelDown
	}
	return s
}
