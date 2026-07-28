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

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/event"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/repo/queue"
)

// maintenanceAt builds a reconciler whose clock reads a fixed instant.
func maintenanceAt(now time.Time) *MaintenanceReconciler {
	return &MaintenanceReconciler{Clock: clocktesting.NewFakeClock(now), tracker: newMaintenanceTracker()}
}

// repoCreatedAt builds a BackupRepository created at t with the given maintenance history.
func repoCreatedAt(t time.Time, history ...cbv1.MaintenanceRecord) *cbv1.BackupRepository {
	return &cbv1.BackupRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "dr", CreationTimestamp: metav1.NewTime(t)},
		Status: cbv1.BackupRepositoryStatus{
			Initialized:       true,
			RecentMaintenance: history,
		},
	}
}

func locWith(m *cbv1.MaintenanceSpec, mode cbv1.LocationMode) *cbv1.ClusterBackupLocation {
	return &cbv1.ClusterBackupLocation{
		ObjectMeta: metav1.ObjectMeta{Name: "dr"},
		Spec:       cbv1.ClusterBackupLocationSpec{Mode: mode, Maintenance: m},
	}
}

// TestLastAttemptUsesTheAttemptHistory is the guard for the subtlest bug in this controller.
//
// status.lastMaintenanceTime advances only when a prune SUCCEEDS — that is what the field means,
// and the staleness signal depends on it. Using it as the cron baseline would therefore make a
// FAILING prune permanently due: every reconcile would see "last success was never / long ago",
// fire again, fail again, and hammer a repository that is already unhealthy as fast as the
// controller could resubmit. The baseline must come from the ATTEMPT history instead.
func TestLastAttemptUsesTheAttemptHistory(t *testing.T) {
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	attempted := time.Date(2026, 7, 20, 3, 0, 0, 0, time.UTC)
	r := maintenanceAt(attempted.Add(time.Hour))

	// A prune that FAILED still counts as an attempt.
	repo := repoCreatedAt(created, cbv1.MaintenanceRecord{
		Operation: string(mover.OpPrune),
		StartTime: metav1.NewTime(attempted),
		Result:    cbv1.MaintenanceFailed,
	})
	if got := r.lastAttempt(repo, mover.OpPrune); !got.Equal(attempted) {
		t.Errorf("lastAttempt(prune) = %v, want the failed attempt at %v — a failing prune would otherwise stay permanently due", got, attempted)
	}
	// An operation with no history falls back to creation, so a fresh repository does not prune on apply.
	if got := r.lastAttempt(repo, mover.OpCheck); !got.Equal(created) {
		t.Errorf("lastAttempt(check) = %v, want the creation timestamp %v", got, created)
	}
	// Newest-first ordering: the most recent attempt wins even with older ones behind it.
	repo.Status.RecentMaintenance = append(repo.Status.RecentMaintenance, cbv1.MaintenanceRecord{
		Operation: string(mover.OpPrune),
		StartTime: metav1.NewTime(created.Add(time.Hour)),
		Result:    cbv1.MaintenanceSucceeded,
	})
	if got := r.lastAttempt(repo, mover.OpPrune); !got.Equal(attempted) {
		t.Errorf("lastAttempt(prune) = %v, want the NEWEST attempt %v", got, attempted)
	}
}

// TestNextDueFiresAndPrefersPrune covers the scheduling decision: an overdue window fires, and when
// both are overdue prune wins. Prune reclaims space and owns a cluster-wide exclusive window; a
// check that waits one reconcile loses nothing, whereas a prune repeatedly displaced by a long
// check would never reclaim anything.
func TestNextDueFiresAndPrefersPrune(t *testing.T) {
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	// Well past both windows, and past the jitter offset (bounded by maintenanceJitterWindow).
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	r := maintenanceAt(now)
	repo := repoCreatedAt(created)
	loc := locWith(&cbv1.MaintenanceSpec{
		PruneSchedule: "0 3 * * *",
		CheckSchedule: "0 5 * * *",
	}, cbv1.LocationModeStandard)

	due, _ := r.nextDue(repo, loc)
	if due == nil {
		t.Fatal("nextDue returned nothing with both windows long overdue")
	}
	if due.op != mover.OpPrune {
		t.Errorf("nextDue picked %s; prune must win when both are due", due.op)
	}
	if due.kind != queue.OpPrune {
		t.Errorf("nextDue queue kind = %s, want %s — the wrong kind would skip the mover-quiescence gate", due.kind, queue.OpPrune)
	}

	// With prune already attempted, check is next.
	repo.Status.RecentMaintenance = []cbv1.MaintenanceRecord{{
		Operation: string(mover.OpPrune),
		StartTime: metav1.NewTime(now.Add(-time.Minute)),
		Result:    cbv1.MaintenanceSucceeded,
	}}
	due, _ = r.nextDue(repo, loc)
	if due == nil || due.op != mover.OpCheck {
		t.Fatalf("nextDue = %v, want check once prune has run", due)
	}
}

// TestNextDueSkipsPruneOnImmutable: object-lock forbids prune until expiry (adr/0005). Admission
// rule 6 already denies pruneSchedule on an Immutable location, so this is the defence for a
// location whose MODE changed after the schedule was set — the case admission cannot catch.
func TestNextDueSkipsPruneOnImmutable(t *testing.T) {
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	r := maintenanceAt(now)
	repo := repoCreatedAt(created)
	loc := locWith(&cbv1.MaintenanceSpec{PruneSchedule: "0 3 * * *"}, cbv1.LocationModeImmutable)

	if due, _ := r.nextDue(repo, loc); due != nil {
		t.Fatalf("nextDue = %s on an Immutable location; prune must never run there", due.op)
	}

	// The check half is unaffected: verifying an immutable repository is not only allowed, it is
	// the only maintenance it gets.
	loc.Spec.Maintenance.CheckSchedule = "0 5 * * *"
	due, _ := r.nextDue(repo, loc)
	if due == nil || due.op != mover.OpCheck {
		t.Fatalf("nextDue = %v, want check to still run on an Immutable location", due)
	}
}

// TestNextDueIgnoresAMalformedCron: a cron expression that cannot parse will never become valid on
// its own, so treating it as an error would hot-loop the controller against an object only a human
// can fix. It is skipped, and the OTHER schedule still runs.
func TestNextDueIgnoresAMalformedCron(t *testing.T) {
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	r := maintenanceAt(now)
	repo := repoCreatedAt(created)
	loc := locWith(&cbv1.MaintenanceSpec{
		PruneSchedule: "not a cron",
		CheckSchedule: "0 5 * * *",
	}, cbv1.LocationModeStandard)

	due, _ := r.nextDue(repo, loc)
	if due == nil || due.op != mover.OpCheck {
		t.Fatalf("nextDue = %v, want the valid check schedule to run despite a malformed prune schedule", due)
	}
}

// TestNextDueRespectsTheTimezone proves the field is actually consulted: "0 3 * * *" is a different
// instant in Europe/Paris than in UTC, and reading it as UTC would put a cluster-wide exclusive
// prune window in the middle of someone's working day.
func TestNextDueRespectsTheTimezone(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Skipf("no tzdata available: %v", err)
	}
	// 01:30 UTC on a July day is 03:30 in Paris: the Paris window has just passed, the UTC one has
	// not. Baseline is 00:00 UTC the same day, so only the Paris reading can be due.
	created := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 7, 3, 1, 30, 0, 0, time.UTC)
	repo := repoCreatedAt(created)

	utcDue, _ := maintenanceAt(now).nextDue(repo,
		locWith(&cbv1.MaintenanceSpec{PruneSchedule: "0 3 * * *"}, cbv1.LocationModeStandard))
	if utcDue != nil {
		t.Errorf("nextDue fired for a 03:00 UTC window at 01:30 UTC")
	}

	parisLoc := locWith(&cbv1.MaintenanceSpec{PruneSchedule: "0 3 * * *", Timezone: paris.String()}, cbv1.LocationModeStandard)
	parisDue, _ := maintenanceAt(now).nextDue(repo, parisLoc)
	if parisDue == nil {
		t.Error("nextDue did not fire for a 03:00 Europe/Paris window at 03:30 local — the timezone is being ignored")
	}
}

// TestClampRequeue keeps an idle repository between the two bounds: never tighter than the minimum
// (which would be a hot loop) and never looser than the maximum (which risks oversleeping a window
// if the cron, the timezone or the clock moves).
func TestClampRequeue(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	r := maintenanceAt(now)

	cases := []struct {
		name string
		next time.Time
		want time.Duration
	}{
		{"no next tick known", time.Time{}, maintenanceMaxRequeue},
		{"a day away", now.Add(24 * time.Hour), maintenanceMaxRequeue},
		{"seconds away", now.Add(2 * time.Second), maintenanceMinRequeue},
		{"already past", now.Add(-time.Hour), maintenanceMinRequeue},
		{"inside the band", now.Add(5 * time.Minute), 5 * time.Minute},
	}
	for _, tc := range cases {
		if got := r.clampRequeue(tc.next); got != tc.want {
			t.Errorf("%s: clampRequeue = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestSetCheckConditionCountsConsecutiveFailures: the count is what turns a one-off S3 blip into an
// actionable "this repository has been damaged for three checks" without a separate counter field.
// It must count only CHECKS, and must stop at the first non-failure.
func TestSetCheckConditionCountsConsecutiveFailures(t *testing.T) {
	r := maintenanceAt(time.Now())
	repo := repoCreatedAt(time.Now())
	failed := func(op mover.Operation) cbv1.MaintenanceRecord {
		return cbv1.MaintenanceRecord{Operation: string(op), Result: cbv1.MaintenanceFailed}
	}
	repo.Status.RecentMaintenance = []cbv1.MaintenanceRecord{
		failed(mover.OpCheck),
		failed(mover.OpPrune), // interleaved prune failures must not inflate the check count
		failed(mover.OpCheck),
		{Operation: string(mover.OpCheck), Result: cbv1.MaintenanceSucceeded}, // stops the count
		failed(mover.OpCheck),
	}

	r.setCheckCondition(repo, errors.New("pack 1a2b3c is damaged"))
	cond := findCondition(repo.Status.Conditions, ConditionRepositoryCheckFailed)
	if cond == nil {
		t.Fatal("no RepositoryCheckFailed condition was set")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("condition status = %s, want True after a failed check", cond.Status)
	}
	if !strings.Contains(cond.Message, "2 consecutive failures") {
		t.Errorf("condition message = %q, want it to report 2 consecutive check failures", cond.Message)
	}

	// A pass clears it, and does not mention a count.
	r.setCheckCondition(repo, nil)
	cond = findCondition(repo.Status.Conditions, ConditionRepositoryCheckFailed)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("condition = %v, want False after a passing check", cond)
	}
}

func findCondition(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}

// TestMaintenanceTrackerIsSingleFlight: the queue would serialise two ops anyway, but submitting a
// second one while the first is running would let a slow prune silently accumulate a backlog of
// identical prunes behind it — each of which would then run in turn against a repository that was
// already pruned.
func TestMaintenanceTrackerIsSingleFlight(t *testing.T) {
	tr := newMaintenanceTracker()
	repo := repoCreatedAt(time.Now())

	m := queue.NewManager(t.Context())
	defer m.Stop()

	block := make(chan struct{})
	entered := make(chan struct{})
	started, err := tr.start(repo, mover.OpPrune, time.Now(), func() (*queue.Handle, error) {
		return m.Enqueue(repo.Name, queue.OpPrune, func(ctx context.Context) error {
			close(entered)
			<-block
			return nil
		})
	})
	if err != nil || !started {
		t.Fatalf("first start: started=%v err=%v", started, err)
	}
	<-entered

	started, err = tr.start(repo, mover.OpCheck, time.Now(), func() (*queue.Handle, error) {
		t.Error("submit was called while an op was already in flight")
		return nil, nil
	})
	if err != nil || started {
		t.Fatalf("second start: started=%v err=%v, want started=false", started, err)
	}
	if _, running := tr.take(repo.Name); !running {
		t.Error("take reported nothing in flight while the prune was running")
	}

	close(block)
	// Once it resolves, the outcome is available exactly once and the slot is free again.
	var out *maintenanceOutcome
	deadline := time.Now().Add(2 * time.Second)
	for out == nil && time.Now().Before(deadline) {
		out, _ = tr.take(repo.Name)
		time.Sleep(2 * time.Millisecond)
	}
	if out == nil {
		t.Fatal("the finished prune outcome never became available")
	}
	if out.op != mover.OpPrune || out.err != nil {
		t.Errorf("outcome = %+v, want a successful prune", out)
	}
	if again, running := tr.take(repo.Name); again != nil || running {
		t.Error("the outcome was handed out twice, or the slot stayed occupied")
	}
}

// TestMaintenanceChurnPredicate is the mirror of TestInventoryChurnPredicate and guards the same
// M3.1 lesson from the other side: two controllers write one object's status, and if either wakes
// on the other's writes they feed each other. Discovery inventories a repository every few minutes;
// unfiltered, each of those writes would re-enter the maintenance controller, which would
// re-evaluate two cron expressions for nothing. Cheap individually, and exactly the shape of churn
// that turned into a saturated worker last time.
func TestMaintenanceChurnPredicate(t *testing.T) {
	p := maintenanceChurnPredicate()
	base := func() *cbv1.BackupRepository {
		return &cbv1.BackupRepository{
			ObjectMeta: metav1.ObjectMeta{Name: "dr", Generation: 1},
			Status: cbv1.BackupRepositoryStatus{
				Initialized:   true,
				RepositoryURL: "s3:https://s3.example/bucket/prod/eu-1",
				Mode:          cbv1.LocationModeStandard,
			},
		}
	}
	now := metav1.NewTime(time.Now())

	cases := []struct {
		name string
		mut  func(*cbv1.BackupRepository)
		want bool
	}{{
		name: "discovery's inventory write is filtered",
		mut: func(r *cbv1.BackupRepository) {
			r.Status.SnapshotCount = 42
			r.Status.NamespacesPresent = 7
			r.Status.LastDiscoveryTime = &now
		},
		want: false,
	}, {
		name: "its own history write is filtered",
		mut: func(r *cbv1.BackupRepository) {
			r.Status.LastMaintenanceTime = &now
			r.Status.RecentMaintenance = []cbv1.MaintenanceRecord{{Operation: "prune", StartTime: now}}
		},
		want: false,
	}, {
		// The flip that makes the repository maintainable at all: before it, there is nothing on
		// the far end to prune or check.
		name: "the Initialized flip wakes it",
		mut:  func(r *cbv1.BackupRepository) { r.Status.Initialized = false },
		want: true,
	}, {
		name: "a change of repository URL wakes it",
		mut:  func(r *cbv1.BackupRepository) { r.Status.RepositoryURL = "s3:https://elsewhere/bucket" },
		want: true,
	}, {
		// Immutable forbids prune, so the mode is part of what to run.
		name: "a change of mode wakes it",
		mut:  func(r *cbv1.BackupRepository) { r.Status.Mode = cbv1.LocationModeImmutable },
		want: true,
	}}

	for _, tc := range cases {
		oldRepo, newRepo := base(), base()
		tc.mut(newRepo)
		got := p.Update(event.UpdateEvent{ObjectOld: oldRepo, ObjectNew: newRepo})
		if got != tc.want {
			t.Errorf("%s: predicate = %v, want %v", tc.name, got, tc.want)
		}
	}

	// A foreign object is not ours to judge — fail open.
	if !p.Update(event.UpdateEvent{}) {
		t.Error("a non-BackupRepository update must pass the filter")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate kept-as-is = %q", got)
	}
	if got := truncate("abcdefghij", 5); got != "ab..." {
		t.Errorf("truncate = %q, want %q", got, "ab...")
	}
	if got := truncate("abcdefghij", 10); got != "abcdefghij" {
		t.Errorf("truncate at exactly the limit = %q", got)
	}
	if got := truncate("abcdef", 2); got != "ab" {
		t.Errorf("truncate below the ellipsis width = %q, want %q", got, "ab")
	}
}
