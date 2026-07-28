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
	if !IsPreProvisionedContent(vsc) {
		if err := setOriginVSCToDelete(ctx, c, vsc); err != nil {
			return err
		}
	}
	if err := c.Delete(ctx, vsc); err != nil && !absentOK(err) {
		return fmt.Errorf("delete orphaned VolumeSnapshotContent %s: %w", vsc.GetName(), err)
	}
	return nil
}
