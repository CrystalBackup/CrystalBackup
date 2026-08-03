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

package soak

import (
	"context"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// ---------------------------------------------------------------------------------------------
// The event half of the high-water stream.
//
// What this exists to fix, stated as the measurement that was wrong: a four-hour collector run on
// the crucible, alongside a campaign that executed dozens of backups, reported sizing classes
// `data` and `manifests` as NOT_MEASURED — "no data mover pod ran while the collector was up".
// Two causes, both real, and fixing either one alone still leaves a broken instrument:
//
//  1. Six of the ten mover creation sites never stamped app.kubernetes.io/name=crystal-mover, and
//     the collector selected on it. Fixed in mover.BuildJob, which now stamps it itself.
//  2. Even correctly labelled, a mover is not there long enough to be polled. Measured at 0.25s
//     resolution on the crucible: the four Jobs of one ClusterBackup were visible for 9.6s, 11.0s,
//     16.3s and 23.7s, because the controller deletes each mover Job on the same reconcile pass
//     that reads its result. Against a 15s poll that is a coin toss, biased against exactly the
//     short movers — and a soak whose headline number is "the peak we observed" must not quietly
//     be "the peak among the ones we happened to catch".
//
// So the exact figures come from a watch and the sampled ones stay on the poll. A watch delivers
// the MODIFIED event the moment the kubelet writes the terminated container status — which is
// where the mover's own peak RSS lives, in its termination message — and the DELETED event
// carries the last state too. Nothing is missed for being brief.
//
// On threading: Collector is single-goroutine by design and says why. This is the deliberate
// exception, and it is bounded — the watcher goroutines do API I/O and then call the same
// observeJobs/observePods the poll calls, under HighWater's own mutex. They never touch the Store
// except through RecordError, which takes the Store's mutex. No new lock ordering, no new
// invariant: the watch is a second source for one existing table, not a second table.
// ---------------------------------------------------------------------------------------------

// watchRetryDelay is the pause after a watch ends before it is re-established.
//
// Short, because the gap is a blind spot: the poll's periodic re-list repairs whatever is still
// present when it next runs, but a mover that both started and finished inside the gap is gone
// for good. Two seconds over a fortnight is a rounding error in API load and keeps the blind spot
// smaller than the shortest mover measured.
const watchRetryDelay = 2 * time.Second

// MoverWatch feeds the high-water table from watch events.
type MoverWatch struct {
	hw      *HighWater
	watcher moverEventWatcher
	store   *Store

	// retryDelay is watchRetryDelay in production and near-zero in tests. A test that had to
	// sleep two seconds per reconnect would be a test nobody runs.
	retryDelay time.Duration
}

// NewMoverWatch wires the watcher to the sampler it feeds. A nil watcher yields a nil MoverWatch,
// whose Run is a no-op — the collector then measures exactly what polling can see, which is what
// the previous release did.
func NewMoverWatch(hw *HighWater, store *Store, watcher moverEventWatcher) *MoverWatch {
	if hw == nil || watcher == nil {
		return nil
	}
	return &MoverWatch{hw: hw, watcher: watcher, store: store, retryDelay: watchRetryDelay}
}

// Run watches Jobs and pods until ctx is cancelled. It never returns an error: a watch that
// cannot be established is a recorded degradation, not a reason to take the collector down —
// everything else it gathers over a fortnight is still worth having.
func (m *MoverWatch) Run(ctx context.Context) {
	if m == nil {
		return
	}
	selector := moverManagedByLabel + "=" + moverManagedByValue

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		m.loop(ctx, "Jobs", selector, m.watcher.WatchJobs, m.applyJob)
	}()
	go func() {
		defer wg.Done()
		m.loop(ctx, "pods", selector, m.watcher.WatchPods, m.applyPod)
	}()
	wg.Wait()
}

// loop keeps one watch alive for the life of ctx.
//
// A watch ALWAYS ends: the API server closes it on its own timeout, on a resourceVersion going
// too old, on an apiserver rollout. That is routine, so a closed channel is not an error and is
// not recorded as one — recording it would fill a fortnight's error log with the API server
// behaving normally, which is how a real error becomes invisible.
func (m *MoverWatch) loop(
	ctx context.Context,
	kind, selector string,
	open func(context.Context, string, string) (watch.Interface, error),
	apply func(any),
) {
	for ctx.Err() == nil {
		w, err := open(ctx, m.hw.Namespace, selector)
		if err != nil {
			// The message is deliberately STABLE — no attempt counter, no timestamp, nothing that
			// varies between one failure and the next. Store.RecordError coalesces by message and
			// keeps a count with a first and last timestamp, so a watch that has been refused
			// every two seconds for eleven days is ONE record that says so, rather than half a
			// million identical lines evicting the measurements this collector exists to keep.
			// Interpolating the attempt number here would make every message unique and defeat
			// exactly that.
			//
			// It names the CONSEQUENCE and not just the error, because a reader finding this in
			// the archive needs to know what it cost them: the figures fell back to poll quality,
			// which misses most movers.
			m.store.RecordError(StreamHighwater,
				"watch mover "+kind+" could not be established: "+err.Error()+
					". The high-water figures fall back to what the poll catches, which misses "+
					"most movers — they live ten to twenty seconds. Check the collector's RBAC "+
					"for `watch` on pods and batch/jobs",
				time.Now().UTC())
			if !sleepCtx(ctx, m.retryDelay) {
				return
			}
			continue
		}
		m.drain(ctx, w, apply)
		w.Stop()
		if !sleepCtx(ctx, m.retryDelay) {
			return
		}
	}
}

// drain consumes one watch until it closes or ctx ends.
func (m *MoverWatch) drain(ctx context.Context, w watch.Interface, apply func(any)) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.ResultChan():
			if !ok {
				return
			}
			// watch.Error carries a Status, not the object type: skip rather than mis-apply.
			// ADDED, MODIFIED, DELETED and BOOKMARK all reach apply, which type-asserts.
			if ev.Type == watch.Error {
				continue
			}
			apply(ev.Object)
		}
	}
}

// applyJob and applyPod funnel a single watched object through the SAME code the poll uses.
//
// Deliberately not a parallel implementation. The mover filter, the class attribution, the
// termination-message read and the unknown-class repair all live in observeJobs/observePods, and
// a second copy of that logic reachable only by the watch would be a second thing to get right
// and a second thing to keep in step. A one-element list is a cheap price for one code path.
func (m *MoverWatch) applyJob(obj any) {
	job, ok := obj.(*batchv1.Job)
	if !ok {
		return
	}
	m.hw.observeJobs(&batchv1.JobList{Items: []batchv1.Job{*job}})
}

func (m *MoverWatch) applyPod(obj any) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	// The event's own arrival time, not a tick boundary. It is what the pod's lifetime is
	// measured against, and it is strictly more accurate than the poll's — the poll can only say
	// "some time in the last interval".
	m.hw.observePods(&corev1.PodList{Items: []corev1.Pod{*pod}}, time.Now().UTC())
}

// sleepCtx sleeps unless ctx ends first. It reports whether the sleep completed, so a caller can
// tell "carry on" from "shut down" without re-checking ctx.Err() and racing it.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
