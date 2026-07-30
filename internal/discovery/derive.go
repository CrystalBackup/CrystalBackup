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
	"cmp"
	"slices"

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
	slices.SortFunc(vols, func(a, b cbv1.VolumeStatus) int { return cmp.Compare(a.Pvc, b.Pvc) })
	return vols
}

// MergeProjectedVolumes folds the repository-derived volumes into the ones a real
// EXECUTION already recorded on the same Backup, and is the rule that keeps a
// projection from erasing a run's own report of itself.
//
// A Backup is two things at once: a catalogue of restore points (R26 — it must
// survive the loss of the cluster, so discovery rebuilds it from the repository)
// and the report of an execution. When the two disagree, the EXECUTION REPORT WINS.
// It is the more specific truth, and a reconstruction from the repository knows, by
// construction, only what SUCCEEDED: `restic snapshots` has no row for a PVC that was
// skipped or that failed. Left to replace the list wholesale, the projection deleted
// exactly the entries nobody else records — a Skipped volume with its
// CSISnapshotUnsupported reason vanished within seconds of the run finishing, taking
// addedBytes, sizeBytes and node with it, and a PartiallyCompleted read Completed
// half a minute later with the failure present nowhere in the cluster.
//
// So, per PVC:
//
//   - recorded AND derived → keep the recorded entry (it carries phase, reason, bytes
//     and node that no snapshot listing can reproduce), but take the snapshotID from the
//     derivation. The split is deliberate: the execution report owns what HAPPENED, the
//     repository owns what EXISTS, and a snapshotID is a pointer into the repository — a
//     recorded ID that has since been forgotten would name a restore that cannot run.
//   - derived only → take the derived entry. This is the pure-projection case, and
//     also how a restore point survives the Backup being rebuilt from scratch.
//   - recorded only → keep it ONLY if it carries information the derivation cannot
//     hold: a non-Completed phase, or a reason. A recorded COMPLETED volume with no
//     snapshot left in the repository is a restore point that was forgotten, and
//     dropping it is the correct catalogue behaviour — keeping it would advertise a
//     restore that cannot happen.
//
// The result is sorted by PVC name so repeated passes are byte-stable.
func MergeProjectedVolumes(recorded, derived []cbv1.VolumeStatus) []cbv1.VolumeStatus {
	byPVC := make(map[string]cbv1.VolumeStatus, len(recorded))
	for _, v := range recorded {
		byPVC[v.Pvc] = v
	}

	out := make([]cbv1.VolumeStatus, 0, len(derived)+len(recorded))
	seen := make(map[string]struct{}, len(derived))
	for _, d := range derived {
		// One entry per PVC. A (namespace, run) group holding two data snapshots for the same
		// PVC — a mover that ran twice after a retry — must not fold the recorded entry in
		// twice; the first derived snapshot wins, as the sort already made deterministic.
		if _, dup := seen[d.Pvc]; dup {
			continue
		}
		seen[d.Pvc] = struct{}{}
		rec, ok := byPVC[d.Pvc]
		if !ok {
			out = append(out, d)
			continue
		}
		rec.SnapshotID = d.SnapshotID
		out = append(out, rec)
	}
	for _, rec := range recorded {
		if _, ok := seen[rec.Pvc]; ok {
			continue
		}
		// No snapshot backs this entry. Keep it only when it says something the repository
		// cannot: a skip, a failure, or an explicit reason. Otherwise it is a stale restore
		// point whose snapshot has been forgotten.
		if rec.Phase != status.VolumePhaseCompleted || rec.Reason != "" {
			out = append(out, rec)
		}
	}
	slices.SortFunc(out, func(a, b cbv1.VolumeStatus) int { return cmp.Compare(a.Pvc, b.Pvc) })
	return out
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
