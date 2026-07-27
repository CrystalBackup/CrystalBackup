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

// TeardownExposure (teardown.go) is the derive-only teardown entry point the leak audit demanded:
// cleanup by deterministic identity, with no PVC read and — above all — no ability to CREATE.
// These tests pin the derivation against expose()'s own names, the never-creates property, and
// the exact crash-window residue the fanout observed on real infrastructure (static pair already
// collected, origin VSC still Retain-parked) being reclaimed on re-entry.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// TestDeriveExposureMatchesExpose pins the derivation against the one source of truth: the
// Exposure expose() itself returns. If a name ever drifts between the create path and the
// derive path, teardown would silently delete the WRONG (nonexistent) objects and leak the
// real ones — precisely the class of bug this file exists to make impossible.
func TestDeriveExposureMatchesExpose(t *testing.T) {
	req := testExposeRequest()
	want := testExposure() // testExposure() builds exactly what expose() returns for req

	got := deriveExposure(req.Namespace, testOperatorNamespace, req.NamePrefix, req.Labels)

	// Kind, StorageClass and Capacity parameterise CREATION and are deliberately zero on a
	// derived exposure; blank them on the expectation so the comparison pins everything else.
	want.Kind = ""
	want.StorageClass = ""
	want.Capacity = got.Capacity

	if !reflect.DeepEqual(got, want) {
		t.Errorf("deriveExposure diverged from expose()'s Exposure:\n got %+v\nwant %+v", got, want)
	}
}

// TestTeardownExposureNeverCreates is the audit's "cleanup path can create" pin, inverted into a
// guarantee: a full teardown over a complete exposure — and a re-run over nothing at all — must
// issue ZERO Create (or Update) calls. The old reconstruct-via-Expose shape re-created the origin
// VolumeSnapshot mid-teardown; a fresh unbound VS then defeated the Retain→Delete restore.
func TestTeardownExposureNeverCreates(t *testing.T) {
	ctx := context.Background()
	var creates, updates int
	c := newCountingHandoverClient(t, &creates, &updates)
	ex := testExposure()
	seedExposureObjects(t, c, ex)
	creates, updates = 0, 0 // the seeding above legitimately creates; the teardown must not

	req := testExposeRequest()
	if err := TeardownExposure(ctx, c, req.Namespace, testOperatorNamespace, req.NamePrefix, req.Labels); err != nil {
		t.Fatalf("TeardownExposure: %v", err)
	}
	if creates != 0 || updates != 0 {
		t.Errorf("teardown issued %d Create and %d Update calls; a teardown must only delete/patch", creates, updates)
	}

	// Everything is gone — teardown by derived identity really tore the seeded exposure down.
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(ctx, client.ObjectKey{Namespace: ex.OperatorNamespace, Name: ex.TempPVCName}, &pvc); !apierrors.IsNotFound(err) {
		t.Errorf("temp PVC survived teardown (err=%v, want NotFound)", err)
	}
	if unstructuredExists(t, c, volumeSnapshotContentGVK(), "", ex.StaticVSCName) {
		t.Errorf("static VolumeSnapshotContent survived teardown")
	}
	if unstructuredExists(t, c, volumeSnapshotGVK(), ex.OriginNamespace, ex.OriginVSName) {
		t.Errorf("origin VolumeSnapshot survived teardown")
	}

	// The re-run over a fully-clean cluster: still no creates, still no error.
	if err := TeardownExposure(ctx, c, req.Namespace, testOperatorNamespace, req.NamePrefix, req.Labels); err != nil {
		t.Fatalf("TeardownExposure re-run over nothing: %v", err)
	}
	if creates != 0 || updates != 0 {
		t.Errorf("re-run issued %d Create and %d Update calls over an empty cluster", creates, updates)
	}
}

// TestTeardownExposureReclaimsStepTwoResidue re-creates the EXACT residue the fanout's leak-check
// recorded on real infrastructure (snapcontent-b355c739…, the audit's confirmed shape): the
// process died mid-cleanup with step 1-2 done — temp PVC and static pair already collected — and
// step 3 not: the origin VolumeSnapshot gone, its dynamic origin VolumeSnapshotContent still
// Retain-parked, exposure-labelled, and owner-less. A TeardownExposure re-entry (the terminal
// sweep, or the reaper) must find it by label and restore-then-delete it, or the storage-side
// snapshot leaks forever.
func TestTeardownExposureReclaimsStepTwoResidue(t *testing.T) {
	ctx := context.Background()
	c := newHandoverClient(t)
	ex := testExposure()
	seedExposureObjects(t, c, ex)

	// Steps 1-2 done: temp PVC + static pair already deleted by the interrupted pass.
	if err := deletePVC(ctx, c, ex.OperatorNamespace, ex.TempPVCName); err != nil {
		t.Fatalf("set up: delete temp PVC: %v", err)
	}
	if err := deleteVolumeSnapshot(ctx, c, ex.OperatorNamespace, ex.StaticVSName); err != nil {
		t.Fatalf("set up: delete static VS: %v", err)
	}
	if err := deleteVolumeSnapshotContent(ctx, c, ex.StaticVSCName); err != nil {
		t.Fatalf("set up: delete static VSC: %v", err)
	}
	// Step 3 interrupted after the origin VS delete, before the content reclaim.
	if err := deleteVolumeSnapshot(ctx, c, ex.OriginNamespace, ex.OriginVSName); err != nil {
		t.Fatalf("set up: delete origin VS: %v", err)
	}
	if !unstructuredExists(t, c, volumeSnapshotContentGVK(), "", originVSCName) {
		t.Fatalf("set up broken: the Retain-parked origin VSC should still exist")
	}

	req := testExposeRequest()
	if err := TeardownExposure(ctx, c, req.Namespace, testOperatorNamespace, req.NamePrefix, req.Labels); err != nil {
		t.Fatalf("TeardownExposure re-entry: %v", err)
	}

	if unstructuredExists(t, c, volumeSnapshotContentGVK(), "", originVSCName) {
		t.Errorf("Retain-parked origin VolumeSnapshotContent survived the re-entry — the observed leak shape would persist")
	}
}

// TestAbsentOK pins the teardown's absence classifier: NotFound and NoKindMatch (no snapshot
// CRDs installed — so no VS/VSC can exist) both read as "already gone"; anything else is a real
// error the sweep must retry on.
func TestAbsentOK(t *testing.T) {
	notFound := apierrors.NewNotFound(schema.GroupResource{Group: volumeSnapshotGroup, Resource: "volumesnapshots"}, "x")
	noMatch := &apimeta.NoKindMatchError{GroupKind: schema.GroupKind{Group: volumeSnapshotGroup, Kind: "VolumeSnapshot"}}

	if !absentOK(notFound) {
		t.Errorf("NotFound must read as absent")
	}
	if !absentOK(noMatch) {
		t.Errorf("NoKindMatch (snapshot CRDs not installed) must read as absent")
	}
	if !absentOK(fmt.Errorf("wrapped: %w", noMatch)) {
		t.Errorf("a wrapped NoKindMatch must still read as absent")
	}
	if absentOK(errors.New("connection refused")) {
		t.Errorf("a transport error must NOT read as absent — the sweep has to retry it")
	}
}

// newCountingHandoverClient is newHandoverClient plus interceptors counting every Create/Update —
// the never-creates property's measuring instrument.
func newCountingHandoverClient(t *testing.T, creates, updates *int) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(corev1): %v", err)
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				*creates++
				return c.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				*updates++
				return c.Update(ctx, obj, opts...)
			},
		}).
		Build()
}
