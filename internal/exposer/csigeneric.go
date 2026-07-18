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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// csiGenericExposer is the default SnapshotExposer (ADR 0003): a dynamic VolumeSnapshot of the
// source PVC, re-bound into the operator namespace as a static VS/VSC pair, then a ReadWriteOnce
// temp PVC created FROM the static snapshot. On Ceph RBD (and any CSI driver whose
// create-from-snapshot is copy-on-write) this is already minimal-movement, since a PVC created
// from an RBD snapshot is a lazy COW clone rather than a real data copy at the nominal clone
// depth. It is correct-but-a-full-copy on drivers that implement create-from-snapshot as an
// eager copy, which is exactly why CephFS is routed to cephfsShallowExposer instead (see
// Registry.For).
type csiGenericExposer struct {
	client client.Client
	// operatorNamespace is crystal-backup-system: where the static VolumeSnapshot and the temp
	// PVC are created (the mover mounts the temp PVC there). Fixed at construction from the
	// Registry.
	operatorNamespace string
	// volumeSnapshotClass is the VolumeSnapshotClass name Registry.For resolved for this
	// PVC's provisioner. Fixed at construction so Expose never has to re-resolve it.
	volumeSnapshotClass string
}

// newCSIGenericExposer builds a csiGenericExposer preconfigured with the operator namespace and
// the resolved VolumeSnapshotClass name (see Registry.For — "the returned exposer is
// preconfigured with the resolved VolumeSnapshotClass name").
func newCSIGenericExposer(c client.Client, operatorNamespace, volumeSnapshotClass string) *csiGenericExposer {
	return &csiGenericExposer{client: c, operatorNamespace: operatorNamespace, volumeSnapshotClass: volumeSnapshotClass}
}

// Kind implements SnapshotExposer.
func (e *csiGenericExposer) Kind() string { return KindCSIGeneric }

// Expose implements SnapshotExposer: create the dynamic VolumeSnapshot and return the Exposure.
// It does not wait or create the static objects/temp PVC (that is Ready's job) and is safe to
// retry (the Create tolerates AlreadyExists). See expose.
func (e *csiGenericExposer) Expose(ctx context.Context, req ExposeRequest) (*Exposure, error) {
	return expose(ctx, e.client, e.Kind(), req, e.operatorNamespace, e.volumeSnapshotClass)
}

// Ready implements SnapshotExposer: it drives the static re-bind and reports readiness (static
// snapshot readyToUse AND temp PVC Bound), building the temp PVC ReadWriteOnce. See ready.
func (e *csiGenericExposer) Ready(ctx context.Context, ex *Exposure) (bool, error) {
	return ready(ctx, e.client, ex, buildTempPVC)
}

// Cleanup implements SnapshotExposer: tear the exposure down in ADR step-5 order, idempotently.
// See cleanup.
func (e *csiGenericExposer) Cleanup(ctx context.Context, ex *Exposure) error {
	return cleanup(ctx, e.client, ex)
}

// buildTempPVC constructs csi-generic's temp PVC: ReadWriteOnce, in the operator namespace, on
// the source's own StorageClass, at the resolved capacity, sourced from the static VolumeSnapshot
// (ex.StaticVSName).
//
// ReadWriteOnce — not ReadOnlyMany — is deliberate and matches ADR 0003's default: a
// crash-consistent snapshot can carry a dirty filesystem journal, and only a writable mount
// lets the kubelet replay it (an always-read-only volume can fail to mount outright; see the
// ADR's ext4/journal discussion). The mover still mounts this PVC read-only regardless
// (internal/mover.PVCMount forces ReadOnly at both the volume source and the mount) — that is
// a different axis (how the MOVER consumes it) from this volume's own access mode (how the
// KUBELET is allowed to mount it, including for journal replay during the bind).
func buildTempPVC(ex *Exposure, capacity resource.Quantity) *corev1.PersistentVolumeClaim {
	return newTempPVCFromSnapshot(ex, capacity, corev1.ReadWriteOnce)
}
