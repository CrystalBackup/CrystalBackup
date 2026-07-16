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

// Package exposer implements the SnapshotExposer abstraction (spec/adr/0003): turning a
// source PersistentVolumeClaim into a read-only, point-in-time volume that a mover Job can
// back up without quiescing the workload it belongs to. It is a pure LIBRARY — it creates and
// tears down a bounded set of Kubernetes objects on request, but never watches, requeues or
// owns a reconcile loop of its own. The Backup controller (internal/controller — not yet
// built at M1) is the only intended caller and owns all retry/backoff/status policy; this
// package's job stops at "create it", "is it ready", "remove it".
//
// # Two namespaces (ADR 0003's static VS/VSC re-bind)
//
// The mover Job holds the platform DEK and therefore MUST run in the operator namespace
// (crystal-backup-system); a Job can only mount PVCs in its OWN namespace. But a PVC created
// FROM a VolumeSnapshot must live in the SAME namespace as that snapshot, and the source PVC's
// dynamic snapshot is unavoidably in the tenant namespace. So an exposure spans two namespaces
// and performs the Velero-style "static re-bind" the ADR specifies:
//
//   - Origin (tenant) namespace: the source PVC and the DYNAMIC VolumeSnapshot of it
//     (Exposure.OriginNamespace / OriginVSName). This is the only object Expose creates.
//   - Operator namespace (crystal-backup-system): a STATIC, pre-provisioned
//     VolumeSnapshotContent (cluster-scoped) + VolumeSnapshot pair that point at the SAME
//     storage-side snapshotHandle as the origin, plus the temp PVC created from that static
//     snapshot (Exposure.StaticVSCName / StaticVSName / TempPVCName). The mover mounts the
//     temp PVC read-only.
//
// Ready drives that handover (ADR §"The csi-generic flow", steps 1-6): wait for the origin
// snapshot to be readyToUse, patch its bound VolumeSnapshotContent to deletionPolicy=Retain
// "for the duration of the handover" so no intermediate delete can destroy the storage-side
// snapshot, create the static VSC/VS pair against the same snapshotHandle, then create the
// temp PVC from the static snapshot. Cleanup reverses it in the ADR's exact order (step 5),
// ending by RESTORING the origin VSC's deletionPolicy to Delete so the storage snapshot is
// reclaimed exactly once.
//
// Two implementations ship in M1, selected per PVC by Registry.For based on the PVC's
// StorageClass provisioner (ADR 0003's Ceph-aware selection). They share EVERY step above and
// differ only in the temp PVC's access mode:
//
//   - csi-generic (Kind KindCSIGeneric, csiGenericExposer): a ReadWriteOnce temp PVC created
//     from the static snapshot. Works on any CSI driver that can snapshot; the default. On
//     Ceph RBD (and any COW create-from-snapshot) this is already minimal-movement.
//   - cephfs-shallow (Kind KindCephFSShallow, cephfsShallowExposer): a ReadOnlyMany "shallow"
//     temp PVC that references the snapshot directly (zero copy). Auto-selected for CephFS,
//     where a normal create-from-snapshot would be a full O(data) subvolume copy instead of
//     the lazy COW clone RBD gets from csi-generic.
//
// A third exposer, rook-rbd-direct, is opt-in/deferred per the ADR (privileged, benchmark-
// gated) and is intentionally NOT implemented here.
//
// # The leak-check invariant
//
// Every object either exposer creates OR patches is stamped with ExposeRequest.Labels, and
// Cleanup MUST remove them all AND undo the Retain patch, idempotently: it is called both on
// the normal happy path and by the orphan reaper (M1 task #22) on anything its labels match,
// and the crucible's leak-check (test/crucible/tests/m1_reliability_test.go,
// m1AssertNoResidualSnapshotObjects) asserts zero residual VolumeSnapshot /
// VolumeSnapshotContent / temp PVC objects — and no leaked storage-side snapshot — survive a
// run. That invariant is this package's entire reason for being more than a thin wrapper
// around client.Create.
//
// # What is unit-tested vs crucible-deferred
//
// The Ready state machine and Cleanup ordering are unit-tested against a fake client by
// MANUALLY simulating the status a real CSI driver would set (readyToUse, snapshotHandle, PVC
// Bound). Only the REAL CSI behaviour — whether readyToUse actually flips, whether the clone
// mounts, whether the storage snapshot is actually reclaimed on the restored Delete policy —
// is deferred to the crucible; see this package's report.
package exposer

import (
	"context"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Kind values identify which SnapshotExposer implementation produced an Exposure. They are
// stable strings (not iota) because they are effectively persisted: a controller that
// restarts mid-backup reads Exposure.Kind back out of its own status/spec to know which
// exposer's Cleanup to invoke, so renaming a constant would orphan any Exposure a previous
// binary created.
const (
	// KindCSIGeneric is csiGenericExposer's Kind(): VolumeSnapshot + ReadWriteOnce temp PVC.
	KindCSIGeneric = "csi-generic"
	// KindCephFSShallow is cephfsShallowExposer's Kind(): VolumeSnapshot + ReadOnlyMany
	// snapshot-backed ("shallow") temp PVC — zero-copy on CephFS.
	KindCephFSShallow = "cephfs-shallow"
)

// SnapshotExposer produces, from a source PVC, a read-only PVC the mover mounts to back up
// a point-in-time copy without quiescing the workload. Implementations create their objects
// in Expose, report readiness in Ready, and MUST fully remove them in Cleanup (idempotent) —
// the leak-check invariant (zero residual VolumeSnapshot/VolumeSnapshotContent/temp PVC).
type SnapshotExposer interface {
	Kind() string
	Expose(ctx context.Context, req ExposeRequest) (*Exposure, error)
	Ready(ctx context.Context, ex *Exposure) (bool, error)
	Cleanup(ctx context.Context, ex *Exposure) error
}

// ExposeRequest is everything an exposer needs to expose one source PVC. It carries no live
// object references itself — Ready and Cleanup re-read/re-derive what they need from an
// Exposure — which is what keeps the whole SnapshotExposer contract restart-safe: a
// controller reconstructs an equivalent ExposeRequest from its own persisted spec/status
// rather than needing to keep a Go value alive across a process restart.
type ExposeRequest struct {
	// Namespace is the source PVC's (tenant) namespace: the ORIGIN namespace, where the
	// dynamic VolumeSnapshot is created. The static VS/VSC pair and the temp PVC the mover
	// mounts are created in the operator namespace instead (Registry's operatorNamespace),
	// per ADR 0003's static re-bind — see the package doc.
	Namespace string
	// PVCName is the source PVC to snapshot.
	PVCName string
	// StorageClass is the source PVC's storageClassName.
	StorageClass string
	// Capacity is the source PVC's requested storage (temp PVC matches it).
	Capacity resource.Quantity
	// NamePrefix is a deterministic prefix for created objects (e.g. "<backup>-<pvc>"),
	// collision-free per run. The CALLER guarantees that uniqueness; Expose derives the
	// VolumeSnapshot's and the temp PVC's own names from it (see volumeSnapshotName /
	// exposedPVCName) and does no collision detection beyond what a Kubernetes
	// Create(AlreadyExists) surfaces.
	NamePrefix string
	// Labels are stamped on EVERY created object; the orphan reaper + leak-check select on
	// these. This package adds no labels of its own beyond what the caller supplies here, so
	// a caller that wants its objects discoverable later (by a restarted controller, by the
	// reaper) MUST include whatever keys it needs — e.g. the owning Backup's identity.
	Labels map[string]string
}

// Exposure is the durable result of a successful Expose: enough identity for a later Ready or
// Cleanup call — potentially from a restarted process — to find the very same objects again
// by name, without needing the original ExposeRequest. Every name below is DETERMINISTIC from
// ExposeRequest.NamePrefix (plus the operator namespace), so a restarted controller that
// reconstructs the same request rebuilds a byte-identical Exposure and re-drives Ready/Cleanup
// idempotently. It is intentionally a small, JSON/YAML-friendly value (no client handles, no
// live object references), so a controller can persist it verbatim in a Backup's status/spec.
type Exposure struct {
	// Kind is the producing exposer's Kind() (== KindCSIGeneric or KindCephFSShallow),
	// letting a generic caller identify which exposer's Cleanup to use without a Registry
	// round-trip (e.g. useful to the orphan reaper or a restarted controller).
	Kind string
	// OriginNamespace is the tenant namespace holding the source PVC and the DYNAMIC
	// VolumeSnapshot (== the originating ExposeRequest.Namespace).
	OriginNamespace string
	// OperatorNamespace is crystal-backup-system: where the STATIC VolumeSnapshot and the temp
	// PVC live (the cluster-scoped static VolumeSnapshotContent has no namespace). The mover
	// Job runs here and can only mount PVCs here, which is the whole reason for the re-bind.
	OperatorNamespace string
	// OriginVSName is the dynamic VolumeSnapshot Expose creates in OriginNamespace
	// (NamePrefix+"-snap"). Ready reads its readyToUse/boundVolumeSnapshotContentName/
	// restoreSize; Cleanup deletes it last so its restored-to-Delete VSC reclaims the storage
	// snapshot exactly once.
	OriginVSName string
	// StaticVSCName is the cluster-scoped, pre-provisioned VolumeSnapshotContent Ready creates
	// pointing at the origin's storage-side snapshotHandle (NamePrefix+"-vsc").
	StaticVSCName string
	// StaticVSName is the static VolumeSnapshot in OperatorNamespace bound to StaticVSCName
	// (NamePrefix+"-restore"); the temp PVC is created from it.
	StaticVSName string
	// TempPVCName is the temp PVC in OperatorNamespace created from the static snapshot
	// (NamePrefix+"-clone"). ExposedPVCName aliases it.
	TempPVCName string
	// ExposedPVCName is the PVC the mover mounts READ-ONLY. It EQUALS TempPVCName (operator
	// namespace); the field is kept distinct so a caller reads intent ("the volume to mount")
	// rather than an implementation name.
	ExposedPVCName string
	// StorageClass is the temp PVC's storageClassName (== the source PVC's class).
	StorageClass string
	// Capacity is the temp PVC's requested storage floor; Ready upsizes it to the origin
	// snapshot's status.restoreSize when that is larger.
	Capacity resource.Quantity
	// Labels are stamped on EVERY object the exposer creates OR patches — the dynamic VS, the
	// static VS/VSC, the temp PVC, AND the Retain-patched origin VSC — so Cleanup and the
	// orphan reaper can re-derive a label selector without the caller re-supplying it. This is
	// the leak-check/reaper selector.
	Labels map[string]string
}
