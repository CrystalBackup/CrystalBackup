---
title: "Alerts"
description: "The alert rules shipped in the chart: what each one fires on, and why its threshold is the number it is — generated from internal/alerts."
tableOfContents:
  minHeadingLevel: 2
  maxHeadingLevel: 3
---

<!-- GENERATED FILE — do not edit. Run `make observability-docs` after changing internal/metrics or internal/alerts. -->

12 alert rules ship with the chart. This page is generated from
`internal/alerts`, so every expression, threshold and annotation below is the one the
chart actually installs — not a transcription of it.

Each rule's expression is assembled in Go from the series-name constants the collectors use, which
is why they can be trusted to *match something*. Before that was true, five of the nine rules this
table replaced read series the operator has never emitted: valid PromQL, evaluated without error,
unable to fire, and invisible to every check in the build.

## Turning them on

```yaml
metrics:
  rules:
    enabled: true
    labels:
      release: kube-prometheus-stack   # must match your Prometheus' ruleSelector
```

They are **off by default**, like the ServiceMonitor, for two reasons: they need the
`monitoring.coreos.com` CRDs, and thresholds are platform policy. Turn them on
deliberately, having read what they will page you about.

The chart installs them as a single `PrometheusRule` for the Prometheus Operator, in one
group named `crystalbackup`. Two knobs matter more than they look:

- `metrics.rules.labels` — a Prometheus Operator only picks up rules matching its own
  `ruleSelector`. An unlabelled PrometheusRule installs cleanly, validates, and is
  completely ignored.
- `metrics.rules.namespace` — set it when the operator's `ruleNamespaceSelector`
  only covers the monitoring namespace. Empty means the operator's own namespace.

If you do not run the Prometheus Operator, the rule bodies are a plain rule group in the chart at
`rules/crystalbackup.rules.yaml`. `helm show` it, or read it in the repository,
and load it into Prometheus however you normally do.

## Routing by tenant

Every rule that can name a tenant carries `namespace` and `tenant` labels
through to the alert, and the repository, discovery and external-sync rules carry
`scope` as well. That is what a per-tenant route matches on:

```yaml
routes:
  - matchers: [ 'alertname=~"Crystalbackup.*"', 'scope="namespace"', 'namespace=~"team-.*"' ]
    receiver: tenant-oncall
  - matchers: [ 'alertname=~"Crystalbackup.*"' ]
    receiver: platform-oncall
```

Order matters: cluster-plane series carry an empty `namespace` and
`scope="cluster"`, so the tenant route must be the specific one and the platform route
the catch-all. See the [label contract](/CrystalBackup/docs/reference/metrics/#the-label-contract)
for what each label can hold — in particular that `scope` is lowercase and is *not* the
API's `Cluster|Namespaced` enum.

## These expressions have been run

Every rule below has promtool unit tests under `internal/alerts/testdata/`, run in CI by
`make test-alert-rules`. Each one is fed synthetic series and asserted in both
directions: a case that makes it fire, and a case just under its threshold — or just inside its
`for` hold — that must stay silent. The absence cases are tested too, because "a
repository that has never been checked must not page" is a property of the collector and the rule
*together*, and only an evaluation can hold them to it.

A rule with no test fails the build rather than passing by not being mentioned. That is enforced
separately, by `make alert-rules-covered`.

## The rules at a glance

| Alert | Severity | `for` | Fires when |
| --- | --- | --- | --- |
| [CrystalbackupBackupMissed](#crystalbackupbackupmissed) | warning | `15m` | nothing has happened for 1.1 × the schedule's own period + 1h (falling back to 26h) |
| [CrystalbackupBackupFailed](#crystalbackupbackupfailed) | warning | none — fires on the first evaluation | the measured value goes above 0 |
| [CrystalbackupBackupStalled](#crystalbackupbackupstalled) | warning | `30m` | nothing has happened for 8h |
| [CrystalbackupRepositoryCheckFailed](#crystalbackuprepositorycheckfailed) | **critical** | `5m` | a state gauge reports the bad state (no numeric bound) |
| [CrystalbackupStaleLocks](#crystalbackupstalelocks) | warning | `30m` | the measured value goes above 0 |
| [CrystalbackupMaintenanceStalled](#crystalbackupmaintenancestalled) | warning | `1h` | nothing has happened for 26h |
| [CrystalbackupDiscoveryFailed](#crystalbackupdiscoveryfailed) | warning | `30m` | a state gauge reports the bad state (no numeric bound) |
| [CrystalbackupErasureBlocked](#crystalbackuperasureblocked) | warning | `1h` | the measured value goes above 0 |
| [CrystalbackupPVCSnapshotPileup](#crystalbackuppvcsnapshotpileup) | warning | `30m` | the measured value goes above 20 |
| [CrystalbackupExternalSyncStale](#crystalbackupexternalsyncstale) | warning | `1h` | nothing has happened for 26h |
| [CrystalbackupSchedulePausedTooLong](#crystalbackupschedulepausedtoolong) | warning | `1h` | nothing has happened for 7d |
| [CrystalbackupExternalSyncPausedTooLong](#crystalbackupexternalsyncpausedtoolong) | warning | `1h` | nothing has happened for 7d |

## CrystalbackupBackupMissed

**Severity** warning · **`for`** `15m` · **Threshold** nothing has happened for 1.1 × the schedule's own period + 1h (falling back to 26h)

> No successful backup for {{ $labels.namespace }}/{{ $labels.schedule }} ({{ $labels.origin }}) within its schedule's deadline

**What to do.** The schedule is active and its deadline has passed with no successful Backup. Check the namespace's Backup objects for a failed or stuck run, and the mover Jobs in the operator namespace.

```promql
(
  (
    (
      (time() - crystalbackup_backup_last_success_timestamp_seconds)
      or on (namespace, schedule, origin, location, cluster)
      (time() - crystalbackup_schedule_created_timestamp_seconds)
    )
    > on (namespace, schedule, origin, location, cluster) group_left()
      (crystalbackup_schedule_period_seconds * 1.1 + 3600)
  )
  or
  (
    ((
      (time() - crystalbackup_backup_last_success_timestamp_seconds)
      or on (namespace, schedule, origin, location, cluster)
      (time() - crystalbackup_schedule_created_timestamp_seconds)
    ) > 93600)
    unless on (namespace, schedule, origin, location, cluster)
      crystalbackup_schedule_period_seconds
  )
)
and on (namespace, schedule, origin, location, cluster)
  (crystalbackup_schedule_active == 1)
```

:::note[Why this threshold]
The deadline is the schedule's OWN period (1.1x + 1h), not a fixed 26h: a flat threshold is silent for a day on an hourly schedule and pages every week on a weekly one. Three parts, and each closes a hole the previous shape had:

- the age term falls back to `crystalbackup_schedule_created_timestamp_seconds` when nothing has ever succeeded — a schedule that was broken from the day it was applied emits no last_success series at all, and `time() - <nothing>` is nothing;
- the second branch keeps the old fixed deadline for a schedule whose cron cannot be parsed (no period series), because that schedule will never run and must still page;
- the `and` guard on `crystalbackup_schedule_active` is what stops a deleted or paused schedule from paging forever on a last_success that will never advance again.
:::

## CrystalbackupBackupFailed

**Severity** warning · **`for`** none — fires on the first evaluation · **Threshold** the measured value goes above 0

> Backup failed for {{ $labels.namespace }}/{{ $labels.schedule }}

**What to do.** At least one Backup reached Failed or PartiallyFailed in the last hour. kubectl get backups -n {{ $labels.namespace }} shows which.

```promql
increase(crystalbackup_backup_failures_total[3600s]) > 0
or (time() - crystalbackup_backup_last_failure_timestamp_seconds < 3600)
```

:::note[Why this threshold]
Two disjuncts, because the counter alone was measured to be SILENT on a real failure. The first is the ordinary reading, and increase() over an hour is also why there is no `for` hold: the range IS the hold. It is also, alone, unable to page the FIRST failure a series ever records — no restart required. A CounterVec child materialises AT ONE (RecordBackupTerminal touches it only on the failure branch, so nothing pre-registers a zero), and a window whose samples all read 1 holds no rise for increase() to measure. Only a SECOND failure moves it. That is the widest form of this defect and the likeliest incident in production: a namespace that was fine yesterday and failed once tonight. The second disjunct also exists because an operator restart does not RESET `crystalbackup_backup_failures_total` to zero — it makes the series DISAPPEAR. The same materialisation rule says a fresh process publishes no such series at all until something fails again, and increase() cannot see across a disappearance the way it sees across a reset. Measured on a live cluster: after the operator pod was replaced the counter returned ZERO series, the increase() returned 0, and a Backup had genuinely failed. Nothing fired. `crystalbackup_backup_last_failure_timestamp_seconds` is derived from the Backup objects at scrape time, so it is restart-safe by construction, and it is ABSENT for a series that has never failed — which is what keeps this rule silent on a healthy install rather than merely below a bound. The recency test is what stops it paging forever. The plain gauge `crystalbackup_backup_failures` survives a restart too, but it has no notion of WHEN: a Backup kept for diagnosis after failing three days ago would page every evaluation until somebody deleted it. RESIDUAL BLIND SPOT, accepted rather than hidden: if the operator restarts AND the failed Backup object is garbage-collected by its schedule's history limit inside the same hour, the counter series is gone and no object is left to derive a timestamp from. That failure is unrecoverable from either source and this rule will not fire for it. The remedy is a history limit greater than one, not a third disjunct. AND ITS MILDER FORM, measured on the crucible rather than reasoned about: deleting the Backup object RESOLVES this alert, even with the operator up, because on a first failure the counter contributes nothing and the timestamp is all there is. In the 0.6.0 run the lane's own teardown removed the namespace twelve seconds after the Backup went terminal, and the alert fired for exactly thirty seconds before resolving. That is correct — no wreckage, no page — but it means this rule's firing window is the OBJECT'S lifetime, not the hour its name implies. In production a retained failure outlives the window and the distinction never shows; with a history limit of one and a success right behind the failure, the page can be shorter than an operator's notification pipeline. Alertmanager's group_wait is the thing to check before blaming the rule.
:::

:::caution[The offline self-check answers this one approximately]
Derived from Backup objects that still exist. The alert's first disjunct reads a COUNTER (increase over 1h), which survives the schedule history limit deleting a failed run; this predicate cannot, and a failure already garbage-collected is not counted here. Its second disjunct is derived from the same objects this predicate reads, so on that half the two agree exactly — including the blind spot they share, which is precisely the deleted run.
:::

## CrystalbackupBackupStalled

**Severity** warning · **`for`** `30m` · **Threshold** nothing has happened for 8h

> Backup {{ $labels.namespace }}/{{ $labels.schedule }} has been running for over 8h without finishing

**What to do.** A run that never ends is invisible to every other rule here: nothing failed, so BackupFailed is silent, and last_success has not gone stale yet so BackupMissed is too. kubectl get backup -n {{ $labels.namespace }} -o wide shows the per-volume phases; the reason on a volume names the cause, and for one stuck in Uploading, kubectl describe pod on its mover Job in the operator namespace has the kubelet's account.

```promql
time() - crystalbackup_backup_in_progress_since_timestamp_seconds > 28800
```

:::note[Why this threshold]
THE INCIDENT. On 0.6.2 a nightly cascade left six movers whose pods could not mount their temp clone PVC in ContainerCreating for thirty-six hours, and four more namespaces in Snapshotting beside them. Nothing failed, so none of the eleven rules in this table could fire — every one of them watches for a FAILURE. concurrencyPolicy: Forbid then meant no further nightly ran at all: one backup in fifteen days, with a green dashboard. A stall is not a failure and it is not a missed schedule. It needs its own series and its own rule, and `crystalbackup_backup_in_progress_since_timestamp_seconds` is that series. It is STATE-DERIVED, recomputed from the Backup objects on every scrape, and that is load-bearing rather than incidental. The lesson is written out at length under CrystalbackupBackupFailed above: a CounterVec child materialises AT ONE, so a counter cannot page on a first occurrence, and after an operator restart it does not reset — it DISAPPEARS. A stall is a first occurrence by definition, and the operator restarting is one of the things that can cause one, so a counter was never an option here. The series is ABSENT when nothing is in flight, which is what keeps this silent on a healthy cluster rather than merely below a bound — the same absence discipline last_failure follows, and for the same reason: a published 0 would make time()-0 fifty-four years and page the whole fleet forever. It reports the OLDEST unfinished Backup of the series, so tonight's fresh run cannot reset the clock on last night's wedged one. WHY THE BOUND IS SO LOOSE: see backupStalledThreshold. The tight, provable deadlines live in the controller (a mover pod that never reached Running at 30m; a snapshot acknowledged and never ready at 2h) and end in a Failed volume that BackupFailed pages on. This rule catches what those cannot prove, including a Backup gated Pending forever and a mover that is running and wedged, and it accepts firing on a genuinely enormous first full to do it.
:::

## CrystalbackupRepositoryCheckFailed

**Severity** **critical** · **`for`** `5m` · **Threshold** a state gauge reports the bad state (no numeric bound)

> restic check failed on repository {{ $labels.location }} ({{ $labels.scope }})

**What to do.** restic found repository damage (R17). Restores from this repository may not work. Do not prune it until the check passes: inspect BackupRepository status.recentMaintenance.

```promql
crystalbackup_repository_last_check_success == 0
```

:::note[Why this threshold]
The only critical rule here, because it is the only one that says the RESTORE PATH is compromised rather than that a backup is late. A repository that has never been checked emits no series at all (spec §2.4), so this stays silent on a fresh location instead of paging the moment one is created.
:::

## CrystalbackupStaleLocks

**Severity** warning · **`for`** `30m` · **Threshold** the measured value goes above 0

> Stale restic locks persist on {{ $labels.location }} (reaper not clearing)

**What to do.** Stale locks block backups and maintenance on this repository. The orphan reaper normally clears them within minutes; 30 minutes of persistence means it is not running or cannot reach the backend.

```promql
crystalbackup_repository_stale_locks > 0
```

:::note[Why this threshold]
The 30m hold is the reaper's own budget: a lock is only stale after 30 minutes by restic's definition, and the reaper gets a full cycle to clear it before this says anything.
:::

## CrystalbackupMaintenanceStalled

**Severity** warning · **`for`** `1h` · **Threshold** nothing has happened for 26h

> No successful prune on {{ $labels.location }} for over 26h — the repository is growing unreclaimed

**What to do.** Forgotten snapshots are not being reclaimed, so the repository grows and the bill with it. Check the maintenance Job history in BackupRepository status.recentMaintenance.

```promql
time() - crystalbackup_repository_last_maintenance_timestamp_seconds > 93600
```

:::note[Why this threshold]
A prune that keeps FAILING never advances lastMaintenanceTime, so staleness covers 'never ran', 'erroring every night' and 'controller wedged' in a single rule. 26h lets a daily pruneSchedule miss one window without paging. An Immutable location never prunes by design and emits no series at all (spec §2.4, adr/0005), so it cannot fire here — the absence is what makes that work, not a label exclusion this rule would have to remember to keep.
:::

## CrystalbackupDiscoveryFailed

**Severity** warning · **`for`** `30m` · **Threshold** a state gauge reports the bad state (no numeric bound)

> Discovery failing on {{ $labels.location }} — Backup projections may be stale vs the repository

**What to do.** The repository is the source of truth (adr/0009). While discovery fails, `kubectl get backups` no longer reflects what is actually restorable.

```promql
crystalbackup_discovery_last_success == 0
```

:::note[Why this threshold]
Tenant-visible: at scope=namespace with a non-empty namespace this routes to the tenant, who is the one whose list of restore points has silently gone stale (spec §2.5).
:::

## CrystalbackupErasureBlocked

**Severity** warning · **`for`** `1h` · **Threshold** the measured value goes above 0

> Right-to-erasure blocked on {{ $labels.location }} (Immutable object-lock not yet expired, R21/ADR 0005)

**What to do.** A ClusterErasure cannot complete until the object-lock window expires. This is expected on Immutable locations, but it is a GDPR clock running: someone has to know.

```promql
crystalbackup_erasure_blocked > 0
```

:::note[Why this threshold]
Not an error: the erasure is waiting, correctly, on a lock that exists to make deletion impossible. It is an alert because a right-to-erasure request quietly parked for weeks is a compliance problem regardless of whose fault it is.
:::

## CrystalbackupPVCSnapshotPileup

**Severity** warning · **`for`** `30m` · **Threshold** the measured value goes above 20

> {{ $value }} VolumeSnapshots piling up on PVC {{ $labels.namespace }}/{{ $labels.pvc }} (ceph-csi flatten risk, ADR 0006)

**What to do.** Snapshots are accumulating faster than they are released — usually a coexisting backup tool, or our own temp snapshots not being cleaned up. On ceph-csi this crosses the flatten threshold and stalls the volume.

```promql
crystalbackup_pvc_volumesnapshot_count > 20
```

:::note[Why this threshold]
The one family carrying a per-PVC label, and the documented exception to the no-per-PVC-label rule (spec §2.9): cardinality is bounded by the live PVC count, and this alert is the reason that cost is worth paying.
:::

## CrystalbackupExternalSyncStale

**Severity** warning · **`for`** `1h` · **Threshold** nothing has happened for 26h

> External sync {{ $labels.sync }} ({{ $labels.source }}→{{ $labels.destination }}) has not completed in 26h

**What to do.** The secondary copy is falling behind. Check the sync CR's phase and the queue: a secondary nobody has verified is not a secondary.

```promql
(
  (
    (time() - (crystalbackup_externalsync_last_success_timestamp_seconds > 0))
    or on (sync, source, destination, scope, namespace, cluster)
    (time() - crystalbackup_externalsync_created_timestamp_seconds)
  ) > 93600
)
and on (sync, source, destination, scope, namespace, cluster)
  (crystalbackup_externalsync_active == 1)
```

:::note[Why this threshold]
A never-completed sync emits last_success as 0, NOT absent (spec §2.12, deliberately unlike the repository family), so a secondary that never worked from day one fires here instead of being the one case the rule misses. The 26h stays fixed, unlike BackupMissed's: a sync's schedule is optional, so a manual sync has no period to derive a deadline from. The `or` branch onto `crystalbackup_externalsync_created_timestamp_seconds` is not decoration. The 0 above is a SENTINEL, and on a real clock time()-0 is fifty-four years — so without this branch the rule was past its 26h bound on the first scrape after ANY sync was created, and only the 1h hold stood between `kubectl apply` and a page claiming a ninety-minute-old sync had not completed in 26h. Age is measured from the last success when there was one, and from the object's creation when there was not. The `and` guard on `crystalbackup_externalsync_active` closes a defect that was latent rather than theoretical: spec.paused has existed on ClusterBackupExternalSync since M5 and nothing read it, so pausing a sync — the documented first step of decommissioning a location (docs/DECOMMISSION.md §1.1) — would have paged 26h later, about the thing the operator had just deliberately stopped.
:::

## CrystalbackupSchedulePausedTooLong

**Severity** warning · **`for`** `1h` · **Threshold** nothing has happened for 7d

> Schedule {{ $labels.namespace }}/{{ $labels.schedule }} ({{ $labels.origin }}) has been paused for over 7 days — nothing is backing this namespace up

**What to do.** A paused schedule protects nothing, and no other alert can say so: BackupMissed is guarded on the same pause. Unpause it (spec.paused=false), or delete it if the namespace is genuinely no longer being protected — an intent nobody has to re-check in a month.

```promql
(
  max_over_time(crystalbackup_schedule_active[604800s]) == 0
)
and on (namespace, schedule, origin, location, cluster)
  (time() - crystalbackup_schedule_created_timestamp_seconds > 604800)
```

:::note[Why this threshold]
This rule exists because of the blind spot the M6 semantics change opened up and then closed. A paused schedule used to emit NO `crystalbackup_schedule_active` series at all, which suppressed BackupMissed correctly and made the pause itself unobservable: someone suspends a schedule 'for the migration', the migration ends, nobody unpauses, and the namespace is unprotected while every rule and every dashboard reports perfect health. For a backup tool, silence that is indistinguishable from safety is the one failure mode that must not exist. Two terms, and the second is doing three jobs at once:

- max_over_time == 0 over the window means the schedule was not active at ANY point in it — robust to an operator restart in a way a 7d `for` would not be, since it reads history rather than accumulating pending state;
- the age term excludes a schedule younger than the window, which would otherwise fire simply because there is no `1` in a lookback longer than its life;
- and because `crystalbackup_schedule_created_timestamp_seconds` is an instant vector, that same term requires the schedule to still EXIST. A deleted schedule leaves samples in the lookback window for a week; without this it would keep alerting about an object that is gone.

Deleting a schedule is a decision someone made. Leaving one paused is usually a decision nobody finished making, and that is the difference this alert is about.

**CAVEAT:** reading history means the lookback is only as long as your retention. Under 7d of retention max_over_time sees a shorter window than it asks for, and a schedule that was active six days ago can look like it never was. Raise retention or raise this threshold; do not silence the rule.
:::

:::caution[The offline self-check answers this one approximately]
The alert asks whether the schedule was active at any point in a 7-day lookback, which is a question about metric HISTORY and has no equivalent in object state. This predicate substitutes the Ready condition's transition into reason=Paused, falling back to the schedule's creation. That reading can be older than the actual pause (a schedule already not-Ready for another reason does not re-transition when paused), so it reports a pause as longer than it was, never shorter.
:::

## CrystalbackupExternalSyncPausedTooLong

**Severity** warning · **`for`** `1h` · **Threshold** nothing has happened for 7d

> External sync {{ $labels.sync }} ({{ $labels.source }}→{{ $labels.destination }}) has been paused for over 7 days — the secondary is no longer being fed

**What to do.** The destination has stopped receiving copies and ExternalSyncStale is guarded on the same pause, so nothing else will say so. Resume it (spec.paused=false), or delete the sync if this secondary is genuinely being retired.

```promql
(
  max_over_time(crystalbackup_externalsync_active[604800s]) == 0
)
and on (sync, source, destination, scope, namespace, cluster)
  (time() - crystalbackup_externalsync_created_timestamp_seconds > 604800)
```

:::note[Why this threshold]
SchedulePausedTooLong's twin, and it exists because the pause guard added to ExternalSyncStale in this same lot re-opened, for syncs, precisely the blind spot that rule closes for schedules: with the guard in place, pausing a sync is silent forever. The stakes are arguably higher here than on a schedule. A secondary is INSURANCE. Somebody pauses replication 'while we migrate the bucket', leaves, and nothing anywhere reports that the safety copy stopped being fed — the silence is indistinguishable from a destination that is perfectly up to date, and the difference only becomes visible on the day the primary is gone, which is the worst possible moment to find out. Same shape as its twin, same three jobs in the second term (see pausedTooLongExpr), and the same retention caveat: max_over_time cannot look further back than your Prometheus keeps.
:::

:::caution[The offline self-check answers this one approximately]
The alert asks whether the pair had an unpaused sync at any point in a 7-day lookback, which is a question about metric HISTORY and has no equivalent in object state. This predicate substitutes the Ready condition's transition into reason=Paused, falling back to the sync's creation. That reading can be older than the actual pause (a sync already not-Ready for another reason — a missing location, an unreachable endpoint — does not re-transition when paused), so it reports a pause as longer than it was, never shorter.
:::

## What no alert here can tell you

None of these verify that a **restore works**. `restic check` verifies that a repository
is readable; it does not verify that restoring it produces a working application, and no expression
over these series ever will.

Restore drills are the administrator's job, on a real cadence. See
[Maintenance and verification](/CrystalBackup/docs/guides/maintenance/#restore-drills-are-yours).

## See also

- [Metrics](/CrystalBackup/docs/reference/metrics/) — the series these rules read, and what the
  absence of one means.
- [Helm values](/CrystalBackup/docs/reference/helm-values/) — every `metrics.rules.*` knob.
- [Observability](/CrystalBackup/docs/guides/observability/) — scraping, logs, and the conditions
  that say *why*.
