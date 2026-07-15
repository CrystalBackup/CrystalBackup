# Architecture

Status: draft v3 (two-plane cascade + shared repository + repository-as-source-of-truth).
Naming contract: see [02-api.md](02-api.md); the model rationale is recorded in
[adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md). Other decisions are in [adr/](adr/).

## 1. Components

```
   cluster plane (admin, cluster-scoped):
     ClusterBackupLocation · ClusterBackupSchedule · ClusterBackup
     ClusterRestore · ClusterErasure · ClusterBackupExternalSync
   namespace plane (user, per namespace):
     BackupLocation · BackupSchedule · Backup · Restore · BackupExternalSync
                         │ watched by
                         ▼
        ┌─────────────────────────────────────┐
        │ operator  (crystal-backup-system)   │
        │  controllers: schedule, backup,     │
        │  restore, discovery, maintenance;   │
        │  admission policies; metrics        │
        └──────────────────┬──────────────────┘
                           │ creates
                           ▼
        ┌─────────────────────────────────────┐
        │ crystal-mover / crystal-manifest-   │
        │ mover Jobs (restic) — 1 per PVC,    │
        │ unprivileged, spread across nodes   │
        └──────────────────┬──────────────────┘
                           │ restic over S3
                           ▼
   object storage:  ONE shared restic repo per ClusterBackupLocation
                    (all namespaces; tenancy by restic tags — adr/0009)
                    + ONE repo per user BackupLocation
                    s3://<bucket>/<prefix>/<clusterID>/{data,manifests}/…

   standalone:  crystalctl (no K8s; S3 creds + key)       — R8, M7
   later:       UI thick client / Rancher ext / Headlamp  — R9
```

- **Operator** (Go, kubebuilder/controller-runtime — [adr/0002](adr/0002-operator-language-go.md)):
  reconciles every CRD across both planes and runs the **schedule**, **backup**, **restore**,
  **discovery**, **maintenance** and **external-sync** controllers; orchestrates snapshots and mover Jobs;
  serves the (dynamic) admission webhook and the metrics endpoint (`crystalbackup_*`); static
  admission is shipped as `ValidatingAdmissionPolicy` ([adr/0010](adr/0010-admission-vap-first.md)).
  It never touches backup data bytes.
- **Mover image** (one container image: `restic` + a thin Go shim —
  [adr/0001](adr/0001-repository-engine-restic-format.md)): runs as a Kubernetes Job. As
  **`crystal-mover`** it backs up/restores one PVC (mounts a read-only snapshot path, runs
  restic, reports a structured result via the termination-message JSON). As
  **`crystal-manifest-mover`** it dumps and restores the namespace's sanitized manifests.
  The same image also runs `prune`/`forget`/`check`. Movers run in `crystal-backup-system`
  and are unprivileged.
- **`crystalctl` CLI** (Go `cmd/crystalctl`, **M7**): standalone binary (no Kubernetes dependency)
  wrapping restic for list/browse/dump/`export tar`/local restore against a repo (R8), plus
  kubectl-style helpers when a kubeconfig is present (trigger a backup, watch status).
- **UI** (later, R9): backend API service opening repositories server-side; see
  [07-ui.md](07-ui.md) and [adr/0008](adr/0008-ui-strategy.md).
- **External-sync controller** (R28, [adr/0013](adr/0013-external-backup-sync.md)): drives
  `ClusterBackupExternalSync`/`BackupExternalSync` by running `restic copy` (via a mover Job in
  `crystal-backup-system`) to replicate a repository to a **secondary** location, re-encrypted
  to the destination's own key. Detailed in §11.

## 2. Repository model (R2, R3, R5, R13, R20)

Two planes, cert-manager-style ([02-api.md](02-api.md)):

- **Cluster plane** — **one shared restic repository per `ClusterBackupLocation`** holding
  all (or selected) namespaces at `s3://<bucket>/<prefix>/<clusterID>/`. Tenancy inside the
  repo is carried by restic **tags** (`tenant`, `namespace`, `pvc`), **not** by
  per-namespace repositories. This buys cross-namespace deduplication and a single
  maintenance/discovery target; the costs (one cluster-wide prune window, physical-only
  erasure) and the deferred **sharding** alternative are recorded in
  [adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md). Sharding could later be added
  behind a `repositoryShardKey` **without changing the CRD surface**.
- **Namespace plane** — **one repository per user `BackupLocation`**, in the user's own
  bucket with the user's own credentials and key (R5). Isolation is by construction.

`clusterID` (`spec.clusterID`, e.g. `prod-eu-1`) is the restic **host** and a repo-path
segment (R20 — one bucket can serve several clusters). Engine format = **restic repo v2**
(zstd compression, content-defined dedup, AES-256 — R13); a user can always read their repo
with upstream `restic` (reversibility).

**Keys** ([adr/0004](adr/0004-encryption-key-management.md), simplified):

- Cluster repo: **one platform key** — a random DEK wrapped by a cluster KEK (age X25519),
  stored as `crystal-dek-*` Secrets in `crystal-backup-system` only. No per-namespace DEK
  and no multi-tier KEK hierarchy.
- User repo: the **user's own** restic password (a Secret in their namespace), or an
  operator-generated password stored **in the user's namespace**; `platformAccess` (default
  false) optionally adds an operator key slot for mediated restore/verification.

**Snapshot layout** — identical for both planes (one mover/restore/CLI codebase):

| restic field | Value |
|---|---|
| **host** | `<clusterID>` |
| **paths** | `/data/<namespace>/<pvc>` (data) · `/manifests/<namespace>` (manifests) |
| **tags** | `crystalbackup`, `tenant=`, `namespace=`, `pvc=`, `kind=data\|manifests`, `schedule=`, `run=` |

Per-PVC retention groups by `--group-by host,paths`; tags drive filtering, discovery,
correlation and right-to-erasure. Full table in
[02-api.md § Repository layout](02-api.md#repository-layout--snapshot-identity).

## 3. The cascade (CronJob → Job → Pod)

```
ClusterBackupSchedule ─cron+template─▶ ClusterBackup ─fan-out─▶ Backup (per ns) ─per PVC─▶ movers
BackupSchedule (ns)   ─cron──────────────────────────────────▶ Backup (same ns) ─per PVC─▶ movers
```

- **`ClusterBackupSchedule`** (cluster, cron) stamps out **`ClusterBackup`** runs from a
  `template`, bounded by `successful`/`failedRunsHistoryLimit`.
- **`ClusterBackup`** (a run) resolves `spec.namespaces` and creates a **`Backup`** in each
  matching namespace, linked by label `crystalbackup.io/cluster-backup=<run>` (a label, not
  an ownerReference — pruning run history never deletes restorable backups). It **also captures
  cluster-scoped resources** (a `kind=cluster-manifests` snapshot) for full-cluster DR —
  [adr/0011](adr/0011-cluster-scoped-dr.md).
- **`BackupSchedule`** (namespaced, cron) stamps out `Backup` directly in its namespace (no
  fan-out).
- **`Backup` (namespaced) is the single unit of execution** — driven identically whichever
  plane created it — and drives the per-PVC mover Jobs. It is **also the projection of a
  restorable backup** (§4). Consequences ([02-api.md](02-api.md)): a `Restore` only ever
  names a `Backup` in its own namespace (so **no `locationRef` on `Restore`**);
  `ClusterBackup.status` keeps only aggregate counters + a **capped** failures list (no
  unbounded `perNamespace`, no 1 MiB etcd risk); tenant visibility is native RBAC on
  namespaced `Backup` objects.

## 4. Discovery — the repository is the source of truth

`Backup` CRs are a **materialized view** of restic snapshots, not the source of truth; the
repository survives total cluster loss. A **discovery controller** (per `BackupRepository`,
on location add and every `discovery.interval`):

1. `restic snapshots --json --tag crystalbackup` → group by `(namespace, run)`.
2. For each group whose **namespace exists**, ensure a `Backup` named `run` projects into it
   (`status.volumes` derived from the snapshots). Namespaces that do not exist are skipped
   (still reachable via `ClusterRestore`, which reads the repo directly).
3. Remove projections whose snapshots are gone (post-`forget`). **CR lifetime = data
   lifetime**, so `kubectl get backups -n X` lists exactly what is restorable in X.

**DR bootstrap**: creating a `[Cluster]BackupLocation` pointed at an existing bucket is
sufficient — the operator inventories the repo and lists what is restorable, with no prior
CRs. `ClusterRestore` therefore targets a **repo coordinate** (`location + namespace +
run/time`), works even when the namespace is gone, and can create it.

## 5. Backup flow

For one `Backup` (R11, R12, R15, R16):

1. **Resolve** location + `BackupRepository`; **init** the restic repo on first use
   (serialized through the per-repo exclusive queue to avoid an init race —
   [adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md)); list target PVCs.
2. **Hooks (pre)** (R16): exec into selected pods (Velero-style, `onError` policy).
3. **Snapshot**: create one `VolumeSnapshot` per PVC in the origin namespace; wait
   `ReadyToUse` (crash-consistent point-in-time, R11).
4. **Hooks (post)**: run as soon as all snapshots are cut (hooks bound the freeze window,
   not the upload).
5. **Expose & move** ([adr/0003](adr/0003-snapshot-exposure-csi-generic-first.md)): per PVC,
   the operator selects the **least-data-movement exposer** for the PVC's CSI —
   `cephfs-shallow` (ROX `backingSnapshot`, zero copy) for CephFS, `csi-generic` (re-bind the
   `VolumeSnapshotContent` into `crystal-backup-system` as a static VS/VSC pair + a temporary
   COW PVC) for RBD and other snapshot-capable CSIs, `rook-rbd-direct` only if opted in. A PVC
   whose CSI **cannot snapshot** is **skipped** (`status.volumes[].phase: Skipped`,
   `reason: CSISnapshotUnsupported`, Event + log), never silently dropped. It then runs a
   **`crystal-mover` Job** mounting the exposed volume `readOnly`; it runs `restic backup` with
   the §2 layout tags. RWO and RWX PVCs follow the same path. Movers are unprivileged
   (`cephfs-shallow`/`csi-generic`) and spread across nodes (`topologySpreadConstraints`) to
   aggregate bandwidth (R12), under global and per-node concurrency limits.
6. **Manifests** (R15): a **`crystal-manifest-mover` Job** (ServiceAccount
   `crystal-manifest-mover`, transiently bound to ClusterRole `crystal-manifest-reader` —
   [03-security-and-tenancy.md](03-security-and-tenancy.md)) dumps the namespace's
   resources, sanitizes them ([04-manifest-backup.md](04-manifest-backup.md)) and uploads
   them as a `kind=manifests` snapshot (`/manifests/<namespace>`).
7. **Cleanup**: delete the temp PVC + static VS/VSC + origin `VolumeSnapshot`; write per-PVC
   status into the `Backup`; **enqueue `forget`** with the R24 retention on the per-repo
   maintenance queue (never inline at backup completion).
8. **Failure handling**: per-PVC status (`PartiallyFailed` phase), Job `backoffLimit`, and
   an **orphan reaper** controller that garbage-collects leftover temp PVCs/VS/VSCs and
   stale repo locks (age-based, `restic unlock`).

## 6. Restore flow (R6, R7, R14)

Restore is **generic**: a `crystal-mover` Job mounts the target PVC read-write and runs
`restic restore` — no Ceph dependency (R4/R6). Both restore kinds share two orthogonal axes,
**mode** and **selection**, specified in
[02-api.md § Restore selection model](02-api.md#restore-selection-model).

- **`Restore`** (namespaced, user): names a **`Backup` in its own namespace** — no
  `locationRef`, no target-namespace field — so it is structurally confined to that
  namespace (R14). If that `Backup` is `crystalbackup.io/origin: cluster`, the operator
  mediates against the shared DR repo with a **non-forgeable server-side tag filter
  `namespace=<this namespace>`** (the R2/R14 cornerstone).
- **`ClusterRestore`** (cluster, admin): addresses a **repo coordinate** (location + origin
  namespace + run/time), can **create** the target namespace, and maps storageClasses.
- **mode**: `Recreate` (delete+replace; data via `restic restore --overwrite always
  --delete`) or `Overwrite` (server-side apply keeping extras; data via `--overwrite always`
  **without** `--delete` → overwrite present files, restore missing, keep extras).
- **selection**: NetworkPolicy-style lists — `resources: [{selector, include, exclude}]` and
  `volumes: [{names, selector, include, exclude, targetPath}]` (OR between items, AND within
  one; both omitted ⇒ whole namespace; `include` on a volume gives partial-PVC restore, R7).
- Operations that modify pre-existing objects require `spec.confirmation == <target
  namespace>` (R23). Manifest restore applies the sanitization-inverse transforms
  (storageClass mapping, R15) and an apply order, running under ClusterRole
  `crystal-manifest-writer` — see [04-manifest-backup.md](04-manifest-backup.md).

## 7. Maintenance, retention & verification (R17, R24)

Every exclusive repository operation runs on a **per-`BackupRepository` exclusive queue**,
serialized against running backups (restic locking) — never inline:

- **`forget`** (schedule retention, R24): enqueued by each successful backup; applied
  per-PVC (`--group-by host,paths`).
- **`prune`**: per-location `maintenance.pruneSchedule`, jittered to avoid S3 bursts.
  Because the cluster plane is **one shared repo**, its prune is a **single cluster-wide
  exclusive window** (long and memory-heavy on large clusters → schedule off-peak,
  `--max-repack-size`; [adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md)). Immutable
  locations never prune ([adr/0005](adr/0005-immutability-mode.md)).
- **`check`** (R17) — repository integrity: `restic check` (structure) plus scheduled
  `check --read-data-subset` (reads a sample of pack data to catch silent bucket / bit-rot
  corruption), per location schedule; result surfaced in `BackupRepository.status`, metrics and
  the `RepositoryCheckFailed` alert. **Restore-testing is the administrator's responsibility**
  (restore drills via the normal restore path); the operator does **not** canary-restore every
  backup. (Offloadable cross-cluster verification and a repository-side verified-state index
  were considered and deferred — [00-requirements.md §6](00-requirements.md).)
- **Right-to-erasure** (`ClusterErasure`): `restic forget --tag tenant= | namespace= | namespace=+pvc=`
  then `prune` — **physical** deletion, on the same exclusive queue. Per-tenant
  crypto-shredding is impossible in a shared repo and is dropped
  ([adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md)); whole-repo key destruction
  remains only as a repository decommission tool. On Immutable locations erasure is
  **blocked** until object-lock expiry ([adr/0005](adr/0005-immutability-mode.md)).

## 8. Concurrency model (R12)

- Operator-level: `MaxConcurrentReconciles` per controller; a cluster-wide semaphore bounds
  simultaneous mover Jobs (`maxConcurrentMovers`) with a per-node bound via
  `topologySpreadConstraints` + anti-affinity.
- Per-repository: restic locking permits concurrent backup sessions; `prune`, `check`,
  `forget`, init and erasure require exclusivity — serialized by the per-`BackupRepository`
  queue. Many movers writing the one shared cluster repo increase lock-refresh/index churn →
  bounded by `maxConcurrentMovers`
  ([adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md)).
- Namespace-plane repositories are independent (distinct buckets), so they back up, prune
  and check in parallel with each other and with the cluster repo.

## 9. Coexistence with other backup tools (R22)

Distinct API group (`crystalbackup.io`), distinct namespace (`crystal-backup-system`),
distinct VolumeSnapshot objects (`crystal-` prefix + labels): no interference with an
incumbent backup tool such as Velero. Both may snapshot the same PVCs at different times
(ceph-csi handles concurrent snapshots; per-image snapshot-count thresholds are monitored).
The cluster plane provides full platform DR on its own, so Crystal Backup **can** be a
cluster's DR mechanism — but the goal is **coexistence, not replacement**: it never modifies
or requires removing the other tool. See [adr/0006](adr/0006-coexistence-with-backup-tools.md).

## 10. Key security properties (summary — details in 03-security-and-tenancy.md)

- User namespaces never host backup pods, mover credentials, or platform secrets; movers run
  only in `crystal-backup-system`, unprivileged (`csi-generic` / `cephfs-shallow` exposure).
- The **shared DR repo key never leaves `crystal-backup-system`**; users reach cluster-origin
  data only through a `Restore` on a cluster-origin `Backup` in their own namespace, mediated
  by the non-forgeable `namespace=` tag filter (R2/R14 invariant).
- Cluster-origin `Backup` objects are **read-only** to users (admission); a user-created
  `Backup`/`BackupSchedule` must reference a namespaced `BackupLocation`, never a
  `ClusterBackupLocation`.
- S3 credentials for a location live only in that location's namespace
  (`crystal-backup-system` for the cluster plane, the user's namespace for the namespace
  plane); movers receive short-lived projected Secrets scoped to the single repo they operate
  on.
- All backup/restore metrics carry the origin `namespace` label (R19). NetworkPolicies
  restrict movers to S3 endpoints (+ mons/OSDs for the **opt-in** `rook-rbd-direct` exposer,
  which confines privileged pods to `crystal-backup-system`, PSA `privileged` on that namespace
  only; `csi-generic` and `cephfs-shallow` stay unprivileged).

## 11. External backup synchronization (R28)

A **secondary** copy of backups in another location — a bonus resilience layer beyond the
primary repository. Two CRDs, one per plane ([02-api.md](02-api.md),
[adr/0013](adr/0013-external-backup-sync.md)):

- **`ClusterBackupExternalSync`** (admin) replicates the shared repo — whole repo by default,
  optional `selection.namespaces` — to a **secondary `ClusterBackupLocation`** (its own DEK).
- **`BackupExternalSync`** (user) replicates the namespace's backups between **two
  `BackupLocation`s in the same namespace** (each its own user key).

Mechanism — **`restic copy`, not a byte clone**:

1. The external-sync controller schedules a run (cron) and enqueues a **`crystal-mover` Job**
   in `crystal-backup-system` that runs `restic copy --from-repo <source> <destination>`,
   filtered by tag for the selected namespaces (whole repo if unfiltered).
2. `restic copy` **decrypts from the source and re-encrypts to the destination's own key**, so
   the destination is an **independent repository** (its own key), not a clone. This is what
   makes the copy **namespace-selective** and keeps a client's secondary under the **client's**
   key — the platform key never travels into a tenant silo (siloing invariant I9,
   [03-security-and-tenancy.md §3](03-security-and-tenancy.md)).
3. The run takes a **shared** lock on the source and writes the destination under a
   **non-exclusive** lock (only `Mirror`'s trailing `forget`+`prune` needs the destination's
   exclusive queue); it is **blob-incremental against a Standard destination** (only blobs absent
   at the destination are copied), so the first sync moves ≈ the selected data and later syncs
   move only the delta. It is **client-side** (no server-side object copy), the accepted cost of
   siloing + selectivity. (An **Immutable** destination rotates window-repos, so dedup resets per
   window — [adr/0005](adr/0005-immutability-mode.md); that combination is finalized with M8.)
4. `mode: Mirror` (default) reconciles the destination to the source's current snapshot set
   (copy missing runs, then `forget`+`prune` the extras on the destination queue);
   `AppendOnly` only ever adds and is **forced when the destination is `Immutable`** (Object
   Lock cannot delete → a WORM secondary, [adr/0005](adr/0005-immutability-mode.md)).

Because copied snapshots keep their `host`/`paths`/tags, **discovery** projects them at the
destination like any other snapshot; the secondary is a fully independent, restic-readable
repository (reversibility, R8) under its own key.
