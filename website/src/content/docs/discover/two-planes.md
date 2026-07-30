---
title: The two planes
description: Which custom resources belong to the platform administrator, which belong to the namespace owner, and what each side can and cannot reach.
---

Crystal Backup splits its API the way cert-manager splits `ClusterIssuer` from `Issuer`:
cluster-scoped kinds for the platform, namespaced kinds for the tenant, driving the same
execution engine underneath.

The two planes are **additive**. A namespace protected by cluster DR can also run its own
schedule to its own bucket; that second copy is not a replacement for the first.

## At a glance

| | Cluster plane | Namespace plane |
|---|---|---|
| Owner | Platform administrator | Namespace owner |
| Scope | Cluster-scoped CRDs | Namespaced CRDs |
| Storage | One shared repository per location | One repository per location, in the user's bucket |
| Key | Platform DEK, wrapped by an admin-held age KEK | The user's own restic password |
| Isolation | Restic tags + an operator-derived `namespace=` filter | By construction — separate bucket, credentials, key |
| Purpose | Platform disaster recovery | Off-platform copy, self-service restore |

## Cluster-plane resources

All cluster-scoped, all admin-only.

| Kind | Short name | What it is |
|---|---|---|
| `ClusterBackupLocation` | `cbl` | The platform's object storage and platform key; backs one shared repository. |
| `ClusterBackupSchedule` | `cbs` | A cron schedule that stamps out `ClusterBackup` runs from a template. |
| `ClusterBackup` | `cb` | One DR run: fans a `Backup` into every matching namespace, and captures cluster-scoped resources. |
| `ClusterRestore` | `crst` | An admin restore addressed by repository coordinate — works when the source namespace no longer exists. |
| `ClusterErasure` | `cer` | Physical deletion of a tenant, namespace or PVC from a location. |
| `ClusterBackupExternalSync` | `cbes` | Replication of the shared repository to a second location. |
| `BackupRepository` | `br` | Operator-internal state and inventory of one restic repository. Not something you write. |

## Namespace-plane resources

All namespaced. A tenant with the shipped `crystal-backup-user` ClusterRole gets the full
verb set on the first four, and **read-only** on `Backup`.

| Kind | Short name | What it is |
|---|---|---|
| `BackupLocation` | `bl` | The user's own object storage and their own key. |
| `BackupSchedule` | `bs` | A cron schedule that stamps `Backup` objects into this namespace. |
| `Backup` | `bk` | The unit of execution **and** the projection of a restorable backup. Read-only to users. |
| `Restore` | `rst` | A self-service restore of this namespace. |
| `BackupExternalSync` | `bes` | Replication between two `BackupLocation`s in this namespace. |

`Backup` is read-only to tenants on purpose: it is a projection of the repository, and
discovery owns it. Deleting one does not delete data — discovery projects it again on the
next pass.

## What each side can reach

A namespace user:

- creates schedules, locations, restores and syncs **in their own namespace**;
- can restore from a cluster-DR backup, but only through a `Restore` naming a
  cluster-origin `Backup` that is already projected **into their namespace** — the
  operator does the repository access on their behalf;
- never holds the shared repository's key, and never runs a pod that does;
- has no access to any cluster-scoped Crystal Backup kind.

A platform administrator:

- owns the shared repository, its key and its maintenance windows;
- can restore any namespace anywhere with `ClusterRestore`, including into a namespace
  that does not exist yet;
- holds the age KEK that unwraps the platform key — and holds it **outside** the cluster.

## Where the work actually happens

Movers run **only** in `crystal-backup-system`, never in a tenant namespace, on both
planes. That is what keeps repository keys and object-storage credentials out of
namespaces the tenant controls, and it is why a snapshot taken in a tenant namespace is
re-bound centrally rather than mounted where it was taken.

The mechanics are in [Architecture](/CrystalBackup/docs/understand/architecture/); the
reasoning is in [Tenancy and isolation](/CrystalBackup/docs/understand/tenancy/).

## Which one do you want?

- **You run the platform.** Start with the cluster plane: it protects namespaces whose
  owners have not asked for anything, which is most of them. See
  [The cluster plane](/CrystalBackup/docs/guides/cluster-plane/).
- **You own a namespace.** The cluster plane already protects you, but the copy lives in
  the platform's bucket under the platform's key. If you want one that does not, see
  [The namespace plane](/CrystalBackup/docs/guides/namespace-plane/).
- **Both.** That is the intended arrangement.
