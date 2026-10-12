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

package selection

import (
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/routes"
)

const (
	ns      = "tenant-a"
	name    = "site-router-branch"
	vmiUID  = types.UID("vmi-uid")
	podUID  = types.UID("pod-uid")
	gwIP    = "10.244.1.9"
	webIP   = "10.244.1.5"
	otherGW = "site-router-head"
)

var (
	t0      = metav1.NewTime(time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC))
	gwLabel = map[string]string{v1alpha1.LabelInstance: "branch"}
)

func forwarder(n string, workload map[string]string) *v1alpha1.Forwarder {
	return &v1alpha1.Forwarder{
		Name: n, Namespace: ns, UID: types.UID(n),
		Spec: v1alpha1.ForwarderSpec{
			WorkloadSelector: metav1.LabelSelector{MatchLabels: workload},
			ScopeSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: v1alpha1.LabelInstance, Operator: metav1.LabelSelectorOpDoesNotExist},
			}},
		},
	}
}

func controlledBy(vmiName string, uid types.UID) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "kubevirt.io/v1", Kind: "VirtualMachineInstance", Name: vmiName, UID: uid, Controller: new(true),
		}}
	}
}

func pod(n string, lbls map[string]string, ip string, opts ...func(*corev1.Pod)) corev1.Pod {
	p := corev1.Pod{
		Name: n, Namespace: ns, UID: types.UID(n), Labels: lbls, CreationTimestamp: t0,
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip},
	}
	for _, o := range opts {
		o(&p)
	}
	return p
}

func gatewayPod(opts ...func(*corev1.Pod)) corev1.Pod {
	p := pod("virt-launcher-"+name+"-x", gwLabel, gwIP, controlledBy(name, vmiUID))
	p.UID = podUID
	for _, o := range opts {
		o(&p)
	}
	return p
}

func instance(active ...types.UID) *Instance {
	in := &Instance{Name: name, UID: vmiUID, ActivePods: map[types.UID]bool{}}
	for _, a := range active {
		in.ActivePods[a] = true
	}
	return in
}

func TestJudge(t *testing.T) {
	f := forwarder(name, gwLabel)
	workload := labels.SelectorFromSet(gwLabel)
	deleting := metav1.NewTime(t0.Add(time.Minute))

	for _, tc := range []struct {
		what string
		pod  corev1.Pod
		vmi  *Instance
		want Verdict
	}{
		{"the pod of the instance", gatewayPod(), instance(podUID), Gateway},
		{"a pod KubeVirt has not listed yet", gatewayPod(), instance(), Unproven},
		{"a pod the instance does not run in", gatewayPod(), instance("another-pod"), NotGateway},
		{"no instance", gatewayPod(), nil, NotGateway},
		{"an instance of another name", gatewayPod(), &Instance{Name: "x", UID: vmiUID, ActivePods: map[types.UID]bool{podUID: true}}, NotGateway},
		{"an owner with the name and not the UID", gatewayPod(controlledBy(name, "forged")), instance(podUID), NotGateway},
		{"an owner of another name", gatewayPod(controlledBy("other", vmiUID)), instance(podUID), NotGateway},
		{"no owner", gatewayPod(func(p *corev1.Pod) { p.OwnerReferences = nil }), instance(podUID), NotGateway},
		{"an owner that does not control it", gatewayPod(func(p *corev1.Pod) { p.OwnerReferences[0].Controller = nil }), instance(podUID), NotGateway},
		{"an owner of another kind", gatewayPod(func(p *corev1.Pod) { p.OwnerReferences[0].Kind = "VirtualMachine" }), instance(podUID), NotGateway},
		{"an owner of another group", gatewayPod(func(p *corev1.Pod) { p.OwnerReferences[0].APIVersion = "example.com/v1" }), instance(podUID), NotGateway},
		{"no label", gatewayPod(func(p *corev1.Pod) { p.Labels = nil }), instance(podUID), NotGateway},
		{"being deleted", gatewayPod(func(p *corev1.Pod) { p.DeletionTimestamp = &deleting }), instance(podUID), NotGateway},
		{"finished", gatewayPod(func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }), instance(podUID), NotGateway},
		{"failed", gatewayPod(func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed }), instance(podUID), NotGateway},
		{"on the network of its node", gatewayPod(func(p *corev1.Pod) { p.Spec.HostNetwork = true }), instance(podUID), NotGateway},
		{"in another namespace", gatewayPod(func(p *corev1.Pod) { p.Namespace = "other" }), instance(podUID), NotGateway},
	} {
		if got := Judge(&tc.pod, f, workload, tc.vmi); got != tc.want {
			t.Errorf("%s: Judge = %v, want %v", tc.what, got, tc.want)
		}
	}
}

func TestInstanceOf(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name, "uid": string(vmiUID)},
		"status":   map[string]any{"activePods": map[string]any{string(podUID): "node-1"}},
	}}
	in := InstanceOf(u)
	if in.Name != name || in.UID != vmiUID || !in.ActivePods[podUID] || len(in.ActivePods) != 1 {
		t.Errorf("InstanceOf = %+v", in)
	}
	u = &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": name}}}
	if in := InstanceOf(u); len(in.ActivePods) != 0 {
		t.Errorf("an instance with no status has active pods: %+v", in)
	}
}

func TestAddress(t *testing.T) {
	for _, tc := range []struct {
		status corev1.PodStatus
		want   string
	}{
		{corev1.PodStatus{PodIP: gwIP}, gwIP},
		{corev1.PodStatus{PodIP: "fd00::5", PodIPs: []corev1.PodIP{{IP: "fd00::5"}, {IP: gwIP}}}, gwIP},
		{corev1.PodStatus{PodIP: "fd00::5"}, ""},
		{corev1.PodStatus{}, ""},
	} {
		a, ok := Address(&corev1.Pod{Status: tc.status})
		got := ""
		if ok {
			got = a.String()
		}
		if got != tc.want {
			t.Errorf("Address(%+v) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestResolve(t *testing.T) {
	f := forwarder(name, gwLabel)
	sibling := *forwarder(otherGW, map[string]string{"app": "other-gateway"})
	deleting := metav1.NewTime(t0.Add(time.Minute))
	pods := []corev1.Pod{
		gatewayPod(),
		// The label of the gateway alone earns nothing, and the pod is no
		// workload of the tenant either.
		pod("impostor", gwLabel, "10.244.1.66"),
		// The gateway of the sibling, by its selector.
		pod("other-gw", map[string]string{"app": "other-gateway"}, "10.244.1.99"),
		pod("web-0", map[string]string{"app": "web"}, webIP),
		pod("web-1", map[string]string{"app": "web"}, "10.244.1.6", func(p *corev1.Pod) { p.DeletionTimestamp = &deleting }),
		pod("job", map[string]string{"app": "job"}, "10.244.1.7", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }),
		pod("agent", map[string]string{"app": "agent"}, "192.168.0.10", func(p *corev1.Pod) { p.Spec.HostNetwork = true }),
		pod("starting", map[string]string{"app": "web"}, ""),
		pod("elsewhere", map[string]string{"app": "web"}, "10.244.1.8", func(p *corev1.Pod) { p.Namespace = "other" }),
	}
	services := []corev1.Service{
		{Name: "web", Namespace: ns, Spec: corev1.ServiceSpec{ClusterIPs: []string{"10.96.0.20", "fd00:96::20"}}},
		{Name: "headless", Namespace: ns, Spec: corev1.ServiceSpec{ClusterIPs: []string{"None"}}},
		{Name: "other", Namespace: "other", Spec: corev1.ServiceSpec{ClusterIPs: []string{"10.96.0.30"}}},
	}

	v, err := Resolve(f, []v1alpha1.Forwarder{sibling, *f}, pods, services, instance(podUID))
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Addresses(); !slices.Equal(got, []string{gwIP}) {
		t.Errorf("gateway addresses = %v, want [%s]", got, gwIP)
	}
	if want := []string{"10.96.0.20", webIP}; !slices.Equal(v.Scope, want) {
		t.Errorf("scope = %v, want %v", v.Scope, want)
	}
	names := make([]string, 0, len(v.ScopePods))
	for _, p := range v.ScopePods {
		names = append(names, p.Name)
	}
	if want := []string{"starting", "web-0"}; !slices.Equal(names, want) {
		t.Errorf("pods of the scope = %v, want %v", names, want)
	}
}

// An empty scope selector selects every pod of the namespace that is not a
// gateway.
func TestResolveWithAnEmptyScope(t *testing.T) {
	f := forwarder(name, gwLabel)
	f.Spec.ScopeSelector = metav1.LabelSelector{}
	pods := []corev1.Pod{gatewayPod(), pod("web-0", nil, webIP), pod("impostor", gwLabel, "10.244.1.66")}
	v, err := Resolve(f, nil, pods, nil, instance(podUID))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(v.Scope, []string{webIP}) {
		t.Errorf("scope = %v, want [%s]", v.Scope, webIP)
	}
}

// While KubeVirt moves the instance there are two pods, and the routes go to the
// one it runs in until the move is done: the older.
func TestResolveOrdersTheGatewaysByAge(t *testing.T) {
	f := forwarder(name, gwLabel)
	target := gatewayPod(func(p *corev1.Pod) {
		p.Name, p.UID, p.Status.PodIP = "virt-launcher-target", "target", "10.244.1.10"
		p.CreationTimestamp = metav1.NewTime(t0.Add(time.Minute))
	})
	v, err := Resolve(f, nil, []corev1.Pod{target, gatewayPod()}, nil, instance(podUID, "target"))
	if err != nil {
		t.Fatal(err)
	}
	if hops := v.Hops(); len(hops) != 2 || hops[0].String() != gwIP {
		t.Errorf("hops = %v, want %s first", hops, gwIP)
	}
}

func TestPending(t *testing.T) {
	mine := []routes.Entry{{Dst: "192.0.2.0/24", Gw: gwIP}}
	with := func(value string) func(*corev1.Pod) {
		return func(p *corev1.Pod) { p.Annotations = map[string]string{routes.Annotation: value} }
	}
	scope := []*corev1.Pod{
		new(pod("old", nil, webIP)),
		new(pod("new", nil, webIP, with(`[{"dst":"192.0.2.0/24","gw":"`+gwIP+`"}]`))),
		new(pod("stale", nil, webIP, with(`[{"dst":"192.0.2.0/24","gw":"10.244.1.1"}]`))),
		new(pod("unreadable", nil, webIP, with(`nonsense`))),
	}
	if got := Pending(scope, mine); !slices.Equal(got, []string{"old", "stale"}) {
		t.Errorf("Pending = %v, want [old stale]", got)
	}
	if got := Pending(scope, nil); got != nil {
		t.Errorf("Pending with no routes = %v, want none", got)
	}
}
