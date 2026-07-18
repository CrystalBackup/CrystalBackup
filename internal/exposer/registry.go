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
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrUnsupported is returned by Registry.For when the PVC's StorageClass provisioner has no
// matching VolumeSnapshotClass anywhere in the cluster — i.e. there is no known way to
// snapshot this volume at all (the textbook case: rancher.io/local-path). It is a sentinel
// error so the Backup controller can test errors.Is(err, ErrUnsupported) to distinguish "skip
// this volume, mark it Skipped/CSISnapshotUnsupported" (ADR 0003) from every other resolution
// failure (a missing StorageClass, a broken client, ...) that warrants a hard error instead.
var ErrUnsupported = errors.New("exposer: storage class has no CSI snapshot support")

// cephfsProvisionerMarker is the substring ADR 0003 pins to recognise a CephFS CSI driver
// (e.g. "rook-ceph.cephfs.csi.ceph.com"): any provisioner whose name contains it is routed to
// cephfsShallowExposer instead of the csi-generic default.
const cephfsProvisionerMarker = ".cephfs.csi."

// volumeSnapshotClassGVK / volumeSnapshotClassListGVK name the unstructured VolumeSnapshotClass
// (cluster-scoped) this package lists to resolve a provisioner's snapshot capability.
func volumeSnapshotClassGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: volumeSnapshotGroup, Version: volumeSnapshotVersion, Kind: "VolumeSnapshotClass"}
}

func volumeSnapshotClassListGVK() schema.GroupVersionKind {
	gvk := volumeSnapshotClassGVK()
	gvk.Kind += "List"
	return gvk
}

// Registry resolves the right SnapshotExposer for a PVC by looking at its StorageClass's CSI
// provisioner and the cluster's installed VolumeSnapshotClasses. It holds a client and the
// operator namespace it hands every exposer (where the static VS/VSC pair and the temp PVC are
// created) — cheap to construct, no state carried between calls.
type Registry struct {
	client client.Client
	// operatorNamespace is crystal-backup-system: threaded into every resolved exposer so its
	// Ready can create the static VolumeSnapshot and temp PVC there (the mover mounts them
	// there). Learned by the operator at startup from POD_NAMESPACE / --operator-namespace and
	// passed to NewRegistry; see apiconst.DefaultOperatorNamespace.
	operatorNamespace string
}

// NewRegistry builds a Registry over c for the given operator namespace. c is used to GET
// StorageClasses (typed, in-scheme) and LIST VolumeSnapshotClasses (unstructured — the snapshot
// CRDs are not in this module's scheme; see the package doc). operatorNamespace is where every
// resolved exposer creates its static VolumeSnapshot and temp PVC.
func NewRegistry(c client.Client, operatorNamespace string) *Registry {
	return &Registry{client: c, operatorNamespace: operatorNamespace}
}

// For resolves the SnapshotExposer for pvc, per ADR 0003's Ceph-aware selection:
//
//  1. Read pvc.Spec.StorageClassName and GET that StorageClass to learn its Provisioner. A
//     PVC with no StorageClassName, or naming a StorageClass that does not exist, is a clear,
//     distinct error — NOT ErrUnsupported, because it is not a verdict about snapshot
//     capability, it is "this PVC cannot even be resolved".
//  2. LIST VolumeSnapshotClasses and find one whose "driver" field equals the provisioner. No
//     match -> ErrUnsupported: the volume cannot be snapshotted at all, which the Backup
//     controller treats as "skip this volume" (status.volumes[].phase Skipped, reason
//     CSISnapshotUnsupported), never a hard failure.
//  3. A CephFS provisioner (name contains ".cephfs.csi.") with a match -> cephfsShallowExposer.
//     Any other provisioner with a match -> csiGenericExposer.
//
// The returned exposer is preconfigured with the resolved VolumeSnapshotClass name, so its
// Expose never has to re-run this resolution.
func (r *Registry) For(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (SnapshotExposer, error) {
	if pvc == nil {
		return nil, errors.New("exposer: nil PVC")
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return nil, fmt.Errorf("exposer: PVC %s/%s has no spec.storageClassName set", pvc.Namespace, pvc.Name)
	}
	scName := *pvc.Spec.StorageClassName

	var sc storagev1.StorageClass
	if err := r.client.Get(ctx, client.ObjectKey{Name: scName}, &sc); err != nil {
		return nil, fmt.Errorf("exposer: get StorageClass %q for PVC %s/%s: %w", scName, pvc.Namespace, pvc.Name, err)
	}
	provisioner := sc.Provisioner

	vsClassName, err := r.findVolumeSnapshotClass(ctx, provisioner)
	if err != nil {
		return nil, fmt.Errorf("exposer: list VolumeSnapshotClasses for provisioner %q: %w", provisioner, err)
	}
	if vsClassName == "" {
		return nil, fmt.Errorf("%w: provisioner %q (StorageClass %q) has no matching VolumeSnapshotClass",
			ErrUnsupported, provisioner, scName)
	}

	if strings.Contains(provisioner, cephfsProvisionerMarker) {
		return newCephFSShallowExposer(r.client, r.operatorNamespace, vsClassName), nil
	}
	return newCSIGenericExposer(r.client, r.operatorNamespace, vsClassName), nil
}

// findVolumeSnapshotClass returns the name of a VolumeSnapshotClass whose "driver" field
// equals provisioner, or "" if none exists. More than one match is legal (a cluster may
// define several classes for the same driver, e.g. differing deletionPolicy); this picks the
// lexicographically smallest name — an arbitrary but DETERMINISTIC tie-break, so the same
// cluster state always resolves to the same exposer configuration instead of flapping with
// API list ordering.
func (r *Registry) findVolumeSnapshotClass(ctx context.Context, provisioner string) (string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(volumeSnapshotClassListGVK())
	if err := r.client.List(ctx, list); err != nil {
		return "", err
	}

	var candidates []string
	for i := range list.Items {
		driver, _, err := unstructured.NestedString(list.Items[i].Object, "driver")
		if err != nil {
			return "", fmt.Errorf("read .driver of VolumeSnapshotClass %s: %w", list.Items[i].GetName(), err)
		}
		if driver == provisioner {
			candidates = append(candidates, list.Items[i].GetName())
		}
	}
	if len(candidates) == 0 {
		return "", nil
	}
	slices.Sort(candidates)
	return candidates[0], nil
}
