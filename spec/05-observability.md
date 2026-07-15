# Observability

Status: draft v3 (two-plane cascade + shared-repository + repository-as-source-of-truth).
Implements the non-functional observability requirements of
[00-requirements.md §5](00-requirements.md) and R19, and the cascade/discovery/erasure
model of R25/R26/R21; metric names below canonicalize the shorthand used in
[90-roadmap.md M1](90-roadmap.md). Naming contract for CRs and labels:
[02-api.md](02-api.md); model rationale: [adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md).

## 1. Principles

- **Metrics come from the operator only.** Movers are ephemeral Jobs; they never expose a
  scrape endpoint and there is no Pushgateway. The mover shim reports a structured result
  (termination message JSON, cf. [01-architecture.md §1](01-architecture.md)) and the
  operator translates it into metrics. One stable scrape target: the operator `/metrics`
  endpoint in `crystal-backup-system`.
- **The cascade drives where a metric comes from.** Per-namespace backup/restore metrics
  are emitted from the **`Backup`/`Restore` objects** (the single unit of execution, both
  planes converge on it); **run-level** metrics come from **`ClusterBackup`**. Just as
  `ClusterBackup.status` keeps only aggregate counters + a capped failures list (no
  unbounded `perNamespace` map — [adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md)),
  run-level metrics carry **no per-namespace label**: per-namespace detail lives in the
  `Backup` metrics.
- **Tenant-attributable everything** (R19): every tenant-relevant metric carries
  `{namespace, tenant, location, cluster}`; alert rules therefore route per tenant with the
  platform Alertmanager, and tenant dashboards filter on `namespace`.
- **Restart-safe gauges**: on operator start, `*_last_*` and boolean state gauges are
  rebuilt from CR/repo state (`BackupSchedule.status.lastSuccessTime`,
  `ClusterBackupSchedule.status.lastRunName`, `BackupRepository.status.*`,
  `ClusterErasure.status.phase`), so alerts do not flap on operator restarts. Counters
  restart at zero; alert expressions use `increase()`.
- **The repository is the source of truth** ([adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md)):
  discovery and inventory gauges (`crystalbackup_discovery_*`, `crystalbackup_repository_*`)
  are derived from `restic snapshots` / `BackupRepository.status`, so they survive a lost
  cluster and are rebuilt on a fresh one as soon as a `[Cluster]BackupLocation` is added.
- **Bounded cardinality**: label values are CR names (namespaces, schedules, locations) —
  no per-PVC, per-backup (`run`) or per-snapshot labels in v1, with a single documented
  exception (`crystalbackup_pvc_volumesnapshot_count`, §2.9). `tenant` is functionally
  determined by `namespace` (one tenant per namespace) → no extra cardinality. Per-volume
  detail lives in `Backup.status.volumes`, logs and traces, not in metrics.

## 2. Metrics

Prefix `crystalbackup_`. **Common tenant labels**: `namespace` (origin namespace of the
CR), `tenant` (owning tenant), `location` (location CR name), `cluster` (the `clusterID`
Helm value, R20). Backup/restore metrics additionally carry `origin` (`cluster|namespace`,
from the `crystalbackup.io/origin` label) and `schedule`. Repository-level metrics (repo,
discovery) use `scope` (`cluster|namespace`, from `BackupRepository.status.scope`) instead
of `origin`, and carry `namespace` **only for namespaced user repos** (empty for the shared
cluster repo). `ClusterRestore` operations are recorded under the **origin** namespace.

### 2.1 Backup (per namespace — from `Backup` objects)

| Metric | Type | Labels | Description |
|---|---|---|---|
| `crystalbackup_backup_last_success_timestamp_seconds` | gauge | namespace, tenant, schedule, origin, location, cluster | Unix time of the last `Completed` `Backup`. Initialized to the schedule's creation time on first reconcile, so `BackupMissed` fires even if no backup ever succeeded. Rebuilt on restart from `Backup`/schedule status. |
| `crystalbackup_backup_duration_seconds` | histogram | namespace, tenant, schedule, origin, location, cluster | End-to-end `Backup` duration (Pending→Completed). Buckets: 60, 300, 900, 1800, 3600, 7200, 14400, 28800. |
| `crystalbackup_backup_last_size_bytes` | gauge | namespace, tenant, schedule, origin, location, cluster | Logical size of the last backup (Σ `status.volumes[].sizeBytes`) — per-namespace even in the shared repo (unaffected by cross-namespace dedup). |
| `crystalbackup_backup_last_added_bytes` | gauge | namespace, tenant, schedule, origin, location, cluster | Dedup delta of the last backup (Σ `status.volumes[].addedBytes`). |
| `crystalbackup_backup_added_bytes_total` | counter | namespace, tenant, schedule, origin, location, cluster | Cumulative bytes uploaded to the repository (S3 egress estimation). |
| `crystalbackup_backup_failures_total` | counter | namespace, tenant, schedule, origin, location, cluster | `Backup`s ending `Failed` or `PartiallyFailed`. |
| `crystalbackup_schedule_active` | gauge | namespace, tenant, schedule, origin, location, cluster | 1 when an unpaused schedule is expected to back up this `(namespace, schedule)`: a namespaced `BackupSchedule` (`origin=namespace`), **or** a `ClusterBackupSchedule` whose namespace selection matches this namespace (`origin=cluster` — the operator resolves the selection and emits one series per matched namespace). Drives per-namespace `BackupMissed` across the cluster-plane fan-out. |

### 2.2 ClusterBackup runs (run-level — from `ClusterBackup`)

Cluster plane DR runs; **no `namespace` label** (per-namespace health is in §2.1). Rebuilt
on restart from `ClusterBackupSchedule.status.lastRunName` → the run's aggregate status.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `crystalbackup_clusterbackup_last_success_timestamp_seconds` | gauge | schedule, location, cluster | Unix time of the last `Completed` `ClusterBackup` run (fleet DR health). |
| `crystalbackup_clusterbackup_duration_seconds` | histogram | schedule, location, cluster | Run duration (fan-out start → all children terminal). Same buckets as §2.1. |
| `crystalbackup_clusterbackup_namespaces_matched` | gauge | schedule, location, cluster | Namespaces matched by the last run (`status.namespacesMatched`). |
| `crystalbackup_clusterbackup_namespaces_failed` | gauge | schedule, location, cluster | Namespaces with a failed child `Backup` in the last run (`status.namespacesFailed`). |
| `crystalbackup_clusterbackup_runs_total` | counter | schedule, location, cluster, result | Runs by terminal `result` ∈ `completed`\|`partiallyfailed`\|`failed` (fleet run success ratio). |

### 2.3 Restore

Both `Restore` (namespaced) and `ClusterRestore` (recorded under the **origin** namespace).
`origin` = plane of the source `Backup`/coordinate; `mode` = the restore mode (the old
`newPVC`/`replacePVC`/`filesInto` targets are gone — [02-api.md § Restore selection model](02-api.md#restore-selection-model)).

| Metric | Type | Labels | Description |
|---|---|---|---|
| `crystalbackup_restore_last_success_timestamp_seconds` | gauge | namespace, tenant, origin, location, cluster | Unix time of the last `Completed` `Restore`/`ClusterRestore`. |
| `crystalbackup_restore_duration_seconds` | histogram | namespace, tenant, origin, location, cluster, mode | Restore duration; `mode` ∈ `Recreate`, `Overwrite`. Same buckets as backup. |
| `crystalbackup_restore_last_restored_bytes` | gauge | namespace, tenant, origin, location, cluster | `status.restoredBytes` of the last completed restore. |
| `crystalbackup_restore_failures_total` | counter | namespace, tenant, origin, location, cluster, mode | Restores ending `Failed`. `AwaitingConfirmation` (R23) is not a failure. |

### 2.4 Repository, maintenance & verification (R17)

The cluster repository is **shared** across all namespaces ([adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md)),
so these metrics are **per-repository, not per-namespace**: keyed by `location`/`scope`,
with `namespace` **empty** for the shared cluster repo and set to the **owner namespace**
for a `scope=namespace` user repo. A `check`/`prune` result on the shared repo is
therefore a **platform-wide** signal (routes to admins); the same metric with
`scope=namespace` routes to that tenant. Sourced from `BackupRepository.status` and
maintenance Job results. (Restore-testing is the administrator's responsibility — no automated
canary metric in v1, R17.)

| Metric | Type | Labels | Description |
|---|---|---|---|
| `crystalbackup_repository_size_bytes` | gauge | location, scope, namespace, cluster | `status.approximateSizeBytes`; refreshed after each backup and prune. For the shared cluster repo this is the whole-cluster physical size (all namespaces, post-dedup). |
| `crystalbackup_repository_snapshot_count` | gauge | location, scope, namespace, cluster | `status.snapshotCount` in the repository. |
| `crystalbackup_repository_last_check_timestamp_seconds` | gauge | location, scope, namespace, cluster | Last `restic check` completion (`status.lastCheckTime`). |
| `crystalbackup_repository_last_check_success` | gauge | location, scope, namespace, cluster | 1 if `status.lastCheckResult: Passed`, else 0. |
| `crystalbackup_repository_last_maintenance_timestamp_seconds` | gauge | location, scope, namespace, cluster | Last successful prune (`status.lastMaintenanceTime`). Absent for `Immutable` locations (no prune, R18). |
| `crystalbackup_repository_stale_locks` | gauge | location, scope, namespace, cluster | Repo lock files older than the restic staleness threshold (30 min) currently present. Normally reaped to 0 by the orphan reaper. |
| `crystalbackup_repository_locks_reaped_total` | counter | location, scope, namespace, cluster | Stale locks removed by the reaper (`restic unlock`). |

### 2.5 Discovery (repository→Backup projection, R26)

Per `BackupRepository`; derived from `restic snapshots` grouped by `(namespace, run)` and
`BackupRepository.status`. Restart- and cluster-loss-safe (the repo is the source of truth).

| Metric | Type | Labels | Description |
|---|---|---|---|
| `crystalbackup_discovery_last_timestamp_seconds` | gauge | location, scope, cluster | Last discovery scan completion (`status.lastDiscoveryTime`). |
| `crystalbackup_discovery_last_success` | gauge | location, scope, cluster | 1 if the last discovery scan succeeded, else 0. Restart-safe; drives `DiscoveryFailed`. |
| `crystalbackup_discovery_projected_backups` | gauge | location, scope, cluster | `Backup` projections currently materialized from this repo into **existing** namespaces — i.e. exactly what `kubectl get backups` lists for it (CR lifetime = data lifetime). |
| `crystalbackup_discovery_orphan_snapshots` | gauge | location, scope, cluster | Snapshot `(namespace, run)` groups whose namespace does **not** exist (not projected; restorable only via `ClusterRestore`). A non-zero value is DR data for gone namespaces, not an error. |

### 2.6 Right-to-erasure (`ClusterErasure`, R21)

Cluster plane only (targets a `ClusterBackupLocation`). Physical deletion —
`restic forget --tag` + `prune`; no per-tenant crypto-shredding in a shared repo
([adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md), [adr/0004](adr/0004-encryption-key-management.md)).

| Metric | Type | Labels | Description |
|---|---|---|---|
| `crystalbackup_erasure_snapshots_forgotten_total` | counter | location, cluster | Snapshots removed by `ClusterErasure` (`restic forget --tag`; Σ `status.snapshotsForgotten`). |
| `crystalbackup_erasure_reclaimed_bytes_total` | counter | location, cluster | Bytes physically reclaimed by the post-erasure `prune` (Σ `status.reclaimedBytes`). |
| `crystalbackup_erasure_blocked` | gauge | location, cluster | `ClusterErasure` objects currently `Blocked` (Immutable object-lock not yet expired). Restart-safe from CR status; drives `ErasureBlocked`. |
| `crystalbackup_erasure_last_completion_timestamp_seconds` | gauge | location, cluster | Unix time of the last `Completed` erasure. |

### 2.7 Concurrency & queueing (R12)

Platform-scope; `cluster` label only (except mover retries, attributed to a namespace).

| Metric | Type | Labels | Description |
|---|---|---|---|
| `crystalbackup_mover_active` | gauge | cluster | Mover Jobs currently running. |
| `crystalbackup_mover_queue_depth` | gauge | cluster | Movers admitted by controllers but waiting on the `maxConcurrentMovers` semaphore. |
| `crystalbackup_mover_concurrency_limit` | gauge | cluster | Configured `maxConcurrentMovers` (exported so dashboards show usage vs limit). |
| `crystalbackup_mover_job_retries_total` | counter | namespace, tenant, cluster | Mover pod retries consumed against `backoffLimit`. |

### 2.8 Admission (VAP-first)

Platform-scope. Static rules are `ValidatingAdmissionPolicy` ([adr/0010](adr/0010-admission-vap-first.md));
their denials surface in the API server's own
`apiserver_validating_admission_policy_check_total{policy}` (scraped from the apiserver, not
emitted here). The operator's webhook metric counts only the dynamic rule(s).

| Metric | Type | Labels | Description |
|---|---|---|---|
| `crystalbackup_webhook_denials_total` | counter | webhook, reason | Requests denied by the operator's **dynamic** webhook (e.g. `multiple_defaults`). Static-rule denials (confirmation, isolation, deny-list, immutable-prune) are VAP and appear in the apiserver's `apiserver_validating_admission_policy_check_total`. |

### 2.9 Snapshot exposure & coexistence

| Metric | Type | Labels | Description |
|---|---|---|---|
| `crystalbackup_exposure_ready_wait_seconds` | histogram | namespace, tenant, exposer, cluster | Wait from snapshot exposure start (VSC re-bind + temp PVC creation) until the exposed PVC is bound and the mover can start. |
| `crystalbackup_pvc_volumesnapshot_count` | gauge | namespace, pvc, cluster | VolumeSnapshot objects per source PVC (includes an incumbent tool's, e.g. Velero's, cf. [ADR 0006](adr/0006-coexistence-with-backup-tools.md)). **Documented exception to the §1 no-per-PVC-label rule**: cardinality is bounded by the live PVC count, the series is deleted with the PVC, and the ceph-csi flatten-threshold risk during coexistence justifies per-PVC visibility. |

### 2.10 Inherited controller-runtime metrics

The Go operator inherits the controller-runtime registry for free (kubebuilder metrics
reference): `controller_runtime_reconcile_total`, `controller_runtime_reconcile_errors_total`,
`controller_runtime_reconcile_time_seconds`, `workqueue_depth`,
`workqueue_queue_duration_seconds`, `rest_client_requests_total`, leader-election and
`controller_runtime_webhook_latency_seconds`, plus standard Go process/runtime metrics.
These are platform-facing (no tenant labels) and feed the platform dashboard only.

**Future metrics (per ADR)**: [ADR 0005](adr/0005-immutability-mode.md) adds at M8
`crystalbackup_immutable_window_start_timestamp_seconds` and
`crystalbackup_immutable_expired_repos_deleted_total`, both `{location, scope, cluster}` —
listed here for name reservation, not part of the v1 catalogue.

### 2.11 Accounting-ready figures (R19 — no billing in-tool)

Crystal Backup does **no accounting or billing**; it exposes the raw figures a downstream
system needs. Most exist above; this names the accounting view and fills the gaps.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `crystalbackup_backup_total` | counter | namespace, tenant, schedule, origin, location, cluster, result | Completed `Backup`s by terminal `result` (`completed`\|`partiallyfailed`\|`failed`) — the backup **count** per tenant. |
| `crystalbackup_backup_protected_bytes` | gauge | namespace, tenant, origin, location, cluster | Logical bytes currently protected for the namespace (Σ newest `status.volumes[].sizeBytes` of its live backups) — "how much data is being backed up". Exact, per-namespace, dedup-independent. |
| `crystalbackup_repository_stored_bytes` | gauge | location, scope, namespace, cluster | Physically stored, **deduplicated + compressed** bytes of the repository (`restic stats --mode raw-data`), refreshed with maintenance — the exact bill for that bucket (companion to `crystalbackup_repository_size_bytes`). |

Already-present inputs to accounting: `crystalbackup_backup_last_size_bytes` (logical, per
namespace), `crystalbackup_backup_last_added_bytes` + `crystalbackup_backup_added_bytes_total`
(dedup delta / cumulative upload), `crystalbackup_repository_size_bytes` (whole-repo physical),
`crystalbackup_backup_duration_seconds`.

**Per-PVC breakdown is on the API, not in metric labels.** `Backup.status.volumes[]` carries
exact per-PVC `sizeBytes` (logical) and `addedBytes` (dedup delta) for every run; an accounting
pipeline reads them from the `Backup` objects (or the repo), keeping Prometheus cardinality
bounded (§1). **Deduplicated _stored_ bytes attributed _per PVC_ are inherently best-effort**:
restic shares blobs across PVCs and snapshots, so only the repository total is exact. Worse, the
per-PVC `addedBytes` split is **order-dependent** — when two PVCs (or two snapshots) share a blob,
restic charges the *first* uploader and the others 0, so a PVC's `addedBytes` is **not reproducible
run-to-run** and depends on mover ordering. Any per-PVC storage figure is therefore an estimate and
must be labelled approximate wherever surfaced.

### 2.12 External backup synchronization (R28)

`ClusterBackupExternalSync`/`BackupExternalSync` — secondary-location replication via
`restic copy` ([adr/0013](adr/0013-external-backup-sync.md)). Sourced from the sync CR status;
labels: `sync` (CR name), `source`/`destination` (location names), `scope` (`cluster|namespace`),
`namespace` (empty for the cluster sync, the owner namespace for a `BackupExternalSync`),
`cluster`.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `crystalbackup_externalsync_last_success_timestamp_seconds` | gauge | sync, source, destination, scope, namespace, cluster | Unix time of the last `Completed` sync run. Restart-safe from CR status; drives `ExternalSyncStale`. |
| `crystalbackup_externalsync_duration_seconds` | histogram | sync, source, destination, scope, namespace, cluster | Sync run duration. Same buckets as §2.1. |
| `crystalbackup_externalsync_snapshots_copied_total` | counter | sync, source, destination, scope, namespace, cluster | Snapshots copied to the destination (`restic copy`). |
| `crystalbackup_externalsync_bytes_copied_total` | counter | sync, source, destination, scope, namespace, cluster | Bytes streamed to the destination (blob-incremental; S3 egress estimation). |
| `crystalbackup_externalsync_lag_snapshots` | gauge | sync, source, destination, scope, namespace, cluster | Source snapshots not yet present at the destination (`status.lagSnapshots`). |
| `crystalbackup_externalsync_failures_total` | counter | sync, source, destination, scope, namespace, cluster | Sync runs ending `Failed` or `PartiallyFailed`. |

## 3. Alert rules

Shipped in the Helm chart as a `PrometheusRule` (optional, `metrics.rules.enabled`).
Tenant-facing alerts carry the `namespace` label for per-tenant routing; repository-level
alerts route to admins for the shared cluster repo (`scope=cluster`) and to the tenant for
a user repo (`scope=namespace`, non-empty `namespace`).

```yaml
groups:
  - name: crystalbackup
    rules:
      - alert: CrystalbackupBackupMissed
        expr: |
          (time() - crystalbackup_backup_last_success_timestamp_seconds > 26 * 3600)
          and on (namespace, schedule, cluster) crystalbackup_schedule_active == 1
        for: 15m
        labels: { severity: warning }
        annotations:
          summary: "No successful backup for {{ $labels.namespace }}/{{ $labels.schedule }} ({{ $labels.origin }}) in 26h"
      - alert: CrystalbackupBackupFailed
        expr: increase(crystalbackup_backup_failures_total[1h]) > 0
        labels: { severity: warning }
        annotations:
          summary: "Backup failed for {{ $labels.namespace }}/{{ $labels.schedule }}"
      - alert: CrystalbackupRepositoryCheckFailed
        expr: crystalbackup_repository_last_check_success == 0
        for: 5m
        labels: { severity: critical }
        annotations:
          summary: "restic check failed on repository {{ $labels.location }} ({{ $labels.scope }})"
      - alert: CrystalbackupStaleLocks
        expr: crystalbackup_repository_stale_locks > 0
        for: 30m
        labels: { severity: warning }
        annotations:
          summary: "Stale restic locks persist on {{ $labels.location }} (reaper not clearing)"
      - alert: CrystalbackupDiscoveryFailed
        expr: crystalbackup_discovery_last_success == 0
        for: 30m
        labels: { severity: warning }
        annotations:
          summary: "Discovery failing on {{ $labels.location }} — Backup projections may be stale vs the repository"
      - alert: CrystalbackupErasureBlocked
        expr: crystalbackup_erasure_blocked > 0
        for: 1h
        labels: { severity: warning }
        annotations:
          summary: "Right-to-erasure blocked on {{ $labels.location }} (Immutable object-lock not yet expired, R21/ADR 0005)"
      - alert: CrystalbackupPVCSnapshotPileup
        expr: crystalbackup_pvc_volumesnapshot_count > 20
        for: 30m
        labels: { severity: warning }
        annotations:
          summary: "{{ $value }} VolumeSnapshots piling up on PVC {{ $labels.namespace }}/{{ $labels.pvc }} (ceph-csi flatten risk, ADR 0006)"
      - alert: CrystalbackupExternalSyncStale
        expr: |
          time() - crystalbackup_externalsync_last_success_timestamp_seconds > 26 * 3600
        for: 1h
        labels: { severity: warning }
        annotations:
          summary: "External sync {{ $labels.sync }} ({{ $labels.source }}→{{ $labels.destination }}) has not completed in 26h"
```

The 26 h `BackupMissed` deadline assumes the platform default of daily schedules (24 h +
2 h grace for jitter and long uploads). Non-daily schedules need a per-schedule threshold
(see Open questions).

## 4. Logging

**Format**: zap JSON-lines on stdout via the controller-runtime zap integration
(`--zap-encoder=json` forced in the chart; `--zap-log-level` defaults to `info`).
**Exactly one event per line, never multi-line**: stack traces are emitted only at
`error` and above, JSON-escaped into a single `stacktrace` field.

Key schema (contextual keys present when applicable):

| Key | Content |
|---|---|
| `ts` | ISO 8601 UTC with milliseconds |
| `level` | `debug` \| `info` \| `error` |
| `logger` | component, e.g. `backup-controller`, `discovery-controller`, `mover`, `maintenance` |
| `msg` | human-readable event |
| `namespace`, `tenant`, `backup`, `schedule`, `clusterbackup`, `restore`, `pvc` | tenant/cascade context (mirror the `crystalbackup.io/*` labels; `clusterbackup` = originating run for `origin=cluster`) |
| `controller`, `reconcileID` | controller-runtime reconcile context |
| `error` | error string (error level only) |
| `traceID` | present when tracing is active (Loki ↔ Tempo correlation) |

Example:

```json
{"ts":"2026-07-12T02:04:12.345Z","level":"info","logger":"backup-controller","msg":"volume uploaded","namespace":"c-team-x","tenant":"team-x","backup":"dr-daily-20260712-020000","clusterbackup":"dr-daily-20260712-020000","pvc":"data-postgres-0","controller":"backup","reconcileID":"f3c1a9d2","addedBytes":52428800}
```

**Mover logs, same schema via the shim**: the shim runs `restic --json`, parses the
machine-readable `status`/`summary` messages and re-emits them as schema-conforming lines
(`logger: "mover"`, with `namespace`/`backup`/`pvc`). Progress lines are throttled to at
most one per volume per 30 s. Any non-JSON restic stderr is wrapped verbatim into `msg`
of a single-line event. **Redaction**: `RESTIC_PASSWORD` (the user password or the wrapped
platform DEK), S3 credentials and unwrapped key material are never logged; the shim scrubs
its environment from any diagnostic output (security review checklist, cf.
[90-roadmap.md DoD](90-roadmap.md)).

## 5. Tracing

OpenTelemetry Go SDK (stable 1.x), configured **exclusively** through standard `OTEL_*`
env vars (`OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_SERVICE_NAME`, `OTEL_TRACES_SAMPLER`,
`OTEL_RESOURCE_ATTRIBUTES`, `OTEL_SDK_DISABLED`, …). When unset the tracer provider is a
no-op — the SDK default — with zero configuration and negligible overhead. Service names:
`crystal-backup-operator`, `crystal-backup-mover`.

Span tree for one backup (one root per `Backup` CR, linked across reconciles):

```
backup                                  crystalbackup.namespace, crystalbackup.tenant,
├── hooks.pre                           crystalbackup.backup, crystalbackup.schedule,
├── snapshot        (per PVC)           crystalbackup.origin, crystalbackup.location,
├── hooks.post                          crystalbackup.cluster
├── expose          (per PVC)           crystalbackup.pvc  (VSC re-bind + temp PVC bind)
├── mover           (per PVC)           crystalbackup.pvc, crystalbackup.node
│   └── restic.backup   (in mover)      crystalbackup.snapshot_id, crystalbackup.bytes_added
├── manifests                           crystalbackup.resource_count
└── forget                              crystalbackup.snapshots_removed
```

**Cluster-plane fan-out**: a `ClusterBackup` run gets its own root span; each per-namespace
`Backup` root span carries a **span link** to the run (mirrors the fan-out and avoids one
giant trace across all namespaces).

**Propagation to mover Jobs**: the operator injects the W3C context as `TRACEPARENT`
(and `TRACESTATE`) env vars into the Job pod template; the shim extracts it and parents
its `restic.*` spans there. `OTEL_*` vars for movers are set from Helm values
(`mover.extraEnv`). Duration histograms attach exemplars to spans when tracing is active
(roadmap M6).

## 6. Kubernetes Events

Emitted with an `EventRecorder` on the **user's own CRs**, so `kubectl describe` tells the
self-service story without platform access. Events are UX, not an alerting path (default
~1 h retention). Cluster-plane CR events target admins.

| CR | Reason | Type |
|---|---|---|
| `Backup` | `BackupStarted`, `SnapshotReady` (per PVC), `HookExecuted`, `VolumeUploaded`, `ManifestsUploaded`, `RetentionApplied`, `BackupCompleted` | Normal |
| `Backup` | `HookFailed`, `SnapshotTimeout`, `VolumeFailed`, `BackupPartiallyFailed`, `BackupFailed` | Warning |
| `BackupSchedule` | `BackupCreated` | Normal |
| `BackupSchedule` | `MissedSchedule` (operator downtime across a cron window) | Warning |
| `ClusterBackupSchedule` | `ClusterBackupCreated` | Normal |
| `ClusterBackupSchedule` | `MissedSchedule` | Warning |
| `ClusterBackup` | `RunStarted`, `NamespaceBackupCreated` (per namespace, fan-out), `RunCompleted` | Normal |
| `ClusterBackup` | `RunPartiallyFailed`, `RunFailed` | Warning |
| `Restore`/`ClusterRestore` | `RestoreStarted`, `ConfirmationAccepted` (R23), `RestoreCompleted` | Normal |
| `Restore`/`ClusterRestore` | `AwaitingConfirmation` (R23), `RestoreDenied`, `RestoreFailed` | Warning |
| `ClusterErasure` | `ErasureStarted`, `ConfirmationAccepted` (R23), `ErasureCompleted` | Normal |
| `ClusterErasure` | `AwaitingConfirmation`, `ErasureBlocked` (Immutable object-lock), `ErasureFailed` | Warning |
| `BackupRepository` | `RepositoryInitialized`, `KeySlotAdded`, `DiscoveryCompleted`, `CheckPassed`, `PruneCompleted` | Normal |
| `BackupRepository` | `CheckFailed`, `StaleLockRemoved`, `DiscoveryFailed` | Warning |
| `BackupLocation`/`ClusterBackupLocation` | `LocationValidated` | Normal |
| `BackupLocation`/`ClusterBackupLocation` | `LocationUnreachable` | Warning |

## 7. Grafana dashboards

Two dashboards ship in the Helm chart as ConfigMaps labeled `grafana_dashboard: "1"`
(sidecar provisioning), JSON sources under `charts/crystal-backup/dashboards/`.

- **Tenant dashboard** (`crystalbackup-tenant`): templated on a `$namespace` variable so it
  plugs into the platform's per-tenant dashboard provisioning (variable pinned per tenant
  console). Panels: time since last successful backup per schedule split by `origin`
  (cluster DR vs the user's own off-platform backups — stat, thresholds at 24 h/26 h),
  backup duration trend, last size vs added bytes (dedup efficiency), the tenant's own
  repository size and check status (`scope=namespace`, their `BackupLocation`), restore history with
  `mode`, recent failures table (from their `Backup` objects). Cluster-origin backups appear
  read-only.
- **Platform dashboard** (`crystalbackup-platform`): fleet success ratio, oldest
  `last_success` age across namespaces (top-N table), failures heatmap by namespace/tenant,
  `ClusterBackup` run history (`crystalbackup_clusterbackup_runs_total` ratio,
  namespaces_matched vs namespaces_failed per run),
  `crystalbackup_mover_active` + `crystalbackup_mover_queue_depth` vs
  `crystalbackup_mover_concurrency_limit`, prune/check recency per repository, stale locks,
  **discovery health** (projected backups, orphan snapshots, last
  discovery age), **erasure activity** (snapshots forgotten, reclaimed bytes, blocked),
  **external-sync health** (lag, last-success and bytes copied per sync),
  controller-runtime reconcile errors and workqueue depth.

## 8. Open questions

1. **Run-level DR-missed alert**: add a fleet `ClusterBackupRunMissed`
   (`time() - crystalbackup_clusterbackup_last_success_timestamp_seconds > threshold`) in
   addition to per-namespace `BackupMissed`? Decide at M1 alongside the per-schedule
   deadline. Location-name collisions between a namespaced `BackupLocation` and a
   `ClusterBackupLocation` are already disambiguated by the `scope`/`origin` labels.
2. **`cluster` label ownership**: the operator stamps `cluster=<clusterID>`; the platform
   Prometheus also sets external labels at federation. Confirm they agree (same value) to
   avoid duplicate series across the platform monitoring stack.
3. **Per-schedule `BackupMissed` deadline**: derive the threshold from the cron interval
   (e.g. 1.1 × period) instead of the fixed 26 h — candidate refinement for M6. The same
   per-schedule treatment applies to `ExternalSyncStale` (§2.12), whose sync schedules may be
   weekly/monthly — its fixed 26 h threshold must become per-schedule too.
