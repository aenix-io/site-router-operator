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
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
)

// Events of a SiteRouter that is being deleted, which has no condition to say them
// in.
const (
	eventWithdrawing = "WithdrawingRoutes"
	eventDeleted     = "RoutesWithdrawn"
)

// What an event says the operator was doing, which the API asks for besides the
// reason: the verb of the thing that was being done.
const (
	actionReconcile = "Reconcile"
	actionDelete    = "Delete"
)

// event records an event of a SiteRouter. The text is shown as it is and is not a
// format, since what an engine or a network says can have a percent sign in it.
func (r *Reconciler) event(sr *v1alpha1.SiteRouter, eventType, reason, action, text string) {
	r.recorder.Eventf(sr, nil, eventType, reason, action, "%s", text)
}

// finalize lets a SiteRouter go once nothing is routed to its gateway. The routes
// are taken away first, and the objects of the gateway go with the SiteRouter
// after that, so that traffic is not sent to a gateway that is being deleted. A
// connector that never answers does not hold the SiteRouter for ever: once none
// has for as long as one is waited for, the routes are taken to be gone.
func (r *Reconciler) finalize(ctx context.Context, sr *v1alpha1.SiteRouter) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(sr, Finalizer) {
		return ctrl.Result{}, nil
	}
	routes := r.forwarders.For(sr)

	// A Forwarder that is not the SiteRouter's is not its to take routes from, and
	// is not a reason to keep it: there is nothing of its own to wait for.
	withdrawn := true
	if err := routes.Withdraw(ctx); err != nil && !isConflict(err) {
		return ctrl.Result{}, fmt.Errorf("withdrawing the routes: %w", err)
	} else if err == nil {
		done, err := routes.Withdrawn(ctx)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("checking that the routes are withdrawn: %w", err)
		}
		withdrawn = done
	}
	if !withdrawn {
		r.event(sr, corev1.EventTypeNormal, eventWithdrawing, actionDelete, "waiting for the routes to the gateway to be taken away before it is deleted")
		return ctrl.Result{RequeueAfter: withdrawRetry}, nil
	}

	orig := sr.DeepCopy()
	controllerutil.RemoveFinalizer(sr, Finalizer)
	if err := r.client.Patch(ctx, sr, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing the finalizer: %w", err)
	}
	r.event(sr, corev1.EventTypeNormal, eventDeleted, actionDelete, "the routes to the gateway are withdrawn")
	r.forget(client.ObjectKeyFromObject(sr))
	return ctrl.Result{}, nil
}

// publish tells the world what a pass found: the events for what changed, and the
// metrics.
func (r *Reconciler) publish(sr *v1alpha1.SiteRouter, before []metav1.Condition, out outcome) {
	r.emit(sr, before, out.status.Conditions)
	for _, n := range out.notes {
		r.event(sr, n.eventType, n.reason, actionUpgrade, n.message)
	}
	if r.metrics == nil {
		return
	}
	r.metrics.observe(client.ObjectKeyFromObject(sr), observation{
		conditions: out.status.Conditions,
		engine:     out.status.Engine,
		tunnels:    out.tunnels,
		wanted:     out.wanted,
		waiting:    out.waiting,
	})
	r.metrics.moving(r.gate.Moving())
	was, is := reasonOf(before), reasonOf(out.status.Conditions)
	switch {
	case is == v1alpha1.ReasonUpgrading && was != v1alpha1.ReasonUpgrading:
		r.metrics.upgraded(resultStarted)
	case is == v1alpha1.ReasonUpgradeFailed && was != v1alpha1.ReasonUpgradeFailed:
		r.metrics.upgraded(resultFailed)
	}
	if out.completed {
		r.metrics.upgraded(resultCompleted)
	}
}

// reasonOf is the reason GatewayReady gives, or none.
func reasonOf(conds []metav1.Condition) string {
	if c := meta.FindStatusCondition(conds, v1alpha1.ConditionGatewayReady); c != nil {
		return c.Reason
	}
	return ""
}

// progress are the reasons of a condition that is not met yet because something
// that is on its way has not got there. A gateway that is starting says them all in
// turn, which is how it goes and not a warning.
var progress = []string{
	v1alpha1.ReasonGatewayPending,
	v1alpha1.ReasonTunnelAddressPending,
	v1alpha1.ReasonRoutesPending,
	v1alpha1.ReasonUpgrading,
}

// emit records an event for each condition that changed to say something is wrong,
// and one for the SiteRouter that became ready. Conditions are the record; events
// are for whoever is watching when it happens, so a condition that stays as it is
// says nothing again. The tunnel is left out: that it is down before the remote
// end calls is not a fault, and so are the states of a gateway that is on its way
// up.
func (r *Reconciler) emit(sr *v1alpha1.SiteRouter, before, after []metav1.Condition) {
	for _, c := range after {
		prev := meta.FindStatusCondition(before, c.Type)
		if prev != nil && prev.Status == c.Status && prev.Reason == c.Reason {
			continue
		}
		switch {
		case c.Type == v1alpha1.ConditionReady && c.Status == metav1.ConditionTrue:
			r.event(sr, corev1.EventTypeNormal, "Ready", actionReconcile, "the gateway is ready")
		case c.Type == v1alpha1.ConditionReady, c.Type == v1alpha1.ConditionTunnelEstablished:
		case c.Type == v1alpha1.ConditionGatewayReady && c.Reason == v1alpha1.ReasonUpgrading:
			r.event(sr, corev1.EventTypeNormal, eventUpgradeStarted, actionUpgrade, c.Message)
		case c.Type == v1alpha1.ConditionGatewayReady && c.Reason == v1alpha1.ReasonUpgradeFailed:
			r.event(sr, corev1.EventTypeWarning, c.Reason, actionUpgrade, c.Message)
		case c.Status == metav1.ConditionFalse && !slices.Contains(progress, c.Reason):
			r.event(sr, corev1.EventTypeWarning, c.Reason, actionReconcile, c.Type+": "+c.Message)
		}
	}
}
