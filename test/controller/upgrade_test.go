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
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/internal/config"
	"github.com/aenix-io/site-router-operator/internal/gateway"
)

// onFirstVersion makes a gateway on the first version while the second is the
// latest, which is what the configuration of an operator that has just been given
// a new image looks like to a gateway made before it.
func onFirstVersion(cfg *config.Config, _ *v1alpha1.SiteRouter) { cfg.Engine.Latest = "1.0" }

// latest is the operator starting again with the second version as the latest.
func (g *rig) latest(mutate ...func(*config.Config)) {
	g.t.Helper()
	g.restart(func(cfg *config.Config) {
		cfg.Engine.Latest = "2.0"
		for _, m := range mutate {
			m(cfg)
		}
	})
}

// A gateway moves to the image the operator now lists as the latest in three
// steps: the disk of the new image is made beside the old one while the gateway
// runs on as it is; the gateway is booted from the new disk once it is ready; and
// the old disk and seed go once it runs.
func TestAGatewayMovesToTheLatestImage(t *testing.T) {
	g := newRig(t, onFirstVersion)
	g.ready()
	if got := g.bootsFrom(); got != diskName(imageV1) {
		t.Fatalf("the gateway boots from %s, want %s", got, diskName(imageV1))
	}
	if got := g.sr().Status.Engine; got.Version != "1.0" || got.ImageDigest != digestV1 {
		t.Fatalf("engine = %+v, want the first version", got)
	}

	g.latest()
	g.reconcile()
	g.expectTheDiskIsMadeBesideTheOld()

	g.setDV(diskName(imageV2), "ImportInProgress")
	g.reconcile()
	g.reconcile()
	g.expectNothingIsTouchedWhileTheDiskIsMade()

	g.setDV(diskName(imageV2), "Succeeded")
	g.expectTheGatewayIsBootedFromTheNewDisk()

	g.reboot()
	g.reconcile()
	g.expectTheGatewayRunsOnTheNewImage()
}

// expectTheDiskIsMadeBesideTheOld checks the first step of a move: the disk and
// the seed of the new image are made, and the gateway is not touched.
func (g *rig) expectTheDiskIsMadeBesideTheOld() {
	g.t.Helper()
	t := g.t
	newDisk, newSeed := diskOf(name, imageV2)
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionTrue, v1alpha1.ReasonUpgrading)
	g.expect(v1alpha1.ConditionConfigured, metav1.ConditionTrue, "ConfigApplied")
	g.expect(v1alpha1.ConditionReady, metav1.ConditionTrue, "AllConditionsMet")
	if !g.exists(gateway.DataVolumeGVK, newDisk) || !g.secretExists(newSeed) {
		t.Error("the disk and the seed of the new image were not made")
	}
	if got := g.bootsFrom(); got != diskName(imageV1) {
		t.Errorf("the VM was pointed at %s before the disk was ready", got)
	}
	if !g.exists(gateway.VirtualMachineInstanceGVK, g.names().VM) {
		t.Error("the gateway was restarted before the disk was ready")
	}
	if got := g.sr().Status.Engine; got.Version != "1.0" {
		t.Errorf("engine = %+v, want the version that runs", got)
	}
	if got := g.asked(); !slices.Equal(got, []string{"192.0.2.0/24"}) {
		t.Errorf("the Forwarder asks for %v while the disk is made", got)
	}
	if v, ok := g.gauge("site_router_upgrades_in_progress", nil); !ok || v != 1 {
		t.Errorf("site_router_upgrades_in_progress = %v (%v), want 1", v, ok)
	}
	if got := g.upgrades("started"); got != 1 {
		t.Errorf("started moves = %v, want 1", got)
	}
}

// expectNothingIsTouchedWhileTheDiskIsMade checks that passes while the importer
// works change nothing, and do not count the move again.
func (g *rig) expectNothingIsTouchedWhileTheDiskIsMade() {
	g.t.Helper()
	if got := g.bootsFrom(); got != diskName(imageV1) || !g.exists(gateway.VirtualMachineInstanceGVK, g.names().VM) {
		g.t.Error("the gateway was touched while the disk was being made")
	}
	if got := g.upgrades("started"); got != 1 {
		g.t.Errorf("a move that was already under way was counted again: %v", got)
	}
}

// expectTheGatewayIsBootedFromTheNewDisk runs the pass once the disk is ready and
// checks the second step: the VM is pointed at the new disk, its instance is
// deleted, and nothing that is still needed goes.
func (g *rig) expectTheGatewayIsBootedFromTheNewDisk() {
	g.t.Helper()
	t := g.t
	oldDisk, oldSeed := diskOf(name, imageV1)
	before := len(g.spy.revisions())
	g.reconcile()
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionFalse, v1alpha1.ReasonUpgrading)
	g.expect(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonUpgrading)
	if got := g.bootsFrom(); got != diskName(imageV2) {
		t.Errorf("the VM boots from %s, want %s", got, diskName(imageV2))
	}
	if g.exists(gateway.VirtualMachineInstanceGVK, g.names().VM) {
		t.Error("the instance of the VM was not deleted, so it would not boot from the new disk")
	}
	if !g.exists(gateway.DataVolumeGVK, oldDisk) || !g.secretExists(oldSeed) {
		t.Error("the old disk and seed went before the gateway ran on the new one")
	}
	if got := g.sr().Status.Engine; got.Version != "1.0" {
		t.Errorf("engine = %+v: the gateway has not booted from the new disk", got)
	}
	// Nothing is asked of a guest that is going.
	if got := len(g.spy.revisions()); got != before {
		t.Errorf("the engine was asked %d times while the gateway restarted", got-before)
	}
	// The routes stay: the tunnel is down for as long as the gateway takes, and the
	// routes are back in place when it is up.
	if got := g.asked(); !slices.Equal(got, []string{"192.0.2.0/24"}) {
		t.Errorf("the Forwarder asks for %v while the gateway restarts", got)
	}

	// Still restarting: the place is held, and the gateway is not said to run the
	// new image before it does.
	g.reconcile()
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionFalse, v1alpha1.ReasonUpgrading)
	if got := g.sr().Status.Engine; got.Version != "1.0" {
		t.Errorf("engine = %+v while the gateway restarts: it has not booted on the new image", got)
	}
	if v, _ := g.gauge("site_router_upgrades_in_progress", nil); v != 1 {
		t.Errorf("site_router_upgrades_in_progress = %v while the gateway restarts, want 1", v)
	}
}

// expectTheGatewayRunsOnTheNewImage checks the third step: it runs on the new disk,
// what it no longer runs from is deleted, and the place is free.
func (g *rig) expectTheGatewayRunsOnTheNewImage() {
	g.t.Helper()
	t := g.t
	oldDisk, oldSeed := diskOf(name, imageV1)
	newDisk, newSeed := diskOf(name, imageV2)
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionTrue, "Running")
	if got := g.sr().Status.Engine; got.Version != "2.0" || got.ImageDigest != digestV2 {
		t.Errorf("engine = %+v, want the second version", got)
	}
	if g.exists(gateway.DataVolumeGVK, oldDisk) || g.secretExists(oldSeed) {
		t.Error("the old disk and seed were not deleted once the gateway ran on the new one")
	}
	if !g.exists(gateway.DataVolumeGVK, newDisk) || !g.secretExists(newSeed) {
		t.Error("what the gateway runs from was deleted")
	}
	if !g.secretExists(g.names().APIKey) || !g.secretExists(g.names().PSK) {
		t.Error("the credentials or the key were deleted with the old seed")
	}
	if v, _ := g.gauge("site_router_upgrades_in_progress", nil); v != 0 {
		t.Errorf("site_router_upgrades_in_progress = %v, want 0", v)
	}
	if got := g.upgrades("completed"); got != 1 {
		t.Errorf("completed moves = %v, want 1", got)
	}
	// The guest that took the last configuration is gone, so the next is applied whole.
	revs := g.spy.revisions()
	if last := revs[len(revs)-1]; last != "" {
		t.Errorf("the engine was told the new guest has the revision %q", last)
	}
	g.sync()
	g.reconcile()
	g.expect(v1alpha1.ConditionReady, metav1.ConditionTrue, "AllConditionsMet")

	var started, completed bool
	for _, e := range g.events() {
		started = started || strings.HasPrefix(e, "Normal UpgradeStarted ")
		completed = completed || strings.HasPrefix(e, "Normal UpgradeCompleted ")
	}
	if !started || !completed {
		t.Errorf("events: started %v, completed %v", started, completed)
	}
}

// diskName is the name of the boot disk of an image, for the SiteRouter of most
// tests.
func diskName(image string) string {
	d, _ := diskOf(name, image)
	return d
}

// With the policy that moves a gateway only when it is told to, nothing moves until
// the annotation names a version, and what it names wins whatever the latest is.
func TestAnAnnotationMovesAGatewayUnderTheAnnotationPolicy(t *testing.T) {
	g := newRig(t, func(cfg *config.Config, sr *v1alpha1.SiteRouter) {
		cfg.Upgrade.Policy = config.UpgradeAnnotation
		sr.Annotations = map[string]string{v1alpha1.AnnotationUpgradeTo: "1.0"}
	})
	g.ready()
	oldDisk, _ := diskOf(name, imageV1)
	newDisk, _ := diskOf(name, imageV2)
	if got := g.bootsFrom(); got != oldDisk {
		t.Fatalf("a gateway made with the annotation boots from %s, want %s", got, oldDisk)
	}

	// The annotation is taken away: the latest is the second version, and it is
	// not what the gateway moves to.
	g.edit(func(sr *v1alpha1.SiteRouter) { delete(sr.Annotations, v1alpha1.AnnotationUpgradeTo) })
	g.reconcile()
	if g.exists(gateway.DataVolumeGVK, newDisk) {
		t.Error("a gateway with no annotation was moved under the annotation policy")
	}
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionTrue, "Running")

	g.edit(func(sr *v1alpha1.SiteRouter) { sr.Annotations = map[string]string{v1alpha1.AnnotationUpgradeTo: "2.0"} })
	g.reconcile()
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionTrue, v1alpha1.ReasonUpgrading)
	if !g.exists(gateway.DataVolumeGVK, newDisk) {
		t.Error("the annotation did not start the move")
	}
}

// A version the operator does not list is not one a gateway is moved to. It is said
// once, and the gateway stays where it is.
func TestAnAnnotationThatNamesAVersionThatIsNotListedIsSaidOnce(t *testing.T) {
	g := newRig(t, func(cfg *config.Config, _ *v1alpha1.SiteRouter) {
		cfg.Upgrade.Policy = config.UpgradeAnnotation
	})
	g.ready()
	g.events()

	say := func() int {
		n := 0
		for _, e := range g.events() {
			if strings.HasPrefix(e, "Warning UpgradeVersionUnsupported ") {
				n++
			}
		}
		return n
	}
	g.edit(func(sr *v1alpha1.SiteRouter) { sr.Annotations = map[string]string{v1alpha1.AnnotationUpgradeTo: "9.9"} })
	g.reconcile()
	if n := say(); n != 1 {
		t.Errorf("%d warnings for an annotation that names a version that is not listed, want 1", n)
	}
	g.reconcile()
	g.reconcile()
	if n := say(); n != 0 {
		t.Errorf("the warning was repeated %d times", n)
	}
	g.expect(v1alpha1.ConditionReady, metav1.ConditionTrue, "AllConditionsMet")

	g.edit(func(sr *v1alpha1.SiteRouter) { sr.Annotations = map[string]string{v1alpha1.AnnotationUpgradeTo: "8.8"} })
	g.reconcile()
	if n := say(); n != 1 {
		t.Errorf("%d warnings for another version that is not listed, want 1", n)
	}

	// Once it is taken away and put back it is said again: it is a new mistake.
	g.edit(func(sr *v1alpha1.SiteRouter) { delete(sr.Annotations, v1alpha1.AnnotationUpgradeTo) })
	g.reconcile()
	g.edit(func(sr *v1alpha1.SiteRouter) { sr.Annotations = map[string]string{v1alpha1.AnnotationUpgradeTo: "8.8"} })
	g.reconcile()
	if n := say(); n != 1 {
		t.Errorf("%d warnings for the same version put back, want 1", n)
	}
}

// A release of the image must not take down every tunnel at once: only as many
// gateways move as the configuration allows, and the others run as they are until
// a place is free.
func TestOnlyAsManyGatewaysMoveAsAreAllowed(t *testing.T) {
	g := newRig(t, onFirstVersion, func(cfg *config.Config, _ *v1alpha1.SiteRouter) { cfg.Upgrade.MaxConcurrent = 1 })
	other := g.sibling()
	g.ready()
	other.ready()

	g.latest(func(cfg *config.Config) { cfg.Upgrade.MaxConcurrent = 1 })
	other.follow(g)
	g.reconcile()
	other.reconcile()

	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionTrue, v1alpha1.ReasonUpgrading)
	other.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionTrue, "Running")
	otherDisk, _ := diskOf("head-office", imageV2)
	if other.exists(gateway.DataVolumeGVK, otherDisk) {
		t.Error("a disk was made for a second gateway while the only place was taken")
	}
	if v, ok := g.gauge("site_router_upgrade_waiting", map[string]string{"name": "head-office"}); !ok || v != 1 {
		t.Errorf("site_router_upgrade_waiting = %v (%v), want 1 for the gateway that waits", v, ok)
	}
	if v, _ := g.gauge("site_router_upgrades_in_progress", nil); v != 1 {
		t.Errorf("site_router_upgrades_in_progress = %v, want 1", v)
	}

	// The first one finishes, and the place is free.
	newDisk, _ := diskOf(name, imageV2)
	g.setDV(newDisk, "Succeeded")
	g.reconcile()
	other.reconcile()
	if other.exists(gateway.DataVolumeGVK, otherDisk) {
		t.Error("a second gateway was given a place while the first was restarting")
	}
	g.reboot()
	g.reconcile()
	other.reconcile()
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionTrue, "Running")
	other.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionTrue, v1alpha1.ReasonUpgrading)
	if !other.exists(gateway.DataVolumeGVK, otherDisk) {
		t.Error("the second gateway was not given the place when it was free")
	}
	if v, ok := g.gauge("site_router_upgrade_waiting", map[string]string{"name": "head-office"}); !ok || v != 0 {
		t.Errorf("site_router_upgrade_waiting = %v (%v), want 0 once it has a place", v, ok)
	}
}

// A place that was taken before the operator restarted is taken after it: the
// status says which gateways were moving, and a gateway that is not may not start
// because the operator has forgotten it.
func TestThePlacesOfMovesSurviveARestartOfTheOperator(t *testing.T) {
	g := newRig(t, onFirstVersion, func(cfg *config.Config, _ *v1alpha1.SiteRouter) { cfg.Upgrade.MaxConcurrent = 1 })
	other := g.sibling()
	g.ready()
	other.ready()
	g.latest(func(cfg *config.Config) { cfg.Upgrade.MaxConcurrent = 1 })
	g.reconcile()
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionTrue, v1alpha1.ReasonUpgrading)

	// The operator starts again, and the gateway that waits is the first it looks at.
	g.restart(func(*config.Config) {})
	other.follow(g)
	other.reconcile()

	otherDisk, _ := diskOf("head-office", imageV2)
	if other.exists(gateway.DataVolumeGVK, otherDisk) {
		t.Error("a second gateway started to move: the operator forgot the place that was taken")
	}
	g.reconcile()
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionTrue, v1alpha1.ReasonUpgrading)
}

// An image that does not import in time is given up on: the gateway stays on the
// image it has, the place is free, and the disk is left for the importer, which
// may yet finish it.
func TestAnImageThatDoesNotImportInTimeIsGivenUpOn(t *testing.T) {
	g := newRig(t, onFirstVersion)
	g.ready()
	oldDisk, _ := diskOf(name, imageV1)
	newDisk, _ := diskOf(name, imageV2)

	g.latest()
	g.reconcile()
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionTrue, v1alpha1.ReasonUpgrading)
	g.events()

	g.now = g.now.Add(31 * time.Minute)
	g.reconcile()

	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionTrue, v1alpha1.ReasonUpgradeFailed)
	g.expect(v1alpha1.ConditionReady, metav1.ConditionTrue, "AllConditionsMet")
	if msg := g.cond(v1alpha1.ConditionGatewayReady).Message; !strings.Contains(msg, newDisk) {
		t.Errorf("the message does not say which disk to delete to try again: %q", msg)
	}
	if got := g.bootsFrom(); got != oldDisk || !g.exists(gateway.VirtualMachineInstanceGVK, g.names().VM) {
		t.Error("the gateway was touched by a move that was given up")
	}
	if !g.exists(gateway.DataVolumeGVK, newDisk) {
		t.Error("the disk that was being made was deleted")
	}
	if v, _ := g.gauge("site_router_upgrades_in_progress", nil); v != 0 {
		t.Errorf("site_router_upgrades_in_progress = %v, want the place free", v)
	}
	if got := g.upgrades("failed"); got != 1 {
		t.Errorf("failed moves = %v, want 1", got)
	}
	var warned bool
	for _, e := range g.events() {
		warned = warned || strings.HasPrefix(e, "Warning "+v1alpha1.ReasonUpgradeFailed+" ")
	}
	if !warned {
		t.Error("no warning for the move that was given up")
	}

	// It is said once, not at every pass, and the gateway is not asked to move again.
	g.reconcile()
	if got := g.upgrades("failed"); got != 1 {
		t.Errorf("a move that was given up on was counted again: %v", got)
	}
	if v, _ := g.gauge("site_router_upgrades_in_progress", nil); v != 0 {
		t.Error("a move that was given up on took the place again")
	}

	// The importer gets there after all, and the move goes on.
	g.setDV(newDisk, "Succeeded")
	g.reconcile()
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionFalse, v1alpha1.ReasonUpgrading)
	if got := g.bootsFrom(); got != newDisk {
		t.Errorf("the VM boots from %s, want %s", got, newDisk)
	}
}

// An importer that says it has failed is not waited on.
func TestAnImporterThatFailsIsNotWaitedOn(t *testing.T) {
	g := newRig(t, onFirstVersion)
	g.ready()
	newDisk, _ := diskOf(name, imageV2)
	g.latest()
	g.reconcile()

	g.setDV(newDisk, "Failed")
	g.reconcile()
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionTrue, v1alpha1.ReasonUpgradeFailed)
	if msg := g.cond(v1alpha1.ConditionGatewayReady).Message; !strings.Contains(msg, "Failed") {
		t.Errorf("the message does not say what the importer said: %q", msg)
	}
}

// An image that is not listed any more is the one a gateway runs, and it goes on:
// the operator that dropped it from the list did not ask for the gateway to move.
func TestAGatewayOnAnImageThatIsNoLongerListedGoesOn(t *testing.T) {
	g := newRig(t, onFirstVersion, func(cfg *config.Config, _ *v1alpha1.SiteRouter) { cfg.Upgrade.Policy = config.UpgradeAnnotation })
	g.ready()
	oldDisk, oldSeed := diskOf(name, imageV1)

	g.latest(func(cfg *config.Config) {
		cfg.Upgrade.Policy = config.UpgradeAnnotation
		cfg.Engine.Versions = cfg.Engine.Versions[1:] // only 2.0 is listed
	})
	g.reconcile()
	g.reconcile()

	g.expect(v1alpha1.ConditionReady, metav1.ConditionTrue, "AllConditionsMet")
	if got := g.sr().Status.Engine; got.Version != "1.0" || got.ImageDigest != digestV1 {
		t.Errorf("engine = %+v, want what runs", got)
	}
	if !g.exists(gateway.DataVolumeGVK, oldDisk) || !g.secretExists(oldSeed) || g.bootsFrom() != oldDisk {
		t.Error("what an image that is no longer listed runs from was changed")
	}
	newDisk, _ := diskOf(name, imageV2)
	if g.exists(gateway.DataVolumeGVK, newDisk) {
		t.Error("a disk was made for an image nobody asked the gateway to move to")
	}
}

// A disk that someone else made under the name of the new one is not the operator's
// to use, and a move that cannot go on does not hold a place.
func TestAMoveThatCannotGoOnDoesNotHoldAPlace(t *testing.T) {
	g := newRig(t, onFirstVersion, func(cfg *config.Config, _ *v1alpha1.SiteRouter) { cfg.Upgrade.MaxConcurrent = 1 })
	g.ready()
	newDisk, _ := diskOf(name, imageV2)
	foreign := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}
	foreign.SetGroupVersionKind(gateway.DataVolumeGVK)
	foreign.SetName(newDisk)
	foreign.SetNamespace(g.ns)
	mustCreate(t, foreign)

	g.latest(func(cfg *config.Config) { cfg.Upgrade.MaxConcurrent = 1 })
	// A disk that is not the operator's is not read as one that took too long to
	// import, however old it is.
	g.now = g.now.Add(31 * time.Minute)
	if _, err := g.reconcileErr(); err != nil {
		t.Errorf("a conflict needs a person: %v", err)
	}
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionFalse, v1alpha1.ReasonObjectConflict)
	if v, _ := g.gauge("site_router_upgrades_in_progress", nil); v != 0 {
		t.Errorf("site_router_upgrades_in_progress = %v: a move that cannot go on holds a place", v)
	}
	if got := g.bootsFrom(); got == newDisk {
		t.Error("the VM was pointed at a disk that is not the operator's")
	}
}

// The instance is what runs, whatever the VM says: a VM that has been pointed at a
// new disk and an instance that was not restarted is a gateway that is still on the
// old image, and the restart is done again.
func TestAnInstanceThatWasNotRestartedIsRestarted(t *testing.T) {
	g := newRig(t, onFirstVersion)
	g.ready()
	oldDisk, newDisk := diskName(imageV1), diskName(imageV2)
	g.latest()
	g.reconcile()
	g.setDV(newDisk, "Succeeded")
	g.reconcile()
	if g.bootsFrom() != newDisk {
		t.Fatal("the VM was not pointed at the new disk")
	}

	// KubeVirt had not yet made the instance again, or it was made from the old
	// disk by a race: an instance on the old disk is there.
	g.vmiDisk = oldDisk
	g.reboot()
	g.reconcile()

	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionFalse, v1alpha1.ReasonUpgrading)
	if g.exists(gateway.VirtualMachineInstanceGVK, g.names().VM) {
		t.Error("an instance that runs on the old disk was taken for one on the new")
	}
	if got := g.sr().Status.Engine; got.Version != "1.0" {
		t.Errorf("engine = %+v, want the version that runs", got)
	}
}

// The status is not what says which image runs: a gateway whose status was lost
// is not taken for a new one, which would be made on the latest image with no
// import and no restart.
func TestAGatewayWhoseStatusWasLostIsNotMadeAgainOnTheLatestImage(t *testing.T) {
	oldDisk, newDisk := diskName(imageV1), diskName(imageV2)

	t.Run("the instance is gone", func(t *testing.T) {
		g := newRig(t, onFirstVersion)
		g.ready()
		g.latest()
		g.deleteObject(gateway.VirtualMachineInstanceGVK, g.names().VM)
		g.wipeStatus()
		g.reconcile()
		if got := g.bootsFrom(); got != oldDisk {
			t.Errorf("the VM boots from %s, want %s: the disk of the image it ran", got, oldDisk)
		}
		g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionFalse, v1alpha1.ReasonGatewayPending)
	})

	t.Run("the VM is gone too", func(t *testing.T) {
		g := newRig(t, onFirstVersion)
		g.ready()
		g.latest()
		g.deleteObject(gateway.VirtualMachineInstanceGVK, g.names().VM)
		g.deleteObject(gateway.VirtualMachineGVK, g.names().VM)
		g.reconcile()
		// What the status remembers is all that is left, and the VM is made again with it.
		if got := g.bootsFrom(); got != oldDisk {
			t.Errorf("the VM boots from %s, want %s: the image the status remembers", got, oldDisk)
		}
		g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionFalse, v1alpha1.ReasonGatewayPending)
		if !g.exists(gateway.DataVolumeGVK, newDisk) {
			t.Error("the disk of the latest image was not started, so the gateway is not on its way to it")
		}
	})
}

// A disk that says which image it holds is believed when the operator made it. One
// that someone else made under the name is not: it would say a gateway runs the image
// it is to move to, and the move would not happen.
func TestADiskThatIsNotTheOperatorsDoesNotSayWhatRuns(t *testing.T) {
	g := newRig(t, onFirstVersion)
	g.ready()
	oldDisk := diskName(imageV1)
	g.deleteObject(gateway.DataVolumeGVK, oldDisk)
	claim := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}
	claim.SetGroupVersionKind(gateway.DataVolumeGVK)
	claim.SetName(oldDisk)
	claim.SetNamespace(g.ns)
	claim.SetAnnotations(map[string]string{v1alpha1.AnnotationImageDigest: digestV2})
	mustCreate(t, claim)

	g.latest()
	if _, err := g.reconcileErr(); err != nil {
		t.Errorf("a conflict needs a person: %v", err)
	}
	// The disk is in the way and is said to be. The gateway is not made again on the
	// image the disk claims to hold, which would point the VM at a disk nobody made
	// ready.
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionFalse, v1alpha1.ReasonObjectConflict)
	if got := g.bootsFrom(); got != oldDisk {
		t.Errorf("the VM boots from %s, want %s: a disk that is not the operator's said what runs", got, oldDisk)
	}
	if g.exists(gateway.DataVolumeGVK, diskName(imageV2)) {
		t.Error("a disk was made for an image the gateway was taken to run already")
	}
}

// A place is taken for a move from the first step, so a disk that is ready does not
// let a gateway restart while the places are taken: it waits for one.
func TestAGatewayWithAReadyDiskWaitsForAPlace(t *testing.T) {
	g := newRig(t, onFirstVersion, func(cfg *config.Config, _ *v1alpha1.SiteRouter) { cfg.Upgrade.MaxConcurrent = 1 })
	other := g.sibling()
	g.ready()
	other.ready()
	mine, theirs := diskName(imageV2), func() string { d, _ := diskOf("head-office", imageV2); return d }()

	g.latest(func(cfg *config.Config) { cfg.Upgrade.MaxConcurrent = 1 })
	other.follow(g)
	g.reconcile() // takes the only place
	g.now = g.now.Add(31 * time.Minute)
	g.reconcile() // gives up, and the place is free
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionTrue, v1alpha1.ReasonUpgradeFailed)
	other.reconcile() // takes it
	other.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionTrue, v1alpha1.ReasonUpgrading)

	// The disk of the first becomes ready after all, while the place is not free.
	g.setDV(mine, "Succeeded")
	g.reconcile()
	if got := g.bootsFrom(); got == mine || !g.exists(gateway.VirtualMachineInstanceGVK, g.names().VM) {
		t.Error("a gateway restarted without a place")
	}

	// The other finishes, and the place goes to the first.
	other.setDV(theirs, "Succeeded")
	other.reconcile()
	other.reboot()
	other.reconcile()
	g.reconcile()
	if got := g.bootsFrom(); got != mine {
		t.Errorf("the VM boots from %s, want %s once a place is free", got, mine)
	}
}

// What the operator did not make is not deleted when the disk and the seed of an
// image that is no longer run are, whatever labels and annotations it has.
func TestWhatIsNotTheOperatorsIsLeftWhenTheOldImageGoes(t *testing.T) {
	g := newRig(t, onFirstVersion)
	g.ready()
	foreignDisk := "someone-elses-disk"
	other := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}
	other.SetGroupVersionKind(gateway.DataVolumeGVK)
	other.SetName(foreignDisk)
	other.SetNamespace(g.ns)
	other.SetLabels(gateway.Labels(name))
	other.SetAnnotations(map[string]string{v1alpha1.AnnotationImageDigest: "sha256:" + strings.Repeat("00", 32)})
	mustCreate(t, other)
	foreignSeed := &corev1.Secret{
		Name: "someone-elses-seed", Namespace: g.ns, Labels: gateway.Labels(name),
		Annotations: map[string]string{v1alpha1.AnnotationImageDigest: "sha256:" + strings.Repeat("00", 32)},
	}
	mustCreate(t, foreignSeed)

	g.latest()
	g.reconcile()
	g.setDV(diskName(imageV2), "Succeeded")
	g.reconcile()
	g.reboot()
	g.reconcile()

	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionTrue, "Running")
	if g.exists(gateway.DataVolumeGVK, diskName(imageV1)) {
		t.Error("the old disk was not deleted")
	}
	if !g.exists(gateway.DataVolumeGVK, foreignDisk) || !g.secretExists("someone-elses-seed") {
		t.Error("an object that is not the operator's was deleted with the old image")
	}
}

// A move that is told to stop, by taking its annotation away, is given up and not
// completed: the gateway stays on the image it has and the disk that was made for the
// other is deleted.
func TestAMoveThatIsToldToStopIsNotCompleted(t *testing.T) {
	g := newRig(t, onFirstVersion, func(cfg *config.Config, sr *v1alpha1.SiteRouter) {
		cfg.Upgrade.Policy = config.UpgradeAnnotation
		sr.Annotations = map[string]string{v1alpha1.AnnotationUpgradeTo: "1.0"}
	})
	g.ready()
	g.edit(func(sr *v1alpha1.SiteRouter) { sr.Annotations = map[string]string{v1alpha1.AnnotationUpgradeTo: "2.0"} })
	g.reconcile()
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionTrue, v1alpha1.ReasonUpgrading)
	g.events()

	g.edit(func(sr *v1alpha1.SiteRouter) { delete(sr.Annotations, v1alpha1.AnnotationUpgradeTo) })
	g.reconcile()

	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionTrue, "Running")
	if got := g.upgrades("completed"); got != 0 {
		t.Errorf("completed moves = %v for a move that was given up", got)
	}
	for _, e := range g.events() {
		if strings.HasPrefix(e, "Normal UpgradeCompleted ") {
			t.Errorf("a move that was given up was said to be completed: %q", e)
		}
	}
	if g.exists(gateway.DataVolumeGVK, diskName(imageV2)) {
		t.Error("the disk made for the move that was given up was not deleted")
	}
	if v, _ := g.gauge("site_router_upgrades_in_progress", nil); v != 0 {
		t.Errorf("site_router_upgrades_in_progress = %v, want the place free", v)
	}
}

// A gateway that does not come up on the new image can be sent back with the
// annotation: the old disk is still there until the new image runs, so the way back
// is a restart and not an import, and it is not counted as a move that completed.
func TestAGatewayThatDoesNotComeUpCanBeSentBack(t *testing.T) {
	g := newRig(t, onFirstVersion, func(cfg *config.Config, sr *v1alpha1.SiteRouter) {
		cfg.Upgrade.Policy = config.UpgradeAnnotation
		sr.Annotations = map[string]string{v1alpha1.AnnotationUpgradeTo: "1.0"}
	})
	g.ready()
	oldDisk, newDisk := diskName(imageV1), diskName(imageV2)
	// The importer has long since finished the disk the gateway runs from.
	g.setDV(oldDisk, "Succeeded")
	g.edit(func(sr *v1alpha1.SiteRouter) { sr.Annotations = map[string]string{v1alpha1.AnnotationUpgradeTo: "2.0"} })
	g.reconcile()
	g.setDV(newDisk, "Succeeded")
	g.reconcile()
	if g.bootsFrom() != newDisk {
		t.Fatal("the VM was not pointed at the new disk")
	}
	g.events()

	// The new image does not boot, and the annotation sends the gateway back.
	g.edit(func(sr *v1alpha1.SiteRouter) { sr.Annotations = map[string]string{v1alpha1.AnnotationUpgradeTo: "1.0"} })
	g.reconcile()
	if got := g.bootsFrom(); got != oldDisk {
		t.Errorf("the VM boots from %s, want %s: the disk that is still there", got, oldDisk)
	}
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionFalse, v1alpha1.ReasonUpgrading)
	for _, e := range g.events() {
		if strings.HasPrefix(e, "Normal UpgradeCompleted ") {
			t.Errorf("a gateway that is on its way back was said to have completed a move: %q", e)
		}
	}

	g.reboot()
	g.reconcile()
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionTrue, "Running")
	if got := g.sr().Status.Engine; got.Version != "1.0" || got.ImageDigest != digestV1 {
		t.Errorf("engine = %+v, want the first version", got)
	}
	if g.exists(gateway.DataVolumeGVK, newDisk) {
		t.Error("the disk of the image that did not come up was not deleted")
	}
	if got := g.upgrades("completed"); got != 0 {
		t.Errorf("completed moves = %v for a gateway that went back", got)
	}
}

// What a gateway has not booted on is not given up for what it is to boot on, even
// when the status that says a move is under way is lost.
func TestTheOldDiskStaysUntilTheGatewayRunsOnTheNew(t *testing.T) {
	g := newRig(t, onFirstVersion)
	g.ready()
	g.latest()
	g.reconcile()
	g.setDV(diskName(imageV2), "Succeeded")
	g.reconcile()
	if g.exists(gateway.VirtualMachineInstanceGVK, g.names().VM) {
		t.Fatal("the instance was not deleted")
	}

	g.wipeStatus()
	g.reconcile()
	if !g.exists(gateway.DataVolumeGVK, diskName(imageV1)) {
		t.Error("the old disk was deleted before the gateway ran on the new one")
	}
}

// The places are shared by every namespace, and a gateway that cannot start, for
// want of memory the cluster has, would hold one for ever. It gives its place back
// when it has had the time to boot, says it did not come up, and keeps the old disk to
// go back to; and if it comes up after all, the move is completed.
func TestAGatewayThatDoesNotComeUpDoesNotHoldAPlaceForEver(t *testing.T) {
	g := newRig(t, onFirstVersion, func(cfg *config.Config, _ *v1alpha1.SiteRouter) { cfg.Upgrade.MaxConcurrent = 1 })
	other := g.sibling()
	g.ready()
	other.ready()
	oldDisk, newDisk := diskName(imageV1), diskName(imageV2)
	theirs := func() string { d, _ := diskOf("head-office", imageV2); return d }()

	g.latest(func(cfg *config.Config) { cfg.Upgrade.MaxConcurrent = 1 })
	other.follow(g)
	g.reconcile()
	g.setDV(newDisk, "Succeeded")
	g.reconcile() // pointed at the new disk, and the instance is deleted
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionFalse, v1alpha1.ReasonUpgrading)

	// Within the time it has the place.
	g.now = g.now.Add(10 * time.Minute)
	g.reconcile()
	other.reconcile()
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionFalse, v1alpha1.ReasonUpgrading)
	if other.exists(gateway.DataVolumeGVK, theirs) {
		t.Error("a second gateway was given a place that the first still had time to use")
	}

	// Past the time it gives the place back.
	g.now = g.now.Add(10 * time.Minute)
	g.events()
	g.reconcile()
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionFalse, v1alpha1.ReasonUpgradeFailed)
	g.expect(v1alpha1.ConditionReady, metav1.ConditionFalse, v1alpha1.ReasonUpgradeFailed)
	if msg := g.cond(v1alpha1.ConditionGatewayReady).Message; !strings.Contains(msg, v1alpha1.AnnotationUpgradeTo) {
		t.Errorf("the message does not say how to send the gateway back: %q", msg)
	}
	if v, _ := g.gauge("site_router_upgrades_in_progress", nil); v != 0 {
		t.Errorf("site_router_upgrades_in_progress = %v, want the place free", v)
	}
	if got := g.upgrades("failed"); got != 1 {
		t.Errorf("failed moves = %v, want 1", got)
	}
	var warned bool
	for _, e := range g.events() {
		warned = warned || strings.HasPrefix(e, "Warning "+v1alpha1.ReasonUpgradeFailed+" ")
	}
	if !warned {
		t.Error("no warning for a gateway that did not come up")
	}
	if !g.exists(gateway.DataVolumeGVK, oldDisk) {
		t.Error("the old disk was deleted, so there is nothing to go back to")
	}
	if got := g.sr().Status.Engine; got.Version != "1.0" {
		t.Errorf("engine = %+v: the gateway has not booted on the new image", got)
	}
	g.reconcile()
	if got := g.upgrades("failed"); got != 1 {
		t.Errorf("the failure was counted again: %v", got)
	}

	// The place is free for another gateway.
	other.reconcile()
	other.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionTrue, v1alpha1.ReasonUpgrading)
	if !other.exists(gateway.DataVolumeGVK, theirs) {
		t.Error("the place was not given to the gateway that waited")
	}

	// It comes up after all.
	g.reboot()
	g.reconcile()
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionTrue, "Running")
	if got := g.sr().Status.Engine; got.Version != "2.0" || got.ImageDigest != digestV2 {
		t.Errorf("engine = %+v, want the second version", got)
	}
	if g.exists(gateway.DataVolumeGVK, oldDisk) {
		t.Error("the old disk was kept after the gateway ran on the new one")
	}
	if got := g.upgrades("completed"); got != 1 {
		t.Errorf("completed moves = %v, want 1", got)
	}
}

// A gateway that did not come up and is sent back is on its way and not a failure,
// whatever else it has to wait for: here the disk it goes back to is gone and is
// made again.
func TestAGatewayThatGaveUpIsNotCalledFailedWhileItIsSentBack(t *testing.T) {
	g := newRig(t, onFirstVersion, func(cfg *config.Config, sr *v1alpha1.SiteRouter) {
		cfg.Upgrade.Policy = config.UpgradeAnnotation
		sr.Annotations = map[string]string{v1alpha1.AnnotationUpgradeTo: "1.0"}
	})
	g.ready()
	g.edit(func(sr *v1alpha1.SiteRouter) { sr.Annotations = map[string]string{v1alpha1.AnnotationUpgradeTo: "2.0"} })
	g.reconcile()
	g.setDV(diskName(imageV2), "Succeeded")
	g.reconcile()
	g.now = g.now.Add(16 * time.Minute)
	g.reconcile()
	g.expect(v1alpha1.ConditionGatewayReady, metav1.ConditionFalse, v1alpha1.ReasonUpgradeFailed)

	g.deleteObject(gateway.DataVolumeGVK, diskName(imageV1))
	g.edit(func(sr *v1alpha1.SiteRouter) { sr.Annotations = map[string]string{v1alpha1.AnnotationUpgradeTo: "1.0"} })
	g.reconcile()

	if c := g.cond(v1alpha1.ConditionGatewayReady); c.Reason == v1alpha1.ReasonUpgradeFailed {
		t.Errorf("a gateway that is being sent back is called failed: %s (%s)", statusOf(c), c.Message)
	}
	if !g.exists(gateway.DataVolumeGVK, diskName(imageV1)) {
		t.Error("the disk of the version it goes back to was not made again")
	}
	if v, _ := g.gauge("site_router_upgrades_in_progress", nil); v != 1 {
		t.Errorf("site_router_upgrades_in_progress = %v, want the place taken for the way back", v)
	}
}
