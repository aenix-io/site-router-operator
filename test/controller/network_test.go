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

package controller_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/internal/config"
	"github.com/aenix-io/site-router-operator/internal/controller"
	"github.com/aenix-io/site-router-operator/internal/forwarder"
	"github.com/aenix-io/site-router-operator/internal/reconcile"
)

// otherNetwork is a network that is not asked through the Forwarder API, and
// records what it is asked.
type otherNetwork struct {
	mu    sync.Mutex
	calls []string
}

func (n *otherNetwork) note(call string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, call)
}

func (n *otherNetwork) saw(call string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Contains(n.calls, call)
}

func (n *otherNetwork) Ensure(context.Context) error { n.note("Ensure"); return nil }

func (n *otherNetwork) Report(context.Context) (forwarder.Report, error) {
	n.note("Report")
	return forwarder.Report{}, nil
}

func (n *otherNetwork) Grant(_ context.Context, cidrs []string) (reconcile.RouteState, error) {
	n.note("Grant")
	return reconcile.RouteState{Programmed: true, Reason: reconcile.ReasonProgrammed}, nil
}

func (n *otherNetwork) Withdraw(context.Context) error { n.note("Withdraw"); return nil }

func (n *otherNetwork) Withdrawn(context.Context) (bool, error) {
	n.note("Withdrawn")
	return true, nil
}

// The controller asks the network it is given, and makes no Forwarder of its own:
// a platform that has another way to route to a gateway is served by a Network and
// nothing else in the controller.
func TestTheControllerAsksTheNetworkItIsGiven(t *testing.T) {
	other := &otherNetwork{}
	g := newRig(t, func(cfg *config.Config, sr *v1alpha1.SiteRouter) {})
	g.network = func(*v1alpha1.SiteRouter) controller.Network { return other }
	g.restart(func(*config.Config) {})

	g.reconcile()
	if !other.saw("Ensure") || !other.saw("Report") {
		t.Errorf("the network was asked %v, want Ensure and Report", other.calls)
	}
	forwarders := &v1alpha1.ForwarderList{}
	if err := k8sClient.List(t.Context(), forwarders, client.InNamespace(g.ns)); err != nil {
		t.Fatal(err)
	}
	if len(forwarders.Items) != 0 {
		t.Errorf("the controller made %d Forwarders for a network that is not asked through them", len(forwarders.Items))
	}
}
