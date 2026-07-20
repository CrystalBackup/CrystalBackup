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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This file holds the shared field types composed by the twelve CrystalBackup
// CRDs. The naming and field contract is spec/02-api.md — keep them in sync.

// LocalObjectReference references another object by name, resolved within the
// same namespace for namespaced kinds or the operator namespace for cluster kinds.
type LocalObjectReference struct {
	// name of the referent.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// LocationReference references a backup location. On the namespace plane the
// kind defaults to BackupLocation; a cluster-origin Backup may reference a
// ClusterBackupLocation.
type LocationReference struct {
	// kind of the referenced location.
	// +optional
	// +kubebuilder:validation:Enum=BackupLocation;ClusterBackupLocation
	// +kubebuilder:default=BackupLocation
	Kind string `json:"kind,omitempty"`

	// name of the referenced location.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// LocationMode selects the durability/erasure semantics of a repository.
// +kubebuilder:validation:Enum=Standard;Immutable
type LocationMode string

const (
	// LocationModeStandard allows prune; erasure is immediate.
	LocationModeStandard LocationMode = "Standard"
	// LocationModeImmutable uses object-lock; no prune, erasure deferred to lock expiry.
	LocationModeImmutable LocationMode = "Immutable"
)

// S3Spec describes the S3-compatible object storage backing a repository.
type S3Spec struct {
	// endpoint URL of the S3-compatible service.
	// +required
	// +kubebuilder:validation:MinLength=1
	Endpoint string `json:"endpoint"`

	// bucket name.
	// +required
	// +kubebuilder:validation:MinLength=1
	Bucket string `json:"bucket"`

	// prefix under which the single shared repository lives (<prefix>/<clusterID>/).
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// region of the bucket.
	// +optional
	Region string `json:"region,omitempty"`

	// credentialsSecretRef references a Secret holding the S3 credentials (same
	// namespace as a BackupLocation, or crystal-backup-system for a ClusterBackupLocation).
	// +required
	CredentialsSecretRef LocalObjectReference `json:"credentialsSecretRef"`

	// caBundle is an optional PEM CA bundle for the endpoint.
	// +optional
	CABundle string `json:"caBundle,omitempty"`

	// forcePathStyle selects path-style addressing (required by most non-AWS gateways).
	// +optional
	ForcePathStyle bool `json:"forcePathStyle,omitempty"`
}

// ClusterEncryptionSpec configures the platform key for a ClusterBackupLocation.
type ClusterEncryptionSpec struct {
	// clusterKEKSecretRef references the age identity wrapping the platform DEK.
	// +required
	ClusterKEKSecretRef LocalObjectReference `json:"clusterKEKSecretRef"`
}

// NamespaceEncryptionSpec configures the user key for a BackupLocation.
type NamespaceEncryptionSpec struct {
	// repositoryPasswordSecretRef references the user-owned restic password Secret
	// (same namespace). If omitted the operator generates one and stores it in the
	// user's namespace (their key, their reversibility).
	// +optional
	RepositoryPasswordSecretRef *LocalObjectReference `json:"repositoryPasswordSecretRef,omitempty"`

	// platformAccess, when true, also gives the operator a key slot for mediated
	// restore/verify; false (default) keeps the off-platform backups private.
	// +optional
	PlatformAccess bool `json:"platformAccess,omitempty"`
}

// DiscoverySpec configures repository→Backup projection.
type DiscoverySpec struct {
	// enabled turns on inventory and projection of Backup objects from the repository.
	// +optional
	// +kubebuilder:default=true
	Enabled bool `json:"enabled,omitempty"`

	// interval between periodic discovery passes.
	// +optional
	// +kubebuilder:default="1h"
	Interval metav1.Duration `json:"interval,omitempty"`
}

// MaintenanceSpec configures Standard-mode repository maintenance.
type MaintenanceSpec struct {
	// pruneSchedule (cron) for the repository-wide exclusive prune window.
	// +optional
	PruneSchedule string `json:"pruneSchedule,omitempty"`
	// pruneMaxRepackSize caps repacking per prune run (e.g. "50G").
	// +optional
	PruneMaxRepackSize string `json:"pruneMaxRepackSize,omitempty"`
	// checkSchedule (cron) for restic check.
	// +optional
	CheckSchedule string `json:"checkSchedule,omitempty"`
	// checkReadDataSubset is the fraction of pack data to verify (e.g. "5%").
	// +optional
	CheckReadDataSubset string `json:"checkReadDataSubset,omitempty"`
}

// ObjectLockMode selects the immutability enforcement mechanism.
// +kubebuilder:validation:Enum=Governance;Compliance;AppendOnlyProxy
type ObjectLockMode string

// ImmutableSpec configures Immutable-mode repositories (object-lock; no prune).
type ImmutableSpec struct {
	// objectLockMode selects the WORM enforcement.
	// +optional
	// +kubebuilder:default=Governance
	ObjectLockMode ObjectLockMode `json:"objectLockMode,omitempty"`
	// rotationPeriod is the window-repo rotation period (object-lock repos cannot prune).
	// +optional
	// +kubebuilder:default="720h"
	RotationPeriod metav1.Duration `json:"rotationPeriod,omitempty"`
}

// RetentionSpec expresses restic-granularity retention, applied per PVC.
type RetentionSpec struct {
	// +optional
	KeepLast int32 `json:"keepLast,omitempty"`
	// +optional
	KeepHourly int32 `json:"keepHourly,omitempty"`
	// +optional
	KeepDaily int32 `json:"keepDaily,omitempty"`
	// +optional
	KeepWeekly int32 `json:"keepWeekly,omitempty"`
	// +optional
	KeepMonthly int32 `json:"keepMonthly,omitempty"`
	// +optional
	KeepYearly int32 `json:"keepYearly,omitempty"`
}

// ConcurrencyPolicy governs overlapping scheduled runs.
// +kubebuilder:validation:Enum=Forbid;Skip
type ConcurrencyPolicy string

// PVCSelector selects PersistentVolumeClaims within a namespace (empty ⇒ all).
type PVCSelector struct {
	// matchLabels selects PVCs by label.
	// +optional
	MatchLabels map[string]string `json:"matchLabels,omitempty"`
	// include is a list of PVC-name globs to add.
	// +optional
	Include []string `json:"include,omitempty"`
	// exclude is a list of PVC-name globs to remove.
	// +optional
	Exclude []string `json:"exclude,omitempty"`
}

// NamespaceSelector selects namespaces for cluster-plane backup. At least one
// positive form (matchNames/matchLabels/matchExpressions/regexp) must be set;
// exclude is applied last (admission rule 8).
type NamespaceSelector struct {
	// matchNames is a list of glob patterns on namespace names.
	// +optional
	MatchNames []string `json:"matchNames,omitempty"`
	// matchLabels selects namespaces by label.
	// +optional
	MatchLabels map[string]string `json:"matchLabels,omitempty"`
	// matchExpressions selects namespaces by label expressions.
	// +optional
	MatchExpressions []metav1.LabelSelectorRequirement `json:"matchExpressions,omitempty"`
	// regexp matches namespace names (power tool; see adr/0009).
	// +optional
	Regexp string `json:"regexp,omitempty"`
	// exclude is a list of glob patterns removed after the positive match.
	// +optional
	Exclude []string `json:"exclude,omitempty"`
}

// HookErrorPolicy governs behaviour when a hook fails.
// +kubebuilder:validation:Enum=Fail;Continue
type HookErrorPolicy string

// Hook is an exec hook run in a selected pod around snapshotting (R16).
type Hook struct {
	// podSelector selects the pod(s) to exec into.
	// +optional
	PodSelector metav1.LabelSelector `json:"podSelector,omitempty"`
	// container name to exec into.
	// +optional
	Container string `json:"container,omitempty"`
	// command to run.
	// +required
	Command []string `json:"command"`
	// timeout for the hook.
	// +optional
	Timeout metav1.Duration `json:"timeout,omitempty"`
	// onError governs whether a failure fails the backup or is tolerated.
	// +optional
	// +kubebuilder:default=Fail
	OnError HookErrorPolicy `json:"onError,omitempty"`
}

// HooksSpec groups pre/post hooks and annotation honouring (R16).
type HooksSpec struct {
	// honorAnnotations enables crystalbackup.io/pre-backup-* pod annotations.
	// +optional
	HonorAnnotations bool `json:"honorAnnotations,omitempty"`
	// pre hooks run before snapshotting.
	// +optional
	Pre []Hook `json:"pre,omitempty"`
	// post hooks run after snapshotting.
	// +optional
	Post []Hook `json:"post,omitempty"`
}

// ClusterResourceCaptureSpec configures cluster-scoped resource capture on a run (adr/0011).
type ClusterResourceCaptureSpec struct {
	// enabled turns on cluster-scoped capture (default true on the cluster plane).
	// +optional
	// +kubebuilder:default=true
	Enabled *bool `json:"enabled,omitempty"`
	// include is an allowlist; empty selects a curated default (CRDs, StorageClasses,
	// IngressClasses, PriorityClasses, RuntimeClasses, non-system ClusterRoles/Bindings, PVs).
	// +optional
	Include []string `json:"include,omitempty"`
	// exclude is a denylist applied after include (default: system:* names).
	// +optional
	Exclude []string `json:"exclude,omitempty"`
	// labelSelector is an optional extra filter.
	// +optional
	LabelSelector metav1.LabelSelector `json:"labelSelector,omitempty"`
}

// ClusterResourceRestoreSpec selects cluster-scoped resources to restore (adr/0011).
// Omitted ⇒ nothing cluster-scoped is restored; admin-only.
type ClusterResourceRestoreSpec struct {
	// include is a list of <group>/<Kind>[/<name>] globs.
	// +optional
	Include []string `json:"include,omitempty"`
	// exclude is applied after include.
	// +optional
	Exclude []string `json:"exclude,omitempty"`
}

// RestoreMode selects how a restore reconciles existing objects and data.
// +kubebuilder:validation:Enum=Recreate;Overwrite
type RestoreMode string

const (
	// RestoreModeRecreate deletes selected existing resources then recreates from backup.
	RestoreModeRecreate RestoreMode = "Recreate"
	// RestoreModeOverwrite applies create-or-update and keeps objects absent from the backup.
	RestoreModeOverwrite RestoreMode = "Overwrite"
)

// ResourceSelectorItem selects manifests to restore (AND within an item, OR between items).
type ResourceSelectorItem struct {
	// selector matches resources by label.
	// +optional
	Selector metav1.LabelSelector `json:"selector,omitempty"`
	// include is a list of <group>/<Kind>[/<name>] globs.
	// +optional
	Include []string `json:"include,omitempty"`
	// exclude removes from what selector and include selected, so an item reads
	// "these kinds, minus these". Applied after both (04-manifest-backup.md §5.4).
	// The backup-time default exclusions already applied at capture and cannot be
	// re-included here.
	// +optional
	Exclude []string `json:"exclude,omitempty"`
}

// ManifestOptions tunes what the namespace manifest dump captures
// (03-security-and-tenancy.md §10).
type ManifestOptions struct {
	// excludeSecretData stores Secret manifests with data/stringData stripped and the
	// annotation crystalbackup.io/secret-data-excluded: "true". Restore recreates them
	// empty, carrying the same annotation, so a workload that needs the values fails
	// visibly instead of silently coming back with wrong ones.
	//
	// This is an opt-out from a deliberate default: a full namespace recovery (R15) needs
	// the Secrets, and the control on them is the repository key — admin-only on the shared
	// DR repo, the user's own on a user location. Excluding the data trades recoverability
	// for a smaller blast radius if that key is ever compromised.
	// +optional
	ExcludeSecretData bool `json:"excludeSecretData,omitempty"`
}

// RestoreResourceOutcome is what happened to one manifest during a restore.
// +kubebuilder:validation:Enum=Created;Configured;Recreated;Failed
type RestoreResourceOutcome string

const (
	// RestoreResourceCreated means the object did not exist and was created.
	RestoreResourceCreated RestoreResourceOutcome = "Created"
	// RestoreResourceConfigured means an existing object was server-side applied (Overwrite).
	RestoreResourceConfigured RestoreResourceOutcome = "Configured"
	// RestoreResourceRecreated means an existing object was deleted then created (Recreate).
	RestoreResourceRecreated RestoreResourceOutcome = "Recreated"
	// RestoreResourceFailed means the object could not be applied; the restore continued.
	RestoreResourceFailed RestoreResourceOutcome = "Failed"
)

// Caps on the per-resource restore report. A restore over a large namespace would otherwise
// grow status without bound — the 1 MiB etcd object limit is a hard ceiling, and a status
// that cannot be written loses the whole report, not just its tail.
const (
	// MaxRestoreResourceEntries is the most per-resource entries kept in status.
	MaxRestoreResourceEntries = 100
	// MaxRestoreChangedPaths is the most changed field paths kept per entry.
	MaxRestoreChangedPaths = 20
)

// RestoreResourcesStatus is the per-resource detail of a manifest restore
// (04-manifest-backup.md §5.4). Additive to the restoredResources counter of 02-api.md.
type RestoreResourcesStatus struct {
	// failedCount is how many resources failed to apply. A restore reports per-resource
	// failures and continues; it does not abort on the first one.
	// +optional
	FailedCount int32 `json:"failedCount,omitempty"`
	// truncated is true when entries were dropped to stay within the caps, so a reader can
	// tell an empty tail from a complete report.
	// +optional
	Truncated bool `json:"truncated,omitempty"`
	// entries records non-trivial outcomes only — a plain Created is the expected case and
	// would drown the interesting ones. Capped at MaxRestoreResourceEntries.
	// +optional
	// +kubebuilder:validation:MaxItems=100
	Entries []RestoreResourceEntry `json:"entries,omitempty"`
}

// RestoreResourceEntry is one resource's outcome in a manifest restore.
type RestoreResourceEntry struct {
	// group is the API group ("" for the core group).
	// +optional
	Group string `json:"group,omitempty"`
	// kind is the PascalCase kind.
	// +optional
	Kind string `json:"kind,omitempty"`
	// name of the object.
	// +optional
	Name string `json:"name,omitempty"`
	// outcome of the apply. In a dry run this is the PLANNED action, not an observed one.
	// +optional
	Outcome RestoreResourceOutcome `json:"outcome,omitempty"`
	// reason carries the server's error when outcome is Failed (a nodePort collision, a
	// finalizer holding a Recreate delete, a CRD absent in the target cluster).
	// +optional
	Reason string `json:"reason,omitempty"`
	// changed lists the field paths a server-side apply modified (Overwrite). Capped at
	// MaxRestoreChangedPaths per entry.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Changed []string `json:"changed,omitempty"`
}

// VolumeSelectorItem selects PVCs (and optionally files within them) to restore.
// When several items match the same PVC, the FIRST matching item wins (02-api.md).
type VolumeSelectorItem struct {
	// names of PVCs (whole-PVC restore).
	// +optional
	Names []string `json:"names,omitempty"`
	// include is a list of file globs within the selected PVC(s) (partial restore, R7).
	// +optional
	Include []string `json:"include,omitempty"`
	// exclude is a list of file globs to skip.
	// +optional
	Exclude []string `json:"exclude,omitempty"`
	// targetPath overrides the restore root within the PVC (empty or "/" ⇒ the PVC root).
	// It is resolved inside the PVC and must not contain ".." segments. Bounded so the CEL
	// rule's cost stays within the apiserver's per-CRD budget.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:XValidation:rule="!self.split('/').exists(p, p == '..')",message="targetPath must not contain '..' segments"
	TargetPath string `json:"targetPath,omitempty"`
}

// RestoreSource identifies a Backup in the same namespace (self-service Restore).
// Exactly one of backup and time must be set (CEL); origin only refines time.
// +kubebuilder:validation:XValidation:rule="has(self.backup) != has(self.time)",message="exactly one of source.backup and source.time must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.origin) || has(self.time)",message="source.origin is only valid together with source.time"
type RestoreSource struct {
	// backup names a Backup in this namespace.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Backup string `json:"backup,omitempty"`
	// time selects "latest" or an RFC3339 instant instead of a named backup. Bounded so the
	// CEL rule's cost stays within the apiserver's per-CRD budget.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="self == 'latest' || self.matches('^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}([.][0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})?$')",message="time must be \"latest\" or an RFC3339 timestamp"
	Time string `json:"time,omitempty"`
	// origin disambiguates when using time.
	// +optional
	// +kubebuilder:validation:Enum=cluster;namespace
	Origin string `json:"origin,omitempty"`
}

// ClusterRestoreSource identifies a repository coordinate for an admin restore.
// Exactly one of backup and time must be set (CEL).
// +kubebuilder:validation:XValidation:rule="has(self.backup) != has(self.time)",message="exactly one of source.backup and source.time must be set"
type ClusterRestoreSource struct {
	// locationRef is the source ClusterBackupLocation.
	// +required
	LocationRef LocalObjectReference `json:"locationRef"`
	// namespace is the origin namespace (repository tag filter).
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`
	// backup names a run; alternatively use time.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Backup string `json:"backup,omitempty"`
	// time selects "latest" or an RFC3339 instant.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="self == 'latest' || self.matches('^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}([.][0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})?$')",message="time must be \"latest\" or an RFC3339 timestamp"
	Time string `json:"time,omitempty"`
}

// ClusterRestoreTarget is where an admin restore lands.
type ClusterRestoreTarget struct {
	// namespace to restore into.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`
	// createNamespace creates the target namespace if absent (non-destructive).
	// +optional
	CreateNamespace bool `json:"createNamespace,omitempty"`
	// storageClassMapping rewrites storageClassName on restore.
	// +optional
	StorageClassMapping map[string]string `json:"storageClassMapping,omitempty"`
}

// ErasureTarget selects exactly one erasure scope (tenant, namespace, or namespace+pvc).
type ErasureTarget struct {
	// tenant erases all snapshots tagged tenant=<t>.
	// +optional
	Tenant string `json:"tenant,omitempty"`
	// namespace erases all snapshots tagged namespace=<ns>.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// pvc, together with namespace, narrows erasure to a single PVC.
	// +optional
	PVC string `json:"pvc,omitempty"`
}

// ExternalSyncSelection narrows a ClusterBackupExternalSync (omitted ⇒ whole repo).
type ExternalSyncSelection struct {
	// namespaces narrows the copy by namespace tag.
	// +optional
	Namespaces *NamespaceSelector `json:"namespaces,omitempty"`
}

// ExternalSyncMode governs how a sync tracks its source.
// +kubebuilder:validation:Enum=Mirror;AppendOnly
type ExternalSyncMode string

const (
	// ExternalSyncModeMirror tracks the source and forgets extras at the destination.
	ExternalSyncModeMirror ExternalSyncMode = "Mirror"
	// ExternalSyncModeAppendOnly only adds snapshots (forced on Immutable destinations).
	ExternalSyncModeAppendOnly ExternalSyncMode = "AppendOnly"
)

// VolumePhase is the per-PVC phase within a Backup.
// +kubebuilder:validation:Enum=Pending;Snapshotting;Uploading;Completed;Skipped;Failed
type VolumePhase string

// VolumeStatus is the per-PVC result within a Backup projection.
type VolumeStatus struct {
	// pvc name.
	Pvc string `json:"pvc"`
	// snapshotID of the PVC data snapshot.
	// +optional
	SnapshotID string `json:"snapshotID,omitempty"`
	// sizeBytes is the logical size of the snapshot.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// addedBytes is the deduplicated bytes added by this backup (best-effort).
	// +optional
	AddedBytes int64 `json:"addedBytes,omitempty"`
	// phase of this volume.
	// +optional
	Phase VolumePhase `json:"phase,omitempty"`
	// node the mover ran on.
	// +optional
	Node string `json:"node,omitempty"`
	// reason explains a non-Completed phase (e.g. CSISnapshotUnsupported).
	// +optional
	Reason string `json:"reason,omitempty"`
}

// ManifestsStatus records the namespace-manifests snapshot within a Backup.
type ManifestsStatus struct {
	// snapshotID of the manifests snapshot.
	// +optional
	SnapshotID string `json:"snapshotID,omitempty"`
	// resourceCount captured.
	// +optional
	ResourceCount int32 `json:"resourceCount,omitempty"`
}

// FailureRecord is one capped failure entry on a ClusterBackup (no unbounded perNamespace map).
type FailureRecord struct {
	// namespace where the failure occurred.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// backup is the child Backup name.
	// +optional
	Backup string `json:"backup,omitempty"`
	// message is a short human-readable cause.
	// +optional
	Message string `json:"message,omitempty"`
}

// ClusterBackupRunSpec is the run configuration shared by a ClusterBackupSchedule
// template and a (manual or fanned-out) ClusterBackup.
type ClusterBackupRunSpec struct {
	// locationRef is the ClusterBackupLocation to write to.
	// +required
	LocationRef LocalObjectReference `json:"locationRef"`
	// namespaces selects the namespaces to back up (rule 8: one positive form + optional exclude).
	// +optional
	Namespaces NamespaceSelector `json:"namespaces,omitempty"`
	// pvcSelector selects PVCs per namespace (default all).
	// +optional
	PVCSelector PVCSelector `json:"pvcSelector,omitempty"`
	// includeManifests also captures namespace manifests (default true).
	// +optional
	// +kubebuilder:default=true
	IncludeManifests *bool `json:"includeManifests,omitempty"`
	// manifestOptions tunes what the manifest dump captures (03-security-and-tenancy.md §10).
	// +optional
	ManifestOptions ManifestOptions `json:"manifestOptions,omitempty"`
	// clusterResources captures cluster-scoped objects for full DR (adr/0011).
	// +optional
	ClusterResources ClusterResourceCaptureSpec `json:"clusterResources,omitempty"`
	// hooks are exec hooks around snapshotting (R16).
	// +optional
	Hooks HooksSpec `json:"hooks,omitempty"`
	// maxConcurrentMovers caps parallel mover Jobs.
	// +optional
	MaxConcurrentMovers int32 `json:"maxConcurrentMovers,omitempty"`
	// backoffLimit for mover Jobs.
	// +optional
	BackoffLimit int32 `json:"backoffLimit,omitempty"`
}

// ClusterBackupTemplate wraps a ClusterBackupRunSpec as a schedule's jobTemplate analogue.
type ClusterBackupTemplate struct {
	// spec is the ClusterBackup run configuration.
	// +required
	Spec ClusterBackupRunSpec `json:"spec"`
}
