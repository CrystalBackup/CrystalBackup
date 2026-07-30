---
title: Architecture
description: The components, how a backup flows end to end, how a restore exposes its target, and why every unit of work is a Job.
sidebar:
  order: 1
---

## Components

**The operator** — a Go controller-runtime process in `crystal-backup-system`. It
reconciles every custom resource of both planes and runs the schedule, backup, restore,
discovery, maintenance and external-sync controllers, plus an orphan reaper. It serves the
dynamic admission webhook and the metrics endpoint.

**It never touches backup data bytes.** Not one. Every byte moves inside a Job.

**The mover image** — restic plus a thin Go shim, in two roles:

- `crystal-mover` backs up or restores **one PVC**: it mounts a read-only snapshot path,
  runs restic, and reports a structured result. It also runs `prune`, `forget`, `check`,
  `unlock` and the discovery inventory.
- `crystal-manifest-mover` dumps and restores a namespace's sanitized manifests. It is the
  **only** mover identity that reaches the API server, transiently bound per Job to a
  reader or writer role.

**The sync image** — the same shim and the same pinned restic, plus rclone. A third image
rather than a bigger mover, so rclone's dependency surface stays off the backup and restore
path. Pulled only when an external sync exists.

**`BackupRepository`** — an operator-internal object per repository, carrying its inventory,
check results and maintenance history, and owning its exclusive queue. Not something you
write.

Movers run **only** in `crystal-backup-system`, on both planes, unprivileged. Never in a
tenant namespace.

## The cascade

```
ClusterBackupSchedule ──cron──▶ ClusterBackup ──fan-out──▶ Backup (one per namespace) ──▶ movers
BackupSchedule ────────cron─────────────────────────────▶ Backup (same namespace) ─────▶ movers
```

`Backup` is the single unit of execution, driven identically whichever plane created it —
and it is *also* the projection of a restorable backup. That double duty is the design's
centre of gravity; see [The cascade](/CrystalBackup/docs/understand/cascade/).

## A backup, end to end

**1 — Resolve and initialise.** Find the location and its `BackupRepository`, initialise the
restic repository on first use (serialised through the repository's exclusive queue, so two
concurrent first backups cannot race), list the target PVCs.

**2 — Pre hooks.** Exec into the pods **mounting the PVCs this run is capturing**. Candidacy
by mounted PVC rather than by label is what confines the exec to workloads whose data is
actually being taken. The run **stops and persists the record before any snapshot exists**:
a controller that dies between the quiesce and the snapshot must come back knowing it froze
something.

**3 — Snapshot.** One `VolumeSnapshot` per PVC, in the origin namespace, waited to
`ReadyToUse`. Crash-consistent, point in time.

**4 — Post hooks.** Run as soon as every snapshot is **cut** — not on their having
succeeded, and not after the upload. Hooks bound the freeze window, not the transfer. The
release is unconditional and retried; the freeze is the thing that costs availability.

**5 — Expose and move.** Per PVC, the operator picks the cheapest exposure for that CSI:

| Exposer | Used for | What it does |
|---|---|---|
| `cephfs-shallow` | CephFS | A read-only `backingSnapshot` mount. Zero copy. |
| `csi-generic` | RBD and other snapshot-capable CSIs | Re-binds the `VolumeSnapshotContent` into `crystal-backup-system` as a static VS/VSC pair with a temporary copy-on-write PVC. |
| `rook-rbd-direct` | opt-in only | The only privileged path, confined to the operator namespace. |

A PVC whose CSI cannot snapshot is **skipped**: `status.volumes[].phase: Skipped`,
`reason: CSISnapshotUnsupported`, plus an Event. Never silently dropped.

Then a `crystal-mover` Job mounts the exposed volume **read-only** and runs `restic backup`
with the tags. Movers are spread across nodes with topology-spread constraints to aggregate
bandwidth, under a cluster-wide concurrency cap.

The re-bind in `csi-generic` is the mechanism that keeps tenant data out of tenant reach:
the snapshot is taken in the tenant's namespace, and mounted centrally, because
`VolumeSnapshotContent` is cluster-scoped and can be re-bound.

**6 — Manifests.** A `crystal-manifest-mover` Job dumps the namespace's resources,
sanitizes them and uploads them as a `kind=manifests` snapshot.

**7 — Clean up.** Delete the temporary PVC, the static VS/VSC pair and the origin
`VolumeSnapshot`; write per-PVC status; **enqueue** the retention `forget` on the
repository's maintenance queue — never inline at backup completion.

**8 — Failures.** Per-PVC status with a `PartiallyFailed` phase, Job `backoffLimit`, and an
orphan reaper that garbage-collects leftover temporary PVCs, VS/VSC pairs and stale
repository locks.

## A restore, end to end

Restore is **generic**: a mover mounts the target volume read-write and runs
`restic restore`. There is no Ceph dependency on this path, which is what lets a
Ceph-backed namespace be restored onto a cluster that has never heard of Ceph.

Movers run in the operator namespace, so the restore has to bridge into a tenant namespace
somehow. The bridge is the cluster-scoped `PersistentVolume`, in two mechanisms:

**`pvc-transplant`** — the target PVC does not exist. The operator provisions a temporary
PVC in `crystal-backup-system`, sized and classed from the snapshot's `pvcsize`,
`pvcclass` and `pvcmodes` tags. The mover is its first consumer, so `WaitForFirstConsumer`
classes bind naturally. On success the PV's reclaim policy is flipped to `Retain`, the
temporary PVC is deleted, the `claimRef` is re-pointed, and the final PVC is created
**pre-bound** in the target namespace under the original name.

**`pv-twin`** — the target PVC exists and is bound. A twin PV clones the bound PV's CSI
source with `Retain`, pre-bound to a temporary PVC in the operator namespace. If the volume
is attached to exactly one node, the mover Job is pinned to that node. RWX needs no pin.

Either way the restored PVC ends up carrying `crystalbackup.io/restored-from` and **none**
of the operator's reaper labels — it is your object, and nothing will collect it.

## Discovery

Per repository, on location add and every `discovery.interval`:

1. `restic snapshots --json --tag crystalbackup`, grouped by `(namespace, run)`.
2. For each group whose namespace **exists**, ensure a `Backup` named for the run projects
   into it, with `status.volumes` derived from the snapshots. Namespaces that do not exist
   are skipped — still reachable through `ClusterRestore`, which reads the repository
   directly.
3. Remove projections whose snapshots are gone.

The consequence is the one worth remembering: **projection lifetime equals data lifetime**,
so `kubectl get backups -n X` lists exactly what is restorable in X.

Discovery listings pass `--no-lock`, so they never queue behind a maintenance window.

## Maintenance

Everything exclusive runs on a **per-repository exclusive queue**, one operation at a time,
FIFO, never inline. Different repositories run fully in parallel.

Reads are not queued. `snapshots`, `stats` and `ls` pass `--no-lock`, and data movers count
as readers — many can run against one repository at once.

Two operations additionally **drain the movers** first:

- `unlock`, because removing all locks would rip out a live backup's lock. That one is
  about correctness.
- `prune`, for a different reason — **throughput**. Restic's own exclusive lock already
  prevents corruption; without a drain the prune and the whole mover fleet stare at each
  other on lock retries until one gives up. The drain converts a contention storm into one
  short serialised window.

`forget` and `check` do not drain; their queue turn suffices.

The drain has its own, shorter deadline than the operation it precedes: the operation's
budget is about correctness (a cap sized for `forget` would kill every prune before it
converged, permanently), while the drain's is about availability, because it holds mover
admission shut cluster-wide.

:::note[Single writer]
The queue is an in-process, per-repository single-flight scoped to a **single leader**. It
is deliberately not a distributed lock. Running `prune` or `forget` yourself, out of band,
is outside its assumption.
:::

## Every unit of work is a Job

Not a goroutine. Four rules follow, and they are the difference between an operator that
survives a restart and one that leaks:

**Deterministic names.** A Job's name is a pure function of what it does — never random. On
restart the operator re-adopts by creating and tolerating `AlreadyExists`, rather than
starting a second mover against the same volume.

**Poll through a transient `NotFound`.** Cache lag is "not ready yet", not "gone". Treating
it as a failure is how a healthy run gets declared dead.

**Explicit deletion propagation**, so a same-name Job can be recreated cleanly.

**A self-cleaning backstop.** Jobs set `ttlSecondsAfterFinished`, so a controller that never
comes back does not leave pods behind.

The same discipline shows up in teardown: a terminal `Backup` is marked
`crystalbackup.io/exposures-cleaned` only once the sweep has verified every exposure object
was collected. Until that annotation is present, the controller **re-runs** the idempotent
sweep rather than returning. Teardown interrupted at any instant is retried by the next
pass or the next process, instead of being sealed forever.

## Repository layout

```
s3://<bucket>/<prefix>/<clusterID>/
```

| restic field | Value |
|---|---|
| `host` | the `clusterID` |
| `paths` | `/data/<namespace>/<pvc>`, `/manifests/<namespace>`, `/cluster-manifests` |
| `tags` | `crystalbackup`, `tenant=`, `namespace=`, `pvc=`, `kind=`, `schedule=`, `run=`, and `pvcsize=`/`pvcclass=`/`pvcmodes=` on data snapshots |

`clusterID` being both the restic host and a path segment is what lets one bucket serve
several clusters without collision.

## See also

- [The cascade](/CrystalBackup/docs/understand/cascade/)
- [Tenancy and isolation](/CrystalBackup/docs/understand/tenancy/)
- [Design choices](/CrystalBackup/docs/understand/design-choices/)
