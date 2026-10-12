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

// Package rollout decides which version of the image a gateway runs, and how many
// gateways may move to another at once.
//
// Moving a gateway to another image is the one planned operation that drops its
// tunnel, for as long as the new disk takes to import and boot. It is bounded
// because of that: a release of the image must not take down the tunnels of
// every tenant at the same time.
package rollout

import (
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/types"

	"github.com/aenix-io/site-router-operator/internal/config"
)

// Decision is the version a gateway is to run.
type Decision struct {
	// Target is the version the gateway is to run, and Image its image.
	Target string
	Image  string
	// Moving is true when the gateway runs another version now.
	Moving bool
	// Unsupported says why the version that was asked for is not the target, when
	// it is not: the annotation names a version the operator does not list. The
	// gateway stays where it is.
	Unsupported string
	// Orphaned is true when the version the gateway runs is not listed any more.
	// It is left as it is, and there is no image to make a disk from, so it
	// cannot be moved back to; the operator still manages it.
	Orphaned bool
}

// Decide returns the version a gateway is to run, from the version it runs now
// (empty for one that is being made), the annotation on its SiteRouter (empty for
// none) and the configuration.
//
// In either policy an annotation that names a listed version decides, up or down,
// and keeps the gateway there. Without one, auto policy moves the gateway to the
// latest version and annotation policy leaves it where it is, and a gateway that
// is being made is made with the latest.
func Decide(cfg config.Config, current, annotation string) (Decision, error) {
	latestImage, ok := cfg.Engine.Image(cfg.Engine.Latest)
	if !ok {
		return Decision{}, fmt.Errorf("the latest version %q is not listed: the configuration was not checked", cfg.Engine.Latest)
	}
	latest := Decision{Target: cfg.Engine.Latest, Image: latestImage}

	// A gateway that is being made has nothing to stay at.
	if current == "" {
		if annotation != "" {
			if img, ok := cfg.Engine.Image(annotation); ok {
				return Decision{Target: annotation, Image: img}, nil
			}
			latest.Unsupported = annotation
		}
		return latest, nil
	}

	currentImage, listed := cfg.Engine.Image(current)
	stay := Decision{Target: current, Image: currentImage, Orphaned: !listed}

	if annotation != "" {
		img, ok := cfg.Engine.Image(annotation)
		if !ok {
			stay.Unsupported = annotation
			return stay, nil
		}
		return move(Decision{Target: annotation, Image: img}, current), nil
	}
	if cfg.Upgrade.Policy == config.UpgradeAuto {
		return move(latest, current), nil
	}
	return stay, nil
}

func move(d Decision, current string) Decision {
	d.Moving = d.Target != current
	return d
}

// Gate bounds how many gateways move at once. It is a count of the gateways
// that are moving, and has no queue: one that is turned away asks again on its
// next pass, so which of them goes next is not promised, and all of them do.
type Gate struct {
	max int

	mu     sync.Mutex
	moving map[types.NamespacedName]bool
}

// NewGate returns a Gate that lets max gateways move at once.
func NewGate(max int) *Gate {
	return &Gate{max: max, moving: map[types.NamespacedName]bool{}}
}

// Acquire says whether the gateway may be moving. A gateway that already is, is
// let go on being so, whatever has been acquired since: turning it away would
// leave it half moved.
func (g *Gate) Acquire(key types.NamespacedName) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.moving[key] {
		return true
	}
	if len(g.moving) >= g.max {
		return false
	}
	g.moving[key] = true
	return true
}

// Release says the gateway has stopped moving.
func (g *Gate) Release(key types.NamespacedName) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.moving, key)
}

// Restore marks the gateways that were moving before the operator started, which
// the operator reads from their status. They count against the limit, and are
// let go on moving even if there are more of them than the limit now is: a lower
// limit does not stop a move that is under way.
func (g *Gate) Restore(keys []types.NamespacedName) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, k := range keys {
		g.moving[k] = true
	}
}

// Moving is how many gateways are moving.
func (g *Gate) Moving() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.moving)
}
