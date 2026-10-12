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

// Package crd tests the generated CRDs against a real apiserver: the schema and
// the CEL rules are only worth what the apiserver makes of them.
package crd_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
)

var k8sClient client.Client

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintln(os.Stderr, "KUBEBUILDER_ASSETS is not set: run these tests with `make test` or `make test-crd`")
		os.Exit(1)
	}
	scheme := runtime.NewScheme()
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "starting envtest:", err)
		os.Exit(1)
	}
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintln(os.Stderr, "creating a client:", err)
		_ = env.Stop()
		os.Exit(1)
	}
	code := m.Run()
	if err := env.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "stopping envtest:", err)
	}
	os.Exit(code)
}

// create creates obj and removes it when the test ends.
func create(t *testing.T, obj client.Object) error {
	t.Helper()
	err := k8sClient.Create(t.Context(), obj)
	if err == nil {
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), obj) })
	}
	return err
}

// wantInvalid fails unless err is a rejection that mentions every fragment.
func wantInvalid(t *testing.T, err error, fragments ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("the object was accepted, want a rejection mentioning %q", fragments)
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("the error is not a validation failure: %v", err)
	}
	for _, f := range fragments {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("the error does not mention %q: %v", f, err)
		}
	}
}

func asn(n int64) *v1alpha1.ASN {
	a := v1alpha1.ASN(n)
	return &a
}

func manyCIDRs(n int) []v1alpha1.IPv4CIDR {
	out := make([]v1alpha1.IPv4CIDR, n)
	for i := range out {
		out[i] = v1alpha1.IPv4CIDR(fmt.Sprintf("10.0.%d.0/24", i))
	}
	return out
}

func siteRouter(mutate func(*v1alpha1.SiteRouter)) *v1alpha1.SiteRouter {
	s := &v1alpha1.SiteRouter{
		GenerateName: "t-",
		Namespace:    "default",
		Spec:         v1alpha1.SiteRouterSpec{RemoteCIDRs: []v1alpha1.IPv4CIDR{"192.0.2.0/24"}},
	}
	if mutate != nil {
		mutate(s)
	}
	return s
}

func TestSiteRouterAccepts(t *testing.T) {
	cases := map[string]func(*v1alpha1.SiteRouter){
		"the minimal object": func(s *v1alpha1.SiteRouter) { s.Spec = v1alpha1.SiteRouterSpec{} },
		"a peer that is not known yet": func(s *v1alpha1.SiteRouter) {
			s.Spec.Peer.Address = ""
		},
		"everything set": func(s *v1alpha1.SiteRouter) {
			s.Spec = v1alpha1.SiteRouterSpec{
				Peer: v1alpha1.Peer{
					Address: "203.0.113.10",
					Auth:    &v1alpha1.PeerAuth{SecretRef: &v1alpha1.LocalSecretRef{Name: "psk"}},
				},
				RemoteCIDRs:  []v1alpha1.IPv4CIDR{"192.0.2.0/24", "198.51.100.0/25"},
				StaticRoutes: []v1alpha1.StaticRoute{{Destination: "10.9.0.0/16", NextHop: "10.0.0.1"}},
				BGP: v1alpha1.BGP{
					Enabled:   true,
					LocalASN:  asn(65001),
					Neighbors: []v1alpha1.BGPNeighbor{{Address: "10.0.0.2", RemoteASN: 4294967295}},
				},
				Resources:    v1alpha1.Resources{CPU: 4, Memory: resource.MustParse("4Gi")},
				StorageClass: "fast",
			}
		},
		"a DNS name as the peer address": func(s *v1alpha1.SiteRouter) { s.Spec.Peer.Address = "vpn.example.org" },
		"sixteen remote CIDRs":           func(s *v1alpha1.SiteRouter) { s.Spec.RemoteCIDRs = manyCIDRs(16) },
		"a name of 40 characters":        func(s *v1alpha1.SiteRouter) { s.GenerateName, s.Name = "", strings.Repeat("a", 40) },
		"BGP switched off without an ASN": func(s *v1alpha1.SiteRouter) {
			s.Spec.BGP = v1alpha1.BGP{Enabled: false}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if err := create(t, siteRouter(mutate)); err != nil {
				t.Fatalf("a valid object was rejected: %v", err)
			}
		})
	}
}

func TestSiteRouterRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*v1alpha1.SiteRouter)
		want   string
	}{
		{"IPv6 remote CIDR", func(s *v1alpha1.SiteRouter) { s.Spec.RemoteCIDRs = []v1alpha1.IPv4CIDR{"2001:db8::/32"} }, "must be an IPv4 CIDR"},
		{"remote CIDR that is not one", func(s *v1alpha1.SiteRouter) { s.Spec.RemoteCIDRs = []v1alpha1.IPv4CIDR{"garbage"} }, "must be an IPv4 CIDR"},
		{"prefix longer than 32", func(s *v1alpha1.SiteRouter) { s.Spec.RemoteCIDRs = []v1alpha1.IPv4CIDR{"192.0.2.0/33"} }, "must be an IPv4 CIDR"},
		{"default route as remote CIDR", func(s *v1alpha1.SiteRouter) { s.Spec.RemoteCIDRs = []v1alpha1.IPv4CIDR{"0.0.0.0/0"} }, "a default route (/0) is not allowed"},
		{"remote CIDR with host bits", func(s *v1alpha1.SiteRouter) { s.Spec.RemoteCIDRs = []v1alpha1.IPv4CIDR{"192.0.2.5/24"} }, "host bits must be zero"},
		{"repeated remote CIDR", func(s *v1alpha1.SiteRouter) {
			s.Spec.RemoteCIDRs = []v1alpha1.IPv4CIDR{"192.0.2.0/24", "192.0.2.0/24"}
		}, "Duplicate value"},
		{"seventeen remote CIDRs", func(s *v1alpha1.SiteRouter) { s.Spec.RemoteCIDRs = manyCIDRs(17) }, "at most 16"},

		{"default route as static route", func(s *v1alpha1.SiteRouter) {
			s.Spec.StaticRoutes = []v1alpha1.StaticRoute{{Destination: "0.0.0.0/0", NextHop: "10.0.0.1"}}
		}, "a default route (/0) is not allowed"},
		{"IPv6 next hop", func(s *v1alpha1.SiteRouter) {
			s.Spec.StaticRoutes = []v1alpha1.StaticRoute{{Destination: "10.9.0.0/16", NextHop: "::1"}}
		}, "must be an IPv4 address"},
		{"empty next hop", func(s *v1alpha1.SiteRouter) {
			s.Spec.StaticRoutes = []v1alpha1.StaticRoute{{Destination: "10.9.0.0/16"}}
		}, "must be an IPv4 address"},
		{"two routes to one destination", func(s *v1alpha1.SiteRouter) {
			s.Spec.StaticRoutes = []v1alpha1.StaticRoute{
				{Destination: "10.9.0.0/16", NextHop: "10.0.0.1"},
				{Destination: "10.9.0.0/16", NextHop: "10.0.0.2"},
			}
		}, "Duplicate value"},

		{"BGP on without a local ASN", func(s *v1alpha1.SiteRouter) { s.Spec.BGP = v1alpha1.BGP{Enabled: true} }, "localASN is required when bgp.enabled is true"},
		{"local ASN 0", func(s *v1alpha1.SiteRouter) { s.Spec.BGP = v1alpha1.BGP{Enabled: true, LocalASN: asn(0)} }, "greater than or equal to 1"},
		{"local ASN beyond 32 bits", func(s *v1alpha1.SiteRouter) { s.Spec.BGP = v1alpha1.BGP{Enabled: true, LocalASN: asn(4294967296)} }, "less than or equal to 4294967295"},
		{"IPv6 BGP neighbor", func(s *v1alpha1.SiteRouter) {
			s.Spec.BGP = v1alpha1.BGP{Enabled: true, LocalASN: asn(65001), Neighbors: []v1alpha1.BGPNeighbor{{Address: "2001:db8::1", RemoteASN: 65002}}}
		}, "must be an IPv4 address"},
		{"BGP neighbor ASN 0", func(s *v1alpha1.SiteRouter) {
			s.Spec.BGP = v1alpha1.BGP{Enabled: true, LocalASN: asn(65001), Neighbors: []v1alpha1.BGPNeighbor{{Address: "10.0.0.2"}}}
		}, "greater than or equal to 1"},

		{"peer address with a space", func(s *v1alpha1.SiteRouter) { s.Spec.Peer.Address = "not an address" }, "should match"},
		{"empty secret name", func(s *v1alpha1.SiteRouter) {
			s.Spec.Peer.Auth = &v1alpha1.PeerAuth{SecretRef: &v1alpha1.LocalSecretRef{}}
		}, "at least 1 chars"},
		{"negative CPU", func(s *v1alpha1.SiteRouter) { s.Spec.Resources.CPU = -1 }, "greater than or equal to 1"},
		{"a name of 41 characters", func(s *v1alpha1.SiteRouter) { s.GenerateName, s.Name = "", strings.Repeat("a", 41) }, "metadata.name must be at most 40 characters"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantInvalid(t, create(t, siteRouter(c.mutate)), c.want)
		})
	}
}

// networkHolders put one network into each place the IPv4CIDR type is used, so
// that a rule on the type is checked wherever the type appears.
var networkHolders = []struct {
	name string
	with func(v1alpha1.IPv4CIDR) client.Object
}{
	{"remoteCIDRs", func(c v1alpha1.IPv4CIDR) client.Object {
		return siteRouter(func(s *v1alpha1.SiteRouter) { s.Spec.RemoteCIDRs = []v1alpha1.IPv4CIDR{c} })
	}},
	{"staticRoutes", func(c v1alpha1.IPv4CIDR) client.Object {
		return siteRouter(func(s *v1alpha1.SiteRouter) {
			s.Spec.StaticRoutes = []v1alpha1.StaticRoute{{Destination: c, NextHop: "10.0.0.1"}}
		})
	}},
	{"forwarder cidrs", func(c v1alpha1.IPv4CIDR) client.Object {
		return forwarder(func(f *v1alpha1.Forwarder) { f.Spec.CIDRs = []v1alpha1.IPv4CIDR{c} })
	}},
}

// The schema refuses the networks that no remote site can have whatever the
// cluster is: nothing shorter than /8, which takes in the two halves of the
// default route, and nothing that overlaps the "this network", loopback,
// link-local or multicast ranges, whether it lies inside one, is one, or
// contains one. The neighbors of those ranges and the shortest prefix allowed
// are accepted, so that a rule that is too wide is caught as well as one that is
// too narrow.
//
// The cases that lie inside or around a range are what keeps the rule on
// containsIP: the CEL containsCIDR of the 1.31 apiserver is true only for equal
// networks, and a rule written with it passes 127.0.0.1/32 there.
func TestNetworkBoundaries(t *testing.T) {
	const (
		tooShort = "a network shorter than /8 is not allowed"
		reserved = "must not overlap 0.0.0.0/8, loopback"
	)
	cases := []struct {
		cidr v1alpha1.IPv4CIDR
		want string // empty: accepted
	}{
		{"10.0.0.0/8", ""},
		{"1.0.0.0/8", ""},
		{"126.0.0.0/8", ""},
		{"128.0.0.0/8", ""},
		{"169.253.0.0/16", ""},
		{"169.255.0.0/16", ""},
		{"223.255.255.0/24", ""},

		{"10.0.0.0/7", tooShort},
		{"0.0.0.0/1", tooShort},
		{"128.0.0.0/1", tooShort},

		{"0.0.0.0/8", reserved},
		{"0.1.0.0/16", reserved},
		{"127.0.0.0/8", reserved},
		{"127.0.0.1/32", reserved},
		{"169.254.0.0/16", reserved},
		{"169.254.169.254/32", reserved},
		{"169.0.0.0/8", reserved},
		{"224.0.0.0/4", reserved},
		{"239.255.255.250/32", reserved},
	}
	for _, h := range networkHolders {
		for _, c := range cases {
			t.Run(h.name+"/"+string(c.cidr), func(t *testing.T) {
				err := create(t, h.with(c.cidr))
				if c.want == "" {
					if err != nil {
						t.Fatalf("%s was rejected: %v", c.cidr, err)
					}
					return
				}
				wantInvalid(t, err, c.want)
			})
		}
	}
}

// What the schema cannot know is the cluster's own networks. A network that
// overlaps them is well-formed and passes; the controller judges it against the
// cluster. This test states that division of labor, and is the one to change if
// it moves.
func TestSchemaDoesNotJudgeClusterNetworks(t *testing.T) {
	for _, cidr := range []v1alpha1.IPv4CIDR{"10.244.0.0/16", "10.96.0.0/12", "100.64.0.0/16", "172.16.0.0/12"} {
		t.Run(string(cidr), func(t *testing.T) {
			if err := create(t, siteRouter(func(s *v1alpha1.SiteRouter) { s.Spec.RemoteCIDRs = []v1alpha1.IPv4CIDR{cidr} })); err != nil {
				t.Fatalf("the schema now rejects %s; update this test and the type's description: %v", cidr, err)
			}
		})
	}
}

// The apiserver reports every violated rule, not only the first, so a tenant
// who made three mistakes sees three messages.
func TestSiteRouterReportsEveryViolation(t *testing.T) {
	err := create(t, siteRouter(func(s *v1alpha1.SiteRouter) {
		s.Spec.RemoteCIDRs = []v1alpha1.IPv4CIDR{"2001:db8::/32", "0.0.0.0/0", "192.0.2.5/24"}
		s.Spec.BGP = v1alpha1.BGP{Enabled: true}
	}))
	wantInvalid(t, err,
		"must be an IPv4 CIDR",
		"a default route (/0) is not allowed",
		"host bits must be zero",
		"localASN is required when bgp.enabled is true",
	)
}

func TestSiteRouterDefaults(t *testing.T) {
	s := siteRouter(nil)
	if err := create(t, s); err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.SiteRouter
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(s), &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Resources.CPU != 2 {
		t.Errorf("default cpu = %d, want 2", got.Spec.Resources.CPU)
	}
	if want := resource.MustParse("2Gi"); got.Spec.Resources.Memory.Cmp(want) != 0 {
		t.Errorf("default memory = %s, want 2Gi", got.Spec.Resources.Memory.String())
	}
}

// A transition rule on the storageClass field itself is not evaluated when the
// field is missing from either version of the object, so it would let a value
// be added or removed after creation. The rule sits on the spec and reads the
// field as optional; this test is what keeps it there.
func TestSiteRouterStorageClassIsImmutable(t *testing.T) {
	cases := []struct {
		name          string
		before, after string
		allowed       bool
	}{
		{"changed", "fast", "slow", false},
		{"removed", "fast", "", false},
		{"added after creation", "", "fast", false},
		{"unchanged", "fast", "fast", true},
		{"left empty", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := siteRouter(func(s *v1alpha1.SiteRouter) { s.Spec.StorageClass = c.before })
			if err := create(t, s); err != nil {
				t.Fatal(err)
			}
			// Another field changes too, so that the update is not a no-op.
			s.Spec.StorageClass = c.after
			s.Spec.RemoteCIDRs = []v1alpha1.IPv4CIDR{"192.0.2.0/24", "198.51.100.0/24"}
			err := k8sClient.Update(t.Context(), s)
			if c.allowed {
				if err != nil {
					t.Fatalf("an update that keeps storageClass was rejected: %v", err)
				}
				return
			}
			wantInvalid(t, err, "storageClass is immutable")
		})
	}
}

func TestSiteRouterStatusRoundTrip(t *testing.T) {
	s := siteRouter(nil)
	if err := create(t, s); err != nil {
		t.Fatal(err)
	}
	s.Status = v1alpha1.SiteRouterStatus{
		ObservedGeneration: s.Generation,
		TunnelAddress:      "198.51.100.7",
		PSKSecretRef:       &v1alpha1.LocalSecretRef{Name: s.Name + "-psk"},
		Engine:             v1alpha1.EngineStatus{Name: "fake", Version: "1.0", ImageDigest: "sha256:abc"},
		Conditions: []metav1.Condition{{
			Type:               v1alpha1.ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             "AllGood",
			LastTransitionTime: metav1.Now(),
		}},
	}
	if err := k8sClient.Status().Update(t.Context(), s); err != nil {
		t.Fatalf("writing the status: %v", err)
	}
	var got v1alpha1.SiteRouter
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(s), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.TunnelAddress != "198.51.100.7" || got.Status.Engine.ImageDigest != "sha256:abc" || len(got.Status.Conditions) != 1 {
		t.Errorf("the status did not survive a round trip: %+v", got.Status)
	}
}

func forwarder(mutate func(*v1alpha1.Forwarder)) *v1alpha1.Forwarder {
	f := &v1alpha1.Forwarder{
		GenerateName: "f-",
		Namespace:    "default",
		Spec: v1alpha1.ForwarderSpec{
			WorkloadSelector: metav1.LabelSelector{MatchLabels: map[string]string{"vm.kubevirt.io/name": "gw"}},
			CIDRs:            []v1alpha1.IPv4CIDR{"192.0.2.0/24"},
		},
	}
	if mutate != nil {
		mutate(f)
	}
	return f
}

func TestForwarderAccepts(t *testing.T) {
	cases := map[string]func(*v1alpha1.Forwarder){
		"labels and CIDRs": nil,
		"no CIDRs yet":     func(f *v1alpha1.Forwarder) { f.Spec.CIDRs = nil },
		"expressions only": func(f *v1alpha1.Forwarder) {
			f.Spec.WorkloadSelector = metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "app", Operator: metav1.LabelSelectorOpExists},
			}}
		},
		"a scope selector": func(f *v1alpha1.Forwarder) {
			f.Spec.ScopeSelector = metav1.LabelSelector{MatchLabels: map[string]string{"tier": "app"}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if err := create(t, forwarder(mutate)); err != nil {
				t.Fatalf("a valid object was rejected: %v", err)
			}
		})
	}
}

func TestForwarderRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*v1alpha1.Forwarder)
		want   string
	}{
		{"an empty workload selector", func(f *v1alpha1.Forwarder) { f.Spec.WorkloadSelector = metav1.LabelSelector{} }, "workloadSelector must select something"},
		{"a workload selector with empty maps", func(f *v1alpha1.Forwarder) {
			f.Spec.WorkloadSelector = metav1.LabelSelector{MatchLabels: map[string]string{}, MatchExpressions: []metav1.LabelSelectorRequirement{}}
		}, "workloadSelector must select something"},
		{"a default route", func(f *v1alpha1.Forwarder) { f.Spec.CIDRs = []v1alpha1.IPv4CIDR{"0.0.0.0/0"} }, "a default route (/0) is not allowed"},
		{"an IPv6 CIDR", func(f *v1alpha1.Forwarder) { f.Spec.CIDRs = []v1alpha1.IPv4CIDR{"2001:db8::/32"} }, "must be an IPv4 CIDR"},
		{"seventeen CIDRs", func(f *v1alpha1.Forwarder) { f.Spec.CIDRs = manyCIDRs(17) }, "at most 16"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantInvalid(t, create(t, forwarder(c.mutate)), c.want)
		})
	}
}

// The samples are what a user copies first. They go to the apiserver as plain
// YAML, not through the Go types, so a sample that the schema rejects fails here.
func TestSamplesAreAccepted(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "config", "samples", "siterouter_*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no samples found: %v", err)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			u := &unstructured.Unstructured{}
			if err := yaml.Unmarshal(data, &u.Object); err != nil {
				t.Fatal(err)
			}
			u.SetNamespace("default")
			if err := create(t, u); err != nil {
				t.Fatalf("the sample was rejected: %v", err)
			}
		})
	}
}

func TestForwarderStatusRoundTrip(t *testing.T) {
	f := forwarder(nil)
	if err := create(t, f); err != nil {
		t.Fatal(err)
	}
	f.Status = v1alpha1.ForwarderStatus{
		Forwarder:        v1alpha1.AddressSet{Addresses: []string{"10.244.1.9"}},
		Scope:            v1alpha1.AddressSet{Addresses: []string{"10.244.1.5", "10.244.1.6"}},
		PendingWorkloads: []string{"web-0"},
		Conditions: []metav1.Condition{{
			Type:               v1alpha1.ForwarderGranted,
			Status:             metav1.ConditionTrue,
			Reason:             "Granted",
			LastTransitionTime: metav1.Now(),
		}},
	}
	if err := k8sClient.Status().Update(t.Context(), f); err != nil {
		t.Fatalf("writing the status: %v", err)
	}
	var got v1alpha1.Forwarder
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(f), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Scope.Addresses) != 2 || len(got.Status.PendingWorkloads) != 1 || len(got.Status.Conditions) != 1 {
		t.Errorf("the status did not survive a round trip: %+v", got.Status)
	}
}
