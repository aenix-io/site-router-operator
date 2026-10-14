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

// Package gateway_test applies the objects of a gateway to a real apiserver:
// what the applier does with an object that exists is only worth what the
// apiserver makes of the requests.
package gateway_test

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aenix-io/site-router-operator/api/engine"
	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/internal/gateway"
	"github.com/aenix-io/site-router-operator/internal/testenv"
)

var k8sClient client.Client

func TestMain(m *testing.M) {
	testenv.Main(m, func(c client.Client) { k8sClient = c }, "crds")
}

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

var secretGVK = corev1.SchemeGroupVersion.WithKind("Secret")
var serviceGVK = corev1.SchemeGroupVersion.WithKind("Service")

// world is a namespace with a SiteRouter in it, and what Render needs to make a
// gateway of it.
type world struct {
	t  *testing.T
	ns string
	sr *v1alpha1.SiteRouter
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ns := &corev1.Namespace{GenerateName: "gw-"}
	mustCreate(t, ns)
	sr := &v1alpha1.SiteRouter{
		Name:      "branch-office",
		Namespace: ns.Name,
		Spec: v1alpha1.SiteRouterSpec{
			Peer:        v1alpha1.Peer{Address: "203.0.113.10"},
			RemoteCIDRs: []v1alpha1.IPv4CIDR{"192.0.2.0/24"},
		},
	}
	mustCreate(t, sr)
	return &world{t: t, ns: ns.Name, sr: sr}
}

func mustCreate(t *testing.T, o client.Object) {
	t.Helper()
	if err := k8sClient.Create(t.Context(), o); err != nil {
		t.Fatal(err)
	}
}

func mustCreateUnstructured(t *testing.T, gvk schema.GroupVersionKind, ns, name string) {
	t.Helper()
	u := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"marker": "tenant"}}}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(ns)
	u.SetName(name)
	mustCreate(t, u)
}

// names are the names of the objects of the gateway for the image of the default
// input.
func (w *world) names() gateway.Names { return gateway.NamesFor(w.sr.Name).ForImage(w.input().Image) }

func (w *world) input(mutate ...func(*gateway.Input)) gateway.Input {
	cfg := gateway.DefaultConfig()
	cfg.Isolation = gateway.IsolationConfig{
		Mode: gateway.IsolationCilium,
		Controller: gateway.ControllerIdentity{
			Namespace: "site-router-system",
			Labels:    map[string]string{"app.kubernetes.io/name": "site-router-operator"},
		},
	}
	in := gateway.Input{
		Instance: w.sr,
		Config:   cfg,
		Image:    "registry.example/appliances/gateway-disk:1.0@" + digest,
		Plan: engine.BootstrapPlan{
			Arch:           "amd64",
			DiskGiB:        12,
			SeedFiles:      map[string][]byte{engine.SeedUserData: []byte("# seed one")},
			Credentials:    map[string][]byte{"token": []byte("token-one-0123456789")},
			ManagementPort: 443,
		},
		PSK: []byte("psk-one-0123456789abcdef"),
	}
	for _, m := range mutate {
		m(&in)
	}
	return in
}

func (w *world) apply(in gateway.Input) error {
	w.t.Helper()
	objs, err := gateway.Render(in)
	if err != nil {
		w.t.Fatalf("Render: %v", err)
	}
	return (&gateway.Applier{Client: k8sClient}).Apply(w.t.Context(), w.sr, objs)
}

func (w *world) mustApply(in gateway.Input) {
	w.t.Helper()
	if err := w.apply(in); err != nil {
		w.t.Fatalf("Apply: %v", err)
	}
}

// get returns the object of the given kind and name, or nil if it is not there.
func (w *world) get(gvk schema.GroupVersionKind, name string) *unstructured.Unstructured {
	w.t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	err := k8sClient.Get(w.t.Context(), client.ObjectKey{Namespace: w.ns, Name: name}, u)
	if client.IgnoreNotFound(err) != nil {
		w.t.Fatal(err)
	}
	if err != nil {
		return nil
	}
	return u
}

func (w *world) secret(name string) *unstructured.Unstructured  { return w.get(secretGVK, name) }
func (w *world) service(name string) *unstructured.Unstructured { return w.get(serviceGVK, name) }
func (w *world) vm(name string) *unstructured.Unstructured {
	return w.get(gateway.VirtualMachineGVK, name)
}
func (w *world) dataVolume(name string) *unstructured.Unstructured {
	return w.get(gateway.DataVolumeGVK, name)
}
func (w *world) policy(name string) *unstructured.Unstructured {
	return w.get(gateway.CiliumNetworkPolicyGVK, name)
}

// nested returns the value at the path as a T, or the zero value if it is
// missing or of another type.
func nested[T any](t *testing.T, u *unstructured.Unstructured, path ...string) T {
	t.Helper()
	v, _, err := unstructured.NestedFieldNoCopy(u.Object, path...)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := v.(T)
	return got
}

// secretValue is the value of a key of a Secret as read from the apiserver,
// where it is base64.
func secretValue(t *testing.T, s *unstructured.Unstructured, key string) string {
	t.Helper()
	enc := nested[string](t, s, "data", key)
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestApplyMakesTheWholeGateway(t *testing.T) {
	w := newWorld(t)
	names := w.names()
	w.mustApply(w.input())

	for name, o := range map[string]*unstructured.Unstructured{
		"Secret " + names.APIKey:                  w.secret(names.APIKey),
		"Secret " + names.CloudInit:               w.secret(names.CloudInit),
		"Secret " + names.PSK:                     w.secret(names.PSK),
		"DataVolume " + names.BootDisk:            w.dataVolume(names.BootDisk),
		"VirtualMachine " + names.VM:              w.vm(names.VM),
		"Service " + names.Tunnel:                 w.service(names.Tunnel),
		"CiliumNetworkPolicy " + names.EgressDeny: w.policy(names.EgressDeny),
		"CiliumNetworkPolicy " + names.Ingress:    w.policy(names.Ingress),
	} {
		switch {
		case o == nil:
			t.Errorf("%s was not made", name)
		case !metav1.IsControlledBy(o, w.sr):
			t.Errorf("%s is not controlled by the SiteRouter", name)
		}
	}
}

func TestApplyingTwiceChangesNothing(t *testing.T) {
	w := newWorld(t)
	names := w.names()
	versions := func() map[string]string {
		out := map[string]string{}
		for _, o := range []*unstructured.Unstructured{
			w.secret(names.APIKey), w.secret(names.CloudInit), w.secret(names.PSK),
			w.dataVolume(names.BootDisk), w.vm(names.VM), w.service(names.Tunnel),
			w.policy(names.EgressDeny), w.policy(names.Ingress),
		} {
			out[o.GetKind()+"/"+o.GetName()] = o.GetResourceVersion()
		}
		return out
	}

	w.mustApply(w.input())
	before := versions()
	w.mustApply(w.input())
	after := versions()

	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s was written again: resourceVersion %s, then %s", k, v, after[k])
		}
	}
}

func TestSecretsAreNeverWrittenAgain(t *testing.T) {
	w := newWorld(t)
	names := w.names()
	w.mustApply(w.input())

	// What a later pass would generate anew: other credentials, another seed
	// and another key. None of it may replace what the gateway runs on.
	w.mustApply(w.input(func(in *gateway.Input) {
		in.Plan.Credentials = map[string][]byte{"token": []byte("token-two-9876543210")}
		in.Plan.SeedFiles = map[string][]byte{engine.SeedUserData: []byte("# seed two")}
		in.PSK = []byte("psk-two-9876543210fedcba")
	}))

	for _, c := range []struct{ secret, key, want string }{
		{names.APIKey, "token", "token-one-0123456789"},
		{names.CloudInit, engine.SeedUserData, "# seed one"},
		{names.PSK, "psk", "psk-one-0123456789abcdef"},
	} {
		if got := secretValue(t, w.secret(c.secret), c.key); got != c.want {
			t.Errorf("Secret %s key %q = %q, want the value it was made with, %q", c.secret, c.key, got, c.want)
		}
	}
}

// Another image is another disk and another seed, beside the old ones, and the
// old ones are never changed: a disk cannot be, and a seed that a gateway
// booted from is not rewritten under it.
func TestAnotherImageGetsAnotherDiskBesideTheOne(t *testing.T) {
	w := newWorld(t)
	names := w.names()
	w.mustApply(w.input())
	diskBefore := nested[map[string]any](t, w.dataVolume(names.BootDisk), "spec")

	other := func(in *gateway.Input) {
		in.Image = "registry.example/appliances/gateway-disk:2.0@sha256:f123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		in.Plan.DiskGiB = 20
		in.Plan.SeedFiles = map[string][]byte{engine.SeedUserData: []byte("# seed for the other image")}
	}
	moved := gateway.NamesFor(w.sr.Name).ForImage(w.input(other).Image)
	w.mustApply(w.input(other))

	if got := nested[map[string]any](t, w.dataVolume(names.BootDisk), "spec"); fmt.Sprint(got) != fmt.Sprint(diskBefore) {
		t.Errorf("the disk of the first image changed, and CDI does not allow that:\nbefore %v\nafter  %v", diskBefore, got)
	}
	if got := secretValue(t, w.secret(names.CloudInit), engine.SeedUserData); got != "# seed one" {
		t.Errorf("the seed of the first image became %q", got)
	}
	if w.dataVolume(moved.BootDisk) == nil {
		t.Fatalf("no disk for the other image, %s", moved.BootDisk)
	}
	if got := nested[string](t, w.dataVolume(moved.BootDisk), "spec", "storage", "resources", "requests", "storage"); got != "20Gi" {
		t.Errorf("the new disk asks for %q, want 20Gi", got)
	}
	if got := secretValue(t, w.secret(moved.CloudInit), engine.SeedUserData); got != "# seed for the other image" {
		t.Errorf("the seed of the other image = %q", got)
	}
	// And the VM follows, since it is applied.
	vm := w.vm(names.VM)
	volumes := nested[[]any](t, vm, "spec", "template", "spec", "volumes")
	if len(volumes) != 2 || nested[string](t, &unstructured.Unstructured{Object: volumes[0].(map[string]any)}, "dataVolume", "name") != moved.BootDisk {
		t.Errorf("the VM does not boot from the new disk: %v", volumes)
	}
}

// Two images can share the first four characters of their digest, and a gateway
// must not boot from the disk of the other.
func TestADiskOfTheNameThatWasMadeForAnotherImageIsNotTaken(t *testing.T) {
	w := newWorld(t)
	names := w.names()
	w.mustApply(w.input())
	before := w.dataVolume(names.BootDisk).GetResourceVersion()

	// Same first four characters of the digest, another digest.
	clash := func(in *gateway.Input) {
		in.Image = "registry.example/appliances/gateway-disk:3.0@sha256:0123" + strings.Repeat("f", 60)
	}
	if gateway.NamesFor(w.sr.Name).ForImage(w.input(clash).Image).BootDisk != names.BootDisk {
		t.Fatal("the images do not clash: the test is wrong")
	}
	err := w.apply(w.input(clash))
	var mismatch *gateway.ImageMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("error = %v, want an ImageMismatchError", err)
	}
	// The seed comes before the disk in the order of creation, so it is the one
	// that is refused first, and the pass stops there.
	if mismatch.Name != names.CloudInit || !strings.Contains(mismatch.Existing, digest) {
		t.Errorf("the error says %+v", mismatch)
	}
	if after := w.dataVolume(names.BootDisk).GetResourceVersion(); after != before {
		t.Error("the disk was written")
	}
	if w.vm(names.VM) != nil && nested[int64](t, w.vm(names.VM), "spec", "template", "spec", "domain", "cpu", "cores") != 2 {
		t.Error("the VM was changed by a pass that stopped before it")
	}
}

func TestVirtualMachineFollowsTheInputAndDropsWhatIsNoLongerAskedFor(t *testing.T) {
	w := newWorld(t)
	names := w.names()
	w.mustApply(w.input(func(in *gateway.Input) {
		in.Config.VM.NodeSelector = map[string]string{"placement.example/pool": "gateways"}
		in.Config.VM.PodLabels = map[string]string{"billing.example/class": "network"}
	}))
	if got := nested[map[string]any](t, w.vm(names.VM), "spec", "template", "spec", "nodeSelector"); got["placement.example/pool"] != "gateways" {
		t.Fatalf("the node selector was not applied: %v", got)
	}

	// The SiteRouter grows, and the platform no longer pins the pool or labels
	// the pod. What is not asked for any more must be gone, not left behind.
	w.sr.Spec.Resources = v1alpha1.Resources{CPU: 4, Memory: resource.MustParse("4Gi")}
	w.mustApply(w.input())

	vm := w.vm(names.VM)
	if got := nested[int64](t, vm, "spec", "template", "spec", "domain", "cpu", "cores"); got != 4 {
		t.Errorf("cores = %d, want 4", got)
	}
	if got := nested[map[string]any](t, vm, "spec", "template", "spec", "nodeSelector"); got["placement.example/pool"] != nil {
		t.Errorf("the node selector the platform dropped is still there: %v", got)
	}
	if got := nested[map[string]any](t, vm, "spec", "template", "metadata", "labels"); got["billing.example/class"] != nil {
		t.Errorf("the pod label the platform dropped is still there: %v", got)
	}
}

func TestDriftOnTheVirtualMachineIsHealed(t *testing.T) {
	w := newWorld(t)
	names := w.names()
	w.mustApply(w.input())

	// Someone, under another field manager, makes the VM much bigger. The next
	// pass puts it back to what the SiteRouter asks for.
	vm := w.vm(names.VM)
	if err := unstructured.SetNestedField(vm.Object, int64(64), "spec", "template", "spec", "domain", "cpu", "cores"); err != nil {
		t.Fatal(err)
	}
	if err := k8sClient.Update(t.Context(), vm, client.FieldOwner("someone-else")); err != nil {
		t.Fatal(err)
	}
	if got := nested[int64](t, w.vm(names.VM), "spec", "template", "spec", "domain", "cpu", "cores"); got != 64 {
		t.Fatalf("the change was not made: cores = %d", got)
	}

	w.mustApply(w.input())

	if got := nested[int64](t, w.vm(names.VM), "spec", "template", "spec", "domain", "cpu", "cores"); got != 2 {
		t.Errorf("cores = %d after a pass, want the 2 the SiteRouter asks for", got)
	}
}

func TestServiceChangesAreAppliedExceptTheLoadBalancerClass(t *testing.T) {
	w := newWorld(t)
	names := w.names()
	withClass := func(class string) func(*gateway.Input) {
		return func(in *gateway.Input) { in.Config.Service.LoadBalancerClass = class }
	}

	w.mustApply(w.input(withClass("example.com/lb-a")))
	if got := nested[string](t, w.service(names.Tunnel), "spec", "loadBalancerClass"); got != "example.com/lb-a" {
		t.Fatalf("loadBalancerClass = %q, want example.com/lb-a", got)
	}

	// A change of the class is refused with the way out, and nothing is written.
	before := w.service(names.Tunnel).GetResourceVersion()
	err := w.apply(w.input(withClass("example.com/lb-b")))
	var lbErr *gateway.LoadBalancerClassError
	if !errors.As(err, &lbErr) {
		t.Fatalf("error = %v, want a LoadBalancerClassError", err)
	}
	if lbErr.Existing != "example.com/lb-a" || lbErr.Requested != "example.com/lb-b" {
		t.Errorf("the error says %+v", lbErr)
	}
	if after := w.service(names.Tunnel).GetResourceVersion(); after != before {
		t.Error("the Service was written although the change was refused")
	}

	// Dropping the class is a change as well, and is refused.
	if err := w.apply(w.input(withClass(""))); !errors.As(err, &lbErr) {
		t.Fatalf("clearing the class: error = %v, want a LoadBalancerClassError", err)
	}

	// Anything else about the Service goes through.
	w.mustApply(w.input(withClass("example.com/lb-a"), func(in *gateway.Input) {
		in.Config.Service.Annotations = map[string]string{"lb.example/pool": "tunnels"}
	}))
	if got := nested[map[string]any](t, w.service(names.Tunnel), "metadata", "annotations"); got["lb.example/pool"] != "tunnels" {
		t.Errorf("the annotation was not applied: %v", got)
	}
}

func TestAServiceThatIsNotALoadBalancerYetMayBeGivenAClass(t *testing.T) {
	w := newWorld(t)
	names := w.names()
	yes := true
	// The class cannot be changed once a Service is a load balancer, but one that
	// is not is free to become one of any class.
	mustCreate(t, &corev1.Service{
		Name:      names.Tunnel,
		Namespace: w.ns,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: v1alpha1.GroupVersion.String(), Kind: "SiteRouter", Name: w.sr.Name, UID: w.sr.UID, Controller: &yes,
		}},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Ports: []corev1.ServicePort{
			{Name: "isakmp", Protocol: corev1.ProtocolUDP, Port: 500},
			{Name: "ipsec-nat-t", Protocol: corev1.ProtocolUDP, Port: 4500},
		}},
	})

	w.mustApply(w.input(func(in *gateway.Input) { in.Config.Service.LoadBalancerClass = "example.com/lb" }))

	svc := w.service(names.Tunnel)
	if got := nested[string](t, svc, "spec", "type"); got != "LoadBalancer" {
		t.Errorf("type = %q, want LoadBalancer", got)
	}
	if got := nested[string](t, svc, "spec", "loadBalancerClass"); got != "example.com/lb" {
		t.Errorf("loadBalancerClass = %q, want example.com/lb", got)
	}
}

func TestWhatSomeoneElseMadeIsNotTakenOver(t *testing.T) {
	names := gateway.NamesFor("branch-office")
	cases := map[string]struct {
		kind, name string
		make       func(t *testing.T, ns string)
		get        func(w *world) *unstructured.Unstructured
	}{
		"the Secret of the credentials": {
			kind: "Secret", name: names.APIKey,
			make: func(t *testing.T, ns string) {
				mustCreate(t, &corev1.Secret{Name: names.APIKey, Namespace: ns, Data: map[string][]byte{"token": []byte("chosen-by-the-tenant")}})
			},
			get: func(w *world) *unstructured.Unstructured { return w.secret(names.APIKey) },
		},
		"the Secret of the pre-shared key": {
			kind: "Secret", name: names.PSK,
			make: func(t *testing.T, ns string) {
				mustCreate(t, &corev1.Secret{Name: names.PSK, Namespace: ns, Data: map[string][]byte{"psk": []byte("chosen-by-the-tenant")}})
			},
			get: func(w *world) *unstructured.Unstructured { return w.secret(names.PSK) },
		},
		"the Service": {
			kind: "Service", name: names.Tunnel,
			make: func(t *testing.T, ns string) {
				mustCreate(t, &corev1.Service{Name: names.Tunnel, Namespace: ns, Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{{Port: 80}}, Selector: map[string]string{"app": "tenant"},
				}})
			},
			get: func(w *world) *unstructured.Unstructured { return w.service(names.Tunnel) },
		},
		"the VM": {
			kind: "VirtualMachine", name: names.VM,
			make: func(t *testing.T, ns string) { mustCreateUnstructured(t, gateway.VirtualMachineGVK, ns, names.VM) },
			get:  func(w *world) *unstructured.Unstructured { return w.vm(names.VM) },
		},
		"the policy": {
			kind: "CiliumNetworkPolicy", name: names.Ingress,
			make: func(t *testing.T, ns string) {
				mustCreateUnstructured(t, gateway.CiliumNetworkPolicyGVK, ns, names.Ingress)
			},
			get: func(w *world) *unstructured.Unstructured { return w.policy(names.Ingress) },
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			c.make(t, w.ns)
			before := c.get(w)

			err := w.apply(w.input())
			var notOwned *gateway.NotOwnedError
			if !errors.As(err, &notOwned) || notOwned.Kind != c.kind || notOwned.Name != c.name {
				t.Fatalf("error = %v, want a NotOwnedError for %s %s", err, c.kind, c.name)
			}
			if after := c.get(w); after.GetResourceVersion() != before.GetResourceVersion() {
				t.Errorf("%s %s was written although it is not the operator's", c.kind, c.name)
			}
		})
	}
}

func TestWhatAnotherSiteRouterOwnsIsNotTakenOver(t *testing.T) {
	w := newWorld(t)
	names := w.names()
	other := &v1alpha1.SiteRouter{Name: "other", Namespace: w.ns, Spec: v1alpha1.SiteRouterSpec{Peer: v1alpha1.Peer{Address: "203.0.113.20"}}}
	mustCreate(t, other)
	yes := true
	mustCreate(t, &corev1.Secret{
		Name:      names.APIKey,
		Namespace: w.ns,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: v1alpha1.GroupVersion.String(), Kind: "SiteRouter", Name: other.Name, UID: other.UID, Controller: &yes,
		}},
		Data: map[string][]byte{"token": []byte("belongs-to-the-other")},
	})

	var notOwned *gateway.NotOwnedError
	if err := w.apply(w.input()); !errors.As(err, &notOwned) {
		t.Fatalf("error = %v, want a NotOwnedError", err)
	}
}

func TestApplyStopsAtTheFirstObjectThatFails(t *testing.T) {
	w := newWorld(t)
	names := w.names()
	// The Secret of the credentials is first in the order of creation.
	mustCreate(t, &corev1.Secret{Name: names.APIKey, Namespace: w.ns, Data: map[string][]byte{"token": []byte("x")}})

	if err := w.apply(w.input()); err == nil {
		t.Fatal("Apply went through a Secret that is not the operator's")
	}
	if w.vm(names.VM) != nil || w.dataVolume(names.BootDisk) != nil || w.service(names.Tunnel) != nil {
		t.Error("objects after the one that failed were made: the VM would then boot with credentials nobody holds")
	}
}
