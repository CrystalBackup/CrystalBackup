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
	"fmt"
	"sync"
)

// Handle is the caller's await point for one enqueued operation. It is returned
// by [Manager.Enqueue] and resolved exactly once when the operation finishes,
// panics, or is cancelled by shutdown.
//
// The design deliberately separates completion signalling ([Handle.Done], a
// channel) from the result ([Handle.Err]): a controller can select on Done
// alongside its own reconcile-timeout timer and its request context, so awaiting
// an exclusive repository operation never blocks the reconcile goroutine
// indefinitely. Callers that can afford to block may use [Handle.Wait] instead.
type Handle struct {
	kind OpKind

	// run is the operation body. It is written once in newHandle (before the
	// Handle is published to a worker via the queue) and read/cleared only by the
	// owning worker goroutine in takeRun, so it needs no lock of its own: the
	// worker-queue mutex that hands the Handle over establishes the
	// happens-before edge.
	run func(context.Context) error

	// once guards the single resolution (and thus the single close of done),
	// making a Handle robust against a double-resolve logic bug (e.g. a panic
	// path racing the normal path), which would otherwise panic on a second
	// close.
	once sync.Once
	done chan struct{}

	// mu guards err so that Err may be called at any time — including before the
	// operation completes — without a data race against resolve. Before
	// resolution Err reports nil.
	mu  sync.Mutex
	err error
}

func newHandle(kind OpKind, run func(context.Context) error) *Handle {
	return &Handle{
		kind: kind,
		run:  run,
		done: make(chan struct{}),
	}
}

// Kind reports the [OpKind] this Handle was enqueued with.
func (h *Handle) Kind() OpKind { return h.kind }

// Done returns a channel closed when the operation has been resolved (completed,
// panicked, or cancelled by shutdown). After it is closed, [Handle.Err] reports
// the final outcome. Select on it to await the operation with your own timeout:
//
//	select {
//	case <-h.Done():
//	        return h.Err()
//	case <-ctx.Done():
//	        return ctx.Err() // stop waiting; the operation keeps its exclusive slot
//	}
func (h *Handle) Done() <-chan struct{} { return h.done }

// Err returns the operation's result. It is meaningful only after [Handle.Done]
// is closed; called earlier it returns nil (indistinguishable from a successful
// completion, hence the Done gate). The value is nil on success, the error
// returned by the run func, a [*PanicError] if the run func panicked, or the
// base context's cancellation error if the Manager stopped before the operation
// ran.
func (h *Handle) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

// Wait blocks until the operation is resolved and returns its result. It is a
// convenience for tests and simple callers; controllers should prefer selecting
// on [Handle.Done] so a slow operation cannot stall the reconcile goroutine.
func (h *Handle) Wait() error {
	<-h.done
	return h.Err()
}

// resolve records err as the outcome and closes done, exactly once. The write to
// err is published to Err readers both through mu (for reads before done closes)
// and through the close of done (for reads after), so it is race-free under both
// orderings.
func (h *Handle) resolve(err error) {
	h.once.Do(func() {
		h.mu.Lock()
		h.err = err
		h.mu.Unlock()
		close(h.done)
	})
}

// takeRun returns the operation body and clears it so the closure — and anything
// it captured — can be garbage collected once it has run. Only the owning worker
// goroutine calls it, and only once, so no synchronisation is required.
func (h *Handle) takeRun() func(context.Context) error {
	run := h.run
	h.run = nil
	return run
}

// PanicError is the error a [Handle] carries when its operation's run func
// panicked. Recovering the panic in the worker keeps one buggy operation from
// unwinding the shared consumer goroutine or crashing the operator; the recovered
// value and the stack captured at recovery time are preserved here so the calling
// controller can log a faithful diagnostic. Detect it with errors.As:
//
//	var pe *queue.PanicError
//	if errors.As(h.Err(), &pe) { log.Error(pe, "repo op panicked", "stack", string(pe.Stack)) }
type PanicError struct {
	// Value is the value passed to panic.
	Value any
	// Stack is the goroutine stack captured at the point of recovery.
	Stack []byte
}

// Error implements error.
func (e *PanicError) Error() string {
	return fmt.Sprintf("repo/queue: operation run func panicked: %v", e.Value)
}
