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

// RestorePhase is the aggregate phase of a Restore or ClusterRestore
// (status.phase — both kinds share one enum in api/v1alpha1).
// AwaitingConfirmation exists ONLY here: it parks a destructive restore until
// spec.confirmation equals the target (R23); Running coarsens every in-flight
// per-volume state (restores keep no persisted per-volume phase list — the live
// mover Jobs are the ground truth, adr/0016).
type RestorePhase string

// Restore aggregate phases. Enum source: RestoreStatus.Phase /
// ClusterRestoreStatus.Phase in api/v1alpha1.
const (
	RestorePhasePending              RestorePhase = "Pending"
	RestorePhaseAwaitingConfirmation RestorePhase = "AwaitingConfirmation"
	RestorePhaseRunning              RestorePhase = "Running"
	RestorePhaseCompleted            RestorePhase = "Completed"
	RestorePhasePartiallyFailed      RestorePhase = "PartiallyFailed"
	RestorePhaseFailed               RestorePhase = "Failed"
)

// RollUpRestoreOutcomes maps a settled restore's per-volume tallies to its
// terminal phase. Unlike the backup roll-ups it takes counts, not a phase list:
// a restore keeps no persisted per-volume phases (the mover Jobs are the ground
// truth), so the caller tallies settled volumes and this fixes the mapping:
//
//	n == 0          -> Completed        (an empty selection restores nothing)
//	f == 0          -> Completed
//	f > 0 && c > 0  -> PartiallyFailed  (some volumes landed beside failures)
//	f > 0 && c == 0 -> Failed
func RollUpRestoreOutcomes(completed, failed int) RestorePhase {
	switch {
	case failed == 0:
		return RestorePhaseCompleted
	case completed > 0:
		return RestorePhasePartiallyFailed
	default:
		return RestorePhaseFailed
	}
}

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
//	Otherwise every volume is terminal (Completed, Skipped, or Failed). Skipped
//	volumes are NEUTRAL — an unsnapshottable PVC (reason CSISnapshotUnsupported)
//	is a deterministic property of the environment, not a backup degradation — so
//	only the Completed (c) and Failed (f) counts decide the outcome; the Skipped
//	count is ignored entirely:
//	  n == 0          -> Completed  (a manifests-only backup with no volumes; the
//	                                 caller may override, e.g. manifests failed)
//	  f == 0          -> Completed  (nothing failed; any Skipped siblings do NOT
//	                                 lower this to PartiallyCompleted, else a
//	                                 namespace holding one unsnapshottable PVC
//	                                 would alarm on every single run, forever)
//	  f > 0 && c > 0  -> PartiallyFailed (real data made it alongside failures)
//	  f > 0 && c == 0 -> Failed     (every non-skipped volume failed; a Skipped
//	                                 volume saved no data, so no partial success)
//
//	PartiallyCompleted is retained as a constant for M3 (a FAILED manifests
//	snapshot beside healthy volumes will produce it), but this roll-up never
//	produces it from a Skipped volume.
//
// Inputs are CRD-enum-validated in practice; any unrecognized phase string is
// neither in-progress nor counted, which conservatively leaves it out of the
// terminal tallies.
func RollUpVolumePhases(volumes []v1alpha1.VolumeStatus) BackupPhase {
	n := len(volumes)

	var c, f int
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
		case VolumePhaseFailed:
			f++
		case VolumePhaseSkipped:
			// Neutral: a Skipped volume (CSISnapshotUnsupported) is deliberately not
			// tallied — it counts neither as success nor failure in the roll-up.
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

	// All volumes are terminal from here on. Skipped volumes are NEUTRAL: a PVC on
	// a CSI with no VolumeSnapshotClass (Skipped, reason CSISnapshotUnsupported) is
	// a deterministic property of the environment, not a backup degradation, so it
	// counts neither for nor against the outcome — only the Completed (c) and Failed
	// (f) tallies decide it.
	if n == 0 {
		// A manifests-only backup; the caller may override (e.g. manifests failed).
		return BackupPhaseCompleted
	}
	switch {
	case f == 0:
		// Nothing failed: full success. Any Skipped siblings never lower this to
		// PartiallyCompleted — otherwise a namespace holding one unsnapshottable PVC
		// would alarm on every run, forever. Skipped stays visible only per-volume
		// (status.volumes[].phase + reason). PartiallyCompleted is kept for M3 (a
		// FAILED manifests snapshot beside healthy volumes), never a skip.
		return BackupPhaseCompleted
	case c > 0:
		// At least one real data snapshot landed alongside the failure(s).
		return BackupPhasePartiallyFailed
	default: // f > 0 && c == 0
		// Every non-skipped volume failed; a Skipped volume saved no data, so there
		// is no partial success to report.
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
