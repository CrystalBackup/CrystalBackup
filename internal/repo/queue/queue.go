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

// Package queue implements the per-BackupRepository exclusive work queue: an
// in-memory, keyed single-flight that serialises every mutating restic
// operation targeting one shared repository.
//
// # Why this exists
//
// CrystalBackup points many namespaces (and the cluster-DR plane) at a single
// shared restic repository per BackupLocation. restic's on-disk format tolerates
// concurrent reads and concurrent backups, but repository-lifecycle operations —
// init, forget, prune, check and erasure — are NOT safe to run concurrently
// against the same repository: two `restic init` racing on an empty repo is the
// exact shared-repo init race reported upstream as K8up #1055, and a prune racing
// a forget (or another prune) can drop packs that another writer still
// references. This queue is the direct fix. For a given repository key it admits
// at most one such operation at a time, in FIFO order, while operations on
// different repositories proceed fully concurrently.
//
// # Why in-memory, not a Lease or mutex CRD
//
// The operator runs single-active under controller-runtime leader election, so
// at any instant exactly one process may mutate repositories. An in-process
// keyed mutex is therefore a sufficient — and far cheaper — serialisation point
// than a Kubernetes Lease or a bespoke mutex CRD: it adds zero API-server
// round-trips to a hot reconcile path and cannot itself wedge on API latency.
// restic's own lock object on the S3 repository remains the cross-process
// backstop for the (leader-election-forbidden, but defensively assumed)
// split-brain window; this queue is the intra-process guarantee that a single
// leader never races itself. See adr/0010 and the M1 plan.
//
// # Concurrency model
//
// Work is dispatched by exactly one worker goroutine per key, created lazily on
// the first [Manager.Enqueue] for that key and living until [Manager.Stop]. Each
// worker drains its key's FIFO queue strictly one operation at a time. There is
// no global lock, so distinct keys never block one another. Idle workers are kept
// alive until Stop rather than torn down when their queue empties: the number of
// distinct keys equals the number of BackupRepository objects, which is small and
// bounded, so a goroutine per key is cheap, and keeping them avoids a whole class
// of lazy-teardown races (a producer selecting a worker that is concurrently
// deciding to exit). Reclaiming idle workers is a deliberate post-M1 option.
//
// # Lifecycle and cancellation
//
// Each operation runs with a context derived from the Manager's base context, so
// Stop cancels every in-flight operation. Queued-but-unstarted operations are
// resolved with the cancellation error and never invoke their run func. Stop
// stops accepting new work and joins all worker goroutines, so a caller may treat
// Stop's return as "no queue goroutine of this Manager is still alive". Because
// goroutines cannot be killed in Go, Stop blocks until in-flight run funcs
// return; run funcs must therefore honour their context for Stop to return
// promptly. Enqueue after Stop returns [ErrStopped] rather than panicking or
// blocking.
package queue

import (
	"context"
	"errors"
	"runtime/debug"
	"sync"
)

// OpKind enumerates the mutating, repository-exclusive restic operations that
// must be serialised per repository. The string values are stable identifiers
// (they may surface in logs, events and metric labels), and the set is the M1
// stub taxonomy onto which the reconcilers plug directly: the BackupRepository
// controller enqueues OpInit on first reconcile, the maintenance controllers
// enqueue OpForget/OpPrune/OpCheck, and ClusterErasure enqueues OpErase. Only
// mutating operations belong here — concurrency-safe reads (list snapshots,
// stats) are intentionally NOT gated by this queue and must not be enqueued.
type OpKind string

const (
	// OpInit is `restic init`. Racing two of these on an empty shared repository
	// is K8up #1055 and the primary reason this queue exists.
	OpInit OpKind = "init"
	// OpForget removes snapshots for retention; it must not race a prune or a
	// concurrent forget, both of which rewrite the snapshot set and index.
	OpForget OpKind = "forget"
	// OpPrune repacks and deletes unreferenced data — the most destructive
	// maintenance operation; Standard-mode only and exclusive by construction.
	OpPrune OpKind = "prune"
	// OpCheck verifies repository integrity; serialised so it never observes a
	// half-applied prune/forget and never contends for the on-repo lock.
	OpCheck OpKind = "check"
	// OpErase performs scoped erasure (ClusterErasure); it mutates the repository
	// and so shares the single exclusive lane.
	OpErase OpKind = "erase"
)

// ErrStopped is returned by [Manager.Enqueue] once the Manager has been stopped
// (via [Manager.Stop] or cancellation of the context passed to [NewManager]).
// It lets a controller distinguish "the queue is shutting down, retry later" from
// an operation error, without the Enqueue call ever blocking or panicking.
var ErrStopped = errors.New("repo/queue: manager stopped")

// ErrNilRun is returned by [Manager.Enqueue] when the run func is nil. A nil run
// is a programming error; returning an error (rather than panicking or silently
// succeeding) surfaces it at the call site without taking the operator down.
var ErrNilRun = errors.New("repo/queue: nil run function")

// Manager is the set of per-key exclusive work queues. It is safe for concurrent
// use by many goroutines. Construct one per operator process with [NewManager].
type Manager struct {
	// baseCtx is the parent of every per-operation context; cancelling it (via
	// cancel, from Stop, or via the parent passed to NewManager) is the single
	// signal that shuts every worker down. It is set once at construction and
	// only read thereafter, so workers may read it without holding mu.
	baseCtx context.Context
	cancel  context.CancelFunc

	// mu guards workers and stopped, and — critically — is the same lock under
	// which wg.Add is called, so that Add can never race Stop's wg.Wait through a
	// zero counter (all Adds happen-before stopped is observed true).
	mu      sync.Mutex
	workers map[string]*worker
	stopped bool
	wg      sync.WaitGroup
}

// NewManager returns a ready Manager whose lifetime is bound to ctx: cancelling
// ctx shuts the queue down exactly as [Manager.Stop] does (minus the synchronous
// join). Pass the operator's root/manager context so queue shutdown follows
// process shutdown; call Stop for an explicit, synchronous drain-and-join. A nil
// ctx is treated as [context.Background].
func NewManager(ctx context.Context) *Manager {
	if ctx == nil {
		ctx = context.Background()
	}
	base, cancel := context.WithCancel(ctx)
	return &Manager{
		baseCtx: base,
		cancel:  cancel,
		workers: make(map[string]*worker),
	}
}

// Enqueue schedules run as an exclusive operation on the repository identified by
// repoKey and returns a [Handle] to await it. repoKey is opaque to the Manager;
// the caller must supply a value that stably and uniquely identifies one
// repository (e.g. namespace/name of the BackupRepository), because that string
// alone defines the serialisation boundary.
//
// Semantics:
//   - At most one operation per repoKey runs at any instant.
//   - Operations for the same repoKey run in the order their Enqueue calls
//     acquire the Manager lock (FIFO); operations for different keys run
//     concurrently.
//   - run receives a context derived from the Manager base context, cancelled
//     when the operation completes or when the Manager stops. run should honour
//     it. A panic inside run is recovered and surfaced as a [*PanicError] on the
//     Handle; it does not crash the worker or the process.
//   - The returned Handle is resolved exactly once: with run's error, with a
//     PanicError, or — if the Manager stops before the operation starts — with
//     the base context's cancellation error.
//
// Enqueue never blocks on queue depth (the per-key queue is unbounded; see the
// package doc for the rationale and the bounded-memory trade-off) and never
// blocks waiting for the operation to run. It returns (nil, [ErrStopped]) once
// the Manager is stopped and (nil, [ErrNilRun]) if run is nil.
func (m *Manager) Enqueue(repoKey string, kind OpKind, run func(ctx context.Context) error) (*Handle, error) {
	if run == nil {
		return nil, ErrNilRun
	}
	h := newHandle(kind, run)

	m.mu.Lock()
	// baseCtx.Err() catches shutdown driven by the parent context passed to
	// NewManager, for which stopped may still be false.
	if m.stopped || m.baseCtx.Err() != nil {
		m.mu.Unlock()
		return nil, ErrStopped
	}
	w, ok := m.workers[repoKey]
	if !ok {
		w = newWorker(repoKey)
		m.workers[repoKey] = w
		// Add under mu (same lock that guards stopped): every Add thus
		// happens-before Stop marks stopped and calls wg.Wait, so Wait never
		// races an Add through the zero counter.
		m.wg.Add(1)
		go m.runWorker(w)
	}
	// Appending while holding mu makes queue order equal Enqueue-lock order even
	// under concurrent producers, so FIFO is defined by Enqueue ordering rather
	// than by an internal race.
	w.enqueue(h)
	m.mu.Unlock()
	return h, nil
}

// Len reports the number of operations queued for repoKey that have not yet
// started. It excludes the in-flight operation (if any) and returns 0 for an
// unknown key. It is a best-effort observability helper — inherently racy — and
// must not be used for control decisions.
func (m *Manager) Len(repoKey string) int {
	m.mu.Lock()
	w, ok := m.workers[repoKey]
	m.mu.Unlock()
	if !ok {
		return 0
	}
	return w.len()
}

// Stop shuts the Manager down: it stops accepting new work, cancels the base
// context (so every in-flight operation observes cancellation and every
// queued-but-unstarted operation is resolved with the cancellation error without
// running), and joins all worker goroutines before returning. Stop is idempotent
// and safe to call from multiple goroutines; every caller returns only once the
// join has completed. Because a run func that ignores its context cannot be
// interrupted, Stop blocks until such an operation returns.
func (m *Manager) Stop() {
	m.mu.Lock()
	already := m.stopped
	m.stopped = true
	m.mu.Unlock()
	if !already {
		m.cancel()
	}
	// wg.Wait is intentionally outside mu so concurrent Stop callers and the
	// workers' wg.Done never contend on the Manager lock during shutdown.
	m.wg.Wait()
}

// runWorker is the single consumer goroutine for one key. It drains the FIFO
// queue one operation at a time and exits — resolving any still-queued
// operations as cancelled — as soon as the base context is done.
func (m *Manager) runWorker(w *worker) {
	defer m.wg.Done()
	for {
		// Prefer shutdown over starting new work: if the Manager is stopping,
		// drain the remainder as cancelled and exit without running anything.
		select {
		case <-m.baseCtx.Done():
			m.drainCancelled(w)
			return
		default:
		}

		h, ok := w.pop()
		if !ok {
			// Queue empty: block until a producer signals new work or shutdown.
			// notify is buffered(1), so a signal sent between the pop above and
			// this select is retained and cannot be lost.
			select {
			case <-w.notify:
				continue
			case <-m.baseCtx.Done():
				m.drainCancelled(w)
				return
			}
		}

		// Re-check shutdown in the window between popping and running so a Stop
		// racing an available operation cancels it instead of starting it.
		select {
		case <-m.baseCtx.Done():
			h.resolve(m.baseCtx.Err())
			m.drainCancelled(w)
			return
		default:
		}

		m.runOne(h)
	}
}

// drainCancelled resolves every operation still queued on w with the base
// context's error, guaranteeing that queued-but-unstarted work never runs after
// shutdown and that no caller awaiting a Handle is left blocked forever.
func (m *Manager) drainCancelled(w *worker) {
	for _, h := range w.drain() {
		h.resolve(m.baseCtx.Err())
	}
}

// runOne executes a single operation under a fresh per-operation context derived
// from baseCtx, recovering any panic, and resolves the Handle with the outcome.
func (m *Manager) runOne(h *Handle) {
	opCtx, cancel := context.WithCancel(m.baseCtx)
	defer cancel()

	run := h.takeRun()
	h.resolve(safeRun(opCtx, run))
}

// safeRun invokes run and converts a panic into a [*PanicError] so that a single
// misbehaving operation cannot unwind the worker goroutine or crash the operator.
// The stack is captured at the recovery point for later logging by the caller.
func safeRun(ctx context.Context, run func(context.Context) error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &PanicError{Value: r, Stack: debug.Stack()}
		}
	}()
	return run(ctx)
}

// worker owns the FIFO queue and the single consumer goroutine for one key.
//
// The queue is an unbounded slice guarded by mu; notify is a buffered(1) wakeup
// that hands off from producers (Enqueue) to the consumer without ever blocking a
// producer. Unboundedness is an accepted M1 trade-off: repository operations are
// driven by reconcile loops, not user traffic, so their in-flight count is
// naturally small, and never blocking a reconcile goroutine matters more than
// back-pressure here. A depth bound can be added later if a pathological producer
// is ever observed.
type worker struct {
	key    string
	mu     sync.Mutex
	queue  []*Handle
	notify chan struct{}
}

func newWorker(key string) *worker {
	return &worker{
		key:    key,
		notify: make(chan struct{}, 1),
	}
}

// enqueue appends h and signals the consumer. The signal is best-effort: if a
// wakeup is already pending the send is dropped, which is safe because a pending
// signal guarantees the consumer will loop at least once more and observe the
// appended item.
func (w *worker) enqueue(h *Handle) {
	w.mu.Lock()
	w.queue = append(w.queue, h)
	w.mu.Unlock()
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

// pop removes and returns the head of the queue, or (nil, false) if empty.
func (w *worker) pop() (*Handle, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.queue) == 0 {
		return nil, false
	}
	h := w.queue[0]
	w.queue[0] = nil // release the reference so the slice backing array can be GC'd
	w.queue = w.queue[1:]
	return h, true
}

// drain removes and returns every queued operation, leaving the queue empty.
func (w *worker) drain() []*Handle {
	w.mu.Lock()
	defer w.mu.Unlock()
	rest := w.queue
	w.queue = nil
	return rest
}

// len returns the current queue depth.
func (w *worker) len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.queue)
}
