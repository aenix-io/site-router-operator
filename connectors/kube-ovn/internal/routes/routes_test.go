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

package routes

import (
	"encoding/json"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	gwA = "10.244.0.5" // the gateway of the Forwarder under test
	gwB = "10.244.0.9" // the gateway of a sibling
	gwH = "10.244.0.77"
)

var (
	t0 = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Hour)
)

func prefixes(cidrs ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(cidrs))
	for i, c := range cidrs {
		out[i] = netip.MustParsePrefix(c)
	}
	return out
}

func addrs(as ...string) []netip.Addr {
	out := make([]netip.Addr, len(as))
	for i, a := range as {
		out[i] = netip.MustParseAddr(a)
	}
	return out
}

func self(cidrs ...string) Claim {
	return Claim{Name: "a", Created: t0, CIDRs: prefixes(cidrs...)}
}

// routeSet reads a value of the annotation into a map from destination to
// gateway, so that tests do not depend on the order of the entries.
func routeSet(t *testing.T, value string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if value == "" {
		return out
	}
	var entries []Entry
	if err := json.Unmarshal([]byte(value), &entries); err != nil {
		t.Fatalf("routes %q are not JSON: %v", value, err)
	}
	for _, e := range entries {
		if _, dup := out[e.Dst]; dup {
			t.Fatalf("routes %q have %s twice", value, e.Dst)
		}
		out[e.Dst] = e.Gw
	}
	return out
}

func ownerSet(t *testing.T, value string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	if value == "" {
		return out
	}
	if err := json.Unmarshal([]byte(value), &out); err != nil {
		t.Fatalf("owners %q are not JSON: %v", value, err)
	}
	return out
}

func plan(t *testing.T, req Request) Result {
	t.Helper()
	res, err := Plan(req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return res
}

func TestProgramWritesOneEntryPerNetwork(t *testing.T) {
	res := plan(t, Request{Self: self("172.31.0.0/16", "10.10.0.0/16"), Gateways: addrs(gwA), Mode: Program})
	want := map[string]string{"172.31.0.0/16": gwA, "10.10.0.0/16": gwA}
	if got := routeSet(t, res.Routes); !maps.Equal(got, want) {
		t.Errorf("routes = %v, want %v", got, want)
	}
	if got := ownerSet(t, res.Owners); !slices.Equal(got["a"], []string{gwA}) || len(got) != 1 {
		t.Errorf("owners = %v, want a: [%s]", got, gwA)
	}
	if !res.Changed || !res.Programmed || len(res.Conflicts) != 0 {
		t.Errorf("changed, programmed, conflicts = %v, %v, %v; want true, true, none", res.Changed, res.Programmed, res.Conflicts)
	}
}

// The routes of the Forwarders of a namespace are merged by destination, not
// replaced.
func TestProgramKeepsTheEntriesOfOthers(t *testing.T) {
	existing := `[{"dst":"10.10.0.0/16","gw":"` + gwB + `"}]`
	res := plan(t, Request{
		Routes:   existing,
		Owners:   `{"b":["` + gwB + `"]}`,
		Self:     self("172.31.0.0/16"),
		Siblings: []Claim{{Name: "b", Created: t1, CIDRs: prefixes("10.10.0.0/16")}},
		Gateways: addrs(gwA),
		Mode:     Program,
	})
	want := map[string]string{"10.10.0.0/16": gwB, "172.31.0.0/16": gwA}
	if got := routeSet(t, res.Routes); !maps.Equal(got, want) {
		t.Errorf("routes = %v, want %v", got, want)
	}
	if got := ownerSet(t, res.Owners); !slices.Equal(got["b"], []string{gwB}) || !slices.Equal(got["a"], []string{gwA}) {
		t.Errorf("owners = %v, want the record of b kept and a added", got)
	}
}

// A gateway that comes back with another address takes its entries with it: the
// record names the old address, so the entries are known as its own.
func TestProgramMovesTheEntriesToANewAddress(t *testing.T) {
	existing := `[{"dst":"172.31.0.0/16","gw":"` + gwA + `"},{"dst":"10.10.0.0/16","gw":"` + gwA + `"}]`
	res := plan(t, Request{
		Routes:   existing,
		Owners:   `{"a":["` + gwA + `"]}`,
		Self:     self("172.31.0.0/16"),
		Gateways: addrs("10.244.0.7"),
		Mode:     Program,
	})
	want := map[string]string{"172.31.0.0/16": "10.244.0.7"}
	if got := routeSet(t, res.Routes); !maps.Equal(got, want) {
		t.Errorf("routes = %v, want %v", got, want)
	}
	if got := ownerSet(t, res.Owners); !slices.Equal(got["a"], []string{"10.244.0.7"}) {
		t.Errorf("owners = %v, want only the new address", got)
	}
}

// A route that is not a Forwarder's is not taken: the Forwarder that wants the
// same network is in conflict and nothing changes.
func TestARouteOfSomebodyElseIsAConflict(t *testing.T) {
	for _, existing := range []string{
		`[{"dst":"172.31.0.0/16","gw":"` + gwH + `"}]`, // the same network
		`[{"dst":"172.31.4.0/24","gw":"` + gwH + `"}]`, // a part of it
		`[{"dst":"172.0.0.0/8","gw":"` + gwH + `"}]`,   // a network it is a part of
	} {
		res := plan(t, Request{Routes: existing, Self: self("172.31.0.0/16"), Gateways: addrs(gwA), Mode: Program})
		if len(res.Conflicts) != 1 || res.Conflicts[0].CIDR.String() != "172.31.0.0/16" || res.Conflicts[0].With != "" {
			t.Errorf("%s: conflicts = %v, want 172.31.0.0/16 with a route that is nobody's", existing, res.Conflicts)
		}
		if res.Changed || res.Programmed {
			t.Errorf("%s: changed, programmed = %v, %v; a conflict changes nothing", existing, res.Changed, res.Programmed)
		}
	}
}

// A default route claims no network in particular, and a route to a network
// wins over it.
func TestADefaultRouteIsNoConflict(t *testing.T) {
	for _, existing := range []string{
		`[{"dst":"0.0.0.0/0","gw":"` + gwH + `"}]`,
		`[{"gw":"` + gwH + `"}]`,
	} {
		res := plan(t, Request{Routes: existing, Self: self("172.31.0.0/16"), Gateways: addrs(gwA), Mode: Program})
		if len(res.Conflicts) != 0 || !res.Programmed {
			t.Errorf("%s: conflicts, programmed = %v, %v; want none, true", existing, res.Conflicts, res.Programmed)
		}
		if got := routeSet(t, res.Routes); len(got) != 2 {
			t.Errorf("%s: routes = %v, want the default route and ours", existing, got)
		}
	}
}

func TestPlanIsStable(t *testing.T) {
	req := Request{Self: self("172.31.0.0/16", "10.10.0.0/16"), Gateways: addrs(gwA), Mode: Program}
	first := plan(t, req)
	req.Routes, req.Owners = first.Routes, first.Owners
	second := plan(t, req)
	if second.Routes != first.Routes || second.Owners != first.Owners || second.Changed {
		t.Errorf("a second pass changed %q/%q into %q/%q (changed %v)", first.Routes, first.Owners, second.Routes, second.Owners, second.Changed)
	}
}

// Writing the same routes with other spaces is not a change, so the namespace is
// not patched for nothing.
func TestSpacesAreNoChange(t *testing.T) {
	res := plan(t, Request{
		Routes:   `[ {"dst": "172.31.0.0/16", "gw": "` + gwA + `"} ]`,
		Owners:   `{ "a": ["` + gwA + `"] }`,
		Self:     self("172.31.0.0/16"),
		Gateways: addrs(gwA),
		Mode:     Program,
	})
	if res.Changed {
		t.Errorf("routes %q and owners %q were taken for a change", res.Routes, res.Owners)
	}
}

func TestWithdrawTakesOutOnlyItsOwnEntries(t *testing.T) {
	existing := `[{"dst":"172.31.0.0/16","gw":"` + gwA + `"},{"dst":"10.10.0.0/16","gw":"` + gwB + `"},{"dst":"192.0.2.0/24","gw":"` + gwA + `"}]`
	res := plan(t, Request{
		Routes:   existing,
		Owners:   `{"a":["` + gwA + `"],"b":["` + gwB + `"]}`,
		Self:     self("172.31.0.0/16"),
		Siblings: []Claim{{Name: "b", Created: t1, CIDRs: prefixes("10.10.0.0/16")}},
		Gateways: addrs(gwA),
		Mode:     Withdraw,
	})
	// 192.0.2.0/24 is no longer asked for and is still taken out: the entry
	// routes to the gateway, which makes it the Forwarder's.
	want := map[string]string{"10.10.0.0/16": gwB}
	if got := routeSet(t, res.Routes); !maps.Equal(got, want) {
		t.Errorf("routes = %v, want %v", got, want)
	}
	if got := ownerSet(t, res.Owners); len(got) != 1 || !slices.Equal(got["b"], []string{gwB}) {
		t.Errorf("owners = %v, want only b", got)
	}
	if res.Programmed {
		t.Error("a withdrawn Forwarder is not programmed")
	}
}

// When the gateway is gone and the record still names its address, the
// withdrawal finds the entries by the record.
func TestWithdrawFindsTheEntriesOfAGatewayThatIsGone(t *testing.T) {
	existing := `[{"dst":"172.31.0.0/16","gw":"` + gwA + `"},{"dst":"10.10.0.0/16","gw":"10.244.0.7"},{"dst":"192.0.2.0/24","gw":"` + gwB + `"}]`
	res := plan(t, Request{
		Routes: existing,
		Owners: `{"a":["` + gwA + `","10.244.0.7"]}`,
		Self:   self("172.31.0.0/16"),
		Mode:   Withdraw,
	})
	want := map[string]string{"192.0.2.0/24": gwB}
	if got := routeSet(t, res.Routes); !maps.Equal(got, want) {
		t.Errorf("routes = %v, want %v", got, want)
	}
	if res.Owners != "" {
		t.Errorf("owners = %q, want the annotation gone", res.Owners)
	}
}

// A sibling that took a network over has an entry with that destination and its
// own gateway; a withdrawal goes by the gateway and leaves it.
func TestWithdrawLeavesTheSameNetworkOfASibling(t *testing.T) {
	existing := `[{"dst":"172.31.0.0/16","gw":"` + gwB + `"}]`
	res := plan(t, Request{
		Routes:   existing,
		Owners:   `{"b":["` + gwB + `"]}`,
		Self:     self("172.31.0.0/16"),
		Siblings: []Claim{{Name: "b", Created: t1, CIDRs: prefixes("172.31.0.0/16")}},
		Gateways: addrs(gwA),
		Mode:     Withdraw,
	})
	if res.Changed || res.Routes != existing {
		t.Errorf("routes = %q (changed %v), want them as they were", res.Routes, res.Changed)
	}
}

// With no address to go by, nothing is guessed: an entry that has the same
// destination may be somebody else's.
func TestWithdrawWithNoAddressChangesNothing(t *testing.T) {
	existing := `[{"dst":"172.31.0.0/16","gw":"` + gwA + `"},{"dst":"192.0.2.0/24","gw":"` + gwB + `"}]`
	res := plan(t, Request{Routes: existing, Self: self("172.31.0.0/16"), Mode: Withdraw})
	if res.Changed || res.Routes != existing {
		t.Errorf("routes = %q (changed %v), want them as they were", res.Routes, res.Changed)
	}
}

func TestProgramTakesOutTheNetworksNoLongerAskedFor(t *testing.T) {
	existing := `[{"dst":"10.10.0.0/16","gw":"` + gwA + `"},{"dst":"172.31.0.0/16","gw":"` + gwA + `"},{"dst":"192.0.2.0/24","gw":"` + gwB + `"}]`
	res := plan(t, Request{
		Routes:   existing,
		Owners:   `{"a":["` + gwA + `"],"b":["` + gwB + `"]}`,
		Self:     self("10.10.0.0/16"),
		Siblings: []Claim{{Name: "b", Created: t1, CIDRs: prefixes("192.0.2.0/24")}},
		Gateways: addrs(gwA),
		Mode:     Program,
	})
	want := map[string]string{"10.10.0.0/16": gwA, "192.0.2.0/24": gwB}
	if got := routeSet(t, res.Routes); !maps.Equal(got, want) {
		t.Errorf("routes = %v, want %v", got, want)
	}
}

// The last entry takes the annotation with it, so that the routes the webhook is
// configured with for the whole cluster apply again.
func TestTheLastEntryTakesTheAnnotationWithIt(t *testing.T) {
	res := plan(t, Request{
		Routes:   `[{"dst":"172.31.0.0/16","gw":"` + gwA + `"}]`,
		Owners:   `{"a":["` + gwA + `"]}`,
		Self:     self("172.31.0.0/16"),
		Gateways: addrs(gwA),
		Mode:     Withdraw,
	})
	if res.Routes != "" || res.Owners != "" || !res.Changed {
		t.Errorf("routes, owners = %q, %q (changed %v); want both gone", res.Routes, res.Owners, res.Changed)
	}
}

// An empty list that somebody wrote is theirs, and a Forwarder with nothing to
// do leaves it.
func TestAnEmptyListOfSomebodyElseIsLeft(t *testing.T) {
	res := plan(t, Request{Routes: "[]", Self: self("172.31.0.0/16"), Gateways: addrs(gwA), Mode: Withdraw})
	if res.Routes != "[]" || res.Changed {
		t.Errorf("routes = %q (changed %v), want [] as it was", res.Routes, res.Changed)
	}
}

// A route somebody else wrote is written back as it was, with what kube-ovn does
// not read.
func TestARouteOfSomebodyElseIsWrittenBackAsItWas(t *testing.T) {
	theirs := `{"gw":"` + gwH + `","dst":"192.0.2.0/24","note":"by hand"}`
	res := plan(t, Request{Routes: `[` + theirs + `]`, Self: self("172.31.0.0/16"), Gateways: addrs(gwA), Mode: Program})
	if !strings.Contains(res.Routes, theirs) {
		t.Errorf("routes = %q, want %s in it as it was", res.Routes, theirs)
	}
}

// Of two Forwarders that claim the same network, the one that was there first
// keeps it, whichever of them programmed it first.
func TestTheForwarderThatWasThereFirstWins(t *testing.T) {
	older := Claim{Name: "a", Created: t0, CIDRs: prefixes("172.31.0.0/16")}
	younger := Claim{Name: "b", Created: t1, CIDRs: prefixes("172.31.0.0/25")}
	// The younger one got there first and holds a part of the network.
	existing := `[{"dst":"172.31.0.0/25","gw":"` + gwB + `"},{"dst":"10.10.0.0/16","gw":"` + gwB + `"}]`
	owners := `{"b":["` + gwB + `"]}`

	res := plan(t, Request{Routes: existing, Owners: owners, Self: older, Siblings: []Claim{younger}, Gateways: addrs(gwA), Mode: Program})
	if len(res.Conflicts) != 0 || !res.Programmed {
		t.Fatalf("the older one: conflicts %v, programmed %v; want none, true", res.Conflicts, res.Programmed)
	}
	// It takes the overlapping entry over, and the other entry of the younger one
	// stays.
	want := map[string]string{"172.31.0.0/16": gwA, "10.10.0.0/16": gwB}
	if got := routeSet(t, res.Routes); !maps.Equal(got, want) {
		t.Errorf("routes = %v, want %v", got, want)
	}

	// The younger one is in conflict with it by what they ask for, and keeps what
	// it has that is not taken.
	younger.CIDRs = prefixes("172.31.0.0/25", "10.10.0.0/16")
	res2 := plan(t, Request{Routes: res.Routes, Owners: res.Owners, Self: younger, Siblings: []Claim{older}, Gateways: addrs(gwB), Mode: Program})
	if len(res2.Conflicts) != 1 || res2.Conflicts[0].With != "a" || res2.Conflicts[0].CIDR.String() != "172.31.0.0/25" {
		t.Errorf("the younger one: conflicts = %v, want 172.31.0.0/25 with a", res2.Conflicts)
	}
	if res2.Changed || res2.Programmed {
		t.Errorf("the younger one: changed, programmed = %v, %v; want false, false", res2.Changed, res2.Programmed)
	}
}

// A Forwarder in conflict adds nothing, and gives up only what it no longer asks
// for.
func TestAForwarderInConflictKeepsWhatItHas(t *testing.T) {
	existing := `[{"dst":"10.10.0.0/16","gw":"` + gwB + `"},{"dst":"10.20.0.0/16","gw":"` + gwB + `"},{"dst":"172.31.0.0/16","gw":"` + gwH + `"}]`
	res := plan(t, Request{
		Routes:   existing,
		Owners:   `{"b":["` + gwB + `"]}`,
		Self:     Claim{Name: "b", Created: t1, CIDRs: prefixes("10.10.0.0/16", "172.31.0.0/16", "192.0.2.0/24")},
		Gateways: addrs(gwB),
		Mode:     Program,
	})
	if len(res.Conflicts) != 1 || res.Conflicts[0].CIDR.String() != "172.31.0.0/16" {
		t.Fatalf("conflicts = %v, want 172.31.0.0/16", res.Conflicts)
	}
	want := map[string]string{"10.10.0.0/16": gwB, "172.31.0.0/16": gwH}
	if got := routeSet(t, res.Routes); !maps.Equal(got, want) {
		t.Errorf("routes = %v, want %v: 10.20 is no longer asked for, 192.0.2 is not added", got, want)
	}
}

// A Forwarder being deleted asks for nothing, so it is in conflict with nobody,
// but the entries it still has are its own until it takes them out.
func TestAForwarderBeingDeletedHoldsItsEntriesUntilItGoes(t *testing.T) {
	deleting := Claim{Name: "a", Created: t0, CIDRs: prefixes("172.31.0.0/16"), Deleting: true}
	res := plan(t, Request{
		Routes:   `[{"dst":"172.31.0.0/16","gw":"` + gwA + `"}]`,
		Owners:   `{"a":["` + gwA + `"]}`,
		Self:     Claim{Name: "b", Created: t1, CIDRs: prefixes("172.31.0.0/16")},
		Siblings: []Claim{deleting},
		Gateways: addrs(gwB),
		Mode:     Program,
	})
	if len(res.Conflicts) != 1 || res.Conflicts[0].With != "a" || res.Changed {
		t.Errorf("conflicts = %v, changed %v; want one with a and no change", res.Conflicts, res.Changed)
	}

	// Once it has gone, the network is free.
	res = plan(t, Request{
		Routes:   `[]`,
		Self:     Claim{Name: "b", Created: t1, CIDRs: prefixes("172.31.0.0/16")},
		Siblings: []Claim{deleting},
		Gateways: addrs(gwB),
		Mode:     Program,
	})
	if len(res.Conflicts) != 0 || !res.Programmed {
		t.Errorf("conflicts, programmed = %v, %v; want none, true", res.Conflicts, res.Programmed)
	}
}

// The entries of a Forwarder that is not there, which nobody took out, are
// nobody's: they are in the way of another until somebody takes them out.
func TestTheEntriesOfAForwarderThatIsGoneAreNobodys(t *testing.T) {
	res := plan(t, Request{
		Routes:   `[{"dst":"172.31.0.0/16","gw":"` + gwB + `"}]`,
		Owners:   `{"gone":["` + gwB + `"]}`,
		Self:     self("172.31.0.0/16"),
		Gateways: addrs(gwA),
		Mode:     Program,
	})
	if len(res.Conflicts) != 1 || res.Conflicts[0].With != "" || res.Changed {
		t.Errorf("conflicts = %v, changed %v; want one with nobody and no change", res.Conflicts, res.Changed)
	}
}

// The gateway of Self has an address that the record of a sibling still names:
// the pod of the sibling went away and the address was given again. The entries
// with it route to the gateway of Self, and are Self's; an address only Self's
// record names, which the sibling's names too, is the sibling's.
func TestTheGatewayThatHasAnAddressNowOwnsIt(t *testing.T) {
	res := plan(t, Request{
		Routes:   `[{"dst":"10.10.0.0/16","gw":"` + gwA + `"},{"dst":"10.20.0.0/16","gw":"` + gwB + `"}]`,
		Owners:   `{"a":["` + gwB + `"],"b":["` + gwA + `","` + gwB + `"]}`,
		Self:     self("172.31.0.0/16"),
		Siblings: []Claim{{Name: "b", Created: t1, CIDRs: prefixes("10.20.0.0/16")}},
		Gateways: addrs(gwA),
		Mode:     Program,
	})
	want := map[string]string{"10.20.0.0/16": gwB, "172.31.0.0/16": gwA}
	if got := routeSet(t, res.Routes); !maps.Equal(got, want) {
		t.Errorf("routes = %v, want %v", got, want)
	}
	if got := ownerSet(t, res.Owners); !slices.Equal(got["a"], []string{gwA}) {
		t.Errorf("owners = %v, want a: [%s]", got, gwA)
	}
}

// With its gateway away, a Forwarder keeps the entries it has for the networks
// it asks for, adds none and takes out those it no longer asks for.
func TestHoldKeepsWhatIsAskedForAndAddsNothing(t *testing.T) {
	res := plan(t, Request{
		Routes: `[{"dst":"172.31.0.0/16","gw":"` + gwA + `"},{"dst":"10.10.0.0/16","gw":"` + gwA + `"}]`,
		Owners: `{"a":["` + gwA + `"]}`,
		Self:   self("172.31.0.0/16", "192.0.2.0/24"),
		Mode:   Program, // with no address, Program holds
	})
	want := map[string]string{"172.31.0.0/16": gwA}
	if got := routeSet(t, res.Routes); !maps.Equal(got, want) {
		t.Errorf("routes = %v, want %v", got, want)
	}
	if res.Programmed {
		t.Error("held routes are not programmed")
	}
}

func TestAnAnnotationThatIsNotRoutesIsAnError(t *testing.T) {
	for _, req := range []Request{
		{Routes: `{"dst":"x"}`},
		{Routes: `[1]`},
		{Routes: `not json`},
		{Owners: `["a"]`},
	} {
		req.Self, req.Gateways, req.Mode = self("172.31.0.0/16"), addrs(gwA), Program
		if _, err := Plan(req); err == nil {
			t.Errorf("Plan(%q, %q) took it", req.Routes, req.Owners)
		}
	}
}

func TestBefore(t *testing.T) {
	a := Claim{Name: "a", Created: t0}
	b := Claim{Name: "b", Created: t0}
	c := Claim{Name: "0", Created: t1}
	if !a.Before(b) || b.Before(a) {
		t.Error("of two made at once, the one whose name sorts first is first")
	}
	if !b.Before(c) || c.Before(b) {
		t.Error("the one made earlier is first")
	}
	if a.Before(a) {
		t.Error("a Forwarder is not before itself")
	}
}

func TestCovers(t *testing.T) {
	want := []Entry{{Dst: "172.31.0.0/16", Gw: gwA}}
	for _, tc := range []struct {
		pod  string
		want bool
	}{
		{"", false},
		{`[{"dst":"172.31.0.0/16","gw":"` + gwA + `"}]`, true},
		{`[{"dst":"10.0.0.0/8","gw":"` + gwB + `"},{"dst":"172.31.0.0/16","gw":"` + gwA + `"}]`, true},
		// The route to the gateway at the address it had before is not the route.
		{`[{"dst":"172.31.0.0/16","gw":"10.244.0.7"}]`, false},
		{`[{"dst":"172.31.0.0/24","gw":"` + gwA + `"}]`, false},
	} {
		got, err := Covers(tc.pod, want)
		if err != nil || got != tc.want {
			t.Errorf("Covers(%q) = %v, %v; want %v", tc.pod, got, err, tc.want)
		}
	}
	if _, err := Covers("nonsense", want); err == nil {
		t.Error("Covers took a value that is not routes")
	}
}
