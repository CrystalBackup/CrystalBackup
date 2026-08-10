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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// absentOK reports whether err means the object — or its whole KIND — cannot exist, which a
// teardown treats as success. NotFound is the obvious case; NoKindMatch is its cluster-level
// twin: on a cluster where the external-snapshotter CRDs were never installed, no
// VolumeSnapshot/VolumeSnapshotContent can exist either, so their collection is vacuously
// complete. That case is reachable: a Backup can go terminal without ever exposing (every volume
// Skipped as unsupported, or a pre-hook failure before the first snapshot), and the terminal
// re-entry sweep still tears it down by derived name.
func absentOK(err error) bool {
	return apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err)
}

// cleanup is the shared Cleanup() body and the leak-check contract (ADR 0003 step 5). It tears
// the exposure down in the EXACT order the ADR pins, and every step tolerates NotFound so a
// partial re-run — the happy-path Cleanup retried, or the orphan reaper sweeping by label after
// a crash — always converges:
//
//  1. Delete the temp PVC (operator ns): the consumer of the static snapshot goes first.
//  2. Delete the static VolumeSnapshot (operator ns) and static VolumeSnapshotContent (cluster).
//     Both are deletionPolicy=Retain, so ONLY the objects go — the storage snapshot is untouched.
//  3. Restore the origin VolumeSnapshotContent's deletionPolicy to Delete (undoing Ready's
//     handover Retain patch), THEN delete the origin VolumeSnapshot — its now-Delete content
//     reclaims the storage-side snapshot EXACTLY ONCE.
//
// Ordering is load-bearing: reclamation (step 3) happens last and only after the static
// re-bind (which shares the same snapshotHandle) is gone, so the storage snapshot can never be
// destroyed while something still points at it.
func cleanup(ctx context.Context, c client.Client, ex *Exposure) error {
	// Step 1: temp PVC.
	if err := deletePVC(ctx, c, ex.OperatorNamespace, ex.TempPVCName); err != nil {
		return err
	}

	// Step 2: static pair (objects only — Retain).
	if err := deleteVolumeSnapshot(ctx, c, ex.OperatorNamespace, ex.StaticVSName); err != nil {
		return err
	}
	if err := deleteVolumeSnapshotContent(ctx, c, ex.StaticVSCName); err != nil {
		return err
	}

	// Step 3: restore the origin content to Delete, then delete the origin snapshot to reclaim.
	return reclaimOrigin(ctx, c, ex)
}

// reclaimOrigin performs Cleanup step 3: restore the origin VolumeSnapshotContent to
// deletionPolicy=Delete and delete the origin VolumeSnapshot so the storage snapshot is reclaimed
// exactly once.
//
// The normal path (origin VolumeSnapshot still present): resolve its bound content via
// status.boundVolumeSnapshotContentName, patch that content back to Delete, THEN delete the
// snapshot. If a crash interrupts between the restore and the delete, a re-run simply repeats
// both (the restore is a no-op the second time, the delete tolerates NotFound) and converges.
//
// The crash-window path (origin VolumeSnapshot already gone): if the process died AFTER Ready's
// Retain patch but the origin snapshot has since been deleted by something else, the origin
// content is left Retain-orphaned and would LEAK its storage snapshot. reclaimOrphanOriginVSC is
// the best-effort recovery — it finds that content by our exposure labels (Ready stamped them in
// the same patch) and restore-then-deletes it. The orphan reaper (M1 task #22) is the ultimate
// backstop for the residual window where even the labelled content cannot be found (e.g. the
// patch itself never landed) or Cleanup never runs at all.
func reclaimOrigin(ctx context.Context, c client.Client, ex *Exposure) error {
	originVS := newUnstructured(volumeSnapshotGVK())
	err := c.Get(ctx, client.ObjectKey{Namespace: ex.OriginNamespace, Name: ex.OriginVSName}, originVS)
	switch {
	case err == nil:
		boundVSCName, _, nErr := unstructured.NestedString(originVS.Object, "status", "boundVolumeSnapshotContentName")
		if nErr != nil {
			return fmt.Errorf("read origin VolumeSnapshot %s/%s boundVolumeSnapshotContentName: %w", ex.OriginNamespace, ex.OriginVSName, nErr)
		}
		if boundVSCName != "" {
			if rErr := restoreOriginVSCByName(ctx, c, boundVSCName); rErr != nil {
				return rErr
			}
		}
		// Now the origin content is Delete again: deleting the snapshot reclaims the storage
		// snapshot exactly once.
		return deleteVolumeSnapshot(ctx, c, ex.OriginNamespace, ex.OriginVSName)

	case apierrors.IsNotFound(err):
		return reclaimOrphanOriginVSC(ctx, c, ex)

	case apimeta.IsNoMatchError(err):
		return nil // no snapshot CRDs on this cluster ⇒ no VS/VSC can exist to reclaim.

	default:
		return fmt.Errorf("get origin VolumeSnapshot %s/%s: %w", ex.OriginNamespace, ex.OriginVSName, err)
	}
}

// reclaimOrphanOriginVSC handles the crash-window where the origin VolumeSnapshot is already gone
// but its Retain-patched content may still be orphaned, holding the storage snapshot. It lists
// VolumeSnapshotContents carrying our exposure labels, skips our own static content (which shares
// those labels but is already deleted by Cleanup step 2), and restore-then-deletes each match:
// setting deletionPolicy=Delete and deleting the content directly makes the CSI snapshotter
// reclaim the storage snapshot (a Retain-orphaned content would otherwise linger forever). With
// no labels to select on there is nothing safe to do here, so it defers entirely to the reaper.
//
// A label whose value is EMPTY is one of those "nothing safe to do" cases, and learning that cost
// 0.6.5 a leaked cluster-scoped content and five failed leak checks. The caller's identity map for a
// namespace-plane backup used to contain crystalbackup.io/cluster-backup="" (no ClusterBackup parent
// to name), and MatchingLabels turns that into `cluster-backup=`, which selects objects whose value
// for that key is the empty STRING. The origin content this function hunts for does not carry the key
// at all — it is label-stamped by patchOriginVSCForHandover, and mergeLabels skips a key whose desired
// value equals the missing-and-therefore-"" current one. So the selector was built from labels the
// target provably could not have, matched nothing, and reported a clean reclaim on every namespace-
// plane teardown.
//
// The guard below refuses that selector instead of issuing it. It cannot repair it: labels are opaque
// to this package by design (nothing here imports apiconst), so we cannot know which key was meant to
// identify the owner, and dropping the empty key would leave a selector that no longer pins one owner
// — on the namespace plane, (managed-by, namespace, pvc) alone matches a CONCURRENT run's origin
// content for the same PVC, and this function's reaction to a match is restore-then-DELETE. Reclaiming
// a live backup's snapshot is far worse than deferring to the reaper, which is the documented backstop
// for exactly this "cannot select safely" case. The real fix is upstream, where the map is built:
// controller.exposureLabels stamps no empty values and names the owner on both planes.
func reclaimOrphanOriginVSC(ctx context.Context, c client.Client, ex *Exposure) error {
	if len(ex.Labels) == 0 {
		return nil // nothing to select on; the orphan reaper is the backstop.
	}
	for key, value := range ex.Labels {
		if value == "" {
			return fmt.Errorf("exposure label %q has an empty value, so no selector built from these"+
				" labels can identify this exposure's origin VolumeSnapshotContent: refusing the"+
				" crash-window reclaim and leaving it to the orphan reaper", key)
		}
	}
	list := newUnstructuredList(volumeSnapshotContentGVK())
	if err := c.List(ctx, list, client.MatchingLabels(ex.Labels)); err != nil {
		if apimeta.IsNoMatchError(err) {
			return nil // no snapshot CRDs on this cluster ⇒ nothing Retain-parked to reclaim.
		}
		return fmt.Errorf("list origin VolumeSnapshotContents by exposure labels: %w", err)
	}
	for i := range list.Items {
		item := &list.Items[i]
		if item.GetName() == ex.StaticVSCName {
			continue // our own static content, already handled in step 2
		}
		if err := setOriginVSCToDelete(ctx, c, item); err != nil {
			return err
		}
		if err := c.Delete(ctx, item); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete orphaned origin VolumeSnapshotContent %s: %w", item.GetName(), err)
		}
	}
	return nil
}

// restoreOriginVSCByName GETs the named origin content and, if present, restores its
// deletionPolicy to Delete. A content already gone (NotFound) is fine — the snapshot it guarded
// is already being reclaimed or was never Retain-patched.
func restoreOriginVSCByName(ctx context.Context, c client.Client, name string) error {
	vsc := newUnstructured(volumeSnapshotContentGVK())
	if err := c.Get(ctx, client.ObjectKey{Name: name}, vsc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get origin VolumeSnapshotContent %s: %w", name, err)
	}
	return setOriginVSCToDelete(ctx, c, vsc)
}

// setOriginVSCToDelete patches vsc's deletionPolicy to Delete via a merge patch, idempotently
// (already-Delete issues no write). This is the undo of Ready's handover Retain patch; once
// applied, deleting the content or its bound snapshot reclaims the storage-side snapshot.
func setOriginVSCToDelete(ctx context.Context, c client.Client, vsc *unstructured.Unstructured) error {
	dp, _, err := unstructured.NestedString(vsc.Object, "spec", "deletionPolicy")
	if err != nil {
		return fmt.Errorf("read origin VolumeSnapshotContent %s spec.deletionPolicy: %w", vsc.GetName(), err)
	}
	if dp == deletionPolicyDelete {
		return nil
	}
	base := vsc.DeepCopy()
	if err := unstructured.SetNestedField(vsc.Object, deletionPolicyDelete, "spec", "deletionPolicy"); err != nil {
		return fmt.Errorf("set origin VolumeSnapshotContent %s spec.deletionPolicy=Delete: %w", vsc.GetName(), err)
	}
	if err := c.Patch(ctx, vsc, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("restore origin VolumeSnapshotContent %s deletionPolicy to Delete: %w", vsc.GetName(), err)
	}
	return nil
}

// deletePVC deletes a PVC by namespace/name, tolerating NotFound.
func deletePVC(ctx context.Context, c client.Client, namespace, name string) error {
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if err := c.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete temp PVC %s/%s: %w", namespace, name, err)
	}
	return nil
}

// deleteVolumeSnapshot deletes a (namespaced) VolumeSnapshot by namespace/name, tolerating both
// NotFound and a cluster with no snapshot CRDs at all (absentOK).
func deleteVolumeSnapshot(ctx context.Context, c client.Client, namespace, name string) error {
	vs := newUnstructured(volumeSnapshotGVK())
	vs.SetNamespace(namespace)
	vs.SetName(name)
	if err := c.Delete(ctx, vs); err != nil && !absentOK(err) {
		return fmt.Errorf("delete VolumeSnapshot %s/%s: %w", namespace, name, err)
	}
	return nil
}

// deleteVolumeSnapshotContent deletes a (cluster-scoped) VolumeSnapshotContent by name, tolerating
// both NotFound and a cluster with no snapshot CRDs at all (absentOK).
func deleteVolumeSnapshotContent(ctx context.Context, c client.Client, name string) error {
	vsc := newUnstructured(volumeSnapshotContentGVK())
	vsc.SetName(name)
	if err := c.Delete(ctx, vsc); err != nil && !absentOK(err) {
		return fmt.Errorf("delete VolumeSnapshotContent %s: %w", name, err)
	}
	return nil
}
