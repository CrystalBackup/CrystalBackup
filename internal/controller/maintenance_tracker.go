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
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/repo/queue"
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

	// wake carries a GenericEvent per finished op so the controller re-enqueues that repository.
	// Buffered: a completing op must never block on a busy controller.
	wake chan event.GenericEvent
}

func newMaintenanceTracker() *maintenanceTracker {
	return &maintenanceTracker{
		inFlight: make(map[string]mover.Operation),
		results:  make(map[string]*maintenanceOutcome),
		wake:     make(chan event.GenericEvent, 32),
	}
}

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
