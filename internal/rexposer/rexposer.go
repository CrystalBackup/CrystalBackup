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

// Package rexposer implements restore TARGET exposure (spec/adr/0016): making the PVC a
// restore writes into mountable — read-write — by a mover Job confined to the operator
// namespace. It is the write-direction sibling of internal/exposer and copies its whole
// discipline: a pure LIBRARY that creates and tears down a bounded set of objects on
// request, never watches or requeues, leaves all retry/backoff/status policy to the calling
// controller, names everything deterministically from a caller-supplied prefix, stamps the
// caller's labels on every object it creates, and keeps every verb idempotent so a
// restarted controller re-drives it safely.
//
// # Why PV-level exposure (adr/0016)
//
// The restore mover holds repository credentials (for a cluster-DR restore, the platform
// DEK) and therefore MUST run in the operator namespace (invariants I3/I5); a pod mounts
// only PVCs of its own namespace; and the restore target PVC lives in the tenant (or
// ClusterRestore target) namespace. The bridge is the cluster-scoped PersistentVolume, in
// two mechanisms selected by the TARGET PVC's state:
//
//   - pvc-transplant (KindTransplant) — the target PVC does NOT exist. Expose provisions a
//     STAGING PVC (NamePrefix+"-target") in the operator namespace with the requested
//     class/capacity/modes; the mover is its first consumer (WaitForFirstConsumer-safe) and
//     restores into it. After the mover succeeds, Finalize TRANSPLANTS the volume: label +
//     Retain the PV, delete the staging PVC, re-point claimRef, create the final PVC
//     pre-bound in the target namespace, then restore the reclaimPolicy and REMOVE the
//     operator labels — the volume is now the user's, invisible to reaper and leak-check.
//   - pv-twin (KindTwin) — the target PVC exists and is BOUND. Expose creates a TWIN PV
//     (NamePrefix+"-twin") cloning the bound PV's volume source (same volumeHandle for CSI)
//     with reclaimPolicy Retain ALWAYS (deleting the twin must delete the PV object, never
//     the underlying volume), pre-bound to a staging PVC in the operator namespace; the
//     mover writes the target volume through it. If a VolumeAttachment pins the underlying
//     volume to exactly one node, the exposure records that node so the caller pins the
//     mover there (an RWO volume must not be asked to attach twice).
//
// An EXISTING but UNBOUND target PVC is neither: it holds no data and no volume. Expose
// returns ErrTargetUnbound and the controller — which owns destructive policy — deletes it
// (behind the R23 confirmation it already holds) and re-drives, landing on pvc-transplant.
// A volumeMode: Block target is ErrBlockUnsupported (restic restores files, not devices).
//
// # The leak-check invariant
//
// Every object this package creates carries the caller's labels (the reaper/leak-check
// selector) — EXCEPT the final transplanted PVC and its PV once handover completes, which
// deliberately lose/never get them (a restored volume is the user's object). Cleanup is
// idempotent and safe on both outcomes: it removes the staging PVC and the twin PV object,
// and reclaims a transplant volume whose handover never completed (labels still present, no
// final claim) by restoring reclaimPolicy Delete — so a failed restore leaks no storage.
package rexposer

import (
	"errors"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Kind values identify which mechanism produced a TargetExposure. Stable strings, not iota:
// a restarted controller re-derives the exposure and must recognise which cleanup/finalize
// path applies (see internal/exposer's Kind rationale).
const (
	// KindTransplant is the fresh-provision + PV-transplant mechanism (target PVC absent).
	KindTransplant = "pvc-transplant"
	// KindTwin is the twin-PV mechanism (target PVC exists and is bound).
	KindTwin = "pv-twin"
)

// ErrTargetUnbound reports an EXISTING target PVC with no bound volume (a
// WaitForFirstConsumer claim no pod ever used). The caller owns destructive policy: it
// deletes the empty claim (behind the R23 confirmation it already enforced) and re-drives
// Expose, which then provisions via pvc-transplant.
var ErrTargetUnbound = errors.New(
	"restore target PVC exists but is not bound: delete it and re-expose to provision via transplant")

// ErrBlockUnsupported reports a volumeMode: Block target. restic restores files into a
// filesystem; a raw block target has none, so the volume is surfaced as failed
// (RestoreBlockUnsupported) rather than silently skipped.
var ErrBlockUnsupported = errors.New("restore into a volumeMode: Block PVC is not supported")

// Object-name suffixes, appended to TargetRequest.NamePrefix. The staging suffix is shared
// by both mechanisms so the mover mount name never depends on the mechanism.
const (
	stagingPVCSuffix = "-target"
	twinPVSuffix     = "-twin"
)

// StagingPVCName returns the operator-namespace staging PVC name for a prefix — exported so
// the calling controller derives the same name when tearing down without an Exposure value.
func StagingPVCName(prefix string) string { return prefix + stagingPVCSuffix }

// TwinPVName returns the twin PV name for a prefix (see StagingPVCName).
func TwinPVName(prefix string) string { return prefix + twinPVSuffix }

// TargetRequest is everything Expose needs to make ONE restore target mountable. Every
// field is caller-resolved (capacity/class/modes come from the source Backup's PVC-meta
// tags, the target PVC itself, or the documented fallback — never from a user-writable
// free field). Like exposer.ExposeRequest it carries no live references, so a restarted
// controller reconstructs an identical request and converges on the same objects.
type TargetRequest struct {
	// TargetNamespace and PVCName identify the PVC being restored into (the final PVC for a
	// transplant; the existing claim for a twin).
	TargetNamespace string
	PVCName         string
	// StorageClass is the class a transplant provisions with (already storageClassMapping-
	// rewritten by the caller); empty means the cluster default. Ignored for a twin (the
	// bound PV dictates it).
	StorageClass string
	// Capacity is the size a transplant requests. Ignored for a twin.
	Capacity resource.Quantity
	// AccessModes are the modes a transplant requests (defaulted to ReadWriteOnce when
	// empty). Ignored for a twin (copied from the bound PV).
	AccessModes []corev1.PersistentVolumeAccessMode
	// NamePrefix is the deterministic per-volume prefix (e.g. "<restore>-<pvc>", sanitized
	// by the caller); staging/twin names derive from it.
	NamePrefix string
	// Labels are stamped on every created object: the reaper/leak-check selector plus the
	// restore-owner identity (crystalbackup.io/restore or /cluster-restore).
	Labels map[string]string
	// RestoredFromRun, when non-empty, is recorded as the crystalbackup.io/restored-from
	// annotation on a transplant's FINAL PVC (provenance for the user; never a label).
	RestoredFromRun string
}

// TargetExposure is the durable result of Expose: names only, all deterministic from the
// request (plus the operator namespace), so Ready/Finalize/Cleanup — possibly in a later
// process — re-find the same objects. StagingPVCName is what the mover mounts READ-WRITE.
type TargetExposure struct {
	// Kind is KindTransplant or KindTwin.
	Kind string
	// TargetNamespace / TargetPVCName echo the request (the claim being restored into).
	TargetNamespace string
	TargetPVCName   string
	// OperatorNamespace is where the staging PVC lives and the mover runs.
	OperatorNamespace string
	// StagingPVCName is the operator-namespace PVC the mover mounts read-write.
	StagingPVCName string
	// TwinPVName is the twin PV (KindTwin only; empty for a transplant).
	TwinPVName string
	// NodeName pins the mover to the node the target volume is attached on (KindTwin with
	// exactly one live VolumeAttachment); empty means unpinned.
	NodeName string
	// Labels echo the request's labels (the selector Cleanup and the reaper re-derive).
	Labels map[string]string
	// RestoredFromRun echoes the request (Finalize stamps it on the final PVC).
	RestoredFromRun string
}
