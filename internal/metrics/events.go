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
	"context"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// The EVENT half of the catalogue: every `_total` counter and every histogram.
//
// The rest of this package is state-derived — recomputed from live objects on each scrape, which
// is what makes those gauges restart-safe. These families cannot be, and the reason is not
// stylistic. A counter answers "how many happened", and the objects that would have to hold the
// answer are deleted out from under it: a schedule's successfulRunsHistoryLimit prunes old
// Backups, so counting the ones currently in phase Failed is a gauge of surviving wreckage, not a
// total — it goes DOWN when history is trimmed, and a PromQL increase() over it under-reports by
// exactly the number of failures the cleanup removed. Duration is worse still: a histogram is a
// distribution over events, and there is no object to re-derive the shape of past runs from.
//
// So these are real Prometheus counters and histograms, registered once on the controller-runtime
// registry and incremented at the site of the event, exactly like locksReaped in repository.go.
// spec/05-observability.md §1 blesses this explicitly ("Counters restart at zero; alert
// expressions use increase()"), and increase()/rate() handle the reset a restart causes. The
// gauge families next to them keep answering the "right now" questions across that same restart —
// the two halves are complementary, not redundant.
//
// EVERY recording site here must be reached AT MOST ONCE per event. That is a contract on the
// callers, not something this file can enforce: each one sits behind a controller's
// already-terminal short-circuit and fires only on the transition INTO a terminal phase, after the
// status write that makes the transition durable. Recording before the write would double-count
// every conflict retry.
const (
	// resultLabel carries a terminal phase, lowercased. It is the only label in the catalogue
	// whose value is an outcome rather than an identity, and it is bounded by the phase enum.
	resultLabel = "result"
	// modeLabel is a Restore's spec.mode (Recreate|Overwrite): the two modes fail for entirely
	// different reasons, so a failure count that merged them would hide which one is broken.
	modeLabel = "mode"
	// webhookLabel / reasonLabel identify a denial: which admission webhook, and which of its
	// rules. Both are code-chosen constants, never user input — a reason taken from a request
	// would be an unbounded-cardinality hole straight through §1.
	webhookLabel = "webhook"
	reasonLabel  = "reason"
	// exposerLabel is the SnapshotExposer Kind (csi-generic|cephfs-shallow). The wait it
	// measures is a property of the mechanism, and comparing the two is the point.
	exposerLabel = "exposer"
)

// The terminal phase strings this package tests against. Mirrored here rather than imported from
// internal/status, for the same reason scopeCluster is (repository.go): the metrics package
// depends on the API types and nothing else, so a controller-side refactor can never reshape a
// published series by accident. The mirror is deliberate and the enum is in
// spec/02-api.md.
const (
	phaseCompleted       = "Completed"
	phaseFailed          = "Failed"
	phasePartiallyFailed = "PartiallyFailed"
)

// BackupSeries identifies one per-namespace backup metric series (spec/05-observability.md §2.1).
// It is shared by the state-derived gauges in collector.go and the counters here so the label
// ORDER is defined once — a second []string built by hand is how a label set silently transposes.
type BackupSeries struct {
	Namespace, Tenant, Schedule, Origin, Location, Cluster string
}

func (s BackupSeries) values() []string {
	return []string{s.Namespace, s.Tenant, s.Schedule, s.Origin, s.Location, s.Cluster}
}

// ClusterBackupSeries identifies one run-level (fleet-DR) series (§2.2). No namespace: the
// per-namespace detail lives in the Backup families.
type ClusterBackupSeries struct {
	Schedule, Location, Cluster string
}

func (s ClusterBackupSeries) values() []string { return []string{s.Schedule, s.Location, s.Cluster} }

// RestoreSeries identifies one restore series (§2.3), both kinds.
type RestoreSeries struct {
	Namespace, Tenant, Origin, Location, Cluster string
}

func (s RestoreSeries) values() []string {
	return []string{s.Namespace, s.Tenant, s.Origin, s.Location, s.Cluster}
}

// ExternalSyncSeries identifies one external-sync series (§2.12).
type ExternalSyncSeries struct {
	Sync, Source, Destination, Scope, Namespace, Cluster string
}

func (s ExternalSyncSeries) values() []string {
	return []string{s.Sync, s.Source, s.Destination, s.Scope, s.Namespace, s.Cluster}
}

// erasureLabels is the ClusterErasure family's label set (§2.6): platform-scope, keyed by the
// location erased from. No tenant label, deliberately — the erasure NAMES a tenant in its target,
// and a right-to-erasure metric that carried the erased tenant as a label would keep that identity
// alive in the monitoring stack long after the data it refers to was physically destroyed.
var erasureLabels = []string{locationLabel, clusterLabel}

var (
	backupDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    NameBackupDuration,
		Help:    "End-to-end Backup duration, from creation to terminal phase.",
		Buckets: durationBuckets,
	}, backupLabels)

	backupAddedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: NameBackupAddedTotal,
		Help: "Cumulative deduplicated bytes uploaded to the repository by terminal Backups (S3 egress estimation).",
	}, backupLabels)

	// backupFailuresTotal is the counter the BackupFailed alert reads. Its gauge sibling
	// crystalbackup_backup_failures (no _total) counts the failed Backups that still EXIST, which
	// is a different and also useful question — "is there wreckage in this namespace right now" —
	// but it is not the one increase() can ask.
	backupFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: NameBackupFailuresTotal,
		Help: "Backups that reached Failed or PartiallyFailed.",
	}, backupLabels)

	backupTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: NameBackupTotal,
		Help: "Backups that reached a terminal phase, by result — the per-tenant backup count (R19 accounting input).",
	}, append(append([]string{}, backupLabels...), resultLabel))

	clusterBackupDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    NameClusterBackupDuration,
		Help:    "ClusterBackup run duration, from fan-out start until every child Backup is terminal.",
		Buckets: durationBuckets,
	}, clusterBackupLabels)

	clusterBackupRunsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: NameClusterBackupRunsTotal,
		Help: "ClusterBackup runs that reached a terminal phase, by result (fleet run success ratio).",
	}, append(append([]string{}, clusterBackupLabels...), resultLabel))

	restoreDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    NameRestoreDuration,
		Help:    "Restore/ClusterRestore duration, from creation to terminal phase.",
		Buckets: durationBuckets,
	}, append(append([]string{}, restoreLabels...), modeLabel))

	// restoreFailuresTotal counts Failed and PartiallyFailed restores. AwaitingConfirmation
	// (R23) is deliberately NOT a failure and never reaches here: it is the system asking a
	// question, and paging an operator for an answered-by-design prompt is how an alert gets
	// muted for good.
	restoreFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: NameRestoreFailuresTotal,
		Help: "Restores/ClusterRestores that reached Failed or PartiallyFailed. AwaitingConfirmation is not a failure.",
	}, append(append([]string{}, restoreLabels...), modeLabel))

	externalSyncDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    NameExternalSyncDuration,
		Help:    "External sync run duration (restic copy), from run start to terminal phase.",
		Buckets: durationBuckets,
	}, externalSyncLabels)

	erasureForgottenTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: NameErasureForgottenTotal,
		Help: "Snapshots removed by a completed ClusterErasure (restic forget --tag).",
	}, erasureLabels)

	erasureReclaimedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: NameErasureReclaimedTotal,
		Help: "Bytes physically reclaimed by the prune of a completed ClusterErasure.",
	}, erasureLabels)

	// moverJobRetries is attributed to the namespace whose data the mover was moving, not to the
	// platform: a namespace whose movers keep being retried is a namespace-shaped problem (a
	// storage class that stalls, a volume too big for the window), and the operator who can act
	// on it is the tenant's.
	moverJobRetries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: NameMoverJobRetriesTotal,
		Help: "Mover pod retries consumed against the Job's backoffLimit.",
	}, []string{namespaceLabel, tenantLabel, clusterLabel})

	webhookDenials = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: NameWebhookDenialsTotal,
		Help: "Requests denied by the operator's dynamic admission webhook. Static-rule (VAP) denials are counted by the apiserver, not here.",
	}, []string{webhookLabel, reasonLabel})

	exposureReadyWait = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    NameExposureReadyWait,
		Help:    "Wait from snapshot exposure start until the exposed PVC is bound and a mover can start.",
		Buckets: exposureBuckets,
	}, []string{namespaceLabel, tenantLabel, exposerLabel, clusterLabel})
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		backupDuration, backupAddedTotal, backupFailuresTotal, backupTotal,
		clusterBackupDuration, clusterBackupRunsTotal,
		restoreDuration, restoreFailuresTotal,
		externalSyncDuration,
		erasureForgottenTotal, erasureReclaimedTotal,
		moverJobRetries, webhookDenials, exposureReadyWait,
	)
}

// exemplarTraceIDLabel is the label name a Prometheus exemplar carries a trace id under. It is
// `trace_id` — the name Grafana's exemplar-to-Tempo link looks for by default and the one
// OpenMetrics conventions settled on — and it is NOT a metric label: exemplars live beside the
// bucket, not in the series, so this adds no cardinality whatsoever (spec/05-observability.md §1).
const exemplarTraceIDLabel = "trace_id"

// observe records one histogram sample, attaching the CURRENT trace as an exemplar when there is
// one (spec/05-observability.md §5).
//
// This is the click-through that makes a latency histogram actionable. A p99 spike in
// crystalbackup_backup_duration_seconds says a backup somewhere took twenty minutes; the exemplar
// says WHICH one, and lands the reader in the trace that shows the eleven minutes it spent
// waiting for one CSI driver. Without it the same investigation is a manual join across a time
// range, a namespace guess and a log search.
//
// With tracing inactive — the default, and the overwhelming majority of installations — this is a
// plain Observe: no exemplar, no allocation, and the same samples that were being recorded before
// this lot existed. The recorded span must be SAMPLED as well as valid; hanging an exemplar on a
// trace id that was dropped at the head would produce a link to a trace the backend never
// received, which is worse than no link at all.
func observe(ctx context.Context, o prometheus.Observer, v float64) {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() || !sc.IsSampled() {
		o.Observe(v)
		return
	}
	ex, ok := o.(prometheus.ExemplarObserver)
	if !ok {
		o.Observe(v)
		return
	}
	ex.ObserveWithExemplar(v, prometheus.Labels{exemplarTraceIDLabel: sc.TraceID().String()})
}

// resultOf renders a terminal phase as the `result` label value: the phase, lowercased.
//
// spec/05-observability.md §2.11 enumerates three values for a Backup — completed,
// partiallyfailed, failed — and there are FOUR terminal Backup phases. PartiallyCompleted (some
// volumes skipped as unsupported storage, none failed) is missing from the spec's list, and it is
// not the same outcome as Completed: it is the one an admin needs to see when a storage class
// silently stops being snapshottable. Folding it into `completed` would erase that; it gets its
// own value, and the enum stays bounded at four. ClusterBackup has exactly the three the spec
// lists, so nothing is added there.
func resultOf(phase string) string { return strings.ToLower(phase) }

// RecordBackupTerminal records ONE Backup reaching a terminal phase: its duration, the dedup delta
// it uploaded, its result, and — for a Failed or PartiallyFailed outcome — a failure.
//
// duration is measured the same way crystalbackup_backup_last_duration_seconds is (creation to
// backupTime) so a dashboard can put the gauge and the histogram's quantiles on one axis without
// explaining why they disagree.
func RecordBackupTerminal(ctx context.Context, s BackupSeries, phase string, duration time.Duration, addedBytes int64) {
	vals := s.values()
	backupTotal.WithLabelValues(append(append([]string{}, vals...), resultOf(phase))...).Inc()

	// A negative duration means clock skew between the object's creation timestamp and the
	// operator's now(). Observing it would poison the histogram's sum permanently (a counter that
	// went backwards), so it is clamped rather than dropped: the run DID happen and its count
	// belongs in the distribution.
	if duration < 0 {
		duration = 0
	}
	observe(ctx, backupDuration.WithLabelValues(vals...), duration.Seconds())

	if addedBytes > 0 {
		backupAddedTotal.WithLabelValues(vals...).Add(float64(addedBytes))
	}
	if backupPhaseFailed(phase) {
		backupFailuresTotal.WithLabelValues(vals...).Inc()
	}
}

// backupPhaseFailed reports whether a terminal Backup phase counts as a failure for
// crystalbackup_backup_failures_total. PartiallyFailed does: some data did not make it, and the
// namespace's restore point is incomplete.
func backupPhaseFailed(phase string) bool {
	return phase == phaseFailed || phase == phasePartiallyFailed
}

// RecordClusterBackupTerminal records ONE ClusterBackup run reaching a terminal phase.
func RecordClusterBackupTerminal(ctx context.Context, s ClusterBackupSeries, phase string, duration time.Duration) {
	vals := s.values()
	clusterBackupRunsTotal.WithLabelValues(append(append([]string{}, vals...), resultOf(phase))...).Inc()
	if duration < 0 {
		duration = 0
	}
	observe(ctx, clusterBackupDuration.WithLabelValues(vals...), duration.Seconds())
}

// RecordRestoreTerminal records ONE Restore/ClusterRestore reaching a terminal phase. mode is the
// restore's spec.mode; an empty mode (an older object written before the field was required)
// records as the empty label rather than being guessed into one of the two.
func RecordRestoreTerminal(ctx context.Context, s RestoreSeries, mode, phase string, duration time.Duration) {
	vals := append(append([]string{}, s.values()...), mode)
	if duration < 0 {
		duration = 0
	}
	observe(ctx, restoreDuration.WithLabelValues(vals...), duration.Seconds())
	if phase == phaseFailed || phase == phasePartiallyFailed {
		restoreFailuresTotal.WithLabelValues(vals...).Inc()
	}
}

// RecordExternalSyncDuration records ONE external-sync run reaching a terminal phase.
//
// Its counterpart crystalbackup_externalsync_bytes_copied_total is NOT shipped and is not merely
// missing: `restic copy --json` emits no machine-readable summary (verified against the pinned
// restic, adr/0013 amendment), so there is no byte count to read. A secondary's entire value rests
// on its numbers being believable, and an estimate presented as a measurement is worse than the
// absence — see spec/05-observability.md §2.12.
func RecordExternalSyncDuration(ctx context.Context, s ExternalSyncSeries, duration time.Duration) {
	if duration < 0 {
		duration = 0
	}
	observe(ctx, externalSyncDuration.WithLabelValues(s.values()...), duration.Seconds())
}

// RecordErasureCompleted records the physical deletion one completed ClusterErasure performed.
// Called once, on the transition into Completed, after that phase is durable.
func RecordErasureCompleted(location, cluster string, snapshotsForgotten int32, reclaimedBytes int64) {
	if snapshotsForgotten > 0 {
		erasureForgottenTotal.WithLabelValues(location, cluster).Add(float64(snapshotsForgotten))
	}
	if reclaimedBytes > 0 {
		erasureReclaimedTotal.WithLabelValues(location, cluster).Add(float64(reclaimedBytes))
	}
}

// RecordMoverJobRetries records the pod retries one mover Job consumed against its backoffLimit.
//
// It is called ONCE per mover, when the Job is observed terminal, with the Job's whole
// status.failed — not incrementally as the retries happen. Incremental recording would need a
// per-Job last-seen counter held in the operator's memory, which is a leak waiting for a Job that
// never reports again; reading the final tally off the terminal Job needs no state at all and
// cannot double-count, at the cost of the metric moving in one step at the end of a mover's life
// rather than during it.
func RecordMoverJobRetries(namespace, tenant, cluster string, retries int32) {
	if retries <= 0 {
		return
	}
	moverJobRetries.WithLabelValues(namespace, tenant, cluster).Add(float64(retries))
}

// RecordWebhookDenial counts one request denied by the operator's dynamic webhook. webhook and
// reason must both be compile-time constants (§1 cardinality).
func RecordWebhookDenial(webhook, reason string) {
	webhookDenials.WithLabelValues(webhook, reason).Inc()
}

// RecordExposureReadyWait records how long one snapshot exposure took to become mountable: the
// VolumeSnapshot going readyToUse, the static VSC/VS re-bind, and the temp PVC binding.
//
// This is the wait that pushes a backup window out without anything appearing to fail — a driver
// answering in four minutes instead of four seconds still completes every backup, just not inside
// the night. It gets its own bucket scale (exposureBuckets) for that reason: on the shared
// duration buckets every healthy exposure lands in the first bucket and the histogram says nothing.
func RecordExposureReadyWait(ctx context.Context, namespace, tenant, exposerKind, cluster string, wait time.Duration) {
	if wait < 0 {
		wait = 0
	}
	observe(ctx, exposureReadyWait.WithLabelValues(namespace, tenant, exposerKind, cluster), wait.Seconds())
}
