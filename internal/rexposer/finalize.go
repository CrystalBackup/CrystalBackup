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
	"maps"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

// Finalize completes the handover AFTER a successful restore. For a twin there is nothing
// to hand over (the data went straight into the user's bound volume) — done immediately.
// For a transplant it drives the multi-step PV re-bind and returns done=false while an
// asynchronous transition (claim deletion, volume release, final bind) is still settling,
// so the CALLER keeps requeuing until done — every step is idempotent and re-derivable
// from live object state, which is what makes the handover survive an operator restart at
// any point:
//
//  1. staging PVC still exists → label the PV (transplant marker + the caller's selector
//     labels, so it stays findable once the claim is gone), set reclaimPolicy Retain (the
//     claim deletion must not reclaim the volume), then delete the staging PVC.
//  2. staging gone → find the labeled PV, wait for its release, re-point claimRef to the
//     final claim, create the final PVC pre-bound in the target namespace (with the
//     restored-from provenance annotation, and NONE of the operator labels).
//  3. final PVC bound → restore the reclaimPolicy to the StorageClass's policy and STRIP
//     the operator labels from the PV: the volume is now the user's, invisible to the
//     reaper and the leak-check. Done.
func (e *TargetExposer) Finalize(ctx context.Context, ex *TargetExposure) (bool, error) {
	if ex.Kind != KindTransplant {
		return true, nil
	}

	var staging corev1.PersistentVolumeClaim
	err := e.Get(ctx, client.ObjectKey{Namespace: ex.OperatorNamespace, Name: ex.StagingPVCName}, &staging)
	switch {
	case err == nil:
		if !staging.DeletionTimestamp.IsZero() {
			return false, nil // our earlier delete is settling; wait for it to vanish.
		}
		return false, e.retainAndReleaseStaging(ctx, ex, &staging)
	case !apierrors.IsNotFound(err):
		return false, fmt.Errorf("get staging PVC %s/%s: %w", ex.OperatorNamespace, ex.StagingPVCName, err)
	}

	pv, err := e.findTransplantPV(ctx, ex)
	if err != nil {
		return false, err
	}
	if pv == nil {
		// No labeled PV and no staging claim: either the handover already completed (the
		// final claim is bound and the PV was unlabeled — done), or the volume never existed.
		var final corev1.PersistentVolumeClaim
		if err := e.Get(ctx, client.ObjectKey{Namespace: ex.TargetNamespace, Name: ex.TargetPVCName}, &final); err == nil &&
			final.Spec.VolumeName != "" {
			return true, nil
		}
		return false, fmt.Errorf("transplant volume for %s/%s not found: no staging PVC, no labeled PV, no bound final claim",
			ex.TargetNamespace, ex.TargetPVCName)
	}

	return e.handOver(ctx, ex, pv)
}

// retainAndReleaseStaging is Finalize step 1: stamp the PV with the transplant labels (its
// re-discovery key once the claim is gone), force reclaimPolicy Retain, then delete the
// staging claim. The label patch and the delete are two API writes; the labels go FIRST so
// a crash between them can never leave a released, unlabeled — unfindable — volume.
func (e *TargetExposer) retainAndReleaseStaging(ctx context.Context, ex *TargetExposure, staging *corev1.PersistentVolumeClaim) error {
	if staging.Spec.VolumeName == "" {
		// The mover reported success, so the claim was bound; an empty volumeName here is
		// cache lag. Not an error — the caller polls.
		return nil
	}
	var pv corev1.PersistentVolume
	if err := e.Get(ctx, client.ObjectKey{Name: staging.Spec.VolumeName}, &pv); err != nil {
		return fmt.Errorf("get transplant PV %s: %w", staging.Spec.VolumeName, err)
	}
	if pv.Labels[apiconst.LabelPVRole] != apiconst.PVRoleTransplant ||
		pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		base := pv.DeepCopy()
		if pv.Labels == nil {
			pv.Labels = map[string]string{}
		}
		maps.Copy(pv.Labels, ex.Labels)
		pv.Labels[apiconst.LabelPVRole] = apiconst.PVRoleTransplant
		pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
		if err := e.Patch(ctx, &pv, client.MergeFrom(base)); err != nil {
			return fmt.Errorf("label+retain transplant PV %s: %w", pv.Name, err)
		}
	}
	if err := e.Delete(ctx, staging, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete staging PVC %s/%s: %w", staging.Namespace, staging.Name, err)
	}
	return nil
}

// handOver is Finalize steps 2-3 on the labeled, staging-less PV: wait for release,
// re-point, create the final claim, wait for its bind, then restore the policy and strip
// the labels.
func (e *TargetExposer) handOver(ctx context.Context, ex *TargetExposure, pv *corev1.PersistentVolume) (bool, error) {
	claimedByFinal := pv.Spec.ClaimRef != nil &&
		pv.Spec.ClaimRef.Namespace == ex.TargetNamespace && pv.Spec.ClaimRef.Name == ex.TargetPVCName

	if !claimedByFinal {
		// Still referencing the deleted staging claim: wait for the PV controller to mark it
		// Released before re-pointing (re-pointing a still-Bound PV would fight the binder).
		if pv.Status.Phase == corev1.VolumeBound {
			return false, nil
		}
		base := pv.DeepCopy()
		pv.Spec.ClaimRef = &corev1.ObjectReference{
			Namespace: ex.TargetNamespace,
			Name:      ex.TargetPVCName,
		}
		if err := e.Patch(ctx, pv, client.MergeFrom(base)); err != nil {
			return false, fmt.Errorf("re-point transplant PV %s claimRef: %w", pv.Name, err)
		}
	}

	var final corev1.PersistentVolumeClaim
	err := e.Get(ctx, client.ObjectKey{Namespace: ex.TargetNamespace, Name: ex.TargetPVCName}, &final)
	switch {
	case apierrors.IsNotFound(err):
		if err := e.createFinalPVC(ctx, ex, pv); err != nil {
			return false, err
		}
		return false, nil // created; wait for the bind.
	case err != nil:
		return false, fmt.Errorf("get final PVC %s/%s: %w", ex.TargetNamespace, ex.TargetPVCName, err)
	}
	switch {
	case final.Spec.VolumeName == "":
		// A PRE-EXISTING, never-bound claim (the WaitForFirstConsumer override workflow):
		// adopt it by setting its still-empty volumeName — the two-sided pre-bind that lets
		// the binder complete without ever deleting the user's object.
		base := final.DeepCopy()
		final.Spec.VolumeName = pv.Name
		if ann := final.Annotations; ex.RestoredFromRun != "" {
			if ann == nil {
				final.Annotations = map[string]string{}
			}
			final.Annotations[apiconst.AnnotationRestoredFrom] = ex.RestoredFromRun
		}
		if err := e.Patch(ctx, &final, client.MergeFrom(base)); err != nil {
			return false, fmt.Errorf("bind pre-existing final PVC %s/%s to the transplant volume: %w",
				ex.TargetNamespace, ex.TargetPVCName, err)
		}
		return false, nil // adopted; wait for the bind.
	case final.Spec.VolumeName != pv.Name:
		// Bound (or pre-bound) to a DIFFERENT volume since the handover started — never
		// fight the binder for a claim that found another home; surface it.
		return false, fmt.Errorf("final PVC %s/%s is bound to volume %q, not the transplant volume %q",
			ex.TargetNamespace, ex.TargetPVCName, final.Spec.VolumeName, pv.Name)
	}
	if final.Status.Phase != corev1.ClaimBound {
		return false, nil
	}

	// Step 3: the volume is the user's now — restore the class's reclaim policy and remove
	// every operator label in one patch.
	base := pv.DeepCopy()
	pv.Spec.PersistentVolumeReclaimPolicy = e.storageClassReclaimPolicy(ctx, pv.Spec.StorageClassName)
	for k := range ex.Labels {
		delete(pv.Labels, k)
	}
	delete(pv.Labels, apiconst.LabelPVRole)
	if err := e.Patch(ctx, pv, client.MergeFrom(base)); err != nil {
		return false, fmt.Errorf("release transplant PV %s to its class policy: %w", pv.Name, err)
	}
	return true, nil
}

// createFinalPVC creates the user-facing claim pre-bound to the transplanted volume. Its
// spec mirrors the PV (class as an explicit pointer even when empty — a nil class would
// have the default-class defaulter break the pre-bind; modes and capacity from the PV so
// the bind is always satisfiable). It carries the restored-from provenance ANNOTATION and
// deliberately NO operator labels: this object belongs to the user from birth.
func (e *TargetExposer) createFinalPVC(ctx context.Context, ex *TargetExposure, pv *corev1.PersistentVolume) error {
	final := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ex.TargetPVCName,
			Namespace: ex.TargetNamespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: append([]corev1.PersistentVolumeAccessMode{}, pv.Spec.AccessModes...),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: pv.Spec.Capacity[corev1.ResourceStorage]},
			},
			StorageClassName: ptrTo(pv.Spec.StorageClassName),
			VolumeName:       pv.Name,
		},
	}
	if ex.RestoredFromRun != "" {
		final.Annotations = map[string]string{apiconst.AnnotationRestoredFrom: ex.RestoredFromRun}
	}
	if err := e.Create(ctx, final); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create final PVC %s/%s: %w", ex.TargetNamespace, ex.TargetPVCName, err)
	}
	return nil
}

// findTransplantPV locates the mid-handover volume by the exposure's labels plus the
// transplant role marker — the re-discovery key retainAndReleaseStaging stamped before the
// staging claim was deleted. Nil (no error) when no such PV exists.
func (e *TargetExposer) findTransplantPV(ctx context.Context, ex *TargetExposure) (*corev1.PersistentVolume, error) {
	sel := copyLabels(ex.Labels)
	sel[apiconst.LabelPVRole] = apiconst.PVRoleTransplant
	var pvs corev1.PersistentVolumeList
	if err := e.List(ctx, &pvs, client.MatchingLabels(sel)); err != nil {
		return nil, fmt.Errorf("list transplant PVs for %s/%s: %w", ex.TargetNamespace, ex.TargetPVCName, err)
	}
	if len(pvs.Items) == 0 {
		return nil, nil
	}
	return &pvs.Items[0], nil
}

// storageClassReclaimPolicy resolves the reclaim policy the transplanted volume should end
// with: its StorageClass's policy, or Delete when the class is unset/gone (the dynamic-
// provisioning default — never Retain, which would leak the volume on eventual deletion).
func (e *TargetExposer) storageClassReclaimPolicy(ctx context.Context, scName string) corev1.PersistentVolumeReclaimPolicy {
	if scName == "" {
		return corev1.PersistentVolumeReclaimDelete
	}
	var sc storagev1.StorageClass
	if err := e.Get(ctx, client.ObjectKey{Name: scName}, &sc); err != nil || sc.ReclaimPolicy == nil {
		return corev1.PersistentVolumeReclaimDelete
	}
	return *sc.ReclaimPolicy
}
