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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
)

// The objects Render returns satisfy the invariants, since Render refuses to
// return any others. These tests start from such a set, break one property of
// it and expect assertInvariants to say so, because a check that is never seen
// to fail protects nothing.
func TestAssertInvariantsAcceptsWhatRenderReturns(t *testing.T) {
	for name, mutate := range map[string]func(*Input){
		"minimal":     nil,
		"full-config": fullConfig,
	} {
		t.Run(name, func(t *testing.T) {
			var m []func(*Input)
			if mutate != nil {
				m = append(m, mutate)
			}
			in := newInput(m...)
			if err := assertInvariants(in, mustRender(t, in)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAssertInvariantsSeesWhatIsBroken(t *testing.T) {
	names := NamesFor("branch-office").ForImage(testImage)
	vmPath := func(p ...string) []string { return append([]string{"spec", "template"}, p...) }
	set := func(u *unstructured.Unstructured, v any, path ...string) {
		if err := unstructured.SetNestedField(u.Object, v, path...); err != nil {
			panic(err)
		}
	}
	vm := func(t *testing.T, objs []client.Object) *unstructured.Unstructured {
		return unstructuredOf(t, objs, "VirtualMachine", names.VM)
	}
	dv := func(t *testing.T, objs []client.Object) *unstructured.Unstructured {
		return unstructuredOf(t, objs, "DataVolume", names.BootDisk)
	}
	svc := func(t *testing.T, objs []client.Object) *corev1.Service {
		return find(t, objs, "Service", names.Tunnel).(*corev1.Service)
	}
	policy := func(name string) func(*testing.T, []client.Object) *unstructured.Unstructured {
		return func(t *testing.T, objs []client.Object) *unstructured.Unstructured {
			return unstructuredOf(t, objs, "CiliumNetworkPolicy", name)
		}
	}
	egress, ingress := policy(names.EgressDeny), policy(names.Ingress)

	cases := []struct {
		name   string
		mutate func(t *testing.T, objs []client.Object) []client.Object
		want   string
	}{
		// What every object has.
		{"an object in another namespace", func(t *testing.T, o []client.Object) []client.Object {
			svc(t, o).Namespace = "elsewhere"
			return o
		}, "is in the namespace"},
		{"no managed-by label", func(t *testing.T, o []client.Object) []client.Object {
			delete(svc(t, o).Labels, v1alpha1.LabelManagedBy)
			return o
		}, "does not carry the label app.kubernetes.io/managed-by"},
		{"no instance label", func(t *testing.T, o []client.Object) []client.Object {
			delete(svc(t, o).Labels, v1alpha1.LabelInstance)
			return o
		}, "does not carry the label siterouter.aenix.io/instance"},
		{"another instance's label", func(t *testing.T, o []client.Object) []client.Object {
			svc(t, o).Labels[v1alpha1.LabelInstance] = "other"
			return o
		}, "does not carry the label siterouter.aenix.io/instance"},
		{"no owner", func(t *testing.T, o []client.Object) []client.Object {
			svc(t, o).OwnerReferences = nil
			return o
		}, "has 0 owners"},
		{"two owners", func(t *testing.T, o []client.Object) []client.Object {
			s := svc(t, o)
			s.OwnerReferences = append(s.OwnerReferences, s.OwnerReferences[0])
			return o
		}, "has 2 owners"},
		{"another UID as the owner", func(t *testing.T, o []client.Object) []client.Object {
			svc(t, o).OwnerReferences[0].UID = types.UID("someone-else")
			return o
		}, "is not controlled by the SiteRouter"},
		{"an owner that does not control", func(t *testing.T, o []client.Object) []client.Object {
			f := false
			svc(t, o).OwnerReferences[0].Controller = &f
			return o
		}, "is not controlled by the SiteRouter"},
		{"an owner of another kind", func(t *testing.T, o []client.Object) []client.Object {
			svc(t, o).OwnerReferences[0].Kind = "Deployment"
			return o
		}, "is not controlled by the SiteRouter"},
		{"the service rendered twice", func(t *testing.T, o []client.Object) []client.Object {
			return append(o, svc(t, o).DeepCopy())
		}, "is rendered twice"},
		{"an object that is not part of the gateway", func(t *testing.T, o []client.Object) []client.Object {
			extra := svc(t, o).DeepCopy()
			extra.Name = "extra"
			return append(o, extra)
		}, "Service/extra is not part of the gateway"},
		{"the data volume missing", func(t *testing.T, o []client.Object) []client.Object {
			var out []client.Object
			for _, x := range o {
				if x.GetName() != names.BootDisk {
					out = append(out, x)
				}
			}
			return out
		}, "DataVolume/" + names.BootDisk + " is missing"},

		// The pod of the VM.
		{"the pod without the label it is found by", func(t *testing.T, o []client.Object) []client.Object {
			unstructured.RemoveNestedField(vm(t, o).Object, vmPath("metadata", "labels", v1alpha1.LabelInstance)...)
			return o
		}, "so nothing can find it"},
		{"the pod with another instance's label", func(t *testing.T, o []client.Object) []client.Object {
			set(vm(t, o), "other", vmPath("metadata", "labels", v1alpha1.LabelInstance)...)
			return o
		}, "so nothing can find it"},
		{"the pod without the managed-by label", func(t *testing.T, o []client.Object) []client.Object {
			unstructured.RemoveNestedField(vm(t, o).Object, vmPath("metadata", "labels", v1alpha1.LabelManagedBy)...)
			return o
		}, "the pod does not carry the label app.kubernetes.io/managed-by"},
		{"port security turned off in the template", func(t *testing.T, o []client.Object) []client.Object {
			set(vm(t, o), "false", vmPath("metadata", "annotations", portSecurityAnnotation)...)
			return o
		}, "port security is for the connector to turn off"},
		{"port security set, whatever the value", func(t *testing.T, o []client.Object) []client.Object {
			set(vm(t, o), "true", vmPath("metadata", "annotations", portSecurityAnnotation)...)
			return o
		}, "port security is for the connector to turn off"},
		{"a second network", func(t *testing.T, o []client.Object) []client.Object {
			set(vm(t, o), []any{
				map[string]any{"name": "default", "pod": map[string]any{}},
				map[string]any{"name": "extra", "multus": map[string]any{"networkName": "x"}},
			}, vmPath("spec", "networks")...)
			return o
		}, "the pod network is not the only network"},
		{"the only network not the pod network", func(t *testing.T, o []client.Object) []client.Object {
			set(vm(t, o), []any{map[string]any{"name": "default", "multus": map[string]any{"networkName": "x"}}}, vmPath("spec", "networks")...)
			return o
		}, "the pod network is not the only network"},
		{"the pod network with extra settings", func(t *testing.T, o []client.Object) []client.Object {
			set(vm(t, o), []any{map[string]any{"name": "default", "pod": map[string]any{}, "multus": map[string]any{}}}, vmPath("spec", "networks")...)
			return o
		}, "the pod network is not the only network"},
		{"no network", func(t *testing.T, o []client.Object) []client.Object {
			set(vm(t, o), []any{}, vmPath("spec", "networks")...)
			return o
		}, "the pod network is not the only network"},
		{"masquerade instead of bridge", func(t *testing.T, o []client.Object) []client.Object {
			set(vm(t, o), []any{map[string]any{"name": "default", "masquerade": map[string]any{}}}, vmPath("spec", "domain", "devices", "interfaces")...)
			return o
		}, "not the default one in bridge mode"},
		{"a second interface", func(t *testing.T, o []client.Object) []client.Object {
			set(vm(t, o), []any{
				map[string]any{"name": "default", "bridge": map[string]any{}},
				map[string]any{"name": "extra", "bridge": map[string]any{}},
			}, vmPath("spec", "domain", "devices", "interfaces")...)
			return o
		}, "not the default one in bridge mode"},
		{"a bridge interface with extra settings", func(t *testing.T, o []client.Object) []client.Object {
			set(vm(t, o), []any{map[string]any{"name": "default", "bridge": map[string]any{}, "macAddress": "02:00:00:00:00:01"}}, vmPath("spec", "domain", "devices", "interfaces")...)
			return o
		}, "not the default one in bridge mode"},
		{"the VM booting from another disk", func(t *testing.T, o []client.Object) []client.Object {
			set(vm(t, o), []any{
				map[string]any{"name": "boot", "dataVolume": map[string]any{"name": "other-disk"}},
				map[string]any{"name": "cloudinitdisk", "cloudInitNoCloud": map[string]any{"secretRef": map[string]any{"name": names.CloudInit}}},
			}, vmPath("spec", "volumes")...)
			return o
		}, `boots from "other-disk"`},
		{"the VM reading another seed", func(t *testing.T, o []client.Object) []client.Object {
			set(vm(t, o), []any{
				map[string]any{"name": "boot", "dataVolume": map[string]any{"name": names.BootDisk}},
				map[string]any{"name": "cloudinitdisk", "cloudInitNoCloud": map[string]any{"secretRef": map[string]any{"name": "other-seed"}}},
			}, vmPath("spec", "volumes")...)
			return o
		}, `reads its seed from "other-seed"`},
		{"the VM with no seed volume", func(t *testing.T, o []client.Object) []client.Object {
			set(vm(t, o), []any{map[string]any{"name": "boot", "dataVolume": map[string]any{"name": names.BootDisk}}}, vmPath("spec", "volumes")...)
			return o
		}, "reads its seed from"},
		{"the wrong architecture", func(t *testing.T, o []client.Object) []client.Object {
			set(vm(t, o), "arm64", vmPath("spec", "nodeSelector", "kubernetes.io/arch")...)
			return o
		}, `runs on the architecture "arm64"`},
		{"no architecture", func(t *testing.T, o []client.Object) []client.Object {
			unstructured.RemoveNestedField(vm(t, o).Object, vmPath("spec", "nodeSelector", "kubernetes.io/arch")...)
			return o
		}, "runs on the architecture"},
		{"another firmware UUID", func(t *testing.T, o []client.Object) []client.Object {
			set(vm(t, o), "00000000-0000-4000-8000-000000000000", vmPath("spec", "domain", "firmware", "uuid")...)
			return o
		}, "has the firmware UUID"},

		// The disk.
		{"a disk imported by tag", func(t *testing.T, o []client.Object) []client.Object {
			set(dv(t, o), "docker://registry.example/appliances/gateway-disk:0.1.1", "spec", "source", "registry", "url")
			return o
		}, "must be the one pinned by digest"},
		{"a disk imported from another digest", func(t *testing.T, o []client.Object) []client.Object {
			set(dv(t, o), "docker://registry.example/appliances/gateway-disk@sha256:"+strings.Repeat("0", 64), "spec", "source", "registry", "url")
			return o
		}, "must be the one pinned by digest"},
		{"a disk with a tag and the digest", func(t *testing.T, o []client.Object) []client.Object {
			set(dv(t, o), "docker://registry.example/appliances/gateway-disk:0.1.1@"+testDigest, "spec", "source", "registry", "url")
			return o
		}, "must be the one pinned by digest"},
		{"a disk from a source other than the registry", func(t *testing.T, o []client.Object) []client.Object {
			set(dv(t, o), map[string]any{
				"registry": map[string]any{"url": "docker://registry.example/appliances/gateway-disk@" + testDigest},
				"http":     map[string]any{"url": "http://example.com/disk.qcow2"},
			}, "spec", "source")
			return o
		}, "a source other than the registry"},
		{"a disk of another size", func(t *testing.T, o []client.Object) []client.Object {
			set(dv(t, o), "10Gi", "spec", "storage", "resources", "requests", "storage")
			return o
		}, `asks for "10Gi"`},

		// CDI hands the labels of a disk to the pod that imports it, and the pod that
		// carries the instance label is the gateway to the policies that close its
		// egress. A disk with it would have its own importer cut off from the registry.
		{"a disk that carries the instance label", func(t *testing.T, o []client.Object) []client.Object {
			dv(t, o).SetLabels(map[string]string{
				v1alpha1.LabelManagedBy:  v1alpha1.ManagedByValue,
				v1alpha1.LabelBootDiskOf: "branch-office",
				v1alpha1.LabelInstance:   "branch-office",
			})
			return o
		}, "carries the label siterouter.aenix.io/instance, which CDI would hand to the pod"},
		{"a disk that does not carry its own label", func(t *testing.T, o []client.Object) []client.Object {
			dv(t, o).SetLabels(map[string]string{v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue})
			return o
		}, "does not carry the label siterouter.aenix.io/boot-disk-of"},
		{"a disk of another instance", func(t *testing.T, o []client.Object) []client.Object {
			dv(t, o).SetLabels(DiskLabels("other"))
			return o
		}, "does not carry the label siterouter.aenix.io/boot-disk-of"},
		{"a disk that does not say which image it is for", func(t *testing.T, o []client.Object) []client.Object {
			unstructured.RemoveNestedField(dv(t, o).Object, "metadata", "annotations", v1alpha1.AnnotationImageDigest)
			return o
		}, "is marked as made for the image"},
		{"a disk made for another image", func(t *testing.T, o []client.Object) []client.Object {
			set(dv(t, o), "sha256:"+strings.Repeat("a", 64), "metadata", "annotations", v1alpha1.AnnotationImageDigest)
			return o
		}, "is marked as made for the image"},
		{"a seed that does not say which image it is for", func(t *testing.T, o []client.Object) []client.Object {
			delete(secretOf(t, o, names.CloudInit).Annotations, v1alpha1.AnnotationImageDigest)
			return o
		}, "is marked as made for the image"},
		{"a seed made for another image", func(t *testing.T, o []client.Object) []client.Object {
			secretOf(t, o, names.CloudInit).Annotations[v1alpha1.AnnotationImageDigest] = "sha256:" + strings.Repeat("b", 64)
			return o
		}, "is marked as made for the image"},

		// The Service.
		{"a service that is not a load balancer", func(t *testing.T, o []client.Object) []client.Object {
			svc(t, o).Spec.Type = corev1.ServiceTypeClusterIP
			return o
		}, "not LoadBalancer"},
		{"a service with traffic policy Cluster", func(t *testing.T, o []client.Object) []client.Object {
			svc(t, o).Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
			return o
		}, "externalTrafficPolicy"},
		{"a service with no traffic policy", func(t *testing.T, o []client.Object) []client.Object {
			svc(t, o).Spec.ExternalTrafficPolicy = ""
			return o
		}, "externalTrafficPolicy"},
		{"a service that selects everything", func(t *testing.T, o []client.Object) []client.Object {
			svc(t, o).Spec.Selector = nil
			return o
		}, "not the gateway alone"},
		{"a service that selects more than the gateway", func(t *testing.T, o []client.Object) []client.Object {
			svc(t, o).Spec.Selector["app"] = "other"
			return o
		}, "not the gateway alone"},
		{"a service that selects another instance", func(t *testing.T, o []client.Object) []client.Object {
			svc(t, o).Spec.Selector[v1alpha1.LabelInstance] = "other"
			return o
		}, "not the gateway alone"},
		{"a service that publishes the management port", func(t *testing.T, o []client.Object) []client.Object {
			s := svc(t, o)
			s.Spec.Ports = append(s.Spec.Ports, corev1.ServicePort{Name: "mgmt", Protocol: corev1.ProtocolTCP, Port: 443, TargetPort: intstr.FromInt32(443)})
			return o
		}, "publishes"},
		{"a service that publishes one tunnel port", func(t *testing.T, o []client.Object) []client.Object {
			s := svc(t, o)
			s.Spec.Ports = s.Spec.Ports[:1]
			return o
		}, "publishes"},
		{"a service with a port over TCP", func(t *testing.T, o []client.Object) []client.Object {
			svc(t, o).Spec.Ports[0].Protocol = corev1.ProtocolTCP
			return o
		}, "publishes"},
		{"a service of another load balancer class", func(t *testing.T, o []client.Object) []client.Object {
			c := "example.com/other"
			svc(t, o).Spec.LoadBalancerClass = &c
			return o
		}, "has loadBalancerClass"},

		// The Secrets.
		{"the credentials missing a key", func(t *testing.T, o []client.Object) []client.Object {
			delete(secretOf(t, o, names.APIKey).Data, "token")
			return o
		}, "holds the keys"},
		{"the credentials with an extra key", func(t *testing.T, o []client.Object) []client.Object {
			secretOf(t, o, names.APIKey).Data["extra"] = []byte("x")
			return o
		}, "holds the keys"},
		{"a credential changed", func(t *testing.T, o []client.Object) []client.Object {
			secretOf(t, o, names.APIKey).Data["token"] = []byte("another")
			return o
		}, `does not hold the value of "token"`},
		{"the seed changed", func(t *testing.T, o []client.Object) []client.Object {
			secretOf(t, o, names.CloudInit).Data["userdata"] = []byte("# another seed")
			return o
		}, `does not hold the value of "userdata"`},
		{"the pre-shared key changed", func(t *testing.T, o []client.Object) []client.Object {
			secretOf(t, o, names.PSK).Data["psk"] = []byte("another")
			return o
		}, `does not hold the value of "psk"`},
		{"a credential in an annotation of the VM", func(t *testing.T, o []client.Object) []client.Object {
			set(vm(t, o), "api-token-0123456789abcdef", vmPath("metadata", "annotations", "debug")...)
			return o
		}, "holds a credential or the pre-shared key"},
		{"the pre-shared key in an annotation of the service", func(t *testing.T, o []client.Object) []client.Object {
			svc(t, o).Annotations = map[string]string{"psk": "psk-0123456789abcdefghijklmnopqrstuv"}
			return o
		}, "holds a credential or the pre-shared key"},
		{"a credential in a policy", func(t *testing.T, o []client.Object) []client.Object {
			set(egress(t, o), map[string]any{"debug": "tls-key-fedcba9876543210"}, "metadata", "annotations")
			return o
		}, "holds a credential or the pre-shared key"},

		// The policies.
		{"a policy that selects everything", func(t *testing.T, o []client.Object) []client.Object {
			set(ingress(t, o), map[string]any{"matchLabels": map[string]any{}}, "spec", "endpointSelector")
			return o
		}, "not the gateway alone"},
		{"a policy that selects another instance", func(t *testing.T, o []client.Object) []client.Object {
			set(egress(t, o), map[string]any{"matchLabels": map[string]any{v1alpha1.LabelInstance: "other"}}, "spec", "endpointSelector")
			return o
		}, "not the gateway alone"},
		{"an egress policy that does not deny link-local", func(t *testing.T, o []client.Object) []client.Object {
			set(egress(t, o), []any{map[string]any{"toCIDR": []any{"10.96.0.0/12"}}}, "spec", "egressDeny")
			return o
		}, "does not deny 169.254.0.0/16"},
		{"no egress rule", func(t *testing.T, o []client.Object) []client.Object {
			set(egress(t, o), []any{}, "spec", "egressDeny")
			return o
		}, "does not deny 169.254.0.0/16"},
		{"an ingress policy with a rule too many", func(t *testing.T, o []client.Object) []client.Object {
			p := ingress(t, o)
			rules, _, _ := unstructured.NestedSlice(p.Object, "spec", "ingress")
			set(p, append(rules, map[string]any{"fromEntities": []any{"all"}}), "spec", "ingress")
			return o
		}, "has 3 ingress rules"},
		{"an ingress policy with no rules", func(t *testing.T, o []client.Object) []client.Object {
			set(ingress(t, o), []any{}, "spec", "ingress")
			return o
		}, "has 0 ingress rules"},
		{"the tunnel admitted from the cluster too", func(t *testing.T, o []client.Object) []client.Object {
			p := ingress(t, o)
			rules, _, _ := unstructured.NestedSlice(p.Object, "spec", "ingress")
			rules[0].(map[string]any)["fromEntities"] = []any{"world", "cluster"}
			set(p, rules, "spec", "ingress")
			return o
		}, "the first rule is not the tunnel ports for the world alone"},
		{"the tunnel with a port too many", func(t *testing.T, o []client.Object) []client.Object {
			p := ingress(t, o)
			rules, _, _ := unstructured.NestedSlice(p.Object, "spec", "ingress")
			rules[0].(map[string]any)["toPorts"] = []any{map[string]any{"ports": []any{
				map[string]any{"port": "500", "protocol": "UDP"},
				map[string]any{"port": "4500", "protocol": "UDP"},
				map[string]any{"port": "443", "protocol": "TCP"},
			}}}
			set(p, rules, "spec", "ingress")
			return o
		}, "the first rule is not the tunnel ports for the world alone"},
		{"the management port open to the world", func(t *testing.T, o []client.Object) []client.Object {
			p := ingress(t, o)
			rules, _, _ := unstructured.NestedSlice(p.Object, "spec", "ingress")
			delete(rules[1].(map[string]any), "fromEndpoints")
			rules[1].(map[string]any)["fromEntities"] = []any{"world"}
			set(p, rules, "spec", "ingress")
			return o
		}, "the second rule is not the management port"},
		{"the management rule also admitting CIDRs", func(t *testing.T, o []client.Object) []client.Object {
			p := ingress(t, o)
			rules, _, _ := unstructured.NestedSlice(p.Object, "spec", "ingress")
			rules[1].(map[string]any)["fromCIDR"] = []any{"0.0.0.0/0"}
			set(p, rules, "spec", "ingress")
			return o
		}, "the second rule is not the management port"},
		{"the management port of another number", func(t *testing.T, o []client.Object) []client.Object {
			p := ingress(t, o)
			rules, _, _ := unstructured.NestedSlice(p.Object, "spec", "ingress")
			rules[1].(map[string]any)["toPorts"] = []any{map[string]any{"ports": []any{map[string]any{"port": "22", "protocol": "TCP"}}}}
			set(p, rules, "spec", "ingress")
			return o
		}, "the second rule is not the management port"},
		{"the management port admitted to every pod of the namespace", func(t *testing.T, o []client.Object) []client.Object {
			p := ingress(t, o)
			rules, _, _ := unstructured.NestedSlice(p.Object, "spec", "ingress")
			rules[1].(map[string]any)["fromEndpoints"] = []any{map[string]any{"matchLabels": map[string]any{"k8s:io.kubernetes.pod.namespace": "site-router-system"}}}
			set(p, rules, "spec", "ingress")
			return o
		}, "not admitted to the operator's namespace and labels"},
		{"the management port admitted to another namespace", func(t *testing.T, o []client.Object) []client.Object {
			p := ingress(t, o)
			rules, _, _ := unstructured.NestedSlice(p.Object, "spec", "ingress")
			rules[1].(map[string]any)["fromEndpoints"] = []any{map[string]any{"matchLabels": map[string]any{
				"k8s:io.kubernetes.pod.namespace": "elsewhere", "k8s:app.kubernetes.io/name": "site-router-operator"}}}
			set(p, rules, "spec", "ingress")
			return o
		}, "not admitted to the operator's namespace and labels"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := newInput()
			objs := c.mutate(t, mustRender(t, in))
			err := assertInvariants(in, objs)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want one that mentions %q", err, c.want)
			}
		})
	}
}

func TestAssertInvariantsReportsEveryViolation(t *testing.T) {
	in := newInput()
	objs := mustRender(t, in)
	names := NamesFor("branch-office").ForImage(testImage)
	svc := find(t, objs, "Service", names.Tunnel).(*corev1.Service)
	svc.Spec.Type = corev1.ServiceTypeClusterIP
	svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
	svc.OwnerReferences = nil

	err := assertInvariants(in, objs)
	if err == nil {
		t.Fatal("three violations were not seen")
	}
	for _, want := range []string{"not LoadBalancer", "externalTrafficPolicy", "has 0 owners"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

func TestAssertInvariantsNeedsNoPolicyWhenThereIsNone(t *testing.T) {
	in := newInput(func(in *Input) { in.Config.Isolation = IsolationConfig{Mode: IsolationExternal} })
	objs := mustRender(t, in)
	if err := assertInvariants(in, objs); err != nil {
		t.Fatal(err)
	}
	// A policy that appears although the platform isolates the gateway itself is
	// not part of the gateway.
	withPolicy := mustRender(t, newInput())
	err := assertInvariants(in, withPolicy)
	if err == nil || !strings.Contains(err.Error(), "is not part of the gateway") {
		t.Fatalf("error = %v, want policies to be reported as not part of the gateway", err)
	}
}
