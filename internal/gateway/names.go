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

// Names are the names of the objects the operator creates for a SiteRouter.
// All of them derive from the name of the SiteRouter, which the schema limits
// to 40 characters so that the longest, the cloud-init Secret, stays within the
// 63 characters that a name may have where it is used as a label value or a
// host name.
type Names struct {
	VM         string
	BootDisk   string
	CloudInit  string
	APIKey     string
	PSK        string
	Tunnel     string
	EgressDeny string
	Ingress    string
}

const namePrefix = "site-router-"

// NamesFor returns the names for the SiteRouter with the given name.
func NamesFor(instance string) Names {
	vm := namePrefix + instance
	return Names{
		VM:         vm,
		BootDisk:   vm + "-boot",
		CloudInit:  vm + "-cloud-init",
		APIKey:     vm + "-api-key",
		PSK:        vm + "-psk",
		Tunnel:     vm + "-tunnel",
		EgressDeny: vm + "-gateway-egress-deny",
		Ingress:    vm + "-gateway-ingress",
	}
}
