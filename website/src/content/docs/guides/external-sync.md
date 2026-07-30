---
title: External sync
description: Replicating a repository to a second location with restic copy, re-encrypted to the destination's own key.
sidebar:
  order: 5
---

External sync keeps a **second, independent repository** in step with a first. It is not a
byte-for-byte clone: snapshots are decrypted from the source and **re-encrypted to the
destination's own key**, so the destination is a genuine repository under a key the source
does not hold.

That is what makes it usable across providers, across accounts, and — on the namespace
plane — between two locations of the same tenant without the platform key ever being
involved.

## Two kinds, one per plane

```yaml
# Cluster plane, admin.
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupExternalSync
metadata:
  name: offsite
spec:
  sourceLocationRef:
    name: dr-primary
  destinationLocationRef:
    name: dr-secondary
  schedule: "0 5 * * *"
  timezone: Europe/Paris
  paused: false
  mode: Mirror
  selection:
    namespaces:
      matchLabels:
        tier: production
```

```yaml
# Namespace plane, tenant. Both refs are BackupLocations in THIS namespace.
apiVersion: crystalbackup.io/v1alpha1
kind: BackupExternalSync
metadata:
  name: to-my-second-provider
  namespace: team-x
spec:
  sourceLocationRef:
    name: my-offsite
  destinationLocationRef:
    name: my-offsite-b
  schedule: "0 6 * * *"
  mode: Mirror
```

An empty `schedule` means on-demand only — the sync runs when you create or edit the object
and not otherwise.

`selection.namespaces` exists on the cluster plane only, and narrows the copy by namespace
tag. Omitted, the whole repository is replicated.

Both source and destination must be locations of the **same plane**, and they must
**differ**. A self-referential `Mirror` would `forget` and `prune` its own source; that is
an admission rule, not a warning.

On the namespace plane both refs resolve inside the CR's own namespace, and neither can
name a `ClusterBackupLocation`. The tenant's siloing is preserved: the platform key is
never involved in a tenant's sync.

## Modes

| | `Mirror` (default) | `AppendOnly` |
|---|---|---|
| Copies missing snapshots | yes | yes |
| Removes snapshots at the destination that are gone from the source | **yes** — `forget` then `prune` | no |
| Destination grows unboundedly | no | yes |

`Mirror` reconciles the destination to the source's *current* snapshot set. Which
destination snapshots to forget is decided by restic's `original` field, which records the
source snapshot's full ID — tags and timestamps cannot tell two runs of the same schedule
apart, so this is the only key that works.

Use `AppendOnly` when the destination is meant to outlive the source's retention: a
secondary that keeps history the primary has already pruned. And use it when you are
re-keying a repository, where nothing should be forgotten at the destination while the copy
is in flight.

## Deduplication at the destination

If the destination will **also** receive native backups, initialise it with the source's
chunker parameters, or the two blob sets will not deduplicate against each other:

```bash
restic -r <destination> init --from-repo <source> --copy-chunker-params
```

The operator's own initialisation does not do this. Do it before you create the location,
or accept that a mixed destination stores some data twice.

## Watching it

```bash
kubectl get clusterbackupexternalsync offsite
```

```
NAME      MODE     PHASE       LAG   AGE
offsite   Mirror   Completed   0     6d
```

`LAG` is `status.lagSnapshots`: source snapshots with no copy at the destination. Zero is
the steady state. A number that grows run after run means the sync is not keeping up —
usually bandwidth, occasionally a destination that is not reachable.

```bash
kubectl get clusterbackupexternalsync offsite \
  -o jsonpath='{.status.phase}{" lag="}{.status.lagSnapshots}{" copied="}{.status.snapshotsCopied}{" bytes="}{.status.bytesCopied}{"\n"}'
```

:::caution[`lagSnapshots: 0` is not a verification]
It says every source snapshot has a copy. It does not say those copies are readable. Before
you rely on a destination — and certainly before you retire a source — run a `restic check`
against it and then do an actual restore from it. See
[Maintenance and verification](/CrystalBackup/docs/guides/maintenance/).
:::

## What it costs

The first sync moves roughly the volume you selected. Afterwards only the blob delta moves,
against a `Standard` destination.

That bandwidth is the price of the two properties this mechanism buys: a destination under
its own key, and per-namespace selectivity. A raw object-storage replication would be
cheaper and would give you neither — it carries the source's master key to the destination,
which on the namespace plane would put the platform key inside a tenant's silo, and it is
whole-repository only.

## Scheduling around maintenance

A sync takes a shared read lock on the source and writes the destination under a
non-exclusive lock — exactly like a backup. Only `Mirror`'s trailing `forget` and `prune`
need the destination's exclusive queue.

In practice: schedule the sync so it does not overlap the *destination's* prune window. The
source's window matters less, since a sync reads through it.

## Copied snapshots at the destination

They keep their `host`, `paths` and tags, so discovery at the destination projects them
exactly like native snapshots. They get **new IDs** — restic content-addresses them under
the destination's key — which is why the `original` field exists and why `Mirror` uses it.

If the destination location is registered in a cluster, `kubectl get backups` there lists
the copies as restorable, and they are.

## Not yet: immutable destinations

`AppendOnly` is forced when the destination is `Immutable`. Object Lock support is not
implemented in this release, so that combination is not something to plan around — see
[When not to choose it](/CrystalBackup/docs/discover/when-not-to-use/).

## Re-keying a repository

External sync is also the mechanism for changing a repository's key, because
`restic key remove` revokes an access password but never rotates the master key. The
procedure is: create a destination with the new key, sync in `AppendOnly`, verify, cut the
schedules over, then decommission the old repository.

See [Maintenance and verification](/CrystalBackup/docs/guides/maintenance/#re-keying-a-repository).
