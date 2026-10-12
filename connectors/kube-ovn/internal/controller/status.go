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
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/grant"
	"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/routes"
	"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/selection"
)

// Reasons of the conditions of a Forwarder.
//
// No message names an address. The routes of a namespace may name the gateway of
// somebody else, a pod of the namespace may have had the address of a node, and
// what the connector writes is read by the tenant: the networks it asked for and
// the names of its own Forwarders and pods are all a message says.
const (
	ReasonServed                  = "Served"
	ReasonInvalidSelector         = "InvalidSelector"
	ReasonGatewayFound            = "GatewayFound"
	ReasonGatewayNotFound         = "GatewayNotFound"
	ReasonGatewayUnproven         = "GatewayUnproven"
	ReasonGranted                 = "Granted"
	ReasonProgrammed              = "Programmed"
	ReasonNoNetworks              = "NoNetworks"
	ReasonNoGateway               = "NoGateway"
	ReasonConflict                = "Conflict"
	ReasonRoutesAnnotationInvalid = "RoutesAnnotationInvalid"
	ReasonNetworkClaimed          = "NetworkClaimed"
	ReasonNoConflict              = "NoConflict"
)

// status is the status of a Forwarder being made.
type status struct {
	v1alpha1.ForwarderStatus
}

func newStatus(f *v1alpha1.Forwarder) *status {
	return &status{v1alpha1.ForwarderStatus{
		ObservedGeneration: f.Generation,
		Conditions:         slices.Clone(f.Status.Conditions),
	}}
}

func (s *status) set(typ string, ok metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&s.Conditions, metav1.Condition{
		Type: typ, Status: ok, Reason: reason, Message: msg, ObservedGeneration: s.ObservedGeneration,
	})
}

func (s *status) rejected(err error) {
	msg := "a selector cannot be read: " + err.Error()
	s.set(v1alpha1.ForwarderAccepted, metav1.ConditionFalse, ReasonInvalidSelector, msg)
	for _, typ := range []string{v1alpha1.ForwarderResolved, v1alpha1.ForwarderGranted, v1alpha1.ForwarderRoutesProgrammed, v1alpha1.ForwarderConflict} {
		s.set(typ, metav1.ConditionUnknown, ReasonInvalidSelector, msg)
	}
}

func (s *status) accepted() {
	s.set(v1alpha1.ForwarderAccepted, metav1.ConditionTrue, ReasonServed, "served by the connector for kube-ovn")
}

func (s *status) resolved(v selection.View, unproven bool) {
	switch {
	case len(v.Gateways) > 0:
		s.set(v1alpha1.ForwarderResolved, metav1.ConditionTrue, ReasonGatewayFound, "the pod of the gateway is found and has an address")
	case unproven:
		s.set(v1alpha1.ForwarderResolved, metav1.ConditionFalse, ReasonGatewayUnproven,
			"waiting for KubeVirt to list the pod of the gateway among the active pods of its VirtualMachineInstance")
	default:
		s.set(v1alpha1.ForwarderResolved, metav1.ConditionFalse, ReasonGatewayNotFound,
			"no pod that the selector selects is run by the VirtualMachineInstance named like the Forwarder and has an address")
	}
}

func (s *status) noNetworks() {
	s.set(v1alpha1.ForwarderGranted, metav1.ConditionFalse, ReasonNoNetworks, "no networks are asked for")
	s.set(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionFalse, ReasonNoNetworks, "no networks are asked for")
	s.set(v1alpha1.ForwarderConflict, metav1.ConditionFalse, ReasonNoConflict, "no networks are asked for")
}

// invalidRoutes says that the routes of the namespace cannot be read. Whether
// the networks are in conflict cannot be told then, when there are any.
func (s *status) invalidRoutes(networks bool) {
	msg := fmt.Sprintf("the annotation %s of the namespace is not a list of routes, or %s is not a map of Forwarders to addresses", routes.Annotation, routes.OwnersAnnotation)
	s.set(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionFalse, ReasonRoutesAnnotationInvalid, msg)
	if networks {
		s.set(v1alpha1.ForwarderConflict, metav1.ConditionUnknown, ReasonRoutesAnnotationInvalid, msg)
	}
}

// granted says whether every pod of the gateway may forward: it has just been
// let forward, the UID of it being in opened, or it already was.
func (s *status) granted(v selection.View, opened map[types.UID]bool) {
	if len(v.Gateways) == 0 {
		s.set(v1alpha1.ForwarderGranted, metav1.ConditionFalse, ReasonNoGateway, "there is no pod of the gateway to let forward")
		return
	}
	all := true
	for _, g := range v.Gateways {
		all = all && (opened[g.Pod.UID] || grant.Forwards(g.Pod))
	}
	if all {
		s.set(v1alpha1.ForwarderGranted, metav1.ConditionTrue, ReasonGranted, "the gateway may forward traffic")
		return
	}
	s.set(v1alpha1.ForwarderGranted, metav1.ConditionFalse, ReasonConflict, "the gateway is not let forward while a network is in conflict or the routes cannot be read")
}

func (s *status) conflict(conflicts []routes.Conflict) {
	if len(conflicts) == 0 {
		s.set(v1alpha1.ForwarderConflict, metav1.ConditionFalse, ReasonNoConflict, "no other route claims the networks")
		return
	}
	parts := make([]string, len(conflicts))
	for i, c := range conflicts {
		if c.With != "" {
			parts[i] = fmt.Sprintf("%s is claimed by the Forwarder %s, which was made first", c.CIDR, c.With)
		} else {
			parts[i] = fmt.Sprintf("%s overlaps a route of the namespace that no Forwarder owns", c.CIDR)
		}
	}
	s.set(v1alpha1.ForwarderConflict, metav1.ConditionTrue, ReasonNetworkClaimed, strings.Join(parts, "; "))
}

func (s *status) routes(v selection.View, plan routes.Result, conflict bool) {
	switch {
	case conflict:
		s.set(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionFalse, ReasonConflict, "no route is added while a network is in conflict")
	case !plan.Programmed:
		s.set(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionFalse, ReasonNoGateway, "waiting for the pod of the gateway to route to")
	default:
		s.set(v1alpha1.ForwarderRoutesProgrammed, metav1.ConditionTrue, ReasonProgrammed, "the networks are routed to the gateway for the pods made from now on")
		s.PendingWorkloads = selection.Pending(v.ScopePods, plan.Mine)
	}
}

// writeStatus writes the status when it has changed. It is written on the
// version of the Forwarder that was read, so that a status of an older pass does
// not replace that of a newer one.
func (r *Reconciler) writeStatus(ctx context.Context, f *v1alpha1.Forwarder, st v1alpha1.ForwarderStatus) error {
	if equality.Semantic.DeepEqual(f.Status, st) {
		return nil
	}
	f.Status = st
	if err := r.Client.Status().Update(ctx, f); err != nil {
		return fmt.Errorf("writing the status: %w", err)
	}
	return nil
}
