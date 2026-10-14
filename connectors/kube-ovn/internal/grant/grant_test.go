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

package grant

import (
	"encoding/json"
	"maps"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/selection"
)

const fwd = "site-router-branch"

func podWith(annotations map[string]string) *corev1.Pod {
	return &corev1.Pod{Name: "virt-launcher", ResourceVersion: "42", Annotations: annotations}
}

// apply applies a merge patch of annotations the way the apiserver does, and
// checks that it is bound to the version of the pod that was read.
func apply(t *testing.T, pod *corev1.Pod, patch []byte) map[string]string {
	t.Helper()
	var p struct {
		Metadata struct {
			ResourceVersion string             `json:"resourceVersion"`
			Annotations     map[string]*string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(patch, &p); err != nil {
		t.Fatalf("patch %s: %v", patch, err)
	}
	if p.Metadata.ResourceVersion != pod.ResourceVersion {
		t.Errorf("patch %s is for version %q, not %q that was read", patch, p.Metadata.ResourceVersion, pod.ResourceVersion)
	}
	out := maps.Clone(pod.Annotations)
	if out == nil {
		out = map[string]string{}
	}
	for k, v := range p.Metadata.Annotations {
		if v == nil {
			delete(out, k)
		} else {
			out[k] = *v
		}
	}
	return out
}

func TestGiveAndWithdraw(t *testing.T) {
	for _, tc := range []struct {
		what   string
		before map[string]string
	}{
		{"a pod the webhook checked", map[string]string{PortSecurityAnnotation: "true", "other": "kept"}},
		{"a pod with no annotation", map[string]string{"other": "kept"}},
		{"a pod with no annotations at all", nil},
		{"a pod somebody turned the check off for", map[string]string{PortSecurityAnnotation: "false"}},
		{"a pod that names another Forwarder", map[string]string{PortSecurityAnnotation: "false", GrantedByAnnotation: "another"}},
	} {
		pod := podWith(tc.before)
		p, err := Give(pod, fwd)
		if err != nil || p == nil {
			t.Fatalf("%s: Give = %s, %v", tc.what, p, err)
		}
		granted := podWith(apply(t, pod, p))
		if !Forwards(granted) || By(granted) != fwd {
			t.Errorf("%s: after the grant the pod has %v", tc.what, granted.Annotations)
		}
		if p, _ := Give(granted, fwd); p != nil {
			t.Errorf("%s: a pod that forwards on the grant is patched again: %s", tc.what, p)
		}

		p, err = Withdraw(granted)
		if err != nil || p == nil {
			t.Fatalf("%s: Withdraw = %s, %v", tc.what, p, err)
		}
		after := apply(t, granted, p)
		want := maps.Clone(tc.before)
		if want == nil {
			want = map[string]string{}
		}
		delete(want, GrantedByAnnotation)
		want[PortSecurityAnnotation] = "true"
		if !maps.Equal(after, want) {
			t.Errorf("%s: after the withdrawal the pod has %v, want %v", tc.what, after, want)
		}
	}
}

// What a pod says it had before is not what it goes back to: whoever may patch
// the pod may write it.
func TestWithdrawGoesBackToTheCheckWhateverThePodSays(t *testing.T) {
	pod := podWith(map[string]string{
		PortSecurityAnnotation:                              "false",
		GrantedByAnnotation:                                 fwd,
		"kube-ovn.siterouter.aenix.io/port-security-before": "false",
	})
	p, err := Withdraw(pod)
	if err != nil || p == nil {
		t.Fatalf("Withdraw = %s, %v", p, err)
	}
	if got := apply(t, pod, p)[PortSecurityAnnotation]; got != "true" {
		t.Errorf("the check is %q after the withdrawal, want true", got)
	}
}

func TestWithdrawLeavesAPodThatIsChecked(t *testing.T) {
	if p, err := Withdraw(podWith(map[string]string{PortSecurityAnnotation: "true"})); p != nil || err != nil {
		t.Errorf("Withdraw of a checked pod = %s, %v", p, err)
	}
	// A pod that names a Forwarder and is checked loses the name.
	p, err := Withdraw(podWith(map[string]string{PortSecurityAnnotation: "true", GrantedByAnnotation: fwd}))
	if err != nil || p == nil {
		t.Errorf("Withdraw of a checked pod that names a Forwarder = %s, %v", p, err)
	}
}

func TestDecide(t *testing.T) {
	for _, tc := range []struct {
		v              selection.Verdict
		entitled, give bool
		want           Action
	}{
		{selection.Gateway, true, true, Open},
		// In conflict, or with routes that cannot be read: what the gateway has
		// stays.
		{selection.Gateway, true, false, Leave},
		// Not listed by KubeVirt yet: what it has stays while the Forwarder is
		// entitled.
		{selection.Unproven, true, true, Leave},
		{selection.Unproven, true, false, Leave},
		// No networks, being deleted or gone: shut, listed or not.
		{selection.Gateway, false, false, Shut},
		{selection.Unproven, false, false, Shut},
		{selection.NotGateway, false, false, Shut},
		// Shown to be no longer the gateway.
		{selection.NotGateway, true, true, Shut},
		{selection.NotGateway, true, false, Shut},
	} {
		if got := Decide(tc.v, tc.entitled, tc.give); got != tc.want {
			t.Errorf("Decide(%v, entitled %v, give %v) = %v, want %v", tc.v, tc.entitled, tc.give, got, tc.want)
		}
	}
}
