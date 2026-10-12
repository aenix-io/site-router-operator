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

// IPv4CIDR is an IPv4 network in canonical form that a remote site can have: not
// the default route, not shorter than /8, and not overlapping 0.0.0.0/8,
// loopback, link-local or multicast.
//
// Only what is wrong whatever the cluster is rejected here. Whether a network
// overlaps the networks of the cluster depends on the cluster and is judged by
// the controller against the cluster itself, not by this schema.
//
// The length limit is what lets the apiserver bound the cost of the rules.
//
// +kubebuilder:validation:MaxLength=18
// +kubebuilder:validation:XValidation:rule="isCIDR(self) && cidr(self).ip().family() == 4",message="must be an IPv4 CIDR"
// +kubebuilder:validation:XValidation:rule="!isCIDR(self) || cidr(self).prefixLength() > 0",message="a default route (/0) is not allowed"
// +kubebuilder:validation:XValidation:rule="!isCIDR(self) || cidr(self).prefixLength() == 0 || cidr(self).prefixLength() >= 8",message="a network shorter than /8 is not allowed"
// +kubebuilder:validation:XValidation:rule="!isCIDR(self) || string(cidr(self).masked()) == self",message="host bits must be zero (use the network address)"
// +kubebuilder:validation:XValidation:rule="!isCIDR(self) || cidr(self).ip().family() != 4 || !['0.0.0.0/8', '127.0.0.0/8', '169.254.0.0/16', '224.0.0.0/4'].exists(r, cidr(r).containsIP(cidr(self).ip()) || cidr(self).containsIP(cidr(r).ip()))",message="must not overlap 0.0.0.0/8, loopback (127.0.0.0/8), link-local (169.254.0.0/16) or multicast (224.0.0.0/4)"
type IPv4CIDR string

// IPv4Address is an IPv4 address.
//
// +kubebuilder:validation:MaxLength=15
// +kubebuilder:validation:XValidation:rule="isIP(self) && ip(self).family() == 4",message="must be an IPv4 address"
type IPv4Address string

// ASN is a 32-bit autonomous system number.
//
// +kubebuilder:validation:Minimum=1
// +kubebuilder:validation:Maximum=4294967295
type ASN int64

// LocalSecretRef names a Secret in the namespace of the referring object.
type LocalSecretRef struct {
	// name is the name of the Secret.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	Name string `json:"name"`
}
