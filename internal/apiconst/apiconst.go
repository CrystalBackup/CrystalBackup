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
