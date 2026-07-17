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

// Package metrics publishes the crystalbackup_ Prometheus series. It is a
// state-derived collector: on every scrape it reads the current Backup and
// ClusterBackup objects and computes the gauges from their status, rather than
// having the controllers imperatively increment counters. That makes the series
// RESTART-SAFE — an operator restart cannot lose or double-count a gauge that is
// simply recomputed from the objects that survive the restart — and it keeps the
// hot reconcile path free of metrics bookkeeping. Cumulative views (a failure or
// upload RATE) are obtained with PromQL increase()/rate() over these gauges.
//
// The v1 series and their labels follow spec/05-observability.md. This M1 subset
// covers backup health (last success, size, dedup delta, duration, failures) and
// cluster-DR fleet health (run success, fan-out width).
package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// collectTimeout bounds the state reads a single scrape performs, so a slow API
// cannot wedge Prometheus's scrape. The reads are cache-backed and near-instant in
// practice; this is only a backstop.
const collectTimeout = 10 * time.Second

// Version is the operator build version stamped on crystalbackup_build_info. It defaults to "dev"
// and is overridable at link time (-ldflags "-X .../internal/metrics.Version=<v>").
var Version = "dev"

// buildInfoDesc backs crystalbackup_build_info: a constant 1 emitted on EVERY scrape regardless of
// how many backups exist, so /metrics always carries at least one crystalbackup_ series. That makes
// the operator's own metric surface hard-assertable (M1 exit criterion) without first running a
// backup, and gives dashboards a version to join on.
var buildInfoDesc = prometheus.NewDesc(
	"crystalbackup_build_info",
	"A constant 1, labelled with the operator build version; always present.",
	[]string{"version"}, nil)

// backupLabels / clusterBackupLabels are the label sets of the two metric families,
// in the fixed order the metric values are appended below.
var (
	backupLabels        = []string{"namespace", "tenant", "schedule", "origin", "location", "cluster"}
	clusterBackupLabels = []string{"schedule", "location", "cluster"}

	backupLastSuccessDesc = prometheus.NewDesc(
		"crystalbackup_backup_last_success_timestamp_seconds",
		"Unix time of the last Completed or PartiallyCompleted Backup for this series.",
		backupLabels, nil)
	backupLastSizeDesc = prometheus.NewDesc(
		"crystalbackup_backup_last_size_bytes",
		"Logical size of the last successful Backup (sum of status.volumes[].sizeBytes).",
		backupLabels, nil)
	backupLastAddedDesc = prometheus.NewDesc(
		"crystalbackup_backup_last_added_bytes",
		"Deduplicated bytes added by the last successful Backup (sum of status.volumes[].addedBytes).",
		backupLabels, nil)
	backupLastDurationDesc = prometheus.NewDesc(
		"crystalbackup_backup_last_duration_seconds",
		"Wall-clock duration of the last successful Backup (backupTime - creationTimestamp).",
		backupLabels, nil)
	backupFailuresDesc = prometheus.NewDesc(
		"crystalbackup_backup_failures",
		"Number of Backups currently in a failed terminal phase (Failed or PartiallyFailed) for this series.",
		backupLabels, nil)

	clusterBackupLastSuccessDesc = prometheus.NewDesc(
		"crystalbackup_clusterbackup_last_success_timestamp_seconds",
		"Unix time of the last Completed ClusterBackup run for this series.",
		clusterBackupLabels, nil)
	clusterBackupNamespacesMatchedDesc = prometheus.NewDesc(
		"crystalbackup_clusterbackup_namespaces_matched",
		"Namespaces matched by the last ClusterBackup run for this series (status.namespacesMatched).",
		clusterBackupLabels, nil)
	clusterBackupNamespacesFailedDesc = prometheus.NewDesc(
		"crystalbackup_clusterbackup_namespaces_failed",
		"Namespaces with a failed child Backup in the last ClusterBackup run for this series (status.namespacesFailed).",
		clusterBackupLabels, nil)
)

// Collector reads Backup/ClusterBackup state through a (cached) reader and emits the
// crystalbackup_ series at scrape time. Register it once on the controller-runtime
// metrics registry (see cmd/main.go) so it is served on the operator's /metrics.
type Collector struct {
	reader client.Reader
}

// NewCollector builds a Collector over reader (the manager's cached client in
// production; a fake client in tests).
func NewCollector(reader client.Reader) *Collector { return &Collector{reader: reader} }

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- buildInfoDesc
	ch <- backupLastSuccessDesc
	ch <- backupLastSizeDesc
	ch <- backupLastAddedDesc
	ch <- backupLastDurationDesc
	ch <- backupFailuresDesc
	ch <- clusterBackupLastSuccessDesc
	ch <- clusterBackupNamespacesMatchedDesc
	ch <- clusterBackupNamespacesFailedDesc
}

// Collect implements prometheus.Collector. It reads the live objects once and emits
// one series per label set. A read error yields no series for that family (a scrape
// never fails), so a transient API blip shows as a gap, not a crash.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(buildInfoDesc, prometheus.GaugeValue, 1, Version)

	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	clusterByLocation := c.locationClusterIDs(ctx)

	var backups cbv1.BackupList
	if err := c.reader.List(ctx, &backups); err == nil {
		collectBackups(ch, backups.Items, clusterByLocation)
	}
	var runs cbv1.ClusterBackupList
	if err := c.reader.List(ctx, &runs); err == nil {
		collectClusterBackups(ch, runs.Items, clusterByLocation)
	}
}

// locationClusterIDs maps each ClusterBackupLocation name to its clusterID, so a
// backup's `cluster` label can be resolved from the location it references. A read
// failure yields an empty map (the cluster label is then empty — a gap, not a crash).
func (c *Collector) locationClusterIDs(ctx context.Context) map[string]string {
	out := map[string]string{}
	var locs cbv1.ClusterBackupLocationList
	if err := c.reader.List(ctx, &locs); err != nil {
		return out
	}
	for i := range locs.Items {
		out[locs.Items[i].Name] = locs.Items[i].Spec.ClusterID
	}
	return out
}

// backupSeriesKey is the 6-tuple that identifies one Backup metric series. Many
// Backups (successive runs in a namespace) collapse to one series, so the collector
// groups by this key and emits the latest/aggregate — never a duplicate series.
type backupSeriesKey struct {
	namespace, tenant, schedule, origin, location, cluster string
}

func (k backupSeriesKey) values() []string {
	return []string{k.namespace, k.tenant, k.schedule, k.origin, k.location, k.cluster}
}

// backupSeries accumulates the state of one series across its Backups.
type backupSeries struct {
	lastSuccessUnix float64
	lastSize        float64
	lastAdded       float64
	lastDuration    float64
	failures        float64
}

func collectBackups(ch chan<- prometheus.Metric, backups []cbv1.Backup, clusterByLocation map[string]string) {
	series := map[backupSeriesKey]*backupSeries{}
	for i := range backups {
		b := &backups[i]
		key := backupKey(b, clusterByLocation)
		s := series[key]
		if s == nil {
			s = &backupSeries{}
			series[key] = s
		}
		accumulateBackup(s, b)
	}
	for key, s := range series {
		vals := key.values()
		// last_success is only meaningful once a backup has succeeded; the others hang off it.
		if s.lastSuccessUnix > 0 {
			ch <- prometheus.MustNewConstMetric(backupLastSuccessDesc, prometheus.GaugeValue, s.lastSuccessUnix, vals...)
			ch <- prometheus.MustNewConstMetric(backupLastSizeDesc, prometheus.GaugeValue, s.lastSize, vals...)
			ch <- prometheus.MustNewConstMetric(backupLastAddedDesc, prometheus.GaugeValue, s.lastAdded, vals...)
			ch <- prometheus.MustNewConstMetric(backupLastDurationDesc, prometheus.GaugeValue, s.lastDuration, vals...)
		}
		ch <- prometheus.MustNewConstMetric(backupFailuresDesc, prometheus.GaugeValue, s.failures, vals...)
	}
}

// accumulateBackup folds one Backup into its series: it tracks the latest successful
// backup's success time/size/added/duration, and counts the failed ones.
func accumulateBackup(s *backupSeries, b *cbv1.Backup) {
	switch b.Status.Phase {
	case string(status.BackupPhaseCompleted), string(status.BackupPhasePartiallyCompleted):
		if b.Status.BackupTime == nil {
			return
		}
		t := float64(b.Status.BackupTime.Unix())
		if t <= s.lastSuccessUnix {
			return // an older (or equal) success — keep the latest.
		}
		s.lastSuccessUnix = t
		var size, added int64
		for _, v := range b.Status.Volumes {
			size += v.SizeBytes
			added += v.AddedBytes
		}
		s.lastSize = float64(size)
		s.lastAdded = float64(added)
		s.lastDuration = t - float64(b.CreationTimestamp.Unix())
		if s.lastDuration < 0 {
			s.lastDuration = 0
		}
	case string(status.BackupPhaseFailed), string(status.BackupPhasePartiallyFailed):
		s.failures++
	}
}

// backupKey derives a Backup's series key: namespace and origin/schedule from its
// labels, tenant defaulting to the namespace (one tenant per namespace, R19),
// location from its spec, and cluster resolved from that location's clusterID.
func backupKey(b *cbv1.Backup, clusterByLocation map[string]string) backupSeriesKey {
	tenant := b.Labels[apiconst.LabelTenant]
	if tenant == "" {
		tenant = b.Namespace
	}
	location := b.Spec.LocationRef.Name
	return backupSeriesKey{
		namespace: b.Namespace,
		tenant:    tenant,
		schedule:  b.Labels[apiconst.LabelSchedule],
		origin:    b.Labels[apiconst.LabelOrigin],
		location:  location,
		cluster:   clusterByLocation[location],
	}
}

// clusterBackupSeriesKey identifies one ClusterBackup (fleet-DR) metric series.
type clusterBackupSeriesKey struct {
	schedule, location, cluster string
}

func (k clusterBackupSeriesKey) values() []string { return []string{k.schedule, k.location, k.cluster} }

type clusterBackupSeries struct {
	lastSuccessUnix   float64
	namespacesMatched float64
	namespacesFailed  float64
	latestRunUnix     float64 // creation time of the run backing matched/failed, to keep the latest
}

func collectClusterBackups(ch chan<- prometheus.Metric, runs []cbv1.ClusterBackup, clusterByLocation map[string]string) {
	series := map[clusterBackupSeriesKey]*clusterBackupSeries{}
	for i := range runs {
		run := &runs[i]
		location := run.Spec.LocationRef.Name
		key := clusterBackupSeriesKey{
			schedule: run.Spec.ScheduleRef,
			location: location,
			cluster:  clusterByLocation[location],
		}
		s := series[key]
		if s == nil {
			s = &clusterBackupSeries{}
			series[key] = s
		}
		accumulateClusterBackup(s, run)
	}
	for key, s := range series {
		vals := key.values()
		if s.lastSuccessUnix > 0 {
			ch <- prometheus.MustNewConstMetric(clusterBackupLastSuccessDesc, prometheus.GaugeValue, s.lastSuccessUnix, vals...)
		}
		ch <- prometheus.MustNewConstMetric(clusterBackupNamespacesMatchedDesc, prometheus.GaugeValue, s.namespacesMatched, vals...)
		ch <- prometheus.MustNewConstMetric(clusterBackupNamespacesFailedDesc, prometheus.GaugeValue, s.namespacesFailed, vals...)
	}
}

// accumulateClusterBackup folds one run into its series: the latest Completed run's
// success time, and the latest run's matched/failed namespace counts (by creation time).
func accumulateClusterBackup(s *clusterBackupSeries, run *cbv1.ClusterBackup) {
	created := float64(run.CreationTimestamp.Unix())
	if created >= s.latestRunUnix {
		s.latestRunUnix = created
		s.namespacesMatched = float64(run.Status.NamespacesMatched)
		s.namespacesFailed = float64(run.Status.NamespacesFailed)
	}
	if run.Status.Phase == string(status.ClusterBackupPhaseCompleted) && run.Status.CompletionTime != nil {
		if t := float64(run.Status.CompletionTime.Unix()); t > s.lastSuccessUnix {
			s.lastSuccessUnix = t
		}
	}
}
