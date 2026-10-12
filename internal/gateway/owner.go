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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
)

// Labels are the labels of everything the operator makes for the SiteRouter with
// the given name, and of the pod of its gateway.
func Labels(instance string) map[string]string {
	return map[string]string{
		v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue,
		v1alpha1.LabelInstance:  instance,
	}
}

// OwnerReference makes the SiteRouter the controller of an object, so that the
// object goes with it.
func OwnerReference(sr *v1alpha1.SiteRouter) metav1.OwnerReference {
	yes := true
	return metav1.OwnerReference{
		APIVersion:         v1alpha1.GroupVersion.String(),
		Kind:               "SiteRouter",
		Name:               sr.Name,
		UID:                sr.UID,
		Controller:         &yes,
		BlockOwnerDeletion: &yes,
	}
}
