# Changelog

All notable changes to Crystal Backup. Versioning follows
[adr/0014](spec/adr/0014-versioning-and-release.md): milestone `Mn` → minor `0.n.z` on
major 0; `1.0.0` is a deliberate post-M9 API-stability decision.

## 0.4.0 — M4 "Consistency hooks, verification & maintenance" (unreleased)

Milestone M4 makes a backup **application-consistent** and a repository **maintained**. Backups
can now quiesce a workload before the snapshot and release it after; repositories prune the space
their retention policy freed and verify that what they hold is still readable.

Two pieces of the API had been declared since M0 and were dead: `MaintenanceSpec` (nothing ever
read `pruneSchedule` or `checkSchedule`) and the hook types (nothing ever exec'd). Both are live.

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
