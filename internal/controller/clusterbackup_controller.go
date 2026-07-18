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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/nsselector"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// clusterBackupPollInterval paces re-reconciles while a run is still Running. The child-Backup
// watch (see SetupWithManager) drives most re-aggregation the instant a child's status changes;
// this requeue is the watch-independent backstop so a run never stalls on a missed event.
const clusterBackupPollInterval = 15 * time.Second

// clusterBackupMessageCap bounds a FailureRecord/condition message so a pathological child error
// cannot bloat the ClusterBackup's status (which already caps the failures LIST via
// status.AppendCappedFailure); this caps each ENTRY's length.
const clusterBackupMessageCap = 256

// ClusterBackupReconciler reconciles a ClusterBackup: one cluster-DR RUN that fans a Backup out
// into every namespace its selector matches, then aggregates those children into a single bounded
// run status. It creates children but never OWNS them — a namespaced Backup cannot carry an
// ownerReference to a cluster-scoped ClusterBackup, and history GC must never cascade-delete a
// still-restorable child (apiconst) — so the parent→child link is the crystalbackup.io/cluster-backup
// label alone, and a label-based Backup watch (not Owns) re-aggregates the run when a child moves.
//
// It is the single writer of ClusterBackup.status: every status mutation happens in Reconcile.
// The child Backups own their OWN status and execution (internal/controller/backup_controller.go);
// this controller only reads them.
type ClusterBackupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// OperatorNamespace is where the cluster-plane platform Secrets and mover Jobs live. The
	// ClusterBackup controller does not touch them directly, but carries the value for parity with
	// the sibling controllers and for future cluster-manifests capture (M3).
	OperatorNamespace string
	Recorder          events.EventRecorder
}

// NewClusterBackupReconciler builds a ClusterBackupReconciler. Callers (main.go, the envtest
// suite) go through this constructor to keep the wiring in one place, mirroring the sibling
// reconcilers.
func NewClusterBackupReconciler(
	c client.Client,
	scheme *runtime.Scheme,
	operatorNamespace string,
	recorder events.EventRecorder,
) *ClusterBackupReconciler {
	return &ClusterBackupReconciler{
		Client:            c,
		Scheme:            scheme,
		OperatorNamespace: operatorNamespace,
		Recorder:          recorder,
	}
}

// +kubebuilder:rbac:groups=crystalbackup.io,resources=clusterbackups,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=clusterbackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=clusterbackuplocations,verbs=get;list;watch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=backups,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Reconcile drives one ClusterBackup run: fail-fast on a missing location, resolve the namespace
// selector, ensure one child Backup per matched namespace (idempotent, label-linked), then
// aggregate the children into the run's status exactly once.
func (r *ClusterBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cb cbv1.ClusterBackup
	if err := r.Get(ctx, req.NamespacedName, &cb); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// A terminal run is frozen: a stray child-watch event on a finished run must not re-run it or
	// re-open its aggregate record. (First reconcile has an empty phase and falls through.)
	if isTerminalClusterBackupPhase(cb.Status.Phase) {
		return ctrl.Result{}, nil
	}

	// (1) Fail fast if the referenced ClusterBackupLocation is absent: do not fan children out into
	// N namespaces only to have every one of them gate on a location that does not exist.
	locName := cb.Spec.LocationRef.Name
	var loc cbv1.ClusterBackupLocation
	if err := r.Get(ctx, client.ObjectKey{Name: locName}, &loc); err != nil {
		if apierrors.IsNotFound(err) {
			return r.blocked(ctx, &cb, "LocationNotFound",
				fmt.Sprintf("ClusterBackupLocation %q not found", locName))
		}
		return ctrl.Result{}, fmt.Errorf("get ClusterBackupLocation %s: %w", locName, err)
	}

	// (2) Resolve the namespace selector against the live namespace set. A rule-8 / regexp error is
	// a spec fault (nsselector fails loudly rather than guess): surface it and refuse to fan out.
	var nsList corev1.NamespaceList
	if err := r.List(ctx, &nsList); err != nil {
		return ctrl.Result{}, fmt.Errorf("list namespaces: %w", err)
	}
	matched, err := nsselector.Match(nsList.Items, cb.Spec.Namespaces)
	if err != nil {
		return r.blocked(ctx, &cb, "SelectorInvalid", clampMessage(err.Error()))
	}

	// (3) Fan out: ensure one child Backup per matched namespace (idempotent, label-linked, no
	// ownerRef). A per-namespace create failure is recorded and does not abort the other namespaces.
	var fanoutFailures []cbv1.FailureRecord
	for _, ns := range matched {
		if err := r.ensureChildBackup(ctx, &cb, ns); err != nil {
			log.Error(err, "fan-out: ensure child Backup failed", "namespace", ns, "run", cb.Name)
			fanoutFailures = append(fanoutFailures, cbv1.FailureRecord{
				Namespace: ns, Backup: cb.Name, Message: clampMessage(err.Error()),
			})
		}
	}

	// (4) Aggregate the children into the run status and write it once.
	return r.aggregateAndWrite(ctx, &cb, matched, fanoutFailures)
}

// ensureChildBackup creates the run's child Backup in namespace ns if it does not already exist.
// The child is named after the run (the name equals the run tag in every namespace; the namespace
// disambiguates), linked to the run by the crystalbackup.io/cluster-backup label, marked
// cluster-origin (read-only to users, per RBAC), and pointed at the run's ClusterBackupLocation.
// It carries NO ownerReference to the ClusterBackup. An existing child is left untouched — it owns
// its own lifecycle.
func (r *ClusterBackupReconciler) ensureChildBackup(ctx context.Context, cb *cbv1.ClusterBackup, ns string) error {
	key := client.ObjectKey{Namespace: ns, Name: cb.Name}
	var existing cbv1.Backup
	if err := r.Get(ctx, key, &existing); err == nil {
		return nil // idempotent: never mutate an existing child here.
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get child Backup %s/%s: %w", ns, cb.Name, err)
	}

	child := &cbv1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cb.Name,
			Namespace: ns,
			Labels:    childBackupLabels(cb, ns),
		},
		Spec: cbv1.BackupSpec{
			ScheduleRef: cb.Spec.ScheduleRef,
			LocationRef: cbv1.LocationReference{
				Kind: kindClusterBackupLocation,
				Name: cb.Spec.LocationRef.Name,
			},
		},
	}
	if err := r.Create(ctx, child); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil // lost a create race with a prior reconcile of this same run; fine.
		}
		return fmt.Errorf("create child Backup %s/%s: %w", ns, cb.Name, err)
	}
	r.Recorder.Eventf(cb, nil, corev1.EventTypeNormal, "FannedOut", "FanOut",
		"created child Backup in namespace %q", ns)
	return nil
}

// childBackupLabels are the labels every fanned-out child carries. crystalbackup.io/cluster-backup
// is the load-bearing parent link (the aggregate List selector, the child's own run resolution,
// and the crucible's cleanup all key off it); origin=cluster marks the child cluster-owned; the
// namespace and (optional) schedule labels mirror the restic tags for queryability.
func childBackupLabels(cb *cbv1.ClusterBackup, ns string) map[string]string {
	l := map[string]string{
		apiconst.LabelClusterBackup: cb.Name,
		apiconst.LabelOrigin:        apiconst.OriginCluster,
		apiconst.LabelNamespace:     ns,
	}
	if cb.Spec.ScheduleRef != "" {
		l[apiconst.LabelSchedule] = cb.Spec.ScheduleRef
	}
	return l
}

// aggregateAndWrite lists every child of the run in one label-scoped, cluster-wide call, folds the
// children into the run's counters/failures/phase, and writes the run status exactly once. The
// roll-up is driven off the MATCHED-namespace set (not merely the children found): a matched
// namespace whose child has not yet appeared in cache counts as in-flight, which keeps the run
// Running until every matched namespace has a reporting child.
func (r *ClusterBackupReconciler) aggregateAndWrite(
	ctx context.Context, cb *cbv1.ClusterBackup, matched []string, fanoutFailures []cbv1.FailureRecord,
) (ctrl.Result, error) {
	var children cbv1.BackupList
	if err := r.List(ctx, &children, client.MatchingLabels{apiconst.LabelClusterBackup: cb.Name}); err != nil {
		return ctrl.Result{}, fmt.Errorf("list child Backups for run %s: %w", cb.Name, err)
	}
	byNS := make(map[string]*cbv1.Backup, len(children.Items))
	for i := range children.Items {
		c := &children.Items[i]
		byNS[c.Namespace] = c
	}

	st := &cb.Status
	// Recompute every tally from scratch each pass so the aggregate is a pure function of the
	// current children (idempotent; no drift from a partial prior write).
	st.NamespacesMatched = int32(len(matched))
	st.NamespacesSucceeded = 0
	st.NamespacesFailed = 0
	st.PVCsSucceeded = 0
	st.PVCsFailed = 0
	st.AddedBytes = 0
	st.ClusterResourcesCaptured = 0 // the kind=cluster-manifests capture Job is M3.
	st.Failures = nil
	for _, f := range fanoutFailures {
		st.Failures = status.AppendCappedFailure(st.Failures, f, status.DefaultFailureCap)
	}

	childPhases := make([]string, 0, len(matched))
	for _, ns := range matched {
		child := byNS[ns]
		if child == nil {
			childPhases = append(childPhases, "") // fanned out but not observed yet → in-flight.
			continue
		}
		childPhases = append(childPhases, child.Status.Phase)

		for _, v := range child.Status.Volumes {
			switch v.Phase {
			case status.VolumePhaseCompleted:
				st.PVCsSucceeded++
			case status.VolumePhaseFailed:
				st.PVCsFailed++
			}
			st.AddedBytes += v.AddedBytes
		}

		switch child.Status.Phase {
		case string(status.BackupPhaseCompleted), string(status.BackupPhasePartiallyCompleted):
			st.NamespacesSucceeded++
		case string(status.BackupPhaseFailed), string(status.BackupPhasePartiallyFailed):
			st.NamespacesFailed++
			st.Failures = status.AppendCappedFailure(st.Failures, cbv1.FailureRecord{
				Namespace: ns, Backup: child.Name, Message: childFailureMessage(child),
			}, status.DefaultFailureCap)
		}
	}

	phase := status.RollUpBackupPhases(childPhases)
	if len(matched) == 0 {
		// A valid selector that matches no namespace has nothing to protect: terminate the run
		// (vacuously Completed) rather than hot-loop in Pending, but surface it so a misaimed
		// selector is diagnosable. The terminal guard then freezes it after this single event.
		phase = status.ClusterBackupPhaseCompleted
		r.Recorder.Eventf(cb, nil, corev1.EventTypeWarning, "NoNamespacesMatched", "SelectNamespaces",
			"namespace selector matched no namespaces; nothing to back up")
	}
	st.Phase = string(phase)

	if st.StartTime == nil {
		now := metav1.Now()
		st.StartTime = &now
	}
	terminal := isTerminalClusterBackupPhase(string(phase))
	if terminal && st.CompletionTime == nil {
		now := metav1.Now()
		st.CompletionTime = &now
	}
	setClusterBackupCondition(cb, phase)

	if err := r.Status().Update(ctx, cb); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status for ClusterBackup %s: %w", cb.Name, err)
	}
	if terminal {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: clusterBackupPollInterval}, nil
}

// blocked records a non-terminal blocker (missing location, invalid selector) on the Ready
// condition, keeps the run Pending, and requeues on the fixable-fault cadence. It never fans out —
// the blocker must clear first (a spec edit re-triggers immediately via the generation change).
func (r *ClusterBackupReconciler) blocked(ctx context.Context, cb *cbv1.ClusterBackup, reason, message string) (ctrl.Result, error) {
	cb.Status.Phase = string(status.ClusterBackupPhasePending)
	status.SetCondition(&cb.Status.Conditions, ConditionReady, metav1.ConditionFalse, reason, message, cb.Generation)
	if err := r.Status().Update(ctx, cb); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status for ClusterBackup %s: %w", cb.Name, err)
	}
	return ctrl.Result{RequeueAfter: shortRequeueInterval}, nil
}

// childFailureMessage extracts a short cause for a failed child from its Ready condition, falling
// back to the raw phase. Always clamped so one child cannot bloat the parent's status.
func childFailureMessage(child *cbv1.Backup) string {
	if c := status.FindCondition(child.Status.Conditions, ConditionReady); c != nil && c.Message != "" {
		return clampMessage(c.Message)
	}
	return clampMessage("backup phase " + child.Status.Phase)
}

// setClusterBackupCondition records the headline Ready condition from the aggregate phase: True
// once every namespace succeeded, False (with a distinguishing reason) while running or on any
// failure.
func setClusterBackupCondition(cb *cbv1.ClusterBackup, phase status.ClusterBackupPhase) {
	switch phase {
	case status.ClusterBackupPhaseCompleted:
		status.SetCondition(&cb.Status.Conditions, ConditionReady, metav1.ConditionTrue, "Completed",
			"all matched namespaces backed up", cb.Generation)
	case status.ClusterBackupPhaseFailed:
		status.SetCondition(&cb.Status.Conditions, ConditionReady, metav1.ConditionFalse, "Failed",
			"every matched namespace failed", cb.Generation)
	case status.ClusterBackupPhasePartiallyFailed:
		status.SetCondition(&cb.Status.Conditions, ConditionReady, metav1.ConditionFalse, "PartiallyFailed",
			"one or more matched namespaces failed", cb.Generation)
	default: // Pending, Running
		status.SetCondition(&cb.Status.Conditions, ConditionReady, metav1.ConditionFalse, "InProgress",
			"run in progress ("+string(phase)+")", cb.Generation)
	}
}

// isTerminalClusterBackupPhase reports whether a run has reached a final phase (no more work, no
// requeue). Mirrors isTerminalBackupPhase one level up.
func isTerminalClusterBackupPhase(phase string) bool {
	switch status.ClusterBackupPhase(phase) {
	case status.ClusterBackupPhaseCompleted,
		status.ClusterBackupPhaseFailed,
		status.ClusterBackupPhasePartiallyFailed:
		return true
	default:
		return false
	}
}

// clampMessage bounds a status message to clusterBackupMessageCap runes, appending an ellipsis
// when it truncates, so a pathological child error string cannot bloat the run status.
func clampMessage(s string) string {
	r := []rune(s)
	if len(r) <= clusterBackupMessageCap {
		return s
	}
	return string(r[:clusterBackupMessageCap-1]) + "…"
}

// SetupWithManager registers this reconciler. It reconciles ClusterBackups directly and, via a
// label-based mapping (NOT Owns — a namespaced child Backup cannot be owned by a cluster-scoped
// ClusterBackup), re-reconciles the run whenever one of its children changes. The
// clusterBackupPollInterval requeue is the watch-independent progress backstop.
func (r *ClusterBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cbv1.ClusterBackup{}).
		Watches(&cbv1.Backup{}, handler.EnqueueRequestsFromMapFunc(r.mapChildToRun)).
		Named("clusterbackup").
		Complete(r)
}

// mapChildToRun maps a child Backup back to its ClusterBackup run using only the child's labels:
// crystalbackup.io/origin=cluster gates it to cluster-owned children, and
// crystalbackup.io/cluster-backup names the run (a cluster-scoped object, so the request carries an
// empty namespace). A user-plane or unlabelled Backup maps to nothing.
func (r *ClusterBackupReconciler) mapChildToRun(_ context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	if labels[apiconst.LabelOrigin] != apiconst.OriginCluster {
		return nil
	}
	run := labels[apiconst.LabelClusterBackup]
	if run == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: run}}}
}
