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

// cephfsShallowExposer is ADR 0003's auto-selected exposer for CephFS provisioners: the same
// dynamic-snapshot + static re-bind as csiGenericExposer, then a ReadOnlyMany "shallow" temp PVC
// that references the snapshot directly instead of cloning it. This matters because, on CephFS, a
// normal (writable) create-from-snapshot PVC is a FULL subvolume copy — O(data), unlike RBD's
// lazy COW clone — which would violate R11's "no full clone" and load the storage backend on
// every backup. A ReadOnlyMany snapshot-backed volume avoids that copy entirely (zero-copy,
// constant-time provisioning). It replaces csi-generic ONLY at the temp-PVC step (ADR step 3);
// steps 1-2, 4-5 and Cleanup are shared verbatim.
//
// Its live mount/bind mechanics (ceph-csi's backingSnapshot behaviour) are validated in the
// crucible (c-media, per test/crucible/tests/m1_cascade_test.go), NOT here — see the
// package's own report for exactly what this implementation approximates versus what M1
// leaves for crucible-only validation.
type cephfsShallowExposer struct {
	client client.Client
	// operatorNamespace is crystal-backup-system: where the static VolumeSnapshot and the temp
	// PVC are created. Fixed at construction from the Registry.
	operatorNamespace string
	// volumeSnapshotClass is the VolumeSnapshotClass name Registry.For resolved for this
	// PVC's CephFS provisioner. Fixed at construction so Expose never has to re-resolve it.
	volumeSnapshotClass string
}

// newCephFSShallowExposer builds a cephfsShallowExposer preconfigured with the operator namespace
// and the resolved VolumeSnapshotClass name (see Registry.For).
func newCephFSShallowExposer(c client.Client, operatorNamespace, volumeSnapshotClass string) *cephfsShallowExposer {
	return &cephfsShallowExposer{client: c, operatorNamespace: operatorNamespace, volumeSnapshotClass: volumeSnapshotClass}
}

// Kind implements SnapshotExposer.
func (e *cephfsShallowExposer) Kind() string { return KindCephFSShallow }

// Expose implements SnapshotExposer: create the dynamic VolumeSnapshot and return the Exposure.
// Identical to csiGenericExposer.Expose except for Kind — the divergence is Ready's temp PVC. See
// expose.
func (e *cephfsShallowExposer) Expose(ctx context.Context, req ExposeRequest) (*Exposure, error) {
	return expose(ctx, e.client, e.Kind(), req, e.operatorNamespace, e.volumeSnapshotClass)
}

// Ready implements SnapshotExposer: it drives the same static re-bind as csi-generic and reports
// readiness, differing ONLY in that it builds the temp PVC ReadOnlyMany (buildShallowPVC). A
// snapshot-backed ROX volume still goes through the normal Kubernetes bind lifecycle
// (status.phase == Bound); ceph-csi's "shallow" behaviour changes how the backend satisfies the
// mount, not the Kubernetes-visible binding contract. See ready.
func (e *cephfsShallowExposer) Ready(ctx context.Context, ex *Exposure) (bool, error) {
	return ready(ctx, e.client, ex, buildShallowPVC)
}

// Cleanup implements SnapshotExposer: tear the exposure down in ADR step-5 order, idempotently —
// identical to csiGenericExposer.Cleanup (ADR 0003: "Cleanup identical"). See cleanup.
func (e *cephfsShallowExposer) Cleanup(ctx context.Context, ex *Exposure) error {
	return cleanup(ctx, e.client, ex)
}

// buildShallowPVC constructs cephfs-shallow's temp PVC: ReadOnlyMany, in the operator namespace,
// at the resolved capacity, sourced from the static VolumeSnapshot (ex.StaticVSName), on the SAME
// StorageClass as the source PVC.
//
// DOCUMENTED CHOICE: this reuses ex.StorageClass rather than inventing a "<sc>-shallow" naming
// convention. ADR 0003 only requires "ReadOnlyMany + backingSnapshot enabled (ceph-csi default)"
// — backingSnapshot is a StorageClass PARAMETER that ceph-csi defaults to true, not a second
// StorageClass object — so reusing the source's own class:
//
//   - needs zero new cluster configuration (no admin has to pre-provision and name a second
//     StorageClass per CephFS class before M1 works at all);
//   - has no naming convention to get wrong or fall out of sync with what actually exists;
//   - matches the ADR's own framing, which never mentions a distinct class for this step.
//
// If a target cluster ever needs backingSnapshot forced on a class where it is not the
// provisioner default, that is a Registry.For-level concern (resolve a distinct shallow class
// name, exactly like the VolumeSnapshotClass lookup already does) — this builder stays a pure
// function of whatever Exposure it is handed, so that extension would not change its shape.
func buildShallowPVC(ex *Exposure, capacity resource.Quantity) *corev1.PersistentVolumeClaim {
	return newTempPVCFromSnapshot(ex, capacity, corev1.ReadOnlyMany)
}
