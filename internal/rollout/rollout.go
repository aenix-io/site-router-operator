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
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/types"

	"github.com/aenix-io/site-router-operator/internal/config"
)

// Current is the image a gateway runs: the name of its version and the digest of
// its disk. Both, because the same version can be rebuilt, and a new build of a
// version the engine can already drive is meant to reach the gateways. The zero
// Current is a gateway that is being made.
type Current struct {
	Version string
	Digest  string
}

// Decision is the version a gateway is to run.
type Decision struct {
	// Target is the version the gateway is to run, and Image its image.
	Target string
	Image  string
	// Moving is true when the gateway is to run another image than it does now. An
	// image is its digest: another version name for the same image is not a move,
	// the disk is the one it has, and another build of the same version is.
	Moving bool
	// Unsupported says why the version that was asked for is not the target, when
	// it is not: the annotation names a version the operator does not list. The
	// gateway stays where it is.
	Unsupported string
	// Orphaned is true when the image the gateway runs is not the one the operator
	// lists for its version, or its version is not listed at all. It is left as it
	// is. Its Image is the repository of the latest with its own digest, which is
	// enough to name its disk and not to make one.
	Orphaned bool
}

// Decide returns the version a gateway is to run, from the image it runs now, the
// annotation on its SiteRouter (empty for none) and the configuration.
//
// In either policy an annotation that names a listed version decides, up or down,
// and keeps the gateway there; when it names the version the gateway runs and the
// operator lists another build of it, that is a move to the build. Without an
// annotation, auto policy moves the gateway to the latest and annotation policy
// leaves it where it is, and a gateway that is being made is made with the latest.
func Decide(cfg config.Config, cur Current, annotation string) (Decision, error) {
	latestImage, ok := cfg.Engine.Image(cfg.Engine.Latest)
	if !ok {
		return Decision{}, fmt.Errorf("the latest version %q is not listed: the configuration was not checked", cfg.Engine.Latest)
	}
	latest := Decision{Target: cfg.Engine.Latest, Image: latestImage}

	// A gateway that is being made has nothing to stay at.
	if cur.Version == "" {
		if annotation != "" {
			if img, ok := cfg.Engine.Image(annotation); ok {
				return Decision{Target: annotation, Image: img}, nil
			}
			latest.Unsupported = annotation
		}
		return latest, nil
	}

	stay := stayAt(cfg, cur, latestImage)

	target := func(name string) (Decision, bool) {
		img, ok := cfg.Engine.Image(name)
		if !ok {
			return Decision{}, false
		}
		d := Decision{Target: name, Image: img}
		d.Moving = digestOf(img) != cur.Digest
		return d, true
	}

	if annotation != "" {
		d, ok := target(annotation)
		if !ok {
			stay.Unsupported = annotation
			return stay, nil
		}
		return d, nil
	}
	if cfg.Upgrade.Policy == config.UpgradeAuto {
		d, _ := target(cfg.Engine.Latest)
		return d, nil
	}
	return stay, nil
}

// stayAt is the decision to keep a gateway on the image it runs.
func stayAt(cfg config.Config, cur Current, latestImage string) Decision {
	stay := Decision{Target: cur.Version}
	if listed, ok := cfg.Engine.Image(cur.Version); ok && digestOf(listed) == cur.Digest {
		stay.Image = listed
		return stay
	}
	// What runs is not what is listed under its name, or is not listed. The
	// repository of the latest stands in for its own.
	stay.Image = withDigest(latestImage, cur.Digest)
	stay.Orphaned = true
	return stay
}

// Stay returns the decision to keep a gateway on the image it runs, whatever the
// policy and the annotation say. It is what a gateway that is to move runs from
// until the disk of the other image is ready, and what one that may not move yet
// goes on running.
func Stay(cfg config.Config, cur Current) (Decision, error) {
	latestImage, ok := cfg.Engine.Image(cfg.Engine.Latest)
	if !ok {
		return Decision{}, fmt.Errorf("the latest version %q is not listed: the configuration was not checked", cfg.Engine.Latest)
	}
	return stayAt(cfg, cur, latestImage), nil
}

// digestOf is the digest an image is pinned by, or "" when it has none.
func digestOf(image string) string {
	_, d, _ := strings.Cut(image, "@")
	return d
}

// withDigest is the image with the digest given in place of its own.
func withDigest(image, digest string) string {
	repo, _, _ := strings.Cut(image, "@")
	return repo + "@" + digest
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
