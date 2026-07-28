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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/repo/queue"
)

// TestMaintenanceMutexHalvesAgree is the regression guard for the one line adr/0015's forward
// contract stars: the per-repository mover mutex has TWO halves, enforced by two different
// predicates in two different packages, and marking one without the other "is only half the mutex".
//
//   - queue.blocksMovers (by OpKind) drives Manager.QuiescenceRequired, the READER side — it holds
//     NEW movers back while such an op is pending or in-flight.
//   - maintenanceOpBlocksMovers (by mover.Operation) drives waitForMoverQuiescence, the WRITER
//     side — it drains the movers ALREADY running before the op starts.
//
// Mark only the reader side and the op runs against movers that were already in flight. Mark only
// the writer side and it drains once, then new movers start underneath it. Both failures are
// silent: nothing errors, the repository just gets rewritten while someone is reading it.
//
// queue.blocksMovers is unexported, so this asserts against the exported signal that actually
// gates admission rather than against the predicate — which is the stronger test anyway.
func TestMaintenanceMutexHalvesAgree(t *testing.T) {
	pairs := []struct {
		kind queue.OpKind
		op   mover.Operation
	}{
		{queue.OpInit, mover.OpInit},
		{queue.OpForget, mover.OpForget},
		{queue.OpUnlock, mover.OpUnlock},
		{queue.OpPrune, mover.OpPrune},
		{queue.OpCheck, mover.OpCheck},
	}

	m := queue.NewManager(context.Background())
	defer m.Stop()

	for _, p := range pairs {
		repoKey := "repo-" + string(p.kind)
		block := make(chan struct{})
		entered := make(chan struct{})
		h, err := m.Enqueue(repoKey, p.kind, func(ctx context.Context) error {
			close(entered)
			<-block
			return nil
		})
		if err != nil {
			t.Fatalf("enqueue %s: %v", p.kind, err)
		}
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s never started on the queue", p.kind)
		}

		readerSide := m.QuiescenceRequired(repoKey)
		writerSide := maintenanceOpBlocksMovers(p.op)
		if readerSide != writerSide {
			t.Errorf("mover mutex halves disagree for %s/%s: admission gate (QuiescenceRequired) = %v, "+
				"writer drain (maintenanceOpBlocksMovers) = %v — adr/0015 requires both or neither",
				p.kind, p.op, readerSide, writerSide)
		}

		close(block)
		if err := h.Wait(); err != nil {
			t.Fatalf("%s op: %v", p.kind, err)
		}
	}
}

// TestMaintenanceOpDeadlineSeparatesShortFromLong pins that prune and check do NOT inherit the
// ten-minute budget sized for forget and unlock. A prune of the shared cluster repository repacks
// real data and a sampled check re-downloads part of every pack; truncating either at ten minutes
// would kill it before it converges — every run, forever, leaving the repository permanently
// un-pruned while reporting a timeout each time.
func TestMaintenanceOpDeadlineSeparatesShortFromLong(t *testing.T) {
	short := []mover.Operation{mover.OpForget, mover.OpUnlock, mover.OpInit}
	long := []mover.Operation{mover.OpPrune, mover.OpCheck}

	for _, op := range short {
		if got := maintenanceOpDeadline(op); got != maintenanceJobDeadline {
			t.Errorf("maintenanceOpDeadline(%s) = %v, want the short budget %v", op, got, maintenanceJobDeadline)
		}
	}
	for _, op := range long {
		if got := maintenanceOpDeadline(op); got != maintenanceLongOpDeadline {
			t.Errorf("maintenanceOpDeadline(%s) = %v, want the long backstop %v", op, got, maintenanceLongOpDeadline)
		}
	}
	if maintenanceLongOpDeadline <= maintenanceJobDeadline {
		t.Fatalf("the long-op backstop (%v) must exceed the short-op budget (%v)",
			maintenanceLongOpDeadline, maintenanceJobDeadline)
	}
}

// TestUnlockOutcomeClearsTheStaleLockCount pins the guard that stops the reap from resubmitting.
//
// The count comes from an S3 probe that runs every fifteen minutes. If a successful unlock left it
// alone, every reconcile in that quarter-hour would look at a non-zero count and enqueue another
// unlock — a tight loop of exclusive ops, each of which shuts mover admission cluster-wide while it
// drains. Zeroing it says what the operator now believes; the next probe re-measures, and anything
// it finds then is a NEW lock.
func TestUnlockOutcomeClearsTheStaleLockCount(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int32
	}{
		{"a successful reap zeroes the count", nil, 0},
		// A FAILED unlock must leave it alone: the locks are still there, and pretending otherwise
		// would silence CrystalbackupStaleLocks on a repository that is genuinely wedged.
		{"a failed reap leaves it standing", errors.New("repository is locked"), 3},
	}
	for _, tc := range cases {
		repo := &cbv1.BackupRepository{Status: cbv1.BackupRepositoryStatus{StaleLocks: 3}}
		out := &maintenanceOutcome{op: mover.OpUnlock, err: tc.err}
		(&MaintenanceReconciler{}).applyOutcomeToStatus(repo, out, metav1.Now())
		if repo.Status.StaleLocks != tc.want {
			t.Errorf("%s: staleLocks = %d, want %d", tc.name, repo.Status.StaleLocks, tc.want)
		}
	}
}

// TestUnlockIsNotGatedOnASchedule: prune and check wait for their cron, the reap does not. A lock
// left by a hard-killed maintenance Job blocks the next exclusive op for restic's full staleness
// horizon, and it must be cleared even on a location that configures no maintenance at all.
func TestUnlockIsNotGatedOnASchedule(t *testing.T) {
	if !maintenanceOpBlocksMovers(mover.OpUnlock) {
		t.Error("OpUnlock must drain the movers: unlock --remove-all would rip out a live backup's lock")
	}
	if maintenanceOpDeadline(mover.OpUnlock) != maintenanceJobDeadline {
		t.Error("the reap is a short op; it must not inherit the multi-hour prune backstop")
	}
}

// TestReapDueRateLimitsAndGivesUp is the regression guard for a fix that was, unbounded, worse
// than the problem it addressed.
//
// A FAILED unlock correctly leaves staleLocks standing, and the reap returns before the schedules
// are consulted. Without a cooldown and a give-up, a repository whose unlock keeps failing would
// never run prune or check again — and since unlock drains the movers, every attempt would shut
// mover admission cluster-wide for up to moverQuiescenceDeadline.
func TestReapDueRateLimitsAndGivesUp(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	r := &MaintenanceReconciler{Clock: clocktesting.NewFakeClock(now)}

	failed := func(at time.Time) cbv1.MaintenanceRecord {
		return cbv1.MaintenanceRecord{
			Operation: string(mover.OpUnlock), Result: cbv1.MaintenanceFailed,
			StartTime: metav1.NewTime(at),
		}
	}

	cases := []struct {
		name    string
		history []cbv1.MaintenanceRecord
		want    bool
	}{
		{"never attempted: reap immediately", nil, true},
		{"just failed: wait out the cooldown", []cbv1.MaintenanceRecord{failed(now.Add(-time.Minute))}, false},
		{"cooldown elapsed: try again", []cbv1.MaintenanceRecord{failed(now.Add(-2 * time.Hour))}, true},
		{
			"three consecutive failures: stop, whatever the cooldown says",
			[]cbv1.MaintenanceRecord{
				failed(now.Add(-2 * time.Hour)), failed(now.Add(-3 * time.Hour)), failed(now.Add(-4 * time.Hour)),
			},
			false,
		},
		{
			// A success between failures resets the run, so an intermittent problem does not
			// permanently disable the reap.
			"a success in between resets the count",
			[]cbv1.MaintenanceRecord{
				failed(now.Add(-2 * time.Hour)),
				{Operation: string(mover.OpUnlock), Result: cbv1.MaintenanceSucceeded, StartTime: metav1.NewTime(now.Add(-3 * time.Hour))},
				failed(now.Add(-4 * time.Hour)), failed(now.Add(-5 * time.Hour)),
			},
			true,
		},
		{
			// Other operations' failures must not count against the reap.
			"a failing prune is not a failing unlock",
			[]cbv1.MaintenanceRecord{
				{Operation: string(mover.OpPrune), Result: cbv1.MaintenanceFailed, StartTime: metav1.NewTime(now.Add(-time.Minute))},
			},
			true,
		},
	}
	for _, tc := range cases {
		repo := &cbv1.BackupRepository{Status: cbv1.BackupRepositoryStatus{
			StaleLocks: 2, RecentMaintenance: tc.history,
		}}
		if got := r.reapDue(repo); got != tc.want {
			t.Errorf("%s: reapDue = %v, want %v", tc.name, got, tc.want)
		}
	}

	if unlockRetryCooldown <= moverQuiescenceDeadline {
		t.Errorf("the reap cooldown (%v) must exceed the drain budget (%v), or admission would be shut "+
			"more often than open on a repository whose drain keeps timing out",
			unlockRetryCooldown, moverQuiescenceDeadline)
	}
}
