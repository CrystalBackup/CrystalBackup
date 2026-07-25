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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/hooks"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// This file is the Backup controller's CONSISTENCY-HOOK phase (R16): the pre-snapshot quiesce and
// the post-snapshot release that bound the freeze window.
//
// The window is bounded by the SNAPSHOT phase, not the upload (01-architecture.md §5). A snapshot
// is a point in time that takes seconds to cut; the upload that follows can take hours. Holding a
// database frozen for the upload would turn every backup into an outage, so the post hooks fire as
// soon as the last snapshot exists — while the movers are still reading from it.
//
// Everything here treats the release as more important than the backup. A failed PRE hook aborts:
// the quiesce did not happen, so a snapshot taken anyway would look application-consistent while
// not being so, which is worse than no snapshot. A failed POST hook does NOT abort — the data is
// already captured, and the outstanding problem is an application that may still be quiesced. It
// retries, and when it runs out of retries it says so loudly, because at that point a human has to
// go and unfreeze something by hand.

const (
	// postHookMaxAttempts bounds the unfreeze retries. Three is enough to ride out a container
	// restart or an API blip and few enough that a genuinely broken release surfaces in minutes
	// rather than being retried into the next backup window.
	postHookMaxAttempts = 3

	// hookPhaseBudget caps the WHOLE hook phase, over and above each hook's own timeout. Hooks run
	// inline on the reconcile worker — they are short, bounded and once per backup, unlike the
	// repository I/O M3.1 moved off the worker — but a namespace with many annotated pods could
	// still add up, so the phase as a whole has a ceiling.
	hookPhaseBudget = 5 * time.Minute

	// hookMessageLimit truncates a recorded failure to the CRD's MaxLength.
	hookMessageLimit = 1024
)

// hookPhaseState is what Reconcile needs to know about the freeze window this pass.
type hookPhaseState struct {
	// preRan is true once the pre phase has been executed and recorded (successfully or not).
	preRan bool
	// aborted is true when a pre hook failed with onError=Fail: the backup must not snapshot.
	aborted bool
	// abortMessage explains the abort, for the terminal condition.
	abortMessage string
}

// hookState derives the freeze window's state from the durable record in status, so it survives an
// operator restart: a controller that dies between the quiesce and the release comes back, sees a
// recorded pre phase and no post phase, and releases. That crash case is the feature's single most
// important behaviour, and deriving state from status rather than memory is what makes it work.
func hookState(backup *cbv1.Backup) hookPhaseState {
	var st hookPhaseState
	for i := range backup.Status.Hooks {
		h := &backup.Status.Hooks[i]
		if h.Phase != string(hooks.PhasePre) {
			continue
		}
		st.preRan = true
		if h.Result == cbv1.HookFailed {
			st.aborted = true
			st.abortMessage = fmt.Sprintf("pre-backup hook failed in pod %s: %s", h.Pod, h.Message)
		}
	}
	return st
}

// postHooksRan reports whether the release phase has already been recorded as done — either every
// hook succeeded, or the retries were exhausted.
func postHooksRan(backup *cbv1.Backup) bool {
	if backup.Status.PostHookAttempts >= postHookMaxAttempts {
		return true
	}
	seen := false
	for i := range backup.Status.Hooks {
		if backup.Status.Hooks[i].Phase != string(hooks.PhasePost) {
			continue
		}
		seen = true
		if backup.Status.Hooks[i].Result == cbv1.HookFailed {
			return false // a recorded failure means another attempt is owed
		}
	}
	return seen
}

// snapshotsCut reports whether every volume has left the snapshot phase — succeeded, skipped or
// failed alike. This is the release trigger, and using the volumes' EXIT from snapshotting rather
// than their success is the whole "unconditional unfreeze" guarantee: a snapshot that failed still
// leaves an application quiesced, and a release conditioned on success would strand it.
func snapshotsCut(volumes []cbv1.VolumeStatus) bool {
	for i := range volumes {
		switch string(volumes[i].Phase) {
		case string(status.VolumePhasePending), "", string(status.VolumePhaseSnapshotting):
			return false
		}
	}
	return true
}

// openFreezeWindow is step (9b) of Reconcile: it runs the quiesce ONCE, before any VolumeSnapshot
// exists, and reports whether this reconcile is over (done=true → the caller returns res, err
// verbatim).
//
// When the hooks ran it persists the record and stops the pass deliberately: the snapshots must
// not start until the quiesce is durable in status, because a controller that died between the two
// has no other way to come back knowing it froze something.
func (r *BackupReconciler) openFreezeWindow(ctx context.Context, backup *cbv1.Backup,
	st hookPhaseState, spec cbv1.HooksSpec,
) (res ctrl.Result, done bool, err error) {
	if st.aborted {
		return ctrl.Result{}, true, r.failHooks(ctx, backup, st.abortMessage)
	}
	if st.preRan {
		return ctrl.Result{}, false, nil
	}
	ran, err := r.advancePreHooks(ctx, backup, spec, backup.Status.Volumes)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if !ran {
		// No hooks apply to this run: fall through to the snapshots with no window open.
		return ctrl.Result{}, false, nil
	}
	backup.Status.Phase = string(status.BackupPhaseSnapshottingHooks)
	if err := r.Status().Update(ctx, backup); err != nil {
		return ctrl.Result{}, true, fmt.Errorf("record pre-backup hooks for Backup %s/%s: %w",
			backup.Namespace, backup.Name, err)
	}
	return ctrl.Result{Requeue: true}, true, nil
}

// closeFreezeWindow is step (10c) of Reconcile: the release, fired on the snapshots being CUT —
// not on their having succeeded.
//
// A snapshot that failed still leaves an application quiesced, so gating the release on success
// would strand exactly the workload whose backup just went wrong (R16, the delta-8 guarantee). It
// also runs long before the upload finishes: hooks bound the snapshot phase, and a database held
// frozen for a multi-hour upload would be an outage rather than a backup.
func (r *BackupReconciler) closeFreezeWindow(ctx context.Context, backup *cbv1.Backup,
	st hookPhaseState, spec cbv1.HooksSpec,
) error {
	if !st.preRan || !snapshotsCut(backup.Status.Volumes) || postHooksRan(backup) {
		return nil
	}
	_, err := r.advancePostHooks(ctx, backup, spec, backup.Status.Volumes)
	return err
}

// advancePreHooks runs the quiesce once, before any VolumeSnapshot exists.
//
// It returns ran=true when it executed this pass (the caller must persist status and requeue
// without touching a volume — the snapshots must not start until the record is durable).
func (r *BackupReconciler) advancePreHooks(ctx context.Context, backup *cbv1.Backup,
	spec cbv1.HooksSpec, volumes []cbv1.VolumeStatus,
) (ran bool, err error) {
	resolved, err := r.resolveHooks(ctx, backup, spec, volumes, hooks.PhasePre)
	if err != nil {
		return false, err
	}
	if len(resolved) == 0 {
		// Nothing to quiesce. Record NOTHING and let hookState report preRan=false forever: a
		// backup with no hooks must not carry an empty freeze-window record implying one happened.
		return false, nil
	}
	if r.Hooks == nil {
		// Hooks are declared but the operator has no exec path (a misconfiguration, not a
		// workload problem). Failing loudly beats silently taking a crash-consistent snapshot the
		// operator believes is application-consistent.
		return false, fmt.Errorf("hooks are configured for Backup %s/%s but no pod-exec executor is wired",
			backup.Namespace, backup.Name)
	}

	phaseCtx, cancel := context.WithTimeout(ctx, hookPhaseBudget)
	defer cancel()
	results := hooks.Run(phaseCtx, r.Hooks, resolved)
	r.recordHookResults(backup, results)
	return true, nil
}

// advancePostHooks runs the release as soon as every snapshot is cut, whatever their outcome.
func (r *BackupReconciler) advancePostHooks(ctx context.Context, backup *cbv1.Backup,
	spec cbv1.HooksSpec, volumes []cbv1.VolumeStatus,
) (ran bool, err error) {
	resolved, err := r.resolveHooks(ctx, backup, spec, volumes, hooks.PhasePost)
	if err != nil {
		return false, err
	}
	if len(resolved) == 0 || r.Hooks == nil {
		return false, nil
	}

	backup.Status.PostHookAttempts++
	// Drop the previous attempt's records so the list shows this attempt, not an accumulation of
	// every retry — the useful question is "is it released NOW", not "how many times did we try".
	backup.Status.Hooks = dropHookPhase(backup.Status.Hooks, hooks.PhasePost)

	phaseCtx, cancel := context.WithTimeout(ctx, hookPhaseBudget)
	defer cancel()
	results := hooks.Run(phaseCtx, r.Hooks, resolved)
	r.recordHookResults(backup, results)

	if failed := failedResults(results); failed > 0 {
		if backup.Status.PostHookAttempts >= postHookMaxAttempts {
			// Out of retries. This is the loud one: the backup itself is fine — the data is
			// captured — but an application may still be quiesced, and only a human can undo that.
			r.Recorder.Eventf(backup, nil, corev1.EventTypeWarning, "UnfreezeFailed", "RunPostHooks",
				"%d post-backup hook(s) still failing after %d attempts on Backup %s/%s — an application may remain quiesced and needs manual attention",
				failed, backup.Status.PostHookAttempts, backup.Namespace, backup.Name)
		} else {
			r.Recorder.Eventf(backup, nil, corev1.EventTypeWarning, "HookFailed", "RunPostHooks",
				"%d post-backup hook(s) failed on attempt %d; retrying", failed, backup.Status.PostHookAttempts)
		}
	}
	return true, nil
}

// resolveHooks lists the pods that MOUNT the backed-up volumes and resolves the phase's hooks
// against them.
//
// The pod set is the security boundary as much as the semantic one: it is built by listing pods in
// the Backup's OWN namespace and keeping those referencing one of its PVCs, so a hook can never
// reach a pod elsewhere, and never reaches a pod that holds none of the data being captured
// (03-security-and-tenancy.md §5).
func (r *BackupReconciler) resolveHooks(ctx context.Context, backup *cbv1.Backup, spec cbv1.HooksSpec,
	volumes []cbv1.VolumeStatus, phase hooks.Phase,
) ([]hooks.Resolved, error) {
	if len(spec.Pre) == 0 && len(spec.Post) == 0 && !spec.HonorAnnotations {
		return nil, nil // no hooks configured at all: skip the pod listing entirely
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(backup.Namespace)); err != nil {
		return nil, fmt.Errorf("list pods for hooks in %s: %w", backup.Namespace, err)
	}
	claimed := map[string]struct{}{}
	for i := range volumes {
		claimed[volumes[i].Pvc] = struct{}{}
	}
	mounting := make([]corev1.Pod, 0, len(pods.Items))
	for i := range pods.Items {
		if podMountsAny(&pods.Items[i], claimed) {
			mounting = append(mounting, pods.Items[i])
		}
	}
	return hooks.Resolve(mounting, spec, phase), nil
}

// podMountsAny reports whether a pod references one of the named PVCs.
func podMountsAny(pod *corev1.Pod, claims map[string]struct{}) bool {
	for i := range pod.Spec.Volumes {
		src := pod.Spec.Volumes[i].PersistentVolumeClaim
		if src == nil {
			continue
		}
		if _, ok := claims[src.ClaimName]; ok {
			return true
		}
	}
	return false
}

// recordHookResults appends one status entry per result and emits the Events 05-observability §5
// specifies (HookExecuted / HookFailed on the Backup).
func (r *BackupReconciler) recordHookResults(backup *cbv1.Backup, results []hooks.Result) {
	now := metav1.Now()
	for _, res := range results {
		entry := cbv1.HookStatus{
			Phase:      string(res.Hook.Phase),
			Pod:        res.Hook.Pod.Name,
			Container:  res.Hook.Container,
			Source:     string(res.Hook.Source),
			Result:     cbv1.HookSucceeded,
			FinishedAt: &now,
		}
		switch {
		case res.Err == nil:
			r.Recorder.Eventf(backup, nil, corev1.EventTypeNormal, "HookExecuted", "RunHooks",
				"%s-backup hook succeeded in pod %s [%s]", res.Hook.Phase, res.Hook.Pod.Name, res.Hook.Container)
		case isSkipped(res):
			entry.Result = cbv1.HookSkipped
			entry.Message = res.Err.Error()
		default:
			entry.Result = cbv1.HookFailed
			entry.Message = truncate(res.Err.Error(), hookMessageLimit)
			r.Recorder.Eventf(backup, nil, corev1.EventTypeWarning, "HookFailed", "RunHooks",
				"%s-backup hook failed in pod %s [%s]: %v", res.Hook.Phase, res.Hook.Pod.Name, res.Hook.Container, res.Err)
		}
		backup.Status.Hooks = append(backup.Status.Hooks, entry)
	}
}

// isSkipped reports whether a result is the "an earlier hook aborted the phase" marker.
func isSkipped(res hooks.Result) bool {
	return res.Err != nil && res.Err == hooks.ErrSkipped //nolint:errorlint // sentinel identity, never wrapped
}

// failedResults counts genuine failures, not the skipped ones behind them.
func failedResults(results []hooks.Result) int {
	n := 0
	for _, res := range results {
		if res.Err != nil && !isSkipped(res) {
			n++
		}
	}
	return n
}

// dropHookPhase removes one phase's entries, so a retried phase replaces its record rather than
// appending to it.
func dropHookPhase(entries []cbv1.HookStatus, phase hooks.Phase) []cbv1.HookStatus {
	out := entries[:0]
	for _, e := range entries {
		if e.Phase != string(phase) {
			out = append(out, e)
		}
	}
	return out
}
