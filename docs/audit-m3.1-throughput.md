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

## Candidate fixes (ranked; measurement-driven, NOT yet chosen)

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
