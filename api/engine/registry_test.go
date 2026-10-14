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

package engine

import (
	"slices"
	"strings"
	"testing"
)

type stub struct{ name string }

func (s stub) Info() Info                                      { return Info{Name: s.name} }
func (stub) Bootstrap(BootstrapRequest) (BootstrapPlan, error) { return BootstrapPlan{}, nil }
func (stub) Connect(Endpoint) (Session, error)                 { return nil, nil }

func register(t *testing.T, name string) {
	t.Helper()
	Register(name, func() (Engine, error) { return stub{name}, nil })
	t.Cleanup(func() {
		mu.Lock()
		delete(factories, name)
		mu.Unlock()
	})
}

func TestRegisterAndNew(t *testing.T) {
	register(t, "registry-test-b")
	register(t, "registry-test-a")

	e, err := New("registry-test-a")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.Info().Name; got != "registry-test-a" {
		t.Errorf("New returned engine %q", got)
	}

	names := Names()
	if !slices.IsSorted(names) {
		t.Errorf("Names() not sorted: %v", names)
	}
	for _, want := range []string{"registry-test-a", "registry-test-b"} {
		if !slices.Contains(names, want) {
			t.Errorf("Names() = %v, missing %q", names, want)
		}
	}
}

func TestNewUnknownNamesTheRegisteredOnes(t *testing.T) {
	register(t, "registry-test-known")
	_, err := New("registry-test-missing")
	if err == nil {
		t.Fatal("New succeeded for an unregistered name")
	}
	if !strings.Contains(err.Error(), "registry-test-known") {
		t.Errorf("error does not list registered engines: %v", err)
	}
}

func TestRegisterPanics(t *testing.T) {
	register(t, "registry-test-dup")
	for name, fn := range map[string]func(){
		"duplicate": func() { Register("registry-test-dup", func() (Engine, error) { return stub{}, nil }) },
		"nil":       func() { Register("registry-test-nil", nil) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: Register did not panic", name)
				}
			}()
			fn()
		}()
	}
}
