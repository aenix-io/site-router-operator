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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// SiteRouterSpec describes the remote site a gateway connects to. It is
// untrusted input: the operator checks it again against the cluster.
//
// +kubebuilder:validation:XValidation:rule="has(self.storageClass) == has(oldSelf.storageClass) && (!has(self.storageClass) || self.storageClass == oldSelf.storageClass)",message="storageClass is immutable"
type SiteRouterSpec struct {
	// peer is the remote end of the tunnel.
	//
	// +required
	Peer Peer `json:"peer"`

	// remoteCIDRs are the networks behind the remote end. Traffic to them is
	// routed to the gateway, and traffic from them is let through to the tenant.
	//
	// +listType=set
	// +kubebuilder:validation:MaxItems=16
	// +optional
	RemoteCIDRs []IPv4CIDR `json:"remoteCIDRs,omitempty"`

	// staticRoutes are routes inside the gateway.
	//
	// +listType=map
	// +listMapKey=destination
	// +kubebuilder:validation:MaxItems=16
	// +optional
	StaticRoutes []StaticRoute `json:"staticRoutes,omitempty"`

	// bgp configures BGP between the gateway and the remote site.
	//
	// +optional
	BGP BGP `json:"bgp,omitzero"`

	// resources are the CPU and memory of the gateway VM.
	//
	// +kubebuilder:default={}
	// +optional
	Resources Resources `json:"resources,omitzero"`

	// storageClass is the storage class of the gateway disk. It cannot be
	// changed after creation, and cannot be set later if it was left empty.
	//
	// +kubebuilder:validation:MaxLength=253
	// +optional
	StorageClass string `json:"storageClass,omitempty"`
}

// Peer is the remote end of the tunnel.
type Peer struct {
	// address is the IP address or DNS name of the remote end. It stays empty
	// while the address is not known, for example when the remote side is to
	// call first.
	//
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9.:-]*$`
	// +optional
	Address string `json:"address,omitempty"`

	// auth says how the tunnel is authenticated.
	//
	// +optional
	Auth *PeerAuth `json:"auth,omitempty"`
}

// PeerAuth is the authentication of the tunnel.
type PeerAuth struct {
	// secretRef names a Secret that holds the pre-shared key under the key
	// "psk". When it is not set, the operator creates the Secret.
	//
	// +optional
	SecretRef *LocalSecretRef `json:"secretRef,omitempty"`
}

// StaticRoute is a route inside the gateway.
type StaticRoute struct {
	// destination is the network the route leads to.
	//
	// +required
	Destination IPv4CIDR `json:"destination"`

	// nextHop is where traffic for the destination is sent.
	//
	// +required
	NextHop IPv4Address `json:"nextHop"`
}

// BGP is the BGP configuration of the gateway.
//
// +kubebuilder:validation:XValidation:rule="!has(self.enabled) || !self.enabled || has(self.localASN)",message="localASN is required when bgp.enabled is true"
type BGP struct {
	// enabled turns BGP on.
	//
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// localASN is the autonomous system number of the gateway.
	//
	// +optional
	LocalASN *ASN `json:"localASN,omitempty"`

	// neighbors are the BGP peers.
	//
	// +listType=map
	// +listMapKey=address
	// +kubebuilder:validation:MaxItems=8
	// +optional
	Neighbors []BGPNeighbor `json:"neighbors,omitempty"`
}

// BGPNeighbor is a BGP peer of the gateway.
type BGPNeighbor struct {
	// address is the address of the peer.
	//
	// +required
	Address IPv4Address `json:"address"`

	// remoteASN is the autonomous system number of the peer.
	//
	// +required
	RemoteASN ASN `json:"remoteASN"`
}

// Resources are the CPU and memory of the gateway VM.
type Resources struct {
	// cpu is the number of virtual CPUs.
	//
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +optional
	CPU int32 `json:"cpu,omitempty"`

	// memory is the amount of memory.
	//
	// +kubebuilder:default="2Gi"
	// +optional
	Memory resource.Quantity `json:"memory,omitzero"`
}

// SiteRouterStatus is the observed state of a SiteRouter.
type SiteRouterStatus struct {
	// observedGeneration is the generation the conditions were computed for.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions describe the state of the gateway. The types are the
	// Condition* constants of this package.
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// tunnelAddress is the address of the gateway as the remote side sees it.
	// It is what the remote side is configured to connect to, and the IKE
	// identity of the gateway.
	//
	// +optional
	TunnelAddress string `json:"tunnelAddress,omitempty"`

	// pskSecretRef names the Secret that holds the pre-shared key.
	//
	// +optional
	PSKSecretRef *LocalSecretRef `json:"pskSecretRef,omitempty"`

	// engine says what runs inside the gateway.
	//
	// +optional
	Engine EngineStatus `json:"engine,omitzero"`
}

// EngineStatus is the appliance a gateway runs.
type EngineStatus struct {
	// name is the name of the engine.
	//
	// +optional
	Name string `json:"name,omitempty"`

	// version is the version of the appliance image.
	//
	// +optional
	Version string `json:"version,omitempty"`

	// imageDigest is the digest of the image the gateway booted from.
	//
	// +optional
	ImageDigest string `json:"imageDigest,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Tunnel Address",type=string,JSONPath=`.status.tunnelAddress`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 40",message="metadata.name must be at most 40 characters"

// SiteRouter is a routed site-to-site IPsec gateway.
type SiteRouter struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	//
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec is the desired state.
	//
	// +required
	Spec SiteRouterSpec `json:"spec"`

	// status is the observed state.
	//
	// +optional
	Status SiteRouterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SiteRouterList is a list of SiteRouter.
type SiteRouterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SiteRouter `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &SiteRouter{}, &SiteRouterList{})
		return nil
	})
}
