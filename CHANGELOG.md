# Changelog

All notable changes to Crystal Backup. Versioning follows
[adr/0014](spec/adr/0014-versioning-and-release.md): milestone `Mn` → minor `0.n.z` on
major 0; `1.0.0` is a deliberate post-M9 API-stability decision.

## 0.2.0 — M2 “Restore” (2026-07-18)

The restore milestone (R2 cornerstone, R6, R7, R14, R23): everything a backup wrote in M1
now comes back — self-service, mediated, byte-verified — including into namespaces that no
longer exist.

### Added

- **`Restore` controller** (namespaced, self-service): consumes a `Backup` in its own
  namespace (name or `time: latest`/RFC3339 + origin), `Recreate`/`Overwrite` modes ×
  NetworkPolicy-style `volumes[]` selection with file-level `include`/`exclude` (partial
  restore, R7), and the R23 `AwaitingConfirmation` flow re-checked at execution.
- **Operator-mediated cluster-DR restore** (R2/R14 cornerstone): a cluster-origin source is
  resolved exclusively through a repository listing filtered server-side by
  `namespace=<the CR's namespace>` — snapshot IDs from the projection are never trusted,
  and a coordinate outside the namespace fails closed (`SnapshotNotFound`).
- **`ClusterRestore` controller** (admin DR): restores a **repo coordinate** (location +
  origin namespace + run/time) with `target.createNamespace` and `storageClassMapping`;
  works with zero surviving in-cluster objects (R26).
- **Restore target exposure** ([adr/0016](spec/adr/0016-restore-execution-and-target-exposure.md)):
  movers stay in `crystal-backup-system` (the repository key never enters a user
  namespace); an absent target PVC is provisioned and **transplanted** (WFFC-safe PV
  re-bind, provenance annotation `crystalbackup.io/restored-from`), a bound one is written
  through a Retain-only **twin PV** with a same-node pin for singly-attached RWO volumes.
- **Restore mover**: `OpRestore` mounts the target read-write, runs
  `restic restore --overwrite always [--delete]` with `--sparse` and full xattr/ACL fidelity
  caps (CHOWN, DAC_OVERRIDE, FOWNER, MKNOD, SETFCAP — PSA-baseline legal), and reports a
  summary-verified `restoredBytes`.
- **PVC-meta snapshot tags** (`pvcsize`, `pvcclass`, `pvcmodes`) on every data snapshot, so
  `ClusterRestore` recreates PVCs at their original size/class/modes from the repository
  alone (documented fallback for pre-0.2 snapshots).
- **Admission, VAP-first** ([adr/0010](spec/adr/0010-admission-vap-first.md)): the chart now
  ships `ValidatingAdmissionPolicy` objects for R23 confirmation (Restore, ClusterRestore,
  ClusterErasure — empty parks, wrong is denied), user isolation (operator SA exempt),
  Immutable-forbids-prune, denied namespaces (ConfigMap `paramRef`), namespace-selector
  shape and external-sync distinctness; plus the one dynamic webhook — single-default
  `ClusterBackupLocation` — fail-open with a chart-generated certificate.
- **Wrapped-DEK bucket escrow** (bare-cluster DR bootstrap, 03-security §4): the age
  ciphertext is mirrored to `<prefix>/<clusterID>.crystal-meta/wrapped-dek.age` and
  recovered automatically when a location is re-created on a fresh cluster with the KEK.
- **Restore metrics** (R19): `crystalbackup_restore_*` / `crystalbackup_clusterrestore_*`
  (last success, restored bytes, failures), state-derived and namespace-labelled.
- **Docs**: [docs/RESTORE.md](docs/RESTORE.md) (user guide + bare-cluster DR runbook);
  `Restore`/`ClusterRestore` samples.

### Changed

- The orphan reaper resolves restore-owned residue (staging claims, twin/transplant PVs,
  restore movers) and can never touch a delivered volume (handover strips the labels).
- The stale-lock unlock machinery is shared: a hard-killed **restore** mover triggers the
  same quiescence-gated `unlock --remove-all` a backup mover does (adr/0015).
- Operator RBAC: PersistentVolume write + VolumeAttachment/Node read (the adr/0016
  machinery; the twin's same-node pin is dropped when the node is gone or NotReady).
- `source.backup`/`source.time` are mutually exclusive (CEL); `targetPath` rejects `..`;
  `source`, `mode` (and `ClusterRestore`'s `target.namespace`) are immutable after
  creation — a mid-run edit cannot mix two points in time in one restore. A time-resolved
  (`latest`/cutoff) source is pinned for the restore's lifetime; a zone-less
  `YYYY-MM-DDThh:mm:ss` is read as UTC.
- Admission rule 8 counts **non-empty** positive selector forms (an empty `matchNames: []`
  no longer masks — or trips over — a real form), denies an absent selector with the
  rule-8 message instead of a CEL evaluation error, and exempts the operator SA.
- The exposure mechanism is **sticky per volume**: once a staging claim exists, its shape —
  never the live target state — decides transplant vs twin, so a target PVC appearing (a
  StatefulSet recreating its claim) or vanishing mid-restore can no longer misroute the
  handover. A restore runs at most **4 concurrent movers per owner** (slots free as movers
  finish; the cross-kind global semaphore remains a roadmap item), and a mediated-resolution
  listing Job is only re-adopted when its baked restic argv matches the current filter —
  a leftover listing from before a controller restart can never masquerade as a different
  run's resolution.
- Validated end to end on real infrastructure (Hetzner RKE2 + Ceph RBD/CephFS + longhorn +
  Hetzner Object Storage): the full crucible suite is **31/31 green** — every restore mode
  and selection byte-verified against the seed, the tampered-projection R14 negative caught
  fail-closed, and a deleted namespace reconstituted from the repository coordinate alone.
  Two defects only real Ceph could surface were fixed: the **pvc-transplant handover
  deadlock** — a completed mover pod kept the staging claim pinned by the pvc-protection
  finalizer, so the handover (which must delete that claim) could never finish; the mover
  result is now stamped on the Job and the pod deleted each pass, backed by a scoped
  `pods:delete` grant in the operator namespace — and a **duplicate-plan bug** where a
  repository holding several snapshots of one PVC under a run made the namespaced restore
  restore it twice (`restorableVolumes` now dedupes by PVC, like the ClusterRestore path).

## 0.1.0 — M1 “Core engine & cluster DR” (2026-07-17)

The restic-backed backup engine and the cluster-DR plane: `ClusterBackupLocation` /
`BackupRepository` (lazy init through the per-repo exclusive queue), the
`ClusterBackupSchedule → ClusterBackup → Backup → movers` cascade with restic-tag tenancy
(adr/0009), envelope encryption (age KEK → per-location DEK, adr/0004), CSI-generic
snapshot exposure (adr/0003), discovery projection (repository as source of truth, R26),
retention, the orphan reaper, mover-concurrency limits, metrics v1, and the backup⇄unlock
reliability mutex (adr/0015). Field-validated by the crucible on a live RKE2 + rook-ceph +
longhorn + local-path platform (25/25 specs).

## 0.0.0 — M0 “Project scaffolding”

Kubebuilder layout, the twelve `crystalbackup.io/v1alpha1` CRDs, CI (lint/test/e2e,
apko/Wolfi multi-arch images with SBOM + SLSA provenance), envtest + kind harnesses, Helm
chart skeleton.
