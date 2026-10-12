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

// Condition types of a SiteRouter. Ready is the aggregate of Validated,
// GatewayReady, Configured and RoutesProgrammed. TunnelEstablished is not part
// of it: a gateway that answers can be Ready while it waits for the remote
// side to call.
const (
	ConditionReady             = "Ready"
	ConditionValidated         = "Validated"
	ConditionGatewayReady      = "GatewayReady"
	ConditionConfigured        = "Configured"
	ConditionRoutesProgrammed  = "RoutesProgrammed"
	ConditionTunnelEstablished = "TunnelEstablished"
)

// Reasons of a SiteRouter condition that is False, or informational.
const (
	// Validated.
	ReasonInvalidRemoteCIDR = "InvalidRemoteCIDR"
	ReasonPeerNotConfigured = "PeerNotConfigured"

	// GatewayReady.
	ReasonGatewayPending       = "GatewayPending"
	ReasonTunnelAddressPending = "TunnelAddressPending"
	ReasonPSKSecretPending     = "PSKSecretPending"
	ReasonUpgrading            = "Upgrading"

	// Configured.
	ReasonConfigureFailed          = "ConfigureFailed"
	ReasonSourceFilterPending      = "SourceFilterPending"
	ReasonAPICredentialsPending    = "APICredentialsPending"
	ReasonBGPLocalASNInvalid       = "BGPLocalASNInvalid"
	ReasonEngineVersionUnsupported = "EngineVersionUnsupported"

	// RoutesProgrammed.
	ReasonForwarderNotResponding = "ForwarderNotResponding"
	ReasonRouteConflict          = "RouteConflict"
	ReasonPendingWorkloads       = "PendingWorkloads"
)

// Condition types of a Forwarder, set by the connector that serves it.
const (
	// ForwarderAccepted: the connector understands the object and serves it.
	ForwarderAccepted = "Accepted"
	// ForwarderResolved: the selectors matched pods and their addresses are known.
	ForwarderResolved = "Resolved"
	// ForwarderGranted: the gateway pods may forward traffic for the CIDRs.
	ForwarderGranted = "Granted"
	// ForwarderRoutesProgrammed: routes to the gateway are in place.
	ForwarderRoutesProgrammed = "RoutesProgrammed"
	// ForwarderConflict: another forwarder in the same scope claims a CIDR.
	ForwarderConflict = "Conflict"
)
