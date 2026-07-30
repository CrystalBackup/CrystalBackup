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

package alerts

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/metrics"
	"github.com/CrystalBackup/CrystalBackup/internal/nsselector"
	"github.com/CrystalBackup/CrystalBackup/internal/schedule"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// Breach is one thing a rule would fire about, described the way the alert would describe it: the
// labels of the offending series and the value that crossed the bound. Same shape as an
// Alertmanager alert instance, deliberately — the point of the state predicates is that an
// operator with no Prometheus gets the SAME answer, not a differently-shaped one.
type Breach struct {
	Labels map[string]string
	Value  float64
	// Detail is the one thing a metric cannot carry: which object, by name.
	Detail string
}

// Predicate answers, from live cluster state, the same question a Rule's Expr asks of Prometheus.
//
// This is the half lot J (the exportable self-check) consumes: an operator who has not wired
// Crystal Backup into a monitoring stack — which is most of them on day one — still needs a
// verdict on whether their backups are healthy. Running the PromQL is not an option for them, and
// re-deriving the thresholds in a second place is how the two answers start disagreeing. So a
// predicate reads its bound from the SAME Rule.Threshold the expression was built from.
//
// `now` is a parameter rather than a call to time.Now so the age-based predicates are testable
// without a fake clock, exactly like internal/schedule.
type Predicate func(ctx context.Context, r client.Reader, now time.Time) ([]Breach, error)

// repositoryCheckFailed mirrors `crystalbackup_repository_last_check_success == 0`.
//
// Including the absence semantics, which are the subtle half: a repository that has never been
// checked emits no series (spec §2.4) and therefore does not fire. Reproducing that here means
// skipping repositories with a nil lastCheckTime rather than treating "no result" as a failure —
// get that wrong and the self-check reports every fresh installation as damaged.
func repositoryCheckFailed(ctx context.Context, r client.Reader, _ time.Time) ([]Breach, error) {
	repos := &cbv1.BackupRepositoryList{}
	if err := r.List(ctx, repos); err != nil {
		return nil, fmt.Errorf("list BackupRepositories: %w", err)
	}
	var out []Breach
	for i := range repos.Items {
		repo := &repos.Items[i]
		if repo.Status.LastCheckTime == nil || repo.Status.LastCheckResult == checkResultPassed {
			continue
		}
		out = append(out, Breach{
			Labels: repositoryLabels(repo),
			Value:  0,
			Detail: fmt.Sprintf("BackupRepository %s: last check %s at %s",
				repo.Name, repo.Status.LastCheckResult, repo.Status.LastCheckTime.Format(time.RFC3339)),
		})
	}
	return out, nil
}

// maintenanceStalled mirrors `time() - crystalbackup_repository_last_maintenance_timestamp_seconds
// > <Age>`, and is the worked example of why Threshold is a field: the bound below is read out of
// the rule table, so moving 26h moves both answers or neither.
//
// The nil-lastMaintenanceTime skip is again the absence semantics: an Immutable location never
// prunes by design (adr/0005) and emits no series, so it must not be reported here either.
func maintenanceStalled(ctx context.Context, r client.Reader, now time.Time) ([]Breach, error) {
	t, err := thresholdOf(ruleMaintenanceStalled)
	if err != nil {
		return nil, err
	}
	age := t.Age
	repos := &cbv1.BackupRepositoryList{}
	if err := r.List(ctx, repos); err != nil {
		return nil, fmt.Errorf("list BackupRepositories: %w", err)
	}
	var out []Breach
	for i := range repos.Items {
		repo := &repos.Items[i]
		last := repo.Status.LastMaintenanceTime
		if last == nil {
			continue
		}
		elapsed := now.Sub(last.Time)
		if elapsed <= age {
			continue
		}
		out = append(out, Breach{
			Labels: repositoryLabels(repo),
			Value:  elapsed.Seconds(),
			Detail: fmt.Sprintf("BackupRepository %s: last successful prune %s ago (bound %s)",
				repo.Name, elapsed.Truncate(time.Minute), age),
		})
	}
	return out, nil
}

// checkResultPassed is the one BackupRepository status value that means the repository verified
// clean; anything else with a lastCheckTime set is a failure.
const checkResultPassed = "Passed"

// repositoryLabels rebuilds the §2.4 label identity of a repository series. `cluster` is left
// empty: resolving it means looking the location up among the ClusterBackupLocations, which is the
// known M6 resolution gap spec §1 documents — filling it in here with a guess would put a WRONG
// value on a self-check report, which is worse than an empty one.
func repositoryLabels(repo *cbv1.BackupRepository) map[string]string {
	location := repo.Status.Location.Name
	if location == "" {
		location = repo.Name
	}
	return map[string]string{
		labelLocation:  location,
		labelScope:     metrics.ScopeLabelValue(repo.Status.Scope),
		labelNamespace: repo.Status.OwnerNamespace,
		labelCluster:   "",
	}
}

// ErrUnknownRule is returned when a predicate names a rule the table does not contain. Exported so
// a caller can tell this apart from a cluster it could not read: one is our bug, the other is the
// operator's environment, and a report that confuses them sends somebody to the wrong place.
var ErrUnknownRule = errors.New("no such rule in the alert table")

// thresholdOf finds a rule's declared bound by name.
//
// It used to panic, on the argument that the only callers are predicates in this package naming
// their own rules, so an unknown name is unreachable by construction. That argument is true today
// and guaranteed by nothing tomorrow — and the place it would come untrue is the worst possible
// one. These predicates run inside the exportable self-check, which is the tool somebody reaches
// for when something is ALREADY wrong. A diagnostic that dies while producing the diagnosis is the
// single worst failure mode this binary has, and its user is the least equipped of anyone to work
// out what just happened.
//
// So it returns an error, and the predicate returns it up: the rule lands in the report as
// `errored` with the reason naming it, which is a state the JSON already models. The one thing it
// must NOT do is degrade to a zero Threshold — a bound of 0 does not read as missing, it reads as
// a number, and a predicate comparing against it would report either everything or nothing as a
// breach. A plausible wrong answer is worse than a refusal to answer, and it is the exact failure
// this milestone exists to stamp out.
//
// The compile-time half of the old panic's job is done properly by
// TestEveryRuleResolvesItsOwnThreshold: the panic claimed to catch the fault at run time on a
// user's cluster, the test catches it at build time on ours.
func thresholdOf(name string) (Threshold, error) {
	for _, r := range Rules() {
		if r.Name == name {
			return r.Threshold, nil
		}
	}
	return Threshold{}, fmt.Errorf("%w: %q — this rule's state predicate cannot resolve the bound "+
		"it must compare against, so it has no honest verdict to give", ErrUnknownRule, name)
}

// The API values the predicates read directly. Spelled once each, because a phase compared against
// a misspelled literal matches nothing, forever, exactly like a metric name that drifted.
//
// The RULE names these predicates look their thresholds up by live in rules.go, beside the table
// that declares them — one declaration site for a name, same principle.
const (
	phaseCompleted = "Completed"
	reasonPaused   = "Paused"
	phaseBlocked   = "Blocked"
	conditionReady = "Ready"
)

// The two label names the predicates below report which rules.go does not already declare (its
// expressions never select on either). Same reasoning as the block there — a name that appears in
// three places appears in three spellings — and predicates_test.go checks every label every
// predicate emits against metrics.Catalogue(), so a predicate that reports a label its series does
// not carry fails the build rather than producing a self-check report that cannot be correlated
// with a firing alert.
const (
	labelTenant = "tenant"
	labelPVC    = "pvc"
)

// Fidelity states, for a rule whose predicate does NOT reproduce its PromQL exactly, what the
// difference is. An empty string means the predicate answers the same question from the same
// state the collector derives the series from, and the two cannot disagree.
//
// It exists because the alternative was to leave the inexact ones unimplemented. A self-check that
// says nothing about failed backups is not more honest than one that says "no failed backup among
// the Backup objects that still exist" — it is less useful and equally silent about the gap. What
// would be dishonest is printing a bare OK over a measurement with a blind spot, so the blind spot
// travels with the verdict, into the JSON and onto the rendered page.
func Fidelity(ruleName string) string {
	switch ruleName {
	case ruleBackupFailed:
		return "Derived from Backup objects that still exist. The alert's first disjunct reads a " +
			"COUNTER (increase over 1h), which survives the schedule history limit deleting a " +
			"failed run; this predicate cannot, and a failure already garbage-collected is not " +
			"counted here. Its second disjunct is derived from the same objects this predicate " +
			"reads, so on that half the two agree exactly — including the blind spot they share, " +
			"which is precisely the deleted run."
	case ruleSchedulePausedTooLong:
		return "The alert asks whether the schedule was active at any point in a 7-day lookback, " +
			"which is a question about metric HISTORY and has no equivalent in object state. This " +
			"predicate substitutes the Ready condition's transition into reason=Paused, falling " +
			"back to the schedule's creation. That reading can be older than the actual pause (a " +
			"schedule already not-Ready for another reason does not re-transition when paused), so " +
			"it reports a pause as longer than it was, never shorter."
	case ruleExternalSyncPausedTooLong:
		return "The alert asks whether the pair had an unpaused sync at any point in a 7-day " +
			"lookback, which is a question about metric HISTORY and has no equivalent in object " +
			"state. This predicate substitutes the Ready condition's transition into reason=Paused, " +
			"falling back to the sync's creation. That reading can be older than the actual pause " +
			"(a sync already not-Ready for another reason — a missing location, an unreachable " +
			"endpoint — does not re-transition when paused), so it reports a pause as longer than " +
			"it was, never shorter."
	default:
		return ""
	}
}

// backupMissed mirrors CrystalbackupBackupMissed — the fifteen-line expression, and the one rule
// here whose whole difficulty is the JOIN rather than the bound.
//
// The PromQL asks: for every (namespace, schedule, origin, location, cluster) that has an ACTIVE
// schedule, is the age of the last success (falling back to the schedule's creation) past
// Factor × period + Grace, or past Age when the cron cannot be parsed? This walks the same three
// inputs in the other direction — iterate the active series, look the last success up — which is
// the same answer without PromQL's many-to-many hazards, because there is no vector matching to
// get wrong.
//
// It reproduces collectSchedules' resolution of the two planes deliberately, including the
// ClusterBackupSchedule selector fan-out: a cluster schedule declares a SELECTION, and the series
// the alert joins against is one per matched namespace. Resolving it any other way would make the
// self-check answer a different question from the alert on precisely the schedules that cover the
// most namespaces.
func backupMissed(ctx context.Context, r client.Reader, now time.Time) ([]Breach, error) {
	t, err := thresholdOf(ruleBackupMissed)
	if err != nil {
		return nil, err
	}

	var namespaces corev1.NamespaceList
	if err := r.List(ctx, &namespaces); err != nil {
		return nil, fmt.Errorf("list Namespaces: %w", err)
	}
	var schedules cbv1.BackupScheduleList
	if err := r.List(ctx, &schedules); err != nil {
		return nil, fmt.Errorf("list BackupSchedules: %w", err)
	}
	var clusterSchedules cbv1.ClusterBackupScheduleList
	if err := r.List(ctx, &clusterSchedules); err != nil {
		return nil, fmt.Errorf("list ClusterBackupSchedules: %w", err)
	}
	var backups cbv1.BackupList
	if err := r.List(ctx, &backups); err != nil {
		return nil, fmt.Errorf("list Backups: %w", err)
	}
	clusterByLocation, _, err := clusterIdentities(ctx, r)
	if err != nil {
		return nil, err
	}
	tenants := tenantsByNamespace(namespaces.Items)

	active := map[seriesKey]scheduleTiming{}
	for i := range schedules.Items {
		s := &schedules.Items[i]
		// A paused schedule is skipped on BOTH planes, which is exactly the `and ... == 1` guard:
		// crystalbackup_schedule_active emits 0 for a paused schedule (it is not absent — see
		// metrics/schedule.go), and `== 1` drops a 0 the same way it drops a missing series. What
		// the 0 is for is the OTHER rule, SchedulePausedTooLong, which is why the two predicates
		// read spec.paused with opposite polarity rather than sharing a helper.
		if s.Spec.Paused {
			continue
		}
		loc := s.Spec.LocationRef.Name
		key := seriesKey{
			namespace: s.Namespace, tenant: tenants.of(s.Namespace), schedule: s.Name,
			origin: apiconst.OriginNamespace, location: loc, cluster: clusterByLocation[loc],
		}
		if _, dup := active[key]; !dup {
			active[key] = scheduleTiming{
				cron: s.Spec.Schedule, timezone: s.Spec.Timezone, created: s.CreationTimestamp.Time,
			}
		}
	}
	for i := range clusterSchedules.Items {
		cs := &clusterSchedules.Items[i]
		if cs.Spec.Paused {
			continue
		}
		matched, err := nsselector.Match(namespaces.Items, cs.Spec.Template.Spec.Namespaces)
		if err != nil {
			// The collector emits nothing for a selector its own controller will refuse. Same
			// reading here: no namespace is being backed up by this schedule, and inventing series
			// for the namespaces it MIGHT have matched would report missed backups against a
			// schedule that has never covered them.
			continue
		}
		loc := cs.Spec.Template.Spec.LocationRef.Name
		timing := scheduleTiming{
			cron: cs.Spec.Schedule, timezone: cs.Spec.Timezone, created: cs.CreationTimestamp.Time,
		}
		for _, ns := range matched {
			key := seriesKey{
				namespace: ns, tenant: tenants.of(ns), schedule: cs.Name,
				origin: apiconst.OriginCluster, location: loc, cluster: clusterByLocation[loc],
			}
			if _, dup := active[key]; !dup {
				active[key] = timing
			}
		}
	}

	// The last success per series, accumulated exactly as collectBackups does: latest terminal
	// success wins, and a run with no backupTime contributes nothing.
	lastSuccess := map[seriesKey]time.Time{}
	for i := range backups.Items {
		b := &backups.Items[i]
		switch b.Status.Phase {
		case string(status.BackupPhaseCompleted), string(status.BackupPhasePartiallyCompleted):
		default:
			continue
		}
		if b.Status.BackupTime == nil {
			continue
		}
		key := backupSeriesKeyOf(b, tenants, clusterByLocation)
		if prev, ok := lastSuccess[key]; ok && !b.Status.BackupTime.After(prev) {
			continue
		}
		lastSuccess[key] = b.Status.BackupTime.Time
	}

	var out []Breach
	for key, timing := range active {
		since, measured := lastSuccess[key], true
		if since.IsZero() {
			// The schedule_created fallback: nothing has ever succeeded for this series, so the
			// clock runs from the moment backups started being expected. Without it the single
			// most likely way for a new installation to be silently unprotected — a schedule
			// broken from the day it was applied — is the one case that never reports.
			since, measured = timing.created, false
		}
		elapsed := now.Sub(since)
		deadline, derived := t.Age, false
		if parsed, err := schedule.Parse(timing.cron, timing.timezone); err == nil {
			deadline = time.Duration(float64(parsed.Period(now))*t.Factor) + t.Grace
			derived = true
		}
		if elapsed <= deadline {
			continue
		}
		what := "no successful backup in"
		if !measured {
			what = "never succeeded; expected since"
		}
		how := fmt.Sprintf("fixed fallback %s (cron %q does not parse)", t.Age, timing.cron)
		if derived {
			how = fmt.Sprintf("deadline %s (%g x period + %s, cron %q)",
				deadline.Truncate(time.Second), t.Factor, t.Grace, timing.cron)
		}
		out = append(out, Breach{
			Labels: map[string]string{
				labelNamespace: key.namespace, labelTenant: key.tenant, labelSchedule: key.schedule,
				labelOrigin: key.origin, labelLocation: key.location, labelCluster: key.cluster,
			},
			Value: elapsed.Seconds(),
			Detail: fmt.Sprintf("schedule %s/%s (%s): %s %s — %s",
				key.namespace, key.schedule, key.origin, what, elapsed.Truncate(time.Minute), how),
		})
	}
	return sorted(out), nil
}

// backupFailed mirrors CrystalbackupBackupFailed as closely as object state allows — see Fidelity
// for the part it cannot reach.
//
// The window is the rule's own: one hour, read from backupFailedWindow, which is also what the
// expression's range selector and its recency term are built from. One duration, three readers.
//
// WHEN a Backup failed is resolved in three steps, and the order is the point:
//
//   - status.completionTime, the field added for exactly this question. It is stamped once, on
//     first arrival at a terminal phase, and never moved.
//   - the Ready condition's lastTransitionTime, for objects that reached a terminal phase before
//     that field existed. It is BIASED EARLY and knowingly so: a failing run goes
//     False(reason=InProgress) → False(reason=Failed), and meta.SetStatusCondition refreshes the
//     transition time only when the Status changes, not the Reason — so what it reports is when the
//     run STARTED. On a forty-minute backup that is forty minutes of skew.
//   - the object's creation, when there is no Ready condition at all (a phase written without one,
//     and discovery projections).
//
// Both fallbacks read EARLIER than the truth, which pushes a failure out of the window rather than
// into it — the wrong direction for a predicate whose known weakness is already under-reporting,
// but the only honest one available for an object that does not carry the answer. Skipping such
// objects would drop a real failure outright, which is worse.
func backupFailed(ctx context.Context, r client.Reader, now time.Time) ([]Breach, error) {
	var backups cbv1.BackupList
	if err := r.List(ctx, &backups); err != nil {
		return nil, fmt.Errorf("list Backups: %w", err)
	}
	var namespaces corev1.NamespaceList
	if err := r.List(ctx, &namespaces); err != nil {
		return nil, fmt.Errorf("list Namespaces: %w", err)
	}
	clusterByLocation, _, err := clusterIdentities(ctx, r)
	if err != nil {
		return nil, err
	}
	tenants := tenantsByNamespace(namespaces.Items)

	counts := map[seriesKey]float64{}
	for i := range backups.Items {
		b := &backups.Items[i]
		switch b.Status.Phase {
		case string(status.BackupPhaseFailed), string(status.BackupPhasePartiallyFailed):
		default:
			continue
		}
		at := failedAt(b)
		if now.Sub(at) > backupFailedWindow {
			continue
		}
		counts[backupSeriesKeyOf(b, tenants, clusterByLocation)]++
	}

	var out []Breach
	for key, n := range counts {
		out = append(out, Breach{
			Labels: map[string]string{
				labelNamespace: key.namespace, labelTenant: key.tenant, labelSchedule: key.schedule,
				labelOrigin: key.origin, labelLocation: key.location, labelCluster: key.cluster,
			},
			Value: n,
			Detail: fmt.Sprintf("%.0f Backup(s) in a failed terminal phase for %s/%s in the last %s",
				n, key.namespace, key.schedule, backupFailedWindow),
		})
	}
	return sorted(out), nil
}

// failedAt resolves when a failed Backup finished, in the order backupFailed's doc comment sets
// out: the stamped completion, then the Ready condition's transition, then the object's creation.
// It always returns something — a failure with no usable timestamp is still a failure, and the
// caller's job is to decide whether it lands in the window, not whether it happened.
func failedAt(b *cbv1.Backup) time.Time {
	if b.Status.CompletionTime != nil {
		return b.Status.CompletionTime.Time
	}
	if c := status.FindCondition(b.Status.Conditions, conditionReady); c != nil {
		return c.LastTransitionTime.Time
	}
	return b.CreationTimestamp.Time
}

// backupFailedWindow is the one hour of CrystalbackupBackupFailed. It is a constant rather than a
// Threshold field because the Threshold of that rule is the COUNT (`> 0`); the window belongs to
// the expression — where it now appears TWICE, as the counter's range selector and as the bound of
// the last_failure recency test. backupFailedExpr builds both from this constant so the two halves
// of one disjunction cannot end up describing two different hours, and predicates_test.go reads
// the range back out of the generated Expr and fails if it ever stops agreeing with this.
const backupFailedWindow = time.Hour

// staleLocks mirrors `crystalbackup_repository_stale_locks > 0`.
//
// Unlike the check and prune families there is no absence case: status.staleLocks is a plain count
// the maintenance controller writes on every probe, and the collector emits it unconditionally, so
// zero here is a measurement and reproducing it needs no nil handling.
func staleLocks(ctx context.Context, r client.Reader, _ time.Time) ([]Breach, error) {
	t, err := thresholdOf(ruleStaleLocks)
	if err != nil {
		return nil, err
	}
	bound := t.Count
	repos := &cbv1.BackupRepositoryList{}
	if err := r.List(ctx, repos); err != nil {
		return nil, fmt.Errorf("list BackupRepositories: %w", err)
	}
	var out []Breach
	for i := range repos.Items {
		repo := &repos.Items[i]
		if float64(repo.Status.StaleLocks) <= bound {
			continue
		}
		out = append(out, Breach{
			Labels: repositoryLabels(repo),
			Value:  float64(repo.Status.StaleLocks),
			Detail: fmt.Sprintf("BackupRepository %s: %d stale lock(s) the reaper has not cleared",
				repo.Name, repo.Status.StaleLocks),
		})
	}
	return sorted(out), nil
}

// discoveryFailed mirrors `crystalbackup_discovery_last_success == 0`, absence included.
//
// status.lastDiscoverySuccess is a POINTER precisely so "never scanned" is distinguishable from
// "scanned and failed", and collectDiscovery emits nothing for the nil case. A predicate that read
// nil as failure would report every location as broken between its creation and its first scan —
// the same class of mistake repositoryCheckFailed's nil skip avoids, and the reason both are
// spelled out rather than folded into one generic helper.
func discoveryFailed(ctx context.Context, r client.Reader, _ time.Time) ([]Breach, error) {
	repos := &cbv1.BackupRepositoryList{}
	if err := r.List(ctx, repos); err != nil {
		return nil, fmt.Errorf("list BackupRepositories: %w", err)
	}
	var out []Breach
	for i := range repos.Items {
		repo := &repos.Items[i]
		ok := repo.Status.LastDiscoverySuccess
		if ok == nil || *ok {
			continue
		}
		when := "never"
		if t := repo.Status.LastDiscoveryTime; t != nil {
			when = t.Format(time.RFC3339)
		}
		out = append(out, Breach{
			Labels: repositoryLabels(repo),
			Value:  0,
			Detail: fmt.Sprintf("BackupRepository %s: last discovery scan did not project cleanly "+
				"(last scan %s) — kubectl get backups no longer reflects the repository",
				repo.Name, when),
		})
	}
	return sorted(out), nil
}

// erasureBlocked mirrors `crystalbackup_erasure_blocked > 0`: ClusterErasures parked on an
// Immutable location's object lock (adr/0005). Not an error — a GDPR clock, which is why it is
// reported at all.
func erasureBlocked(ctx context.Context, r client.Reader, _ time.Time) ([]Breach, error) {
	t, err := thresholdOf(ruleErasureBlocked)
	if err != nil {
		return nil, err
	}
	bound := t.Count
	erasures := &cbv1.ClusterErasureList{}
	if err := r.List(ctx, erasures); err != nil {
		return nil, fmt.Errorf("list ClusterErasures: %w", err)
	}
	clusterByLocation, _, err := clusterIdentities(ctx, r)
	if err != nil {
		return nil, err
	}
	blocked := map[string]float64{}
	until := map[string]string{}
	for i := range erasures.Items {
		er := &erasures.Items[i]
		if er.Status.Phase != phaseBlocked {
			continue
		}
		loc := er.Spec.LocationRef.Name
		blocked[loc]++
		if er.Status.BlockedUntil != "" {
			until[loc] = er.Status.BlockedUntil
		}
	}
	var out []Breach
	for loc, n := range blocked {
		if n <= bound {
			continue
		}
		detail := fmt.Sprintf("%.0f ClusterErasure(s) blocked on location %s", n, loc)
		if u := until[loc]; u != "" {
			detail += " until " + u
		}
		out = append(out, Breach{
			Labels: map[string]string{labelLocation: loc, labelCluster: clusterByLocation[loc]},
			Value:  n,
			Detail: detail,
		})
	}
	return sorted(out), nil
}

// volumeSnapshotListGVK is the snapshot API's list kind, asked for by GVK because the type is not
// in the operator's scheme (internal/metrics and internal/exposer do the same).
var volumeSnapshotListGVK = schema.GroupVersionKind{
	Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotList",
}

// pvcSnapshotPileup mirrors `crystalbackup_pvc_volumesnapshot_count > 20`.
//
// It counts EVERYONE's snapshots, not the operator's, for the reason metrics/snapshots.go spells
// out: during coexistence the incumbent tool's snapshots are the ones stacking up, and a count
// restricted to our own reads reassuringly low on exactly the cluster where the volume is about to
// be flattened. Static snapshots (no spec.source.persistentVolumeClaimName) are skipped — they add
// nothing to the source volume's chain, and one of them is our own re-bind alias.
//
// A cluster with no snapshot CRDs installed reports no breach rather than an error. That is not a
// silent OK: with no VolumeSnapshot kind there are no VolumeSnapshots, so zero per PVC is a true
// measurement, and it is exactly what the absent metric family means for the alert.
func pvcSnapshotPileup(ctx context.Context, r client.Reader, _ time.Time) ([]Breach, error) {
	t, err := thresholdOf(rulePVCSnapshotPileup)
	if err != nil {
		return nil, err
	}
	bound := t.Count
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(volumeSnapshotListGVK)
	if err := r.List(ctx, list); err != nil {
		if apimeta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list VolumeSnapshots: %w", err)
	}
	_, clusterID, err := clusterIdentities(ctx, r)
	if err != nil {
		return nil, err
	}
	type pvcKey struct{ namespace, pvc string }
	counts := map[pvcKey]float64{}
	for i := range list.Items {
		vs := &list.Items[i]
		pvc, found, err := unstructured.NestedString(vs.Object, "spec", "source", "persistentVolumeClaimName")
		if err != nil || !found || pvc == "" {
			continue
		}
		counts[pvcKey{namespace: vs.GetNamespace(), pvc: pvc}]++
	}
	var out []Breach
	for key, n := range counts {
		if n <= bound {
			continue
		}
		out = append(out, Breach{
			Labels: map[string]string{
				labelNamespace: key.namespace, labelPVC: key.pvc, labelCluster: clusterID,
			},
			Value: n,
			Detail: fmt.Sprintf("%.0f VolumeSnapshots on PVC %s/%s (bound %g) — ceph-csi flatten risk",
				n, key.namespace, key.pvc, bound),
		})
	}
	return sorted(out), nil
}

// externalSyncStale mirrors `time() - crystalbackup_externalsync_last_success_timestamp_seconds >
// <Age> and on (...) crystalbackup_externalsync_active == 1`.
//
// Two semantics have to be reproduced and they pull in opposite directions. §2.12's deliberate
// exception to the never-measured-is-absent rule says a sync that has NEVER completed publishes 0
// rather than nothing, so a secondary that never worked from day one is the first thing this
// reports instead of the one case it misses. The pause guard says a sync every one of whose CRs is
// paused is not maintained on purpose and must be silent. `active` is therefore an OR across the
// CRs folded into a series, not a last-one-wins: two syncs can address the same pair, and one live
// sync means the pair is still being maintained.
func externalSyncStale(ctx context.Context, r client.Reader, now time.Time) ([]Breach, error) {
	t, err := thresholdOf(ruleExternalSyncStale)
	if err != nil {
		return nil, err
	}
	age := t.Age
	var clusterSyncs cbv1.ClusterBackupExternalSyncList
	if err := r.List(ctx, &clusterSyncs); err != nil {
		return nil, fmt.Errorf("list ClusterBackupExternalSyncs: %w", err)
	}
	var syncs cbv1.BackupExternalSyncList
	if err := r.List(ctx, &syncs); err != nil {
		return nil, fmt.Errorf("list BackupExternalSyncs: %w", err)
	}
	clusterByLocation, _, err := clusterIdentities(ctx, r)
	if err != nil {
		return nil, err
	}

	type syncState struct {
		at     time.Time
		active bool
	}
	seen := map[syncSeriesKey]*syncState{}
	consider := func(key syncSeriesKey, paused bool, phase string, t *metav1.Time) {
		s := seen[key]
		if s == nil {
			s = &syncState{}
			seen[key] = s
		}
		s.active = s.active || !paused
		var at time.Time
		if phase == phaseCompleted && t != nil {
			at = t.Time
		}
		if at.After(s.at) {
			s.at = at
		}
	}
	for i := range clusterSyncs.Items {
		cs := &clusterSyncs.Items[i]
		consider(syncSeriesKey{
			sync: cs.Name, source: cs.Spec.SourceLocationRef.Name,
			destination: cs.Spec.DestinationLocationRef.Name, scope: apiconst.OriginCluster,
			cluster: clusterByLocation[cs.Spec.SourceLocationRef.Name],
		}, cs.Spec.Paused, cs.Status.Phase, cs.Status.LastSuccessTime)
	}
	for i := range syncs.Items {
		bs := &syncs.Items[i]
		consider(syncSeriesKey{
			sync: bs.Name, source: bs.Spec.SourceLocationRef.Name,
			destination: bs.Spec.DestinationLocationRef.Name, scope: apiconst.OriginNamespace,
			namespace: bs.Namespace,
		}, bs.Spec.Paused, bs.Status.Phase, bs.Status.LastSuccessTime)
	}

	var out []Breach
	for key, s := range seen {
		if !s.active {
			continue
		}
		at := s.at
		// The zero timestamp is the Unix epoch, which is how the metric renders "never completed"
		// and what makes `time() - 0` cross any staleness bound. Reproduced rather than
		// special-cased, so the value in the report is the value the alert would have carried.
		elapsed := now.Sub(at)
		if at.IsZero() {
			elapsed = now.Sub(time.Unix(0, 0))
		}
		if elapsed <= age {
			continue
		}
		what := fmt.Sprintf("last completed %s ago", elapsed.Truncate(time.Minute))
		if at.IsZero() {
			what = "has NEVER completed"
		}
		out = append(out, Breach{
			Labels: map[string]string{
				labelSync: key.sync, labelSource: key.source, labelDestination: key.destination,
				labelScope: key.scope, labelNamespace: key.namespace, labelCluster: key.cluster,
			},
			Value: elapsed.Seconds(),
			Detail: fmt.Sprintf("external sync %s (%s -> %s): %s (bound %s)",
				key.sync, key.source, key.destination, what, age),
		})
	}
	return sorted(out), nil
}

// schedulePausedTooLong mirrors CrystalbackupSchedulePausedTooLong: a schedule that exists, is not
// protecting anything, and has been that way long enough that nobody is going to notice on their
// own.
//
// It is the one predicate here whose rule asks a question about metric HISTORY —
// `max_over_time(schedule_active[7d]) == 0` — which object state simply does not hold. See
// Fidelity for what is substituted and in which direction it errs. The second term of the
// expression IS reproducible exactly: the schedule must itself be older than the window, which is
// also what makes the alert ignore a schedule created paused yesterday.
func schedulePausedTooLong(ctx context.Context, r client.Reader, now time.Time) ([]Breach, error) {
	t, err := thresholdOf(ruleSchedulePausedTooLong)
	if err != nil {
		return nil, err
	}
	age := t.Age

	var namespaces corev1.NamespaceList
	if err := r.List(ctx, &namespaces); err != nil {
		return nil, fmt.Errorf("list Namespaces: %w", err)
	}
	var schedules cbv1.BackupScheduleList
	if err := r.List(ctx, &schedules); err != nil {
		return nil, fmt.Errorf("list BackupSchedules: %w", err)
	}
	var clusterSchedules cbv1.ClusterBackupScheduleList
	if err := r.List(ctx, &clusterSchedules); err != nil {
		return nil, fmt.Errorf("list ClusterBackupSchedules: %w", err)
	}
	clusterByLocation, _, err := clusterIdentities(ctx, r)
	if err != nil {
		return nil, err
	}
	tenants := tenantsByNamespace(namespaces.Items)

	var out []Breach
	report := func(key seriesKey, created, since time.Time) {
		// Both terms, and both must hold. The age bound on `created` is the expression's second
		// term verbatim; `since` stands in for the lookback.
		if now.Sub(created) <= age || now.Sub(since) <= age {
			return
		}
		out = append(out, Breach{
			Labels: map[string]string{
				labelNamespace: key.namespace, labelTenant: key.tenant, labelSchedule: key.schedule,
				labelOrigin: key.origin, labelLocation: key.location, labelCluster: key.cluster,
			},
			Value: now.Sub(since).Seconds(),
			Detail: fmt.Sprintf("schedule %s/%s (%s): paused for %s (bound %s) — nothing is backing "+
				"this namespace up", key.namespace, key.schedule, key.origin,
				now.Sub(since).Truncate(time.Hour), age),
		})
	}

	for i := range schedules.Items {
		s := &schedules.Items[i]
		if !s.Spec.Paused {
			continue
		}
		loc := s.Spec.LocationRef.Name
		report(seriesKey{
			namespace: s.Namespace, tenant: tenants.of(s.Namespace), schedule: s.Name,
			origin: apiconst.OriginNamespace, location: loc, cluster: clusterByLocation[loc],
		}, s.CreationTimestamp.Time, pausedSince(s.Status.Conditions, s.CreationTimestamp.Time))
	}
	for i := range clusterSchedules.Items {
		cs := &clusterSchedules.Items[i]
		if !cs.Spec.Paused {
			continue
		}
		// The selection is resolved even though the schedule is paused, because the namespaces it
		// WOULD have covered are precisely the ones that stopped being protected — the same reason
		// collectSchedules resolves it and emits one 0 per matched namespace.
		matched, err := nsselector.Match(namespaces.Items, cs.Spec.Template.Spec.Namespaces)
		if err != nil {
			continue
		}
		loc := cs.Spec.Template.Spec.LocationRef.Name
		since := pausedSince(cs.Status.Conditions, cs.CreationTimestamp.Time)
		for _, ns := range matched {
			report(seriesKey{
				namespace: ns, tenant: tenants.of(ns), schedule: cs.Name,
				origin: apiconst.OriginCluster, location: loc, cluster: clusterByLocation[loc],
			}, cs.CreationTimestamp.Time, since)
		}
	}
	return sorted(out), nil
}

// externalSyncPausedTooLong mirrors CrystalbackupExternalSyncPausedTooLong: a pair every one of
// whose syncs is paused, and has been for long enough that nobody is going to notice on their own.
//
// schedulePausedTooLong's twin, and it reads the SAME two terms with the same substitution — see
// Fidelity for the direction it errs in. The one thing it does not share is the fold: `active` is an
// OR across the CRs on a pair (one unpaused sync means the pair is still maintained, exactly as in
// externalSyncStale), and both timestamps take the EARLIEST, so a pair that has been unfed for a
// month does not have its clock reset by a second sync being added onto it today.
func externalSyncPausedTooLong(ctx context.Context, r client.Reader, now time.Time) ([]Breach, error) {
	t, err := thresholdOf(ruleExternalSyncPausedTooLong)
	if err != nil {
		return nil, err
	}
	age := t.Age

	var clusterSyncs cbv1.ClusterBackupExternalSyncList
	if err := r.List(ctx, &clusterSyncs); err != nil {
		return nil, fmt.Errorf("list ClusterBackupExternalSyncs: %w", err)
	}
	var syncs cbv1.BackupExternalSyncList
	if err := r.List(ctx, &syncs); err != nil {
		return nil, fmt.Errorf("list BackupExternalSyncs: %w", err)
	}
	clusterByLocation, _, err := clusterIdentities(ctx, r)
	if err != nil {
		return nil, err
	}

	type pausedState struct {
		active  bool
		created time.Time
		since   time.Time
	}
	seen := map[syncSeriesKey]*pausedState{}
	consider := func(key syncSeriesKey, paused bool, created, since time.Time) {
		s := seen[key]
		if s == nil {
			s = &pausedState{created: created, since: since}
			seen[key] = s
		}
		s.active = s.active || !paused
		if created.Before(s.created) {
			s.created = created
		}
		if since.Before(s.since) {
			s.since = since
		}
	}
	for i := range clusterSyncs.Items {
		cs := &clusterSyncs.Items[i]
		created := cs.CreationTimestamp.Time
		consider(syncSeriesKey{
			sync: cs.Name, source: cs.Spec.SourceLocationRef.Name,
			destination: cs.Spec.DestinationLocationRef.Name, scope: apiconst.OriginCluster,
			cluster: clusterByLocation[cs.Spec.SourceLocationRef.Name],
		}, cs.Spec.Paused, created, pausedSince(cs.Status.Conditions, created))
	}
	for i := range syncs.Items {
		bs := &syncs.Items[i]
		created := bs.CreationTimestamp.Time
		consider(syncSeriesKey{
			sync: bs.Name, source: bs.Spec.SourceLocationRef.Name,
			destination: bs.Spec.DestinationLocationRef.Name, scope: apiconst.OriginNamespace,
			namespace: bs.Namespace,
		}, bs.Spec.Paused, created, pausedSince(bs.Status.Conditions, created))
	}

	var out []Breach
	for key, s := range seen {
		// Both terms, and both must hold: the age bound on `created` is the expression's second term
		// verbatim, and `since` stands in for the 7-day lookback.
		if s.active || now.Sub(s.created) <= age || now.Sub(s.since) <= age {
			continue
		}
		out = append(out, Breach{
			Labels: map[string]string{
				labelSync: key.sync, labelSource: key.source, labelDestination: key.destination,
				labelScope: key.scope, labelNamespace: key.namespace, labelCluster: key.cluster,
			},
			Value: now.Sub(s.since).Seconds(),
			Detail: fmt.Sprintf("external sync %s (%s -> %s): paused for %s (bound %s) — the "+
				"secondary is no longer being fed", key.sync, key.source, key.destination,
				now.Sub(s.since).Truncate(time.Hour), age),
		})
	}
	return sorted(out), nil
}

// pausedSince reads when an object went not-Ready with reason Paused — both schedule controllers
// and both external-sync controllers write exactly that on their pause branch. It falls back to the
// object's creation, which is the right answer for one created paused and a conservative one
// otherwise.
func pausedSince(conds []metav1.Condition, created time.Time) time.Time {
	c := status.FindCondition(conds, conditionReady)
	if c == nil || c.Reason != reasonPaused || c.LastTransitionTime.IsZero() {
		return created
	}
	return c.LastTransitionTime.Time
}

// seriesKey is the six-label identity of a Backup-family series (metrics' backupLabels). It is a
// local shape rather than metrics.BackupSeries because the predicates only ever build maps with
// it; sharing the exported type would couple this package to a field order it never depends on.
type seriesKey struct {
	namespace, tenant, schedule, origin, location, cluster string
}

// syncSeriesKey is the same for the §2.12 external-sync family, shared by the two predicates that
// fold CRs into series (externalSyncStale and externalSyncPausedTooLong). One declaration, so the
// two cannot disagree about what counts as the same sync — which would make one of them silent on
// exactly the pairs the other reports.
type syncSeriesKey struct {
	sync, source, destination, scope, namespace, cluster string
}

// scheduleTiming is the part of a schedule the deadline is derived from, carried so both planes
// reach one code path — the same split metrics/schedule.go makes, for the same reason.
type scheduleTiming struct {
	cron     string
	timezone string
	created  time.Time
}

// backupSeriesKeyOf derives a Backup's series identity the way metrics.backupKey does: namespace
// and origin/schedule from labels, tenant from the NAMESPACE's label falling back to the namespace
// name, location from the spec, cluster from that location's clusterID.
//
// The tenant resolution is the collector's namespace-based one and not the Backup's own
// crystalbackup.io/tenant label, on purpose: rules.go drops `tenant` from every join key precisely
// because the two sources can disagree while a label is being changed, so a predicate that
// preferred the object's copy would group two runs of one schedule into two series mid-edit.
func backupSeriesKeyOf(b *cbv1.Backup, tenants namespaceTenants, clusterByLocation map[string]string) seriesKey {
	loc := b.Spec.LocationRef.Name
	return seriesKey{
		namespace: b.Namespace,
		tenant:    tenants.of(b.Namespace),
		schedule:  b.Labels[apiconst.LabelSchedule],
		origin:    b.Labels[apiconst.LabelOrigin],
		location:  loc,
		cluster:   clusterByLocation[loc],
	}
}

// namespaceTenants resolves a namespace to its tenant exactly as metrics.tenantsByNamespace does:
// the crystalbackup.io/tenant label if set, else the namespace name (one tenant per namespace, R19).
type namespaceTenants map[string]string

func tenantsByNamespace(namespaces []corev1.Namespace) namespaceTenants {
	out := make(namespaceTenants, len(namespaces))
	for i := range namespaces {
		ns := &namespaces[i]
		if t := ns.Labels[apiconst.LabelTenant]; t != "" {
			out[ns.Name] = t
		}
	}
	return out
}

func (t namespaceTenants) of(namespace string) string {
	if tenant, ok := t[namespace]; ok {
		return tenant
	}
	return namespace
}

// clusterIdentities reproduces metrics.Collector.locationClusterIDs: the per-location clusterID
// map, and THE cluster's identity for the platform-scope families that carry `cluster` and have no
// location to resolve it from.
//
// The consensus rule is copied deliberately, disagreement included: the default location's
// clusterID wins, else the single value every location agrees on, else EMPTY. An arbitrary pick
// would put a cluster name on a self-check report that no Prometheus series in the same cluster
// carries, which is worse than an empty label because it looks like an answer.
func clusterIdentities(ctx context.Context, r client.Reader) (map[string]string, string, error) {
	out := map[string]string{}
	var locs cbv1.ClusterBackupLocationList
	if err := r.List(ctx, &locs); err != nil {
		return nil, "", fmt.Errorf("list ClusterBackupLocations: %w", err)
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
		return out, defaultID, nil
	}
	if agreed {
		return out, consensus, nil
	}
	return out, "", nil
}

// sorted orders breaches by their Detail so a report generated twice from unchanged state is
// byte-identical. Map iteration is the only source of disorder above, and two self-check reports
// attached to the same issue that differ only in line order waste the reader's attention on a
// diff that means nothing.
func sorted(b []Breach) []Breach {
	slices.SortFunc(b, func(x, y Breach) int { return strings.Compare(x.Detail, y.Detail) })
	return b
}
