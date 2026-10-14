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

package v1alpha1

// Labels the operator puts on what it creates.
const (
	// LabelInstance is set on every object the operator creates for a SiteRouter
	// and on the pod template of its gateway VM, with the name of the SiteRouter
	// as the value. It is how the pods of a gateway are found, by the Service in
	// front of them, by the network policies and by a Forwarder.
	LabelInstance = "siterouter.aenix.io/instance"

	// LabelManagedBy is the standard label that names the manager of an object.
	LabelManagedBy = "app.kubernetes.io/managed-by"

	// ManagedByValue is the value of LabelManagedBy on what the operator creates.
	ManagedByValue = "site-router-operator"
)
