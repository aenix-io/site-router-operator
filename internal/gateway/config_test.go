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

package gateway

import (
	"strings"
	"testing"
)

const fullConfigYAML = `
vm:
  podAnnotations: {backup.example/policy: daily}
  podLabels: {billing.example/class: network}
  nodeSelector: {node-role.example/gateway: "true"}
  tolerations:
  - {key: dedicated, operator: Equal, value: gateways, effect: NoSchedule}
  priorityClassName: network-critical
  runtimeClassName: kata
dataVolume:
  storageClass: replicated
  blockSize: {logical: 512, physical: 4096}
  source: {pullMethod: node, certConfigMap: registry-ca, secretRef: registry-credentials}
service:
  loadBalancerClass: example.com/lb
  annotations: {lb.example/pool: tunnels}
isolation:
  mode: cilium
  controller:
    namespace: site-router-system
    labels: {app.kubernetes.io/name: site-router-operator}
  egressDenyCIDRs: [10.96.0.0/12, 10.244.0.0/16]
`

func TestParseConfigAcceptsTheWholeConfiguration(t *testing.T) {
	cfg, err := ParseConfig([]byte(fullConfigYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VM.PodLabels["billing.example/class"] != "network" || cfg.VM.Tolerations[0].Key != "dedicated" ||
		cfg.DataVolume.Source.PullMethod != "node" || cfg.Service.LoadBalancerClass != "example.com/lb" ||
		cfg.Isolation.Controller.Namespace != "site-router-system" || len(cfg.Isolation.EgressDenyCIDRs) != 2 {
		t.Errorf("the configuration was not read as written: %+v", cfg)
	}
}

func TestParseConfigFillsInTheDefaults(t *testing.T) {
	cfg, err := ParseConfig([]byte("isolation: {mode: external}"))
	if err != nil {
		t.Fatal(err)
	}
	if bs := cfg.DataVolume.BlockSize; bs.Logical != 512 || bs.Physical != 4096 {
		t.Errorf("block size = %+v, want 512 and 4096", bs)
	}
}

func TestParseConfigRefusesAKeyThatIsNotASetting(t *testing.T) {
	// Each of these would change something the gateway depends on, or is a typo
	// for a setting: either way, ignoring it would leave the operator doing
	// something other than what the file says.
	const isolation = "isolation: {mode: external}\n"
	for name, doc := range map[string]string{
		"externalTrafficPolicy":   isolation + "service: {externalTrafficPolicy: Cluster}",
		"a port security setting": isolation + "vm: {portSecurity: false}",
		"the architecture":        isolation + "vm: {arch: arm64}",
		"the disk size":           isolation + "dataVolume: {size: 20Gi}",
		"a typo in a key":         isolation + "vm: {nodeSelectr: {a: b}}",
		"a typo at the top":       isolation + "vms: {}",
		"a key in isolation":      "isolation: {mode: external, strict: true}",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseConfig([]byte(doc))
			if err == nil || !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("error = %v, want one about an unknown field", err)
			}
		})
	}
}

func TestParseConfigRefusesWhatIsNotYAML(t *testing.T) {
	if _, err := ParseConfig([]byte("isolation: [")); err == nil {
		t.Fatal("a document that is not YAML was accepted")
	}
}

func TestConfigValidate(t *testing.T) {
	good := func() Config {
		c := DefaultConfig()
		c.Isolation = IsolationConfig{Mode: IsolationExternal}
		return c
	}
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string // empty: accepted
	}{
		{"the default with external isolation", func(*Config) {}, ""},
		{"none, acknowledged", func(c *Config) { c.Isolation = IsolationConfig{Mode: IsolationNone, AcknowledgeNone: true} }, ""},
		{"none, not acknowledged (Render refuses it, the file is fine)", func(c *Config) { c.Isolation = IsolationConfig{Mode: IsolationNone} }, ""},

		{"a label the operator sets", func(c *Config) { c.VM.PodLabels = map[string]string{"app.kubernetes.io/managed-by": "helm"} }, "set by the operator"},
		{"the label of the instance", func(c *Config) { c.VM.PodLabels = map[string]string{"siterouter.aenix.io/instance": "other"} }, "set by the operator"},
		{"a label in the group of the operator", func(c *Config) { c.VM.PodLabels = map[string]string{"siterouter.aenix.io/anything": "x"} }, "set by the operator"},
		{"the label KubeVirt sets", func(c *Config) { c.VM.PodLabels = map[string]string{"vm.kubevirt.io/name": "other"} }, "set by the operator"},
		{"port security", func(c *Config) { c.VM.PodAnnotations = map[string]string{"ovn.kubernetes.io/port_security": "false"} }, "set by the operator"},
		{"port security set to true", func(c *Config) { c.VM.PodAnnotations = map[string]string{"ovn.kubernetes.io/port_security": "true"} }, "set by the operator"},
		{"the live migration annotation", func(c *Config) {
			c.VM.PodAnnotations = map[string]string{"kubevirt.io/allow-pod-bridge-network-live-migration": "false"}
		}, "set by the operator"},
		{"an annotation in the group of the operator", func(c *Config) { c.VM.PodAnnotations = map[string]string{"siterouter.aenix.io/upgrade-to": "0.1.0"} }, "set by the operator"},
		{"the architecture", func(c *Config) { c.VM.NodeSelector = map[string]string{"kubernetes.io/arch": "arm64"} }, "set by the operator"},
		{"a label that is not valid", func(c *Config) { c.VM.PodLabels = map[string]string{"a/b/c": "x"} }, "key"},
		{"a label value that is not valid", func(c *Config) { c.VM.PodLabels = map[string]string{"a": "not valid!"} }, "value of"},
		{"an annotation of the service in the group of the operator", func(c *Config) {
			c.Service.Annotations = map[string]string{"siterouter.aenix.io/x": "y"}
		}, "set by the operator"},

		{"a load balancer class that is not valid", func(c *Config) { c.Service.LoadBalancerClass = "not valid!" }, "loadBalancerClass"},
		{"a priority class that is not valid", func(c *Config) { c.VM.PriorityClassName = "Not_Valid" }, "priorityClassName"},
		{"a storage class that is not valid", func(c *Config) { c.DataVolume.StorageClass = "Not_Valid" }, "storageClass"},
		{"a certificate ConfigMap that is not valid", func(c *Config) { c.DataVolume.Source.CertConfigMap = "Not_Valid" }, "certConfigMap"},

		{"no block size", func(c *Config) { c.DataVolume.BlockSize = BlockSize{} }, "blockSize"},
		{"a block size that is not a power of two", func(c *Config) { c.DataVolume.BlockSize = BlockSize{Logical: 512, Physical: 3000} }, "blockSize"},
		{"a block size above 4096", func(c *Config) { c.DataVolume.BlockSize = BlockSize{Logical: 512, Physical: 8192} }, "blockSize"},
		{"a block size below 512", func(c *Config) { c.DataVolume.BlockSize = BlockSize{Logical: 256, Physical: 4096} }, "blockSize"},
		{"a logical block above the physical one", func(c *Config) { c.DataVolume.BlockSize = BlockSize{Logical: 4096, Physical: 512} }, "blockSize"},
		{"4096 and 4096", func(c *Config) { c.DataVolume.BlockSize = BlockSize{Logical: 4096, Physical: 4096} }, ""},
		{"a pull method that is unknown", func(c *Config) { c.DataVolume.Source.PullMethod = "http" }, "pullMethod"},
		{"the pod pull method", func(c *Config) { c.DataVolume.Source.PullMethod = "pod" }, ""},

		{"no isolation mode", func(c *Config) { c.Isolation = IsolationConfig{} }, "isolation.mode is required"},
		{"an unknown isolation mode", func(c *Config) { c.Isolation = IsolationConfig{Mode: "strict"} }, "not one of cilium, external, none"},
		{"cilium without an identity", func(c *Config) { c.Isolation = IsolationConfig{Mode: IsolationCilium} }, "controller.namespace is required"},
		{"cilium without labels", func(c *Config) {
			c.Isolation = IsolationConfig{Mode: IsolationCilium, Controller: ControllerIdentity{Namespace: "ns"}}
		}, "controller.labels is required"},
		{"cilium with an invalid namespace", func(c *Config) {
			c.Isolation = IsolationConfig{Mode: IsolationCilium, Controller: ControllerIdentity{Namespace: "Not_Valid", Labels: map[string]string{"a": "b"}}}
		}, "controller.namespace"},
		{"cilium with an invalid label", func(c *Config) {
			c.Isolation = IsolationConfig{Mode: IsolationCilium, Controller: ControllerIdentity{Namespace: "ns", Labels: map[string]string{"a b": "c"}}}
		}, "controller.labels"},
		{"an acknowledgement of none that does not apply", func(c *Config) {
			c.Isolation = IsolationConfig{Mode: IsolationExternal, AcknowledgeNone: true}
		}, "acknowledgeNone is set but"},
		{"an egress CIDR that is IPv6", func(c *Config) { c.Isolation.EgressDenyCIDRs = []string{"2001:db8::/32"} }, "egressDenyCIDRs"},
		{"an egress CIDR with host bits", func(c *Config) { c.Isolation.EgressDenyCIDRs = []string{"10.0.0.1/8"} }, "egressDenyCIDRs"},
		{"an egress CIDR that is not one", func(c *Config) { c.Isolation.EgressDenyCIDRs = []string{"nonsense"} }, "egressDenyCIDRs"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := good()
			c.mutate(&cfg)
			err := cfg.Validate()
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("a valid configuration was refused: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("error = %v, want one that mentions %q", err, c.want)
			}
		})
	}
}

func TestConfigValidateReportsEveryProblem(t *testing.T) {
	cfg := DefaultConfig()
	cfg.VM.NodeSelector = map[string]string{"kubernetes.io/arch": "arm64"}
	cfg.DataVolume.Source.PullMethod = "http"
	cfg.Isolation = IsolationConfig{Mode: IsolationCilium}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("a configuration with four problems was accepted")
	}
	for _, want := range []string{"kubernetes.io/arch", "pullMethod", "controller.namespace", "controller.labels"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}
