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

package discovery

import (
	"reflect"
	"testing"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// dataSnap builds a data snapshot with the pinned tag identity.
func dataSnap(id, ns, pvc, run string) restic.Snapshot {
	return restic.Snapshot{
		ID:    id,
		Paths: []string{"/data/" + ns + "/" + pvc},
		Tags: []string{
			restic.TagBase,
			restic.Tag(restic.TagKeyKind, restic.KindData),
			restic.Tag(restic.TagKeyNamespace, ns),
			restic.Tag(restic.TagKeyPVC, pvc),
			restic.Tag(restic.TagKeyRun, run),
		},
	}
}

// manifestsSnap builds a manifests snapshot (no pvc= tag): contributes no volume.
func manifestsSnap(id, ns, run string) restic.Snapshot {
	return restic.Snapshot{
		ID:    id,
		Paths: []string{"/manifests/" + ns},
		Tags: []string{
			restic.TagBase,
			restic.Tag(restic.TagKeyKind, restic.KindManifests),
			restic.Tag(restic.TagKeyNamespace, ns),
			restic.Tag(restic.TagKeyRun, run),
		},
	}
}

func TestVolumesFromSnapshots(t *testing.T) {
	// Two data snapshots (out of PVC order) plus a manifests snapshot for the same group.
	snaps := []restic.Snapshot{
		dataSnap("id-web", "c-db", "web-data", "R"),
		manifestsSnap("id-man", "c-db", "R"),
		dataSnap("id-app", "c-db", "app-data", "R"),
	}

	got := VolumesFromSnapshots(snaps)
	want := []cbv1.VolumeStatus{
		{Pvc: "app-data", SnapshotID: "id-app", Phase: status.VolumePhaseCompleted},
		{Pvc: "web-data", SnapshotID: "id-web", Phase: status.VolumePhaseCompleted},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("VolumesFromSnapshots = %#v, want %#v (one Completed volume per data snapshot, sorted, manifests excluded)", got, want)
	}
}

func TestVolumesFromSnapshotsSkipsUntaggedData(t *testing.T) {
	// A data snapshot missing its pvc= tag must be skipped, not projected under "".
	bad := restic.Snapshot{
		ID:   "id-bad",
		Tags: []string{restic.TagBase, restic.Tag(restic.TagKeyKind, restic.KindData), restic.Tag(restic.TagKeyNamespace, "c-db")},
	}
	if got := VolumesFromSnapshots([]restic.Snapshot{bad}); len(got) != 0 {
		t.Fatalf("VolumesFromSnapshots kept a data snapshot without a pvc= tag: %#v", got)
	}
}

func TestVolumesFromSnapshotsEmpty(t *testing.T) {
	if got := VolumesFromSnapshots(nil); got != nil {
		t.Fatalf("VolumesFromSnapshots(nil) = %#v, want nil", got)
	}
}

func TestDistinctNamespaces(t *testing.T) {
	snaps := []restic.Snapshot{
		dataSnap("a", "c-db", "d", "R"),
		manifestsSnap("b", "c-db", "R"),
		dataSnap("c", "c-media", "d", "R"),
		// a cluster-manifests snapshot carries no namespace= tag → not a namespace.
		{ID: "cm", Tags: []string{restic.TagBase, restic.Tag(restic.TagKeyKind, restic.KindClusterManifests), restic.Tag(restic.TagKeyRun, "R")}},
	}
	if got := DistinctNamespaces(snaps); got != 2 {
		t.Fatalf("DistinctNamespaces = %d, want 2 (c-db, c-media; cluster-manifests excluded)", got)
	}
}

// ---------------------------------------------------------------------------
// MergeProjectedVolumes — the projection COMPLETES an execution report, never
// replaces it. Each case below is a shape that was observed being destroyed on a
// live cluster, or the GC behaviour that must survive the fix.
// ---------------------------------------------------------------------------

// TestMergeKeepsSkippedVolumeWithNoSnapshot is the regression: a Skipped volume with its
// CSISnapshotUnsupported reason has NO data snapshot by definition (nothing was
// snapshotted), so the repository-derived list cannot contain it. Replacing the list wiped
// it — and with it the only record anywhere that a PVC was not backed up.
func TestMergeKeepsSkippedVolumeWithNoSnapshot(t *testing.T) {
	recorded := []cbv1.VolumeStatus{
		{Pvc: "data", Phase: status.VolumePhaseCompleted, SnapshotID: "s1", AddedBytes: 4096, SizeBytes: 8192, Node: "node-a"},
		{Pvc: "scratch", Phase: status.VolumePhaseSkipped, Reason: "CSISnapshotUnsupported"},
	}
	derived := []cbv1.VolumeStatus{{Pvc: "data", Phase: status.VolumePhaseCompleted, SnapshotID: "s1"}}

	got := MergeProjectedVolumes(recorded, derived)
	if len(got) != 2 {
		t.Fatalf("MergeProjectedVolumes dropped an entry: got %#v, want both data and scratch", got)
	}
	if got[0].Pvc != "data" || got[0].AddedBytes != 4096 || got[0].SizeBytes != 8192 || got[0].Node != "node-a" {
		t.Fatalf("execution measurements lost on the recorded volume: %#v", got[0])
	}
	if got[1].Pvc != "scratch" || got[1].Phase != status.VolumePhaseSkipped || got[1].Reason != "CSISnapshotUnsupported" {
		t.Fatalf("the Skipped volume and its reason must survive the projection: %#v", got[1])
	}
}

// TestMergeKeepsFailedVolumeWithNoSnapshot: a failure is likewise invisible to
// `restic snapshots`, and is the entry an operator most needs to keep.
func TestMergeKeepsFailedVolumeWithNoSnapshot(t *testing.T) {
	recorded := []cbv1.VolumeStatus{{Pvc: "broken", Phase: status.VolumePhaseFailed, Reason: "MoverFailed"}}
	got := MergeProjectedVolumes(recorded, nil)
	if len(got) != 1 || got[0].Phase != status.VolumePhaseFailed || got[0].Reason != "MoverFailed" {
		t.Fatalf("a Failed volume must survive the projection: %#v", got)
	}
}

// TestMergeAddsDerivedVolumesAbsentFromTheRecord: a restore point discovery finds and the
// record does not know about is still added — that is what makes a Backup a catalogue
// that survives the loss of the cluster (R26).
func TestMergeAddsDerivedVolumesAbsentFromTheRecord(t *testing.T) {
	recorded := []cbv1.VolumeStatus{{Pvc: "a", Phase: status.VolumePhaseCompleted, SnapshotID: "s-a"}}
	derived := []cbv1.VolumeStatus{
		{Pvc: "a", Phase: status.VolumePhaseCompleted, SnapshotID: "s-a"},
		{Pvc: "b", Phase: status.VolumePhaseCompleted, SnapshotID: "s-b"},
	}
	got := MergeProjectedVolumes(recorded, derived)
	if len(got) != 2 || got[1].Pvc != "b" || got[1].SnapshotID != "s-b" {
		t.Fatalf("a derived-only restore point must be added: %#v", got)
	}
}

// TestMergeDropsForgottenCompletedVolume: a Completed volume whose snapshot is no longer in
// the repository was forgotten by retention. Keeping it would advertise a restore that
// cannot happen, so the catalogue half legitimately wins here.
func TestMergeDropsForgottenCompletedVolume(t *testing.T) {
	recorded := []cbv1.VolumeStatus{
		{Pvc: "kept", Phase: status.VolumePhaseCompleted, SnapshotID: "s-kept"},
		{Pvc: "forgotten", Phase: status.VolumePhaseCompleted, SnapshotID: "s-gone"},
	}
	derived := []cbv1.VolumeStatus{{Pvc: "kept", Phase: status.VolumePhaseCompleted, SnapshotID: "s-kept"}}
	got := MergeProjectedVolumes(recorded, derived)
	if len(got) != 1 || got[0].Pvc != "kept" {
		t.Fatalf("a Completed volume with no surviving snapshot must be dropped: %#v", got)
	}
}

// TestMergeFillsMissingSnapshotID: the one field the derivation may contribute to an
// existing record.
func TestMergeFillsMissingSnapshotID(t *testing.T) {
	recorded := []cbv1.VolumeStatus{{Pvc: "a", Phase: status.VolumePhaseCompleted, AddedBytes: 12}}
	derived := []cbv1.VolumeStatus{{Pvc: "a", Phase: status.VolumePhaseCompleted, SnapshotID: "s-a"}}
	got := MergeProjectedVolumes(recorded, derived)
	if len(got) != 1 || got[0].SnapshotID != "s-a" || got[0].AddedBytes != 12 {
		t.Fatalf("snapshotID must be filled in without disturbing the record: %#v", got)
	}
}

// TestMergeIsStableAndSorted: repeated passes must produce the identical list, or the SSA
// apply churns status forever.
func TestMergeIsStableAndSorted(t *testing.T) {
	recorded := []cbv1.VolumeStatus{{Pvc: "z", Phase: status.VolumePhaseSkipped, Reason: "CSISnapshotUnsupported"}}
	derived := []cbv1.VolumeStatus{
		{Pvc: "m", Phase: status.VolumePhaseCompleted, SnapshotID: "s-m"},
		{Pvc: "a", Phase: status.VolumePhaseCompleted, SnapshotID: "s-a"},
	}
	first := MergeProjectedVolumes(recorded, derived)
	second := MergeProjectedVolumes(first, derived)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("merge is not idempotent:\n first=%#v\nsecond=%#v", first, second)
	}
	if first[0].Pvc != "a" || first[1].Pvc != "m" || first[2].Pvc != "z" {
		t.Fatalf("merged volumes must be sorted by PVC: %#v", first)
	}
}

// TestMergeOnEmptyRecordIsThePureProjection: nothing recorded → exactly the derivation.
func TestMergeOnEmptyRecordIsThePureProjection(t *testing.T) {
	derived := []cbv1.VolumeStatus{{Pvc: "a", Phase: status.VolumePhaseCompleted, SnapshotID: "s-a"}}
	if got := MergeProjectedVolumes(nil, derived); !reflect.DeepEqual(got, derived) {
		t.Fatalf("MergeProjectedVolumes(nil, derived) = %#v, want %#v", got, derived)
	}
}

// TestMergeTakesSnapshotIDFromTheRepository: the record owns what HAPPENED, the repository owns
// what EXISTS. A recorded ID that no longer matches the surviving snapshot would name a restore
// that cannot run.
func TestMergeTakesSnapshotIDFromTheRepository(t *testing.T) {
	recorded := []cbv1.VolumeStatus{{Pvc: "a", Phase: status.VolumePhaseCompleted, SnapshotID: "stale", AddedBytes: 7}}
	derived := []cbv1.VolumeStatus{{Pvc: "a", Phase: status.VolumePhaseCompleted, SnapshotID: "live"}}
	got := MergeProjectedVolumes(recorded, derived)
	if len(got) != 1 || got[0].SnapshotID != "live" || got[0].AddedBytes != 7 {
		t.Fatalf("snapshotID must come from the repository, the rest from the record: %#v", got)
	}
}

// TestMergeEmitsOneEntryPerPVC: a retried mover can leave two data snapshots for one PVC in the
// same run. The merge must not fold the recorded entry in twice.
func TestMergeEmitsOneEntryPerPVC(t *testing.T) {
	recorded := []cbv1.VolumeStatus{{Pvc: "a", Phase: status.VolumePhaseCompleted, AddedBytes: 3}}
	derived := []cbv1.VolumeStatus{
		{Pvc: "a", Phase: status.VolumePhaseCompleted, SnapshotID: "s1"},
		{Pvc: "a", Phase: status.VolumePhaseCompleted, SnapshotID: "s2"},
	}
	got := MergeProjectedVolumes(recorded, derived)
	if len(got) != 1 || got[0].SnapshotID != "s1" {
		t.Fatalf("one entry per PVC, first derived snapshot wins: %#v", got)
	}
}
