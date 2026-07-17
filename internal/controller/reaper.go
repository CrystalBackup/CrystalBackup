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
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

const (
	// defaultReaperInterval is how often the orphan reaper sweeps.
	defaultReaperInterval = 10 * time.Minute

	// defaultReaperMinAge is the minimum age an object must reach before the reaper will touch it,
	// even when it already looks orphaned. It is a race guard: an exposure's temp PVC / mover Job
	// can exist for a few reconciles before the owning Backup's status catches up, and reaping one
	// out from under a slow-but-live reconcile would corrupt an in-flight backup. Nothing younger
	// than this is ever swept.
	defaultReaperMinAge = 30 * time.Minute
)

// OrphanReaper is the periodic backstop that keeps a run from leaking storage objects when the
// happy-path teardown was missed (an operator crash mid-cleanup, a namespace deleted out from under
// an in-flight backup). It sweeps the operator namespace for the NATIVE per-PVC exposure objects a
// backup creates — the temp clone PVC, the mover Job and its per-Job creds Secret, all stamped with
// the exposure labels — and deletes any whose owning Backup is gone, or whose volume for that PVC
// has already reached a terminal phase (so its teardown should already have run). It is a
// manager.Runnable (a timer loop), not a reconciler: there is no single object to reconcile, and a
// periodic full sweep is exactly the shape of a leak backstop.
//
// The VolumeSnapshot / VolumeSnapshotContent objects (and the storage-snapshot reclaim by
// snapshotHandle / Retain re-assertion) are NOT swept here: their lifecycle is the exposer's
// (internal/exposer's ordered Cleanup and its reclaimOrphanOriginVSC crash-window recovery, which
// reason about deletionPolicy correctly), and they are exercised by the crucible against a real CSI
// driver. This reaper owns the native residue an operator without CSI can still verify.
type OrphanReaper struct {
	client.Client
	// OperatorNamespace is where the temp clone PVCs, mover Jobs and creds Secrets live.
	OperatorNamespace string
	// MinAge and Interval default to defaultReaperMinAge / defaultReaperInterval when zero.
	MinAge   time.Duration
	Interval time.Duration
}

// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=crystalbackup.io,resources=backups,verbs=get;list;watch

// Start runs the sweep loop until ctx is cancelled. It satisfies manager.Runnable. It applies the
// production defaults for Interval and MinAge here (not in sweepOnce), so a caller can drive
// sweepOnce directly with an explicit MinAge of 0 to reap regardless of age.
func (r *OrphanReaper) Start(ctx context.Context) error {
	if r.Interval <= 0 {
		r.Interval = defaultReaperInterval
	}
	if r.MinAge <= 0 {
		r.MinAge = defaultReaperMinAge
	}
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()

	log := logf.FromContext(ctx).WithName("orphan-reaper")
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := r.sweepOnce(ctx); err != nil {
				log.Error(err, "orphan reaper sweep failed; will retry next interval")
			}
		}
	}
}

// sweepOnce reaps every orphaned native exposure object in one pass. It is best-effort: a delete
// that races another actor (NotFound) is success, and a single object's failure is logged and does
// not abort the rest of the sweep.
func (r *OrphanReaper) sweepOnce(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("orphan-reaper")
	sel := client.MatchingLabels{apiconst.LabelManagedBy: apiconst.ManagedByValue}
	inOperatorNS := client.InNamespace(r.OperatorNamespace)
	// r.MinAge is used literally: Start applies the production default, so a direct sweepOnce caller
	// (a test) may pass 0 to reap regardless of age.
	cutoff := time.Now().Add(-r.MinAge)

	// Mover Jobs and their per-Job creds Secrets share the managed-by + per-PVC labels; the temp
	// clone PVCs carry the same exposure labels. A repository-init Job (managed-by, no PVC label) is
	// never a candidate — reapObjects requires a per-PVC label.
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, inOperatorNS, sel); err != nil {
		return err
	}
	var pvcs corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &pvcs, inOperatorNS, sel); err != nil {
		return err
	}
	var secrets corev1.SecretList
	if err := r.List(ctx, &secrets, inOperatorNS, sel); err != nil {
		return err
	}

	reap := func(obj client.Object) {
		orphaned, err := r.orphaned(ctx, obj, cutoff)
		if err != nil {
			log.Error(err, "orphan reaper: orphan check failed", "kind", obj.GetObjectKind().GroupVersionKind().Kind, "name", obj.GetName())
			return
		}
		if !orphaned {
			return
		}
		if err := r.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "orphan reaper: delete failed", "name", obj.GetName())
			return
		}
		log.Info("orphan reaper: reaped leftover exposure object", "name", obj.GetName(),
			"run", obj.GetLabels()[apiconst.LabelClusterBackup], "pvc", obj.GetLabels()[apiconst.LabelPVC])
	}

	for i := range jobs.Items {
		reap(&jobs.Items[i])
	}
	for i := range pvcs.Items {
		reap(&pvcs.Items[i])
	}
	for i := range secrets.Items {
		reap(&secrets.Items[i])
	}
	return nil
}

// orphaned reports whether one exposure object should be reaped: it must be older than the cutoff,
// carry a per-PVC label (so it is a per-PVC exposure object, not a repository-init Job), and its
// owning Backup must be gone — or present but with this PVC's volume already terminal (or absent),
// meaning the backup's own teardown should already have removed it. A Backup that is being deleted
// is left to its finalizer; a volume still in flight is live and never reaped.
func (r *OrphanReaper) orphaned(ctx context.Context, obj client.Object, cutoff time.Time) (bool, error) {
	labels := obj.GetLabels()
	pvc := labels[apiconst.LabelPVC]
	run := labels[apiconst.LabelClusterBackup]
	ns := labels[apiconst.LabelNamespace]
	if pvc == "" || run == "" || ns == "" {
		return false, nil // not a per-PVC exposure object (e.g. a repository-init Job).
	}
	if obj.GetCreationTimestamp().Time.After(cutoff) {
		return false, nil // too young — a live reconcile may still be settling its status.
	}

	var backup cbv1.Backup
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: run}, &backup); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil // owning Backup is gone: pure orphan.
		}
		return false, err
	}
	if !backup.DeletionTimestamp.IsZero() {
		return false, nil // being deleted — its finalizer owns the teardown.
	}
	for i := range backup.Status.Volumes {
		if backup.Status.Volumes[i].Pvc == pvc {
			return volumePhaseTerminal(backup.Status.Volumes[i].Phase), nil
		}
	}
	return true, nil // the Backup no longer tracks this PVC — its exposure is residue.
}

// volumePhaseTerminal reports whether a per-PVC volume phase is terminal, so its exposure objects
// should already have been torn down.
func volumePhaseTerminal(phase status.VolumePhase) bool {
	switch phase {
	case status.VolumePhaseCompleted, status.VolumePhaseSkipped, status.VolumePhaseFailed:
		return true
	default:
		return false
	}
}
