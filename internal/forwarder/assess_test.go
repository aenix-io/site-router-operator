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

package forwarder

import (
	"maps"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/internal/reconcile"
)

var (
	asked = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	want  = []string{"192.0.2.0/24"}
)

const wait = time.Minute

// connector is what a connector writes into the status of a Forwarder.
type connector struct {
	observed int64
	conds    map[string]metav1.ConditionStatus
	pending  []string
	// text is written as the message of every condition.
	text string
}

func (c connector) forwarder(generation int64) *v1alpha1.Forwarder {
	f := &v1alpha1.Forwarder{}
	f.Generation = generation
	f.Status.ObservedGeneration = c.observed
	for typ, status := range c.conds {
		f.Status.Conditions = append(f.Status.Conditions, metav1.Condition{Type: typ, Status: status, Reason: "Because", Message: c.text})
	}
	f.Status.PendingWorkloads = c.pending
	return f
}

func allTrue() map[string]metav1.ConditionStatus {
	return map[string]metav1.ConditionStatus{
		v1alpha1.ForwarderAccepted:         metav1.ConditionTrue,
		v1alpha1.ForwarderResolved:         metav1.ConditionTrue,
		v1alpha1.ForwarderGranted:          metav1.ConditionTrue,
		v1alpha1.ForwarderRoutesProgrammed: metav1.ConditionTrue,
	}
}

func with(conds map[string]metav1.ConditionStatus, typ string, status metav1.ConditionStatus) map[string]metav1.ConditionStatus {
	out := map[string]metav1.ConditionStatus{}
	maps.Copy(out, conds)
	out[typ] = status
	return out
}

func without(conds map[string]metav1.ConditionStatus, typ string) map[string]metav1.ConditionStatus {
	out := map[string]metav1.ConditionStatus{}
	for k, v := range conds {
		if k != typ {
			out[k] = v
		}
	}
	return out
}

func TestAssess(t *testing.T) {
	soon := asked.Add(wait / 2)
	late := asked.Add(2 * wait)
	cases := []struct {
		name       string
		c          connector
		generation int64
		want       []string
		now        time.Time
		programmed bool
		reason     string
	}{
		{"all is in place", connector{observed: 3, conds: allTrue()}, 3, want, soon, true, reconcile.ReasonProgrammed},
		{"a connector ahead of the generation is still current", connector{observed: 5, conds: allTrue()}, 3, want, soon, true, reconcile.ReasonProgrammed},

		{"no connector has looked yet, and it is early", connector{}, 3, want, soon, false, v1alpha1.ReasonRoutesPending},
		{"no connector has looked in time", connector{}, 3, want, late, false, v1alpha1.ReasonForwarderNotResponding},
		{"a connector that has seen an older generation only", connector{observed: 2, conds: allTrue()}, 3, want, soon, false, v1alpha1.ReasonRoutesPending},
		{"an older generation, and too long", connector{observed: 2, conds: allTrue()}, 3, want, late, false, v1alpha1.ReasonForwarderNotResponding},
		{"the generation is current and nothing was written", connector{observed: 3}, 3, want, soon, false, v1alpha1.ReasonRoutesPending},
		{"exactly as long as the wait is not yet too long", connector{}, 3, want, asked.Add(wait), false, v1alpha1.ReasonRoutesPending},
		{"a moment past the wait is too long", connector{}, 3, want, asked.Add(wait + time.Second), false, v1alpha1.ReasonForwarderNotResponding},

		{"the connector refuses the request", connector{observed: 3, conds: with(allTrue(), v1alpha1.ForwarderAccepted, metav1.ConditionFalse)}, 3, want, soon, false, v1alpha1.ReasonForwarderRejected},
		{"a refusal beats a conflict", connector{observed: 3, conds: with(with(allTrue(), v1alpha1.ForwarderAccepted, metav1.ConditionFalse), v1alpha1.ForwarderConflict, metav1.ConditionTrue)}, 3, want, soon, false, v1alpha1.ReasonForwarderRejected},
		{"acceptance that is not reported is not a refusal", connector{observed: 3, conds: without(allTrue(), v1alpha1.ForwarderAccepted)}, 3, want, soon, true, reconcile.ReasonProgrammed},

		{"a conflict with a neighbor", connector{observed: 3, conds: with(allTrue(), v1alpha1.ForwarderConflict, metav1.ConditionTrue)}, 3, want, soon, false, v1alpha1.ReasonRouteConflict},
		{"no conflict is no problem", connector{observed: 3, conds: with(allTrue(), v1alpha1.ForwarderConflict, metav1.ConditionFalse)}, 3, want, soon, true, reconcile.ReasonProgrammed},
		{"a conflict beats what is pending", connector{observed: 3, conds: with(with(allTrue(), v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionFalse), v1alpha1.ForwarderConflict, metav1.ConditionTrue)}, 3, want, soon, false, v1alpha1.ReasonRouteConflict},

		{"nothing is asked for", connector{observed: 3, conds: map[string]metav1.ConditionStatus{v1alpha1.ForwarderAccepted: metav1.ConditionTrue}}, 3, nil, soon, true, reconcile.ReasonProgrammed},
		{"nothing is asked for, and a connector that refuses", connector{observed: 3, conds: map[string]metav1.ConditionStatus{v1alpha1.ForwarderAccepted: metav1.ConditionFalse}}, 3, nil, soon, false, v1alpha1.ReasonForwarderRejected},

		{"the gateway is not found yet", connector{observed: 3, conds: with(allTrue(), v1alpha1.ForwarderResolved, metav1.ConditionFalse)}, 3, want, soon, false, v1alpha1.ReasonRoutesPending},
		{"resolution is not reported", connector{observed: 3, conds: without(allTrue(), v1alpha1.ForwarderResolved)}, 3, want, soon, false, v1alpha1.ReasonRoutesPending},
		{"the grant is not given yet", connector{observed: 3, conds: with(allTrue(), v1alpha1.ForwarderGranted, metav1.ConditionFalse)}, 3, want, soon, false, v1alpha1.ReasonRoutesPending},
		{"the routes are not in yet", connector{observed: 3, conds: with(allTrue(), v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionFalse)}, 3, want, soon, false, v1alpha1.ReasonRoutesPending},
		{"a condition that is Unknown is not met", connector{observed: 3, conds: with(allTrue(), v1alpha1.ForwarderGranted, metav1.ConditionUnknown)}, 3, want, soon, false, v1alpha1.ReasonRoutesPending},

		{"some workloads were left behind", connector{observed: 3, conds: allTrue(), pending: []string{"web-0"}}, 3, want, soon, false, v1alpha1.ReasonPendingWorkloads},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Assess(c.c.forwarder(c.generation), c.want, asked, c.now, wait)
			if got.Programmed != c.programmed || got.Reason != c.reason {
				t.Errorf("Assess = %+v, want programmed=%v reason=%s", got, c.programmed, c.reason)
			}
		})
	}
}

// What the connector writes in its conditions is not repeated: it can carry the
// address of a pod or a node of someone else, and the answer to a tenant must
// not.
func TestAssessDoesNotRepeatWhatTheConnectorSays(t *testing.T) {
	const text = "pod 10.244.7.7 on node 192.168.100.5 belongs to tenant secret-corp"
	for name, conds := range map[string]map[string]metav1.ConditionStatus{
		"conflict":  with(allTrue(), v1alpha1.ForwarderConflict, metav1.ConditionTrue),
		"refusal":   with(allTrue(), v1alpha1.ForwarderAccepted, metav1.ConditionFalse),
		"not found": with(allTrue(), v1alpha1.ForwarderResolved, metav1.ConditionFalse),
		"no grant":  with(allTrue(), v1alpha1.ForwarderGranted, metav1.ConditionFalse),
		"no routes": with(allTrue(), v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionFalse),
		"all good":  allTrue(),
	} {
		got := Assess(connector{observed: 1, conds: conds, text: text}.forwarder(1), want, asked, asked, wait)
		for _, leak := range []string{"10.244.7.7", "192.168.100.5", "secret-corp", text} {
			if strings.Contains(got.Message, leak) {
				t.Errorf("%s: the message carries %q: %s", name, leak, got.Message)
			}
		}
	}
}

func TestAssessSaysWhatToLookAtWhenNobodyAnswers(t *testing.T) {
	got := Assess(connector{}.forwarder(1), want, asked, asked.Add(5*time.Minute), 90*time.Second)
	for _, s := range []string{"1m30s", "connector", "installed"} {
		if !strings.Contains(got.Message, s) {
			t.Errorf("the message does not mention %q: %s", s, got.Message)
		}
	}
}

func TestAssessNamesTheWorkloadsThatNeedARestart(t *testing.T) {
	for _, c := range []struct {
		pending []string
		want    []string
		not     string
	}{
		{[]string{"a"}, []string{"1 workloads", ": a"}, "more"},
		{[]string{"a", "b", "c", "d", "e"}, []string{"5 workloads", "a, b, c, d, e"}, "more"},
		{[]string{"a", "b", "c", "d", "e", "f", "g"}, []string{"7 workloads", "a, b, c, d, e", "and 2 more"}, ", f"},
	} {
		got := Assess(connector{observed: 1, conds: allTrue(), pending: c.pending}.forwarder(1), want, asked, asked, wait)
		for _, s := range c.want {
			if !strings.Contains(got.Message, s) {
				t.Errorf("%d pending: the message does not mention %q: %s", len(c.pending), s, got.Message)
			}
		}
		if strings.Contains(got.Message, c.not) {
			t.Errorf("%d pending: the message mentions %q: %s", len(c.pending), c.not, got.Message)
		}
	}
}

func TestAssessOfAnEmptyForwarderDoesNotPanic(t *testing.T) {
	// No status, no generation, no conditions: a Forwarder just made.
	got := Assess(&v1alpha1.Forwarder{}, nil, asked, asked, wait)
	if got.Programmed || got.Reason != v1alpha1.ReasonRoutesPending {
		t.Errorf("Assess = %+v", got)
	}
}
