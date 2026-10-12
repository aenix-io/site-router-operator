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
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/grant"
	"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/selection"
)

// What the connector is allowed to do. The markers are what `make manifests`
// turns into the ClusterRole, so a verb that is not needed is not given.
//
// Everything is read from the cache of the manager, which lists and watches, so
// nothing is got one at a time. A pod is patched for its annotation of port
// security and for the one that names the Forwarder that turned it off; RBAC
// cannot narrow a patch to an annotation, and the admission policy shipped with
// the connector does. A Namespace is patched for its routes and the record of their
// owners. A Forwarder is patched for the finalizer of the connector only, and
// its status is written.
//
// +kubebuilder:rbac:groups=siterouter.aenix.io,resources=forwarders,verbs=list;watch;patch
// +kubebuilder:rbac:groups=siterouter.aenix.io,resources=forwarders/status,verbs=update
// +kubebuilder:rbac:groups=siterouter.aenix.io,resources=forwarders/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=list;watch;patch
// +kubebuilder:rbac:groups="",resources=services,verbs=list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=list;watch;patch
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachineinstances,verbs=list;watch

// maxConcurrentReconciles is how many Forwarders are served at once. Two of the
// same namespace write the same annotation, each on the version it read, so one
// of them fails and is served again rather than undo the other.
const maxConcurrentReconciles = 4

// CacheOptions keep of the pods and Services of the cluster only what the
// connector reads, since it watches all of them.
func CacheOptions() cache.Options {
	return cache.Options{
		DefaultTransform: cache.TransformStripManagedFields(),
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Pod{}:     {Transform: trimPod},
			&corev1.Service{}: {Transform: trimService},
		},
	}
}

func trimPod(obj any) (any, error) {
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return obj, nil
	}
	out := &corev1.Pod{
		TypeMeta:   p.TypeMeta,
		ObjectMeta: p.ObjectMeta,
		Spec:       corev1.PodSpec{HostNetwork: p.Spec.HostNetwork},
		Status:     corev1.PodStatus{Phase: p.Status.Phase, PodIP: p.Status.PodIP, PodIPs: p.Status.PodIPs},
	}
	out.ManagedFields = nil
	return out, nil
}

func trimService(obj any) (any, error) {
	s, ok := obj.(*corev1.Service)
	if !ok {
		return obj, nil
	}
	out := &corev1.Service{
		TypeMeta:   s.TypeMeta,
		ObjectMeta: s.ObjectMeta,
		Spec:       corev1.ServiceSpec{ClusterIPs: s.Spec.ClusterIPs},
	}
	out.ManagedFields = nil
	return out, nil
}

// SetupWithManager registers the controller. A Forwarder is served again when
// it changes or another Forwarder of its namespace does, and when a pod, a
// Service or the Namespace changes, or the VirtualMachineInstance named like it.
// The manager has to read unstructured objects from its cache, for the
// VirtualMachineInstances.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(selection.InstanceGVK)
	return ctrl.NewControllerManagedBy(mgr).
		Named("forwarder").
		For(&v1alpha1.Forwarder{}, builder.WithPredicates(forwarderChanged)).
		Watches(&v1alpha1.Forwarder{}, handler.EnqueueRequestsFromMapFunc(r.forwardersOf), builder.WithPredicates(forwarderChanged)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.forwardersOfPod), builder.WithPredicates(podChanged)).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(r.forwardersOf), builder.WithPredicates(serviceChanged)).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.forwardersIn), builder.WithPredicates(predicate.AnnotationChangedPredicate{})).
		Watches(vmi, handler.EnqueueRequestsFromMapFunc(forwarderNamedLike)).
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrentReconciles}).
		Complete(r)
}

// forwarderChanged lets through what a Forwarder or its siblings are served on:
// not a change of its status alone, which the connector writes.
var forwarderChanged = predicate.Or(
	predicate.GenerationChangedPredicate{},
	predicate.AnnotationChangedPredicate{},
	predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		return !e.ObjectOld.GetDeletionTimestamp().Equal(e.ObjectNew.GetDeletionTimestamp()) ||
			!slices.Equal(e.ObjectOld.GetFinalizers(), e.ObjectNew.GetFinalizers())
	}},
)

// podChanged lets through a change of what the connector reads of a pod.
var podChanged = predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
	o, ok1 := e.ObjectOld.(*corev1.Pod)
	n, ok2 := e.ObjectNew.(*corev1.Pod)
	if !ok1 || !ok2 {
		return true
	}
	return !maps.Equal(o.Labels, n.Labels) || !maps.Equal(o.Annotations, n.Annotations) ||
		!o.DeletionTimestamp.Equal(n.DeletionTimestamp) || o.Status.Phase != n.Status.Phase ||
		o.Status.PodIP != n.Status.PodIP || !slices.Equal(o.Status.PodIPs, n.Status.PodIPs) ||
		o.Spec.HostNetwork != n.Spec.HostNetwork ||
		!equality.Semantic.DeepEqual(o.OwnerReferences, n.OwnerReferences)
}}

// serviceChanged lets through a change of the cluster IPs of a Service.
var serviceChanged = predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
	o, ok1 := e.ObjectOld.(*corev1.Service)
	n, ok2 := e.ObjectNew.(*corev1.Service)
	return !ok1 || !ok2 || !slices.Equal(o.Spec.ClusterIPs, n.Spec.ClusterIPs)
}}

// forwardersOf maps an object to the Forwarders of its namespace.
func (r *Reconciler) forwardersOf(ctx context.Context, o client.Object) []reconcile.Request {
	return r.forwardersInNamespace(ctx, o.GetNamespace())
}

// forwardersOfPod maps a pod to the Forwarders of its namespace, and to the one
// it names as the one that let it forward, which may be gone: then the pod is
// swept.
func (r *Reconciler) forwardersOfPod(ctx context.Context, o client.Object) []reconcile.Request {
	out := r.forwardersInNamespace(ctx, o.GetNamespace())
	if name := o.GetAnnotations()[grant.GrantedByAnnotation]; name != "" {
		out = append(out, reconcile.Request{Namespace: o.GetNamespace(), Name: name})
	}
	return out
}

// forwardersIn maps a Namespace to its Forwarders.
func (r *Reconciler) forwardersIn(ctx context.Context, o client.Object) []reconcile.Request {
	return r.forwardersInNamespace(ctx, o.GetName())
}

func (r *Reconciler) forwardersInNamespace(ctx context.Context, ns string) []reconcile.Request {
	if ns == "" {
		return nil
	}
	list := &v1alpha1.ForwarderList{}
	if err := r.Client.List(ctx, list, client.InNamespace(ns)); err != nil {
		return nil
	}
	out := make([]reconcile.Request, len(list.Items))
	for i, f := range list.Items {
		out[i] = reconcile.Request{Namespace: f.Namespace, Name: f.Name}
	}
	return out
}

// forwarderNamedLike maps a VirtualMachineInstance to the Forwarder of its name.
func forwarderNamedLike(_ context.Context, o client.Object) []reconcile.Request {
	return []reconcile.Request{{Namespace: o.GetNamespace(), Name: o.GetName()}}
}
