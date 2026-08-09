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
// extra carries any additional typed objects the test needs seeded (PersistentVolumes, in practice).
func newRegistryTestClient(t *testing.T, storageClasses []*storagev1.StorageClass, vsClasses []*unstructured.Unstructured, extra ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(corev1): %v", err)
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(storagev1): %v", err)
	}

	objs := make([]client.Object, 0, len(storageClasses)+len(extra))
	for _, sc := range storageClasses {
		objs = append(objs, sc)
	}
	objs = append(objs, extra...)
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

// csiPV builds a PersistentVolume served by driver, named so a PVC can bind to it by volumeName.
func csiPV(name, driver, storageClass string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName:       storageClass,
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: driver}},
		},
	}
}

// nfsPV builds a statically provisioned NFS PersistentVolume — no CSI driver anywhere on it. It
// takes a storageClass name on purpose: for a static binding that string is only a matching LABEL
// between PVC and PV, and Kubernetes never requires the StorageClass object to exist.
func nfsPV(name, storageClass string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: storageClass,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				NFS: &corev1.NFSVolumeSource{Server: "nfs.example.invalid", Path: "/export/data"},
			},
		},
	}
}

// pvcBoundTo builds a PVC bound to volumeName while ALSO naming a StorageClass, which is the normal
// shape of a bound PVC and the shape that makes the two sources of truth disagreeable.
func pvcBoundTo(namespace, name, storageClass, volumeName string) *corev1.PersistentVolumeClaim {
	pvc := pvcOnStorageClass(namespace, name, storageClass)
	pvc.Spec.VolumeName = volumeName
	pvc.Status.Phase = corev1.ClaimBound
	return pvc
}

// TestRegistryForResolvesABoundPVCThroughItsPersistentVolume is the base case for the primary
// path: a bound PVC's driver comes off its PV, and the resolution is identical to what the class
// would have said when the two agree.
func TestRegistryForResolvesABoundPVCThroughItsPersistentVolume(t *testing.T) {
	c := newRegistryTestClient(t,
		[]*storagev1.StorageClass{newStorageClass("ceph-block", rbdProvisioner)},
		[]*unstructured.Unstructured{newVolumeSnapshotClass("ceph-block-snapclass", rbdProvisioner)},
		csiPV("pv-data", rbdProvisioner, "ceph-block"),
	)
	pvc := pvcBoundTo("c-db", "data", "ceph-block", "pv-data")

	exp, err := NewRegistry(c, testOperatorNamespace).For(context.Background(), pvc)
	if err != nil {
		t.Fatalf("For: unexpected error: %v", err)
	}
	if exp.Kind() != KindCSIGeneric {
		t.Errorf("Kind() = %q, want %q", exp.Kind(), KindCSIGeneric)
	}
}

// TestRegistryForPrefersThePVOverAStorageClassThatDisagrees is the test that matters, and the one
// the StorageClass-first implementation fails.
//
// A StorageClass's provisioner is immutable, but the CLASS is not permanent: deleting it and
// re-creating one of the same name over a different backend is legal, and is what a cluster
// migrating from one storage system to another actually does. Every PersistentVolume provisioned
// under the old class keeps spec.storageClassName pointing at that name — a dangling string that
// now resolves to the wrong driver.
//
// Here the data lives on RBD; the class of that name now says CephFS. Resolving through the class
// would return cephfsShallowExposer and take the shallow-snapshot path over a block volume: not an
// error, a confident WRONG answer, whose eventual symptom is a VolumeSnapshot that never becomes
// ready and a SnapshotReadyDeadlineExceeded two hours later blaming the storage. The PV is the only
// object that knows what is holding the bytes.
func TestRegistryForPrefersThePVOverAStorageClassThatDisagrees(t *testing.T) {
	c := newRegistryTestClient(t,
		// The class was re-created over CephFS; the PV underneath is still RBD.
		[]*storagev1.StorageClass{newStorageClass("standard", cephfsProvisioner)},
		[]*unstructured.Unstructured{
			newVolumeSnapshotClass("cephfs-snapclass", cephfsProvisioner),
			newVolumeSnapshotClass("rbd-snapclass", rbdProvisioner),
		},
		csiPV("pv-legacy", rbdProvisioner, "standard"),
	)
	pvc := pvcBoundTo("c-db", "data", "standard", "pv-legacy")

	exp, err := NewRegistry(c, testOperatorNamespace).For(context.Background(), pvc)
	if err != nil {
		t.Fatalf("For: unexpected error: %v", err)
	}
	if exp.Kind() != KindCSIGeneric {
		t.Fatalf("Kind() = %q, want %q — resolution followed the StorageClass instead of the PersistentVolume", exp.Kind(), KindCSIGeneric)
	}
	gen, ok := exp.(*csiGenericExposer)
	if !ok {
		t.Fatalf("resolved exposer type = %T, want *csiGenericExposer", exp)
	}
	if got := vsClassName(gen.vsClass); got != "rbd-snapclass" {
		t.Errorf("resolved VolumeSnapshotClass = %q, want %q (the PV's driver, not the class's)", got, "rbd-snapclass")
	}
}

// TestRegistryForStaticNFSVolumeIsUnsupportedNotUnresolvable is the incident, reduced.
//
// A 200Gi PVC, Bound, on a hand-made NFS PersistentVolume, naming a StorageClass that does not
// exist as an object. Resolving through the class produced "StorageClass not found" — a hard error,
// which the Backup controller retried forever, which held the head of its namespace's volume queue
// for thirty hours and starved every nightly run behind it.
//
// Two claims, both required. The verdict must be ErrUnsupported, because that is the TRUE statement
// about a plain NFS volume: there is no CSI driver to ask for a snapshot, ever. And ErrUnsupported
// is what the Backup controller turns into Skipped — terminal in the queue, neutral in the roll-up —
// so the honest verdict is also the one that lets the rest of the namespace through. The two
// properties are not a coincidence worth relying on silently, hence this test.
func TestRegistryForStaticNFSVolumeIsUnsupportedNotUnresolvable(t *testing.T) {
	c := newRegistryTestClient(t,
		nil, // "slow" is referenced by both PVC and PV, and exists as neither
		[]*unstructured.Unstructured{newVolumeSnapshotClass("ceph-block-snapclass", rbdProvisioner)},
		nfsPV("recette-back", "slow"),
	)
	pvc := pvcBoundTo("develop", "recette-back", "slow", "recette-back")

	_, err := NewRegistry(c, testOperatorNamespace).For(context.Background(), pvc)
	if err == nil {
		t.Fatal("For: expected an error for a static NFS volume, got nil")
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("For error = %v, want ErrUnsupported (a plain NFS volume can never be CSI-snapshotted)", err)
	}
	// And it must not look transient: a caller that requeues on IsNotFound would reproduce the
	// thirty-hour block, since the missing StorageClass is still missing on every retry.
	if apierrors.IsNotFound(err) {
		t.Errorf("For error = %v, must not satisfy apierrors.IsNotFound — that is what made this retry forever", err)
	}
}

// TestRegistryForFollowsTheCSIMigrationAnnotation covers the volumes that are CSI-served without
// carrying a .spec.csi stanza: an in-tree PersistentVolume whose plugin has been superseded keeps
// its original source and names the real driver in pv.kubernetes.io/migrated-to. Reading only
// .spec.csi.driver would declare every one of them unsnapshottable.
func TestRegistryForFollowsTheCSIMigrationAnnotation(t *testing.T) {
	const migratedDriver = "pd.csi.storage.gke.io"
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pv-legacy-pd",
			Annotations: map[string]string{migratedToAnnotation: migratedDriver},
		},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: "standard",
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				GCEPersistentDisk: &corev1.GCEPersistentDiskVolumeSource{PDName: "disk-1"},
			},
		},
	}
	c := newRegistryTestClient(t,
		[]*storagev1.StorageClass{newStorageClass("standard", "kubernetes.io/gce-pd")},
		[]*unstructured.Unstructured{newVolumeSnapshotClass("pd-snapclass", migratedDriver)},
		pv,
	)
	pvc := pvcBoundTo("c-db", "data", "standard", "pv-legacy-pd")

	exp, err := NewRegistry(c, testOperatorNamespace).For(context.Background(), pvc)
	if err != nil {
		t.Fatalf("For: unexpected error: %v", err)
	}
	gen, ok := exp.(*csiGenericExposer)
	if !ok {
		t.Fatalf("resolved exposer type = %T, want *csiGenericExposer", exp)
	}
	if got := vsClassName(gen.vsClass); got != "pd-snapclass" {
		t.Errorf("resolved VolumeSnapshotClass = %q, want %q (the migrated-to driver)", got, "pd-snapclass")
	}
}

// TestRegistryForMissingPVIsRetryableNotAVerdict separates the two ways resolution can fail. A PVC
// naming a PersistentVolume that cannot be read says nothing about snapshot capability: a cached
// client can answer NotFound for an object that exists, and a bound PVC whose PV is genuinely gone
// is a broken cluster somebody can fix. So it must stay a plain, IsNotFound-carrying error the
// caller requeues on — NOT ErrUnsupported, which would silently mark a snapshottable volume Skipped
// and let a backup report success over data it never touched.
func TestRegistryForMissingPVIsRetryableNotAVerdict(t *testing.T) {
	c := newRegistryTestClient(t,
		[]*storagev1.StorageClass{newStorageClass("ceph-block", rbdProvisioner)},
		[]*unstructured.Unstructured{newVolumeSnapshotClass("ceph-block-snapclass", rbdProvisioner)},
	)
	pvc := pvcBoundTo("c-db", "data", "ceph-block", "pv-vanished")

	_, err := NewRegistry(c, testOperatorNamespace).For(context.Background(), pvc)
	if err == nil {
		t.Fatal("For: expected an error for a PVC bound to a missing PersistentVolume, got nil")
	}
	if errors.Is(err, ErrUnsupported) {
		t.Errorf("For error = %v, incorrectly matched ErrUnsupported (an unreadable PV is not a verdict about the storage)", err)
	}
	if !apierrors.IsNotFound(err) {
		t.Errorf("For error = %v, want apierrors.IsNotFound so the caller can requeue", err)
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
	if vsClassName(gen.vsClass) != "ceph-block-snapclass" {
		t.Errorf("resolved VolumeSnapshotClass = %q, want %q", vsClassName(gen.vsClass), "ceph-block-snapclass")
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
	if vsClassName(shallow.vsClass) != "cephfs-snapclass" {
		t.Errorf("resolved VolumeSnapshotClass = %q, want %q", vsClassName(shallow.vsClass), "cephfs-snapclass")
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

	got, err := (&Registry{client: c}).findVolumeSnapshotClass(context.Background(), rbdProvisioner)
	if err != nil {
		t.Fatalf("findVolumeSnapshotClass: unexpected error: %v", err)
	}
	if vsClassName(got) != "aaa-delete" {
		t.Errorf("findVolumeSnapshotClass = %q, want %q (lexicographically smallest)", vsClassName(got), "aaa-delete")
	}
}

// TestFindVolumeSnapshotClassNoMatchReturnsNil pins the zero-value contract
// Registry.For relies on to decide ErrUnsupported: no matching class is reported as (nil, nil),
// never an error.
func TestFindVolumeSnapshotClassNoMatchReturnsNil(t *testing.T) {
	c := newRegistryTestClient(t, nil, []*unstructured.Unstructured{
		newVolumeSnapshotClass("ceph-block-snapclass", rbdProvisioner),
	})

	got, err := (&Registry{client: c}).findVolumeSnapshotClass(context.Background(), localPathProvisioner)
	if err != nil {
		t.Fatalf("findVolumeSnapshotClass: unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("findVolumeSnapshotClass = %q, want nil (no VolumeSnapshotClass names this driver)", got.GetName())
	}
}
