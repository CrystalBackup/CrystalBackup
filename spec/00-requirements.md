# Crystal Backup — Requirements

Status: **agreed** — two-plane cascade rework, aligned with [02-api.md](02-api.md) v3 and
[adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md) (2026-07-12).

## 1. Context

A platform operates managed, multi-tenant Kubernetes clusters that are tenant-isolated by
namespace: a **namespace user** owns one or more namespaces and is self-service inside them
through RBAC. Storage is typically Rook Ceph (RBD primarily, CephFS possible); S3-compatible
object storage is available.

Such platforms commonly run a cluster-wide backup tool (e.g. Velero, daily, 10-day retention)
as an **admin-only** safety net. Namespace users then have no self-service backup or restore
capability, no visibility on their backups, and no way to back up outside the platform.

**Crystal Backup** is a Kubernetes operator providing **multi-tenant, self-service backup and
restore of namespaces (PVC data + Kubernetes manifests)** across **two planes** — a *cluster
plane* for platform **disaster recovery** and a *namespace plane* for users' own
**off-platform** backups — with storage isolation and encryption, a standalone CLI
(`crystalctl`), and (later) a browse UI. Cluster DR is a **core** capability; Crystal Backup is
designed to run **alongside** any existing backup tool (Velero being the common example)
without interference — coexistence, not replacement.

A state-of-the-art survey established that no existing open-source or commercial solution
covers these requirements; the closest are K8up+Backrest and Veeam Kasten, each with
structural gaps on the discriminating requirements (R2/R3/R5, R9, R11, R14).

## 2. Personas

- **Namespace user**: self-service inside their namespace(s). Creates schedules, restores
  their own backups, optionally registers their own external S3 location and encryption key.
  Must never be able to read or restore another user's data.
- **Platform administrator**: operates the system, owns cluster-level defaults and the shared
  DR repository, can restore anything anywhere (cross-namespace, cross-cluster), runs platform
  DR.

## 3. Core requirements (R1–R14)

These are the original requirements used in the state-of-the-art survey; codes are kept for
traceability throughout the specs and ADRs.

| # | Requirement |
|---|---|
| R1 | Kubernetes operator with CRD-driven scheduled backups (cron; e.g. daily), across a cluster plane and a namespace plane. |
| R2 | **Tenant isolation, two planes.** The cluster DR repository is a single **shared** restic repo, **admin-owned** — its key never leaves `crystal-backup-system`. Users never hold that key: DR access is **operator-mediated** through a `Restore` referencing a cluster-origin `Backup` in their own namespace, enforced by a **non-forgeable server-side tag filter `namespace=<the CR's namespace>`**. On the namespace plane, isolation is by construction (the user's own bucket, credentials and key). Tenant **visibility** is native Kubernetes RBAC on namespaced `Backup` objects — a user lists only their own. |
| R3 | **Two independent encryption tiers.** Cluster repo: one **platform key** — a random restic DEK wrapped by a cluster KEK (age X25519) held in `crystal-backup-system`. Namespace repo: the **user's key** — their restic password (their Secret), or an operator-generated one stored in the user's namespace; an optional operator key slot only if `platformAccess: true` (default `false`). Native restic encryption only, no custom crypto layer (preserves R8 reversibility + R13 dedup). No cluster→client→namespace KEK hierarchy. See [adr/0004](adr/0004-encryption-key-management.md). |
| R4 | Backup MAY be coupled/optimized for Rook Ceph; restore MUST be generic. |
| R5 | **Two-plane backup locations.** A cluster-scoped `ClusterBackupLocation` (platform storage + platform key) drives DR for all/selected namespaces into one shared repo; a namespaced `BackupLocation` lets a user register their **own** external S3 + **own** key for off-platform backups, **in addition to** cluster DR. |
| R6 | Restore into any Kubernetes cluster, with or without Rook Ceph. |
| R7 | Partial restore: a subset of files/directories of a PVC. |
| R8 | CLI export to a local tar without any Kubernetes dependency (S3 credentials + key only). |
| R9 | Web UI: list backups of a location, browse a backup's file tree, download individual files/directories without retrieving everything. |
| R10 | Preserve file metadata: uid, gid, mode, xattrs, ACLs, hardlinks, sparse files. |
| R11 | Back up from a read-only PVC snapshot, **minimizing data movement**. On Rook Ceph, use the least-copy path (RBD thin snapshot, CephFS shallow `backingSnapshot` ROX — no full clone); other CSIs fall back to a generic snapshot→temporary-PVC path **iff** the CSI supports snapshots. A PVC on a CSI that cannot snapshot is **skipped**, with the reason surfaced in status (never a silent drop). Consistency and independence from live mounts. See [adr/0003](adr/0003-snapshot-exposure-csi-generic-first.md). |
| R12 | Parallelize backups of multiple PVCs as jobs spread across several nodes to aggregate bandwidth. |
| R13 | Backup storage is compressed, deduplicated and encrypted. |
| R14 | **Restore multi-tenancy through the cascade.** A **namespaced** `Restore` consumes a `Backup` **in its own namespace** (no target-namespace field, no `locationRef`) → structurally confined; a cluster-origin `Backup` is served via the operator-mediated tag filter. A **cluster-scoped** `ClusterRestore` (admin) targets a **repo coordinate** (location + origin namespace + run/time), works even when the namespace no longer exists, and can create it; it can also **selectively restore cluster-scoped resources** captured by `ClusterBackup` (opt-in — [adr/0011](adr/0011-cluster-scoped-dr.md)). |

## 4. Extended requirements (R15–R28)

R15–R24 agreed 2026-07-11; R25–R26 added with the cascade / two-plane rework 2026-07-12; R28
(external backup sync) added 2026-07-15. R27 (preferred backup window) was retired to the
backlog (§6) and its number is not reused.

| # | Requirement |
|---|---|
| R15 | **Manifest backup with sanitization.** Back up the namespace's Kubernetes manifests alongside PVC data. Manifests are cleaned for cross-cluster restore: strip `metadata.uid`, `resourceVersion`, `creationTimestamp`, `status`, `managedFields` (à la `kubectl-neat`); strip Service `spec.clusterIP(s)` but **preserve `nodePort` numbers**; support storageClass mapping at restore time. See [04-manifest-backup.md](04-manifest-backup.md). |
| R16 | **Application-consistency hooks.** Optional pre/post-backup exec hooks in the backed-up namespace's pods (e.g. database flush/fsfreeze). Snapshots are crash-consistent by default. |
| R17 | **Automated repository integrity verification.** Scheduled integrity checks (`restic check`, including `--read-data-subset` to catch silent bucket / bit-rot corruption); results surface in `BackupRepository.status` and metrics. **Restore validation is the administrator's responsibility** — restore drills via the normal restore path — because the tool cannot canary-restore every backup daily; the ethos "a backup you never restore is not a backup" is upheld operationally, not by an automated per-backup canary. (Offloadable cross-cluster verification with a repository-side verified-state index was considered and **deferred** — see §6.) See [01-architecture.md](01-architecture.md). |
| R18 | **Immutability as a location mode.** A backup location is either `Standard` (dedup + prune) or `Immutable` (append-only / S3 Object Lock; no prune; retention via repository rotation). Mutually exclusive, chosen on the location CR. Feasibility is a v1 **design** constraint; implementation may land in a later milestone. See [adr/0005](adr/0005-immutability-mode.md). |
| R19 | **Per-tenant metrics, accounting-ready.** All backup/restore metrics (prefix `crystalbackup_`) carry the origin `namespace` label (and a `cluster` label whose value is the `clusterID`), enabling tenant dashboards and per-tenant alerting. The tool does **no accounting/billing itself**, but the metrics MUST expose the raw data that makes it possible downstream: backup counts, **logical bytes** protected, **deduplicated bytes actually stored** (per PVC/namespace/tenant — best-effort, as restic attributes dedup at the repository level — plus an exact per-repo total), and durations. See [05-observability.md](05-observability.md). |
| R20 | **Multi-cluster layout.** The snapshot **host is the `clusterID`** and the repo path carries it (`<prefix>/<clusterID>/`), so one shared repository can serve several clusters; per-PVC retention groups by `host,paths`. Cluster-level default location, with per-namespace locations on the namespace plane (R5). |
| R21 | **Right to erasure.** `ClusterErasure` performs **physical** deletion in the shared repo: `restic forget` filtered by tag (`tenant` / `namespace` / `namespace`+`pvc`) then `prune`. Per-tenant **crypto-shredding is dropped** — impossible in a single-key shared repo; whole-repo key destruction remains only as a repository **decommission** tool. On `Immutable` locations, erasure is blocked until object-lock expiry. See [adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md), [adr/0004](adr/0004-encryption-key-management.md). |
| R22 | **Coexistence with third-party backup tools.** Installs and runs **alongside** any existing backup tooling (Velero, K8up, … — Velero is the common case) without interference: distinct API group, namespaces, credentials, repositories and snapshot objects. **Cluster disaster recovery** is a **core** capability: a `ClusterBackup` captures **cluster-scoped resources** alongside one `Backup` per namespace, and a `ClusterRestore` can restore them **selectively** (opt-in — the admin chooses what to restore, typically not `kube-system` or GitOps-managed stacks). So Crystal Backup *can* be a cluster's DR mechanism — but the design goal is **coexistence, not replacement**: adopting it never requires removing another tool. See [adr/0006](adr/0006-coexistence-with-backup-tools.md) and [adr/0011](adr/0011-cluster-scoped-dr.md). |
| R23 | **Destructive-restore confirmation.** Any operation that modifies pre-existing objects — `Restore`/`ClusterRestore` in mode `Recreate`, or `Overwrite` into an existing namespace/PVC, and every `ClusterErasure` — requires a typed `confirmation` field equal to the exact target (target namespace name for restores; target identity for erasure). A static field-equality check, enforced in-API-server by a **ValidatingAdmissionPolicy** (CEL), not the operator webhook. See [adr/0010](adr/0010-admission-vap-first.md). |
| R24 | **Retention policy per schedule.** Retention is configurable on the backup schedule CRD with restic granularity: `keepLast`, `keepHourly`, `keepDaily`, `keepWeekly`, `keepMonthly`, `keepYearly` (+ optional `keepWithinDuration`), applied per-PVC. No storage quotas in v1. |
| R25 | **Cascade & unified execution.** CronJob-style cascade: `ClusterBackupSchedule` → `ClusterBackup` (a run) → `Backup` (per namespace) → mover Jobs; and `BackupSchedule` → `Backup`. **`Backup` (namespaced) is the single unit of execution** for both planes, and a `Restore` always consumes a namespaced `Backup`. Per-namespace status lives in the child `Backup`; `ClusterBackup` keeps only aggregate counters + a capped failures list (no unbounded map → no etcd bloat). See [02-api.md](02-api.md), [adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md). |
| R26 | **Repository as source of truth + discovery.** `Backup` CRs are a **projection** of the restic repository, not the source of truth. Adding a `[Cluster]BackupLocation` triggers a discovery controller that inventories the repo and projects `Backup` objects into **existing** namespaces (skipping non-existent ones); DR restore works with **no pre-existing CRs or namespaces**. CR lifetime = data lifetime (`kubectl get backups` lists exactly what is restorable). See [02-api.md](02-api.md), [adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md). |
| R28 | **External backup synchronization.** A `ClusterBackupExternalSync` (cluster) copies the shared repo — whole repo by default, optional namespace filter — to a **secondary** `ClusterBackupLocation`; a `BackupExternalSync` (namespaced) copies the namespace's backups to a **secondary** `BackupLocation` resolved within the same namespace. Replication is **`restic copy`** (snapshot-level), **re-encrypting to the destination repository's own key** — never a raw byte clone — so the destination is an independent repo with its **own key** and can be **namespace-selective**; tenant siloing is preserved (a client's copy is under the client's key, distinct from the platform key). Incremental at blob level; client-side. See [adr/0013](adr/0013-external-backup-sync.md). |

## 5. Non-functional requirements

- **Language/runtime**: Go operator (see [adr/0002](adr/0002-operator-language-go.md)); engine
  = restic format (see [adr/0001](adr/0001-repository-engine-restic-format.md)).
- **Cloud native observability**: Prometheus metrics endpoint (`crystalbackup_` prefix),
  JSON-lines logs on stdout, OpenTelemetry traces activated by standard `OTEL_*` env vars. See
  [05-observability.md](05-observability.md).
- **Testing**: unit tests, integration tests (envtest), end-to-end tests (kind + CSI snapshot
  support + an S3 test backend — SeaweedFS for the fast loop, Ceph RGW for Object-Lock),
  metadata-fidelity test suite (xattrs/ACLs/hardlinks/sparse). See
  [08-testing-and-dod.md](08-testing-and-dod.md).
- **Security**: tenant isolation is enforced **server-side** by the operator (repository
  identity and the `namespace=` tag filter derived from the CR's namespace, never
  user-declared). Privileged pods, if any, are confined to `crystal-backup-system`. See
  [03-security-and-tenancy.md](03-security-and-tenancy.md).
- **Supply chain / images**: operator and mover images are built with **apko** on a **Wolfi (glibc)** base for a **0-known-CVE** posture (not Alpine/musl), carry an **SBOM**, are **cosign-signed** and **digest-pinned**, and ship with **SLSA L3+** build provenance from GitHub Actions; a CI CVE-scan gate blocks release. See [adr/0012](adr/0012-container-images-apko-wolfi-slsa.md).
- **Open source**: documentation and specs in English; the project is open-source under a
  permissive license, and all dependencies must be permissive-license compatible
  (Apache-2.0/MIT/BSD).
- **Reversibility**: a namespace user must always be able to read their backups with standard
  upstream tooling (`restic`), given their S3 credentials and key.

## 6. Out of scope (v1)

- Storage quotas / billing of backup storage.
- Application-aware backups beyond exec hooks (no database-specific agents; CloudNativePG has
  its own barman-based backups).
- Cross-cluster **self-service** restore (namespaced `Restore` is same-cluster, same-namespace;
  cross-cluster restore is admin-only via `ClusterRestore` in v1 — a delegation mechanism may
  come later, see [03-security-and-tenancy.md](03-security-and-tenancy.md)).
- Kubernetes-cluster-state backup (etcd); platform DR covers resources + data, not the control
  plane itself.
- **Per-tenant / per-namespace repository sharding** (v1 = one shared repo per
  `[Cluster]BackupLocation`; deferred, see [adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md)).
- **Per-tenant crypto-shredding** (dropped — impossible in a shared single-key repo; superseded
  by the right to erasure, R21).
- **Namespace-plane backup as a partial repo copy** (`restic copy` mirroring selected snapshots
  from the cluster DR repo into the user's off-platform bucket, instead of an independent
  re-backup) — a **future** efficiency option that could supersede independent namespace-plane
  backups; feasibility/complexity TBD (`copy` re-uploads blobs and re-encrypts to the user's
  key). Distinct from **external sync** (R28, [adr/0013](adr/0013-external-backup-sync.md)),
  which is **same-plane**; this backlog item is the **cross-plane** variant (cluster repo →
  user bucket) and would reuse the same `restic copy` controller. See
  [adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md).
- **PVC shrink via backup/restore** (a low-priority future CRD that recreates a PVC at a smaller
  size through a backup→restore round-trip; requires the target PVC unmounted, so the operator
  never fights a controller re-creating it).
- **Node/zone affinity & tolerations on backup locations** (steer mover Jobs near the S3
  endpoint for bandwidth, IO cost or network segmentation) — a **future** option reusing core
  `NodeAffinity`/`Toleration` types; carries a governance question (whether namespace users may
  pin nodes). See [02-api.md](02-api.md).
- **Preferred backup window** (was R27; removed from v1 on 2026-07-15 after the first design
  proved the `start–end` shape ambiguous across midnight). If re-introduced, model it as
  **`start` + `duration`** — an unambiguous timestamp interval that naturally crosses midnight —
  rather than `start`/`end`, with `enforcement: Preferred|Required`, and ship it with a skip
  Event + metric and a controller `WindowUnsatisfiable` condition so a window no cron can ever
  satisfy is surfaced rather than silently skipped forever. Off-peak steering in v1 is achieved
  through the cron `schedule` itself.

## 7. Priorities (agreed)

1. Core two-plane backup/restore path: operator + cascade (`ClusterBackupSchedule` →
   `ClusterBackup` → `Backup`, and `BackupSchedule` → `Backup`), movers, self-service CRDs,
   manifests, hooks, verification, metrics, cluster DR + repository discovery.
2. Namespace-plane locations (user's own storage + own key), then **external sync** to a
   secondary location (R28) — a bonus resilience layer, scheduled after restore.
3. **CLI + UI: lower priority** (agreed). The reversibility promise (R8) is already met by the
   repository being standard restic (readable with upstream `restic`), so `crystalctl` is a
   convenience — the CLI and the local browse UI land **together in M7**; long-term UI =
   Rancher extension or Headlamp plugin. See [06-cli.md](06-cli.md), [07-ui.md](07-ui.md).
4. Immutable-mode implementation: later milestones.
