---
title: The cascade
description: Why Backup is both the unit of execution and the projection of a restorable backup, and what follows from that.
sidebar:
  order: 2
---

The cascade is CronJob-shaped, and deliberately so:

```
ClusterBackupSchedule ──cron + template──▶ ClusterBackup ──fan-out──▶ Backup ──per PVC──▶ movers
BackupSchedule ─────────cron──────────────────────────────────────▶ Backup ──per PVC──▶ movers
```

- **`ClusterBackupSchedule`** stamps out `ClusterBackup` runs from a template, bounded by
  `successfulRunsHistoryLimit` and `failedRunsHistoryLimit`.
- **`ClusterBackup`** resolves its namespace selector and creates a `Backup` in each
  matching namespace, plus one cluster-scoped resources snapshot.
- **`BackupSchedule`** stamps `Backup` objects directly, with no fan-out.
- **`Backup`** is the single unit of execution, driven identically whichever plane created
  it, and it drives one mover Job per PVC.

## One object, two jobs

`Backup` is the unit of execution **and** the projection of a restorable backup. That is
unusual, and it is where most of the design's consequences come from.

Two producers write objects of that kind:

- the execution fan-out, which knows the run configuration;
- **discovery**, which reconstructs objects from restic snapshots alone.

Discovery's only input is the repository. It can rebuild which location a backup lives in
(the repository *is* the location) and its per-volume results (from the tags). It **cannot**
rebuild a PVC selector, a manifest option or a hook command — none of that was ever written
to restic, and none of it ever will be.

## "The backup carries identity, not intent"

That constraint gives the rule that shapes `Backup.spec`: a field a projection cannot
reproduce must not live in a place a projection owns.

So `spec` holds **identity** — which repository, which schedule stamped it — and the run
configuration is **materialized** into `spec.run` by whatever created the object, once, at
creation. Not pulled from a parent at every reconcile.

Two things follow:

- **A `Backup` executes even when its parent is gone.** The configuration was copied down;
  the schedule can be deleted, the run record pruned, and the backup still knows what it
  was told to do.
- **Discovery never claims `spec.run`.** Under server-side apply, an owner that claims a
  field it cannot reproduce fights the execution controller over the object forever. So
  projections leave it absent — and a `crystalbackup.io/projected` annotation makes them
  inert regardless.

The pointer distinction matters here too: absent means "this object predates
materialization, fall back to the parent", while an empty struct means "materialized, every
knob at its default". Collapsing those two would either break old objects or silently
re-read defaults as an instruction to go looking for a parent.

## Consequences you can observe

**A `Restore` never names a location.** It names a `Backup` in its own namespace, and the
`Backup` knows where it lives. That is why `Restore` has no `locationRef` — the absence is
not an omission, it is the cascade doing its job.

**Run history and restorability are decoupled.** A child `Backup` is linked to its
`ClusterBackup` by the **label** `crystalbackup.io/cluster-backup`, not an ownerReference.
Pruning run records to `successfulRunsHistoryLimit` therefore never deletes a restorable
backup. (It could not be an ownerReference anyway: a namespaced object cannot be owned by a
cluster-scoped one — Kubernetes would treat the reference as dangling and garbage-collect
the child.)

**Deleting a `Backup` deletes nothing.** Discovery projects it again on the next pass. The
repository is the source of truth; the object is a view of it. Conversely, expiring a
snapshot makes the projection disappear — which is why `kubectl get backups` keeps telling
the truth about what is restorable.

**`ClusterBackup.status` is aggregate only.** Counters plus a **capped** failure list. An
unbounded per-namespace map on a 500-namespace cluster produces an object that eventually
cannot be written at all, and a status that cannot be written loses the whole report rather
than its tail.

**Tenant visibility is native RBAC.** Because `Backup` is namespaced, a user listing
backups sees exactly their own. No filtering layer, no view, no proxy — just RBAC doing
what RBAC does.

**A `Backup` is not an audit record of what ran.** Editing a schedule changes the apparent
configuration of finished runs, because they reference it. Anything that must be auditable
per run lives in `status`, written at execution time: `status.volumes`, `status.hooks`,
`status.manifests`.

## Naming

A scheduled run is named `<schedule>-<YYYYMMDD-HHMMSS>` in UTC. That exact string is:

- the `ClusterBackup` object's name,
- every child `Backup`'s name, in every namespace,
- the restic `run` tag.

One identifier, end to end. It is what lets discovery and the fan-out converge on the same
object keyed by `(namespace, run)` without coordination — and what makes this work:

```bash
kubectl get backups -A -l crystalbackup.io/cluster-backup=dr-daily-20260730-020000
restic -r $REPO snapshots --tag crystalbackup,run=dr-daily-20260730-020000
```

## Where the two planes differ

The shared run configuration — PVC selector, manifest options, hooks, backoff limit — is
one type, declared once and used by both planes. The cluster plane inlines it alongside its
fan-out fields; the namespace plane declares it on the tenant-facing schedule.

One field is deliberately **not** on the tenant surface: `maxConcurrentMovers`. It is a
cluster-wide cap, checked against every mover Job in the operator namespace, so a tenant
setting it would be setting a platform-wide limit.

That asymmetry is the general rule for what belongs on a tenant-facing surface. Every field
added there becomes something a namespace user can make the operator do on their behalf —
which is why hooks in particular required a whole identity mechanism before they could be
exposed. See [Tenancy and isolation](/CrystalBackup/docs/understand/tenancy/).
