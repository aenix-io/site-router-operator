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
	"errors"
	"fmt"
	"net/netip"
)

// Config names the networks of the cluster that the operator cannot discover.
// They are set by whoever installs the operator, because only they know them,
// and they have no defaults: a deny set that silently assumed the pod network of
// one distribution would let a tenant route into the pods of any other.
type Config struct {
	PodCIDR     string `json:"podCIDR,omitempty"`
	ServiceCIDR string `json:"serviceCIDR,omitempty"`
	// JoinCIDR is the network that joins the nodes to the pod network, where the
	// cluster network has one.
	JoinCIDR string `json:"joinCIDR,omitempty"`
	// ExtraCIDRs are any other networks the platform keeps off the tenants.
	ExtraCIDRs []string `json:"extraCIDRs,omitempty"`
}

// Validate lists every problem with the configuration.
func (c Config) Validate() error {
	var errs []error
	check := func(name, value string, required bool) {
		if value == "" {
			if required {
				errs = append(errs, fmt.Errorf("%s is required", name))
			}
			return
		}
		p, err := netip.ParsePrefix(value)
		switch {
		case err != nil || !p.Addr().Is4():
			errs = append(errs, fmt.Errorf("%s: %q is not an IPv4 network", name, value))
		case p.Masked() != p:
			errs = append(errs, fmt.Errorf("%s: %q has host bits set, use %s", name, value, p.Masked()))
		}
	}
	check("podCIDR", c.PodCIDR, true)
	check("serviceCIDR", c.ServiceCIDR, true)
	check("joinCIDR", c.JoinCIDR, false)
	for _, e := range c.ExtraCIDRs {
		check("extraCIDRs", e, true)
	}
	return errors.Join(errs...)
}

// Networks returns the configured networks. Discovery adds the rest.
func (c Config) Networks() Networks {
	n := Networks{Pod: []string{c.PodCIDR}, Service: []string{c.ServiceCIDR}, Cluster: c.ExtraCIDRs}
	if c.JoinCIDR != "" {
		n.Join = []string{c.JoinCIDR}
	}
	return n
}
