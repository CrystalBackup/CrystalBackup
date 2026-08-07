---
title: "Metrics"
description: "Every crystalbackup_ Prometheus series the operator publishes, with the labels it actually carries — generated from internal/metrics."
tableOfContents:
  minHeadingLevel: 2
  maxHeadingLevel: 3
---

<!-- GENERATED FILE — do not edit. Run `make observability-docs` after changing internal/metrics or internal/alerts. -->

Every series the operator publishes, with the label set it really carries.

This page is generated from `internal/metrics`: the names, the label sets and the
descriptions below are read back out of the `prometheus.Desc` values the collectors
register, and the histogram buckets out of a real exposition. It is the same source a scrape is
served from, so it cannot describe a series that does not exist or a label that is not there.

That matters more than tidiness. A label that is documented and not emitted produces an alert
expression which is valid PromQL, evaluates without error, and matches nothing — forever, in
silence. On a backup tool, silence is indistinguishable from health.

## How to read this page

### Two kinds of series, and only two

**Derived at scrape.** Every gauge here is recomputed from live object state each time
Prometheus scrapes, through the operator's cached client. The operator holds no counter for
them, so restarting it changes nothing: the value after a restart is the value before it, because
both are read from the same objects.

**Event-driven.** The `_total` counters and the histograms are the exception. They are
incremented once at the transition into a terminal phase, after the status write that makes the
transition durable, and they **reset to zero when the operator restarts**. Write expressions over
them with `increase()` or `rate()`, never against the raw value.

The split is exact, and the generator enforces it: every `_total` family and every
histogram is event-driven, and every other family is derived at scrape. If that ever stops being
true, this page fails to regenerate rather than shipping the wrong rule.

Histograms expose the usual `_bucket`, `_sum` and `_count` series.
Their bucket boundaries are printed under each table.

### Absence is a value

A series that is not there does not mean zero. It means *not measured*, and the distinction is
deliberate in several places:

- a repository that has never been verified emits **no** `crystalbackup_repository_last_check_*`
  series — not a zero, and not a 1970 timestamp;
- an Immutable location emits no `crystalbackup_repository_last_maintenance_timestamp_seconds`,
  because it never prunes;
- a schedule whose cron expression cannot be parsed emits no
  `crystalbackup_schedule_period_seconds`.

This is what keeps the shipped alerts quiet about things they have nothing true to say about: a
location created five minutes ago does not page critical, because `== 0` cannot match a
series that is absent. It also means that if you want to alert on *unmeasured*, you have to say so
with `absent()` — nothing else will.

The one deliberate exception is `crystalbackup_externalsync_last_success_timestamp_seconds`,
which publishes `0` rather than nothing for a sync that has never completed. See
[External sync](#external-sync).

### The label contract

Every series carries the `crystalbackup_` prefix. Beyond that, the label sets are per
family — the tables below are the authority, not this list — but the shared names mean the same
thing everywhere:

| Label | Values | Notes |
| --- | --- | --- |
| `namespace` | a namespace name, or empty | Empty on cluster-scoped repository, discovery and external-sync series: those describe the cluster plane, which belongs to no namespace. |
| `tenant` | the namespace's `crystalbackup.io/tenant` label, else the namespace name | One tenant per namespace. It is what you route a tenant-visible alert on. |
| `cluster` | a `ClusterBackupLocation`'s `spec.clusterID`, or empty | So one Prometheus can hold several clusters without collision. **It is resolved only from cluster locations**: a series whose location is a namespaced `BackupLocation`, or a cluster location with no `clusterID` set, carries an empty `cluster` — a gap, deliberately, rather than a borrowed identity. |
| `scope` | `cluster` or `namespace` | **Lowercase, and deliberately not the API enum.** `BackupRepositoryStatus.Scope` is `Cluster;Namespaced`; the label is neither of those spellings. One vocabulary across every family that carries it, matching `origin`. |
| `origin` | `cluster` or `namespace` | Which plane produced the object: a cluster-DR fan-out, or the user plane. |
| `location` | a location name | The repository the data went to. |
| `schedule` | a schedule name, or empty | A CR name, so a `BackupSchedule` and a `ClusterBackupSchedule` may legitimately share one — they are told apart by `origin`, not by this. Empty for a one-off Backup. |
| `result` | `completed`, `partiallycompleted`, `partiallyfailed`, `failed` | A terminal phase, lowercased. The only label whose value is an outcome rather than an identity. |
| `mode` | `Recreate`, `Overwrite`, or empty | A Restore's `spec.mode`, in the API's own casing — unlike `scope`. Empty on an object written before the field was required. |
| `webhook`, `reason` | code-chosen constants | Never request-derived. A reason taken from user input would be an unbounded-cardinality hole. |
| `exposer` | `csi-generic`, `cephfs-shallow` | The SnapshotExposer kind. Comparing the two is the point of the histogram. |
| `pvc` | a PVC name | The single per-PVC label in the catalogue. See [Snapshot exposure and coexistence](#snapshot-exposure-and-coexistence). |

Label *values* are object names and bounded enums. No user-supplied free text becomes a label
value anywhere in this catalogue.

### Reaching the endpoint

Port **8443**, HTTPS, with API-server authentication and authorisation. See
[Observability](/CrystalBackup/docs/guides/observability/) for the ServiceMonitor, the
`crystal-backup-metrics-reader` ClusterRole, and the NetworkPolicy knob.

## Operator identity

Always present, on every operator, from the first scrape. It is the series to check first when `/metrics` looks empty: if this one is missing the scrape is not reaching the operator at all, and if it is the only one there is simply nothing to report yet.

| Series | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `crystalbackup_build_info` | gauge | `version` | A constant 1, labelled with the operator build version; always present. |

## Backup and schedules

Three series carry the word `failure` and none of them is a spelling of another. `crystalbackup_backup_failures` is a gauge counting failed Backup objects that still EXIST, so it drops back to zero when a schedule's history limit deletes them — the question it answers is what wreckage is lying around right now, which is a dashboard question. `crystalbackup_backup_failures_total` is an in-process counter of every Backup that reached a failed phase; read it with `increase()`, and know that an operator restart does not zero it but makes it VANISH (a counter series only exists once something has incremented it), which `increase()` cannot see across. `crystalbackup_backup_last_failure_timestamp_seconds` is the Unix time of the most recent failure, derived from the Backup objects at scrape time and therefore rebuilt intact after a restart; it is ABSENT for a series that has never failed. `CrystalbackupBackupFailed` reads the last two together, because the counter cannot survive a restart and the timestamp cannot survive the object being garbage-collected.

`crystalbackup_schedule_active` is `1` for an unpaused schedule and `0` for one that exists and is paused — it is not absent while paused, which is what makes `CrystalbackupSchedulePausedTooLong` possible at all.

| Series | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `crystalbackup_backup_added_bytes_total` | counter | `namespace` `tenant` `schedule` `origin` `location` `cluster` | Cumulative deduplicated bytes uploaded to the repository by terminal Backups (S3 egress estimation). |
| `crystalbackup_backup_duration_seconds` | histogram | `namespace` `tenant` `schedule` `origin` `location` `cluster` | End-to-end Backup duration, from creation to terminal phase. |
| `crystalbackup_backup_failures` | gauge | `namespace` `tenant` `schedule` `origin` `location` `cluster` | Number of Backups currently in a failed terminal phase (Failed or PartiallyFailed) for this series. |
| `crystalbackup_backup_failures_total` | counter | `namespace` `tenant` `schedule` `origin` `location` `cluster` | Backups that reached Failed or PartiallyFailed. |
| `crystalbackup_backup_in_progress_since_timestamp_seconds` | gauge | `namespace` `tenant` `schedule` `origin` `location` `cluster` | Unix time the oldest still-unfinished Backup for this series was created; absent when none is in flight. |
| `crystalbackup_backup_last_added_bytes` | gauge | `namespace` `tenant` `schedule` `origin` `location` `cluster` | Deduplicated bytes added by the last successful Backup (sum of status.volumes[].addedBytes). |
| `crystalbackup_backup_last_duration_seconds` | gauge | `namespace` `tenant` `schedule` `origin` `location` `cluster` | Wall-clock duration of the last successful Backup (backupTime - creationTimestamp). |
| `crystalbackup_backup_last_failure_timestamp_seconds` | gauge | `namespace` `tenant` `schedule` `origin` `location` `cluster` | Unix time of the last Backup that reached Failed or PartiallyFailed for this series. |
| `crystalbackup_backup_last_size_bytes` | gauge | `namespace` `tenant` `schedule` `origin` `location` `cluster` | Logical size of the last successful Backup (sum of status.volumes[].sizeBytes). |
| `crystalbackup_backup_last_success_timestamp_seconds` | gauge | `namespace` `tenant` `schedule` `origin` `location` `cluster` | Unix time of the last Completed or PartiallyCompleted Backup for this series. |
| `crystalbackup_backup_protected_bytes` | gauge | `namespace` `tenant` `origin` `location` `cluster` | Logical bytes currently protected for the namespace: the newest recorded size of every PVC that has a live restore point. |
| `crystalbackup_backup_total` | counter | `namespace` `tenant` `schedule` `origin` `location` `cluster` `result` | Backups that reached a terminal phase, by result — the per-tenant backup count (R19 accounting input). |
| `crystalbackup_schedule_active` | gauge | `namespace` `tenant` `schedule` `origin` `location` `cluster` | 1 when an unpaused schedule is expected to back up this (namespace, schedule) — a namespaced BackupSchedule, or a ClusterBackupSchedule whose namespace selection matches — and 0 when that schedule exists but is paused. |
| `crystalbackup_schedule_created_timestamp_seconds` | gauge | `namespace` `tenant` `schedule` `origin` `location` `cluster` | Unix time the schedule object was created — the instant from which backups started being expected for this series. |
| `crystalbackup_schedule_period_seconds` | gauge | `namespace` `tenant` `schedule` `origin` `location` `cluster` | Longest gap between two consecutive activations of this schedule's cron expression, in seconds. Absent when the expression cannot be parsed. |

Buckets of `crystalbackup_backup_duration_seconds`, in seconds: `60`, `300`, `900`, `1800`, `3600`, `7200`, `14400`, `28800`.

## Cluster backup runs

Run-level, with no `namespace` label: a ClusterBackup fans out over many namespaces, and its per-namespace detail is in the Backup families above. `namespaces_failed` above zero on a run that otherwise reports success is the signal — the run completed, and some namespace in it did not.

| Series | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `crystalbackup_clusterbackup_duration_seconds` | histogram | `schedule` `location` `cluster` | ClusterBackup run duration, from fan-out start until every child Backup is terminal. |
| `crystalbackup_clusterbackup_last_success_timestamp_seconds` | gauge | `schedule` `location` `cluster` | Unix time of the last Completed ClusterBackup run for this series. |
| `crystalbackup_clusterbackup_namespaces_failed` | gauge | `schedule` `location` `cluster` | Namespaces with a failed child Backup in the last ClusterBackup run for this series (status.namespacesFailed). |
| `crystalbackup_clusterbackup_namespaces_matched` | gauge | `schedule` `location` `cluster` | Namespaces matched by the last ClusterBackup run for this series (status.namespacesMatched). |
| `crystalbackup_clusterbackup_runs_total` | counter | `schedule` `location` `cluster` `result` | ClusterBackup runs that reached a terminal phase, by result (fleet run success ratio). |

Buckets of `crystalbackup_clusterbackup_duration_seconds`, in seconds: `60`, `300`, `900`, `1800`, `3600`, `7200`, `14400`, `28800`.

## Restore

One family covers Restore and ClusterRestore alike. `mode` appears only on the duration histogram and the failure counter, because Recreate and Overwrite fail for entirely different reasons and a merged count hides which one is broken.

| Series | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `crystalbackup_restore_duration_seconds` | histogram | `namespace` `tenant` `origin` `location` `cluster` `mode` | Restore/ClusterRestore duration, from creation to terminal phase. |
| `crystalbackup_restore_failures` | gauge | `namespace` `tenant` `origin` `location` `cluster` | Number of Restores/ClusterRestores currently in a failed terminal phase (Failed or PartiallyFailed) for this series. |
| `crystalbackup_restore_failures_total` | counter | `namespace` `tenant` `origin` `location` `cluster` `mode` | Restores/ClusterRestores that reached Failed or PartiallyFailed. AwaitingConfirmation is not a failure. |
| `crystalbackup_restore_last_restored_bytes` | gauge | `namespace` `tenant` `origin` `location` `cluster` | status.restoredBytes of the last Completed Restore/ClusterRestore for this series. |
| `crystalbackup_restore_last_success_timestamp_seconds` | gauge | `namespace` `tenant` `origin` `location` `cluster` | Unix time of the last Completed Restore/ClusterRestore for this series. |

Buckets of `crystalbackup_restore_duration_seconds`, in seconds: `60`, `300`, `900`, `1800`, `3600`, `7200`, `14400`, `28800`.

## Repository

This is the family where absence carries the most meaning. A repository that has never been verified emits no `last_check_*` series at all rather than a zero, and an Immutable location — which never prunes, by design — emits no `last_maintenance_timestamp_seconds`. Both absences are what keep `CrystalbackupRepositoryCheckFailed` and `CrystalbackupMaintenanceStalled` silent on repositories they have nothing true to say about.

There is no separate "stored bytes" series. `restic stats --mode raw-data` is not something the operator runs, so the only number available would have been the one `crystalbackup_repository_size_bytes` already publishes, and two names for one reading invite a reader to reason about a gap that does not exist.

| Series | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `crystalbackup_repository_last_check_success` | gauge | `location` `scope` `namespace` `cluster` | 1 if the last restic check passed, 0 if it found repository damage. Drives RepositoryCheckFailed. |
| `crystalbackup_repository_last_check_timestamp_seconds` | gauge | `location` `scope` `namespace` `cluster` | Unix time of the last completed restic check, whether it passed or failed (status.lastCheckTime). |
| `crystalbackup_repository_last_maintenance_timestamp_seconds` | gauge | `location` `scope` `namespace` `cluster` | Unix time of the last SUCCESSFUL prune (status.lastMaintenanceTime). Absent on Immutable locations, which never prune. |
| `crystalbackup_repository_locks_reaped_total` | counter | `location` `scope` `namespace` `cluster` | Stale repository locks removed by an unlock operation. |
| `crystalbackup_repository_size_bytes` | gauge | `location` `scope` `namespace` `cluster` | Physical size of the repository in object storage, post-dedup and post-compression (status.approximateSizeBytes). |
| `crystalbackup_repository_snapshot_count` | gauge | `location` `scope` `namespace` `cluster` | Snapshots present in the repository (status.snapshotCount). |
| `crystalbackup_repository_stale_locks` | gauge | `location` `scope` `namespace` `cluster` | Repository lock objects older than restic's 30-minute staleness threshold (status.staleLocks). |

## Discovery

Discovery is the repository→Backup projection: what `kubectl get backups` lists comes from here. `orphan_snapshots` above zero is not an error — it is DR data for namespaces that no longer exist, restorable through ClusterRestore.

| Series | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `crystalbackup_discovery_last_success` | gauge | `location` `scope` `namespace` `cluster` | 1 if the last discovery scan projected the whole repository cleanly, 0 if the listing failed or some groups could not be reconciled. Drives DiscoveryFailed. |
| `crystalbackup_discovery_last_timestamp_seconds` | gauge | `location` `scope` `namespace` `cluster` | Unix time the last discovery scan of this repository completed (status.lastDiscoveryTime). |
| `crystalbackup_discovery_orphan_snapshots` | gauge | `location` `scope` `namespace` `cluster` | Snapshot (namespace, run) groups whose namespace no longer exists. Non-zero is DR data for gone namespaces, restorable only via ClusterRestore — not an error. |
| `crystalbackup_discovery_projected_backups` | gauge | `location` `scope` `namespace` `cluster` | Snapshot (namespace, run) groups projected into existing namespaces by the last scan — what kubectl get backups lists for this repository. |

## Right to erasure

`crystalbackup_erasure_blocked` above zero is also not an error: it is an erasure waiting, correctly, on an object-lock window that exists to make deletion impossible. It is worth an alert anyway, because a right-to-erasure request parked for weeks is a compliance clock running regardless of whose fault it is.

| Series | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `crystalbackup_erasure_blocked` | gauge | `location` `cluster` | ClusterErasure objects currently Blocked on this location (Immutable object-lock not yet expired). Drives ErasureBlocked. |
| `crystalbackup_erasure_last_completion_timestamp_seconds` | gauge | `location` `cluster` | Unix time of the last Completed erasure on this location. |
| `crystalbackup_erasure_reclaimed_bytes_total` | counter | `location` `cluster` | Bytes physically reclaimed by the prune of a completed ClusterErasure. |
| `crystalbackup_erasure_snapshots_forgotten_total` | counter | `location` `cluster` | Snapshots removed by a completed ClusterErasure (restic forget --tag). |

## Movers, concurrency and queueing

A census of the mover Jobs in the operator's own namespace. `queue_depth` is work admitted by a controller with no Job running yet: sustained above zero while `active` sits at `concurrency_limit` means the limit, not the storage, is what backups are waiting on. A `concurrency_limit` of `0` means unlimited.

| Series | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `crystalbackup_mover_active` | gauge | `cluster` | Mover Jobs currently occupying a concurrency slot (backup and restore alike). |
| `crystalbackup_mover_concurrency_limit` | gauge | `cluster` | Configured maxConcurrentMovers across the live cluster-plane runs and schedules. 0 means unlimited. |
| `crystalbackup_mover_job_retries_total` | counter | `namespace` `tenant` `cluster` | Mover pod retries consumed against the Job's backoffLimit. |
| `crystalbackup_mover_queue_depth` | gauge | `cluster` | Backup volumes admitted by a controller that have no mover Job running yet — work waiting on the maxConcurrentMovers semaphore. |

## Admission

The operator's own dynamic webhook only. Denials from the static ValidatingAdmissionPolicies are counted by the apiserver's own admission metrics and never appear here, so a zero on this series does not mean nothing was rejected. See [Admission rules](/CrystalBackup/docs/reference/admission/) for which check lives where.

| Series | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `crystalbackup_webhook_denials_total` | counter | `webhook` `reason` | Requests denied by the operator's dynamic admission webhook. Static-rule (VAP) denials are counted by the apiserver, not here. |

## Snapshot exposure and coexistence

`crystalbackup_pvc_volumesnapshot_count` is the one family carrying a per-PVC label, and it is a deliberate exception: its cardinality is bounded by the number of live PVCs, and the pile-up it detects is worth that cost. It counts VolumeSnapshots from **every** tool, an incumbent backup product's included — the ceph-csi flatten thresholds do not care who created the snapshot.

| Series | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `crystalbackup_exposure_ready_wait_seconds` | histogram | `namespace` `tenant` `exposer` `cluster` | Wait from snapshot exposure start until the exposed PVC is bound and a mover can start. |
| `crystalbackup_pvc_volumesnapshot_count` | gauge | `namespace` `pvc` `cluster` | VolumeSnapshot objects per source PVC, from ALL tools (an incumbent's included) — the ceph-csi flatten-threshold signal during coexistence. |

Buckets of `crystalbackup_exposure_ready_wait_seconds`, in seconds: `1`, `5`, `15`, `30`, `60`, `120`, `300`, `600`.

## External sync

The one family that deliberately breaks the absence rule above: a sync that has never completed publishes `last_success_timestamp_seconds` as **0**, not as nothing, so that a secondary broken from the day it was created is visible rather than being the one case an alert cannot see. Treat that `0` as a sentinel, never as a timestamp — `time() - 0` is fifty-odd years, and any staleness expression written against it needs the `> 0` guard the shipped rule uses.

There is no bytes-copied counter. `restic copy --json` emits no machine-readable summary, so there is no byte count to publish, and an estimate presented as a measurement is worse for a secondary than an honest gap.

| Series | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `crystalbackup_externalsync_active` | gauge | `sync` `source` `destination` `scope` `namespace` `cluster` | 1 when a sync exists for this pair and is expected to copy, 0 when every sync on the pair is paused. |
| `crystalbackup_externalsync_created_timestamp_seconds` | gauge | `sync` `source` `destination` `scope` `namespace` `cluster` | Unix time the external-sync object was created — the instant from which copies started being expected for this pair. |
| `crystalbackup_externalsync_duration_seconds` | histogram | `sync` `source` `destination` `scope` `namespace` `cluster` | External sync run duration (restic copy), from run start to terminal phase. |
| `crystalbackup_externalsync_failures` | gauge | `sync` `source` `destination` `scope` `namespace` `cluster` | External syncs currently in a failed terminal phase (Failed or PartiallyFailed) for this series. |
| `crystalbackup_externalsync_lag_snapshots` | gauge | `sync` `source` `destination` `scope` `namespace` `cluster` | Source snapshots NOT yet at the destination, as of the last completed sync. Zero is the property a secondary exists for. |
| `crystalbackup_externalsync_last_success_timestamp_seconds` | gauge | `sync` `source` `destination` `scope` `namespace` `cluster` | Unix time of the last Completed external sync for this series. |
| `crystalbackup_externalsync_snapshots_copied` | gauge | `sync` `source` `destination` `scope` `namespace` `cluster` | Snapshots present at the destination as copies of the source, as of the last completed sync. |

Buckets of `crystalbackup_externalsync_duration_seconds`, in seconds: `60`, `300`, `900`, `1800`, `3600`, `7200`, `14400`, `28800`.

## What these metrics are not for

The per-tenant series exist so a platform team can see who is generating what. The operator does
no accounting and no billing with them, and there is **no quota mechanism anywhere in the tool** —
a namespace generating far more data than anyone expected is visible, not bounded.

Deduplicated bytes per tenant are best-effort. restic deduplicates at the *repository* level, so
attributing the saving to one namespace is an estimate by construction; the exact number is the
repository total in `crystalbackup_repository_size_bytes`.

And none of this tells you a restore works. `restic check` verifies that a repository is
readable, which is not the same claim as an application coming back up. Restore drills are the
administrator's job, on a real cadence — see
[Maintenance and verification](/CrystalBackup/docs/guides/maintenance/#restore-drills-are-yours).

## See also

- [Alerts](/CrystalBackup/docs/reference/alerts/) — the twelve rules built on these series.
- [Observability](/CrystalBackup/docs/guides/observability/) — scraping, logs, and the conditions
  that say *why*.
