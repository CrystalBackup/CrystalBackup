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

package rexposer

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Cleanup removes everything an exposure created that is NOT the user's restored volume,
// idempotently and in an order that can never destroy user data:
//
//   - Both kinds: delete the staging PVC (NotFound is success — a completed transplant
//     already deleted it in Finalize).
//   - Twin: delete the twin PV OBJECT. Its reclaimPolicy is Retain by construction, so this
//     removes the alias only, never the user's underlying volume; the PV protection
//     finalizer holds the object until the staging claim is gone, which is why the claim is
//     deleted first and both deletes tolerate a not-yet state (the next call, or the orphan
//     reaper, converges it).
//   - Transplant: reclaim a volume whose handover never completed. A labeled transplant PV
//     with no bound final claim belongs to a FAILED restore — its content is not
//     deliverable — so its reclaimPolicy is set back to Delete and the release (the staging
//     delete above, or the already-Released state) lets the provisioner reclaim the storage.
//     If the final claim IS bound to it, the handover in fact succeeded and only Finalize's
//     tail was missed: the labels are stripped and the class policy restored instead —
//     Cleanup can never undo a delivered restore.
func (e *TargetExposer) Cleanup(ctx context.Context, ex *TargetExposure) error {
	staging := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: ex.StagingPVCName, Namespace: ex.OperatorNamespace},
	}
	if err := e.Delete(ctx, staging, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete staging PVC %s/%s: %w", ex.OperatorNamespace, ex.StagingPVCName, err)
	}

	switch ex.Kind {
	case KindTwin:
		twin := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: ex.TwinPVName}}
		if err := e.Delete(ctx, twin); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete twin PV %s: %w", ex.TwinPVName, err)
		}
		return nil
	case KindTransplant:
		return e.reclaimUnfinishedTransplant(ctx, ex)
	default:
		return nil
	}
}

// reclaimUnfinishedTransplant handles the transplant residue cases described on Cleanup.
func (e *TargetExposer) reclaimUnfinishedTransplant(ctx context.Context, ex *TargetExposure) error {
	pv, err := e.findTransplantPV(ctx, ex)
	if err != nil || pv == nil {
		return err // no labeled PV: nothing mid-handover (success already unlabeled, or never provisioned).
	}

	// Defensive: a bound final claim means the handover actually succeeded — finish
	// Finalize's tail (class policy + label strip) instead of reclaiming user data.
	var final corev1.PersistentVolumeClaim
	if err := e.Get(ctx, client.ObjectKey{Namespace: ex.TargetNamespace, Name: ex.TargetPVCName}, &final); err == nil &&
		final.Spec.VolumeName == pv.Name {
		_, err := e.handOver(ctx, ex, pv)
		return err
	}

	// Failed restore: hand the volume back to the provisioner. With the staging claim
	// deleted (above) the PV goes Released, and a Released PV with a Delete policy is
	// reclaimed by the provisioner — storage freed exactly once, object removed with it.
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
		base := pv.DeepCopy()
		pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete
		if err := e.Patch(ctx, pv, client.MergeFrom(base)); err != nil {
			return fmt.Errorf("set reclaimPolicy Delete on failed transplant PV %s: %w", pv.Name, err)
		}
	}
	return nil
}
