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
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/grant"
	"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/routes"
)

// The ServiceAccount of the connector may patch any pod and any Namespace by
// RBAC; the admission policy of the deploy files lets it change only its own
// annotations.
func TestTheConnectorMayChangeOnlyItsOwnAnnotations(t *testing.T) {
	w := newWorld(t)
	w.pod("web-0", web, webIP)
	as, err := client.New(asConnector(), client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	patch := func(o client.Object, body string) error {
		return as.Patch(t.Context(), o, client.RawPatch(types.MergePatchType, []byte(body)))
	}
	ns := &corev1.Namespace{}
	ns.Name = w.ns
	pod := &corev1.Pod{}
	pod.Name, pod.Namespace = "web-0", w.ns
	refused := func(err error) error {
		if err == nil {
			return errors.New("it was let through")
		}
		if !strings.Contains(err.Error(), "kube-ovn-connector-writes") {
			return fmt.Errorf("it was refused by something else: %w", err)
		}
		return nil
	}

	// The policy is in force a little after it is made. Each try changes the
	// label to another value: a patch that changes nothing is let through by any
	// policy, so a try that was let through is not tried again as it was.
	try := 0
	eventually(t, "a label of a Namespace is refused", func() error {
		try++
		return refused(patch(ns, fmt.Sprintf(`{"metadata":{"labels":{"example.com/probe":"%d"}}}`, try)))
	})
	if err := refused(patch(ns, `{"metadata":{"labels":{"pod-security.kubernetes.io/enforce":"privileged"}}}`)); err != nil {
		t.Errorf("the level of security of a Namespace: %v", err)
	}
	for what, body := range map[string]string{
		"an annotation of a Namespace that is not the connector's": `{"metadata":{"annotations":{"example.com/x":"y"}}}`,
		"a finalizer of a Namespace":                               `{"metadata":{"finalizers":["example.com/x"]}}`,
	} {
		if err := refused(patch(ns, body)); err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
	for what, body := range map[string]string{
		"a label of a pod": `{"metadata":{"labels":{"app":"other"}}}`,
		"an annotation of a pod that is not the connector's": `{"metadata":{"annotations":{"ovn.kubernetes.io/ip_address":"10.244.9.9"}}}`,
		"what a pod says its check was":                      `{"metadata":{"annotations":{"kube-ovn.siterouter.aenix.io/port-security-before":"false"}}}`,
	} {
		if err := refused(patch(pod, body)); err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}

	if err := patch(ns, `{"metadata":{"annotations":{"`+routes.Annotation+`":"[]","`+routes.OwnersAnnotation+`":"{}"}}}`); err != nil {
		t.Errorf("the routes of a Namespace are refused: %v", err)
	}
	if err := patch(pod, `{"metadata":{"annotations":{"`+grant.PortSecurityAnnotation+`":"false","`+grant.GrantedByAnnotation+`":"x"}}}`); err != nil {
		t.Errorf("the check of a pod is refused: %v", err)
	}
}
