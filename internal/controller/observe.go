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
	"net/netip"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/internal/gateway"
)

// What the gateway VM calls its boot disk and the one network it has, which the
// operator renders and reads back.
const (
	bootVolume  = "boot"
	podNetwork  = "default"
	phaseActive = "Running"
)

// vmiFacts is what the VirtualMachineInstance of a gateway says about it.
type vmiFacts struct {
	// Phase is the phase of the instance. It is empty for none.
	Phase string
	// IP is the address of the gateway on the pod network, where its management API
	// is. It is empty when it is not known or is not an IPv4 address: the address is
	// put into a connection that carries credentials, so nothing else is let in.
	IP string
	// MACs are the hardware addresses of the network cards of the gateway.
	MACs []string
	// BootDisk is the name of the DataVolume the instance boots from.
	BootDisk string
}

func (f vmiFacts) running() bool { return f.Phase == phaseActive && f.IP != "" }

// readVM reads the VM of the gateway, which is nil when there is none or when it
// is not the SiteRouter's own, and the instance KubeVirt made for it, which is nil
// when there is none. An instance is the gateway's only when the VM controls it, by
// its UID: the management credentials go to the address it reports, so one that
// someone else made under the name, with an owner that has the right kind and
// name, is not taken for it. What the check does not stop is someone who has read
// the UID of the VM and can make instances, which is the limit the ownership of
// the other objects has too.
func (r *Reconciler) readVM(ctx context.Context, sr *v1alpha1.SiteRouter, name string) (*unstructured.Unstructured, *vmiFacts, error) {
	vm, err := r.getObject(ctx, gateway.VirtualMachineGVK, sr.Namespace, name)
	if err != nil || vm == nil || !metav1.IsControlledBy(vm, sr) {
		return nil, nil, err
	}
	u, err := r.getObject(ctx, gateway.VirtualMachineInstanceGVK, sr.Namespace, name)
	if err != nil || u == nil || !madeByVM(u, vm) {
		return vm, nil, err
	}
	return vm, vmiFactsOf(u), nil
}

// bootDiskOfVM is the DataVolume the VM boots from.
func bootDiskOfVM(vm *unstructured.Unstructured) string {
	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	return bootDiskOf(volumes)
}

// madeByVM says whether the object is controlled by the VirtualMachine, which is
// told by its group, kind, name and UID: a name and a kind are for anyone to write.
func madeByVM(u, vm *unstructured.Unstructured) bool {
	for _, o := range u.GetOwnerReferences() {
		gv, err := schema.ParseGroupVersion(o.APIVersion)
		if err != nil || o.Controller == nil || !*o.Controller {
			continue
		}
		if gv.Group == gateway.VirtualMachineGVK.Group && o.Kind == gateway.VirtualMachineGVK.Kind &&
			o.Name == vm.GetName() && o.UID == vm.GetUID() {
			return true
		}
	}
	return false
}

func vmiFactsOf(u *unstructured.Unstructured) *vmiFacts {
	f := &vmiFacts{}
	f.Phase, _, _ = unstructured.NestedString(u.Object, "status", "phase")

	ifaces, _, _ := unstructured.NestedSlice(u.Object, "status", "interfaces")
	for _, i := range ifaces {
		m, ok := i.(map[string]any)
		if !ok {
			continue
		}
		if mac, _ := m["mac"].(string); mac != "" {
			f.MACs = append(f.MACs, mac)
		}
		if name, _ := m["name"].(string); name == podNetwork && f.IP == "" {
			ip, _ := m["ipAddress"].(string)
			if a, err := netip.ParseAddr(ip); err == nil && a.Is4() {
				f.IP = a.String()
			}
		}
	}
	volumes, _, _ := unstructured.NestedSlice(u.Object, "spec", "volumes")
	f.BootDisk = bootDiskOf(volumes)
	return f
}

// bootDiskOf is the name of the DataVolume the volume called boot is.
func bootDiskOf(volumes []any) string {
	for _, v := range volumes {
		m, ok := v.(map[string]any)
		if !ok || m["name"] != bootVolume {
			continue
		}
		name, _, _ := unstructured.NestedString(m, "dataVolume", "name")
		return name
	}
	return ""
}

// getObject reads an object that has no Go type here, or returns nil when it is
// not there.
func (r *Reconciler) getObject(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) (*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	if err := r.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, u); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return u, nil
}

// tunnelAddress is the address the load balancer has given the Service of the
// tunnel, which the remote end calls and the gateway presents as its identity. A
// load balancer that gives a name and not an address gives none, since the
// identity is an address.
func tunnelAddress(svc *corev1.Service) string {
	for _, i := range svc.Status.LoadBalancer.Ingress {
		if a, err := netip.ParseAddr(i.IP); err == nil && a.Is4() {
			return a.String()
		}
	}
	return ""
}

// getSecret reads a Secret, or returns nil when it is not there. It is read from
// the apiserver and not from a cache, so that the operator does not watch the
// Secrets of every namespace.
func (r *Reconciler) getSecret(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	s := &corev1.Secret{}
	if err := r.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, s); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return s, nil
}
