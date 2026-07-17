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

// Package discovery holds the pure, client-free projections the discovery
// controller derives from a repository's restic snapshot inventory: the
// per-PVC VolumeStatus set of a projected Backup, and the distinct-namespace
// count reported on the repository. Keeping the derivation here (a total function
// of a []restic.Snapshot) makes it exhaustively unit-testable without an API
// server, restic, or S3, and lets the controller focus on reconciliation.
package discovery

import (
	"sort"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// VolumesFromSnapshots derives the status.volumes of a projected Backup from the
// snapshots of one (namespace, run) group. It emits exactly one Completed
// VolumeStatus per DATA snapshot (kind=data) — the restorable per-PVC restore
// points — carrying the snapshot's own restic ID and pvc= tag; manifests and
// cluster-manifests snapshots contribute no volume (they are not per-PVC data). A
// data snapshot missing its pvc= tag is skipped defensively rather than projected
// under an empty PVC name. The result is sorted by PVC name so a projection is
// byte-stable across passes (no spurious status churn / SSA conflicts).
func VolumesFromSnapshots(snaps []restic.Snapshot) []cbv1.VolumeStatus {
	var vols []cbv1.VolumeStatus
	for _, s := range snaps {
		if kind, _ := restic.TagValue(s.Tags, restic.TagKeyKind); kind != restic.KindData {
			continue
		}
		pvc, ok := restic.TagValue(s.Tags, restic.TagKeyPVC)
		if !ok || pvc == "" {
			continue
		}
		vols = append(vols, cbv1.VolumeStatus{
			Pvc:        pvc,
			SnapshotID: s.ID,
			Phase:      status.VolumePhaseCompleted,
		})
	}
	sort.Slice(vols, func(i, j int) bool { return vols[i].Pvc < vols[j].Pvc })
	return vols
}

// DistinctNamespaces counts the distinct, non-empty namespace= tags across the
// inventory — the repository's namespacesPresent. The empty namespace (a
// cluster-manifests snapshot carries no namespace= tag) is not a namespace and is
// not counted.
func DistinctNamespaces(snaps []restic.Snapshot) int {
	seen := map[string]struct{}{}
	for _, s := range snaps {
		if ns, ok := restic.TagValue(s.Tags, restic.TagKeyNamespace); ok && ns != "" {
			seen[ns] = struct{}{}
		}
	}
	return len(seen)
}
