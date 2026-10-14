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
	"flag"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/aenix-io/site-router-operator/api/engine"
	"github.com/aenix-io/site-router-operator/api/v1alpha1"
)

var update = flag.Bool("update", false, "rewrite the golden files")

const (
	testUID    = "11111111-2222-4333-8444-555555555555"
	testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testImage  = "registry.example/appliances/gateway-disk:0.1.1@" + testDigest
)

// newInput is a gateway as the operator would make one, with everything set
// that Render needs and nothing optional.
func newInput(mutate ...func(*Input)) Input {
	cfg := DefaultConfig()
	cfg.Isolation = IsolationConfig{
		Mode: IsolationCilium,
		Controller: ControllerIdentity{
			Namespace: "site-router-system",
			Labels:    map[string]string{"app.kubernetes.io/name": "site-router-operator"},
		},
	}
	in := Input{
		Instance: &v1alpha1.SiteRouter{
			Name:      "branch-office",
			Namespace: "tenant-a",
			UID:       testUID,
			Spec: v1alpha1.SiteRouterSpec{
				Peer:        v1alpha1.Peer{Address: "203.0.113.10"},
				RemoteCIDRs: []v1alpha1.IPv4CIDR{"192.0.2.0/24"},
				Resources:   v1alpha1.Resources{CPU: 2, Memory: resource.MustParse("2Gi")},
			},
		},
		Config: cfg,
		Image:  testImage,
		Plan: engine.BootstrapPlan{
			Arch:    "amd64",
			DiskGiB: 12,
			SeedFiles: map[string][]byte{
				engine.SeedUserData: []byte("# seed for branch-office\n"),
			},
			Credentials: map[string][]byte{
				"token":          []byte("api-token-0123456789abcdef"),
				"tls.key":        []byte("tls-key-fedcba9876543210"),
				"tls.servername": []byte("branch-office.tenant-a.gateway.internal"),
			},
			ManagementPort: 443,
		},
		PSK: []byte("psk-0123456789abcdefghijklmnopqrstuv"),
	}
	for _, m := range mutate {
		m(&in)
	}
	return in
}

func mustRender(t *testing.T, in Input) []client.Object {
	t.Helper()
	objs, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return objs
}

// find returns the object of the given kind, which must be one.
func find(t *testing.T, objs []client.Object, kind, name string) client.Object {
	t.Helper()
	for _, o := range objs {
		m, err := toMap(o)
		if err != nil {
			t.Fatal(err)
		}
		if str(m, "kind") == kind && str(m, "metadata", "name") == name {
			return o
		}
	}
	t.Fatalf("no %s/%s among the objects", kind, name)
	return nil
}

func unstructuredOf(t *testing.T, objs []client.Object, kind, name string) *unstructured.Unstructured {
	t.Helper()
	u, ok := find(t, objs, kind, name).(*unstructured.Unstructured)
	if !ok {
		t.Fatalf("%s/%s is not unstructured", kind, name)
	}
	return u
}

func secretOf(t *testing.T, objs []client.Object, name string) *corev1.Secret {
	t.Helper()
	s, ok := find(t, objs, "Secret", name).(*corev1.Secret)
	if !ok {
		t.Fatalf("Secret/%s is not a Secret", name)
	}
	return s
}

// marshal writes the objects as YAML documents the way a reader wants to see
// them: Secret data in clear, no empty status and no null timestamp.
func marshal(t *testing.T, objs []client.Object) []byte {
	t.Helper()
	var out bytes.Buffer
	for i, o := range objs {
		m, err := toMap(o)
		if err != nil {
			t.Fatal(err)
		}
		m = deepCopy(m)
		unstructured.RemoveNestedField(m, "metadata", "creationTimestamp")
		// Render never sets a status; the empty one of a typed object is noise.
		delete(m, "status")
		if s, ok := o.(*corev1.Secret); ok {
			delete(m, "data")
			plain := map[string]any{}
			for k, v := range s.Data {
				plain[k] = string(v)
			}
			m["stringData"] = plain
		}
		b, err := yaml.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			out.WriteString("---\n")
		}
		out.Write(b)
	}
	return out.Bytes()
}

func deepCopy(m map[string]any) map[string]any {
	return (&unstructured.Unstructured{Object: m}).DeepCopy().Object
}

// golden compares got with the file under testdata, or rewrites the file with
// -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name+".yaml")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run the tests with -update to write it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the objects differ from %s (run the tests with -update to accept the change):\n--- got\n%s\n--- want\n%s", path, got, want)
	}
}
