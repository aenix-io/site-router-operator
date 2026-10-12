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
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/controller"
	"github.com/aenix-io/site-router-operator/connectors/kube-ovn/internal/grant"
)

// Whoever may patch a pod may write its annotations, those the connector writes
// as well. The tests below change them while the connector is away, and change
// what the Forwarder asks for, so that the connector that comes back decides on
// what the cluster is then and on nothing it remembers.

// beforeAnnotation is an annotation a pod may carry to say what its check was
// before a grant. The connector does not read it, and a pod that carries it
// gains nothing.
const beforeAnnotation = "kube-ovn.siterouter.aenix.io/port-security-before"

// launcher is the pod of the gateway that granting makes.
const launcher = "virt-launcher-branch"

// editLauncher changes the pod of the gateway as whoever may patch it would.
func (w *world) editLauncher(edit func(*corev1.Pod)) {
	w.t.Helper()
	p := w.getPod(launcher)
	edit(p)
	if err := k8sClient.Update(w.t.Context(), p); err != nil {
		w.t.Fatal(err)
	}
}

// granting is a gateway whose pod forwards, and the connector that let it. The
// pod is checked to forward, not only the condition: a test of taking the grant
// back is worth nothing if there was none.
func granting(t *testing.T) (*world, *gateway, *connector) {
	t.Helper()
	w := newWorld(t)
	conn := startConnector(t, nil)
	g := w.newGateway("branch")
	g.launcher("virt-launcher-branch", gwIP, true)
	g.forwarder(remote)
	g.condition(v1alpha1.ForwarderGranted, metav1.ConditionTrue, controller.ReasonGranted)
	if err := g.granted("virt-launcher-branch"); err != nil {
		t.Fatal(err)
	}
	return w, g, conn
}

func TestATamperedPodIsCheckedWhenTheNetworksGo(t *testing.T) {
	for _, tc := range []struct {
		what   string
		tamper func(*world, *corev1.Pod)
	}{
		{"the name of the Forwarder removed", func(_ *world, p *corev1.Pod) {
			delete(p.Annotations, grant.GrantedByAnnotation)
		}},
		{"the name of a Forwarder that is not there", func(_ *world, p *corev1.Pod) {
			p.Annotations[grant.GrantedByAnnotation] = "site-router-other"
		}},
		{"the name of another Forwarder", func(w *world, p *corev1.Pod) {
			w.newGateway("other").forwarder()
			p.Annotations[grant.GrantedByAnnotation] = "site-router-other"
		}},
		{"a check that was off before", func(_ *world, p *corev1.Pod) {
			p.Annotations[beforeAnnotation] = "false"
		}},
		{"the label and the name removed", func(_ *world, p *corev1.Pod) {
			p.Labels = nil
			delete(p.Annotations, grant.GrantedByAnnotation)
		}},
		// KubeVirt still runs the instance in it, and says so where only it writes.
		{"the label, the owner and the name removed", func(_ *world, p *corev1.Pod) {
			p.Labels = nil
			p.OwnerReferences = nil
			delete(p.Annotations, grant.GrantedByAnnotation)
		}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			w, g, conn := granting(t)
			conn.stop()
			w.editLauncher(func(p *corev1.Pod) { tc.tamper(w, p) })
			g.ask()

			startConnector(t, nil)
			g.condition(v1alpha1.ForwarderGranted, metav1.ConditionFalse, controller.ReasonNoNetworks)
			eventually(t, "the pod is checked again", func() error { return w.checked("virt-launcher-branch") })
		})
	}
}

// KubeVirt runs the instance in another pod now, and the pod it left has lost
// its label and the name of the Forwarder: all it says is that the instance
// controls it, and that is enough to check it again.
func TestAPodKubeVirtLeftIsCheckedWhateverItSays(t *testing.T) {
	w, g, conn := granting(t)
	conn.stop()
	other := g.launcher("virt-launcher-branch-2", gwIP2, false)
	g.setActive(other)
	w.editLauncher(func(p *corev1.Pod) {
		p.Labels = nil
		delete(p.Annotations, grant.GrantedByAnnotation)
	})

	startConnector(t, nil)
	eventually(t, "the pod KubeVirt left is checked", func() error { return w.checked("virt-launcher-branch") })
	eventually(t, "the pod KubeVirt runs the instance in now forwards", func() error { return g.granted(other.Name) })
}

// The gateway of an entitled Forwarder is let forward whatever it says, and
// the connector does not fight itself over a name: the pod ends forwarding,
// named by its Forwarder, and stays so.
func TestAnotherNameOnAGatewayIsPutRight(t *testing.T) {
	w, g, conn := granting(t)
	conn.stop()
	w.newGateway("other").forwarder()
	w.editLauncher(func(p *corev1.Pod) {
		p.Annotations[grant.GrantedByAnnotation] = "site-router-other"
		p.Annotations[grant.PortSecurityAnnotation] = "true"
	})

	startConnector(t, nil)
	settled := func() error {
		if err := g.granted("virt-launcher-branch"); err != nil {
			return err
		}
		if got := w.getPod("virt-launcher-branch").Annotations[grant.GrantedByAnnotation]; got != g.name {
			return fmt.Errorf("the pod names %q", got)
		}
		return nil
	}
	eventually(t, "the gateway forwards again", settled)
	consistently(t, "and stays so", time.Second, settled)
}

// A Forwarder that is deleted takes the grant back from its gateway before it
// goes, whatever the pod says.
func TestATamperedPodIsCheckedBeforeTheForwarderGoes(t *testing.T) {
	w, g, conn := granting(t)
	conn.stop()
	w.editLauncher(func(p *corev1.Pod) { delete(p.Annotations, grant.GrantedByAnnotation) })
	f := g.get()
	if err := k8sClient.Delete(t.Context(), f); err != nil {
		t.Fatal(err)
	}

	startConnector(t, nil)
	eventually(t, "the Forwarder goes", func() error {
		err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(f), &v1alpha1.Forwarder{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("it is still there: %v", err)
	})
	// The finalizer goes only after the pod is checked, so it is checked now.
	if err := w.checked("virt-launcher-branch"); err != nil {
		t.Error(err)
	}
}

// A pod that names a Forwarder that is not there is checked: there is nothing it
// may forward for.
func TestAPodNamingAForwarderThatIsGoneIsChecked(t *testing.T) {
	w := newWorld(t)
	startConnector(t, nil)
	w.pod("leftover", nil, webIP, func(p *corev1.Pod) {
		p.Annotations = map[string]string{
			grant.PortSecurityAnnotation: "false",
			grant.GrantedByAnnotation:    "site-router-gone",
		}
	})
	eventually(t, "the pod is checked", func() error { return w.checked("leftover") })
}

// The connector comes back to a gateway whose pod KubeVirt has not listed yet,
// and whose Forwarder asks for nothing any more: the pod is checked at once,
// and does not wait for KubeVirt.
func TestAnUnprovenPodIsCheckedWhenTheForwarderAsksForNothing(t *testing.T) {
	w, g, conn := granting(t)
	conn.stop()
	g.setActive()
	g.ask()

	startConnector(t, nil)
	g.condition(v1alpha1.ForwarderResolved, metav1.ConditionFalse, controller.ReasonGatewayUnproven)
	eventually(t, "the pod is checked", func() error { return w.checked("virt-launcher-branch") })
}
