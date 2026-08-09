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
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/hooks"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// hookCall is one recorded exec, so a spec can assert WHICH pod and container were touched — the
// namespace-confinement invariant is only meaningful if it is observable.
type hookCall struct {
	Pod       types.NamespacedName
	Container string
	Command   []string
	// ServiceAccountName is the identity the exec was made AS — empty meaning "as the operator".
	// Recorded because it is the whole point of the hook-impersonation design: a spec asserting
	// only that a command ran would pass just as happily if it ran with the operator's own rights.
	ServiceAccountName string
}

// stubHookExecutor replaces pods/exec in envtest, which has no kubelet. It is mutex-guarded
// because the manager reconciles on another goroutine while a spec reads the calls.
type stubHookExecutor struct {
	mu    sync.Mutex
	calls []hookCall
	// failPod makes that pod's hook fail, so a spec can drive the onError policies.
	failPod map[string]error
	// failCommand makes the hook whose argv[0] matches fail, keyed by that first word.
	//
	// It exists because failPod cannot express the case the pre/post asymmetry is made of: a run
	// whose QUIESCE succeeded and whose RELEASE failed, in the same pod — which is every real
	// unfreeze problem there is. Keyed on the command rather than the phase because that is what
	// the executor can see, and because it reads like the workload it stands in for ("thaw" is
	// broken, "freeze" is not).
	failCommand map[string]error
}

func (s *stubHookExecutor) Exec(_ context.Context, pod types.NamespacedName, container string, command []string,
	serviceAccountName string,
) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, hookCall{
		Pod: pod, Container: container, Command: command, ServiceAccountName: serviceAccountName,
	})
	if err, ok := s.failPod[pod.Name]; ok {
		return "", "stub failure", err
	}
	if len(command) > 0 {
		if err, ok := s.failCommand[command[0]]; ok {
			return "", "stub failure", err
		}
	}
	return "", "", nil
}

func (s *stubHookExecutor) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = nil
	s.failPod = nil
	s.failCommand = nil
}

func (s *stubHookExecutor) recorded() []hookCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]hookCall(nil), s.calls...)
}

// TestSnapshotsCutIsUnconditional is the guard for the freeze window's most important property.
//
// The release fires on the volumes having LEFT the snapshot phase, not on their having succeeded.
// A snapshot that failed still leaves an application quiesced, so a release conditioned on success
// would strand exactly the workload whose backup just went wrong — the case where an operator is
// least likely to be watching.
func TestSnapshotsCutIsUnconditional(t *testing.T) {
	cases := []struct {
		name    string
		phases  []status.VolumePhase
		wantCut bool
	}{
		{"nothing started", []status.VolumePhase{status.VolumePhasePending}, false},
		{"still snapshotting", []status.VolumePhase{status.VolumePhaseCompleted, status.VolumePhaseSnapshotting}, false},
		{"uploading: the snapshots exist", []status.VolumePhase{status.VolumePhaseUploading}, true},
		{"all completed", []status.VolumePhase{status.VolumePhaseCompleted, status.VolumePhaseCompleted}, true},
		// The three that matter: a failed, skipped or mixed outcome must still release.
		{"failed", []status.VolumePhase{status.VolumePhaseFailed}, true},
		{"skipped", []status.VolumePhase{status.VolumePhaseSkipped}, true},
		{"one failed, one done", []status.VolumePhase{status.VolumePhaseFailed, status.VolumePhaseCompleted}, true},
		{"no volumes at all", nil, true},
	}
	for _, tc := range cases {
		vols := make([]cbv1.VolumeStatus, 0, len(tc.phases))
		for i, p := range tc.phases {
			vols = append(vols, cbv1.VolumeStatus{Pvc: string(rune('a' + i)), Phase: p})
		}
		if got := snapshotsCut(vols); got != tc.wantCut {
			t.Errorf("%s: snapshotsCut = %v, want %v", tc.name, got, tc.wantCut)
		}
	}
}

// TestHookStateSurvivesARestart: the controller keeps NO in-memory record of the freeze window, so
// a process that dies between the quiesce and the release must rebuild both facts from status.
// This is the mechanism behind the roadmap's "controller crash between pre and post hook →
// unfreeze still happens", which it calls the feature's most important test.
func TestHookStateSurvivesARestart(t *testing.T) {
	backup := &cbv1.Backup{}
	if st := hookState(backup); st.preRan || st.aborted {
		t.Fatalf("a fresh Backup reported %+v, want no hooks run", st)
	}

	backup.Status.Hooks = []cbv1.HookStatus{
		{Phase: string(hooks.PhasePre), Pod: "db-0", Result: cbv1.HookSucceeded},
	}
	st := hookState(backup)
	if !st.preRan || st.aborted {
		t.Fatalf("after a successful pre hook: %+v, want preRan without abort", st)
	}
	// And the release is still owed — which is what makes the crash case work.
	if postHooksRan(backup) {
		t.Error("postHooksRan = true with no post record; a restarted controller would skip the unfreeze")
	}

	backup.Status.Hooks = append(backup.Status.Hooks,
		cbv1.HookStatus{Phase: string(hooks.PhasePost), Pod: "db-0", Result: cbv1.HookSucceeded})
	if !postHooksRan(backup) {
		t.Error("postHooksRan = false after a successful release")
	}
}

// TestHookStateAbortsOnAFailedPreHook: a failed quiesce must abort. A snapshot taken anyway would
// look application-consistent while not being so — discovered only at restore time, which is the
// one outcome worse than having no backup.
func TestHookStateAbortsOnAFailedPreHook(t *testing.T) {
	backup := &cbv1.Backup{Status: cbv1.BackupStatus{Hooks: []cbv1.HookStatus{
		{Phase: string(hooks.PhasePre), Pod: "db-0", Result: cbv1.HookFailed, Message: "exit status 1"},
	}}}
	st := hookState(backup)
	if !st.aborted {
		t.Fatal("a failed pre hook did not abort")
	}
	if st.abortMessage == "" {
		t.Error("the abort carries no message; the terminal condition would say nothing")
	}
}

// TestPostHooksRanRetriesUntilTheBudget: a failed release is retried, because the outstanding
// problem is an application that may still be quiesced. It stops at the budget so a permanently
// broken hook does not retry into the next backup window.
func TestPostHooksRanRetriesUntilTheBudget(t *testing.T) {
	backup := &cbv1.Backup{Status: cbv1.BackupStatus{
		PostHookAttempts: 1,
		Hooks: []cbv1.HookStatus{
			{Phase: string(hooks.PhasePost), Pod: "db-0", Result: cbv1.HookFailed},
		},
	}}
	if postHooksRan(backup) {
		t.Error("a failed release with attempts left must be retried")
	}

	backup.Status.PostHookAttempts = postHookMaxAttempts
	if !postHooksRan(backup) {
		t.Error("the release must stop retrying once the budget is spent")
	}
}

// TestPodMountsAny is the pod-selection rule, and it is a boundary as much as a filter: a pod in
// the namespace that holds none of the backed-up data is not consistency-relevant and must never
// be exec'd into.
func TestPodMountsAny(t *testing.T) {
	claims := map[string]struct{}{"data": {}, "wal": {}}
	mounting := &corev1.Pod{Spec: corev1.PodSpec{Volumes: []corev1.Volume{
		{Name: "cfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}},
		{Name: "d", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "wal"}}},
	}}}
	unrelated := &corev1.Pod{Spec: corev1.PodSpec{Volumes: []corev1.Volume{
		{Name: "other", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "somebody-elses"}}},
	}}}
	noVolumes := &corev1.Pod{}

	if !podMountsAny(mounting, claims) {
		t.Error("a pod mounting a backed-up PVC must be a hook candidate")
	}
	if podMountsAny(unrelated, claims) {
		t.Error("a pod mounting an unrelated PVC must NOT be a hook candidate")
	}
	if podMountsAny(noVolumes, claims) {
		t.Error("a pod with no volumes must NOT be a hook candidate")
	}
}

// TestDropHookPhaseReplacesRatherThanAccumulates: a retried release replaces its record, so the
// list answers "is it released now" instead of growing one entry per attempt.
func TestDropHookPhaseReplacesRatherThanAccumulates(t *testing.T) {
	entries := []cbv1.HookStatus{
		{Phase: string(hooks.PhasePre), Pod: "db-0"},
		{Phase: string(hooks.PhasePost), Pod: "db-0"},
		{Phase: string(hooks.PhasePost), Pod: "db-1"},
	}
	got := dropHookPhase(entries, hooks.PhasePost)
	if len(got) != 1 || got[0].Phase != string(hooks.PhasePre) {
		t.Fatalf("dropHookPhase = %+v, want only the pre entry", got)
	}
}

// noopRecorder swallows Events so a pure-logic test needs no manager.
type noopRecorder struct{}

func (noopRecorder) Eventf(_ runtime.Object, _ runtime.Object, _, _, _, _ string, _ ...any) {}

// TestRecordHookResultsMarksSkippedDistinctly: "did not run" and "ran and passed" must not look
// the same in status, or an operator reading a partial list assumes the rest succeeded.
func TestRecordHookResultsMarksSkippedDistinctly(t *testing.T) {
	r := &BackupReconciler{Recorder: noopRecorder{}}
	backup := &cbv1.Backup{ObjectMeta: metav1.ObjectMeta{Namespace: "team-x", Name: "run"}}

	base := hooks.Resolved{Phase: hooks.PhasePre, Container: "db", Source: hooks.SourceSpec}
	ok := base
	ok.Pod = types.NamespacedName{Name: "db-0"}
	bad := base
	bad.Pod = types.NamespacedName{Name: "db-1"}
	skipped := base
	skipped.Pod = types.NamespacedName{Name: "db-2"}

	r.recordHookResults(backup, []hooks.Result{
		{Hook: ok},
		{Hook: bad, Err: context.DeadlineExceeded},
		{Hook: skipped, Err: hooks.ErrSkipped},
	})

	if len(backup.Status.Hooks) != 3 {
		t.Fatalf("recorded %d entries for 3 results", len(backup.Status.Hooks))
	}
	want := []cbv1.HookResult{cbv1.HookSucceeded, cbv1.HookFailed, cbv1.HookSkipped}
	for i, w := range want {
		if backup.Status.Hooks[i].Result != w {
			t.Errorf("entry %d result = %q, want %q", i, backup.Status.Hooks[i].Result, w)
		}
	}
}
