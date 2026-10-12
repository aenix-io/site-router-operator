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

// Package testenv starts a real apiserver for the tests that are only worth what
// an apiserver makes of them. It is for TestMain and for nothing else.
package testenv

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
)

// Config and Scheme are those of the apiserver Main started, for a test that runs a
// manager against it. They are set before the tests run.
var (
	Config *rest.Config
	Scheme *k8sruntime.Scheme
)

// Main starts an apiserver that serves the CRDs of the operator and those in
// crdDirs, gives set a client for it, runs the tests and exits with their
// result. It needs the binaries of envtest, which `make test` provides through
// KUBEBUILDER_ASSETS.
func Main(m *testing.M, set func(client.Client), crdDirs ...string) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintln(os.Stderr, "KUBEBUILDER_ASSETS is not set: run these tests with `make test` or `make test-crd`")
		os.Exit(1)
	}
	scheme := k8sruntime.NewScheme()
	for _, add := range []func(*k8sruntime.Scheme) error{clientgoscheme.AddToScheme, v1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			fmt.Fprintln(os.Stderr, "building the scheme:", err)
			os.Exit(1)
		}
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     append([]string{operatorCRDs()}, crdDirs...),
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "starting envtest:", err)
		os.Exit(1)
	}
	Config, Scheme = cfg, scheme
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintln(os.Stderr, "creating a client:", err)
		_ = env.Stop()
		os.Exit(1)
	}
	set(c)

	code := m.Run()
	if err := env.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "stopping envtest:", err)
	}
	os.Exit(code)
}

// operatorCRDs is the directory of the generated CRDs, found from this file so
// that it does not depend on where the tests of a package are.
func operatorCRDs() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "config", "crd", "bases")
}
