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

import "strings"

// Names are the names of the objects the operator creates for a SiteRouter.
// All of them derive from the name of the SiteRouter, which the schema limits
// to 40 characters so that the longest, the cloud-init Secret, stays within the
// 63 characters that a name may have where it is used as a label value or a
// host name.
type Names struct {
	VM string
	// BootDisk and CloudInit are named after the image the gateway boots from, so
	// that a gateway that moves to another image gets a new disk and a new seed
	// beside the old ones, which are never changed. NamesFor leaves them empty and
	// ForImage fills them.
	BootDisk   string
	CloudInit  string
	APIKey     string
	PSK        string
	Tunnel     string
	EgressDeny string
	Ingress    string
	// Forwarder is the name of the Forwarder, which is not part of what Render
	// makes: its addresses come from the network, so it is made once the VM is.
	Forwarder string
}

const namePrefix = "site-router-"

// NamesFor returns the names for the SiteRouter with the given name.
func NamesFor(instance string) Names {
	vm := namePrefix + instance
	return Names{
		VM:         vm,
		APIKey:     vm + "-api-key",
		PSK:        vm + "-psk",
		Tunnel:     vm + "-tunnel",
		EgressDeny: vm + "-gateway-egress-deny",
		Ingress:    vm + "-gateway-ingress",
		Forwarder:  vm,
	}
}

// ForImage fills in the names that depend on the image: the boot disk and the
// seed, named after the first four characters of the digest. With the name of the
// SiteRouter at its longest, 40 characters, they are 62.
func (n Names) ForImage(image string) Names {
	short := shortDigest(image)
	n.BootDisk = n.VM + "-boot-" + short
	n.CloudInit = n.VM + "-seed-" + short
	return n
}

// ImageDigest returns the digest an image is pinned by, with its algorithm.
func ImageDigest(image string) (string, error) {
	if _, err := registryURL(image); err != nil {
		return "", err
	}
	_, digest, _ := strings.Cut(image, "@")
	return digest, nil
}

// shortDigest is the first four hex characters of the digest of an image, or
// nothing when it has none.
func shortDigest(image string) string {
	_, digest, ok := strings.Cut(image, "@")
	if !ok {
		return ""
	}
	hex := strings.TrimPrefix(digest, "sha256:")
	if len(hex) < 4 {
		return ""
	}
	return hex[:4]
}
