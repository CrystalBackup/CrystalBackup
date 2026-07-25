# M3.1 — Operator throughput & discovery scalability audit

Status: **measurement complete (step 1)** — root cause established and code-confirmed.
Fixes (steps 2–4) not started: the roadmap mandates measure-first / no design pre-commit.
Method: released `0.3.0` operator + mover on a real Hetzner RKE2 crucible (rook-ceph +
longhorn + local-path), free controller-runtime metrics scraped over the whole full-suite
run, plus two isolated `restic snapshots` scaling benchmarks (local filesystem and real
Hetzner S3). No operator code was changed for the measurement (so the instrumentation could
not perturb the result).

## TL;DR

The bottleneck is **not** O(snapshots) restic scanning and **not** operator CPU/memory. It is
the **discovery controller**, whose single reconcile worker (concurrency 1) was **~100 %
saturated for the entire run at only 3–6 snapshots**. Two independent mechanisms combine:

- **A — ephemeral-Job-per-cycle:** each discovery cycle spawns a fresh `restic snapshots`
  Job (cold cache) and **blocks the reconcile worker** for its whole lifecycle (~5.7 s
  measured: ~3.5–4 s Job orchestration + ~2 s cold restic-on-S3 at low N), growing O(N) at
  ~22–27 ms per snapshot on cold S3.
- **B — self-trigger loop (a ~10× amplifier):** every reconcile unconditionally writes
  `LastDiscoveryTime = now` to the BackupRepository status, and the controller's `For`
  watch has **no predicate**, so it re-enqueues itself immediately. The `RequeueAfter:
  interval` (1 min) is dead code — discovery runs **back-to-back continuously** instead of
  once per interval.

A third, cheaper waste: the discovery Job is owned by the BackupRepository, and the
`backuprepository` controller does `Owns(&batchv1.Job{})`, so every discovery Job's lifecycle
events wake `backuprepository` too — **~2700 reconciles / 33 min (~82/min) of pure churn.**

Separately, part of the crucible's "different failure each run" flakiness is **storage**, not
throughput: the m2 Recreate restore hung on a Ceph RBD "image not found" under an OSD
`BLUESTORE_SLOW_OP` warning.

## The definitive number

`controller_runtime_reconcile_time_seconds` over a ~33 min (1980 s wall-clock) run:

| controller | reconciles | total time | mean | note |
|---|---:|---:|---:|---|
| **discovery** | 350 | **1991 s** | **5.69 s** | ~100 % of one worker for the whole run |
| backuprepository | 2700 | 40.8 s | 15 ms | hot-loop (Owns(Job) woken by discovery Jobs) |
| clusterbackuplocation | 348 | 26.8 s | 77 ms | |
| restore | 293 | 21.3 s | 73 ms | mediated-snapshot listing Jobs (concurrency 4 masks it) |
| backup | 45 | 2.4 s | 54 ms | |
| clusterrestore | 14 | 6.4 s | | |
| clusterbackup | 29 | 0.5 s | | |

Discovery alone spent **1991 s reconciling inside a 1980 s window** — its single worker never
rested. Every other controller **combined** is under 100 s. Operator process: CPU ~0.01 core,
RSS ~285–300 MB, ~13 goroutines — **not resource-bound.** `MaxConcurrentReconciles` confirmed
empirically: all controllers 1 except restore/clusterrestore = 4.

## Evidence — the two scaling benchmarks

`restic snapshots --json --tag crystalbackup`, median of 3, vs snapshot count N:

**Local filesystem repo (isolates restic's own compute, zero network):**
FLAT ~0.76 s (N=10) → 0.82 s (N=2000), warm and `--no-cache` identical. ⇒ restic's listing
compute is ~constant; the O(N) that matters is 100 % storage-backend latency, not restic.

**Real Hetzner S3 repo, cold cache (`--no-cache` = the discovery Job's exact model):**

| N | warm | **no-cache (discovery)** |
|---:|---:|---:|
| 10 | 2.0 s | 2.3 s |
| 50 | 2.0 s | 3.1 s |
| 100 | 1.9 s | 4.7 s |
| 200 | 1.9 s | 6.9 s |
| 350 | 2.0 s | **10.1 s** |

Cold slope ≈ 22–27 ms/snapshot (one S3 GET per snapshot object). Extrapolated: N=1000 ≈ 25–30 s
**per discovery cycle**. warm stays ~2 s (index cached, only new objects fetched) — but the
ephemeral Job is **always cold**, so it pays the no-cache curve every single cycle.

## Root cause, mechanism by mechanism (code-confirmed)

**A. Ephemeral cold Job per cycle, blocking the worker.**
`internal/controller/discovery_lister.go` `JobSnapshotLister.List` creates a one-shot Job
(`SetControllerReference(repo, job)` at :293), polls its pod every 2 s, reads the log, then
deletes the Job+Secret — all on the reconcile goroutine. Cost decomposition at N=3: ~5.7 s in
cluster = ~2 s cold restic-on-S3 (from the S3 benchmark, lower in-region) + ~3.5–4 s pure Job
lifecycle (schedule + container start + log read + delete). Both terms vanish with a warm,
long-lived lister.

**B. Self-trigger loop (`discovery_controller.go`).**
`updateInventoryStatus` (:285) writes `repo.Status.LastDiscoveryTime = metav1.Now()`
**unconditionally** every reconcile (:289) via `Status().Update`. `SetupWithManager` (:320)
registers `For(&BackupRepository{})` with **no predicate**. So the status write re-enqueues the
controller instantly; `RequeueAfter: interval` (:165) never fires. Live proof: the operator log
shows discovery reconciling every ~4–6 s (≈ its own duration) with no ClusterBackups present to
trigger the other watch, and `LastDiscoveryTime` advancing every ~6 s. This is the ~10×
amplifier: without it, a 5.7 s cycle once per minute is ~10 % duty; with it, it is ~100 %.

**C. Cross-controller Job-event storm.**
`backuprepository_controller.go` `SetupWithManager` does `.Owns(&batchv1.Job{})`. Because each
discovery Job is owner-referenced to the BackupRepository (B above), every discovery Job's
create/update/delete events (a fresh Job every ~6 s) wake the `backuprepository` reconciler →
~2700 no-op reconciles in 33 min. Cheap each (15 ms) but pure API/cache waste.

## Validation on the crucible — the same measurement, re-run against the fixes

Same instrumentation, same live RKE2 / Ceph crucible, operator built from this branch:

| controller | 0.3.0 — 33 min run | 0.3.1 — **58.6 min** run |
|---|---:|---:|
| **discovery** | **1991 s** over 350 reconciles (5.69 s each) | **9.9 s** over 101 (98 ms each) |
| **backuprepository** | 40.8 s over **2700** reconciles | 0.67 s over **56** |

Discovery spends **~200x less reconcile time across a run that is 1.8x longer**, and its worker is
never blocked (`workqueue_longest_running_processor_seconds{discovery}` stayed at 0 — it was up to
5.7 s before). backuprepository does **48x fewer** reconciles.

The terminating-namespace fix was verified against the real stuck object rather than a mock: with
`m2-restore` still wedged (22 minutes and counting), deploying it unfroze discovery immediately —
`lastDiscoveryTime` went from stuck at `00:07:26` to `00:26:28`, snapshot count resumed climbing
32 → 60, and the "being terminated" errors stopped.

**Suite result: 37 passed, 2 failed, 4 skipped, finished in 58.6 min.** For comparison the 0.3.0
baseline failed `m1_reliability`, `m1_discovery`, `m1_repository` and an m2 restore, and then blew
its 60-minute `go test` budget outright — the binary panicked and the report was lost.
`m1_discovery` and `m1_repository` now pass. The two remaining failures:

- **m2 `[BeforeAll]` — "namespace m2-restore still terminating"**: the spec waits 300 s for the
  namespace to be gone before recreating it, and that is the SAME namespace left wedged by the
  previous run's leaked finalizer. It was left in place deliberately, to prove the discovery fix
  against a real stuck object; the cost was this collateral failure. It is the leak (below), not
  the restore logic.
- **m1 OOM — "crashed volume reported as a silent success"**: a SIGKILLed mover's volume ended
  `Completed` instead of `Failed`. Pre-existing and already on the M3 flaky list; it lives in the
  backup/mover result path, which this patch does not touch.

## What shipped in 0.3.1 (all four, in this order)

1. **Self-trigger killed** — `inventoryChurnPredicate` on discovery's `For` watch drops an update
   only when the change is confined to the three inventory fields discovery itself writes.
   Everything else still wakes it (the `Initialized` flip, another controller's status write, any
   metadata change — which matters because `BackupRepositorySpec` is empty, so generation never
   moves for this kind). Discovery now honours `RequeueAfter: interval` instead of spinning.
   **~10x duty-cycle reduction** on its own.
2. **Job-event storm killed** — the inventory Job carries a plain owner reference instead of a
   controller reference, so `backuprepository`'s `Owns(&batchv1.Job{})` (which matches only the
   controller owner) stops waking on every listing Job event. Removes ~2700 no-op reconciles / 33 min.
   GC still cascades on repository delete.
3. **Inventory moved off the reconcile worker** — `inventoryTracker` runs each `SnapshotLister`
   pass on a background goroutine, single-flight per repository, and re-enqueues the repository
   through a `source.Channel` when it lands (plus a 30 s watchdog requeue in case a wake is ever
   lost). Reconcile consumes a finished result and does only the fast in-memory work (project, GC,
   status). Deliberately does NOT change the `SnapshotLister` contract, so the production lister
   stays a simple blocking call and the envtest stub is untouched. A panicking pass is recovered
   and surfaced as a normal inventory error, so a repository can never wedge as in-flight.
   This is what makes MULTIPLE repositories stop queueing behind one another — the single-repo case
   was already fine after fix 1.
4. **Harness** — the crucible suite budget moved 60 m → 90 m and became `CRUCIBLE_TIMEOUT`.

Still open, deliberately: the cold O(N) cost per pass is unchanged (a pass still re-scans the whole
repository from a cold cache). It is now paid off the worker and once per interval, so it no longer
starves anything; a warm/incremental inventory (cold O(N) → warm O(Δ)) remains the next lever if a
repository grows into the thousands of snapshots. `--no-lock` still belongs to M4, with `prune`.

## Candidate fixes as ranked at audit time (kept for the record)

1. **Kill the self-trigger (B).** Add a status/generation predicate to discovery's `For`
   watch (drop self status-only updates) and/or only bump `LastDiscoveryTime` when the
   inventory actually changed. Preserve prompt first-inventory (the `Initialized:
   false→true` transition) and the post-run ClusterBackup-watch nudge. **Impact ~10×,
   effort low, risk low-med.** The single biggest win.
2. **Decouple discovery's Job from `Owns(Job)` (C).** Don't make the discovery Job a
   *controller*-owned child of the BackupRepository (plain owner ref for GC without
   `controller=true`, or a predicate on backuprepository's `Owns(Job)` that ignores
   discovery Jobs). **Impact: −2700 reconciles, effort low, risk low.**
3. **Stop spawning a cold Job per cycle (A).** The architectural item — roadmap's "evaluate
   throughput models." Options to weigh with data: (a) long-lived warm-cache lister
   (in-operator or sidecar) — removes Job overhead AND turns cold O(N) into warm O(Δ);
   (b) async Job that does not block the reconcile worker (spawn, return, finish on a
   Job-complete watch); (c) cached/incremental inventory. **Impact: removes the ~4 s fixed
   overhead + the O(N) tail, effort high, risk med-high — needs a design decision.**
4. **Harness (roadmap step 4).** Raise the `go test` budget (60 m was overrun), isolate/prune
   the shared "dr" repo between specs, and root-cause the m2-Recreate **RBD storage flake**
   (Ceph `BLUESTORE_SLOW_OP` on osd.1 + twin-PVC image-not-found) — decide if it is a
   restore-flow bug or crucible Ceph tuning.

Note: discovery's `--no-lock` stays deferred to M4 (it lands with `prune`, the first exclusive
lock — it buys nothing before that).

## The flakiness chain — found during the 0.3.1 validation run

The single most useful thing the validation run produced was not a number, it was a **causal
chain** that explains how one stuck object freezes the whole inventory:

1. An m2 restore leaves a PVC behind carrying the finalizer
   `snapshot.storage.kubernetes.io/pvc-as-source-protection` — **with zero VolumeSnapshots left in
   the cluster**, so the finalizer is orphaned and protects nothing.
2. The PVC therefore never finishes deleting, so its namespace stays `Terminating` indefinitely
   (observed: 19 minutes and counting).
3. Discovery's projection pass hits that namespace. It exists (the `IsNotFound` guard does not
   catch it), but the API server rejects every create in it.
4. That error aborted the pass **before** it recorded the inventory, so `lastDiscoveryTime` froze
   and every retry re-ran a FULL re-inventory — a fresh `restic snapshots` Job every few seconds,
   indefinitely.

So a single leaked finalizer took discovery out of service for the rest of the run. Any spec that
depends on a fresh inventory — `m1_discovery`'s GC-after-forget above all — then fails on a
timeout, with a *different* victim depending on which spec was running when the leak happened.
That is exactly the "fails differently every run" signature M3.1 was chartered to explain.

Step 3 is fixed here (a terminating namespace is skipped like an absent one). Steps 1–2 are a
**leak-check invariant violation** (roadmap delta 5: zero residual VS/VSC/PVC after every
scenario) and remain open — see the RBD item below, which is very likely the same incident seen
from the other end.

Also still open: **any** projection error still discards the consumed inventory, so the retry pays
for a full re-listing. Making a pass survive a per-group failure (accumulate, record the inventory,
report the failures) is the follow-up.

## Open — the m2-Recreate RBD flake (investigated, NOT fixed: needs reproduction)

**Not a throughput problem, and the more worrying of the two findings.** During the audit run the
m2 Recreate restore hung ~20 min in `ContainerCreating`:

```
MountVolume.MountDevice failed for volume "rst-m2-restore-m2-recreate-data-twin":
  rpc error: code = Internal desc = error generating volume
  0001-0009-rook-ceph-0000000000000001-d5fc4ceb-…:
  Failed as image not found (internal RBD image not found: rbd: ret=-2, No such file or directory)
```

The PVC was `Bound`, `AttachVolume.Attach` **succeeded**, and the mount then failed because the
RBD image behind the volume handle did not exist. Ceph itself was healthy (81/81 PGs
`active+clean`, 3/3 OSDs up); the only warning was `BLUESTORE_SLOW_OP_ALERT` on osd.1.

What makes it worth a real investigation rather than a "flaky infra" shrug: **the m2 overwrite and
partial restores completed successfully on the same target volume just before**, and only the third
one found the image gone. The restore twin PV (`internal/rexposer/expose.go`) deliberately copies
the bound PV's `PersistentVolumeSource` verbatim — same CSI `volumeHandle` — and pins
`PersistentVolumeReclaimPolicy: Retain` precisely so that deleting a twin can never destroy the
user's volume. So either that guarantee held and the image vanished for an unrelated (Ceph-side)
reason, or something in the earlier restores' teardown released a PV bound to that same handle
under a Delete policy — which would be a data-destruction bug, not a flake.

Reproduction plan (the audit rule applies — do not fix on this hypothesis):
1. Run `mise run test 'm2'` alone on a fresh crucible, so no other spec's churn is in play.
2. Between each of the three restores, record `rbd ls -p <pool>` from the toolbox plus
   `kubectl get pv -o custom-columns=NAME:.metadata.name,HANDLE:.spec.csi.volumeHandle,POLICY:.spec.persistentVolumeReclaimPolicy,PHASE:.status.phase`.
3. The question to answer: does the image disappear at a specific teardown step, or only under the
   concurrent Job churn of the full suite (pointing at Ceph/osd.1 slow-ops instead)?

Until that is answered, this is tracked, not diagnosed.

## Minor — PodSecurity warn spam (analysed, deliberately not "fixed" here)
Every discovery cycle logged `would violate PodSecurity "restricted:latest"` for the mover pod.
The namespace enforces `baseline`, which admits it; only the `warn=restricted` label fires.

It is not cheaply silenceable, and the obvious tweak does not work: the warning is driven by
`runAsUser=0` / `runAsNonRoot != true` (internal/mover/job.go), not by the capability list, so
dropping `DAC_OVERRIDE` would not quiet it. Root + `DAC_OVERRIDE` is a deliberate, documented
choice for the data path (a mover reads files it does not own —
spec/03-security-and-tenancy.md §6). Silencing the warning properly means giving the mover image
a non-root user model, which is its own lot with its own restore-fidelity risk (CHOWN/FOWNER/
MKNOD on restore).

Worth noting separately: `OpSnapshots` — discovery's listing Job — mounts **no** data volume
(`PVC: nil`) and still receives `DAC_OVERRIDE` from the catch-all branch of `moverCapabilities`.
Narrowing that one operation to no added capabilities is a genuine least-privilege improvement
(independent of the warning). Left out of this patch because it wants crucible validation of the
restic cache path under a reduced capability set.

Meanwhile the **volume** of the spam is down ~10x on its own: the self-trigger fix means discovery
builds one Job per `discovery.interval` instead of one every ~6 s.

## Artifacts (this session, scratchpad)
- `m31-findings.md` — raw running findings.
- `m31-scaling.csv` — local filesystem curve. `m31-s3-scaling.csv` — Hetzner S3 curve
  (discard the N=500 row: the repo was concurrently emptied by teardown).
- `m31-crucible-metrics.log` — full per-tick scrape. `m31-metrics-final.txt` — final /metrics.
- `m31-analyze.py` — the offline analyzer.
