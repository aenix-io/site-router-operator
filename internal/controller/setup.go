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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	"github.com/aenix-io/site-router-operator/internal/gateway"
)

// What the operator is allowed to do. The markers are what `make manifests` turns
// into the ClusterRole, so a verb that is not needed is not given.
//
// A Secret is read and made once; none is ever updated. The objects that are
// applied are made and patched. The VM of a gateway is not deleted by the
// operator but by the garbage collector, with its SiteRouter, and so is the
// Forwarder. Nodes and Services are listed to learn the addresses the cluster
// has.
//
// +kubebuilder:rbac:groups=siterouter.aenix.io,resources=siterouters,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=siterouter.aenix.io,resources=siterouters/status,verbs=get;patch
// +kubebuilder:rbac:groups=siterouter.aenix.io,resources=siterouters/finalizers,verbs=update
// +kubebuilder:rbac:groups=siterouter.aenix.io,resources=forwarders,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=list
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachineinstances,verbs=get;list;watch
// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=datavolumes,verbs=get;create
// +kubebuilder:rbac:groups=cilium.io,resources=ciliumnetworkpolicies,verbs=get;create;patch

// maxConcurrentReconciles is how many SiteRouters are reconciled at once. A pass
// waits on the gateway, so one at a time would make a slow gateway everyone's.
const maxConcurrentReconciles = 4

// of returns an empty object of a kind that has no Go type here.
func of(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return u
}

// CacheObjects limits what the manager caches to what the operator made. The
// Services, VMs and instances of every namespace are not its business, and a
// cache of them would be a copy of most of the cluster. A gateway is found by the
// label the operator puts on everything it makes; the VM carries it on its pod
// template, so its instance does too.
//
// The cache is for waking the controller and for nothing else. An object that
// someone else made under the name of one of the gateway has no such label, and the
// cache does not show it, so nothing that decides whether an object is the
// operator's is read from it.
func CacheObjects() map[client.Object]cache.ByObject {
	mine := cache.ByObject{Label: labels.SelectorFromSet(labels.Set{v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue})}
	return map[client.Object]cache.ByObject{
		&corev1.Service{}:                     mine,
		&v1alpha1.Forwarder{}:                 mine,
		of(gateway.VirtualMachineGVK):         mine,
		of(gateway.VirtualMachineInstanceGVK): mine,
	}
}

// SetupWithManager registers the controller. A SiteRouter is looked at again when
// its spec or an annotation changes or it is deleted, and when something its
// gateway is made of changes: the Service of the tunnel gets an address, the VM
// or its instance starts, the connector answers a Forwarder.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	own := predicate.Or(
		predicate.GenerationChangedPredicate{},
		predicate.AnnotationChangedPredicate{},
		predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectOld.GetDeletionTimestamp().IsZero() != e.ObjectNew.GetDeletionTimestamp().IsZero()
		}},
	)
	return ctrl.NewControllerManagedBy(mgr).
		Named("siterouter").
		For(&v1alpha1.SiteRouter{}, builder.WithPredicates(own)).
		Owns(&corev1.Service{}).
		Owns(&v1alpha1.Forwarder{}).
		Owns(of(gateway.VirtualMachineGVK)).
		Watches(of(gateway.VirtualMachineInstanceGVK), handler.EnqueueRequestsFromMapFunc(instanceOf)).
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrentReconciles}).
		Complete(r)
}

// instanceOf maps an object of a gateway to its SiteRouter, by the label the
// operator puts on it.
func instanceOf(_ context.Context, o client.Object) []reconcile.Request {
	name := o.GetLabels()[v1alpha1.LabelInstance]
	if name == "" {
		return nil
	}
	return []reconcile.Request{{Namespace: o.GetNamespace(), Name: name}}
}
