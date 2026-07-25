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
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
)

// blockingLister is a SnapshotLister whose List parks until released, so a test can observe the
// tracker while a pass is genuinely in flight. calls counts invocations (single-flight proof).
type blockingLister struct {
	release chan struct{}
	calls   atomic.Int32
	snaps   []restic.Snapshot
	err     error
	panics  bool
}

func (l *blockingLister) List(context.Context, *cbv1.BackupRepository) ([]restic.Snapshot, error) {
	l.calls.Add(1)
	if l.release != nil {
		<-l.release
	}
	if l.panics {
		panic("boom")
	}
	return l.snaps, l.err
}

func trackerTestRepo() *cbv1.BackupRepository {
	return &cbv1.BackupRepository{ObjectMeta: metav1.ObjectMeta{Name: "dr"}}
}

// waitFor polls until cond holds, so the tests never depend on goroutine scheduling luck.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestInventoryTrackerRunsOffTheWorker pins the property the whole lot exists for: starting a pass
// must NOT block the caller (the reconcile worker), and the repository must read as pending until
// the pass lands (docs/audit-m3.1-throughput.md).
func TestInventoryTrackerRunsOffTheWorker(t *testing.T) {
	tr := newInventoryTracker()
	lister := &blockingLister{
		release: make(chan struct{}),
		snaps:   []restic.Snapshot{{ID: "id-1"}},
	}
	repo := trackerTestRepo()

	if _, state := tr.take(repo.Name); state != inventoryIdle {
		t.Fatalf("fresh tracker: state = %v, want inventoryIdle", state)
	}

	// start must return while List is still parked — that is the non-blocking guarantee.
	done := make(chan struct{})
	go func() { tr.start(repo, lister); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("start blocked on the lister: the reconcile worker would be held")
	}

	waitFor(t, "the pass to be in flight", func() bool { return lister.calls.Load() == 1 })
	if _, state := tr.take(repo.Name); state != inventoryPending {
		t.Fatalf("while in flight: state = %v, want inventoryPending", state)
	}

	// Single-flight: a second start while one is in flight must not launch another pass.
	tr.start(repo, lister)
	if got := lister.calls.Load(); got != 1 {
		t.Errorf("lister called %d times, want 1 (single-flight per repository)", got)
	}

	close(lister.release)

	waitFor(t, "the result to land", func() bool {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		return tr.results[repo.Name] != nil
	})

	res, state := tr.take(repo.Name)
	if state != inventoryReady {
		t.Fatalf("after completion: state = %v, want inventoryReady", state)
	}
	if res.err != nil || len(res.snaps) != 1 || res.snaps[0].ID != "id-1" {
		t.Errorf("result = %+v, want the lister's one snapshot", res)
	}

	// The result is consumed exactly once: the next look is idle, so the next tick starts fresh.
	if _, state := tr.take(repo.Name); state != inventoryIdle {
		t.Errorf("after consuming: state = %v, want inventoryIdle", state)
	}
	// And a wake was published so the controller re-enqueues rather than waiting for the watchdog.
	select {
	case <-tr.wake:
	default:
		t.Error("no wake event published for the finished pass")
	}
}

// TestInventoryTrackerRetain pins the partial-pass optimisation: a consumed result can be handed
// back so the retry re-attempts only the projection, instead of paying for a whole fresh
// `restic snapshots` listing to learn the same thing (docs/audit-m3.1-throughput.md). The reuse is
// bounded by the caller's discovery interval, and never displaces newer data.
func TestInventoryTrackerRetain(t *testing.T) {
	repo := trackerTestRepo()
	fresh := func() *inventoryResult {
		return &inventoryResult{snaps: []restic.Snapshot{{ID: "id-1"}}, at: time.Now()}
	}

	t.Run("a fresh result is handed back and consumed once", func(t *testing.T) {
		tr := newInventoryTracker()
		if !tr.retain(repo.Name, fresh(), time.Minute) {
			t.Fatal("retain() = false, want true for a result well inside maxAge")
		}
		res, state := tr.take(repo.Name)
		if state != inventoryReady || len(res.snaps) != 1 {
			t.Fatalf("take() = (%+v, %v), want the retained result as inventoryReady", res, state)
		}
		if _, state := tr.take(repo.Name); state != inventoryIdle {
			t.Errorf("after consuming a retained result: state = %v, want inventoryIdle", state)
		}
	})

	t.Run("a result older than maxAge is refused so the next pass lists afresh", func(t *testing.T) {
		tr := newInventoryTracker()
		stale := &inventoryResult{snaps: []restic.Snapshot{{ID: "id-1"}}, at: time.Now().Add(-2 * time.Minute)}
		if tr.retain(repo.Name, stale, time.Minute) {
			t.Error("retain() = true for a result past maxAge, want false (the inventory must not go stale)")
		}
		if _, state := tr.take(repo.Name); state != inventoryIdle {
			t.Errorf("state = %v, want inventoryIdle so the caller starts a real pass", state)
		}
	})

	t.Run("newer data always wins", func(t *testing.T) {
		tr := newInventoryTracker()
		lister := &blockingLister{release: make(chan struct{})}
		tr.start(repo, lister)
		waitFor(t, "the pass to be in flight", func() bool { return lister.calls.Load() == 1 })
		if tr.retain(repo.Name, fresh(), time.Minute) {
			t.Error("retain() = true while a pass is in flight, want false")
		}
		close(lister.release)

		waitFor(t, "the pass to land", func() bool {
			tr.mu.Lock()
			defer tr.mu.Unlock()
			return tr.results[repo.Name] != nil
		})
		if tr.retain(repo.Name, fresh(), time.Minute) {
			t.Error("retain() = true over an unconsumed result, want false")
		}
	})
}

// TestInventoryTrackerSurfacesErrors checks a failed pass is handed back as a consumable error
// (discovery turns it into an InventoryFailed event + retry) and does not strand the repository.
func TestInventoryTrackerSurfacesErrors(t *testing.T) {
	tr := newInventoryTracker()
	wantErr := errors.New("s3 unreachable")
	lister := &blockingLister{err: wantErr}
	repo := trackerTestRepo()

	tr.start(repo, lister)
	waitFor(t, "the failed pass to land", func() bool {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		return tr.results[repo.Name] != nil
	})

	res, state := tr.take(repo.Name)
	if state != inventoryReady || !errors.Is(res.err, wantErr) {
		t.Fatalf("state = %v, err = %v; want inventoryReady carrying the lister error", state, res.err)
	}
}

// TestInventoryTrackerRecoversFromPanic is the anti-wedge guard: a panicking lister must release
// the in-flight slot, or that repository would never be inventoried again for the operator's life.
func TestInventoryTrackerRecoversFromPanic(t *testing.T) {
	tr := newInventoryTracker()
	repo := trackerTestRepo()

	tr.start(repo, &blockingLister{panics: true})

	waitFor(t, "the panicking pass to be recorded", func() bool {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		return tr.results[repo.Name] != nil
	})

	res, state := tr.take(repo.Name)
	if state != inventoryReady || !errors.Is(res.err, errInventoryPanicked) {
		t.Fatalf("state = %v, err = %v; want a consumable errInventoryPanicked", state, res.err)
	}
	// Not stuck: the repository can start a new pass.
	if _, state := tr.take(repo.Name); state != inventoryIdle {
		t.Errorf("after a panic: state = %v, want inventoryIdle (repository must not wedge)", state)
	}
}
