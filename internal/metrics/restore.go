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

import (
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// The restore metric families (M2, R19): like every crystalbackup_ series they are
// STATE-DERIVED — computed from the live Restore/ClusterRestore objects at scrape time, so
// they survive operator restarts and never need in-process counters. A namespaced
// Restore's series carries the origin namespace; its cluster label is resolved through the
// source Backup's location (best-effort — a time-based source that no longer resolves
// leaves it empty, a gap rather than a lie). A ClusterRestore's namespace label is its
// TARGET namespace.
var (
	restoreLabels        = []string{namespaceLabel, clusterLabel}
	clusterRestoreLabels = []string{namespaceLabel, locationLabel, clusterLabel}

	restoreLastSuccessDesc = prometheus.NewDesc(
		"crystalbackup_restore_last_success_timestamp_seconds",
		"Unix time the last Completed Restore in this namespace reached its terminal phase.",
		restoreLabels, nil)
	restoreLastBytesDesc = prometheus.NewDesc(
		"crystalbackup_restore_last_restored_bytes",
		"Bytes the last Completed Restore in this namespace wrote (status.restoredBytes).",
		restoreLabels, nil)
	restoreFailuresDesc = prometheus.NewDesc(
		"crystalbackup_restore_failures",
		"Number of Restores currently in a failed terminal phase (Failed or PartiallyFailed) for this series.",
		restoreLabels, nil)

	clusterRestoreLastSuccessDesc = prometheus.NewDesc(
		"crystalbackup_clusterrestore_last_success_timestamp_seconds",
		"Unix time the last Completed ClusterRestore into this target namespace reached its terminal phase.",
		clusterRestoreLabels, nil)
	clusterRestoreFailuresDesc = prometheus.NewDesc(
		"crystalbackup_clusterrestore_failures",
		"Number of ClusterRestores currently in a failed terminal phase for this series.",
		clusterRestoreLabels, nil)
)

// terminalTime reads WHEN a restore reached its terminal phase: the Ready condition's last
// transition (neither restore status carries a completion timestamp by contract). Zero when
// the condition is absent.
func terminalTime(conds []metav1.Condition) float64 {
	c := status.FindCondition(conds, "Ready")
	if c == nil {
		return 0
	}
	return float64(c.LastTransitionTime.Unix())
}

// restoreSeries accumulates one {namespace, cluster} restore series.
type restoreSeries struct {
	lastSuccessUnix float64
	lastBytes       float64
	failures        float64
}

// collectRestores derives the namespaced-Restore series. backupLocationCluster resolves a
// (namespace, source backup name) to its cluster label via the projected Backups already
// listed for the backup families — no extra API reads.
func collectRestores(ch chan<- prometheus.Metric, restores []cbv1.Restore,
	backupCluster func(namespace, backupName string) string,
) {
	series := map[[2]string]*restoreSeries{}
	for i := range restores {
		r := &restores[i]
		cluster := ""
		if r.Spec.Source.Backup != "" {
			cluster = backupCluster(r.Namespace, r.Spec.Source.Backup)
		}
		key := [2]string{r.Namespace, cluster}
		s := series[key]
		if s == nil {
			s = &restoreSeries{}
			series[key] = s
		}
		switch status.RestorePhase(r.Status.Phase) {
		case status.RestorePhaseCompleted:
			if ts := terminalTime(r.Status.Conditions); ts > s.lastSuccessUnix {
				s.lastSuccessUnix = ts
				s.lastBytes = float64(r.Status.RestoredBytes)
			}
		case status.RestorePhaseFailed, status.RestorePhasePartiallyFailed:
			s.failures++
		}
	}
	for key, s := range series {
		labels := []string{key[0], key[1]}
		if s.lastSuccessUnix > 0 {
			ch <- prometheus.MustNewConstMetric(restoreLastSuccessDesc, prometheus.GaugeValue, s.lastSuccessUnix, labels...)
			ch <- prometheus.MustNewConstMetric(restoreLastBytesDesc, prometheus.GaugeValue, s.lastBytes, labels...)
		}
		ch <- prometheus.MustNewConstMetric(restoreFailuresDesc, prometheus.GaugeValue, s.failures, labels...)
	}
}

// collectClusterRestores derives the ClusterRestore series, keyed by (target namespace,
// location, cluster).
func collectClusterRestores(ch chan<- prometheus.Metric, restores []cbv1.ClusterRestore, clusterByLocation map[string]string) {
	type key struct{ namespace, location, cluster string }
	type series struct {
		lastSuccessUnix float64
		failures        float64
	}
	acc := map[key]*series{}
	for i := range restores {
		cr := &restores[i]
		k := key{
			namespace: cr.Spec.Target.Namespace,
			location:  cr.Spec.Source.LocationRef.Name,
			cluster:   clusterByLocation[cr.Spec.Source.LocationRef.Name],
		}
		s := acc[k]
		if s == nil {
			s = &series{}
			acc[k] = s
		}
		switch status.RestorePhase(cr.Status.Phase) {
		case status.RestorePhaseCompleted:
			if ts := terminalTime(cr.Status.Conditions); ts > s.lastSuccessUnix {
				s.lastSuccessUnix = ts
			}
		case status.RestorePhaseFailed, status.RestorePhasePartiallyFailed:
			s.failures++
		}
	}
	for k, s := range acc {
		labels := []string{k.namespace, k.location, k.cluster}
		if s.lastSuccessUnix > 0 {
			ch <- prometheus.MustNewConstMetric(clusterRestoreLastSuccessDesc, prometheus.GaugeValue, s.lastSuccessUnix, labels...)
		}
		ch <- prometheus.MustNewConstMetric(clusterRestoreFailuresDesc, prometheus.GaugeValue, s.failures, labels...)
	}
}
