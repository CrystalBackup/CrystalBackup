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

package queue

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestOpKindValues pins the stub taxonomy's string values. They may appear in
// logs, events and metric labels and are the identifiers the reconcilers plug
// into, so a silent change is a contract break and is asserted verbatim.
func TestOpKindValues(t *testing.T) {
	cases := []struct {
		got  OpKind
		want string
	}{
		{OpInit, "init"},
		{OpForget, "forget"},
		{OpPrune, "prune"},
		{OpCheck, "check"},
		{OpErase, "erase"},
		{OpUnlock, "unlock"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("OpKind = %q, want %q", string(c.got), c.want)
		}
	}
}

// TestExclusiveSerializationPerKey is the core correctness gate: N concurrent
// Enqueue calls on the SAME key must never overlap in execution. Overlap is
// detected two ways that do not depend on timing luck: a sync.Mutex.TryLock that
// must always succeed (a failure means a second op holds it — a true overlap),
// and an atomic "active" counter that must read exactly 1 on entry. A small sleep
// widens the window so any accidental concurrency is caught. Run under
// -race -count=20; a per-key mutex regression fails deterministically via the
// TryLock guard, not merely as a race warning.
func TestExclusiveSerializationPerKey(t *testing.T) {
	m := NewManager(context.Background())
	defer m.Stop()

	const n = 100
	var (
		guard   sync.Mutex // TryLock must always succeed if truly exclusive
		active  int32      // must be exactly 1 while an op runs
		ran     int32      // total ops that executed
		overlap int32      // set to 1 on any detected overlap
	)

	handles := make([]*Handle, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			h, err := m.Enqueue("repo-A", OpForget, func(ctx context.Context) error {
				if !guard.TryLock() {
					atomic.StoreInt32(&overlap, 1)
				} else {
					defer guard.Unlock()
				}
				if atomic.AddInt32(&active, 1) != 1 {
					atomic.StoreInt32(&overlap, 1)
				}
				time.Sleep(200 * time.Microsecond)
				atomic.AddInt32(&active, -1)
				atomic.AddInt32(&ran, 1)
				return nil
			})
			if err != nil {
				t.Errorf("Enqueue: %v", err)
				return
			}
			handles[i] = h
		}()
	}
	wg.Wait()

	for i, h := range handles {
		if h == nil {
			t.Fatalf("handle %d is nil", i)
		}
		if err := h.Wait(); err != nil {
			t.Errorf("op %d: unexpected error %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&ran); got != n {
		t.Errorf("ran = %d, want %d", got, n)
	}
	if atomic.LoadInt32(&overlap) != 0 {
		t.Error("detected overlapping execution on a single key (exclusivity violated)")
	}
	if got := atomic.LoadInt32(&active); got != 0 {
		t.Errorf("active counter = %d after drain, want 0", got)
	}
}

// TestConcurrentAcrossKeys proves distinct keys are NOT globally serialised: K
// operations, one per key, all block on a shared barrier that only releases once
// all K have entered. If a global lock existed, only the first would enter and
// the barrier would never release, so the test would time out. Reaching the
// barrier and observing K simultaneously in-flight is the positive proof of
// per-key (not global) exclusivity.
func TestConcurrentAcrossKeys(t *testing.T) {
	m := NewManager(context.Background())
	defer m.Stop()

	const k = 8
	var arrived sync.WaitGroup
	arrived.Add(k)
	release := make(chan struct{})
	var inflight, maxSeen int32

	handles := make([]*Handle, k)
	for i := 0; i < k; i++ {
		h, err := m.Enqueue(fmt.Sprintf("repo-%d", i), OpCheck, func(ctx context.Context) error {
			cur := atomic.AddInt32(&inflight, 1)
			for {
				old := atomic.LoadInt32(&maxSeen)
				if cur <= old || atomic.CompareAndSwapInt32(&maxSeen, old, cur) {
					break
				}
			}
			arrived.Done()
			<-release
			atomic.AddInt32(&inflight, -1)
			return nil
		})
		if err != nil {
			t.Fatalf("Enqueue key %d: %v", i, err)
		}
		handles[i] = h
	}

	// All K ops must reach the barrier concurrently; a global lock would deadlock
	// here and trip the timeout.
	done := make(chan struct{})
	go func() { arrived.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("keys did not run concurrently: barrier never reached (looks globally serialized)")
	}

	if got := atomic.LoadInt32(&maxSeen); got != k {
		t.Errorf("max concurrent in-flight = %d, want %d", got, k)
	}

	close(release)
	for i, h := range handles {
		if err := h.Wait(); err != nil {
			t.Errorf("op %d: unexpected error %v", i, err)
		}
	}
}

// TestFIFOWithinKey checks in-key ordering: operations enqueued sequentially run
// in enqueue order. Concurrent enqueue has no observable pre-ordering, so FIFO is
// asserted here with deterministic sequential submission (the exclusivity gate is
// TestExclusiveSerializationPerKey). Each op records its index; the result slice
// must be strictly ascending 0..M-1.
func TestFIFOWithinKey(t *testing.T) {
	m := NewManager(context.Background())
	defer m.Stop()

	const items = 50
	var (
		mu  sync.Mutex
		got []int
	)
	handles := make([]*Handle, items)
	for i := 0; i < items; i++ {
		idx := i
		h, err := m.Enqueue("repo-fifo", OpForget, func(ctx context.Context) error {
			mu.Lock()
			got = append(got, idx)
			mu.Unlock()
			return nil
		})
		if err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
		handles[i] = h
	}
	for _, h := range handles {
		if err := h.Wait(); err != nil {
			t.Errorf("unexpected error %v", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != items {
		t.Fatalf("ran %d ops, want %d", len(got), items)
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("execution order not FIFO: position %d ran op %d; full order %v", i, v, got)
		}
	}
}

// TestStopCancelsInFlightAndQueued exercises the shutdown contract: an in-flight
// op observes context cancellation, ops queued behind it never run and resolve
// with the cancellation error, and Enqueue after Stop returns ErrStopped. The
// blocking head op keeps the two followers strictly queued, so the outcome is
// deterministic.
func TestStopCancelsInFlightAndQueued(t *testing.T) {
	m := NewManager(context.Background())

	started := make(chan struct{})
	sawCancel := make(chan struct{})
	h0, err := m.Enqueue("repo-stop", OpPrune, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(sawCancel)
		return ctx.Err()
	})
	if err != nil {
		t.Fatalf("Enqueue head: %v", err)
	}
	<-started // guarantee the head op is in-flight before we queue behind it

	var followerRan int32
	follower := func(ctx context.Context) error {
		atomic.AddInt32(&followerRan, 1)
		return nil
	}
	h1, err := m.Enqueue("repo-stop", OpCheck, follower)
	if err != nil {
		t.Fatalf("Enqueue follower 1: %v", err)
	}
	h2, err := m.Enqueue("repo-stop", OpForget, follower)
	if err != nil {
		t.Fatalf("Enqueue follower 2: %v", err)
	}

	m.Stop() // cancels in-flight, drains queued, joins workers

	select {
	case <-sawCancel:
	default:
		t.Fatal("in-flight op did not observe context cancellation")
	}
	if err := h0.Err(); !errors.Is(err, context.Canceled) {
		t.Errorf("head op Err = %v, want context.Canceled", err)
	}
	if got := atomic.LoadInt32(&followerRan); got != 0 {
		t.Errorf("queued ops ran after Stop: %d executed", got)
	}
	if err := h1.Err(); !errors.Is(err, context.Canceled) {
		t.Errorf("queued op 1 Err = %v, want context.Canceled", err)
	}
	if err := h2.Err(); !errors.Is(err, context.Canceled) {
		t.Errorf("queued op 2 Err = %v, want context.Canceled", err)
	}

	if _, err := m.Enqueue("repo-stop", OpInit, follower); !errors.Is(err, ErrStopped) {
		t.Errorf("Enqueue after Stop = %v, want ErrStopped", err)
	}
}

// TestStopIsIdempotent verifies Stop can be called repeatedly and from an
// already-stopped state without panicking or blocking forever.
func TestStopIsIdempotent(t *testing.T) {
	m := NewManager(context.Background())
	if _, err := m.Enqueue("k", OpCheck, func(ctx context.Context) error { return nil }); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	m.Stop()
	m.Stop() // second call must be a no-op that returns
}

// TestParentContextCancellationStops proves the manager honours the context
// passed to NewManager: cancelling it shuts the queue down so Enqueue returns
// ErrStopped, without an explicit Stop.
func TestParentContextCancellationStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := NewManager(ctx)
	defer m.Stop()

	cancel()
	// The shutdown is observed synchronously in Enqueue via baseCtx.Err().
	if _, err := m.Enqueue("k", OpInit, func(ctx context.Context) error { return nil }); !errors.Is(err, ErrStopped) {
		t.Errorf("Enqueue after parent cancel = %v, want ErrStopped", err)
	}
}

// TestNoGoroutineLeak proves Stop joins every worker goroutine. Many keys are
// used so a real leak (a goroutine per key) would dwarf any test-runtime noise;
// the count is polled back to baseline with a tolerance for transient runtime
// goroutines. Stop's wg.Wait is the authoritative join proof; this is the
// belt-and-suspenders observable check.
func TestNoGoroutineLeak(t *testing.T) {
	runtime.GC()
	before := runtime.NumGoroutine()

	m := NewManager(context.Background())
	const keys = 20
	for i := 0; i < keys; i++ {
		key := fmt.Sprintf("repo-%d", i)
		for j := 0; j < 5; j++ {
			if _, err := m.Enqueue(key, OpCheck, func(ctx context.Context) error {
				time.Sleep(time.Millisecond)
				return nil
			}); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
		}
	}
	m.Stop()

	const tolerance = 3
	deadline := time.Now().Add(3 * time.Second)
	var after int
	for {
		after = runtime.NumGoroutine()
		if after <= before+tolerance || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if after > before+tolerance {
		t.Errorf("goroutine leak after Stop: before=%d after=%d (started %d worker goroutines)", before, after, keys)
	}
}

// TestErrorSurfaces checks that a run func's error reaches the Handle unchanged
// (errors.Is-comparable to the sentinel).
func TestErrorSurfaces(t *testing.T) {
	m := NewManager(context.Background())
	defer m.Stop()

	sentinel := errors.New("restic init failed")
	h, err := m.Enqueue("k", OpInit, func(ctx context.Context) error { return sentinel })
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := h.Wait(); !errors.Is(err, sentinel) {
		t.Errorf("Handle.Err = %v, want %v", err, sentinel)
	}
}

// TestPanicDoesNotKillManager proves a panicking run func is recovered and
// surfaced as *PanicError, and that the worker survives to run the next op on the
// same key.
func TestPanicDoesNotKillManager(t *testing.T) {
	m := NewManager(context.Background())
	defer m.Stop()

	h, err := m.Enqueue("k", OpPrune, func(ctx context.Context) error {
		panic("kaboom")
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	perr := h.Wait()
	var pe *PanicError
	if !errors.As(perr, &pe) {
		t.Fatalf("Handle.Err = %v, want *PanicError", perr)
	}
	if pe.Value != "kaboom" {
		t.Errorf("PanicError.Value = %v, want %q", pe.Value, "kaboom")
	}
	if len(pe.Stack) == 0 {
		t.Error("PanicError.Stack is empty, want a captured stack")
	}

	// The worker must still be alive and process subsequent work on the same key.
	h2, err := m.Enqueue("k", OpCheck, func(ctx context.Context) error { return nil })
	if err != nil {
		t.Fatalf("Enqueue after panic: %v", err)
	}
	if err := h2.Wait(); err != nil {
		t.Errorf("op after panic failed, worker did not survive: %v", err)
	}
}

// TestEnqueueNilRun checks the nil-run guard.
func TestEnqueueNilRun(t *testing.T) {
	m := NewManager(context.Background())
	defer m.Stop()
	if _, err := m.Enqueue("k", OpInit, nil); !errors.Is(err, ErrNilRun) {
		t.Errorf("Enqueue(nil) = %v, want ErrNilRun", err)
	}
}

// TestHandleMetadata checks the Handle exposes its kind and that Err before
// completion is nil (the documented pre-resolution reading).
func TestHandleMetadata(t *testing.T) {
	m := NewManager(context.Background())
	defer m.Stop()

	block := make(chan struct{})
	h, err := m.Enqueue("k", OpErase, func(ctx context.Context) error {
		<-block
		return nil
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if h.Kind() != OpErase {
		t.Errorf("Kind = %q, want %q", h.Kind(), OpErase)
	}
	// Not yet resolved: Err reports nil and Done is open.
	select {
	case <-h.Done():
		t.Fatal("Done closed before the op completed")
	default:
	}
	if err := h.Err(); err != nil {
		t.Errorf("Err before completion = %v, want nil", err)
	}
	close(block)
	if err := h.Wait(); err != nil {
		t.Errorf("Wait = %v, want nil", err)
	}
}

// TestLen checks the queue-depth helper: it counts queued-but-unstarted ops,
// excludes the in-flight op, and returns 0 for an unknown key.
func TestLen(t *testing.T) {
	m := NewManager(context.Background())
	defer m.Stop()

	if got := m.Len("never-used"); got != 0 {
		t.Errorf("Len(unknown) = %d, want 0", got)
	}

	block := make(chan struct{})
	entered := make(chan struct{})
	// Head op occupies the single in-flight slot and blocks.
	if _, err := m.Enqueue("k", OpPrune, func(ctx context.Context) error {
		close(entered)
		<-block
		return nil
	}); err != nil {
		t.Fatalf("Enqueue head: %v", err)
	}
	<-entered

	// Queue three more behind it.
	for i := 0; i < 3; i++ {
		if _, err := m.Enqueue("k", OpCheck, func(ctx context.Context) error { return nil }); err != nil {
			t.Fatalf("Enqueue follower: %v", err)
		}
	}
	if got := m.Len("k"); got != 3 {
		t.Errorf("Len = %d, want 3 (in-flight op excluded)", got)
	}
	close(block)
}

// TestBlocksMovers pins which OpKinds require mover quiescence (the backup⇄unlock mutex). Only
// OpUnlock does in M1 — it force-removes every lock; adding OpPrune/OpErase later is a deliberate,
// test-guarded change.
func TestBlocksMovers(t *testing.T) {
	for _, k := range []OpKind{OpInit, OpForget, OpPrune, OpCheck, OpErase} {
		if blocksMovers(k) {
			t.Errorf("blocksMovers(%q) = true, want false", k)
		}
	}
	if !blocksMovers(OpUnlock) {
		t.Errorf("blocksMovers(OpUnlock) = false, want true")
	}
}

// TestQuiescenceRequired verifies the mover-admission signal: an OpUnlock counts for its whole
// pending+in-flight lifetime, a non-blocking op never counts, and the flag clears once the unlock
// resolves. The decrement happens in the worker goroutine just after resolution, so the post-run
// assertions poll briefly rather than reading immediately.
func TestQuiescenceRequired(t *testing.T) {
	m := NewManager(context.Background())
	defer m.Stop()

	if m.QuiescenceRequired("unknown") {
		t.Errorf("QuiescenceRequired(unknown) = true, want false")
	}

	// (1) In-flight: a blocking OpUnlock occupies the slot; the flag is set while it runs.
	block := make(chan struct{})
	entered := make(chan struct{})
	hUnlock, err := m.Enqueue("k1", OpUnlock, func(ctx context.Context) error {
		close(entered)
		<-block
		return nil
	})
	if err != nil {
		t.Fatalf("Enqueue in-flight unlock: %v", err)
	}
	<-entered
	if !m.QuiescenceRequired("k1") {
		t.Errorf("QuiescenceRequired while an OpUnlock is in-flight = false, want true")
	}
	close(block)
	if err := hUnlock.Wait(); err != nil {
		t.Fatalf("unlock op: %v", err)
	}
	if !eventually(func() bool { return !m.QuiescenceRequired("k1") }) {
		t.Errorf("QuiescenceRequired stayed true after the in-flight OpUnlock resolved")
	}

	// (2) Pending: a blocking non-unlock head occupies the slot, an OpUnlock queues behind it; the
	// flag is set while the unlock is merely PENDING (not yet in-flight), and clears once it drains.
	block2 := make(chan struct{})
	entered2 := make(chan struct{})
	if _, err := m.Enqueue("k2", OpForget, func(ctx context.Context) error {
		close(entered2)
		<-block2
		return nil
	}); err != nil {
		t.Fatalf("Enqueue forget head: %v", err)
	}
	<-entered2
	if m.QuiescenceRequired("k2") {
		t.Errorf("QuiescenceRequired with only a non-blocking OpForget in-flight = true, want false")
	}
	if _, err := m.Enqueue("k2", OpUnlock, func(ctx context.Context) error { return nil }); err != nil {
		t.Fatalf("Enqueue pending unlock: %v", err)
	}
	if !m.QuiescenceRequired("k2") {
		t.Errorf("QuiescenceRequired with an OpUnlock pending behind a live head = false, want true")
	}
	close(block2) // release the head; the unlock then runs and resolves
	if !eventually(func() bool { return !m.QuiescenceRequired("k2") }) {
		t.Errorf("QuiescenceRequired stayed true after the pending OpUnlock drained")
	}
}

// eventually polls cond for up to a couple of seconds, returning true as soon as it holds. It
// absorbs the small window between a Handle resolving and the worker goroutine running afterResolve.
func eventually(cond func() bool) bool {
	deadline := time.Now().Add(2 * time.Second)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}
