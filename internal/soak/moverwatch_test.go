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
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

// fakeWatcher hands out watch.FakeWatcher instances and counts how many times each stream was
// opened, so a test can assert the reconnect actually happened rather than assuming it.
type fakeWatcher struct {
	mu       sync.Mutex
	pods     []*watch.FakeWatcher
	jobs     []*watch.FakeWatcher
	podErr   error
	jobErr   error
	podOpens int
	jobOpens int
}

func (f *fakeWatcher) WatchPods(_ context.Context, _, _ string) (watch.Interface, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.podOpens++
	if f.podErr != nil {
		return nil, f.podErr
	}
	w := watch.NewFake()
	f.pods = append(f.pods, w)
	return w, nil
}

func (f *fakeWatcher) WatchJobs(_ context.Context, _, _ string) (watch.Interface, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobOpens++
	if f.jobErr != nil {
		return nil, f.jobErr
	}
	w := watch.NewFake()
	f.jobs = append(f.jobs, w)
	return w, nil
}

// nthPod / nthJob block until the watcher has handed out at least n streams, then return the nth.
// Without this a test races the goroutine that opens the watch.
func (f *fakeWatcher) nthPod(t *testing.T, n int) *watch.FakeWatcher {
	t.Helper()
	return waitFor(t, func() *watch.FakeWatcher {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.pods) < n {
			return nil
		}
		return f.pods[n-1]
	}, "pod watch #%d was never opened", n)
}

func (f *fakeWatcher) nthJob(t *testing.T, n int) *watch.FakeWatcher {
	t.Helper()
	return waitFor(t, func() *watch.FakeWatcher {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.jobs) < n {
			return nil
		}
		return f.jobs[n-1]
	}, "job watch #%d was never opened", n)
}

func (f *fakeWatcher) opens() (podOpens, jobOpens int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.podOpens, f.jobOpens
}

// waitFor polls a condition to a deadline. Every wait in this file is bounded and fails with a
// sentence rather than a timeout panic, because a hung watch test is otherwise indistinguishable
// from a slow machine.
//
// The deadline is generous on purpose. These tests are cross-goroutine, and `make test` runs
// thirty-odd packages at once — a 3s deadline passed in isolation and failed under the full suite,
// which is a flake, not a finding. Polling at 1ms means a genuinely broken watch still fails in
// milliseconds; only the failing path waits.
func waitFor[T comparable](t *testing.T, f func() T, format string, args ...any) T {
	t.Helper()
	var zero T
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if v := f(); v != zero {
			return v
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf(format, args...)
	return zero
}

// deliverMover sends one mover's Job and then its pod, and — this is the point — does not send
// the pod until the Job's event has actually been APPLIED.
//
// The two watches are independent goroutines with no ordering between them. A pod whose Job the
// collector has not seen yet is filed as class `unknown` and only repaired by a LATER event for
// that same pod; a test that raced the two would attribute the mover to `unknown` about as often
// as not. In a real cluster the Job is created before its pod exists, so waiting here is the
// faithful ordering rather than a convenience.
func deliverMover(t *testing.T, h *HighWater, fw *fakeWatcher, gen int, job, pod string, at time.Time) {
	t.Helper()
	j := moverJob(job, moverOpBackup, at, time.Time{})
	fw.nthJob(t, gen).Add(&j)

	waitFor(t, func() bool {
		return h.Marks(at.Add(time.Minute)).Classes["data"].JobsObserved > 0
	}, "the Job event was never applied, so its pod could only be filed as class unknown")

	p := terminatedWith(moverPod(pod, job, "worker-1", at), moverResultFixture())
	fw.nthPod(t, gen).Add(&p)
}

// startWatch runs a MoverWatch against a HighWater fed by nothing else — no poll, no lister
// contents — so anything that lands in the table can only have come from an event.
func startWatch(t *testing.T, w moverEventWatcher) (*HighWater, *fakeWatcher) {
	t.Helper()
	store := newTestStore(t, 1<<20)
	h := NewHighWater(store, &fakeLister{}, testNS, 15*time.Second, false)

	m := NewMoverWatch(h, store, w)
	if m == nil {
		t.Fatal("NewMoverWatch returned nil for a non-nil watcher")
	}
	m.retryDelay = time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("MoverWatch.Run did not return after its context was cancelled — the " +
				"collector would not shut down cleanly")
		}
	})
	fw, _ := w.(*fakeWatcher)
	return h, fw
}

// TestAMoverTooShortToPollIsStillMeasured is the defect, restated as a test.
//
// The lister is EMPTY: every poll this HighWater could ever do returns nothing, which is exactly
// what the crucible produced — a mover Job visible for 9.6s against a 15s sample interval, and a
// `data` class reported NOT_MEASURED through a campaign that ran dozens of backups. The pod here
// is only ever delivered as an event, and its exact peak must still land.
func TestAMoverTooShortToPollIsStillMeasured(t *testing.T) {
	h, fw := startWatch(t, &fakeWatcher{})

	start := day0.Add(time.Hour)
	deliverMover(t, h, fw, 1, "job-backup", "job-backup-1", start)

	// And DELETED right after, with nothing in between: the whole life of a mover the poll never
	// saw. The peak must survive the pod's disappearance, because that is the normal case — the
	// controller deletes the Job the moment it has read the result.
	pod := terminatedWith(moverPod("job-backup-1", "job-backup", "worker-1", start), moverResultFixture())
	fw.nthPod(t, 1).Delete(&pod)

	waitFor(t, func() bool {
		return h.Marks(start.Add(time.Minute)).Classes["data"].ReportedPeakRSSBytes == 412<<20
	}, "the data class never got the mover's peak from the watch: %+v",
		h.Marks(start.Add(time.Minute)).Classes["data"])

	data := h.Marks(start.Add(time.Minute)).Classes["data"]
	if data.Memory.Status != statusOK {
		t.Errorf("data memory status = %q (%s), want %q — a peak arrived but the class still "+
			"reads as unmeasured", data.Memory.Status, data.Memory.Reason, statusOK)
	}
	if data.Memory.Source != sourceMover {
		t.Errorf("memory source = %q, want %q", data.Memory.Source, sourceMover)
	}
}

// TestTheWatchReconnectsWhenTheAPIServerClosesIt pins the property that makes a fortnight-long
// watch usable at all. The API server ends a watch routinely — its own timeout, a rollout, a
// resourceVersion aging out — and a collector that treated the first close as the end would
// measure day one and nothing after it. That is the failure mode the RBAC comment originally
// cited to refuse `watch` altogether, so it has to be tested, not asserted.
func TestTheWatchReconnectsWhenTheAPIServerClosesIt(t *testing.T) {
	h, fw := startWatch(t, &fakeWatcher{})

	fw.nthPod(t, 1).Stop() // the API server hanging up
	fw.nthJob(t, 1).Stop()

	// Second generation of both streams, and a mover that only exists after the reconnect.
	start := day0.Add(2 * time.Hour)
	deliverMover(t, h, fw, 2, "job-late", "job-late-1", start)

	waitFor(t, func() bool {
		return h.Marks(start.Add(time.Minute)).Classes["data"].ReportedPeakRSSBytes == 412<<20
	}, "nothing was measured after the watch was re-established — the collector would go silent "+
		"the first time the API server closed its watch, which is a routine event")
}

// TestAWatchThatCannotBeOpenedIsRecordedAndRetried covers the degraded path: no RBAC, an API
// server refusing the verb. It must not spin silently, and it must not give up — the poll keeps
// working meanwhile, so the collector degrades to the previous release's behaviour rather than
// stopping.
func TestAWatchThatCannotBeOpenedIsRecordedAndRetried(t *testing.T) {
	w := &fakeWatcher{
		podErr: errors.New(`pods is forbidden: cannot watch resource "pods"`),
		jobErr: errors.New(`jobs.batch is forbidden: cannot watch resource "jobs"`),
	}

	h, _ := startWatch(t, w)

	waitFor(t, func() bool {
		p, j := w.opens()
		return p >= 20 && j >= 20
	}, "the watch gave up after a failure to open; it must keep retrying, because the grant may "+
		"be added while the collector is running")

	// AND ~40 failed opens must COALESCE. Store.RecordError keys by message, so a stable sentence
	// becomes one record with a count; a message carrying an attempt number or a timestamp would
	// be unique every time and a fortnight of a refused watch would be several hundred thousand
	// entries evicting the measurements this collector exists to keep. Two records — one per
	// watched kind — is the whole expected output.
	errs := h.store.ErrorsFor(StreamHighwater)
	if len(errs) == 0 {
		t.Fatal("a watch that cannot be established recorded nothing at all; the collector would " +
			"silently fall back to polling and the archive would look complete")
	}
	if len(errs) > 2 {
		t.Errorf("%d error records for ~40 failed opens across 2 kinds, want 2. The message is not "+
			"stable, so Store.RecordError cannot coalesce it and the volume fills with the "+
			"collector complaining about itself.\nfirst: %s", len(errs), errs[0].Message)
	}
	var total int
	for _, e := range errs {
		total += e.Count
	}
	if total < 20 {
		t.Errorf("the coalesced records account for only %d failures; the count is what tells a "+
			"reader this was permanent rather than a blip", total)
	}
	// The record has to name the consequence, not just the error: a reader finding this in the
	// archive needs to know the mover figures are poll-quality.
	if !strings.Contains(errs[0].Message, "poll") {
		t.Errorf("the recorded error does not say what the failure COSTS: %q", errs[0].Message)
	}
}

// TestNonMoverWorkloadsAreNotMeasuredAsMovers is the counterweight to widening the selector.
//
// The collector now lists and watches on app.kubernetes.io/managed-by=crystal-backup, which is
// deliberately wider than "movers" — it reaches hook runners and the crucible's restic oracle. A
// Job that runs no --operation is not a mover, and letting one in would put a non-mover's memory
// into a table whose only purpose is sizing movers.
func TestNonMoverWorkloadsAreNotMeasuredAsMovers(t *testing.T) {
	h, fw := startWatch(t, &fakeWatcher{})

	start := day0.Add(3 * time.Hour)
	oracle := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "restic-oracle", Namespace: testNS,
			Labels: map[string]string{moverManagedByLabel: moverManagedByValue},
		},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Args: []string{"restic", "version"}}}},
		}},
	}
	fw.nthJob(t, 1).Add(&oracle)

	oraclePod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "restic-oracle-1", Namespace: testNS,
			Labels: map[string]string{moverManagedByLabel: moverManagedByValue, "job-name": "restic-oracle"},
		},
		Status: corev1.PodStatus{StartTime: ptrTime(start)},
	}
	fw.nthPod(t, 1).Add(&oraclePod)

	// A real mover after it, so this test cannot pass by the watch being wired to nothing.
	deliverMover(t, h, fw, 1, "job-real", "job-real-1", start)

	waitFor(t, func() bool {
		return h.Marks(start.Add(time.Minute)).Classes["data"].ReportedPeakRSSBytes == 412<<20
	}, "the real mover never landed — this test would pass vacuously")

	marks := h.Marks(start.Add(time.Minute))
	for _, p := range marks.Pods {
		if p.Pod == "restic-oracle-1" {
			t.Errorf("a non-mover pod was recorded in the mover high-water table as class %q; "+
				"its memory is now mixed into a sizing decision it has nothing to do with", p.Class)
		}
	}
	for _, j := range marks.Jobs {
		if j.Job == "restic-oracle" {
			t.Errorf("a Job that runs no mover operation was recorded as a mover (class %q)", j.Class)
		}
	}
}

// TestAWatchErrorEventIsNotAnObject: watch.Error carries a metav1.Status, not the resource type.
// Applying it as one is a panic in a resident process that is supposed to survive fourteen days.
func TestAWatchErrorEventIsNotAnObject(t *testing.T) {
	h, fw := startWatch(t, &fakeWatcher{})

	fw.nthPod(t, 1).Error(&metav1.Status{Reason: metav1.StatusReasonExpired})
	fw.nthJob(t, 1).Error(&metav1.Status{Reason: metav1.StatusReasonExpired})

	// Still alive and still measuring afterwards.
	start := day0.Add(4 * time.Hour)
	deliverMover(t, h, fw, 1, "job-after-error", "job-after-error-1", start)

	waitFor(t, func() bool {
		return h.Marks(start.Add(time.Minute)).Classes["data"].ReportedPeakRSSBytes == 412<<20
	}, "the watch stopped measuring after an Error event")
}

// TestNilWatcherLeavesTheCollectorPollingOnly: the watch is an addition, not a dependency. A
// MoverWatch that cannot be built must be a no-op rather than a nil-pointer panic at startup.
func TestNilWatcherLeavesTheCollectorPollingOnly(t *testing.T) {
	if m := NewMoverWatch(nil, nil, nil); m != nil {
		t.Fatalf("NewMoverWatch(nil, nil, nil) = %+v, want nil", m)
	}
	var m *MoverWatch
	m.Run(t.Context()) // must not panic
}

// moverOpBackup and moverResultFixture keep the tests above reading as scenarios rather than as
// struct literals. The peak is the same 412Mi the poll-side tests use, so a reader comparing the
// two paths is comparing the same number.
const moverOpBackup = mover.OpBackup

func moverResultFixture() mover.MoverResult {
	return mover.MoverResult{
		OK: true, Operation: string(mover.OpBackup), SnapshotID: "abc",
		PeakRSSBytes: 412 << 20, ShimPeakRSSBytes: 11 << 20, CgroupPeakBytes: 3900 << 20,
		MemorySource: mover.MemorySourceBoth,
	}
}
