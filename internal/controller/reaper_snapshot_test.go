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

// The reaper's VolumeSnapshot/VolumeSnapshotContent sweep (reapSnapshotObjects) — the backstop
// the leak audit proved was promised in four comments and implemented in none. Fake-client tests
// rather than envtest: the envtest suite deliberately runs without the external snapshot CRDs
// (its exposer is a stub), which is itself pinned here — a NoMatch cluster must skip silently.
// The per-content reclaim SEMANTICS (restore-then-delete vs object-only) are pinned in
// internal/exposer/teardown_test.go; these tests pin SELECTION: who gets reaped, who is spared.

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

const reaperTestOperatorNS = "crystal-backup-system"

func newReaperClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(clientgo): %v", err)
	}
	if err := cbv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(cbv1): %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).
		// Without this, the fake client's Status().Update reports the status SUBRESOURCE as
		// NotFound for Backup — the tests seed volume phases through exactly that call.
		WithStatusSubresource(&cbv1.Backup{}).
		Build()
}

// seedSnapshotResidue creates the audit's residual shape for (run, ns, pvc): a Retain-parked
// DYNAMIC origin content plus an origin VolumeSnapshot and a static (pre-provisioned) content,
// all exposure-labelled.
func seedSnapshotResidue(t *testing.T, c client.Client, run, ns, pvc string) (originVSC, staticVSC, originVS string) {
	t.Helper()
	ctx := context.Background()
	labels := exposureLabelsFor(run, ns, pvc)
	prefix := moverNamePrefix(ns, run, pvc)
	// The fake client does not stamp creationTimestamp on unstructured objects; a zero timestamp
	// would read as infinitely old, silently defeating the MinAge pin below.
	now := metav1.Now()

	originVSC = "snapcontent-" + run + "-" + pvc
	item := &unstructured.Unstructured{}
	item.SetAPIVersion("snapshot.storage.k8s.io/v1")
	item.SetKind("VolumeSnapshotContent")
	item.SetName(originVSC)
	item.SetCreationTimestamp(now)
	item.SetLabels(labels)
	if err := unstructured.SetNestedField(item.Object, "Retain", "spec", "deletionPolicy"); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(item.Object, "csi-vol-1", "spec", "source", "volumeHandle"); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, item); err != nil {
		t.Fatalf("seed origin VSC: %v", err)
	}

	staticVSC = prefix + "-vsc"
	st := &unstructured.Unstructured{}
	st.SetAPIVersion("snapshot.storage.k8s.io/v1")
	st.SetKind("VolumeSnapshotContent")
	st.SetName(staticVSC)
	st.SetCreationTimestamp(now)
	st.SetLabels(labels)
	if err := unstructured.SetNestedField(st.Object, "Retain", "spec", "deletionPolicy"); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(st.Object, "handle-1", "spec", "source", "snapshotHandle"); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, st); err != nil {
		t.Fatalf("seed static VSC: %v", err)
	}

	originVS = prefix + "-snap"
	vs := &unstructured.Unstructured{}
	vs.SetAPIVersion("snapshot.storage.k8s.io/v1")
	vs.SetKind("VolumeSnapshot")
	vs.SetNamespace(ns)
	vs.SetName(originVS)
	vs.SetCreationTimestamp(now)
	vs.SetLabels(labels)
	if err := c.Create(ctx, vs); err != nil {
		t.Fatalf("seed origin VS: %v", err)
	}
	return originVSC, staticVSC, originVS
}

func vscExists(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("snapshot.storage.k8s.io/v1")
	u.SetKind("VolumeSnapshotContent")
	return c.Get(context.Background(), client.ObjectKey{Name: name}, u) == nil
}

func vsExists(t *testing.T, c client.Client, ns, name string) bool {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("snapshot.storage.k8s.io/v1")
	u.SetKind("VolumeSnapshot")
	return c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, u) == nil
}

// TestReapSnapshotObjectsOwnerGone: the pure orphan — the owning Backup no longer exists. Every
// labelled snapshot object must be reaped: static content, origin VolumeSnapshot, and the
// Retain-parked dynamic content (the exact shape the fanout recorded, whose Backup CR the specs
// had long deleted).
func TestReapSnapshotObjectsOwnerGone(t *testing.T) {
	c := newReaperClient(t)
	originVSC, staticVSC, originVS := seedSnapshotResidue(t, c, "run-a", "c-db", "data-db-1")

	r := &OrphanReaper{Client: c, OperatorNamespace: reaperTestOperatorNS, MinAge: 0}
	r.reapSnapshotObjects(context.Background(), time.Now(), nil)

	if vscExists(t, c, originVSC) {
		t.Errorf("Retain-parked dynamic origin VSC survived the sweep — the observed leak shape would persist")
	}
	if vscExists(t, c, staticVSC) {
		t.Errorf("static VSC survived the sweep")
	}
	if vsExists(t, c, "c-db", originVS) {
		t.Errorf("origin VolumeSnapshot survived the sweep")
	}
}

// TestReapSnapshotObjectsSparesLiveWork: an owning Backup whose volume for that PVC is still
// non-terminal is LIVE — reaping its exposure would corrupt an in-flight backup. Nothing may be
// touched, exactly as the native sweep spares live Jobs and clones.
func TestReapSnapshotObjectsSparesLiveWork(t *testing.T) {
	c := newReaperClient(t)
	ctx := context.Background()
	originVSC, staticVSC, originVS := seedSnapshotResidue(t, c, "run-b", "c-db", "data-db-1")

	backup := &cbv1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "c-db", Name: "run-b",
			Labels: map[string]string{apiconst.LabelClusterBackup: "run-b"},
		},
		Spec: cbv1.BackupSpec{
			LocationRef: cbv1.LocationReference{Kind: "ClusterBackupLocation", Name: "loc"},
		},
	}
	if err := c.Create(ctx, backup); err != nil {
		t.Fatal(err)
	}
	backup.Status.Volumes = []cbv1.VolumeStatus{{Pvc: "data-db-1", Phase: status.VolumePhaseSnapshotting}}
	if err := c.Status().Update(ctx, backup); err != nil {
		t.Fatal(err)
	}

	r := &OrphanReaper{Client: c, OperatorNamespace: reaperTestOperatorNS, MinAge: 0}
	r.reapSnapshotObjects(ctx, time.Now(), nil)

	if !vscExists(t, c, originVSC) || !vscExists(t, c, staticVSC) || !vsExists(t, c, "c-db", originVS) {
		t.Errorf("the sweep touched a LIVE exposure (volume still Snapshotting) — that corrupts an in-flight backup")
	}
}

// TestReapSnapshotObjectsRespectsMinAge: even a pure orphan is spared while younger than MinAge —
// the race guard against a slow-but-live reconcile whose status has not caught up.
func TestReapSnapshotObjectsRespectsMinAge(t *testing.T) {
	c := newReaperClient(t)
	originVSC, staticVSC, originVS := seedSnapshotResidue(t, c, "run-c", "c-db", "data-db-1")

	r := &OrphanReaper{Client: c, OperatorNamespace: reaperTestOperatorNS, MinAge: time.Hour}
	// cutoff = now-1h: everything just seeded is far too young.
	r.reapSnapshotObjects(context.Background(), time.Now().Add(-time.Hour), nil)

	if !vscExists(t, c, originVSC) || !vscExists(t, c, staticVSC) || !vsExists(t, c, "c-db", originVS) {
		t.Errorf("the sweep reaped an object younger than MinAge")
	}
}

// TestReapSnapshotObjectsSparesTerminalSweptBackup: a terminal Backup whose volume is terminal IS
// reap-eligible (its teardown should already have run) — pin the positive case with the owner
// still present, since the crucible's residual had its CR deleted but a real cluster's may not be.
func TestReapSnapshotObjectsTerminalOwnerStillReaped(t *testing.T) {
	c := newReaperClient(t)
	ctx := context.Background()
	originVSC, staticVSC, originVS := seedSnapshotResidue(t, c, "run-d", "c-db", "data-db-1")

	backup := &cbv1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "c-db", Name: "run-d",
			Labels: map[string]string{apiconst.LabelClusterBackup: "run-d"},
		},
		Spec: cbv1.BackupSpec{
			LocationRef: cbv1.LocationReference{Kind: "ClusterBackupLocation", Name: "loc"},
		},
	}
	if err := c.Create(ctx, backup); err != nil {
		t.Fatal(err)
	}
	backup.Status.Phase = "Completed"
	backup.Status.Volumes = []cbv1.VolumeStatus{{Pvc: "data-db-1", Phase: status.VolumePhaseCompleted}}
	if err := c.Status().Update(ctx, backup); err != nil {
		t.Fatal(err)
	}

	r := &OrphanReaper{Client: c, OperatorNamespace: reaperTestOperatorNS, MinAge: 0}
	r.reapSnapshotObjects(ctx, time.Now(), nil)

	if vscExists(t, c, originVSC) || vscExists(t, c, staticVSC) || vsExists(t, c, "c-db", originVS) {
		t.Errorf("residue of a terminal volume must be reaped even while its Backup CR still exists")
	}
}
