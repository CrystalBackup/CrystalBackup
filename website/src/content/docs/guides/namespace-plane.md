---
title: The namespace plane
description: Self-service backup for a namespace owner — your own bucket, your own key, your own schedule.
sidebar:
  order: 2
---

The cluster plane already protects your namespace, into the platform's bucket under the
platform's key. The namespace plane gives you a **second, independent copy** that the
platform cannot read.

Nothing here requires an administrator once the operator is installed and your platform
team has bound you the `crystal-backup-tenant` role — which, if they left
`rbac.aggregateToDefaultRoles` on, you already have if you have `edit` in your namespace.

## What you can do

With the tenant role you get the full verb set on `BackupLocation`, `BackupSchedule`,
`Restore` and `BackupExternalSync` in your own namespaces, and **read-only** on `Backup`.

`Backup` is read-only because it is a projection of the repository, not a thing you author.
Delete one and discovery projects it again on the next pass; the repository is the source
of truth, not the object.

## Your key

A `BackupLocation` repository has exactly one key slot: yours. There is no API field that
could give the platform a second one.

Bring your own password:

```bash
kubectl -n team-x create secret generic offsite-key \
  --from-literal=password="$(openssl rand -base64 32)"
```

Or omit `repositoryPasswordSecretRef` and the operator generates one **into your
namespace**, named `crystal-repo-password-<location>`.

:::danger[There is no recovery path]
Lose this password and your backups are unreadable. The platform does not hold a copy,
because the mechanism to hold one was removed rather than guarded — removing a restic key
slot does not rotate the master key, so a platform slot would have been permanent.

Escrow it wherever you keep your own root secrets, **outside this cluster**.
:::

## Your credentials

```bash
kubectl -n team-x create secret generic offsite-s3 \
  --from-literal=AWS_ACCESS_KEY_ID=... \
  --from-literal=AWS_SECRET_ACCESS_KEY=...
```

Both Secrets are referenced **by name only**, and must be in the same namespace as the
location. An admission rule enforces it, so a location cannot reach into another namespace
for its credentials.

## The location

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: BackupLocation
metadata:
  name: my-offsite
  namespace: team-x
spec:
  mode: Standard
  s3:
    endpoint: https://s3.other-provider.example
    bucket: team-x-backups
    prefix: crystal
    forcePathStyle: true
    credentialsSecretRef:
      name: offsite-s3
  encryption:
    repositoryPasswordSecretRef:
      name: offsite-key
  discovery:
    enabled: true
    interval: 1h
  retention:
    keepDaily: 14
    keepWeekly: 8
```

As on the cluster plane, `clusterID`, `s3.endpoint`, `s3.bucket`, `s3.prefix` and `mode`
are **immutable after creation**: they compose the repository path. `clusterID` is optional
here and defaults from the platform's default `ClusterBackupLocation` — but once resolved
it is recorded in `status.clusterID` and never re-derived, so an administrator changing the
platform default later cannot silently move your repository.

Use a bucket the platform does not control. That is the whole point; a location pointing at
the platform's own object storage gets you a second copy but not a second trust boundary.

Check it is up:

```bash
kubectl -n team-x get backuplocation my-offsite
```

```
NAME         MODE       PHASE   AGE
my-offsite   Standard   Ready   38s
```

## The schedule

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: BackupSchedule
metadata:
  name: nightly
  namespace: team-x
spec:
  locationRef:
    name: my-offsite
  schedule: "0 1 * * *"
  timezone: Europe/Paris
  jitter: true
  concurrencyPolicy: Forbid
  startingDeadlineSeconds: 3600
  pvcSelector:
    exclude: ["*-cache"]
  includeManifests: true
  backoffLimit: 2
```

`locationRef` must name a `BackupLocation` **in this namespace**. It can never name a
`ClusterBackupLocation` — that is an admission rule, and it is what keeps a tenant from
writing into the shared platform repository.

Two fields you will not find here, and why:

- **`paused`** does not exist on `BackupSchedule`. To stop it, delete it or change the cron
  expression.
- **`maxConcurrentMovers`** does not exist either. It is a cluster-wide cap, and a tenant
  setting it would be setting a platform-wide limit.

## Watching it

```bash
kubectl -n team-x get backupschedules
```

```
NAME      SCHEDULE    LOCATION     LAST-SUCCESS   AGE
nightly   0 1 * * *   my-offsite   7h             3d
```

```bash
kubectl -n team-x get backups
```

```
NAME                      PHASE       LOCATION     BACKUP-TIME   AGE
nightly-20260730-010000   Completed   my-offsite   7h            7h
dr-daily-20260730-020000  Completed   dr-primary   6h            6h
```

Both planes' backups appear in your namespace. Tell them apart by origin:

```bash
kubectl -n team-x get backups -l crystalbackup.io/origin=namespace   # yours
kubectl -n team-x get backups -l crystalbackup.io/origin=cluster     # platform DR
```

You can restore from either. A cluster-origin backup is restored through the same
`Restore` object; the operator resolves the shared repository on your behalf, with a
namespace filter derived from your namespace that no field of your object can influence.

Per-volume detail:

```bash
kubectl -n team-x get backup nightly-20260730-010000 \
  -o jsonpath='{range .status.volumes[*]}{.pvc}{"\t"}{.phase}{"\t"}{.sizeBytes}{"\t"}{.reason}{"\n"}{end}'
```

## Manifests

`includeManifests` defaults to `true`, and the manifest snapshot **includes your Secret
objects**. On your own repository, under your own key, that is normally what you want — a
namespace recovery without Secrets is a namespace that does not start.

If you would rather trade recoverability for a smaller blast radius:

```yaml
manifestOptions:
  excludeSecretData: true
```

Secrets are then stored with `data` and `stringData` stripped and annotated
`crystalbackup.io/secret-data-excluded: "true"`. Restore recreates them **empty**, carrying
the same annotation — so a workload that needs the values fails visibly on a missing key
rather than silently starting with the wrong ones.

## Consistency hooks

A snapshot is crash-consistent by default. If your application needs more, hooks let you
quiesce it around the snapshot — and on this plane they require a ServiceAccount **you**
grant, which the operator impersonates. See [Consistency hooks](/CrystalBackup/docs/guides/hooks/).

## Where the work runs

Not in your namespace. Mover Jobs run in `crystal-backup-system`, on both planes, and your
namespace never receives credentials or key material — only restored PVCs.

That is a property of the design rather than a courtesy: a snapshot taken on your PVC is
re-bound centrally, so your data is never mounted somewhere a neighbour could reach it.

## Next

- [Restoring](/CrystalBackup/docs/guides/restore/)
- [External sync](/CrystalBackup/docs/guides/external-sync/) — a second location, in this
  namespace, under a second key of yours
- [Consistency hooks](/CrystalBackup/docs/guides/hooks/)
