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
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Real-world-shaped provisioner names, reused across tests so each test's intent (which
// provisioner routes where) is legible without re-deriving the string every time.
const (
	rbdProvisioner       = "rook-ceph.rbd.csi.ceph.com"
	cephfsProvisioner    = "rook-ceph.cephfs.csi.ceph.com"
	localPathProvisioner = "rancher.io/local-path"
	longhornProvisioner  = "driver.longhorn.io"
)

// testOperatorNamespace is the operator namespace every test threads through NewRegistry; the
// resolved exposers carry it (it is where their static VolumeSnapshot + temp PVC would be
// created). apiconst.DefaultOperatorNamespace's value, spelled locally so the test does not
// depend on that package.
const testOperatorNamespace = "crystal-backup-system"

// newRegistryTestClient builds a fake client seeded with typed StorageClasses at Build time
// and unstructured VolumeSnapshotClasses via explicit post-Build Create calls. The split is
// deliberate: controller-runtime's fake client auto-registers an *unstructured.Unstructured's
// GVK into its scheme the moment it is handed to Get/Create/Delete/List (reading the GVK the
// object already carries), so seeding VolumeSnapshotClasses via Create — rather than
// WithObjects, which would require the scheme to already recognise the GVK — needs no
// upfront scheme registration for the (out-of-module) snapshot CRDs at all.
func newRegistryTestClient(t *testing.T, storageClasses []*storagev1.StorageClass, vsClasses []*unstructured.Unstructured) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(corev1): %v", err)
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(storagev1): %v", err)
	}

	objs := make([]client.Object, 0, len(storageClasses))
	for _, sc := range storageClasses {
		objs = append(objs, sc)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	ctx := context.Background()
	for _, vsc := range vsClasses {
		if err := c.Create(ctx, vsc); err != nil {
			t.Fatalf("seed VolumeSnapshotClass %s: %v", vsc.GetName(), err)
		}
	}
	return c
}

// newStorageClass builds a minimal typed StorageClass naming provisioner.
func newStorageClass(name, provisioner string) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: name},
		Provisioner: provisioner,
	}
}

// newVolumeSnapshotClass builds a minimal unstructured VolumeSnapshotClass naming driver —
// the one field Registry.findVolumeSnapshotClass reads.
func newVolumeSnapshotClass(name, driver string) *unstructured.Unstructured {
	vsc := &unstructured.Unstructured{}
	vsc.SetGroupVersionKind(volumeSnapshotClassGVK())
	vsc.SetName(name)
	if err := unstructured.SetNestedField(vsc.Object, driver, "driver"); err != nil {
		panic(err) // fresh object, fresh path: cannot fail (see buildVolumeSnapshot's identical reasoning)
	}
	return vsc
}

// pvcOnStorageClass builds a minimal PVC naming its StorageClass — the only field
// Registry.For reads off the PVC itself.
func pvcOnStorageClass(namespace, name, storageClass string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &storageClass},
	}
}

// TestRegistryForSelectsCSIGenericForRBD pins the default path: an RBD (non-CephFS)
// provisioner with a matching VolumeSnapshotClass resolves to csiGenericExposer, preconfigured
// with the resolved class name.
func TestRegistryForSelectsCSIGenericForRBD(t *testing.T) {
	c := newRegistryTestClient(t,
		[]*storagev1.StorageClass{newStorageClass("ceph-block", rbdProvisioner)},
		[]*unstructured.Unstructured{newVolumeSnapshotClass("ceph-block-snapclass", rbdProvisioner)},
	)
	pvc := pvcOnStorageClass("c-db", "data", "ceph-block")

	exp, err := NewRegistry(c, testOperatorNamespace).For(context.Background(), pvc)
	if err != nil {
		t.Fatalf("For: unexpected error: %v", err)
	}
	if exp.Kind() != KindCSIGeneric {
		t.Errorf("Kind() = %q, want %q", exp.Kind(), KindCSIGeneric)
	}
	gen, ok := exp.(*csiGenericExposer)
	if !ok {
		t.Fatalf("resolved exposer type = %T, want *csiGenericExposer", exp)
	}
	if gen.volumeSnapshotClass != "ceph-block-snapclass" {
		t.Errorf("resolved VolumeSnapshotClass = %q, want %q", gen.volumeSnapshotClass, "ceph-block-snapclass")
	}
	if gen.operatorNamespace != testOperatorNamespace {
		t.Errorf("resolved operatorNamespace = %q, want %q", gen.operatorNamespace, testOperatorNamespace)
	}
}

// TestRegistryForSelectsCSIGenericForLonghorn proves the routing rule is "CephFS is special",
// not "only RBD gets csi-generic": any other snapshot-capable driver also lands on
// csiGenericExposer.
func TestRegistryForSelectsCSIGenericForLonghorn(t *testing.T) {
	c := newRegistryTestClient(t,
		[]*storagev1.StorageClass{newStorageClass("longhorn", longhornProvisioner)},
		[]*unstructured.Unstructured{newVolumeSnapshotClass("longhorn-snapclass", longhornProvisioner)},
	)
	pvc := pvcOnStorageClass("c-edge", "edge-data", "longhorn")

	exp, err := NewRegistry(c, testOperatorNamespace).For(context.Background(), pvc)
	if err != nil {
		t.Fatalf("For: unexpected error: %v", err)
	}
	if exp.Kind() != KindCSIGeneric {
		t.Errorf("Kind() = %q, want %q", exp.Kind(), KindCSIGeneric)
	}
}

// TestRegistryForSelectsCephFSShallowForCephFS pins the CephFS special-case: a provisioner
// name containing ".cephfs.csi." with a matching VolumeSnapshotClass resolves to
// cephfsShallowExposer, not csiGenericExposer.
func TestRegistryForSelectsCephFSShallowForCephFS(t *testing.T) {
	c := newRegistryTestClient(t,
		[]*storagev1.StorageClass{newStorageClass("cephfs", cephfsProvisioner)},
		[]*unstructured.Unstructured{newVolumeSnapshotClass("cephfs-snapclass", cephfsProvisioner)},
	)
	pvc := pvcOnStorageClass("c-media", "media-data", "cephfs")

	exp, err := NewRegistry(c, testOperatorNamespace).For(context.Background(), pvc)
	if err != nil {
		t.Fatalf("For: unexpected error: %v", err)
	}
	if exp.Kind() != KindCephFSShallow {
		t.Errorf("Kind() = %q, want %q", exp.Kind(), KindCephFSShallow)
	}
	shallow, ok := exp.(*cephfsShallowExposer)
	if !ok {
		t.Fatalf("resolved exposer type = %T, want *cephfsShallowExposer", exp)
	}
	if shallow.volumeSnapshotClass != "cephfs-snapclass" {
		t.Errorf("resolved VolumeSnapshotClass = %q, want %q", shallow.volumeSnapshotClass, "cephfs-snapclass")
	}
	if shallow.operatorNamespace != testOperatorNamespace {
		t.Errorf("resolved operatorNamespace = %q, want %q", shallow.operatorNamespace, testOperatorNamespace)
	}
}

// TestRegistryForNoVolumeSnapshotClassIsUnsupported pins the ErrUnsupported contract for the
// textbook case ADR 0003 names explicitly: rancher.io/local-path, which the crucible expects
// the Backup controller to turn into Skipped/CSISnapshotUnsupported — but ONLY via
// errors.Is, which is what this test actually proves (a string-contains check would not).
func TestRegistryForNoVolumeSnapshotClassIsUnsupported(t *testing.T) {
	c := newRegistryTestClient(t,
		[]*storagev1.StorageClass{newStorageClass("local-path", localPathProvisioner)},
		nil, // no VolumeSnapshotClass anywhere in the cluster
	)
	pvc := pvcOnStorageClass("c-legacy", "legacy-data", "local-path")

	_, err := NewRegistry(c, testOperatorNamespace).For(context.Background(), pvc)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("For error = %v, want errors.Is(err, ErrUnsupported)", err)
	}
}

// TestRegistryForNoVolumeSnapshotClassForThisDriverIsUnsupported proves the match is
// per-driver, not "any VolumeSnapshotClass exists somewhere": a cluster with OTHER classes
// installed (e.g. for RBD) still reports ErrUnsupported for a provisioner none of them name.
func TestRegistryForNoVolumeSnapshotClassForThisDriverIsUnsupported(t *testing.T) {
	c := newRegistryTestClient(t,
		[]*storagev1.StorageClass{
			newStorageClass("ceph-block", rbdProvisioner),
			newStorageClass("local-path", localPathProvisioner),
		},
		[]*unstructured.Unstructured{newVolumeSnapshotClass("ceph-block-snapclass", rbdProvisioner)},
	)
	pvc := pvcOnStorageClass("c-legacy", "legacy-data", "local-path")

	_, err := NewRegistry(c, testOperatorNamespace).For(context.Background(), pvc)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("For error = %v, want errors.Is(err, ErrUnsupported)", err)
	}
}

// TestRegistryForMissingStorageClassIsAClearDistinctError pins the OTHER failure mode: a PVC
// naming a StorageClass that does not exist is a resolution failure, not a snapshot-capability
// verdict — it must NOT satisfy errors.Is(err, ErrUnsupported), so a caller cannot mistake a
// dangling/typo'd storageClassName for "this volume has no snapshot support".
func TestRegistryForMissingStorageClassIsAClearDistinctError(t *testing.T) {
	c := newRegistryTestClient(t, nil, nil)
	pvc := pvcOnStorageClass("c-db", "data", "does-not-exist")

	_, err := NewRegistry(c, testOperatorNamespace).For(context.Background(), pvc)
	if err == nil {
		t.Fatal("For: expected an error for a PVC referencing a missing StorageClass, got nil")
	}
	if errors.Is(err, ErrUnsupported) {
		t.Errorf("For error = %v, incorrectly matched ErrUnsupported (missing StorageClass is a distinct failure mode)", err)
	}
	// The %w-wrapping must preserve the apierrors predicate chain (errors.As-compatible),
	// since that is exactly what a caller does to tell "requeue, it may appear" apart from a
	// genuine misconfiguration.
	if !apierrors.IsNotFound(err) {
		t.Errorf("For error = %v, want apierrors.IsNotFound (wrapping the underlying Get error)", err)
	}
}

// TestRegistryForNilStorageClassNameIsAClearDistinctError covers the other "cannot resolve"
// shape: a PVC with no StorageClassName at all (nil pointer, e.g. no default StorageClass and
// none requested). Same distinctness requirement as the missing-StorageClass case above.
func TestRegistryForNilStorageClassNameIsAClearDistinctError(t *testing.T) {
	c := newRegistryTestClient(t, nil, nil)
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "c-db"}}

	_, err := NewRegistry(c, testOperatorNamespace).For(context.Background(), pvc)
	if err == nil {
		t.Fatal("For: expected an error for a PVC with nil StorageClassName, got nil")
	}
	if errors.Is(err, ErrUnsupported) {
		t.Errorf("For error = %v, incorrectly matched ErrUnsupported (nil StorageClassName is a distinct failure mode)", err)
	}
}

// TestFindVolumeSnapshotClassIsDeterministicUnderMultipleMatches pins the tie-break: when
// several VolumeSnapshotClasses name the same driver (legal — e.g. differing
// deletionPolicy), resolution always returns the lexicographically smallest name, regardless
// of creation order, so the same cluster state can never resolve to two different exposer
// configurations across runs.
func TestFindVolumeSnapshotClassIsDeterministicUnderMultipleMatches(t *testing.T) {
	c := newRegistryTestClient(t, nil, []*unstructured.Unstructured{
		newVolumeSnapshotClass("zzz-retain", rbdProvisioner),
		newVolumeSnapshotClass("aaa-delete", rbdProvisioner),
		newVolumeSnapshotClass("mmm-other", rbdProvisioner),
	})

	name, err := (&Registry{client: c}).findVolumeSnapshotClass(context.Background(), rbdProvisioner)
	if err != nil {
		t.Fatalf("findVolumeSnapshotClass: unexpected error: %v", err)
	}
	if name != "aaa-delete" {
		t.Errorf("findVolumeSnapshotClass = %q, want %q (lexicographically smallest)", name, "aaa-delete")
	}
}

// TestFindVolumeSnapshotClassNoMatchReturnsEmpty pins the zero-value contract
// Registry.For relies on to decide ErrUnsupported: no matching class is reported as ("", nil),
// never an error.
func TestFindVolumeSnapshotClassNoMatchReturnsEmpty(t *testing.T) {
	c := newRegistryTestClient(t, nil, []*unstructured.Unstructured{
		newVolumeSnapshotClass("ceph-block-snapclass", rbdProvisioner),
	})

	name, err := (&Registry{client: c}).findVolumeSnapshotClass(context.Background(), localPathProvisioner)
	if err != nil {
		t.Fatalf("findVolumeSnapshotClass: unexpected error: %v", err)
	}
	if name != "" {
		t.Errorf("findVolumeSnapshotClass = %q, want \"\" (no VolumeSnapshotClass names this driver)", name)
	}
}
