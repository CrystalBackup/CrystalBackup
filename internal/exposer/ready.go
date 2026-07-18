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

package exposer

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ready is the shared Ready() body and the DRIVER of ADR 0003's static VS/VSC re-bind
// (§"The csi-generic flow", steps 1-6). It is idempotent and reconstructs everything by Get, so
// the Backup controller can call it every reconcile — including after a restart — and it
// converges: each object it needs is either already present (adopted) or created afresh, and
// the origin-VSC Retain patch is a no-op once applied. It never blocks; it reports readiness and
// returns.
//
// buildTempPVC is the ONLY thing that differs between the two exposers: csi-generic injects the
// ReadWriteOnce builder, cephfs-shallow the ReadOnlyMany one (ADR step 3 / "cephfs-shallow"
// replaces only the temp PVC). Everything else — steps 1-2, 4-5, 7 — is identical, which is why
// it lives here once.
//
// The sequence (all steps tolerant of a real CSI driver still catching up, reported as
// not-ready rather than an error, so the controller simply polls again):
//
//  1. Origin VolumeSnapshot readyToUse? If not -> (false, nil).
//  2. Resolve its bound VolumeSnapshotContent; read spec.driver + status.snapshotHandle.
//  3. Patch that origin VSC to deletionPolicy=Retain (+ stamp our labels) — the handover guard.
//  4. Create the static, pre-provisioned VolumeSnapshotContent against the same snapshotHandle.
//  5. Create the static VolumeSnapshot (operator ns) bound to it.
//  6. Create the temp PVC (operator ns) from the static snapshot, sized max(Capacity, restoreSize).
//  7. Ready == static VolumeSnapshot readyToUse AND temp PVC Bound.
func ready(
	ctx context.Context,
	c client.Client,
	ex *Exposure,
	buildTempPVC func(ex *Exposure, capacity resource.Quantity) *corev1.PersistentVolumeClaim,
) (bool, error) {
	// Step 1: is the origin dynamic snapshot usable yet?
	originVS := newUnstructured(volumeSnapshotGVK())
	if err := c.Get(ctx, client.ObjectKey{Namespace: ex.OriginNamespace, Name: ex.OriginVSName}, originVS); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil // Expose's Create not visible yet; poll again.
		}
		return false, fmt.Errorf("get origin VolumeSnapshot %s/%s: %w", ex.OriginNamespace, ex.OriginVSName, err)
	}
	originReady, _, err := unstructured.NestedBool(originVS.Object, "status", "readyToUse")
	if err != nil {
		return false, fmt.Errorf("read origin VolumeSnapshot %s/%s status.readyToUse: %w", ex.OriginNamespace, ex.OriginVSName, err)
	}
	if !originReady {
		return false, nil
	}

	// Step 2: resolve the bound content and read the storage-side identity from it.
	boundVSCName, _, err := unstructured.NestedString(originVS.Object, "status", "boundVolumeSnapshotContentName")
	if err != nil {
		return false, fmt.Errorf("read origin VolumeSnapshot %s/%s boundVolumeSnapshotContentName: %w", ex.OriginNamespace, ex.OriginVSName, err)
	}
	if boundVSCName == "" {
		return false, nil // readyToUse set before the bound content name lands; poll again.
	}
	originVSC := newUnstructured(volumeSnapshotContentGVK())
	if err := c.Get(ctx, client.ObjectKey{Name: boundVSCName}, originVSC); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get origin VolumeSnapshotContent %s: %w", boundVSCName, err)
	}
	driver, _, err := unstructured.NestedString(originVSC.Object, "spec", "driver")
	if err != nil {
		return false, fmt.Errorf("read origin VolumeSnapshotContent %s spec.driver: %w", boundVSCName, err)
	}
	snapshotHandle, _, err := unstructured.NestedString(originVSC.Object, "status", "snapshotHandle")
	if err != nil {
		return false, fmt.Errorf("read origin VolumeSnapshotContent %s status.snapshotHandle: %w", boundVSCName, err)
	}
	if driver == "" || snapshotHandle == "" {
		return false, nil // content bound but its status not fully populated; poll again.
	}

	// Step 3: guard the storage snapshot for the handover — Retain + our labels, idempotently.
	if err := patchOriginVSCForHandover(ctx, c, originVSC, ex.Labels); err != nil {
		return false, err
	}

	// Step 4: the static, pre-provisioned content pointing at the same snapshotHandle.
	staticVSC := buildStaticVolumeSnapshotContent(ex, driver, snapshotHandle)
	if err := c.Create(ctx, staticVSC); err != nil && !apierrors.IsAlreadyExists(err) {
		return false, fmt.Errorf("create static VolumeSnapshotContent %s: %w", ex.StaticVSCName, err)
	}

	// Step 5: the static snapshot in the operator namespace bound to that content.
	staticVS := buildStaticVolumeSnapshot(ex)
	if err := c.Create(ctx, staticVS); err != nil && !apierrors.IsAlreadyExists(err) {
		return false, fmt.Errorf("create static VolumeSnapshot %s/%s: %w", ex.OperatorNamespace, ex.StaticVSName, err)
	}

	// Step 6: the temp PVC from the static snapshot, floored at the source capacity but grown to
	// the snapshot's restoreSize when that is larger (a snapshot can be bigger than the PVC's
	// requested size, and a too-small temp PVC would fail to provision).
	capacity := resolveTempPVCCapacity(ex.Capacity, originVS)
	tempPVC := buildTempPVC(ex, capacity)
	if err := c.Create(ctx, tempPVC); err != nil && !apierrors.IsAlreadyExists(err) {
		return false, fmt.Errorf("create temp PVC %s/%s: %w", ex.OperatorNamespace, ex.TempPVCName, err)
	}

	// Step 7: consumable == static snapshot readyToUse AND temp PVC Bound.
	staticReady, err := volumeSnapshotReady(ctx, c, ex.OperatorNamespace, ex.StaticVSName)
	if err != nil || !staticReady {
		return false, err
	}
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(ctx, client.ObjectKey{Namespace: ex.OperatorNamespace, Name: ex.TempPVCName}, &pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get temp PVC %s/%s: %w", ex.OperatorNamespace, ex.TempPVCName, err)
	}
	return pvc.Status.Phase == corev1.ClaimBound, nil
}

// patchOriginVSCForHandover sets the origin VolumeSnapshotContent's deletionPolicy to Retain and
// stamps ex.Labels onto it, via a merge patch, so (a) no intermediate delete during the handover
// can destroy the storage-side snapshot (ADR 0003 step 2 / the Risks table) and (b) Cleanup and
// the orphan reaper can later FIND this externally-created object by our labels. It is
// idempotent: if the content is already Retain and already carries our labels it issues no write
// at all, so re-driving Ready is free. The patch is computed against a pre-modification deep
// copy, so it touches only deletionPolicy and the added label keys — never the driver-owned
// status.snapshotHandle Ready just read from it.
func patchOriginVSCForHandover(ctx context.Context, c client.Client, vsc *unstructured.Unstructured, labels map[string]string) error {
	base := vsc.DeepCopy()

	changed := false
	dp, _, err := unstructured.NestedString(vsc.Object, "spec", "deletionPolicy")
	if err != nil {
		return fmt.Errorf("read origin VolumeSnapshotContent %s spec.deletionPolicy: %w", vsc.GetName(), err)
	}
	if dp != deletionPolicyRetain {
		if err := unstructured.SetNestedField(vsc.Object, deletionPolicyRetain, "spec", "deletionPolicy"); err != nil {
			return fmt.Errorf("set origin VolumeSnapshotContent %s spec.deletionPolicy=Retain: %w", vsc.GetName(), err)
		}
		changed = true
	}
	if mergeLabels(vsc, labels) {
		changed = true
	}
	if !changed {
		return nil
	}
	if err := c.Patch(ctx, vsc, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patch origin VolumeSnapshotContent %s to Retain for handover: %w", vsc.GetName(), err)
	}
	return nil
}

// buildStaticVolumeSnapshotContent constructs the cluster-scoped, pre-provisioned
// VolumeSnapshotContent that re-binds the origin's storage-side snapshot into the operator
// namespace (ADR 0003 step 2). deletionPolicy is Retain — OBJECTS ONLY: the underlying storage
// snapshot is owned by the ORIGIN content, so tearing this pair down in Cleanup must not reclaim
// it. spec.source.snapshotHandle is the same handle the origin content advertises;
// spec.volumeSnapshotRef points forward at the static VolumeSnapshot (buildStaticVolumeSnapshot)
// that will bind to it.
//
// spec.volumeSnapshotClassName is intentionally OMITTED: it is a dynamic-provisioning input, and
// a pre-provisioned content derives everything it needs from spec.driver + the snapshotHandle.
// Omitting it also keeps the Exposure fully self-describing (no class field to carry across a
// restart). See the package report.
//
// Errors from SetNestedField are discarded for the same reason as buildVolumeSnapshot: every
// path below is a fresh subtree of a freshly-allocated object, so the one error case (a
// pre-existing non-map value along the path) is unreachable.
func buildStaticVolumeSnapshotContent(ex *Exposure, driver, snapshotHandle string) *unstructured.Unstructured {
	vsc := newUnstructured(volumeSnapshotContentGVK())
	vsc.SetName(ex.StaticVSCName) // cluster-scoped: no namespace
	vsc.SetLabels(ex.Labels)

	_ = unstructured.SetNestedField(vsc.Object, deletionPolicyRetain, "spec", "deletionPolicy")
	_ = unstructured.SetNestedField(vsc.Object, driver, "spec", "driver")
	_ = unstructured.SetNestedField(vsc.Object, snapshotHandle, "spec", "source", "snapshotHandle")
	_ = unstructured.SetNestedField(vsc.Object, volumeSnapshotGVK().GroupVersion().String(), "spec", "volumeSnapshotRef", "apiVersion")
	_ = unstructured.SetNestedField(vsc.Object, dataSourceKindVolumeSnapshot, "spec", "volumeSnapshotRef", "kind")
	_ = unstructured.SetNestedField(vsc.Object, ex.StaticVSName, "spec", "volumeSnapshotRef", "name")
	_ = unstructured.SetNestedField(vsc.Object, ex.OperatorNamespace, "spec", "volumeSnapshotRef", "namespace")

	return vsc
}

// buildStaticVolumeSnapshot constructs the static VolumeSnapshot in the operator namespace bound
// to the static content by name (spec.source.volumeSnapshotContentName) — the pre-provisioned
// binding form, so it too omits volumeSnapshotClassName. The temp PVC's dataSource references
// THIS snapshot (same namespace).
func buildStaticVolumeSnapshot(ex *Exposure) *unstructured.Unstructured {
	vs := newUnstructured(volumeSnapshotGVK())
	vs.SetName(ex.StaticVSName)
	vs.SetNamespace(ex.OperatorNamespace)
	vs.SetLabels(ex.Labels)

	_ = unstructured.SetNestedField(vs.Object, ex.StaticVSCName, "spec", "source", "volumeSnapshotContentName")

	return vs
}

// resolveTempPVCCapacity returns the larger of the requested capacity (the source PVC's size) and
// the origin snapshot's status.restoreSize. A snapshot's restoreSize can exceed the PVC's
// requested storage (e.g. a resized volume), and a temp PVC smaller than the snapshot fails to
// provision; flooring at requested keeps a missing/garbage restoreSize harmless.
func resolveTempPVCCapacity(requested resource.Quantity, originVS *unstructured.Unstructured) resource.Quantity {
	restore := readRestoreSize(originVS)
	if restore != nil && restore.Cmp(requested) > 0 {
		return *restore
	}
	return requested
}

// readRestoreSize reads originVS.status.restoreSize as a resource.Quantity, or nil when it is
// absent or unparseable. A VolumeSnapshot serialises restoreSize as a Quantity string (e.g.
// "10Gi"); anything else (missing, a non-string encoding, garbage) is treated as "no floor
// bump" rather than an error, since resolveTempPVCCapacity only ever uses it to grow the PVC.
func readRestoreSize(originVS *unstructured.Unstructured) *resource.Quantity {
	raw, found, err := unstructured.NestedString(originVS.Object, "status", "restoreSize")
	if err != nil || !found || raw == "" {
		return nil
	}
	q, err := resource.ParseQuantity(raw)
	if err != nil {
		return nil
	}
	return &q
}
