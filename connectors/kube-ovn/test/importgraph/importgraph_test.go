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

// Package importgraph keeps the connector apart from the operator: of the
// repository it may use its own packages and the api module, and nothing else,
// so that it can be moved to a repository of its own. Go lets it import the
// internal packages of the operator, since its path is under the operator's.
package importgraph

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const (
	repository = "github.com/aenix-io/site-router-operator"
	self       = repository + "/connectors/kube-ovn"
	api        = repository + "/api"
	// conformance is the suite a connector is held to. The connector runs it in its
	// tests, and nothing of what it ships imports it.
	conformance = repository + "/connectors/conformance"
)

// forbidden are substrings of import paths that nothing of the connector may
// depend on: it is not tied to a platform either.
var forbidden = []string{"github.com/cozystack/", "github.com/fluxcd/", "cozystack.io", "lineage"}

// violations returns the packages of deps that the connector may not use.
func violations(deps []string) []string {
	var found []string
	for _, dep := range deps {
		lower := strings.ToLower(dep)
		ours := dep == repository || strings.HasPrefix(dep, repository+"/")
		allowed := dep == self || strings.HasPrefix(dep, self+"/") || dep == api || strings.HasPrefix(dep, api+"/") ||
			dep == conformance || strings.HasPrefix(dep, conformance+"/")
		if (ours && !allowed) || slices.ContainsFunc(forbidden, func(f string) bool { return strings.Contains(lower, f) }) {
			found = append(found, dep)
		}
	}
	return found
}

func TestViolations(t *testing.T) {
	deps := []string{
		"sigs.k8s.io/controller-runtime",
		api + "/v1alpha1",
		self + "/internal/routes",
		conformance,
		repository + "/internal/gateway",
		repository + "/connectors/other/internal/x",
		repository,
		"github.com/cozystack/cozystack/pkg/lineage",
	}
	want := []string{
		repository + "/internal/gateway",
		repository + "/connectors/other/internal/x",
		repository,
		"github.com/cozystack/cozystack/pkg/lineage",
	}
	if got := violations(deps); !slices.Equal(got, want) {
		t.Fatalf("violations() = %v, want %v", got, want)
	}
}

// TestConnectorPackages checks the real dependency graph of the module.
func TestConnectorPackages(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-test", "-f", "{{.ImportPath}}", self+"/...")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	deps := strings.Fields(string(out))
	if !slices.Contains(deps, self+"/internal/controller") || !slices.Contains(deps, api+"/v1alpha1") {
		t.Fatalf("go list -deps did not list the packages of the connector: %v", deps)
	}
	if got := violations(deps); len(got) != 0 {
		t.Fatalf("the connector depends on packages it may not use:\n%s", strings.Join(got, "\n"))
	}
}
