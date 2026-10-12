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
	"fmt"
	"slices"
	"sync"
)

// Factory builds an engine.
type Factory func() (Engine, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register makes an engine available under name. Like database/sql.Register it
// is meant to be called from an init function of the package that implements
// the engine, so that importing the package is enough to enable it and the
// operator itself never names an implementation. It panics on a nil factory
// and on a name registered twice.
func Register(name string, f Factory) {
	if f == nil {
		panic("engine: Register factory is nil for " + name)
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := factories[name]; dup {
		panic("engine: Register called twice for " + name)
	}
	factories[name] = f
}

// New builds the engine registered under name.
func New(name string) (Engine, error) {
	mu.RLock()
	f, ok := factories[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("engine %q is not registered (registered: %v); is the package that implements it imported?", name, Names())
	}
	return f()
}

// Names returns the registered engine names, sorted.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(factories))
	for name := range factories {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
