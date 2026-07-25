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
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
)

// inventoryState is what the tracker knows about one repository when a reconcile asks.
type inventoryState int

const (
	// inventoryIdle: nothing running and nothing to consume — the caller should start a pass.
	inventoryIdle inventoryState = iota
	// inventoryPending: a pass is in flight on a background goroutine; the caller must NOT block
	// on it and will be re-enqueued when it lands.
	inventoryPending
	// inventoryReady: a finished pass is waiting to be consumed (snapshots or error).
	inventoryReady
)

// inventoryResult is one completed inventory pass. at stamps when the pass landed, so a consumer
// can report the repository's inventory with the time it was actually measured (rather than the
// time it was consumed) and decide whether the result is still fresh enough to reuse (retain).
type inventoryResult struct {
	snaps []restic.Snapshot
	err   error
	at    time.Time
}

// inventoryTracker runs SnapshotLister passes OFF the reconcile worker.
//
// The lister inventories a repository by creating a `restic snapshots` Job, polling it to
// completion and reading its pod log — seconds of wall time, and cold every pass because the Job
// is one-shot (measured on the crucible: ~5.7 s per pass, growing ~27 ms per snapshot against S3;
// docs/audit-m3.1-throughput.md). Called inline it held the discovery controller's only reconcile
// worker for that entire time, so every other repository queued behind it.
//
// The tracker keeps that work single-flight PER REPOSITORY on its own goroutine and hands the
// result back to the next reconcile, which then does the fast, purely in-memory part (project, GC,
// status). Reconcile never blocks on restic again.
//
// It deliberately does NOT change the SnapshotLister contract: the production lister stays a
// straightforward blocking call, and the envtest stub keeps returning canned snapshots.
type inventoryTracker struct {
	mu       sync.Mutex
	inFlight map[string]bool
	results  map[string]*inventoryResult

	// wake carries a GenericEvent per finished pass so the controller re-enqueues that repository.
	// Buffered: a completing pass must never block on a busy controller.
	wake chan event.GenericEvent
}

func newInventoryTracker() *inventoryTracker {
	return &inventoryTracker{
		inFlight: make(map[string]bool),
		results:  make(map[string]*inventoryResult),
		wake:     make(chan event.GenericEvent, 64),
	}
}

// take reports what the tracker holds for a repository, consuming a ready result. A returned
// inventoryReady hands ownership of the result to the caller (the next pass must be started fresh).
func (t *inventoryTracker) take(repoName string) (*inventoryResult, inventoryState) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if res, ok := t.results[repoName]; ok {
		delete(t.results, repoName)
		return res, inventoryReady
	}
	if t.inFlight[repoName] {
		return nil, inventoryPending
	}
	return nil, inventoryIdle
}

// retain hands a CONSUMED result back to the tracker so the next reconcile of that repository can
// reuse it instead of paying for a whole fresh listing. It exists for the partial-pass path: when a
// pass inventoried the repository fine but could not project every group, the retry only needs to
// re-attempt the projection — re-listing the repository from S3 would cost the full O(snapshots)
// round trip to learn nothing new (docs/audit-m3.1-throughput.md).
//
// Reuse is bounded by maxAge (the location's discovery interval): a retained inventory is never
// older than the freshness discovery already promises, so a group that keeps failing costs one
// listing per interval — not one per retry, and not a frozen inventory. Returns whether the result
// was put back; false means the caller's next pass will list afresh.
//
// A pass that is somehow in flight, or a result that landed meanwhile, always wins: retention is an
// optimisation and must never displace newer data.
func (t *inventoryTracker) retain(repoName string, res *inventoryResult, maxAge time.Duration) bool {
	if res == nil || time.Since(res.at) >= maxAge {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.inFlight[repoName] || t.results[repoName] != nil {
		return false
	}
	t.results[repoName] = res
	return true
}

// start launches one inventory pass on a background goroutine, unless one is already in flight.
// It returns immediately; the reconcile that called it must return without blocking.
//
// The goroutine deliberately does NOT inherit the reconcile context (cancelled the moment Reconcile
// returns). It gets its own deadline — the same bound the lister applies internally — so a
// black-holed S3 can never strand a pass in flight forever.
func (t *inventoryTracker) start(repo *cbv1.BackupRepository, lister SnapshotLister) {
	t.mu.Lock()
	if t.inFlight[repo.Name] {
		t.mu.Unlock()
		return
	}
	t.inFlight[repo.Name] = true
	t.mu.Unlock()

	// Snapshot the fields the lister reads, so the goroutine never races the cached object.
	repoCopy := repo.DeepCopy()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), inventoryPassDeadline)
		defer cancel()

		defer func() {
			// A panic in the lister must not wedge the repository as permanently in-flight.
			if r := recover(); r != nil {
				logf.Log.WithName("discovery").Error(nil, "inventory pass panicked",
					"repository", repoCopy.Name, "panic", r)
				t.finish(repoCopy, &inventoryResult{err: errInventoryPanicked})
			}
		}()

		snaps, err := lister.List(ctx, repoCopy)
		t.finish(repoCopy, &inventoryResult{snaps: snaps, err: err})
	}()
}

// finish records a completed pass and wakes the controller for that repository.
func (t *inventoryTracker) finish(repo *cbv1.BackupRepository, res *inventoryResult) {
	res.at = time.Now()

	t.mu.Lock()
	delete(t.inFlight, repo.Name)
	t.results[repo.Name] = res
	t.mu.Unlock()

	select {
	case t.wake <- event.GenericEvent{Object: repo}:
	default:
		// The buffer is full: the controller is already saturated with wakes, and the watchdog
		// requeue will pick this result up. Dropping the event is safe, never lossy.
		logf.Log.WithName("discovery").V(1).Info("inventory wake channel full; relying on the watchdog requeue",
			"repository", repo.Name)
	}
}

// errInventoryPanicked is the result recorded when a lister pass panics, so the repository is
// released from in-flight and the failure surfaces as a normal inventory error (retried) rather
// than a permanently stuck controller.
var errInventoryPanicked = errors.New("inventory pass panicked")

const (
	// inventoryPassDeadline bounds ONE background inventory pass. Slightly above the lister's own
	// discoveryJobDeadline so the lister's error surfaces first when a Job is simply slow.
	inventoryPassDeadline = 6 * time.Minute

	// inventoryWatchdogInterval re-checks a repository whose pass is in flight (or was just
	// started). It is pure insurance against a lost wake event; the wake normally arrives first.
	// Cheap: such a reconcile consumes nothing and returns in microseconds.
	inventoryWatchdogInterval = 30 * time.Second
)
