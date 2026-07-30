---
title: The cluster plane
description: Platform disaster recovery — ClusterBackupLocation, ClusterBackupSchedule, namespace selection and cluster-scoped capture.
sidebar:
  order: 1
---

The cluster plane is the platform team's: one shared repository, one retention policy, one
maintenance window, covering every namespace you select — including the ones whose owners
have never heard of Crystal Backup. That is the point of it.

## The location

A `ClusterBackupLocation` is the object storage plus the platform key, and it backs exactly
one restic repository at `s3://<bucket>/<prefix>/<clusterID>/`.

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupLocation
metadata:
  name: dr-primary
spec:
  default: true
  mode: Standard
  clusterID: prod-eu-1
  s3:
    endpoint: https://s3.example.com
    bucket: crystal-backups
    prefix: dr
    region: eu-west-1
    forcePathStyle: true
    credentialsSecretRef:
      name: dr-s3
  encryption:
    clusterKEKSecretRef:
      name: cluster-kek
  discovery:
    enabled: true
    interval: 1h
  retention:
    keepLast: 3
    keepDaily: 7
    keepWeekly: 4
    keepMonthly: 6
  maintenance:
    pruneSchedule: "0 3 * * 0"
    pruneMaxRepackSize: "50G"
    checkSchedule: "0 4 * * 0"
    checkReadDataSubset: "1%"
    timezone: Europe/Paris
```

### Fields that are immutable

`clusterID`, `s3.endpoint`, `s3.bucket`, `s3.prefix` and `mode` cannot be changed after
creation. They compose the repository path, so an edit would re-point the location at a
*different* repository with no data moving — every backup taken so far would be orphaned
while the object still looked healthy. `mode` is fixed because it is a property chosen when
the repository is created.

To change any of them, create a new location and, if you need the history, replicate into
it with [external sync](/CrystalBackup/docs/guides/external-sync/).

### `clusterID` is not cosmetic

It is the restic snapshot `host` **and** a path segment. One bucket can therefore serve
several clusters, each under its own prefix, without collision. Choose a name you will
still recognise in two years, during an incident, in someone else's terminal.

### `default: true`

Exactly one location may be the default. It is what a `BackupLocation` inherits its
`clusterID` from when the tenant does not set one. The uniqueness check is the operator's
webhook rather than an admission policy, because it is a cross-object constraint that
per-object CEL cannot express; a race that slips through surfaces as a `MultipleDefaults`
condition.

### `retention` lives here, not on schedules

`restic forget` operates on the whole repository. One location backs one repository, so a
single authoritative policy per location is the only arrangement in which two schedules
cannot fight over the same snapshots. It is applied **per PVC** (grouped by restic
`host,paths`) after each successful backup.

There is no `keepWithinDuration`. The available fields are exactly `keepLast`,
`keepHourly`, `keepDaily`, `keepWeekly`, `keepMonthly`, `keepYearly`.

## The schedule

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupSchedule
metadata:
  name: dr-daily
spec:
  schedule: "0 2 * * *"
  timezone: Europe/Paris
  paused: false
  jitter: true
  concurrencyPolicy: Forbid
  startingDeadlineSeconds: 3600
  successfulRunsHistoryLimit: 10
  failedRunsHistoryLimit: 10
  template:
    spec:
      locationRef:
        name: dr-primary
      namespaces:
        matchLabels:
          crystalbackup.io/protect: "true"
        exclude: ["kube-*", "crystal-backup-system"]
      includeManifests: true
      manifestOptions:
        excludeSecretData: false
      clusterResources:
        enabled: true
      pvcSelector:
        exclude: ["*-cache", "scratch-*"]
      maxConcurrentMovers: 4
      backoffLimit: 2
```

`jitter: true` spreads the per-namespace fan-out deterministically. On a cluster with
fifty protected namespaces it is the difference between a thundering herd at 02:00 and a
spread-out hour. Turn it on.

`concurrencyPolicy: Forbid` (the default) means a run still going when the next one is due
blocks it. `Skip` drops the missed run instead.

`startingDeadlineSeconds` bounds catch-up after the operator has been down: without it a
long outage produces a burst of missed runs on recovery.

`maxConcurrentMovers` is a **cluster-wide** cap, not a per-run one — it is checked against
every mover Job in the operator namespace. That is why it exists only on the cluster plane;
a tenant setting it would be setting a platform-wide limit.

## Selecting namespaces

`namespaces` needs **exactly one non-empty positive form**, plus an optional `exclude`
applied last. An empty list or map counts as unset.

```yaml
# By opt-in label — the recommended default.
namespaces:
  matchLabels:
    crystalbackup.io/protect: "true"

# By name glob.
namespaces:
  matchNames: ["team-*", "prod-*"]
  exclude: ["team-sandbox"]

# By label expression.
namespaces:
  matchExpressions:
    - key: tier
      operator: In
      values: ["production", "staging"]

# By regexp — a power tool. Prefer one of the above.
namespaces:
  regexp: "^c-[a-z0-9]+$"
```

`crystalbackup.io/protect` is a convention, not a magic key: the operator reads it because
your selector names it, and never sets it. Two workable postures:

- **opt-in** — `matchLabels: {crystalbackup.io/protect: "true"}`. Namespaces are protected
  when someone asks. Nothing is backed up by accident, and nothing is backed up by
  omission either.
- **opt-out** — `matchNames: ["*"]` with an `exclude` list. Everything is covered by
  default. Safer, and it will pick up namespaces full of caches and build artefacts unless
  you also tune `pvcSelector`.

Choose deliberately; the failure modes are opposite.

## Selecting PVCs

```yaml
pvcSelector:
  matchLabels:
    backup: "yes"
  include: ["data-*"]
  exclude: ["*-cache", "*-tmp"]
```

Empty means every PVC in the namespace. `exclude` is applied after the positive forms.

## Cluster-scoped resources

A run also captures the objects that live outside any namespace, as one snapshot with
`kind=cluster-manifests`. This is on by default on the cluster plane, and it is what makes
bare-cluster DR possible: restoring a namespace into a cluster with no matching
`StorageClass` restores nothing usable.

```yaml
clusterResources:
  enabled: true
  include: []          # empty ⇒ the curated default set
  exclude: ["system:*"]
```

With `include` empty, the default set is: `CustomResourceDefinition`, `StorageClass`,
`VolumeSnapshotClass`, `IngressClass`, `PriorityClass`, `RuntimeClass`, non-system
`ClusterRole` and `ClusterRoleBinding`, and `PersistentVolume`. Objects named `system:*`
and add-on-owned objects are excluded by default, so a restore does not fight the API
server.

Capture is cheap and broad; **restore is opt-in and narrow**. See
[Disaster recovery](/CrystalBackup/docs/guides/disaster-recovery/).

## Running a backup now

Schedules stamp out `ClusterBackup` objects. You can create one yourself — the same run
configuration, inline:

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackup
metadata:
  name: before-the-upgrade
spec:
  locationRef:
    name: dr-primary
  namespaces:
    matchNames: ["team-x"]
  includeManifests: true
  clusterResources:
    enabled: false
```

Scheduled runs are named `<schedule>-<YYYYMMDD-HHMMSS>` in UTC. That same string is the
`ClusterBackup` name, every child `Backup`'s name in every namespace, and the restic `run`
tag — so a run is one identifier from end to end.

## Watching a run

```bash
kubectl get clusterbackups
kubectl -n <namespace> get backups
```

Aggregate counters live on the `ClusterBackup`; per-namespace detail lives on the child
`Backup` objects. The parent keeps only a **capped** failure list, because an unbounded
per-namespace map on a 500-namespace cluster is an object that eventually cannot be
written at all.

Children are linked to the run by the label `crystalbackup.io/cluster-backup`, **not** an
ownerReference. Pruning old run records therefore never deletes a restorable backup.

```bash
kubectl get backups -A -l crystalbackup.io/cluster-backup=dr-daily-20260730-020000
```

If a run reports `PartiallyFailed`:

```bash
kubectl get clusterbackup <run> -o jsonpath='{range .status.failures[*]}{.namespace}{"\t"}{.backup}{"\t"}{.message}{"\n"}{end}'
```

## Pausing

```bash
kubectl patch clusterbackupschedule dr-daily --type=merge -p '{"spec":{"paused":true}}'
```

`paused` exists on `ClusterBackupSchedule` and on `ClusterBackupExternalSync`. It does
**not** exist on the namespaced `BackupSchedule` — to stop a tenant schedule, delete it or
change its cron expression.

## Next

- [The namespace plane](/CrystalBackup/docs/guides/namespace-plane/)
- [Maintenance and verification](/CrystalBackup/docs/guides/maintenance/)
- [Disaster recovery](/CrystalBackup/docs/guides/disaster-recovery/)
