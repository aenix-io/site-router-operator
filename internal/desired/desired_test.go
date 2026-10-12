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

package desired

import (
	"reflect"
	"slices"
	"testing"

	"github.com/aenix-io/site-router-operator/api/engine"
	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/internal/denyset"
)

func asn(n int64) *v1alpha1.ASN {
	a := v1alpha1.ASN(n)
	return &a
}

func input(mutate ...func(*Input)) Input {
	in := Input{
		Instance: &v1alpha1.SiteRouter{
			Name: "branch-office",
			Spec: v1alpha1.SiteRouterSpec{
				Peer:        v1alpha1.Peer{Address: "203.0.113.10"},
				RemoteCIDRs: []v1alpha1.IPv4CIDR{"198.51.100.0/24", "192.0.2.0/24"},
			},
		},
		TunnelAddress:  "198.51.100.200",
		PSK:            "s3cr3t",
		ManagementCIDR: "10.244.0.0/16",
		Scope:          []string{"10.244.1.6", "10.244.1.5"},
		Networks:       denyset.Networks{Pod: []string{"10.244.0.0/16"}, Nodes: []string{"192.168.100.5/32"}},
		NICMACs:        []string{"02:00:00:00:00:02", "02:00:00:00:00:01"},
	}
	for _, m := range mutate {
		m(&in)
	}
	return in
}

func TestBuildAFullInstance(t *testing.T) {
	out, err := Build(input(func(in *Input) {
		in.Instance.Spec.StaticRoutes = []v1alpha1.StaticRoute{
			{Destination: "10.9.0.0/16", NextHop: "10.0.0.2"}, {Destination: "10.8.0.0/16", NextHop: "10.0.0.1"},
		}
		in.Instance.Spec.BGP = v1alpha1.BGP{
			Enabled: true, LocalASN: asn(65001),
			Neighbors: []v1alpha1.BGPNeighbor{{Address: "10.0.0.9", RemoteASN: 4294967295}, {Address: "10.0.0.2", RemoteASN: 65002}},
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := engine.Desired{
		Tunnels: []engine.Tunnel{{
			ID: "tunnel-0", PeerAddress: "203.0.113.10", PSK: "s3cr3t", LocalID: "198.51.100.200",
			LocalSelectors: []string{"0.0.0.0/0"}, RemoteSelectors: []string{"192.0.2.0/24", "198.51.100.0/24"},
		}},
		Routes: []engine.Route{{Dest: "10.8.0.0/16", NextHop: "10.0.0.1"}, {Dest: "10.9.0.0/16", NextHop: "10.0.0.2"}},
		BGP: &engine.BGP{LocalASN: 65001, Neighbors: []engine.Neighbor{
			{Address: "10.0.0.2", RemoteASN: 65002}, {Address: "10.0.0.9", RemoteASN: 4294967295},
		}},
		Guards: engine.Guards{
			ManagementAllow: "10.244.0.0/16",
			TunnelIngress: engine.TunnelIngress{
				Sources:      []string{"192.0.2.0/24", "198.51.100.0/24"},
				Destinations: []string{"10.244.1.5/32", "10.244.1.6/32"},
			},
		},
		TunnelNICMACs: []string{"02:00:00:00:00:01", "02:00:00:00:00:02"},
	}
	if !reflect.DeepEqual(out.Desired, want) {
		t.Errorf("Desired =\n%+v\nwant\n%+v", out.Desired, want)
	}
	if !out.TunnelConfigured || !slices.Equal(out.RemoteCIDRs, []string{"192.0.2.0/24", "198.51.100.0/24"}) {
		t.Errorf("Output = %+v", out)
	}
}

// What the engine gets is the same for two inputs that mean the same, so that a
// reordered list is not taken for a change.
func TestBuildDoesNotDependOnTheOrderOfLists(t *testing.T) {
	a := input(func(in *Input) {
		in.Instance.Spec.StaticRoutes = []v1alpha1.StaticRoute{{Destination: "10.8.0.0/16", NextHop: "10.0.0.1"}, {Destination: "10.9.0.0/16", NextHop: "10.0.0.2"}}
		in.Instance.Spec.BGP = v1alpha1.BGP{Enabled: true, LocalASN: asn(65001), Neighbors: []v1alpha1.BGPNeighbor{{Address: "10.0.0.2", RemoteASN: 1}, {Address: "10.0.0.9", RemoteASN: 2}}}
	})
	b := input(func(in *Input) {
		in.Instance.Spec.RemoteCIDRs = []v1alpha1.IPv4CIDR{"192.0.2.0/24", "198.51.100.0/24"}
		in.Instance.Spec.StaticRoutes = []v1alpha1.StaticRoute{{Destination: "10.9.0.0/16", NextHop: "10.0.0.2"}, {Destination: "10.8.0.0/16", NextHop: "10.0.0.1"}}
		in.Instance.Spec.BGP = v1alpha1.BGP{Enabled: true, LocalASN: asn(65001), Neighbors: []v1alpha1.BGPNeighbor{{Address: "10.0.0.9", RemoteASN: 2}, {Address: "10.0.0.2", RemoteASN: 1}}}
		in.Scope = []string{"10.244.1.5", "10.244.1.6", "10.244.1.5"}
		in.NICMACs = []string{"02:00:00:00:00:01", "02:00:00:00:00:02"}
	})
	oa, err := Build(a)
	if err != nil {
		t.Fatal(err)
	}
	ob, err := Build(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(oa.Desired, ob.Desired) {
		t.Errorf("the same instance in another order gives another state:\n%+v\n%+v", oa.Desired, ob.Desired)
	}
}

func TestBuildDoesNotChangeTheInstance(t *testing.T) {
	in := input()
	before := in.Instance.DeepCopy()
	scope := slices.Clone(in.Scope)
	macs := slices.Clone(in.NICMACs)
	if _, err := Build(in); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in.Instance, before) || !slices.Equal(in.Scope, scope) || !slices.Equal(in.NICMACs, macs) {
		t.Error("Build changed what it was given")
	}
}

func TestTheTunnelIsLeftOutWhenThereIsNothingToCall(t *testing.T) {
	for name, mutate := range map[string]func(*Input){
		"no peer address":    func(in *Input) { in.Instance.Spec.Peer.Address = "" },
		"no key":             func(in *Input) { in.PSK = "" },
		"no tunnel address":  func(in *Input) { in.TunnelAddress = "" },
		"nothing of the lot": func(in *Input) { in.Instance.Spec.Peer.Address, in.PSK, in.TunnelAddress = "", "", "" },
	} {
		out, err := Build(input(mutate))
		if err != nil {
			t.Fatal(err)
		}
		if out.TunnelConfigured || len(out.Desired.Tunnels) != 0 {
			t.Errorf("%s: the tunnel was configured: %+v", name, out.Desired.Tunnels)
		}
		// The gateway is configured all the same: its filter and routes do not wait
		// for a peer.
		if out.Desired.Guards.ManagementAllow == "" || len(out.Desired.Guards.TunnelIngress.Destinations) == 0 {
			t.Errorf("%s: the guards were left out with the tunnel: %+v", name, out.Desired.Guards)
		}
	}
}

func TestBGPIsConfiguredOnlyWhenItIsOnAndHasAnAS(t *testing.T) {
	cases := map[string]struct {
		bgp  v1alpha1.BGP
		want bool
	}{
		"on, with an AS":      {v1alpha1.BGP{Enabled: true, LocalASN: asn(65001)}, true},
		"off, with an AS":     {v1alpha1.BGP{Enabled: false, LocalASN: asn(65001)}, false},
		"on, with no AS":      {v1alpha1.BGP{Enabled: true}, false},
		"off, with neighbors": {v1alpha1.BGP{Neighbors: []v1alpha1.BGPNeighbor{{Address: "10.0.0.2", RemoteASN: 1}}}, false},
	}
	for name, c := range cases {
		out, err := Build(input(func(in *Input) { in.Instance.Spec.BGP = c.bgp }))
		if err != nil {
			t.Fatal(err)
		}
		if got := out.Desired.BGP != nil; got != c.want {
			t.Errorf("%s: BGP configured = %v, want %v", name, got, c.want)
		}
	}
}

func TestNoRemoteNetworksIsAnEmptyFilter(t *testing.T) {
	out, err := Build(input(func(in *Input) { in.Instance.Spec.RemoteCIDRs = nil }))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.RemoteCIDRs) != 0 || len(out.Desired.Guards.TunnelIngress.Sources) != 0 {
		t.Errorf("Output = %+v: with no remote network nothing is let in", out)
	}
}

func TestTheGatewayIsOnlyOpenedToWhatIsTheTenantsToReach(t *testing.T) {
	out, err := Build(input(func(in *Input) {
		in.Scope = []string{"10.244.1.5", "192.168.100.5", "169.254.169.254", "2001:db8::1", "web-0"}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(out.Desired.Guards.TunnelIngress.Destinations, []string{"10.244.1.5/32"}) || out.DroppedDestinations != 4 {
		t.Errorf("destinations %v, dropped %d; want the pod alone and four dropped", out.Desired.Guards.TunnelIngress.Destinations, out.DroppedDestinations)
	}
}

func TestANetworkOfTheClusterThatCannotBeReadFailsTheBuild(t *testing.T) {
	if _, err := Build(input(func(in *Input) { in.Networks.Nodes = []string{"nonsense"} })); err == nil {
		t.Error("a state was built from a deny set that could not be read")
	}
}

func TestTheKeyIsInTheTunnelAndNowhereElse(t *testing.T) {
	out, err := Build(input())
	if err != nil {
		t.Fatal(err)
	}
	if got := out.Desired.Secrets(); !slices.Equal(got, []string{"s3cr3t"}) {
		t.Errorf("secrets = %v, want the key alone", got)
	}
}
