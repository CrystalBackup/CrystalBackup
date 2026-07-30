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

package metrics

// Every crystalbackup_ series name, in one place.
//
// This file exists because of a specific failure the 0.5.x cycle taught: a name that appears in
// two places drifts, and the drift is SILENT. Until M6 the alert rules in
// spec/05-observability.md §3 were prose, and five of the nine referenced series the operator
// never emitted — `crystalbackup_backup_failures_total` against a shipped
// `crystalbackup_backup_failures`, plus four families that did not exist at all. Each of those
// rules is valid PromQL. Each evaluates without error. None of them can ever fire. Nothing in the
// build could have caught it, because the rule text and the metric definition never met.
//
// So they meet here. The collectors below build their prometheus.Desc from these constants, and
// the alert-rule table (rules.go) builds its expressions from the SAME constants — which is what
// lets a compile failure, rather than a silent non-firing alert in production, be the consequence
// of renaming a series. See rules.go for the other half of that contract.
//
// Names follow spec/05-observability.md §2. Grouped by the section that specifies them.
const (
	// Operator identity (§2.10 neighbourhood — always present, so /metrics is never empty).
	NameBuildInfo = "crystalbackup_build_info"

	// §2.1 Backup, per namespace, from Backup objects.
	NameBackupLastSuccess    = "crystalbackup_backup_last_success_timestamp_seconds"
	NameBackupLastSize       = "crystalbackup_backup_last_size_bytes"
	NameBackupLastAdded      = "crystalbackup_backup_last_added_bytes"
	NameBackupLastDuration   = "crystalbackup_backup_last_duration_seconds"
	NameBackupFailures       = "crystalbackup_backup_failures"
	NameBackupDuration       = "crystalbackup_backup_duration_seconds"
	NameBackupAddedTotal     = "crystalbackup_backup_added_bytes_total"
	NameBackupFailuresTotal  = "crystalbackup_backup_failures_total"
	NameScheduleActive       = "crystalbackup_schedule_active"
	NameSchedulePeriod       = "crystalbackup_schedule_period_seconds"
	NameScheduleCreated      = "crystalbackup_schedule_created_timestamp_seconds"
	NameBackupTotal          = "crystalbackup_backup_total"
	NameBackupProtectedBytes = "crystalbackup_backup_protected_bytes"

	// §2.2 ClusterBackup runs, run-level, no namespace label.
	NameClusterBackupLastSuccess       = "crystalbackup_clusterbackup_last_success_timestamp_seconds"
	NameClusterBackupNamespacesMatched = "crystalbackup_clusterbackup_namespaces_matched"
	NameClusterBackupNamespacesFailed  = "crystalbackup_clusterbackup_namespaces_failed"
	NameClusterBackupDuration          = "crystalbackup_clusterbackup_duration_seconds"
	NameClusterBackupRunsTotal         = "crystalbackup_clusterbackup_runs_total"

	// §2.3 Restore, both kinds.
	NameRestoreLastSuccess   = "crystalbackup_restore_last_success_timestamp_seconds"
	NameRestoreLastBytes     = "crystalbackup_restore_last_restored_bytes"
	NameRestoreFailures      = "crystalbackup_restore_failures"
	NameRestoreDuration      = "crystalbackup_restore_duration_seconds"
	NameRestoreFailuresTotal = "crystalbackup_restore_failures_total"

	// §2.4 Repository, maintenance and verification.
	NameRepositorySize         = "crystalbackup_repository_size_bytes"
	NameRepositorySnapshots    = "crystalbackup_repository_snapshot_count"
	NameRepositoryLastCheck    = "crystalbackup_repository_last_check_timestamp_seconds"
	NameRepositoryCheckSuccess = "crystalbackup_repository_last_check_success"
	NameRepositoryLastPrune    = "crystalbackup_repository_last_maintenance_timestamp_seconds"
	NameRepositoryStaleLocks   = "crystalbackup_repository_stale_locks"
	NameRepositoryLocksReaped  = "crystalbackup_repository_locks_reaped_total"

	// §2.5 Discovery, the repository→Backup projection.
	NameDiscoveryLastTimestamp = "crystalbackup_discovery_last_timestamp_seconds"
	NameDiscoveryLastSuccess   = "crystalbackup_discovery_last_success"
	NameDiscoveryProjected     = "crystalbackup_discovery_projected_backups"
	NameDiscoveryOrphans       = "crystalbackup_discovery_orphan_snapshots"

	// §2.6 Right-to-erasure.
	NameErasureForgottenTotal = "crystalbackup_erasure_snapshots_forgotten_total"
	NameErasureReclaimedTotal = "crystalbackup_erasure_reclaimed_bytes_total"
	NameErasureBlocked        = "crystalbackup_erasure_blocked"
	NameErasureLastCompletion = "crystalbackup_erasure_last_completion_timestamp_seconds"

	// §2.7 Concurrency and queueing.
	NameMoverActive           = "crystalbackup_mover_active"
	NameMoverQueueDepth       = "crystalbackup_mover_queue_depth"
	NameMoverConcurrencyLimit = "crystalbackup_mover_concurrency_limit"
	NameMoverJobRetriesTotal  = "crystalbackup_mover_job_retries_total"

	// §2.8 Admission (the operator's dynamic webhook only; VAP denials live in the apiserver).
	NameWebhookDenialsTotal = "crystalbackup_webhook_denials_total"

	// §2.9 Snapshot exposure and coexistence.
	NameExposureReadyWait     = "crystalbackup_exposure_ready_wait_seconds"
	NamePVCVolumeSnapshotting = "crystalbackup_pvc_volumesnapshot_count"

	// §2.12 External backup synchronization.
	NameExternalSyncLastSuccess = "crystalbackup_externalsync_last_success_timestamp_seconds"
	NameExternalSyncCopied      = "crystalbackup_externalsync_snapshots_copied"
	NameExternalSyncLag         = "crystalbackup_externalsync_lag_snapshots"
	NameExternalSyncFailures    = "crystalbackup_externalsync_failures"
	NameExternalSyncDuration    = "crystalbackup_externalsync_duration_seconds"
	NameExternalSyncActive      = "crystalbackup_externalsync_active"
	NameExternalSyncCreated     = "crystalbackup_externalsync_created_timestamp_seconds"
)

// durationBuckets are the histogram buckets spec/05-observability.md §2.1 fixes for every
// duration family (backup, cluster run, restore, external sync), so a dashboard can put them on
// one axis and an operator reading two panels is comparing like with like.
//
// They span one minute to eight hours because that is the real range of this workload: a small
// namespace's incremental finishes inside the first bucket, and a first full backup of a
// multi-terabyte volume over a throttled S3 link lands in the last. Linear buckets would waste
// their resolution where nothing happens.
var durationBuckets = []float64{60, 300, 900, 1800, 3600, 7200, 14400, 28800}

// exposureBuckets cover a different phenomenon on a much shorter scale: the wait for a
// VolumeSnapshot to become ReadyToUse and its temp PVC to bind. Healthy CSI drivers answer in
// seconds; the failure this histogram exists to make visible is the driver that takes minutes and
// pushes the whole backup window out. Sharing durationBuckets would put every healthy exposure in
// the first bucket and show nothing.
var exposureBuckets = []float64{1, 5, 15, 30, 60, 120, 300, 600}
