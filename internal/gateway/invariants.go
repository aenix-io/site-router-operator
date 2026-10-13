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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
)

// assertInvariants checks the objects of a gateway against what its security
// and the controller depend on, and reports every violation, not only the first.
// It looks at the objects as they would be sent to the API server, not at how
// they were built, so that a change to the builders that breaks one of these is
// caught here and not in a cluster.
//
// The invariants:
//   - everything is in the namespace of the SiteRouter, owned by it and labeled
//     as the operator's, and nothing but the expected objects is there;
//   - the pod of the VM carries the label the gateway is found by, has the pod
//     network as its only network, in bridge mode, and has no annotation that
//     turns port security off: whether the gateway forwards is for the network
//     connector to grant after the source filter is confirmed;
//   - the VM boots from the disk and reads the seed of the plan, and runs on
//     the architecture of the plan;
//   - the disk is imported from an image pinned by digest;
//   - the Service of the tunnel is a LoadBalancer for the two tunnel ports only,
//     reaches only the gateway, and keeps the address of the remote end;
//   - the Secrets hold what the plan and the pre-shared key say, and none of
//     those values is in an object that is not a Secret;
//   - the policies, when there are any, select only the gateway, deny the
//     link-local range, and in their own rules admit nothing to the gateway
//     beyond the tunnel from the world and the management port for the
//     operator. What other policies of the namespace admit is not up to them.
func assertInvariants(in Input, objs []client.Object) error {
	a := &audit{in: in, names: NamesFor(in.Instance.Name).ForImage(in.Image)}
	a.run(objs)
	return errors.Join(a.errs...)
}

type audit struct {
	in    Input
	names Names
	errs  []error
	// objects by "Kind/name"
	byKey map[string]map[string]any
}

func (a *audit) fail(key, format string, args ...any) {
	a.errs = append(a.errs, fmt.Errorf("%s: %s", key, fmt.Sprintf(format, args...)))
}

func toMap(o client.Object) (map[string]any, error) {
	if u, ok := o.(*unstructured.Unstructured); ok {
		return u.Object, nil
	}
	return runtime.DefaultUnstructuredConverter.ToUnstructured(o)
}

func (a *audit) run(objs []client.Object) {
	a.byKey = map[string]map[string]any{}
	for _, o := range objs {
		m, err := toMap(o)
		if err != nil {
			a.errs = append(a.errs, fmt.Errorf("an object cannot be read: %w", err))
			continue
		}
		key := str(m, "kind") + "/" + str(m, "metadata", fieldName)
		if _, dup := a.byKey[key]; dup {
			a.fail(key, "is rendered twice")
		}
		a.byKey[key] = m
		a.common(key, m)
	}
	a.complete()
	a.virtualMachine()
	a.dataVolume()
	a.service()
	a.secrets(objs)
	a.policies()
	a.noSecretsOutsideSecrets(objs)
}

func (a *audit) common(key string, m map[string]any) {
	sr := a.in.Instance
	if ns := str(m, "metadata", "namespace"); ns != sr.Namespace {
		a.fail(key, "is in the namespace %q, not that of the SiteRouter, %q", ns, sr.Namespace)
	}
	labels := strMap(m, "metadata", "labels")
	if labels[v1alpha1.LabelManagedBy] != v1alpha1.ManagedByValue {
		a.fail(key, "does not carry the label %s=%s", v1alpha1.LabelManagedBy, v1alpha1.ManagedByValue)
	}
	// The boot disk is the one object that does not carry the instance label, and is
	// checked for not carrying it with the rest of what is said about it.
	if str(m, "kind") != DataVolumeGVK.Kind && labels[v1alpha1.LabelInstance] != sr.Name {
		a.fail(key, "does not carry the label %s=%s", v1alpha1.LabelInstance, sr.Name)
	}
	owners := list(m, "metadata", "ownerReferences")
	if len(owners) != 1 {
		a.fail(key, "has %d owners, want the SiteRouter alone", len(owners))
		return
	}
	o := asMap(owners[0])
	if str(o, "uid") != string(sr.UID) || str(o, "kind") != "SiteRouter" || str(o, fieldName) != sr.Name || at(o, "controller") != true {
		a.fail(key, "is not controlled by the SiteRouter")
	}
}

// complete checks that the gateway is made of exactly the objects it should be.
func (a *audit) complete() {
	want := []string{
		"Secret/" + a.names.APIKey,
		"Secret/" + a.names.CloudInit,
		"DataVolume/" + a.names.BootDisk,
		"VirtualMachine/" + a.names.VM,
		"Service/" + a.names.Tunnel,
	}
	if OwnsPSKSecret(a.in.Instance) {
		want = append(want, "Secret/"+a.names.PSK)
	}
	if a.in.Config.Isolation.Mode == IsolationCilium {
		want = append(want, "CiliumNetworkPolicy/"+a.names.EgressDeny, "CiliumNetworkPolicy/"+a.names.Ingress)
	}
	for _, k := range want {
		if _, ok := a.byKey[k]; !ok {
			a.errs = append(a.errs, fmt.Errorf("%s is missing", k))
		}
	}
	for _, k := range slices.Sorted(maps.Keys(a.byKey)) {
		if !slices.Contains(want, k) {
			a.errs = append(a.errs, fmt.Errorf("%s is not part of the gateway", k))
		}
	}
}

func (a *audit) virtualMachine() {
	key := "VirtualMachine/" + a.names.VM
	m, ok := a.byKey[key]
	if !ok {
		return
	}
	sr := a.in.Instance
	tmpl := []string{fieldSpec, "template"}
	path := func(p ...string) []string { return append(slices.Clone(tmpl), p...) }

	labels := strMap(m, path("metadata", "labels")...)
	if labels[v1alpha1.LabelInstance] != sr.Name {
		a.fail(key, "the pod does not carry the label %s=%s, so nothing can find it", v1alpha1.LabelInstance, sr.Name)
	}
	if labels[v1alpha1.LabelManagedBy] != v1alpha1.ManagedByValue {
		a.fail(key, "the pod does not carry the label %s=%s", v1alpha1.LabelManagedBy, v1alpha1.ManagedByValue)
	}
	if _, set := strMap(m, path("metadata", "annotations")...)[portSecurityAnnotation]; set {
		a.fail(key, "the pod has the annotation %s: port security is for the connector to turn off, once the source filter is confirmed", portSecurityAnnotation)
	}

	networks := list(m, path(fieldSpec, "networks")...)
	if len(networks) != 1 || str(asMap(networks[0]), fieldName) != networkName || at(asMap(networks[0]), "pod") == nil || len(asMap(networks[0])) != 2 {
		a.fail(key, "the pod network is not the only network")
	}
	interfaces := list(m, path(fieldSpec, "domain", "devices", "interfaces")...)
	if len(interfaces) != 1 || str(asMap(interfaces[0]), fieldName) != networkName || at(asMap(interfaces[0]), "bridge") == nil || len(asMap(interfaces[0])) != 2 {
		a.fail(key, "the only interface is not the default one in bridge mode")
	}

	volumes := map[string]map[string]any{}
	for _, v := range list(m, path(fieldSpec, "volumes")...) {
		volumes[str(asMap(v), fieldName)] = asMap(v)
	}
	if got := str(volumes["boot"], "dataVolume", fieldName); got != a.names.BootDisk {
		a.fail(key, "boots from %q, not from the disk %q", got, a.names.BootDisk)
	}
	if got := str(volumes["cloudinitdisk"], "cloudInitNoCloud", "secretRef", fieldName); got != a.names.CloudInit {
		a.fail(key, "reads its seed from %q, not from the Secret %q", got, a.names.CloudInit)
	}
	if got := strMap(m, path(fieldSpec, "nodeSelector")...)["kubernetes.io/arch"]; got != a.in.Plan.Arch {
		a.fail(key, "runs on the architecture %q, not on %q, which the plan asks for", got, a.in.Plan.Arch)
	}
	if got := str(m, path(fieldSpec, "domain", "firmware", "uuid")...); got != string(sr.UID) {
		a.fail(key, "has the firmware UUID %q, not that of the SiteRouter, %q", got, sr.UID)
	}
}

// portSecurityAnnotation turns port security off for a pod, in kube-ovn.
const portSecurityAnnotation = "ovn.kubernetes.io/port_security"

func (a *audit) dataVolume() {
	key := "DataVolume/" + a.names.BootDisk
	m, ok := a.byKey[key]
	if !ok {
		return
	}
	// CDI gives the pod that imports the disk the labels of the disk. With the
	// instance label on it, that pod would be taken for the gateway by the network
	// policies, the Service and the Forwarder, and would lose its egress.
	labels := strMap(m, "metadata", "labels")
	if _, ok := labels[v1alpha1.LabelInstance]; ok {
		a.fail(key, "carries the label %s, which CDI would hand to the pod that imports the disk, and which makes a pod the gateway", v1alpha1.LabelInstance)
	}
	if labels[v1alpha1.LabelBootDiskOf] != a.in.Instance.Name {
		a.fail(key, "does not carry the label %s=%s", v1alpha1.LabelBootDiskOf, a.in.Instance.Name)
	}
	want, err := registryURL(a.in.Image)
	if err != nil {
		a.fail(key, "%v", err)
		return
	}
	if got := str(m, fieldSpec, "source", "registry", "url"); got != want {
		a.fail(key, "is imported from %q, not from %q: the image must be the one pinned by digest", got, want)
	}
	if source := asMap(at(m, fieldSpec, "source")); len(source) != 1 || source["registry"] == nil {
		a.fail(key, "has a source other than the registry")
	}
	if got, want := str(m, fieldSpec, "storage", "resources", "requests", "storage"), fmt.Sprintf("%dGi", a.in.Plan.DiskGiB); got != want {
		a.fail(key, "asks for %q, not for %q, which the plan needs", got, want)
	}
	a.madeForTheImage(key, m)
}

// madeForTheImage checks that an object that is named after the image says which
// one it was made for, so that a name that two images share is not mistaken for
// the one that is meant.
func (a *audit) madeForTheImage(key string, m map[string]any) {
	want, err := ImageDigest(a.in.Image)
	if err != nil {
		return
	}
	if got := strMap(m, "metadata", "annotations")[v1alpha1.AnnotationImageDigest]; got != want {
		a.fail(key, "is marked as made for the image %q, not for %q", got, want)
	}
}

func (a *audit) service() {
	key := "Service/" + a.names.Tunnel
	m, ok := a.byKey[key]
	if !ok {
		return
	}
	if got := str(m, fieldSpec, "type"); got != string(corev1.ServiceTypeLoadBalancer) {
		a.fail(key, "is of the type %q, not LoadBalancer", got)
	}
	if got := str(m, fieldSpec, "externalTrafficPolicy"); got != string(corev1.ServiceExternalTrafficPolicyLocal) {
		a.fail(key, "has externalTrafficPolicy %q, not Local: the address of the remote end would be lost", got)
	}
	selector := strMap(m, fieldSpec, "selector")
	if len(selector) != 1 || selector[v1alpha1.LabelInstance] != a.in.Instance.Name {
		a.fail(key, "selects %v, not the gateway alone", selector)
	}
	var ports []string
	for _, p := range list(m, fieldSpec, "ports") {
		pm := asMap(p)
		ports = append(ports, fmt.Sprintf("%s/%v", str(pm, "protocol"), at(pm, "port")))
	}
	slices.Sort(ports)
	if want := []string{"UDP/4500", "UDP/500"}; !slices.Equal(ports, want) {
		a.fail(key, "publishes %v, not %v", ports, want)
	}
	if got, want := str(m, fieldSpec, "loadBalancerClass"), a.in.Config.Service.LoadBalancerClass; got != want {
		a.fail(key, "has loadBalancerClass %q, not %q", got, want)
	}
}

func (a *audit) secrets(objs []client.Object) {
	secrets := map[string]*corev1.Secret{}
	for _, o := range objs {
		if s, ok := o.(*corev1.Secret); ok {
			secrets[s.Name] = s
		}
	}
	check := func(name string, want map[string][]byte) {
		key := "Secret/" + name
		s, ok := secrets[name]
		if !ok {
			return // complete reports a missing object
		}
		if len(s.Data) != len(want) {
			a.fail(key, "holds the keys %v, want %v", slices.Sorted(maps.Keys(s.Data)), slices.Sorted(maps.Keys(want)))
		}
		for k, v := range want {
			if !bytes.Equal(s.Data[k], v) {
				a.fail(key, "does not hold the value of %q the plan gave", k)
			}
		}
	}
	check(a.names.APIKey, a.in.Plan.Credentials)
	check(a.names.CloudInit, a.in.Plan.SeedFiles)
	if m, ok := a.byKey["Secret/"+a.names.CloudInit]; ok {
		a.madeForTheImage("Secret/"+a.names.CloudInit, m)
	}
	if OwnsPSKSecret(a.in.Instance) {
		check(a.names.PSK, map[string][]byte{"psk": a.in.PSK})
	}
}

func (a *audit) policies() {
	if a.in.Config.Isolation.Mode != IsolationCilium {
		return
	}
	name := a.in.Instance.Name
	for _, n := range []string{a.names.EgressDeny, a.names.Ingress} {
		key := "CiliumNetworkPolicy/" + n
		m, ok := a.byKey[key]
		if !ok {
			continue
		}
		if got := strMap(m, fieldSpec, "endpointSelector", "matchLabels"); len(got) != 1 || got[v1alpha1.LabelInstance] != name {
			a.fail(key, "selects %v, not the gateway alone", got)
		}
	}

	if m, ok := a.byKey["CiliumNetworkPolicy/"+a.names.EgressDeny]; ok {
		key := "CiliumNetworkPolicy/" + a.names.EgressDeny
		rules := list(m, fieldSpec, "egressDeny")
		var cidrs []any
		if len(rules) == 1 {
			cidrs = list(asMap(rules[0]), "toCIDR")
		}
		if !slices.Contains(cidrs, any(linkLocal)) {
			a.fail(key, "does not deny %s", linkLocal)
		}
	}

	if m, ok := a.byKey["CiliumNetworkPolicy/"+a.names.Ingress]; ok {
		key := "CiliumNetworkPolicy/" + a.names.Ingress
		rules := list(m, fieldSpec, "ingress")
		if len(rules) != 2 {
			a.fail(key, "has %d ingress rules, want the tunnel and the management port", len(rules))
			return
		}
		tunnel, mgmt := asMap(rules[0]), asMap(rules[1])
		if !slices.Equal(list(tunnel, "fromEntities"), []any{"world"}) || len(tunnel) != 2 || !samePorts(tunnel, "UDP/4500", "UDP/500") {
			a.fail(key, "the first rule is not the tunnel ports for the world alone")
		}
		from := list(mgmt, "fromEndpoints")
		if len(mgmt) != 2 || len(from) != 1 || !samePorts(mgmt, fmt.Sprintf("TCP/%d", a.in.Plan.ManagementPort)) {
			a.fail(key, "the second rule is not the management port for the endpoint of the operator alone")
		} else if sel := strMap(asMap(from[0]), "matchLabels"); sel["k8s:io.kubernetes.pod.namespace"] != a.in.Config.Isolation.Controller.Namespace || len(sel) < 2 {
			a.fail(key, "the management port is not admitted to the operator's namespace and labels")
		}
	}
}

func samePorts(rule map[string]any, want ...string) bool {
	var got []string
	for _, tp := range list(rule, "toPorts") {
		for _, p := range list(asMap(tp), "ports") {
			pm := asMap(p)
			got = append(got, fmt.Sprintf("%s/%s", str(pm, "protocol"), str(pm, "port")))
		}
	}
	slices.Sort(got)
	return slices.Equal(got, want)
}

// noSecretsOutsideSecrets checks that no credential and no pre-shared key is in
// an object that is not a Secret, where it would be readable by more than the
// Secrets are.
func (a *audit) noSecretsOutsideSecrets(objs []client.Object) {
	var values [][]byte
	for _, k := range slices.Sorted(maps.Keys(a.in.Plan.Credentials)) {
		values = append(values, a.in.Plan.Credentials[k])
	}
	if OwnsPSKSecret(a.in.Instance) {
		values = append(values, a.in.PSK)
	}
	for _, o := range objs {
		if _, isSecret := o.(*corev1.Secret); isSecret {
			continue
		}
		raw, err := json.Marshal(o)
		if err != nil {
			a.errs = append(a.errs, fmt.Errorf("an object cannot be read: %w", err))
			continue
		}
		for _, v := range values {
			if len(v) > 0 && bytes.Contains(raw, v) {
				m, _ := toMap(o)
				a.fail(str(m, "kind")+"/"+str(m, "metadata", fieldName), "holds a credential or the pre-shared key")
				break
			}
		}
	}
}

func at(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func str(m map[string]any, path ...string) string {
	s, _ := at(m, path...).(string)
	return s
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func list(m map[string]any, path ...string) []any {
	l, _ := at(m, path...).([]any)
	return l
}

func strMap(m map[string]any, path ...string) map[string]string {
	out := map[string]string{}
	for k, v := range asMap(at(m, path...)) {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}
