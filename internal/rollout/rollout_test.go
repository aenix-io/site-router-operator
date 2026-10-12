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

package rollout

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/aenix-io/site-router-operator/internal/config"
)

const (
	img11 = "registry.example/disk:1.1@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	img10 = "registry.example/disk:1.0@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	img09 = "registry.example/disk:0.9@sha256:00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
)

func cfg(policy config.UpgradePolicy) config.Config {
	return config.Config{
		Engine: config.Engine{
			Name:   "appliance",
			Latest: "1.1",
			Versions: []config.Version{
				{Name: "1.1", Image: img11},
				{Name: "1.0", Image: img10},
				{Name: "0.9", Image: img09},
			},
		},
		Upgrade: config.Upgrade{Policy: policy, MaxConcurrent: 2},
	}
}

func TestDecide(t *testing.T) {
	cases := []struct {
		name       string
		policy     config.UpgradePolicy
		current    string
		annotation string
		want       Decision
	}{
		// A gateway that is being made.
		{"new, no annotation, auto", config.UpgradeAuto, "", "", Decision{Target: "1.1", Image: img11}},
		{"new, no annotation, annotation policy", config.UpgradeAnnotation, "", "", Decision{Target: "1.1", Image: img11}},
		{"new, with an annotation", config.UpgradeAnnotation, "", "1.0", Decision{Target: "1.0", Image: img10}},
		{"new, with an annotation, auto", config.UpgradeAuto, "", "0.9", Decision{Target: "0.9", Image: img09}},
		{"new, with an annotation that is not listed", config.UpgradeAnnotation, "", "9.9", Decision{Target: "1.1", Image: img11, Unsupported: "9.9"}},

		// Annotation policy: nothing moves unless told to.
		{"annotation policy leaves a gateway where it is", config.UpgradeAnnotation, "1.0", "", Decision{Target: "1.0", Image: img10}},
		{"annotation policy: up", config.UpgradeAnnotation, "1.0", "1.1", Decision{Target: "1.1", Image: img11, Moving: true}},
		{"annotation policy: down", config.UpgradeAnnotation, "1.1", "0.9", Decision{Target: "0.9", Image: img09, Moving: true}},
		{"annotation policy: the same version", config.UpgradeAnnotation, "1.0", "1.0", Decision{Target: "1.0", Image: img10}},
		{"annotation policy: a version that is not listed", config.UpgradeAnnotation, "1.0", "9.9", Decision{Target: "1.0", Image: img10, Unsupported: "9.9"}},

		// Auto policy: every gateway goes to the latest, and an annotation keeps one
		// where it is told.
		{"auto policy moves a gateway to the latest", config.UpgradeAuto, "1.0", "", Decision{Target: "1.1", Image: img11, Moving: true}},
		{"auto policy has nothing to do at the latest", config.UpgradeAuto, "1.1", "", Decision{Target: "1.1", Image: img11}},
		{"auto policy: an annotation holds a gateway back", config.UpgradeAuto, "1.1", "1.0", Decision{Target: "1.0", Image: img10, Moving: true}},
		{"auto policy: an annotation keeps a gateway where it is", config.UpgradeAuto, "1.0", "1.0", Decision{Target: "1.0", Image: img10}},
		{"auto policy: a version that is not listed does not move it", config.UpgradeAuto, "1.0", "9.9", Decision{Target: "1.0", Image: img10, Unsupported: "9.9"}},

		// A version that is not listed any more.
		{"orphaned, annotation policy", config.UpgradeAnnotation, "0.5", "", Decision{Target: "0.5", Orphaned: true}},
		{"orphaned, auto policy goes to the latest", config.UpgradeAuto, "0.5", "", Decision{Target: "1.1", Image: img11, Moving: true}},
		{"orphaned, told to move", config.UpgradeAnnotation, "0.5", "1.0", Decision{Target: "1.0", Image: img10, Moving: true}},
		{"orphaned, told to move to what is not listed", config.UpgradeAnnotation, "0.5", "9.9", Decision{Target: "0.5", Orphaned: true, Unsupported: "9.9"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Decide(cfg(c.policy), c.current, c.annotation)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("Decide(%q, %q) = %+v, want %+v", c.current, c.annotation, got, c.want)
			}
		})
	}
}

func TestDecideRefusesAConfigurationThatWasNotChecked(t *testing.T) {
	c := cfg(config.UpgradeAuto)
	c.Engine.Latest = "7.7"
	if _, err := Decide(c, "1.0", ""); err == nil {
		t.Error("a decision was made with a latest version that is not listed")
	}
}

var (
	a = types.NamespacedName{Namespace: "ns", Name: "a"}
	b = types.NamespacedName{Namespace: "ns", Name: "b"}
	c = types.NamespacedName{Namespace: "ns", Name: "c"}
)

func TestGateLetsMaxGatewaysMoveAtOnce(t *testing.T) {
	g := NewGate(2)
	if !g.Acquire(a) || !g.Acquire(b) {
		t.Fatal("the first two were turned away")
	}
	if g.Acquire(c) {
		t.Error("a third gateway was let move with a limit of two")
	}
	if g.Moving() != 2 {
		t.Errorf("Moving() = %d, want 2", g.Moving())
	}

	g.Release(a)
	if !g.Acquire(c) {
		t.Error("a gateway was turned away after another had finished")
	}
	if g.Acquire(a) {
		t.Error("the one that finished took a third place")
	}
}

func TestAGatewayThatIsMovingStaysSo(t *testing.T) {
	g := NewGate(1)
	if !g.Acquire(a) {
		t.Fatal("turned away")
	}
	for range 3 {
		if !g.Acquire(a) {
			t.Fatal("a gateway that is moving was turned away on its next pass")
		}
	}
	if g.Moving() != 1 {
		t.Errorf("Moving() = %d after the same gateway asked four times, want 1", g.Moving())
	}
}

func TestReleaseOfAGatewayThatIsNotMovingIsHarmless(t *testing.T) {
	g := NewGate(1)
	g.Release(a)
	if !g.Acquire(a) || g.Moving() != 1 {
		t.Error("the gate is not as it was")
	}
}

func TestRestoredGatewaysCountAndAreNotTurnedAwayEvenOverTheLimit(t *testing.T) {
	g := NewGate(1)
	g.Restore([]types.NamespacedName{a, b})
	if g.Moving() != 2 {
		t.Errorf("Moving() = %d, want the two that were moving", g.Moving())
	}
	if !g.Acquire(a) || !g.Acquire(b) {
		t.Error("a move that was under way was stopped by a lower limit")
	}
	if g.Acquire(c) {
		t.Error("a new gateway was let move while two were over the limit")
	}
	g.Release(a)
	if g.Acquire(c) {
		t.Error("a new gateway was let move while one was still over the limit")
	}
	g.Release(b)
	if !g.Acquire(c) {
		t.Error("a new gateway was turned away with the gate empty")
	}
}

func TestGateNeverLetsMoreThanMaxMoveAtOnce(t *testing.T) {
	const max = 3
	g := NewGate(max)
	var inside, peak atomic.Int64
	var wg sync.WaitGroup
	for i := range 40 {
		key := types.NamespacedName{Namespace: "ns", Name: fmt.Sprintf("gw-%d", i)}
		wg.Go(func() {
			for range 200 {
				if !g.Acquire(key) {
					continue
				}
				n := inside.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				inside.Add(-1)
				g.Release(key)
			}
		})
	}
	wg.Wait()
	if p := peak.Load(); p > max {
		t.Errorf("%d gateways were moving at once with a limit of %d", p, max)
	}
	if g.Moving() != 0 {
		t.Errorf("Moving() = %d at the end, want 0", g.Moving())
	}
}
