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

// ReapOrphanVolumeSnapshotContent (reap.go) is the orphan reaper's per-content reclaim: the
// policy-correct halves of cleanup(), callable one object at a time. These pins guard the two
// semantics that keep a storage snapshot from being either leaked or double-destroyed.

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// TestReapOrphanVSCDynamicRestoresThenDeletes pins the reaper-facing reclaim semantics on the
// leak audit's exact residual shape: a DYNAMIC (volumeHandle-sourced), Retain-parked, labelled
// origin content. The deletionPolicy restore MUST land before the delete — the order is what
// makes the CSI snapshotter reclaim the storage-side snapshot instead of orphaning it at the
// backend forever.
func TestReapOrphanVSCDynamicRestoresThenDeletes(t *testing.T) {
	ctx := context.Background()
	var events []string
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(corev1): %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				events = append(events, "patch:"+obj.GetName())
				return cl.Patch(ctx, obj, patch, opts...)
			},
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				events = append(events, "delete:"+obj.GetName())
				return cl.Delete(ctx, obj, opts...)
			},
		}).Build()

	vsc := newUnstructured(volumeSnapshotContentGVK())
	vsc.SetName("snapcontent-orphan")
	vsc.SetLabels(testExposeRequest().Labels)
	mustSet(t, vsc, deletionPolicyRetain, "spec", "deletionPolicy")
	mustSet(t, vsc, "csi-vol-handle", "spec", "source", "volumeHandle") // dynamic provenance
	if err := c.Create(ctx, vsc); err != nil {
		t.Fatalf("seed dynamic VSC: %v", err)
	}
	events = nil

	if err := ReapOrphanVolumeSnapshotContent(ctx, c, vsc); err != nil {
		t.Fatalf("ReapOrphanVolumeSnapshotContent: %v", err)
	}

	want := []string{"patch:snapcontent-orphan", "delete:snapcontent-orphan"}
	if !reflect.DeepEqual(events, want) {
		t.Errorf("reap order = %v, want %v (restore deletionPolicy BEFORE the delete)", events, want)
	}
	if unstructuredExists(t, c, volumeSnapshotContentGVK(), "", "snapcontent-orphan") {
		t.Errorf("orphaned dynamic VSC survived the reap")
	}
}

// TestReapOrphanVSCStaticIsObjectOnly pins the other half: a PRE-PROVISIONED (snapshotHandle-
// sourced) content — our static re-bind alias — is deleted object-only, with NO deletionPolicy
// patch: the backend snapshot is the origin content's to reclaim, and patching an alias to
// Delete before removing it would destroy the snapshot out from under that reclaim.
func TestReapOrphanVSCStaticIsObjectOnly(t *testing.T) {
	ctx := context.Background()
	var patches int
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(corev1): %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patches++
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).Build()

	ex := testExposure()
	if err := c.Create(ctx, buildStaticVolumeSnapshotContent(ex, testDriver, testSnapshotHandle)); err != nil {
		t.Fatalf("seed static VSC: %v", err)
	}

	got := getUnstructured(t, c, volumeSnapshotContentGVK(), "", ex.StaticVSCName)
	if !IsPreProvisionedContent(got) {
		t.Fatalf("the static re-bind content must classify as pre-provisioned")
	}
	if err := ReapOrphanVolumeSnapshotContent(ctx, c, got); err != nil {
		t.Fatalf("ReapOrphanVolumeSnapshotContent: %v", err)
	}
	if patches != 0 {
		t.Errorf("static content reap issued %d Patch call(s); an alias must be deleted object-only", patches)
	}
	if unstructuredExists(t, c, volumeSnapshotContentGVK(), "", ex.StaticVSCName) {
		t.Errorf("static VSC survived the reap")
	}
}
