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
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/metrics"
)

// seriesOfRule maps each rule to the crystalbackup_ series whose LABEL SET its alert instances
// carry, so the label check below has something to check against.
//
// It is written out here rather than parsed from Expr on purpose: a regex over PromQL that picked
// the wrong series would silently weaken the very assertion this table exists to make. Every value
// is a metrics constant, so a renamed series is a compile error here exactly as it is in rules.go.
//
// BackupFailed reads TWO series (a counter and its state-derived companion) and this table names
// only one of them. That is not a gap: the entry is about the LABEL SET an alert instance carries,
// the two series carry the same one by construction, and
// TestBackupFailedNamesBothOfItsSeries is what holds them to it.
var seriesOfRule = map[string]string{
	ruleBackupMissed:              metrics.NameScheduleActive,
	ruleBackupFailed:              metrics.NameBackupFailuresTotal,
	ruleBackupStalled:             metrics.NameBackupInProgressSince,
	ruleRepositoryCheckFailed:     metrics.NameRepositoryCheckSuccess,
	ruleStaleLocks:                metrics.NameRepositoryStaleLocks,
	ruleMaintenanceStalled:        metrics.NameRepositoryLastPrune,
	ruleDiscoveryFailed:           metrics.NameDiscoveryLastSuccess,
	ruleErasureBlocked:            metrics.NameErasureBlocked,
	rulePVCSnapshotPileup:         metrics.NamePVCVolumeSnapshotting,
	ruleExternalSyncStale:         metrics.NameExternalSyncLastSuccess,
	ruleSchedulePausedTooLong:     metrics.NameScheduleActive,
	ruleExternalSyncPausedTooLong: metrics.NameExternalSyncActive,
}

// TestEveryRuleCarriesAPredicate is what survives of the map-consistency check, and it is the half
// that was testing something real.
//
// The other half — that a separate exported map and the Rule.Predicate field agreed — has no object
// now that the map is gone: with one declaration site there is nothing to drift. This is the part
// that still bites. A rule with no predicate is reported by the self-check as "not evaluated",
// which is honest but is ALSO indistinguishable, to a hurried reader, from a rule that passed. It
// used to be a t.Logf, and a log nobody reads is how a rule ships unanswered. Every rule has a
// predicate today, so the honest bar is that adding one without a predicate has to be a deliberate
// act with a reason written here — not a silent omission.
func TestEveryRuleCarriesAPredicate(t *testing.T) {
	for _, r := range Rules() {
		if r.Predicate == nil {
			t.Errorf("rule %s has no state predicate: the self-check will report it as "+
				"'not evaluated', which reads as a gap in the report for every operator with no "+
				"Prometheus.\n    If this rule genuinely cannot be answered from object state, say "+
				"so here and in its Fidelity() entry rather than leaving the field nil in silence.",
				r.Name)
		}
	}
}

// TestEveryRuleResolvesItsOwnThreshold is what actually replaces the panic thresholdOf used to
// carry.
//
// That panic claimed to catch a predicate naming a rule the table does not have. It caught it at
// RUN time, inside the exportable self-check — the binary somebody runs when something is already
// wrong, where a crash is both most expensive and least explicable. The fault is a programming
// error, so it belongs at build time, here: the lookup now returns an error the report can render,
// and this test is what makes "no predicate can fail to resolve its bound" a checked property
// rather than an assertion in a comment.
//
// It runs every predicate against an EMPTY cluster, which is enough because every predicate that
// has a bound resolves it before it reads anything. Three predicates have no bound to resolve
// (RepositoryCheckFailed and DiscoveryFailed are state tests, BackupFailed's window is a range in
// the expression rather than a Threshold) and pass this vacuously — which is correct, not a gap:
// what is asserted is that no predicate fails on a name, and a predicate that looks up no name
// cannot.
func TestEveryRuleResolvesItsOwnThreshold(t *testing.T) {
	empty := newFakeClient(t)
	now := time.Now()

	for _, r := range Rules() {
		// The direct half: the table can look its own rule up.
		if _, err := thresholdOf(r.Name); err != nil {
			t.Errorf("rule %s cannot resolve its own threshold: %v", r.Name, err)
		}
		if r.Predicate == nil {
			continue
		}
		// The half that matters: the predicate resolves whatever name IT names, which is a
		// different string from r.Name only when somebody has made the mistake this catches.
		if _, err := r.Predicate(context.Background(), empty, now); errors.Is(err, ErrUnknownRule) {
			t.Errorf("rule %s's predicate looks up a rule the table does not contain: %v\n"+
				"    This used to be a panic inside the self-check, on somebody else's broken "+
				"cluster. It is a build failure now, which is where it belongs.", r.Name, err)
		}
	}
}

// TestThresholdOfRefusesRatherThanInventsABound is the test of the test above: a checker nobody has
// seen fail is a checker nobody should trust, and the failure mode being guarded against is
// specific.
//
// The tempting alternative to the panic was a zero Threshold and no error. That is worse than
// either a panic or an error, and quietly so: 0 does not read as "missing", it reads as a number. A
// count predicate comparing `> 0` would report every object in the cluster as a breach, and an age
// predicate comparing against 0 would report everything as overdue. Both are plausible-looking
// answers, produced by a tool whose entire value is that its answers can be believed.
func TestThresholdOfRefusesRatherThanInventsABound(t *testing.T) {
	got, err := thresholdOf("CrystalbackupNoSuchRule")
	if err == nil {
		t.Fatalf("thresholdOf returned %+v and no error for a rule that does not exist", got)
	}
	if !errors.Is(err, ErrUnknownRule) {
		t.Errorf("error does not wrap ErrUnknownRule, so a caller cannot tell our bug from a "+
			"cluster it could not read: %v", err)
	}
	if !strings.Contains(err.Error(), "CrystalbackupNoSuchRule") {
		t.Errorf("the error does not name the rule, so the report cannot say which one failed: %v", err)
	}
	if got != (Threshold{}) {
		t.Errorf("a failed lookup returned a non-zero Threshold %+v; the contract is that the "+
			"caller gets NOTHING usable, precisely so it cannot proceed with an invented bound", got)
	}
}

// allPredicates is every rule's predicate, keyed by rule name — derived from the table rather than
// declared, which is the whole point of the field. The tests below iterate it so that a rule added
// to the table is automatically held to the label, fidelity and determinism properties.
func allPredicates() map[string]Predicate {
	out := map[string]Predicate{}
	for _, r := range Rules() {
		if r.Predicate != nil {
			out[r.Name] = r.Predicate
		}
	}
	return out
}

// TestEveryPredicateReportsItsSeriesLabelSet is the assertion that makes a self-check report
// correlatable with a firing alert. A breach labelled differently from the alert instance it
// mirrors cannot be matched to it by anyone reading both.
func TestEveryPredicateReportsItsSeriesLabelSet(t *testing.T) {
	catalogue := metrics.Catalogue()
	now := time.Now()
	c := breachingCluster(t, now)

	for name, p := range allPredicates() {
		series, ok := seriesOfRule[name]
		if !ok {
			t.Errorf("rule %s has no entry in seriesOfRule; add one so its predicate's labels are checked", name)
			continue
		}
		want, ok := catalogue[series]
		if !ok {
			t.Errorf("%s: series %s is not in the metrics catalogue", name, series)
			continue
		}
		breaches, err := p(context.Background(), c, now)
		if err != nil {
			t.Errorf("%s: predicate errored: %v", name, err)
			continue
		}
		if len(breaches) == 0 {
			t.Errorf("%s: the breaching fixture produced no breach, so its labels are unchecked; "+
				"either the fixture or the predicate is wrong", name)
			continue
		}
		for _, b := range breaches {
			got := slices.Sorted(maps(b.Labels))
			expect := slices.Clone(want)
			slices.Sort(expect)
			if !slices.Equal(got, expect) {
				t.Errorf("%s: breach labels %v, but %s carries %v", name, got, series, expect)
			}
			if b.Detail == "" {
				t.Errorf("%s: breach has no Detail; a metric label set alone does not name an object", name)
			}
		}
	}
}

// backupFailedExprText is CrystalbackupBackupFailed's assembled PromQL, for the three tests below
// that each check a different property of it.
func backupFailedExprText(t *testing.T) string {
	t.Helper()
	for _, r := range Rules() {
		if r.Name == ruleBackupFailed {
			return r.Expr
		}
	}
	t.Fatal("no CrystalbackupBackupFailed rule")
	return ""
}

// TestBackupFailedWindowMatchesTheExpression keeps the one duration this package types outside a
// Threshold honest against the expression it came from. The range is not a Threshold field (the
// rule's Threshold is the count), so nothing else would notice the two diverging.
//
// It insists on finding EXACTLY ONE range selector. The expression grew a second disjunct when the
// restart hole was closed, and a FindStringSubmatch that silently takes the first of several
// matches is how a test keeps passing while checking the wrong half of a rule — the same shape of
// silence this whole package exists to remove. If a future edit legitimately adds a second range,
// this fails loudly and whoever adds it has to say which one the window is.
func TestBackupFailedWindowMatchesTheExpression(t *testing.T) {
	expr := backupFailedExprText(t)
	m := regexp.MustCompile(`\[(\d+[smhdw])\]`).FindAllStringSubmatch(expr, -1)
	if len(m) != 1 {
		t.Fatalf("expected exactly one range selector in %q, found %d", expr, len(m))
	}
	want, err := time.ParseDuration(m[0][1])
	if err != nil {
		t.Fatalf("range %q: %v", m[0][1], err)
	}
	if want != backupFailedWindow {
		t.Errorf("backupFailedWindow = %s but the expression ranges over %s", backupFailedWindow, want)
	}
}

// TestBackupFailedRecencyBoundMatchesTheWindow is the other half of the same coupling, and the
// reason backupFailedExpr is a function.
//
// The rule reads one window in two units: `[3600s]` as the counter's range and a bare `3600` as
// the last_failure recency bound. Written by hand they would eventually describe two different
// hours — a counter looking back further than the state series, or the reverse — and the resulting
// rule would fire on one branch and not the other with nothing to say why.
func TestBackupFailedRecencyBoundMatchesTheWindow(t *testing.T) {
	expr := backupFailedExprText(t)
	pattern := regexp.MustCompile(`time\(\) - ` + regexp.QuoteMeta(metrics.NameBackupLastFailure) + ` < (\d+)\)`)
	m := pattern.FindStringSubmatch(expr)
	if m == nil {
		t.Fatalf("no last_failure recency term in %q", expr)
	}
	want, err := time.ParseDuration(m[1] + "s")
	if err != nil {
		t.Fatalf("bound %q: %v", m[1], err)
	}
	if want != backupFailedWindow {
		t.Errorf("the recency term bounds at %s but backupFailedWindow is %s", want, backupFailedWindow)
	}
}

// TestBackupFailedNamesBothOfItsSeries pins the shape of the fix, not just its arithmetic.
//
// The rule is a disjunction over an EVENT counter and a STATE gauge, and it is a disjunction
// because neither survives what the other does: the counter's series disappears with the process
// that owned it, and the gauge cannot see a failure whose Backup object has been collected. A
// future edit that dropped either branch would leave a rule that still parses, still fires in the
// common case, and is silent again on exactly the restart this lot was written for. Both names
// must also be in the catalogue — a disjunct on a series nobody emits is the M6 defect verbatim.
func TestBackupFailedNamesBothOfItsSeries(t *testing.T) {
	expr := backupFailedExprText(t)
	catalogue := metrics.Catalogue()
	for _, name := range []string{metrics.NameBackupFailuresTotal, metrics.NameBackupLastFailure} {
		if !strings.Contains(expr, name) {
			t.Errorf("%s does not appear in the expression %q", name, expr)
		}
		if _, ok := catalogue[name]; !ok {
			t.Errorf("%s is not in the metrics catalogue, so nothing emits it", name)
		}
	}
	// Identical label sets, because the alert `or`s across the two: an instance produced by the
	// counter branch and one produced by the gauge branch have to be the SAME alert, or an
	// operator watching a page across an operator restart sees it resolve and re-fire under a
	// different identity.
	counter := slices.Clone(catalogue[metrics.NameBackupFailuresTotal])
	state := slices.Clone(catalogue[metrics.NameBackupLastFailure])
	slices.Sort(counter)
	slices.Sort(state)
	if !slices.Equal(counter, state) {
		t.Errorf("%s carries %v but %s carries %v; the two disjuncts would produce two different alerts",
			metrics.NameBackupFailuresTotal, counter, metrics.NameBackupLastFailure, state)
	}
}

// TestFidelityIsDeclaredForEveryInexactPredicate names, explicitly, the predicates that do NOT
// reproduce their PromQL exactly. It is a list, not a check on the strings: what it protects is
// that the list cannot grow by accident. Adding a predicate with a known gap and forgetting to
// declare it is how "OK" starts meaning "unmeasured".
func TestFidelityIsDeclaredForEveryInexactPredicate(t *testing.T) {
	inexact := []string{ruleBackupFailed, ruleSchedulePausedTooLong, ruleExternalSyncPausedTooLong}
	for name := range allPredicates() {
		got := Fidelity(name)
		want := slices.Contains(inexact, name)
		if want && got == "" {
			t.Errorf("%s is a known-inexact predicate but Fidelity() says nothing about it", name)
		}
		if !want && got != "" {
			t.Errorf("%s declares a fidelity caveat (%q) but is not in the inexact list; either it "+
				"became exact or the list is stale", name, got)
		}
	}
}

// TestBackupMissedDerivesTheDeadlineFromTheCron is the point of ThresholdPeriod: an hourly schedule
// and a weekly one do not share a deadline, and the flat 26h that spec §3 shipped is right for
// neither.
func TestBackupMissedDerivesTheDeadlineFromTheCron(t *testing.T) {
	now := time.Now()
	// Two hours since the last success. Well past an hourly schedule's 1.1x1h+1h = 2h6m? No — just
	// inside it. Take three hours instead so the hourly one is unambiguously breached while the
	// daily one (deadline ~1d1h) is nowhere near.
	lastSuccess := metav1.NewTime(now.Add(-3 * time.Hour))
	c := newFakeClient(t,
		ns("team-a"),
		bs("team-a", "hourly", func(s *cbv1.BackupSchedule) { s.Spec.Schedule = "0 * * * *" }),
		bs("team-a", "daily", func(s *cbv1.BackupSchedule) { s.Spec.Schedule = "0 2 * * *" }),
		backup("team-a", "hourly-run", "hourly", lastSuccess),
		backup("team-a", "daily-run", "daily", lastSuccess),
	)

	breaches, err := backupMissed(context.Background(), c, now)
	if err != nil {
		t.Fatalf("predicate: %v", err)
	}
	if len(breaches) != 1 {
		t.Fatalf("want exactly the hourly schedule breached; a flat 26h bound would have caught "+
			"neither. got %+v", breaches)
	}
	if breaches[0].Labels["schedule"] != "hourly" {
		t.Errorf("breached schedule = %q, want hourly", breaches[0].Labels["schedule"])
	}
}

// TestBackupMissedFallsBackToCreationWhenNothingEverSucceeded covers the case the rule could not
// see before schedule_created existed, and the one most likely to matter on a fresh install: a
// schedule that was broken from the day it was applied emits no last_success series at all.
func TestBackupMissedFallsBackToCreationWhenNothingEverSucceeded(t *testing.T) {
	now := time.Now()
	c := newFakeClient(t,
		ns("team-a"),
		bs("team-a", "never-worked", func(s *cbv1.BackupSchedule) {
			s.Spec.Schedule = "0 2 * * *"
			s.CreationTimestamp = metav1.NewTime(now.Add(-30 * 24 * time.Hour))
		}),
		bs("team-a", "just-created", func(s *cbv1.BackupSchedule) {
			s.Spec.Schedule = "0 2 * * *"
			s.CreationTimestamp = metav1.NewTime(now.Add(-time.Minute))
		}),
	)
	breaches, err := backupMissed(context.Background(), c, now)
	if err != nil {
		t.Fatalf("predicate: %v", err)
	}
	if len(breaches) != 1 || breaches[0].Labels["schedule"] != "never-worked" {
		t.Fatalf("want the month-old schedule that never succeeded, and NOT the one applied a "+
			"minute ago. got %+v", breaches)
	}
	if !strings.Contains(breaches[0].Detail, "never succeeded") {
		t.Errorf("Detail should say the schedule never succeeded, not report a false age: %q", breaches[0].Detail)
	}
}

// TestBackupMissedSkipsPausedSchedules is the `and ... schedule_active == 1` guard. A paused
// schedule emits 0, `== 1` drops it, and this must drop it too — the alternative is paging someone
// about the thing they deliberately turned off.
func TestBackupMissedSkipsPausedSchedules(t *testing.T) {
	now := time.Now()
	old := metav1.NewTime(now.Add(-90 * 24 * time.Hour))
	c := newFakeClient(t,
		ns("team-a"),
		bs("team-a", "paused", func(s *cbv1.BackupSchedule) {
			s.Spec.Schedule = "0 2 * * *"
			s.Spec.Paused = true
			s.CreationTimestamp = old
		}),
		cbs("paused-cluster", func(s *cbv1.ClusterBackupSchedule) {
			s.Spec.Schedule = "0 2 * * *"
			s.Spec.Paused = true
			s.CreationTimestamp = old
		}),
	)
	breaches, err := backupMissed(context.Background(), c, now)
	if err != nil {
		t.Fatalf("predicate: %v", err)
	}
	if len(breaches) != 0 {
		t.Fatalf("a paused schedule on either plane must not be reported as missing backups: %+v", breaches)
	}
}

// TestBackupMissedResolvesTheClusterScheduleSelection is the hard half of schedule_active: a
// ClusterBackupSchedule declares a SELECTION, and the series the alert joins against is one per
// matched namespace. A predicate that emitted one series per schedule would join against nothing.
func TestBackupMissedResolvesTheClusterScheduleSelection(t *testing.T) {
	now := time.Now()
	c := newFakeClient(t,
		ns("team-a"), ns("team-b"), ns("unprotected"),
		cbs("dr-daily", func(s *cbv1.ClusterBackupSchedule) {
			s.Spec.Schedule = "0 2 * * *"
			s.CreationTimestamp = metav1.NewTime(now.Add(-30 * 24 * time.Hour))
			s.Spec.Template.Spec.Namespaces.MatchNames = []string{"team-a", "team-b"}
		}),
	)
	breaches, err := backupMissed(context.Background(), c, now)
	if err != nil {
		t.Fatalf("predicate: %v", err)
	}
	if len(breaches) != 2 {
		t.Fatalf("want one breach per MATCHED namespace (team-a, team-b) and none for the "+
			"unselected one. got %d: %+v", len(breaches), breaches)
	}
	for _, b := range breaches {
		if b.Labels["origin"] != apiconst.OriginCluster {
			t.Errorf("origin = %q, want %q", b.Labels["origin"], apiconst.OriginCluster)
		}
	}
}

// TestExternalSyncStaleReportsTheNeverCompletedOne is §2.12's deliberate exception: last_success is
// 0 rather than absent, so a secondary that never worked from day one is the FIRST thing this
// reports instead of the one case it misses.
func TestExternalSyncStaleReportsTheNeverCompletedOne(t *testing.T) {
	now := time.Now()
	fresh := metav1.NewTime(now.Add(-time.Hour))
	c := newFakeClient(t,
		clusterSync("never", func(s *cbv1.ClusterBackupExternalSync) {}),
		clusterSync("current", func(s *cbv1.ClusterBackupExternalSync) {
			s.Status.Phase = "Completed"
			s.Status.LastSuccessTime = &fresh
		}),
	)
	breaches, err := externalSyncStale(context.Background(), c, now)
	if err != nil {
		t.Fatalf("predicate: %v", err)
	}
	if len(breaches) != 1 || breaches[0].Labels["sync"] != "never" {
		t.Fatalf("want the never-completed sync only: %+v", breaches)
	}
	if !strings.Contains(breaches[0].Detail, "NEVER") {
		t.Errorf("Detail should say it never completed rather than print an epoch age: %q", breaches[0].Detail)
	}
}

// TestExternalSyncStaleHonoursThePauseGuard covers the guard rules.go added on
// crystalbackup_externalsync_active: pausing a sync is the documented first step of
// decommissioning a location, and being paged 26h later about the thing you just stopped is the
// defect the guard closes.
func TestExternalSyncStaleHonoursThePauseGuard(t *testing.T) {
	now := time.Now()
	c := newFakeClient(t, clusterSync("paused", func(s *cbv1.ClusterBackupExternalSync) { s.Spec.Paused = true }))
	breaches, err := externalSyncStale(context.Background(), c, now)
	if err != nil {
		t.Fatalf("predicate: %v", err)
	}
	if len(breaches) != 0 {
		t.Fatalf("a paused sync must be silent: %+v", breaches)
	}
}

// TestDiscoveryFailedMirrorsAbsenceSemantics: lastDiscoverySuccess is a POINTER so that "never
// scanned" is distinguishable from "scanned and failed". Reading nil as failure would report every
// location as broken between its creation and its first scan.
func TestDiscoveryFailedMirrorsAbsenceSemantics(t *testing.T) {
	no, yes := false, true
	c := newFakeClient(t,
		repo("never-scanned", func(*cbv1.BackupRepository) {}),
		repo("clean", func(r *cbv1.BackupRepository) { r.Status.LastDiscoverySuccess = &yes }),
		repo("stale", func(r *cbv1.BackupRepository) { r.Status.LastDiscoverySuccess = &no }),
	)
	breaches, err := discoveryFailed(context.Background(), c, time.Now())
	if err != nil {
		t.Fatalf("predicate: %v", err)
	}
	if len(breaches) != 1 || breaches[0].Labels["location"] != "stale" {
		t.Fatalf("want only the repository whose last scan failed: %+v", breaches)
	}
}

// TestStaleLocksReadsTheRuleBound and its siblings below check that the count predicates take
// their number from the table rather than restating it. The pileup bound (20) is the one where a
// second copy would be easiest to miss.
func TestStaleLocksReadsTheRuleBound(t *testing.T) {
	c := newFakeClient(t,
		repo("clean", func(r *cbv1.BackupRepository) { r.Status.StaleLocks = 0 }),
		repo("locked", func(r *cbv1.BackupRepository) { r.Status.StaleLocks = 3 }),
	)
	breaches, err := staleLocks(context.Background(), c, time.Now())
	if err != nil {
		t.Fatalf("predicate: %v", err)
	}
	if len(breaches) != 1 || breaches[0].Value != 3 {
		t.Fatalf("want the repository with 3 stale locks: %+v", breaches)
	}
}

func TestPVCSnapshotPileupUsesTheDeclaredBound(t *testing.T) {
	bound := int(mustThreshold(t, rulePVCSnapshotPileup).Count)
	c := snapshotClient(t,
		volumeSnapshots("team-a", "busy", bound+1)...,
	)
	breaches, err := pvcSnapshotPileup(context.Background(), c, time.Now())
	if err != nil {
		t.Fatalf("predicate: %v", err)
	}
	if len(breaches) != 1 || breaches[0].Labels["pvc"] != "busy" {
		t.Fatalf("want the PVC one snapshot over the bound: %+v", breaches)
	}

	atBound := snapshotClient(t, volumeSnapshots("team-a", "busy", bound)...)
	breaches, err = pvcSnapshotPileup(context.Background(), atBound, time.Now())
	if err != nil {
		t.Fatalf("predicate: %v", err)
	}
	if len(breaches) != 0 {
		t.Fatalf("the bound is exclusive (`> %d`), so exactly %d must not breach: %+v", bound, bound, breaches)
	}
}

// TestPVCSnapshotPileupIsSilentWithoutTheSnapshotCRDs: the metric family is simply absent on a
// cluster with no snapshot API, and the alert cannot fire. Reporting an error there would make
// every self-check on a cluster without CSI snapshots look broken.
func TestPVCSnapshotPileupIsSilentWithoutTheSnapshotCRDs(t *testing.T) {
	breaches, err := pvcSnapshotPileup(context.Background(), newFakeClient(t), time.Now())
	if err != nil {
		t.Fatalf("a cluster with no VolumeSnapshot kind must not error: %v", err)
	}
	if len(breaches) != 0 {
		t.Fatalf("no snapshot kind means no snapshots: %+v", breaches)
	}
}

// TestSchedulePausedTooLongNeedsBothTerms: the schedule must be older than the window AND paused
// for longer than it. A schedule created paused yesterday satisfies neither, and the alert's second
// term exists precisely to keep it quiet.
func TestSchedulePausedTooLongNeedsBothTerms(t *testing.T) {
	age := mustThreshold(t, ruleSchedulePausedTooLong).Age
	now := time.Now()
	longAgo := metav1.NewTime(now.Add(-age - 48*time.Hour))
	recent := metav1.NewTime(now.Add(-time.Hour))

	c := newFakeClient(t,
		ns("team-a"),
		bs("team-a", "forgotten", func(s *cbv1.BackupSchedule) {
			s.Spec.Paused = true
			s.CreationTimestamp = longAgo
			s.Status.Conditions = []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionFalse, Reason: "Paused",
				LastTransitionTime: longAgo, Message: "paused",
			}}
		}),
		bs("team-a", "just-paused", func(s *cbv1.BackupSchedule) {
			s.Spec.Paused = true
			s.CreationTimestamp = longAgo
			s.Status.Conditions = []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionFalse, Reason: "Paused",
				LastTransitionTime: recent, Message: "paused",
			}}
		}),
		bs("team-a", "born-paused-yesterday", func(s *cbv1.BackupSchedule) {
			s.Spec.Paused = true
			s.CreationTimestamp = recent
		}),
		bs("team-a", "running", func(s *cbv1.BackupSchedule) { s.CreationTimestamp = longAgo }),
	)
	breaches, err := schedulePausedTooLong(context.Background(), c, now)
	if err != nil {
		t.Fatalf("predicate: %v", err)
	}
	if len(breaches) != 1 || breaches[0].Labels["schedule"] != "forgotten" {
		t.Fatalf("want only the schedule that has been paused past the window: %+v", breaches)
	}
}

// TestExternalSyncPausedTooLongNeedsBothTerms is schedulePausedTooLong's test on the other family,
// deliberately the same four cases: the sync must be older than the window AND paused for longer
// than it, and one still copying must never appear.
func TestExternalSyncPausedTooLongNeedsBothTerms(t *testing.T) {
	age := mustThreshold(t, ruleExternalSyncPausedTooLong).Age
	now := time.Now()
	longAgo := metav1.NewTime(now.Add(-age - 48*time.Hour))
	recent := metav1.NewTime(now.Add(-time.Hour))
	paused := func(at metav1.Time) []metav1.Condition {
		return []metav1.Condition{{
			Type: "Ready", Status: metav1.ConditionFalse, Reason: "Paused",
			LastTransitionTime: at, Message: "paused",
		}}
	}

	c := newFakeClient(t,
		clusterSync("forgotten", func(s *cbv1.ClusterBackupExternalSync) {
			s.Spec.Paused = true
			s.CreationTimestamp = longAgo
			s.Status.Conditions = paused(longAgo)
		}),
		clusterSync("just-paused", func(s *cbv1.ClusterBackupExternalSync) {
			s.Spec.Paused = true
			s.CreationTimestamp = longAgo
			s.Status.Conditions = paused(recent)
		}),
		clusterSync("born-paused-yesterday", func(s *cbv1.ClusterBackupExternalSync) {
			s.Spec.Paused = true
			s.CreationTimestamp = recent
		}),
		clusterSync("copying", func(s *cbv1.ClusterBackupExternalSync) {
			s.CreationTimestamp = longAgo
		}),
	)
	breaches, err := externalSyncPausedTooLong(context.Background(), c, now)
	if err != nil {
		t.Fatalf("predicate: %v", err)
	}
	if len(breaches) != 1 || breaches[0].Labels["sync"] != "forgotten" {
		t.Fatalf("want only the sync that has been paused past the window: %+v", breaches)
	}
}

// TestPausedSyncIsGuardedOutOfStaleAndReportedByPausedTooLong is the pairing the two rules exist to
// make true, checked where it actually matters: on ONE object. A paused sync must be absent from the
// staleness report — that is the guard — and present in the paused-too-long one. Getting either half
// alone is what produced the blind spot this pair closes.
func TestPausedSyncIsGuardedOutOfStaleAndReportedByPausedTooLong(t *testing.T) {
	now := time.Now()
	longAgo := metav1.NewTime(now.Add(-30 * 24 * time.Hour))
	c := newFakeClient(t, clusterSync("offsite", func(s *cbv1.ClusterBackupExternalSync) {
		s.Spec.Paused = true
		s.CreationTimestamp = longAgo
		s.Status.Conditions = []metav1.Condition{{
			Type: "Ready", Status: metav1.ConditionFalse, Reason: "Paused",
			LastTransitionTime: longAgo, Message: "paused",
		}}
	}))

	stale, err := externalSyncStale(context.Background(), c, now)
	if err != nil {
		t.Fatalf("stale predicate: %v", err)
	}
	if len(stale) != 0 {
		t.Errorf("a paused sync must not be reported as stale — that page is what the guard "+
			"removes: %+v", stale)
	}
	longPaused, err := externalSyncPausedTooLong(context.Background(), c, now)
	if err != nil {
		t.Fatalf("paused-too-long predicate: %v", err)
	}
	if len(longPaused) != 1 {
		t.Fatalf("a sync paused for a month must be reported by SOMETHING; guarding it out of the "+
			"staleness rule and reporting it nowhere is the blind spot, not the fix: %+v", longPaused)
	}
}

// TestBackupFailedCountsOnlyTheRecentOnes: the rule's window is an hour, and a Backup that failed
// last week is wreckage the gauge sibling counts, not something increase() would report.
func TestBackupFailedCountsOnlyTheRecentOnes(t *testing.T) {
	now := time.Now()
	c := newFakeClient(t,
		ns("team-a"),
		failedBackup("team-a", "recent", "daily", metav1.NewTime(now.Add(-10*time.Minute))),
		failedBackup("team-a", "ancient", "daily", metav1.NewTime(now.Add(-72*time.Hour))),
	)
	breaches, err := backupFailed(context.Background(), c, now)
	if err != nil {
		t.Fatalf("predicate: %v", err)
	}
	if len(breaches) != 1 || breaches[0].Value != 1 {
		t.Fatalf("want exactly the failure inside the 1h window: %+v", breaches)
	}
}

// TestPredicatesAreDeterministic: two reports generated from unchanged state must be
// byte-identical, or a maintainer diffing the JSON attached to two comments on one issue reads a
// diff that means nothing. Map iteration is the only disorder in this package.
func TestPredicatesAreDeterministic(t *testing.T) {
	now := time.Now()
	c := breachingCluster(t, now)
	for name, p := range allPredicates() {
		first, err := p(context.Background(), c, now)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for range 5 {
			again, err := p(context.Background(), c, now)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if len(again) != len(first) {
				t.Fatalf("%s: breach count varies between runs", name)
			}
			for i := range first {
				if again[i].Detail != first[i].Detail {
					t.Fatalf("%s: breach order varies between runs (%q vs %q)",
						name, first[i].Detail, again[i].Detail)
				}
			}
		}
	}
}

// --- fixtures -------------------------------------------------------------------------------

// breachingCluster is one cluster in which EVERY implemented predicate finds something. It is
// shared by the label and determinism tests so that adding a predicate without a matching fixture
// fails loudly (its breach list comes back empty) rather than passing vacuously.
func breachingCluster(t *testing.T, now time.Time) client.Client {
	t.Helper()
	old := metav1.NewTime(now.Add(-90 * 24 * time.Hour))
	recent := metav1.NewTime(now.Add(-10 * time.Minute))
	no := false

	objs := make([]client.Object, 0, 16)
	objs = append(objs,
		ns("team-a"),
		// A location, so `cluster` resolves to something rather than being empty everywhere.
		&cbv1.ClusterBackupLocation{
			ObjectMeta: metav1.ObjectMeta{Name: "primary"},
			Spec: cbv1.ClusterBackupLocationSpec{
				Default: true, ClusterID: "prod-eu",
				S3:         cbv1.S3Spec{Endpoint: "https://s3.example", Bucket: "b"},
				Encryption: cbv1.ClusterEncryptionSpec{ClusterKEKSecretRef: cbv1.LocalObjectReference{Name: "kek"}},
			},
		},
		// BackupMissed: active, month-old, nothing ever succeeded.
		bs("team-a", "daily", func(s *cbv1.BackupSchedule) {
			s.Spec.Schedule = "0 2 * * *"
			s.Spec.LocationRef.Name = "primary"
			s.CreationTimestamp = old
		}),
		// SchedulePausedTooLong: paused since long before the window.
		bs("team-a", "forgotten", func(s *cbv1.BackupSchedule) {
			s.Spec.Schedule = "0 3 * * *"
			s.Spec.LocationRef.Name = "primary"
			s.Spec.Paused = true
			s.CreationTimestamp = old
			s.Status.Conditions = []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionFalse, Reason: "Paused",
				LastTransitionTime: old, Message: "paused",
			}}
		}),
		// BackupFailed: inside the 1h window.
		failedBackup("team-a", "run-1", "daily", recent),
		// BackupStalled: still Uploading ninety days after it was created. Its own schedule label,
		// so its breach is one series and not entangled with the failed run above — the two rules
		// describe different halves of the same namespace and must be readable apart.
		stalledBackup("team-a", "run-wedged", "nightly", old),
		// RepositoryCheckFailed / MaintenanceStalled / StaleLocks / DiscoveryFailed, all on one
		// repository: they are independent fields and one object exercises every label path.
		repo("primary", func(r *cbv1.BackupRepository) {
			r.Status.LastCheckTime = &old
			r.Status.LastCheckResult = "Failed"
			r.Status.LastMaintenanceTime = &old
			r.Status.StaleLocks = 4
			r.Status.LastDiscoverySuccess = &no
		}),
		// ErasureBlocked.
		&cbv1.ClusterErasure{
			ObjectMeta: metav1.ObjectMeta{Name: "gdpr-1"},
			Spec: cbv1.ClusterErasureSpec{
				LocationRef: cbv1.LocalObjectReference{Name: "primary"},
			},
			Status: cbv1.ClusterErasureStatus{Phase: "Blocked", BlockedUntil: "2027-01-01T00:00:00Z"},
		},
		// ExternalSyncStale: never completed, not paused.
		clusterSync("dr-copy", func(s *cbv1.ClusterBackupExternalSync) {
			s.Spec.SourceLocationRef.Name = "primary"
			s.Spec.DestinationLocationRef.Name = "secondary"
		}),
		// ExternalSyncPausedTooLong: paused since long before the window. It must NOT also appear
		// under ExternalSyncStale — the pause guard is the whole point — which is why the stale
		// fixture above is a separate, unpaused sync rather than this one.
		clusterSync("dr-copy-cold", func(s *cbv1.ClusterBackupExternalSync) {
			s.Spec.SourceLocationRef.Name = "primary"
			s.Spec.DestinationLocationRef.Name = "cold"
			s.Spec.Paused = true
			s.CreationTimestamp = old
			s.Status.Conditions = []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionFalse, Reason: "Paused",
				LastTransitionTime: old, Message: "paused",
			}}
		}),
	)
	// PVCSnapshotPileup needs the snapshot list kind, so this client registers it.
	objs = append(objs, volumeSnapshots("team-a", "data-postgres-0",
		int(mustThreshold(t, rulePVCSnapshotPileup).Count)+3)...)
	return snapshotClient(t, objs...)
}

// snapshotClient is newFakeClient plus the VolumeSnapshot list kind, which is not in the operator's
// scheme (the collector asks for it by GVK through unstructured, and so does the predicate).
func snapshotClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := cbv1.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	s.AddKnownTypeWithName(volumeSnapshotListGVK, &unstructured.UnstructuredList{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{
		Group: volumeSnapshotListGVK.Group, Version: volumeSnapshotListGVK.Version, Kind: "VolumeSnapshot",
	}, &unstructured.Unstructured{})
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func volumeSnapshots(namespace, pvc string, n int) []client.Object {
	out := make([]client.Object, 0, n)
	for i := range n {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot",
		})
		u.SetNamespace(namespace)
		u.SetName(fmt.Sprintf("%s-snap-%03d", pvc, i))
		_ = unstructured.SetNestedField(u.Object, pvc, "spec", "source", "persistentVolumeClaimName")
		out = append(out, u)
	}
	return out
}

// mustThreshold is thresholdOf for tests that need the bound to build a fixture around it. A
// failure here is a broken test setup, not a scenario under test — TestThresholdOfRefusesRatherThan
// InventsABound is where the error path itself is exercised.
func mustThreshold(t *testing.T, rule string) Threshold {
	t.Helper()
	th, err := thresholdOf(rule)
	if err != nil {
		t.Fatalf("resolve the bound for %s: %v", rule, err)
	}
	return th
}

func ns(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func bs(namespace, name string, mutate func(*cbv1.BackupSchedule)) *cbv1.BackupSchedule {
	s := &cbv1.BackupSchedule{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: cbv1.BackupScheduleSpec{
			Schedule:    "0 2 * * *",
			LocationRef: cbv1.LocalObjectReference{Name: "primary"},
		},
	}
	mutate(s)
	return s
}

func cbs(name string, mutate func(*cbv1.ClusterBackupSchedule)) *cbv1.ClusterBackupSchedule {
	s := &cbv1.ClusterBackupSchedule{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       cbv1.ClusterBackupScheduleSpec{Schedule: "0 2 * * *"},
	}
	s.Spec.Template.Spec.LocationRef.Name = "primary"
	mutate(s)
	return s
}

func backup(namespace, name, schedule string, at metav1.Time) *cbv1.Backup {
	b := &cbv1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			Labels: map[string]string{
				apiconst.LabelSchedule: schedule,
				apiconst.LabelOrigin:   apiconst.OriginNamespace,
			},
		},
	}
	b.Spec.LocationRef.Name = "primary"
	b.Status.Phase = "Completed"
	b.Status.BackupTime = &at
	return b
}

func failedBackup(namespace, name, schedule string, at metav1.Time) *cbv1.Backup {
	b := backup(namespace, name, schedule, at)
	b.Status.Phase = "Failed"
	b.Status.BackupTime = nil
	b.Status.Conditions = []metav1.Condition{{
		Type: "Ready", Status: metav1.ConditionFalse, Reason: "Failed",
		LastTransitionTime: at, Message: "mover failed",
	}}
	return b
}

// stalledBackup is a Backup that started long ago and never reached a terminal phase — the shape
// CrystalbackupBackupStalled exists for, and the one no other fixture here produces: every other
// Backup in this file is already finished, one way or the other, which is precisely why a stall was
// invisible to the whole table before this rule.
//
// backupTime is nil and the phase is a running one, so nothing else picks it up: backupFailed skips
// it on the phase, and the collector's last_success/last_failure gates skip it for want of a
// terminal timestamp.
func stalledBackup(namespace, name, schedule string, created metav1.Time) *cbv1.Backup {
	b := backup(namespace, name, schedule, created)
	b.CreationTimestamp = created
	b.Status.Phase = "Uploading"
	b.Status.BackupTime = nil
	return b
}

func clusterSync(name string, mutate func(*cbv1.ClusterBackupExternalSync)) *cbv1.ClusterBackupExternalSync {
	s := &cbv1.ClusterBackupExternalSync{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cbv1.ClusterBackupExternalSyncSpec{
			SourceLocationRef:      cbv1.LocalObjectReference{Name: "primary"},
			DestinationLocationRef: cbv1.LocalObjectReference{Name: "secondary"},
		},
	}
	mutate(s)
	return s
}

// maps yields a map's keys. (slices.Sorted takes an iterator; this keeps the call sites short.)
func maps(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
