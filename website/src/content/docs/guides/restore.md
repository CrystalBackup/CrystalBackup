---
title: Restoring
description: The Recreate and Overwrite modes, the selection model, the confirmation gate, and how to rehearse a destructive restore.
sidebar:
  order: 3
---

A restore has two orthogonal axes: **mode** — how existing things are reconciled — and
**selection** — what is in scope. Get those two right and everything else follows.

## The shape of it

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: Restore
metadata:
  name: recover-uploads
  namespace: team-x
spec:
  source:
    backup: dr-daily-20260730-020000
  mode: Overwrite
  volumes:
    - names: ["uploads"]
      include: ["images/2026/**"]
  confirmation: team-x
```

There is **no location field and no target-namespace field**. A `Restore` names a `Backup`
in its own namespace, and that is the entire addressing model. If the source is a
cluster-origin backup, the operator resolves the shared repository itself, filtered by your
namespace — a filter no field of this object can influence.

## Choosing the source

Exactly one of `backup` and `time` must be set.

```yaml
# A named Backup in this namespace.
source:
  backup: nightly-20260730-010000

# The most recent one.
source:
  time: latest

# A point in time — RFC3339. A zone-less timestamp is read as UTC.
source:
  time: "2026-07-28T02:00:00Z"

# Disambiguate when both planes have backups near that time.
source:
  time: latest
  origin: cluster        # or: namespace
```

`origin` is only valid together with `time`. Once a time-based source resolves, it is
**pinned** — the restore does not drift to a newer backup between reconciles.

`source` and `mode` are **immutable after creation**. The controller re-derives both on
every pass, so an edit mid-run would mix two points in time, or two destructive modes,
inside one restore. To change either, delete the `Restore` and create another.

## Mode

| | `Overwrite` (default) | `Recreate` |
|---|---|---|
| **Manifests** | Server-side apply, create-or-update. Objects absent from the backup are **kept**. | Selected objects that exist are **deleted**, then created from the backup. |
| **PVC files** | Files in the backup overwrite or return. Files in the PVC but not in the backup are **kept**. | Exact match: files not in the backup are **deleted**. |
| **A missing PVC** | Created. | Created. |

Under the hood that is `restic restore --overwrite always`, with `--delete` added in
`Recreate`.

Pick `Overwrite` when you are putting back something that was lost. Pick `Recreate` when
you need the target to *be* the backup — after a corruption, or when stray files are the
problem. `Recreate` deletes; be sure that is what you want.

## Selection

Two independent lists, `resources` (manifests) and `volumes` (PVC data). Within one item
the conditions are ANDed; between items they are ORed. A thing is restored if **any** item
matches it.

```yaml
resources:
  - selector:
      matchLabels:
        app: web
    include: ["apps/Deployment"]
    exclude: ["apps/Deployment/legacy-*"]
  - include: ["apps/StatefulSet/postgres", "Secret/db-creds"]

volumes:
  - names: ["data-postgres-0"]
  - names: ["uploads"]
    include: ["images/2026/**"]
    exclude: ["images/2026/tmp/**"]
    targetPath: "/"
```

`include` and `exclude` on `resources` are `<group>/<Kind>[/<name>]` globs. On `volumes`
they are **file** globs inside the PVC, which is how a partial restore works.

### The rule that catches people

Each list defaults **independently**:

| You wrote | It means |
|---|---|
| the field is **omitted** | everything of that kind |
| the field is **present but empty** (`[]`) | **nothing** of that kind |
| the field lists items | only what the items match |

So omitting both restores the whole namespace, and `resources: []` with `volumes` set means
"data only, no manifests". These are genuinely different, and the API is built to keep them
different — the Go types deliberately omit `omitempty` on both lists so an empty slice
cannot be silently re-read as "everything".

Two more rules worth knowing:

- **First match wins, for volumes.** When several `volumes` items match the same PVC, the
  first one applies — one PVC is restored by exactly one mover pass. An item with no
  `names` matches every PVC, so put your specific items first.
- **Backup-time exclusions are final.** Anything excluded when the manifests were captured
  cannot be re-included at restore. It is not in the snapshot.

`targetPath` overrides the restore root inside the PVC. Empty or `/` is the PVC root, and
`..` segments are rejected.

## Confirmation

Any restore that can modify existing objects requires `spec.confirmation` equal to the
target namespace — its own namespace for a `Restore`, `spec.target.namespace` for a
`ClusterRestore`. Since the only two modes are `Recreate` and `Overwrite`, in practice
**every restore needs it**.

Two behaviours, and the difference matters:

- **A wrong value is rejected at admission.** The object is never created.
- **An empty or absent value is admitted**, and the restore parks in phase
  `AwaitingConfirmation` with condition `Ready=False`, reason `ConfirmationRequired`.

The second is the deliberate two-step. Create the restore, read what it is about to do,
then type the namespace in:

```bash
kubectl -n team-x patch restore recover-uploads --type=merge \
  -p '{"spec":{"confirmation":"team-x"}}'
```

`confirmation` is one of the few mutable fields, precisely so this works.

## Rehearsing: `dryRun`

```yaml
spec:
  dryRun: true
```

Runs the whole pipeline — ordering, selection, mode resolution — with server-side dry-run
applies, persists nothing, and writes the plan to `status.resources`. Before a `Recreate`
against a live namespace, this is the difference between a reviewed restore and a hopeful
one.

```bash
kubectl -n team-x get restore recover-uploads \
  -o jsonpath='{range .status.resources.entries[*]}{.outcome}{"\t"}{.kind}{"/"}{.name}{"\t"}{.reason}{"\n"}{end}'
```

Outcomes are `Created`, `Configured` (an existing object was applied over), `Recreated`
(deleted then created) and `Failed`. Under `dryRun` these are **planned** actions.

:::caution[The volume half has no dry run]
`dryRun` covers the manifest pipeline. It does not simulate the data restore. A dry run
tells you which objects would change; it does not tell you which files would be deleted by
a `Recreate`.
:::

## Watching it

```bash
kubectl -n team-x get restore recover-uploads -w
```

```
NAME              PHASE                  RESTORED   FAILED   VOLUMES   AGE
recover-uploads   AwaitingConfirmation   0          0        0         4s
recover-uploads   Running                2          0        9         31s
recover-uploads   Running                7          1        9         1m44s
recover-uploads   PartiallyFailed        8          1        9         2m18s
```

`RESTORED` and `FAILED` move on **every** reconcile, against the `VOLUMES` the restore planned,
so the three columns answer the questions you actually have while it runs: is it moving, how far
along is it, and what did not come back. `VOLUMES - RESTORED - FAILED` is what is still in
flight; on a terminal restore that difference is `0`.

They count **volumes only**. The manifest half is `status.restoredResources` and
`status.resources.failedCount`, and the phase rolls up both — so a restore can read
`PartiallyFailed` with `FAILED` at `0` because objects, not data, failed to apply.

Phases: `Pending`, `AwaitingConfirmation`, `Running`, `Completed`, `PartiallyFailed`,
`Failed`. A restore reports per-resource failures and **continues**; it does not abort on
the first one.

```bash
kubectl -n team-x get restore recover-uploads \
  -o jsonpath='{.status.restoredVolumes}{"/"}{.status.plannedVolumes}{" volumes back, "}{.status.failedVolumes}{" lost, "}{.status.restoredBytes}{" bytes, "}{.status.restoredResources}{" resources\n"}'
```

The per-resource report is capped at 100 entries with 20 changed field paths each — etcd's
1 MiB object limit is a hard ceiling, and a status that cannot be written loses the whole
report rather than its tail. `status.resources.truncated` tells you when that happened.

## What to expect operationally

- **Movers run in `crystal-backup-system`**, never in your namespace. Your namespace
  receives restored PVCs and nothing else.
- **At most four mover Jobs per restore** run at once.
- **A PVC that does not exist is created** with the capacity, storage class and access modes
  recorded in the snapshot's tags at backup time. Pre-create it yourself to override any of
  that.
- **A PVC that exists and is bound** is restored into in place. If it is attached to exactly
  one node, the mover is pinned to that node.
- **A restored PVC is yours.** It carries the annotation
  `crystalbackup.io/restored-from: <run>` and none of the operator's labels, so nothing
  will ever garbage-collect it.
- **Restoring under a live writer is discouraged.** Scale the workload down first; the
  recommended drill is `Recreate` plus a scale-down.
- **`volumeMode: Block` is not supported** — those volumes fail with reason
  `RestoreBlockUnsupported`.
- **`trusted.*` extended attributes are not restored** (they need `CAP_SYS_ADMIN`).

## Restoring from cluster DR as a tenant

Nothing different to do. A cluster-origin `Backup` is projected into your namespace by
discovery, and you name it like any other:

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: Restore
metadata:
  name: recover-from-dr
  namespace: team-x
spec:
  source:
    backup: dr-daily-20260730-020000
  mode: Overwrite
  confirmation: team-x
```

What happens underneath: the operator runs a listing against the shared repository with
the filter `namespace=team-x` built from this object's own metadata, and hands the mover
only the snapshot IDs that came back. A PVC the filtered listing does not resolve **fails
closed** — there is no unfiltered fallback. You never hold the shared repository's key, and
no pod in your namespace does either.

The cost is one extra listing Job, a few seconds, before data moves.

## Administrator restores

For restoring into a different namespace, into a namespace that no longer exists, or on a
rebuilt cluster, see [Disaster recovery](/CrystalBackup/docs/guides/disaster-recovery/).
