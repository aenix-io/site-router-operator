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

package reconcile_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/aenix-io/site-router-operator/api/engine"
	"github.com/aenix-io/site-router-operator/api/engine/enginetest/cmdfake"
	"github.com/aenix-io/site-router-operator/api/engine/enginetest/filesfake"
	"github.com/aenix-io/site-router-operator/internal/reconcile"
)

const (
	psk        = "s3cr3t-psk"
	token      = "t0ken-for-the-api"
	remoteCIDR = "192.0.2.0/24"
)

var endpoint = engine.Endpoint{Address: "10.244.1.9", Credentials: map[string][]byte{"token": []byte(token)}}

// rig adapts a fake engine so the same test body runs against both.
type rig struct {
	name                                string
	eng                                 engine.Engine
	dial, breakFilter                   func()
	failApply, failConfirm, failObserve func(error)
}

func rigs() []rig {
	c, f := cmdfake.NewDevice(), filesfake.NewDevice()
	return []rig{
		{"cmdfake", cmdfake.Engine{Dev: c}, func() { c.Dial("t1") }, c.BreakFilter,
			func(e error) { c.FailApply = e }, func(e error) { c.FailConfirm = e }, func(e error) { c.FailObserve = e }},
		{"filesfake", filesfake.Engine{Dev: f}, func() { f.Dial("t1") }, f.BreakFilter,
			func(e error) { f.FailApply = e }, func(e error) { f.FailConfirm = e }, func(e error) { f.FailObserve = e }},
	}
}

// harness records the calls to the engine and to the forwarder in one log, so
// that their relative order is observable, and injects the failures the fakes
// cannot produce on their own.
type harness struct {
	stepper *reconcile.Stepper
	log     []string
	changed []bool   // Applied.Changed per Apply call
	prevs   []string // revision handed to each Apply call

	validateErr   error // when set, the validator rejects every instance with it
	connectErr    error
	confirmReport *engine.GuardReport // when set, Confirm returns it instead of asking the guest
	routesErr     error
	routeState    *reconcile.RouteState // when set, Grant returns it instead of "programmed"
	withdrawErr   error
	granted       [][]string // the CIDRs of each Grant
}

type spy struct {
	engine.Engine
	h *harness
}

func (s spy) Connect(ep engine.Endpoint) (engine.Session, error) {
	s.h.log = append(s.h.log, "connect")
	if s.h.connectErr != nil {
		return nil, s.h.connectErr
	}
	sess, err := s.Engine.Connect(ep)
	return spySession{sess, s.h}, err
}

type spySession struct {
	engine.Session
	h *harness
}

func (x spySession) Apply(ctx context.Context, d engine.Desired, prev string) (engine.Applied, error) {
	x.h.log = append(x.h.log, "apply")
	x.h.prevs = append(x.h.prevs, prev)
	a, err := x.Session.Apply(ctx, d, prev)
	x.h.changed = append(x.h.changed, a.Changed)
	return a, err
}

func (x spySession) Confirm(ctx context.Context) (engine.GuardReport, error) {
	x.h.log = append(x.h.log, "confirm")
	if x.h.confirmReport != nil {
		return *x.h.confirmReport, nil
	}
	return x.Session.Confirm(ctx)
}

func (x spySession) Observe(ctx context.Context) (engine.Observed, error) {
	x.h.log = append(x.h.log, "observe")
	return x.Session.Observe(ctx)
}

// routes is the network seam of the harness.
type routes struct{ h *harness }

func (r routes) Grant(_ context.Context, cidrs []string) (reconcile.RouteState, error) {
	r.h.log = append(r.h.log, "routes")
	r.h.granted = append(r.h.granted, slices.Clone(cidrs))
	if r.h.routesErr != nil {
		return reconcile.RouteState{}, r.h.routesErr
	}
	if r.h.routeState != nil {
		return *r.h.routeState, nil
	}
	return reconcile.RouteState{Programmed: true}, nil
}

func (r routes) Withdraw(context.Context) error {
	r.h.log = append(r.h.log, "withdraw")
	return r.h.withdrawErr
}

func rejectDefaultRoute(_ context.Context, in reconcile.Instance) error {
	for _, c := range in.RemoteCIDRs {
		if strings.HasSuffix(c, "/0") {
			return errors.New("remote CIDR " + c + " is a default route")
		}
	}
	return nil
}

func newHarness(t *testing.T, e engine.Engine) *harness {
	t.Helper()
	h := &harness{}
	validate := func(ctx context.Context, in reconcile.Instance) error {
		if h.validateErr != nil {
			return h.validateErr
		}
		return rejectDefaultRoute(ctx, in)
	}
	s, err := reconcile.New(spy{e, h}, validate)
	if err != nil {
		t.Fatal(err)
	}
	h.stepper = s
	return h
}

func (h *harness) step(in reconcile.Instance, st *reconcile.State) reconcile.Result {
	if in.Routes == nil {
		in.Routes = routes{h}
	}
	return h.stepper.Step(context.Background(), endpoint, in, st)
}

func (h *harness) takeLog() []string {
	l := h.log
	h.log = nil
	return l
}

func instance(dests ...string) reconcile.Instance {
	return reconcile.Instance{
		Name:        "a",
		RemoteCIDRs: []string{remoteCIDR},
		Desired: engine.Desired{
			Tunnels: []engine.Tunnel{{ID: "t1", PeerAddress: "203.0.113.10", PSK: psk, LocalID: "198.51.100.1", RemoteSelectors: []string{remoteCIDR}}},
			Guards:  engine.Guards{TunnelIngress: engine.TunnelIngress{Sources: []string{remoteCIDR}, Destinations: dests}},
		},
	}
}

func TestOrderRoutesAfterConfirm(t *testing.T) {
	for _, r := range rigs() {
		t.Run(r.name, func(t *testing.T) {
			h := newHarness(t, r.eng)
			res := h.step(instance("10.244.1.5/32"), &reconcile.State{})
			want := []string{"connect", "apply", "confirm", "routes", "observe"}
			if !slices.Equal(h.log, want) {
				t.Fatalf("call order = %v, want %v", h.log, want)
			}
			for _, c := range []string{reconcile.CondValidated, reconcile.CondConfigured, reconcile.CondRoutesProgrammed} {
				if !res.Conditions[c].OK {
					t.Errorf("condition %s not OK: %+v", c, res.Conditions[c])
				}
			}
			if res.Conditions[reconcile.CondTunnelEstablished].OK {
				t.Error("tunnel reported established before the peer dialed")
			}
			r.dial()
			res = h.step(instance("10.244.1.5/32"), &reconcile.State{})
			if !res.Conditions[reconcile.CondTunnelEstablished].OK {
				t.Errorf("tunnel not established after dial: %+v", res.Conditions[reconcile.CondTunnelEstablished])
			}
		})
	}
}

func TestRevisionIsOpaqueAndIdempotent(t *testing.T) {
	for _, r := range rigs() {
		t.Run(r.name, func(t *testing.T) {
			h := newHarness(t, r.eng)
			st := &reconcile.State{}
			h.step(instance("10.244.1.5/32"), st)
			first := st.Revision
			if first == "" {
				t.Fatal("no revision stored")
			}
			h.step(instance("10.244.1.5/32"), st)
			if st.Revision != first {
				t.Errorf("revision changed on identical input: %q -> %q", first, st.Revision)
			}
			if h.prevs[1] != first {
				t.Errorf("second Apply got prev %q, want %q", h.prevs[1], first)
			}
			if h.changed[1] {
				t.Error("identical desired state reported Changed=true")
			}
			h.step(instance("10.244.1.5/32", "10.244.1.6/32"), st)
			if st.Revision == first || !h.changed[2] {
				t.Error("changed desired state did not produce a new revision / Changed=true")
			}
		})
	}
}

func TestConfirmFailureBlocksRoutesAndForcesReapply(t *testing.T) {
	for _, r := range rigs() {
		t.Run(r.name, func(t *testing.T) {
			h := newHarness(t, r.eng)
			st := &reconcile.State{}
			// No destinations: the engine installs a fail-closed filter that
			// admits nothing, so the guard is not Active.
			res := h.step(instance(), st)
			if slices.Contains(h.log, "routes") {
				t.Fatalf("routes programmed with the source filter down: %v", h.log)
			}
			if got := res.Conditions[reconcile.CondConfigured].Reason; got != reconcile.ReasonSourceFilterPending {
				t.Errorf("reason = %q", got)
			}
			if st.Revision != "" {
				t.Error("revision kept after a failed confirm; the next pass would skip the re-apply")
			}
		})
	}
}

func TestGuardDownWithdrawsRoutes(t *testing.T) {
	for _, r := range rigs() {
		t.Run(r.name, func(t *testing.T) {
			h := newHarness(t, r.eng)
			st := &reconcile.State{}
			h.step(instance("10.244.1.5/32"), st)
			h.takeLog()

			h.confirmReport = &engine.GuardReport{TunnelIngress: engine.GuardDown, Detail: "filter missing"}
			res := h.step(instance("10.244.1.5/32"), st)
			if got := h.takeLog(); slices.Contains(got, "routes") || !slices.Contains(got, "withdraw") {
				t.Errorf("with the filter down the routes must be withdrawn, not programmed: %v", got)
			}
			if c := res.Conditions[reconcile.CondRoutesProgrammed]; c.OK || c.Reason != reconcile.ReasonSourceFilterPending {
				t.Errorf("RoutesProgrammed = %+v, want false with reason %s", c, reconcile.ReasonSourceFilterPending)
			}
			if res.Conditions[reconcile.CondConfigured].OK {
				t.Error("Configured is OK with the filter down")
			}

			h.confirmReport = nil
			res = h.step(instance("10.244.1.5/32"), st)
			if !res.Conditions[reconcile.CondRoutesProgrammed].OK || !slices.Contains(h.log, "routes") {
				t.Errorf("the pass after the filter came back did not restore the routes: %+v, calls %v", res.Conditions, h.log)
			}
		})
	}
}

func TestGuardDownWithdrawFailureIsReported(t *testing.T) {
	h := newHarness(t, rigs()[0].eng)
	h.confirmReport = &engine.GuardReport{TunnelIngress: engine.GuardDown}
	h.withdrawErr = errors.New("connector is down")
	res := h.step(instance("10.244.1.5/32"), &reconcile.State{})
	if c := res.Conditions[reconcile.CondRoutesProgrammed]; c.OK || c.Reason != reconcile.ReasonRoutesFailed {
		t.Errorf("a failed withdrawal after a down filter was not reported: %+v", c)
	}
}

// When the engine cannot say whether the filter is there, the routes stay: a
// failure of the management API must not blackhole the tenant's traffic.
func TestUnknownGuardLeavesRoutesAlone(t *testing.T) {
	injections := map[string]func(*harness, rig){
		"confirm error": func(_ *harness, rg rig) { rg.failConfirm(errors.New("management API unreachable")) },
		"unknown state": func(h *harness, _ rig) { h.confirmReport = &engine.GuardReport{TunnelIngress: engine.GuardUnknown} },
	}
	for name, inject := range injections {
		for _, r := range rigs() {
			t.Run(name+"/"+r.name, func(t *testing.T) {
				h := newHarness(t, r.eng)
				st := &reconcile.State{}
				h.step(instance("10.244.1.5/32"), st)
				h.takeLog()

				inject(h, r)
				res := h.step(instance("10.244.1.5/32"), st)
				if got := h.takeLog(); slices.Contains(got, "routes") || slices.Contains(got, "withdraw") {
					t.Errorf("an unconfirmed guard of unknown state touched the routes: %v", got)
				}
				if res.Conditions[reconcile.CondConfigured].OK {
					t.Error("Configured is OK although the filter could not be confirmed")
				}
			})
		}
	}
}

func TestInvalidInstanceNeverReachesEngine(t *testing.T) {
	for _, r := range rigs() {
		t.Run(r.name, func(t *testing.T) {
			h := newHarness(t, r.eng)
			in := instance("10.244.1.5/32")
			in.RemoteCIDRs = []string{"0.0.0.0/0"}
			res := h.step(in, &reconcile.State{})
			if !slices.Equal(h.log, []string{"withdraw"}) {
				t.Fatalf("calls = %v, want only a route withdrawal", h.log)
			}
			if got := res.Conditions[reconcile.CondValidated].Reason; got != reconcile.ReasonInvalidRemoteCIDR {
				t.Errorf("reason = %q", got)
			}
		})
	}
}

// A validator that cannot look is not a validator that refuses. Withdrawing the
// routes of a tenant because the API of the cluster did not answer would turn a
// hiccup into an outage, so the pass stops and leaves everything as it is.
func TestAValidatorThatCannotJudgeLeavesTheRoutesAlone(t *testing.T) {
	cause := errors.New("listing the nodes: Get https://10.0.0.1:6443/api/v1/nodes: connection refused with " + psk)
	for _, r := range rigs() {
		t.Run(r.name, func(t *testing.T) {
			h := newHarness(t, r.eng)
			h.validateErr = fmt.Errorf("%w: %w", reconcile.ErrValidationUnavailable, cause)
			st := &reconcile.State{Revision: "kept"}
			res := h.step(instance("10.244.1.5/32"), st)

			if len(h.log) != 0 {
				t.Errorf("calls = %v: nothing should reach the engine or the network", h.log)
			}
			if st.Revision != "kept" {
				t.Errorf("the revision became %q: the engine was not contacted, so it must stay", st.Revision)
			}
			c := res.Conditions[reconcile.CondValidated]
			if c.OK || c.Reason != reconcile.ReasonValidationUnavailable {
				t.Errorf("Validated = %+v, want False and ValidationUnavailable", c)
			}
			if _, set := res.Conditions[reconcile.CondRoutesProgrammed]; set {
				t.Error("RoutesProgrammed was set although the routes were not touched")
			}
			// The cause is for the log and the condition is for a tenant.
			if strings.Contains(c.Message, "10.0.0.1") || strings.Contains(c.Message, "connection refused") {
				t.Errorf("the condition carries the cause: %q", c.Message)
			}
			if res.Err == nil || !strings.Contains(res.Err.Error(), "connection refused") {
				t.Errorf("Err = %v, want the cause, for the log", res.Err)
			}
			if res.Err != nil && strings.Contains(res.Err.Error(), psk) {
				t.Errorf("Err leaks a secret: %v", res.Err)
			}
		})
	}
}

// Judge is the first stage of a pass and nothing else: a gateway that is not up
// has nothing to configure, and a tenant whose networks are refused is told so
// before the VM has booted.
func TestJudgeIsTheValidationStageAlone(t *testing.T) {
	judge := func(h *harness, in reconcile.Instance) reconcile.Result {
		if in.Routes == nil {
			in.Routes = routes{h}
		}
		return h.stepper.Judge(context.Background(), in)
	}

	t.Run("an instance that is fine", func(t *testing.T) {
		h := newHarness(t, rigs()[0].eng)
		res := judge(h, instance("10.244.1.5/32"))
		if c := res.Conditions[reconcile.CondValidated]; !c.OK || c.Reason != reconcile.ReasonAccepted {
			t.Errorf("Validated = %+v", c)
		}
		if len(res.Conditions) != 1 || len(h.log) != 0 {
			t.Errorf("conditions = %v and calls = %v, want the verdict alone and nothing called", res.Conditions, h.log)
		}
	})

	t.Run("an instance that is refused loses its routes", func(t *testing.T) {
		h := newHarness(t, rigs()[0].eng)
		in := instance("10.244.1.5/32")
		in.RemoteCIDRs = []string{"0.0.0.0/0"}
		res := judge(h, in)
		if c := res.Conditions[reconcile.CondValidated]; c.OK || c.Reason != reconcile.ReasonInvalidRemoteCIDR {
			t.Errorf("Validated = %+v", c)
		}
		if !slices.Equal(h.log, []string{"withdraw"}) {
			t.Errorf("calls = %v, want only a withdrawal", h.log)
		}
	})

	t.Run("a refusal whose routes cannot be taken away says so", func(t *testing.T) {
		h := newHarness(t, rigs()[0].eng)
		h.withdrawErr = errors.New("the connector is down, token " + psk)
		in := instance("10.244.1.5/32")
		in.RemoteCIDRs = []string{"0.0.0.0/0"}
		res := judge(h, in)
		c := res.Conditions[reconcile.CondRoutesProgrammed]
		if c.OK || c.Reason != reconcile.ReasonRoutesFailed || strings.Contains(c.Message, psk) || !strings.Contains(c.Message, "***") {
			t.Errorf("RoutesProgrammed = %+v, want a failure with the secret redacted", c)
		}
	})

	t.Run("a verdict that cannot be given leaves the routes", func(t *testing.T) {
		h := newHarness(t, rigs()[0].eng)
		h.validateErr = fmt.Errorf("%w: the API is down", reconcile.ErrValidationUnavailable)
		res := judge(h, instance("10.244.1.5/32"))
		if c := res.Conditions[reconcile.CondValidated]; c.OK || c.Reason != reconcile.ReasonValidationUnavailable {
			t.Errorf("Validated = %+v", c)
		}
		if len(h.log) != 0 || res.Err == nil {
			t.Errorf("calls = %v, Err = %v; want nothing called and the cause for the log", h.log, res.Err)
		}
	})

	t.Run("an instance with no Routes", func(t *testing.T) {
		h := newHarness(t, rigs()[0].eng)
		res := h.stepper.Judge(context.Background(), instance("10.244.1.5/32"))
		if res.Err == nil || res.Conditions[reconcile.CondRoutesProgrammed].OK {
			t.Errorf("result = %+v", res)
		}
	})
}

func TestWithdrawFailureIsReported(t *testing.T) {
	h := newHarness(t, rigs()[0].eng)
	h.withdrawErr = errors.New("connector is down")
	in := instance("10.244.1.5/32")
	in.RemoteCIDRs = []string{"0.0.0.0/0"}
	res := h.step(in, &reconcile.State{})
	c := res.Conditions[reconcile.CondRoutesProgrammed]
	if c.OK || c.Reason != reconcile.ReasonRoutesFailed || !strings.Contains(c.Message, "connector is down") {
		t.Errorf("a failed withdrawal was not reported: %+v", c)
	}
}

func TestSecretsRedactedOnEveryPath(t *testing.T) {
	leak := errors.New("boom " + psk + " and " + token)
	cases := []struct {
		name string
		cond string
		inj  func(*harness, rig)
	}{
		{"validator", reconcile.CondValidated, func(h *harness, _ rig) { h.validateErr = leak }},
		{"connect", reconcile.CondConfigured, func(h *harness, _ rig) { h.connectErr = leak }},
		{"apply", reconcile.CondConfigured, func(_ *harness, r rig) { r.failApply(leak) }},
		{"confirm error", reconcile.CondConfigured, func(_ *harness, r rig) { r.failConfirm(leak) }},
		{"confirm detail", reconcile.CondConfigured, func(h *harness, _ rig) {
			h.confirmReport = &engine.GuardReport{TunnelIngress: engine.GuardDown, Detail: leak.Error()}
		}},
		{"routes", reconcile.CondRoutesProgrammed, func(h *harness, _ rig) { h.routesErr = leak }},
		{"observe", reconcile.CondTunnelEstablished, func(_ *harness, r rig) { r.failObserve(leak) }},
	}
	for _, c := range cases {
		for _, r := range rigs() {
			t.Run(c.name+"/"+r.name, func(t *testing.T) {
				h := newHarness(t, r.eng)
				c.inj(h, r)
				res := h.step(instance("10.244.1.5/32"), &reconcile.State{})
				for typ, cond := range res.Conditions {
					if strings.Contains(cond.Message, psk) || strings.Contains(cond.Message, token) {
						t.Errorf("condition %s leaks a secret: %q", typ, cond.Message)
					}
				}
				// An absent message would pass the check above; the redacted one must be there.
				if msg := res.Conditions[c.cond].Message; !strings.Contains(msg, "***") {
					t.Errorf("condition %s has no redacted message: %q", c.cond, msg)
				}
			})
		}
	}
}

func TestNewRequiresEveryDependency(t *testing.T) {
	h := &harness{}
	eng := engine.Engine(spy{cmdfake.Engine{Dev: cmdfake.NewDevice()}, h})
	for _, tc := range []struct {
		missing  string
		eng      engine.Engine
		validate reconcile.Validator
	}{
		{"engine", nil, rejectDefaultRoute},
		{"validator", eng, nil},
	} {
		if _, err := reconcile.New(tc.eng, tc.validate); err == nil {
			t.Errorf("New accepted a missing %s", tc.missing)
		}
	}
	if _, err := reconcile.New(eng, rejectDefaultRoute); err != nil {
		t.Errorf("New refused complete dependencies: %v", err)
	}
}

// The network says how the routes stand, and Step shows that as it is said,
// without a secret in it.
func TestRoutesProgrammedShowsWhatTheNetworkSays(t *testing.T) {
	cases := []struct {
		name       string
		state      reconcile.RouteState
		wantOK     bool
		wantReason string
	}{
		{"programmed, with no reason given", reconcile.RouteState{Programmed: true}, true, reconcile.ReasonProgrammed},
		{"programmed, with a reason", reconcile.RouteState{Programmed: true, Reason: "Elsewhere"}, true, "Elsewhere"},
		{"not yet, with no reason given", reconcile.RouteState{}, false, reconcile.ReasonRoutesPending},
		{"not yet, with a reason", reconcile.RouteState{Reason: "PendingWorkloads", Message: "2 workloads need a restart"}, false, "PendingWorkloads"},
	}
	for _, c := range cases {
		for _, r := range rigs() {
			t.Run(c.name+"/"+r.name, func(t *testing.T) {
				h := newHarness(t, r.eng)
				h.routeState = &c.state
				res := h.step(instance("10.244.1.5/32"), &reconcile.State{})
				got := res.Conditions[reconcile.CondRoutesProgrammed]
				if got.OK != c.wantOK || got.Reason != c.wantReason || got.Message != c.state.Message {
					t.Errorf("RoutesProgrammed = %+v, want OK=%v reason=%q message=%q", got, c.wantOK, c.wantReason, c.state.Message)
				}
				// The tunnel is observed whether or not the routes are in.
				if _, ok := res.Conditions[reconcile.CondTunnelEstablished]; !ok {
					t.Error("the pass stopped before it observed the tunnel")
				}
				if !slices.Equal(h.takeLog(), []string{"connect", "apply", "confirm", "routes", "observe"}) {
					t.Error("calls out of order")
				}
			})
		}
	}
}

func TestTheMessageOfTheNetworkIsRedacted(t *testing.T) {
	h := newHarness(t, rigs()[0].eng)
	h.routeState = &reconcile.RouteState{Reason: "RoutesPending", Message: "waiting for " + psk}
	res := h.step(instance("10.244.1.5/32"), &reconcile.State{})
	if msg := res.Conditions[reconcile.CondRoutesProgrammed].Message; strings.Contains(msg, psk) || !strings.Contains(msg, "***") {
		t.Errorf("message = %q, want the secret redacted", msg)
	}
}

func TestTheRemoteNetworksAreWhatIsGranted(t *testing.T) {
	h := newHarness(t, rigs()[0].eng)
	in := instance("10.244.1.5/32")
	in.RemoteCIDRs = []string{"192.0.2.0/24", "198.51.100.0/24"}
	h.step(in, &reconcile.State{})
	if len(h.granted) != 1 || !slices.Equal(h.granted[0], in.RemoteCIDRs) {
		t.Errorf("granted = %v, want the remote networks of the instance", h.granted)
	}
}

func TestAnInstanceWithNoRoutesCannotBePassed(t *testing.T) {
	h := newHarness(t, rigs()[0].eng)
	// The harness fills in Routes, so the stepper is called directly.
	res := h.stepper.Step(context.Background(), endpoint, instance("10.244.1.5/32"), &reconcile.State{})
	if len(h.log) != 0 {
		t.Errorf("calls = %v: nothing should reach the engine", h.log)
	}
	if res.Err == nil || res.Conditions[reconcile.CondRoutesProgrammed].OK {
		t.Errorf("result = %+v, want an error and no routes programmed", res)
	}
}
