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

// Package metrics publishes the crystalbackup_ Prometheus series specified in
// spec/05-observability.md §2. It has two halves, and which half a family belongs to is a
// question about the family, not a matter of taste.
//
// The COLLECTOR half (this file and its neighbours) is state-derived: on every scrape it reads
// the current objects and computes gauges from their status, rather than having controllers
// imperatively track them. That makes those series RESTART-SAFE — an operator restart cannot
// lose or double-count a value that is simply recomputed from objects that survive the restart —
// and it keeps the hot reconcile path free of metrics bookkeeping. "What is true right now"
// questions live here: last success, current size, how many failed Backups still exist, whether a
// schedule is expected to run, how many movers are live.
//
// The EVENT half (events.go) is real in-process counters and histograms, incremented at the site
// of the event. It exists because the state-derived trick has a hard limit: an object deleted by
// a history limit takes its contribution with it, so a count of surviving failures is not a total
// and increase() over it under-reports. Duration has no state to be derived from at all. §1 of
// the spec blesses the trade explicitly — counters restart at zero and alerts use increase().
//
// One rule spans both: A NEVER-MEASURED SERIES IS ABSENT, NOT ZERO. A zero is a measurement, and
// publishing one for something nobody has measured yet is how a fresh location pages the platform
// team for the crime of existing. The one deliberate exception is documented at its emission site
// (crystalbackup_externalsync_last_success_timestamp_seconds, externalsync.go).
package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
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
	NameBuildInfo,
	"A constant 1, labelled with the operator build version; always present.",
	[]string{"version"}, nil)

// Prometheus label names shared by the two metric families' label sets below (extracted so the
// repeated names are defined once): the originating schedule, the location, and the cluster
// identity (resolved from a location's clusterID).
const (
	namespaceLabel = "namespace"
	tenantLabel    = "tenant"
	originLabel    = "origin"
	scheduleLabel  = "schedule"
	locationLabel  = "location"
	clusterLabel   = "cluster"
)

// backupLabels / clusterBackupLabels are the label sets of the two metric families,
// in the fixed order the metric values are appended below.
var (
	backupLabels        = []string{namespaceLabel, tenantLabel, scheduleLabel, originLabel, locationLabel, clusterLabel}
	clusterBackupLabels = []string{scheduleLabel, locationLabel, clusterLabel}
	// protectedLabels drops `schedule` from backupLabels: "how much data is protected for this
	// namespace" is a question about the namespace, and summing the same PVC once per schedule
	// that happens to cover it would double-count the volume, not describe it.
	protectedLabels = []string{namespaceLabel, tenantLabel, originLabel, locationLabel, clusterLabel}

	backupLastSuccessDesc = prometheus.NewDesc(
		NameBackupLastSuccess,
		"Unix time of the last Completed or PartiallyCompleted Backup for this series.",
		backupLabels, nil)
	backupLastSizeDesc = prometheus.NewDesc(
		NameBackupLastSize,
		"Logical size of the last successful Backup (sum of status.volumes[].sizeBytes).",
		backupLabels, nil)
	backupLastAddedDesc = prometheus.NewDesc(
		NameBackupLastAdded,
		"Deduplicated bytes added by the last successful Backup (sum of status.volumes[].addedBytes).",
		backupLabels, nil)
	backupLastDurationDesc = prometheus.NewDesc(
		NameBackupLastDuration,
		"Wall-clock duration of the last successful Backup (backupTime - creationTimestamp).",
		backupLabels, nil)
	backupFailuresDesc = prometheus.NewDesc(
		NameBackupFailures,
		"Number of Backups currently in a failed terminal phase (Failed or PartiallyFailed) for this series.",
		backupLabels, nil)
	backupProtectedBytesDesc = prometheus.NewDesc(
		NameBackupProtectedBytes,
		"Logical bytes currently protected for the namespace: the newest recorded size of every PVC that has a live restore point.",
		protectedLabels, nil)

	clusterBackupLastSuccessDesc = prometheus.NewDesc(
		NameClusterBackupLastSuccess,
		"Unix time of the last Completed ClusterBackup run for this series.",
		clusterBackupLabels, nil)
	clusterBackupNamespacesMatchedDesc = prometheus.NewDesc(
		NameClusterBackupNamespacesMatched,
		"Namespaces matched by the last ClusterBackup run for this series (status.namespacesMatched).",
		clusterBackupLabels, nil)
	clusterBackupNamespacesFailedDesc = prometheus.NewDesc(
		NameClusterBackupNamespacesFailed,
		"Namespaces with a failed child Backup in the last ClusterBackup run for this series (status.namespacesFailed).",
		clusterBackupLabels, nil)
)

// Collector reads Backup/ClusterBackup state through a (cached) reader and emits the
// crystalbackup_ series at scrape time. Register it once on the controller-runtime
// metrics registry (see cmd/main.go) so it is served on the operator's /metrics.
type Collector struct {
	reader client.Reader
	// operatorNamespace is where the mover Jobs live. The mover family (§2.7) is a census of
	// those Jobs, and there is no label or owner reference that identifies them cluster-wide —
	// the namespace IS the scope.
	operatorNamespace string
}

// NewCollector builds a Collector over reader (the manager's cached client in
// production; a fake client in tests) and the namespace holding the mover Jobs.
func NewCollector(reader client.Reader, operatorNamespace string) *Collector {
	return &Collector{reader: reader, operatorNamespace: operatorNamespace}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- buildInfoDesc
	ch <- backupLastSuccessDesc
	ch <- backupLastSizeDesc
	ch <- backupLastAddedDesc
	ch <- backupLastDurationDesc
	ch <- backupFailuresDesc
	ch <- backupProtectedBytesDesc
	ch <- scheduleActiveDesc
	ch <- clusterBackupLastSuccessDesc
	ch <- clusterBackupNamespacesMatchedDesc
	ch <- clusterBackupNamespacesFailedDesc
	ch <- restoreLastSuccessDesc
	ch <- restoreLastBytesDesc
	ch <- restoreFailuresDesc
	ch <- repositorySizeDesc
	ch <- repositorySnapshotCountDesc
	ch <- repositoryLastCheckDesc
	ch <- repositoryLastCheckSuccessDesc
	ch <- repositoryLastMaintenanceDesc
	ch <- repositoryStaleLocksDesc
	ch <- discoveryLastTimestampDesc
	ch <- discoveryLastSuccessDesc
	ch <- discoveryProjectedDesc
	ch <- discoveryOrphansDesc
	ch <- erasureBlockedDesc
	ch <- erasureLastCompletionDesc
	ch <- moverActiveDesc
	ch <- moverQueueDepthDesc
	ch <- moverConcurrencyLimitDesc
	ch <- pvcVolumeSnapshotCountDesc
	ch <- externalSyncLastSuccessDesc
	ch <- externalSyncSnapshotsCopiedDesc
	ch <- externalSyncLagDesc
	ch <- externalSyncFailuresDesc
}

// Collect implements prometheus.Collector. It reads the live objects once and emits
// one series per label set. A read error yields no series for that family (a scrape
// never fails), so a transient API blip shows as a gap, not a crash.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(buildInfoDesc, prometheus.GaugeValue, 1, Version)

	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	clusterByLocation, clusterID := c.locationClusterIDs(ctx)

	var backups cbv1.BackupList
	backupsListed := c.reader.List(ctx, &backups) == nil
	if backupsListed {
		collectBackups(ch, backups.Items, clusterByLocation)
	}
	var runs cbv1.ClusterBackupList
	if err := c.reader.List(ctx, &runs); err == nil {
		collectClusterBackups(ch, runs.Items, clusterByLocation)
	}

	// The unified restore family (M2, 05-observability §2.3). A namespaced Restore's
	// origin/location/tenant/cluster resolve through its source Backup — joined against the
	// Backups already listed above via a one-pass index (never a per-restore linear scan:
	// the scrape cost must stay O(backups + restores), not their product).
	sourceByKey := make(map[[2]string]restoreSourceInfo, len(backups.Items))
	if backupsListed {
		for i := range backups.Items {
			b := &backups.Items[i]
			sourceByKey[[2]string{b.Namespace, b.Name}] = restoreSourceInfo{
				tenant:   b.Labels[apiconst.LabelTenant],
				origin:   b.Labels[apiconst.LabelOrigin],
				location: b.Spec.LocationRef.Name,
				cluster:  clusterByLocation[b.Spec.LocationRef.Name],
			}
		}
	}
	resolveSource := func(namespace, backupName string) restoreSourceInfo {
		return sourceByKey[[2]string{namespace, backupName}]
	}
	var restores cbv1.RestoreList
	var clusterRestores cbv1.ClusterRestoreList
	restoresListed := c.reader.List(ctx, &restores) == nil
	clusterRestoresListed := c.reader.List(ctx, &clusterRestores) == nil
	if restoresListed || clusterRestoresListed {
		collectRestores(ch, restores.Items, clusterRestores.Items, resolveSource, clusterByLocation)
	}

	// The repository family (M4, 05-observability §2.4). Per-repository, not per-namespace: a
	// check or prune result on the shared cluster repository is a platform-wide signal. The
	// discovery family (§2.5) rides the same objects — discovery records its outcome on the
	// repository status, so one listing answers both.
	var repos cbv1.BackupRepositoryList
	if err := c.reader.List(ctx, &repos); err == nil {
		collectRepositories(ch, repos.Items, clusterByLocation)
		collectDiscovery(ch, repos.Items, clusterByLocation)
	}

	// schedule_active (§2.1) — the series BackupMissed joins against. It needs the namespaces
	// too, because a ClusterBackupSchedule declares a SELECTION and the metric has to be one
	// series per namespace that selection resolves to.
	var namespaces corev1.NamespaceList
	var schedules cbv1.BackupScheduleList
	var clusterSchedules cbv1.ClusterBackupScheduleList
	namespacesListed := c.reader.List(ctx, &namespaces) == nil
	schedulesListed := c.reader.List(ctx, &schedules) == nil
	clusterSchedulesListed := c.reader.List(ctx, &clusterSchedules) == nil
	if schedulesListed || (clusterSchedulesListed && namespacesListed) {
		collectSchedules(ch, schedules.Items, clusterSchedules.Items, namespaces.Items, clusterByLocation)
	}

	// The erasure family's two gauges (§2.6). Their counter siblings are recorded at the event.
	var erasures cbv1.ClusterErasureList
	if err := c.reader.List(ctx, &erasures); err == nil {
		collectErasures(ch, erasures.Items, clusterByLocation)
	}

	// Concurrency and queueing (§2.7): a census of the live mover Jobs against the configured cap.
	c.collectMovers(ctx, ch, backups.Items, clusterSchedules.Items, runs.Items, clusterID)

	// pvc_volumesnapshot_count (§2.9): the one per-PVC label in the catalogue, and the one that
	// makes coexistence with an incumbent tool visible.
	c.collectPVCSnapshots(ctx, ch, clusterID)

	// The external-sync family (M5, R28). One family, both planes — an operator asking whether
	// their secondary is current is asking the same question on either.
	var clusterSyncs cbv1.ClusterBackupExternalSyncList
	var namespacedSyncs cbv1.BackupExternalSyncList
	clusterSyncsListed := c.reader.List(ctx, &clusterSyncs) == nil
	namespacedSyncsListed := c.reader.List(ctx, &namespacedSyncs) == nil
	if clusterSyncsListed || namespacedSyncsListed {
		collectExternalSyncs(ch, clusterSyncs.Items, namespacedSyncs.Items, clusterByLocation)
	}
}

// locationClusterIDs maps each ClusterBackupLocation name to its clusterID, so a
// backup's `cluster` label can be resolved from the location it references. A read
// failure yields an empty map (the cluster label is then empty — a gap, not a crash).
//
// The second return is THE cluster's identity, for the handful of platform-scope families
// (§2.7, §2.9) that carry `cluster` and nothing else to resolve it from: they describe mover Jobs
// and VolumeSnapshots, which belong to no location. It is the DEFAULT location's clusterID, or —
// when no location claims default — the single value every location agrees on. Disagreement
// yields an empty label rather than an arbitrary pick: clusterID is the identity the platform
// Prometheus also stamps at federation (§8 open question 2), and guessing wrong here would create
// a duplicate series under a second cluster name that nobody could reconcile with the first.
func (c *Collector) locationClusterIDs(ctx context.Context) (map[string]string, string) {
	out := map[string]string{}
	var locs cbv1.ClusterBackupLocationList
	if err := c.reader.List(ctx, &locs); err != nil {
		return out, ""
	}
	var defaultID, consensus string
	agreed := true
	for i := range locs.Items {
		loc := &locs.Items[i]
		out[loc.Name] = loc.Spec.ClusterID
		if loc.Spec.Default && loc.Spec.ClusterID != "" {
			defaultID = loc.Spec.ClusterID
		}
		switch {
		case loc.Spec.ClusterID == "":
		case consensus == "":
			consensus = loc.Spec.ClusterID
		case consensus != loc.Spec.ClusterID:
			agreed = false
		}
	}
	if defaultID != "" {
		return out, defaultID
	}
	if agreed {
		return out, consensus
	}
	return out, ""
}

// backupSeriesKey is the 6-tuple that identifies one Backup metric series. Many
// Backups (successive runs in a namespace) collapse to one series, so the collector
// groups by this key and emits the latest/aggregate — never a duplicate series.
//
// It is an ALIAS of the exported BackupSeries (events.go), not a second declaration: the gauges
// here and the counters there must agree on the label order for the rest of eternity, and an
// alias makes disagreement impossible rather than merely unlikely.
type backupSeriesKey = BackupSeries

// backupSeries accumulates the state of one series across its Backups.
type backupSeries struct {
	lastSuccessUnix float64
	lastSize        float64
	lastAdded       float64
	lastDuration    float64
	failures        float64
}

// protectedSeriesKey identifies one crystalbackup_backup_protected_bytes series: the Backup series
// minus its schedule.
type protectedSeriesKey struct {
	namespace, tenant, origin, location, cluster string
}

func (k protectedSeriesKey) values() []string {
	return []string{k.namespace, k.tenant, k.origin, k.location, k.cluster}
}

// protectedVolume is the newest recorded logical size of ONE PVC, with the time that reading was
// taken so a later run supersedes an earlier one.
type protectedVolume struct {
	atUnix float64
	size   float64
}

func collectBackups(ch chan<- prometheus.Metric, backups []cbv1.Backup, clusterByLocation map[string]string) {
	series := map[backupSeriesKey]*backupSeries{}
	// protected is keyed by series, then by PVC: "how much data is protected" is a sum over
	// DISTINCT volumes, not over backups. Two runs of the same PVC are one protected volume, and
	// a PVC that only the weekly schedule covers is still protected between the dailies — which
	// is exactly why this cannot be read off last_size_bytes.
	protected := map[protectedSeriesKey]map[string]protectedVolume{}
	for i := range backups {
		b := &backups[i]
		key := backupKey(b, clusterByLocation)
		s := series[key]
		if s == nil {
			s = &backupSeries{}
			series[key] = s
		}
		accumulateBackup(s, b)
		accumulateProtected(protected, key, b)
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
	for key, volumes := range protected {
		var total float64
		for _, v := range volumes {
			total += v.size
		}
		ch <- prometheus.MustNewConstMetric(backupProtectedBytesDesc, prometheus.GaugeValue, total, key.values()...)
	}
}

// accumulateProtected folds one Backup's volumes into the protected-bytes view: for every PVC it
// captured successfully, the newest recorded logical size wins.
//
// Only successful terminal phases contribute. A Failed run's volumes carry no size, and a
// PartiallyFailed one's failed volumes carry none either — the per-volume Completed check is what
// keeps a half-finished run from erasing the last known good size of a volume it did not capture.
func accumulateProtected(protected map[protectedSeriesKey]map[string]protectedVolume, key backupSeriesKey, b *cbv1.Backup) {
	if b.Status.BackupTime == nil {
		return
	}
	at := float64(b.Status.BackupTime.Unix())
	pk := protectedSeriesKey{
		namespace: key.Namespace, tenant: key.Tenant,
		origin: key.Origin, location: key.Location, cluster: key.Cluster,
	}
	for _, v := range b.Status.Volumes {
		if v.Phase != status.VolumePhaseCompleted || v.SizeBytes <= 0 {
			continue
		}
		byPVC := protected[pk]
		if byPVC == nil {
			byPVC = map[string]protectedVolume{}
			protected[pk] = byPVC
		}
		if prev, ok := byPVC[v.Pvc]; ok && prev.atUnix >= at {
			continue
		}
		byPVC[v.Pvc] = protectedVolume{atUnix: at, size: float64(v.SizeBytes)}
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
		Namespace: b.Namespace,
		Tenant:    tenant,
		Schedule:  b.Labels[apiconst.LabelSchedule],
		Origin:    b.Labels[apiconst.LabelOrigin],
		Location:  location,
		Cluster:   clusterByLocation[location],
	}
}

// clusterBackupSeriesKey identifies one ClusterBackup (fleet-DR) metric series. An alias of the
// exported ClusterBackupSeries, for the reason backupSeriesKey is one.
type clusterBackupSeriesKey = ClusterBackupSeries

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
			Schedule: run.Spec.ScheduleRef,
			Location: location,
			Cluster:  clusterByLocation[location],
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
