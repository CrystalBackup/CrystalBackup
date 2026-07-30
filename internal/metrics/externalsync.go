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
		NameExternalSyncLastSuccess,
		"Unix time of the last Completed external sync for this series.",
		externalSyncLabels, nil)
	externalSyncSnapshotsCopiedDesc = prometheus.NewDesc(
		NameExternalSyncCopied,
		"Snapshots present at the destination as copies of the source, as of the last completed sync.",
		externalSyncLabels, nil)
	externalSyncLagDesc = prometheus.NewDesc(
		NameExternalSyncLag,
		"Source snapshots NOT yet at the destination, as of the last completed sync. Zero is the property a secondary exists for.",
		externalSyncLabels, nil)
	externalSyncFailuresDesc = prometheus.NewDesc(
		NameExternalSyncFailures,
		"External syncs currently in a failed terminal phase (Failed or PartiallyFailed) for this series.",
		externalSyncLabels, nil)

	// crystalbackup_externalsync_active is schedule_active's counterpart for §2.12, and it closes a
	// defect that was LATENT rather than theoretical: ClusterBackupExternalSync.spec.paused has
	// existed since M5 and nothing read it, so the moment ExternalSyncStale shipped, an operator who
	// paused a sync — the documented way to stop writes before decommissioning a location — would
	// have been paged about it 26 hours later, with the rule correctly reporting that the thing they
	// deliberately stopped had indeed stopped.
	//
	// 0 rather than absent, for the same reason schedule_active is: `== 1` in the guard drops a 0
	// exactly as it drops a missing series, and only the 0 leaves something behind that says "this
	// secondary is deliberately not being maintained". A sync that is DELETED still disappears.
	externalSyncActiveDesc = prometheus.NewDesc(
		NameExternalSyncActive,
		"1 when a sync exists for this pair and is expected to copy, 0 when every sync on the pair is paused.",
		externalSyncLabels, nil)

	// crystalbackup_externalsync_created_timestamp_seconds — when this pair started being expected
	// to stay in step. schedule_created's counterpart, added for the same reason and against a
	// sharper failure.
	//
	// last_success is 0 for a sync that has never completed, deliberately (see below). On a REAL
	// clock `time() - 0` is about 1.77e9 seconds — fifty-four years — so a pure staleness rule
	// crosses a 26 h threshold on the FIRST scrape after any sync is created, and only the rule's
	// 1 h hold stands between `kubectl apply` and a page reading "has not completed in 26h" about an
	// object ninety minutes old whose first copy is not due until tonight. promtool's synthetic
	// clock starts at the epoch, which is exactly why the unit tests did not show it: there, 0 and
	// "created just now" are the same instant.
	//
	// The fix belongs in the rule, not here. Rewriting last_success to something non-zero would make
	// every dashboard reading "last successful copy" lie about a sync that has never copied
	// anything; 0 means "never", which is the true and useful thing to show. So the metric goes on
	// saying it, and the rule measures age from the last success when there was one and from this
	// series when there was not.
	externalSyncCreatedDesc = prometheus.NewDesc(
		NameExternalSyncCreated,
		"Unix time the external-sync object was created — the instant from which copies started being expected for this pair.",
		externalSyncLabels, nil)
)

// externalSyncSeriesKey identifies one sync: the CR, the pair it copies between, and where it
// sits. An ALIAS of the exported ExternalSyncSeries (events.go), which the duration histogram is
// recorded against — one declaration, one label order.
type externalSyncSeriesKey = ExternalSyncSeries

type externalSyncSeries struct {
	lastSuccessUnix float64
	snapshotsCopied float64
	lag             float64
	failures        float64
	// active is an OR across the CRs folded into this series, not a last-one-wins like the
	// timestamps above. Two syncs can address the same (source, destination) pair, and if even one
	// of them is still expected to copy, the pair is still being maintained — reporting 0 there
	// would silence ExternalSyncStale on a relationship that genuinely has a live sync behind it.
	active bool
	// createdUnix is the EARLIEST creation among the CRs folded into this series, not the latest.
	// It is the fallback age for a pair that has never completed a copy, and the honest reading of
	// "how long has this been failing" is measured from the first time somebody asked for it: a
	// sync that has sat there never succeeding for a month must not have its clock reset by a
	// second sync being added onto the same pair today.
	createdUnix float64
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
	tally := func(key externalSyncSeriesKey, paused bool, created float64,
		phase string, lastSuccess float64, copied, lag int32,
	) {
		s := series[key]
		if s == nil {
			s = &externalSyncSeries{createdUnix: created}
			series[key] = s
		}
		s.active = s.active || !paused
		if created < s.createdUnix {
			s.createdUnix = created
		}
		switch phase {
		case phaseCompleted:
			// Last one wins on timestamp, so several CRs sharing a series (two schedules onto the
			// same pair) report the most recent state rather than an arbitrary one.
			if lastSuccess >= s.lastSuccessUnix {
				s.lastSuccessUnix = lastSuccess
				s.snapshotsCopied = float64(copied)
				s.lag = float64(lag)
			}
		case phaseFailed, phasePartiallyFailed:
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
			Sync:        cs.Name,
			Source:      cs.Spec.SourceLocationRef.Name,
			Destination: cs.Spec.DestinationLocationRef.Name,
			Scope:       apiconst.OriginCluster,
			// No namespace: a cluster sync belongs to the platform, not to a tenant. An empty
			// label is the honest rendering — inventing one would make it group with a namespace.
			Cluster: clusterByLocation[cs.Spec.SourceLocationRef.Name],
		}, cs.Spec.Paused, float64(cs.CreationTimestamp.Unix()),
			cs.Status.Phase, unixOrZero(cs.Status.LastSuccessTime),
			cs.Status.SnapshotsCopied, cs.Status.LagSnapshots)
	}

	for i := range syncs {
		bs := &syncs[i]
		tally(externalSyncSeriesKey{
			Sync:        bs.Name,
			Source:      bs.Spec.SourceLocationRef.Name,
			Destination: bs.Spec.DestinationLocationRef.Name,
			Scope:       apiconst.OriginNamespace,
			Namespace:   bs.Namespace,
		}, bs.Spec.Paused, float64(bs.CreationTimestamp.Unix()),
			bs.Status.Phase, unixOrZero(bs.Status.LastSuccessTime),
			bs.Status.SnapshotsCopied, bs.Status.LagSnapshots)
	}

	for key, s := range series {
		labels := key.values()
		// Emitted unconditionally, INCLUDING a zero last-success. A sync that has never completed
		// must appear in the scrape as 0 rather than be absent: `time() - metric` on an absent
		// series produces no alert at all, so a secondary that never worked from day one would be
		// the one case ExternalSyncStale silently misses.
		//
		// The 0 is a SENTINEL, not a timestamp, and the created series next to it is what lets the
		// rule tell the difference. Without it `time() - 0` is fifty-four years on a real clock and
		// every freshly created sync is instantly past a 26 h bound.
		ch <- prometheus.MustNewConstMetric(externalSyncLastSuccessDesc, prometheus.GaugeValue, s.lastSuccessUnix, labels...)
		ch <- prometheus.MustNewConstMetric(externalSyncCreatedDesc, prometheus.GaugeValue, s.createdUnix, labels...)
		ch <- prometheus.MustNewConstMetric(externalSyncSnapshotsCopiedDesc, prometheus.GaugeValue, s.snapshotsCopied, labels...)
		ch <- prometheus.MustNewConstMetric(externalSyncLagDesc, prometheus.GaugeValue, s.lag, labels...)
		ch <- prometheus.MustNewConstMetric(externalSyncFailuresDesc, prometheus.GaugeValue, s.failures, labels...)
		ch <- prometheus.MustNewConstMetric(externalSyncActiveDesc, prometheus.GaugeValue, boolValue(s.active), labels...)
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
