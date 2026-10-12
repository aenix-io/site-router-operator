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

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/aenix-io/site-router-operator/api/v1alpha1"
	"github.com/aenix-io/site-router-operator/internal/gateway"
	"github.com/aenix-io/site-router-operator/internal/rollout"
)

// A move to another image is the one planned operation that drops the tunnel of
// a gateway. It goes in steps, each of which is read from the objects in the
// cluster and not remembered, so that an operator that restarts, or a pass that
// fails half way, goes on from where the gateway is:
//
//  1. The disk of the other image and the seed for it are made beside those of the
//     image the gateway runs. They are never changed, so the gateway is not
//     touched, and it goes on being configured and routed as before.
//  2. Once the disk is imported, the VM is pointed at it and its instance is
//     deleted, so that KubeVirt boots it from the new disk.
//  3. Once the instance runs on the new disk, the disk and the seed of the old
//     image are deleted.
//
// A move holds one of the places the configuration allows from the first step to
// the third, so that a release of the image does not take down every tunnel at
// once. A gateway that is told to move and has no place waits, running as it is.
// An image that does not import in time is given up on: the gateway stays on the
// image it has, the place is freed, and the disk that was made is left for the
// importer to go on with, and for whoever deletes it to try again.

// movePhase is where a gateway is in a move to another image.
type movePhase int

const (
	// movePhaseNone: the gateway is not moving.
	movePhaseNone movePhase = iota
	// movePhaseWaiting: the gateway is to move and has no place yet.
	movePhaseWaiting
	// movePhaseImporting: the disk of the other image is being made.
	movePhaseImporting
	// movePhaseSwap: the disk is ready and the gateway is to boot from it.
	movePhaseSwap
	// movePhaseFailed: the disk did not become ready in time, and the move is
	// given up.
	movePhaseFailed
)

// DataVolume phases the operator reads. Anything else is a disk that is not ready.
const (
	dvSucceeded = "Succeeded"
	dvFailed    = "Failed"
)

// Events of a move.
const (
	eventUpgradeStarted     = "UpgradeStarted"
	eventUpgradeCompleted   = "UpgradeCompleted"
	eventUpgradeUnsupported = "UpgradeVersionUnsupported"
	actionUpgrade           = "Upgrade"
)

// note is an event a pass has to say, which no change of a condition says.
type note struct{ eventType, reason, message string }

// current is the image the gateway runs, as the objects say it: the disk its
// instance boots from, else the disk its VM is pointed at, else what the status
// remembers, which is what is left once the VM is gone. A gateway that has none of
// them is being made, which is the zero Current.
func (r *Reconciler) current(ctx context.Context, sr *v1alpha1.SiteRouter, vm *unstructured.Unstructured, vmi *vmiFacts) (rollout.Current, error) {
	disk := ""
	if vmi != nil {
		disk = vmi.BootDisk
	}
	if disk == "" && vm != nil {
		disk = bootDiskOfVM(vm)
	}
	digest := ""
	if disk != "" {
		dv, err := r.getObject(ctx, gateway.DataVolumeGVK, sr.Namespace, disk)
		if err != nil {
			return rollout.Current{}, err
		}
		if dv != nil && metav1.IsControlledBy(dv, sr) {
			digest = dv.GetAnnotations()[v1alpha1.AnnotationImageDigest]
		}
	}
	if digest == "" {
		digest = sr.Status.Engine.ImageDigest
	}
	if digest == "" {
		return rollout.Current{}, nil
	}
	return rollout.Current{Version: r.versionOf(sr, digest), Digest: digest}, nil
}

// versionOf names the version of the image with the digest: the one the status
// remembers when it is the same image, else the first the configuration lists
// with it, else the last the status knew. A gateway on an image no version is
// listed for is not one the operator can name, and is kept as it is.
func (r *Reconciler) versionOf(sr *v1alpha1.SiteRouter, digest string) string {
	if e := sr.Status.Engine; e.ImageDigest == digest && e.Version != "" {
		return e.Version
	}
	for _, v := range r.config.Engine.Versions {
		if d, err := gateway.ImageDigest(v.Image); err == nil && d == digest {
			return v.Name
		}
	}
	if sr.Status.Engine.Version != "" {
		return sr.Status.Engine.Version
	}
	return "unlisted"
}

// planMove says where a gateway that is to move to the image of dec is in the move,
// and takes or gives up its place for that. The message is for the condition.
func (r *Reconciler) planMove(ctx context.Context, sr *v1alpha1.SiteRouter, dec rollout.Decision) (movePhase, string, error) {
	key := client.ObjectKeyFromObject(sr)
	newDisk := gateway.NamesFor(sr.Name).ForImage(dec.Image).BootDisk
	dv, err := r.getObject(ctx, gateway.DataVolumeGVK, sr.Namespace, newDisk)
	if err != nil {
		return movePhaseNone, "", err
	}
	// A disk that is not the SiteRouter's is for the applier to name.
	if dv != nil && !metav1.IsControlledBy(dv, sr) {
		dv = nil
	}

	phase := ""
	if dv != nil {
		phase, _, _ = unstructured.NestedString(dv.Object, "status", "phase")
	}
	switch {
	case dv != nil && phase == dvSucceeded:
		if !r.gate.Acquire(key) {
			return movePhaseWaiting, "", nil
		}
		return movePhaseSwap, fmt.Sprintf("restarting on version %s", dec.Target), nil
	case dv != nil && (phase == dvFailed || r.now().Sub(dv.GetCreationTimestamp().Time) > r.config.Upgrade.ImportTimeout.Duration):
		r.gate.Release(key)
		return movePhaseFailed, fmt.Sprintf("the disk of version %s could not be made ready (%s), so the gateway stays on version %s; "+
			"the importer goes on trying, and deleting the DataVolume %s makes a new attempt",
			dec.Target, describe(phase), sr.Status.Engine.Version, newDisk), nil
	}
	if !r.gate.Acquire(key) {
		return movePhaseWaiting, "", nil
	}
	return movePhaseImporting, fmt.Sprintf("making the disk of version %s; the gateway runs on as it is", dec.Target), nil
}

func describe(phase string) string {
	if phase == "" {
		return "not imported in the time allowed"
	}
	return "phase " + phase
}

// boot points the gateway at the disk of another image: the VM is applied with it,
// and its instance is deleted, which KubeVirt answers by booting the VM again.
// The revision the engine applied is forgotten, since the guest that took it is
// gone.
func (r *Reconciler) boot(ctx context.Context, sr *v1alpha1.SiteRouter, vmi *vmiFacts, newDisk string) error {
	if vmi != nil && vmi.BootDisk != newDisk {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gateway.VirtualMachineInstanceGVK)
		u.SetNamespace(sr.Namespace)
		u.SetName(gateway.NamesFor(sr.Name).VM)
		if err := client.IgnoreNotFound(r.client.Delete(ctx, u)); err != nil {
			return fmt.Errorf("restarting the gateway: %w", err)
		}
	}
	r.revisions.forget(client.ObjectKeyFromObject(sr))
	return nil
}

// cleanup deletes the disk and the seed of the images the gateway does not run
// any more, which are the ones that carry the digest of an image and are not the
// ones that are in use. What the operator did not make is not touched.
func (r *Reconciler) cleanup(ctx context.Context, sr *v1alpha1.SiteRouter, keep gateway.Names) error {
	selector := client.MatchingLabels(gateway.Labels(sr.Name))

	disks := &unstructured.UnstructuredList{}
	disks.SetGroupVersionKind(gateway.DataVolumeGVK.GroupVersion().WithKind("DataVolumeList"))
	if err := r.reader.List(ctx, disks, client.InNamespace(sr.Namespace), selector); err != nil {
		return fmt.Errorf("listing the disks: %w", err)
	}
	for i := range disks.Items {
		d := &disks.Items[i]
		if !superseded(d, sr, keep.BootDisk) {
			continue
		}
		if err := client.IgnoreNotFound(r.client.Delete(ctx, d)); err != nil {
			return fmt.Errorf("deleting the disk %s: %w", d.GetName(), err)
		}
	}

	seeds := &corev1.SecretList{}
	if err := r.reader.List(ctx, seeds, client.InNamespace(sr.Namespace), selector); err != nil {
		return fmt.Errorf("listing the seeds: %w", err)
	}
	for i := range seeds.Items {
		s := &seeds.Items[i]
		if !superseded(s, sr, keep.CloudInit) {
			continue
		}
		if err := client.IgnoreNotFound(r.client.Delete(ctx, s)); err != nil {
			return fmt.Errorf("deleting the seed %s: %w", s.GetName(), err)
		}
	}
	return nil
}

// superseded says whether an object is the SiteRouter's, was made for an image, and
// is not the one named. The credentials and the key carry no digest and are never
// superseded.
func superseded(o client.Object, sr *v1alpha1.SiteRouter, current string) bool {
	return metav1.IsControlledBy(o, sr) &&
		o.GetAnnotations()[v1alpha1.AnnotationImageDigest] != "" &&
		o.GetName() != current
}

// moving says whether the status has the gateway in a move: the marker is the
// reason Upgrading on GatewayReady, which is what an operator that starts reads to
// know which places are taken.
func moving(conds []metav1.Condition) bool {
	c := meta.FindStatusCondition(conds, v1alpha1.ConditionGatewayReady)
	return c != nil && c.Reason == v1alpha1.ReasonUpgrading
}

// restoreGate takes the places of the moves that were under way before the
// operator started, which the statuses say. It runs before the first pass, and is
// tried again until it has been able to read them: a pass that ran with the places
// of moves it did not know of would let more gateways move than the configuration
// allows.
func (r *Reconciler) restoreGate(ctx context.Context) error {
	r.restoreMu.Lock()
	defer r.restoreMu.Unlock()
	if r.restored {
		return nil
	}
	list := &v1alpha1.SiteRouterList{}
	if err := r.client.List(ctx, list); err != nil {
		return fmt.Errorf("reading which gateways are moving: %w", err)
	}
	var keys []types.NamespacedName
	for i := range list.Items {
		if sr := &list.Items[i]; sr.DeletionTimestamp.IsZero() && moving(sr.Status.Conditions) {
			keys = append(keys, client.ObjectKeyFromObject(sr))
		}
	}
	r.gate.Restore(keys)
	r.restored = true
	logf.FromContext(ctx).Info("the places of the moves that were under way are taken", "moving", len(keys))
	return nil
}

// warnUnsupported says once that the annotation names a version the operator does
// not list, and not again until it names another or is removed.
func (r *Reconciler) warnUnsupported(key types.NamespacedName, dec rollout.Decision, g *gatewayView) {
	r.unsupportedMu.Lock()
	defer r.unsupportedMu.Unlock()
	if dec.Unsupported == "" {
		delete(r.unsupported, key)
		return
	}
	if r.unsupported[key] == dec.Unsupported {
		return
	}
	r.unsupported[key] = dec.Unsupported
	g.notes = append(g.notes, note{
		eventType: corev1.EventTypeWarning, reason: eventUpgradeUnsupported,
		message: fmt.Sprintf("the annotation %s names the version %q, which the operator does not list; the gateway stays on version %s",
			v1alpha1.AnnotationUpgradeTo, dec.Unsupported, dec.Target),
	})
}
