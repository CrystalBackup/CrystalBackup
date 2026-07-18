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
