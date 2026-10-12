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

package config

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aenix-io/site-router-operator/api/engine"
)

const (
	digestA = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	digestB = "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

const minimal = `
managementCIDR: 10.244.0.0/16
clusterNetworks: {podCIDR: 10.244.0.0/16, serviceCIDR: 10.96.0.0/16}
gateway:
  isolation: {mode: external}
engine:
  name: appliance
  latest: "1.1"
  versions:
  - {name: "1.1", image: "registry.example/disk:1.1@` + digestA + `"}
  - {name: "1.0", image: "registry.example/disk@` + digestB + `"}
upgrade: {policy: annotation}
`

func TestParseTheSmallestUsableConfiguration(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upgrade.MaxConcurrent != 2 {
		t.Errorf("maxConcurrent = %d, want the default 2", cfg.Upgrade.MaxConcurrent)
	}
	if cfg.Timing.ForwarderNotResponding.Duration != time.Minute || cfg.Timing.Poll.Duration != 30*time.Second ||
		cfg.Timing.Discovery.Duration != time.Minute || cfg.Upgrade.ImportTimeout.Duration != 30*time.Minute {
		t.Errorf("timing = %+v, want the defaults", cfg.Timing)
	}
	if bs := cfg.Gateway.DataVolume.BlockSize; bs.Logical != 512 || bs.Physical != 4096 {
		t.Errorf("block size = %+v, want the default", bs)
	}
	if img, ok := cfg.Engine.Image("1.0"); !ok || !strings.Contains(img, digestB) {
		t.Errorf("Image(1.0) = %q, %v", img, ok)
	}
	if _, ok := cfg.Engine.Image("9.9"); ok {
		t.Error("Image answers for a version that is not listed")
	}
	if got := cfg.Engine.Names(); !slices.Equal(got, []string{"1.1", "1.0"}) {
		t.Errorf("Names() = %v, want the order of the file", got)
	}
}

func TestParseReadsWhatItIsGiven(t *testing.T) {
	cfg, err := Parse([]byte(minimal + `
timing: {forwarderNotResponding: 90s, poll: 10s, discovery: 2m}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timing.ForwarderNotResponding.Duration != 90*time.Second || cfg.Timing.Poll.Duration != 10*time.Second ||
		cfg.Timing.Discovery.Duration != 2*time.Minute {
		t.Errorf("timing = %+v", cfg.Timing)
	}
	cfg, err = Parse([]byte(strings.Replace(minimal, "upgrade: {policy: annotation}", "upgrade: {policy: auto, maxConcurrent: 3, importTimeout: 1h}", 1)))
	if err != nil || cfg.Upgrade.Policy != UpgradeAuto || cfg.Upgrade.MaxConcurrent != 3 || cfg.Upgrade.ImportTimeout.Duration != time.Hour {
		t.Errorf("upgrade = %+v, %v", cfg.Upgrade, err)
	}
}

func TestParseRefusesWhatIsNotASetting(t *testing.T) {
	for name, extra := range map[string]string{
		"a typo at the top":        "managementCidr: 1",
		"a typo in the engine":     "engine: {nam: x}",
		"a key of the gateway":     "gateway: {service: {externalTrafficPolicy: Cluster}}",
		"an unknown upgrade key":   "upgrade: {parallel: 5}",
		"a duration that is not":   "timing: {poll: soon}",
		"a time as a bare number":  "timing: {poll: 30}",
		"an unknown timing key":    "timing: {sleep: 1s}",
		"a cluster network typo":   "clusterNetworks: {podCidr: 10.0.0.0/8}",
		"a wrong type for a field": "managementCIDR: [10.0.0.0/8]",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(minimal + "\n" + extra)); err == nil {
				t.Fatal("the configuration was accepted")
			}
		})
	}
	if _, err := Parse([]byte("engine: [")); err == nil {
		t.Error("a document that is not YAML was accepted")
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"no management network", func(c *Config) { c.ManagementCIDR = "" }, "managementCIDR is required"},
		{"a management network that is not one", func(c *Config) { c.ManagementCIDR = "pods" }, "not an IPv4 network"},
		{"an IPv6 management network", func(c *Config) { c.ManagementCIDR = "fd00::/8" }, "not an IPv4 network"},
		{"a management network with host bits", func(c *Config) { c.ManagementCIDR = "10.244.0.5/16" }, "host bits"},
		{"a management network that is everything", func(c *Config) { c.ManagementCIDR = "0.0.0.0/0" }, "leaves the management API open"},
		{"no pod network", func(c *Config) { c.ClusterNetworks.PodCIDR = "" }, "clusterNetworks: podCIDR is required"},
		{"no isolation mode", func(c *Config) { c.Gateway.Isolation.Mode = "" }, "gateway: isolation.mode is required"},
		{"no engine", func(c *Config) { c.Engine.Name = "" }, "engine.name is required"},
		{"no versions", func(c *Config) { c.Engine.Versions = nil; c.Engine.Latest = "" }, "engine.versions is empty"},
		{"a version with no name", func(c *Config) { c.Engine.Versions[0].Name = "" }, "the name is empty"},
		{"a version listed twice", func(c *Config) { c.Engine.Versions[1].Name = "1.1" }, `"1.1" is listed twice`},
		{"an image that is not pinned", func(c *Config) { c.Engine.Versions[0].Image = "registry.example/disk:1.1" }, "not pinned by a digest"},
		{"no latest", func(c *Config) { c.Engine.Latest = "" }, "engine.latest is required"},
		{"a latest that is not listed", func(c *Config) { c.Engine.Latest = "2.0" }, `"2.0" is not in engine.versions`},
		{"no upgrade policy", func(c *Config) { c.Upgrade.Policy = "" }, "upgrade.policy is required"},
		{"an upgrade policy that is unknown", func(c *Config) { c.Upgrade.Policy = "eager" }, "not one of auto, annotation"},
		{"no gateway may move", func(c *Config) { c.Upgrade.MaxConcurrent = 0 }, "below 1"},
		{"a negative concurrency", func(c *Config) { c.Upgrade.MaxConcurrent = -1 }, "below 1"},
		{"no wait for the connector", func(c *Config) { c.Timing.ForwarderNotResponding.Duration = 0 }, "timing.forwarderNotResponding"},
		{"a negative poll", func(c *Config) { c.Timing.Poll.Duration = -time.Second }, "timing.poll"},
		{"no discovery time", func(c *Config) { c.Timing.Discovery.Duration = 0 }, "timing.discovery"},
		{"no time to import an image", func(c *Config) { c.Upgrade.ImportTimeout.Duration = 0 }, "upgrade.importTimeout"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := Parse([]byte(minimal))
			if err != nil {
				t.Fatal(err)
			}
			c.mutate(&cfg)
			err = cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want one that mentions %q", err, c.want)
			}
		})
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	_, err := Parse([]byte("upgrade: {policy: sometimes}"))
	if err == nil {
		t.Fatal("an empty configuration was accepted")
	}
	for _, want := range []string{"managementCIDR", "podCIDR", "isolation.mode", "engine.name", "engine.versions", "engine.latest", "upgrade.policy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

func TestCheckAgainstTheEngine(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	good := engine.Info{Name: "appliance", ContractVersion: engine.ContractVersion, Versions: []string{"1.0", "1.1", "1.2"}}
	if err := cfg.CheckAgainst(good); err != nil {
		t.Fatalf("a configuration the engine can drive was refused: %v", err)
	}

	for name, c := range map[string]struct {
		info engine.Info
		want string
	}{
		"another engine":           {engine.Info{Name: "other", ContractVersion: engine.ContractVersion, Versions: good.Versions}, `the engine is "other"`},
		"another contract":         {engine.Info{Name: "appliance", ContractVersion: engine.ContractVersion + 1, Versions: good.Versions}, "version"},
		"a version it cannot":      {engine.Info{Name: "appliance", ContractVersion: engine.ContractVersion, Versions: []string{"1.1"}}, `"1.0", which the engine cannot drive`},
		"no version at all":        {engine.Info{Name: "appliance", ContractVersion: engine.ContractVersion}, "cannot drive"},
		"several things are wrong": {engine.Info{Name: "x", ContractVersion: 99, Versions: nil}, "the engine is"},
	} {
		err := cfg.CheckAgainst(c.info)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error = %v, want one that mentions %q", name, err, c.want)
		}
	}
}
