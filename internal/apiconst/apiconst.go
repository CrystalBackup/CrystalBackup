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

// Package apiconst is the single source of truth for the label, annotation and
// finalizer keys, their value formats, and the operator namespace that the
// CrystalBackup controllers stamp onto objects. Today these strings live only in
// CRD field comments (api/v1alpha1) and prose (spec/02-api.md); centralising them
// here removes the byte-for-byte drift risk between the code and the API contract.
//
// The naming and value contract is spec/02-api.md — keep them in sync. Nothing in
// this package imports the API types, so every other internal package may import
// it without a cycle.
package apiconst

import "time"

// Domain is the API group and the shared prefix for every CrystalBackup label,
// annotation and finalizer key.
const Domain = "crystalbackup.io"

// DefaultOperatorNamespace is where the cluster-plane platform Secrets
// (crystal-dek-*, the cluster KEK, the DR S3 credentials), the mover Jobs and
// their per-Job projected Secrets live. The running namespace is learned at
// startup from POD_NAMESPACE / --operator-namespace (downward API); this constant
// is the fallback and the Helm chart default.
const DefaultOperatorNamespace = "crystal-backup-system"

// Label keys stamped by the controllers. Cross-namespace parent→child links use a
// label (never an ownerReference — a namespaced Backup cannot be owned by a
// cluster-scoped ClusterBackup, and history GC must not cascade-delete still
// restorable projections), so these keys are load-bearing for fan-out, discovery
// and aggregation.
const (
	// LabelClusterBackup links a fanned-out child Backup to its ClusterBackup run.
	// Its value is the ClusterBackup name, which equals the restic run tag and the
	// child Backup.metadata.name (see spec/02-api.md §Repository layout — "run" tag).
	LabelClusterBackup = Domain + "/cluster-backup"

	// LabelOrigin records which plane produced a Backup: OriginCluster for a
	// cluster-DR fan-out (read-only to users), OriginNamespace for the user plane.
	LabelOrigin = Domain + "/origin"

	// LabelSchedule records the originating schedule name on a run and its children
	// (absent for a manual/ad-hoc run). Mirrors the restic "schedule=" tag.
	LabelSchedule = Domain + "/schedule"

	// LabelNamespace records a child Backup's origin namespace as a queryable label.
	// Mirrors the restic "namespace=" tag.
	LabelNamespace = Domain + "/namespace"

	// LabelTenant records a child Backup's resolved tenant as a queryable label.
	// Mirrors the restic "tenant=" tag; the derivation lives behind internal/tenant.
	LabelTenant = Domain + "/tenant"

	// LabelProtect is the conventional namespace opt-in label referenced from a
	// ClusterBackupSchedule's namespaces.matchLabels (spec/02-api.md example:
	// `crystalbackup.io/protect: "true"`). The operator reads it; it never sets it.
	LabelProtect = Domain + "/protect"

	// LabelPVC records the source PersistentVolumeClaim a per-PVC exposure (its dynamic
	// VolumeSnapshot, the static VS/VSC re-bind pair, and the temp clone PVC) and its mover
	// Job belong to. Together with LabelClusterBackup and LabelNamespace it is the selector
	// the orphan reaper (M1 task #22) and the crucible leak-check
	// (test/crucible/tests/m1_reliability_test.go) use to find and tear down the objects a
	// Backup leaves behind, so it is stamped on EVERY exposure object and mover Job.
	LabelPVC = Domain + "/pvc"

	// LabelRestore records the owning namespaced Restore on every object a restore creates in
	// the operator namespace (the staging PVC, the twin PV, the restore mover Job and its creds
	// Secret). Its value is the Restore name; LabelNamespace carries the Restore's namespace.
	// It is how the orphan reaper resolves a restore-owned object to its owner instead of
	// misreading LabelClusterBackup as a Backup run (M2, adr/0016).
	LabelRestore = Domain + "/restore"

	// LabelClusterRestore is LabelRestore's cluster-plane sibling: the owning ClusterRestore's
	// name on the objects an admin restore creates. ClusterRestore is cluster-scoped, so the
	// owner lookup needs no namespace; LabelNamespace on these objects carries the TARGET
	// namespace for correlation only.
	LabelClusterRestore = Domain + "/cluster-restore"

	// LabelPVRole marks the PersistentVolume objects a restore creates or adopts, so the
	// reaper and the leak-check can find them without sweeping every PV in the cluster:
	// PVRoleTwin on the twin PV aliasing a bound target volume, PVRoleTransplant on a
	// dynamically-provisioned volume mid-handover (removed when the transplant completes and
	// the volume becomes the user's — a restored PVC/PV must never look operator-owned).
	LabelPVRole = Domain + "/pv-role"
)

// LabelPVRole values (see LabelPVRole).
const (
	PVRoleTwin       = "twin"
	PVRoleTransplant = "transplant"
)

// Standard Kubernetes "managed-by" label stamped on the operator-owned objects a backup
// creates (the per-PVC exposure objects and the mover Jobs), so the orphan reaper can select
// every CrystalBackup-managed workload object with one label. It intentionally reuses the
// recommended app.kubernetes.io/managed-by key already stamped on the wrapped-DEK Secrets
// (internal/keys) and the repository-init Jobs (the BackupRepository controller), and is
// deliberately NOT app.kubernetes.io/name=crystal-backup — that is the operator pod's own
// label, which the crucible's operator-restart test selects on; a mover pod carrying it would
// be swept up by that test.
const (
	// LabelManagedBy is the app.kubernetes.io/managed-by key.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// ManagedByValue is the LabelManagedBy value shared by every operator-managed object.
	ManagedByValue = "crystal-backup"
)

// Annotations the controllers honour on the objects they read.
const (
	// AnnotationProjected marks a Backup as a read-only materialized view PROJECTED from the
	// repository by discovery (M1 task #21), not a unit of execution. The Backup controller
	// treats a Backup carrying this annotation (=AnnotationProjectedValue) as inert: it never
	// snapshots or moves data for it, because a projection merely mirrors snapshots that
	// already exist. It is the forward-compat guard that keeps the execution controller and
	// the discovery projector from fighting over the same Backup kind.
	AnnotationProjected = Domain + "/projected"
	// AnnotationProjectedValue is the truthy value discovery sets AnnotationProjected to.
	AnnotationProjectedValue = "true"

	// AnnotationRestoredFrom is stamped on a PVC a restore CREATED (the pvc-transplant
	// handover, adr/0016) with the originating run name. It is informational provenance for
	// the user — deliberately an annotation, not a label, and never accompanied by the
	// operator's managed-by/reaper labels: a restored PVC is the USER'S object and must never
	// be selectable by the reaper or the leak-check.
	AnnotationRestoredFrom = Domain + "/restored-from"
)

// Origin label values (see LabelOrigin).
const (
	OriginCluster   = "cluster"
	OriginNamespace = "namespace"
)

// AnnotationPreBackupPrefix is the prefix for pod annotations honoured as pre-backup
// hooks when HooksSpec.HonorAnnotations is true (e.g. "crystalbackup.io/pre-backup-command").
const AnnotationPreBackupPrefix = Domain + "/pre-backup-"

// Finalizers guarding the delete contract of the cluster-DR lifecycle owners.
// spec/02-api.md does not fix the exact strings (an open question surfaced during
// M1 planning); these pin them. Deleting a location or repository never erases repo
// objects (Immutable/object-lock forbids it, and erasure is an explicit ClusterErasure).
const (
	// FinalizerLocation guards a ClusterBackupLocation/BackupLocation delete.
	FinalizerLocation = Domain + "/location"

	// FinalizerRepository guards a BackupRepository delete.
	FinalizerRepository = Domain + "/repository"

	// FinalizerBackup guards a Backup delete so the Backup controller can, before the object
	// is removed, tear down any live per-PVC exposure and mover Job it created (the "effective
	// cancel / no leak on delete" guarantee). It is distinct from FinalizerRepository: a Backup
	// is a namespaced unit of execution, not the cluster-scoped repository.
	FinalizerBackup = Domain + "/backup"
)

// RunTimestampLayout is the Go reference-time layout for the timestamp segment of a
// scheduled run name, in UTC: YYYYMMDD-HHMMSS.
const RunTimestampLayout = "20060102-150405"

// RunName builds the deterministic name of a scheduled run: "<schedule>-<YYYYMMDD-HHMMSS>"
// in UTC. This name is reused verbatim as the ClusterBackup name, the child Backup
// name in every fanned-out namespace, and the restic "run" tag, so discovery and
// fan-out converge on the same object keyed by (namespace, run).
func RunName(schedule string, t time.Time) string {
	return schedule + "-" + t.UTC().Format(RunTimestampLayout)
}
