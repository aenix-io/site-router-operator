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

package e2e

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

func deployDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "deploy")
}

// Every file the kustomization lists is taken by the apiserver as it is, and the
// pod of the Deployment is let into its namespace at the restricted level of pod
// security.
func TestTheDeployFilesAreAccepted(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(deployDir(), "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var k struct {
		Resources []string `json:"resources"`
	}
	if err := yaml.Unmarshal(data, &k); err != nil {
		t.Fatal(err)
	}
	if len(k.Resources) == 0 {
		t.Fatal("the kustomization lists no files")
	}

	decode := serializer.NewCodecFactory(scheme).UniversalDeserializer()
	var deployment *appsv1.Deployment
	for _, name := range k.Resources {
		data, err := os.ReadFile(filepath.Join(deployDir(), name))
		if err != nil {
			t.Fatal(err)
		}
		for doc := range strings.SplitSeq(string(data), "\n---\n") {
			obj, _, err := decode.Decode([]byte(doc), nil, nil)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			o := obj.(client.Object)
			switch v := o.(type) {
			case *corev1.Namespace:
				// It is made for real, so that the pod below is judged by its labels.
				if err := k8sClient.Create(t.Context(), v); err != nil && !apierrors.IsAlreadyExists(err) {
					t.Fatalf("%s: %v", name, err)
				}
				continue
			case *appsv1.Deployment:
				deployment = v.DeepCopy()
			}
			err = k8sClient.Create(t.Context(), o, client.DryRunAll)
			// The files TestMain put in place are there already.
			installed := apierrors.IsAlreadyExists(err) && slices.Contains(deployFiles, name)
			if err != nil && !installed {
				t.Errorf("%s: %s %s: %v", name, o.GetObjectKind().GroupVersionKind().Kind, o.GetName(), err)
			}
		}
	}
	if deployment == nil {
		t.Fatal("the kustomization has no Deployment")
	}

	pod := &corev1.Pod{
		Name: "kube-ovn-connector", Namespace: deployment.Namespace, Labels: deployment.Spec.Template.Labels,
		Spec: deployment.Spec.Template.Spec,
	}
	pod.Spec.Containers[0].Image = "registry.example/kube-ovn-connector:test"
	if err := k8sClient.Create(t.Context(), pod, client.DryRunAll); err != nil {
		t.Errorf("the pod of the Deployment is not let into its namespace: %v", err)
	}
	c := pod.Spec.Containers[0].SecurityContext
	if c == nil || c.ReadOnlyRootFilesystem == nil || !*c.ReadOnlyRootFilesystem {
		t.Error("the root filesystem of the connector is writable")
	}
}
