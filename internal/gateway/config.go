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
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
)

// Config is what the platform decides about a gateway and the SiteRouter does
// not say: where the VM runs, how its disk is stored and imported, how the
// tunnel is published and how the gateway is isolated. It comes from the
// operator's configuration and is checked when the operator starts.
//
// Only what is safe to set is here. The fields that the gateway's security or
// the controller depend on have no setting, and ParseConfig refuses a key it
// does not know instead of ignoring it.
type Config struct {
	VM         VMConfig         `json:"vm,omitzero"`
	DataVolume DataVolumeConfig `json:"dataVolume,omitzero"`
	Service    ServiceConfig    `json:"service,omitzero"`
	Isolation  IsolationConfig  `json:"isolation,omitzero"`
}

// VMConfig is the placement and metadata of the gateway pod.
type VMConfig struct {
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`
	PodLabels      map[string]string `json:"podLabels,omitempty"`
	// NodeSelector is added to the architecture selector the engine's plan asks
	// for; it cannot name the architecture itself.
	NodeSelector      map[string]string   `json:"nodeSelector,omitempty"`
	Tolerations       []corev1.Toleration `json:"tolerations,omitempty"`
	Affinity          *corev1.Affinity    `json:"affinity,omitempty"`
	PriorityClassName string              `json:"priorityClassName,omitempty"`
	RuntimeClassName  string              `json:"runtimeClassName,omitempty"`
}

// DataVolumeConfig is how the boot disk is stored and imported.
type DataVolumeConfig struct {
	// StorageClass is used when the SiteRouter names none. Empty means the
	// default class of the cluster.
	StorageClass string       `json:"storageClass,omitempty"`
	BlockSize    BlockSize    `json:"blockSize,omitzero"`
	Source       SourceConfig `json:"source,omitzero"`
}

// BlockSize is the block size the guest sees on its disk. A DRBD volume with a
// 4Kn backing device needs 512 and 4096.
type BlockSize struct {
	Logical  int `json:"logical,omitempty"`
	Physical int `json:"physical,omitempty"`
}

// SourceConfig is how the image is pulled by the importer.
type SourceConfig struct {
	// PullMethod is "pod" (the importer pulls) or "node" (the node pulls, and
	// uses its registry mirrors). Empty leaves the choice to the importer.
	PullMethod string `json:"pullMethod,omitempty"`
	// CertConfigMap names a ConfigMap with the certificates of the registry.
	CertConfigMap string `json:"certConfigMap,omitempty"`
	// SecretRef names a Secret with the credentials for the registry.
	SecretRef string `json:"secretRef,omitempty"`
}

// ServiceConfig is how the tunnel Service is published.
type ServiceConfig struct {
	// LoadBalancerClass cannot be changed on a Service that exists.
	LoadBalancerClass string            `json:"loadBalancerClass,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
}

// IsolationMode says who keeps the gateway pod apart from the other pods.
type IsolationMode string

// Isolation modes.
const (
	// IsolationCilium has the operator create two CiliumNetworkPolicies.
	IsolationCilium IsolationMode = "cilium"
	// IsolationExternal leaves the isolation to the platform, which provides it
	// itself. The operator creates no policy.
	IsolationExternal IsolationMode = "external"
	// IsolationNone means no isolation. The gateway is not created unless the
	// configuration acknowledges it.
	IsolationNone IsolationMode = "none"
)

// IsolationConfig is the isolation of the gateway pod.
type IsolationConfig struct {
	Mode IsolationMode `json:"mode,omitempty"`
	// AcknowledgeNone confirms that IsolationNone is meant.
	AcknowledgeNone bool `json:"acknowledgeNone,omitempty"`
	// Controller is the identity the management API accepts, and the only one.
	// Required for IsolationCilium.
	Controller ControllerIdentity `json:"controller,omitzero"`
	// EgressDenyCIDRs are denied to the gateway on top of the link-local range,
	// which is always denied.
	EgressDenyCIDRs []string `json:"egressDenyCIDRs,omitempty"`
}

// ControllerIdentity selects the pods of the operator: those in Namespace that
// carry every one of Labels.
type ControllerIdentity struct {
	Namespace string            `json:"namespace,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// DefaultConfig is the configuration a key left out of the file falls back to.
func DefaultConfig() Config {
	return Config{DataVolume: DataVolumeConfig{BlockSize: BlockSize{Logical: 512, Physical: 4096}}}
}

// ParseConfig reads a configuration, fills in the defaults and checks it. A key
// that is not a setting is an error, so that a setting that was meant to change
// something and cannot is not silently ignored.
func ParseConfig(data []byte) (Config, error) {
	cfg := DefaultConfig()
	if err := yaml.UnmarshalStrict(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("reading the gateway configuration: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// What the configuration cannot set because the gateway depends on it. Labels
// and annotations are the keys, the others are prefixes.
var (
	reservedPodLabels = []string{
		v1alpha1.LabelManagedBy,
		// KubeVirt sets this one, and the pod is found by the label of the
		// operator, not by this.
		"vm.kubevirt.io/name",
	}
	reservedPodAnnotations = []string{
		// A gateway must be born with port security on. Whether it forwards is
		// for the network connector to grant once the source filter is confirmed.
		"ovn.kubernetes.io/port_security",
		// Set by the operator for live migration over the pod network.
		"kubevirt.io/allow-pod-bridge-network-live-migration",
	}
	reservedPrefix = "siterouter.aenix.io/"
	// The architecture comes from the engine's plan.
	reservedNodeSelector = []string{"kubernetes.io/arch"}
)

// Validate lists every problem with the configuration, not only the first.
func (c Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	checkKeys := func(what string, m map[string]string, reserved []string, value bool) {
		for _, k := range slices.Sorted(maps.Keys(m)) {
			if slices.Contains(reserved, k) || strings.HasPrefix(k, reservedPrefix) {
				add("%s: %q is set by the operator and cannot be configured", what, k)
				continue
			}
			for _, msg := range validation.IsQualifiedName(k) {
				add("%s: key %q: %s", what, k, msg)
			}
			if value {
				for _, msg := range validation.IsValidLabelValue(m[k]) {
					add("%s: value of %q: %s", what, k, msg)
				}
			}
		}
	}
	checkKeys("vm.podLabels", c.VM.PodLabels, reservedPodLabels, true)
	checkKeys("vm.podAnnotations", c.VM.PodAnnotations, reservedPodAnnotations, false)
	checkKeys("vm.nodeSelector", c.VM.NodeSelector, reservedNodeSelector, true)
	checkKeys("service.annotations", c.Service.Annotations, nil, false)

	if c.Service.LoadBalancerClass != "" {
		for _, msg := range validation.IsQualifiedName(c.Service.LoadBalancerClass) {
			add("service.loadBalancerClass: %s", msg)
		}
	}
	for what, name := range map[string]string{
		"vm.priorityClassName":            c.VM.PriorityClassName,
		"vm.runtimeClassName":             c.VM.RuntimeClassName,
		"dataVolume.source.certConfigMap": c.DataVolume.Source.CertConfigMap,
		"dataVolume.source.secretRef":     c.DataVolume.Source.SecretRef,
		"dataVolume.storageClass":         c.DataVolume.StorageClass,
		"isolation.controller.namespace":  c.Isolation.Controller.Namespace,
	} {
		if name == "" {
			continue
		}
		for _, msg := range validation.IsDNS1123Subdomain(name) {
			add("%s: %s", what, msg)
		}
	}

	if bs := c.DataVolume.BlockSize; !validBlockSizes(bs) {
		add("dataVolume.blockSize: logical %d and physical %d must be powers of two from 512 to 4096, with logical not above physical", bs.Logical, bs.Physical)
	}
	switch c.DataVolume.Source.PullMethod {
	case "", "pod", "node":
	default:
		add("dataVolume.source.pullMethod: %q is not one of pod, node", c.DataVolume.Source.PullMethod)
	}

	errs = append(errs, c.Isolation.validate()...)
	return errors.Join(errs...)
}

func (i IsolationConfig) validate() []error {
	var errs []error
	switch i.Mode {
	case IsolationCilium:
		if i.Controller.Namespace == "" {
			errs = append(errs, errors.New("isolation.controller.namespace is required when isolation.mode is cilium"))
		}
		if len(i.Controller.Labels) == 0 {
			errs = append(errs, errors.New("isolation.controller.labels is required when isolation.mode is cilium: without a label every pod of the namespace would reach the management API"))
		}
		for _, k := range slices.Sorted(maps.Keys(i.Controller.Labels)) {
			for _, msg := range validation.IsQualifiedName(k) {
				errs = append(errs, fmt.Errorf("isolation.controller.labels: key %q: %s", k, msg))
			}
			for _, msg := range validation.IsValidLabelValue(i.Controller.Labels[k]) {
				errs = append(errs, fmt.Errorf("isolation.controller.labels: value of %q: %s", k, msg))
			}
		}
	case IsolationExternal, IsolationNone:
	case "":
		errs = append(errs, errors.New("isolation.mode is required: one of cilium, external, none"))
	default:
		errs = append(errs, fmt.Errorf("isolation.mode: %q is not one of cilium, external, none", i.Mode))
	}
	if i.AcknowledgeNone && i.Mode != IsolationNone {
		errs = append(errs, errors.New("isolation.acknowledgeNone is set but isolation.mode is not none"))
	}
	for _, c := range i.EgressDenyCIDRs {
		p, err := netip.ParsePrefix(c)
		if err != nil || !p.Addr().Is4() || p.Masked() != p {
			errs = append(errs, fmt.Errorf("isolation.egressDenyCIDRs: %q is not an IPv4 network in canonical form", c))
		}
	}
	return errs
}

func validBlockSizes(b BlockSize) bool {
	ok := func(n int) bool { return n >= 512 && n <= 4096 && n&(n-1) == 0 }
	return ok(b.Logical) && ok(b.Physical) && b.Logical <= b.Physical
}
