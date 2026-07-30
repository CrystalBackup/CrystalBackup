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

// Package alerts is the single declaration of Crystal Backup's Prometheus alert rules.
//
// # Why this is Go and not YAML
//
// Until M6 these nine rules lived as prose in spec/05-observability.md §3, and FIVE of them read
// series the operator has never emitted. Every one is valid PromQL. Every one evaluates without
// error. None of them could ever fire — and the only way to find that out was to run a fleet and
// notice the silence. Nothing in the build could catch it, because the rule text and the metric
// definitions never met.
//
// They meet here. Each expression below is CONCATENATED from the constants in
// internal/metrics/names.go, so renaming a series is a compile error in this file rather than an
// alert that quietly stops firing in production. The chart's PrometheusRule is GENERATED from this
// table (see render.go and `make alert-rules`); the YAML is an artifact, never a source. Copying
// the rules into a Helm template is precisely what produced the drift, and doing it again would
// reintroduce it within one release.
//
// The label names are not compile-checked the same way — they are strings — so rules_test.go
// checks every one of them against metrics.Catalogue(), which is built from the very []string the
// collectors hand to prometheus.NewDesc. A selector or a join on a label a family does not carry
// fails exactly like an unknown series name: valid PromQL, no matches, forever, in silence.
package alerts

import (
	"fmt"
	"strings"
	"time"

	"github.com/CrystalBackup/CrystalBackup/internal/metrics"
)

//go:generate go run ./cmd/genrules

// Severity is the routing class Alertmanager sees. Two values only: a page and a ticket. A third
// tier is where alerts go to be filtered out and never looked at again.
type Severity string

const (
	// SeverityWarning: something is degrading and a human should look today.
	SeverityWarning Severity = "warning"
	// SeverityCritical: restore capability is compromised right now.
	SeverityCritical Severity = "critical"
)

// GroupName is the PrometheusRule group every rule below belongs to.
const GroupName = "crystalbackup"

// ThresholdKind says what shape a rule's bound has, so a consumer that is not Prometheus can act
// on it without re-parsing PromQL.
type ThresholdKind string

const (
	// ThresholdState: no number at all. The rule tests a boolean state gauge.
	ThresholdState ThresholdKind = "state"
	// ThresholdAge: a fixed staleness bound on a *_timestamp_seconds series.
	ThresholdAge ThresholdKind = "age"
	// ThresholdCount: a gauge strictly above a number.
	ThresholdCount ThresholdKind = "count"
	// ThresholdPeriod: a staleness bound DERIVED per series from the schedule's own cron period,
	// with Age kept as the fallback for a schedule whose period cannot be computed.
	ThresholdPeriod ThresholdKind = "period"
)

// Threshold is a rule's firing bound, declared ONCE.
//
// It is a field rather than a number buried in the expression string because two consumers read
// it: the PromQL below, and the Go predicate lot J will evaluate for an operator who has no
// Prometheus. Two implementations of one threshold diverge — it is only a question of which
// release. A threshold declared once and read by both cannot.
type Threshold struct {
	Kind ThresholdKind
	// Age is the staleness bound (ThresholdAge, and the fallback for ThresholdPeriod).
	Age time.Duration
	// Count is the value a gauge must strictly exceed (ThresholdCount).
	Count float64
	// Factor and Grace turn a schedule's own period into its deadline (ThresholdPeriod):
	// Factor × period + Grace.
	Factor float64
	Grace  time.Duration
}

// Rule is one alert. Everything a PrometheusRule entry needs, plus the state predicate.
type Rule struct {
	// Name is the alert name, `Crystalbackup`-prefixed so it is unambiguous in an Alertmanager
	// shared with the rest of a platform.
	Name string
	// Severity routes it.
	Severity Severity
	// For is the duration the expression must hold before firing. Zero means fire on the first
	// evaluation, which is right only for rules whose expression is already an over-time
	// aggregation (increase(...)).
	For time.Duration
	// Summary is the `summary` annotation, Go-template'd by Prometheus at fire time.
	Summary string
	// Description is the `description` annotation: what to actually do about it. A rule with no
	// stated remedy gets silenced instead of fixed.
	Description string
	// Threshold is the bound the expression compares against — see Threshold.
	Threshold Threshold
	// Expr is the PromQL, assembled from internal/metrics constants.
	Expr string
	// Rationale is emitted as a comment above the rule in the generated YAML. An operator
	// deciding whether to silence an alert at 03:00 is reading the rule file, not the spec.
	Rationale string
	// Predicate answers the same question as Expr from live object state instead of from
	// Prometheus — the half lot J (exportable self-check) consumes, so an operator with no
	// monitoring stack can still get a verdict. Nil means "not yet implemented for this rule":
	// the two below are the worked shape, not a complete set. Any implementation MUST read its
	// bound from Threshold rather than restating a number.
	Predicate Predicate
}

// The label names these rules join and route on. Named constants rather than inline strings for
// the same reason the series names are: rules_test.go checks every one of them against the label
// sets the collectors register, and a name that appears in three places is a name that will
// eventually appear in three spellings.
const (
	labelNamespace = "namespace"
	labelSchedule  = "schedule"
	labelOrigin    = "origin"
	labelLocation  = "location"
	labelCluster   = "cluster"
	labelScope     = "scope"
)

// missedJoinKeys is the identity BackupMissed matches on, for EVERY term of the expression: the
// full label set of a (namespace, schedule) series MINUS `tenant`.
//
// Dropping tenant is not cosmetic. crystalbackup_backup_last_success_timestamp_seconds resolves
// its tenant from the BACKUP object's crystalbackup.io/tenant label, while
// crystalbackup_schedule_active resolves it from the NAMESPACE's — two sources that agree in every
// healthy cluster and are not guaranteed to. A join on the full label set would silently produce
// no match exactly when a tenant label was being changed, which is to say exactly when someone is
// touching the cluster.
//
// Keeping the other five for EVERY term, including the `schedule_active` guard, is the part worth
// spelling out, because spec §3 wrote that guard on three labels (namespace, schedule, cluster) and
// three is not enough. `schedule` is a CR name, and a `ClusterBackupSchedule` and a
// `BackupSchedule` may both be called `daily` — nothing forbids it, they are different scopes. On
// three labels, the guard asks only "does SOME active schedule agree on (namespace, schedule,
// cluster)", so an unrelated namesake answers for a plane that is paused: a real breach retained by
// the wrong partner, or a real breach silenced by one.
//
// Two failures follow from the narrow form, and only one of them is hypothetical:
//
//   - The namesake collision is LATENT today, and only by accident. The namespace plane emits
//     cluster="" and the cluster plane cluster=c1, so the two never meet on the third label. spec §1
//     documents filling that empty label in as 0.6 work — the day it lands, the accident stops
//     protecting us, and it stops silently.
//   - A schedule whose locationRef is EDITED breaks the narrow form right now. The Backups already
//     taken keep the old location on their series; schedule_active carries the new one. On three
//     labels the stale series keeps its guard partner and pages forever on a last_success that can
//     never advance again — which is the exact failure the guard was added to prevent. On five it
//     loses its partner and goes quiet, while schedule_created (emitted from the same loop as
//     schedule_active, so always at the NEW identity) keeps the live series alertable.
//
// `and` is a set operator, so the wider key costs nothing at evaluation: many-to-many is legal
// here, unlike the comparison above which needs group_left().
var missedJoinKeys = []string{labelNamespace, labelSchedule, labelOrigin, labelLocation, labelCluster}

// backupMissedThreshold: 1.1 × the schedule's own period, plus an hour.
//
// spec §3 shipped a flat 26 h ("24 h + 2 h grace"), which is only ever correct for the platform
// default of a daily schedule. At 26 h an hourly schedule is twenty-five hours dead before anyone
// hears; a weekly one pages every single Tuesday until someone silences it permanently, which is
// the worst outcome an alert can have. spec §8 question 3 lists deriving it from the cron period
// as the M6 refinement, and crystalbackup_schedule_period_seconds (added with this lot) is what
// makes it possible.
//
// The proportional factor absorbs jitter and a run that overruns; the flat hour keeps a
// five-minute schedule from having a five-and-a-half-minute deadline, where a single slow upload
// pages. Age stays as the fallback for the one case that has no period: a cron expression the
// operator cannot parse, which will never run and must still page.
var backupMissedThreshold = Threshold{
	Kind:   ThresholdPeriod,
	Factor: 1.1,
	Grace:  time.Hour,
	Age:    26 * time.Hour,
}

// The two remaining fixed staleness bounds. Declared as values, and their expressions built FROM
// them below, so 26 h is typed once per rule rather than once in the Threshold and once in the
// expression — the second copy being precisely the drift this field exists to prevent, at a
// smaller scale.
var (
	maintenanceStalledThreshold = Threshold{Kind: ThresholdAge, Age: 26 * time.Hour}
	externalSyncStaleThreshold  = Threshold{Kind: ThresholdAge, Age: 26 * time.Hour}
)

// Rules is the table. Order is the order rules appear in the generated file.
func Rules() []Rule {
	return []Rule{
		{
			Name:      "CrystalbackupBackupMissed",
			Severity:  SeverityWarning,
			For:       15 * time.Minute,
			Threshold: backupMissedThreshold,
			Summary: "No successful backup for {{ $labels.namespace }}/{{ $labels.schedule }} " +
				"({{ $labels.origin }}) within its schedule's deadline",
			Description: "The schedule is active and its deadline has passed with no successful Backup. " +
				"Check the namespace's Backup objects for a failed or stuck run, and the mover Jobs in the " +
				"operator namespace.",
			Rationale: strings.Join([]string{
				"The deadline is the schedule's OWN period (1.1x + 1h), not a fixed 26h: a flat",
				"threshold is silent for a day on an hourly schedule and pages every week on a weekly one.",
				"Three parts, and each closes a hole the previous shape had:",
				"  * the age term falls back to " + metrics.NameScheduleCreated + " when nothing has",
				"    ever succeeded — a schedule that was broken from the day it was applied emits no",
				"    last_success series at all, and `time() - <nothing>` is nothing;",
				"  * the second branch keeps the old fixed deadline for a schedule whose cron cannot be",
				"    parsed (no period series), because that schedule will never run and must still page;",
				"  * the `and` guard on " + metrics.NameScheduleActive + " is what stops a deleted or",
				"    paused schedule from paging forever on a last_success that will never advance again.",
			}, "\n"),
			Expr: backupMissedExpr(),
		},
		{
			Name:     "CrystalbackupBackupFailed",
			Severity: SeverityWarning,
			// No `for`: increase() over an hour IS the over-time aggregation, and adding a hold
			// would only delay a signal that is already an hour old by construction.
			Threshold: Threshold{Kind: ThresholdCount, Count: 0},
			Summary:   "Backup failed for {{ $labels.namespace }}/{{ $labels.schedule }}",
			Description: "At least one Backup reached Failed or PartiallyFailed in the last hour. " +
				"kubectl get backups -n {{ $labels.namespace }} shows which.",
			Rationale: strings.Join([]string{
				"Counter, so increase(): it survives the operator restart that resets it to zero (spec §1).",
				"The gauge sibling crystalbackup_backup_failures counts wreckage that still EXISTS — a",
				"different and also useful question, but not one increase() can ask.",
			}, "\n"),
			Expr: fmt.Sprintf("increase(%s[1h]) > 0", metrics.NameBackupFailuresTotal),
		},
		{
			Name:      "CrystalbackupRepositoryCheckFailed",
			Severity:  SeverityCritical,
			For:       5 * time.Minute,
			Threshold: Threshold{Kind: ThresholdState},
			Summary:   "restic check failed on repository {{ $labels.location }} ({{ $labels.scope }})",
			Description: "restic found repository damage (R17). Restores from this repository may not work. " +
				"Do not prune it until the check passes: inspect BackupRepository status.recentMaintenance.",
			Rationale: strings.Join([]string{
				"The only critical rule here, because it is the only one that says the RESTORE PATH is",
				"compromised rather than that a backup is late.",
				"A repository that has never been checked emits no series at all (spec §2.4), so this stays",
				"silent on a fresh location instead of paging the moment one is created.",
			}, "\n"),
			Expr:      fmt.Sprintf("%s == 0", metrics.NameRepositoryCheckSuccess),
			Predicate: repositoryCheckFailed,
		},
		{
			Name:      "CrystalbackupStaleLocks",
			Severity:  SeverityWarning,
			For:       30 * time.Minute,
			Threshold: Threshold{Kind: ThresholdCount, Count: 0},
			Summary:   "Stale restic locks persist on {{ $labels.location }} (reaper not clearing)",
			Description: "Stale locks block backups and maintenance on this repository. The orphan reaper " +
				"normally clears them within minutes; 30 minutes of persistence means it is not running or " +
				"cannot reach the backend.",
			Rationale: strings.Join([]string{
				"The 30m hold is the reaper's own budget: a lock is only stale after 30 minutes by restic's",
				"definition, and the reaper gets a full cycle to clear it before this says anything.",
			}, "\n"),
			Expr: fmt.Sprintf("%s > 0", metrics.NameRepositoryStaleLocks),
		},
		{
			Name:      "CrystalbackupMaintenanceStalled",
			Severity:  SeverityWarning,
			For:       time.Hour,
			Threshold: maintenanceStalledThreshold,
			Summary: "No successful prune on {{ $labels.location }} for over 26h — " +
				"the repository is growing unreclaimed",
			Description: "Forgotten snapshots are not being reclaimed, so the repository grows and the bill " +
				"with it. Check the maintenance Job history in BackupRepository status.recentMaintenance.",
			Rationale: strings.Join([]string{
				"A prune that keeps FAILING never advances lastMaintenanceTime, so staleness covers 'never",
				"ran', 'erroring every night' and 'controller wedged' in a single rule. 26h lets a daily",
				"pruneSchedule miss one window without paging.",
				"An Immutable location never prunes by design and emits no series at all (spec §2.4,",
				"adr/0005), so it cannot fire here — the absence is what makes that work, not a label",
				"exclusion this rule would have to remember to keep.",
			}, "\n"),
			Expr:      staleTimestampExpr(metrics.NameRepositoryLastPrune, maintenanceStalledThreshold.Age),
			Predicate: maintenanceStalled,
		},
		{
			Name:      "CrystalbackupDiscoveryFailed",
			Severity:  SeverityWarning,
			For:       30 * time.Minute,
			Threshold: Threshold{Kind: ThresholdState},
			Summary: "Discovery failing on {{ $labels.location }} — " +
				"Backup projections may be stale vs the repository",
			Description: "The repository is the source of truth (adr/0009). While discovery fails, " +
				"`kubectl get backups` no longer reflects what is actually restorable.",
			Rationale: strings.Join([]string{
				"Tenant-visible: at scope=namespace with a non-empty namespace this routes to the tenant,",
				"who is the one whose list of restore points has silently gone stale (spec §2.5).",
			}, "\n"),
			Expr: fmt.Sprintf("%s == 0", metrics.NameDiscoveryLastSuccess),
		},
		{
			Name:      "CrystalbackupErasureBlocked",
			Severity:  SeverityWarning,
			For:       time.Hour,
			Threshold: Threshold{Kind: ThresholdCount, Count: 0},
			Summary: "Right-to-erasure blocked on {{ $labels.location }} " +
				"(Immutable object-lock not yet expired, R21/ADR 0005)",
			Description: "A ClusterErasure cannot complete until the object-lock window expires. This is " +
				"expected on Immutable locations, but it is a GDPR clock running: someone has to know.",
			Rationale: strings.Join([]string{
				"Not an error: the erasure is waiting, correctly, on a lock that exists to make deletion",
				"impossible. It is an alert because a right-to-erasure request quietly parked for weeks is",
				"a compliance problem regardless of whose fault it is.",
			}, "\n"),
			Expr: fmt.Sprintf("%s > 0", metrics.NameErasureBlocked),
		},
		{
			Name:      "CrystalbackupPVCSnapshotPileup",
			Severity:  SeverityWarning,
			For:       30 * time.Minute,
			Threshold: Threshold{Kind: ThresholdCount, Count: 20},
			Summary: "{{ $value }} VolumeSnapshots piling up on PVC {{ $labels.namespace }}/{{ $labels.pvc }} " +
				"(ceph-csi flatten risk, ADR 0006)",
			Description: "Snapshots are accumulating faster than they are released — usually a coexisting " +
				"backup tool, or our own temp snapshots not being cleaned up. On ceph-csi this crosses the " +
				"flatten threshold and stalls the volume.",
			Rationale: strings.Join([]string{
				"The one family carrying a per-PVC label, and the documented exception to the",
				"no-per-PVC-label rule (spec §2.9): cardinality is bounded by the live PVC count, and this",
				"alert is the reason that cost is worth paying.",
			}, "\n"),
			Expr: fmt.Sprintf("%s > 20", metrics.NamePVCVolumeSnapshotting),
		},
		{
			Name:      "CrystalbackupExternalSyncStale",
			Severity:  SeverityWarning,
			For:       time.Hour,
			Threshold: externalSyncStaleThreshold,
			Summary: "External sync {{ $labels.sync }} " +
				"({{ $labels.source }}→{{ $labels.destination }}) has not completed in 26h",
			Description: "The secondary copy is falling behind. Check the sync CR's phase and the queue: a " +
				"secondary nobody has verified is not a secondary.",
			Rationale: strings.Join([]string{
				"A never-completed sync emits last_success as 0, NOT absent (spec §2.12, deliberately unlike",
				"the repository family), so a secondary that never worked from day one fires here instead of",
				"being the one case the rule misses.",
				"The 26h stays fixed, unlike BackupMissed's: a sync's schedule is optional, so a manual sync",
				"has no period to derive a deadline from. The pause guard this rule also wants waits on",
				"crystalbackup_externalsync_active, which does not exist yet — and a rule referencing a series",
				"nobody emits is the exact defect this table was built to make impossible.",
			}, "\n"),
			Expr: staleTimestampExpr(metrics.NameExternalSyncLastSuccess, externalSyncStaleThreshold.Age),
		},
	}
}

// staleTimestampExpr builds `time() - <series> > <seconds>`, the shape every "has not happened
// recently" rule shares. Written once so the seconds are always the Threshold's own duration and
// never a number typed twice.
func staleTimestampExpr(series string, age time.Duration) string {
	return fmt.Sprintf("time() - %s > %d", series, int64(age.Seconds()))
}

// backupMissedExpr assembles the one rule that is not a one-liner. See the Rationale on the rule
// itself for why each of the three parts is there.
func backupMissedExpr() string {
	on := "on (" + strings.Join(missedJoinKeys, ", ") + ")"

	// The age of the last success, falling back to the schedule's creation time when there has
	// never been one. `or on (...)` rather than a bare `or` for the same tenant-resolution reason
	// missedJoinKeys drops tenant: a bare `or` matches on the full label set, so a tenant
	// disagreement would let BOTH branches through and double-count the same schedule.
	age := fmt.Sprintf(
		"(\n      (time() - %s)\n      or %s\n      (time() - %s)\n    )",
		metrics.NameBackupLastSuccess, on, metrics.NameScheduleCreated)

	t := backupMissedThreshold

	// group_left() is not decoration: the left side can legitimately carry two series for one join
	// key while a tenant label is being changed, and without it Prometheus fails the WHOLE
	// evaluation with "many-to-one matching must be explicit" — an alert that errors instead of
	// firing is an alert that does not fire.
	derived := fmt.Sprintf(
		"  (\n    %s\n    > %s group_left()\n      (%s * %g + %d)\n  )",
		age, on, metrics.NameSchedulePeriod, t.Factor, int64(t.Grace.Seconds()))

	fallback := fmt.Sprintf(
		"  (\n    (%s > %d)\n    unless %s\n      %s\n  )",
		age, int64(t.Age.Seconds()), on, metrics.NameSchedulePeriod)

	// The guard joins on the SAME five labels as the threshold above, not on the three spec §3
	// used. See missedJoinKeys for why: on three, a `BackupSchedule` and a `ClusterBackupSchedule`
	// that happen to share a name answer for each other, and a series left behind by an edited
	// locationRef keeps a partner it should have lost.
	//
	// The parentheses around the or-branches are redundant to PromQL (comparison and `unless`
	// both bind tighter than `or`) and are kept because the reader of a fifteen-line expression
	// should not have to know that. The OUTER pair is load-bearing: `and` binds tighter than `or`,
	// so without it the schedule_active guard would apply to the fallback branch alone.
	return fmt.Sprintf("(\n%s\n  or\n%s\n)\nand %s\n  (%s == 1)",
		derived, fallback, on, metrics.NameScheduleActive)
}
