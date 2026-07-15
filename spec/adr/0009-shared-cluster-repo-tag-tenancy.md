# ADR 0009 — Shared cluster repository, tag-based tenancy, and the cascade / repository-as-source-of-truth model

Status: **Accepted** (2026-07-12)

## Context

Two design questions were settled together after the two-plane rework
([02-api.md](../02-api.md)):

1. **How is tenancy represented inside the platform DR backups?** One shared restic
   repository for the whole cluster with per-namespace **tags**, or **one repository per
   namespace/tenant** (sharding)?
2. **How are backup executions and their status modeled** so that: per-namespace status
   does not bloat a single object past etcd's ~1 MiB limit; namespace users see only their
   own backups; a restore cannot escape its namespace; and disaster recovery works when the
   cluster (and its CRs) are gone?

The requirements that shaped the answer:

- **DR-first.** Adding a `[Cluster]BackupLocation` must be enough for the operator to
  discover the backups already present in the object storage and make them restorable —
  including into namespaces that do not exist yet. The system must survive total cluster
  loss.
- **Tenant self-service visibility** without leaking other tenants' backups.
- **Cross-namespace deduplication** for platform DR (identical base images / data across
  namespaces stored once).
- **Right-to-erasure** (GDPR) at tenant / namespace / PVC granularity.

## Decision

### 1. One shared repository per `[Cluster]BackupLocation`, tenancy by restic tags

The cluster plane writes all namespaces into a **single restic repository** per
`ClusterBackupLocation`. A snapshot's tenancy is carried by its **path and tags**
(`host=<clusterID>`, `paths=/data/<namespace>/<pvc>`, `tags: tenant=…, namespace=…,
pvc=…, run=…`; see [02-api.md § Repository layout](../02-api.md#repository-layout--snapshot-identity)).
Right-to-erasure is `restic forget --tag …` + `prune`. Per-namespace / per-tenant repository
**sharding is deferred** (see Alternatives).

### 2. The cascade — `ClusterBackupSchedule` → `ClusterBackup` → `Backup` → movers

Modeled on `CronJob → Job → Pod`:

- **`ClusterBackupSchedule`** (cluster, cron) stamps out **`ClusterBackup`** runs from a
  `template` (like `CronJob.spec.jobTemplate`), with `successful/failedRunsHistoryLimit`.
- **`ClusterBackup`** (a run) resolves `spec.namespaces` and creates a **`Backup`** in each
  matching namespace, linked by the label `crystalbackup.io/cluster-backup=<run>`.
- **`Backup`** (namespaced) is the **single unit of execution** — driven identically whether
  it came from a `ClusterBackup` or from a namespace-plane `BackupSchedule` — and drives the
  per-PVC mover Jobs (in `crystal-backup-system`).
- **`BackupSchedule`** (namespaced, cron) stamps out `Backup` directly (single namespace, no
  fan-out).

Consequences of making `Backup` the convergence point:

- **A `Restore` only ever references a `Backup` in its own namespace** → structurally
  confined; it does not need to know whether the backup came from cluster DR or the
  namespace's own schedule, and it carries **no `locationRef`** (the `Backup` has it).
- **Per-namespace status lives in the per-namespace `Backup`.** `ClusterBackup.status`
  keeps only aggregate counters + a **capped** `failures` list — never an unbounded
  `perNamespace` map, so etcd's 1 MiB limit is never at risk regardless of namespace count.
- **Tenant visibility is native RBAC.** `Backup` objects are namespaced; a user
  `kubectl get backups` sees only their own. No custom authorization logic.

### 3. The repository is the source of truth; `Backup` CRs are a projection

`Backup` objects are a **materialized view of restic snapshots**, not the source of truth.

- **Discovery.** A controller per `BackupRepository` (on location add, then every
  `discovery.interval`) runs `restic snapshots`, groups by `(namespace, run)` via tags, and
  ensures a `Backup` named `run` exists in each **existing** namespace. Non-existent
  namespaces are skipped (still restorable via `ClusterRestore`, which reads the repo
  directly). Projections whose snapshots were `forget`-ten are removed.
- **CR lifetime = data lifetime.** A `Backup` exists for exactly as long as its snapshots
  are retained, so `kubectl get backups -n X` lists **exactly** what is restorable in X.
  This is decoupled from `ClusterBackup` run records, which are history-limited like Jobs
  and linked to their `Backup` children by **label, not a cascade `ownerReference`** (so
  pruning old run records never deletes restorable backups).
- **DR bootstrap.** On a fresh cluster, creating a `ClusterBackupLocation` pointing at an
  existing bucket is sufficient: the operator inventories the repo and an admin can
  `ClusterRestore` any namespace (creating it). `ClusterRestore` therefore targets a **repo
  coordinate** (`location + namespace + run/time`), not an in-cluster object.

### 4. Debuggability without etcd bloat — three tiers

| Tier | Where | Content |
|---|---|---|
| Aggregate | `ClusterBackup.status` | counters + **capped** failure list |
| Per-namespace | `Backup.status` (bounded, one namespace) | per-PVC status, snapshot IDs, short messages |
| Unbounded detail | **metrics + JSON logs + Events** | per-PVC / per-file errors, mover logs — outside etcd, labeled `namespace=` |

## Consequences

### Positive

- **Cross-namespace dedup** across the whole cluster DR (a major storage win over
  per-namespace repos).
- **One repo to operate** per location: one init, one maintenance target, one discovery
  scan — simpler than N per-namespace repos.
- **etcd-safe at scale**: bounded objects; `Backup` count ≈ Σ(namespaces × retained runs)
  of small objects (~3000 for 100 ns × ~30 points), well within etcd limits.
- **Tenant isolation and visibility fall out of native Kubernetes** (namespaced `Backup` +
  RBAC + server-side tag filter on mediated restore).
- **True DR**: repo is authoritative; the cluster and all CRs can be lost and rebuilt.
- **Uniform restore** path regardless of backup origin.

### Negative / costs

- **One cluster-wide exclusive `prune`/`check` window** for the shared repo (restic's
  prune takes the exclusive lock). On a large cluster this is a long, memory-heavy
  operation → mitigations: schedule off-peak, `--max-repack-size`, `--pack-size` tuning,
  dedicated resources (lessons from upstream backup-operator GitHub issues).
- **No per-tenant crypto-shredding**: a shared repo has one master key; erasure must be
  physical (`forget`+`prune`), which is impossible on an Immutable location until
  object-lock expiry ([adr/0005](0005-immutability-mode.md)).
- **Prune memory scales with total cluster data**, not per-namespace.
- **Anyone with the repo key reads all namespaces** → the key is admin-only; tenant access
  is always operator-mediated with a non-forgeable `namespace=` tag filter.
- **Concurrent writers**: many movers write the one repo at once (restic supports
  concurrent backup sessions, but lock-refresh/index churn increases) — bounded by
  `maxConcurrentMovers` and per-repo serialized maintenance.

### Risks & mitigations

| Risk | Mitigation |
|---|---|
| Repo init race (N movers `restic init` the empty shared repo) | Init serialized through the per-repo exclusive queue before any mover runs (cf. K8up #1055) |
| Discovery misgroups snapshots | `run` tag is the authoritative grouping key; discovery is idempotent and reconciles |
| Tenant tampers with a cluster-origin `Backup` | Admission: `origin=cluster` Backups are read-only to users; repo (not CR) is source of truth, so discovery re-creates a deleted projection |
| Shared-repo prune starves backups | Serialized per-repo queue with a bounded prune window; backups wait, never corrupt |

## Alternatives considered

### A. One repository per namespace / per tenant (sharding) — **deferred, not rejected**

Reasons one might still want it (to revisit — the product owner explicitly flagged this on
2026-07-12):

- **Smaller blast radius**: repo corruption or a bad prune affects one tenant, not all.
- **Per-tenant `prune`/`check` isolation**: no single cluster-wide exclusive window;
  maintenance parallelizes across repos; prune memory bounded per tenant.
- **Per-tenant keys → per-tenant crypto-shredding** becomes possible again (destroy one
  tenant's DEK) as an alternative/complement to `forget`+`prune`, and works even on
  Immutable locations.
- **Independent retention / lifecycle / even object storage** per tenant.
- **Bounded RAM** for index operations per repo.

Why deferred for v1:

- **Loses cross-namespace dedup** (the main DR storage win), unless a shared chunk store is
  added (restic has none).
- **N repos to init, maintain, discover, lock** — more moving parts, more failure surface.
- **Discovery and the "one shared bucket" DR story are simpler** with a single repo.

Decision: **v1 = one shared repo per `[Cluster]BackupLocation`.** The tag layout, discovery,
and `BackupRepository` abstraction are designed so a `repositoryShardKey` (e.g. per
namespace or per tenant) could be introduced later **without changing the CRD surface** —
only the repo-path derivation and the maintenance/discovery fan-out change.

Revisit triggers: shared-repo prune windows become operationally painful at cluster scale;
a regulator/customer requires per-tenant crypto-shredding or per-tenant key custody; index
RAM on the shared repo exceeds practical limits; per-tenant blast-radius isolation becomes
a contractual requirement.

### B. One giant `ClusterBackup` with a `perNamespace` status map

Rejected: unbounded status → 1 MiB etcd limit at ~hundreds of namespaces; also forces
custom authorization for tenant visibility. The cascade replaces it with per-namespace
`Backup` objects (idiomatic `Job → Pods`).

### C. CRs as the source of truth (operator writes canonical state only in etcd)

Rejected: cannot survive cluster loss — the antithesis of DR. Repo-as-truth + discovery is
mandatory.

### D. `Restore` names a repo coordinate directly (like `ClusterRestore`)

Rejected for the namespace plane: naming a `Backup` in the namespace is what makes restore
structurally unable to leave the namespace and removes `locationRef`. `ClusterRestore`
(admin) keeps the repo-coordinate form because it must address gone namespaces.

## Revisit triggers

- Sharding triggers listed under Alternative A.
- restic gains a shared-chunk-store across repos (would make sharding dedup-neutral).
- Discovery cost on very large repos (millions of snapshots) forces an incremental /
  cached inventory instead of full `restic snapshots` scans.
- **Namespace-plane backups via `restic copy` instead of an independent re-backup** becomes
  attractive: rather than re-reading and re-encrypting a namespace's data a second time for the
  user's off-platform bucket, mirror the already-made snapshots from the shared cluster repo
  with `restic copy` (which re-uploads blobs and re-encrypts to the destination key). This
  would make the namespace plane a **selective projection** of the cluster repo rather than a
  second full backup — potentially far more efficient. Deferred; feasibility/complexity and the
  key-custody implications are TBD ([00-requirements.md §6](../00-requirements.md)).
