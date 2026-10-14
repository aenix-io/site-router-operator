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
	ReasonInvalidRemoteCIDR     = "InvalidRemoteCIDR"
	ReasonPeerNotConfigured     = "PeerNotConfigured"
	ReasonValidationUnavailable = "ValidationUnavailable"

	// GatewayReady.
	ReasonGatewayPending       = "GatewayPending"
	ReasonTunnelAddressPending = "TunnelAddressPending"
	ReasonPSKSecretPending     = "PSKSecretPending"
	ReasonUpgrading            = "Upgrading"
	// ReasonUpgradeFailed: the disk of the other image could not be made ready,
	// and the gateway is still on the one it had.
	ReasonUpgradeFailed = "UpgradeFailed"
	// ReasonObjectConflict: an object of the name of one of the gateway exists and
	// is not the operator's.
	ReasonObjectConflict = "ObjectConflict"
	// ReasonLoadBalancerClassChanged: the class of the load balancer of the tunnel
	// cannot be changed on a Service that exists.
	ReasonLoadBalancerClassChanged = "LoadBalancerClassChanged"
	// ReasonGatewayIsolationUnavailable: the platform has the gateway run with no
	// isolation and does not say so on purpose.
	ReasonGatewayIsolationUnavailable = "GatewayIsolationUnavailable"
	// ReasonReconcileFailed is for a failure that has no reason of its own, so that
	// none goes unsaid.
	ReasonReconcileFailed = "ReconcileFailed"

	// Configured.
	ReasonConfigureFailed          = "ConfigureFailed"
	ReasonSourceFilterPending      = "SourceFilterPending"
	ReasonAPICredentialsPending    = "APICredentialsPending"
	ReasonBGPLocalASNInvalid       = "BGPLocalASNInvalid"
	ReasonEngineVersionUnsupported = "EngineVersionUnsupported"

	// RoutesProgrammed.
	ReasonForwarderNotResponding = "ForwarderNotResponding"
	ReasonForwarderRejected      = "ForwarderRejected"
	ReasonRouteConflict          = "RouteConflict"
	ReasonPendingWorkloads       = "PendingWorkloads"
	// ReasonRoutesPending: the connector has the request and has not carried it
	// out yet.
	ReasonRoutesPending = "RoutesPending"
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
