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

// Package testchart renders the chart of the operator with Helm, for the tests
// that check what an installation is given. It is for tests and for nothing else.
package testchart

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// Dir is the directory of the chart, found from this file so that it does not
// depend on where the tests of a package are.
func Dir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "charts", "site-router-operator")
}

// Values is a value set of the ci directory of the chart, which sets what has
// no default.
func Values(name string) string {
	return filepath.Join(Dir(), "ci", name)
}

// ValueSets lists the value sets of the ci directory of the chart.
func ValueSets(t testing.TB) []string {
	t.Helper()
	files, err := filepath.Glob(Values("*-values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no value set in %s", Values(""))
	}
	return files
}

// helm returns the path of helm. The test is skipped where it is not installed,
// and fails in CI, where a check that does not run would pass for one that does.
func helm(t testing.TB) string {
	t.Helper()
	path, err := exec.LookPath("helm")
	if err == nil {
		return path
	}
	if os.Getenv("CI") != "" {
		t.Fatalf("helm is not on PATH, and in CI the chart must be rendered: %v", err)
	}
	t.Skipf("helm is not on PATH: %v", err)
	return ""
}

// Objects is what the chart renders.
type Objects []*unstructured.Unstructured

// Render renders the chart as a release in a namespace, with more arguments of
// helm template, and fails the test when it does not render.
func Render(t testing.TB, release, namespace string, args ...string) Objects {
	t.Helper()
	objs, err := RenderErr(t, release, namespace, args...)
	if err != nil {
		t.Fatal(err)
	}
	return objs
}

// RenderErr is Render that returns why the chart did not render, with what Helm
// said.
func RenderErr(t testing.TB, release, namespace string, args ...string) (Objects, error) {
	t.Helper()
	argv := append([]string{"template", release, Dir(), "--namespace", namespace}, args...)
	cmd := exec.CommandContext(t.Context(), helm(t), argv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("helm %v: %w: %s", argv, err, stderr.String())
	}
	return decode(stdout.Bytes())
}

func decode(data []byte) (Objects, error) {
	var objs Objects
	r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for {
		doc, err := r.Read()
		if errors.Is(err, io.EOF) {
			return objs, nil
		}
		if err != nil {
			return nil, err
		}
		j, err := yaml.YAMLToJSON(doc)
		if err != nil {
			return nil, err
		}
		if string(bytes.TrimSpace(j)) == "null" {
			continue
		}
		u := &unstructured.Unstructured{}
		if err := u.UnmarshalJSON(j); err != nil {
			return nil, fmt.Errorf("%w: %s", err, doc)
		}
		objs = append(objs, u)
	}
}

// Kind lists the objects of a kind.
func (o Objects) Kind(kind string) []*unstructured.Unstructured {
	var out []*unstructured.Unstructured
	for _, u := range o {
		if u.GetKind() == kind {
			out = append(out, u)
		}
	}
	return out
}

// Get returns the object of a kind and a name, and fails the test when there is
// none.
func (o Objects) Get(t testing.TB, kind, name string) *unstructured.Unstructured {
	t.Helper()
	for _, u := range o.Kind(kind) {
		if u.GetName() == name {
			return u
		}
	}
	t.Fatalf("the chart renders no %s %s", kind, name)
	return nil
}

// One returns the only object of a kind, and fails the test when there is not
// exactly one.
func (o Objects) One(t testing.TB, kind string) *unstructured.Unstructured {
	t.Helper()
	all := o.Kind(kind)
	if len(all) != 1 {
		t.Fatalf("the chart renders %d objects of kind %s, want 1", len(all), kind)
	}
	return all[0]
}

// Into converts a rendered object to its type.
func Into(t testing.TB, u *unstructured.Unstructured, out any) {
	t.Helper()
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(u.Object, out, true); err != nil {
		t.Fatalf("%s %s: %v", u.GetKind(), u.GetName(), err)
	}
}
