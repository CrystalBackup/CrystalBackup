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

// ErrUnsupported is returned by Registry.For when there is no known way to snapshot this volume at
// all, which happens two ways: the driver serving it has no matching VolumeSnapshotClass anywhere in
// the cluster (the textbook case: rancher.io/local-path), or the volume is not a CSI volume in the
// first place (a static NFS / hostPath / local / iSCSI PersistentVolume — nothing to ask for a
// snapshot). Both are verdicts about the storage, not about the cluster's health today. It is a sentinel
// error so the Backup controller can test errors.Is(err, ErrUnsupported) to distinguish "skip
// this volume, mark it Skipped/CSISnapshotUnsupported" (ADR 0003) from every other resolution
// failure (a missing StorageClass, a broken client, ...) that warrants a hard error instead.
// Its sibling is ErrPrecheckFailed (precheck.go), and the pair is the whole vocabulary of
// exposure admission: ErrUnsupported means "never, on this storage", ErrPrecheckFailed means "not
// on this cluster today". They are defined apart because each lives with the code that produces
// it, and the difference between them is spelled out on ErrPrecheckFailed.
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
//  1. Learn the CSI driver that actually serves this volume — from its PersistentVolume when the
//     PVC names one, and only from its StorageClass when it does not. See driverFor: the PV is
//     the source of truth, and the StorageClass is a mutable indirection that can name a
//     DIFFERENT driver than the one holding the data.
//  2. LIST VolumeSnapshotClasses and find one whose "driver" field equals the driver. No
//     match -> ErrUnsupported: the volume cannot be snapshotted at all, which the Backup
//     controller treats as "skip this volume" (status.volumes[].phase Skipped, reason
//     CSISnapshotUnsupported), never a hard failure.
//  3. A CephFS driver (name contains ".cephfs.csi.") with a match -> cephfsShallowExposer.
//     Any other driver with a match -> csiGenericExposer.
//
// The returned exposer is preconfigured with the resolved VolumeSnapshotClass OBJECT, so its
// Expose never has to re-run this resolution and its Precheck can read the class's parameters
// (the snapshotter Secret reference) without a second List.
//
// For remains PURE RESOLUTION: it answers "which exposer, if any", and nothing about whether the
// cluster can serve a snapshot today. That separation is load-bearing beyond taste —
// hack/gen-preflight-table executes this very function against a fake cluster to generate
// website/public/preflight.sh, and the script it emits promises administrators that it predicts
// what the operator will do. Folding the active pre-check in here would make that promise false on
// any cluster whose snapshotter Secret is missing, in a generated artifact that has no way to say
// so. The pre-check is therefore a separate, explicit step (SnapshotExposer.Precheck).
func (r *Registry) For(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (SnapshotExposer, error) {
	if pvc == nil {
		return nil, errors.New("exposer: nil PVC")
	}
	driver, origin, err := r.driverFor(ctx, pvc)
	if err != nil {
		return nil, err
	}

	vsClass, err := r.findVolumeSnapshotClass(ctx, driver)
	if err != nil {
		return nil, fmt.Errorf("exposer: list VolumeSnapshotClasses for driver %q: %w", driver, err)
	}
	if vsClass == nil {
		return nil, fmt.Errorf("%w: driver %q (%s) has no matching VolumeSnapshotClass",
			ErrUnsupported, driver, origin)
	}

	if strings.Contains(driver, cephfsProvisionerMarker) {
		return newCephFSShallowExposer(r.client, r.operatorNamespace, vsClass), nil
	}
	return newCSIGenericExposer(r.client, r.operatorNamespace, vsClass), nil
}

// migratedToAnnotation is the signal Kubernetes leaves on an in-tree PersistentVolume whose plugin
// has been superseded by a CSI driver (CSI migration): the volume source stays gcePersistentDisk /
// awsElasticBlockStore / cinder / azureDisk, but the driver that serves it — and therefore the one a
// VolumeSnapshotClass must name — is in this annotation. Reading only .spec.csi.driver would call
// every such volume unsnapshottable.
const migratedToAnnotation = "pv.kubernetes.io/migrated-to"

// driverFor resolves the CSI driver serving pvc, and a human-readable phrase naming where that
// answer came from (for the ErrUnsupported message: an administrator reading "driver X has no
// matching VolumeSnapshotClass" needs to know whether X came from the PV or the class).
//
// # Why the PersistentVolume, and not the StorageClass
//
// A StorageClass's provisioner is immutable, which makes it tempting to treat the class as the
// volume's identity. It is not. A class can be DELETED and re-created under the same name with a
// different provisioner — perfectly legal, and a normal thing to do when migrating a cluster from
// one storage backend to another. Every PersistentVolume created under the old class keeps
// spec.storageClassName pointing at that name: a dangling string that now resolves to the WRONG
// driver. Resolving through it does not produce an error, it produces a confident wrong answer —
// the operator would route an NFS volume to the RBD exposer, cut a VolumeSnapshot that can never
// become ready, and report SnapshotReadyDeadlineExceeded two hours later. A plausible, coherent,
// entirely false diagnosis that accuses the storage. The PV names the driver holding the data;
// nothing else does.
//
// Statically provisioned volumes make the same point from the other side: for a static binding,
// storageClassName is only a matching LABEL between PVC and PV, and Kubernetes never requires the
// StorageClass object to exist at all. A PVC bound to a hand-made PV whose class was never created
// is valid, common, and not a misconfiguration — but resolving through the class turns it into
// "StorageClass not found", which is neither true nor actionable.
//
// The StorageClass is therefore consulted for exactly one case: a PVC that names NO volume, i.e.
// one not yet bound to anything. There the class is the only evidence available — and a volume with
// no PV has no data to back up yet either.
func (r *Registry) driverFor(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (driver string, origin string, err error) {
	if pvc.Spec.VolumeName != "" {
		var pv corev1.PersistentVolume
		if err := r.client.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, &pv); err != nil {
			// Deliberately NOT wrapped as ErrUnsupported: a PVC naming a PV we cannot read is not a
			// verdict about snapshot capability. A NotFound here is a broken or momentarily
			// inconsistent cluster (a cached client can answer NotFound for an object that exists),
			// so it must retry rather than settle — bounded by the Backup controller's Pending
			// deadline, which is what stops a retry from becoming a head-of-line block.
			return "", "", fmt.Errorf("exposer: get PersistentVolume %q bound to PVC %s/%s: %w",
				pvc.Spec.VolumeName, pvc.Namespace, pvc.Name, err)
		}
		if pv.Spec.CSI != nil && pv.Spec.CSI.Driver != "" {
			return pv.Spec.CSI.Driver, fmt.Sprintf("PersistentVolume %q", pv.Name), nil
		}
		if migrated := pv.Annotations[migratedToAnnotation]; migrated != "" {
			return migrated, fmt.Sprintf("PersistentVolume %q, CSI-migrated", pv.Name), nil
		}
		// Not a CSI volume at all: NFS, hostPath, local, iSCSI, an in-tree plugin with no
		// migration. There is no CSI driver to ask for a snapshot, so this is ErrUnsupported —
		// "never, on this storage" — and the Backup controller makes it Skipped, which is neutral in
		// the roll-up and terminal in the queue. That is the honest verdict AND the one that lets
		// the other volumes of the namespace through.
		return "", "", fmt.Errorf("%w: PersistentVolume %q bound to PVC %s/%s is a %s volume, not CSI",
			ErrUnsupported, pv.Name, pvc.Namespace, pvc.Name, persistentVolumeSourceName(&pv))
	}

	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return "", "", fmt.Errorf("exposer: PVC %s/%s is bound to no volume and has no spec.storageClassName set",
			pvc.Namespace, pvc.Name)
	}
	scName := *pvc.Spec.StorageClassName

	var sc storagev1.StorageClass
	if err := r.client.Get(ctx, client.ObjectKey{Name: scName}, &sc); err != nil {
		return "", "", fmt.Errorf("exposer: get StorageClass %q for unbound PVC %s/%s: %w",
			scName, pvc.Namespace, pvc.Name, err)
	}
	if migrated := sc.Annotations[migratedToAnnotation]; migrated != "" {
		return migrated, fmt.Sprintf("StorageClass %q, CSI-migrated", scName), nil
	}
	return sc.Provisioner, fmt.Sprintf("StorageClass %q", scName), nil
}

// persistentVolumeSourceName names the non-CSI source a PersistentVolume carries, so the
// ErrUnsupported message says "is a nfs volume" rather than leaving an administrator to go and look.
// The list is the sources still creatable on a supported Kubernetes; anything else answers
// "non-CSI", which is the only claim this function needs to be right about.
func persistentVolumeSourceName(pv *corev1.PersistentVolume) string {
	switch s := pv.Spec.PersistentVolumeSource; {
	case s.NFS != nil:
		return "nfs"
	case s.HostPath != nil:
		return "hostPath"
	case s.Local != nil:
		return "local"
	case s.ISCSI != nil:
		return "iscsi"
	case s.FC != nil:
		return "fc"
	default:
		return "non-CSI"
	}
}

// findVolumeSnapshotClass returns the VolumeSnapshotClass whose "driver" field equals
// provisioner, or nil if none exists. More than one match is legal (a cluster may define several
// classes for the same driver, e.g. differing deletionPolicy); this picks the lexicographically
// smallest NAME — an arbitrary but DETERMINISTIC tie-break, so the same cluster state always
// resolves to the same exposer configuration instead of flapping with API list ordering.
//
// It returns the OBJECT, not just the name, because the resolved class is read twice downstream
// for two different reasons: its name goes into the dynamic VolumeSnapshot's
// spec.volumeSnapshotClassName, and its `parameters` carry the snapshotter Secret reference the
// pre-check verifies. Re-reading it by name at pre-check time would be a second API round-trip
// AND a second chance to resolve a different object than the one this tie-break chose.
func (r *Registry) findVolumeSnapshotClass(ctx context.Context, provisioner string) (*unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(volumeSnapshotClassListGVK())
	if err := r.client.List(ctx, list); err != nil {
		return nil, err
	}

	var candidates []*unstructured.Unstructured
	for i := range list.Items {
		driver, _, err := unstructured.NestedString(list.Items[i].Object, "driver")
		if err != nil {
			return nil, fmt.Errorf("read .driver of VolumeSnapshotClass %s: %w", list.Items[i].GetName(), err)
		}
		if driver == provisioner {
			candidates = append(candidates, &list.Items[i])
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	slices.SortFunc(candidates, func(a, b *unstructured.Unstructured) int {
		return strings.Compare(a.GetName(), b.GetName())
	})
	return candidates[0], nil
}
