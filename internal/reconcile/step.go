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
	ReasonRoutesPending         = v1alpha1.ReasonRoutesPending

	ReasonAccepted      = "Accepted"
	ReasonConfigApplied = "ConfigApplied"
	ReasonProgrammed    = "Programmed"
	ReasonRoutesFailed  = "RoutesFailed"
	ReasonObserved      = "Observed"
	ReasonObserveFailed = "ObserveFailed"
)

// Routes is what Step needs from the network seam for one instance: to ask for
// the remote networks to be routed to the gateway, and to take that back.
type Routes interface {
	// Grant asks for cidrs to be routed to the gateway and for the gateway to be
	// let forward traffic for them, and says how that stands. The request is
	// idempotent. An error is for a request that could not be made; a request
	// that was made and is not carried out yet is a RouteState that is not
	// Programmed.
	Grant(ctx context.Context, cidrs []string) (RouteState, error)
	// Withdraw takes the routes and the grant away.
	Withdraw(ctx context.Context) error
}

// RouteState is how the network stands with a request for routes. Reason and
// Message are for a tenant to read.
type RouteState struct {
	Programmed bool
	Reason     string
	Message    string
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
	RemoteCIDRs []string
	Desired     engine.Desired
	// Routes is the network seam for this instance.
	Routes Routes
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
	engine   engine.Engine
	validate Validator
}

// New returns a Stepper. Both arguments are required: a missing validator would
// silently accept anything, so it is refused instead.
func New(eng engine.Engine, validate Validator) (*Stepper, error) {
	switch {
	case eng == nil:
		return nil, errors.New("reconcile: engine is required")
	case validate == nil:
		return nil, errors.New("reconcile: validator is required")
	}
	return &Stepper{engine: eng, validate: validate}, nil
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

// Judge runs the first stage of a pass and no other: whether the instance may be
// configured at all. A gateway that is not up yet has nothing to configure, and
// a tenant whose networks are refused is told so at once, not when the VM has
// booted. It sets the Validated condition, and takes the routes away when the
// instance is refused, as Step does.
func (s *Stepper) Judge(ctx context.Context, in Instance) Result {
	res := Result{Conditions: map[string]Condition{}}
	secrets := in.Desired.Secrets()
	s.judge(ctx, in, &res, func(err error) string { return redact(err.Error(), secrets) })
	return res
}

// Unavailable is the result of a pass that could not judge the instance, because
// what it is judged against could not be learned. The routes are left as they
// are, as for a validator that wraps ErrValidationUnavailable, and nothing
// reaches the engine. cause is for the log: it can name the API server or what it
// failed on, which is not for a tenant, so the condition does not carry it.
func Unavailable(cause error) Result {
	res := Result{Conditions: map[string]Condition{}, Err: cause}
	set(&res, CondValidated, false, ReasonValidationUnavailable, "the networks of the cluster could not be checked now; trying again")
	return res
}

// judge runs the validation stage into res, and says whether the pass goes on.
func (s *Stepper) judge(ctx context.Context, in Instance, res *Result, safe func(error) string) bool {
	if in.Routes == nil {
		// A fault of the caller, not of the tenant, so it is for the log; but
		// nothing can be said to be programmed either.
		res.Err = errors.New("reconcile: the instance has no Routes")
		set(res, CondRoutesProgrammed, false, ReasonRoutesFailed, "no network is set up to carry routes for this instance")
		return false
	}
	err := s.validate(ctx, in)
	switch {
	case err == nil:
		set(res, CondValidated, true, ReasonAccepted, "")
		return true
	case errors.Is(err, ErrValidationUnavailable):
		u := Unavailable(errors.New(safe(err)))
		res.Err = u.Err
		res.Conditions[CondValidated] = u.Conditions[CondValidated]
		return false
	}
	set(res, CondValidated, false, ReasonInvalidRemoteCIDR, safe(err))
	if werr := in.Routes.Withdraw(ctx); werr != nil {
		set(res, CondRoutesProgrammed, false, ReasonRoutesFailed, "withdrawing routes: "+safe(werr))
	}
	return false
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
	if !s.judge(ctx, in, &res, safe) {
		return res
	}
	// withdraw takes the routes away and records a failure to do so.
	withdraw := func() bool {
		if werr := in.Routes.Withdraw(ctx); werr != nil {
			set(&res, CondRoutesProgrammed, false, ReasonRoutesFailed, "withdrawing routes: "+safe(werr))
			return false
		}
		return true
	}

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

	state, err := in.Routes.Grant(ctx, in.RemoteCIDRs)
	if err != nil {
		set(&res, CondRoutesProgrammed, false, ReasonRoutesFailed, safe(err))
		return res
	}
	// What the network says is shown as it says it, redacted like the rest. The
	// tunnel is observed whether or not the routes are in yet: a gateway that is
	// not routed to can still be up.
	reason := state.Reason
	if reason == "" {
		reason = ReasonRoutesPending
		if state.Programmed {
			reason = ReasonProgrammed
		}
	}
	set(&res, CondRoutesProgrammed, state.Programmed, reason, redact(state.Message, secrets))

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
