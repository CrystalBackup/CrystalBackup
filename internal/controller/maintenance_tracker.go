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
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/repo/queue"
	"github.com/CrystalBackup/CrystalBackup/internal/repo/s3stat"
)

// maintenanceOutcome is one finished maintenance op waiting to be written into status.
type maintenanceOutcome struct {
	op        mover.Operation
	startedAt time.Time
	endedAt   time.Time
	err       error
}

// maintenanceTracker keeps a repository's maintenance op OFF the reconcile worker.
//
// The exclusive queue already runs the op body on its own goroutine, so the only thing missing is
// a way to learn that it finished without anybody blocking on the Handle. That distinction is the
// whole point: a prune holds the lane for hours (maintenanceOpDeadline), and a reconcile worker
// waiting on it is precisely the failure M3.1 spent a milestone removing from discovery — one
// worker pinned on repository I/O while every other repository queues behind it.
//
// So the tracker does three things: refuse to submit a second op for a repository that already has
// one running (the queue would serialise them anyway, but piling them up would mean a slow prune
// silently accumulating a backlog of identical prunes behind it); park the finished outcome for the
// next reconcile to consume; and wake that reconcile so the status write is prompt rather than
// waiting for the next requeue.
//
// It mirrors inventoryTracker deliberately — same states, same wake channel, same watchdog
// insurance — because the two solve the same problem for the same reason, and one shape is easier
// to reason about than two.
type maintenanceTracker struct {
	mu       sync.Mutex
	inFlight map[string]mover.Operation
	results  map[string]*maintenanceOutcome

	// The footprint probe (physical size + stale locks) is tracked the same way and for the same
	// reason, but SEPARATELY from ops: it takes no queue turn, blocks no mover, and must keep
	// running on its own cadence while a multi-hour prune holds the lane. Conflating the two would
	// mean a repository under maintenance stops reporting its size for the duration.
	probeInFlight map[string]bool
	probeResults  map[string]*probeOutcome
	probedAt      map[string]time.Time

	// wake carries a GenericEvent per finished op or probe so the controller re-enqueues that
	// repository. Buffered: a completing op must never block on a busy controller.
	wake chan event.GenericEvent
}

func newMaintenanceTracker() *maintenanceTracker {
	return &maintenanceTracker{
		inFlight:      make(map[string]mover.Operation),
		results:       make(map[string]*maintenanceOutcome),
		probeInFlight: make(map[string]bool),
		probeResults:  make(map[string]*probeOutcome),
		probedAt:      make(map[string]time.Time),
		wake:          make(chan event.GenericEvent, 32),
	}
}

// probeOutcome is one finished footprint measurement.
type probeOutcome struct {
	stats s3stat.Stats
	err   error
}

// takeProbe consumes a finished probe result, if any, and reports whether one is still running.
func (t *maintenanceTracker) takeProbe(repoName string) (*probeOutcome, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	res := t.probeResults[repoName]
	if res != nil {
		delete(t.probeResults, repoName)
	}
	return res, t.probeInFlight[repoName]
}

// probeDue reports whether it is time to measure this repository again. The interval is generous
// on purpose: this is a gauge nobody alerts on to the second, and the measurement is a full
// recursive LIST of the repository's prefix — hundreds of thousands of objects on a large one.
func (t *maintenanceTracker) probeDue(repoName string, now time.Time, every time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.probeInFlight[repoName] {
		return false
	}
	last, seen := t.probedAt[repoName]
	return !seen || now.Sub(last) >= every
}

// startProbe runs one footprint measurement on a background goroutine. Like an op, it must never
// run on the reconcile worker: a LIST over a multi-terabyte repository is a minutes-long walk, and
// blocking a worker on it would starve every other repository for a metric.
func (t *maintenanceTracker) startProbe(repo *cbv1.BackupRepository, now time.Time, measure func(context.Context) (s3stat.Stats, error)) {
	t.mu.Lock()
	if t.probeInFlight[repo.Name] {
		t.mu.Unlock()
		return
	}
	t.probeInFlight[repo.Name] = true
	t.probedAt[repo.Name] = now
	t.mu.Unlock()

	repoCopy := repo.DeepCopy()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), repositoryProbeDeadline)
		defer cancel()

		defer func() {
			// A panic in the S3 client must not wedge this repository as permanently probing.
			if rec := recover(); rec != nil {
				logf.Log.WithName("maintenance").Error(nil, "repository probe panicked",
					"repository", repoCopy.Name, "panic", rec)
				t.finishProbe(repoCopy, &probeOutcome{err: errProbePanicked})
			}
		}()

		stats, err := measure(ctx)
		t.finishProbe(repoCopy, &probeOutcome{stats: stats, err: err})
	}()
}

// finishProbe records a completed measurement and wakes the controller for that repository.
func (t *maintenanceTracker) finishProbe(repo *cbv1.BackupRepository, res *probeOutcome) {
	t.mu.Lock()
	delete(t.probeInFlight, repo.Name)
	t.probeResults[repo.Name] = res
	t.mu.Unlock()

	select {
	case t.wake <- event.GenericEvent{Object: repo}:
	default:
		logf.Log.WithName("maintenance").V(1).Info("maintenance wake channel full; relying on the watchdog requeue",
			"repository", repo.Name)
	}
}

// errProbePanicked releases a repository whose probe panicked, so the failure surfaces as a normal
// probe error (retried at the next interval) rather than a permanently stuck measurement.
var errProbePanicked = errors.New("repository probe panicked")

// take consumes a finished outcome for a repository, if one is waiting, and reports whether an op
// is still in flight. A returned outcome hands ownership to the caller: it must be written to
// status, because nothing else remembers it.
func (t *maintenanceTracker) take(repoName string) (*maintenanceOutcome, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	res := t.results[repoName]
	if res != nil {
		delete(t.results, repoName)
	}
	_, running := t.inFlight[repoName]
	return res, running
}

// start submits one maintenance op and watches for its completion in the background. It returns
// false when this repository already has an op in flight — the caller must not submit another.
//
// submit is injected rather than called directly so the whole path stays testable without a real
// queue Manager or a real cluster: the test supplies a Handle-like completion it drives itself.
func (t *maintenanceTracker) start(repo *cbv1.BackupRepository, op mover.Operation, startedAt time.Time,
	submit func() (*queue.Handle, error),
) (bool, error) {
	t.mu.Lock()
	if _, running := t.inFlight[repo.Name]; running {
		t.mu.Unlock()
		return false, nil
	}
	t.inFlight[repo.Name] = op
	t.mu.Unlock()

	handle, err := submit()
	if err != nil {
		// Never enqueued (the queue is stopping): release the slot immediately, or this repository
		// would be permanently "in flight" for an op that will never run.
		t.mu.Lock()
		delete(t.inFlight, repo.Name)
		t.mu.Unlock()
		return false, err
	}

	// Snapshot the object so the watcher goroutine never races the controller's cached copy.
	repoCopy := repo.DeepCopy()
	go func() {
		err := handle.Wait()
		t.finish(repoCopy, &maintenanceOutcome{op: op, startedAt: startedAt, endedAt: time.Now(), err: err})
	}()
	return true, nil
}

// finish records a completed op and wakes the controller for that repository.
func (t *maintenanceTracker) finish(repo *cbv1.BackupRepository, res *maintenanceOutcome) {
	t.mu.Lock()
	delete(t.inFlight, repo.Name)
	t.results[repo.Name] = res
	t.mu.Unlock()

	select {
	case t.wake <- event.GenericEvent{Object: repo}:
	default:
		// The buffer is full: the controller is already saturated with wakes, and the watchdog
		// requeue will pick this outcome up. Dropping the event is safe, never lossy.
		logf.Log.WithName("maintenance").V(1).Info("maintenance wake channel full; relying on the watchdog requeue",
			"repository", repo.Name)
	}
}
