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

package gateway

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
)

// FieldOwner is the name the operator applies the objects of a gateway under.
const FieldOwner = "site-router-operator"

// Applier makes the objects Render returns exist in the cluster.
//
// What is made once is left as it is afterwards. The Secrets hold credentials
// and a key that were generated for the gateway and are in use by it, so a
// Secret that exists is never written again; the boot disk is a DataVolume,
// whose spec cannot be changed after creation, so it is not either. Everything
// else is applied server-side, so that what the operator no longer asks for
// is removed from the object and what someone else changed is put back.
//
// An object that exists under the name of one of the gateway but is not
// controlled by its SiteRouter is not touched: someone else made it, and a
// credential or a policy that came from someone else is not one the gateway can
// be trusted with.
//
// What that guarantees is limited. Controlled-by is a claim written into the
// object by whoever made it. It tells an object of the operator from one that
// merely has the same name, and a mistake from a collision. It does not tell it
// from a forgery by someone who can make objects in the namespace and has read
// the UID of the SiteRouter, which is in its metadata. Closing that takes the
// operator remembering the UIDs of what it made and accepting only those, or the
// platform keeping tenants from making objects under these names.
//
// Objects that are no longer part of the gateway, such as the Secret of the
// pre-shared key once the SiteRouter names its own, are not removed here.
type Applier struct {
	Client client.Client
}

// NotOwnedError is returned when an object of the gateway exists and is not
// controlled by the SiteRouter.
type NotOwnedError struct{ Kind, Name string }

func (e *NotOwnedError) Error() string {
	return fmt.Sprintf("%s %s exists and is not controlled by this SiteRouter: it was not made by the operator, and the operator does not take it over", e.Kind, e.Name)
}

// LoadBalancerClassError is returned when the class asked for is not the class
// of the Service of the tunnel that exists.
type LoadBalancerClassError struct{ Service, Existing, Requested string }

func (e *LoadBalancerClassError) Error() string {
	return fmt.Sprintf("the Service %s is a load balancer of the class %q and cannot be given %q: the class cannot be changed once set. "+
		"Delete the Service to have it made again with the new class; its address is allocated anew and may change, "+
		"and the remote end then has to be told", e.Service, e.Existing, e.Requested)
}

// Apply applies the objects in order and stops at the first one that fails.
func (a *Applier) Apply(ctx context.Context, sr *v1alpha1.SiteRouter, objs []client.Object) error {
	for _, o := range objs {
		if err := a.apply(ctx, sr, o); err != nil {
			return fmt.Errorf("%s %s: %w", o.GetObjectKind().GroupVersionKind().Kind, o.GetName(), err)
		}
	}
	return nil
}

func (a *Applier) apply(ctx context.Context, sr *v1alpha1.SiteRouter, o client.Object) error {
	gvk := o.GetObjectKind().GroupVersionKind()
	existing, err := a.get(ctx, o)
	if err != nil {
		return err
	}
	if existing != nil && !metav1.IsControlledBy(existing, sr) {
		return &NotOwnedError{Kind: gvk.Kind, Name: o.GetName()}
	}

	switch gvk.Kind {
	case "Secret", DataVolumeGVK.Kind:
		if existing != nil {
			return nil
		}
		return a.Client.Create(ctx, o.DeepCopyObject().(client.Object))
	case "Service":
		svc, ok := o.(*corev1.Service)
		if !ok {
			return fmt.Errorf("a Service of type %T cannot be checked", o)
		}
		if existing != nil {
			if err := checkLoadBalancerClass(existing, svc); err != nil {
				return err
			}
		}
	}
	ac, err := ApplyConfiguration(o)
	if err != nil {
		return err
	}
	return a.Client.Apply(ctx, ac, client.FieldOwner(FieldOwner), client.ForceOwnership)
}

// ApplyConfiguration is what is applied for an object: only what the operator
// sets. An object of a Go type is turned into its unstructured form, which is
// safe for the objects of the operator because none of them sets a field to a
// value that is also its zero value, and none has a status or a timestamp,
// which the conversion would add as empty. A new kind of object has to keep to
// that, or be built unstructured.
func ApplyConfiguration(o client.Object) (runtime.ApplyConfiguration, error) {
	u, ok := o.(*unstructured.Unstructured)
	if !ok {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
		if err != nil {
			return nil, fmt.Errorf("converting the object: %w", err)
		}
		u = &unstructured.Unstructured{Object: m}
		unstructured.RemoveNestedField(u.Object, "status")
		unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	}
	return client.ApplyConfigurationFromUnstructured(u.DeepCopy()), nil
}

// get returns the object as it is in the cluster, or nil when it is not there.
func (a *Applier) get(ctx context.Context, o client.Object) (*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(o.GetObjectKind().GroupVersionKind())
	if err := a.Client.Get(ctx, client.ObjectKeyFromObject(o), u); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return u, nil
}

func checkLoadBalancerClass(existing *unstructured.Unstructured, want *corev1.Service) error {
	if typ, _, _ := unstructured.NestedString(existing.Object, "spec", "type"); typ != string(corev1.ServiceTypeLoadBalancer) {
		return nil
	}
	have, _, _ := unstructured.NestedString(existing.Object, "spec", "loadBalancerClass")
	requested := ""
	if want.Spec.LoadBalancerClass != nil {
		requested = *want.Spec.LoadBalancerClass
	}
	if have != requested {
		return &LoadBalancerClassError{Service: want.Name, Existing: have, Requested: requested}
	}
	return nil
}
