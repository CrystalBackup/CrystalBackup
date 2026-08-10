---
title: Labels and annotations
description: Every crystalbackup.io/* label, annotation and finalizer, what sets it, and which ones you set yourself.
---

Everything here is under the `crystalbackup.io` domain, which is also the API group. The
authoritative source is `internal/apiconst/apiconst.go`.

## The one you set

| Key | Where | Meaning |
|---|---|---|
| `crystalbackup.io/protect` | on a **Namespace** | A convention, not a magic key. The operator reads it because your `ClusterBackupSchedule`'s `namespaces.matchLabels` names it; it never sets it. `crystalbackup.io/protect: "true"` is the usual opt-in. |
| `crystalbackup.io/tenant` | on a **Namespace** | Groups several namespaces under one tenant. When absent, a namespace's tenant is its own name. Determines the restic `tenant=` tag, and therefore the scope of `ClusterErasure` with `target.tenant`. |

## Labels worth querying

| Key | On | Meaning |
|---|---|---|
| `crystalbackup.io/origin` | `Backup` | `cluster` or `namespace` — which plane produced it. The one tenants use most. |
| `crystalbackup.io/cluster-backup` | `Backup` | The `ClusterBackup` run that fanned this one out. A **label, not an ownerReference**, so pruning run history never deletes a restorable backup. |
| `crystalbackup.io/schedule` | `Backup`, `ClusterBackup` | The originating schedule. Absent on a manual run. Mirrors the restic `schedule=` tag. |
| `crystalbackup.io/namespace` | `Backup`, operator-owned objects | A child `Backup`'s origin namespace, as a queryable label. Mirrors the restic `namespace=` tag. |
| `crystalbackup.io/tenant` | `Backup` | The resolved tenant. Mirrors the restic `tenant=` tag. |
| `crystalbackup.io/location` | `BackupRepository` | The location this repository backs. Together with `crystalbackup.io/namespace` it is the back-link that stands in for an ownerReference on the namespace plane — a cluster-scoped object cannot be owned by a namespaced one. |

Useful queries:

```bash
# Everything a given DR run produced.
kubectl get backups -A -l crystalbackup.io/cluster-backup=dr-daily-20260730-020000

# What a tenant produced themselves, versus what the platform did for them.
kubectl -n team-x get backups -l crystalbackup.io/origin=namespace
kubectl -n team-x get backups -l crystalbackup.io/origin=cluster

# Every namespace covered by one tenant identity.
kubectl get ns -l crystalbackup.io/tenant=acme
```

## Annotations you will see

| Key | On | Meaning |
|---|---|---|
| `crystalbackup.io/restored-from` | a PVC a restore **created** | The run it came from. Deliberately an annotation and never accompanied by the operator's own labels — a restored PVC is **your** object and must never be garbage-collected. |
| `crystalbackup.io/projected` | `Backup` | `"true"` when the object is a read-only projection reconstructed from the repository by discovery. The controller treats such a `Backup` as inert: it never snapshots or moves data for it. This is why some `Backup` objects cannot be acted on. |
| `crystalbackup.io/secret-data-excluded` | a restored `Secret` | `"true"` when the Secret was captured under `manifestOptions.excludeSecretData` and its `data`/`stringData` were stripped. The restored Secret exists and is empty, and says so — a workload that needs the values fails visibly rather than silently starting with the wrong ones. |

## Hook annotations

Honoured on pods only when the schedule sets `hooks.honorAnnotations: true`. Four suffixes
on each of two prefixes.

| Key | Value |
|---|---|
| `crystalbackup.io/pre-backup-command` | JSON argv, e.g. `'["psql","-c","CHECKPOINT"]'` |
| `crystalbackup.io/pre-backup-container` | container name |
| `crystalbackup.io/pre-backup-timeout` | a duration, e.g. `30s` |
| `crystalbackup.io/pre-backup-on-error` | `Fail` or `Continue` |
| `crystalbackup.io/post-backup-command` | JSON argv |
| `crystalbackup.io/post-backup-container` | container name |
| `crystalbackup.io/post-backup-timeout` | a duration |
| `crystalbackup.io/post-backup-on-error` | `Fail` or `Continue` |

Annotations **replace** the schedule's hooks for that pod — they never merge. The
annotation supplies the command, never the identity: hooks always run as
`hooks.serviceAccountName`. See [Consistency hooks](/CrystalBackup/docs/guides/hooks/).

## Finalizers

These are why an object can sit in `Terminating`. They exist so the controller can tear
down live exposures, mover Jobs and staging volumes **before** the object disappears —
without them, deleting a `Backup` mid-run would leak a temporary PVC and a
`VolumeSnapshotContent` that no garbage collector can ever reach.

| Finalizer | On |
|---|---|
| `crystalbackup.io/location` | `ClusterBackupLocation`, `BackupLocation` |
| `crystalbackup.io/repository` | `BackupRepository` |
| `crystalbackup.io/backup` | `Backup` |
| `crystalbackup.io/restore-teardown` | `Restore` |
| `crystalbackup.io/cluster-restore-teardown` | `ClusterRestore` |

Deleting a location or a repository **never** erases repository objects. Erasure is an
explicit, confirmed [`ClusterErasure`](/CrystalBackup/docs/guides/erasure/).

If an object is stuck in `Terminating`, read the operator's logs rather than removing the
finalizer by hand. Removing it is how you get the leak the finalizer exists to prevent.

## Operator-internal

You will see these on objects in `crystal-backup-system` while a backup or a restore is in
flight. They are listed so they are not a mystery; nothing outside the operator should
depend on them.

| Key | Meaning |
|---|---|
| `crystalbackup.io/backup` | The `Backup` an exposure object or mover Job belongs to, by name. Present on both planes — unlike `crystalbackup.io/cluster-backup`, which only a cluster-DR run has — so it is what the teardown sweep and the orphan reaper resolve an owner with. (Same string as the `Backup` finalizer; labels and finalizers are different fields.) |
| `crystalbackup.io/pvc` | The source PVC an exposure object or mover Job belongs to. |
| `crystalbackup.io/restore`, `crystalbackup.io/cluster-restore` | The owning restore. |
| `crystalbackup.io/pv-role` | `twin` or `transplant` — marks a PersistentVolume a restore created or adopted. |
| `crystalbackup.io/exposure-kind` | Which target-exposure mechanism a restore mover started with. |
| `crystalbackup.io/mover-role` | `data` or `manifest` — what the mover pod may talk to. NetworkPolicies select on it, because a NetworkPolicy selects pods by label and not by ServiceAccount. |
| `crystalbackup.io/mover-job`, `crystalbackup.io/operator-namespace` | On a transient RoleBinding: which Job it accompanies, and which operator created it. |
| `crystalbackup.io/exposure-node` | On a staging claim: the node the target volume was attached to. |
| `crystalbackup.io/mover-result` | A completed restore mover's result JSON, kept because the pod is deleted before it can be read. |
| `crystalbackup.io/exposures-cleaned` | `"true"` once the terminal teardown sweep has verified every exposure object was collected. |

## Not in the domain, but load-bearing

`app.kubernetes.io/managed-by: crystal-backup` is stamped on every operator-managed
workload object — mover Jobs, exposure objects, wrapped-key Secrets. It is the single
selector for "everything Crystal Backup created":

```bash
kubectl -n crystal-backup-system get jobs,secrets -l app.kubernetes.io/managed-by=crystal-backup
```

It is deliberately **not** `app.kubernetes.io/name=crystal-backup`, which is the operator
pod's own label.

## Restic tags

Not Kubernetes labels, but the same idea inside the repository. Every snapshot carries:

| Tag | Value |
|---|---|
| `crystalbackup` | the marker every Crystal Backup snapshot carries |
| `tenant=` | the resolved tenant |
| `namespace=` | the origin namespace |
| `pvc=` | the source PVC (data snapshots) |
| `kind=` | `data`, `manifests` or `cluster-manifests` |
| `schedule=` | the originating schedule |
| `run=` | the run name — the same string as the `Backup` object's name |
| `pvcsize=`, `pvcclass=`, `pvcmodes=` | the PVC's shape, on `kind=data` snapshots since 0.2 — this is what lets a `ClusterRestore` rebuild a PVC with nothing surviving to describe it |

The snapshot `host` is the location's `clusterID`; `paths` are `/data/<namespace>/<pvc>`,
`/manifests/<namespace>` and `/cluster-manifests`.

`namespace=` is the tag the operator filters on when mediating a tenant's restore against
the shared repository — built from the custom resource's own `metadata.namespace`, which is
why it cannot be forged. See [Tenancy and isolation](/CrystalBackup/docs/understand/tenancy/).
