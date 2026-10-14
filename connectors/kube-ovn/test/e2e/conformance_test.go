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

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aenix-io/site-router-operator/connectors/conformance"
	"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/grant"
	"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/routes"
)

// kubeOVN is kube-ovn, as the suite asks about it: what its annotations say. There
// is no kube-ovn here, and no kubeovn-webhook, so a pod is made the way the webhook
// would make it, and what kube-ovn makes of the annotations is what the notes on
// kube-ovn say.
type kubeOVN struct{}

var _ conformance.Network = kubeOVN{}

// Admit is what the kubeovn-webhook of Cozystack does to a pod when it is made:
// the routes of the Namespace go onto a pod that has none of its own, and the
// check of addresses is turned on for a pod that does not say.
func (kubeOVN) Admit(ctx context.Context, c client.Client, p *corev1.Pod) error {
	ns := &corev1.Namespace{}
	if err := c.Get(ctx, types.NamespacedName{Name: p.Namespace}, ns); err != nil {
		return err
	}
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	if v, ok := ns.Annotations[routes.Annotation]; ok {
		if _, own := p.Annotations[routes.Annotation]; !own {
			p.Annotations[routes.Annotation] = v
		}
	}
	if _, own := p.Annotations[grant.PortSecurityAnnotation]; !own {
		p.Annotations[grant.PortSecurityAnnotation] = "true"
	}
	return nil
}

// Forwards is a pod whose check of addresses is off.
func (kubeOVN) Forwards(p *corev1.Pod) bool {
	return p.Annotations[grant.PortSecurityAnnotation] == "false"
}

type routeEntry struct {
	Dst string `json:"dst"`
	GW  string `json:"gw"`
}

func parseRoutes(v string) ([]routeEntry, error) {
	if v == "" {
		return nil, nil
	}
	var list []routeEntry
	if err := json.Unmarshal([]byte(v), &list); err != nil {
		return nil, fmt.Errorf("the routes %q: %w", v, err)
	}
	return list, nil
}

func asMap(list []routeEntry) map[string]string {
	m := make(map[string]string, len(list))
	for _, r := range list {
		m[r.Dst] = r.GW
	}
	return m
}

func (kubeOVN) Routes(ctx context.Context, c client.Client, namespace string) (map[string]string, error) {
	ns := &corev1.Namespace{}
	if err := c.Get(ctx, types.NamespacedName{Name: namespace}, ns); err != nil {
		return nil, err
	}
	list, err := parseRoutes(ns.Annotations[routes.Annotation])
	return asMap(list), err
}

func (kubeOVN) AddRoute(ctx context.Context, c client.Client, namespace, destination, nextHop string) error {
	ns := &corev1.Namespace{}
	if err := c.Get(ctx, types.NamespacedName{Name: namespace}, ns); err != nil {
		return err
	}
	list, err := parseRoutes(ns.Annotations[routes.Annotation])
	if err != nil {
		return err
	}
	list = append(list, routeEntry{Dst: destination, GW: nextHop})
	data, err := json.Marshal(list)
	if err != nil {
		return err
	}
	if ns.Annotations == nil {
		ns.Annotations = map[string]string{}
	}
	ns.Annotations[routes.Annotation] = string(data)
	return c.Update(ctx, ns)
}

func (kubeOVN) PodRoutes(p *corev1.Pod) map[string]string {
	list, _ := parseRoutes(p.Annotations[routes.Annotation])
	return asMap(list)
}

// The connector for kube-ovn keeps the contract of a connector for the Forwarder
// API, which docs/connector-contract.md states.
func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Setup{
		Client:  k8sClient,
		Network: kubeOVN{},
		Start: func(t *testing.T) func() {
			return startConnector(t, nil).stop
		},
		StartUnreadable: func(t *testing.T, down *atomic.Bool) func() {
			return startConnector(t, func(c client.Client) client.Client { return flaky{Client: c, down: down} }).stop
		},
	})
}
