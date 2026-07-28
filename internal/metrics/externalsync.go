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
)

// The external-sync metric family (M5, R28, spec/05-observability.md).
//
// ONE family covers both planes, the way the restore family covers both restore kinds: a
// ClusterBackupExternalSync is recorded with an empty namespace and origin=cluster, a
// BackupExternalSync under its own namespace and origin=namespace. An operator asking "is my
// secondary current" is asking the same question either way.
//
// The series that matters is LAG. A last-success timestamp answers "did a sync run", which is the
// question a broken sync answers reassuringly right up until it stops running at all — and the
// failure this family exists to catch is the quiet one: a sync that keeps completing while falling
// further behind, because the source is producing snapshots faster than the copy moves them. Lag is
// what makes that visible, and it is what ExternalSyncStale alerts on.
//
// Like every crystalbackup_ series these are STATE-DERIVED at scrape time, so an operator restart
// never resets them and there is no in-process counter to drift.
const (
	// syncLabel is the sync CR's own name. The ExternalSyncStale annotation names it, and without
	// it an operator reading a firing alert learns which PAIR is stale but not which object to go
	// and look at.
	syncLabel = "sync"
	// sourceLabel / destinationLabel are the two location names. BOTH are labels, because a
	// location can be the destination of one sync and the source of another (a chain), and
	// collapsing them onto one would silently merge the two relationships.
	sourceLabel      = "source"
	destinationLabel = "destination"
)

var (
	// The label set 05-observability §2.12 specifies. scope (not origin) because this family is
	// about a REPOSITORY relationship, like the repository family, not about which plane produced
	// a backup.
	externalSyncLabels = []string{syncLabel, sourceLabel, destinationLabel, scopeLabel, namespaceLabel, clusterLabel}

	externalSyncLastSuccessDesc = prometheus.NewDesc(
		"crystalbackup_externalsync_last_success_timestamp_seconds",
		"Unix time of the last Completed external sync for this series.",
		externalSyncLabels, nil)
	externalSyncSnapshotsCopiedDesc = prometheus.NewDesc(
		"crystalbackup_externalsync_snapshots_copied",
		"Snapshots present at the destination as copies of the source, as of the last completed sync.",
		externalSyncLabels, nil)
	externalSyncLagDesc = prometheus.NewDesc(
		"crystalbackup_externalsync_lag_snapshots",
		"Source snapshots NOT yet at the destination, as of the last completed sync. Zero is the property a secondary exists for.",
		externalSyncLabels, nil)
	externalSyncFailuresDesc = prometheus.NewDesc(
		"crystalbackup_externalsync_failures",
		"External syncs currently in a failed terminal phase (Failed or PartiallyFailed) for this series.",
		externalSyncLabels, nil)
)

// externalSyncSeriesKey identifies one sync: the CR, the pair it copies between, and where it sits.
type externalSyncSeriesKey struct {
	sync, source, destination, scope, namespace, cluster string
}

func (k externalSyncSeriesKey) values() []string {
	return []string{k.sync, k.source, k.destination, k.scope, k.namespace, k.cluster}
}

type externalSyncSeries struct {
	lastSuccessUnix float64
	snapshotsCopied float64
	lag             float64
	failures        float64
}

// collectExternalSyncs derives the family from both planes. clusterByLocation resolves the
// `cluster` label from the location a cluster sync references; a namespaced sync's locations are
// namespaced and carry no entry there, so its cluster label stays empty — a gap rather than a
// wrong cluster ID borrowed from a same-named admin location.
func collectExternalSyncs(ch chan<- prometheus.Metric,
	clusterSyncs []cbv1.ClusterBackupExternalSync, syncs []cbv1.BackupExternalSync,
	clusterByLocation map[string]string,
) {
	series := map[externalSyncSeriesKey]*externalSyncSeries{}
	tally := func(key externalSyncSeriesKey, phase string, lastSuccess float64, copied, lag int32) {
		s := series[key]
		if s == nil {
			s = &externalSyncSeries{}
			series[key] = s
		}
		switch phase {
		case "Completed":
			// Last one wins on timestamp, so several CRs sharing a series (two schedules onto the
			// same pair) report the most recent state rather than an arbitrary one.
			if lastSuccess >= s.lastSuccessUnix {
				s.lastSuccessUnix = lastSuccess
				s.snapshotsCopied = float64(copied)
				s.lag = float64(lag)
			}
		case "Failed", "PartiallyFailed":
			s.failures++
			// A PartiallyFailed run still measured the lag it could — and that measurement is
			// exactly what an operator needs when something is wrong. Keep it if it is fresher.
			if lastSuccess >= s.lastSuccessUnix {
				s.lag = float64(lag)
			}
		}
	}

	for i := range clusterSyncs {
		cs := &clusterSyncs[i]
		tally(externalSyncSeriesKey{
			sync:        cs.Name,
			source:      cs.Spec.SourceLocationRef.Name,
			destination: cs.Spec.DestinationLocationRef.Name,
			scope:       apiconst.OriginCluster,
			// No namespace: a cluster sync belongs to the platform, not to a tenant. An empty
			// label is the honest rendering — inventing one would make it group with a namespace.
			cluster: clusterByLocation[cs.Spec.SourceLocationRef.Name],
		}, cs.Status.Phase, unixOrZero(cs.Status.LastSuccessTime),
			cs.Status.SnapshotsCopied, cs.Status.LagSnapshots)
	}

	for i := range syncs {
		bs := &syncs[i]
		tally(externalSyncSeriesKey{
			sync:        bs.Name,
			source:      bs.Spec.SourceLocationRef.Name,
			destination: bs.Spec.DestinationLocationRef.Name,
			scope:       apiconst.OriginNamespace,
			namespace:   bs.Namespace,
		}, bs.Status.Phase, unixOrZero(bs.Status.LastSuccessTime),
			bs.Status.SnapshotsCopied, bs.Status.LagSnapshots)
	}

	for key, s := range series {
		labels := key.values()
		// Emitted unconditionally, INCLUDING a zero last-success. A sync that has never completed
		// must appear in the scrape as 0 rather than be absent: `time() - metric` on an absent
		// series produces no alert at all, so a secondary that never worked from day one would be
		// the one case ExternalSyncStale silently misses.
		ch <- prometheus.MustNewConstMetric(externalSyncLastSuccessDesc, prometheus.GaugeValue, s.lastSuccessUnix, labels...)
		ch <- prometheus.MustNewConstMetric(externalSyncSnapshotsCopiedDesc, prometheus.GaugeValue, s.snapshotsCopied, labels...)
		ch <- prometheus.MustNewConstMetric(externalSyncLagDesc, prometheus.GaugeValue, s.lag, labels...)
		ch <- prometheus.MustNewConstMetric(externalSyncFailuresDesc, prometheus.GaugeValue, s.failures, labels...)
	}
}

// unixOrZero renders an optional timestamp as Unix seconds, with a nil (never succeeded) reading
// as zero rather than being skipped. Zero is a value an alert can act on; an absent series is not.
func unixOrZero(t *metav1.Time) float64 {
	if t == nil {
		return 0
	}
	return float64(t.Unix())
}
