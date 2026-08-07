---
title: Removing Crystal Backup
description: The delete order and why getting it wrong wedges the cluster, and a per-object table of what each deletion removes in the cluster, in the CSI layer and in the repository.
sidebar:
  order: 4
---

The command sequence lives in
[Uninstall](/CrystalBackup/docs/start/install/#uninstall), on the install page, where
somebody who is undoing an install will look for it. This page is the other half: why that
order is the order, and what each deletion actually removes.

Start with the answer to the question everybody asks second and should ask first.
**Nothing in an uninstall deletes your backups.** Not deleting a `Backup`, not deleting a
location, not deleting a repository, not `helm uninstall`, not deleting the CRDs. Removing
data from a repository takes a [`ClusterErasure`](/CrystalBackup/docs/guides/erasure/) with a
typed confirmation, and there is no other path — no `restic forget` runs anywhere on any
delete path, deliberately, because Immutable and object-lock buckets forbid it and because
erasure has to be something you asked for rather than something you triggered.

What an uninstall *can* cost you is the ability to **read** those backups, and it costs it in
exactly one step: deleting the operator namespace, which holds the cluster KEK and the wrapped
DEKs. That is the step to be careful about, and it does its damage without deleting a single
byte of backup data.

## Why the order is mandatory

Five finalizers, across six kinds, and the operator is the only process in the cluster that
removes one:

| Kind | Finalizer |
|---|---|
| `ClusterBackupLocation`, `BackupLocation` | `crystalbackup.io/location` |
| `BackupRepository` | `crystalbackup.io/repository` |
| `Backup` | `crystalbackup.io/backup` |
| `Restore` | `crystalbackup.io/restore-teardown` |
| `ClusterRestore` | `crystalbackup.io/cluster-restore-teardown` |

So the operator has to still be **running** when those objects are deleted. Remove it first
and every one of them stops at `Terminating` with nobody left to release the finalizer —
permanently, not slowly. A namespace containing one never finishes deleting, and a later
`kubectl delete crd` waits on it forever. `helm uninstall` reports success either way; the
damage shows up afterwards.

That gives the sequence, and each step exists because of the step after it:

1. **Schedules and syncs first**, so nothing new fires into a teardown that is already
   underway.
2. **Restores, then `ClusterBackup` records, then `Backup` objects**, while the operator is
   there to tear down their movers and exposures.
3. **Locations, then repositories** — after the objects that address them.
4. **Verify that nothing is `Terminating`.** This is a gate, not a formality: it is the last
   moment at which the process that can fix a stuck finalizer is still running.
5. **Then remove the install**, and only then consider the namespace.

**Never delete the CRDs as a shortcut.** `kubectl delete crd` on this group deletes every
custom resource of the group, cluster-wide, in every namespace — including the `Backup`
projections that are your tenants' view of what they can restore. It is the last step of a
deliberate teardown, never a way to get unstuck.

If you are already stuck, the fix is to **reinstall the operator at the same version** and let
it finish the deletions it owes; that is written up under
[Already stuck in Terminating?](/CrystalBackup/docs/start/install/#already-stuck-in-terminating).

## What each deletion deletes

Read the last column first. It says `no` everywhere but one row, and that row needs a typed
confirmation.

| You delete | In the cluster | CSI snapshot | Repository snapshots |
|---|---|---|---|
| `ClusterBackupSchedule` | the schedule, and the `ClusterBackup` run records it owns (they are its `ownerReference` children) | — | **no** |
| `BackupSchedule` | the schedule only. It deliberately owns nothing: the `Backup` objects it stamped *are* restore points, and cascading into them would delete a tenant's view of snapshots that still exist | — | **no** |
| `ClusterBackup` | the run record. Its per-namespace `Backup` children are linked by label, never by `ownerReference`, so none of them is touched | — | **no** |
| `Backup` | the mover Job and its credentials Secret, the manifest-capture Job and the transient RoleBinding it needed, and any exposure still live — temp PVC, static `VolumeSnapshot`/`VolumeSnapshotContent` pair, origin `VolumeSnapshot` and its `Retain`-parked content. On a backup that already finished, all of that was torn down when the volume finished, so there is usually nothing left to remove | the exposure's own snapshot, yes — teardown restores the origin content's `deletionPolicy` to `Delete` so the storage snapshot is reclaimed rather than leaked. A `VolumeSnapshot` you created yourself is never touched | **no** — and discovery re-creates the `Backup` as a projection on its next pass, because the snapshots it describes are still there |
| `Restore` / `ClusterRestore` | restore mover Jobs and their Secrets, staging PVCs, twin PVs and any mid-handover transplant volume. The data already restored into the target namespace stays where it is | — (a restore creates none) | **no** |
| `BackupLocation` | the location, and the `BackupRepository` it created — only when the labels prove that repository is this location's. The repository password Secret is **kept**, even one the operator generated: it is the only thing that can still read those backups | — | **no** |
| `ClusterBackupLocation` | the location and, by `ownerReference` garbage collection, its `BackupRepository`. The wrapped DEK Secret `crystal-dek-<location>` is **kept** | — | **no** |
| `BackupRepository` | the object. The DEK Secret is kept for the same reason, and recreating the location re-adopts the same repository rather than making a new one | — | **no** |
| the Helm release | the operator Deployment, its ServiceAccount and RBAC, the admission policy bindings, and whichever optional observability objects you enabled. Not the CRDs — Helm never deletes those — and not the namespace, which it does not own under `namespace.create: false` | — | **no** |
| the operator namespace | the cluster KEK and every wrapped DEK in it | — | **no**, and this is the dangerous row: the data survives untouched and becomes permanently unreadable, because the key that opens it is gone. Move those Secrets out first or leave the namespace alone |
| the CRDs | every custom resource of `crystalbackup.io`, cluster-wide, in every namespace | — | **no** |
| a confirmed `ClusterErasure` | nothing of yours | — | **yes** — `restic forget` filtered by tag, then `prune`. This is the only path in the product that deletes repository data, it is irreversible, and it will not run without the typed confirmation |

Two consequences worth stating on their own, because they are the ones administrators get
wrong in opposite directions:

**A repository outlives its Kubernetes objects.** Delete the location, the repository object
and every `Backup`, reinstall from scratch, recreate the location with the same `clusterID` and
`prefix`, and discovery projects the same backups back. That is the whole point of the
repository being the source of truth rather than etcd.

**A key does not outlive its Secret.** There is no second copy inside the cluster, so the
bucket escrow and your out-of-cluster KEK escrow are the only things standing between a
deleted namespace and unreadable backups.
