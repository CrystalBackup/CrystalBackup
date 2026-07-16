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

package status

import "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"

// The three phase vocabularies below are DISTINCT. They are pinned here as typed
// string constants (guarded by a pinning test) because api/v1alpha1 spells them
// today only inside +kubebuilder:validation:Enum markers and plain string status
// fields — there are no Go consts to reference yet. Treat these as candidates to
// promote into api/v1alpha1 later; until then this package is the single Go
// source of truth and the pinning test is the anti-drift guard against the CRD
// markers in api/v1alpha1/{common,backup,clusterbackup}_types.go.

// VolumePhase is the per-PVC phase within a Backup. It is a type ALIAS for
// v1alpha1.VolumePhase (deliberately an alias, not a re-declaration) so these
// constants compare directly against VolumeStatus.Phase with no conversion.
type VolumePhase = v1alpha1.VolumePhase

// Per-PVC volume phases. Enum source: VolumePhase in
// api/v1alpha1/common_types.go. Skipped exists ONLY here — a PVC the mover could
// not or need not snapshot (e.g. CSISnapshotUnsupported) — never on the
// aggregate phases.
const (
	VolumePhasePending      VolumePhase = "Pending"
	VolumePhaseSnapshotting VolumePhase = "Snapshotting"
	VolumePhaseUploading    VolumePhase = "Uploading"
	VolumePhaseCompleted    VolumePhase = "Completed"
	VolumePhaseSkipped      VolumePhase = "Skipped"
	VolumePhaseFailed       VolumePhase = "Failed"
)

// BackupPhase is the aggregate phase of a Backup (status.phase). api/v1alpha1
// stores it as a plain string constrained by an Enum marker; this named type
// gives RollUpVolumePhases a return type. SnapshottingHooks is a
// controller-driven pre-snapshot phase (never produced by the roll-up), and
// PartiallyCompleted/PartiallyFailed exist only on the aggregates, never
// per-volume.
type BackupPhase string

// Backup aggregate phases. Enum source: BackupStatus.Phase in
// api/v1alpha1/backup_types.go.
const (
	BackupPhasePending            BackupPhase = "Pending"
	BackupPhaseSnapshottingHooks  BackupPhase = "SnapshottingHooks"
	BackupPhaseSnapshotting       BackupPhase = "Snapshotting"
	BackupPhaseUploading          BackupPhase = "Uploading"
	BackupPhaseCompleted          BackupPhase = "Completed"
	BackupPhasePartiallyCompleted BackupPhase = "PartiallyCompleted"
	BackupPhasePartiallyFailed    BackupPhase = "PartiallyFailed"
	BackupPhaseFailed             BackupPhase = "Failed"
)

// ClusterBackupPhase is the aggregate phase of a ClusterBackup DR run
// (status.phase), stored as a plain Enum string in api/v1alpha1. Running exists
// ONLY here: the ClusterBackup coarsens every in-progress child phase down to a
// single Running, so there is no Snapshotting/Uploading distinction at this
// level.
type ClusterBackupPhase string

// ClusterBackup aggregate phases. Enum source: ClusterBackupStatus.Phase in
// api/v1alpha1/clusterbackup_types.go.
const (
	ClusterBackupPhasePending         ClusterBackupPhase = "Pending"
	ClusterBackupPhaseRunning         ClusterBackupPhase = "Running"
	ClusterBackupPhaseCompleted       ClusterBackupPhase = "Completed"
	ClusterBackupPhasePartiallyFailed ClusterBackupPhase = "PartiallyFailed"
	ClusterBackupPhaseFailed          ClusterBackupPhase = "Failed"
)

// RollUpVolumePhases maps the set of per-PVC VolumePhases in a Backup to the
// Backup's aggregate phase. It is one of the two DISTINCT roll-ups in this
// package (RollUpBackupPhases aggregates one level up) and must not stand in for
// the other: the input and output enums differ.
//
// The mapping, in evaluation order:
//
//	In-progress wins. If ANY volume is still in-progress — phase "", Pending,
//	Snapshotting, or Uploading — the backup is in-progress regardless of any
//	already-terminal siblings, and the result is the furthest-along in-progress
//	phase: Uploading if any volume is Uploading, else Snapshotting if any is
//	Snapshotting, else Pending (an empty/unset phase counts toward Pending).
//	SnapshottingHooks is NEVER produced here — it is a controller-driven
//	pre-snapshot phase, not a per-volume state.
//
//	Otherwise every volume is terminal (Completed, Skipped, or Failed). With
//	c/s/f the counts of Completed/Skipped/Failed and n the total:
//	  n == 0                    -> Completed  (a manifests-only backup with no
//	                                           volumes; the caller may override,
//	                                           e.g. if the manifests snapshot
//	                                           itself failed)
//	  f == 0 && s == 0          -> Completed  (all volumes completed)
//	  f == 0 && s > 0 && c > 0  -> PartiallyCompleted (some skipped, none failed)
//	  f == 0 && s > 0 && c == 0 -> Completed  (all volumes skipped; nothing
//	                                           failed, so this is not a partial
//	                                           success — there was simply nothing
//	                                           snapshottable)
//	  f > 0 && (c > 0 || s > 0) -> PartiallyFailed (some data made it)
//	  f > 0 && c == 0 && s == 0 -> Failed     (every volume failed)
//
// Inputs are CRD-enum-validated in practice; any unrecognized phase string is
// neither in-progress nor counted, which conservatively leaves it out of the
// terminal tallies.
func RollUpVolumePhases(volumes []v1alpha1.VolumeStatus) BackupPhase {
	n := len(volumes)

	var c, s, f int
	var anyUploading, anySnapshotting, anyPending bool
	for _, v := range volumes {
		switch v.Phase {
		case VolumePhaseUploading:
			anyUploading = true
		case VolumePhaseSnapshotting:
			anySnapshotting = true
		case VolumePhasePending, "":
			anyPending = true
		case VolumePhaseCompleted:
			c++
		case VolumePhaseSkipped:
			s++
		case VolumePhaseFailed:
			f++
		}
	}

	// In-progress takes precedence over any terminal siblings, with the
	// furthest-along phase winning: Uploading > Snapshotting > Pending.
	if anyUploading || anySnapshotting || anyPending {
		switch {
		case anyUploading:
			return BackupPhaseUploading
		case anySnapshotting:
			return BackupPhaseSnapshotting
		default:
			return BackupPhasePending
		}
	}

	// All volumes are terminal from here on.
	if n == 0 {
		return BackupPhaseCompleted
	}
	switch {
	case f == 0 && s == 0:
		return BackupPhaseCompleted
	case f == 0 && s > 0 && c > 0:
		return BackupPhasePartiallyCompleted
	case f == 0 && s > 0 && c == 0:
		return BackupPhaseCompleted
	case f > 0 && (c > 0 || s > 0):
		return BackupPhasePartiallyFailed
	default: // f > 0 && c == 0 && s == 0
		return BackupPhaseFailed
	}
}

// RollUpBackupPhases aggregates the phases of the child Backup objects of a
// ClusterBackup DR run into the run's aggregate phase. It is the second, DISTINCT
// roll-up (RollUpVolumePhases operates one level down); childPhases carries plain
// Backup.status.phase strings because that field is an unenumerated Go string.
//
// The mapping, in evaluation order:
//
//	len == 0                                        -> Pending  (nothing fanned
//	                                                             out yet)
//	any child in {"", Pending, SnapshottingHooks,
//	              Snapshotting, Uploading}          -> Running  (at least one
//	                                                             child in flight,
//	                                                             even alongside a
//	                                                             failed sibling)
//	otherwise every child is terminal; with
//	ok  = count in {Completed, PartiallyCompleted}
//	bad = count in {Failed, PartiallyFailed}:
//	  bad == 0                                      -> Completed
//	  ok == 0                                       -> Failed
//	  else                                          -> PartiallyFailed
func RollUpBackupPhases(childPhases []string) ClusterBackupPhase {
	if len(childPhases) == 0 {
		return ClusterBackupPhasePending
	}

	// Any in-flight child coarsens the whole run to Running.
	for _, p := range childPhases {
		switch p {
		case "",
			string(BackupPhasePending),
			string(BackupPhaseSnapshottingHooks),
			string(BackupPhaseSnapshotting),
			string(BackupPhaseUploading):
			return ClusterBackupPhaseRunning
		}
	}

	// All children terminal: tally successes vs failures.
	var ok, bad int
	for _, p := range childPhases {
		switch p {
		case string(BackupPhaseCompleted), string(BackupPhasePartiallyCompleted):
			ok++
		case string(BackupPhaseFailed), string(BackupPhasePartiallyFailed):
			bad++
		}
	}
	switch {
	case bad == 0:
		return ClusterBackupPhaseCompleted
	case ok == 0:
		return ClusterBackupPhaseFailed
	default:
		return ClusterBackupPhasePartiallyFailed
	}
}
