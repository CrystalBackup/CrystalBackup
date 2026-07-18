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
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Cleanup (cleanup.go) is the leak-check contract (ADR 0003 step 5): it tears an exposure down
// in a fixed order and, crucially, RESTORES the origin VolumeSnapshotContent's deletionPolicy to
// Delete before deleting the origin VolumeSnapshot, so the storage-side snapshot is reclaimed
// exactly once and never leaked. These tests drive the shared cleanup() body against the fake
// client — asserting the API calls it makes (delete order, the Retain->Delete restore, the
// crash-window orphan sweep). What a real cluster does in response (actually reclaiming the
// storage snapshot) is crucible-deferred, as ready_test.go's header explains.

// seedExposureObjects creates, in c, every object a completed Expose+Ready leaves behind: the
// temp PVC + static VS (operator ns) + static VSC (cluster, Retain), the origin dynamic VS
// (origin ns, bound to originVSCName), and the origin VSC — Retain-patched and carrying the
// exposure labels, exactly the state Ready leaves it in mid-handover. It is the starting point
// for every teardown test.
func seedExposureObjects(t *testing.T, c client.Client, ex *Exposure) {
	t.Helper()
	ctx := context.Background()

	if err := c.Create(ctx, buildTempPVC(ex, ex.Capacity)); err != nil {
		t.Fatalf("seed temp PVC: %v", err)
	}
	if err := c.Create(ctx, buildStaticVolumeSnapshot(ex)); err != nil {
		t.Fatalf("seed static VolumeSnapshot: %v", err)
	}
	if err := c.Create(ctx, buildStaticVolumeSnapshotContent(ex, testDriver, testSnapshotHandle)); err != nil {
		t.Fatalf("seed static VolumeSnapshotContent: %v", err)
	}

	// Origin dynamic VolumeSnapshot (origin ns), bound to originVSCName.
	originVS := newUnstructured(volumeSnapshotGVK())
	originVS.SetNamespace(ex.OriginNamespace)
	originVS.SetName(ex.OriginVSName)
	originVS.SetLabels(ex.Labels)
	mustSet(t, originVS, originVSCName, "status", "boundVolumeSnapshotContentName")
	if err := c.Create(ctx, originVS); err != nil {
		t.Fatalf("seed origin VolumeSnapshot: %v", err)
	}

	// Origin VolumeSnapshotContent — the Retain-patched, exposure-labelled state Ready leaves it
	// in (its provisioner label preserved alongside ours, since Ready's patch adds not replaces).
	originVSC := newUnstructured(volumeSnapshotContentGVK())
	originVSC.SetName(originVSCName)
	labels := map[string]string{"snapshot.storage.kubernetes.io/managed-by": "csi-snapshotter"}
	for k, v := range ex.Labels {
		labels[k] = v
	}
	originVSC.SetLabels(labels)
	mustSet(t, originVSC, deletionPolicyRetain, "spec", "deletionPolicy")
	mustSet(t, originVSC, testDriver, "spec", "driver")
	mustSet(t, originVSC, testSnapshotHandle, "status", "snapshotHandle")
	if err := c.Create(ctx, originVSC); err != nil {
		t.Fatalf("seed origin VolumeSnapshotContent: %v", err)
	}
}

// TestCleanupTeardownOrderAndReclaim pins the happy path: from a full exposure, cleanup removes
// the temp PVC and the static VS/VSC pair, and — the load-bearing part — RESTORES the origin
// VolumeSnapshotContent to deletionPolicy=Delete before deleting the origin VolumeSnapshot, so a
// real cluster reclaims the storage-side snapshot exactly once instead of leaking a
// Retain-orphaned one.
func TestCleanupTeardownOrderAndReclaim(t *testing.T) {
	ctx := context.Background()
	c := newHandoverClient(t)
	ex := testExposure()
	seedExposureObjects(t, c, ex)

	if err := cleanup(ctx, c, ex); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	// Step 1: temp PVC gone.
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(ctx, client.ObjectKey{Namespace: ex.OperatorNamespace, Name: ex.TempPVCName}, &pvc); !apierrors.IsNotFound(err) {
		t.Errorf("temp PVC still present after cleanup (err=%v, want NotFound)", err)
	}
	// Step 2: static pair gone (objects only; a real cluster's Retain policy leaves the storage snapshot).
	if unstructuredExists(t, c, volumeSnapshotGVK(), ex.OperatorNamespace, ex.StaticVSName) {
		t.Errorf("static VolumeSnapshot still present after cleanup")
	}
	if unstructuredExists(t, c, volumeSnapshotContentGVK(), "", ex.StaticVSCName) {
		t.Errorf("static VolumeSnapshotContent still present after cleanup")
	}
	// Step 3: origin VolumeSnapshot deleted, AND its content restored to Delete first — the
	// reclaim guarantee. (The fake client has no CSI to cascade-delete the content; asserting it
	// is now Delete-policy proves cleanup undid the handover Retain patch before the delete.)
	if unstructuredExists(t, c, volumeSnapshotGVK(), ex.OriginNamespace, ex.OriginVSName) {
		t.Errorf("origin VolumeSnapshot still present after cleanup")
	}
	originVSC := getUnstructured(t, c, volumeSnapshotContentGVK(), "", originVSCName)
	assertNestedString(t, originVSC.Object, deletionPolicyDelete, "spec", "deletionPolicy")
}

// TestCleanupIsIdempotent proves a re-run converges: calling cleanup twice never errors and never
// resurrects an object. The second call takes the crash-window branch (origin VS already gone by
// then) and must still be a clean finish — exactly what the orphan reaper relies on when it
// re-invokes Cleanup on labelled leftovers.
func TestCleanupIsIdempotent(t *testing.T) {
	ctx := context.Background()
	c := newHandoverClient(t)
	ex := testExposure()
	seedExposureObjects(t, c, ex)

	if err := cleanup(ctx, c, ex); err != nil {
		t.Fatalf("cleanup #1: %v", err)
	}
	if err := cleanup(ctx, c, ex); err != nil {
		t.Fatalf("cleanup #2 (idempotent re-run): %v", err)
	}

	// The static pair and temp PVC stay gone; nothing came back.
	if unstructuredExists(t, c, volumeSnapshotGVK(), ex.OperatorNamespace, ex.StaticVSName) {
		t.Errorf("static VolumeSnapshot reappeared after the second cleanup")
	}
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(ctx, client.ObjectKey{Namespace: ex.OperatorNamespace, Name: ex.TempPVCName}, &pvc); !apierrors.IsNotFound(err) {
		t.Errorf("temp PVC reappeared after the second cleanup (err=%v, want NotFound)", err)
	}
}

// TestCleanupCrashWindowOriginVSGone exercises the ADR risk-table path: the operator crashed
// AFTER Ready's Retain patch, and the origin VolumeSnapshot has since been deleted by something
// else, leaving its Retain-patched content orphaned — which would leak the storage-side snapshot
// forever. cleanup must find that content by our exposure labels and reclaim it (restore to
// Delete + delete), never mistaking our own static content for it.
func TestCleanupCrashWindowOriginVSGone(t *testing.T) {
	ctx := context.Background()
	c := newHandoverClient(t)
	ex := testExposure()
	seedExposureObjects(t, c, ex)

	// Simulate the crash window: the origin dynamic VolumeSnapshot is already gone.
	originVS := newUnstructured(volumeSnapshotGVK())
	originVS.SetNamespace(ex.OriginNamespace)
	originVS.SetName(ex.OriginVSName)
	if err := c.Delete(ctx, originVS); err != nil {
		t.Fatalf("delete origin VolumeSnapshot to set up the crash window: %v", err)
	}

	if err := cleanup(ctx, c, ex); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	// The orphaned origin content — found by exposure labels — is reclaimed, so nothing leaks.
	if unstructuredExists(t, c, volumeSnapshotContentGVK(), "", originVSCName) {
		t.Errorf("orphaned origin VolumeSnapshotContent %s survived cleanup (would leak its storage snapshot)", originVSCName)
	}
	// And our own static content was still torn down (not confused for the orphan, not skipped).
	if unstructuredExists(t, c, volumeSnapshotContentGVK(), "", ex.StaticVSCName) {
		t.Errorf("static VolumeSnapshotContent survived cleanup")
	}
}
