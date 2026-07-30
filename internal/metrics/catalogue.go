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

import "slices"

// Catalogue exposes, to code OUTSIDE this package, which labels each crystalbackup_ series
// actually carries.
//
// It exists for one consumer: internal/alerts, whose expressions are built from the constants in
// names.go and must be checkable against the label sets those series really have. A name check
// alone is not enough — `crystalbackup_repository_stale_locks{schedule="x"}` names a real series
// and matches nothing, forever, in silence. That is the same class of defect as an unknown series
// name and it fails the same way.
//
// The label sets are NOT restated here. Every entry points at the very []string the collector
// passes to prometheus.NewDesc, so there is no second copy to drift: adding a label to a family
// updates this map by construction. catalogue_export_test.go closes the remaining gap by checking
// this map against the Descs the collectors actually register, so a family added without an entry
// here fails the build rather than quietly losing its alert-side validation.
func Catalogue() map[string][]string {
	c := map[string][]string{
		NameBuildInfo: {versionLabel},

		// §2.1 backup, per namespace.
		NameBackupLastSuccess:    backupLabels,
		NameBackupLastSize:       backupLabels,
		NameBackupLastAdded:      backupLabels,
		NameBackupLastDuration:   backupLabels,
		NameBackupFailures:       backupLabels,
		NameBackupDuration:       backupLabels,
		NameBackupAddedTotal:     backupLabels,
		NameBackupFailuresTotal:  backupLabels,
		NameScheduleActive:       backupLabels,
		NameSchedulePeriod:       backupLabels,
		NameScheduleCreated:      backupLabels,
		NameBackupTotal:          withLabel(backupLabels, resultLabel),
		NameBackupProtectedBytes: protectedLabels,

		// §2.2 ClusterBackup runs.
		NameClusterBackupLastSuccess:       clusterBackupLabels,
		NameClusterBackupNamespacesMatched: clusterBackupLabels,
		NameClusterBackupNamespacesFailed:  clusterBackupLabels,
		NameClusterBackupDuration:          clusterBackupLabels,
		NameClusterBackupRunsTotal:         withLabel(clusterBackupLabels, resultLabel),

		// §2.3 restore.
		NameRestoreLastSuccess:   restoreLabels,
		NameRestoreLastBytes:     restoreLabels,
		NameRestoreFailures:      restoreLabels,
		NameRestoreDuration:      withLabel(restoreLabels, modeLabel),
		NameRestoreFailuresTotal: withLabel(restoreLabels, modeLabel),

		// §2.4 repository.
		NameRepositorySize:         repositoryLabels,
		NameRepositorySnapshots:    repositoryLabels,
		NameRepositoryLastCheck:    repositoryLabels,
		NameRepositoryCheckSuccess: repositoryLabels,
		NameRepositoryLastPrune:    repositoryLabels,
		NameRepositoryStaleLocks:   repositoryLabels,
		NameRepositoryLocksReaped:  repositoryLabels,

		// §2.5 discovery.
		NameDiscoveryLastTimestamp: discoveryLabels,
		NameDiscoveryLastSuccess:   discoveryLabels,
		NameDiscoveryProjected:     discoveryLabels,
		NameDiscoveryOrphans:       discoveryLabels,

		// §2.6 right-to-erasure.
		NameErasureForgottenTotal: erasureLabels,
		NameErasureReclaimedTotal: erasureLabels,
		NameErasureBlocked:        erasureLabels,
		NameErasureLastCompletion: erasureLabels,

		// §2.7 concurrency and queueing.
		NameMoverActive:           moverLabels,
		NameMoverQueueDepth:       moverLabels,
		NameMoverConcurrencyLimit: moverLabels,
		NameMoverJobRetriesTotal:  {namespaceLabel, tenantLabel, clusterLabel},

		// §2.8 admission.
		NameWebhookDenialsTotal: {webhookLabel, reasonLabel},

		// §2.9 snapshot exposure and coexistence.
		NameExposureReadyWait:     {namespaceLabel, tenantLabel, exposerLabel, clusterLabel},
		NamePVCVolumeSnapshotting: {namespaceLabel, pvcLabel, clusterLabel},

		// §2.12 external backup synchronization.
		NameExternalSyncLastSuccess: externalSyncLabels,
		NameExternalSyncCopied:      externalSyncLabels,
		NameExternalSyncLag:         externalSyncLabels,
		NameExternalSyncFailures:    externalSyncLabels,
		NameExternalSyncDuration:    externalSyncLabels,
	}
	// Cloned on the way out: the values are the collectors' own label slices, and a caller that
	// sorted or appended to one in place would reshape a live metric family.
	out := make(map[string][]string, len(c))
	for name, labels := range c {
		out[name] = slices.Clone(labels)
	}
	return out
}

// Histograms names the families Prometheus derives _bucket/_sum/_count series from, so a checker
// can tell `crystalbackup_backup_duration_seconds_count` (real) from
// `crystalbackup_backup_failures_count` (not a series anyone emits).
func Histograms() []string {
	return []string{
		NameBackupDuration,
		NameClusterBackupDuration,
		NameRestoreDuration,
		NameExternalSyncDuration,
		NameExposureReadyWait,
	}
}

// ScopeLabelValue exposes the API-enum → label-value mapping metricScope applies, for code that
// has to reproduce a repository series' identity outside this package (internal/alerts' state
// predicates, which must report the same labels the alert would have carried).
//
// Exported as a function, not as the two constants: the whole point of §2.4's `scope` correction
// is that the API enum (Cluster|Namespaced) is NOT what is published, and handing a caller the
// output values without the translation invites it to skip the translation.
func ScopeLabelValue(apiScope string) string { return metricScope(apiScope) }

func withLabel(base []string, extra string) []string {
	return append(slices.Clone(base), extra)
}
