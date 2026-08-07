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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestMoverPodEverStarted is the unit test of the predicate the mover start-deadline rests on, and
// it is worth more than the deadline itself.
//
// moverStartDeadline is safe ONLY because this function is right. Get it wrong in the false
// direction and the operator becomes a wall-clock cap on backups: a 500 GB volume that has been
// uploading happily for two hours is reported as "never started" and killed, which is a far worse
// defect than the thirty-six-hour hang the deadline exists to close. Get it wrong in the true
// direction — the shape below that tempts most, reading pod.status.startTime — and the deadline is
// dead code that never fires, and the hang comes back with a timeout constant on top of it.
//
// So the cases are organised as two lists: everything that MUST count as started, and everything
// that must not.
func TestMoverPodEverStarted(t *testing.T) {
	// waiting builds the container status a pod wedged in ContainerCreating actually carries: a
	// Waiting state, no Running, no Terminated, and nothing in LastTerminationState.
	waiting := func(reason string) corev1.ContainerStatus {
		return corev1.ContainerStatus{
			Name:  "mover",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}},
		}
	}
	started := metav1.NewTime(time.Now().Add(-36 * time.Hour))

	tests := []struct {
		name string
		pods []corev1.Pod
		want bool
	}{
		// ── NOT started: the shapes the deadline must fire on ──────────────
		{
			// THE incident, exactly: the kubelet accepted the pod thirty-six hours ago, stamped
			// startTime, and has been failing to map the RBD clone ever since. Nothing here is a
			// start signal, and startTime being set is precisely the trap — reading it would make
			// this predicate always true.
			name: "wedged in ContainerCreating for 36h, with a startTime",
			pods: []corev1.Pod{{
				Status: corev1.PodStatus{
					Phase:             corev1.PodPending,
					StartTime:         &started,
					ContainerStatuses: []corev1.ContainerStatus{waiting("ContainerCreating")},
				},
			}},
			want: false,
		},
		{
			name: "pending with no container statuses at all — unschedulable",
			pods: []corev1.Pod{{Status: corev1.PodStatus{Phase: corev1.PodPending}}},
			want: false,
		},
		{
			// A Job half an hour old that has produced no pod is stuck by the same evidence: a
			// ResourceQuota denying it, an admission webhook refusing it, a broken Job controller.
			// The empty slice must NOT read as "started" simply because there is nothing to inspect.
			name: "no pods at all",
			pods: nil,
			want: false,
		},
		{
			name: "image pull backing off — waiting, never ran",
			pods: []corev1.Pod{{
				Status: corev1.PodStatus{
					Phase:             corev1.PodPending,
					ContainerStatuses: []corev1.ContainerStatus{waiting("ImagePullBackOff")},
				},
			}},
			want: false,
		},

		// ── Started: the shapes that must NEVER be failed by this deadline ──
		{
			// THE anti-regression, in unit form. Everything after this point is restic's business
			// and none of it is bounded.
			name: "running",
			pods: []corev1.Pod{{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{{
						Name:  "mover",
						State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: started}},
					}},
				},
			}},
			want: true,
		},
		{
			// The pod phase has not caught up yet but the container is demonstrably executing.
			// Trusting the phase alone would leave a real upload exposed for one reconcile.
			name: "phase still Pending but the container is Running",
			pods: []corev1.Pod{{
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
					ContainerStatuses: []corev1.ContainerStatus{{
						Name:  "mover",
						State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: started}},
					}},
				},
			}},
			want: true,
		},
		{
			name: "succeeded",
			pods: []corev1.Pod{{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}},
			want: true,
		},
		{
			// A pod that reached Failed belongs to the Job controller's backoffLimit accounting, and
			// advanceUploading's own Job-terminal branch reports it with the mover's reason. A
			// start-deadline that raced that would relabel a crash as "never started" — wrong, and
			// strictly less informative than the answer already on its way.
			name: "failed",
			pods: []corev1.Pod{{Status: corev1.PodStatus{Phase: corev1.PodFailed}}},
			want: true,
		},
		{
			// Waiting NOW, but it ran before: CrashLoopBackOff. The current state says nothing; the
			// previous termination is the proof.
			name: "crash-looping — waiting now, terminated before",
			pods: []corev1.Pod{{
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
					ContainerStatuses: []corev1.ContainerStatus{{
						Name:  "mover",
						State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
						LastTerminationState: corev1.ContainerState{
							Terminated: &corev1.ContainerStateTerminated{ExitCode: 1},
						},
					}},
				},
			}},
			want: true,
		},
		{
			// A retried Job: the first attempt ran and died, the replacement is still pulling. One
			// started pod anywhere in the set is enough — the volume has had its chance to move data
			// and the Job's own accounting owns what happens next.
			name: "one wedged pod beside one that ran",
			pods: []corev1.Pod{
				{Status: corev1.PodStatus{
					Phase:             corev1.PodPending,
					ContainerStatuses: []corev1.ContainerStatus{waiting("ContainerCreating")},
				}},
				{Status: corev1.PodStatus{Phase: corev1.PodFailed}},
			},
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := moverPodEverStarted(tc.pods); got != tc.want {
				t.Errorf("moverPodEverStarted() = %v, want %v.\n"+
					"    true  ⇒ the volume is left alone however long it takes (large backups depend on it)\n"+
					"    false ⇒ the volume is failed once moverStartDeadline passes", got, tc.want)
			}
		})
	}
}

// TestDeadlineReasonCarriesTheCause pins the composition rule for a VolumeStatus.reason: the bare
// deadline name when there is nothing to add, and "<name>: <cause>" when there is — the same shape
// podKillReason produces for MoverEvicted, so an operator reads one grammar and not two.
//
// The bare form is not cosmetic. backupReasonSnapshotProgressDeadline is asserted VERBATIM by both
// the envtest suite and test/crucible/tests/m6_precheck_test.go, so a composer that always appended
// something would break a cross-repo contract.
func TestDeadlineReasonCarriesTheCause(t *testing.T) {
	if got := deadlineReason(backupReasonMoverStartDeadline, ""); got != backupReasonMoverStartDeadline {
		t.Errorf("with no cause available the reason must stay bare, got %q", got)
	}

	const kubelet = "FailedMount: MountVolume.MountDevice failed ... rbd: map failed with error (exit status 22)"
	got := deadlineReason(backupReasonMoverStartDeadline, kubelet)
	if want := backupReasonMoverStartDeadline + ": " + kubelet; got != want {
		t.Errorf("deadlineReason = %q, want %q", got, want)
	}

	// Capped, because status is not a log. shortReason's bound is what stops a driver with a
	// verbose opinion from writing a kilobyte into every VolumeStatus in the namespace.
	long := deadlineReason(backupReasonMoverStartDeadline, strings.Repeat("x", 4096))
	if len(long) > 200 {
		t.Errorf("reason is %d chars; a status field must never carry an unbounded blob", len(long))
	}
	if !strings.HasPrefix(long, backupReasonMoverStartDeadline) {
		t.Errorf("truncation ate the reason name itself: %q", long)
	}
}

// TestEventObservedAtPrefersTheLastOccurrence is the small piece of arithmetic that decides WHICH
// warning an operator is shown.
//
// It matters because of the incident's own numbers: the kubelet published "x1069 over 36h". An
// aggregated Event's creationTimestamp is when it FIRST happened, and sorting on that would rank a
// thirty-six-hour-old first occurrence below any trivial warning recorded since — showing the
// operator the least relevant line available. The last occurrence is the one that describes the
// cluster now.
func TestEventObservedAtPrefersTheLastOccurrence(t *testing.T) {
	base := time.Now().Add(-36 * time.Hour)
	recent := time.Now().Add(-time.Minute)

	// The aggregated core/v1 shape: created 36h ago, last seen a minute ago.
	aggregated := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(base)},
		FirstTimestamp: metav1.NewTime(base),
		LastTimestamp:  metav1.NewTime(recent),
	}
	if got := eventObservedAt(aggregated); !got.Equal(recent) {
		t.Errorf("eventObservedAt = %s, want the LAST occurrence %s", got, recent)
	}

	// The events.k8s.io/v1 shape read back through the core view: no lastTimestamp, a series.
	series := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(base)},
		EventTime:  metav1.NewMicroTime(base),
		Series:     &corev1.EventSeries{Count: 1069, LastObservedTime: metav1.NewMicroTime(recent)},
	}
	if got := eventObservedAt(series); !got.Equal(recent) {
		t.Errorf("eventObservedAt = %s for a series Event, want %s", got, recent)
	}

	// Nothing but a creation stamp: the fallback must still produce a usable instant rather than
	// the zero time, or every such Event would sort last and be invisible.
	bare := &corev1.Event{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(base)}}
	if got := eventObservedAt(bare); got.IsZero() {
		t.Error("eventObservedAt returned the zero time for an Event carrying only a creationTimestamp")
	}
}
