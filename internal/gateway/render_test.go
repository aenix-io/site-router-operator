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
	"errors"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aenix-io/site-router-operator/api/engine"
	"github.com/aenix-io/site-router-operator/api/v1alpha1"
)

func fullConfig(in *Input) {
	in.Config.VM = VMConfig{
		PodAnnotations:    map[string]string{"backup.example/policy": "daily"},
		PodLabels:         map[string]string{"billing.example/class": "network"},
		NodeSelector:      map[string]string{"node-role.example/gateway": "true"},
		Tolerations:       []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "gateways", Effect: corev1.TaintEffectNoSchedule}},
		PriorityClassName: "network-critical",
		RuntimeClassName:  "kata",
		Affinity: &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
				Weight: 100,
				PodAffinityTerm: corev1.PodAffinityTerm{
					TopologyKey:   "kubernetes.io/hostname",
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue}},
				},
			}},
		}},
	}
	in.Config.DataVolume = DataVolumeConfig{
		StorageClass: "replicated",
		BlockSize:    BlockSize{Logical: 4096, Physical: 4096},
		Source:       SourceConfig{PullMethod: "node", CertConfigMap: "registry-ca", SecretRef: "registry-credentials"},
	}
	in.Config.Service = ServiceConfig{
		LoadBalancerClass: "example.com/lb",
		Annotations:       map[string]string{"lb.example/pool": "tunnels"},
	}
	in.Config.Isolation.EgressDenyCIDRs = []string{"10.96.0.0/12", "10.244.0.0/16", "169.254.0.0/16", "10.96.0.0/12"}
}

func TestRenderGolden(t *testing.T) {
	cases := map[string]func(*Input){
		"minimal":     nil,
		"full-config": fullConfig,
		"own-storage-class-and-psk-secret": func(in *Input) {
			in.Instance.Spec.StorageClass = "fast"
			in.Config.DataVolume.StorageClass = "replicated"
			in.Instance.Spec.Peer.Auth = &v1alpha1.PeerAuth{SecretRef: &v1alpha1.LocalSecretRef{Name: "tenant-psk"}}
			in.PSK = nil
			in.Instance.Spec.Resources = v1alpha1.Resources{CPU: 4, Memory: resource.MustParse("4Gi")}
		},
		"external-isolation": func(in *Input) {
			in.Config.Isolation = IsolationConfig{Mode: IsolationExternal}
		},
		"no-isolation-acknowledged": func(in *Input) {
			in.Config.Isolation = IsolationConfig{Mode: IsolationNone, AcknowledgeNone: true}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var m []func(*Input)
			if mutate != nil {
				m = append(m, mutate)
			}
			golden(t, name, marshal(t, mustRender(t, newInput(m...))))
		})
	}
}

func TestRenderIsAFunctionOfItsInput(t *testing.T) {
	first := marshal(t, mustRender(t, newInput(fullConfig)))
	second := marshal(t, mustRender(t, newInput(fullConfig)))
	if !bytes.Equal(first, second) {
		t.Error("two renders of the same input differ")
	}
}

func TestRenderLeavesItsInputAlone(t *testing.T) {
	in := newInput(fullConfig)
	before := newInput(fullConfig)
	objs := mustRender(t, in)

	// What is rendered must not alias what was passed in: a controller that
	// keeps the plan across passes would otherwise see it change under it.
	secretOf(t, objs, NamesFor(in.Instance.Name).APIKey).Data["token"][0] ^= 0xff
	secretOf(t, objs, NamesFor(in.Instance.Name).ForImage(in.Image).CloudInit).Data[engine.SeedUserData][0] ^= 0xff

	if !reflect.DeepEqual(in.Plan, before.Plan) {
		t.Error("Render changed the plan it was given")
	}
	if !reflect.DeepEqual(in.Config, before.Config) {
		t.Error("Render changed the configuration it was given")
	}
	if !reflect.DeepEqual(in.Instance, before.Instance) {
		t.Error("Render changed the SiteRouter it was given")
	}
}

func TestRenderOrderIsTheOrderOfCreation(t *testing.T) {
	objs := mustRender(t, newInput())
	got := make([]string, 0, len(objs))
	for _, o := range objs {
		m, err := toMap(o)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, str(m, "kind"))
	}
	want := []string{"Secret", "Secret", "Secret", "DataVolume", "VirtualMachine", "Service", "CiliumNetworkPolicy", "CiliumNetworkPolicy"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("kinds in order = %v, want %v: what the VM uses must exist before it", got, want)
	}
}

// The disk and the seed are named after the image, so that a gateway that moves
// to another one gets new ones beside the old, which are never changed.
func TestTheDiskAndTheSeedAreNamedAfterTheImage(t *testing.T) {
	names := NamesFor("branch-office").ForImage(testImage)
	if names.BootDisk != "site-router-branch-office-boot-0123" || names.CloudInit != "site-router-branch-office-seed-0123" {
		t.Errorf("names = %q and %q, want the first four characters of the digest in both", names.BootDisk, names.CloudInit)
	}
	objs := mustRender(t, newInput())
	for _, name := range []string{names.BootDisk, names.CloudInit} {
		var o client.Object
		for _, x := range objs {
			if x.GetName() == name {
				o = x
			}
		}
		if o == nil {
			t.Fatalf("no object named %s", name)
		}
		if got := o.GetAnnotations()[v1alpha1.AnnotationImageDigest]; got != testDigest {
			t.Errorf("%s is marked %q, want the digest of the image, %q", name, got, testDigest)
		}
	}

	other := newInput(func(in *Input) {
		in.Image = "registry.example/appliances/gateway-disk:2.0@sha256:fedc" + strings.Repeat("0", 60)
	})
	moved := NamesFor("branch-office").ForImage(other.Image)
	if moved.BootDisk == names.BootDisk || moved.CloudInit == names.CloudInit {
		t.Error("another image has the same disk and seed")
	}
	rendered := mustRender(t, other)
	if find(t, rendered, "DataVolume", moved.BootDisk) == nil || find(t, rendered, "Secret", moved.CloudInit) == nil {
		t.Error("the objects of another image are not named after it")
	}
}

func TestNamesStayWithinWhatTheyMayBe(t *testing.T) {
	longest := strings.Repeat("a", 40)
	n := NamesFor(longest).ForImage(testImage)
	for _, name := range []string{n.VM, n.BootDisk, n.CloudInit, n.Tunnel} {
		if len(name) > 63 {
			t.Errorf("%q is %d characters, more than the 63 a name may have where it becomes a label value or a host name", name, len(name))
		}
	}
	if len(n.BootDisk) != 62 {
		t.Errorf("the boot disk is %d characters with the longest name, want 62", len(n.BootDisk))
	}
}

func TestImageDigest(t *testing.T) {
	if got, err := ImageDigest(testImage); err != nil || got != testDigest {
		t.Errorf("ImageDigest = %q, %v, want %q", got, err, testDigest)
	}
	if _, err := ImageDigest("registry.example/disk:1.0"); err == nil {
		t.Error("an image with no digest has one")
	}
	if got := shortDigest("x"); got != "" {
		t.Errorf("shortDigest of an image with no digest = %q", got)
	}
	if got := shortDigest("x@sha256:ab"); got != "" {
		t.Errorf("shortDigest of a digest that is too short = %q", got)
	}
}

func TestPSKSecretIsTheOperatorsOnlyWhenNoneIsNamed(t *testing.T) {
	names := NamesFor("branch-office").ForImage(testImage)
	own := mustRender(t, newInput())
	if string(secretOf(t, own, names.PSK).Data["psk"]) != "psk-0123456789abcdefghijklmnopqrstuv" {
		t.Error("the Secret with the pre-shared key does not hold the key that was passed in")
	}
	if !OwnsPSKSecret(newInput().Instance) {
		t.Error("OwnsPSKSecret is false with no secretRef")
	}
	emptyAuth := newInput(func(in *Input) { in.Instance.Spec.Peer.Auth = &v1alpha1.PeerAuth{} })
	if !OwnsPSKSecret(emptyAuth.Instance) {
		t.Error("OwnsPSKSecret is false for an auth block that names no Secret")
	}
	secretOf(t, mustRender(t, emptyAuth), names.PSK)

	named := newInput(func(in *Input) {
		in.Instance.Spec.Peer.Auth = &v1alpha1.PeerAuth{SecretRef: &v1alpha1.LocalSecretRef{Name: "tenant-psk"}}
		in.PSK = nil
	})
	for _, o := range mustRender(t, named) {
		if m, _ := toMap(o); str(m, "metadata", "name") == names.PSK {
			t.Error("the operator renders a Secret for the pre-shared key although the SiteRouter names one")
		}
	}
	if OwnsPSKSecret(named.Instance) {
		t.Error("OwnsPSKSecret is true with a secretRef")
	}
}

func TestManagementPortComesFromThePlan(t *testing.T) {
	in := newInput(func(in *Input) { in.Plan.ManagementPort = 8443 })
	p := unstructuredOf(t, mustRender(t, in), "CiliumNetworkPolicy", NamesFor("branch-office").Ingress)
	rules := list(p.Object, "spec", "ingress")
	if !samePorts(asMap(rules[1]), "TCP/8443") {
		t.Errorf("the management rule admits %v, want TCP/8443 as the plan says", rules[1])
	}
}

func TestStorageClassOfTheSiteRouterWinsOverThePlatforms(t *testing.T) {
	names := NamesFor("branch-office").ForImage(testImage)
	for name, c := range map[string]struct{ spec, platform, want string }{
		"both":          {"fast", "replicated", "fast"},
		"platform only": {"", "replicated", "replicated"},
		"spec only":     {"fast", "", "fast"},
		"neither":       {"", "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			in := newInput(func(in *Input) {
				in.Instance.Spec.StorageClass = c.spec
				in.Config.DataVolume.StorageClass = c.platform
			})
			dv := unstructuredOf(t, mustRender(t, in), "DataVolume", names.BootDisk)
			got, set := dv.Object["spec"].(map[string]any)["storage"].(map[string]any)["storageClassName"]
			if c.want == "" {
				// An empty storageClassName asks for a pre-provisioned volume, so
				// the field must be absent, not empty.
				if set {
					t.Errorf("storageClassName is %q, want the field absent", got)
				}
			} else if got != c.want {
				t.Errorf("storageClassName = %v, want %q", got, c.want)
			}
		})
	}
}

func TestRegistryURL(t *testing.T) {
	const d = testDigest
	for _, c := range []struct {
		image, want, wantErr string
	}{
		{"registry.example/disk:0.1.1@" + d, "docker://registry.example/disk@" + d, ""},
		{"registry.example/disk@" + d, "docker://registry.example/disk@" + d, ""},
		{"registry.example:5000/org/disk:1.0@" + d, "docker://registry.example:5000/org/disk@" + d, ""},
		{"registry.example:5000/org/disk@" + d, "docker://registry.example:5000/org/disk@" + d, ""},
		{"registry.example/disk:0.1.1", "", "not pinned by a digest"},
		{"registry.example/disk", "", "not pinned by a digest"},
		{"", "", "not pinned by a digest"},
		{"registry.example/disk@sha256:abc", "", "not pinned by a digest"},
		{"registry.example/disk@sha512:" + strings.Repeat("a", 64), "", "not pinned by a digest"},
		{"registry.example/disk@" + strings.ToUpper(d), "", "not pinned by a digest"},
		{"@" + d, "", "no repository"},
		{"docker://registry.example/disk@" + d, "", "no repository"},
		{"registry.example/my disk@" + d, "", "no repository"},
	} {
		t.Run(c.image, func(t *testing.T) {
			got, err := registryURL(c.image)
			switch {
			case c.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("error = %v, want one that mentions %q", err, c.wantErr)
				}
			case err != nil:
				t.Fatal(err)
			case got != c.want:
				t.Errorf("registryURL = %q, want %q", got, c.want)
			}
		})
	}
}

func TestRenderRefusesInputItCannotBuildAGatewayFrom(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Input)
		want   string
	}{
		{"no SiteRouter", func(in *Input) { in.Instance = nil }, "no SiteRouter"},
		{"no UID", func(in *Input) { in.Instance.UID = "" }, "no UID"},
		{"no name", func(in *Input) { in.Instance.Name = "" }, "no name or namespace"},
		{"no namespace", func(in *Input) { in.Instance.Namespace = "" }, "no name or namespace"},
		{"no CPU", func(in *Input) { in.Instance.Spec.Resources.CPU = 0 }, "resources of the SiteRouter are not set"},
		{"no memory", func(in *Input) { in.Instance.Spec.Resources.Memory = resource.Quantity{} }, "resources of the SiteRouter are not set"},
		{"no architecture", func(in *Input) { in.Plan.Arch = "" }, "no architecture"},
		{"no disk size", func(in *Input) { in.Plan.DiskGiB = 0 }, "no disk size"},
		{"no management port", func(in *Input) { in.Plan.ManagementPort = 0 }, "no management port"},
		{"a management port beyond 65535", func(in *Input) { in.Plan.ManagementPort = 65536 }, "no management port"},
		{"no seed", func(in *Input) { in.Plan.SeedFiles = nil }, `no seed file "userdata"`},
		{"a seed under another key", func(in *Input) {
			in.Plan.SeedFiles = map[string][]byte{"user-data": []byte("#cloud-config")}
		}, `no seed file "userdata"`},
		{"an empty seed", func(in *Input) {
			in.Plan.SeedFiles = map[string][]byte{engine.SeedUserData: nil}
		}, `no seed file "userdata"`},
		{"no credentials", func(in *Input) { in.Plan.Credentials = nil }, "no credentials"},
		{"no PSK and none named", func(in *Input) { in.PSK = nil }, "no Secret for the pre-shared key"},
		{"an image with a tag only", func(in *Input) { in.Image = "registry.example/disk:0.1.1" }, "not pinned by a digest"},
		{"no image", func(in *Input) { in.Image = "" }, "not pinned by a digest"},
		{"a configuration that does not validate", func(in *Input) { in.Config.Isolation.Mode = "" }, "isolation.mode is required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			objs, err := Render(newInput(c.mutate))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want one that mentions %q", err, c.want)
			}
			if objs != nil {
				t.Error("Render returned objects together with an error")
			}
		})
	}
}

func TestRenderReportsEveryProblemWithTheInput(t *testing.T) {
	_, err := Render(newInput(func(in *Input) {
		in.Instance.UID = ""
		in.Plan.Arch = ""
		in.Image = "registry.example/disk"
	}))
	if err == nil {
		t.Fatal("Render accepted input with three problems")
	}
	for _, want := range []string{"no UID", "no architecture", "not pinned by a digest"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

func TestPSKIsNotRequiredWhenTheSiteRouterNamesASecret(t *testing.T) {
	in := newInput(func(in *Input) {
		in.Instance.Spec.Peer.Auth = &v1alpha1.PeerAuth{SecretRef: &v1alpha1.LocalSecretRef{Name: "tenant-psk"}}
		in.PSK = nil
	})
	if _, err := Render(in); err != nil {
		t.Fatalf("Render: %v", err)
	}
}

func TestGatewayWithoutIsolationIsNotCreatedUnlessAcknowledged(t *testing.T) {
	in := newInput(func(in *Input) { in.Config.Isolation = IsolationConfig{Mode: IsolationNone} })
	objs, err := Render(in)
	if !errors.Is(err, ErrIsolationUnavailable) {
		t.Fatalf("error = %v, want ErrIsolationUnavailable", err)
	}
	if objs != nil {
		t.Error("objects were returned for a gateway that must not be created")
	}

	in.Config.Isolation.AcknowledgeNone = true
	if _, err := Render(in); err != nil {
		t.Errorf("an acknowledged gateway without isolation was refused: %v", err)
	}
}

func TestIsolationModeDecidesWhetherThereArePolicies(t *testing.T) {
	for mode, want := range map[IsolationMode]int{IsolationCilium: 2, IsolationExternal: 0, IsolationNone: 0} {
		t.Run(string(mode), func(t *testing.T) {
			in := newInput(func(in *Input) {
				in.Config.Isolation.Mode = mode
				in.Config.Isolation.AcknowledgeNone = mode == IsolationNone
			})
			got := 0
			for _, o := range mustRender(t, in) {
				if m, _ := toMap(o); str(m, "kind") == "CiliumNetworkPolicy" {
					got++
				}
			}
			if got != want {
				t.Errorf("%d policies, want %d", got, want)
			}
		})
	}
}

func TestEveryObjectNamesTheSiteRouterAsItsOwner(t *testing.T) {
	for _, o := range mustRender(t, newInput(fullConfig)) {
		refs := o.GetOwnerReferences()
		if len(refs) != 1 || string(refs[0].UID) != testUID || refs[0].Controller == nil || !*refs[0].Controller {
			t.Errorf("%s/%s: owners = %+v", o.GetObjectKind().GroupVersionKind().Kind, o.GetName(), refs)
		}
	}
}
