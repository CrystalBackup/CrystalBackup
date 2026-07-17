# API — Custom Resource Definitions

Status: draft v3 (cascade model + repository-as-source-of-truth, 2026-07-12).
API group: **`crystalbackup.io`**, version **`v1alpha1`**.

This file is the **naming and field contract** for the whole project. Other specs and
ADRs must use these exact kind, field and label names. The architectural rationale for the
cascade and the shared-repository model lives in
[adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md).

## Two planes (cert-manager-style scoping)

Like cert-manager's `ClusterIssuer` vs `Issuer`, Crystal Backup separates a **cluster
plane** (platform administrators) from a **namespace plane** (namespace users):

- **Cluster plane** — platform **disaster recovery**. Admins back up **all (or selected)
  namespaces** into **one shared restic repository** per `ClusterBackupLocation`. Tenancy
  inside that repo is expressed with restic **tags** (`tenant`, `namespace`, `pvc`), not
  with separate repos (sharding deferred — see [§ Sharding](#repository-sharding-deferred)).
- **Namespace plane** — namespace users also back up **their own namespace** to **their
  own object storage**, with **their own key**, *in addition to* the cluster DR. Isolation
  is by construction: their bucket, their credentials, their key.

## The cascade (CronJob-style)

```
ClusterBackupSchedule ──cron+template──▶ ClusterBackup ──namespaces, fan-out──▶ Backup (per ns) ──per PVC──▶ mover Jobs
      (≈ CronJob)                        (a run; ≈ a Job                        (≈ Job)                       (≈ Pods)
                                          that creates Jobs)
BackupSchedule (namespaced) ──cron──▶ Backup (same ns) ──per PVC──▶ mover Jobs
```

**`Backup` (namespaced) is the single unit of execution**, whether it originates from a
namespace-plane `BackupSchedule` or a cluster-plane `ClusterBackup`. Therefore:

- A **`Restore` only ever consumes a `Backup` in its own namespace** → it is structurally
  confined to that namespace and does not need to know the backup's origin (hence **no
  `locationRef` on `Restore`** — the `Backup` already carries its location).
- **Per-namespace status lives in the per-namespace `Backup`**, so `ClusterBackup` keeps
  only aggregate counters + a capped failure list (no unbounded `perNamespace`, no 1 MiB
  etcd risk).
- **Tenant visibility is native**: `Backup` objects are namespaced, so standard RBAC lets
  a user `kubectl get backups` see only their own — never other tenants'.

## Repository is the source of truth

`Backup` CRs are a **projection** of the restic repository, not the source of truth. The
repository (snapshots + tags) survives total cluster loss. Adding a
`ClusterBackupLocation`/`BackupLocation` triggers a **discovery** controller that
inventories the repo and (re)projects `Backup` objects into existing namespaces (ignoring
namespaces that do not exist). See [§ Discovery](#discovery-repositorybackup-projection)
and [01-architecture.md](01-architecture.md).

## Overview

| Kind | Scope | Audience | Purpose |
|---|---|---|---|
| `ClusterBackupLocation` | Cluster | admin | Platform object storage + platform key + maintenance/verification. One shared repo. |
| `ClusterBackupSchedule` | Cluster | admin | Cron; stamps out `ClusterBackup` runs from a template. |
| `ClusterBackup` | Cluster | admin/operator | One DR run; fans out `Backup` into selected namespaces **and captures cluster-scoped resources** (adr/0011). Aggregate status. |
| `ClusterRestore` | Cluster | admin | Restore any namespace from a repo coordinate (works even if the namespace does not exist). |
| `ClusterErasure` | Cluster | admin | Right-to-erasure: `forget`+`prune` a tenant/namespace/PVC from a `ClusterBackupLocation`. |
| `ClusterBackupExternalSync` | Cluster | admin | Replicate the shared repo (whole or namespace-filtered) to a **secondary** `ClusterBackupLocation` via `restic copy`, re-keyed (R28, [adr/0013](adr/0013-external-backup-sync.md)). |
| `BackupLocation` | Namespaced | user | The user's **own** external object storage + the user's **own** key. |
| `BackupSchedule` | Namespaced | user | Cron; stamps out `Backup` in the namespace. |
| `Backup` | Namespaced | user (read) / operator | The unit of execution **and** the projection of a restorable backup. |
| `Restore` | Namespaced | user | Self-service restore of the user's own namespace, from a `Backup` in that namespace. |
| `BackupExternalSync` | Namespaced | user | Replicate the namespace's backups to a **secondary** `BackupLocation` in the same namespace via `restic copy`, re-keyed (R28, [adr/0013](adr/0013-external-backup-sync.md)). |
| `BackupRepository` | Cluster | operator (internal) | Operator-managed state + inventory of one restic repository. Not user-facing. |

## Design invariants

- **`Backup` is the execution unit and the projection.** Created by a run *and* by
  discovery; deleted only when its snapshots are `forget`-ten (CR lifetime = data lifetime,
  so `kubectl get backups -n X` lists exactly what is restorable in X).
- **Cluster DR repo is admin-owned.** Its key never leaves `crystal-backup-system`. Users
  reach DR data only through a `Restore` referencing a cluster-origin `Backup` in their own
  namespace; the operator mediates with a **server-side tag filter `namespace=<the CR's
  namespace>`** that users cannot forge — the R2/R14 cornerstone.
- **Cluster-origin `Backup` objects are read-only to users** (admission): users may
  get/list/watch them but not create/update/delete; a user-created `Backup`/`BackupSchedule`
  must reference a **namespaced `BackupLocation`**, never a `ClusterBackupLocation`.
- **Namespace-plane isolation is by construction**: a `BackupLocation` references Secrets in
  its own namespace; a `Restore` only reads its own namespace's `Backup` objects.
- Every CR exposes `status.conditions` (`metav1.Condition`) and a human `phase`;
  controllers reconcile on spec, never on phase.
- Controller-created objects carry labels: `crystalbackup.io/origin` (`cluster|namespace`),
  `crystalbackup.io/cluster-backup` (originating run, on cluster-origin Backups),
  `crystalbackup.io/schedule`, `crystalbackup.io/namespace` (origin namespace),
  `crystalbackup.io/tenant`.

---

## Cluster plane

### ClusterBackupLocation (cluster-scoped, admin)

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupLocation
metadata:
  name: dr-primary
spec:
  default: true                      # exactly one may be default
  mode: Standard                     # Standard (prunable) | Immutable (object-lock; no prune)
  clusterID: prod-eu-1               # snapshot host + repo path segment (R20, multi-cluster)
  s3:
    endpoint: https://s3.example.net
    bucket: platform-dr
    prefix: prod                     # single shared repo at <prefix>/<clusterID>/
    region: eu-west
    credentialsSecretRef: { name: dr-s3 }   # Secret in crystal-backup-system
    caBundle: ""
    forcePathStyle: true
  encryption:
    clusterKEKSecretRef: { name: cluster-kek }   # age identity wrapping the platform DEK
  discovery:
    enabled: true                    # inventory the repo + project Backups on add & periodically
    interval: 1h
  maintenance:                       # Standard mode only
    pruneSchedule: "0 3 * * *"       # one shared repo → one cluster-wide exclusive prune window
    pruneMaxRepackSize: "50G"
    checkSchedule: "0 5 * * 0"
    checkReadDataSubset: "5%"
  retention:                         # R24, restic granularity, applied per-PVC; Standard mode only
    keepLast: 7                      # ONE authoritative policy per location/repository (adr/0009):
    keepDaily: 14                    #   applied by a `restic forget` after each successful backup,
    keepMonthly: 12                  #   NOT per-run — several runs share the repo and must not fight
    keepYearly: 3                    #   over snapshots. Ignored on Immutable (RetentionIgnored cond).
  immutable:                         # Immutable mode only
    objectLockMode: Governance       # Governance | Compliance | AppendOnlyProxy
    rotationPeriod: 720h
status:
  conditions: [...]                  # Ready, Reachable
  repositoryRef: dr-primary
  namespacesProtected: 42
  lastDiscoveryTime: "2026-07-12T02:55:00Z"
```

### ClusterBackupSchedule (cluster-scoped, admin) — ≈ CronJob

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupSchedule
metadata:
  name: dr-daily
spec:
  schedule: "0 2 * * *"
  timezone: Europe/Paris
  paused: false
  jitter: true                       # deterministic per-namespace spread (anti thundering herd)
  concurrencyPolicy: Forbid          # Forbid | Skip
  startingDeadlineSeconds: 3600      # bound catch-up after downtime to one run
  successfulRunsHistoryLimit: 10     # ClusterBackup run records kept (≠ snapshot retention)
  failedRunsHistoryLimit: 10
  template:                          # ≈ jobTemplate: a ClusterBackup spec
    spec:
      locationRef: { name: dr-primary }
      namespaces:                    # see § Namespace selection
        regexp: "^c-.+$"
        exclude: ["kube-*", "crystal-backup-system"]
      pvcSelector: { matchLabels: {} }         # per namespace, default all
      includeManifests: true
      clusterResources:                        # R22 — cluster-scoped capture for full DR (adr/0011)
        enabled: true                          # default true on the cluster plane
        include: []                            # empty ⇒ curated default allowlist (CRDs, StorageClasses,
                                               #   IngressClasses, PriorityClasses, RuntimeClasses,
                                               #   ClusterRoles/Bindings excl. system:*, PersistentVolumes)
        exclude: []                            # denylist applied after include (default: system:* names)
        labelSelector: {}                      # optional extra filter
      hooks: { honorAnnotations: true }        # crystalbackup.io/pre-backup-* on pods (R16)
      # retention is NOT here: it lives on the ClusterBackupLocation — one shared repo, one policy.
      maxConcurrentMovers: 8
      backoffLimit: 2
status:
  lastRunName: dr-daily-20260712-020000
  lastSuccessTime: "2026-07-12T02:41:10Z"
  nextScheduleTime: "2026-07-13T02:00:00Z"
  conditions: [...]
```

### ClusterBackup (cluster-scoped) — a run; fans out `Backup`

Created by the schedule (name `<schedule>-<timestamp>`) or manually. Resolves
`spec.namespaces` and creates a `Backup` in each matching namespace, linked by label
`crystalbackup.io/cluster-backup`. **No `perNamespace` list**; per-namespace detail lives
in the child `Backup` objects; `status.failures` is capped.

```yaml
spec:
  scheduleRef: dr-daily              # empty for manual
  locationRef: { name: dr-primary }
  namespaces: { regexp: "^c-.+$", exclude: ["kube-*"] }
  # (pvcSelector, includeManifests, hooks, ... inherited from the template; retention lives on the location)
status:
  phase: PartiallyFailed             # Pending|Running|Completed|PartiallyFailed|Failed
  startTime: "2026-07-12T02:00:03Z"
  completionTime: "2026-07-12T02:41:10Z"
  namespacesMatched: 42
  namespacesSucceeded: 41
  namespacesFailed: 1
  pvcsSucceeded: 135
  pvcsFailed: 2
  clusterResourcesCaptured: 37       # cluster-scoped objects in the kind=cluster-manifests snapshot (adr/0011)
  addedBytes: 8123456789
  failures:                          # CAPPED list (default: first N failures)
    - namespace: c-team-x
      backup: dr-daily-20260712-020000
      message: "pvc data-2: mover OOMKilled, raise memory limit"
  conditions: [...]
```

### ClusterRestore (cluster-scoped, admin)

Targets a **repo coordinate** (location + origin namespace + run/time), so it works even
when the source namespace no longer exists and can create the target namespace. Uses the
shared [Restore selection model](#restore-selection-model).

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterRestore
metadata:
  name: recover-team-x
spec:
  source:
    locationRef: { name: dr-primary }
    namespace: c-team-x                        # origin namespace (repo tag filter)
    backup: dr-daily-20260711-020000           # a run name; OR time: latest | <RFC3339>
  target:
    namespace: c-team-x-restored
    createNamespace: true
    storageClassMapping: { fast-rbd: standard }
  mode: Recreate                               # Recreate | Overwrite
  # resources / volumes omitted → whole namespace
  clusterResources:                            # opt-in; omitted ⇒ NO cluster-scoped restore (adr/0011)
    include: ["apiextensions.k8s.io/CustomResourceDefinition", "storage.k8s.io/StorageClass/*"]
    exclude: []                                # admin-only; apply order CRDs→cluster-scoped→namespaced
  confirmation: "c-team-x-restored"            # required iff the op modifies existing objects
status:
  phase: Completed                             # Pending|AwaitingConfirmation|Running|Completed|PartiallyFailed|Failed
  restoredResources: 148
  restoredVolumes: 5
  restoredBytes: 10737418240
  conditions: [...]
```

### ClusterErasure (cluster-scoped, admin — right-to-erasure)

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterErasure
metadata: { name: gdpr-team-x }
spec:
  locationRef: { name: dr-primary }
  target:                             # exactly one
    tenant: team-x                    # OR namespace: c-team-x  OR { namespace: c-team-x, pvc: uploads }
  confirmation: "team-x"              # typed, must equal the target identity
status:
  phase: Completed                    # Pending|AwaitingConfirmation|Running|Completed|Blocked|Failed
  snapshotsForgotten: 92
  reclaimedBytes: 5368709120
  blockedUntil: ""                    # set on Immutable locations (object-lock expiry)
  conditions: [...]
```

On **Immutable** locations, erasure is `Blocked` until object-lock expiry (WORM vs erasure
tension — [adr/0005](adr/0005-immutability-mode.md)); Standard erases immediately.

### ClusterBackupExternalSync (cluster-scoped, admin) — secondary-location replication (R28)

Copies the shared repository to a **secondary** `ClusterBackupLocation` using `restic copy`
(snapshot-level, **re-encrypted to the destination's own platform DEK** — an independent repo,
not a byte clone). Whole repo by default; `selection.namespaces` narrows it. See
[adr/0013](adr/0013-external-backup-sync.md).

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupExternalSync
metadata: { name: dr-to-region-b }
spec:
  sourceLocationRef: { name: dr-primary }        # a ClusterBackupLocation (default: the default one)
  destinationLocationRef: { name: dr-secondary } # another ClusterBackupLocation, its OWN DEK
  schedule: "0 6 * * *"                           # cron; empty ⇒ on-demand only
  timezone: Europe/Paris
  paused: false
  mode: Mirror                                    # Mirror (track source, forget extras) | AppendOnly
                                                  #   forced AppendOnly if destination is Immutable
  selection:                                      # optional; omitted ⇒ whole repo
    namespaces: { regexp: "^c-.+$", exclude: ["kube-*"] }
status:
  phase: Completed                                # Pending|Running|Completed|PartiallyFailed|Failed
  lastSuccessTime: "2026-07-15T06:12:04Z"
  snapshotsCopied: 128
  bytesCopied: 4831838208                         # data streamed this run (blob-incremental)
  lagSnapshots: 0                                 # source snapshots not yet at the destination
  conditions: [...]
```

`destinationLocationRef` must be a **different** `ClusterBackupLocation` with its own key (admission
rule 9); the operator holds both keys transiently in `crystal-backup-system` (like any mover) and
never persists plaintext. The copy writes under a **non-exclusive** lock on the destination (like a
backup); only `Mirror`'s trailing `forget`+`prune` takes the destination's **exclusive** queue. An
**Immutable** destination is a rotating window-repo set ([adr/0005](adr/0005-immutability-mode.md))
→ `AppendOnly` is forced and dedup resets per rotation window (full handling with M8).

---

## Namespace plane

### BackupLocation (namespaced, user's own storage)

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: BackupLocation
metadata: { name: my-offsite, namespace: c-team-x }
spec:
  mode: Standard
  clusterID: prod-eu-1               # defaults from the default ClusterBackupLocation if unset
  s3:
    endpoint: https://s3.team-x.example
    bucket: team-x-backups
    prefix: crystal
    credentialsSecretRef: { name: offsite-s3 }         # Secret in c-team-x
  encryption:
    repositoryPasswordSecretRef: { name: offsite-key } # user-owned restic password (Secret in c-team-x)
    platformAccess: false            # true → operator also holds a key slot (mediated restore/verify)
  discovery: { enabled: true }       # project Backups from this repo into this namespace
  retention: { keepLast: 5, keepDaily: 10, keepWeekly: 4, keepMonthly: 6 }  # R24; on the location, not the schedule (Standard only)
status:
  conditions: [...]
  repositoryRef: c-team-x--my-offsite
```

If `repositoryPasswordSecretRef` is omitted the operator generates a password and stores it
as a Secret **in the user's namespace** (their key, their reversibility). `platformAccess:
false` (default) keeps off-platform backups private to the user.

### BackupSchedule (namespaced, user) — ≈ CronJob

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: BackupSchedule
metadata: { name: daily, namespace: c-team-x }
spec:
  locationRef: { name: my-offsite }  # a BackupLocation in this namespace (required)
  schedule: "0 1 * * *"
  timezone: Europe/Paris
  jitter: true
  concurrencyPolicy: Forbid
  startingDeadlineSeconds: 3600
  pvcSelector: { matchLabels: {}, include: [], exclude: [] }   # default all
  includeManifests: true
  hooks:                             # R16
    pre:
      - podSelector: { matchLabels: { app: postgres } }
        container: postgres
        command: ["psql", "-c", "CHECKPOINT"]
        timeout: 30s
        onError: Fail                # Fail | Continue
    post: []
  # retention is NOT here: it lives on the BackupLocation — one repo, one policy.
  backoffLimit: 2
status:
  lastRunName: daily-20260712-010000
  lastSuccessTime: "2026-07-12T01:04:12Z"
  nextScheduleTime: "2026-07-13T01:00:00Z"
  conditions: [...]
```

### Backup (namespaced) — execution unit **and** restore-point projection

```yaml
metadata:
  name: daily-20260712-010000
  namespace: c-team-x
  labels:
    crystalbackup.io/origin: namespace          # or "cluster" for DR-originated backups
    crystalbackup.io/schedule: daily
    crystalbackup.io/cluster-backup: ""         # set for origin=cluster
spec:
  scheduleRef: daily                 # empty for manual/ad-hoc
  locationRef: { name: my-offsite }  # or { kind: ClusterBackupLocation, name: dr-primary } (origin=cluster)
status:
  phase: Completed                   # Pending|SnapshottingHooks|Snapshotting|Uploading|Completed|PartiallyCompleted|PartiallyFailed|Failed
  backupTime: "2026-07-12T01:00:00Z"
  manifests: { snapshotID: "ab12…", resourceCount: 148 }
  volumes:
    - pvc: data-postgres-0
      snapshotID: "cd34…"
      sizeBytes: 10737418240
      addedBytes: 52428800
      phase: Completed                        # Pending|Snapshotting|Uploading|Completed|Skipped|Failed
      node: worker-3
    - pvc: scratch-nfs
      phase: Skipped                          # CSI cannot snapshot (adr/0003); manifests still captured
      reason: CSISnapshotUnsupported
  conditions: [...]
```

### Restore (namespaced, user — self-service)

Restores **only this namespace**, referencing a `Backup` **in this namespace** (no
`locationRef`, no target-namespace field). If that `Backup` is `origin=cluster`, the
operator mediates against the shared DR repo with the `namespace=<this namespace>` tag
filter. Uses the shared [Restore selection model](#restore-selection-model).

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: Restore
metadata: { name: recover-uploads, namespace: c-team-x }
spec:
  source:
    backup: dr-daily-20260711-020000   # a Backup in THIS namespace; OR time: latest [+ origin: cluster|namespace]
  mode: Overwrite
  resources: []                        # nothing (data only)
  volumes:
    - names: ["uploads"]
      include: ["images/2026/**"]      # a single folder of a single PVC
  confirmation: "c-team-x"             # required iff the op modifies existing objects
status:
  phase: Completed                     # Pending|AwaitingConfirmation|Running|Completed|PartiallyFailed|Failed
  restoredResources: 0
  restoredVolumes: 1
  restoredBytes: 734003200
  conditions: [...]
```

### BackupExternalSync (namespaced, user) — secondary-location replication (R28)

Copies the namespace's backups from one `BackupLocation` to **another `BackupLocation` in the
same namespace**, using `restic copy` (**re-encrypted to the destination's own user key** — an
independent repo, not a byte clone). Both refs are same-namespace (structural confinement, like
`Restore`); the platform key is never involved, so client siloing holds. See
[adr/0013](adr/0013-external-backup-sync.md).

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: BackupExternalSync
metadata: { name: offsite-mirror, namespace: c-team-x }
spec:
  sourceLocationRef: { name: my-offsite }        # a BackupLocation in THIS namespace (default: the default one)
  destinationLocationRef: { name: my-offsite-2 } # another BackupLocation in THIS namespace, its OWN key
  schedule: "0 4 * * *"
  timezone: Europe/Paris
  mode: Mirror                                    # Mirror | AppendOnly (forced on Immutable destination)
status:
  phase: Completed
  lastSuccessTime: "2026-07-15T04:07:41Z"
  snapshotsCopied: 12
  bytesCopied: 268435456
  lagSnapshots: 0
  conditions: [...]
```

A `BackupExternalSync` cannot name a `ClusterBackupLocation` or a location in another namespace
(admission rule 2); it moves only the user's own snapshots between the user's own repositories,
each under the user's own key.

---

## Internal

### BackupRepository (cluster-scoped, operator-internal)

One per repository (per `ClusterBackupLocation`, or per namespaced `BackupLocation`). Holds
repo state **and the discovery inventory**. Not user-facing (users read `Backup` status).

```yaml
status:
  location: { kind: ClusterBackupLocation, name: dr-primary }   # or BackupLocation + ownerNamespace
  scope: Cluster                             # Cluster | Namespaced
  ownerNamespace: ""
  repositoryURL: "s3:…/prod/prod-eu-1"
  initialized: true
  mode: Standard
  keySlots: [platform]                       # cluster: [platform]; tenant repo: [tenant] (+platform if platformAccess)
  snapshotCount: 4123
  namespacesPresent: 42                      # distinct namespace tags found in the repo
  lastDiscoveryTime: "2026-07-12T02:55:00Z"
  lastMaintenanceTime: ...
  lastCheckTime: ...
  lastCheckResult: Passed
  approximateSizeBytes: ...
  conditions: [...]
```

---

## Repository layout & snapshot identity

Identical for both planes (one mover/restore/CLI codebase; upstream `restic` reads either).
Rationale in [adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md).

| restic field | Value | Role |
|---|---|---|
| **host** | `<clusterID>` | which cluster; multi-cluster into one bucket; retention grouping key |
| **paths** | `/data/<namespace>/<pvc>` (data) · `/manifests/<namespace>` (namespace manifests) · `/cluster-manifests` (cluster-scoped objects, one per run — [adr/0011](adr/0011-cluster-scoped-dr.md)) | **identity + per-PVC retention** (`--group-by host,paths`) + clean subtree restore |
| **tags** | `crystalbackup`, `tenant=<t>`, `namespace=<ns>`, `pvc=<name>`, `kind=data\|manifests\|cluster-manifests`, `schedule=<name>`, `run=<backup>` | filtering, right-to-erasure (`forget --tag`), discovery, correlation |

- **Retention** (per PVC): `restic forget --tag crystalbackup --group-by host,paths --keep-*`
  → groups by `(clusterID, namespace, pvc)`; manifests are their own chain.
- **Erasure** (`ClusterErasure`): `restic forget` filtered by `--tag tenant=` / `namespace=`
  / `namespace=+pvc=` then `prune`.
- **Restore**: `restic restore <snapshotID>:/data/<ns>/<pvc> --target <mount>` restores the
  subtree at the target root.
- **Discovery**: the `run` tag is the stable identity that groups a namespace's snapshots
  into one `Backup` (`metadata.name == run`).

## Discovery (repository→Backup projection)

The discovery controller (per `BackupRepository`, on location add and every
`discovery.interval`):

1. `restic snapshots --json --tag crystalbackup` → group by `(namespace, run)`.
2. For each `(namespace, run)` where the **namespace exists**: ensure a `Backup` object
   named `run` exists in that namespace (create/update the projection; `status.volumes`
   from the snapshots). Namespaces that do not exist are skipped (available to
   `ClusterRestore`, which reads the repo directly).
3. Remove `Backup` projections whose snapshots are gone (post-`forget`).

This makes adding a `[Cluster]BackupLocation` sufficient to bootstrap DR: the operator
inventories the repo and lists what is restorable, with no prior CRs.

## Restore selection model

Shared by `Restore` (target = own namespace) and `ClusterRestore` (target = any namespace).
Two orthogonal axes: **mode** and **selection** (NetworkPolicy-style lists).

### Mode (`spec.mode`)

| Mode | Manifests | PVC data |
|---|---|---|
| `Recreate` | delete selected resources that exist, then create from backup | fresh PVC, or into an existing PVC `restic restore --overwrite always --delete` (exact match) |
| `Overwrite` | server-side apply (create-or-update); **keep** resources absent from the backup | `restic restore --overwrite always` **without** `--delete`: overwrite present files, restore missing files, **keep** files present in the PVC but absent from the backup |

Restore into a non-existent namespace (`target.createNamespace`) is non-destructive.
`Recreate`, and `Overwrite` into an existing target, are **destructive** → require
`spec.confirmation == <target namespace>` (R23). A per-item `mode` override is reserved.

### Selection (lists, OR between items, AND within an item)

```yaml
resources:                           # a resource is restored iff ANY item matches
  - selector: { matchLabels: { app: web } }        # AND: label match
    include: ["apps/Deployment"]                    #  AND: type/name (globs)
  - include: ["apps/StatefulSet/postgres", "Secret/db-creds"]
volumes:                             # a PVC is restored iff ANY item matches
  - names: ["data-postgres-0"]                       # whole PVC
  - names: ["uploads"]
    include: ["images/2026/**"]                      # file-level narrowing of this PVC
    exclude: ["images/2026/tmp/**"]
    targetPath: "/"
```

Defaults: **both `resources` and `volumes` omitted ⇒ whole namespace**. A present field
(even `[]`) restores only what it lists (`[]` ⇒ nothing of that kind).

Manifest restore applies transformations (storageClass mapping, sanitized fields) and an
apply order — see [04-manifest-backup.md](04-manifest-backup.md).

---

## Namespace selection (`ClusterBackupSchedule`/`ClusterBackup`)

```yaml
namespaces:
  matchNames: ["c-app-*"]            # glob
  matchLabels: { crystalbackup.io/protect: "true" }
  matchExpressions: [ ... ]          # standard label expressions
  regexp: "^c-.+$"                   # name regex (power tool; see adr/0009 caveat)
  exclude: ["kube-*", "crystal-backup-system"]
```

At least one positive selector (`matchNames` / `matchLabels` / `matchExpressions` /
`regexp`) must be set; `exclude` is applied last.

## Repository sharding (deferred)

v1 uses **one repository per `[Cluster]BackupLocation`** (all namespaces share the cluster
repo). Per-namespace / per-tenant **sharding** of restic repositories is a deliberate open
question (blast radius, per-tenant prune isolation, per-tenant keys, RAM bounds) recorded
in [adr/0009 § Alternatives](adr/0009-shared-cluster-repo-tag-tenancy.md); it is **not**
implemented now. The tag layout and discovery are designed so sharding could be introduced
later without changing the CRD surface.

## Validation & admission (VAP-first)

Blocking, **static** validations (safety, isolation, confirmation) are expressed as
**`ValidatingAdmissionPolicy`** (CEL, evaluated in-API-server), so they hold even when the
operator is down. The operator's **webhook** is reserved for the one genuinely **dynamic**
blocking check (single-default uniqueness), is scoped to this project's CRDs, and runs with
`failurePolicy: Ignore` so an unavailable operator never wedges the API server; cross-object
**advisory** (non-blocking) checks are emitted by **controllers**, not admission. See
[adr/0010](adr/0010-admission-vap-first.md). Rule numbers are stable (other specs cite them).

| # | Rule | Enforcement |
|---|---|---|
| 1 | **Destructive confirmation (R23)** — enforced as a **conservative superset**: **every** `Restore`/`ClusterRestore` in `Recreate` **or** `Overwrite`, and **every** `ClusterErasure`, requires `spec.confirmation` equal to the target (namespace name for restores; target identity for erasure). Pure static field equality — VAP cannot test whether the target already exists, so it asks unconditionally in those modes (safe over-approximation). | **VAP** (Deny) |
| 2 | **User isolation** — a user-created `Backup`/`BackupSchedule` must reference a namespaced `BackupLocation`, never a `ClusterBackupLocation`; a `BackupExternalSync`'s `sourceLocationRef` **and** `destinationLocationRef` are both same-namespace `BackupLocation`s (never a `ClusterBackupLocation`); a `Restore` has no target-namespace field. The binding **excludes the operator's ServiceAccount** (`matchConditions`) so the operator's own cluster-origin fan-out `Backup`s are not denied. (Cluster-origin `Backup` read-only to users is **RBAC**, not admission.) | **VAP** (Deny) |
| 3 | **Retention (on the location)** — `spec.retention` lives on the `ClusterBackupLocation`/`BackupLocation`, not on schedules/runs: one shared repository has one authoritative policy (adr/0009), so per-run policies cannot fight over the same snapshots. A keep-less policy is a safe no-op (the controller runs no `forget`, so it can never drop every snapshot — no VAP needed). On an `Immutable` location keep* is ignored (object-lock governs expiry); the location knows its own mode, so this is a **controller-side advisory** (`RetentionIgnored` condition on the location) — a same-object check that could be tightened to CEL later if a hard reject is ever wanted. | **controller** (advisory) |
| 4 | **Single default `ClusterBackupLocation`** — cross-object uniqueness is not expressible in per-object CEL → the operator checks it (**webhook** + `MultipleDefaults` status condition on races). | **webhook** |
| 5 | `credentialsSecretRef`/`repositoryPasswordSecretRef` on a `BackupLocation` are same-namespace (name-only). | **VAP** (Deny) |
| 6 | `Immutable` mode forbids `maintenance.pruneSchedule`. | **VAP** (Deny) |
| 7 | **Denied namespaces** — tenant-facing CRs rejected in a configurable deny-list (default `kube-*`, `crystal-backup-system`, and any incumbent backup tool's namespace). The list is a **ConfigMap** bound to the policy via `paramRef`. | **VAP** (Deny, parameterized) |
| 8 | `namespaces` selector must set exactly one positive form + optional `exclude`. | **VAP** (Deny) |
| 9 | **External sync target** — `spec.sourceLocationRef.name != spec.destinationLocationRef.name` on `BackupExternalSync` **and** `ClusterBackupExternalSync` (a self-referential `Mirror` sync would `forget`/`prune` its own source). Static field inequality. | **VAP** (Deny) |

Defaulting (e.g. `clusterID` from the default `ClusterBackupLocation`, the generated repository
password Secret) is a **mutating** concern handled in the operator's mutating webhook / reconcile
path; not safety-critical, so `failurePolicy: Ignore` there is acceptable too.

## Reserved / pending fields

- `Restore`/`ClusterRestore` `manifests`-level `dryRun` + `status` diff report (owned by
  [04-manifest-backup.md §5](04-manifest-backup.md), M3).
- Per-item `mode` override on `resources[]`/`volumes[]` (deferred).
- `BackupSchedule`/`ClusterBackupSchedule` `manifestOptions.excludeSecretData`
  ([03-security-and-tenancy.md §10](03-security-and-tenancy.md), M3).
- **Mover placement on `[Cluster]BackupLocation`** (`spec.jobAffinity` = core `nodeAffinity` +
  `tolerations`, reused verbatim from `corev1`) to steer mover Jobs near the S3 endpoint —
  bandwidth, S3 IO cost, or network segmentation where S3 is reachable only from some nodes.
  **Future.** On a `ClusterBackupLocation` (admin) it is unrestricted; on a namespaced
  `BackupLocation` it is governance-gated — off by default, or bounded to an admin-defined
  allowlist — since choosing nodes is normally outside a tenant's scope (R2). See
  [00-requirements.md §6](00-requirements.md).

## RBAC packaging

- **Namespace user** ClusterRole (aggregated into the platform's per-namespace user roles;
  uniform write verbs, no `*`): CRUD on `backupschedules`, `backuplocations`, `restores`,
  `backupexternalsyncs`; **read-only** on `backups` (they are operator/discovery-managed
  projections).
- **Admin**: manage `clusterbackuplocations`, `clusterbackupschedules`, `clusterbackups`,
  `clusterrestores`, `clustererasures`, `clusterbackupexternalsyncs`.
- `backuprepositories` are operator-internal (no tenant access).
