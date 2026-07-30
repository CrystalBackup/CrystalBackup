---
title: Maintenance and verification
description: Retention, prune, restic check, the exclusive queue, and re-keying a repository.
sidebar:
  order: 7
---

Three operations keep a repository healthy: `forget` reclaims by policy, `prune` reclaims
physically, and `check` tells you whether what is in there is still readable. Only the
third is verification; the first two are housekeeping.

## Retention lives on the location

```yaml
spec:
  retention:
    keepLast: 3
    keepHourly: 24
    keepDaily: 7
    keepWeekly: 4
    keepMonthly: 6
    keepYearly: 2
```

Those six fields are the whole vocabulary. There is no `keepWithinDuration`.

It is on the **location**, not on schedules or runs, because `restic forget` operates on
the whole repository and one location backs exactly one repository. A single authoritative
policy per location is the only arrangement in which two schedules cannot fight over the
same snapshots.

It is applied **per PVC** — grouped by restic `host,paths` — after each successful backup,
enqueued on the repository's maintenance queue rather than run inline. A run finishing does
not wait for its own retention pass.

An all-zero policy is a safe no-op: nothing is forgotten.

On an `Immutable` location retention is reported ignored, with a `RetentionIgnored`
condition on the location. Object Lock governs expiry there instead.

## Prune

`forget` removes snapshot references. `prune` removes the data those snapshots were the
last holders of. Until you prune, forgetting frees nothing.

```yaml
spec:
  maintenance:
    pruneSchedule: "0 3 * * 0"
    pruneMaxRepackSize: "50G"
    checkSchedule: "0 4 * * 0"
    checkReadDataSubset: "1%"
    timezone: Europe/Paris
```

`timezone` matters more than it looks. The entire point of `pruneSchedule` is to put a
cluster-wide exclusive window somewhere off-peak, and "off-peak" is a local-time notion —
`"0 3 * * 0"` read as UTC lands in the middle of the working day for half the world. Empty
means UTC.

`pruneMaxRepackSize` caps repacking per run and is the practical bound on how long the
window lasts. Empty means restic's default: repack whatever the run needs, for as long as
that takes. On a large shared repository, set it.

:::caution[Prune is the one cluster-wide exclusive window]
While it runs, no namespace can start a backup on that repository. Its memory use scales
with **total** repository size, not with per-namespace size. Schedule it off-peak, cap it,
and give it room.
:::

Backups do not fail during the window; they **wait**. A `Backup` stays in `Pending` (or a
volume in `Snapshotting`) until a slot opens. There is no `Queued` phase — the wait is
silent and self-resolving.

`Immutable` locations never prune, and setting `pruneSchedule` on one is rejected at
admission.

## Check — the only verification

```yaml
maintenance:
  checkSchedule: "0 4 * * 0"
  checkReadDataSubset: "1%"
```

Without `checkReadDataSubset`, `restic check` is a **structural** check: it catches a
missing or truncated object, and never catches a silently corrupted one whose bytes rotted
while its name and length stayed right. That is the failure mode object storage actually
has.

`checkReadDataSubset` makes each check actually **read** pack data. It accepts:

| Form | Meaning |
|---|---|
| `"1%"`, `"2.5%"` | that percentage of packs, a different sample each run |
| `"1/20"` | a specific twentieth — cycle the numerator across runs to cover everything |
| `"5G"`, `"500M"` | that much data |

A weekly `1%` covers the whole repository in about two years, which is not enough for
anything you care about; a weekly `5%` covers it in five months. Pick against your data's
value, not against your CPU budget.

Results land on the repository:

```bash
kubectl get backuprepository dr-primary \
  -o jsonpath='{.status.lastCheckTime}{"\t"}{.status.lastCheckResult}{"\n"}'
```

`lastCheckResult` is `Passed` or `Failed`. `Failed` means restic found repository damage —
it is an incident, not a transient error.

Note the asymmetry between the two timestamps, because it is load-bearing:

- `lastCheckTime` is updated **whether the check passed or failed**. Paired with the result
  it distinguishes "verified recently and it was bad" from "not verified in weeks", which
  are different incidents.
- `lastMaintenanceTime` is updated **only when a prune succeeded**. A failed prune
  deliberately leaves it alone, so a staleness alert keeps firing.

## Restore drills are yours

This is stated plainly rather than buried: the tool cannot canary-restore every backup
daily, and does not pretend to. `restic check` verifies the repository is readable. It does
not verify that a restore produces a working application.

**A backup you never restore is not a backup.** Schedule real restore drills into a scratch
namespace, on a real cadence, and treat a drill that fails the way you would treat a
production incident. `ClusterRestore` with `createNamespace: true` and a `storageClassMapping`
makes that cheap.

## Repository health

```bash
kubectl get backuprepository
```

```
NAME         SCOPE     INITIALIZED   URL                                                      SNAPSHOTS   AGE
dr-primary   Cluster   true          s3:https://s3.example.com/crystal-backups/dr/prod-eu-1   1284        41d
```

```bash
kubectl get backuprepository dr-primary -o jsonpath='{.status.approximateSizeBytes}{" bytes, "}{.status.staleLocks}{" stale locks, "}{.status.namespacesPresent}{" namespaces\n"}'
```

`approximateSizeBytes` is the **physical** size — objects actually stored under the prefix,
post-dedup and post-compression. For the shared repository that is the whole cluster's
footprint in that bucket.

`staleLocks` counts repository lock objects older than restic's 30-minute threshold.
Normally zero; a hard-killed mover's lock is cleared by an unlock operation. **A persistent
non-zero value is a real problem**: locks are accumulating faster than they are reaped, and
every exclusive operation will eventually stall behind them.

### Why a maintenance run failed

The maintenance Job and its pod are deleted as soon as an operation finishes, so the
repository status is the only durable trace:

```bash
kubectl get backuprepository dr-primary \
  -o jsonpath='{range .status.recentMaintenance[*]}{.operation}{"\t"}{.startTime}{"\t"}{.result}{"\t"}{.message}{"\n"}{end}'
```

Newest first, capped at ten. `startTime` is when the operation was **enqueued**, not when
it started running — it deliberately includes the wait for its turn, because "the prune
took three hours" is the number you need and the queue is exactly where contention shows
up.

## The exclusive queue, briefly

Every mutating restic operation — `init`, `forget`, `prune`, `check`, `unlock`, erasure —
runs one at a time per repository, FIFO. Different repositories run fully in parallel.

**Reads are not queued.** `snapshots`, `stats` and `ls` pass `--no-lock`, so a discovery
pass or a restore's source resolution never waits behind a maintenance window. Data movers
are readers too: many can run against one repository at once.

Two operations additionally **drain** the movers before running: `unlock`, because removing
all locks would rip out a live backup's lock; and `prune`, for a different reason —
throughput. Restic's own exclusive lock already prevents corruption there, but without a
drain the prune and the whole mover fleet stare at each other on lock retries until one
gives up. Draining converts a contention storm into one short serialised window.

`forget` and `check` do not drain. Their queue turn is enough.

## Re-keying a repository

`restic key remove` revokes an *access password*. Every password decrypts the same **master
key**, and that never changes. So a leaked key cannot be revoked — the only real answer is
to copy into a fresh repository.

**1 — Create the destination** with a new key:

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupLocation
metadata:
  name: dr-primary-rekeyed
spec:
  clusterID: prod-eu-1
  s3:
    endpoint: https://s3.example.com
    bucket: crystal-backups-rekeyed
    prefix: dr
    forcePathStyle: true
    credentialsSecretRef: { name: dr-s3 }
  encryption:
    clusterKEKSecretRef: { name: cluster-kek-v2 }
```

If it will also receive native backups, initialise it with the source's chunker parameters
first (`restic init --from-repo <old> --copy-chunker-params`) — the operator's own
initialisation does not.

**2 — Sync in `AppendOnly`.** Nothing should be forgotten at the destination during a
re-key.

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupExternalSync
metadata:
  name: rekey
spec:
  sourceLocationRef: { name: dr-primary }
  destinationLocationRef: { name: dr-primary-rekeyed }
  mode: AppendOnly
```

**3 — Verify before destroying anything.** `lagSnapshots: 0` says every source snapshot has
a copy; it does not say the copies are readable.

```bash
kubectl get backuprepository dr-primary-rekeyed \
  -o jsonpath='{.status.lastCheckTime}{"\t"}{.status.lastCheckResult}{"\n"}'
```

Then run an actual `ClusterRestore` from the new location into a scratch namespace.

**4 — Cut over**, let a full backup cycle complete against the new location, and only then
retire the old one.

## Retiring a repository

Deleting a location removes its `BackupRepository` and **never touches the bucket**. To
make a retired repository unreadable you destroy its key:

```bash
kubectl -n crystal-backup-system delete secret crystal-dek-<location>
```

That is sufficient only if no copy of the unwrapped key exists anywhere else — and it is
best-effort by nature, which is why it is a runbook rather than a custom resource.

On the namespace plane, a password the tenant supplied is **theirs**: the operator never
generated it and will not delete it. One the operator generated lives at
`crystal-repo-password-<location>` in their namespace, deliberately without an
ownerReference, so it survives the location's deletion until someone decides otherwise.

Record what you are destroying before you destroy it — location name, bucket, prefix,
cluster ID, snapshot count and size at the time — because afterwards that record is the
only artefact that exists.
