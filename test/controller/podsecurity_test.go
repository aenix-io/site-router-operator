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

package controller_test

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/aenix-io/site-router-operator/internal/testchart"
)

// The operator pod the chart makes is admitted to a namespace that enforces the
// restricted Pod Security Standard, which is what the apiserver checks of a pod
// that runs as non-root with no capabilities and the RuntimeDefault seccomp
// profile.
func TestTheOperatorPodIsAdmittedUnderTheRestrictedStandard(t *testing.T) {
	ns := &corev1.Namespace{
		GenerateName: "restricted-",
		Labels:       map[string]string{"pod-security.kubernetes.io/enforce": "restricted"},
	}
	mustCreate(t, ns)
	objs := testchart.Render(t, ns.Name, ns.Name, "--values", testchart.Values("minimal-values.yaml"))
	mustCreate(t, objs.One(t, "ServiceAccount"))
	var dep appsv1.Deployment
	testchart.Into(t, objs.One(t, "Deployment"), &dep)

	pod := &corev1.Pod{
		Name:      "operator",
		Namespace: ns.Name,
		Labels:    dep.Spec.Template.Labels,
		Spec:      dep.Spec.Template.Spec,
	}
	if err := k8sClient.Create(t.Context(), pod); err != nil {
		t.Fatalf("the operator pod is refused: %v", err)
	}
}
