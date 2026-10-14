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

// Package conformance is the suite that a connector for the Forwarder API of the
// site-router operator is held to, whatever network it serves. The contract is
// docs/connector-contract.md; every scenario here is one clause of it.
//
// The suite runs against a cluster that has the CRD of the Forwarder and the CRD
// of the VirtualMachineInstance of KubeVirt, with the connector running in it: an
// apiserver of the envtest kind will do, and there need be no network. The suite
// makes the namespaces, the pods and the instance as the tenant, KubeVirt and the
// operator would, writes the status of the pods and of the instance as KubeVirt
// and the kubelet would, and reads what the connector made of them. What only the
// network can say is asked of a Network.
package conformance

import (
	"context"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Network is what the suite cannot know of a cluster network: how a pod is made
// there, whether it is let forward, and which routes the network has.
type Network interface {
	// Admit does to a pod what the network does when the pod is made, before the
	// suite creates it: a network that gives a pod its routes or turns its checks
	// on at that moment does it here.
	Admit(ctx context.Context, c client.Client, pod *corev1.Pod) error
	// Forwards says whether the network lets the pod, as it is now, forward the
	// traffic of addresses that are not its own.
	Forwards(pod *corev1.Pod) bool
	// Routes says where the network sends the traffic of the pods of the namespace
	// that is for a destination: the destinations and their next hops. It has
	// every route of the namespace, the connector's and anybody else's.
	Routes(ctx context.Context, c client.Client, namespace string) (map[string]string, error)
	// AddRoute makes a route in the namespace that no Forwarder owns, as an
	// administrator would.
	AddRoute(ctx context.Context, c client.Client, namespace, destination, nextHop string) error
	// PodRoutes says which routes a pod carries: what the network gave it when it
	// was made, with Admit.
	PodRoutes(pod *corev1.Pod) map[string]string
}

// Setup is what Run needs of the connector under test.
type Setup struct {
	// Client reaches the apiserver the connector serves, with the rights of an
	// administrator.
	Client client.Client
	// Network answers for the network.
	Network Network
	// Start runs the connector and returns what stops it, which may be called more
	// than once. The connector is stopped when the scenario ends.
	Start func(t *testing.T) (stop func())
	// StartUnreadable runs the connector so that its reads of the cluster fail
	// while down is true. It is for a connector that can be given another client;
	// without it the scenario that needs it is skipped.
	StartUnreadable func(t *testing.T, down *atomic.Bool) (stop func())
}
