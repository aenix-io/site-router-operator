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

// Package chart checks what the chart of the operator gives an installation: a
// configuration the operator starts with, the rights the code says it needs and
// no more, and the CRDs the code describes.
package chart

import (
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	"github.com/aenix-io/site-router-operator/internal/config"
	"github.com/aenix-io/site-router-operator/internal/gateway"
	"github.com/aenix-io/site-router-operator/internal/testchart"
)

const (
	release   = "srop"
	namespace = "site-router-system"
	fullname  = "srop-site-router-operator"
)

// repo is the root of the repository.
func repo() string { return filepath.Join(testchart.Dir(), "..", "..") }

// installation is what the chart renders for a value set, and the configuration
// the operator makes of it.
type installation struct {
	objs testchart.Objects
	cfg  config.Config
	dep  appsv1.Deployment
}

// install renders the chart and reads the configuration the way the operator
// does: from the file its --config flag names, in the volume mounted there, out
// of the ConfigMap of that volume, through the operator's own loader, which
// refuses a key it does not know and a value it cannot use.
func install(t *testing.T, args ...string) installation {
	t.Helper()
	in := installation{objs: testchart.Render(t, release, namespace, args...)}
	testchart.Into(t, in.objs.Get(t, "Deployment", fullname), &in.dep)

	containers := in.dep.Spec.Template.Spec.Containers
	if len(containers) != 1 || containers[0].Name != "manager" {
		t.Fatalf("the Deployment runs %d containers, want the manager alone", len(containers))
	}
	manager := containers[0]
	var file string
	for _, a := range manager.Args {
		if v, ok := strings.CutPrefix(a, "--config="); ok {
			file = v
		}
	}
	if file == "" {
		t.Fatalf("the manager is not told where its configuration is: %v", manager.Args)
	}
	var volume string
	for _, m := range manager.VolumeMounts {
		if m.MountPath == path.Dir(file) {
			volume = m.Name
		}
	}
	var configMap string
	for _, v := range in.dep.Spec.Template.Spec.Volumes {
		if v.Name == volume && v.ConfigMap != nil {
			configMap = v.ConfigMap.Name
		}
	}
	if configMap == "" {
		t.Fatalf("no ConfigMap is mounted where the manager reads %s", file)
	}
	var cm corev1.ConfigMap
	testchart.Into(t, in.objs.Get(t, "ConfigMap", configMap), &cm)
	data, ok := cm.Data[path.Base(file)]
	if !ok {
		t.Fatalf("the ConfigMap %s has no key %s", configMap, path.Base(file))
	}

	cfg, err := config.Parse([]byte(data))
	if err != nil {
		t.Fatalf("the operator would not start with the configuration the chart gives it: %v\n%s", err, data)
	}
	in.cfg = cfg
	return in
}

// Every value set the chart is linted with gives the operator a configuration it
// starts with, so that a default of the chart that the operator would refuse
// fails here and not in a cluster. Each set also checks what it is there for.
func TestTheOperatorStartsWithTheConfigurationTheChartGivesIt(t *testing.T) {
	checks := map[string]func(*testing.T, installation){
		"minimal-values.yaml": func(t *testing.T, in installation) {
			if in.cfg.Upgrade.Policy != config.UpgradeAuto {
				t.Errorf("upgrade.policy = %q, want the default of the chart, auto", in.cfg.Upgrade.Policy)
			}
			if in.cfg.Gateway.Isolation.Mode != gateway.IsolationCilium {
				t.Errorf("isolation.mode = %q, want the default of the chart, cilium", in.cfg.Gateway.Isolation.Mode)
			}
			expectTheIdentitySelectsTheOperator(t, in)
		},
		"annotation-values.yaml": func(t *testing.T, in installation) {
			if in.cfg.Upgrade.Policy != config.UpgradeAnnotation || in.cfg.Upgrade.MaxConcurrent != 3 {
				t.Errorf("upgrade = %+v, want the annotation policy, three at once", in.cfg.Upgrade)
			}
			if got := in.cfg.Engine.Names(); !slices.Equal(got, []string{"1.1", "1.0", "0.9"}) || in.cfg.Engine.Latest != "1.1" {
				t.Errorf("engine versions %v, latest %q", got, in.cfg.Engine.Latest)
			}
			if in.cfg.ClusterNetworks.JoinCIDR != "100.64.0.0/16" || len(in.cfg.ClusterNetworks.ExtraCIDRs) != 1 {
				t.Errorf("clusterNetworks = %+v", in.cfg.ClusterNetworks)
			}
		},
		"isolation-external-values.yaml": func(t *testing.T, in installation) {
			if iso := in.cfg.Gateway.Isolation; iso.Mode != gateway.IsolationExternal || iso.Controller.Namespace != "" || len(iso.Controller.Labels) != 0 {
				t.Errorf("isolation = %+v, want external with no identity filled in", iso)
			}
		},
		"isolation-none-values.yaml": func(t *testing.T, in installation) {
			if iso := in.cfg.Gateway.Isolation; iso.Mode != gateway.IsolationNone || !iso.AcknowledgeNone {
				t.Errorf("isolation = %+v, want none, acknowledged", iso)
			}
		},
		"metrics-values.yaml": func(t *testing.T, in installation) {
			args := in.dep.Spec.Template.Spec.Containers[0].Args
			if !slices.Contains(args, "--metrics-bind-address=:8443") || !slices.Contains(args, "--leader-elect=true") {
				t.Errorf("args = %v, want metrics on :8443 and leader election", args)
			}
			var svc corev1.Service
			testchart.Into(t, in.objs.Get(t, "Service", fullname+"-metrics"), &svc)
			if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 8443 || svc.Spec.Ports[0].TargetPort.StrVal != "metrics" {
				t.Errorf("the metrics Service publishes %+v", svc.Spec.Ports)
			}
			if *in.dep.Spec.Replicas != 2 {
				t.Errorf("replicas = %d, want 2", *in.dep.Spec.Replicas)
			}
			expectTheIdentitySelectsTheOperator(t, in)
		},
		"placement-values.yaml": func(t *testing.T, in installation) {
			gw := in.cfg.Gateway
			if gw.DataVolume.StorageClass != "replicated" || gw.DataVolume.Source.PullMethod != "node" ||
				gw.VM.NodeSelector["node-role.example/gateways"] != "true" || len(gw.VM.Tolerations) != 1 {
				t.Errorf("gateway = %+v, want the placement and the storage of the value set", gw)
			}
			if c := gw.Isolation.Controller; c.Namespace != "platform-operators" || !reflect.DeepEqual(c.Labels, map[string]string{"app": "site-router"}) {
				t.Errorf("the identity it was given was replaced: %+v", c)
			}
			if in.cfg.Timing.Poll.Duration.String() != "1m0s" {
				t.Errorf("timing.poll = %v", in.cfg.Timing.Poll.Duration)
			}
		},
	}
	for _, file := range testchart.ValueSets(t) {
		t.Run(filepath.Base(file), func(t *testing.T) {
			check, ok := checks[filepath.Base(file)]
			if !ok {
				t.Fatalf("no check says what the value set %s is there for", filepath.Base(file))
			}
			check(t, install(t, "--values", file))
		})
	}
}

// expectTheIdentitySelectsTheOperator checks that under cilium the management API
// of a gateway admits the operator pods, and only pods that carry their labels in
// their namespace.
func expectTheIdentitySelectsTheOperator(t *testing.T, in installation) {
	t.Helper()
	ctl := in.cfg.Gateway.Isolation.Controller
	if ctl.Namespace != namespace || ctl.Namespace != in.dep.Namespace {
		t.Errorf("the identity names the namespace %q, the operator runs in %q", ctl.Namespace, in.dep.Namespace)
	}
	if len(ctl.Labels) == 0 {
		t.Fatal("the identity names no label: every pod of the namespace would reach the management API")
	}
	pod := in.dep.Spec.Template.Labels
	for k, v := range ctl.Labels {
		if pod[k] != v {
			t.Errorf("the identity asks for %s=%s, the operator pods have %q", k, v, pod[k])
		}
	}
	// The labels that select the pods of the Deployment are the ones that do not
	// change with the version of the chart.
	if !reflect.DeepEqual(ctl.Labels, in.dep.Spec.Selector.MatchLabels) {
		t.Errorf("the identity is %v, the selector of the operator pods %v", ctl.Labels, in.dep.Spec.Selector.MatchLabels)
	}
}

// What has no default makes an install fail, each of it by name, before anything
// reaches a cluster.
func TestTheChartDoesNotRenderWithoutWhatHasNoDefault(t *testing.T) {
	_, err := testchart.RenderErr(t, release, namespace)
	if err == nil {
		t.Fatal("the chart renders with no networks, no management network and no engine")
	}
	for _, key := range []string{
		"/config/managementCIDR",
		"/config/clusterNetworks/podCIDR",
		"/config/clusterNetworks/serviceCIDR",
		"/config/engine/name",
		"/config/engine/versions",
		"/config/engine/latest",
	} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("the failure does not name %s: %v", key, err)
		}
	}
}

// The configuration is read once, when the operator starts, so a change of it
// rolls the pods, and nothing else that leaves it as it is does.
func TestAChangeOfTheConfigurationRollsThePods(t *testing.T) {
	checksum := func(args ...string) string {
		var dep appsv1.Deployment
		testchart.Into(t, testchart.Render(t, release, namespace, args...).Get(t, "Deployment", fullname), &dep)
		sum := dep.Spec.Template.Annotations["checksum/config"]
		if sum == "" {
			t.Fatal("the pods carry no checksum of the configuration")
		}
		return sum
	}
	minimal := checksum("--values", testchart.Values("minimal-values.yaml"))
	if got := checksum("--values", testchart.Values("minimal-values.yaml"), "--set", "config.upgrade.policy=annotation"); got == minimal {
		t.Error("the pods are not rolled when the configuration changes")
	}
	if got := checksum("--values", testchart.Values("minimal-values.yaml"), "--set", "replicaCount=3"); got != minimal {
		t.Error("the pods are rolled although the configuration did not change")
	}
}

// grant is one verb on one resource of one group.
type grant struct{ group, resource, verb string }

// grants lists what rules grant, one verb on one resource at a time.
func grants(rules []rbacv1.PolicyRule) []grant {
	var out []grant
	for _, r := range rules {
		for _, g := range r.APIGroups {
			for _, res := range r.Resources {
				for _, v := range r.Verbs {
					out = append(out, grant{g, res, v})
				}
			}
		}
	}
	return out
}

// The ClusterRole of the operator grants what the markers of the controller say
// it needs, as controller-gen makes it, and nothing else.
func TestTheClusterRoleIsTheGeneratedOne(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repo(), "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var generated rbacv1.ClusterRole
	if err := yaml.UnmarshalStrict(data, &generated); err != nil {
		t.Fatal(err)
	}
	if len(generated.Rules) == 0 {
		t.Fatal("config/rbac/role.yaml has no rules")
	}
	var rendered rbacv1.ClusterRole
	testchart.Into(t, testchart.Render(t, release, namespace, "--values", testchart.Values("minimal-values.yaml")).Get(t, "ClusterRole", fullname), &rendered)
	if !reflect.DeepEqual(rendered.Rules, generated.Rules) {
		t.Errorf("the ClusterRole of the chart is not the generated one: run make chart-manifests\nchart:     %+v\ngenerated: %+v", rendered.Rules, generated.Rules)
	}
	if !slices.Contains(grants(rendered.Rules), grant{"siterouter.aenix.io", "siterouters/finalizers", "update"}) {
		t.Error("the operator cannot update the finalizers of a SiteRouter, which it needs to make objects that block its deletion")
	}
}

// What the operator has no business doing is not granted by any value set, nor
// is anything granted with a wildcard.
func TestTheChartGrantsNothingTheOperatorDoesNotNeed(t *testing.T) {
	forbidden := func(g grant) bool {
		switch {
		case g.group == "*" || g.resource == "*" || g.verb == "*":
			return true
		case g.group == "" && (g.resource == "namespaces" || g.resource == "pods") && g.verb != "get" && g.verb != "list" && g.verb != "watch":
			return true
		case g.resource == "helmreleases":
			return true
		}
		return false
	}
	for _, file := range testchart.ValueSets(t) {
		objs := testchart.Render(t, release, namespace, "--values", file)
		roles := append(objs.Kind("ClusterRole"), objs.Kind("Role")...)
		rules := make([]rbacv1.PolicyRule, 0, len(roles))
		for _, u := range roles {
			var r rbacv1.ClusterRole
			testchart.Into(t, u, &r)
			rules = append(rules, r.Rules...)
		}
		if len(rules) == 0 {
			t.Fatalf("%s: the chart grants nothing", filepath.Base(file))
		}
		for _, g := range grants(rules) {
			if forbidden(g) {
				t.Errorf("%s: the chart grants %s on %s of the group %q", filepath.Base(file), g.verb, g.resource, g.group)
			}
		}
	}
}

// The chart installs the CRDs controller-gen makes from the types, as they are,
// and keeps them when it is uninstalled.
func TestTheCRDsAreTheGeneratedOnes(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(repo(), "config", "crd", "bases", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no generated CRD: %v", err)
	}
	objs := testchart.Render(t, release, namespace, "--values", testchart.Values("minimal-values.yaml"))
	if got := len(objs.Kind("CustomResourceDefinition")); got != len(files) {
		t.Errorf("the chart installs %d CRDs, controller-gen makes %d: run make chart-manifests", got, len(files))
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var generated apiextensionsv1.CustomResourceDefinition
		if err := yaml.UnmarshalStrict(data, &generated); err != nil {
			t.Fatal(err)
		}
		var rendered apiextensionsv1.CustomResourceDefinition
		testchart.Into(t, objs.Get(t, "CustomResourceDefinition", generated.Name), &rendered)
		if !reflect.DeepEqual(rendered.Spec, generated.Spec) {
			t.Errorf("the chart installs %s with another schema than the generated one: run make chart-manifests", generated.Name)
		}
		if rendered.Annotations["helm.sh/resource-policy"] != "keep" {
			t.Errorf("%s is not kept when the release goes: deleting it would delete every object of it", generated.Name)
		}
	}

	off := testchart.Render(t, release, namespace, "--values", testchart.Values("minimal-values.yaml"), "--set", "crds.install=false")
	if got := off.Kind("CustomResourceDefinition"); len(got) != 0 {
		t.Errorf("the chart installs %d CRDs with crds.install off", len(got))
	}
}
