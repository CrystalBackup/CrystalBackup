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

// TargetExposer makes restore targets mountable from the operator namespace. One concrete
// type covers both mechanisms — Expose resolves which applies per call from the live target
// PVC state, so a caller never chooses (and can never choose wrong).
type TargetExposer struct {
	client.Client
	// OperatorNamespace is where staging PVCs live and the mover runs.
	OperatorNamespace string
}

// NewTargetExposer builds a TargetExposer, mirroring the package constructors elsewhere.
func NewTargetExposer(c client.Client, operatorNamespace string) *TargetExposer {
	return &TargetExposer{Client: c, OperatorNamespace: operatorNamespace}
}

// Expose makes ONE restore target mountable, resolving the mechanism from the target PVC's
// live state: absent ⇒ transplant (provision a staging PVC), bound ⇒ twin (alias the bound
// PV into the operator namespace), existing-but-unbound ⇒ ErrTargetUnbound (the caller owns
// that destructive resolution). Idempotent: every Create tolerates AlreadyExists, so a
// re-reconcile (or a restarted controller) converges on the same objects.
func (e *TargetExposer) Expose(ctx context.Context, req TargetRequest) (*TargetExposure, error) {
	var target corev1.PersistentVolumeClaim
	err := e.Get(ctx, client.ObjectKey{Namespace: req.TargetNamespace, Name: req.PVCName}, &target)
	switch {
	case apierrors.IsNotFound(err):
		// The staging PVC may already exist from a prior pass whose target PVC creation is
		// itself the missing piece (transplant handover deletes staging BEFORE creating the
		// final PVC — see Finalize). Either way the transplant path owns this shape.
		return e.exposeTransplant(ctx, req)
	case err != nil:
		return nil, fmt.Errorf("get restore target PVC %s/%s: %w", req.TargetNamespace, req.PVCName, err)
	}

	if target.Spec.VolumeMode != nil && *target.Spec.VolumeMode == corev1.PersistentVolumeBlock {
		return nil, ErrBlockUnsupported
	}
	if target.Status.Phase != corev1.ClaimBound || target.Spec.VolumeName == "" {
		return nil, ErrTargetUnbound
	}
	return e.exposeTwin(ctx, req, &target)
}

// exposeTransplant provisions the staging PVC the mover will fill: a normal dynamic claim
// in the operator namespace with the requested class/capacity/modes. No volumeName, no
// claimRef gymnastics — provisioning is entirely the CSI's; the transplant happens later in
// Finalize. The mover pod is the claim's first consumer, which is what makes
// WaitForFirstConsumer classes bind at all.
func (e *TargetExposer) exposeTransplant(ctx context.Context, req TargetRequest) (*TargetExposure, error) {
	modes := req.AccessModes
	if len(modes) == 0 {
		modes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	}
	staging := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      StagingPVCName(req.NamePrefix),
			Namespace: e.OperatorNamespace,
			Labels:    copyLabels(req.Labels),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: modes,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: req.Capacity},
			},
		},
	}
	// An empty class means "the cluster default": leave StorageClassName nil so the
	// admission defaulter injects it, exactly as a user-authored claim would get it.
	if req.StorageClass != "" {
		staging.Spec.StorageClassName = ptrTo(req.StorageClass)
	}
	if err := e.Create(ctx, staging); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create staging PVC %s/%s: %w", e.OperatorNamespace, staging.Name, err)
	}
	return e.exposure(KindTransplant, req, "", ""), nil
}

// exposeTwin aliases the target's bound volume into the operator namespace: a twin PV
// cloning the bound PV's volume source with reclaimPolicy Retain ALWAYS (the twin's
// deletion must remove the PV OBJECT only, never the user's underlying volume), pre-bound
// to a staging PVC. Pre-binding is two-sided (PV.claimRef → claim, claim.volumeName → PV)
// so the bind is immediate and can never capture a foreign claim or volume.
func (e *TargetExposer) exposeTwin(ctx context.Context, req TargetRequest, target *corev1.PersistentVolumeClaim) (*TargetExposure, error) {
	var boundPV corev1.PersistentVolume
	if err := e.Get(ctx, client.ObjectKey{Name: target.Spec.VolumeName}, &boundPV); err != nil {
		return nil, fmt.Errorf("get bound PV %s of target PVC %s/%s: %w",
			target.Spec.VolumeName, target.Namespace, target.Name, err)
	}
	if boundPV.Spec.VolumeMode != nil && *boundPV.Spec.VolumeMode == corev1.PersistentVolumeBlock {
		return nil, ErrBlockUnsupported
	}

	stagingName := StagingPVCName(req.NamePrefix)
	twinName := TwinPVName(req.NamePrefix)

	twinLabels := copyLabels(req.Labels)
	twinLabels[apiconst.LabelPVRole] = apiconst.PVRoleTwin
	twin := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: twinName, Labels: twinLabels},
		Spec: corev1.PersistentVolumeSpec{
			// The whole PersistentVolumeSource union is copied verbatim (CSI volumeHandle +
			// attributes + secret refs for CSI volumes; NFS/hostPath/... for static ones), so
			// the twin resolves to the SAME storage-side volume. NodeAffinity travels too —
			// topology-pinned volumes (local-path) must keep their constraint on the twin.
			PersistentVolumeSource: *boundPV.Spec.PersistentVolumeSource.DeepCopy(),
			Capacity:               boundPV.Spec.Capacity.DeepCopy(),
			AccessModes:            append([]corev1.PersistentVolumeAccessMode{}, boundPV.Spec.AccessModes...),
			// Retain ALWAYS: cleanup deletes the twin PV object; a Delete policy here would
			// have the provisioner destroy the user's volume when the twin is released.
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			// The class must match the staging claim's for binding; copied from the bound PV.
			StorageClassName: boundPV.Spec.StorageClassName,
			MountOptions:     append([]string{}, boundPV.Spec.MountOptions...),
			VolumeMode:       boundPV.Spec.VolumeMode,
			NodeAffinity:     boundPV.Spec.NodeAffinity.DeepCopy(),
			// Reserve the twin for OUR staging claim before it even exists.
			ClaimRef: &corev1.ObjectReference{
				Namespace: e.OperatorNamespace,
				Name:      stagingName,
			},
		},
	}
	if err := e.Create(ctx, twin); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create twin PV %s: %w", twinName, err)
	}

	staging := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      stagingName,
			Namespace: e.OperatorNamespace,
			Labels:    copyLabels(req.Labels),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: append([]corev1.PersistentVolumeAccessMode{}, boundPV.Spec.AccessModes...),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: boundPV.Spec.Capacity[corev1.ResourceStorage]},
			},
			// The bound PV's class, as an EXPLICIT pointer even when empty: a nil class would
			// have the default-class admission plugin inject the cluster default and break the
			// twin bind on classless static PVs.
			StorageClassName: ptrTo(boundPV.Spec.StorageClassName),
			VolumeName:       twinName,
		},
	}
	if err := e.Create(ctx, staging); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create staging PVC %s/%s: %w", e.OperatorNamespace, stagingName, err)
	}

	node, err := e.attachedNode(ctx, boundPV.Name)
	if err != nil {
		return nil, err
	}
	return e.exposure(KindTwin, req, twinName, node), nil
}

// attachedNode reports the single node the volume is attached on, or "" when it is not
// attached (mount anywhere) or attached on several nodes (RWX — any node works, no pin).
// The attach-after-check race is accepted: a second attach of an RWO volume is refused by
// the CSI stack and surfaces as a mover scheduling/attach timeout, never as corruption.
func (e *TargetExposer) attachedNode(ctx context.Context, pvName string) (string, error) {
	var attachments storagev1.VolumeAttachmentList
	if err := e.List(ctx, &attachments); err != nil {
		return "", fmt.Errorf("list VolumeAttachments for PV %s: %w", pvName, err)
	}
	nodes := make(map[string]struct{})
	for i := range attachments.Items {
		va := &attachments.Items[i]
		if va.Spec.Source.PersistentVolumeName != nil && *va.Spec.Source.PersistentVolumeName == pvName && va.Status.Attached {
			nodes[va.Spec.NodeName] = struct{}{}
		}
	}
	if len(nodes) == 1 {
		for n := range nodes {
			return n, nil
		}
	}
	return "", nil
}

// exposure assembles the TargetExposure for one request — pure, so both mechanisms and any
// re-derivation produce byte-identical values.
func (e *TargetExposer) exposure(kind string, req TargetRequest, twinPV, node string) *TargetExposure {
	return &TargetExposure{
		Kind:              kind,
		TargetNamespace:   req.TargetNamespace,
		TargetPVCName:     req.PVCName,
		OperatorNamespace: e.OperatorNamespace,
		StagingPVCName:    StagingPVCName(req.NamePrefix),
		TwinPVName:        twinPV,
		NodeName:          node,
		Labels:            copyLabels(req.Labels),
		RestoredFromRun:   req.RestoredFromRun,
	}
}

// Ready reports whether the staging PVC is mountable enough to start the mover. A twin's
// pre-bound claim binds without any consumer, so Ready waits for Bound — a claim that never
// binds is a mis-built twin the caller's per-phase timeout should surface, not a mover that
// wedges Pending. A transplant claim on a WaitForFirstConsumer class binds only once the
// mover schedules, so its readiness is simply "the claim exists".
func (e *TargetExposer) Ready(ctx context.Context, ex *TargetExposure) (bool, error) {
	var staging corev1.PersistentVolumeClaim
	if err := e.Get(ctx, client.ObjectKey{Namespace: ex.OperatorNamespace, Name: ex.StagingPVCName}, &staging); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil // just created; cache lag — keep polling.
		}
		return false, err
	}
	if ex.Kind == KindTwin {
		return staging.Status.Phase == corev1.ClaimBound, nil
	}
	return true, nil
}

// copyLabels returns an independent copy of in (nil in ⇒ empty map, so callers may add
// entries without guarding).
func copyLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}

// ptrTo returns a pointer to v (the package-local equivalent of k8s.io/utils/ptr.To,
// mirroring internal/mover's choice not to import it).
func ptrTo[T any](v T) *T { return &v }
