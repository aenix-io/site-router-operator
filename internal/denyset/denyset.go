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

// Package denyset judges the networks a SiteRouter declares against the
// networks that belong to the cluster.
//
// The schema of the SiteRouter refuses what is wrong whatever the cluster is. A
// network that overlaps the pods, the services, the nodes or the load balancers
// of this cluster is only wrong here, so it is judged here, by the operator, and
// refused with Validated=False. A route to such a network would send the traffic
// of the cluster into the tunnel, or let the remote end send traffic to the
// cluster addresses.
package denyset

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// Class names the kind of network a declared value collides with. A rejection
// names the class and never the address, so that the answer to a tenant does not
// carry the address of a node or of a load balancer of someone else.
type Class string

// Classes of network.
const (
	ClassPod          Class = "pod"
	ClassService      Class = "service"
	ClassJoin         Class = "join"
	ClassCluster      Class = "cluster"
	ClassNode         Class = "node"
	ClassLoadBalancer Class = "load-balancer"
	ClassLinkLocal    Class = "link-local"
	ClassLoopback     Class = "loopback"
	ClassDefaultRoute Class = "default-route"
	ClassMalformed    Class = "malformed"
	// ClassUnsupported is a network that is not IPv4. Overlaps reports false
	// across address families, so an IPv6 value would pass every check below and
	// be programmed as a route that can never match the traffic of the cluster.
	ClassUnsupported Class = "unsupported-address-family"
)

// Field names the field of the SiteRouter a value came from, so that a rejection
// says what to fix.
type Field string

// Fields the deny set judges: every one that programs a route or opens a rule on
// the gateway. A guard on one of several inputs of the same subsystem reads as a
// guard on all of them.
const (
	FieldRemoteCIDRs Field = "remoteCIDRs"
	FieldStaticRoute Field = "staticRoutes[].destination"
	FieldBGPNeighbor Field = "bgp.neighbors[].address"
)

// The two networks that are denied whatever the configuration says. The
// link-local range holds the metadata endpoint of a cloud.
const (
	linkLocalCIDR = "169.254.0.0/16"
	loopbackCIDR  = "127.0.0.0/8"
)

// Networks are the networks of the cluster a declared value must not overlap.
// Each entry is a network in CIDR notation; a single address is its /32.
type Networks struct {
	Pod           []string
	Service       []string
	Join          []string
	Cluster       []string // others the platform names
	Nodes         []string
	LoadBalancers []string
}

// Inputs are the declared values the deny set judges.
type Inputs struct {
	// RemoteCIDRs are the networks behind the remote end.
	RemoteCIDRs []string
	// StaticRouteDestinations are the destinations of the static routes.
	StaticRouteDestinations []string
	// BGPNeighborAddresses are bare addresses and are judged as /32. Besides the
	// neighbor itself, each one opens the BGP port of the gateway to its address,
	// so an unjudged neighbor would open it to an address that the same value
	// would have been refused as a remote network.
	BGPNeighborAddresses []string
}

// Rejection is one declared value that is refused.
type Rejection struct {
	Value string
	Field Field
	Class Class
}

// Message says what is wrong with the value in words for a tenant. It names the
// class of the network and not its address, except for the two that are the
// same everywhere.
func (r Rejection) Message() string {
	what := fmt.Sprintf("%s %q", r.Field, r.Value)
	switch r.Class {
	case ClassMalformed:
		return what + " is not a network"
	case ClassUnsupported:
		return what + " is not an IPv4 network, and only IPv4 is supported"
	case ClassDefaultRoute:
		return what + " is a default route, which would send all traffic of the cluster into the tunnel"
	case ClassLinkLocal:
		return what + " overlaps the link-local range " + linkLocalCIDR
	case ClassLoopback:
		return what + " overlaps the loopback range " + loopbackCIDR
	case ClassNode:
		return what + " overlaps the address of a node of the cluster"
	case ClassLoadBalancer:
		return what + " overlaps an address of a load balancer of the cluster"
	default:
		return fmt.Sprintf("%s overlaps a %s network of the cluster", what, r.Class)
	}
}

// RejectedError carries the rejections of a check. Its message is for a tenant.
type RejectedError struct{ Rejections []Rejection }

func (e *RejectedError) Error() string {
	msgs := make([]string, len(e.Rejections))
	for i, r := range e.Rejections {
		msgs[i] = r.Message()
	}
	return strings.Join(msgs, "; ")
}

type denied struct {
	class  Class
	prefix netip.Prefix
}

// Validate returns a Rejection for each declared value that is malformed, is not
// IPv4, is a default route or overlaps a network of the cluster, in the order
// the values were given. A value that overlaps several is reported for the first
// of them, in the order pod, service, join, cluster, node, load balancer,
// link-local, loopback.
//
// Overlap is judged in both directions: a value inside a network of the cluster
// is refused, and so is a broad one that contains such a network or a single
// address of one. Declared values that overlap each other are not a problem: the
// routes are scoped to a namespace.
//
// The error is for a network in nets that does not parse. That is a fault of the
// configuration and not of the tenant, and the check fails closed: leaving the
// network out would make the deny set narrower than the operator meant.
func Validate(in Inputs, nets Networks) ([]Rejection, error) {
	deny, err := build(nets)
	if err != nil {
		return nil, err
	}
	out := make([]Rejection, 0, len(in.RemoteCIDRs)+len(in.StaticRouteDestinations)+len(in.BGPNeighborAddresses))
	out = append(out, judge(FieldRemoteCIDRs, in.RemoteCIDRs, false, deny)...)
	out = append(out, judge(FieldStaticRoute, in.StaticRouteDestinations, false, deny)...)
	out = append(out, judge(FieldBGPNeighbor, in.BGPNeighborAddresses, true, deny)...)
	return out, nil
}

func judge(field Field, values []string, host bool, deny []denied) []Rejection {
	out := make([]Rejection, 0, len(values))
	for _, raw := range values {
		p, err := parse(raw, host)
		switch {
		case err != nil:
			out = append(out, Rejection{Value: raw, Field: field, Class: ClassMalformed})
			continue
		case !p.Addr().Is4():
			out = append(out, Rejection{Value: raw, Field: field, Class: ClassUnsupported})
			continue
		case p.Bits() == 0:
			// Reported as what it is, not as an overlap with whichever network
			// comes first.
			out = append(out, Rejection{Value: raw, Field: field, Class: ClassDefaultRoute})
			continue
		}
		p = p.Masked()
		for _, d := range deny {
			if p.Overlaps(d.prefix) {
				out = append(out, Rejection{Value: raw, Field: field, Class: d.class})
				break
			}
		}
	}
	return out
}

func parse(raw string, host bool) (netip.Prefix, error) {
	if !host {
		return netip.ParsePrefix(raw)
	}
	a, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

func build(n Networks) ([]denied, error) {
	var out []denied
	var errs []error
	add := func(class Class, cidrs []string) {
		for _, c := range cidrs {
			p, err := netip.ParsePrefix(c)
			if err != nil || !p.Addr().Is4() {
				errs = append(errs, fmt.Errorf("the %s network %q is not an IPv4 network: %w", class, c, orInvalid(err)))
				continue
			}
			out = append(out, denied{class: class, prefix: p.Masked()})
		}
	}
	add(ClassPod, n.Pod)
	add(ClassService, n.Service)
	add(ClassJoin, n.Join)
	add(ClassCluster, n.Cluster)
	add(ClassNode, n.Nodes)
	add(ClassLoadBalancer, n.LoadBalancers)
	add(ClassLinkLocal, []string{linkLocalCIDR})
	add(ClassLoopback, []string{loopbackCIDR})
	return out, errors.Join(errs...)
}

func orInvalid(err error) error {
	if err != nil {
		return err
	}
	return errors.New("not IPv4")
}
