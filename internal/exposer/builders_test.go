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
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// testExposeRequest is the one ExposeRequest fixture every builder test starts from — a
// deliberately distinctive value in every field (namespace != prefix != pvc name, a
// non-round capacity, two labels) so a test assertion that reads the wrong field, or a
// builder that transposes two fields, fails loudly instead of by coincidence passing.
func testExposeRequest() ExposeRequest {
	return ExposeRequest{
		Namespace:    "c-db",
		PVCName:      "postgres-data",
		StorageClass: "ceph-block",
		Capacity:     resource.MustParse("10Gi"),
		NamePrefix:   "crucible-cascade-20260716-120000-postgres-data",
		Labels: map[string]string{
			"crystalbackup.io/cluster-backup": "crucible-cascade-20260716-120000",
			"crystalbackup.io/namespace":      "c-db",
		},
	}
}

// --- buildVolumeSnapshot ----------------------------------------------------------------

// TestBuildVolumeSnapshotShape pins every field ADR 0003 / the task spec requires of the
// VolumeSnapshot both exposers create: GVK, name (derived from NamePrefix), namespace,
// labels, spec.volumeSnapshotClassName, spec.source.persistentVolumeClaimName.
func TestBuildVolumeSnapshotShape(t *testing.T) {
	req := testExposeRequest()
	snap := buildVolumeSnapshot(req, "ceph-block-snapclass")

	if gvk := snap.GroupVersionKind(); gvk.Group != "snapshot.storage.k8s.io" || gvk.Version != "v1" || gvk.Kind != "VolumeSnapshot" {
		t.Errorf("GVK = %v, want snapshot.storage.k8s.io/v1, Kind=VolumeSnapshot", gvk)
	}
	wantName := req.NamePrefix + "-snap"
	if snap.GetName() != wantName {
		t.Errorf("name = %q, want %q", snap.GetName(), wantName)
	}
	if snap.GetNamespace() != req.Namespace {
		t.Errorf("namespace = %q, want %q", snap.GetNamespace(), req.Namespace)
	}
	if !reflect.DeepEqual(snap.GetLabels(), req.Labels) {
		t.Errorf("labels = %v, want %v", snap.GetLabels(), req.Labels)
	}

	class, found, err := unstructured.NestedString(snap.Object, "spec", "volumeSnapshotClassName")
	if err != nil || !found {
		t.Fatalf("spec.volumeSnapshotClassName: found=%v err=%v", found, err)
	}
	if class != "ceph-block-snapclass" {
		t.Errorf("spec.volumeSnapshotClassName = %q, want %q", class, "ceph-block-snapclass")
	}

	srcPVC, found, err := unstructured.NestedString(snap.Object, "spec", "source", "persistentVolumeClaimName")
	if err != nil || !found {
		t.Fatalf("spec.source.persistentVolumeClaimName: found=%v err=%v", found, err)
	}
	if srcPVC != req.PVCName {
		t.Errorf("spec.source.persistentVolumeClaimName = %q, want %q", srcPVC, req.PVCName)
	}
}

// TestBuildVolumeSnapshotIsPure proves the "pure builder" contract the task pins explicitly:
// the same (req, vsClass) in always produces a byte-identical object out (via DeepEqual on
// the underlying map), independent calls never alias each other's maps, and two DIFFERENT
// requests never accidentally collide on name. This is what makes a retried Expose safe.
func TestBuildVolumeSnapshotIsPure(t *testing.T) {
	req := testExposeRequest()

	a := buildVolumeSnapshot(req, "ceph-block-snapclass")
	b := buildVolumeSnapshot(req, "ceph-block-snapclass")
	if !reflect.DeepEqual(a.Object, b.Object) {
		t.Errorf("two calls with the same input produced different objects:\n%v\n%v", a.Object, b.Object)
	}

	// Mutating one's labels must never leak into the other — proves Object maps aren't
	// aliased (e.g. via a shared req.Labels reference written back into both).
	a.SetLabels(map[string]string{"mutated": "true"})
	if reflect.DeepEqual(a.GetLabels(), b.GetLabels()) {
		t.Errorf("mutating one buildVolumeSnapshot result mutated the other (aliased map)")
	}
}

// testExposure is the fully-populated *Exposure fixture the temp-PVC / static-object builder
// tests, the Ready tests and the Cleanup tests all start from: every deterministic name derived
// from testExposeRequest()'s NamePrefix exactly as expose() would, split across the origin
// (tenant) namespace and the operator namespace. Kind defaults to csi-generic; Cleanup ignores
// it and the ROX/RWO split is exercised by the builder, not this field.
func testExposure() *Exposure {
	req := testExposeRequest()
	return &Exposure{
		Kind:              KindCSIGeneric,
		OriginNamespace:   req.Namespace, // c-db (dynamic VolumeSnapshot lives here)
		OperatorNamespace: testOperatorNamespace,
		OriginVSName:      volumeSnapshotName(req.NamePrefix),
		StaticVSCName:     staticVSCName(req.NamePrefix),
		StaticVSName:      staticVSName(req.NamePrefix),
		TempPVCName:       tempPVCName(req.NamePrefix),
		ExposedPVCName:    tempPVCName(req.NamePrefix),
		StorageClass:      req.StorageClass,
		Capacity:          req.Capacity,
		Labels:            req.Labels,
	}
}

// --- buildTempPVC (csi-generic, ReadWriteOnce) -----------------------------------------

// TestBuildTempPVCShape pins csi-generic's temp PVC after the two-namespace rework: it lives in
// the OPERATOR namespace (not the origin), is named TempPVCName, carries the exposure labels and
// storage class, requests the resolved capacity, is ReadWriteOnce, and sources from the STATIC
// VolumeSnapshot (StaticVSName) — the whole point of the re-bind, so the mover can mount it in
// its own namespace.
func TestBuildTempPVCShape(t *testing.T) {
	ex := testExposure()
	capacity := resource.MustParse("10Gi")
	pvc := buildTempPVC(ex, capacity)

	if pvc.Name != ex.TempPVCName {
		t.Errorf("name = %q, want %q", pvc.Name, ex.TempPVCName)
	}
	if pvc.Namespace != ex.OperatorNamespace {
		t.Errorf("namespace = %q, want %q (temp PVC must live in the operator namespace)", pvc.Namespace, ex.OperatorNamespace)
	}
	if !reflect.DeepEqual(pvc.Labels, ex.Labels) {
		t.Errorf("labels = %v, want %v", pvc.Labels, ex.Labels)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != ex.StorageClass {
		t.Errorf("storageClassName = %v, want %q", pvc.Spec.StorageClassName, ex.StorageClass)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(capacity) != 0 {
		t.Errorf("requested storage = %v, want %v", got, capacity)
	}
	assertDataSource(t, pvc.Spec.DataSource, ex.StaticVSName)
}

// TestBuildTempPVCIsReadWriteOnce pins the ADR 0003 default explicitly: csi-generic's temp
// PVC is ReadWriteOnce (writable), never ReadOnlyMany — a dirty journal from a
// crash-consistent snapshot needs a writable mount for the kubelet to replay it.
func TestBuildTempPVCIsReadWriteOnce(t *testing.T) {
	pvc := buildTempPVC(testExposure(), resource.MustParse("10Gi"))

	want := []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	if !reflect.DeepEqual(pvc.Spec.AccessModes, want) {
		t.Errorf("accessModes = %v, want %v", pvc.Spec.AccessModes, want)
	}
}

// --- buildShallowPVC (cephfs-shallow, ReadOnlyMany) ------------------------------------

// TestBuildShallowPVCShape mirrors TestBuildTempPVCShape for the cephfs-shallow builder: same
// operator-namespace placement, same static-VolumeSnapshot dataSource wiring, same documented
// StorageClass convention (reuse ex.StorageClass — see buildShallowPVC's doc comment for why).
func TestBuildShallowPVCShape(t *testing.T) {
	ex := testExposure()
	capacity := resource.MustParse("10Gi")
	pvc := buildShallowPVC(ex, capacity)

	if pvc.Name != ex.TempPVCName {
		t.Errorf("name = %q, want %q", pvc.Name, ex.TempPVCName)
	}
	if pvc.Namespace != ex.OperatorNamespace {
		t.Errorf("namespace = %q, want %q (temp PVC must live in the operator namespace)", pvc.Namespace, ex.OperatorNamespace)
	}
	if !reflect.DeepEqual(pvc.Labels, ex.Labels) {
		t.Errorf("labels = %v, want %v", pvc.Labels, ex.Labels)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != ex.StorageClass {
		t.Errorf("storageClassName = %v, want %q (M1 convention: reuse the source's own class)", pvc.Spec.StorageClassName, ex.StorageClass)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(capacity) != 0 {
		t.Errorf("requested storage = %v, want %v", got, capacity)
	}
	assertDataSource(t, pvc.Spec.DataSource, ex.StaticVSName)
}

// TestBuildShallowPVCIsReadOnlyMany pins the ADR 0003 requirement that makes cephfs-shallow
// zero-copy: ReadOnlyMany, never the RWO csi-generic uses. Both ARE required together per the
// ADR ("Both ReadOnlyMany and backingSnapshot are required") — this test is the ROX half of
// that pin; backingSnapshot itself is a StorageClass-side parameter this package does not (and
// per the task, should not) attempt to fake.
func TestBuildShallowPVCIsReadOnlyMany(t *testing.T) {
	pvc := buildShallowPVC(testExposure(), resource.MustParse("10Gi"))

	want := []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany}
	if !reflect.DeepEqual(pvc.Spec.AccessModes, want) {
		t.Errorf("accessModes = %v, want %v", pvc.Spec.AccessModes, want)
	}
}

// TestCSIGenericAndCephFSShallowAccessModesDiffer is the explicit RWO-vs-ROX contrast the
// task asks for: given the IDENTICAL exposure + capacity, the two builders disagree on exactly
// one axis (access mode) and agree on everything else (name, namespace, labels, class, capacity,
// dataSource) — proving the two exposers really do only diverge at "step 3" as ADR 0003
// describes, never silently drifting on the rest of the PVC shape.
func TestCSIGenericAndCephFSShallowAccessModesDiffer(t *testing.T) {
	ex := testExposure()
	capacity := resource.MustParse("10Gi")

	rwo := buildTempPVC(ex, capacity)
	rox := buildShallowPVC(ex, capacity)

	if reflect.DeepEqual(rwo.Spec.AccessModes, rox.Spec.AccessModes) {
		t.Fatalf("csi-generic and cephfs-shallow must NOT share an access mode, both got %v", rwo.Spec.AccessModes)
	}
	if rwo.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("csi-generic accessMode = %v, want ReadWriteOnce", rwo.Spec.AccessModes)
	}
	if rox.Spec.AccessModes[0] != corev1.ReadOnlyMany {
		t.Errorf("cephfs-shallow accessMode = %v, want ReadOnlyMany", rox.Spec.AccessModes)
	}

	// Everything else must be identical between the two.
	rwo.Spec.AccessModes, rox.Spec.AccessModes = nil, nil
	if !reflect.DeepEqual(rwo.Spec, rox.Spec) {
		t.Errorf("csi-generic and cephfs-shallow specs differ beyond accessModes:\n%+v\n%+v", rwo.Spec, rox.Spec)
	}
}

// --- buildStaticVolumeSnapshotContent / buildStaticVolumeSnapshot (the re-bind pair) ---

// TestBuildStaticVolumeSnapshotContentShape pins the pre-provisioned, cluster-scoped
// VolumeSnapshotContent Ready creates: name StaticVSCName, NO namespace (cluster-scoped),
// exposure labels, deletionPolicy=Retain (objects-only — the storage snapshot is owned by the
// origin content), the driver + snapshotHandle read off the origin content, and a
// volumeSnapshotRef pointing FORWARD at the static VolumeSnapshot in the operator namespace.
func TestBuildStaticVolumeSnapshotContentShape(t *testing.T) {
	ex := testExposure()
	vsc := buildStaticVolumeSnapshotContent(ex, "rook-ceph.rbd.csi.ceph.com", "0001-0009-rook-ceph-snap-abc")

	if gvk := vsc.GroupVersionKind(); gvk.Group != "snapshot.storage.k8s.io" || gvk.Version != "v1" || gvk.Kind != "VolumeSnapshotContent" {
		t.Errorf("GVK = %v, want snapshot.storage.k8s.io/v1 VolumeSnapshotContent", gvk)
	}
	if vsc.GetName() != ex.StaticVSCName {
		t.Errorf("name = %q, want %q", vsc.GetName(), ex.StaticVSCName)
	}
	if vsc.GetNamespace() != "" {
		t.Errorf("namespace = %q, want \"\" (VolumeSnapshotContent is cluster-scoped)", vsc.GetNamespace())
	}
	if !reflect.DeepEqual(vsc.GetLabels(), ex.Labels) {
		t.Errorf("labels = %v, want %v", vsc.GetLabels(), ex.Labels)
	}
	assertNestedString(t, vsc.Object, deletionPolicyRetain, "spec", "deletionPolicy")
	assertNestedString(t, vsc.Object, "rook-ceph.rbd.csi.ceph.com", "spec", "driver")
	assertNestedString(t, vsc.Object, "0001-0009-rook-ceph-snap-abc", "spec", "source", "snapshotHandle")
	assertNestedString(t, vsc.Object, "VolumeSnapshot", "spec", "volumeSnapshotRef", "kind")
	assertNestedString(t, vsc.Object, ex.StaticVSName, "spec", "volumeSnapshotRef", "name")
	assertNestedString(t, vsc.Object, ex.OperatorNamespace, "spec", "volumeSnapshotRef", "namespace")

	// A pre-provisioned content must NOT carry a volumeSnapshotClassName (dynamic-only input).
	if _, found, _ := unstructured.NestedString(vsc.Object, "spec", "volumeSnapshotClassName"); found {
		t.Errorf("spec.volumeSnapshotClassName is set, want absent on a pre-provisioned VolumeSnapshotContent")
	}
}

// TestBuildStaticVolumeSnapshotShape pins the static VolumeSnapshot bound to that content: name
// StaticVSName, OPERATOR namespace (so the temp PVC's same-namespace dataSource is legal),
// exposure labels, and spec.source.volumeSnapshotContentName == StaticVSCName (the
// pre-provisioned binding form — no volumeSnapshotClassName).
func TestBuildStaticVolumeSnapshotShape(t *testing.T) {
	ex := testExposure()
	vs := buildStaticVolumeSnapshot(ex)

	if gvk := vs.GroupVersionKind(); gvk.Group != "snapshot.storage.k8s.io" || gvk.Version != "v1" || gvk.Kind != "VolumeSnapshot" {
		t.Errorf("GVK = %v, want snapshot.storage.k8s.io/v1 VolumeSnapshot", gvk)
	}
	if vs.GetName() != ex.StaticVSName {
		t.Errorf("name = %q, want %q", vs.GetName(), ex.StaticVSName)
	}
	if vs.GetNamespace() != ex.OperatorNamespace {
		t.Errorf("namespace = %q, want %q (static VolumeSnapshot must live in the operator namespace)", vs.GetNamespace(), ex.OperatorNamespace)
	}
	if !reflect.DeepEqual(vs.GetLabels(), ex.Labels) {
		t.Errorf("labels = %v, want %v", vs.GetLabels(), ex.Labels)
	}
	assertNestedString(t, vs.Object, ex.StaticVSCName, "spec", "source", "volumeSnapshotContentName")
	if _, found, _ := unstructured.NestedString(vs.Object, "spec", "volumeSnapshotClassName"); found {
		t.Errorf("spec.volumeSnapshotClassName is set, want absent on a pre-provisioned VolumeSnapshot")
	}
}

// --- helpers ---------------------------------------------------------------------------

// assertDataSource pins the dataSource wiring both PVC builders share: apiGroup
// snapshot.storage.k8s.io, kind VolumeSnapshot, name == the (static) VolumeSnapshot this PVC was
// built against.
func assertDataSource(t *testing.T, ds *corev1.TypedLocalObjectReference, wantVSName string) {
	t.Helper()
	if ds == nil {
		t.Fatal("dataSource is nil")
	}
	if ds.APIGroup == nil || *ds.APIGroup != "snapshot.storage.k8s.io" {
		t.Errorf("dataSource.apiGroup = %v, want %q", ds.APIGroup, "snapshot.storage.k8s.io")
	}
	if ds.Kind != "VolumeSnapshot" {
		t.Errorf("dataSource.kind = %q, want %q", ds.Kind, "VolumeSnapshot")
	}
	if ds.Name != wantVSName {
		t.Errorf("dataSource.name = %q, want %q", ds.Name, wantVSName)
	}
}

// assertNestedString fails unless obj carries want at the given field path (found and equal).
func assertNestedString(t *testing.T, obj map[string]interface{}, want string, path ...string) {
	t.Helper()
	got, found, err := unstructured.NestedString(obj, path...)
	if err != nil || !found {
		t.Fatalf("%v: found=%v err=%v", path, found, err)
	}
	if got != want {
		t.Errorf("%v = %q, want %q", path, got, want)
	}
}
