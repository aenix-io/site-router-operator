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

package controller

import (
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/internal/reconcile"
)

// maxMessage bounds a condition message: they are read by people and by tools
// that store them, and what an engine or a network says is not the operator's to
// make long.
const maxMessage = 1024

// allConditions are the conditions of a SiteRouter, in the order they are shown.
var allConditions = []string{
	v1alpha1.ConditionReady,
	v1alpha1.ConditionValidated,
	v1alpha1.ConditionGatewayReady,
	v1alpha1.ConditionConfigured,
	v1alpha1.ConditionRoutesProgrammed,
	v1alpha1.ConditionTunnelEstablished,
}

// readyDependsOn are the conditions that Ready is the aggregate of. The tunnel is
// not one: a gateway that answers can be ready while it waits for the remote end
// to call.
var readyDependsOn = []string{
	v1alpha1.ConditionValidated,
	v1alpha1.ConditionGatewayReady,
	v1alpha1.ConditionConfigured,
	v1alpha1.ConditionRoutesProgrammed,
}

// conditions collects the conditions of one pass, on top of those of the last, so
// that a condition that did not change keeps the time it changed at.
type conditions struct {
	list       []metav1.Condition
	generation int64
	said       map[string]bool
	// now is the clock a condition is stamped with. It is the clock the controller
	// decides by, because the time a move began is read back from a condition and
	// compared with that clock.
	now func() time.Time
}

func newConditions(from []metav1.Condition, generation int64) *conditions {
	return &conditions{
		list:       append([]metav1.Condition(nil), from...),
		generation: generation,
		said:       map[string]bool{},
		now:        time.Now,
	}
}

func (c *conditions) put(typ string, status metav1.ConditionStatus, reason, msg string) {
	if len(msg) > maxMessage {
		msg = msg[:maxMessage-3] + "..."
	}
	c.said[typ] = true
	meta.SetStatusCondition(&c.list, metav1.Condition{
		Type: typ, Status: status, Reason: reason, Message: msg, ObservedGeneration: c.generation,
		LastTransitionTime: metav1.NewTime(c.now()),
	})
}

// set records a condition that is met or is not.
func (c *conditions) set(typ string, ok bool, reason, msg string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	c.put(typ, status, reason, msg)
}

// unknown records a condition that this pass could not say anything about.
func (c *conditions) unknown(typ, reason, msg string) {
	c.put(typ, metav1.ConditionUnknown, reason, msg)
}

// fromStep takes the conditions a pass of the step produced.
func (c *conditions) fromStep(res reconcile.Result) {
	for _, typ := range []string{
		v1alpha1.ConditionValidated, v1alpha1.ConditionConfigured,
		v1alpha1.ConditionRoutesProgrammed, v1alpha1.ConditionTunnelEstablished,
	} {
		if sc, ok := res.Conditions[typ]; ok {
			c.set(typ, sc.OK, sc.Reason, sc.Message)
		}
	}
}

// finish records Ready, as the aggregate of what it depends on, and an Unknown for
// every condition the pass left unsaid, so that none is left from an earlier pass
// as if it were this one's. The reason of Ready, when it is not, is the reason of
// the first condition that is not.
func (c *conditions) finish(unsaidReason, unsaidMessage string) []metav1.Condition {
	for _, typ := range allConditions {
		if typ != v1alpha1.ConditionReady && !c.said[typ] {
			c.unknown(typ, unsaidReason, unsaidMessage)
		}
	}
	for _, typ := range readyDependsOn {
		cur := meta.FindStatusCondition(c.list, typ)
		if cur.Status != metav1.ConditionTrue {
			c.put(v1alpha1.ConditionReady, metav1.ConditionFalse, cur.Reason, typ+": "+cur.Message)
			return c.ordered()
		}
	}
	c.put(v1alpha1.ConditionReady, metav1.ConditionTrue, "AllConditionsMet", "")
	return c.ordered()
}

func (c *conditions) ordered() []metav1.Condition {
	out := make([]metav1.Condition, 0, len(c.list))
	for _, typ := range allConditions {
		if cur := meta.FindStatusCondition(c.list, typ); cur != nil {
			out = append(out, *cur)
		}
	}
	return out
}
