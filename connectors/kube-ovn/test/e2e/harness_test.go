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

// Package e2e runs the connector against a real apiserver: the grants, the
// routes and the status as the apiserver keeps them, the watches that wake the
// connector, and what is left when it stops and starts again.
//
// There is no kube-ovn here, and no kubeovn-webhook: a pod is made the way the
// webhook would make it (see webhook), and what kube-ovn makes of the
// annotations is what the notes on kube-ovn say, not something these tests see.
package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/controller"
	"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/grant"
	"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/routes"
	"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/selection"
)

var (
	cfg       *rest.Config
	scheme    = k8sruntime.NewScheme()
	k8sClient client.Client
)

const timeout = 30 * time.Second

// connectorUser is the ServiceAccount the deploy files run the connector as.
// The connector of the tests runs as it, with the ClusterRole generated from
// the RBAC markers and the admission policy of the deploy files: what it is let
// do in a cluster is what it is let do here.
const connectorUser = "system:serviceaccount:site-router-kube-ovn-connector:kube-ovn-connector"

// deployFiles are the files of deploy/ that bind the connector, put in place
// as they are shipped.
var deployFiles = []string{"rbac/role.yaml", "rbac/role_binding.yaml", "admission_policy.yaml"}

// asConnector is a configuration that acts as the ServiceAccount of the
// connector.
func asConnector() *rest.Config {
	c := rest.CopyConfig(cfg)
	c.Impersonate = rest.ImpersonationConfig{
		UserName: connectorUser,
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:site-router-kube-ovn-connector", "system:authenticated"},
	}
	return c
}

// install makes the objects of the files of dir.
func install(ctx context.Context, c client.Client, dir string, files ...string) error {
	decode := serializer.NewCodecFactory(scheme).UniversalDeserializer()
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		for doc := range strings.SplitSeq(string(data), "\n---\n") {
			if strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(doc), "---")) == "" {
				continue
			}
			obj, _, err := decode.Decode([]byte(doc), nil, nil)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			if err := c.Create(ctx, obj.(client.Object)); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	return nil
}

func TestMain(m *testing.M) {
	ctrl.SetLogger(zap.New(zap.WriteTo(io.Discard)))
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintln(os.Stderr, "KUBEBUILDER_ASSETS is not set: run these tests with `make test`")
		os.Exit(1)
	}
	for _, add := range []func(*k8sruntime.Scheme) error{clientgoscheme.AddToScheme, v1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			fmt.Fprintln(os.Stderr, "building the scheme:", err)
			os.Exit(1)
		}
	}
	_, file, _, _ := runtime.Caller(0)
	here := filepath.Dir(file)
	env := &envtest.Environment{
		// The CRD of the Forwarder is the one the operator generates and ships.
		CRDDirectoryPaths:     []string{filepath.Join(here, "..", "..", "..", "..", "config", "crd", "bases"), filepath.Join(here, "..", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	var err error
	if cfg, err = env.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "starting envtest:", err)
		os.Exit(1)
	}
	if k8sClient, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		fmt.Fprintln(os.Stderr, "creating a client:", err)
		_ = env.Stop()
		os.Exit(1)
	}
	if err := install(context.Background(), k8sClient, filepath.Join(here, "..", "..", "deploy"), deployFiles...); err != nil {
		fmt.Fprintln(os.Stderr, "installing the deploy files:", err)
		_ = env.Stop()
		os.Exit(1)
	}
	code := m.Run()
	if err := env.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "stopping envtest:", err)
	}
	os.Exit(code)
}

// connector is a running connector.
type connector struct {
	cancel context.CancelFunc
	done   chan error
}

// startConnector runs the connector as cmd/main.go does, with wrap around the
// client it reads and writes through, when wrap is not nil.
func startConnector(t *testing.T, wrap func(client.Client) client.Client) *connector {
	t.Helper()
	mgr, err := ctrl.NewManager(asConnector(), ctrl.Options{
		Scheme:                 scheme,
		Cache:                  controller.CacheOptions(),
		Client:                 client.Options{Cache: &client.CacheOptions{Unstructured: true}},
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             config.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := mgr.GetClient()
	if wrap != nil {
		c = wrap(c)
	}
	if err := (&controller.Reconciler{Client: c}).SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	conn := &connector{cancel: cancel, done: make(chan error, 1)}
	go func() { conn.done <- mgr.Start(ctx) }()
	t.Cleanup(conn.stop)
	return conn
}

func (c *connector) stop() {
	if c.cancel == nil {
		return
	}
	c.cancel()
	<-c.done
	c.cancel = nil
}

// world is a namespace of a tenant.
type world struct {
	t  *testing.T
	ns string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ns := &corev1.Namespace{GenerateName: "tenant-"}
	mustCreate(t, ns)
	// Nothing makes the default ServiceAccount of a namespace here, and a pod
	// cannot be made without it.
	mustCreate(t, &corev1.ServiceAccount{Name: "default", Namespace: ns.Name})
	return &world{t: t, ns: ns.Name}
}

func mustCreate(t *testing.T, o client.Object) {
	t.Helper()
	if err := k8sClient.Create(t.Context(), o); err != nil {
		t.Fatalf("creating %T %s: %v", o, o.GetName(), err)
	}
}

// webhook does to a pod what the kubeovn-webhook of Cozystack does when the pod
// is made: the routes of the Namespace go onto a pod that has none of its own,
// and the check of addresses is turned on for a pod that does not say.
func (w *world) webhook(p *corev1.Pod) {
	w.t.Helper()
	ns := w.namespace()
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	if v, ok := ns.Annotations[routes.Annotation]; ok {
		if _, own := p.Annotations[routes.Annotation]; !own {
			p.Annotations[routes.Annotation] = v
		}
	}
	if _, own := p.Annotations[grant.PortSecurityAnnotation]; !own {
		p.Annotations[grant.PortSecurityAnnotation] = "true"
	}
}

// pod makes a running pod with the address ip, through the webhook.
func (w *world) pod(name string, labels map[string]string, ip string, opts ...func(*corev1.Pod)) *corev1.Pod {
	w.t.Helper()
	p := &corev1.Pod{
		Name: name, Namespace: w.ns, Labels: labels,
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "registry.example/app"}}},
	}
	for _, o := range opts {
		o(p)
	}
	w.webhook(p)
	// Making a pod drops its status, so it is kept and written after.
	status := p.Status
	status.Phase = corev1.PodRunning
	status.PodIP = ip
	if ip != "" {
		status.PodIPs = []corev1.PodIP{{IP: ip}}
	}
	mustCreate(w.t, p)
	p.Status = status
	if err := k8sClient.Status().Update(w.t.Context(), p); err != nil {
		w.t.Fatal(err)
	}
	return p
}

// gateway is the VirtualMachineInstance of a gateway, and the forwarder of it.
type gateway struct {
	w    *world
	name string
	vmi  *unstructured.Unstructured
}

var gatewayLabels = func(instance string) map[string]string {
	return map[string]string{v1alpha1.LabelInstance: instance}
}

// newGateway makes the VirtualMachineInstance site-router-<instance>.
func (w *world) newGateway(instance string) *gateway {
	w.t.Helper()
	g := &gateway{w: w, name: "site-router-" + instance}
	g.vmi = &unstructured.Unstructured{}
	g.vmi.SetGroupVersionKind(selection.InstanceGVK)
	g.vmi.SetName(g.name)
	g.vmi.SetNamespace(w.ns)
	g.vmi.SetLabels(gatewayLabels(instance))
	mustCreate(w.t, g.vmi)
	return g
}

// launcher makes a pod the instance controls, as KubeVirt does, and lists it
// among the active pods of the instance when active is true.
func (g *gateway) launcher(name, ip string, active bool) *corev1.Pod {
	g.w.t.Helper()
	p := g.w.pod(name, gatewayLabels(g.name[len("site-router-"):]), ip, func(p *corev1.Pod) {
		p.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "kubevirt.io/v1", Kind: "VirtualMachineInstance", Name: g.name, UID: g.vmi.GetUID(), Controller: new(true),
		}}
	})
	if active {
		g.setActive(p)
	}
	return p
}

// setActive writes the active pods of the instance, as KubeVirt does.
func (g *gateway) setActive(pods ...*corev1.Pod) {
	g.w.t.Helper()
	active := map[string]any{}
	for _, p := range pods {
		active[string(p.UID)] = "node-1"
	}
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(selection.InstanceGVK)
	if err := k8sClient.Get(g.w.t.Context(), client.ObjectKeyFromObject(g.vmi), vmi); err != nil {
		g.w.t.Fatal(err)
	}
	if err := unstructured.SetNestedField(vmi.Object, active, "status", "activePods"); err != nil {
		g.w.t.Fatal(err)
	}
	if err := k8sClient.Status().Update(g.w.t.Context(), vmi); err != nil {
		g.w.t.Fatal(err)
	}
}

// forwarder makes the Forwarder of the gateway as the operator does, with
// cidrs.
func (g *gateway) forwarder(cidrs ...string) *v1alpha1.Forwarder {
	g.w.t.Helper()
	f := &v1alpha1.Forwarder{
		Name: g.name, Namespace: g.w.ns,
		Spec: v1alpha1.ForwarderSpec{
			WorkloadSelector: metav1.LabelSelector{MatchLabels: gatewayLabels(g.name[len("site-router-"):])},
			ScopeSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: v1alpha1.LabelInstance, Operator: metav1.LabelSelectorOpDoesNotExist},
			}},
		},
	}
	for _, c := range cidrs {
		f.Spec.CIDRs = append(f.Spec.CIDRs, v1alpha1.IPv4CIDR(c))
	}
	mustCreate(g.w.t, f)
	return f
}

// ask changes the networks the Forwarder asks for.
func (g *gateway) ask(cidrs ...string) {
	g.w.t.Helper()
	f := g.get()
	f.Spec.CIDRs = nil
	for _, c := range cidrs {
		f.Spec.CIDRs = append(f.Spec.CIDRs, v1alpha1.IPv4CIDR(c))
	}
	if err := k8sClient.Update(g.w.t.Context(), f); err != nil {
		g.w.t.Fatal(err)
	}
}

func (g *gateway) get() *v1alpha1.Forwarder {
	g.w.t.Helper()
	f := &v1alpha1.Forwarder{}
	if err := k8sClient.Get(g.w.t.Context(), types.NamespacedName{Namespace: g.w.ns, Name: g.name}, f); err != nil {
		g.w.t.Fatal(err)
	}
	return f
}

func (w *world) namespace() *corev1.Namespace {
	w.t.Helper()
	ns := &corev1.Namespace{}
	if err := k8sClient.Get(w.t.Context(), types.NamespacedName{Name: w.ns}, ns); err != nil {
		w.t.Fatal(err)
	}
	return ns
}

func (w *world) getPod(name string) *corev1.Pod {
	w.t.Helper()
	p := &corev1.Pod{}
	if err := k8sClient.Get(w.t.Context(), types.NamespacedName{Namespace: w.ns, Name: name}, p); err != nil {
		w.t.Fatal(err)
	}
	return p
}

// eventually waits for check to pass, and fails the test with what it said last
// when it does not within the timeout.
func eventually(t *testing.T, what string, check func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var err error
	for time.Now().Before(deadline) {
		if err = check(); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s: %v", what, err)
}

// consistently checks that check keeps passing for a while: what a connector
// that is not meant to act must show.
func consistently(t *testing.T, what string, d time.Duration, check func() error) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if err := check(); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// condition waits for a condition of the Forwarder to have status and reason.
func (g *gateway) condition(typ string, status metav1.ConditionStatus, reason string) *v1alpha1.Forwarder {
	g.w.t.Helper()
	var f *v1alpha1.Forwarder
	eventually(g.w.t, fmt.Sprintf("%s of %s is %s/%s", typ, g.name, status, reason), func() error {
		f = g.get()
		for _, c := range f.Status.Conditions {
			if c.Type == typ {
				if c.Status != status || c.Reason != reason || c.ObservedGeneration != f.Generation {
					return fmt.Errorf("it is %s/%s for generation %d of %d: %s", c.Status, c.Reason, c.ObservedGeneration, f.Generation, c.Message)
				}
				return nil
			}
		}
		return errors.New("there is no such condition")
	})
	return f
}
