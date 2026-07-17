# ADR 0003 — Snapshot exposure: Ceph-aware selection (csi-generic default, cephfs-shallow for CephFS)

Status: **Accepted** (2026-07-11, product owner + tech lead; amended 2026-07-15 — `cephfs-shallow`
promoted to v1 and auto-selected for CephFS, `rook-rbd-direct` kept opt-in, unsupported-CSI
volumes skipped with status; the file name is retained for link stability)

## Context

Backing up a PVC requires reading a **point-in-time, read-only** view of its data from a
mover Job (R11), without a full disk clone, in parallel across nodes (R12), on clusters
with or without Rook Ceph (R4/R6). Three exposure strategies were studied during the
2026-07-11 research pass:

1. **`csi-generic`** — `VolumeSnapshot` → temp PVC from snapshot → unprivileged mover.
   The Velero CSI Snapshot Data Movement pattern
   ([velero.io/docs/main/csi-snapshot-data-movement](https://velero.io/docs/main/csi-snapshot-data-movement/)).
2. **`rook-rbd-direct`** — `rbd device map --read-only pool/image@snap` (krbd) from a
   privileged Job; zero intermediate Kubernetes volume objects.
3. **`cephfs-shallow`** — ceph-csi snapshot-backed (shallow) volumes: a `ReadOnlyMany`
   PVC created from a `VolumeSnapshot` references the snapshot directly, zero clone
   ([ceph-csi design: cephfs-snapshot-backed-volumes](https://github.com/ceph/ceph-csi/blob/devel/docs/design/proposals/cephfs-snapshot-backed-volumes.md)).

Key facts established by the research on our primary storage (ceph-csi RBD):

- A PVC created from a `VolumeSnapshot` is a **lazy copy-on-write clone**
  (`rbd clone --rbd-default-clone-format 2`): creation is instant, initial space
  consumption is ~zero, reads fall through to the parent
  ([ceph-csi design: rbd-snap-clone](https://github.com/ceph/ceph-csi/blob/devel/docs/design/proposals/rbd-snap-clone.md)).
- **Flatten** (real data copy) is asynchronous (ceph-mgr `rbd_support` task) and only
  triggers on deep clone chains: soft limit `--rbdsoftmaxclonedepth=4` (background
  flatten), hard limit `--rbdhardmaxclonedepth=8` (`ReadyToUse=false` until done). In the
  nominal daily-backup regime — snapshot → temp PVC → backup → delete both — the chain
  depth stays at **2** (parent → csi-snap image → temp clone), so **no flatten ever
  occurs**. Deletion goes through `rbd trash mv` + an async mgr task.
- Consequently the generic path is already "hollow enough" on RBD: the theoretical space
  and time advantage of `rook-rbd-direct` is an architectural optimization (no temp PVC
  lifecycle), not a data-copy avoidance.
- No surveyed product does direct `rbd map` today; Velero, VolSync (`copyMethod:
  Snapshot`) and the industry converge on "CSI snapshot + cheapest temp volume".

## Decision

**v1 ships a `SnapshotExposer` interface with Ceph-aware auto-selection of the
least-data-movement path per PVC, defaulting to `csi-generic`.**

- **CephFS → `cephfs-shallow`** (v1): a generic create-from-snapshot on CephFS is a **full
  subvolume copy** (O(data)), which violates R11's "no full clone" and loads the storage backend;
  a snapshot-backed ROX volume (`backingSnapshot`) references the snapshot with **zero copy**.
  CephFS is therefore auto-exposed via `cephfs-shallow`, not `csi-generic`.
- **Ceph RBD → `csi-generic`** (default): a PVC created from an RBD snapshot is already a **lazy
  COW clone** (no data copy at the nominal depth-2 chain), so `csi-generic` is already
  minimal-movement on RBD. `rook-rbd-direct` (privileged krbd map) stays **opt-in** (operator
  config `exposure.rbdDirect: true`) and otherwise **deferred** — it trades a privileged mover +
  hostPath + an orphan-mapping reaper for only a temp-PVC-lifecycle gain, not a data-copy gain, so
  it remains benchmark-gated (M6).
- **Any other snapshot-capable CSI → `csi-generic`.**
- **A CSI that cannot snapshot → the volume is skipped**, never silently: the operator checks
  snapshot capability (a `VolumeSnapshotClass` for the driver / advertised capability) before
  exposing, and on failure sets `Backup.status.volumes[].phase: Skipped` with
  `reason: CSISnapshotUnsupported`, emits an Event and logs it. The Skipped volume is **neutral**
  in the phase roll-up: it stays visibly `Skipped` (never dressed up as a successful backup), yet it
  never degrades the aggregate phase — the `Backup` ends `Completed` (manifests still captured),
  never a hard failure. An unsnapshottable PVC is a deterministic property of the environment, so
  degrading every such run to `PartiallyCompleted` would be permanent alarm noise; per-volume
  visibility (phase + reason) is the right signal instead.

Selection is by StorageClass provisioner (operator config maps provisioner→exposer, extensible;
an explicit per-location/annotation override is the escape hatch).

### The `csi-generic` flow (per PVC, orchestrated by the Backup controller)

1. Create a `VolumeSnapshot` in the **origin namespace** (name
   `crystal-<backup>-<pvc>`, labels `crystalbackup.io/backup`,
   `crystalbackup.io/namespace`); wait for `status.readyToUse: true`.
2. **Static re-bind** (Velero exposer pattern): patch the bound
   `VolumeSnapshotContent.spec.deletionPolicy` to `Retain` **for the duration of the
   handover**, then create a pre-provisioned `VolumeSnapshotContent` + `VolumeSnapshot`
   pair in **`crystal-backup-system`** pointing at the same `snapshotHandle`. `Retain`
   guarantees no intermediate deletion can destroy the storage-side snapshot.
3. Create a **temp PVC** in `crystal-backup-system` with
   `spec.dataSource: {kind: VolumeSnapshot}`, size = source PVC capacity (≥
   `status.restoreSize`), storage class = source class.
   - **Access mode default: `ReadWriteOnce`, writable.** A crash-consistent snapshot
     almost always carries a dirty filesystem journal; a writable mount lets the kubelet
     replay it normally. This is deliberate: an always-read-only volume can **fail to
     mount** (ext4 replays the journal even on `ro` mounts; see kernel
     [admin-guide/ext4](https://www.kernel.org/doc/html/latest/admin-guide/ext4.html)).
   - **`ReadOnlyMany` is an opt-in optimization** per allow-listed StorageClass
     (operator config `exposure.readOnlyManyStorageClasses: []`), mirroring Velero's
     `backupPVC.readOnly` knob — only for drivers/filesystems known to mount cleanly RO.
4. Run the **mover Job** in `crystal-backup-system`, mounting the temp PVC with
   `readOnly: true` on the volume mount. The mover is **unprivileged** and runs under
   PodSecurity **baseline** with baseline-legal capabilities only: the backup mover drops
   ALL then adds `DAC_OVERRIDE` (the write half is moot on the read-only mount; see
   03-security-and-tenancy.md §6), seccomp `RuntimeDefault` — no privileged namespace
   needed in v1.
5. **Cleanup**, in order: delete temp PVC → delete the static VS/VSC pair (policy
   `Retain`, objects only) → restore/delete the origin `VolumeSnapshot` (its original
   VSC `Delete` policy removes the storage snapshot exactly once).

`volumeMode: Block` PVCs are exposed identically; the mover receives a `volumeDevices`
path instead of a mount (backup of raw block devices is a documented limitation: restic
reads a device as a single file, no sub-file dedup granularity guarantees — v1 documents
this; full block support may warrant its own ADR).

**RWO and RWX PVCs follow the exact same path** — the temp PVC is always a fresh volume
in the platform namespace, so source access mode, source mounting pods and source node
placement are all irrelevant to mover scheduling. This is what lets the scheduler spread
movers freely across nodes (R12) and what makes the path work on **any snapshot-capable
CSI driver** (R6), CephFS included.

### The `SnapshotExposer` interface (Go, `pkg/exposer`)

```go
type SnapshotExposer interface {
    // Expose makes one PVC's point-in-time data mountable inside
    // crystal-backup-system; returns the volume source for the mover Job.
    Expose(ctx context.Context, req ExposeRequest) (*Exposure, error)
    // Ready reports whether the exposure is consumable (e.g. temp PVC Bound).
    Ready(ctx context.Context, exp *Exposure) (bool, error)
    // Cleanup tears down all exposure objects. Idempotent; also called by
    // the orphan reaper on objects matching exposure labels.
    Cleanup(ctx context.Context, exp *Exposure) error
}
```

Implementations are registered by name; selection is per StorageClass provisioner (operator
config), Ceph-aware per the Decision. `csi-generic` and `cephfs-shallow` ship in v1;
`rook-rbd-direct` is opt-in/deferred (privileged).

### Opt-in / deferred exposer — `rook-rbd-direct` (RBD; privileged, `exposure.rbdDirect: true`)

Privileged Job confined to `crystal-backup-system` (PSA `privileged` on that namespace
only): derive `pool/csi-snap-<uuid>` from the VSC `snapshotHandle`; `rbd device map
--read-only` (krbd, kernel ≥ 5.1 for `deep-flatten` images); mount `-o ro,noload` (ext4)
or `-o ro,norecovery,nouuid` (XFS — `nouuid` mandatory when the origin PVC is mounted on
the same node, per kernel
[admin-guide/xfs](https://www.kernel.org/doc/html/latest/admin-guide/xfs.html)); Ceph
identity via a dedicated Rook `CephClient` with `osd: profile rbd-read-only pool=<pool>`
(never `client.admin`); umount/unmap on exit. Because krbd mappings **survive pod
death**, an orphan-mapping reaper (`rbd device list` + `rbd device unmap -o force` per
node) is a hard prerequisite, in addition to hostPath `/dev`, `/sys`, `/lib/modules` and
`hostNetwork: true`.

### v1 exposer — `cephfs-shallow` (CephFS, auto-selected)

Temp PVC created from the `VolumeSnapshot` with `accessModes: [ReadOnlyMany]` on a
CephFS StorageClass with `backingSnapshot` enabled (ceph-csi default): the volume
references the snapshot directly — no subvolume clone at all, constant-time provisioning,
unprivileged mover. Both `ReadOnlyMany` **and** `backingSnapshot` are required: a normal
writable/RWO CephFS PVC created from a snapshot is a **full subvolume copy** (O(data),
unlike RBD's COW clone); only a snapshot-backed ROX volume references the snapshot with
zero copy. This replaces `csi-generic` **only at step 3** for CephFS volumes; steps 1–2 and
4–5 are unchanged and the mover stays unprivileged.

## Consequences

### Positive

- One code path for all volumes: RWO, RWX, RBD, CephFS, and any third-party
  snapshot-capable CSI driver (R4, R6).
- Movers are unprivileged with baseline-legal capabilities only (backup mover: drop ALL
  + `DAC_OVERRIDE`, 03-security-and-tenancy.md §6); PodSecurity **baseline** holds on
  every namespace, including `crystal-backup-system`, until (if ever) `rook-rbd-direct`
  lands.
- No Ceph credentials anywhere in the backup system; movers need network access to S3
  only (simple NetworkPolicy).
- Free mover placement → node-spread parallelism (R12) without CSI attach constraints.
- The `SnapshotExposer` interface keeps the optimization door open with zero refactoring
  of the mover, repository, or encryption layers.

### Negative

- **+1 temp PVC and +2 snapshot API objects per volume per backup**: more etcd/API
  churn and more lifecycle states to reconcile; the orphan reaper
  (01-architecture.md §5) is **mandatory, not defensive**.
- Temp PVC provisioning adds scheduler + CSI latency (seconds on Ceph) to each volume's
  critical path.
- On CSI drivers whose create-from-snapshot is a **full copy** (not COW), R11's "no full
  clone" degrades to "full clone on the backup side". This does **not** affect Ceph: **RBD**
  is COW-cheap via `csi-generic`, and **CephFS** is auto-exposed via the v1 `cephfs-shallow`
  exposer (ROX + `backingSnapshot`, zero copy). It remains a revisit trigger for **other**
  full-copy third-party drivers reached through `csi-generic` — they work correctly but pay a
  full copy until a driver-specific exposer or ROX allow-listing is added.
- Restore-from-snapshot on ceph-csi must target the same pool; irrelevant for backup
  (we delete the temp PVC).

### Risks & mitigations

| Risk | Mitigation |
|---|---|
| Orphaned temp PVC / VS / VSC after operator or Job crash | Orphan reaper GCs by exposure labels + age; `Cleanup()` idempotent; **also re-asserts a `Retain`-patched origin VSC's `deletionPolicy` (or deletes the dangling storage snapshot by `snapshotHandle`)** so a crash between the `Retain` patch and its restore cannot leak a storage-side snapshot; e2e kill-tests (08-testing-and-dod.md) |
| Storage snapshot destroyed during handover | `deletionPolicy: Retain` on the bound VSC before re-bind; deletion happens exactly once, in cleanup step 5; dedicated e2e test |
| Clone-chain flatten storms (tenants stacking restores-of-restores) | Nominal depth is 2; expose histogram `crystalbackup_exposure_ready_wait_seconds{namespace,exposer}` — a long tail signals hard-depth-limit flattens (`ReadyToUse=false`); platform alert on ceph-mgr `ceph rbd task list` flatten backlog |
| ROX temp PVC fails to mount on dirty journal | RWO writable is the default; ROX only via explicit StorageClass allow-list |
| Temp PVCs inflate pool usage under heavy concurrency | Global `maxConcurrentMovers` semaphore + per-repo serialization already bound simultaneous exposures |
| `--maxsnapshotsonimage` (default 450) exhaustion from tenant + Velero + Crystal Backup snapshots on one image | Snapshot-count monitoring during Velero coexistence (R22, ADR 0006); backups delete their snapshots at cleanup |

## Alternatives considered

- **`rook-rbd-direct` first** — rejected for v1: requires privileged pods, hostPath
  `/dev`/`/sys`/`/lib/modules`, `hostNetwork`, Ceph key distribution, and a
  non-optional krbd orphan-mapping reaper — significant security surface and
  operational machinery **before any proven need**, given that the generic path is
  already COW-cheap on RBD. Kept as a benchmark-gated exposer.
- **K8up-style live mount of the source PVC** (no snapshot at all) — rejected: no
  point-in-time consistency, fails R11 outright; RWO volumes also force
  node-affinity scheduling of movers, defeating R12 spread.
- **Always-`ReadOnlyMany` temp PVC** — rejected as default: mount can fail on a dirty
  journal (kubelet cannot replay on an RO volume); retained as the opt-in
  allow-list optimization described above.
- **CSI volume clone of the live PVC** (`dataSource: {kind: PersistentVolumeClaim}`,
  VolSync `copyMethod: Clone`) — rejected: clones cannot cross namespaces, which would
  force backup objects or pods into the origin namespace, violating the invariant that a
  backed-up namespace never hosts backup pods (03-security-and-tenancy.md).

## Revisit triggers

- **M6 load/bench results** (90-roadmap.md): if exposure setup/teardown or read
  throughput on the temp-PVC path measurably dominates backup wall-clock or cluster IO
  at production scale, implement `rook-rbd-direct` (RBD) and/or `cephfs-shallow`
  (CephFS) behind the existing interface.
- A target cluster whose CSI driver implements create-from-snapshot as a **full data
  copy** (no COW): the generic path's cost model collapses there; evaluate
  driver-specific exposers or ROX allow-listing for that class.
- Sustained flatten-task alerts or `crystalbackup_exposure_ready_wait_seconds` degradation
  on a production cluster (e.g. `prod-eu-1`) from clone-depth pressure under tenant usage
  patterns.
- `VolumeGroupSnapshot` reaching GA on our clusters: multi-PVC crash-consistent groups
  may change the exposure sequencing (one group snapshot instead of N `VolumeSnapshot`s).
