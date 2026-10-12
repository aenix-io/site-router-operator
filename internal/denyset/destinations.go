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
	"net/netip"
	"slices"
)

// Destinations returns the addresses the gateway may be opened to, from those a
// connector reported as the scope of a Forwarder, as the /32 networks the
// source filter takes, in order and without repeats. It drops the rest and says
// how many, for the log and not for a tenant: the connector is trusted to say
// what is the tenant's and is not taken on its word, because what the filter
// admits is the one thing standing between the remote site and the rest of the
// cluster.
//
// What is dropped is an address that is not IPv4, and one that is not the
// tenant's to reach: an address of a node, of a load balancer, of a network the
// platform keeps off its tenants, and of the link-local and loopback ranges. The
// pods and the services of the tenant are inside the networks of the cluster, so
// those are not what is dropped.
//
// The error is for a network of the cluster that does not parse, as in
// Validate.
func Destinations(addrs []string, nets Networks) (kept []string, dropped int, err error) {
	deny, err := build(nets)
	if err != nil {
		return nil, 0, err
	}
	deny = slices.DeleteFunc(deny, func(d denied) bool {
		return d.class == ClassPod || d.class == ClassService || d.class == ClassJoin
	})

	set := map[string]bool{}
	for _, raw := range addrs {
		a, perr := netip.ParseAddr(raw)
		if perr != nil || !a.Is4() {
			dropped++
			continue
		}
		host := netip.PrefixFrom(a, a.BitLen())
		if slices.ContainsFunc(deny, func(d denied) bool { return d.prefix.Overlaps(host) }) {
			dropped++
			continue
		}
		set[host.String()] = true
	}
	for p := range set {
		kept = append(kept, p)
	}
	slices.Sort(kept)
	return kept, dropped, nil
}
