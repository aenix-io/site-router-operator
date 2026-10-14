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

// Package config is the configuration of the operator: what the platform
// decides about gateways and the SiteRouter does not say. It is one file, read
// once when the operator starts, and checked as a whole: an operator that starts
// with a configuration it cannot use fails there, and not on the first gateway
// that needs the part that is wrong.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/aenix-io/site-router-operator/api/engine"
	"github.com/aenix-io/site-router-operator/internal/denyset"
	"github.com/aenix-io/site-router-operator/internal/gateway"
)

// Config is the configuration of the operator.
type Config struct {
	// ManagementCIDR is where the management API of a gateway accepts calls from:
	// the pod network of the platform, which is where the operator runs. There is
	// no way to leave it open.
	ManagementCIDR string `json:"managementCIDR,omitempty"`
	// ClusterNetworks are the networks of the cluster that a tenant's networks
	// must not overlap, and that the operator cannot discover.
	ClusterNetworks denyset.Config `json:"clusterNetworks,omitzero"`
	// Gateway is how a gateway is made.
	Gateway gateway.Config `json:"gateway,omitzero"`
	// Engine is the appliance gateways run, and the images it comes in.
	Engine Engine `json:"engine,omitzero"`
	// Upgrade says when a gateway moves to another image.
	Upgrade Upgrade `json:"upgrade,omitzero"`
	// Timing is how long the operator waits for, and how often it looks.
	Timing Timing `json:"timing,omitzero"`
}

// Engine names the engine and the versions of its image that gateways can run.
type Engine struct {
	Name     string    `json:"name,omitempty"`
	Versions []Version `json:"versions,omitempty"`
	// Latest is the version new gateways are made with, and the one that gateways
	// move to when the policy is auto. It is named, not the last of the list.
	Latest string `json:"latest,omitempty"`
}

// Version is a version of the image, and where it is.
type Version struct {
	Name string `json:"name,omitempty"`
	// Image is the reference of the disk, pinned by digest. It is data, so that a
	// new build of a version the engine can drive is a change of this file and not
	// of the operator.
	Image string `json:"image,omitempty"`
}

// UpgradePolicy says when a gateway moves to another image.
type UpgradePolicy string

// Upgrade policies.
const (
	// UpgradeAuto moves every gateway to the latest version.
	UpgradeAuto UpgradePolicy = "auto"
	// UpgradeAnnotation moves a gateway only when it is told to, with the
	// annotation that names the version.
	UpgradeAnnotation UpgradePolicy = "annotation"
)

// Upgrade says when a gateway moves to another image. Moving drops the tunnel of
// the gateway for as long as it takes the new disk to boot, so how many do it at
// once is bounded, whatever asked for it.
type Upgrade struct {
	Policy UpgradePolicy `json:"policy,omitempty"`
	// MaxConcurrent is how many gateways move at once. It defaults to 2.
	MaxConcurrent int `json:"maxConcurrent,omitempty"`
	// ImportTimeout is how long the disk of the other image may take to be made
	// ready before the move is given up. Giving up leaves the gateway on the image
	// it had and frees its place. It defaults to 30 minutes.
	ImportTimeout metav1.Duration `json:"importTimeout,omitzero"`
	// BootTimeout is how long a gateway may take to run on the new disk once it has
	// been pointed at it. The places that bound how many gateways move at once are
	// shared by every namespace, and a gateway that cannot start, for want of memory
	// the cluster has or storage that is never provided, would hold one for ever.
	// Past the time the place is free and the gateway says it did not come up, with
	// the old disk still there to go back to. It defaults to 15 minutes.
	BootTimeout metav1.Duration `json:"bootTimeout,omitzero"`
}

// Timing is how long the operator waits for, and how often it looks.
type Timing struct {
	// ForwarderNotResponding is how long a request for routes goes unanswered
	// before the condition says that no connector serves it. It defaults to a
	// minute.
	ForwarderNotResponding metav1.Duration `json:"forwarderNotResponding,omitzero"`
	// Poll is how often a gateway is looked at when nothing has changed, to put
	// its configuration back and to read its tunnels. It defaults to 30 seconds.
	Poll metav1.Duration `json:"poll,omitzero"`
	// Discovery is how long the addresses of the nodes and load balancers are kept.
	// It defaults to a minute. A gateway is judged against the networks of the
	// cluster on every pass, so this is also how stale the judgement can be.
	Discovery metav1.Duration `json:"discovery,omitzero"`
}

// Defaults fill in what a file leaves out.
const (
	defaultMaxConcurrent = 2
	defaultImportTimeout = 30 * time.Minute
	defaultBootTimeout   = 15 * time.Minute
	defaultNotResponding = time.Minute
	defaultPoll          = 30 * time.Second
	defaultDiscovery     = time.Minute
)

// Default is the configuration a key left out of the file falls back to.
func Default() Config {
	return Config{
		Gateway: gateway.DefaultConfig(),
		Upgrade: Upgrade{
			MaxConcurrent: defaultMaxConcurrent,
			ImportTimeout: metav1.Duration{Duration: defaultImportTimeout},
			BootTimeout:   metav1.Duration{Duration: defaultBootTimeout},
		},
		Timing: Timing{
			ForwarderNotResponding: metav1.Duration{Duration: defaultNotResponding},
			Poll:                   metav1.Duration{Duration: defaultPoll},
			Discovery:              metav1.Duration{Duration: defaultDiscovery},
		},
	}
}

// Parse reads a configuration, fills in the defaults and checks it. A key that
// is not a setting is an error, so that a setting that was meant to change
// something and cannot is not silently ignored.
func Parse(data []byte) (Config, error) {
	cfg := Default()
	if err := yaml.UnmarshalStrict(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("reading the configuration: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate lists every problem with the configuration, not only the first.
func (c Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	switch p, err := netip.ParsePrefix(c.ManagementCIDR); {
	case c.ManagementCIDR == "":
		add("managementCIDR is required: the management API of a gateway is never left open")
	case err != nil || !p.Addr().Is4():
		add("managementCIDR: %q is not an IPv4 network", c.ManagementCIDR)
	case p.Masked() != p:
		add("managementCIDR: %q has host bits set, use %s", c.ManagementCIDR, p.Masked())
	case p.Bits() == 0:
		add("managementCIDR: %q is every address, which leaves the management API open", c.ManagementCIDR)
	}

	if err := c.ClusterNetworks.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("clusterNetworks: %w", err))
	}
	if err := c.Gateway.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("gateway: %w", err))
	}
	errs = append(errs, c.Engine.validate()...)

	switch c.Upgrade.Policy {
	case UpgradeAuto, UpgradeAnnotation:
	case "":
		add("upgrade.policy is required: one of auto, annotation")
	default:
		add("upgrade.policy: %q is not one of auto, annotation", c.Upgrade.Policy)
	}
	if c.Upgrade.MaxConcurrent < 1 {
		add("upgrade.maxConcurrent: %d is below 1: no gateway could ever move", c.Upgrade.MaxConcurrent)
	}

	for name, d := range map[string]metav1.Duration{
		"timing.forwarderNotResponding": c.Timing.ForwarderNotResponding,
		"timing.poll":                   c.Timing.Poll,
		"timing.discovery":              c.Timing.Discovery,
		"upgrade.importTimeout":         c.Upgrade.ImportTimeout,
		"upgrade.bootTimeout":           c.Upgrade.BootTimeout,
	} {
		if d.Duration <= 0 {
			add("%s: %s is not a duration above zero", name, d.Duration)
		}
	}
	return errors.Join(errs...)
}

func (e Engine) validate() []error {
	var errs []error
	if e.Name == "" {
		errs = append(errs, errors.New("engine.name is required"))
	}
	if len(e.Versions) == 0 {
		errs = append(errs, errors.New("engine.versions is empty: there is no image to make a gateway from"))
	}
	seen := map[string]bool{}
	for i, v := range e.Versions {
		switch {
		case v.Name == "":
			errs = append(errs, fmt.Errorf("engine.versions[%d]: the name is empty", i))
		case seen[v.Name]:
			errs = append(errs, fmt.Errorf("engine.versions: %q is listed twice", v.Name))
		}
		seen[v.Name] = true
		if err := gateway.CheckImage(v.Image); err != nil {
			errs = append(errs, fmt.Errorf("engine.versions[%d] (%s): %w", i, v.Name, err))
		}
	}
	switch {
	case e.Latest == "":
		errs = append(errs, errors.New("engine.latest is required: new gateways are made with it"))
	case !seen[e.Latest]:
		errs = append(errs, fmt.Errorf("engine.latest: %q is not in engine.versions", e.Latest))
	}
	return errs
}

// Image returns the image of a version, and whether the version is listed.
func (e Engine) Image(version string) (string, bool) {
	for _, v := range e.Versions {
		if v.Name == version {
			return v.Image, true
		}
	}
	return "", false
}

// Names lists the versions in the order of the file.
func (e Engine) Names() []string {
	out := make([]string, len(e.Versions))
	for i, v := range e.Versions {
		out[i] = v.Name
	}
	return out
}

// CheckAgainst says whether the engine can drive what the configuration lists.
// It is run when the operator starts, once the engine is known: a version the
// engine does not know is not one a seed can be made for.
func (c Config) CheckAgainst(info engine.Info) error {
	var errs []error
	if info.Name != c.Engine.Name {
		errs = append(errs, fmt.Errorf("the engine is %q, the configuration names %q", info.Name, c.Engine.Name))
	}
	if info.ContractVersion != engine.ContractVersion {
		errs = append(errs, fmt.Errorf("the engine implements version %d of the contract, the operator speaks %d", info.ContractVersion, engine.ContractVersion))
	}
	for _, v := range c.Engine.Versions {
		if !slices.Contains(info.Versions, v.Name) {
			errs = append(errs, fmt.Errorf("the configuration lists the version %q, which the engine cannot drive (it can: %v)", v.Name, info.Versions))
		}
	}
	return errors.Join(errs...)
}
