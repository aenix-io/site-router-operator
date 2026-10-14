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
	"slices"
	"strings"
	"testing"
)

func cluster() Networks {
	return Networks{
		Pod:           []string{"10.244.0.0/16"},
		Service:       []string{"10.96.0.0/16"},
		Join:          []string{"100.64.0.0/16"},
		Cluster:       []string{"172.20.0.0/14"},
		Nodes:         []string{"192.168.100.5/32", "192.168.100.6/32"},
		LoadBalancers: []string{"203.0.113.10/32"},
	}
}

func remote(values ...string) Inputs { return Inputs{RemoteCIDRs: values} }

func TestValidateJudgesEveryKindOfNetwork(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  Class // empty: accepted
	}{
		{"disjoint", "198.51.100.0/24", ""},
		{"another disjoint", "172.31.0.0/16", ""},
		{"next to the pod network", "10.245.0.0/16", ""},
		{"a host next to a node", "192.168.100.7/32", ""},

		{"inside the pod network", "10.244.5.0/24", ClassPod},
		{"the pod network itself", "10.244.0.0/16", ClassPod},
		{"swallowing the pod network", "10.0.0.0/8", ClassPod},
		{"inside the service network", "10.96.1.0/24", ClassService},
		{"inside the join network", "100.64.0.0/20", ClassJoin},
		{"inside another network of the cluster", "172.21.0.0/24", ClassCluster},
		{"a node address", "192.168.100.5/32", ClassNode},
		{"a network that holds a node address", "192.168.100.0/24", ClassNode},
		{"a network that holds a node address, host bits set", "192.168.100.77/24", ClassNode},
		{"a load balancer address", "203.0.113.10/32", ClassLoadBalancer},
		{"a network that holds a load balancer address", "203.0.113.0/24", ClassLoadBalancer},
		{"the metadata address", "169.254.169.254/32", ClassLinkLocal},
		{"the link-local range", "169.254.0.0/16", ClassLinkLocal},
		{"swallowing the link-local range", "169.0.0.0/8", ClassLinkLocal},
		{"loopback", "127.0.0.0/8", ClassLoopback},
		{"an address of loopback", "127.0.0.1/32", ClassLoopback},

		{"the default route", "0.0.0.0/0", ClassDefaultRoute},
		{"the default route in another notation", "10.0.0.0/0", ClassDefaultRoute},
		{"IPv6", "2001:db8::/32", ClassUnsupported},
		{"IPv4 mapped into IPv6", "::ffff:10.244.0.0/112", ClassUnsupported},
		{"not a network", "not-a-network", ClassMalformed},
		{"a prefix beyond 32", "10.0.0.0/33", ClassMalformed},
		{"a bare address", "172.31.0.5", ClassMalformed},
		{"empty", "", ClassMalformed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Validate(remote(c.value), cluster())
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case c.want == "" && len(got) != 0:
				t.Fatalf("%q was refused: %v", c.value, got)
			case c.want != "" && len(got) != 1:
				t.Fatalf("%q: %d rejections, want one of class %s", c.value, len(got), c.want)
			case c.want != "" && got[0].Class != c.want:
				t.Errorf("%q was refused as %s, want %s", c.value, got[0].Class, c.want)
			}
		})
	}
}

func TestValidateJudgesEveryFieldThatProgramsTheGateway(t *testing.T) {
	got, err := Validate(Inputs{
		RemoteCIDRs:             []string{"10.244.1.0/24"},
		StaticRouteDestinations: []string{"10.96.0.0/24"},
		BGPNeighborAddresses:    []string{"192.168.100.5"},
	}, cluster())
	if err != nil {
		t.Fatal(err)
	}
	want := []Rejection{
		{Value: "10.244.1.0/24", Field: FieldRemoteCIDRs, Class: ClassPod},
		{Value: "10.96.0.0/24", Field: FieldStaticRoute, Class: ClassService},
		{Value: "192.168.100.5", Field: FieldBGPNeighbor, Class: ClassNode},
	}
	if !slices.Equal(got, want) {
		t.Errorf("rejections = %+v\nwant         %+v", got, want)
	}
}

func TestABGPNeighborIsJudgedAsOneHost(t *testing.T) {
	for _, c := range []struct {
		addr string
		want Class
	}{
		{"10.244.0.9", ClassPod},
		{"192.168.100.5", ClassNode},
		{"192.168.100.9", ""}, // next to a node, but not a node
		{"198.51.100.1", ""},
		{"10.244.0.0/16", ClassMalformed}, // a network is not an address
		{"2001:db8::1", ClassUnsupported},
	} {
		got, err := Validate(Inputs{BGPNeighborAddresses: []string{c.addr}}, cluster())
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case c.want == "" && len(got) != 0:
			t.Errorf("neighbor %s was refused: %v", c.addr, got)
		case c.want != "" && (len(got) != 1 || got[0].Class != c.want):
			t.Errorf("neighbor %s: %v, want one of class %s", c.addr, got, c.want)
		}
	}
}

func TestValidateReportsEveryOffenderInOrder(t *testing.T) {
	got, err := Validate(remote("198.51.100.0/24", "10.244.0.0/24", "garbage", "203.0.113.0/24", "172.31.0.0/16"), cluster())
	if err != nil {
		t.Fatal(err)
	}
	classes := make([]Class, 0, len(got))
	for _, r := range got {
		classes = append(classes, r.Class)
	}
	if want := []Class{ClassPod, ClassMalformed, ClassLoadBalancer}; !slices.Equal(classes, want) {
		t.Errorf("classes = %v, want %v", classes, want)
	}
}

func TestFirstNetworkInTheOrderWins(t *testing.T) {
	// 10.96.0.0/16 is the service network, and the pod network is checked first:
	// a value that holds both is reported for the pod network.
	got, err := Validate(remote("10.0.0.0/8"), cluster())
	if err != nil || len(got) != 1 || got[0].Class != ClassPod {
		t.Errorf("got %v, %v; want one rejection of class pod", got, err)
	}
	// With no pod network configured the same value is a service one.
	nets := cluster()
	nets.Pod = nil
	got, err = Validate(remote("10.0.0.0/8"), nets)
	if err != nil || len(got) != 1 || got[0].Class != ClassService {
		t.Errorf("got %v, %v; want one rejection of class service", got, err)
	}
}

func TestOverlapAmongDeclaredValuesIsNotAProblem(t *testing.T) {
	got, err := Validate(remote("198.51.100.0/24", "198.51.100.0/25", "198.51.100.0/24"), cluster())
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v; routes are scoped to a namespace, so overlap among declared values is allowed", got, err)
	}
}

func TestLinkLocalAndLoopbackAreDeniedWithNoConfiguration(t *testing.T) {
	for _, v := range []string{"169.254.169.254/32", "127.0.0.1/32"} {
		got, err := Validate(remote(v), Networks{})
		if err != nil || len(got) != 1 {
			t.Errorf("%s with no networks configured: %v, %v; want one rejection", v, got, err)
		}
	}
}

func TestAMalformedNetworkOfTheClusterFailsTheCheck(t *testing.T) {
	for name, nets := range map[string]Networks{
		"pod":     {Pod: []string{"not-a-network"}},
		"node":    {Nodes: []string{"10.0.0.5"}},
		"IPv6":    {Service: []string{"2001:db8::/32"}},
		"empty":   {Join: []string{""}},
		"several": {Pod: []string{"x"}, Service: []string{"y"}},
	} {
		got, err := Validate(remote("198.51.100.0/24"), nets)
		if err == nil || got != nil {
			t.Errorf("%s: got %v, %v; a network the deny set cannot read must fail the check, not narrow it", name, got, err)
		}
	}
	if _, err := Validate(remote("1.0.0.0/8"), Networks{Pod: []string{"x"}, Service: []string{"y"}}); err == nil ||
		!strings.Contains(err.Error(), "pod") || !strings.Contains(err.Error(), "service") {
		t.Errorf("the error names only some of the networks: %v", err)
	}
}

// The answer to a tenant names the class of the network, not its address: the
// address of a node, of a load balancer or of the networks of the cluster is not
// the tenant's to learn from an error.
func TestMessagesDoNotCarryAddressesOfTheCluster(t *testing.T) {
	nets := cluster()
	secrets := []string{"192.168.100", "203.0.113", "10.244", "10.96", "100.64", "172.20"}
	for _, v := range []string{"192.168.100.5/32", "192.168.100.0/24", "203.0.113.0/24", "10.244.0.0/16", "10.96.1.0/24", "100.64.0.0/20", "172.21.0.0/24"} {
		got, err := Validate(remote(v), nets)
		if err != nil || len(got) != 1 {
			t.Fatalf("%s: %v, %v", v, got, err)
		}
		msg := got[0].Message()
		if !strings.Contains(msg, `"`+v+`"`) || !strings.Contains(msg, string(FieldRemoteCIDRs)) {
			t.Errorf("the message does not say which value of which field is wrong: %s", msg)
		}
		// What is left after taking out the declared value itself.
		rest := strings.ReplaceAll(msg, v, "")
		for _, s := range secrets {
			if strings.Contains(rest, s) {
				t.Errorf("the message for %s carries %s: %s", v, s, msg)
			}
		}
	}
}

func TestMessageForEveryClass(t *testing.T) {
	for class, want := range map[Class]string{
		ClassMalformed:    "is not a network",
		ClassUnsupported:  "only IPv4 is supported",
		ClassDefaultRoute: "is a default route",
		ClassLinkLocal:    "169.254.0.0/16",
		ClassLoopback:     "127.0.0.0/8",
		ClassNode:         "address of a node",
		ClassLoadBalancer: "address of a load balancer",
		ClassPod:          "pod network",
		ClassService:      "service network",
		ClassJoin:         "join network",
		ClassCluster:      "cluster network",
	} {
		msg := Rejection{Value: "v", Field: FieldStaticRoute, Class: class}.Message()
		if !strings.Contains(msg, want) || !strings.Contains(msg, "staticRoutes[].destination") {
			t.Errorf("class %s: message %q does not say %q and the field", class, msg, want)
		}
	}
}

func TestRejectedErrorJoinsTheMessages(t *testing.T) {
	var err error = &RejectedError{Rejections: []Rejection{
		{Value: "10.244.0.0/24", Field: FieldRemoteCIDRs, Class: ClassPod},
		{Value: "garbage", Field: FieldStaticRoute, Class: ClassMalformed},
	}}
	if _, ok := errors.AsType[*RejectedError](err); !ok {
		t.Fatal("errors.AsType does not find the RejectedError")
	}
	for _, want := range []string{"10.244.0.0/24", "pod network", "garbage", "is not a network", "; "} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	good := Config{PodCIDR: "10.244.0.0/16", ServiceCIDR: "10.96.0.0/16"}
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string // empty: accepted
	}{
		{"pod and service only", func(*Config) {}, ""},
		{"with a join network and extras", func(c *Config) {
			c.JoinCIDR = "100.64.0.0/16"
			c.ExtraCIDRs = []string{"172.20.0.0/14", "192.168.0.0/24"}
		}, ""},
		{"no pod network", func(c *Config) { c.PodCIDR = "" }, "podCIDR is required"},
		{"no service network", func(c *Config) { c.ServiceCIDR = "" }, "serviceCIDR is required"},
		{"a pod network that is not one", func(c *Config) { c.PodCIDR = "pods" }, "podCIDR"},
		{"an IPv6 service network", func(c *Config) { c.ServiceCIDR = "fd00::/108" }, "not an IPv4 network"},
		{"host bits in the pod network", func(c *Config) { c.PodCIDR = "10.244.0.1/16" }, "host bits"},
		{"a join network that is not one", func(c *Config) { c.JoinCIDR = "x" }, "joinCIDR"},
		{"an empty extra", func(c *Config) { c.ExtraCIDRs = []string{""} }, "extraCIDRs is required"},
		{"an extra with host bits", func(c *Config) { c.ExtraCIDRs = []string{"172.20.0.1/14"} }, "host bits"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := good
			c.mutate(&cfg)
			err := cfg.Validate()
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("error = %v, want one that mentions %q", err, c.want)
			}
		})
	}
}

func TestConfigValidateReportsEveryProblem(t *testing.T) {
	err := Config{JoinCIDR: "x", ExtraCIDRs: []string{"y"}}.Validate()
	if err == nil {
		t.Fatal("a configuration with four problems was accepted")
	}
	for _, want := range []string{"podCIDR", "serviceCIDR", "joinCIDR", "extraCIDRs"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

func TestConfigNetworks(t *testing.T) {
	n := Config{PodCIDR: "10.244.0.0/16", ServiceCIDR: "10.96.0.0/16", JoinCIDR: "100.64.0.0/16", ExtraCIDRs: []string{"172.20.0.0/14"}}.Networks()
	if !slices.Equal(n.Pod, []string{"10.244.0.0/16"}) || !slices.Equal(n.Service, []string{"10.96.0.0/16"}) ||
		!slices.Equal(n.Join, []string{"100.64.0.0/16"}) || !slices.Equal(n.Cluster, []string{"172.20.0.0/14"}) {
		t.Errorf("Networks() = %+v", n)
	}
	if n := (Config{PodCIDR: "10.244.0.0/16", ServiceCIDR: "10.96.0.0/16"}).Networks(); len(n.Join) != 0 {
		t.Errorf("a join network that is not configured became %v", n.Join)
	}
}

func TestDestinationsKeepsWhatIsTheTenantsToReach(t *testing.T) {
	kept, dropped, err := Destinations([]string{"10.244.1.5", "10.244.1.6", "10.96.0.10", "100.64.0.4", "198.51.100.9"}, cluster())
	if err != nil {
		t.Fatal(err)
	}
	// Pods and services of the tenant are inside the networks of the cluster, so
	// those are not what is dropped. Neither is an address outside every one, as a
	// pod of a custom subnet has.
	want := []string{"10.244.1.5/32", "10.244.1.6/32", "10.96.0.10/32", "100.64.0.4/32", "198.51.100.9/32"}
	slices.Sort(want)
	if !slices.Equal(kept, want) || dropped != 0 {
		t.Errorf("kept %v and dropped %d, want %v and none", kept, dropped, want)
	}
}

func TestDestinationsDropsWhatIsNotTheTenantsToReach(t *testing.T) {
	for name, addr := range map[string]string{
		"a node":                  "192.168.100.5",
		"a load balancer":         "203.0.113.10",
		"a network of the others": "172.21.3.4",
		"the metadata address":    "169.254.169.254",
		"loopback":                "127.0.0.1",
		"IPv6":                    "2001:db8::5",
		"not an address":          "web-0",
		"a network":               "10.244.1.0/24",
		"empty":                   "",
	} {
		kept, dropped, err := Destinations([]string{addr, "10.244.1.5"}, cluster())
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(kept, []string{"10.244.1.5/32"}) || dropped != 1 {
			t.Errorf("%s: kept %v and dropped %d, want only the pod kept and one dropped", name, kept, dropped)
		}
	}
}

func TestDestinationsAreSortedAndUnique(t *testing.T) {
	kept, dropped, err := Destinations([]string{"10.244.1.9", "10.244.1.5", "10.244.1.9", "010.244.1.5"}, cluster())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(kept, []string{"10.244.1.5/32", "10.244.1.9/32"}) || dropped != 1 {
		t.Errorf("kept %v and dropped %d, want two in order and the badly written one dropped", kept, dropped)
	}
}

func TestDestinationsOfNothingIsNothing(t *testing.T) {
	kept, dropped, err := Destinations(nil, cluster())
	if err != nil || len(kept) != 0 || dropped != 0 {
		t.Errorf("got %v, %d, %v", kept, dropped, err)
	}
}

func TestDestinationsFailWhenANetworkOfTheClusterCannotBeRead(t *testing.T) {
	if _, _, err := Destinations([]string{"10.244.1.5"}, Networks{Nodes: []string{"nonsense"}}); err == nil {
		t.Error("an address was passed by a deny set that could not be read")
	}
}

func TestDestinationsDropsLinkLocalAndLoopbackWithNoConfiguration(t *testing.T) {
	kept, dropped, err := Destinations([]string{"169.254.1.1", "127.0.0.1", "10.244.1.5"}, Networks{})
	if err != nil || !slices.Equal(kept, []string{"10.244.1.5/32"}) || dropped != 2 {
		t.Errorf("got %v, %d, %v", kept, dropped, err)
	}
}
