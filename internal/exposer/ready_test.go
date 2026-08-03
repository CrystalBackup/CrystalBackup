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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The Ready state machine (ready.go) IS unit-testable against the fake client by MANUALLY
// simulating the status a real CSI driver / snapshot-controller would set: the fake client
// stores unstructured VolumeSnapshot/VolumeSnapshotContent (status writable via Update) and typed
// PVCs (status writable via Status().Update). These tests drive the full ADR 0003 handover —
// origin snapshot ready -> Retain patch -> static VSC/VS -> temp PVC -> Bound — for BOTH Kinds.
//
// What stays crucible-deferred (test/crucible/tests/m1_*): whether a real driver ACTUALLY flips
// readyToUse, whether the clone ACTUALLY mounts, and whether the storage snapshot is ACTUALLY
// reclaimed on the restored Delete policy. The fake client only proves this package issues the
// right API calls in the right order against simulated status — not the storage-side effects.

const (
	// originVSCName is the name a snapshot-controller would generate for the dynamic snapshot's
	// bound content; the tests set it as the origin VS's boundVolumeSnapshotContentName and
	// create a matching cluster-scoped content.
	originVSCName = "snapcontent-origin-7f3a"
	// testDriver / testSnapshotHandle are the storage-side identity the origin content advertises
	// and the static content must copy verbatim.
	testDriver         = "rook-ceph.rbd.csi.ceph.com"
	testSnapshotHandle = "0001-0009-rook-ceph-0000000000000001-snap-abc"
)

// newHandoverClient builds a fake client with only corev1 in scheme (the typed temp PVC); the
// unstructured VolumeSnapshot/VolumeSnapshotContent GVKs auto-register on first Create.
func newHandoverClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(corev1): %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).Build()
}

// getUnstructured GETs obj by (ns, name) at gvk, failing the test if it is absent.
func getUnstructured(t *testing.T, c client.Client, gvk schema.GroupVersionKind, ns, name string) *unstructured.Unstructured {
	t.Helper()
	u := newUnstructured(gvk)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, u); err != nil {
		t.Fatalf("get %s %s/%s: %v", gvk.Kind, ns, name, err)
	}
	return u
}

// unstructuredExists reports whether obj exists at (ns, name)/gvk without failing the test.
func unstructuredExists(t *testing.T, c client.Client, gvk schema.GroupVersionKind, ns, name string) bool {
	t.Helper()
	u := newUnstructured(gvk)
	err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, u)
	return err == nil
}

// simulateOriginReady plays the role of the CSI driver + snapshot-controller for the ORIGIN
// snapshot: it flips the dynamic VolumeSnapshot to readyToUse with a bound content name and a
// restoreSize, and creates that cluster-scoped content with a driver + snapshotHandle and an
// initial deletionPolicy=Delete (what a dynamically-provisioned content starts at, so the tests
// can prove Ready patches it to Retain).
func simulateOriginReady(t *testing.T, c client.Client, ex *Exposure, restoreSize string) {
	t.Helper()
	ctx := context.Background()

	vs := getUnstructured(t, c, volumeSnapshotGVK(), ex.OriginNamespace, ex.OriginVSName)
	mustSet(t, vs, true, "status", "readyToUse")
	mustSet(t, vs, originVSCName, "status", "boundVolumeSnapshotContentName")
	mustSet(t, vs, restoreSize, "status", "restoreSize")
	if err := c.Update(ctx, vs); err != nil {
		t.Fatalf("simulate origin VS ready: %v", err)
	}

	vsc := newUnstructured(volumeSnapshotContentGVK())
	vsc.SetName(originVSCName)
	vsc.SetLabels(map[string]string{"snapshot.storage.kubernetes.io/managed-by": "csi-snapshotter"})
	mustSet(t, vsc, deletionPolicyDelete, "spec", "deletionPolicy")
	mustSet(t, vsc, testDriver, "spec", "driver")
	mustSet(t, vsc, testSnapshotHandle, "status", "snapshotHandle")
	if err := c.Create(ctx, vsc); err != nil {
		t.Fatalf("simulate origin VSC: %v", err)
	}
}

// simulateStaticReady plays the role of the snapshot-controller + CSI provisioner for the STATIC
// side: static VolumeSnapshot readyToUse, temp PVC Bound. After this, Ready's step 7 must return
// true.
func simulateStaticReady(t *testing.T, c client.Client, ex *Exposure) {
	t.Helper()
	ctx := context.Background()

	vs := getUnstructured(t, c, volumeSnapshotGVK(), ex.OperatorNamespace, ex.StaticVSName)
	mustSet(t, vs, true, "status", "readyToUse")
	if err := c.Update(ctx, vs); err != nil {
		t.Fatalf("simulate static VS ready: %v", err)
	}

	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(ctx, client.ObjectKey{Namespace: ex.OperatorNamespace, Name: ex.TempPVCName}, &pvc); err != nil {
		t.Fatalf("get temp PVC to bind: %v", err)
	}
	pvc.Status.Phase = corev1.ClaimBound
	if err := c.Status().Update(ctx, &pvc); err != nil {
		t.Fatalf("simulate temp PVC Bound: %v", err)
	}
}

// mustSet sets a nested field, failing the test on the (here unreachable) error.
func mustSet(t *testing.T, u *unstructured.Unstructured, value interface{}, path ...string) {
	t.Helper()
	if err := unstructured.SetNestedField(u.Object, value, path...); err != nil {
		t.Fatalf("set %v: %v", path, err)
	}
}

// TestReadyDrivesHandover is the headline test: for BOTH exposers, Expose creates the origin
// snapshot; then, with the origin snapshot simulated ready, Ready must (1) patch the origin
// content to Retain + our labels, (2) create the static content with the right
// snapshotHandle/driver/volumeSnapshotRef, (3) create the static snapshot bound to it, (4) create
// the temp PVC in the operator namespace with the right dataSource/accessMode/size, and return
// false until (5) the static snapshot is ready AND the temp PVC is Bound, at which point it
// returns true.
func TestReadyDrivesHandover(t *testing.T) {
	cases := []struct {
		name           string
		newExposer     func(c client.Client) SnapshotExposer
		wantAccessMode corev1.PersistentVolumeAccessMode
	}{
		{"csi-generic", func(c client.Client) SnapshotExposer {
			return newCSIGenericExposer(c, testOperatorNamespace, newVolumeSnapshotClass("ceph-block-snapclass", rbdProvisioner))
		}, corev1.ReadWriteOnce},
		{"cephfs-shallow", func(c client.Client) SnapshotExposer {
			return newCephFSShallowExposer(c, testOperatorNamespace, newVolumeSnapshotClass("cephfs-snapclass", cephfsProvisioner))
		}, corev1.ReadOnlyMany},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c := newHandoverClient(t)
			e := tc.newExposer(c)

			ex, err := e.Expose(ctx, testExposeRequest())
			if err != nil {
				t.Fatalf("Expose: %v", err)
			}
			// Before the origin snapshot is ready, Ready is a no-op false.
			if ready, err := e.Ready(ctx, ex); err != nil || ready {
				t.Fatalf("Ready before origin readyToUse = (%v, %v), want (false, nil)", ready, err)
			}
			// No static objects or temp PVC yet.
			if unstructuredExists(t, c, volumeSnapshotContentGVK(), "", ex.StaticVSCName) {
				t.Errorf("static VSC created before origin snapshot ready")
			}

			// The origin snapshot becomes ready; restoreSize (12Gi) exceeds Capacity (10Gi) to
			// prove Ready upsizes the temp PVC to the snapshot's restoreSize.
			simulateOriginReady(t, c, ex, "12Gi")

			ready, err := e.Ready(ctx, ex)
			if err != nil {
				t.Fatalf("Ready (post origin-ready): %v", err)
			}
			if ready {
				t.Fatalf("Ready = true before static snapshot ready / temp PVC Bound, want false")
			}

			// (1) origin content patched to Retain + carries our labels.
			originVSC := getUnstructured(t, c, volumeSnapshotContentGVK(), "", originVSCName)
			assertNestedString(t, originVSC.Object, deletionPolicyRetain, "spec", "deletionPolicy")
			for k, v := range ex.Labels {
				if originVSC.GetLabels()[k] != v {
					t.Errorf("origin VSC missing handover label %s=%s (labels=%v)", k, v, originVSC.GetLabels())
				}
			}
			// Its own provisioner label must be preserved (the patch adds, never replaces).
			if originVSC.GetLabels()["snapshot.storage.kubernetes.io/managed-by"] != "csi-snapshotter" {
				t.Errorf("origin VSC lost its provisioner label after the Retain patch: %v", originVSC.GetLabels())
			}

			// (2) static content: same handle/driver, Retain, forward volumeSnapshotRef.
			staticVSC := getUnstructured(t, c, volumeSnapshotContentGVK(), "", ex.StaticVSCName)
			assertNestedString(t, staticVSC.Object, deletionPolicyRetain, "spec", "deletionPolicy")
			assertNestedString(t, staticVSC.Object, testDriver, "spec", "driver")
			assertNestedString(t, staticVSC.Object, testSnapshotHandle, "spec", "source", "snapshotHandle")
			assertNestedString(t, staticVSC.Object, ex.StaticVSName, "spec", "volumeSnapshotRef", "name")
			assertNestedString(t, staticVSC.Object, ex.OperatorNamespace, "spec", "volumeSnapshotRef", "namespace")

			// (3) static snapshot bound to the static content, in the operator namespace.
			staticVS := getUnstructured(t, c, volumeSnapshotGVK(), ex.OperatorNamespace, ex.StaticVSName)
			assertNestedString(t, staticVS.Object, ex.StaticVSCName, "spec", "source", "volumeSnapshotContentName")

			// (4) temp PVC: operator namespace, static-VS dataSource, right access mode, upsized.
			var pvc corev1.PersistentVolumeClaim
			if err := c.Get(ctx, client.ObjectKey{Namespace: ex.OperatorNamespace, Name: ex.TempPVCName}, &pvc); err != nil {
				t.Fatalf("get temp PVC: %v", err)
			}
			if pvc.Namespace != ex.OperatorNamespace {
				t.Errorf("temp PVC namespace = %q, want %q", pvc.Namespace, ex.OperatorNamespace)
			}
			assertDataSource(t, pvc.Spec.DataSource, ex.StaticVSName)
			if pvc.Spec.AccessModes[0] != tc.wantAccessMode {
				t.Errorf("temp PVC accessMode = %v, want %v", pvc.Spec.AccessModes, tc.wantAccessMode)
			}
			want12Gi := resource.MustParse("12Gi")
			if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(want12Gi) != 0 {
				t.Errorf("temp PVC requested storage = %v, want 12Gi (upsized to restoreSize)", got)
			}

			// (5) once the static snapshot is ready and the temp PVC is Bound, Ready flips true.
			simulateStaticReady(t, c, ex)
			ready, err = e.Ready(ctx, ex)
			if err != nil {
				t.Fatalf("Ready (post static-ready): %v", err)
			}
			if !ready {
				t.Fatalf("Ready = false after static snapshot ready + temp PVC Bound, want true")
			}
		})
	}
}

// TestReadyIsIdempotent proves calling Ready repeatedly (while still not consumable) neither
// errors nor creates duplicate objects: names are deterministic and every Create tolerates
// AlreadyExists, and the Retain patch is a no-op once applied.
func TestReadyIsIdempotent(t *testing.T) {
	ctx := context.Background()
	c := newHandoverClient(t)
	e := newCSIGenericExposer(c, testOperatorNamespace, newVolumeSnapshotClass("ceph-block-snapclass", rbdProvisioner))

	ex, err := e.Expose(ctx, testExposeRequest())
	if err != nil {
		t.Fatalf("Expose: %v", err)
	}
	simulateOriginReady(t, c, ex, "10Gi")

	for i := 0; i < 3; i++ {
		if ready, err := e.Ready(ctx, ex); err != nil || ready {
			t.Fatalf("Ready call #%d = (%v, %v), want (false, nil)", i+1, ready, err)
		}
	}

	// Exactly two VolumeSnapshotContents exist (origin + static), never duplicates.
	vscList := newUnstructuredList(volumeSnapshotContentGVK())
	if err := c.List(ctx, vscList); err != nil {
		t.Fatalf("list VolumeSnapshotContent: %v", err)
	}
	if len(vscList.Items) != 2 {
		t.Errorf("VolumeSnapshotContent count = %d, want 2 (origin + static), no duplicates", len(vscList.Items))
	}

	// Exactly two VolumeSnapshots exist (origin dynamic + static), never duplicates.
	vsList := newUnstructuredList(volumeSnapshotGVK())
	if err := c.List(ctx, vsList); err != nil {
		t.Fatalf("list VolumeSnapshot: %v", err)
	}
	if len(vsList.Items) != 2 {
		t.Errorf("VolumeSnapshot count = %d, want 2 (origin + static), no duplicates", len(vsList.Items))
	}
}

// TestResolveTempPVCCapacity pins the size floor: the temp PVC is max(requested, restoreSize).
// A restoreSize larger than the source PVC's request (a resized volume) grows the temp PVC; an
// absent, smaller, or garbage restoreSize leaves it floored at the requested capacity.
func TestResolveTempPVCCapacity(t *testing.T) {
	requested := resource.MustParse("10Gi")

	cases := []struct {
		name        string
		restoreSize interface{} // string set at status.restoreSize, or nil for absent
		want        string
	}{
		{"restoreSize larger -> upsize", "12Gi", "12Gi"},
		{"restoreSize smaller -> floor at requested", "5Gi", "10Gi"},
		{"restoreSize equal -> requested", "10Gi", "10Gi"},
		{"restoreSize absent -> requested", nil, "10Gi"},
		{"restoreSize garbage -> requested", "not-a-quantity", "10Gi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			originVS := newUnstructured(volumeSnapshotGVK())
			if tc.restoreSize != nil {
				mustSet(t, originVS, tc.restoreSize, "status", "restoreSize")
			}
			got := resolveTempPVCCapacity(requested, originVS)
			want := resource.MustParse(tc.want)
			if got.Cmp(want) != 0 {
				t.Errorf("resolveTempPVCCapacity = %v, want %v", got.String(), want.String())
			}
		})
	}
}

// TestOriginProgress covers the other half of delta 9: the signal that tells a caller apart a
// snapshot that is SLOW from one that nobody is listening to. Ready reports "not yet" identically
// for both, so this is the only place the distinction exists.
func TestOriginProgress(t *testing.T) {
	cases := []struct {
		name string
		// mutate plays whatever the CSI stack did (or did not do) to the origin snapshot's status.
		mutate           func(t *testing.T, c client.Client, ex *Exposure)
		wantOK           bool
		wantAcknowledged bool
	}{
		{
			// Freshly created by Expose, untouched: the shape a dead/absent snapshotter leaves
			// behind forever. This is the ONLY combination that may ever justify failing a volume.
			name:   "untouched by any snapshotter",
			mutate: func(*testing.T, client.Client, *Exposure) {},
			wantOK: true,
		},
		{
			// The external snapshot-controller bound a content: the request was received. Whatever
			// happens next, this is not abandonment, however long readyToUse takes.
			name: "bound content — acknowledged, even though it is nowhere near ready",
			mutate: func(t *testing.T, c client.Client, ex *Exposure) {
				vs := getUnstructured(t, c, volumeSnapshotGVK(), ex.OriginNamespace, ex.OriginVSName)
				mustSet(t, vs, originVSCName, "status", "boundVolumeSnapshotContentName")
				if err := c.Update(context.Background(), vs); err != nil {
					t.Fatalf("set boundVolumeSnapshotContentName: %v", err)
				}
			},
			wantOK:           true,
			wantAcknowledged: true,
		},
		{
			// Something tried and said why it could not. That is a diagnosed failure with its own
			// reporting path — never a stall, and never this deadline's business.
			name: "recorded error — acknowledged",
			mutate: func(t *testing.T, c client.Client, ex *Exposure) {
				vs := getUnstructured(t, c, volumeSnapshotGVK(), ex.OriginNamespace, ex.OriginVSName)
				mustSet(t, vs, "failed to create snapshot: rpc error", "status", "error", "message")
				if err := c.Update(context.Background(), vs); err != nil {
					t.Fatalf("set status.error: %v", err)
				}
			},
			wantOK:           true,
			wantAcknowledged: true,
		},
		{
			// The snapshot is gone (cleaned up, RBAC, no CRDs). ok=false — and the caller's
			// contract is that it must conclude NOTHING from this, least of all "no progress".
			name: "origin snapshot unreadable — unknown, not 'no progress'",
			mutate: func(t *testing.T, c client.Client, ex *Exposure) {
				vs := getUnstructured(t, c, volumeSnapshotGVK(), ex.OriginNamespace, ex.OriginVSName)
				if err := c.Delete(context.Background(), vs); err != nil {
					t.Fatalf("delete origin VS: %v", err)
				}
			},
		},
		{
			// No creationTimestamp means no clock, and a deadline measured from a zero time is
			// "started in 1970" — i.e. instantly, catastrophically expired. Unknown, not stalled.
			name: "no creationTimestamp — unknown, never 'expired long ago'",
			mutate: func(t *testing.T, c client.Client, ex *Exposure) {
				vs := getUnstructured(t, c, volumeSnapshotGVK(), ex.OriginNamespace, ex.OriginVSName)
				vs.SetCreationTimestamp(metav1.Time{})
				if err := c.Update(context.Background(), vs); err != nil {
					t.Fatalf("clear creationTimestamp: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c := newHandoverClient(t)
			e := newCSIGenericExposer(c, testOperatorNamespace, newVolumeSnapshotClass("ceph-block-snapclass", rbdProvisioner))

			ex, err := e.Expose(ctx, testExposeRequest())
			if err != nil {
				t.Fatalf("Expose: %v", err)
			}
			// The fake client does not populate metadata.creationTimestamp on an unstructured
			// object the way an API server does, and that field IS the exposure's clock (see
			// StartedAt / SnapshotProgress). Stamp it here so these cases measure what they claim
			// to; the "no creationTimestamp" case below then clears it back on purpose.
			stampOriginCreation(t, c, ex, time.Now().Add(-3*time.Minute))
			tc.mutate(t, c, ex)

			progress, ok := e.Progress(ctx, ex)
			if ok != tc.wantOK {
				t.Fatalf("Progress ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if progress.Acknowledged != tc.wantAcknowledged {
				t.Errorf("Acknowledged = %v, want %v", progress.Acknowledged, tc.wantAcknowledged)
			}
			if progress.StartedAt.IsZero() {
				t.Error("StartedAt is zero — the deadline has no clock to measure against")
			}
		})
	}
}

// stampOriginCreation sets the origin VolumeSnapshot's metadata.creationTimestamp, standing in for
// the API server (which the fake client does not emulate for unstructured objects).
func stampOriginCreation(t *testing.T, c client.Client, ex *Exposure, at time.Time) {
	t.Helper()
	vs := getUnstructured(t, c, volumeSnapshotGVK(), ex.OriginNamespace, ex.OriginVSName)
	vs.SetCreationTimestamp(metav1.NewTime(at))
	if err := c.Update(context.Background(), vs); err != nil {
		t.Fatalf("stamp origin VS creationTimestamp: %v", err)
	}
}
