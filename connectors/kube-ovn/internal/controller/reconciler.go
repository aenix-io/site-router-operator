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

// Package controller is the connector of the Forwarder API to kube-ovn. For each
// Forwarder it lets the pods of the gateway forward traffic, routes the networks
// of the Forwarder to the gateway for the pods of its namespace, and reports the
// addresses of the gateway and of the tenant.
//
// Every pass decides from the cluster as it is: which pods are the gateway, by
// what KubeVirt says, and whether the Forwarder asks for networks. Nothing is
// decided on what the connector wrote on a pod before, since whoever may patch
// the pod may write that too. A tunnel that works is not torn down because the
// connector restarts, is slow or cannot write: the gateway is shut only when
// the Forwarder asks for no networks, is deleted, or the pod is shown to be no
// longer the gateway.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/grant"
	"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/routes"
	"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/selection"
)

const (
	// Finalizer keeps a Forwarder until the connector has taken back its routes
	// and its grants.
	Finalizer = "kube-ovn.siterouter.aenix.io/finalizer"
	// FieldOwner is the name the connector writes under.
	FieldOwner = "site-router-kube-ovn-connector"
)

// Reconciler serves the Forwarders of the cluster.
type Reconciler struct {
	Client client.Client
}

// world is what a Forwarder is served from: the objects of its namespace.
type world struct {
	ns       *corev1.Namespace // nil when there is none
	siblings []v1alpha1.Forwarder
	pods     []corev1.Pod
	services []corev1.Service
	vmi      *selection.Instance
}

// Reconcile serves one Forwarder.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	f := &v1alpha1.Forwarder{}
	if err := r.Client.Get(ctx, req.NamespacedName, f); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, r.sweep(ctx, req.Namespace, req.Name)
		}
		return reconcile.Result{}, err
	}
	if !f.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, r.finalize(ctx, f)
	}
	// The finalizer is there before anything is granted, so that nothing granted
	// outlives the Forwarder.
	if !controllerutil.ContainsFinalizer(f, Finalizer) {
		orig := f.DeepCopy()
		controllerutil.AddFinalizer(f, Finalizer)
		if err := r.Client.Patch(ctx, f, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}), client.FieldOwner(FieldOwner)); err != nil {
			return reconcile.Result{}, fmt.Errorf("adding the finalizer: %w", err)
		}
	}
	w, err := r.read(ctx, f)
	if err != nil {
		return reconcile.Result{}, err
	}
	status, err := r.serve(ctx, f, w)
	if err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, r.writeStatus(ctx, f, status)
}

// sweep turns the check back on for the pods that name a Forwarder that is not
// there as the one that let them forward. With the Forwarder gone there is
// nothing to judge them by but that name, and it leads only to the check being
// turned on, which lets nothing through that it did not before.
func (r *Reconciler) sweep(ctx context.Context, namespace, name string) error {
	pods := &corev1.PodList{}
	if err := r.Client.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("listing the pods: %w", err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if grant.By(p) == name && selection.Live(p) {
			if err := r.withdraw(ctx, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// finalize takes back what the Forwarder was given and lets it go.
func (r *Reconciler) finalize(ctx context.Context, f *v1alpha1.Forwarder) error {
	if !controllerutil.ContainsFinalizer(f, Finalizer) {
		return nil
	}
	w, err := r.read(ctx, f)
	if err != nil {
		return err
	}
	var hops []netip.Addr
	if v, err := selection.Resolve(f, w.siblings, w.pods, w.services, w.vmi); err == nil {
		hops = v.Hops()
	}
	if err := r.withdrawRoutes(ctx, f, w, hops); err != nil && !isInvalid(err) {
		return err
	} else if err != nil {
		// What is in the annotation cannot be read, and kube-ovn cannot read it
		// either, so no route of the Forwarder works there. Holding the Forwarder
		// for it would hold the deletion of its gateway for nothing.
		logf.FromContext(ctx).Info("Leaving routes that cannot be read", "namespace", f.Namespace, "error", err.Error())
	}
	if _, err := r.settle(ctx, f, w, false, false); err != nil {
		return err
	}
	orig := f.DeepCopy()
	controllerutil.RemoveFinalizer(f, Finalizer)
	if err := r.Client.Patch(ctx, f, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}), client.FieldOwner(FieldOwner)); err != nil {
		return client.IgnoreNotFound(err)
	}
	return nil
}

// serve does what the Forwarder asks and says how that stands.
func (r *Reconciler) serve(ctx context.Context, f *v1alpha1.Forwarder, w world) (v1alpha1.ForwarderStatus, error) {
	st := newStatus(f)
	view, err := selection.Resolve(f, w.siblings, w.pods, w.services, w.vmi)
	if err != nil {
		st.rejected(err)
		return st.ForwarderStatus, nil
	}
	st.accepted()
	st.Forwarder.Addresses = view.Addresses()
	st.Scope.Addresses = view.Scope
	st.resolved(view, unproven(f, w))

	if len(f.Spec.CIDRs) == 0 {
		st.noNetworks()
		// Routes first: a pod made in between gets no route to a gateway that no
		// longer forwards.
		if err := r.withdrawRoutes(ctx, f, w, view.Hops()); err != nil && !isInvalid(err) {
			return st.ForwarderStatus, err
		} else if err != nil {
			logf.FromContext(ctx).Info("The routes of the namespace cannot be read", "error", err.Error())
			st.invalidRoutes(false)
		}
		_, err := r.settle(ctx, f, w, false, false)
		return st.ForwarderStatus, err
	}

	if w.ns == nil {
		return st.ForwarderStatus, fmt.Errorf("the namespace %s is not there", f.Namespace)
	}
	// What the Forwarder may have is decided before anything is given: a gateway
	// is let forward only for networks it can be routed.
	plan, err := r.plan(f, w, view.Hops(), routes.Program)
	if err != nil {
		logf.FromContext(ctx).Info("The routes of the namespace cannot be read", "error", err.Error())
		st.invalidRoutes(true)
		opened, err := r.settle(ctx, f, w, true, false)
		st.granted(view, opened)
		return st.ForwarderStatus, err
	}
	conflict := len(plan.Conflicts) > 0
	// The grant comes before the routes: a route to a gateway that does not
	// forward loses what it carries.
	opened, err := r.settle(ctx, f, w, true, !conflict)
	if err != nil {
		return st.ForwarderStatus, err
	}
	if err := r.patchNamespace(ctx, w.ns, plan); err != nil {
		return st.ForwarderStatus, err
	}
	st.granted(view, opened)
	st.conflict(plan.Conflicts)
	st.routes(view, plan, conflict)
	return st.ForwarderStatus, nil
}

// settle makes the check of addresses of every pod that may be the gateway of f
// what grant.Decide says, on what the pod is judged to be. The pods looked at are
// those the Forwarder selects, those KubeVirt runs the instance named like it in,
// those that say that instance controls them, and those that name the Forwarder
// as the one that let them forward. All but the active pods of the instance are
// written by whoever makes or patches the pod, and a pod that is looked at for
// them alone is not the gateway: they only lead to the check being turned on.
//
// entitled is true when the Forwarder is not being deleted and asks for
// networks, and give when it may be given what it asks for now. It returns the
// pods it has let forward, so that the status says what was done and not what
// was meant.
func (r *Reconciler) settle(ctx context.Context, f *v1alpha1.Forwarder, w world, entitled, give bool) (map[types.UID]bool, error) {
	opened := map[types.UID]bool{}
	workload, selectorErr := metav1.LabelSelectorAsSelector(&f.Spec.WorkloadSelector)
	for i := range w.pods {
		p := &w.pods[i]
		if !selection.Live(p) {
			// kube-ovn no longer applies the annotation of a pod that is being
			// deleted or has finished, and its port goes with it.
			continue
		}
		v := selection.NotGateway
		if selectorErr == nil {
			v = selection.Judge(p, f, workload, w.vmi)
		}
		active := w.vmi != nil && w.vmi.ActivePods[p.UID]
		if v == selection.NotGateway && !active && !selection.ControlledByInstanceNamed(p, f.Name) && grant.By(p) != f.Name {
			continue
		}
		var err error
		switch grant.Decide(v, entitled, give) {
		case grant.Open:
			if err = r.give(ctx, f, p); err == nil {
				opened[p.UID] = true
			}
		case grant.Shut:
			err = r.withdraw(ctx, p)
		}
		if err != nil {
			return opened, err
		}
	}
	return opened, nil
}

// unproven says whether a pod that may be the gateway waits for KubeVirt to say
// that it is.
func unproven(f *v1alpha1.Forwarder, w world) bool {
	workload, err := metav1.LabelSelectorAsSelector(&f.Spec.WorkloadSelector)
	if err != nil {
		return false
	}
	for i := range w.pods {
		if selection.Judge(&w.pods[i], f, workload, w.vmi) == selection.Unproven {
			return true
		}
	}
	return false
}

// read reads the objects of the namespace of the Forwarder.
func (r *Reconciler) read(ctx context.Context, f *v1alpha1.Forwarder) (world, error) {
	var w world
	in := client.InNamespace(f.Namespace)

	ns := &corev1.Namespace{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: f.Namespace}, ns); err != nil {
		if !apierrors.IsNotFound(err) {
			return w, fmt.Errorf("reading the namespace: %w", err)
		}
		ns = nil
	}
	w.ns = ns

	forwarders := &v1alpha1.ForwarderList{}
	if err := r.Client.List(ctx, forwarders, in); err != nil {
		return w, fmt.Errorf("listing the Forwarders: %w", err)
	}
	for _, o := range forwarders.Items {
		if o.UID != f.UID {
			w.siblings = append(w.siblings, o)
		}
	}
	pods := &corev1.PodList{}
	if err := r.Client.List(ctx, pods, in); err != nil {
		return w, fmt.Errorf("listing the pods: %w", err)
	}
	w.pods = pods.Items
	services := &corev1.ServiceList{}
	if err := r.Client.List(ctx, services, in); err != nil {
		return w, fmt.Errorf("listing the Services: %w", err)
	}
	w.services = services.Items

	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(selection.InstanceGVK)
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: f.Name}, vmi); err != nil {
		if !apierrors.IsNotFound(err) {
			return w, fmt.Errorf("reading the VirtualMachineInstance: %w", err)
		}
	} else {
		w.vmi = selection.InstanceOf(vmi)
	}
	return w, nil
}

// plan says what the routes of the namespace become for the Forwarder.
func (r *Reconciler) plan(f *v1alpha1.Forwarder, w world, hops []netip.Addr, mode routes.Mode) (routes.Result, error) {
	req := routes.Request{Self: claimOf(f), Gateways: hops, Mode: mode}
	if w.ns != nil {
		req.Routes = w.ns.Annotations[routes.Annotation]
		req.Owners = w.ns.Annotations[routes.OwnersAnnotation]
	}
	for _, s := range w.siblings {
		req.Siblings = append(req.Siblings, claimOf(&s))
	}
	res, err := routes.Plan(req)
	if err != nil {
		return routes.Result{}, invalidRoutesError{err}
	}
	return res, nil
}

// withdrawRoutes takes the routes of the Forwarder out of the namespace.
func (r *Reconciler) withdrawRoutes(ctx context.Context, f *v1alpha1.Forwarder, w world, hops []netip.Addr) error {
	if w.ns == nil {
		return nil
	}
	res, err := r.plan(f, w, hops, routes.Withdraw)
	if err != nil {
		return err
	}
	return r.patchNamespace(ctx, w.ns, res)
}

// patchNamespace writes the routes and their owners. The patch applies only to
// the version of the Namespace that was read: the entries of others are kept on
// what that version had, and a change made in between makes the patch fail and
// the Forwarder be served again.
func (r *Reconciler) patchNamespace(ctx context.Context, ns *corev1.Namespace, res routes.Result) error {
	if ns == nil || !res.Changed {
		return nil
	}
	value := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"resourceVersion": ns.ResourceVersion,
		"annotations": map[string]any{
			routes.Annotation:       value(res.Routes),
			routes.OwnersAnnotation: value(res.Owners),
		},
	}})
	if err != nil {
		return err
	}
	if err := r.Client.Patch(ctx, ns, client.RawPatch(types.MergePatchType, patch), client.FieldOwner(FieldOwner)); err != nil {
		return fmt.Errorf("writing the routes of the namespace: %w", err)
	}
	logf.FromContext(ctx).Info("Wrote the routes of the namespace", "namespace", ns.Name, "routes", res.Routes)
	return nil
}

// give lets the pod forward.
func (r *Reconciler) give(ctx context.Context, f *v1alpha1.Forwarder, pod *corev1.Pod) error {
	patch, err := grant.Give(pod, f.Name)
	if err != nil || patch == nil {
		return err
	}
	if err := r.Client.Patch(ctx, pod, client.RawPatch(types.MergePatchType, patch), client.FieldOwner(FieldOwner)); err != nil {
		return fmt.Errorf("letting the pod %s forward: %w", pod.Name, err)
	}
	logf.FromContext(ctx).Info("Let the pod forward", "pod", client.ObjectKeyFromObject(pod))
	return nil
}

// withdraw turns the check of addresses of the pod back on.
func (r *Reconciler) withdraw(ctx context.Context, pod *corev1.Pod) error {
	patch, err := grant.Withdraw(pod)
	if err != nil || patch == nil {
		return err
	}
	if err := r.Client.Patch(ctx, pod, client.RawPatch(types.MergePatchType, patch), client.FieldOwner(FieldOwner)); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("turning the check of the pod %s back on: %w", pod.Name, err)
	}
	logf.FromContext(ctx).Info("Turned the check of addresses back on", "pod", client.ObjectKeyFromObject(pod))
	return nil
}

func claimOf(f *v1alpha1.Forwarder) routes.Claim {
	c := routes.Claim{Name: f.Name, Created: f.CreationTimestamp.Time, Deleting: !f.DeletionTimestamp.IsZero()}
	for _, cidr := range f.Spec.CIDRs {
		if p, err := netip.ParsePrefix(string(cidr)); err == nil {
			c.CIDRs = append(c.CIDRs, p)
		}
	}
	return c
}

// invalidRoutesError is an annotation of the namespace that cannot be read.
type invalidRoutesError struct{ error }

func (e invalidRoutesError) Unwrap() error { return e.error }

func isInvalid(err error) bool {
	var invalid invalidRoutesError
	return errors.As(err, &invalid)
}
