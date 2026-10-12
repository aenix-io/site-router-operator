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

// Package grant lets a gateway pod forward traffic on kube-ovn and takes that
// back. kube-ovn checks the source of what a pod sends and the destination of
// what it is sent against the address of the pod, unless the annotation
// ovn.kubernetes.io/port_security of the pod is other than "true"; it applies a
// change of the annotation to the port of a running pod at once, both ways.
//
// What a pod is given is decided on what the connector verifies about it, and
// never on an annotation it reads back from the pod: whoever may patch the pod
// may write those too. The check is turned off only for the gateway of a
// Forwarder that asks for networks, and is turned back on by writing "true":
// kube-ovn takes a pod with no annotation for one with the check off, and a
// gateway is never meant to be made with the check off, so "true" is what it
// goes back to whatever the pod says it had.
package grant

import (
	"encoding/json"

	corev1 "k8s.io/api/core/v1"

	"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/selection"
)

const (
	// PortSecurityAnnotation is the annotation of a pod on the default network of
	// kube-ovn that turns the check of addresses on its port on or off.
	PortSecurityAnnotation = "ovn.kubernetes.io/port_security"
	// GrantedByAnnotation names the Forwarder that let the pod forward. It is
	// there for whoever looks at the pod, and nothing is decided on it but to
	// turn the check of the pod back on.
	GrantedByAnnotation = "kube-ovn.siterouter.aenix.io/granted-by"

	on  = "true"
	off = "false"
)

// Forwards says whether the check of addresses of the pod is off.
func Forwards(pod *corev1.Pod) bool {
	return pod.Annotations[PortSecurityAnnotation] == off
}

// By returns the Forwarder the pod says let it forward, or empty.
func By(pod *corev1.Pod) string {
	return pod.Annotations[GrantedByAnnotation]
}

// Action is what is done to the check of addresses of a pod.
type Action int

const (
	// Leave leaves the pod as it is.
	Leave Action = iota
	// Open turns the check off: the pod may forward.
	Open
	// Shut turns the check on.
	Shut
)

func (a Action) String() string {
	switch a {
	case Open:
		return "Open"
	case Shut:
		return "Shut"
	}
	return "Leave"
}

// Decide says what is done to a pod that may be the gateway of a Forwarder: one
// the Forwarder selects, that the instance named like it controls, or that says
// the Forwarder let it forward.
//
// entitled is true when the Forwarder exists, is not being deleted and asks for
// networks. give is true when it may be given what it asks for now: none of its
// networks is in conflict and the routes of the namespace can be read.
//
// Only the gateway of an entitled Forwarder is opened. A pod KubeVirt has not
// listed yet keeps what it has while the Forwarder is entitled: when the
// connector starts, a pod that shows the check off and is not listed yet, as
// after a restart of KubeVirt, is left until KubeVirt lists it or does not, so
// that a status that is late does not cut a tunnel. Every other pod is shut,
// at once, listed or not: writing "true" is never what lets traffic through.
func Decide(v selection.Verdict, entitled, give bool) Action {
	switch {
	case entitled && v == selection.Gateway && give:
		return Open
	case entitled && v != selection.NotGateway:
		return Leave
	}
	return Shut
}

// Give returns the patch that lets the pod forward on the grant of forwarder,
// or nil when it already does.
func Give(pod *corev1.Pod, forwarder string) ([]byte, error) {
	if Forwards(pod) && By(pod) == forwarder {
		return nil, nil
	}
	return patch(pod, map[string]any{PortSecurityAnnotation: off, GrantedByAnnotation: forwarder})
}

// Withdraw returns the patch that turns the check of the pod back on, or nil
// when it is on and the pod names no Forwarder.
func Withdraw(pod *corev1.Pod) ([]byte, error) {
	_, named := pod.Annotations[GrantedByAnnotation]
	if pod.Annotations[PortSecurityAnnotation] == on && !named {
		return nil, nil
	}
	return patch(pod, map[string]any{PortSecurityAnnotation: on, GrantedByAnnotation: nil})
}

// patch is a merge patch of the annotations that applies only to the version of
// the pod that was read: a change made by someone in between makes it fail, and
// the pod is judged again on what it is then.
func patch(pod *corev1.Pod, annotations map[string]any) ([]byte, error) {
	return json.Marshal(map[string]any{
		"metadata": map[string]any{
			"resourceVersion": pod.ResourceVersion,
			"annotations":     annotations,
		},
	})
}
