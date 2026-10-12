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

// Package importgraph enforces the dependency rules of the operator: it is
// not tied to a platform, to an appliance or to a network.
package importgraph

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// forbidden lists substrings of import paths that must not appear anywhere in
// the dependency graph of the operator's own packages (internal/...). cmd/ is
// not checked: it is where an engine implementation is registered.
var forbidden = []string{
	"github.com/cozystack/",
	"github.com/fluxcd/",
	"cozystack.io",
	"lineage",
	"vyos",
	"barerouter",
	// Fakes and the conformance suite are for tests. Production code must not
	// reach an engine implementation, not even a fake.
	"enginetest",
	// A connector serves the Forwarder API from the other side, and the operator
	// knows it only through that API.
	"site-router-operator/connectors/",
}

// violations returns the import paths in deps that contain a forbidden substring.
func violations(deps []string) []string {
	var found []string
	for _, dep := range deps {
		lower := strings.ToLower(dep)
		if slices.ContainsFunc(forbidden, func(f string) bool { return strings.Contains(lower, f) }) {
			found = append(found, dep)
		}
	}
	return found
}

func TestViolations(t *testing.T) {
	deps := []string{
		"sigs.k8s.io/controller-runtime",
		"github.com/aenix-io/site-router-operator/api/v1alpha1",
		"github.com/fluxcd/helm-controller/api/v2",
		"github.com/aenix-io/barerouter/engine",
		"example.com/Some/VyOS-client",
		"github.com/cozystack/cozystack/internal/lineage",
		"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/grant",
	}
	want := []string{
		"github.com/fluxcd/helm-controller/api/v2",
		"github.com/aenix-io/barerouter/engine",
		"example.com/Some/VyOS-client",
		"github.com/cozystack/cozystack/internal/lineage",
		"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/grant",
	}
	if got := violations(deps); !slices.Equal(got, want) {
		t.Fatalf("violations() = %v, want %v", got, want)
	}
	if got := violations(deps[:2]); len(got) != 0 {
		t.Fatalf("violations() on clean deps = %v, want none", got)
	}
}

// TestOperatorPackages checks the real dependency graph of ./internal/...
func TestOperatorPackages(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatalf("go list -m: %v", err)
	}
	root := strings.TrimSpace(string(out))
	if _, err := os.Stat(filepath.Join(root, "internal")); errors.Is(err, fs.ErrNotExist) {
		t.Skip("no ./internal directory yet")
	}
	// No -e: with it an unmatched pattern comes back as a pseudo-package and
	// reads as a clean graph.
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "./internal/...")
	cmd.Dir = root
	out, err = cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps ./internal/...: %v", err)
	}
	deps := strings.Fields(string(out))
	if len(deps) == 0 {
		t.Fatal("go list -deps ./internal/... returned no packages")
	}
	if got := violations(deps); len(got) != 0 {
		t.Fatalf("operator packages depend on forbidden packages:\n%s", strings.Join(got, "\n"))
	}
}
