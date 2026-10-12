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

package denyset

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/aenix-io/site-router-operator/internal/reconcile"
)

// NewValidator returns the validator of the operator: it judges what an
// instance is going to program into the gateway against the networks of the
// cluster, which are those of the configuration and those found by the
// Discoverer.
//
// A rejection is a RejectedError, whose text is for a tenant. Failing to learn
// the networks of the cluster is not a rejection: the error wraps
// reconcile.ErrValidationUnavailable, so that the pass leaves the routes as they
// are until the cluster answers.
func NewValidator(cfg Config, d *Discoverer) (reconcile.Validator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("the networks of the cluster: %w", err)
	}
	if d == nil {
		return nil, errors.New("a Discoverer is required: without one the deny set would not know the nodes and the load balancers")
	}
	configured := cfg.Networks()
	return func(ctx context.Context, in reconcile.Instance) error {
		found, err := d.Discover(ctx)
		if err != nil {
			return fmt.Errorf("%w: %w", reconcile.ErrValidationUnavailable, err)
		}
		nets := configured
		nets.Nodes, nets.LoadBalancers = found.Nodes, found.LoadBalancers

		rejections, err := Validate(inputsOf(in), nets)
		if err != nil {
			return fmt.Errorf("%w: %w", reconcile.ErrValidationUnavailable, err)
		}
		if len(rejections) > 0 {
			return &RejectedError{Rejections: rejections}
		}
		return nil
	}, nil
}

// inputsOf collects every value of the instance that programs a route or opens a
// rule on the gateway. The same network is often declared in several places, as
// the remote network, as a selector of the tunnel and as a source of the filter,
// and it is judged once.
//
// The destinations of the filter are not here: they are the pods of the tenant,
// which are inside the networks of the cluster by definition. The local
// selectors, the next hops and the address of the peer are not judged either:
// none of them is a network the gateway routes to or accepts traffic from.
func inputsOf(in reconcile.Instance) Inputs {
	remote := appendNew(nil, in.RemoteCIDRs...)
	for _, t := range in.Desired.Tunnels {
		remote = appendNew(remote, t.RemoteSelectors...)
	}
	remote = appendNew(remote, in.Desired.Guards.TunnelIngress.Sources...)

	var routes []string
	for _, r := range in.Desired.Routes {
		routes = appendNew(routes, r.Dest)
	}
	var neighbors []string
	if in.Desired.BGP != nil {
		for _, n := range in.Desired.BGP.Neighbors {
			neighbors = appendNew(neighbors, n.Address)
		}
	}
	return Inputs{RemoteCIDRs: remote, StaticRouteDestinations: routes, BGPNeighborAddresses: neighbors}
}

func appendNew(dst []string, values ...string) []string {
	for _, v := range values {
		if !slices.Contains(dst, v) {
			dst = append(dst, v)
		}
	}
	return dst
}
