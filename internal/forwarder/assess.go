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

// Package forwarder is the side of the Forwarder API that belongs to the
// operator: it makes the Forwarder of a SiteRouter, fills in the networks once
// the gateway is safe to route to, and says in the RoutesProgrammed condition
// what the connector of the cluster network makes of it.
package forwarder

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/internal/reconcile"
)

// namedPending is how many pods that need a restart are named in a message.
const namedPending = 5

// Assess says how the routes stand for a Forwarder that was asked for the
// networks in want, as the RoutesProgrammed condition of the SiteRouter.
//
// The words are the operator's own. What the connector writes in its conditions
// is not repeated: it can name the address of a pod or a node of someone else,
// and the answer to a tenant must not.
//
// requested is when the networks were asked for, which is when the connector is
// first owed an answer. A Forwarder that no connector has looked at after
// notResponding is not waiting for the connector but for one that is not there.
func Assess(f *v1alpha1.Forwarder, want []string, requested, now time.Time, notResponding time.Duration) reconcile.RouteState {
	pending := func(msg string) reconcile.RouteState {
		return reconcile.RouteState{Reason: v1alpha1.ReasonRoutesPending, Message: msg}
	}

	// The connector writes the conditions for the generation it has seen. Until
	// it has seen this one, what is there says nothing about this request.
	if f.Status.ObservedGeneration < f.Generation || len(f.Status.Conditions) == 0 {
		if now.Sub(requested) > notResponding {
			return reconcile.RouteState{
				Reason: v1alpha1.ReasonForwarderNotResponding,
				Message: fmt.Sprintf("no connector has picked up the request for the routes in %s: check that the connector for the cluster network is installed",
					notResponding.Round(time.Second)),
			}
		}
		return pending("waiting for the connector of the cluster network to pick up the request")
	}

	switch {
	case isFalse(f, v1alpha1.ForwarderAccepted):
		return reconcile.RouteState{
			Reason:  v1alpha1.ReasonForwarderRejected,
			Message: "the connector of the cluster network does not serve this request",
		}
	case meta.IsStatusConditionTrue(f.Status.Conditions, v1alpha1.ForwarderConflict):
		return reconcile.RouteState{
			Reason:  v1alpha1.ReasonRouteConflict,
			Message: "another gateway in this namespace routes some of the same networks",
		}
	case len(want) == 0:
		return reconcile.RouteState{Programmed: true, Reason: reconcile.ReasonProgrammed, Message: "there are no remote networks to route"}
	case !meta.IsStatusConditionTrue(f.Status.Conditions, v1alpha1.ForwarderResolved):
		return pending("the network has not found the gateway yet")
	case !meta.IsStatusConditionTrue(f.Status.Conditions, v1alpha1.ForwarderGranted):
		return pending("waiting for the network to let the gateway forward traffic")
	case !meta.IsStatusConditionTrue(f.Status.Conditions, v1alpha1.ForwarderRoutesProgrammed):
		return pending("waiting for the network to program the routes")
	case len(f.Status.PendingWorkloads) > 0:
		return reconcile.RouteState{
			Reason:  v1alpha1.ReasonPendingWorkloads,
			Message: pendingMessage(f.Status.PendingWorkloads),
		}
	}
	return reconcile.RouteState{Programmed: true, Reason: reconcile.ReasonProgrammed}
}

// isFalse is true only for a condition that is there and is False: one that is
// absent is not yet reported, and says nothing.
func isFalse(f *v1alpha1.Forwarder, typ string) bool {
	c := meta.FindStatusCondition(f.Status.Conditions, typ)
	return c != nil && c.Status == metav1.ConditionFalse
}

func pendingMessage(names []string) string {
	shown := names
	if len(shown) > namedPending {
		shown = shown[:namedPending]
	}
	msg := fmt.Sprintf("%d workloads were created before their routes and need a restart to get them: %s", len(names), strings.Join(shown, ", "))
	if len(names) > len(shown) {
		msg += fmt.Sprintf(" and %d more", len(names)-len(shown))
	}
	return msg
}
