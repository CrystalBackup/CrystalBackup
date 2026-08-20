---
title: Upgrading
description: Upgrading the chart, the CRD problem Helm does not solve for you, and what the v1alpha1 API guarantees.
sidebar:
  order: 10
---

## What a version number means here

The project follows SemVer on major `0`: each milestone is a **minor** release
(`M_n` → `0.n.z`), hardening iterations are **patches**. One version string covers the
operator image, the mover image, the sync image and the chart's `appVersion` — they are one
release train and are meant to move together.

The CRD API is **`v1alpha1`**, and that is not a formality. Fields can be added, renamed or
removed between minor releases until `1.0.0`, which is a deliberate API-stability decision
taken after M9. Read the release notes before every minor upgrade; do not assume a manifest
that applied against `0.5` applies against `0.6`.

Patch releases do not change the API.

## The CRD problem

**Helm installs CRDs on first install and never upgrades them.** That is Helm's behaviour,
not a choice of this chart, and it means `helm upgrade` alone will leave you running a new
operator against old CRDs — which fails in the most confusing possible way: fields you set
are silently pruned by the API server, and the operator reconciles as though you never set
them.

Apply the CRDs yourself, before the chart:

```bash
# Pull the chart and take its CRDs. Use the version you are upgrading *to*;
# 0.6.6 is the current release.
helm pull oci://ghcr.io/crystalbackup/charts/crystal-backup --version 0.6.6 --untar
kubectl apply -f crystal-backup/crds/

# Then upgrade the operator.
helm upgrade crystal-backup \
  oci://ghcr.io/crystalbackup/charts/crystal-backup \
  --version 0.6.6 \
  --namespace crystal-backup-system
```

`kubectl apply` on CRDs is additive and safe: it adds new fields and never drops stored
objects.

## 0.6.6 → 0.6.7: there IS a schema step this time, and a self-check that condemned your volumes may stop

**Do not carry the conclusion of the section below over to this one.** `0.6.5` → `0.6.6` had no
CRD change, and it says so in as many words; this hop has one. `ClusterBackup.status` gains
**`blockedReasons`**, a new optional list, so the CRDs must be applied **before or with** the
chart. Skip it and the API server prunes the field on every status write while the operator goes on
computing it — the field simply never appears, and the run tells you nothing about why it blocked
what it blocked.

The scope is exactly that one field and the type under it. Only
`config/crd/bases/crystalbackup.io_clusterbackups.yaml` moved; no other CRD changed, nothing was
renamed, nothing was removed, and no `spec` field anywhere gained or lost a validation. Apply the
CRDs before the chart as above, with `--version 0.6.7`.

Each entry of `blockedReasons` is one **cause**, not one namespace — `reason` (a stable token:
`OwnChild`, `ForeignParentUID`, `DiscoveryProjection`, `UnstampedTerminalChild`,
`UnstampedWithResults`), `namespaces`, and two counts that are the point of the field:
`withDataAtCoordinate`, how many of those coordinates nevertheless hold a Backup carrying snapshots,
and `stampedByThisRun`, how many hold an object stamped with this run's own UID. The list is keyed
by cause precisely so it stays short, which is what lets it account for **every** blocked namespace
where the ten-entry `status.failures` list can only sample:

```bash
kubectl get clusterbackup <name> -o \
  jsonpath='{range .status.blockedReasons[*]}{.reason}{"\t"}{.namespaces}{"\t"}{.withDataAtCoordinate}{"\t"}{.stampedByThisRun}{"\n"}{end}'
```

**No classification changed.** A namespace blocked on `0.6.6` is blocked on `0.6.7`, for the same
reason, and `status.namespacesBlocked` still holds the same total — the branches were instrumented,
not rewritten. The field is absent when nothing was blocked.

**The self-check's coverage census stops condemning volumes it was merely not allowed to look at.**
On a real cluster the resident soak collector reported `30 of 30` PVCs as `ExposerUnresolvable`,
under a headline saying they would **NOT** be backed up, for nine consecutive nights — while 28 of
them were being backed up successfully every one of those nights. Every number was arithmetically
correct and its meaning was false: the collector's ClusterRole had no read on
`persistentvolumes`, which is where the exposer has resolved a bound PVC's driver from since
`0.6.5`, so the resolver's read was refused and the refusal was recorded as a fact about the
storage. `0.6.7` gives a refused or failed read its own class, **`ExposerUndetermined`**, keeps it
out of the "volumes that will NOT be backed up" count, and states it as its own clause instead. The
same rule now covers the selection counts: a schedule or namespace list that could not be read no
longer renders as *"selected by NO schedule"*. **So if you had learned to ignore that headline, read
it again after upgrading** — what is left in it is the part that was always real.

**The chart's soak collector ClusterRole gains two read-only rules, and this is the one upgrade
action that is not automatic.** `persistentvolumes` (`get`, `list`) in the core group, and
`storageclasses` (`get`, `list`) in `storage.k8s.io` — the second for PVCs that are not bound, where
the class is the only evidence of a driver there is. Install the chart's RBAC and you get them. If
you pin, vendor or hand-write that ClusterRole, **add the two rules yourself**; without them the
census still degrades, just honestly now, reporting `ExposerUndetermined` instead of condemning
your data. The collector's ServiceAccount is `<release>-soak`:

```bash
kubectl auth can-i list persistentvolumes \
  --as=system:serviceaccount:crystal-backup-system:crystal-backup-soak
kubectl auth can-i list storageclasses \
  --as=system:serviceaccount:crystal-backup-system:crystal-backup-soak
```

None of this concerns you if `soak.enabled` is `false`. The operator's own ClusterRole is unchanged
and always held both grants — that is why the backups were fine while the report said they were not.

**Two message shapes changed, and a script or an alert could be matching on either.** The
`ConcurrencySkip` Warning now **quotes the PVC name**: `PVC data is Uploading` becomes
`PVC "data" is Uploading`, on both planes and in all four in-flight reasons. That is not cosmetic —
the soak export substitutes an identifier out of free text only where it is quoted and matches
exactly, and a PVC routinely called `data` or `backups` cannot be substituted safely on any looser
rule. And the cluster fan-out's `RunNameCollision` Warning is now **one Event per pass**, naming the
count and the per-cause breakdown, rather than one Event per collided namespace per pass: a run
blocking thirty-two namespaces was writing thirty-two Warnings per reconcile and evicting the rest
of that namespace's events well inside the hour they live. The per-namespace text moved to the
operator log line, untruncated, with `reason` and `facts` as structured fields, and a sample stays
in `status.failures`. That message also now **leads** with a bracketed fact block —
`[class=… stamp=… phase=… data=… age=…]` — in front of the prose, and the status message cap rose
from 256 to 384 runes to fit it. Anything anchored to the start of that string should look.

**`selfcheck` JSON gains one key and one class value, both additive.** `coverage.selectionUndetermined`
is a new boolean beside the existing counts, and `ExposerUndetermined` is a new value a
`coverage.classes[].class` can take. Nothing was renamed and no key was removed, so the only thing
to check is a parser that rejects unknown JSON keys or enumerates class names exhaustively — the
soak kit's own CronJob is unaffected.

**Nothing about backups themselves changed in this release.** No selection, snapshot, mover,
retention or restore behaviour moves; no data moves; no repository is touched. What changed is what
the product *says* about runs it was already doing, in one field, one report and two messages.

## 0.6.5 → 0.6.6: nothing to apply, but a failing backup now takes longer to say it failed

**There is no CRD or API change in this release.** No field was added, renamed or removed on any
type, so unlike every section below this one, this hop has **no schema step**. Run the `kubectl
apply` above anyway if it is already in your procedure — applying an unchanged CRD changes
nothing — but do not go looking for the new fields; there are none. What changes is behaviour,
in five places, and two of them change an **outcome** a dashboard or a script can be watching.

**An aborted quiesce now releases the applications it froze before it reports `Failed`.** On
`0.6.5`, a pre hook failing under `onError: Fail` stopped the chain — correctly — and wrote the
terminal `Failed` immediately. The hooks that had **already succeeded** had frozen their
applications, and the already-terminal short-circuit meant nothing ever thawed them: the run
reported *the quiesce did not work* while part of it had worked and was still in effect. `0.6.6`
runs the release **first** and holds the terminal write while a thaw is owed. The verdict is
unchanged — the terminal reason is still `PreHookFailed` — but **the Backup now stays
non-terminal while the thaw retries**, within the same three-attempt budget the unfreeze path
already had, carrying a `Ready` reason of `ReleasingAfterAbortedQuiesce` that names the abort and
the attempt. A thaw that keeps failing still ends at the existing `UnfreezeFailed` Warning Event
and only then reports `Failed`. **So if you alert on the terminal phase, that alert now arrives
later** — seconds to a couple of reconciles, not minutes — and the intervening state is the
retry, not a stall:

```bash
kubectl get backup <name> -o \
  jsonpath='{.status.phase}{"\t"}{range .status.conditions[?(@.type=="Ready")]}{.reason}{end}{"\n"}'
```

Only `Succeeded` pre entries are thawed. A `Skipped` hook never ran, and a thaw against a pod
nothing froze is a command its owner never asked for.

**A `Fail`-policy failure in a *post* hook no longer skips the rest of the chain.** Stopping at
the first failure is right for the pre phase and wrong for the post phase, where every entry is a
thaw owed to a **different** application: a permanently broken first post hook meant the later
pods were never thawed at all, across all three attempts. Loud — `UnfreezeFailed` did fire — but
about the wrong pod. The remaining post hooks are now attempted regardless.

**A clean restore can no longer be re-reported as maybe-broken, and its counters can no longer
drop to 0.** Two passes in the restore controllers discarded work on an error return. One
re-derived the manifest apply's verdict from a mover pod that was already gone, turning a clean
apply of a whole namespace into *"did not report a result … some resources may have been
applied"* with `failedCount 1`. The other published an empty volume tally when it could not read
the mover census, overwriting a real four-of-six with `plannedVolumes: 0`. **Both were wrong
answers rather than missing ones**, in front of somebody deciding whether to abandon a restore.
`plannedVolumes` and `failedVolumes` now hold their last published values when a pass cannot
recount them, and the condition says why — so a panel keyed on them stops flickering to zero
mid-restore.

**A `ClusterErasure` waiting out an object lock stops emitting four Warnings a minute.** The
`ErasureBlocked` Warning fired on every pass of a 15-second recheck — for weeks, on the most
sensitive compliance path there is, drowning the record somebody points at to assert data was
destroyed. It now fires on the **transition**, and the recheck cadence is the hourly one that was
always configured and never once used. Nothing about the erasure's decision changed; if you built
anything on the Event's *rate*, it is now one Event per transition.

**`selfcheck` and `preflight.sh` gain one new observation, and it is purely additive.**
VolumeSnapshots that are **bound** to a content and still not ready after an hour are now
reported: a `stuckSnapshots` object beside `leakIndicators` in the JSON, a
`stuckSnapshotsOnStorageClass` qualification on `0.6.5`'s per-PVC coverage census, and the same
finding in the preflight script. This exists because a cluster was found that had **never once**
backed up a CephFS volume — the product's verdict was right every night and no artefact said it
had never worked, while `preflight.sh` called the class perfectly usable. It is deliberately an
**observation, not a verdict**: nothing becomes `Skipped` or `Failed`, no phase moves, and a
StorageClass with no snapshots at all is not maligned. The only thing to check on upgrade is a
parser that rejects unknown JSON keys.

**Two new `soak` values, both defaulting to exactly today's behaviour.** `soak.accessModes`
(default `[ReadWriteOnce]`) and `soak.storageClassName` (default `""`, the cluster default). They
exist together for one case: an RWX class removes the exclusive-volume handover that lost a
fortnight's archive when an autosync replaced the collector pod — the chart's
`strategy: Recreate` did what it promises and the archive was unreachable anyway. Setting the mode without a class
that provides it is a PVC that never binds. The collector also now writes its per-class
high-water table to its **own stderr** on `SIGTERM`, which is the one channel a terminating pod
has that is not the volume it is about to release. If `soak.enabled` is `false`, none of this
concerns you; if it is `true`, read `hack/soak/README.md`'s reset procedure before you replace
that pod on purpose, and export the archive first.

## 0.6.4 → 0.6.5: a panel that read a number can now read 0, and a backup that failed can now complete

Nothing to do before the upgrade beyond applying the CRDs, and no data moves. But two things
change an **outcome** rather than a wording, and one of them will make an existing dashboard go
quiet about namespaces that are still unprotected. Read this before you conclude the upgrade
fixed something it did not.

**A counter that was one field is now two.** On `0.6.4` a namespace whose fan-out coordinate
collided was counted as *failed* — the same field used for a namespace that was attempted and
failed, so neither number could be believed. `0.6.5` gives the collision its own counter,
`status.namespacesBlocked`, with its own metric
`crystalbackup_clusterbackup_namespaces_blocked`, and `namespacesFailed` now counts only
children that really failed. **A dashboard or alert keyed on
`crystalbackup_clusterbackup_namespaces_failed` alone will therefore read 0 where it used to
read a number** — the namespaces are just as unprotected, and the panel that said so has stopped
saying it. Add the blocked series beside the failed one, ideally before you upgrade:

```bash
kubectl get clusterbackup <name> -o \
  jsonpath='{.status.namespacesSucceeded}/{.status.namespacesFailed}/{.status.namespacesBlocked}{"\n"}'
```

**`onError: Continue` on a pre hook is now honoured, and that is a change in outcome.** `0.6.4`
recorded `Failed` for every hook failure regardless of the policy, so a user who had explicitly
asked for the backup to proceed past a failed quiesce got a terminally failed backup and no
snapshot at all. The same run on `0.6.5` **completes, with a snapshot**, and carries a new
`ApplicationConsistent` condition set `False` with reason `CrashConsistent`, naming the pod,
container and error. That is the documented contract and it is what the field was always for —
but if you set `onError: Continue` and have been treating the hard failure as your signal, the
signal is now a condition on a `Completed` Backup, and the restore point it describes is
crash-consistent:

```bash
kubectl get backup <name> \
  -o jsonpath='{range .status.conditions[?(@.type=="ApplicationConsistent")]}{.status} {.reason}: {.message}{"\n"}{end}'
```

The condition is deliberately tri-state: **absent** when no pre hook ran, so a hookless backup
does not acquire a `False` it cannot act on.

**A new `critical` alert can fire.** `CrystalbackupBackupMissedCritical` escalates on magnitude,
bounded at three times the schedule's own period plus an hour — 4 h for an hourly schedule, 73 h
for a nightly one. **No existing threshold moved**, and the warning tier is unchanged and fires
alongside it, so nothing you already route is narrowed. But if your Alertmanager treats
`critical` differently from `warning` — a page rather than a ticket — a cluster that has produced
nothing for three schedule periods will now page. There is also a new
`crystalbackup_restore_volumes_failed`.

**New status fields, all additive and all optional.** Nothing is renamed and nothing is removed:

- `status.volumes[]` gains `firstAttemptAt` and `phaseEnteredAt`;
- `status.hooks[]` gains `onError` — the policy that was in effect for that execution. Empty is
  read as `Fail`, which is what makes upgrading over a Backup already inside its freeze window
  safe: entries written by the old operator abort exactly as they did before rather than becoming
  tolerated by a newer binary;
- `Restore` and `ClusterRestore` gain `plannedVolumes` and `failedVolumes`, stamped on
  non-terminal passes too, so a long restore visibly progresses;
- `ClusterErasure` gains `snapshotsTargeted` and `snapshotsRemaining`;
- `ClusterBackup` gains `namespacesBlocked`.

Apply the CRDs before the chart, as above. Skip that and the API server prunes every one of them
and the new operator reconciles as though you never had them.

**Exposure objects gain a `crystalbackup.io/backup` label, and pre-upgrade residue does not have
it.** Any leaked snapshot or content left behind by `0.6.4` or earlier carries no such label, so
the inline teardown does not match it; it is collected by the **orphan reaper** instead, on its
own sweep, by exclusion — it reaps only when no Backup in that namespace could still want an
exposure of that PVC, and refuses on a list it cannot read. So expect old leftovers to clear
within a sweep or two rather than at the next backup's teardown, and expect the reaper to say so.
Nothing force-removes another controller's finalizer; an object genuinely stuck is now reported as
stuck, with the finalizers named, rather than logged as reaped.

**`selfcheck` gains `--format text`** — a compact plain-language report including a per-PVC
coverage census, which is the first time the product can answer *what will and will not be backed
up*, including PVCs that no schedule selects at all. **JSON remains the default**, so anything
that parses `selfcheck` output — the soak kit's unattended CronJob included — is unaffected.

## 0.6.3 → 0.6.4: a location that reported Ready can now report Degraded

Nothing to do before the upgrade, and no data moves — but the new operator can report a
location as not-Ready where the old one reported it healthy, so read this before you conclude
the upgrade broke something.

`0.6.4` makes the wrapped-DEK escrow pass **block repository provisioning in every state it
cannot positively prove safe**. Two states are safe: an in-cluster DEK already exists, so
nothing can be minted; or there is provably no DEK anywhere, so minting is what should happen.
Everything else now blocks and sets `Ready=False` with reason `DEKEscrowUnresolved`, phase
`Degraded`, and condition `DEKEscrowed` carrying which case it is:

```bash
kubectl get clusterbackuplocations
kubectl get clusterbackuplocation <name> \
  -o jsonpath='{range .status.conditions[?(@.type=="DEKEscrowed")]}{.reason}: {.message}{"\n"}{end}'
```

If one goes `Degraded` on the first reconcile after the upgrade, **the state it names was
already true on `0.6.3`** — it was simply not blocking anything, which is the defect this
release fixes. Three of them are worth knowing:

- **`EscrowConflict`** — the bucket object and the in-cluster DEK are both readable and are
  different keys. That is two repository generations, and the bucket copy may be the only key to
  the older one. On `0.6.3` such a location reported `Ready` while handing a wrong key to every
  mover. Do not delete anything; the bucket object is evidence.
- **`EscrowUnreachable`** — the bucket could not be read and there is no local DEK, so a
  recoverable key may be sitting in there. Usually credentials or endpoint, and it clears on its
  own once the bucket is reachable. Distinct from **`EscrowUnverifiable`**, which is the same I/O
  failure *with* a local DEK present and does **not** block, because there is nothing to mint.
- **`CredentialsUnavailable`** / **`KEKUnavailable`** — the Secret is missing. Restore it; the
  location recovers without intervention.

`EscrowWriteFailed` still does not block: the in-cluster DEK is known-good and only the bucket
copy is behind, which degrades bare-cluster DR rather than your backups.

## 0.6.2 → 0.6.3 under Argo CD: an object stops being rendered

Read this before you sync, not after. It has happened on a real cluster.

In `0.6.2` the chart rendered a `Namespace` object by default (`namespace.create` defaulted to
`true`). In `0.6.3` that default is `false`, and the object is simply gone from the render. Under
Argo CD with automated prune, **an object that stops being rendered is an object that gets
deleted** — that is what prune means, and it does not distinguish between "the author removed
this" and "the author changed the default". So a `0.6.2` → `0.6.3` sync can delete
`crystal-backup-system` and everything inside it, including the Secret holding your cluster KEK
and every wrapped DEK. Nothing in object storage is touched, and every repository those keys
protect becomes permanently unreadable — a
[decommission](https://github.com/CrystalBackup/CrystalBackup/blob/main/docs/DECOMMISSION.md#14-the-key-itself)
executed by accident, during a patch upgrade.

The remedy is to get the namespace out of the Application's prunable set **before** the upgrade:
stop tracking it in the operator `Application` — a separate Application of its own, with prune
off, or exclude it from the sync scope. Once the namespace is not something that Application
renders, no change to `namespace.create` can reach it. Then upgrade. The reasoning, and the
shape to use, are in
[Install with Argo CD](/CrystalBackup/docs/start/install-argocd/#the-namespace--yours-not-the-charts).

The same hazard applies to a Flux `HelmRelease` with pruning enabled, and to any other
reconciler that treats "no longer rendered" as "delete". After the upgrade, `namespace.create`
should stay `false` permanently: a namespace Helm owns is a namespace a prune or a
`helm uninstall` can take, with the keys inside it.

## Before you upgrade

**1 — Let in-flight work finish.** An upgrade restarts the operator, which is safe by
design — mover Jobs have deterministic names and are re-adopted rather than restarted — but
there is no reason to do it during a prune window.

```bash
kubectl -n crystal-backup-system get jobs -l app.kubernetes.io/managed-by=crystal-backup
kubectl get clusterbackups
kubectl get restores,clusterrestores -A
```

**2 — Know where your keys are.** An upgrade does not touch them, but the moment you
discover your KEK escrow is stale should not be the moment you need it.

**3 — Read the release notes.** Particularly for a minor bump.

## During

```bash
kubectl -n crystal-backup-system rollout status deploy/crystal-backup
```

The operator restarts. If you run more than one replica, leader election keeps exactly one
active and the rest are warm standbys, so the restart is a leadership handover rather than
an outage.

Nothing in object storage is touched by an upgrade. Repositories are not migrated, keys are
not rotated, and no data moves.

## After

```bash
# The operator is up on the new version.
kubectl -n crystal-backup-system get deploy crystal-backup \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'

# Locations are still Ready.
kubectl get clusterbackuplocations
kubectl get backuplocations -A

# Repositories are still reachable.
kubectl get backuprepositories
```

Then let one scheduled backup run and check it completed. An upgrade you did not verify
with a real backup is an upgrade you have not verified.

## Downgrading

Not supported, and worth being blunt about. New CRD fields written by a newer operator are
unknown to an older one; the API server prunes them on the next write, and the older
operator reconciles against a truncated spec.

If you have to go back: uninstall, re-apply the older CRDs, reinstall the older chart, and
re-create your custom resources. The **repositories are unaffected** — that is the point of
the repository being the source of truth. Discovery will project the backups again.

Follow the ordered [uninstall procedure](/CrystalBackup/docs/start/install/#uninstall) for
that first step. Removing the operator while a `Backup`, `Restore` or location still carries
its finalizer strands the namespace in `Terminating` permanently, and a downgrade is exactly
the moment you will be deleting objects in a hurry.

## Upgrading across several minors

Go one minor at a time (`0.3` → `0.4` → `0.5`), applying each release's CRDs and letting
a backup cycle complete in between. Skipping minors on an alpha API is how you find out
which migration you needed.

## Images

Production references images **by digest**, never by tag. The chart's published values
carry the real digests for that release; a chart installed from a source checkout carries a
placeholder and will not pull.

```bash
kubectl -n crystal-backup-system get pods \
  -o jsonpath='{range .items[*]}{.spec.containers[0].image}{"\n"}{end}'
```

The mover and sync images are pinned separately and are passed to every mover Job. They
move with the operator, so a partial upgrade — new operator, old mover digest — is
something to avoid rather than something to try.
