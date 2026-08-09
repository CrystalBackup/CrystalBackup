# Changelog

All notable changes to Crystal Backup. Versioning follows
[adr/0014](spec/adr/0014-versioning-and-release.md): milestone `Mn` → minor `0.n.z` on
major 0; `1.0.0` is a deliberate post-M9 API-stability decision.

## 0.6.5 — One volume held the queue, and every night after it was skipped (unreleased)

> **Campaign not yet run.** This section is written as the lots land; the crucible verdict and the
> report link go in when the campaign has actually passed, not before. A release note that describes
> a campaign it has not seen is the exact habit this project spent M6 removing.

A nightly schedule on a live cluster produced nothing for **thirty-one hours**. Nothing crashed,
nothing alerted above `warning`, and the operator's own dashboard read *0% backup success* — which
was true, and which nobody could act on because every number around it was either silent or wrong.

The cause was one `PersistentVolumeClaim`, in one namespace, out of thirty-three.

`develop/recette4-back`: `Bound`, 200 GiB, a hand-made **NFS** `PersistentVolume`, naming
`storageClassName: slow` — a StorageClass that does not exist as an object. That is not a
misconfiguration. For a static binding, `storageClassName` is only a matching **label** between the
claim and the volume, and Kubernetes never requires the class object to exist. The administrator had
done nothing wrong.

The operator resolved that PVC's snapshot capability through its StorageClass, got
`StorageClass "slow" not found`, and returned an error. `Reconcile` advances one volume per pass and
picked the first non-terminal one by position, so the error re-drove the same volume forever and the
five volumes behind it in that namespace **were never attempted at all** — which is why they carried
no reason, and why not one of 0.6.3's three phase deadlines could fire on them: a deadline needs a
phase, and they had never left `Pending`. The `Backup` never went terminal, so its `ClusterBackup`
did not either, so the schedule's `concurrencyPolicy: Forbid` skipped every following night.

### The StorageClass was never the volume's identity

The fix is not a better error path. It is that the question was being asked of the wrong object.

A StorageClass's `provisioner` is immutable, which makes the class look like a stable identity. It
is not: the class can be **deleted and re-created under the same name over a different backend**,
which is a normal step when migrating a cluster's storage. Every `PersistentVolume` provisioned
under the old class keeps `spec.storageClassName` pointing at that name — a dangling string that now
resolves to the **wrong driver**. Resolving through it does not fail; it succeeds and is wrong. An
NFS volume routes to the RBD exposer, a `VolumeSnapshot` is cut that can never become ready, and two
hours later `SnapshotReadyDeadlineExceeded` blames the storage. A coherent, actionable, entirely
false diagnosis.

`Registry.For` now takes a bound PVC's CSI driver from its **PersistentVolume**, and consults the
StorageClass only for a PVC bound to nothing — where the class is the only evidence available, and
where there is no data to back up yet either. A PV with no `spec.csi` and no
`pv.kubernetes.io/migrated-to` is not a CSI volume, so it is `ErrUnsupported` → `Skipped` /
`CSISnapshotUnsupported`: terminal in the queue, neutral in the roll-up. The honest verdict about a
plain NFS volume and the verdict that lets its namespace through turn out to be the same one.

### The fix that was rejected, because it guessed

The first version of this classified the error: a `NotFound` became an immediate `Failed`, on the
reasoning that a missing reference is a configuration fact no retry can change. That is wrong in
both directions. A StorageClass absent this second can be created the next; a bound PVC whose PV
reads `NotFound` may be looking at a cache that has not caught up — this operator has been bitten by
exactly that before. And an error that looks transient can be permanent. Every such guess is wrong
sometimes, and the direction that hurts is failing a volume whose data was fine.

So nothing judges permanence any more. A volume whose exposer cannot be resolved records the cause,
**stays `Pending`, and does not error the reconcile**. Two mechanisms replace the guess, and neither
is sufficient alone:

- `firstNonTerminalVolume` prefers a volume that has not been tried over a **parked** one (`Pending`
  with an `ExposerUnresolvable` reason already recorded). A broken volume can no longer starve
  healthy ones; it keeps its turn once the queue drains. This is the rule stated by the product
  owner and it is the point of the release: *blocking a backup that could have succeeded, because
  something scheduled ahead of it is broken, is not acceptable.*
- **`pendingResolveDeadline` (1 h)** — the fourth deadline, bounding the one phase the other three
  left unbounded. They each hang off an object that carries its own creation time (the origin
  `VolumeSnapshot`, the mover `Job`); a volume that never created anything had no clock at all. Its
  clock is a new `status.volumes[].firstAttemptAt`, stamped on the first **attempt** rather than at
  enumeration — volumes wait their turn, and a clock shared with the run would have failed the next
  volume the instant it reached the head of the queue, having never been tried.

The recorded cause survives the deadline: a volume failed by the clock keeps
`ExposerUnresolvable: <cause>` rather than being overwritten with the deadline's own name, because
the cause is what an administrator can act on and the clock is not.

### The published preflight script was predicting the wrong thing, and its guard was blind

`preflight.sh` promises administrators, per StorageClass, which exposer CrystalBackup would choose.
After the change above that framing is incomplete: it is still right for **dynamically** provisioned
volumes, and it cannot speak for a volume bound by hand. The table now says so where the table is
read, and a new `bound-volumes` check reports every bound PVC whose PV contradicts — or is simply
absent from — the class it names. A PVC naming a class that does not exist is reported explicitly as
**normal and nothing to correct**, because it is.

Worse than the drift was what did not catch it. `make preflight-table-verify` exists to make exactly
this impossible, and it stayed green — because every probe it drove had an effective driver identical
to its `.provisioner`, so the scripts' *derivation* of a driver was invisible to the guard. When
`driverFor` learned to prefer `pv.kubernetes.io/migrated-to`, a CSI-migrated in-tree class would have
been printed **DATA SKIPPED** for volumes the operator snapshots perfectly well. The guard now probes
a class whose serving driver differs from its provisioner, and the derivation is **generated into
both scripts** instead of hand-written in each. The truth used to seed the fake cluster and the model
of what the scripts derive are deliberately separate functions: collapsing them would make the two
agree by construction rather than by being right, which is how it went blind in the first place.

A guard that misses the drift it exists to catch is a worse defect than the drift.

### `Forbid` now tells a run that is working from a run that is wedged

The thirty-one hours were not caused by the stuck volume alone. The run stayed non-terminal, and
`concurrencyPolicy: Forbid` asked only *"is a previous run non-terminal?"* — which is right when the
previous run is working and catastrophic when it is not. Every following night was skipped.

`Forbid` now skips the new run only while the previous one is genuinely progressing, and otherwise
terminates it and lets the new one start. The predicate needs **both** halves, and neither alone can
authorise a kill:

- **Nothing is legitimately in flight.** `Uploading` counts as in flight **unconditionally** — no
  elapsed time overrides it, because nothing in the product bounds a running mover *by design*, and a
  multi-terabyte first full legitimately runs for hours. `Snapshotting` likewise. **`Pending` is the
  only phase judged**, because it is the only one whose clock lives on the volume; a
  `firstAttemptAt` of nil means *queued*, not stuck.
- **No progress for four hours**, measured on a progress clock rather than the run's age. The four
  hours are **derived** from the deadline ladder (`pendingResolveDeadline` + `snapshotReadyDeadline`
  + `moverStartDeadline` = 3h30m), and a test pins the derivation so the number cannot drift away
  from the bounds it is supposed to cover.

A slow upload therefore cannot be killed at any age: the first clause rejects it on the phase alone,
and the second is a *floor* that can only delay a kill the first has already authorised. A killed run
is never given a success phase, so `lastSuccessTime` cannot advance, and each volume keeps its own
recorded cause with the termination prefixed to it — so *"we shot a stuck run"* is distinguishable
from *"it failed"*. The fix applies to **every** `concurrencyPolicy` value rather than to `Forbid`
alone, because `Forbid` and `Skip` are identical in this build and branching would have let the
defect be re-armed by choosing the other value.

### `namespacesFailed: 32` for a run whose children were 29 Completed and 3 PartiallyFailed

The same run reported 32 failed namespaces. Not one of those 32 had failed.

The children had been fanned out before `crystalbackup.io/parent-uid` existed, and the operator was
upgraded while the run was in flight. `classifyCoordinate` admits into its adoption window only a
child holding no result of any kind, so every one of the run's 32 **terminal** children was
classified a foreign occupant of its coordinate — and each collision incremented
`NamespacesFailed`, a field that a second, independent site also fed from the child phases. "This
run never backed up this namespace" and "it tried and failed" were the same number.

Every namespace count now comes from **one** classification, in one pass, with a total the
controller checks on every write (and an `AggregateInconsistent` Warning if it ever fails to add
up). `namespacesBlocked` is a new, separate counter, with its own metric — because
`namespaces_failed` alone would now read 0 over 32 unprotected namespaces. `Skipped` stays neutral
end to end.

**A sum invariant would not have caught this, and that is the lesson worth keeping.** The published
numbers *did* add up: 0 + 32 + 1 = 33. Only reading the counters back **against the children they
claim to summarise** finds it. Three further honesty defects fell out of the same rewrite: children
are now matched at the run's coordinate rather than by namespace key alone; a stamped child in a
de-selected namespace keeps its success and its bytes instead of vanishing; and a child phase the
roll-up does not recognise now reads `Running` rather than counting for nothing and letting the run
report `Completed`.

The trigger itself — a run unable to recognise its own unstamped terminal child — is deliberately
**not** fixed here. It is a migration artefact that cannot recur once a stamping build has fanned
out, and the plausible fix reopens the guard against a run reporting success for data it never
wrote. It is recorded in the roadmap's backlog with the reasoning, and it needs its own campaign.

### The reaper logged "reaped" 186 times for three objects it never deleted

Every ten minutes, for thirty-one hours, the orphan reaper logged that it had reaped three
`VolumeSnapshot` objects. It had not. Their deletion was stuck on the snapshot pair's
bound-protection finalizers, so each sweep re-issued the `DELETE` and logged success again. The
operator's own leak detector was right the whole time — it counts what is *there* — so the same
binary shipped a component asserting a leak was gone and another reporting eight residual objects,
the oldest 31 hours old.

A successful `DELETE` means *accepted for processing*. With a finalizer, that and *gone* can be
separated by forever. Only a confirmed absence may now be called a reap: the reaper reads the object
back through an uncached reader and distinguishes **confirmed gone**, **deletion requested but
unconfirmed**, and **stuck** — the last naming the finalizers holding it, as a Warning Event on the
object itself (so `kubectl describe` explains its own `Terminating`) and as a new
`crystalbackup_orphan_reap_stuck{kind}` gauge published for every kind, including at zero. An object
already terminating gets no new `DELETE`. Nothing force-removes a finalizer: stripping
external-snapshotter's own finalizer is a destructive act on another controller's contract, and this
release is about honest reporting, not forced removal.

### `selfcheck` answers what an administrator actually asks after installing

`selfcheck` produced a JSON installation report whose inventory contained **no PVC information at
all**, so the one question a fresh installer has — *what will and will not be backed up* — had no
answer anywhere in the product.

`--format text` renders a compact answer in three parts: whether anything is wrong, **what the CRs
will do** in plain sentences (including each cron translated into words *and its next occurrence*,
retention in words, and the maintenance windows), and a per-PVC census with the treatment class.
The classes are not a mapping table: they are `SnapshotExposer.Kind()` and the controller's own
reason constants, with a test that parses the controller and fails on a rename. `csi-generic` and
`cephfs-shallow` are the "with copy" and "without copy" the shape of the question implies, and
**"best-effort" is absent**, with a sentence saying this version has no filesystem fallback — a class
the operator cannot deliver would be worse than no class.

The census also reports what nothing else could: PVCs **selected by no schedule at all**, and PVCs
selected only by a schedule that cannot fire, which are *covered on paper and unprotected in fact*.
It costs `5 + k` API requests regardless of how many PVCs exist — measured, printed in the output,
and asserted equal at 10 and at 400 PVCs — by putting a read-through cache under the real resolver
rather than reimplementing it. Secrets are only ever fetched by name, never listed, which is exactly
the right the chart grants.

JSON remains the default output. The soak kit redirects a bare `selfcheck` into a `.json` file,
including in a shipped CronJob built to run unattended for months, and flipping the default would
have made it write text into that file silently.

### Thirty-one hours without a backup was a `warning`

The self-check on the affected cluster read *"2 rule(s) breached, none critical"*. Both breached
rules were `warning`, so a cluster producing nothing for thirty-one hours was indistinguishable, in
severity, from one running an hour late.

`CrystalbackupBackupMissedCritical` escalates with magnitude, and its bound is **derived** from the
same per-schedule source as the warning's — `3 × the schedule's own period + 1h`, so it is 4 h for an
hourly schedule and 73 h for a nightly one instead of one hardcoded number for both. The warning's
`26h` fallback became `missedFallbackPeriod + missedFallbackGrace` for the same reason, at the same
value: **no existing threshold moved**, and a test pins all three constants so an accidental change
fails the build. Both tiers fire together; narrowing the warning would have silenced an
administrator's existing routing at the moment things got worse.

`BackupStalled` deliberately gets **no** escalation. A stall says *a* run has not finished, not that
*none* has — under `concurrencyPolicy: Allow` a wedged Backup sits beside successful nightlies. And
there is nothing honest to scale it by: an in-flight duration is a property of the volume, not of the
cron, so scaling by period would send a 3 TB first full critical after twenty minutes. The real
answer is a per-volume throughput series, not a multiple of eight hours.

The verdict logic needed no change and never had the defect: it already escalated on any critical
breach. The defect was that **no rule in the table could ever be critical about missing backups**, so
the escalation was unreachable — which is why the test for it now goes through the real rule table
instead of a synthetic result that could stay green while the incident was live.

### A failed erasure claimed it had destroyed data

`ClusterErasure.status.snapshotsForgotten` was written from the count taken **before** the work and
never re-derived. An erasure that failed therefore reported having forgotten N snapshots. This is not
an operational annoyance: an erasure object is the compliance record somebody points at to assert
that data was destroyed.

Every number is now either measured after the work or the conservative floor, never the optimistic
assumption. The pre-erasure count goes to a new `snapshotsTargeted`; `snapshotsForgotten` stays 0
while running; on failure the controller **re-lists the repository** under the erasure's own tags and
derives what was actually forgotten, publishing the residue. Four of ten reads 4 forgotten and 6
remaining. A prune that failed after a successful forget reads 10 and 0. A residue that cannot be
listed claims **nothing**.

`Restore` and `ClusterRestore` gained the denominator they never had — `plannedVolumes` and
`failedVolumes`, stamped on non-terminal passes too, so a long restore visibly progresses instead of
reading 0 until it ends. A restore is the operation people run on their worst day; *how far along is
it and what did not come back* has to be answerable from `kubectl get restore`.

### The same defect, three times, and what closing it structurally required

The head-of-line block was found three times in three lots, in the same family of functions, which is
the signal that the per-call-site shape was the defect rather than any individual site. `Reconcile`
returned a volume's error at step (10), **before** the status write at step (11) — so nothing that
pass computed was ever persisted. Reverting the fixed `Expose` path in a mutation test did not merely
lose a timestamp: the assertion that failed was that `status.volumes` was **nil**. The enumeration
itself never reached etcd. And because the error returned upstream of the deadline evaluation, the
two bounds that exist to end exactly this were unreachable on the one path that needed them.

A failure to advance a volume now records its cause and lets the pass persist what it knows. The
volume's **phase is deliberately untouched**, since that is what the next pass dispatches on and what
its durable clock is measured against. A failed readiness check is treated as *not ready*, which is
what makes `snapshotProgressDeadline` and `snapshotReadyDeadline` reachable when the check itself
cannot be carried out — and only a clean `ready == true` still creates a mover Job, so the change
cannot manufacture one. `advancePending` no longer has an `error` result at all: the signature is now
the invariant, and reintroducing the bug requires changing it.

Propagating the error after persisting was considered and rejected, for a reason worth recording:
the backoff would be charged to the wrong object. It is the *Backup* that requeues, so one durably
failing volume would stretch the poll driving every **other** volume in that namespace from five
seconds to the rate limiter's ceiling — the same head-of-line block moved into the time domain.

Every remaining instance in this controller was then closed, in two further passes, because a
guarantee that holds on three paths out of four is not a guarantee:

- **step (9b) `openFreezeWindow`** — a durable hooks-resolution failure (RBAC narrowed under a
  running operator) discarded `status.volumes` entirely and the run never terminated: the
  thirty-hour signature with nothing per-volume to read. It now routes into `failHooks`, the
  mechanism that already knows how to record a failed quiesce and decide its consequence, whose own
  status write is what finally persists the volumes. Deliberately without retries: the API's own
  `postHookAttempts` doc states that post hooks are retried where pre hooks are not, and giving a
  failed pods-*list* patience that a failed pods-*exec* does not have would be incoherent. R16 is
  kept — `Expose` is never reached, so a quiesce that did not happen never becomes a backup claiming
  consistency;
- **steps (10b) `advanceManifests` and (10c) `closeFreezeWindow`** — persist first, then propagate.
  Here the shape rejected for volumes is the right one, because the failure is not per-volume: the
  backoff is charged to the Backup, which *is* the object at fault, and `reconcile_errors_total`
  keeps the observation;
- **the terminal phase is now held while a freeze-window release is owed**, the way it was already
  held while the manifest half was in flight. Without it, a run whose volumes all went `Skipped` or
  `Failed` went terminal on the very pass the release fired, so a failed release got one attempt
  where the product promises three — and the loud `UnfreezeFailed` Event, which only fires once the
  attempts are gone, was unreachable. An application could be left quiesced with no loud signal;
- **the four unbounded sites** gained a backstop: a new `status.volumes[].phaseEnteredAt`, a
  *sibling* of `firstAttemptAt` rather than a generalisation of it, because a field re-stamped on
  every transition cannot also be the once-on-first-contact clock `pendingResolveDeadline` needs —
  merging them would have quietly turned that deadline into the enumeration clock its own comment
  was written to avoid. `advanceRetryDeadline` is four hours, sized so it never pre-empts a bound
  that can give a better answer: the longest sibling is two hours, and a test asserts the whole
  ladder stays strictly under it so the safety argument cannot rot. It is consulted at exactly the
  six sites where no per-object bound is reachable, and **never** at "still running; requeue" — a
  mutation that added it there is one of the tests.

### Two documented hook contracts the controller did not honour

`onError: Continue` was documented in two places — the field's own comment says "records the failure
and proceeds", and `Result.Aborts()` implements the rule — and the controller threw the distinction
away: `recordHookResults` wrote `Failed` for every failure regardless of the policy, so the user's
answer was erased at the moment it was written down, and the abort decision is taken on a later
reconcile from status alone. A user who explicitly asked for the backup to proceed if the quiesce
failed got a terminally `Failed` backup and no snapshot at all. Nothing tested it, which is how it
shipped.

The apparent conflict with R16 was not one. R16's argument — a snapshot that looks
application-consistent and is not is worse than none — is already the stated reason `Fail` is the
**default**, not a claim that `Continue` should not exist. Removing `Continue` was weighed and
rejected: it would move the outage from a degraded backup to no backup, which is 0.6.4's lesson
inverted.

So `Continue` is honoured, and the policy is now recorded durably (`status.hooks[].onError`, empty
read as `Fail` so an upgrade over a run mid-freeze-window is safe). A backup that proceeded past a
failed quiesce carries a new **`ApplicationConsistent` condition**, written in the same status write
as the hook entries so there is no window in which the run continues with nothing saying so. It is
tri-state on purpose: absent when no pre hook ran (a `False` on every hookless run would be exactly
the alert fatigue this release fights), `True`/`Quiesced`, or `False`/`CrashConsistent` naming the pod,
container and error. Without that record the field would have been a way for users to silently
downgrade their own restore points, which is worse than the bug. The phase stays `Completed`: nothing
happened partially.

The same erasure affected **post** hooks, where it was worse — a tolerated release failure spent all
three retry attempts, held the terminal phase, and paged a human with "an application may remain
quiesced" about a failure the user had said to tolerate. And a run declaring **only** post hooks ran
none of them, because the release was gated on a pre phase having run; the field is "post-backup",
not "unfreeze", and a thaw for an out-of-band quiesce or a "backup finished" command were both
unreachable. Fixing that surfaced a third: hook resolution was gated on *any* hook being declared,
so a post-only run listed pods for a pre phase it never declared — and in a namespace where that
listing is refused, it died terminally complaining about a quiesce it had never asked for.

**What this release does NOT close.** Two instances of the same family remain, both in the hook
chain, and the first is the more serious: when a pre hook with `onError: Fail` fails, the chain stops
— correctly — but the hooks that **already succeeded** have quiesced their applications, and the
terminal `Failed` write means the release never runs. Nothing thaws them and nothing says so, which
is R16's own priority inverted on the path where a human is least likely to be looking. Second, a
`Fail`-policy post-hook failure marks the rest of the chain `Skipped` and a retry restarts it, so a
permanently broken first hook means later pods are never thawed across all three attempts —
`UnfreezeFailed` fires, but about the wrong pod. Both are recorded in the roadmap's backlog with the
reasoning; both need structural changes to the path this release already rewrote twice, and neither
belongs in a twelfth entry of an eleven-lot release. Separately, the errored-pass class was closed
and verified **in the Backup controller only** — `clusterbackup_controller.go`,
`restore_controller.go` and `restore_engine.go` share the same single-status-writer shape and were
never swept. The honest statement is that nobody has looked, not that they are clean.

## 0.6.4 — The operator minted a key over the one that could open the repository (2026-08-07)

**Validated on real infrastructure: 90 of 90 crucible checks, 0 failed, 0 skipped, in 2h44m1s**
— the whole suite, M0 through M6, unfiltered, on a freshly provisioned six-node RKE2 v1.35.7 +
Rook-Ceph cluster with real S3, against the exact operator digest this release ships. Report:
[crucible-m6.4](https://crystalbackup.github.io/CrystalBackup/reports/crucible-m6-4.html).

The check that matters is `increments the failure counter and pages, with no hold to wait out`,
which passed in **17m11s** having timed out at 300s on the first attempt. That is the one that
caught the over-blocking below, and it is the only evidence that separates a gate which refuses
the right things from one which refuses too much.

**What this campaign does NOT establish, stated because 90 green checks invite the opposite
reading.** The release's central change is a *refusal to act*, and no lane in this suite puts the
escrow into an unresolved state on purpose — nothing strips a location's S3 credentials out from
under a live operator. So the campaign proves the gate did not break the working paths, which
after tightening a guard is exactly the risk worth measuring. The behaviour under uncertainty is
covered by fourteen unit cases and the invariant guard, against a stubbed S3.

**0.6.3 shipped in the evening and this was found before midnight, on the same cluster, by the
same person.** It is a patch release with one defect at its centre, and that defect could destroy
the only key to a repository.

The setup was mundane. An administrator lost the operator's namespace to an Argo CD prune — the
hazard 0.6.3's own install page warns about, made live by 0.6.3 itself, which stopped rendering the
`Namespace` object so a prune saw it as no longer wanted. They rebuilt the namespace and restored
the cluster KEK from escrow. The S3 credentials came two minutes later.

In those two minutes the operator started, found a `ClusterBackupLocation` that had survived
because it is cluster-scoped, and began reconciling it. It could not read the S3 credentials, so it
could not read the bucket escrow, so it did not know whether a recoverable DEK existed. It
provisioned the repository anyway. `EnsureDEK` minted a fresh DEK **four seconds after the KEK
landed**, and for the next hour the location reported `Ready: True` while every mover failed:

```
Fatal: wrong password or no key found
```

against a repository holding 38 snapshots.

**The escrow's conflict guard is the only reason this is a patch note and not a post-mortem.** When
the credentials arrived, the operator compared the bucket object with its new DEK, found they
disagreed, and refused to overwrite:

```
bucket escrow and in-cluster DEK disagree … refusing to overwrite the escrow object
```

The original key was still there, 244 bytes, dated three days earlier. The repository came back.

### The rule was right and it had been inverted in seven places

`reconcileDEKEscrow`'s doc comment already said what must not happen — *"EnsureDEK would mint a
fresh DEK over a recoverable one"* — and the caller already turned its return value into
`Ready: False` and a `Degraded` phase. Nothing was missing. The function simply returned "do not
block" from every branch that failed **before it could ask the question**: no credentials, no KEK,
an unparseable KEK, an S3 client that would not build, a DEK Secret that would not read, and an
unreachable bucket. Six ways to be uncertain, all of them treated as certainty.

So the list of cases is gone and an invariant replaces it: **provisioning is allowed only where
this function has positively established that it is safe.** Two states qualify — an in-cluster DEK
already exists, so there is nothing to mint; or there is provably no DEK anywhere, so minting is
what should happen. Everything else blocks.

`EscrowWriteFailed` is the one `False` state that still does not block, and it is the only one the
old "advisory by design" argument ever fit: the in-cluster DEK is known-good, backups keep working
correctly, and what degrades is bare-cluster DR.

`EscrowConflict` blocks now too. A location in conflict was handing a wrong key to every mover
while reporting `Ready`.

**And the first cut of this fix over-applied its own rule, which the crucible caught and a unit
test in this very package had blessed.** `EscrowUnreachable` was made to block in both branches —
including the one where an in-cluster DEK is already present. That contradicts the invariant
directly: a DEK that exists is one of the two states where minting cannot happen. The justification
written into the test row was that "the bucket may hold a different generation's key", which is a
statement about conflict *detection* and licenses nothing about minting.

`m6/alerts` failed on it. That lane drives a backup to fail on purpose by replacing a location's S3
credentials with garbage; the credentials Secret still reads, so the failure surfaces at `Fetch`,
the location went `Degraded`, and the run then started failing **early — before the mover ran**,
which took `crystalbackup_backup_failures_total` with it. An alert about failing backups stopped
being able to see a failing backup.

So: **over-blocking is not erring on the safe side. It moves the outage**, and here it moved it
somewhere no rule was watching — the same shape as the thirty-six-hour stall 0.6.3 closed.

Fixing it needed a second correction. Returning "do not block" from that branch left one reason,
`EscrowUnreachable`, carrying two opposite verdicts — this release's own defect, reproduced three
lines from where it was being fixed. The two states now have their own names because they say
different things to the administrator: **`EscrowUnverifiable`** (the bucket could not be read, an
in-cluster DEK is present, and you are fine) does not block; **`EscrowUnreachable`** (the bucket
could not be read and there is no local DEK, so a recoverable key may be sitting in there) does.

### One message for three emergencies

The conflict branch tested `bucketErr != nil || clusterErr != nil || !bytes.Equal(…)` and emitted a
single sentence for all three: *"the bucket copy does not decrypt to the same key"*. That asserts a
comparison which, in two of the three cases, never happened. It sent the person diagnosing this —
with the file open — looking for a key mismatch when the truth was a fresh mint.

Three reasons now, because they are three different emergencies with three different remedies:
`EscrowUnreadableUnderKEK` (the bucket object does not open under this cluster's KEK — the KEK
itself is in question, and nothing can read that repository generation), `ClusterDEKUnreadableUnderKEK`
(the local Secret is the foreign one; the bucket is the survivor), and `EscrowConflict` (both open,
different keys — two repository generations, and the bucket may hold the only key to the older).

### The escrow had no tests at all

Not one, on the code with the worst failure mode in the product: everything else can lose a backup
run, and this can lose the ability to read a repository.

There are now fourteen cases pinning both halves of every outcome — the reason an administrator
reads **and** whether the repository may be provisioned — because the incident was a correct reason
beside a wrong decision. And above them, the guard that will outlive them: it enumerates inputs,
reads whichever reason each one reaches, and fails on any reason outside a five-entry allow-list
where each entry argues why minting cannot fork the repository. A `return false` copy-pasted into a
branch added in two years fails there without anybody having written a case for it.

### Also

- **`report` no longer needs `--from`.** Asked for it by the administrator mid-incident, and the
  reason is exactly that: getting a readable picture of the install meant running `selfcheck`,
  capturing 17 KB of JSON and parsing it by hand. `report` already knew how to format that document
  and `selfcheck` already knew how to produce it, in the same binary with the same RBAC — so
  without `--from` it now collects the self-check itself and formats the current instant. One
  collection path serves both subcommands; there is no second collector to drift.
  With `--from` it still needs no cluster at all, which is what lets you attach the JSON to an
  issue and have a maintainer regenerate the page. In that mode the collection flags are now
  **refused rather than ignored**: `report --from x.json --full` used to be silently accepted and
  would print a fully redacted page to somebody who had just asked for verbatim identifiers.
- **The self-check verdict no longer reads `healthy` beside a non-zero leak residual.** During the
  incident it said *"No rule breached among the 12 evaluated"* while `leakIndicators` counted seven
  residual VolumeSnapshots, the oldest 65 hours. True in its own terms, and not an answer to the
  question the reader is asking. The rule tally is untouched; the framing is not.
- **An uninstall page with the order and a table of what each deletion removes.** The order is not
  cosmetic: five finalizers across six kinds mean the operator must still be running when those
  objects are deleted, or they hang in `Terminating` with nobody to release them. The table answers
  the question asked verbatim during the incident — *does deleting a `Backup` delete the CSI
  snapshot and the restic snapshot?* Yes to its own transient exposure snapshot (teardown restores
  the content's `deletionPolicy` to `Delete` so the storage is reclaimed rather than leaked), never
  to one you made yourself, and **no** to the repository. Only a confirmed `ClusterErasure` removes
  repository data. Nothing in an uninstall deletes your backups, and that is the first fact a
  frightened administrator needs.
- **The DR runbook covers partial loss of the operator namespace.** More dangerous than total loss,
  and for a reason worth stating: on total loss the location does not exist yet, so the operator
  never reconciles one without credentials. On partial loss it survived, so the ordering trap above
  is not a possibility but a certainty. Restore both Secrets before the operator starts.
- **Upgrading 0.6.2 → 0.6.3 under Argo CD is now warned about where an upgrader meets it.**
  `install-argocd.md` had the warning in general terms; `upgrading.md` said nothing, and 0.6.3 is
  the release that turned it from theory into a live trap.
- **Adopting the escrowed DEK by hand is documented**, reconstructed from the code during the
  incident and written nowhere: the object key, the one-command KEK test with `age -d`, the Secret
  name and data key, and the reason the operator must be scaled to zero first — otherwise
  `EnsureDEK` re-mints before the escrow pass can adopt, which is a race the administrator loses.

### Two things this release does NOT claim

**It does not make the escrow bulletproof.** It makes the operator refuse to act while it does not
know. An administrator who restores the wrong KEK still has an unreadable repository, and 0.6.4's
contribution there is that it now says so in a reason of its own instead of a sentence about a
comparison it never made.

**And the incident's real first cause is not in this release at all.** The namespace was deleted by
a prune, and nothing in the chart stops that. The warning is documentation; a chart-side guard —
an annotation that makes Argo CD refuse to prune the namespace it must not touch — is a change to
how the chart is installed, and it is not something to design at midnight in a patch release.

## 0.6.3 — The first hour on somebody else's cluster (2026-08-07)

**Four defects blocked one user's first hour with 0.6.2** on their own RKE2 + Rook-Ceph cluster.
Not one of the four was reachable by any test in this repository, and two of them were hidden by
one file. `test/crucible/deploy/deploy.sh` set `namespace.create=false` and
`networkPolicy.apiServerPort=6443` before the first crucible run ever happened — the second with
a comment quoting the operator's startup failure verbatim, so the workaround had been understood,
written down and shipped here for longer than the bug had been visible to anyone outside. The
third was a documentation gap the campaign had no reason to notice; the fourth is a real hole in
the product and is the one worth keeping.

Eighty-two green checks, three campaigns in a row, and for two of the four they were proving that
a chart nobody installs works.

Both overrides are gone. The rule that replaces them is written into that file: **every `--set`
in the crucible's install is either something the documentation tells a user to set, or it is a
bug in the defaults.** Three remain, and each is now told to the reader on every install page.

That is the through-line, and most of this release is it. There is a fifth defect the incident
exposed rather than caused: once the RBD failure was pinned to a kernel feature bit present on one
of their nine nodes, it turned out **nothing in this product could say where a mover may run**, so
the only available advice was "upgrade every kernel first". `mover.placement` is the answer to
that. The rest is delta 9's API side, delta 13, and one CI gate that had been red on `main` for
four days without anybody noticing.

**Validated on real infrastructure: 90 of 90 crucible checks, 0 failed, 0 skipped, in 2h43m24s**
— the whole suite, M0 through M6, unfiltered, on a live six-node RKE2 v1.35.7 + Rook-Ceph cluster
with real S3, against the exact operator digest this release ships. Report:
[crucible-m6.3](https://crystalbackup.github.io/CrystalBackup/reports/crucible-m6-3.html).

Eight checks more than 0.6.2's 82, and fifty minutes longer, for one reason worth stating: **the
suite grew a lane that had never verified anything.** `m6/stall` shipped red and took three runs to
become evidence. First it was reverting its own fault injection inside the same second (see below).
Then, twice, it compared the operator's output against strings nobody emits — `FailedMount`, which
is the word from the field report but not the one *this* injection produces, and "never reached
Running" against a product that says "no pod of it ever reached Running". The operator was correct
on all three occasions. A spec written against an imagined message tests its author's imagination,
which is the same family as a campaign run against a chart nobody installs; the assertions now read
the Events the cluster actually recorded and compare against those.

**Breaking-ish, and deliberately so: `namespace.create` now defaults to `false`.** An existing
install that relied on the chart owning the namespace must set `namespace.create: true`
explicitly. `networkPolicy.apiServerPort` (scalar) is still accepted and still *replaces*
`networkPolicy.apiServerPorts` when set, so an install that had narrowed it to one port keeps
exactly the posture it asked for.

### A backup waited thirty-six hours and nothing gave up

Six mover pods sat in `ContainerCreating` because an RBD clone could not be mapped:

```
rbd: map failed: (22) Invalid argument
```

Their kernel, their clone format — not a defect in this operator. What *is* a defect in this
operator is everything that happened next, which is nothing. The kubelet published that Warning
**1069 times over thirty-six hours**, starting one minute in, and nothing read it. Four more
namespaces sat in `Snapshotting` beside them. `concurrencyPolicy: Forbid` then meant no further
nightly run fired at all: **one backup in fifteen days**, with a green dashboard the whole time.

No rule could fire, and that was structural rather than bad luck. All eleven rules in the table
watch for a **failure**. Nothing failed. A stall is not a failure and it is not a missed
schedule, and there was no series in the catalogue that described one.

The per-phase timeout that would have ended it had been marked "deferred to task #22" in
`advanceUploading` since M1.

Three things ship instead of that note:

- **Two deadlines in the controller, each with a predicate that makes its number safe.**
  `moverStartDeadline` is **30 minutes** and it is emphatically not a cap on how long a backup
  may take — the predicate is *the mover Job has existed that long and not one of its pods has
  ever demonstrably run*. A pod stuck before start has moved no bytes, taken no repository lock
  and consumed no snapshot, so giving up costs nothing; once the pod is Running nothing looks at
  it again, however long it takes. `snapshotReadyDeadline` is **2 hours** and is the opposite
  trade: a snapshot that was acknowledged and never became ready *was* being worked on, a slow
  driver looks identical to a dead one from the object alone, so the burden of proof is set high
  and the bound is eight times the existing 15-minute unacknowledged one. Both are measured
  against a durable clock the waited-on object already carries — the Job's `creationTimestamp`,
  the origin VolumeSnapshot's — so neither adds a CRD field and both survive an operator restart.
  Every "I do not know" answer waits rather than fails.
- **The kubelet's own words in `status.volumes[].reason`.** The most recent Warning Event on the
  pod, or on the origin VolumeSnapshot, appended after a colon, the way `MoverEvicted` already
  carried them. A reason that says only "timed out" leaves the reader exactly as blind as the
  thirty-six hours did. The Events are read field-selected through the uncached `APIReader` and
  only on a path already failing a volume — going through the cache would start an informer over
  the largest object stream a cluster has to serve a handful of reads.
- **`CrystalbackupBackupStalled`**, the twelfth rule, on a new series
  `crystalbackup_backup_in_progress_since_timestamp_seconds`. State-derived from the Backup
  objects at scrape time, absent when nothing is in flight, and reporting the **oldest**
  unfinished Backup of the series so tonight's fresh run cannot reset the clock on last night's
  wedged one. A counter was never an option: a `CounterVec` child materialises at one, so it
  cannot page on a first occurrence, and a stall is a first occurrence by definition.

The alert bound is **eight hours**, which is the loosest in the table and is taken from this
project's own published model rather than invented: `internal/metrics`' shared `durationBuckets`
top out at 28800 seconds, on the stated grounds that "a first full backup of a multi-terabyte
volume over a throttled S3 link lands in the last". Past that, a run is off the top of the scale
we designed for. It will occasionally fire on a genuine multi-terabyte first full. That is the
direction to err in, and the alternative — a bound above every conceivable legitimate run — is a
bound above the thirty-six hours this closes.

**What none of it catches**, stated because the gap is real: a mover Job that has *vanished*
still requeues forever. Every deadline here is measured against a clock belonging to the object
being waited on, and when the Job is absent there is no such clock. The candidates were the
Backup's own `creationTimestamp`, which says nothing about one volume, and a phase-entry
timestamp on `VolumeStatus`, which is a CRD field and a bigger change than this. The alert is
what covers it — that is part of why the series is state-derived.

### The chart made the documented install order impossible

The documentation provisions the cluster KEK Secret into `crystal-backup-system` **before**
`helm install` runs, and it has to: the KEK is generated and escrowed out of band, and this chart
never creates a Secret. So the namespace exists by then, and a chart that renders a Namespace
object is asking Helm to adopt it:

```
Namespace "crystal-backup-system" ... exists and cannot be imported into the current release:
invalid ownership metadata; label validation error: missing key "app.kubernetes.io/managed-by"
```

Every first-time installer who followed the documented order hit that, on the first command. The
install did not half-work; it died. Under a GitOps controller the object was worse than an error
— `install-argocd.md` warned in its own words that "a prune can delete the namespace holding your
cluster KEK", a hazard that existed *only* because the chart claimed the namespace, and
`install-flux.md` already devoted a section to telling the reader to turn the default off. When
the documentation for two of three install paths tells you to disable a default, the default is
wrong.

**`namespace.create` now defaults to `false`.** `true` remains for the genuinely greenfield case.

That file was also the only thing applying `namespace.podSecurityLabels`, and dropping it without
replacing the guarantee would have traded a loud failure for a quiet one — `enforce: restricted`
denies the `runAsUser: 0` + `DAC_OVERRIDE` the data movers need to preserve file ownership on
restore, and it denies it weeks later, on the first mover Job, as a pod that never starts. So
`podSecurityLabels` stops meaning "labels the chart stamps" and starts meaning "the posture this
namespace must have", verified three ways: stamped when the chart creates the namespace, checked
against the live namespace with `lookup` when it does not, and printed by `NOTES.txt`. A posture
naming no enforce level, and one naming `restricted`, are refused at template time in every mode.

`lookup` has two blind spots that are not edge cases — it returns nothing under `helm template`,
which is how Argo CD renders, and nothing under `helm install --create-namespace`, because Helm
creates the namespace after rendering. Both produce an unlabelled namespace and a silent guard,
and `operations/dr-runbook.md` still documents a reinstall with `--create-namespace`, which is
the second-worst moment to find out. So the operator **checks its own namespace once on startup**
and says so in the log and as an Event on the namespace. It does not refuse to run: exiting on an
upgrade of a cluster that has been running happily without the labels would turn a latent problem
into an outage.

### `networkPolicy.apiServerPort: 443` stopped the operator starting

On k3s, RKE2 and kubeadm — most of the world. The `kubernetes` Service listens on 443 and
kube-proxy DNATs to the API server's Endpoints on 6443 **before** the CNI evaluates egress, so a
rule naming only 443 never matches the packet that leaves the pod:

```
Failed to start manager: failed to get server groups:
Get "https://10.43.0.1:443/api": dial tcp: i/o timeout
```

It is now `networkPolicy.apiServerPorts`, a list, defaulting to `[443, 6443]` — a deliberate
superset, because the chart cannot know which one a cluster uses and guessing wrong costs an
operator that never starts. The cost of one extra outbound TCP port on a rule whose destination
is already the API server is not comparable.

The crucible knew this before the chart did. `deploy.sh` had carried `--set
networkPolicy.apiServerPort=6443` with a comment quoting that exact startup error, so the
workaround had been understood, written down and shipped in this repository for longer than the
bug had been visible to anyone outside it.

Two adjacent things were fixed in the same pass, both found by reading the rendered object rather
than the template. `apiServerCIDRs` narrowed only the manifest-mover policy while the operator's
own egress stayed at `0.0.0.0/0` — an asymmetry the name gave no hint of, and one that made the
value worth less than it looked; it now narrows both. And the operator's egress listed `port: 443`
twice, once from `apiServerPort` and once hardcoded for the storage probes, with nothing to tell a
reader which was which. The two destinations are now named. Object storage keeps its own
unnarrowed rule, because an S3 endpoint is not the API server.

### The soak was in none of the six install pages

The headline feature of 0.6.2, and there was no path to it by following the documentation. It is
now on all six (`install`, `install-argocd`, `install-flux`, English and French), along with the
three settings the crucible sets: "observability is opt-in, and off means no alerts". That
sentence is load-bearing for the crucible too — if it ever leaves the install pages, those three
`--set`s become silent adjustments again and have to come out.

`test/chart/install_test.go` is new and holds the default render itself: what a first-time
installer actually gets, which until this release nothing in this repository had ever looked at.

### Nothing could say where a mover may run

Diagnosing the RBD failure above settled the fact with a control: the same pod, the same PVC, two
nodes. On a 5.15 kernel it mounted the clone and read the file; on 5.4 it stayed in
`ContainerCreating` with `rbd: map failed: (22)`. Mapping an RBD *clone* — which is what restoring
from a CSI snapshot is — needs the clone-child feature bit, and eight of that cluster's nine nodes
did not have it.

Then the second finding, which is ours. **The operator had no way to act on that.** The chart's
`nodeSelector`, `tolerations` and `affinity` are the operator pod's and have never touched a mover;
`mover.JobRequest` carried `NodeName` alone, set by the same-node restore path and nothing else.
The only advice available was "upgrade every kernel first". It is correct advice and it is not
something an administrator can do this afternoon, which makes it the wrong thing for a backup tool
to require before it will work.

**`mover.placement`** takes `nodeSelector`, `tolerations` and `affinity`, and applies them to
**every** mover Job — per-PVC backup and restore, manifest capture, discovery, retention, prune,
check, unlock, external sync. Every one, not most: "backup pods run on the backup nodes" is a
sentence you can check with one `kubectl get pods -o wide`, and a rule with exceptions is one you
meet for the first time while debugging. It is admin-only, with no per-namespace and no
per-schedule override, because which nodes the platform's backup pods land on is not a tenant's
decision — the same line adr/0019 draws about credentials, drawn again about scheduling. Empty is
the default and produces a Job byte-identical to every release before this one.

Three things about it are worth knowing before you set it:

- **`nodeSelector` is hard and has no soft form.** On a cluster where few nodes match it does not
  make movers *prefer* those nodes; it serialises every backup in the cluster through them, and
  turns their absence into a cluster with no backups at all. When a preference is what you mean,
  `affinity.nodeAffinity`'s `preferredDuringSchedulingIgnoredDuringExecution` degrades the way you
  want: the capable nodes when they have room, elsewhere rather than nowhere when they do not.
- **The same-node restore drops it, on purpose.** A restore into an existing RWO volume pins its
  mover to the node the volume is attached to, because it can only be mounted there. On that Job
  the selector and the affinity are removed and only the tolerations survive. This is not leniency:
  the kubelet re-checks nodeSelector and node affinity on admission even for a pod it never
  scheduled, so keeping them would not place the pod anywhere better — it would get it rejected
  outright, on the one operation with no second choice of node. Dropping them makes the failure be
  the real one, the CSI driver's, rather than a scheduling error about a pod that was never
  scheduled. Tolerations stay because a `NoExecute` taint is enforced against running pods however
  they were placed, and would evict a restore mid-copy.
- **A placement the operator cannot make sense of stops it at startup** — an unknown field, an
  invalid label key, a toleration the API server would refuse, an affinity term matching no node.
  The alternative is the failure mode this whole release is about: a value visible in
  `helm get values` that never reached a pod. Here that value is "which nodes can mount the
  volume", so the pod it never reaches is a backup that does not exist.

Guarded at four levels, because a knob wired to nothing is this project's recurring defect and this
one is invisible when it fails: unit tests over `BuildJob` for the pod spec and the pinned-Job
rule, `test/chart/placement_test.go` parsing the rendered ConfigMap **with the operator's own
loader** rather than grepping it, an AST guard requiring the field at all ten `JobRequest` sites
with no exemption list, and a crucible spec that labels one node, writes the placement into the
ConfigMap the operator mounts, restarts it, and reads the answer off the pods the cluster created.

### Also

- **An upgraded soak reported one flat version and mixed two systems into one measurement.**
  `CollectorInfo.OperatorVersion` was rewritten in full on every collector start, so an archive
  reported whichever build started last, while `highwater/marks.json` persisted across restarts —
  a fortnight of six days on one build and eight on another came out as one system, with §5's
  mover-memory table silently averaging two. The gap between sessions was recorded; that the
  measured system changed at that gap was recorded nowhere. The version now rides on the Session,
  and consecutive same-version sessions collapse into spans — consecutive only, so a
  `0.6.2 → 0.6.3 → 0.6.2` rollback yields three spans rather than erasing the rollback while
  leaving its gap in place. §5 carries the disclosure above the first number, and a pod that
  started inside a gap is `unattributed` rather than filed under the neighbouring build.
  `crystalbackup_build_info` was already being scraped every minute and read by nothing; when it
  and the sessions disagree both are reported and neither wins, because the disagreement is the
  finding. Archives written before this field read `unknown`.
- **Delta 9's API side: the snapshotter-Secret pre-check.** Resolving the VolumeSnapshotClass
  already existed; what is new is checking the snapshotter Secret its parameters name. Missing →
  the volume fails with `SnapshotPrecheckFailed` and the Secret named in the Event, rather than a
  snapshot request that goes nowhere. Fail, not gate: a per-volume failure rolls up to
  `PartiallyCompleted`, which is visible and alertable, where a gate waits for a human who is not
  watching. When the Secret reference is CSI-templated (`${volumesnapshotcontent.name}`) it is not
  statically knowable and the verdict is `NOT_CHECKABLE` with the reason — never "pass", never
  "fail". The pre-check runs strictly *before* Expose, and that ordering is asserted rather than
  assumed: a refusal that exposed first would leave a VolumeSnapshot behind.
  **Neither half catches the shape that user hit**, and that is worth saying plainly, because the
  four namespaces they had sitting in `Snapshotting` were exactly it. Both fields the 15-minute
  progress deadline reads come from the cluster-wide snapshot-controller, which binds a
  VolumeSnapshotContent within seconds of any request it can see — so it reports "acknowledged"
  long before the storage system has done any work. A dead per-driver sidecar leaves precisely
  that: content bound, `readyToUse` never, indistinguishable from a driver taking its time. The
  pre-check is silent on it too, because the Secret existed. What ends that hang is
  `snapshotReadyDeadline` above, at two hours — and it ends it without diagnosing it. Diagnosing
  it means picking a maximum legitimate snapshot duration on storage we do not own, which is a
  guess, and it is not attempted.
- **Delta 9's other two parts are deliberately not implemented.** Trash monitoring and VSC ↔ RBD
  reconciliation both need `rbd trash ls` / `rbd snap ls`, and adr/0003 accepts as a consequence
  of its whole design that there are "no Ceph credentials anywhere in the backup system"; its own
  risk table already assigns that ground to a platform alert. Recorded in the roadmap as a
  decision, not carried as a gap, and the reasoning is written into `precheck.go` rather than left
  in a commit message.
- **Delta 13: `spec.s3.connections` reaches restic as `-o s3.connections=N`.** This operator had
  no `-o` plumbing at all before; the seam is new and the field rides it. The field is a pointer
  and the `Maximum` is the load-bearing half — `BackupLocation` is tenant-writable and every
  namespace of a cluster points at one shared gateway (adr/0009), so an unbounded `connections` is
  a tenant-authored denial of service against every *other* tenant's backups. `nil` emits no flag,
  so restic's own default stays free to change across an engine bump rather than being frozen by
  this CRD at whatever 5 meant in 0.19.1. Measured against the pinned
  restic 0.19.1 rather than assumed: restic errors only on an unknown key inside a namespace it
  applies, so the s3-scheme guard in `BuildJob` is tidiness and the test says exactly that instead
  of claiming a correctness role it does not have. `ForcePathStyle` is resolved rather than left
  ambiguous, and the answer is that it must **not** be forwarded — minio-go's `auto` bucket lookup
  already resolves to path style for every non-AWS endpoint, so forwarding it would change only
  the one case where it would be wrong. The point of the lot is the guard: thirteen `JobRequest`
  sites now set the field, one exemption is argued in a map rather than skipped by a boolean, and
  deleting the field from a single site fails an AST test by name. `JobRequest.GoMemLimit` existed,
  was consumed, was covered by tests and was assigned by no caller from M1 until 0.6.1; this is
  what stops the next one.
- **`preflight.sh` would have reported that user's cluster READY.** A VolumeSnapshotClass whose
  driver matches a StorageClass proves snapshot *availability*. It does not prove *usability* —
  that a volume restored from that snapshot can be mounted and read on those nodes — and their
  cluster answered yes to the first and no to the second. Preflight (now `2.0.0`, schema
  `crystalbackup.preflight/v2`) reports every resolvable class as **USABILITY NOT ASSESSED**,
  counted as a reservation and never as a pass; there is no code path that turns it into one.
  The companion `website/public/snapshot-probe.sh` answers it by creating a PVC, writing a known
  pattern, snapshotting, restoring and mounting the result read-only — per StorageClass, using the
  same VolumeSnapshotClass tie-break and the same access mode the exposer would use, which is why
  its selection block is spliced from the same generator. It says in its own header exactly which
  objects it creates, it never touches an object it did not create, and **on any outcome that is
  not FEASIBLE it deletes nothing**: the objects are the evidence, and that evidence took an
  administrator thirty-six hours to obtain the hard way. It is checksummed and cosign-signed
  alongside `preflight.sh`.
- **`make check-translations` had been red on `main` since 0.6.2 and nobody noticed** — eleven
  translated pages stale against their English sources, drifted in by that release's own site
  pass. The gate exists, works, and self-tests; it just runs only in CI, not in the local gates,
  so four days of local runs said nothing. The eleven are current again, and the target has been
  added to the delivery runbook's §1 local gates, where the cheap checks run before the expensive
  ones — a gate that exists only in CI was never in §1 at all.
- **A silenced build step shipped the previous release's binary as this one.** Preparing this
  release, `melange build` was run with its output redirected to keep the transcript readable. It
  decided the package was up to date, skipped the rebuild, reused a three-day-old `.apk`, and
  exited 0; apko then published an image whose digest was byte-identical to 0.6.2's. Nothing
  failed. The next step would have been a two-hour crucible campaign against the *previous*
  release's operator, reported as validation of this one. Three rules are now written into
  `build/README.md`: never silence a step whose staleness is invisible in its exit code, check the
  `.apk` mtime before pushing, and treat an unchanged digest after a code change as an alert rather
  than a convenience. It is the same defect class as `check-alert-rules` opening with
  `command -v promtool || exit 0` for five milestones — a step that declines to do the work and
  reports success.
- **The new `m6/stall` spec was reverting its own fault injection.** It parks the RBD node plugin
  DaemonSet, and it timed out after 300s waiting for the pods to go away — which reads like a slow
  cluster and invites a bigger timeout. It was not slow. rook ≥ 1.17 hands the CSI driver plane to
  a ceph-csi-operator that owns that DaemonSet: six `SuccessfulDelete` and six `SuccessfulCreate`
  events carry the same timestamp, with "node plugin daemonset updated successfully" in the
  reconciler's log between them. The park lasted a fraction of a second, and no timeout would ever
  have worked, because **a reconciler is not a race you can wait out**. The spec now stops the
  reconciler before touching the object — the pattern `m6_precheck_test.go` already used on
  `rook-ceph-operator` — and restores it after the unpark, in that order. Setting the Driver CR's
  own nodePlugin affinity instead was rejected: rook rewrites that CR on every CephCluster
  reconcile, so a reconcile inside the spec's 45-minute window would have un-injected the fault
  silently and made the spec flaky rather than red.

## 0.6.2 — The instrument, not the measurement (2026-08-03)

This release ships the **soak kit**: everything needed to run M6's two-week soak on a real
cluster with real data, and to send the result back as one archive. It does not ship the soak's
findings — that is a fortnight of somebody's calendar, and no release can contain it.

What it deliberately is not: the millions-of-files load test. That still needs infrastructure
this project does not have, and the mover ceilings it would fit remain conservative by
admission.

**Validated on real infrastructure: 82 of 82 crucible checks, 0 failed, 0 skipped, in 1h53m17s**
— the whole suite, M0 through M6, unfiltered, on a live RKE2 + Rook-Ceph cluster with real S3.

### The kit

A resident **collector** — one pod, 200m/384Mi with requests equal to limits, one PVC capped at
`soak.maxBytes`, cluster-wide **read-only** RBAC — enabled with `soak.enabled=true` and off by
default. It runs as a subcommand of the operator binary, from the same digest the operator
Deployment resolves, so there is no second image to keep in step.

It collects continuously rather than daily: operator metrics into windows, hourly CR-status
snapshots, every Warning event in the cluster (Kubernetes forgets them after an hour), the
operator's error-level log lines read from the API as they happen, a daily installation
self-check, and the mover high-water table. `soak-export` writes the whole thing to stdout as
one tar.gz, every identifier tokenised under an HMAC salt that never travels with it.

**One heartbeat line a day**, in the collector's own log, is the whole health check — because a
soak nobody can check is a soak you can waste, and the failure mode of the shape before this one
was discovering on day 14 that day 3 had broken.

### The instrument was broken, and that is most of this release

Its first four-hour run on the crucible reported sizing classes `data` and `manifests` as
NOT_MEASURED — *"no data mover pod ran while the collector was up"* — alongside a campaign that
had just executed dozens of backups. The sentence was true about what the collector saw and
false about the cluster.

Two causes, each sufficient on its own:

- **`mover.BuildJob` stamped no identity label.** It copied the caller's label map verbatim, and
  six of the ten mover creation sites never included `app.kubernetes.io/name=crystal-mover` —
  both data movers and all four manifest movers. Nothing in the operator selects on that label,
  so the omission was invisible in-cluster for six milestones; what it broke was every observer
  outside it, `kubectl get jobs -l app.kubernetes.io/name=crystal-mover` included.
- **A mover Job is not there long enough to be polled.** The Backup controller deletes it on the
  same reconcile pass that reads its result, so `ttlSecondsAfterFinished` is a crash fallback
  that normally never fires. Measured at 0.25s resolution: the four Jobs of one `ClusterBackup`
  were visible for **9.6s, 11.0s, 16.3s and 23.7s** against a 15s sample interval.

`BuildJob` now stamps the label itself, last, so no call site can omit or override it — and the
collector no longer depends on it either, listing on `managed-by` and identifying a mover by the
`--operation` flag it runs. The exact figures now come from a **watch**, with the poll kept for
the sampled quantities and as the re-list that repairs a dropped watch.

Three places that let the failure hide are closed too: the heartbeat carries `movers_by_class=`
with the zeros printed (an aggregate only has to be non-zero — `movers=87` looked healthy while
two classes sat empty); `--status` prints classes with no pods instead of omitting them; and
`COLLECTION-REPORT.txt` now states the per-class answer instead of only grading the stream that
holds it.

### Two more, both found by running the kit rather than reading it

**`collect.sh` died before it exported anything.** An unbraced expansion with an ellipsis glued
to it: the ellipsis is three bytes above 0x7F, and a shell whose locale does not classify them as
non-identifier characters reads them as part of the variable name, so `set -u` killed the run
three lines before the export. Valid syntax — `sh -n` and shellcheck both pass it — and only the
locale decides, so the author's machine can be fine and the operator's not. A Go test now forbids
the construct across the kit's scripts.

**The leak check failed on every correctly redacted archive.** It searches every cluster
identifier verbatim, and `default` is both a namespace name and an ordinary English word — one
this kit's own prose uses, since the self-check's warning about weak salts lists `default` as an
example of a guessable namespace. The check was matching its own documentation. Kubernetes'
built-in namespaces are now excluded, on the grounds that every cluster has them and the user
chose none of them, and they are NAMED as excluded rather than silently dropped. A check that
fails every run is a check nobody reads, and this one stands between a customer's namespace names
and a maintainer's inbox.

### And a claim that was wrong

Every place reporting `cgroupPeakBytes` called it "an upper bound" on the RSS peak. The soak's own
measurement disproved it: across the eight data movers of one campaign the cgroup peak sat
**20–22Mi below** restic's `ru_maxrss` on every single pod, the gap narrowing to ~3Mi on manifest
movers. The reading was right; the label was not. `memory.peak` counts what was **charged** to the
cgroup, RSS counts what the process had **mapped**, and a file page is charged to whichever cgroup
first faulted it in — so a mover whose image pages were already resident maps them for free.
Neither figure bounds the other. Both are still reported, each saying what it counts.

### What §5 now says

Measured on the crucible, all four classes, from the movers' own termination messages:

| class | peak RSS | shipped limit | headroom |
|---|---|---|---|
| `data` | 81Mi | 4Gi | ×50 |
| `manifests` | 105Mi | 2Gi | ×20 |
| `repo-heavy` | 74Mi | 8Gi | ×110 |
| `repo-light` | 101Mi | 1Gi | ×10 |

Zero OOM kills, zero evictions, zero limit hits. **On a small repository** — which is the whole
caveat, and exactly what the two-week soak on real data exists to replace.

### Also

- The crucible leak-check called the soak collector's own PVC a leak. It was a domain-prefix
  match on `crystalbackup.io/*` in the operator namespace, correct only while nothing permanent
  lived there; the collector is the first permanent resident. Seven checks burned their full
  ten-minute budget on a cluster with zero actual residue.
- The M1 discovery spec asserted on `status.volumes` with a single read, immediately after
  waiting only for the projected Backup to EXIST. It won that race in an 82-of-82 campaign and
  lost it twenty minutes later on a repository holding twenty-odd runs — which is precisely what
  a two-week soak produces on purpose.

## 0.6.1 — Every mover Job ran unbounded (2026-08-01)

A patch release with one theme: the mover Jobs this operator creates had **no resource
requests, no limits, an unbounded restic cache and no heap cap** — since M1, in every release.
`JobRequest.Resources` existed and no caller ever assigned it; `JobRequest.GoMemLimit` existed,
was consumed by `moverEnv`, was covered by tests, and no caller ever assigned that either.
90-roadmap.md M1 lists `GOMEMLIMIT` among what the mover ships. It never once shipped.

So this is not "tune the numbers". There were no numbers.

**Validated on real infrastructure: 82 of 82 crucible checks, 0 failed, 0 skipped, in 2h0m2s**
— the whole suite, M0 through M6, on a freshly provisioned RKE2 + Rook-Ceph cluster with real
S3, against the exact image digest this release ships, with `mover.profiles` empty so the
**built-in defaults** were what got exercised. Zero evicted pods and zero OOM kills across the
run. [Full report](https://crystalbackup.github.io/CrystalBackup/reports/crucible-m6-1.html).

**No breaking change**, and nothing to do on upgrade: the defaults apply themselves. An
installation that wants the old behaviour back can set `limits: {memory: "0"}` and
`cacheSizeLimit: "0"` per operation.

### Fixed

- **Every mover Job ran BestEffort with an unbounded cache.** BestEffort is the bottom of the
  kubelet's eviction order — under node memory pressure a running backup is killed before
  almost anything else — and an uncapped `emptyDir` lets one restic cache fill a node's disk.
  There are now four sizing classes over the thirteen operations (`data`, `repo-heavy`,
  `repo-light`, `manifests`), overridable per operation through `mover.profiles`, with the
  defaults living **only** in `internal/mover/profiles.go` and the chart carrying no numbers at
  all. Requests are modest — a scheduling floor multiplied by `maxConcurrentMovers` — and
  limits are generous, because they are node protection rather than right-sizing.
  **No class sets a CPU limit**, deliberately: CPU is compressible, so a hard limit buys
  nothing but a slower backup.
  The cache `sizeLimit` is 20Gi, and it is a **ceiling against a runaway, not a fitted
  estimate** — the load test that would give the real curve is the part of this lot that did
  not ship. The restic cache is cold on every run (each mover gets a fresh emptyDir), so the
  cap bounds one operation's download rather than an accumulation, which is what makes a
  generous ceiling defensible without the benchmark.
- **An evicted mover was reported as a mover bug.** Exceeding an `emptyDir` `sizeLimit` is not
  an ENOSPC restic sees: the kubelet removes the pod with no termination message, and the
  operator read that as `MoverCrashed` — diagnosing its own configuration as a fault in the
  binary. It now reads the pod's eviction reason and reports **`MoverEvicted`** carrying the
  kubelet's message verbatim, naming the volume and the number. Introducing memory limits
  creates the identical blind spot, so **`MoverOOMKilled`** ships with it, and the maintenance
  path — `prune`, the likeliest victim, whose Job pod was never parsed at all — now appends the
  reason instead of a pod count.
- **`GOMEMLIMIT` is derived instead of configured.** It and `limits.memory` are two statements
  about one budget; a knob that let a caller set them independently only ever produced the
  disagreement that kills a backup. It is now 80% of the operation's own memory limit, floored
  to whole MiB, **omitted entirely** when there is no memory limit to derive from (`limits.memory:
  "0"` means "do not cap this pod", and capping its heap anyway would invert the override's
  point) or when the result would fall under 256Mi. That floor matters: the failure `GOMEMLIMIT`
  prevents is an OOM kill, and the failure it *causes* when set too low is a GC death spiral —
  continuous collection, no progress, a backup that neither finishes nor fails.

### Changed

- `Backup.status` gains **`completionTime`** — additive — stamped once on first arrival at a
  terminal phase and never moved. It exists because `crystalbackup_backup_last_failure_timestamp_seconds`
  needed an honest answer to "when did this fail", and neither candidate worked: `backupTime` is
  the point-in-time of the snapshot set, and the Ready condition's `lastTransitionTime` is the
  run's START, since a failing run goes `False(InProgress)` → `False(Failed)` and only a STATUS
  change refreshes it.
- `docs/MOVER-RESOURCES.md` is generated from the same table the code reads, with a CI verify
  target, so the numbers an operator reads and the numbers a pod gets cannot drift.

### Fixed (build)

- **`make deploy` rewrote a tracked file, and it reached a binary.** kubebuilder scaffolds
  `deploy` and `build-installer` as `kustomize edit set image` against
  `config/manager/kustomization.yaml`; `make e2e` goes through that path, so every e2e run left
  the repository dirty. `git describe --dirty` is what stamps `crystalbackup_build_info`, so the
  first 0.6.1 images were built stamped `-dirty` — precisely the "build_info names no build"
  defect the version-stamping work existed to remove. Nothing published carries it; the images
  were rebuilt. Both targets now render from a temporary copy, and a test holds the file to its
  pristine form, because the reintroduction is silent.

### Documented

- The site is now **bilingual EN/FR** — 30 of 33 documentation pages — behind a staleness guard
  that records the git blob hash of the English source in each translated page and fails CI when
  they diverge. The three untranslated pages are the generated ones (`reference/metrics.md`,
  `reference/alerts.md`, `reference/api/index.md`); a hand-written French copy of a generated page
  drifts the next time the Go changes and has no generator to refresh it, so the checker exempts
  them from coverage and **errors** on a translation of one. The exemption is derived from the
  `<!-- GENERATED FILE` marker the generators already write, not from a list of paths.
  The guard earned itself immediately: bumping the English install pins for this release turned
  eight French pages red by name, and the French files were untouched — nothing else would ever
  have shown them in a diff.
- The English documentation had been telling people to install `0.5.1` for the whole of 0.6.0:
  every `helm install --version`, the Argo CD and Flux pins, the DR runbook. The 0.6.0 release
  updated the README and two Astro pages and left thirty documentation pages behind.

## 0.6.0 — M6 "Observability hardening & production readiness" (2026-07-31)

Milestone M6 began with one measurement. **Five of the nine alert rules this project had been
shipping as a specification since M1 were inert**: `schedule_active`, `backup_failures_total`,
`discovery_last_success`, `erasure_blocked` and `pvc_volumesnapshot_count` are series the operator
**never emitted**. Each rule is valid PromQL. Each evaluates without error. None of them could ever
fire, and nothing in the build could notice — the rule text and the metric definitions had never
met. An alert that cannot fire is indistinguishable from an alert that has nothing to report, which
is the exact failure mode a backup tool cannot afford.

That defect class — **an absence reading as health** — turned out to be everywhere once we started
looking for it, and finding the rest of it is most of what this release is. `make
check-alert-rules` opened with `command -v promtool || exit 0`, so it passed on every CI runner by
virtue of the binary being absent; the crucible's operator-readiness check had self-skipped in
**every published run since M1** behind an escape hatch whose own documentation gave the wrong
value; the crucible report called a two-spec filtered run "a green non-regression gate"; a test
fixture swallowed a failed `setfattr` behind `|| echo WARN` while every assertion that depended on
those xattrs kept passing; and the fanout campaign script could not report the one outcome you
hope for, because `grep` finding no failures exited 1 and killed it.

Then the new instrumentation was pointed at the product and found correctness defects that
**predate this milestone**, including the worst class a backup tool can have: **a run that reported
`Completed`, `namespacesSucceeded=1`, `pvcsSucceeded=1` for data it never wrote.** M6 did not
introduce it — it is M1 code — but the restore-fidelity gate found it on its first real run, which
is the entire argument for building the gate. See **Fixed** below; that one is the reason to
upgrade.

**What 0.6.0 is: ready to test in real conditions by early adopters.** Backups, restores, the two
planes, external sync and erasure work and are exercised on real infrastructure; the metrics,
alerts, dashboards and traces now describe what the operator actually does, and the self-check will
tell you where your installation stands without a Prometheus. **What it is not: production-ready.**
The roadmap's own bar for M6 includes a two-week soak alongside Velero on a staging cluster and a
pilot rollout; neither has happened. Run it on a cluster whose loss you can absorb, alongside — not
instead of — whatever you back up with today, and read
[when *not* to choose it](https://crystalbackup.github.io/CrystalBackup/docs/discover/when-not-to-use/)
first.

**Validated on real infrastructure: 82 of 82 checks, 0 failed, 0 skipped, in 1 h 51 min** — the
whole suite, M0 through M6, on a freshly provisioned RKE2 + Rook-Ceph cluster with real S3, against
the exact image digest this release ships. Not a filtered run: the crucible report states its own
coverage, and a run that selects a subset says so in its verdict line rather than presenting itself
as a gate. [Full report, check by check](https://crystalbackup.github.io/CrystalBackup/reports/crucible-m6.html).

**No breaking change.** Every API addition is additive (`spec.paused` on the two namespace-plane
types, `Backup.status.completionTime`, three `BackupRepository.status` fields), so an existing
installation upgrades in place. Two things to know: **the CRDs must be applied** — Helm does not
upgrade them ([upgrade guide](https://crystalbackup.github.io/CrystalBackup/docs/guides/upgrading/))
— and the `scope` metric label changed value vocabulary; see **Changed**.

### Added

- **The metrics catalogue the alert rules were already written against**
  ([spec/05-observability.md](spec/05-observability.md)). 54 families, and the mechanism matters
  more than the count: every series name is a **constant** in `internal/metrics/names.go`, the
  collectors build their `Desc` from it, and the rule table builds its expressions from the same
  constant — so a rename breaks the compile instead of silently killing an alert. The series-key
  structs are shared by the gauges and their new counter siblings, so the two physically cannot
  disagree on label order. New: discovery (§2.5, with `lastDiscoverySuccess` / `projectedBackups` /
  `orphanSnapshots` as new `BackupRepository` status fields), erasure (§2.6), mover concurrency
  (§2.7), webhook denials (§2.8), exposure wait and per-PVC `VolumeSnapshot` count (§2.9), plus the
  event half of the backup/run/restore/sync families — durations, byte counters, terminal-result
  counters.
  The counters are **real in-process counters, not state-derived gauges**, deliberately: counting
  live objects in a terminal phase is not monotone, because the run-history limit deletes them.
  Every one fires **after** the durable status write and only on first arrival at a terminal phase,
  guarded by the durable marker, so a conflict retry or a re-list cannot inflate it. The accepted
  cost — they do not survive a restart — is stated in §1 and is not free; see the `BackupFailed`
  fix below for what it cost in practice.
- **Eleven alert rules, generated from one table** and shipped as a `PrometheusRule`
  (`metrics.rules.enabled`, off by default: the thresholds are platform policy and should be read
  before they are enabled). `internal/alerts` holds the table — name, severity, `for`, threshold,
  annotations — and **builds** each expression from the `metrics.Name*` constants; no series name is
  ever typed as a literal. The chart's YAML is generated from it and a test regenerates into a temp
  dir and diffs, because a file that has never been committed is invisible to `git diff` and the
  guard has to work on the commit that *adds* a rule.
  Three of the eleven exist because writing the other eight exposed a hole:
  - **`schedule_created_timestamp_seconds`.** §2.1 claimed the last-success gauge was "initialized
    to the schedule's creation time on first reconcile, so `BackupMissed` fires even if no backup
    ever succeeded". It is not, and cannot be — the value is derived from `Backup` objects, so a
    schedule that never produced a single successful backup emitted **no series at all**, and
    `BackupMissed` was blind to an installation broken since the day it was installed. Fixed with a
    separate series rather than by seeding a fake success, which would have lied to every "last
    backup" panel on both dashboards.
  - **`CrystalbackupSchedulePausedTooLong` and `CrystalbackupExternalSyncPausedTooLong`**, at seven
    days. A pause guard alone trades a false page for permanent silence: someone suspends a schedule
    "just for the migration", leaves, and nothing alerts on that namespace again. Neither uses a
    seven-day `for:` — Prometheus restores pending-alert state only within its outage tolerance, so
    a week-long hold silently restarts after any outage, in an alert whose whole job is noticing
    what nobody is watching. `max_over_time` reads history instead. A test walks the table and fails
    if any `_active` series silences a rule without also feeding one.
  - `CrystalbackupBackupMissed` gains the per-schedule deadline §8 left open: **1.1 × the period +
    1 h, derived from the *longest* gap between two firings**, not the next one — `0 2,3 * * *`
    alternates one hour and twenty-three, and a deadline from the next gap pages every night at
    03:05. An unreadable cron emits no period and falls back to a fixed 26 h, because a schedule
    that will never fire is exactly what should page.
- **promtool unit tests, executed in CI, for all eleven rules.** Each rule has a firing case, a case
  just under the threshold (or just inside its `for` hold), and the absence case wherever absence
  carries meaning — a never-checked repository must **not** page, an Immutable location must not
  look like a stalled prune. Twelve mutations prove the tests can fail, including renaming a series
  to one nobody emits, which is the original defect class. `make alert-rules-covered` closes the
  hole one level down: an untested rule passes a promtool run simply by never being mentioned.
  A crucible lane provokes three of the conditions for real, because a unit test cannot tell you
  whether the operator emits, whether Prometheus scrapes, or whether the rule the chart packages is
  the rule that loads — and it asserts every shipped rule loads with `health: ok` and no
  `lastError`, which catches a many-to-one join failure against real cardinalities that synthetic
  series never reach.
- **Two Grafana dashboards** — `crystalbackup-tenant` (29 panels) and `crystalbackup-platform` (36)
  — as sidecar-provisioned ConfigMaps behind `metrics.dashboards.enabled`, covering all 49
  catalogue series as they stood when the boards were written. The check is the larger half: a panel
  querying a series that does not exist renders "No data", and on a backup dashboard "No data" is
  visually indistinguishable from "nothing to report". `make check-dashboards` parses every
  expression and cross-checks it against `internal/metrics` — series names, label names per family,
  and label **values**, resolved from the address of each enumeration rather than copied out of it.
  Eleven injected faults, eleven exits with status 1; wired into `lint.yml`. It exists because
  writing the dashboards produced exactly the bug it is meant to catch (see **Changed**, `scope`).
- **OpenTelemetry tracing across the pipeline, with exemplars.** The nine `go.opentelemetry.io`
  dependencies were all marked *indirect*: no file imported otel, the `traceID` log key §4 describes
  could never be populated, and the span tree §5 draws did not exist. M0 recorded the SDK as wired;
  it was not.
  An operator has no process continuity — a root span for a `Backup` would have to survive dozens of
  reconciles, a leader handover and a restart — so **nothing holds one open**. `tracing.Anchor`
  derives a Backup's trace and root-span IDs as domain-separated hashes of its UID, so any process
  computes the same answer from the object it already has, and every span is emitted after the fact
  with explicit timestamps read back from state Kubernetes already keeps (`creationTimestamp`,
  `backupTime`, the VolumeSnapshot's CSI `creationTime`, the Job's `startTime`/`completionTime`).
  The in-memory registry of open spans was rejected because it fails in the worst direction: an
  unended span is never exported, so a restart would **orphan** its children rather than truncate
  them. Unconfigured, no SDK provider is installed at all — the OTLP exporter defaults to
  `localhost:4317`, so merely constructing one would have every operator on earth retrying against a
  collector that is not there. Measured at **3.9 ns per guarded emit site, zero allocations, zero
  goroutines**, and a mover Job spec byte-identical to before.
  Three blind spots, named rather than hidden: the movers inherit the operator's own `OTEL_*`,
  allow-listed (§5 sourced them from a `mover.extraEnv` Helm value that does not exist); the
  snapshot/expose boundary is **not** observable unless the CSI driver fills the optional
  `status.creationTime`, so without it one span covers the wait rather than inventing a split in
  someone's storage latency; and `crystalbackup.snapshots_removed` is left undeclared rather than
  declared-and-empty, because `restic forget` reports removals as prose and the 4096-byte mover
  result protocol has no field for a count.
- **An exportable self-check.** `crystal-backup selfcheck` emits versioned JSON; `crystal-backup
  report --from` turns it into standalone HTML with **no cluster, no network and no clock**, so a
  maintainer regenerates the page from a file pasted into an issue. Both are subcommands of the
  operator binary — no new artefact, no new supply chain, and no CLI invented ahead of M7. Image
  digests are read from `status.containerStatuses[].imageID`, never from the spec: a report
  describing the tag someone configured rather than the artefact the kubelet resolved would repeat
  the 0.5.1 mistake. Every rule's verdict comes from the predicate on its **table entry**, so the
  self-check and the alert bundle cannot disagree about a threshold.
  Two limitations stated on the page itself. **Three predicates cannot reproduce their PromQL
  exactly** and say so next to their verdict rather than in a footnote: `BackupFailed` under-reports,
  the two paused-too-long rules substitute a condition transition for metric history and so
  over-report. And a rule that cannot be evaluated reports **"not evaluated" with a diagnostic —
  never OK**; an unmeasured OK is the exact lie this milestone exists to remove.
  Redaction is **on by default**, because the intended use is attaching the file to a public issue
  and the contents are somebody's customer list: HMAC over a 32-byte random salt minted per report
  and never written into it, so correlation inside the report survives and identity does not.
  Secrets, repository passwords, S3 credentials and key material are **never read**, in any mode
  including `--full`, and a test greps the serialised bytes of both modes for fixture credentials —
  on the serialised output rather than struct fields, so a newly added field that leaks fails the
  test instead of passing forever.
- **`spec.paused` on the namespace plane.** A tenant could suspend their backups only by **deleting**
  their `BackupSchedule`, which throws away `lastSuccessTime` and the history every alert measures
  against. `BackupSchedule` and `BackupExternalSync` now have `paused`, and pausing **preserves
  status** — which is the whole difference from deleting. `schedule_active` emits `0` for a paused
  schedule rather than nothing, so a forgotten pause is representable at all.
- **`preflight.sh`**, published on the site with a checksum and a keyless cosign bundle. `helm
  install` answers "does it install"; the question an administrator actually has is *what will be
  backed up, and what will be silently ignored*. The core is a table: per StorageClass, the exposer
  that would be resolved, the VolumeSnapshotClass that would actually be picked, and **the number of
  existing PVCs sitting on it** — the column that turns an abstract verdict into a measured
  consequence ("local-path … VOLUME SKIPPED … 1 in 1 ns", and it says out loud that those volumes'
  data will never be backed up, their manifests will, and the `Backup` will still report
  `Completed`). The CSI-to-exposer table is **generated**, not transcribed: the generator extracts
  the constants with `go/parser` *and* runs the real `exposer.Registry.For` against nine probe
  provisioners, refusing to emit a rule that disagrees with what the registry answers. `volumeMode:
  Block` gets its own check because that limitation is on the **restore** side, so without it the
  main table would have reported those volumes covered. Three principles: UNKNOWN is never PASS,
  it is strictly read-only (with the one honest nuance — `kubectl auth can-i` POSTs a
  `SelfSubjectAccessReview` — behind a flag), and the docs put download → verify → read → run first
  and the `curl | sh` shortcut second, named as such.
- **The operator can READ `CustomResourceDefinitions`** — `get`/`list`/`watch`, nothing else — so the
  self-check can report storage versions and per-CRD controller-gen provenance. Without it that read
  was `Forbidden` on every real chart install and the report fell back to API discovery, which knows
  only the *served* versions of the group. It stays read-only on purpose: an operator that could
  **write** a CRD could rewrite the schema of the objects it is trusted to back up. The discovery
  fallback stays too, for clusters installed by a chart older than this rule.
- `Backup.status.completionTime`, mirroring `ClusterBackup`'s — stamped once on first arrival at a
  terminal phase and never moved, through both doors (`writeStatus` and `failHooks`, the second
  being where creation and failure are furthest apart). See the `BackupFailed` fix for why it had to
  exist.

### Fixed

- **A run reported success for data it never wrote.** Found by the restore-fidelity gate on its
  first real run, and it is the worst class of defect a backup tool can have: not a failure, an
  **invisible** failure, discovered only when someone tries to restore. Measured on the crucible:
  the same `ClusterBackup` ran four times against a namespace whose volume was re-seeded with fresh
  random content between each; **exactly one data snapshot exists — the first**. The three later
  runs captured nothing at all and each reported `phase=Completed`, `namespacesSucceeded=1`,
  `pvcsSucceeded=1`. `restic check --read-data` found the repository perfectly intact: nothing
  corrupted, nothing written. The only honest signal was `addedBytes` staying empty, and nobody
  alerts on that.
  `ensureChildBackup` tested for an existing child on `(namespace, name)` alone. Nothing tied the
  object it found to **this** run, so a discovery projection, a terminal `Backup` from an earlier run
  of the same name, or the namespace plane's own stamped `Backup` all satisfied it; the controller
  no-oped and `aggregateAndWrite` counted the found child's status as this run's result — on a
  projection, a status built from repository snapshots, so the run counted somebody else's bytes as
  its own.
  The vector nobody would guess is **not** name reuse. Both planes build the run name with the same
  function, so a `ClusterBackupSchedule` named `daily` covering a namespace and a `BackupSchedule`
  named `daily` inside it, on the same cron, produce a **byte-identical name in the same
  namespace** — and both directions lose data silently. Two administrators naming a schedule `daily`
  is the whole prerequisite.
  The fix keys on `cb.UID`, which is exact rather than heuristic: a crash-restarted run keeps its CR
  and therefore its UID, while a run recreated under the same name gets a new one. Children carry
  `crystalbackup.io/parent-uid`; a matching UID is the ordinary idempotent second pass, and a
  different one — or a projection, a terminal phase, or any durable result already at the coordinate
  — is a **`RunNameCollision`** that fails loudly per namespace and takes the run to `Failed`.
  Projections are barred from the roll-up outright: even if the ownership test were imperfect, a
  *view* of the repository must not be able to increment `pvcsSucceeded`. The unstamped, unfinished,
  resultless case is adopted, which is what lets an existing installation upgrade without declaring
  its whole history a collision. **This is M1 code — M6 did not introduce it, its gate found it.**
- **Discovery was erasing an execution record.** `projectGroup` adopted any terminal `Backup` and
  force-applied a status rebuilt from the repository. Observed live, four seconds apart: a `Skipped`
  volume and its `CSISnapshotUnsupported` reason vanished along with `addedBytes`, `sizeBytes` and
  `node`, turning a `PartiallyCompleted` into a `Completed` within thirty seconds. A `Backup` is both
  a restore-point catalogue and an execution record, and when they disagree the **execution record
  wins**: a reconstruction from the repository knows, by construction, only what succeeded.
  Projection now completes and never overwrites — merging per PVC, never raising a recorded phase.
  Also M1 code.
- **The ServiceMonitor was overwriting every tenant's namespace label.** A ServiceMonitor attaches
  its target's labels and, with `honorLabels` false — the default — the **target wins every
  collision**. So the operator's own namespace overwrote the metric's, and every `crystalbackup_`
  series in Prometheus arrived carrying `namespace="crystal-backup-system"`, for every tenant on the
  cluster; the real value survived only as `exported_namespace`, which nothing queries.
  Nothing looked broken, and that is the point. The alert expressions still joined, because both
  sides of `on (namespace, …)` carried the same wrong value — so `BackupMissed` fired exactly when it
  should and then named the operator's namespace in its summary, **routing every tenant's page to the
  same place**. The tenant dashboard's `$namespace` variable offered one choice.
  `PVCSnapshotPileup` named the wrong namespace for the PVC it had correctly counted. Every metric
  this project emits is specified to be tenant-attributable (R19, §1), and at the scrape none of them
  were. No unit test could have caught it, nor promtool, nor `helm template` — it took a real
  Prometheus scraping a real operator.
- **`CrystalbackupBackupFailed` could not page a first failure, restart or not.** Caught by the
  0.6.0 crucible campaign, silent on a `Backup` that had genuinely failed. Two defects underneath.
  A `CounterVec` child is created **by** its first `Inc()`, so it appears at 1 rather than stepping
  0 → 1, and `increase()` measures a rise: a window whose samples all read 1 has none. **The first
  failure a series ever records could never move the rule**, with the process up the whole time —
  which is precisely the incident an operator most wants paged, a namespace that was fine yesterday
  and failed once tonight. The same materialisation rule means an operator restart does not *reset*
  the counter, it makes the series **disappear**, and `increase()` sees across a reset but not across
  a disappearance (measured on a live cluster after the operator pod was replaced: zero series, three
  `increase()` series all equal to 0, one failed `Backup`, nothing firing).
  The rule now reads a state-derived companion alongside the counter — `or (time() -
  crystalbackup_backup_last_failure_timestamp_seconds < 3600)` — with both numbers coming from the
  same window constant, so the two halves of one disjunction cannot describe two different hours.
  The plain `crystalbackup_backup_failures` gauge was rejected for the second disjunct: it survives a
  restart too, but has no notion of *when*, so a `Backup` retained for diagnosis after failing three
  days ago would page every evaluation until someone deleted it — which teaches an operator to
  silence the rule.
  **Verified on the crucible, by measurement rather than by a green tick.** Over the whole run
  `max_over_time(increase(crystalbackup_backup_failures_total[1h])[2h:1m])` was **0** — the counter
  disjunct never once exceeded zero — and the alert fired anyway, so only the state-derived half can
  account for it. Without this fix the run would have been red exactly as the campaign before it.
  **Four things this fix does not do, named rather than hidden.** It cannot recover a failure whose
  operator restarted *and* whose `Backup` was garbage-collected inside the hour — no source survives
  that, and the remedy is a history limit above one. In its milder and far more common form, deleting
  the `Backup` **resolves** this alert even with the operator up, because on a first failure the
  timestamp is the entire expression: the firing window is the *object's lifetime*, not the hour the
  rule's name implies. The crucible measured thirty seconds of firing, its own teardown having
  removed the namespace twelve seconds after the `Backup` went terminal. In production a retained
  failure outlives the window and the distinction never shows; with a history limit of one and a
  success right behind the failure, check Alertmanager's `group_wait` before blaming the rule. The
  Grafana panels inherit the counter's blind
  spot; the honest addition is a new "time since last failure" stat rather than an or'd timestamp on
  a rate axis, which is a decision about the dashboards' shape and is **not** in this release. And
  `crystalbackup_restore_failures_total` behaves identically but no rule reads it, so it is inert; a
  future rule on it needs the same companion, and `RestoreStatus` has no `completionTime` to derive
  one from.
- **`ExternalSyncStale` false-paged on every newly created sync.** `last_success` is deliberately 0
  for a sync that has never completed (§2.12, so a secondary broken from day one is not silently
  missed), but `time()` minus zero is 1.77e9, which crosses 26 h on the first scrape — only `for: 1h`
  stood between creating a sync and a page claiming it had not completed in 26 hours. It now measures
  from `externalsync_created` when there is no success yet. The metric keeps saying 0, because 0 is
  the true answer to "when did it last succeed"; it is the rule's job to know what that means.
- **`ClusterBackupExternalSync.Paused` has existed since M5 with nothing reading it.** Pausing a
  cluster sync for maintenance would have paged 26 h later, as soon as the rule bundle landed. Guard
  added — with its paused-too-long companion, above.
- **Every published image reported `crystalbackup_build_info{version="dev"}`.** The workflow built
  with `-ldflags="-s -w"` and nothing passed `-X`, so the one series that exists specifically to give
  a dashboard a build to join against named no build — in a release whose headline is that its
  metrics can be trusted. Fixed in **all three** build paths: the release workflow (the tag on a tag,
  `dev-<sha>` elsewhere, never an empty string), `make build`, and the Dockerfile that `make
  docker-build` and the kind e2e use (`git describe --tags --always --dirty`, where `--dirty` is
  load-bearing — a binary built from an edited tree is not the tag it is nearest to). The mover and
  sync images are untouched: they link `./cmd/crystal-mover`, which serves no metrics and has no
  `Version` symbol to stamp.
  Guarded, because the linker does **not** report a `-X` flag whose target does not exist — it is
  silently dropped — so renaming the package would break all three paths at once with no symptom but
  the "dev" just removed. `TestVersionStampTargetsThisPackage` asks the compiler for the package's
  real import path and holds every build file to it. The second guard deliberately does *not* assert
  `Version == "dev"`: the test binary is itself linkable with the flag, and a test forbidding a
  stamped build would fail for the one person doing the right thing.
- **The container build could not see two embedded files.** `go:embed report.css` failed inside the
  Docker build: `.dockerignore` denies everything and re-includes, for source, only `**/*.go` — so a
  file pulled in by `go:embed` is Go source to the compiler and invisible to the build context. It
  builds everywhere a developer looks, because the file is on disk, and fails **only in the image**.
  The file already carried the instruction ("Add every new embed target to this list"), which was
  right and was not enforced; a test now walks the tree for `go:embed` directives and fails naming
  any target that is not re-included.
- **`make e2e` could hang forever on teardown.** It hung for 35 minutes and was cut by the suite
  timeout: `make undeploy` deletes the operator Deployment, its namespace and the CRDs in one
  `kubectl delete`, and four `Backup` objects still carrying the `crystalbackup.io/backup` finalizer
  outlived the operator that was supposed to release them. Three things made it unbounded rather than
  merely slow — waiting only on the `ClusterBackupLocation` before undeploying, no `--timeout` on the
  waits, and a deadline that SIGKILLed `make` and not the `kubectl` it spawned while
  `CombinedOutput`'s `Wait` blocked on the inherited pipe (`exec.Cmd.WaitDelay` is what makes it
  binding). Teardown now drains the custom resources **first**, while the operator is alive to
  process its own finalizers, and separates the two cases: leftovers with a serving operator are a
  product defect and fail the spec; leftovers with no operator are orphans, force-stripped and
  reported. M3 and M5 no longer force-strip in `AfterAll`, which was masking the very defect the
  suite should catch. **32 of 32 specs pass in 514 s with no finalizer forced anywhere in the run.**
  The same trap waits for anyone who uninstalls the operator with live `Backup`s, and no document
  said so; see **Documented**.

### Changed

- **The `scope` metric label is lowercase.** It carried two vocabularies at once: `repository_*` and
  `discovery_*` published `BackupRepositoryStatus.Scope` verbatim (`Cluster|Namespaced`, the
  kubebuilder enum) while `externalsync_*` emitted `cluster|namespace`. One alert expression written
  across both families matches nothing on one of them, and nothing says so. `metricScope()` now maps
  the API enum onto the lowercase pair §2 always specified, which is also what `origin` uses and the
  Prometheus convention for enumerated values. Found because six tenant dashboard panels filtered
  `scope="namespace"` on the spec's authority and would have read "No data" forever.
  **No published rule depended on the old values, and this is the last release in which changing them
  is free** — a custom dashboard or recording rule written against 0.5.x needs updating.
- **The discovery metric family gains a `namespace` label** — the repository's owner, empty for the
  shared cluster repo. It does not split a scan. Without it a tenant could see whether their
  repository was intact (check result, prune recency, stale locks) and nothing about whether the list
  `kubectl get backups` hands them still corresponds to what is inside it. A failing discovery means
  their restore points are silently stale, which is actionable by them and was visible only to the
  platform.
- **A run recreated at a coordinate it does not own now fails loudly** rather than silently
  succeeding (see `RunNameCollision`, above). This matters most under GitOps: a `ClusterBackup` is an
  **execution**, not desired state, so prune-and-recreate brings it back under a name the restic
  repository has already seen. What belongs in Git is the declarations — schedules and locations.
  Both new GitOps pages explain why, not just the rule.
- **`result` carries a fourth value.** `PartiallyCompleted` is a success with a hole in it, and the
  hole is the signal that a storage class stopped being snapshottable. The platform dashboard's fleet
  success ratio counted it as a failure and now reads it as the success it is, beside a by-result
  breakdown that keeps the hole visible.
- **`repository_stored_bytes` is withdrawn before ever shipping.** It was specified against `restic
  stats --mode raw-data`; no such operation runs, so the only value available was one
  `repository_size_bytes` already publishes. Two names for one reading is a lie by implication — and
  the withdrawn one was the worse of the two for the billing it claimed to serve, since `raw-data`
  excludes unpruned garbage the bucket still charges for. A test fails if the name comes back,
  mirroring the guard already standing over the equally unshipped
  `externalsync_bytes_copied_total`: a withdrawal nobody pins is a name somebody re-adds for
  completeness two milestones later.
- **"SLSA L3+" is now "SLSA Build Level 3"**, in all thirteen places it appeared. There is no L3+;
  the SLSA v1.0 Build Track stops at L3, and `images.yml` had said it correctly all along — the
  *documentation* was the part overreaching, on the subject where this project has least earned the
  benefit of the doubt. [adr/0012](spec/adr/0012-container-images-apko-wolfi-slsa.md) carries a dated
  amendment saying so. Two neighbouring claims retired for the same reason: "re-attested on a
  scheduled rebuild" (no such workflow exists — only the daily VEX refresh, which re-attests without
  rebuilding), and an upgrade guide whose copy-pasteable `helm` commands pinned a chart version that
  had not been published.
- Terraform state is ignored **at the repository root**, not only in `test/crucible/`. One empty file
  duly appeared at the root; nothing leaked and no state file has ever been tracked, but a populated
  one holds the RKE2 token, the Hetzner API token and the S3 credentials, and a single `git add -A`
  after a real apply is the whole distance between here and a token in a public repository's history.

### Documented

- **A documentation site** — 26 written pages plus a **generated** API reference (12 CRDs, `make
  api-docs`, with a staleness guard) — and the project stops describing itself as design-stage when
  six milestones have shipped. Every `crystalbackup.io` example was walked field by field against the
  OpenAPI schemas in `config/crd/bases`, and the quickstart is **marked UNVERIFIED** until a crucible
  scenario executes it verbatim: a quickstart nobody has run is the documentation equivalent of an
  alert rule nobody has fired. Where spec and code disagreed the code is documented and the
  divergence named — hook annotations are `crystalbackup.io/pre-backup-*`, not the `pre.hook.*` form
  [01-architecture](spec/01-architecture.md) describes, and R24's `keepWithinDuration` does not exist.
  The `/quality` page finally links all nine published acceptance reports; the evidence was on disk
  and reachable from nowhere, on the page most likely to be read by someone deciding whether to
  believe any of this.
- **The Metrics and Alerts references are generated** — from the `prometheus.Desc` values the
  collectors register and from the rule table — with a freshness guard in CI. Fifty-three families
  written by hand drift within a release, and this milestone was spent proving that. Generation also
  turns one claim into something machine-checked: the page states that every `_total` family and
  every histogram is event-driven and everything else is derived at scrape, and **generation fails if
  that stops being true**. Writing them against the code rather than the spec found a stub promising
  three sections of series that do not exist (there is no `crystalbackup_hook_*` family at all) and
  nine planned alert names that were never built.
- **Installing with Argo CD and with Flux**, and the three sharp edges that only cut there. A prune
  **deletes your keys**: the chart renders the `crystal-backup-system` Namespace, which holds
  `cluster-kek` and every wrapped DEK, so an Argo CD prune — or a Flux Kustomization pruning the
  HelmRelease into a `helm uninstall` — is DECOMMISSION §1.4 executed by accident. The two tools have
  genuinely **opposite** CRD behaviour, which "Helm never upgrades CRDs" is only half right about.
  And the webhook certificate churn is the finding worth reading twice: `webhook.yaml` mints a CA on
  every render, which the chart gets away with because `helm upgrade` renders once — Argo CD renders
  every refresh, three minutes by default, so with self-heal the Secret and `caBundle` differ every
  cycle and **the Deployment rolls with them**. An operator restarting every three minutes is a
  fault, not a cost, and `lookup` cannot fix it because `helm template` has no cluster to look in.
  Both pages carry the ignore blocks and the honest alternative of turning the webhook off.
- **The uninstall order nobody had written down.** `DECOMMISSION.md` covered repositories and
  re-encryption; the site's uninstall section warned only that Helm keeps the CRDs. Both now carry the
  ordered procedure with a verify gate that must print nothing before the operator is touched, and the
  recovery for someone already wedged — reinstalling the operator is usually enough, because a
  `Terminating` CRD still serves patches to its instances and the controller clears its own backlog.
- **The runbooks pointed at fields that do not exist.** `DECOMMISSION.md` ran three jsonpath
  expressions against absent fields, so the commands printed nothing and an operator read that as "no
  data" rather than "wrong query" (`.status.sizeBytes` is `approximateSizeBytes`;
  `.status.lastCheckSucceeded` is the `lastCheckResult`/`lastCheckTime` pair). The pair matters more
  than the rename: that command sits in *verify before destroying anything*, and `lastCheckTime` is
  refreshed on failure as well as success, so a lone `Passed` can be inherited from before the copy
  existed — the go-ahead now requires `Passed` **and** a `lastCheckTime` after the sync finished. The
  runbook also patched `spec.paused` on a `BackupSchedule`, which did not exist at the time.
  `RESTORE.md` was frozen at M2 and told readers manifest restore did not work; it has shipped since
  0.3.0. The remaining gap is real and now stated: **`ClusterRestore` accepts `spec.resources` and
  restores nothing from it.**
- **The metadata-fidelity contract is written down where a user can read it** (`docs/RESTORE.md`); it
  existed only as a comment in `internal/mover/job.go`. What survives a restore, and the four things
  that deliberately do not: `trusted.*` xattrs (need `CAP_SYS_ADMIN`), atime (reading a file to check
  it destroys it), ctime (nothing can set it), and device nodes (a property of your volume's mount
  options, not of the backup).

### Tests

- **The restore-fidelity gate — the `m6` crucible lane, and M6's exit criterion as a command rather
  than a paragraph.** A 235-entry corpus engineered to be hard to restore faithfully: a 384 MiB file
  spanning ~6 restic packs, sparse files, setuid and setgid bits, numeric ownership, binary and
  512-byte xattrs, a directory's default POSIX ACL, nanosecond mtimes on directories, dangling
  symlinks, shared inodes, a FIFO, and names carrying newlines, quotes and glob metacharacters —
  backed up from a Rook-Ceph RBD volume and restored **into a namespace that does not exist yet**,
  then compared field by field.
  The empty target is load-bearing: an in-place restore would pass every metadata facet for free,
  because an xattr, an ACL or a mode the restore failed to re-apply would still be sitting on the
  pre-existing file and the diff would come back green. The restored PVC is built from the snapshot's
  own `pvcsize`/`pvcclass` tags onto a fresh filesystem, so everything measured afterwards was put
  there by the restore. Content is digested per file **and** per 16 MiB window, so a failure names the
  corrupted byte range rather than reporting that a hash differs. Validated offline in a container: a
  faithful tar round-trip produces zero deviations across all fourteen fields, and a deliberately
  damaged copy fires all eight facets and reports the corrupted window exactly. It carries no enable
  flag and no conditional skip, and **nothing was trimmed from the corpus to keep it green** — a gate
  made green by removing what fails is the worst of both worlds.
- **Our labels must survive the scrape.** A general invariant that would have caught the
  `honorLabels` defect in seconds instead of through a five-minute timeout on an unrelated assertion:
  no `crystalbackup_` series may carry an `exported_` label, which is exactly what Prometheus renames
  our label to when a target label wins a collision. It consults `metrics.Catalogue()` for which
  families own a `namespace` label rather than hardcoding a list, and it **refuses to pass
  vacuously** — zero `crystalbackup_` series is a failure, not a clean bill. It runs first in the
  alert container, because a scrape that relabels our series invalidates every measurement after it.
- **Every crucible run name is now per-campaign, enforced by a test.** The campaign that went 60/5
  where the previous went 76/1 was not a regression: the restic repository is shared and persistent,
  and several specs used **constant** run names whose `AfterAll` deleted the Kubernetes objects and
  never the snapshots — so the second campaign inherited the first one's data, discovery projected
  yesterday's snapshots back into the re-created namespace as a `Completed` `Backup` before any
  `ClusterBackup` existed, and the fan-out correctly refused a coordinate it did not own. **The
  operator was right every time**; `RunNameCollision` said so on the CR, in the events and in its log,
  and none of that reached the Ginkgo output, which reports only `phase="Failed"`. The sweep found 54
  run-name sites across 31 files and four independently grown run-ID mechanisms, now one
  `crucibleRunID`. The lesson had already been written down after M5, and a note that gets re-read is
  not a control: `TestNoFixedRunNames` fails the build on a fixed name, and is `//go:build !crucible`
  so it runs in the ordinary suite while inspecting the tagged one. Two cases are deliberately not
  uniqued — the collision spec *needs* a collision, and the R26 projection specs are testing exactly
  the case where finding what is already there is the point. **The `m6` lane is 15/15 green on the
  contaminated cluster**, the one whose repository still holds the snapshots that caused the
  failures; passing on a fresh bucket would have proven nothing.
- **Four self-disabling guards removed.**
  - The `m0` operator-readiness spec had been skipped in **every published crucible run** — M1, M2,
    M3, M4's seven-lane fanout, M5 — behind a message reading "no released operator image yet
    (pre-v0.0.1)"; there have been twelve releases since. It self-skipped unless
    `CRUCIBLE_EXPECT_OPERATOR_READY` equalled exactly `"true"`, and the one place documenting the
    variable proposed `=1`, which the comparison rejects. The guard is **removed** rather than
    re-defaulted: whether the operator comes up at all is the single most useful thing a run on real
    infrastructure can tell us, and it should not be possible to silence it. The published reports are
    left as they are — they are dated records of what those runs did.
  - `make check-alert-rules` began with `command -v promtool || exit 0`. It now fetches promtool
    pinned by version and checksum.
  - The `c-edge` seed swallowed a failed `apk add attr` and a failed `setfattr` behind `|| echo
    WARN`, so the fixture the m1 and m2 restore assertions run against could arrive with **no xattrs
    at all** while every one of those assertions kept passing. It now fails the init container and
    verifies the xattr it just wrote reads back.
  - `m1SkipIfNoS3`, on the path of every data-touching spec from m1 to m5, turned one unset variable
    into a wall of skips. There is no legitimate run without S3, so an empty coordinate means the
    harness is broken — renamed `m1RequireS3`, and it now fails at all fourteen call sites.
- **The crucible report no longer calls a filtered run a non-regression gate.** A run of two specs out
  of eighty-three printed "✅ PASS" and, underneath, "Safe to treat this run as a green non-regression
  gate." The root cause is a conflation in Ginkgo's own reporting: a spec **deselected by a label
  filter** and a spec that ran and called `Skip("reason")` arrive in the same `SpecStateSkipped`. The
  report now answers two questions in that order — did everything that ran pass (the colour), and did
  everything run (the scope) — with **coverage before result**, and only an unfiltered full run may
  use the word *gate*. A "Not exercised by this run" section names what did not run, area by area. A
  run cut short by `--fail-fast`, a timeout or an interrupt produces the same empty-`Failure`
  signature as a filter and would have been described as "filtered"; it now reports INCOMPLETE and
  says where it stopped. `CRUCIBLE_VERBOSE` is also actually verbose now — `go test` buffers a
  package's stdout and **discards it when the package passes**, so verbose mode was verbose only on
  failure (71 bytes before, 6333 after, on one spec).
- **`fanout.sh` could not report an all-green campaign.** With `set -euo pipefail`, the `grep '^❌'`
  that collects per-lane failures exits 1 when no lane has any — so the script died before the
  residual-object measurement or the verdict, and exited 1: the exact inverse of its own contract. A
  campaign *with* failures ran to the end and reported correctly; a campaign with none died silently.
  This is the tool whose header says "green-green-red is timing, red-red-red is a bug" — the one you
  reach for when you no longer know what to believe — and it could not say green-green-green. The
  verdict is now coverage-aware, with the exit code answering "did something go wrong" and the words
  answering "can I ship". Verified end to end under stubs across ten scenarios; the four green-ish
  ones do not exist at all without the `set -e` fix.
- **The crucible report's section order was not total, and an entire test package had never been
  linted.** `areaRank` answers the same value for every area that is neither infra nor `m<N>`, so two
  such areas compared equal and Go's deliberately randomised map order decided — two runs of the same
  suite emitted their sections in a different order and could not be diffed by eye, which is most of
  what a saved report is for. The larger finding is that this was invisible: the suite is behind
  `//go:build crucible`, `make lint` did not pass the tag, and staticcheck flags it in one line
  (SA4010) the moment the tag is on. `make lint` now makes a second, **scoped** pass — scoped, not
  global, because the tag is a two-way switch and turning it on everywhere would hide the
  `!crucible` guard that keeps the tagged suite honest.
- `.dockerignore` embed coverage, the version-stamp target, Unix-time gauge naming
  (`TestUnixTimeGaugesAreNamedTimestampSeconds` — ten of eleven such gauges ended
  `_timestamp_seconds` and the eleventh was caught by eye; a metric name is compiled into somebody's
  dashboard, recording rule and alert, none of them in this tree), and the "no `_active` series may
  silence a rule without also feeding one" table walk. The pattern throughout this release: a lesson
  written down is not a control.

### Not in this release

Four M6 roadmap items are deliberately not delivered here, and 0.6.0 should not be read as covering
them ([spec/90-roadmap.md](spec/90-roadmap.md)):

- **Mover resources by operation type** (prune > backup), the cache `emptyDir` `sizeLimit` decision,
  and the **millions-of-files load test** (with it, the restic-vs-rustic revisit of
  [adr/0001](spec/adr/0001-repository-engine-restic-format.md)).
- **VSC ↔ RBD-image reconciliation**, trash monitoring and the active pre-check before
  `VolumeSnapshot` creation; and **S3 RGW tuning** (`s3.connections`, wave test against
  `rgw_max_concurrent_requests`).
- **The two-week soak alongside Velero on a staging cluster**, which runs on a real build cluster
  after this tag, and the pilot rollout the milestone's exit criteria call for. This is the main
  reason 0.6.0 is offered for testing rather than for production.
- A dashboard panel for "time since last failure", which is what would close the Grafana half of the
  `BackupFailed` blind spot described above.
- **The PodSecurity review** the same roadmap bullet asks for. The pieces it would review already
  ship and did not change here — the chart's `NetworkPolicy`, the four PodSecurity namespace labels,
  and the operator's requests/limits — so this is an unexamined posture rather than a missing one.
  What the review would have to conclude is now on record, because the crucible surfaced it in
  passing: the **operator** already satisfies every `restricted` criterion (`runAsNonRoot`,
  `seccompProfile: RuntimeDefault`, `allowPrivilegeEscalation: false`, `readOnlyRootFilesystem`,
  `capabilities: drop ALL`), but the **mover cannot**, by design. It runs as uid 0 with a
  per-operation capability set and everything else stripped, because restic has to read and restore
  files owned by arbitrary uids inside somebody's volume. Both run in `crystal-backup-system`, so
  `enforce: baseline` there is a **constraint, not caution** — tightening it to `restricted` would
  block every mover Job. Under the namespace's `warn: restricted`, the API server says so on every
  mover creation, which is why the operator log carries a PodSecurity warning on a healthy install.

## 0.5.1 — Supply-chain: the signed artefact was the wrong one (2026-07-29)

A patch release with no functional change. It exists because verifying 0.5.0's artefacts — rather
than its pipeline's green ticks — turned up a defect that had been shipping since signing was
introduced.

**`cosign verify ghcr.io/crystalbackup/<image>:<version>` failed for every consumer, on every
signed release, v0.4.0 included.** The signatures were real; they were attached to the wrong
artefact. The digest handed to cosign came from `head -n1 image-refs.txt`, which for a multi-arch
publish is whichever ref apko wrote first — a **per-arch child manifest**. So the amd64 child was
signed, the SBOM attestation and the SLSA provenance were bound to it, and the multi-arch index the
tag actually resolves to carried none of them. [adr/0012](spec/adr/0012-container-images-apko-wolfi-slsa.md)
promises a signed index; the artefacts did not deliver one.

The line had a comment asserting it was the index. It was not — and `build/README.md`'s own
troubleshooting table already warned readers off exactly that construct. The release workflow was
doing the thing the documentation tells people not to do.

### Fixed

- **The signed subject is now the multi-arch index**, resolved from the registry — the same
  question a consumer's `cosign verify` asks — and the job **refuses to sign anything that is not
  an index**. The failure it guards against is silent: signing succeeds and verification fails
  months later, which is how this survived four releases.
- **The chart's image pinning is hardened the same way.** It resolved digests with
  `--format '{{.Manifest.Digest}}'` and checked them with `[ -n "$x" ]` — but that template is
  ignored by some buildx versions, which print the entire inspect instead: non-empty, so it passes
  that check and pins the chart to a blob of text. It happened to work, and 0.5.0's chart does pin
  the three correct indexes (verified by pulling it), but the guard could not have caught the
  failure it was written for. A chart pinned to a per-arch child would deploy an amd64-only image
  onto arm64 nodes.
- **The documentation stopped recommending the fragile form.** `build/README.md` and both delivery
  skills gave `--format '{{.Manifest.Digest}}'` as the *fix* for deploying a child digest. They now
  give the plain-output form, and say why.

### Note on 0.5.0

0.5.0's images, chart and Release are correct and stay published — the code is identical to this
release's. What 0.5.0 lacks is a verifiable signature on the index. Anyone who needs one should use
0.5.1; anyone already running 0.5.0 has the same bits.

## 0.5.0 — M5 "Namespace plane, external sync & right to erasure" (2026-07-29)

Milestone M5 opens a second plane. Until now every backup was the platform's: an admin's
`ClusterBackupLocation`, an admin's key, an admin's schedule. A namespace user can now back their
own namespace up to their **own** object storage under their **own** key, alongside cluster DR and
independent of it. Backups can be copied to a **second** location that opens with a **second** key.
And erasure became a physical operation rather than a promise.

**`spec.encryption.platformAccess` is gone** (breaking, and the only breaking change). It was
specified in M0, never implemented, and dropped here rather than built: an operator key slot on a
user repository is a password living in `crystal-backup-system` that keeps working after the user
rotates their key or deletes their Secret — and because removing a restic key slot does not rotate
the master key, one they could never take back. The guarantee that platform access ends when the
user's key does is now bought by the mechanism not existing. The crucible asserts it against the
artefact: a user repository holds **exactly one key slot**.

Validated on real infrastructure, and this is where the milestone earned its keep. Writing and
running the acceptance suite found **six defects**, three of which left an advertised M5 feature
completely inert — in every case the visible step succeeded and the step after it failed, which is
precisely why unit tests and a green CI had not noticed:

- a namespace-plane external sync could **never** reach `Completed`;
- `mode: Mirror` **never** pruned anything;
- the right to erasure **never** removed a snapshot.

None of the three was reachable without a real repository. See **Fixed** below. Final state:
14/14 on the crucible's `m5` label, 32/32 on the kind e2e.

### Added

- **The namespace plane (R3, R5).** `BackupLocation` and `BackupSchedule`, namespaced: the user's
  bucket, the user's credentials (read from **their** namespace, never the operator's), and the
  user's restic password — either a Secret they reference or one the operator generates **in their
  namespace**. A `Backup` against a namespaced location runs the same execution path as a cluster-DR
  one, with no fan-out.
- **External sync (R28, [adr/0013](spec/adr/0013-external-backup-sync.md)).**
  `ClusterBackupExternalSync` (admin, whole shared repo or a namespace-tagged slice → a secondary
  `ClusterBackupLocation`) and `BackupExternalSync` (user, their namespace's backups → a second
  `BackupLocation` **in the same namespace**, structurally confined like `Restore`). The copy is
  `restic copy`: it decrypts from the source and **re-encrypts to the destination's own key**, so
  the second copy is an independent repository and never a byte clone carrying the source's key.
  `mode: Mirror` tracks the source (copy what is missing, forget what is gone) or `AppendOnly`.
- **A third image, `sync`.** restic holds two repository keys but only ONE set of backend
  credentials, so both repositories are addressed as `rclone:` remotes, each carrying its own — which
  makes rclone a hard requirement of sync and of nothing else. It is a separate image so that
  surface stays off the backup and restore path.
- **`ClusterErasure` (R21).** Physical right-to-erasure: `restic forget` by `tenant=` /
  `namespace=` / `namespace=`+`pvc=` followed by `prune`, as ONE queued operation on the
  repository's exclusive lane — inseparable, because a forget without its prune leaves the tenant's
  bytes in the packs. Typed confirmation (R23); `Blocked` on Immutable locations rather than a
  success that did not happen. Per-tenant crypto-shredding stays dropped: one shared repository has
  one master key ([adr/0009](spec/adr/0009-shared-cluster-repo-tag-tenancy.md)).
- The external-sync **metric family** — lag, last success, snapshots and bytes copied, both planes
  ([05-observability.md §2](spec/05-observability.md)). The `ExternalSyncStale` **alert rule itself
  is specified, not shipped**: §3 of that document is a specification as of this release, and the
  whole alert bundle lands with M6. What ships here is the series it will read.
- A repository decommission runbook.

### Changed

- **Consistency hooks (R16) now exec as a tenant ServiceAccount, not as the operator.** The hook
  path had been running with the operator's own identity, which meant a quiesce command executed
  with cluster-wide credentials in a namespace the tenant controls. Deployments that restricted the
  operator's `pods/exec` grant should re-check it against the tenant identity instead.

### Removed

- **`spec.encryption.platformAccess`** — see above ([adr/0004](spec/adr/0004-encryption-key-management.md)
  amendment). Nothing read it, so no behaviour changes; the field is simply no longer accepted.

### Fixed

Every one of these was found by the M5 acceptance suite before release, and each ships with the
regression test that would have caught it.

- **A namespace-plane external sync could never complete.** The snapshot inventory knew only the
  cluster plane: it looked for an ownerReference to a `ClusterBackupLocation` and unwrapped the
  platform DEK, while a namespaced repository is attached by back-link labels and opens with the
  user's key. The copy succeeded every time; the accounting after it never could. The S3
  credentials moved with the fix — read from the location's own namespace, never the operator's,
  where a same-named admin Secret would have silently been used instead.
- **`mode: Mirror` never pruned.** Both sync reconcilers built their maintenance dependencies
  without a mover image, so Mirror's trailing `forget` Job was created with **no container image**
  and the API server rejected it. The copy half was unaffected — it passes the sync image
  explicitly — so Mirror copied and never deleted, on both planes.
- **The right to erasure removed nothing.** The forget argv carried no keep policy, which is the
  correct intent, and a command restic refuses outright: *"no policy was specified, no snapshots
  will be removed"*, exit 1. `--unsafe-allow-remove-all` is restic's explicit opt-in to exactly
  this. Verified against the pinned engine, not assumed.
- **A `ClusterErasure` panicked the reconciler when the location had no `maintenance` block** —
  which is the default. `spec.maintenance` is an optional pointer and erasure is the only caller
  that reaches prune without it. A panic requeues, so it panicked again on every retry, on the most
  destructive path in the system.
- **A location alias slipped past the self-copy guard.** It compared resolved repository *names*,
  and a repository's name derives from its location's on both planes — so it only ever reproduced
  the admission rule that already denies source == destination by name. Two differently-named
  locations on one bucket, prefix and cluster ID are one repository, and a Mirror between them
  would have had restic open it as both ends. It now compares the resolved repository URL.
- **rclone probed — and would have created — the destination bucket.** The `s3:` spelling of the
  same repository never does either, and a secondary reached with credentials scoped to its own
  bucket (the least privilege such a destination should have) has no `CreateBucket` right, so the
  probe alone failed the copy for a bucket that was there all along.
- A bookkeeping failure after a successful copy re-enqueued the **whole copy** on every requeue
  rather than retrying only the accounting.

### Documented

- [adr/0013](spec/adr/0013-external-backup-sync.md) amended twice: rclone as the addressing layer
  and why sync is a third image, then five empirical findings about `restic copy` and rclone that
  changed the implementation — including that `--json` emits no summary, so sync is not a
  summary-parsed operation, and that `no_check_bucket` is mandatory.
- [adr/0004](spec/adr/0004-encryption-key-management.md) amended for the `platformAccess` removal.
- The crucible gained an M5 harness and three acceptance containers; `deploy.sh` and the fanout
  now pass the sync image digest, without which a sync Job silently lands on the chart placeholder.

## 0.4.0 — M4 "Consistency hooks, verification & maintenance" (2026-07-27)

Milestone M4 makes a backup **application-consistent** and a repository **maintained**. Backups
can now quiesce a workload before the snapshot and release it after; repositories prune the space
their retention policy freed and verify that what they hold is still readable.

Two pieces of the API had been declared since M0 and were dead: `MaintenanceSpec` (nothing ever
read `pruneSchedule` or `checkSchedule`) and the hook types (nothing ever exec'd). Both are live.

Validated on real infrastructure: **seven independent full-suite crucible lanes, seven times zero
residual snapshot objects**, including a spec that SIGKILLs the operator at the exact
terminal-transition window the teardown leak lived in (previous reproduction rate: ~1 run in 3).
The final sample ran the release image and passed 46/46.

### Added

- **Consistency hooks (R16).** Pre-snapshot quiesce and post-snapshot release, executed through
  `pods/exec`. Candidate pods are those in the Backup's namespace **mounting the PVCs the run is
  capturing** — not a namespace-wide label match, which is what confines the operator's
  cluster-wide exec grant to workloads whose data is actually being captured. Velero's precedence
  is matched: a pod's `crystalbackup.io/pre-backup-*` annotations **replace** (never merge with)
  the run's spec hooks for that pod. Commands are an argv, never a shell string.
- **The freeze window is bounded by the snapshot phase, not the upload** (01-architecture §5). A
  database held frozen for a multi-hour upload is an outage, not a backup.
- **The unfreeze is unconditional.** The release fires on the snapshots being *cut*, whatever
  their outcome: a failed or skipped snapshot leaves the application just as quiesced, so gating
  the thaw on success would strand exactly the workload whose backup just went wrong. It is
  retried within a bounded budget and, being derived from `status.hooks`, survives a controller
  restart — a workload left frozen by a crashed operator is an outage the backup itself caused.
- **`onError: Fail` aborts before snapshotting.** A snapshot taken after a failed quiesce would
  *look* application-consistent without being so, which is only discovered at restore time.
- **Scheduled `prune` and `check` (R17)** on the per-`BackupRepository` exclusive queue, with
  cron schedules, jitter, `--max-repack-size`, and `check --read-data-subset` to catch silent
  bit-rot that a structural check cannot see. `status.recentMaintenance` keeps the attempt
  history; `RepositoryCheckFailed` is raised for the alert.
- **The seven repository metrics of 05-observability §2.4.** Physical size and stale-lock count
  come from a single recursive S3 LIST — no mover Job, no image pull, no repository password
  needed to answer "how big is it, and is anything stuck".

### Changed

- **`prune` drains the movers before running.** Not for corruption — restic's exclusive lock
  already bars that — but for **throughput**: otherwise a prune and the whole mover fleet contend
  on `--retry-lock`, and on the shared cluster repository that is every namespace's backup. The
  drain has its **own** short deadline rather than inheriting the prune's multi-hour one, because
  mover admission is shut cluster-wide while it waits and one stuck mover must not close the
  backup plane for hours (adr/0015 §3).
- **Repository reads pass `--no-lock`** (deferred from M3.1 to land with `prune`). Both read
  paths: discovery *and* the restore's mediated source listing. Without it a restore resolving
  its source during a maintenance window would block on the prune's lock.
- **A `ClusterBackupLocation` reports `Ready` only once its repository is initialized.** It used
  to flip `Ready`/`Phase: Ready` the moment the `BackupRepository` **object** was created, while
  the `restic init` mover Job behind it was still to run — so the documented sequence (create a
  location, wait for `Ready`, create a `Backup`) parked the `Backup` in `Pending` with
  `BackupRepository "<name>" is not initialized yet`, which reads as a `Backup` fault rather than
  as setup that was not finished. It cost an e2e run five minutes of timeout **inside the feature
  under test** before pointing anywhere near the actual cause. `Ready` now means **usable**. No
  controller behaviour changes — `Backup`, `ClusterBackup`, `Restore`, `ClusterRestore`, discovery
  and maintenance all gated on the repository's own `initialized` already, which is exactly the
  evidence that the location's verdict was not trusted.
- **New location phase `Initializing`**, for the window between the two, with `Ready=False` and
  reason `RepositoryInitializing`. Deliberately **not** `Degraded`: nothing is wrong and there is
  nothing for an admin to fix. `Degraded` stays reserved for faults that need action, so
  `kubectl get clusterbackuplocation` tells *wait* from *act*.
- **A stale repository lock is now acted on, not merely counted.** The pre-existing reaper fires
  when a *data mover* is hard-killed, and a prune is not a data mover. Note what this does and
  does not do: the count is age-based on restic's own 30-minute horizon, so it never touches a
  fresh orphan, and at that mark restic removes the lock itself. It does **not** shorten how long
  an exclusive op can queue behind a dead lock. What it fixes is that the signal was a dead end —
  `CrystalbackupStaleLocks` fired and nothing ever drove the gauge back down.

### Security

- **`google.golang.org/grpc` bumped past GO-2026-6061** (GHSA-hrxh-6v49-42gf: xDS RBAC engine +
  HTTP/2 transport server; fixed in 1.82.1) in **both** places it ships: the operator/mover
  module (1.79.3 → 1.82.1) and the restic binary the mover image builds from source (restic
  0.19.1 pins 1.81.1; the melange override now bumps it, in lockstep with the VEX analysis, so
  the shipped binary is what the signed statements describe). The advisory was reachable in all
  three binaries per `govulncheck`'s symbol-level analysis — the release gate that blocks on
  exactly this is what caught it, on the first `v0.4.0` tag attempt.

### Fixed

- **A backup's exposure teardown is now crash-only: re-entrant until verified, never one-shot.**
  A three-lane crucible fanout kept reproducing (~1 run in 3) a single residual
  `VolumeSnapshotContent` — `deletionPolicy: Retain`, no `deletionTimestamp`, no owner — and the
  audit that followed proved the shape was structural, and **predates 0.4.0**: the reconcile pass
  that persisted a Backup's terminal status was also the *only* pass that ever deleted its
  VolumeSnapshot/VolumeSnapshotContent pair; the terminal short-circuit barred every later pass,
  ownerReference GC cannot reach a cluster-scoped Retain content, and the orphan reaper's charter
  excluded exactly that object class while four comments promised it as the backstop. One process
  death (or one status `Update` that *commits server-side while erroring client-side* — SIGTERM
  cancelling the call in flight) inside that window leaked the content, and with it a storage-side
  snapshot, forever. Four changes close it, each of which stands alone:
  - the terminal short-circuit now **re-runs an idempotent teardown sweep until it has verified
    every delete**, then stamps `crystalbackup.io/exposures-cleaned` — a kill at any instant just
    means the next pass (or the next process: controller-runtime re-reconciles everything on
    startup) finishes the job;
  - teardown is **derive-only**: it reconstructs every object name from the Backup+PVC identity
    and can no longer call `Expose` (which could re-create the origin VolumeSnapshot
    mid-teardown) nor conclude "nothing to clean" from a deleted source PVC;
  - an ambiguous terminal status write is **disambiguated with an uncached re-read** instead of
    being assumed unpersisted;
  - the orphan reaper now actually **sweeps labelled VolumeSnapshots and
    VolumeSnapshotContents**, restore-then-deleting a dynamic origin content (the storage
    snapshot is reclaimed exactly once) and object-only-deleting the static re-bind alias — the
    backstop those four comments promised.
  Deleting a Backup now also **holds its finalizer until the exposure teardown succeeds**, and
  covers every volume phase (a crash between `Expose` and the first status write leaves residue
  on a still-`Pending` volume). On upgrade, the operator sweeps every historical terminal Backup
  once — idempotent, a handful of tolerated NotFounds per volume — and stamps the marker.
- **`pods/exec` was granted by the Helm chart but absent from `config/rbac/role.yaml`.** No
  kubebuilder marker existed, so `make manifests` had nothing to notice. A kustomize install could
  never have run a hook.
- **`Hook.Timeout` had no default.** As a non-pointer `metav1.Duration`, "unset" arrived as `0s`,
  and `context.WithTimeout(ctx, 0)` expires immediately — every hook without an explicit timeout
  would have failed before starting.

### Documented

- **[adr/0017](spec/adr/0017-cascade-materialization-backup-carries-identity.md)** — why
  `Backup.spec` carries **identity, not intent**: two producers write the kind, and discovery
  (server-side apply with `ForceOwnership`) cannot reconstruct run config from restic snapshots.
  Records the four accepted costs and the M5 direction.

## 0.3.2 — M3.2 restore-safety fixes (2026-07-25)

The 0.3.1 audit closed the throughput question and left four items behind, one of which was
recorded as "two explanations calling for opposite fixes → reproduce, do not guess". Reproducing
it took five minutes on a fresh crucible and turned up **two distinct bugs on the restore path,
the second of which destroys user data**. Both are fixed here.

No API or CRD change. The sanitization ruleset gains a rule (S8), so manifest snapshots taken by
0.3.2 differ from earlier ones — older snapshots are restored safely by the applier's own guard.

### Fixed

- **`Recreate` mode no longer destroys the volume it is restoring.** A `Restore`/`ClusterRestore`
  in `Recreate` mode deleted every selected resource before recreating it from the backup —
  including the `PersistentVolumeClaim`. Deleting a PVC releases its PersistentVolume, and a
  dynamically-provisioned volume's reclaimPolicy is `Delete`, so the CSI driver **destroyed the
  user's data** as a side effect of restoring a manifest; the recreated claim then bound a fresh
  empty volume. Worse, the data half of the same restore was already mounting a twin PV built
  from that volume's handle, so it hung forever on `internal RBD image not found` — the volume it
  was restoring had been deleted underneath it. PVCs and PVs are now reconciled **in place** even
  in `Recreate` mode (reported `Configured`, not `Recreated`). Nothing is lost: the mode's promise
  for the *contents* is delivered by `restic restore --delete` on the data path.
- **A restore can no longer leave a namespace stuck in `Terminating` forever.** The manifest
  capture photographs live objects, and the CSI snapshot-controller holds
  `snapshot.storage.kubernetes.io/pvc-as-source-protection` on a snapshot's source PVC for the
  ~2 s it takes the snapshot to become ready — so the dump caught it, and the restore re-applied
  it onto a PVC with no VolumeSnapshot behind it. Nothing would ever remove it: the PVC could not
  be deleted and its namespace never finished terminating. Sanitization rule **S8** now strips
  **every** finalizer at capture (enumerating them by name is a losing game — every CSI driver and
  operator adds its own), and the applier strips them again on the way in so snapshots taken
  before 0.3.2 are safe too. A finalizer the target cluster genuinely warrants is re-added by its
  own controller.
- **A whole-namespace `Recreate` restore no longer reports `PartiallyFailed` for an object that
  came back on its own.** The control plane recreates a namespace's `default` ServiceAccount the
  instant it is deleted, so `Recreate`'s create always found the replacement already there and
  reported that one object `Failed` — enough to make the entire restore `PartiallyFailed`. An
  `AlreadyExists` on the recreate now falls back to the in-place apply, which converges on exactly
  the state `Recreate` asked for.
- **A projection failure no longer discards the whole inventory.** One namespace refusing a
  projection aborted the discovery pass before it recorded what it had just listed, so
  `lastDiscoveryTime` froze and every retry paid for a full re-listing from S3. Per-group failures
  are now accumulated and reported (`ProjectionIncomplete`), the inventory is recorded either way,
  and the retry reuses the listing it already has rather than re-running it.

### Changed

- **Least privilege: a maintenance mover gets no added capabilities at all.** The capability set
  now follows what a Job can touch rather than what it is called: `DAC_OVERRIDE` exists so a data
  mover can walk a tenant's files, and a Job with no PVC mounted — `init`, `forget`, `prune`,
  `check`, `snapshots`, `unlock` and the manifest shapes — has no foreign-uid file in reach. Only
  a data job keeps `DAC_OVERRIDE`; a restore keeps the metadata-fidelity set (R10) unchanged.

### Tests

- The crucible's OOM spec was **not** testing what it claimed. It killed whichever
  `crystalbackup.io`-labelled mover pod happened to be Running, which could be another run's mover
  or the same run's *manifest* mover — after which the data volume completed normally and the
  spec failed. It is now scoped to its own run's data movers, and asserts only about the volumes
  it actually killed (`c-db` has two PVCs, so "no volume is Completed" was never satisfiable).
- Specs that restore into a namespace now tear it down with `deleteNamespaceAndWaitGone`, which
  fails with the namespace's own "finalizers remaining" message — so the next leak of this class
  fails the run that caused it instead of the run after.
- Full crucible suite on live Hetzner/RKE2/Ceph: **42 passed, 0 failed, 1 skipped in 36m51s** (the
  skip is conditional on a released image). 0.3.1 was 37 passed / 2 failed / 4 skipped in 58.6 min;
  0.3.0 failed four specs and then overran its budget outright. The m2 `Recreate` spec passes for
  the first time — 41 s, where it previously hung 20 minutes and failed.

## 0.3.1 — M3.1 operator throughput audit (2026-07-25)

A **measure-first** hardening patch. The M3 full-suite crucible run kept timing out differently
on every attempt, and the roadmap's working hypothesis was that discovery's `restic snapshots`
re-scan was O(snapshots). Instrumenting the released 0.3.0 operator on a live cluster showed
something else: **the discovery controller's single reconcile worker was ~100 % saturated for an
entire run at only three snapshots** — `reconcile_time_seconds{controller="discovery"}` summed to
1991 s inside a 1980 s window, while every other controller combined stayed under 100 s. The
operator was never resource-bound (CPU ~0.01 core, RSS ~290 MB). The full audit, with the
per-controller table and the two restic scaling curves, is in
[docs/audit-m3.1-throughput.md](docs/audit-m3.1-throughput.md).

No API, CRD or chart-value change: this is behaviour only.

### Fixed

- **Discovery no longer re-triggers itself.** Discovery writes `snapshotCount`,
  `namespacesPresent` and `lastDiscoveryTime` onto the very BackupRepository it watches, and the
  watch had no predicate — so each write came straight back as an event and re-enqueued discovery
  immediately. `RequeueAfter: discovery.interval` never got to fire and the controller spun
  back-to-back, blocking its worker on a fresh cold `restic snapshots` Job every ~6 s. It now
  honours its configured interval (~10x duty-cycle reduction). The filter is narrow on purpose:
  it drops an update only when the change is confined to those three fields, so the `Initialized`
  flip, another controller's status write and any metadata change still wake discovery.
- **The inventory Job no longer storms the BackupRepository controller.** That Job was
  controller-owned by the repository, and the repository reconciler watches `Owns(Job)` for its
  init Job — so every listing Job's lifecycle events woke it too: ~2700 no-op reconciles in 33
  minutes. A plain owner reference still cascades the GC on repository delete.
- **The inventory runs off the reconcile worker.** A pass creates a Job, polls it and reads its
  pod log — seconds, cold every time. It now runs single-flight per repository on a background
  goroutine and re-enqueues the repository when it lands (with a watchdog requeue backing up the
  wake), so multiple repositories stop serializing behind one worker. A panicking pass is
  recovered and retried instead of wedging that repository.

### Changed

- The crucible suite's `go test` budget is 90 m (was a hardcoded 60 m, which the M3 full-suite run
  overran — the binary panicked and took the report with it) and is overridable via
  `CRUCIBLE_TIMEOUT`.

### Known issues

- A restore twin PV was seen failing to mount with `internal RBD image not found` after two
  earlier restores on the same volume had succeeded (Ceph otherwise healthy). Tracked with a
  reproduction plan in the audit — deliberately **not** fixed on a hypothesis, because the two
  candidate explanations (a Ceph-side vanish vs. a teardown that releases a PV under a Delete
  policy) call for opposite fixes.

## 0.3.0 — M3 "Manifests & cluster-scoped DR" (2026-07-23)

Milestone M3 adds **Kubernetes-manifest backup & restore** alongside the existing PVC-data
engine, plus a **cluster-scoped disaster-recovery** path. A namespace backup can now capture its
API objects — sanitized for restore portability — into their own restic snapshots, and a restore
re-applies them mode-aware (Recreate, or server-side-apply Overwrite). Cluster-scoped objects
(CRDs, StorageClasses, ClusterRoles…) get their own capture and a **selective, opt-in** restore,
all behind the unconditional R23 confirmation gate. This release also lands the supply-chain
OpenVEX attestation work.

Validated on real infrastructure: envtest + `make e2e` on kind (25/25, real mover Jobs) and the
crucible **M3 acceptance gate (11/11)** on a live RKE2 / Ceph cluster.

### Added

- **Namespace-manifest backup.** A discovery-driven dump engine enumerates a namespace's
  resources and writes them, through the sanitization engine, into a `kind=manifests` restic
  snapshot. The mover gains a `manifests-backup` operation; the Backup controller wires the
  manifests phase alongside the data phase (adr/0007, spec/04).
- **Sanitization engine + golden corpus.** A rules engine strips cluster-assigned and
  non-portable fields (`resourceVersion`, `uid`, `clusterIP`, …) while preserving stable ones
  (e.g. a Service `nodePort`), covered by a golden-corpus test (R15, adr/0007).
- **Manifest restore.** `resources[]` selection (a `selector` **and** `include` select, `exclude`
  removes) with mode-aware apply: Recreate (delete-then-create, exact match) and Overwrite
  (server-side apply, keeping target-only extras). The R23 confirmation gate is unconditional for
  every destructive restore (spec/04 §5).
- **Cluster-scoped capture (`ClusterBackup`, R22).** An allow-listed capture of cluster-scoped
  resources into a distinct `kind=cluster-manifests` snapshot; `status.clusterResourcesCaptured`
  records it (adr/0011 §1).
- **Cluster-scoped restore (`ClusterRestore`).** Selective, **opt-in** restore of cluster
  resources — nothing cluster-wide moves unless explicitly selected — with scope-aware apply
  ordering, gated by R23 and admin-only RBAC (adr/0011 §2).
- **RBAC & network isolation for the manifest mover.** Least-privilege manifest-mover grants with
  a transient RoleBinding lifecycle, and M3 NetworkPolicies (default-deny + mover-egress + a
  manifest-mover API-server allow). The API-server allow honours a configurable
  `networkPolicy.apiServerPort` for CNIs that evaluate egress post-DNAT (e.g. RKE2 / Canal).
- **API surface.** The reserved M3 fields land: `spec.dryRun`, `status.resources`, and the
  manifest / cluster-resource selection fields (spec/02).
- **Supply-chain: OpenVEX attestation.** `images.yml` produces an OpenVEX attestation, a
  scheduled day-N workflow re-attests without a rebuild, and the release actions are pinned by
  SHA (adr/0012).

## 0.2.1 — M2 hardening (2026-07-19)

A security- and resilience-hardening patch from a full read-only audit of the M0–M2 code
(adequacy to spec, code quality, attack/algorithmic/resilience security, multi-tenant
isolation). The tenant-isolation *read* boundary (the I1 `namespace=` mediation) and the
crypto core were found sound; the fixes below close one critical data-integrity defect in the
backup fan-out, three high-severity correctness/security gaps, and a set of medium/low items.

### Fixed

- **Cross-namespace mover object-name collision (critical, data loss).** A cluster-DR run fans
  out one child `Backup` of the same name into every matched namespace, and every per-PVC
  mover/exposure object (mover Job, creds Secret, temp clone PVC, static VolumeSnapshot, and the
  cluster-scoped VolumeSnapshotContent) lived in the shared operator namespace named only from
  `(backup, pvc)`. Two namespaces holding a same-named PVC (`data`, `redis-data`, …) in one run
  derived colliding names; because every create tolerates `AlreadyExists`, the second namespace
  silently adopted the first's Job/exposure — its PVC never backed up, its `Backup` falsely
  recording the first's snapshot or hanging. Names are now namespace-qualified. The restic
  snapshot itself was always correct (namespace-scoped identity); only the k8s object names
  lacked the qualifier. New unit + crucible regressions (a homonym PVC in two namespaces of one
  run) cover it — the seed uses distinct PVC names, so the full suite never exercised it.
- **`[Cluster]BackupLocation` repository identity and mode were mutable (high).** `spec.mode`
  and the identity fields (`clusterID`, `s3.endpoint/bucket/prefix`) had no immutability guard:
  editing an identity field silently re-points the location at a different repository (orphaning
  every backup), and flipping `mode` Immutable→Standard defeats the R18 WORM intent by
  re-enabling prune/forget. Now pinned with update-only CEL, with an envtest.
- **`mover⇄unlock` mutex TOCTOU (high, repo corruption).** The quiescence gate and the mover Job
  create were not atomic, so a stale-lock `unlock --remove-all` could strip a freshly-created
  backup mover's repository lock (the drain census lags the cache). The controller now
  re-verifies quiescence after a fresh create and undoes it if a lock-removal is pending.
- **Discovery projection GC deleted other locations' projections (high).** `gcProjections` ran
  cluster-wide; with ≥2 `ClusterBackupLocation`s each location's discovery deleted the other's
  projections every pass. GC is now scoped to the reconciled location, with an envtest.
- **No restore progress deadline (medium, liveness).** A restore mover whose pod never starts (a
  twin pinned to a departed node, an unprovisionable staging PVC) wedged the restore in `Running`
  forever and, counting in the shared mover census, blocked the repository's maintenance drain.
  Such a volume now settles `Failed`/`RestoreTimedOut` past a per-volume deadline measured from
  pod creation, applied only while the pod has never started — a legitimately long restore is
  never timed out. Decision unit-tested with an injected clock.
- **`ClusterErasure.spec.confirmation` was required (medium).** Being `+required`+`MinLength=1`,
  the structural schema rejected an empty value before the confirmation VAP ran, making the
  documented `AwaitingConfirmation` park-then-confirm flow unreachable. Now optional, matching
  `Restore`/`ClusterRestore`.
- **Unanchored `source.time` CEL regex (medium).** The RFC3339 regex had no end anchor, so a
  malformed value was admitted and then reported with a misleading "not projected yet" gate
  forever. The regex is anchored, and both restore controllers now gate an unparseable instant
  (including a shape-valid but impossible date) with a distinct `InvalidSourceTime` reason.
- **Swallowed `List` errors in restore teardown (medium).** The residue sweep ignored `List`
  failures before removing the finalizer; they are now logged (the orphan reaper still backstops).
- **Retention `forget` missing `--retry-lock` (medium).** On a busy shared repo `forget` failed
  the instant another namespace's mover held the lock, silently dropping retention; it now waits.
- **Hardening (low).** Orphan reaper selects candidates by a positive per-PVC label so the
  wrapped-DEK Secret is never a reap candidate; `Restore`/`ClusterRestore` `spec.resources` gain
  `MaxItems`; docs corrected (`02-api` namespace-selector prose).

### Changed

- **Docs: mover credential scoping (I4) is stated as not-yet-implemented.** The escrow package
  doc and `03-security-and-tenancy.md` §4 read as if movers held repo-prefix-scoped credentials;
  in fact every mover receives the location's **root** S3 credentials (STS / per-repo keys are
  deferred to M6). The invariant now carries an explicit M0–M2 status note: a compromised mover's
  blast radius is the whole bucket (a leaked-Job-credential threat, not a namespace-user vector —
  I1/I5/I6 still confine namespace users), and the escrow's protection is the KEK, not the S3 path.

### Deferred (tracked, not in this patch)

- Per-repo mover credential scoping (invariant I4) — an M6 hardening item (STS `AssumeRole` or
  RGW static per-repo keys). Until then the mover blast radius is documented as the whole bucket.
- A dedicated tokenless `crystal-mover` ServiceAccount (movers run under the operator-namespace
  `default` SA with `automountServiceAccountToken: false`, so I6 — zero API access — already
  holds; the dedicated SA is defense in depth).
- An `ownerReference`/TTL backstop on the maintenance creds Secret, and an injectable clock for
  the orphan reaper — both low-severity.

## 0.2.0 — M2 “Restore” (2026-07-18)

The restore milestone (R2 cornerstone, R6, R7, R14, R23): everything a backup wrote in M1
now comes back — self-service, mediated, byte-verified — including into namespaces that no
longer exist.

### Added

- **`Restore` controller** (namespaced, self-service): consumes a `Backup` in its own
  namespace (name or `time: latest`/RFC3339 + origin), `Recreate`/`Overwrite` modes ×
  NetworkPolicy-style `volumes[]` selection with file-level `include`/`exclude` (partial
  restore, R7), and the R23 `AwaitingConfirmation` flow re-checked at execution.
- **Operator-mediated cluster-DR restore** (R2/R14 cornerstone): a cluster-origin source is
  resolved exclusively through a repository listing filtered server-side by
  `namespace=<the CR's namespace>` — snapshot IDs from the projection are never trusted,
  and a coordinate outside the namespace fails closed (`SnapshotNotFound`).
- **`ClusterRestore` controller** (admin DR): restores a **repo coordinate** (location +
  origin namespace + run/time) with `target.createNamespace` and `storageClassMapping`;
  works with zero surviving in-cluster objects (R26).
- **Restore target exposure** ([adr/0016](spec/adr/0016-restore-execution-and-target-exposure.md)):
  movers stay in `crystal-backup-system` (the repository key never enters a user
  namespace); an absent target PVC is provisioned and **transplanted** (WFFC-safe PV
  re-bind, provenance annotation `crystalbackup.io/restored-from`), a bound one is written
  through a Retain-only **twin PV** with a same-node pin for singly-attached RWO volumes.
- **Restore mover**: `OpRestore` mounts the target read-write, runs
  `restic restore --overwrite always [--delete]` with `--sparse` and full xattr/ACL fidelity
  caps (CHOWN, DAC_OVERRIDE, FOWNER, MKNOD, SETFCAP — PSA-baseline legal), and reports a
  summary-verified `restoredBytes`.
- **PVC-meta snapshot tags** (`pvcsize`, `pvcclass`, `pvcmodes`) on every data snapshot, so
  `ClusterRestore` recreates PVCs at their original size/class/modes from the repository
  alone (documented fallback for pre-0.2 snapshots).
- **Admission, VAP-first** ([adr/0010](spec/adr/0010-admission-vap-first.md)): the chart now
  ships `ValidatingAdmissionPolicy` objects for R23 confirmation (Restore, ClusterRestore,
  ClusterErasure — empty parks, wrong is denied), user isolation (operator SA exempt),
  Immutable-forbids-prune, denied namespaces (ConfigMap `paramRef`), namespace-selector
  shape and external-sync distinctness; plus the one dynamic webhook — single-default
  `ClusterBackupLocation` — fail-open with a chart-generated certificate.
- **Wrapped-DEK bucket escrow** (bare-cluster DR bootstrap, 03-security §4): the age
  ciphertext is mirrored to `<prefix>/<clusterID>.crystal-meta/wrapped-dek.age` and
  recovered automatically when a location is re-created on a fresh cluster with the KEK.
- **Restore metrics** (R19): `crystalbackup_restore_*` / `crystalbackup_clusterrestore_*`
  (last success, restored bytes, failures), state-derived and namespace-labelled.
- **Docs**: [docs/RESTORE.md](docs/RESTORE.md) (user guide + bare-cluster DR runbook);
  `Restore`/`ClusterRestore` samples.

### Changed

- The orphan reaper resolves restore-owned residue (staging claims, twin/transplant PVs,
  restore movers) and can never touch a delivered volume (handover strips the labels).
- The stale-lock unlock machinery is shared: a hard-killed **restore** mover triggers the
  same quiescence-gated `unlock --remove-all` a backup mover does (adr/0015).
- Operator RBAC: PersistentVolume write + VolumeAttachment/Node read (the adr/0016
  machinery; the twin's same-node pin is dropped when the node is gone or NotReady).
- `source.backup`/`source.time` are mutually exclusive (CEL); `targetPath` rejects `..`;
  `source`, `mode` (and `ClusterRestore`'s `target.namespace`) are immutable after
  creation — a mid-run edit cannot mix two points in time in one restore. A time-resolved
  (`latest`/cutoff) source is pinned for the restore's lifetime; a zone-less
  `YYYY-MM-DDThh:mm:ss` is read as UTC.
- Admission rule 8 counts **non-empty** positive selector forms (an empty `matchNames: []`
  no longer masks — or trips over — a real form), denies an absent selector with the
  rule-8 message instead of a CEL evaluation error, and exempts the operator SA.
- The exposure mechanism is **sticky per volume**: once a staging claim exists, its shape —
  never the live target state — decides transplant vs twin, so a target PVC appearing (a
  StatefulSet recreating its claim) or vanishing mid-restore can no longer misroute the
  handover. A restore runs at most **4 concurrent movers per owner** (slots free as movers
  finish; the cross-kind global semaphore remains a roadmap item), and a mediated-resolution
  listing Job is only re-adopted when its baked restic argv matches the current filter —
  a leftover listing from before a controller restart can never masquerade as a different
  run's resolution.
- Validated end to end on real infrastructure (Hetzner RKE2 + Ceph RBD/CephFS + longhorn +
  Hetzner Object Storage): the full crucible suite is **31/31 green** — every restore mode
  and selection byte-verified against the seed, the tampered-projection R14 negative caught
  fail-closed, and a deleted namespace reconstituted from the repository coordinate alone.
  Two defects only real Ceph could surface were fixed: the **pvc-transplant handover
  deadlock** — a completed mover pod kept the staging claim pinned by the pvc-protection
  finalizer, so the handover (which must delete that claim) could never finish; the mover
  result is now stamped on the Job and the pod deleted each pass, backed by a scoped
  `pods:delete` grant in the operator namespace — and a **duplicate-plan bug** where a
  repository holding several snapshots of one PVC under a run made the namespaced restore
  restore it twice (`restorableVolumes` now dedupes by PVC, like the ClusterRestore path).

## 0.1.0 — M1 “Core engine & cluster DR” (2026-07-17)

The restic-backed backup engine and the cluster-DR plane: `ClusterBackupLocation` /
`BackupRepository` (lazy init through the per-repo exclusive queue), the
`ClusterBackupSchedule → ClusterBackup → Backup → movers` cascade with restic-tag tenancy
(adr/0009), envelope encryption (age KEK → per-location DEK, adr/0004), CSI-generic
snapshot exposure (adr/0003), discovery projection (repository as source of truth, R26),
retention, the orphan reaper, mover-concurrency limits, metrics v1, and the backup⇄unlock
reliability mutex (adr/0015). Field-validated by the crucible on a live RKE2 + rook-ceph +
longhorn + local-path platform (25/25 specs).

## 0.0.0 — M0 “Project scaffolding”

Kubebuilder layout, the twelve `crystalbackup.io/v1alpha1` CRDs, CI (lint/test/e2e,
apko/Wolfi multi-arch images with SBOM + SLSA provenance), envtest + kind harnesses, Helm
chart skeleton.
