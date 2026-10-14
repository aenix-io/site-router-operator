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

// Package routes keeps the routes kube-ovn gives the pods of a namespace: the
// annotation ovn.kubernetes.io/routes on the Namespace, which the kubeovn-webhook
// copies onto a pod when the pod is made. Every Forwarder of the namespace shares
// that one annotation, and people write to it too, so a Forwarder changes only
// the entries that are its own and leaves the others as they are.
//
// An entry is a Forwarder's when it routes to an address of the gateway of that
// Forwarder. The addresses each Forwarder routes to are written next to the
// routes, in the same patch, so that an entry stays known as the Forwarder's when
// the gateway comes back with another address.
package routes

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"time"
)

const (
	// Annotation is the annotation of a Namespace that holds its routes.
	Annotation = "ovn.kubernetes.io/routes"

	// OwnersAnnotation is the annotation of a Namespace that says, for each
	// Forwarder, the addresses of its gateway that entries of Annotation route to.
	// It is a JSON object from the name of the Forwarder to the addresses.
	OwnersAnnotation = "kube-ovn.siterouter.aenix.io/route-owners"
)

// Entry is one route of the annotation, in the form kube-ovn reads.
type Entry struct {
	Dst string `json:"dst,omitempty"`
	Gw  string `json:"gw,omitempty"`
}

// Claim is what a Forwarder of the namespace asks for.
type Claim struct {
	Name     string
	Created  time.Time
	CIDRs    []netip.Prefix
	Deleting bool
}

// Before says whether c was there first: the one made earlier, and of two made
// in the same second, the one whose name sorts first. Of two Forwarders that
// claim the same network, the one that was there first keeps it. The order does
// not depend on which of them was served first, so it is the same after the
// connector restarts.
func (c Claim) Before(o Claim) bool {
	if !c.Created.Equal(o.Created) {
		return c.Created.Before(o.Created)
	}
	return c.Name < o.Name
}

// Mode is what is done with the entries of a Forwarder.
type Mode int

const (
	// Withdraw takes every entry of the Forwarder out.
	Withdraw Mode = iota
	// Hold adds nothing and keeps the entries the Forwarder has for the networks
	// it still asks for. It is for a Forwarder whose gateway is not there to route
	// to for now: a pod made in the meantime gets the routes the gateway will be
	// back at, and none is taken out because the gateway restarts.
	Hold
	// Program routes every network of the Forwarder to its gateway.
	Program
)

// Request is everything Plan needs: the annotations as they are, the Forwarder,
// the others of its namespace, and the addresses of its gateway.
type Request struct {
	// Routes and Owners are the values of Annotation and OwnersAnnotation on the
	// Namespace, empty when there is none.
	Routes string
	Owners string

	Self Claim
	// Siblings are the other Forwarders of the namespace, those being deleted too.
	Siblings []Claim
	// Gateways are the addresses of the gateway of Self. The first is the next hop
	// of the routes. Program needs at least one.
	Gateways []netip.Addr
	Mode     Mode
}

// Conflict is a network of the Forwarder that it cannot have.
type Conflict struct {
	CIDR netip.Prefix
	// With is the Forwarder that holds the network, or empty for a route that is
	// not a Forwarder's.
	With string
}

// Result is what the annotations become.
type Result struct {
	// Routes and Owners are the new values. An empty one means that the
	// annotation is to be removed.
	Routes string
	Owners string
	// Changed is true when either differs from what was there.
	Changed bool
	// Conflicts are the networks of the Forwarder that another route holds. While
	// there is one, nothing is added for the Forwarder and nothing it has is taken
	// from it but what it no longer asks for.
	Conflicts []Conflict
	// Programmed is true when every network of the Forwarder is routed to the
	// next hop.
	Programmed bool
	// Mine are the entries of the Forwarder once the change is made.
	Mine []Entry
}

// entry is an entry of the annotation as it was written, and what it says.
type entry struct {
	raw json.RawMessage
	Entry
	dst   netip.Prefix // invalid when Dst is not a network
	gw    string       // Gw written the one way an address is
	owner string       // the Forwarder whose entry it is, or empty
	mine  bool
}

// Plan says what the annotations of the namespace become for one Forwarder. It
// does not read the cluster.
//
// An entry of another Forwarder is taken over when Self was there first and they
// overlap; one that is not a Forwarder's, or is the entry of a Forwarder that
// was there first, is a conflict and is left as it is. A default route claims no
// network in particular, and a more specific route wins over it, so it is no
// conflict.
func Plan(req Request) (Result, error) {
	entries, err := decode(req.Routes)
	if err != nil {
		return Result{}, err
	}
	owners, err := decodeOwners(req.Owners)
	if err != nil {
		return Result{}, err
	}

	mode := req.Mode
	if mode == Program && len(req.Gateways) == 0 {
		mode = Hold
	}
	var want []netip.Prefix
	if mode != Withdraw && !req.Self.Deleting {
		want = canonical(req.Self.CIDRs)
	}

	siblings := make(map[string]Claim, len(req.Siblings))
	for _, s := range req.Siblings {
		siblings[s.Name] = s
	}
	classify(entries, ownerKeys(req, owners))

	var conflicts []Conflict
	if mode != Withdraw {
		conflicts = arbitrate(req.Self, req.Siblings, siblings, entries, want)
	}
	// A Forwarder in conflict adds nothing, and gives up only what it no longer
	// asks for.
	program := mode == Program && len(conflicts) == 0
	var hop string
	if program {
		hop = req.Gateways[0].String()
	}
	out := rewrite(entries, want, hop, func(e entry) bool {
		return program && e.owner != "" && overlapsAny(e.dst, want) && req.Self.Before(siblings[e.owner])
	})

	res := Result{Conflicts: conflicts, Programmed: program && len(want) > 0}
	res.Mine = record(owners, req.Self.Name, out)
	if res.Routes, err = encode(out, len(entries) == 0, req.Routes); err != nil {
		return Result{}, err
	}
	if res.Owners, err = encodeOwners(owners); err != nil {
		return Result{}, err
	}
	res.Changed = !sameJSON(req.Routes, res.Routes) || !sameJSON(req.Owners, res.Owners)
	return res, nil
}

// classify marks the entries of Self, and those of the other Forwarders with
// their names.
func classify(entries []entry, k keys) {
	for i := range entries {
		e := &entries[i]
		if k.self[e.gw] {
			e.mine = true
			continue
		}
		if name, ok := k.others[e.gw]; ok {
			e.owner = name
		}
	}
}

// rewrite returns the entries the annotation is to have. An entry of Self stays
// while its network is asked for, once, and is routed to hop when there is one;
// an entry for which take is true is taken out; one for each network asked for
// that has none is added when there is a hop. The entries keep the order they
// had, and the added ones come last.
func rewrite(entries []entry, want []netip.Prefix, hop string, take func(entry) bool) []entry {
	out := make([]entry, 0, len(entries)+len(want))
	have := map[netip.Prefix]bool{}
	for _, e := range entries {
		switch {
		case e.mine:
			if !e.dst.IsValid() || !slices.Contains(want, e.dst) || have[e.dst] {
				continue
			}
			have[e.dst] = true
			if hop != "" && e.gw != hop {
				e.Gw, e.gw, e.raw = hop, hop, nil
			}
		case take(e):
			continue
		}
		out = append(out, e)
	}
	if hop == "" {
		return out
	}
	for _, w := range want {
		if !have[w] {
			out = append(out, entry{Dst: w.String(), Gw: hop, dst: w, gw: hop, mine: true})
		}
	}
	return out
}

// record writes in owners the addresses the entries of Self route to, and
// returns those entries.
func record(owners map[string][]string, self string, out []entry) []Entry {
	var mine []Entry
	var gws []string
	for _, e := range out {
		if e.mine {
			mine = append(mine, e.Entry)
			gws = append(gws, e.gw)
		}
	}
	slices.Sort(gws)
	if gws = slices.Compact(gws); len(gws) > 0 {
		owners[self] = gws
	} else {
		delete(owners, self)
	}
	return mine
}

// keys are the addresses that tell whose an entry is.
type keys struct {
	self   map[string]bool
	others map[string]string
}

// ownerKeys are the addresses the entries of each Forwarder route to: those the
// record says, and for Self the addresses its gateway has now as well, so that
// the entries of a Forwarder whose record was lost are still known as its own.
//
// The address of a pod that is gone goes back to the pool, and the gateway of
// another Forwarder of the namespace may get it while a record still names it.
// The one whose gateway has the address now is the one an entry with it routes
// to, so an address of the gateway of Self is Self's whatever another record
// says, and an address only Self's record names is Self's only when no other
// record names it. A record of a Forwarder that is not there says nothing: its
// entries are nobody's.
func ownerKeys(req Request, owners map[string][]string) keys {
	k := keys{self: map[string]bool{}, others: map[string]string{}}
	for _, a := range req.Gateways {
		k.self[a.String()] = true
	}
	exists := map[string]bool{}
	for _, s := range req.Siblings {
		exists[s.Name] = true
	}
	for name, addrs := range owners {
		if name == req.Self.Name || !exists[name] {
			continue
		}
		for _, a := range addrs {
			if !k.self[a] {
				k.others[a] = name
			}
		}
	}
	for _, a := range owners[req.Self.Name] {
		if _, theirs := k.others[a]; !theirs {
			k.self[a] = true
		}
	}
	return k
}

// arbitrate finds the networks of Self that it cannot have: those that a
// Forwarder that was there first asks for, and those that a route that Self
// cannot take holds.
func arbitrate(self Claim, siblings []Claim, byName map[string]Claim, entries []entry, want []netip.Prefix) []Conflict {
	var out []Conflict
	for _, w := range want {
		if c, ok := conflictOf(self, siblings, byName, entries, w); ok {
			out = append(out, c)
		}
	}
	return out
}

func conflictOf(self Claim, siblings []Claim, byName map[string]Claim, entries []entry, w netip.Prefix) (Conflict, bool) {
	for _, s := range siblings {
		if s.Deleting || !s.Before(self) {
			continue
		}
		if overlapsAny(w, canonical(s.CIDRs)) {
			return Conflict{CIDR: w, With: s.Name}, true
		}
	}
	for _, e := range entries {
		if e.mine || !e.dst.IsValid() || e.dst.Bits() == 0 || !e.dst.Overlaps(w) {
			continue
		}
		if e.owner == "" {
			return Conflict{CIDR: w}, true
		}
		if !self.Before(byName[e.owner]) {
			return Conflict{CIDR: w, With: e.owner}, true
		}
	}
	return Conflict{}, false
}

func overlapsAny(p netip.Prefix, set []netip.Prefix) bool {
	if !p.IsValid() {
		return false
	}
	return slices.ContainsFunc(set, p.Overlaps)
}

// canonical returns the IPv4 networks of cidrs with their host bits cleared,
// sorted and without repeats.
func canonical(cidrs []netip.Prefix) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		if c.IsValid() && c.Addr().Is4() {
			out = append(out, c.Masked())
		}
	}
	slices.SortFunc(out, comparePrefix)
	return slices.Compact(out)
}

func comparePrefix(a, b netip.Prefix) int {
	if c := a.Addr().Compare(b.Addr()); c != 0 {
		return c
	}
	return cmp.Compare(a.Bits(), b.Bits())
}

// Covers says whether the routes a pod was made with, the value of Annotation on
// the pod, hold every entry of want. A pod that does not have them was made
// before they were in place: kube-ovn applies the routes of a pod only when the
// pod is made.
func Covers(podRoutes string, want []Entry) (bool, error) {
	entries, err := decode(podRoutes)
	if err != nil {
		return false, err
	}
	have := make(map[Entry]bool, len(entries))
	for _, e := range entries {
		have[canonicalEntry(e)] = true
	}
	for _, w := range want {
		e := entry{Entry: w}
		if p, err := netip.ParsePrefix(w.Dst); err == nil {
			e.dst = p.Masked()
		}
		if !have[canonicalEntry(e)] {
			return false, nil
		}
	}
	return true, nil
}

// canonicalEntry writes the network and the address of an entry the one way
// they can be written, so that two entries that say the same compare equal.
func canonicalEntry(e entry) Entry {
	out := Entry{Dst: e.Dst, Gw: e.Gw}
	if e.dst.IsValid() {
		out.Dst = e.dst.String()
	}
	if a, err := netip.ParseAddr(e.Gw); err == nil {
		out.Gw = a.String()
	}
	return out
}

// decode parses a value of Annotation. An empty value has no entries.
func decode(value string) ([]entry, error) {
	if value == "" {
		return nil, nil
	}
	var raws []json.RawMessage
	if err := json.Unmarshal([]byte(value), &raws); err != nil {
		return nil, fmt.Errorf("the annotation %s is not a list of routes: %w", Annotation, err)
	}
	out := make([]entry, 0, len(raws))
	for _, raw := range raws {
		e := entry{raw: raw}
		if err := json.Unmarshal(raw, &e.Entry); err != nil {
			return nil, fmt.Errorf("the annotation %s has a route that is not one: %w", Annotation, err)
		}
		if e.Dst == "" {
			// kube-ovn takes a route with no destination for the default route.
			e.dst = netip.PrefixFrom(netip.IPv4Unspecified(), 0)
		} else if p, err := netip.ParsePrefix(e.Dst); err == nil {
			e.dst = p.Masked()
		}
		e.gw = e.Gw
		if a, err := netip.ParseAddr(e.Gw); err == nil {
			e.gw = a.String()
		}
		out = append(out, e)
	}
	return out, nil
}

// encode renders entries in the order they have. An entry that was not changed
// is written as it was, so a route somebody else wrote keeps what kube-ovn does
// not read as well.
//
// When the last entry goes, so does the annotation: while a Namespace has none,
// the webhook gives its pods the routes it is configured with for the whole
// cluster, and an empty list left behind would take them away. A value with no
// entries that was there before is left as it was, since nothing was taken out
// of it.
func encode(entries []entry, wasEmpty bool, before string) (string, error) {
	if len(entries) == 0 {
		if wasEmpty {
			return before, nil
		}
		return "", nil
	}
	raws := make([]json.RawMessage, len(entries))
	for i, e := range entries {
		if e.raw != nil {
			raws[i] = e.raw
			continue
		}
		b, err := json.Marshal(e.Entry)
		if err != nil {
			return "", err
		}
		raws[i] = b
	}
	b, err := json.Marshal(raws)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func decodeOwners(value string) (map[string][]string, error) {
	owners := map[string][]string{}
	if value == "" {
		return owners, nil
	}
	if err := json.Unmarshal([]byte(value), &owners); err != nil {
		return nil, fmt.Errorf("the annotation %s is not a map of Forwarders to addresses: %w", OwnersAnnotation, err)
	}
	return owners, nil
}

func encodeOwners(owners map[string][]string) (string, error) {
	if len(owners) == 0 {
		return "", nil
	}
	b, err := json.Marshal(owners)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// sameJSON says whether two values say the same, ignoring the spaces.
func sameJSON(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	var ca, cb bytes.Buffer
	if json.Compact(&ca, []byte(a)) != nil || json.Compact(&cb, []byte(b)) != nil {
		return a == b
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}
