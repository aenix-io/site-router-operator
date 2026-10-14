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

package controller

import (
	"context"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/internal/forwarder"
	"github.com/aenix-io/site-router-operator/internal/reconcile"
)

// Network is the network of the cluster as the controller asks it for the routes
// of one SiteRouter: where the gateway is, which workloads the tenant has, and
// the routes to the gateway and the permission for it to forward. The Forwarder API is
// how it is asked today, and Routes of the forwarder package is the Network that
// does it; another way of asking is another Network, given in Options.
type Network interface {
	reconcile.Routes
	// Ensure makes the request exist, so that the network can find the gateway and
	// report the addresses of the tenant before anything is routed.
	Ensure(ctx context.Context) error
	// Report says what the network found of the gateway and of the tenant.
	Report(ctx context.Context) (forwarder.Report, error)
	// Withdrawn says whether what Withdraw asked for has taken effect.
	Withdrawn(ctx context.Context) (bool, error)
}

// NetworkOf returns the Network of a SiteRouter.
type NetworkOf func(sr *v1alpha1.SiteRouter) Network

var _ Network = (*forwarder.Routes)(nil)
