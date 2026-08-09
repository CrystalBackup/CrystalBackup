/*
Copyright 2026.

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

package exposer

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// VolumeSnapshotList and VolumeSnapshotContentList return empty unstructured lists stamped with
// the snapshot.storage.k8s.io GVKs, for callers OUTSIDE this package — the orphan reaper — that
// must LIST snapshot objects without duplicating the group/version literals this package owns.
func VolumeSnapshotList() *unstructured.UnstructuredList {
	return newUnstructuredList(volumeSnapshotGVK())
}

// VolumeSnapshotContentList is VolumeSnapshotList's cluster-scoped sibling.
func VolumeSnapshotContentList() *unstructured.UnstructuredList {
	return newUnstructuredList(volumeSnapshotContentGVK())
}

// IsPreProvisionedContent reports whether vsc is a PRE-PROVISIONED VolumeSnapshotContent — one
// with spec.source.snapshotHandle set, the shape this package's static re-bind creates — as
// opposed to a dynamically-provisioned content (spec.source.volumeHandle) the external
// snapshot-controller generated for an origin VolumeSnapshot. The distinction decides reap
// semantics: a static content is an ALIAS whose object-only delete is always safe (Retain by
// construction, the backend snapshot is the origin content's to reclaim), while a dynamic
// content OWNS the backend snapshot and must be reclaimed policy-correctly.
func IsPreProvisionedContent(vsc *unstructured.Unstructured) bool {
	handle, found, err := unstructured.NestedString(vsc.Object, "spec", "source", "snapshotHandle")
	return err == nil && found && handle != ""
}

// ReapOrphanVolumeSnapshotContent removes ONE labelled, already-vetted-as-orphaned
// VolumeSnapshotContent with cleanup()'s exact policy semantics, for the orphan reaper:
//
//   - pre-provisioned (our static re-bind alias): delete the OBJECT only — cleanup step 2;
//   - dynamically-provisioned (the Retain-parked origin content, the leak audit's residual
//     shape): restore deletionPolicy to Delete FIRST, then delete, so the CSI snapshotter
//     reclaims the storage-side snapshot exactly once — cleanup step 3's reclaim, exactly as
//     reclaimOrphanOriginVSC performs it.
//
// The caller owns the orphan decision (owner gone / volume terminal, MinAge passed); this
// function owns only the how. Idempotent and NotFound-tolerant like every teardown step.
func ReapOrphanVolumeSnapshotContent(ctx context.Context, c client.Client, vsc *unstructured.Unstructured) error {
	if err := PrepareOrphanVolumeSnapshotContentForReclaim(ctx, c, vsc); err != nil {
		return err
	}
	if err := c.Delete(ctx, vsc); err != nil && !absentOK(err) {
		return fmt.Errorf("delete orphaned VolumeSnapshotContent %s: %w", vsc.GetName(), err)
	}
	return nil
}

// PrepareOrphanVolumeSnapshotContentForReclaim is the NON-DESTRUCTIVE half of
// ReapOrphanVolumeSnapshotContent, split out so a caller that must NOT re-issue a delete can still
// perform it: it restores a dynamically-provisioned content's deletionPolicy to Delete (idempotent,
// a no-op when already Delete) and does nothing at all to a pre-provisioned static alias, whose
// Retain is correct by construction.
//
// The split exists because of a case the 0.6.5 reporting fix would otherwise have regressed. The
// reaper no longer re-issues a DELETE on an object whose deletion is already pending — re-asking
// cannot move a finalizer, and the repeated request is what produced 31 hours of false success
// claims. But an origin content can be found ALREADY terminating and still Retain-parked: the crash
// window reclaimOrigin describes (something else deleted the content after Ready's handover patch,
// before Cleanup restored the policy), with a finalizer holding the object meanwhile. Skipping this
// restore on that object means that the moment the finalizer clears, the content disappears with
// deletionPolicy=Retain and its storage-side snapshot is orphaned in the backend forever — an
// invisible, billable leak, and precisely the failure the reaper exists to prevent.
//
// So this half runs on EVERY sweep, pending deletion or not. It is a write, which is why it is
// deliberately narrow: one merge patch of one field, only on a content the caller has already vetted
// as orphaned, and only when the field is not already what it needs to be. Patching a terminating
// object is permitted (unlike ADDING a finalizer to one), and it does not interfere with whatever
// controller is holding it.
func PrepareOrphanVolumeSnapshotContentForReclaim(ctx context.Context, c client.Client, vsc *unstructured.Unstructured) error {
	if IsPreProvisionedContent(vsc) {
		return nil
	}
	return setOriginVSCToDelete(ctx, c, vsc)
}
