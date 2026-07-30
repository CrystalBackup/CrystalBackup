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
  **Known gap, being closed in 0.6.0**: the collector resolves `cluster` by looking a metric's
  location name up among the `ClusterBackupLocation`s, so every NAMESPACE-plane series (whose
  location is a namespaced `BackupLocation`) carries `cluster=""`. The controllers know the real
  value — `BackupLocation.status.clusterID` — so this is a resolution gap, not missing data. Two
  consequences worth naming, because they are why it must be fixed in ONE place: filling it in
  only at the recording sites would split every namespace-plane family into two label sets, one
  from the counter and one from the gauge; and the lookup being keyed by BARE NAME means a
  namespaced location that happens to share a name with a cluster one currently borrows its
  `clusterID`, which is worse than an empty label — it is a wrong one.
- **Restart-safe gauges**: on operator start, `*_last_*` and boolean state gauges are
  rebuilt from CR/repo state (`BackupSchedule.status.lastSuccessTime`,
  `ClusterBackupSchedule.status.lastRunName`, `BackupRepository.status.*`,
  `ClusterErasure.status.phase`), so alerts do not flap on operator restarts.
  **Counters do not restart at zero — they DISAPPEAR, and `increase()` does not cope.**
  This bullet used to end "counters restart at zero; alert expressions use `increase()`",
  and the second clause does not follow from the first because the first is not what
  happens. A `prometheus.CounterVec` child is created by its first `Inc()`: before that
  there is no series, and after a restart there is no series again until something fails
  anew. `increase()` handles a counter RESET (a series that dips and continues); it has
  nothing to say about a series that simply ENDS, and a series whose samples in the window
  are all the same value has no rise to measure either. That second half is the wider
  defect and it needs no restart at all: because the child is created BY the first `Inc()`,
  it appears at **1** rather than stepping `0 → 1`, so the **first failure a series ever
  records cannot move `increase()`**. Only a second one can. An alert built on the counter
  alone was therefore blind to exactly the incident an operator most wants paged — a
  namespace that was fine yesterday and failed once tonight. Measured on a live cluster after
  the operator pod was replaced: `crystalbackup_backup_failures_total` returned **zero
  series**, `increase(...[1h])` returned three series all equal to **0**, and a `Backup`
  had genuinely failed. `CrystalbackupBackupFailed` was silent.
  The correction is not to abandon counters — a counter is still the only thing that can
  count an event whose object was deleted — but to give any counter an alert depends on a
  **state-derived companion**, and to `or` the two. `crystalbackup_backup_last_failure_timestamp_seconds`
  (§2.1) is that companion for the backup-failure family. What survives neither is a
  failure whose operator restarted AND whose `Backup` object was garbage-collected inside
  the alert window; that residual hole is named in the rule's own rationale rather than
  papered over.
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

**`scope` is `cluster|namespace`, NOT the API's enum spelling.** The collector MAPS
`BackupRepository.status.scope` (`Cluster`/`Namespaced`, a kubebuilder enum) onto these values; it
does not publish it verbatim. Corrected in M6, where publishing it raw had made `scope` mean two
different things depending on family — repository and discovery emitted `Cluster|Namespaced` while
external sync emitted `cluster|namespace`, deriving from the same constants as `origin`. One alert
expression written across both families would silently match nothing on one of them. One
vocabulary, lowercase, matching `origin` (the dimension `scope` replaces on these families) and
matching the Prometheus convention for enumerated label values.

### 2.1 Backup (per namespace — from `Backup` objects)

| Metric | Type | Labels | Description |
|---|---|---|---|
| `crystalbackup_backup_last_success_timestamp_seconds` | gauge | namespace, tenant, schedule, origin, location, cluster | Unix time of the last `Completed` or `PartiallyCompleted` `Backup`. Derived from the `Backup` objects at scrape time, so it is **absent** until one has succeeded. |
| `crystalbackup_backup_duration_seconds` | histogram | namespace, tenant, schedule, origin, location, cluster | End-to-end `Backup` duration (Pending→Completed). Buckets: 60, 300, 900, 1800, 3600, 7200, 14400, 28800. |
| `crystalbackup_backup_last_duration_seconds` | gauge | namespace, tenant, schedule, origin, location, cluster | Duration of the LAST successful `Backup`, the same quantity the histogram above observes. Shipped since M1 and omitted from this table until M6 — recorded here because the omission is exactly the drift `names.go` now prevents. The gauge answers "how long did last night take" without a quantile; the histogram answers "how long does this usually take". |
| `crystalbackup_backup_last_size_bytes` | gauge | namespace, tenant, schedule, origin, location, cluster | Logical size of the last backup (Σ `status.volumes[].sizeBytes`) — per-namespace even in the shared repo (unaffected by cross-namespace dedup). |
| `crystalbackup_backup_last_added_bytes` | gauge | namespace, tenant, schedule, origin, location, cluster | Dedup delta of the last backup (Σ `status.volumes[].addedBytes`). |
| `crystalbackup_backup_added_bytes_total` | counter | namespace, tenant, schedule, origin, location, cluster | Cumulative bytes uploaded to the repository (S3 egress estimation). |
| `crystalbackup_backup_failures_total` | counter | namespace, tenant, schedule, origin, location, cluster | `Backup`s ending `Failed` or `PartiallyFailed`. In-process, so the series is **absent** until the first failure and absent again after a restart — see §1, and see the companion below. |
| `crystalbackup_backup_last_failure_timestamp_seconds` | gauge | namespace, tenant, schedule, origin, location, cluster | Unix time of the last `Backup` that reached `Failed` or `PartiallyFailed`. Derived from the `Backup` objects at scrape time (`status.completionTime`, falling back to `metadata.creationTimestamp` for objects that predate that field and for projections), so it is **absent** until one has failed and is rebuilt intact across an operator restart. The restart-safe half of `CrystalbackupBackupFailed`. |
| `crystalbackup_schedule_active` | gauge | namespace, tenant, schedule, origin, location, cluster | 1 when an unpaused schedule is expected to back up this `(namespace, schedule)`: a namespaced `BackupSchedule` (`origin=namespace`), **or** a `ClusterBackupSchedule` whose namespace selection matches this namespace (`origin=cluster` — the operator resolves the selection and emits one series per matched namespace). **0** when that schedule exists but is paused; **absent** when it does not exist. Drives per-namespace `BackupMissed` across the cluster-plane fan-out. |
| `crystalbackup_schedule_period_seconds` | gauge | namespace, tenant, schedule, origin, location, cluster | Longest gap between two consecutive activations of the schedule's cron expression. **Absent** when the expression cannot be parsed. Added in M6; it is what lets `BackupMissed` carry a per-schedule deadline instead of a flat 26 h (§8 q3). |
| `crystalbackup_schedule_created_timestamp_seconds` | gauge | namespace, tenant, schedule, origin, location, cluster | Unix time the schedule object was created — the instant from which backups started being expected. Added in M6; see the note below. |

**The never-succeeded hole, and why there are two extra series rather than one.** Until M6 this
table claimed `last_success` was "initialized to the schedule's creation time on first reconcile,
so `BackupMissed` fires even if no backup ever succeeded". The collector has never done that and
cannot honestly: `last_success` is derived from `Backup` objects at scrape time, and a schedule
that has never produced a successful one emits no series at all. `time() - <nothing>` is nothing —
so a schedule that was broken from the day it was applied, which is the single most likely way for
a NEW installation to be silently unprotected, was the one case the rule could never see. Seeding
`last_success` with a fake success timestamp would have closed it while lying to every dashboard
panel that reads "last backup". `schedule_created_timestamp_seconds` says the true thing instead,
and `BackupMissed` falls back to it when there is no success to measure from.

**Both are emitted from the same site as `schedule_active`**, with the same label set and the same
absence semantics, because `BackupMissed` joins all three in one expression: a schedule that is
deleted or has an unselectable selector drops all of them at once and the rule goes quiet together
rather than half-quiet.

**A paused schedule reports `0`, and until M6 it reported nothing at all.** For the `BackupMissed`
join the two readings are equivalent — the guard is `== 1`, which drops a `0` exactly as it drops a
missing series, so pausing still silences the rule, which is what pausing means. What absence
additionally destroyed was the **observation**. Somebody suspends a schedule "for the migration",
the migration ends, nobody resumes it, and there is no series anywhere that says so: the namespace
is unprotected and every rule and every dashboard agrees that all is well, indefinitely. A backup
tool cannot have a state in which silence and health are indistinguishable. `0` is a schedule
stating that it exists and is protecting nothing, which is something an alert can read —
`CrystalbackupSchedulePausedTooLong` (§3) is what reads it. **Deletion** remains the only thing that
removes the series, and that asymmetry is the point: deleting a schedule is a decision somebody
finished making, and leaving one paused usually is not.

`schedule_period_seconds` and `schedule_created_timestamp_seconds` follow `schedule_active` into the
paused state rather than dropping out with it. `SchedulePausedTooLong` needs `schedule_created` for
two separate reasons — to know the schedule is old enough for a seven-day lookback to mean anything,
and, because it is an instant vector, to require that the schedule still exists at evaluation time
rather than merely leaving samples behind in the window.

Both planes carry `spec.paused` since M6. Before it, a `BackupSchedule` had no way to suspend
itself, so a tenant's only option was to **delete** the schedule — which took the status history and
the baseline tick with it, and left a recreated schedule with no memory of when it last succeeded.

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

**A never-measured series is absent, not zero** — the alerts above depend on it. A repository that
has not yet been checked emits no `last_check_*` sample at all: a `0` success would page
`CrystalbackupRepositoryCheckFailed` the moment a location is created, and a `0` timestamp renders
as 1970 in every dashboard. Absence is the honest encoding of "not measured yet", and it makes both
rules no-op until there is something real to say. Same reasoning for `last_maintenance_*` on
`Immutable` locations, which never prune by design ([adr/0005](adr/0005-immutability-mode.md)) —
`CrystalbackupMaintenanceStalled` must not fire on them.

All seven are **state-derived**: a collector reads them off `BackupRepository.status` at scrape
time, so they survive an operator restart with no replay. `locks_reaped_total` is the one exception
and is necessarily a real counter — it records an event, and events are not recoverable from state.

### 2.5 Discovery (repository→Backup projection, R26)

Per `BackupRepository`; derived from `restic snapshots` grouped by `(namespace, run)` and
`BackupRepository.status`. Restart- and cluster-loss-safe (the repo is the source of truth).

| Metric | Type | Labels | Description |
|---|---|---|---|
| `crystalbackup_discovery_last_timestamp_seconds` | gauge | location, scope, namespace, cluster | Last discovery scan completion (`status.lastDiscoveryTime`). |
| `crystalbackup_discovery_last_success` | gauge | location, scope, namespace, cluster | 1 if the last discovery scan succeeded, else 0. Restart-safe; drives `DiscoveryFailed`. |
| `crystalbackup_discovery_projected_backups` | gauge | location, scope, namespace, cluster | `Backup` projections currently materialized from this repo into **existing** namespaces — i.e. exactly what `kubectl get backups` lists for it (CR lifetime = data lifetime). |
| `crystalbackup_discovery_orphan_snapshots` | gauge | location, scope, namespace, cluster | Snapshot `(namespace, run)` groups whose namespace does **not** exist (not projected; restorable only via `ClusterRestore`). A non-zero value is DR data for gone namespaces, not an error. |

`namespace` carries the repository's **owner** — empty for the shared cluster repo, the tenant's
namespace for a `scope=namespace` one — exactly as §2.4 uses it. It does not split a scan: a scan
covers every namespace in its repository at once. Added in M6, because without it a tenant could
see whether their repository was INTACT (check, prune, locks) but never whether the list
`kubectl get backups` gives them still matches what is in it. A failing discovery means precisely
that their restore points are silently stale — and it was visible only to the platform.

### 2.6 Right-to-erasure (`ClusterErasure`, R21)

Cluster plane only (targets a `ClusterBackupLocation`). Physical deletion —
`restic forget --tag` + `prune`; no per-tenant crypto-shredding in a shared repo
([adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md), [adr/0004](adr/0004-encryption-key-management.md)).

| Metric | Type | Labels | Description |
|---|---|---|---|
| `crystalbackup_erasure_snapshots_forgotten_total` | counter | location, cluster | Snapshots removed by `ClusterErasure` (`restic forget --tag`), counted at completion. |
| `crystalbackup_erasure_reclaimed_bytes_total` | counter | location, cluster | Bytes physically reclaimed by the post-erasure `prune`, counted at completion. |
| `crystalbackup_erasure_blocked` | gauge | location, cluster | `ClusterErasure` objects currently `Blocked` (Immutable object-lock not yet expired). Restart-safe from CR status; drives `ErasureBlocked`. |
| `crystalbackup_erasure_last_completion_timestamp_seconds` | gauge | location, cluster | Unix time of the last `Completed` erasure. |

**The two counters here are the one family that CANNOT be state-derived** (clarified M6; this
section first phrased them as "Σ `status.snapshotsForgotten`", which reads like a scrape-time sum
over live objects). It cannot work that way, and the reason is the point of the feature: a
completed erasure exists to make data gone, and the `ClusterErasure` object is itself
garbage-collected by the run-history limit. Summing live objects would make the running total of
what has been erased silently *decrease* — the one number a GDPR audit would ask for, drifting
downward. They are true counters, incremented once at completion, and like every other counter here
they do not survive an operator restart — the series disappears rather than resetting (§1).
Nothing alerts on them today, and that is fortunate: the state-derived companion
`CrystalbackupBackupFailed` grew to close the restart hole cannot be built here, because
the object a completed erasure would be derived from is exactly the one that has been
deleted. Any future rule on these has to be written knowing it can miss an erasure whose
operator restarted inside the alert window.

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
| `crystalbackup_exposure_ready_wait_seconds` | histogram | namespace, tenant, exposer, cluster | Wait from snapshot exposure start (VSC re-bind + temp PVC creation) until the exposed PVC is bound and the mover can start. **Backup path only** — `internal/rexposer`, which exposes a RESTORE target, is a different mechanism and is not instrumented here (M6 note; a restore's wait shows up inside `crystalbackup_restore_duration_seconds`). |
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
| `crystalbackup_backup_total` | counter | namespace, tenant, schedule, origin, location, cluster, result | Completed `Backup`s by terminal `result` (`completed`\|`partiallycompleted`\|`partiallyfailed`\|`failed`) — the backup **count** per tenant. |
| `crystalbackup_backup_protected_bytes` | gauge | namespace, tenant, origin, location, cluster | Logical bytes currently protected for the namespace (Σ newest `status.volumes[].sizeBytes` of its live backups) — "how much data is being backed up". Exact, per-namespace, dedup-independent. |

**`result` has four values, not three** (amended M6). `PartiallyCompleted` — volumes skipped because
their CSI cannot snapshot, none failed — is a distinct terminal phase, and folding it into
`completed` would erase the single signal that says a storage class quietly stopped being
snapshottable. It is a success with a hole in it, and the hole is the thing worth counting.

**There is no separate `repository_stored_bytes`** (amended M6; the name was specified and is
withdrawn before it ever shipped). It was to carry `restic stats --mode raw-data` as "the exact
bill", beside `crystalbackup_repository_size_bytes`. Two problems, found while implementing it.
The operator runs no `stats` operation, so the only number available to fill it was
`status.approximateSizeBytes` — which §2.4 already publishes. Two names, one reading, is a lie by
implication: a reader who sees both assumes they were measured differently and reasons about the
gap. And the withdrawn name was the *worse* of the two for its stated purpose, because
`--mode raw-data` counts data the repository still REFERENCES, excluding packs that are garbage
but not yet pruned. The bucket charges for those. `crystalbackup_repository_size_bytes` — the sum
of the objects actually stored under the prefix — is the bill.

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

| Metric | Type | Labels | Description | Status |
|---|---|---|---|---|
| `crystalbackup_externalsync_last_success_timestamp_seconds` | gauge | sync, source, destination, scope, namespace, cluster | Unix time of the last `Completed` sync run. Restart-safe from CR status; drives `ExternalSyncStale`. | **shipped (M5)** |
| `crystalbackup_externalsync_snapshots_copied` | gauge | sync, source, destination, scope, namespace, cluster | Snapshots present at the destination as copies of the source, as of the last completed sync (`status.snapshotsCopied`). | **shipped (M5)** |
| `crystalbackup_externalsync_lag_snapshots` | gauge | sync, source, destination, scope, namespace, cluster | Source snapshots not yet present at the destination (`status.lagSnapshots`). | **shipped (M5)** |
| `crystalbackup_externalsync_failures` | gauge | sync, source, destination, scope, namespace, cluster | External syncs currently in a failed terminal phase (`Failed` or `PartiallyFailed`). | **shipped (M5)** |
| `crystalbackup_externalsync_active` | gauge | sync, source, destination, scope, namespace, cluster | 1 when a sync exists for this pair and is expected to copy, **0** when every sync on the pair is paused, absent when none exists. `schedule_active`'s counterpart, and the join partner `ExternalSyncStale` is guarded on. | **shipped (M6)** |
| `crystalbackup_externalsync_created_timestamp_seconds` | gauge | sync, source, destination, scope, namespace, cluster | Unix time the sync object was created — the instant from which copies started being expected for this pair. The **earliest** creation when several syncs share a pair. `ExternalSyncStale` measures age from it when nothing has ever completed; see the note below. | **shipped (M6)** |
| `crystalbackup_externalsync_duration_seconds` | histogram | sync, source, destination, scope, namespace, cluster | Sync run duration. Same buckets as §2.1. | M6 |
| `crystalbackup_externalsync_bytes_copied_total` | counter | sync, source, destination, scope, namespace, cluster | Bytes streamed to the destination (blob-incremental; S3 egress estimation). | M6 |

**Why gauges rather than the `_total` counters this section first specified.** Every shipped
`crystalbackup_` family is state-derived at scrape time (§1), and this one follows: an operator
restart resets no value and there is no in-process counter to drift. `snapshots_copied` and
`failures` are therefore the CURRENT state, not a monotonic total — the M6 catalogue layers the
counter/histogram variants on top, exactly as it does for the backup and restore families.

**`bytes_copied` is not shipped, and will not be until there is a real number behind it.**
`restic copy --json` emits no machine-readable summary — verified against the pinned restic, not
assumed ([adr/0013](adr/0013-external-backup-sync.md) amendment) — so there is nothing to read a
byte count off. `BackupExternalSync.status.bytesCopied` exists in the API and stays zero rather
than carrying an estimate: a secondary's whole value rests on its numbers being believable, and a
fabricated figure is worse than an absent one.

**Lag is the series that matters.** A last-success timestamp answers "did a sync run", which a
broken sync keeps answering reassuringly right up until it stops running at all. The failure this
family exists to catch is the quiet one: a sync that keeps completing while falling further behind,
because the source produces snapshots faster than the copy moves them. Only `lag_snapshots` shows
that. Note also that a never-completed sync emits `last_success…` as **0, not absent** — unlike the
repository family (§2.4), and deliberately: `time() - metric` over an absent series produces no
alert, so a secondary that never worked from day one would be the one case `ExternalSyncStale`
silently missed.

**That `0` is a sentinel, not a timestamp, and reading it as one was a real bug.** On a real
cluster's clock `time()` is around 1.77 × 10⁹, so `time() - 0` is about fifty-four years. A pure
staleness test therefore crossed a 26 h bound on the **first scrape after any sync was created**,
and the only thing between `kubectl apply` and a page reading "has not completed in 26h" was the
rule's 1 h hold — an alert about an object ninety minutes old whose first copy was not due until
that night. The promtool unit tests were green throughout, because promtool's synthetic clock starts
at the epoch: there the sentinel and "created just now" are the same instant, and the arithmetic
that misbehaves on every real cluster cannot misbehave at all.

The fix is `crystalbackup_externalsync_created_timestamp_seconds` and a rule that reads it, **not** a
rewritten `last_success`. Back-filling the creation time into `last_success` would have closed the
alert bug while lying to every dashboard panel that shows "last successful copy" for a sync that has
copied nothing: `0` means *never*, which is the true and useful thing to display. So the metric goes
on saying it, and `ExternalSyncStale` measures age from the last success when there was one and from
the object's creation when there was not — the same fallback `BackupMissed` makes onto
`schedule_created_timestamp_seconds` (§2.1), against the same class of hole.

**`externalsync_active` closes a defect that was latent from M5 to M6, not a theoretical one.**
`ClusterBackupExternalSync.spec.paused` shipped in M5 and **nothing read it** — not this family, not
the alert rules. `ExternalSyncStale` was a pure staleness test, so the first operator to follow
[docs/DECOMMISSION.md §1.1](../docs/DECOMMISSION.md) and pause a sync before retiring its location
would have been paged 26 hours later, the rule correctly reporting that the thing they had
deliberately stopped had indeed stopped. The value is `0` rather than absent for the same reason
`schedule_active`'s is (§2.1): the guard is `== 1`, so both readings silence the alert, and only the
`0` leaves something behind that says the secondary is deliberately not being maintained. Where
several syncs address one `(source, destination)` pair the value is an **OR**, not a last-one-wins:
if even one of them is still expected to copy, the relationship is still being maintained.

## 3. Alert rules

**Shipped in M6, and no longer written here.** The eleven rules live in a Go table,
`internal/alerts/rules.go`, from which the chart's `PrometheusRule` body is GENERATED into
`charts/crystal-backup/rules/crystalbackup.rules.yaml` (`make alert-rules`). Enable it with
`metrics.rules.enabled=true`; `metrics.rules.labels` sets whatever the Prometheus Operator's
`ruleSelector` matches on, without which the rules install, validate and are completely ignored.

**Why the expressions are not reproduced in this section.** They were, from M1 to 0.5.1, and
**five of the nine referenced series the operator never emitted** — `crystalbackup_backup_failures_total`
against a shipped `crystalbackup_backup_failures`, plus four families that did not exist at all.
Every one was valid PromQL. Every one evaluated without error. None could ever fire, and nothing in
the build could notice, because the rule text and the metric definitions never met. Restating them
here would be re-opening that gap on the first rename. Each expression is now CONCATENATED from the
constants in `internal/metrics/names.go`, so a renamed series is a compile failure;
`internal/alerts/rules_test.go` additionally checks every label, every join key and every
`{{ $labels.x }}` against the label sets the collectors actually register, and proves it can by
failing on seven injected faults.

Read the generated file for the current expressions and thresholds — it carries the reasoning for
each rule as a comment, which is what an operator deciding whether to silence one at 03:00 actually
needs.

| Alert | Severity | For | Fires when |
|---|---|---|---|
| `CrystalbackupBackupMissed` | warning | 15m | An active schedule has gone past **its own** deadline (1.1 × the cron period + 1 h) with no successful `Backup`. Falls back to the schedule's creation time when nothing has ever succeeded, and to a fixed 26 h when the cron expression cannot be parsed. |
| `CrystalbackupBackupFailed` | warning | — | A `Backup` reached `Failed` or `PartiallyFailed` in the last hour — read from the counter with `increase()`, **or** from `crystalbackup_backup_last_failure_timestamp_seconds` being less than an hour old. The second disjunct is what makes the rule survive an operator restart, which the counter alone does not (§1). |
| `CrystalbackupRepositoryCheckFailed` | **critical** | 5m | `restic check` found repository damage. The only critical rule: it is the only one that says the RESTORE PATH is compromised rather than that a backup is late. |
| `CrystalbackupStaleLocks` | warning | 30m | Stale restic locks persist past the reaper's own cycle. |
| `CrystalbackupMaintenanceStalled` | warning | 1h | No successful prune for 26 h. Cannot fire on an `Immutable` location, which emits no series (§2.4). |
| `CrystalbackupDiscoveryFailed` | warning | 30m | Discovery is failing, so `kubectl get backups` no longer matches the repository. |
| `CrystalbackupErasureBlocked` | warning | 1h | A `ClusterErasure` is parked on an object-lock window. Not an error; a GDPR clock. |
| `CrystalbackupPVCSnapshotPileup` | warning | 30m | More than 20 VolumeSnapshots on one PVC (ceph-csi flatten risk, ADR 0006). |
| `CrystalbackupExternalSyncStale` | warning | 1h | No completed external sync in 26 h — measured from the last success, or from the sync's **creation** when there has never been one — **and** the pair still has an unpaused sync (`externalsync_active == 1`, §2.12). |
| `CrystalbackupSchedulePausedTooLong` | warning | 1h | A schedule has been paused for more than **7 days**, on either plane. Cannot fire on a schedule younger than the window, nor on a deleted one. |
| `CrystalbackupExternalSyncPausedTooLong` | warning | 1h | An external sync has been paused for more than **7 days**, on either plane — the secondary has stopped being fed. Same shape and same exclusions as the rule above. |

Tenant-facing alerts carry the `namespace` label for per-tenant routing; repository-level
alerts route to admins for the shared cluster repo (`scope=cluster`) and to the tenant for
a user repo (`scope=namespace`, non-empty `namespace`).

**`BackupMissed` joins on `(namespace, schedule, origin, location, cluster)` — every term, and not
the `(namespace, schedule, cluster)` this section specified until M6.** `schedule` is a CR name, and
a `BackupSchedule` and a `ClusterBackupSchedule` may both be called `daily`; on the narrow key an
unrelated namesake on the other plane answers the "is this schedule still active" guard, so a
paused cluster schedule's breach is retained by the namespace schedule next to it — or silenced by
it. That collision is invisible today only because the namespace plane emits `cluster=""` and the
cluster plane `cluster=<clusterID>`, which is the very gap §1 says is being closed in 0.6: filling
the label in would have activated the bug, silently. The narrow key also fails **now** on a
schedule whose `locationRef` is edited, where the series left behind by the old location keeps a
guard partner it should have lost and pages forever. `tenant` stays out of the join because
`last_success` resolves it from the `Backup`'s label and `schedule_active` from the namespace's —
two sources, and a join on the full set would break precisely while someone is changing one.

**`BackupMissed`'s deadline is derived, the rest are fixed.** A flat 26 h is only ever right for a
daily schedule: an hourly one could be twenty-five hours dead in silence, and a weekly one paged
every Tuesday until somebody silenced it permanently, which is the worst thing that can happen to
an alert. `crystalbackup_schedule_period_seconds` (§2.1) is what makes the deadline follow the cron
expression, so changing a schedule moves its alert with no rule edit. `ExternalSyncStale` keeps its
fixed 26 h because a sync's schedule is optional — a manual sync has no period to derive from
(§8 q3 remains open for that one).

**The two items this section previously listed as blocked are now shipped**, because the series they
waited on now exist — and a third rule came with them, because guarding a rule on a pause is only
half an answer. The count is **eleven**: nine, plus two new rules, with the pause guard modifying an
existing one rather than adding to the total.

- The **pause guard on `ExternalSyncStale`** reads `crystalbackup_externalsync_active` (§2.12). This
  was not a nicety — `ClusterBackupExternalSync.spec.paused` had existed since M5 with nothing
  reading it, so the rule as shipped would have paged an operator 26 h after they paused a sync,
  which is the documented first step of decommissioning a location. The same rule also gained a
  **never-completed fallback** onto `crystalbackup_externalsync_created_timestamp_seconds`, because
  reading the `last_success = 0` sentinel as a timestamp made every freshly created sync page one
  hour after `kubectl apply` (§2.12). A sync created and immediately paused now fires for neither
  reason, which is the case where the two corrections meet.
- **`CrystalbackupSchedulePausedTooLong`** reads `schedule_active`'s new `0` (§2.1). Its expression
  is two terms and the second one is load-bearing three times over: `max_over_time(...[7d]) == 0`
  says the schedule was not active at any point in the window — read from history rather than
  accumulated as a seven-day `for`, which Prometheus resets after an outage longer than its
  restoration tolerance — while `time() - schedule_created > 7d` excludes a schedule too young to
  have a `1` in a seven-day lookback *and*, being an instant vector, requires the schedule to still
  exist, so a deleted one stops alerting immediately instead of trailing its samples for a week.

**Every pause guard needs a paused-too-long companion, and that is a rule about rules.** Guarding an
alert on a pause trades a false page for permanent silence — the right trade for the page and the
wrong one for the operator, since the whole reason `schedule_active` and `externalsync_active` emit
`0` rather than nothing is that a backup tool cannot have a state where silence and health are
indistinguishable. So `CrystalbackupExternalSyncPausedTooLong` ships alongside the guard on
`ExternalSyncStale`, exactly as `SchedulePausedTooLong` ships alongside the one on `BackupMissed`.

The stakes are, if anything, higher on the sync side. A secondary is **insurance**: somebody pauses
replication "while we migrate the bucket", leaves, and nothing anywhere reports that the safety copy
stopped being fed. The silence is indistinguishable from a destination that is perfectly current,
and the difference only becomes visible on the day the primary is gone — which is the worst possible
moment to discover it. `internal/alerts/rules_test.go` enforces the pairing directly: an `_active`
series that silences a rule must also feed one, so a third family growing a pause guard and no
companion fails the build.

**Each rule also declares a `Threshold` and a Go state predicate** that answers the same question by
reading object state instead of Prometheus — the half the exportable self-check consumes, so an
operator with no monitoring stack still gets a verdict. All eleven are implemented, and the
threshold is declared ONCE and read by both: two implementations of one bound diverge, and it is
only a question of which release.

**The predicate hangs off `Rule.Predicate`, and that is the only place it is declared.** For part of
M6 it was declared twice — in the field, and in a separate exported map that landed with the
self-check while the rule table was owned by another change — with a test holding the two together.
That test is the tell: a second declaration site for one thing needs a guard precisely because it
will drift, which is the defect this entire section exists to have ended. There is one site now, and
the self-check reads the field.

Three predicates cannot reproduce their PromQL exactly, and each says so in `alerts.Fidelity()`,
which travels with the verdict into the JSON and onto the rendered page. `BackupFailed` reads a
counter the object graph no longer holds once a failed run is garbage-collected, so it *under*
reports; the two paused-too-long rules ask about metric HISTORY, which object state does not hold at
all, and substitute the `Ready` condition's transition into `reason=Paused` — a reading that can be
older than the real pause, so they *over* report. Both directions are stated rather than smoothed
over: a bare OK printed over a measurement with a blind spot is the one thing a self-check must
never produce.

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
3. **Per-schedule `BackupMissed` deadline** — **DONE in M6 for `BackupMissed`**, still open for
   `ExternalSyncStale`. The threshold is 1.1 × the schedule's own cron period + 1 h, read from
   `crystalbackup_schedule_period_seconds` (§2.1) rather than hardcoded, so it follows a change to
   the cron expression with no rule edit. The proportional factor absorbs jitter and an overrunning
   run; the flat hour keeps a five-minute schedule from getting a five-and-a-half-minute deadline.
   `ExternalSyncStale` keeps the fixed 26 h, and the reason is not laziness: a sync's `schedule` is
   **optional** (a manual sync has no period at all), so the same treatment needs a decision about
   what a manual sync's deadline even means before it needs a series.
