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
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// The restore metric family (M2, R19, spec/05-observability.md §2.3): ONE family covers
// both restore kinds — a ClusterRestore is recorded under its SOURCE (origin) namespace,
// exactly like the namespaced Restore it is the admin twin of. Like every crystalbackup_
// series these are STATE-DERIVED at scrape time (restart-safe, no in-process counters);
// the M6 catalogue's histogram/counter variants layer on later. A namespaced Restore's
// origin/location/tenant resolve through its source Backup (best-effort — a source that no
// longer resolves leaves them empty, a gap rather than a lie).
var (
	restoreLabels = []string{namespaceLabel, tenantLabel, originLabel, locationLabel, clusterLabel}

	restoreLastSuccessDesc = prometheus.NewDesc(
		NameRestoreLastSuccess,
		"Unix time of the last Completed Restore/ClusterRestore for this series.",
		restoreLabels, nil)
	restoreLastBytesDesc = prometheus.NewDesc(
		NameRestoreLastBytes,
		"status.restoredBytes of the last Completed Restore/ClusterRestore for this series.",
		restoreLabels, nil)
	restoreFailuresDesc = prometheus.NewDesc(
		NameRestoreFailures,
		"Number of Restores/ClusterRestores currently in a failed terminal phase (Failed or PartiallyFailed) for this series.",
		restoreLabels, nil)
	// The same restores restore_failures counts, measured in VOLUMES instead of objects — so the two
	// can be read side by side, and an alert can tell "one restore lost one volume" from "one restore
	// lost all nine". Volumes only: a restore that lost only manifests reports 0 here and still counts
	// in restore_failures, which is the same volumes/manifests split status.failedVolumes documents.
	restoreVolumesFailedDesc = prometheus.NewDesc(
		NameRestoreVolumesFailed,
		"Volumes that did not come back, summed over the Restores/ClusterRestores currently in a failed terminal phase (Failed or PartiallyFailed) for this series (status.failedVolumes). Counts volumes only — a restore whose manifests failed and whose volumes all landed contributes 0.",
		restoreLabels, nil)
)

// restoreSourceInfo is what the collector resolves about a restore's SOURCE: the identity
// labels of its series.
type restoreSourceInfo struct {
	tenant, origin, location, cluster string
}

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

// restoreSeriesKey / restoreSeries accumulate one series across its restores. The key is an
// ALIAS of the exported RestoreSeries (events.go) so the gauges here and the duration/failure
// counters there can never drift apart on label order.
type restoreSeriesKey = RestoreSeries

type restoreSeries struct {
	lastSuccessUnix float64
	lastBytes       float64
	failures        float64
	volumesFailed   float64
}

// collectRestores derives the unified restore family from BOTH kinds. resolveSource maps a
// namespaced Restore's (namespace, source backup name) to its identity labels via the
// Backups already listed for the backup families — no extra API reads.
func collectRestores(ch chan<- prometheus.Metric, restores []cbv1.Restore, clusterRestores []cbv1.ClusterRestore,
	resolveSource func(namespace, backupName string) restoreSourceInfo,
	clusterByLocation map[string]string,
) {
	series := map[restoreSeriesKey]*restoreSeries{}
	tally := func(key restoreSeriesKey, phase string, conds []metav1.Condition, restoredBytes int64, failedVolumes int32) {
		s := series[key]
		if s == nil {
			s = &restoreSeries{}
			series[key] = s
		}
		switch status.RestorePhase(phase) {
		case status.RestorePhaseCompleted:
			if ts := terminalTime(conds); ts > s.lastSuccessUnix {
				s.lastSuccessUnix = ts
				s.lastBytes = float64(restoredBytes)
			}
		case status.RestorePhaseFailed, status.RestorePhasePartiallyFailed:
			// The object count and the volume count move together, in one place, over the same set of
			// restores: read side by side they say how bad each failure was, and they cannot disagree
			// about WHICH restores they describe.
			s.failures++
			s.volumesFailed += float64(failedVolumes)
		}
	}

	for i := range restores {
		r := &restores[i]
		var src restoreSourceInfo
		if r.Spec.Source.Backup != "" {
			src = resolveSource(r.Namespace, r.Spec.Source.Backup)
		}
		tally(restoreSeriesKey{
			Namespace: r.Namespace,
			Tenant:    src.tenant,
			Origin:    src.origin,
			Location:  src.location,
			Cluster:   src.cluster,
		}, r.Status.Phase, r.Status.Conditions, r.Status.RestoredBytes, r.Status.FailedVolumes)
	}

	// A ClusterRestore is recorded under its SOURCE namespace (05-observability §2.3): the
	// admin restored THAT namespace's data, wherever it landed. Its origin is always the
	// cluster plane, and its tenant defaults to the source namespace (the namespace — and
	// its tenant label — may no longer exist; the restic tenant tag is not surfaced in the
	// CR, so the namespace itself is the honest v1 fallback, matching the M1 tenant default).
	for i := range clusterRestores {
		cr := &clusterRestores[i]
		tally(restoreSeriesKey{
			Namespace: cr.Spec.Source.Namespace,
			Tenant:    cr.Spec.Source.Namespace,
			Origin:    apiconst.OriginCluster,
			Location:  cr.Spec.Source.LocationRef.Name,
			Cluster:   clusterByLocation[cr.Spec.Source.LocationRef.Name],
		}, cr.Status.Phase, cr.Status.Conditions, cr.Status.RestoredBytes, cr.Status.FailedVolumes)
	}

	for key, s := range series {
		if s.lastSuccessUnix > 0 {
			ch <- prometheus.MustNewConstMetric(restoreLastSuccessDesc, prometheus.GaugeValue, s.lastSuccessUnix, key.values()...)
			ch <- prometheus.MustNewConstMetric(restoreLastBytesDesc, prometheus.GaugeValue, s.lastBytes, key.values()...)
		}
		// Both emitted unconditionally, zero included: a series that has a restore in it has been
		// measured, and "no volume was lost" is an answer. (The never-measured rule this package
		// applies elsewhere is about series that do not exist at all.)
		ch <- prometheus.MustNewConstMetric(restoreFailuresDesc, prometheus.GaugeValue, s.failures, key.values()...)
		ch <- prometheus.MustNewConstMetric(restoreVolumesFailedDesc, prometheus.GaugeValue, s.volumesFailed, key.values()...)
	}
}
