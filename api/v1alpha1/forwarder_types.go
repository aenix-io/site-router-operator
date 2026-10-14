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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ForwarderSpec says which pods forward traffic for which networks. The
// operator writes it; a connector for the cluster network reads it and makes
// the network do what it says.
//
// An empty workload selector would select every pod of the namespace, and the
// connector grants forwarding to the pods it selects, so it is refused.
//
// +kubebuilder:validation:XValidation:rule="(has(self.workloadSelector.matchLabels) && size(self.workloadSelector.matchLabels) > 0) || (has(self.workloadSelector.matchExpressions) && size(self.workloadSelector.matchExpressions) > 0)",message="workloadSelector must select something"
type ForwarderSpec struct {
	// workloadSelector selects the pods that forward: the gateway.
	//
	// +required
	WorkloadSelector metav1.LabelSelector `json:"workloadSelector"`

	// scopeSelector selects the pods that need a route to the gateway: the
	// tenant's workloads. Empty selects every pod of the namespace.
	//
	// +optional
	ScopeSelector metav1.LabelSelector `json:"scopeSelector,omitzero"`

	// cidrs are the networks that are routed to the gateway. They are also the
	// networks the gateway may forward traffic from and to. While the list is
	// empty the connector grants nothing and programs no routes.
	//
	// +listType=set
	// +kubebuilder:validation:MaxItems=16
	// +optional
	CIDRs []IPv4CIDR `json:"cidrs,omitempty"`
}

// AddressSet is a list of addresses.
type AddressSet struct {
	// addresses are IP addresses in the addressing of the cluster network.
	//
	// +listType=set
	// +optional
	Addresses []string `json:"addresses,omitempty"`
}

// ForwarderStatus is the state of a Forwarder as the connector sees it.
type ForwarderStatus struct {
	// observedGeneration is the generation the status was computed for.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions describe the state. The types are the Forwarder* constants of
	// this package.
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// forwarder holds the addresses of the pods selected by workloadSelector.
	// They are the next hop of the routes.
	//
	// +optional
	Forwarder AddressSet `json:"forwarder,omitzero"`

	// scope holds the addresses of the pods selected by scopeSelector. They
	// are what the gateway lets decrypted traffic through to.
	//
	// +optional
	Scope AddressSet `json:"scope,omitzero"`

	// pendingWorkloads are the names of pods in scope that were created before
	// their routes were in place. On networks that apply routes only when a
	// pod is created, they need to be restarted.
	//
	// +listType=set
	// +optional
	PendingWorkloads []string `json:"pendingWorkloads,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Forwarder asks the cluster network to route networks to a gateway and to let
// the gateway forward their traffic.
type Forwarder struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	//
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec is the desired state.
	//
	// +required
	Spec ForwarderSpec `json:"spec"`

	// status is the state the connector reports.
	//
	// +optional
	Status ForwarderStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ForwarderList is a list of Forwarder.
type ForwarderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Forwarder `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Forwarder{}, &ForwarderList{})
		return nil
	})
}
