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

// Every alert name, as a constant, beside the table that declares them.
//
// They are constants because the names are read in three places — the table's Name field, the
// predicates' thresholdOf() lookups, and Fidelity()'s switch — and a rule name that appears in
// three places is a rule name that will eventually appear in three spellings. Against string
// literals a misspelling costs a rule that cannot resolve its bound and a fidelity caveat that
// silently returns "", neither of which a compiler would have caught; against constants both are
// build failures.
const (
	ruleBackupMissed              = "CrystalbackupBackupMissed"
	ruleBackupFailed              = "CrystalbackupBackupFailed"
	ruleBackupStalled             = "CrystalbackupBackupStalled"
	ruleRepositoryCheckFailed     = "CrystalbackupRepositoryCheckFailed"
	ruleStaleLocks                = "CrystalbackupStaleLocks"
	ruleMaintenanceStalled        = "CrystalbackupMaintenanceStalled"
	ruleDiscoveryFailed           = "CrystalbackupDiscoveryFailed"
	ruleErasureBlocked            = "CrystalbackupErasureBlocked"
	rulePVCSnapshotPileup         = "CrystalbackupPVCSnapshotPileup"
	ruleExternalSyncStale         = "CrystalbackupExternalSyncStale"
	ruleSchedulePausedTooLong     = "CrystalbackupSchedulePausedTooLong"
	ruleExternalSyncPausedTooLong = "CrystalbackupExternalSyncPausedTooLong"
)

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
	// Prometheus — the half the exportable self-check consumes, so an operator with no monitoring
	// stack still gets a verdict.
	//
	// EVERY rule carries one, and this field is the ONLY place a rule and its predicate are
	// associated. They were briefly associated twice — here and in an exported map — because the
	// two halves landed in parallel and this file was owned by the other one; that map needed a
	// test to hold it against this table, which is the tell that a second declaration site is a
	// drift risk rather than a convenience. There is one site now.
	//
	// A nil Predicate is reported by the self-check as "not evaluated", never as a pass. That is
	// the honest outcome for a question object state cannot answer — but no rule is in that
	// position today, and predicates_test.go fails if one appears without somebody deciding so.
	//
	// Any implementation MUST read its bound from Threshold rather than restating a number, and
	// MUST declare a Fidelity() caveat if it cannot reproduce its PromQL exactly.
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

	// The external-sync family's own identity (§2.12). `sync` is the CR name, `source` and
	// `destination` the two location names.
	labelSync        = "sync"
	labelSource      = "source"
	labelDestination = "destination"
)

// syncJoinKeys is the identity the ExternalSyncStale guard matches on: the FULL label set of the
// §2.12 family, with nothing dropped.
//
// That is the opposite choice from missedJoinKeys, and it is not an inconsistency. BackupMissed
// drops `tenant` because its two sides resolve it from different objects and can legitimately
// disagree mid-edit. Here both sides — last_success and active — are emitted from one loop over one
// map in collectExternalSyncs, keyed by exactly this struct, so there is no label that could
// disagree and no reason to weaken the key. Spelling all six out rather than writing a bare `and`
// says which identity the rule means, which is what makes a later divergence a visible edit rather
// than a silent behaviour change.
var syncJoinKeys = []string{
	labelSync, labelSource, labelDestination, labelScope, labelNamespace, labelCluster,
}

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

// schedulePausedTooLongThreshold: seven days of a schedule existing and protecting nothing.
//
// A week is chosen against the pause people actually take. "Pause it while I migrate the storage
// class", "pause it for the release freeze", "pause it over the weekend" all end well inside seven
// days, so the alert says nothing about any of them; what it catches is the pause that outlived the
// reason for it, which is the only kind nobody is going to notice on their own. There is no shorter
// threshold that does not page a maintenance window, and no longer one that beats a monthly
// backup-coverage review to the answer.
//
// It is an Age rather than a Count because the quantity really is a duration, and it is read TWICE
// in the expression — as the lookback window and as the minimum age of the schedule itself. See
// pausedTooLongExpr for why the second reading is not redundant.
var schedulePausedTooLongThreshold = Threshold{Kind: ThresholdAge, Age: 7 * 24 * time.Hour}

// backupStalledThreshold: eight hours of a Backup that has not finished.
//
// This is the loosest bound in the table and that is on purpose, because it is the only one that
// has to survive not knowing how big somebody's volume is. Read it against the two TIGHT bounds it
// backs up, which live in the controller and fail the volume outright:
//
//   - moverStartDeadline (30m): a mover pod that never reached Running. Provable — restic has read
//     nothing — so the operator can be quick and certain.
//   - snapshotReadyDeadline (2h): a VolumeSnapshot acknowledged and never ready.
//
// Both of those end in a Failed volume, which CrystalbackupBackupFailed already pages on. This rule
// is for the stalls neither can prove, and the list is not short: a Backup gated Pending forever on
// a BackupRepository that never initialises, a mover pod that IS Running with restic wedged on a
// dead endpoint, a volume held behind maxConcurrentMovers by a cascade that is itself stuck, a
// mover Job that vanished (advanceUploading cannot time that one out — there is no clock left to
// measure), and the case that is hardest to argue against: the deadline code above not running.
//
// EIGHT HOURS, and the number is taken from this project's own published model of its workload
// rather than invented. internal/metrics' durationBuckets — the buckets every duration family in
// the catalogue shares — top out at 28800 seconds, and the comment there says why: "a first full
// backup of a multi-terabyte volume over a throttled S3 link lands in the last". A run past eight
// hours is therefore off the top of the scale we ourselves designed for. That is the honest meaning
// of this alert, and it is what the operator should read it as: not "this backup is broken", but
// "this backup is longer than anything we modelled, come and look".
//
// It will occasionally fire on a genuine multi-terabyte first full, and that is the direction to
// err in. The alternative is a bound above every conceivable legitimate run, which is a bound above
// eight hours, and the incident this rule closes ran for THIRTY-SIX of them with every rule in this
// table silent. Eight hours also keeps it well inside BackupMissed's 26h deadline, so a stalled
// nightly is reported the same day rather than by the next night's absence.
var backupStalledThreshold = Threshold{Kind: ThresholdAge, Age: 8 * time.Hour}

// externalSyncPausedTooLongThreshold: the same week, for the same reason, on the other family.
//
// Declared separately rather than shared with the schedule's, following maintenanceStalled and
// externalSyncStale which both hold 26h and both say so themselves. The two bounds answer to
// different things — one to how long a namespace may go unprotected, the other to how long a
// secondary may go unfed — and collapsing them into one constant would mean a future change to
// either silently moved the other.
var externalSyncPausedTooLongThreshold = Threshold{Kind: ThresholdAge, Age: 7 * 24 * time.Hour}

// Rules is the table. Order is the order rules appear in the generated file.
func Rules() []Rule {
	return []Rule{
		{
			Name:      ruleBackupMissed,
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
			Expr:      backupMissedExpr(),
			Predicate: backupMissed,
		},
		{
			Name:     ruleBackupFailed,
			Severity: SeverityWarning,
			// No `for`: increase() over an hour IS the over-time aggregation, and adding a hold
			// would only delay a signal that is already an hour old by construction.
			Threshold: Threshold{Kind: ThresholdCount, Count: 0},
			Summary:   "Backup failed for {{ $labels.namespace }}/{{ $labels.schedule }}",
			Description: "At least one Backup reached Failed or PartiallyFailed in the last hour. " +
				"kubectl get backups -n {{ $labels.namespace }} shows which.",
			Rationale: strings.Join([]string{
				"Two disjuncts, because the counter alone was measured to be SILENT on a real failure.",
				"The first is the ordinary reading, and increase() over an hour is also why there is no",
				"`for` hold: the range IS the hold.",
				"It is also, alone, unable to page the FIRST failure a series ever records — no restart",
				"required. A CounterVec child materialises AT ONE (RecordBackupTerminal touches it only on",
				"the failure branch, so nothing pre-registers a zero), and a window whose samples all read 1",
				"holds no rise for increase() to measure. Only a SECOND failure moves it. That is the",
				"widest form of this defect and the likeliest incident in production: a namespace that was",
				"fine yesterday and failed once tonight.",
				"The second disjunct also exists because an operator restart does not RESET " + metrics.NameBackupFailuresTotal + " to",
				"zero — it makes the series DISAPPEAR. The same materialisation rule says a fresh process",
				"publishes no such series at all until something fails again, and increase() cannot see",
				"across a disappearance the way it sees across a reset. Measured on a",
				"live cluster: after the operator pod was replaced the counter returned ZERO series, the",
				"increase() returned 0, and a Backup had genuinely failed. Nothing fired.",
				metrics.NameBackupLastFailure + " is derived from the Backup objects at scrape time, so it is",
				"restart-safe by construction, and it is ABSENT for a series that has never failed — which is",
				"what keeps this rule silent on a healthy install rather than merely below a bound.",
				"The recency test is what stops it paging forever. The plain gauge crystalbackup_backup_failures",
				"survives a restart too, but it has no notion of WHEN: a Backup kept for diagnosis after failing",
				"three days ago would page every evaluation until somebody deleted it.",
				"RESIDUAL BLIND SPOT, accepted rather than hidden: if the operator restarts AND the failed",
				"Backup object is garbage-collected by its schedule's history limit inside the same hour, the",
				"counter series is gone and no object is left to derive a timestamp from. That failure is",
				"unrecoverable from either source and this rule will not fire for it. The remedy is a history",
				"limit greater than one, not a third disjunct.",
				"AND ITS MILDER FORM, measured on the crucible rather than reasoned about: deleting the",
				"Backup object RESOLVES this alert, even with the operator up, because on a first failure the",
				"counter contributes nothing and the timestamp is all there is. In the 0.6.0 run the lane's",
				"own teardown removed the namespace twelve seconds after the Backup went terminal, and the",
				"alert fired for exactly thirty seconds before resolving. That is correct — no wreckage, no",
				"page — but it means this rule's firing window is the OBJECT'S lifetime, not the hour its",
				"name implies. In production a retained failure outlives the window and the distinction never",
				"shows; with a history limit of one and a success right behind the failure, the page can be",
				"shorter than an operator's notification pipeline. Alertmanager's group_wait is the thing to",
				"check before blaming the rule.",
			}, "\n"),
			Expr:      backupFailedExpr(),
			Predicate: backupFailed,
		},
		{
			Name:     ruleBackupStalled,
			Severity: SeverityWarning,
			// 30m, not zero and not an hour. The series is a timestamp read at scrape time, so the
			// expression is already true continuously once the bound is crossed — the hold is there
			// to absorb a scrape gap or an operator restart re-listing objects, not to add patience
			// to a bound that is already eight hours long.
			For:       30 * time.Minute,
			Threshold: backupStalledThreshold,
			Summary: "Backup {{ $labels.namespace }}/{{ $labels.schedule }} has been running for " +
				"over 8h without finishing",
			Description: "A run that never ends is invisible to every other rule here: nothing failed, " +
				"so BackupFailed is silent, and last_success has not gone stale yet so BackupMissed is too. " +
				"kubectl get backup -n {{ $labels.namespace }} -o wide shows the per-volume phases; the " +
				"reason on a volume names the cause, and for one stuck in Uploading, kubectl describe pod " +
				"on its mover Job in the operator namespace has the kubelet's account.",
			Rationale: strings.Join([]string{
				"THE INCIDENT. On 0.6.2 a nightly cascade left six movers whose pods could not mount their",
				"temp clone PVC in ContainerCreating for thirty-six hours, and four more namespaces in",
				"Snapshotting beside them. Nothing failed, so none of the eleven rules in this table could",
				"fire — every one of them watches for a FAILURE. concurrencyPolicy: Forbid then meant no",
				"further nightly ran at all: one backup in fifteen days, with a green dashboard.",
				"A stall is not a failure and it is not a missed schedule. It needs its own series and its",
				"own rule, and " + metrics.NameBackupInProgressSince + " is that series.",
				"It is STATE-DERIVED, recomputed from the Backup objects on every scrape, and that is",
				"load-bearing rather than incidental. The lesson is written out at length under",
				"CrystalbackupBackupFailed above: a CounterVec child materialises AT ONE, so a counter",
				"cannot page on a first occurrence, and after an operator restart it does not reset — it",
				"DISAPPEARS. A stall is a first occurrence by definition, and the operator restarting is one",
				"of the things that can cause one, so a counter was never an option here.",
				"The series is ABSENT when nothing is in flight, which is what keeps this silent on a healthy",
				"cluster rather than merely below a bound — the same absence discipline last_failure follows,",
				"and for the same reason: a published 0 would make time()-0 fifty-four years and page the",
				"whole fleet forever.",
				"It reports the OLDEST unfinished Backup of the series, so tonight's fresh run cannot reset",
				"the clock on last night's wedged one.",
				"WHY THE BOUND IS SO LOOSE: see backupStalledThreshold. The tight, provable deadlines live in",
				"the controller (a mover pod that never reached Running at 30m; a snapshot acknowledged and",
				"never ready at 2h) and end in a Failed volume that BackupFailed pages on. This rule catches",
				"what those cannot prove, including a Backup gated Pending forever and a mover that is",
				"running and wedged, and it accepts firing on a genuinely enormous first full to do it.",
			}, "\n"),
			Expr:      staleTimestampExpr(metrics.NameBackupInProgressSince, backupStalledThreshold.Age),
			Predicate: backupStalled,
		},
		{
			Name:      ruleRepositoryCheckFailed,
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
			Name:      ruleStaleLocks,
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
			Expr:      fmt.Sprintf("%s > 0", metrics.NameRepositoryStaleLocks),
			Predicate: staleLocks,
		},
		{
			Name:      ruleMaintenanceStalled,
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
			Name:      ruleDiscoveryFailed,
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
			Expr:      fmt.Sprintf("%s == 0", metrics.NameDiscoveryLastSuccess),
			Predicate: discoveryFailed,
		},
		{
			Name:      ruleErasureBlocked,
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
			Expr:      fmt.Sprintf("%s > 0", metrics.NameErasureBlocked),
			Predicate: erasureBlocked,
		},
		{
			Name:      rulePVCSnapshotPileup,
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
			Expr:      fmt.Sprintf("%s > 20", metrics.NamePVCVolumeSnapshotting),
			Predicate: pvcSnapshotPileup,
		},
		{
			Name:      ruleExternalSyncStale,
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
				"has no period to derive a deadline from.",
				"The `or` branch onto " + metrics.NameExternalSyncCreated + " is not decoration. The 0 above",
				"is a SENTINEL, and on a real clock time()-0 is fifty-four years — so without this branch the",
				"rule was past its 26h bound on the first scrape after ANY sync was created, and only the 1h",
				"hold stood between `kubectl apply` and a page claiming a ninety-minute-old sync had not",
				"completed in 26h. Age is measured from the last success when there was one, and from the",
				"object's creation when there was not.",
				"The `and` guard on " + metrics.NameExternalSyncActive + " closes a defect that was latent",
				"rather than theoretical: spec.paused has existed on ClusterBackupExternalSync since M5 and",
				"nothing read it, so pausing a sync — the documented first step of decommissioning a location",
				"(docs/DECOMMISSION.md §1.1) — would have paged 26h later, about the thing the operator had",
				"just deliberately stopped.",
			}, "\n"),
			Expr:      externalSyncStaleExpr(),
			Predicate: externalSyncStale,
		},
		{
			Name:      ruleSchedulePausedTooLong,
			Severity:  SeverityWarning,
			For:       time.Hour,
			Threshold: schedulePausedTooLongThreshold,
			Summary: "Schedule {{ $labels.namespace }}/{{ $labels.schedule }} ({{ $labels.origin }}) " +
				"has been paused for over 7 days — nothing is backing this namespace up",
			Description: "A paused schedule protects nothing, and no other alert can say so: BackupMissed is " +
				"guarded on the same pause. Unpause it (spec.paused=false), or delete it if the namespace is " +
				"genuinely no longer being protected — an intent nobody has to re-check in a month.",
			Rationale: strings.Join([]string{
				"This rule exists because of the blind spot the M6 semantics change opened up and then closed.",
				"A paused schedule used to emit NO " + metrics.NameScheduleActive + " series at all, which",
				"suppressed BackupMissed correctly and made the pause itself unobservable: someone suspends a",
				"schedule 'for the migration', the migration ends, nobody unpauses, and the namespace is",
				"unprotected while every rule and every dashboard reports perfect health. For a backup tool,",
				"silence that is indistinguishable from safety is the one failure mode that must not exist.",
				"Two terms, and the second is doing three jobs at once:",
				"  * max_over_time == 0 over the window means the schedule was not active at ANY point in it —",
				"    robust to an operator restart in a way a 7d `for` would not be, since it reads history",
				"    rather than accumulating pending state;",
				"  * the age term excludes a schedule younger than the window, which would otherwise fire",
				"    simply because there is no `1` in a lookback longer than its life;",
				"  * and because " + metrics.NameScheduleCreated + " is an instant vector, that same term",
				"    requires the schedule to still EXIST. A deleted schedule leaves samples in the lookback",
				"    window for a week; without this it would keep alerting about an object that is gone.",
				"Deleting a schedule is a decision someone made. Leaving one paused is usually a decision",
				"nobody finished making, and that is the difference this alert is about.",
				"CAVEAT: reading history means the lookback is only as long as your retention. Under 7d of",
				"retention max_over_time sees a shorter window than it asks for, and a schedule that was",
				"active six days ago can look like it never was. Raise retention or raise this threshold;",
				"do not silence the rule.",
			}, "\n"),
			Expr: pausedTooLongExpr(metrics.NameScheduleActive, metrics.NameScheduleCreated,
				missedJoinKeys, schedulePausedTooLongThreshold.Age),
			Predicate: schedulePausedTooLong,
		},
		{
			Name:      ruleExternalSyncPausedTooLong,
			Severity:  SeverityWarning,
			For:       time.Hour,
			Threshold: externalSyncPausedTooLongThreshold,
			Summary: "External sync {{ $labels.sync }} " +
				"({{ $labels.source }}→{{ $labels.destination }}) has been paused for over 7 days — " +
				"the secondary is no longer being fed",
			Description: "The destination has stopped receiving copies and ExternalSyncStale is guarded " +
				"on the same pause, so nothing else will say so. Resume it (spec.paused=false), or delete " +
				"the sync if this secondary is genuinely being retired.",
			Rationale: strings.Join([]string{
				"SchedulePausedTooLong's twin, and it exists because the pause guard added to",
				"ExternalSyncStale in this same lot re-opened, for syncs, precisely the blind spot that",
				"rule closes for schedules: with the guard in place, pausing a sync is silent forever.",
				"The stakes are arguably higher here than on a schedule. A secondary is INSURANCE. Somebody",
				"pauses replication 'while we migrate the bucket', leaves, and nothing anywhere reports that",
				"the safety copy stopped being fed — the silence is indistinguishable from a destination",
				"that is perfectly up to date, and the difference only becomes visible on the day the",
				"primary is gone, which is the worst possible moment to find out.",
				"Same shape as its twin, same three jobs in the second term (see pausedTooLongExpr), and the",
				"same retention caveat: max_over_time cannot look further back than your Prometheus keeps.",
			}, "\n"),
			Expr: pausedTooLongExpr(metrics.NameExternalSyncActive, metrics.NameExternalSyncCreated,
				syncJoinKeys, externalSyncPausedTooLongThreshold.Age),
			Predicate: externalSyncPausedTooLong,
		},
	}
}

// backupFailedExpr: "a Backup failed in the last hour", asked twice, of two different kinds of
// series, because neither kind can answer it alone.
//
//	increase(<counter>[<w>]) > 0                <- the event, while the process that saw it lives
//	or (time() - <last_failure> < <w>)          <- the state, which outlives the process
//
// The two branches read ONE window. That is the whole reason this is a function rather than two
// numbers typed into a format string: `[1h]` and `3600` are the same quantity expressed in
// different units, and a pair like that drifts the first time somebody widens the window and edits
// only the half they were looking at. Both come from backupFailedWindow, which the state predicate
// also reads — so the alert, its self-check twin and the range selector move together or not at
// all. The window is rendered in SECONDS on both sides for the same reason staleTimestampExpr does
// it: it is the one form that survives being derived from a time.Duration without a bespoke
// formatter.
//
// A bare `or` is right here where the other rules need `or on (...)`: both sides carry the
// IDENTICAL label set (metrics.Catalogue() has last_failure on backupLabels, exactly like the
// counter), and both are derived from the same Backups, so there is no label that could disagree
// and nothing to drop from the key. The alert instance comes out the same whichever branch
// produced it — which is what lets an operator correlate a page across an operator restart instead
// of seeing it resolve and re-fire under a different identity.
func backupFailedExpr() string {
	seconds := int64(backupFailedWindow.Seconds())
	return fmt.Sprintf("increase(%s[%ds]) > 0\nor (time() - %s < %d)",
		metrics.NameBackupFailuresTotal, seconds, metrics.NameBackupLastFailure, seconds)
}

// staleTimestampExpr builds `time() - <series> > <seconds>`, the shape every "has not happened
// recently" rule shares. Written once so the seconds are always the Threshold's own duration and
// never a number typed twice.
func staleTimestampExpr(series string, age time.Duration) string {
	return fmt.Sprintf("time() - %s > %d", series, int64(age.Seconds()))
}

// externalSyncStaleExpr: age since the last copy, the never-copied fallback, and the pause guard.
//
// The fallback is the same shape as BackupMissed's and it fixes a sharper bug. §2.12 has
// last_success emit 0 rather than nothing for a sync that has never completed, so that a secondary
// broken from day one is not the one case the rule cannot see — good reasoning, and it makes 0 a
// SENTINEL rather than a timestamp. On a real clock `time() - 0` is about 1.77e9 seconds, so a
// pure staleness test is past a 26 h bound on the FIRST scrape after any sync is created, and only
// the 1 h hold separates `kubectl apply` from a page telling the operator their ninety-minute-old
// sync "has not completed in 26h".
//
// promtool's clock starts at the epoch, which is precisely why the unit tests looked fine: there,
// the sentinel and "created just now" are the same instant, and the bug is invisible.
//
//	(time() - (last_success > 0))            <- a real success, measured honestly
//	or on (...) (time() - created)           <- never succeeded: age since somebody asked for it
//
// `> 0` is what separates the two branches, and `or` only supplies the right side for series the
// left has dropped — so a sync that HAS copied is never measured from its creation. Both branches
// name the same six labels as the guard below; see syncJoinKeys.
func externalSyncStaleExpr() string {
	on := "on (" + strings.Join(syncJoinKeys, ", ") + ")"
	age := fmt.Sprintf(
		"(\n    (time() - (%s > 0))\n    or %s\n    (time() - %s)\n  )",
		metrics.NameExternalSyncLastSuccess, on, metrics.NameExternalSyncCreated)

	return fmt.Sprintf("(\n  %s > %d\n)\nand %s\n  (%s == 1)",
		age, int64(externalSyncStaleThreshold.Age.Seconds()),
		on, metrics.NameExternalSyncActive)
}

// pausedTooLongExpr asks "was this thing active at any point in the last <age>", and answers from
// HISTORY rather than from a multi-day `for`. Written once and used by both paused-too-long rules,
// because two copies of this shape would eventually stop being the same shape.
//
// The `for` form would have been shorter and is worse: Prometheus rebuilds pending-alert state
// after a restart only within its outage-tolerance window, so a week-long hold is reset by any
// meaningful outage — and these alerts' whole job is to notice something nobody is watching, which
// makes "quietly restarted the clock" the exact failure they cannot have.
//
// The window and the minimum age are ONE duration, read twice from the Threshold. They have to
// agree: a lookback longer than the age bound would fire on objects too young to have a `1` in the
// window, and a shorter one would let something paused since creation slip through the gap.
//
// The created term does a third job beyond declaring the bound and excluding the young: it is an
// INSTANT vector, so it is empty for a deleted object — while max_over_time keeps finding that
// object's old zeros in the lookback for a full week. Deletion is a decision somebody finished
// making; these rules are about the other kind.
func pausedTooLongExpr(activeSeries, createdSeries string, joinKeys []string, age time.Duration) string {
	seconds := int64(age.Seconds())
	return fmt.Sprintf(
		"(\n  max_over_time(%s[%ds]) == 0\n)\nand on (%s)\n  (time() - %s > %d)",
		activeSeries, seconds,
		strings.Join(joinKeys, ", "),
		createdSeries, seconds)
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
