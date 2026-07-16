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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// This file holds the primitives shared by BOTH exposer implementations: the GVKs, the
// deterministic object-naming convention, the common dynamic-VolumeSnapshot builder and temp
// PVC shell, the shared Expose body, and the small unstructured/label helpers. The Ready
// handover (ready.go) and Cleanup teardown (cleanup.go) build on these. Keeping the shared
// parts in one place means the two exposers cannot silently drift apart on the flow ADR 0003
// says is identical for csi-generic and cephfs-shallow (everything but the temp PVC's access
// mode).

// volumeSnapshotGroup/Version are the external snapshot.storage.k8s.io API this package talks
// to exclusively through *unstructured.Unstructured (it is not in the operator's scheme — the
// snapshot CRDs are a cluster-installed dependency, not a Go type this module vendors; see
// the package doc and the HARD CONSTRAINTS this package was built under).
const (
	volumeSnapshotGroup   = "snapshot.storage.k8s.io"
	volumeSnapshotVersion = "v1"
)

// dataSourceKindVolumeSnapshot is the Kind a temp PVC's spec.dataSource names to bind against
// a VolumeSnapshot. It is a core PersistentVolumeClaim field (TypedLocalObjectReference); only
// the REFERENCED object's group lives in volumeSnapshotGroup, which is why newTempPVCFromSnapshot
// sets APIGroup but not a group-qualified Kind.
const dataSourceKindVolumeSnapshot = "VolumeSnapshot"

// deletionPolicyRetain / deletionPolicyDelete are the two VolumeSnapshotContent.spec.deletionPolicy
// values this package toggles. Ready patches the origin content to Retain "for the duration of
// the handover" (ADR 0003 step 2) so no intermediate delete can destroy the storage-side
// snapshot; Cleanup restores it to Delete (step 5) so deleting the origin VolumeSnapshot
// reclaims that snapshot exactly once. The static content this package creates is Retain
// forever (objects-only — the storage snapshot is owned by the origin content).
const (
	deletionPolicyRetain = "Retain"
	deletionPolicyDelete = "Delete"
)

// volumeSnapshotGVK / volumeSnapshotContentGVK are the GroupVersionKinds of the (namespaced)
// VolumeSnapshot and (cluster-scoped) VolumeSnapshotContent objects this package creates, reads,
// patches and deletes.
func volumeSnapshotGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: volumeSnapshotGroup, Version: volumeSnapshotVersion, Kind: "VolumeSnapshot"}
}

func volumeSnapshotContentGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: volumeSnapshotGroup, Version: volumeSnapshotVersion, Kind: "VolumeSnapshotContent"}
}

// newUnstructured returns an empty *unstructured.Unstructured stamped with gvk — the minimum a
// controller-runtime client needs to Get/Create/Delete an out-of-scheme object.
func newUnstructured(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return u
}

// newUnstructuredList returns an empty list stamped with the "<Kind>List" GVK for gvk, mirroring
// registry.go's volumeSnapshotClassListGVK convention.
func newUnstructuredList(gvk schema.GroupVersionKind) *unstructured.UnstructuredList {
	l := &unstructured.UnstructuredList{}
	gvk.Kind += "List"
	l.SetGroupVersionKind(gvk)
	return l
}

// The four object names an exposure uses, all DETERMINISTIC from one shared NamePrefix (see
// ExposeRequest.NamePrefix's collision-free-per-run contract) so a restarted controller
// reconstructs identical names and re-drives idempotently. VolumeSnapshot,
// VolumeSnapshotContent and PersistentVolumeClaim are different API kinds, so the derived names
// need not differ for correctness — the suffixes exist purely so `kubectl get vs,vsc,pvc`
// output and logs read unambiguously.
func volumeSnapshotName(prefix string) string { return prefix + "-snap" }    // origin dynamic VS (tenant ns)
func staticVSCName(prefix string) string      { return prefix + "-vsc" }     // static VSC (cluster-scoped)
func staticVSName(prefix string) string       { return prefix + "-restore" } // static VS (operator ns)
func tempPVCName(prefix string) string        { return prefix + "-clone" }   // temp PVC (operator ns)

// buildVolumeSnapshot constructs the dynamic VolumeSnapshot both exposers create as their common
// first step, in the ORIGIN (tenant) namespace. Pure: the same (req, volumeSnapshotClass) always
// produces a byte-identical object, which is what makes a retried Expose safe (Create tolerates
// AlreadyExists) and this function unit-testable without a cluster.
func buildVolumeSnapshot(req ExposeRequest, volumeSnapshotClass string) *unstructured.Unstructured {
	snap := &unstructured.Unstructured{}
	snap.SetGroupVersionKind(volumeSnapshotGVK())
	snap.SetName(volumeSnapshotName(req.NamePrefix))
	snap.SetNamespace(req.Namespace)
	snap.SetLabels(req.Labels)

	// SetGroupVersionKind (above) already lazily allocates snap.Object, and "spec" is an
	// entirely fresh subtree the two calls below are the only writers of, so SetNestedField
	// cannot hit its one error case (an existing non-map value along the path). Errors are
	// deliberately discarded rather than threaded through — this function's whole point is to
	// be a plain, error-free pure builder.
	_ = unstructured.SetNestedField(snap.Object, volumeSnapshotClass, "spec", "volumeSnapshotClassName")
	_ = unstructured.SetNestedField(snap.Object, req.PVCName, "spec", "source", "persistentVolumeClaimName")

	return snap
}

// newTempPVCFromSnapshot is the shared shell both buildTempPVC (csi-generic, ReadWriteOnce) and
// buildShallowPVC (cephfs-shallow, ReadOnlyMany) build on: it produces the temp PVC in the
// OPERATOR namespace, sourced from the STATIC VolumeSnapshot (ex.StaticVSName, same namespace —
// a PVC dataSource of kind VolumeSnapshot must be namespace-local), at the resolved capacity.
// The two callers differ ONLY in accessMode, so the parts that ADR 0003 says are identical
// cannot drift. Pure: (ex, capacity, accessMode) fully determine the object.
func newTempPVCFromSnapshot(ex *Exposure, capacity resource.Quantity, accessMode corev1.PersistentVolumeAccessMode) *corev1.PersistentVolumeClaim {
	storageClass := ex.StorageClass
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ex.TempPVCName,
			Namespace: ex.OperatorNamespace,
			Labels:    ex.Labels,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{accessMode},
			StorageClassName: &storageClass,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: capacity},
			},
			DataSource: &corev1.TypedLocalObjectReference{
				APIGroup: ptrTo(volumeSnapshotGroup),
				Kind:     dataSourceKindVolumeSnapshot,
				Name:     ex.StaticVSName,
			},
		},
	}
}

// expose is the shared Expose() body for both exposers: create the dynamic VolumeSnapshot in the
// origin namespace, then return a fully-populated Exposure with every deterministic name plus
// the operator namespace, storage class, capacity and labels. It does NOT wait and does NOT
// create the static objects or temp PVC — that handover is Ready's job (ADR 0003 steps 2-6),
// driven once the origin snapshot is readyToUse. Idempotent: the one Create tolerates
// AlreadyExists so a retried Expose (partial failure, or a controller reconciling the same
// request again after a restart) converges instead of erroring.
func expose(ctx context.Context, c client.Client, kind string, req ExposeRequest, operatorNamespace, volumeSnapshotClass string) (*Exposure, error) {
	snap := buildVolumeSnapshot(req, volumeSnapshotClass)
	if err := c.Create(ctx, snap); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("exposer(%s): create origin VolumeSnapshot %s/%s: %w", kind, req.Namespace, snap.GetName(), err)
	}

	prefix := req.NamePrefix
	return &Exposure{
		Kind:              kind,
		OriginNamespace:   req.Namespace,
		OperatorNamespace: operatorNamespace,
		OriginVSName:      volumeSnapshotName(prefix),
		StaticVSCName:     staticVSCName(prefix),
		StaticVSName:      staticVSName(prefix),
		TempPVCName:       tempPVCName(prefix),
		ExposedPVCName:    tempPVCName(prefix),
		StorageClass:      req.StorageClass,
		Capacity:          req.Capacity,
		Labels:            req.Labels,
	}, nil
}

// volumeSnapshotReady reads back the named VolumeSnapshot and reports its status.readyToUse. A
// VolumeSnapshot that does not exist yet (e.g. a Create still propagating) reports not-ready
// rather than erroring, matching Ready's "poll me until true" contract. Used for BOTH the origin
// dynamic snapshot (step 1) and the static snapshot (step 7).
func volumeSnapshotReady(ctx context.Context, c client.Client, namespace, name string) (bool, error) {
	got := newUnstructured(volumeSnapshotGVK())
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, got); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get VolumeSnapshot %s/%s: %w", namespace, name, err)
	}
	ready, _, err := unstructured.NestedBool(got.Object, "status", "readyToUse")
	if err != nil {
		return false, fmt.Errorf("read status.readyToUse of VolumeSnapshot %s/%s: %w", namespace, name, err)
	}
	return ready, nil
}

// mergeLabels adds every key of add that is missing or different from obj's labels, and reports
// whether it changed anything. It never removes a label already on obj (the origin VSC carries
// its own provisioner labels, which the Retain patch must preserve). A no-change call returns
// false so callers can skip a redundant API write.
func mergeLabels(obj *unstructured.Unstructured, add map[string]string) bool {
	if len(add) == 0 {
		return false
	}
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	changed := false
	for k, v := range add {
		if labels[k] != v {
			labels[k] = v
			changed = true
		}
	}
	if changed {
		obj.SetLabels(labels)
	}
	return changed
}

// ptrTo returns a pointer to a copy of v. Mirrors internal/mover's identical helper
// (unexported there too, so it cannot be imported — see that package's job.go); used here for
// the *string fields the k8s API insists on (TypedLocalObjectReference.APIGroup) where v is a
// constant or another non-addressable value.
func ptrTo[T any](v T) *T { return &v }
