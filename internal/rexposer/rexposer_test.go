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
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

// These tests drive the full adr/0016 mechanics against the fake client by MANUALLY playing
// the roles Kubernetes controllers play in a real cluster (the PV controller marking a
// volume Released, the binder marking a claim Bound), exactly as internal/exposer's tests
// simulate the CSI side. What stays crucible-deferred: whether a real kubelet actually
// stages the twin by volumeHandle, whether a real provisioner reclaims the failed-transplant
// volume, and whether the transplanted claim mounts.

const (
	opNS     = "crystal-backup-system"
	tenantNS = "c-team-x"
)

func testLabels() map[string]string {
	return map[string]string{
		apiconst.LabelManagedBy: apiconst.ManagedByValue,
		apiconst.LabelRestore:   "recover-1",
		apiconst.LabelNamespace: tenantNS,
		apiconst.LabelPVC:       "uploads",
	}
}

func testRequest() TargetRequest {
	return TargetRequest{
		TargetNamespace: tenantNS,
		PVCName:         "uploads",
		StorageClass:    "std",
		Capacity:        resource.MustParse("10Gi"),
		AccessModes:     []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		NamePrefix:      "recover-1-uploads",
		Labels:          testLabels(),
		RestoredFromRun: "daily-20260718-010000",
	}
}

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(corev1): %v", err)
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(storagev1): %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// boundTargetFixture returns a bound target PVC and its PV, as a real cluster would hold
// them (two-sided bind, CSI source, node affinity).
func boundTargetFixture() (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume) {
	fsMode := corev1.PersistentVolumeFilesystem
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-1111"},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:    corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       "rook-ceph.rbd.csi.ceph.com",
					VolumeHandle: "0001-abc-volume",
					VolumeAttributes: map[string]string{
						"pool": "replicapool",
					},
				},
			},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			StorageClassName:              "fast-rbd",
			VolumeMode:                    &fsMode,
			ClaimRef: &corev1.ObjectReference{
				Namespace: tenantNS, Name: "uploads", UID: "uid-1",
			},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "uploads", Namespace: tenantNS},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: ptrTo("fast-rbd"),
			VolumeName:       pv.Name,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	return pvc, pv
}

// TestExposeTransplantWhenTargetAbsent: an absent target PVC selects pvc-transplant and
// provisions a labeled staging claim with the requested class/capacity/modes — and no
// volumeName (dynamic provisioning is the CSI's job).
func TestExposeTransplantWhenTargetAbsent(t *testing.T) {
	c := newClient(t)
	e := NewTargetExposer(c, opNS)

	ex, err := e.Expose(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Expose: %v", err)
	}
	if ex.Kind != KindTransplant || ex.StagingPVCName != "recover-1-uploads-target" || ex.TwinPVName != "" || ex.NodeName != "" {
		t.Fatalf("exposure = %+v, want transplant with staging recover-1-uploads-target", ex)
	}

	var staging corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: opNS, Name: ex.StagingPVCName}, &staging); err != nil {
		t.Fatalf("staging PVC not created: %v", err)
	}
	if staging.Spec.VolumeName != "" {
		t.Errorf("staging.volumeName = %q, want empty (dynamic provisioning)", staging.Spec.VolumeName)
	}
	if got := *staging.Spec.StorageClassName; got != "std" {
		t.Errorf("staging class = %q, want std", got)
	}
	if got := staging.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(resource.MustParse("10Gi")) != 0 {
		t.Errorf("staging capacity = %v, want 10Gi", got)
	}
	if staging.Labels[apiconst.LabelRestore] != "recover-1" {
		t.Errorf("staging labels = %v, want the restore owner labels", staging.Labels)
	}

	// Idempotent: a second Expose converges on the same objects.
	if _, err := e.Expose(context.Background(), testRequest()); err != nil {
		t.Fatalf("second Expose: %v", err)
	}

	// A transplant is ready as soon as the claim exists (WFFC: the mover is the first consumer).
	ready, err := e.Ready(context.Background(), ex)
	if err != nil || !ready {
		t.Errorf("Ready(transplant) = %v, %v; want true, nil", ready, err)
	}
}

// TestExposeTransplantDefaultClass: an empty StorageClass leaves the staging claim's class
// nil, so the cluster's default-class defaulter applies exactly as it would to a user claim.
func TestExposeTransplantDefaultClass(t *testing.T) {
	c := newClient(t)
	e := NewTargetExposer(c, opNS)
	req := testRequest()
	req.StorageClass = ""

	if _, err := e.Expose(context.Background(), req); err != nil {
		t.Fatalf("Expose: %v", err)
	}
	var staging corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: opNS, Name: "recover-1-uploads-target"}, &staging); err != nil {
		t.Fatalf("staging PVC not created: %v", err)
	}
	if staging.Spec.StorageClassName != nil {
		t.Errorf("staging class = %q, want nil (cluster default)", *staging.Spec.StorageClassName)
	}
}

// TestExposeTwinWhenTargetBound: a bound target selects pv-twin. The twin PV must copy the
// volume source verbatim, carry reclaimPolicy Retain ALWAYS (the no-data-loss guarantee),
// the caller labels + the twin role marker, and a two-sided pre-bind with the staging claim
// (explicit class pointer so the defaulter can never break a classless bind).
func TestExposeTwinWhenTargetBound(t *testing.T) {
	pvc, pv := boundTargetFixture()
	c := newClient(t, pvc, pv)
	e := NewTargetExposer(c, opNS)

	ex, err := e.Expose(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Expose: %v", err)
	}
	if ex.Kind != KindTwin || ex.TwinPVName != "recover-1-uploads-twin" {
		t.Fatalf("exposure = %+v, want twin recover-1-uploads-twin", ex)
	}

	var twin corev1.PersistentVolume
	if err := c.Get(context.Background(), client.ObjectKey{Name: ex.TwinPVName}, &twin); err != nil {
		t.Fatalf("twin PV not created: %v", err)
	}
	if twin.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		t.Errorf("twin reclaimPolicy = %v, want Retain (must never delete the user's volume)", twin.Spec.PersistentVolumeReclaimPolicy)
	}
	if twin.Spec.CSI == nil || twin.Spec.CSI.VolumeHandle != "0001-abc-volume" {
		t.Errorf("twin CSI source = %+v, want the bound PV's volumeHandle", twin.Spec.CSI)
	}
	if twin.Labels[apiconst.LabelPVRole] != apiconst.PVRoleTwin {
		t.Errorf("twin labels = %v, want pv-role=twin", twin.Labels)
	}
	if twin.Spec.ClaimRef == nil || twin.Spec.ClaimRef.Namespace != opNS || twin.Spec.ClaimRef.Name != ex.StagingPVCName {
		t.Errorf("twin claimRef = %+v, want reserved for %s/%s", twin.Spec.ClaimRef, opNS, ex.StagingPVCName)
	}

	var staging corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: opNS, Name: ex.StagingPVCName}, &staging); err != nil {
		t.Fatalf("staging PVC not created: %v", err)
	}
	if staging.Spec.VolumeName != ex.TwinPVName {
		t.Errorf("staging.volumeName = %q, want %s", staging.Spec.VolumeName, ex.TwinPVName)
	}
	if staging.Spec.StorageClassName == nil || *staging.Spec.StorageClassName != "fast-rbd" {
		t.Errorf("staging class = %v, want explicit fast-rbd", staging.Spec.StorageClassName)
	}

	// A twin is ready only once its pre-bound claim is Bound.
	if ready, err := e.Ready(context.Background(), ex); err != nil || ready {
		t.Errorf("Ready(twin, pending) = %v, %v; want false, nil", ready, err)
	}
	staging.Status.Phase = corev1.ClaimBound
	if err := c.Status().Update(context.Background(), &staging); err != nil {
		t.Fatalf("simulate staging bound: %v", err)
	}
	if ready, err := e.Ready(context.Background(), ex); err != nil || !ready {
		t.Errorf("Ready(twin, bound) = %v, %v; want true, nil", ready, err)
	}
}

// TestExposeTwinSameNodePin: exactly one live VolumeAttachment for the underlying volume
// pins the mover to that node; several (RWX) or none pin nothing.
func TestExposeTwinSameNodePin(t *testing.T) {
	va := func(name, node, pvName string, attached bool) *storagev1.VolumeAttachment {
		return &storagev1.VolumeAttachment{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: storagev1.VolumeAttachmentSpec{
				Attacher: "csi", NodeName: node,
				Source: storagev1.VolumeAttachmentSource{PersistentVolumeName: &pvName},
			},
			Status: storagev1.VolumeAttachmentStatus{Attached: attached},
		}
	}

	pvc, pv := boundTargetFixture()
	c := newClient(t, pvc, pv, va("va-1", "worker-3", pv.Name, true), va("va-2", "worker-9", "other-pv", true))
	ex, err := NewTargetExposer(c, opNS).Expose(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Expose: %v", err)
	}
	if ex.NodeName != "worker-3" {
		t.Errorf("NodeName = %q, want worker-3 (single live attachment)", ex.NodeName)
	}

	pvc2, pv2 := boundTargetFixture()
	c = newClient(t, pvc2, pv2, va("va-1", "worker-3", pv2.Name, true), va("va-2", "worker-4", pv2.Name, true))
	ex, err = NewTargetExposer(c, opNS).Expose(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Expose: %v", err)
	}
	if ex.NodeName != "" {
		t.Errorf("NodeName = %q, want empty (multi-attach ⇒ no pin)", ex.NodeName)
	}
}

// TestExposeRefusesUnboundAndBlockTargets: an existing-but-unbound claim is the caller's
// destructive decision (ErrTargetUnbound), and a Block-mode target is unsupported.
func TestExposeRefusesUnboundAndBlockTargets(t *testing.T) {
	pending := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "uploads", Namespace: tenantNS},
		Spec:       corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	_, err := NewTargetExposer(newClient(t, pending), opNS).Expose(context.Background(), testRequest())
	if !errors.Is(err, ErrTargetUnbound) {
		t.Errorf("Expose(pending target) error = %v, want ErrTargetUnbound", err)
	}

	block := corev1.PersistentVolumeBlock
	blockPVC, pv := boundTargetFixture()
	blockPVC.Spec.VolumeMode = &block
	_, err = NewTargetExposer(newClient(t, blockPVC, pv), opNS).Expose(context.Background(), testRequest())
	if !errors.Is(err, ErrBlockUnsupported) {
		t.Errorf("Expose(block target) error = %v, want ErrBlockUnsupported", err)
	}
}

// TestFinalizeTransplantHandover walks the whole multi-pass handover, playing the PV
// controller between passes: label+Retain+staging-delete → (Released) → re-point + final
// claim created pre-bound with provenance and NO operator labels → (Bound) → class policy
// restored + labels stripped → done.
func TestFinalizeTransplantHandover(t *testing.T) {
	ctx := context.Background()
	scPolicy := corev1.PersistentVolumeReclaimDelete
	sc := &storagev1.StorageClass{
		ObjectMeta:    metav1.ObjectMeta{Name: "std"},
		Provisioner:   "any",
		ReclaimPolicy: &scPolicy,
	}

	// The provisioned volume, bound to the staging claim as a real provisioner leaves it.
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-9999"},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeSource:        corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "d", VolumeHandle: "h"}},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			StorageClassName:              "std",
			ClaimRef:                      &corev1.ObjectReference{Namespace: opNS, Name: "recover-1-uploads-target", UID: "uid-s"},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
	}
	staging := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "recover-1-uploads-target", Namespace: opNS, Labels: testLabels()},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: ptrTo("std"),
			VolumeName:       pv.Name,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	c := newClient(t, sc, pv, staging)
	e := NewTargetExposer(c, opNS)
	ex := e.exposure(KindTransplant, testRequest(), "", "")

	// Pass 1: labels + Retain land on the PV, the staging claim is deleted.
	done, err := e.Finalize(ctx, ex)
	if done || err != nil {
		t.Fatalf("Finalize pass 1 = %v, %v; want false, nil", done, err)
	}
	var gotPV corev1.PersistentVolume
	if err := c.Get(ctx, client.ObjectKey{Name: pv.Name}, &gotPV); err != nil {
		t.Fatalf("get PV: %v", err)
	}
	if gotPV.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		t.Fatalf("PV policy after pass 1 = %v, want Retain", gotPV.Spec.PersistentVolumeReclaimPolicy)
	}
	if gotPV.Labels[apiconst.LabelPVRole] != apiconst.PVRoleTransplant {
		t.Fatalf("PV labels after pass 1 = %v, want pv-role=transplant", gotPV.Labels)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: opNS, Name: staging.Name}, &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
		t.Fatalf("staging claim still exists after pass 1 (err=%v), want deleted", err)
	}

	// Play the PV controller: the claim is gone, the volume goes Released.
	gotPV.Status.Phase = corev1.VolumeReleased
	if err := c.Status().Update(ctx, &gotPV); err != nil {
		t.Fatalf("simulate Released: %v", err)
	}

	// Pass 2: claimRef re-pointed, final claim created pre-bound.
	if done, err = e.Finalize(ctx, ex); done || err != nil {
		t.Fatalf("Finalize pass 2 = %v, %v; want false, nil", done, err)
	}
	var final corev1.PersistentVolumeClaim
	if err := c.Get(ctx, client.ObjectKey{Namespace: tenantNS, Name: "uploads"}, &final); err != nil {
		t.Fatalf("final claim not created: %v", err)
	}
	if final.Spec.VolumeName != pv.Name {
		t.Errorf("final.volumeName = %q, want %s", final.Spec.VolumeName, pv.Name)
	}
	if final.Annotations[apiconst.AnnotationRestoredFrom] != "daily-20260718-010000" {
		t.Errorf("final annotations = %v, want restored-from provenance", final.Annotations)
	}
	if len(final.Labels) != 0 {
		t.Errorf("final labels = %v, want NONE (the user's object, never reaper-selectable)", final.Labels)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: pv.Name}, &gotPV); err != nil {
		t.Fatalf("get PV: %v", err)
	}
	if gotPV.Spec.ClaimRef == nil || gotPV.Spec.ClaimRef.Namespace != tenantNS || gotPV.Spec.ClaimRef.Name != "uploads" {
		t.Fatalf("PV claimRef after pass 2 = %+v, want %s/uploads", gotPV.Spec.ClaimRef, tenantNS)
	}

	// Play the binder: the final claim binds.
	final.Status.Phase = corev1.ClaimBound
	if err := c.Status().Update(ctx, &final); err != nil {
		t.Fatalf("simulate final bound: %v", err)
	}

	// Pass 3: done — class policy restored, operator labels stripped.
	if done, err = e.Finalize(ctx, ex); !done || err != nil {
		t.Fatalf("Finalize pass 3 = %v, %v; want true, nil", done, err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: pv.Name}, &gotPV); err != nil {
		t.Fatalf("get PV: %v", err)
	}
	if gotPV.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
		t.Errorf("PV policy after handover = %v, want the class's Delete", gotPV.Spec.PersistentVolumeReclaimPolicy)
	}
	for k := range testLabels() {
		if _, ok := gotPV.Labels[k]; ok {
			t.Errorf("PV still carries operator label %q after handover", k)
		}
	}
	if _, ok := gotPV.Labels[apiconst.LabelPVRole]; ok {
		t.Errorf("PV still carries the pv-role label after handover")
	}

	// Finalize is idempotent once done: a re-drive (fresh process, same names) stays done.
	if done, err = e.Finalize(ctx, ex); !done || err != nil {
		t.Errorf("Finalize after completion = %v, %v; want true, nil", done, err)
	}

	// A twin finalize is a no-op.
	if done, err = e.Finalize(ctx, &TargetExposure{Kind: KindTwin}); !done || err != nil {
		t.Errorf("Finalize(twin) = %v, %v; want true, nil", done, err)
	}
}

// TestCleanupTwin removes the staging claim and the twin PV OBJECT (Retain policy —
// asserted intact right up to deletion so the underlying volume is provably safe).
func TestCleanupTwin(t *testing.T) {
	ctx := context.Background()
	pvc, pv := boundTargetFixture()
	c := newClient(t, pvc, pv)
	e := NewTargetExposer(c, opNS)
	ex, err := e.Expose(ctx, testRequest())
	if err != nil {
		t.Fatalf("Expose: %v", err)
	}

	if err := e.Cleanup(ctx, ex); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: opNS, Name: ex.StagingPVCName}, &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
		t.Errorf("staging claim survives cleanup (err=%v)", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: ex.TwinPVName}, &corev1.PersistentVolume{}); !apierrors.IsNotFound(err) {
		t.Errorf("twin PV survives cleanup (err=%v)", err)
	}
	// The user's own PV and claim are untouched.
	if err := c.Get(ctx, client.ObjectKey{Name: pv.Name}, &corev1.PersistentVolume{}); err != nil {
		t.Errorf("user PV gone after cleanup: %v", err)
	}
	// Cleanup is idempotent.
	if err := e.Cleanup(ctx, ex); err != nil {
		t.Errorf("second Cleanup: %v", err)
	}
}

// TestCleanupFailedTransplantReclaims: a labeled mid-handover PV whose final claim never
// bound belongs to a failed restore — cleanup flips its policy back to Delete so the
// provisioner reclaims the storage, and removes the staging claim.
func TestCleanupFailedTransplantReclaims(t *testing.T) {
	ctx := context.Background()
	labels := testLabels()
	labels[apiconst.LabelPVRole] = apiconst.PVRoleTransplant
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-9999", Labels: labels},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeSource:        corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "d", VolumeHandle: "h"}},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			StorageClassName:              "std",
			ClaimRef:                      &corev1.ObjectReference{Namespace: opNS, Name: "recover-1-uploads-target", UID: "u"},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeReleased},
	}
	c := newClient(t, pv)
	e := NewTargetExposer(c, opNS)
	ex := e.exposure(KindTransplant, testRequest(), "", "")

	if err := e.Cleanup(ctx, ex); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	var gotPV corev1.PersistentVolume
	if err := c.Get(ctx, client.ObjectKey{Name: pv.Name}, &gotPV); err != nil {
		t.Fatalf("get PV: %v", err)
	}
	if gotPV.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
		t.Errorf("failed-transplant PV policy = %v, want Delete (reclaim)", gotPV.Spec.PersistentVolumeReclaimPolicy)
	}
}

// TestCleanupCompletedTransplantFinishesTail: if the final claim IS bound to the labeled PV
// (a crash between the bind and the label strip), Cleanup must complete the handover tail —
// never reclaim delivered data.
func TestCleanupCompletedTransplantFinishesTail(t *testing.T) {
	ctx := context.Background()
	labels := testLabels()
	labels[apiconst.LabelPVRole] = apiconst.PVRoleTransplant
	scPolicy := corev1.PersistentVolumeReclaimDelete
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "std"}, Provisioner: "any", ReclaimPolicy: &scPolicy}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-9999", Labels: labels},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeSource:        corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "d", VolumeHandle: "h"}},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			StorageClassName:              "std",
			ClaimRef:                      &corev1.ObjectReference{Namespace: tenantNS, Name: "uploads"},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
	}
	final := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "uploads", Namespace: tenantNS},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: ptrTo("std"),
			VolumeName:       pv.Name,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	c := newClient(t, sc, pv, final)
	e := NewTargetExposer(c, opNS)
	ex := e.exposure(KindTransplant, testRequest(), "", "")

	if err := e.Cleanup(ctx, ex); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	var gotPV corev1.PersistentVolume
	if err := c.Get(ctx, client.ObjectKey{Name: pv.Name}, &gotPV); err != nil {
		t.Fatalf("get PV: %v", err)
	}
	if _, ok := gotPV.Labels[apiconst.LabelPVRole]; ok {
		t.Errorf("delivered PV still labeled after Cleanup tail: %v", gotPV.Labels)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: tenantNS, Name: "uploads"}, &corev1.PersistentVolumeClaim{}); err != nil {
		t.Errorf("delivered final claim gone after Cleanup: %v", err)
	}
}
