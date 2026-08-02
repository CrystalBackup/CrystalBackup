# `soak-collect` / `soak-export` — what the Go side has to do

This is the specification for the two subcommands `hack/soak/manifests/collector.yaml` and
`hack/soak/collect.sh` are written against. **Neither exists yet.** Everything else in
`hack/soak/` is complete and usable the moment they do; the fallback CronJob is what runs until
then.

It is written as a lot brief: what to build, where it goes, and — for each decision that has a
tempting wrong answer — which one to take and why.

## 0. The rule that shapes everything

Both are **subcommands of the operator binary**, dispatched from the bare `os.Args` switch in
`cmd/main.go` alongside `selfcheck` and `report`. The comment there states the rule and the
reason: a second artefact means a second supply chain to sign, scan and get wrong, and this is
not the beginning of a `crystalctl` — that is M7, a different audience, from outside the
cluster. These two run *as* the operator's neighbour, inside it.

Practical consequence, from that same comment: `go build cmd/main.go` compiles ONE file, so all
of this lives in `internal/`. `internal/soak/` is the natural home. It will want
`internal/selfcheck`'s `Collect()` and its `Redactor`, so expect to export a little more of the
latter (see §6).

```
crystal-backup soak-collect [flags]     # the resident loop; runs until killed
crystal-backup soak-export  [flags]     # writes one tar.gz to stdout, or --status to stderr
```

## 1. `soak-collect` — flags

| flag | default | meaning |
|---|---|---|
| `--data-dir` | `/var/lib/crystal-backup-soak` | the PVC mount. Everything is written here and nowhere else. |
| `--max-bytes` | `512Mi` | hard cap on the on-disk footprint. §4. |
| `--metrics-url` | — | the operator's `/metrics`. Required; without it, say so and exit non-zero rather than run blind. |
| `--metrics-insecure-skip-verify` | `false` | §2. |
| `--metrics-interval` | `60s` | scrape cadence. |
| `--metrics-resolution` | `5m` | downsample window. §3. |
| `--mover-sample-interval` | `15s` | how often mover pods are sampled for the high-water marks. §5. |
| `--selfcheck-interval` | `24h` | |
| `--state-interval` | `1h` | CR snapshots. |
| `--operator-namespace` | `$POD_NAMESPACE` | same resolution as everywhere else. |
| `--salt-method` | `auto` | where the redaction salt comes from: `auto` (derived from the operator namespace's UID) or `from-secret`. §6. |
| `--redaction-salt-file` | — | REQUIRED by, and only by, `--salt-method=from-secret`. Recorded so `soak-export` in the same container finds it without being told; passing it under `auto` is refused rather than ignored. |
| `--kubelet-stats` | `false` | opt-in; needs `nodes/proxy`. §5. |
| `--heartbeat-check` | `false` | not a collector: reads the heartbeat file, exits 0 if it is fresh, non-zero if it is not, prints nothing. This is the liveness probe. |

Exit non-zero at startup — loudly, with the reason — if `--data-dir` is not writable, if the
free space there is below `--max-bytes`, or if `--metrics-url` cannot be scraped after the first
three attempts. A collector that starts happily and collects nothing is the failure mode this
whole kit exists to avoid, and it must not be possible to reach it by accident.

The salt flags are the same discipline applied to a different failure: an unknown `--salt-method`,
a `from-secret` with no readable Secret, an `auto` that cannot read its namespace, and a
`--redaction-salt-file` passed under `auto` (where it would be silently ignored) are all startup
REFUSALS. None of them falls back to another method: a collector that quietly changed method
would produce an archive claiming one guarantee and holding another, and nothing about the
running pod would show it.

**These are flags, not chart values.** The collector is `hack/soak/manifests/collector.yaml`,
applied with `kubectl`; nothing in `charts/crystal-backup/` renders it today. Moving it into the
chart is its own lot — Deployment, PVC, ServiceAccount, ClusterRole and bindings, the two
NetworkPolicies, all release-prefixed, and the image digest, which is the whole reason to do it
(the standalone manifest makes an administrator hand-copy four things the chart already knows,
and soaking a different build from the one under test for a fortnight has nothing to signal it).
That lot maps these flags onto values; a value added before anything renders it would be an inert
knob.

## 2. Metrics — scrape, auth, TLS

The operator serves `/metrics` on `:8443` with `--metrics-secure` (default true) and
controller-runtime's `filters.WithAuthenticationAndAuthorization`. A scrape is therefore a
TokenReview plus a SubjectAccessReview on the non-resource URL `/metrics`.

- **Credential**: the pod's own projected ServiceAccount token, read from
  `/var/run/secrets/kubernetes.io/serviceaccount/token`, sent as `Authorization: Bearer`. Read
  it per scrape, not once at startup — the projected token is rotated, and a collector that
  cached it starts failing on day 3 with a 401.
- **Authorization**: the chart's `crystal-backup-metrics-reader` ClusterRole, bound to the
  collector's SA by `manifests/collector.yaml`.
- **TLS**: the metrics server presents a self-signed certificate; the chart's own ServiceMonitor
  sets `insecureSkipVerify` for exactly this reason. `--metrics-insecure-skip-verify` is
  therefore expected to be on, and the flag exists rather than being implicit so that a cluster
  which has wired a real cert can turn it off. Do not silently fall back from a verification
  failure to no verification.
- **Parsing**: `github.com/prometheus/common/expfmt` is already in the dependency graph
  (client_golang pulls it). Do not write a text-format parser.

Scraping the operator directly is what removes the Prometheus dependency from the whole kit.
That is the single largest adoption difference in this design: the soak now works on a cluster
whose monitoring stack has 24 hours of retention, or none at all.

## 3. Metrics — what is stored, and what is not

**Raw scrapes are never kept.** One exposition a minute for fourteen days is millions of lines
and would blow the cap on day two. Each series `(name, labels)` is reduced, per
`--metrics-resolution` window, to:

```
{ "t": <window start, unix seconds>, "last": <float>, "min": <float>, "max": <float>, "n": <samples in window> }
```

`min`/`max` are not decoration: a queue depth that touched the concurrency limit for four
minutes is invisible in a five-minute `last`, and that spike is a finding. For counters `last`
is the only meaningful field and `min`/`max` cost nothing.

Storage: one gzipped NDJSON segment per UTC day, `metrics/YYYY-MM-DD.ndjson.gz`, one line per
series per window, with the label set carried in the line. Segment files are append-only and are
closed at midnight; rotation deletes whole segments (§4).

A series that disappears from the exposition (a CounterVec child after a restart, a namespace
that was deleted) must be recorded as ABSENT for the windows it was absent from, not carried
forward at its last value. The difference between "0" and "not there" is load-bearing in this
project — `internal/metrics/names.go` documents the alert that could not fire because of exactly
that confusion — and an export that smooths it over would recreate the bug in the data.

### 3b. The series that must survive the cap

Scraping `/metrics` takes everything, which is right: a curated scrape would inevitably omit the
one series that turned out to matter. But when the cap bites (§4) something has to go first, and
the order is not arbitrary. Keep these at full resolution to the end; degrade the rest.

They are chosen for what their **trend** says over a fortnight. A soak buys exactly one thing a
test suite cannot — the time axis — and a series whose value is only meaningful instantaneously
wastes it.

**Repository growth and retention effectiveness.** The slowest signal there is, and the one no
other test can produce: does deduplication hold as the corpus churns, and does retention actually
reclaim?
`crystalbackup_repository_size_bytes` · `crystalbackup_repository_snapshot_count` ·
`crystalbackup_backup_protected_bytes` · `crystalbackup_backup_added_bytes_total`
— size alone is unreadable; it means something only against protected bytes (the denominator) and
against added bytes (the churn that fed it). A repository growing faster than its input is the
headline finding of a soak, and it takes a week to become visible.

**Maintenance, the operation that blocks windows.**
`crystalbackup_repository_last_maintenance_timestamp_seconds` ·
`crystalbackup_repository_last_check_timestamp_seconds` ·
`crystalbackup_repository_last_check_success` · `crystalbackup_repository_stale_locks` ·
`crystalbackup_repository_locks_reaped_total`
— **there is no prune-duration series in `internal/metrics/names.go`.** Maintenance is observable
only as the timestamp of the last successful one, so a forty-minute prune has to be reconstructed:
a plateau in the timestamp, then a step, with `mover_active` and `mover_queue_depth` raised in
between. That is why those three keep company here. A stale lock that appears at 3am and clears by
6am exists only if something was sampling.

**Failure counters.** `crystalbackup_backup_failures_total` ·
`crystalbackup_restore_failures_total` · `crystalbackup_mover_job_retries_total` ·
`crystalbackup_clusterbackup_runs_total` · `crystalbackup_externalsync_failures` ·
`crystalbackup_webhook_denials_total`
— a counter's *value* says nothing; its slope over fourteen days is the soak's core evidence.
Paired with `crystalbackup_backup_failures` (the consecutive-failure gauge, which shows the
transient that recovered) and with
`crystalbackup_backup_last_success_timestamp_seconds` /
`…_last_failure_timestamp_seconds`, which together reconstruct every namespace's timeline
without a single log line.

**Durations, because drift is the finding.** `crystalbackup_backup_duration_seconds_bucket` ·
`crystalbackup_backup_last_duration_seconds` · `crystalbackup_exposure_ready_wait_seconds_bucket`
— a p95 that moves from four minutes to twenty-five over ten days is precisely what vanishes
without continuous capture, and the exposure histogram is where real CSI latency under real
contention shows up.

**Contention and queueing.** `crystalbackup_mover_active` · `crystalbackup_mover_queue_depth` ·
`crystalbackup_mover_concurrency_limit` · `crystalbackup_pvc_volumesnapshot_count`
— instantaneous state whose *trend* is the point: a queue pinned at the limit for six hours means
the window is too small, and a snapshot pileup accumulates over days and is invisible at any
single moment.

**Coverage drift.** `crystalbackup_backup_total` · `crystalbackup_schedule_active` ·
`crystalbackup_discovery_projected_backups` · `crystalbackup_discovery_orphan_snapshots` ·
`crystalbackup_discovery_last_success` · `crystalbackup_externalsync_lag_snapshots`
— a schedule that went inactive on day six, or a sync lag that has been climbing all fortnight.

**Identity.** `crystalbackup_build_info` — the only way to know the operator restarted or was
upgraded mid-soak, and the thing that pins which build produced the whole window.

**And `ALERTS{alertname=~"Crystalbackup.*"}` if a Prometheus is present.** Not a crystalbackup_
series and not available from the operator's own `/metrics`, but if the admin does run Prometheus
it is the single most informative series in a soak: which of the eleven rules fired, when, and for
how long. `soak-export` should include it opportunistically and mark it NOT MEASURED otherwise.

Deliberately **not** in this list: `schedule_period_seconds`, `schedule_created_timestamp_seconds`,
`externalsync_created_timestamp_seconds` and the rest of the static configuration surface. Every
daily self-check carries them already, where a change between two days reads as a change.

## 4. The footprint cap, and what happens when it is reached

The collector is a workload that can perturb what it measures. A PVC that fills is, on a
node-backed StorageClass, precisely the DiskPressure eviction the soak was deployed to observe.
So:

- everything is written under `--data-dir` and nothing anywhere else — no node ephemeral
  storage, no `emptyDir` beyond the 64Mi `/tmp` the manifest grants;
- after each segment close, and at least every ten minutes, total on-disk bytes under
  `--data-dir` are measured;
- **above `--max-bytes`, the oldest raw metrics segment is deleted**, then the oldest CR-state
  segment, then the oldest error-log segment — in that order, one at a time, until under the
  cap. Never the high-water marks, never the self-check reports, never the event stream: those
  are the parts that cannot be reconstructed and they are two orders of magnitude smaller;
- **every deletion is recorded** in the manifest as `{stream, from, to, reason: "cap"}`. An
  archive that lost its first three days says so on its first page. This is not optional and it
  is not a log line: it is a field in the manifest that `collect.sh` reads and reports;
- if free space on the volume falls below 64Mi despite the cap (someone else's data, a
  filesystem smaller than the PVC claims), the collector stops writing raw samples entirely,
  keeps only aggregates and the event stream, and records `degraded: true` with the timestamp.
  It does not exit — a degraded collector still holds the high-water marks — and it does not
  fill the disk.

## 5. High-water marks — the part 0.6.1 could not prove

`internal/mover/profiles.go` says it in its own words: the shipped limits are "deliberately
generous", their job is "to keep ONE runaway mover from taking a node down, not to right-size
restic", and the 20Gi cache ceiling is "a CEILING AGAINST A RUNAWAY, not a sizing estimate"
because "the load test that would give us the real curve has not been run". This stream is that
curve, measured on real data instead of paid infrastructure.

What to produce, per **operation class** (`data`, `repo-heavy`, `repo-light`, `manifests` —
`builtinSpecs` in `profiles.go` is the authority for which operation is in which class):

| mark | source | note |
|---|---|---|
| peak memory per mover pod | the mover's own `MoverResult`, read off the terminated pod's termination message | **the source that works** — exact, and available for pods no sampler can see. See "Why polling cannot answer this" below |
| peak memory per mover pod (fallback) | `metrics.k8s.io` PodMetrics, sampled every `--mover-sample-interval` | keep the max per pod, then the max and the p95 per class. This is the highest SAMPLE, not the peak |
| OOM kills | pod `containerStatuses[].lastState.terminated.reason == OOMKilled` | definitive, always available, and the one number that says a limit is too low |
| evictions | pod phase `Failed`, reason `Evicted`, plus the kubelet message | the emptyDir-over-limit kill lands here, naming the volume and the number |
| restic cache high-water | kubelet `stats/summary`, per-pod volume stats | `--kubelet-stats` only; otherwise NOT MEASURED |
| longest prune, longest backup, longest check | mover Job start/completion timestamps | wall clock, which is what blocks a window — the duration histogram measures the operator's view of the same thing and both should be in the archive |

Attribution: a mover pod's operation comes from its owning **Job**, which the operator labels.
Where the operation is not directly recoverable from the labels the controller stamps, derive it
from the Job and record `unknown` — **never guess a class**, because a `data` peak filed under
`repo-heavy` would be read as evidence that prune's 8Gi limit is right when it is evidence about
backup's 4Gi.

Sampling honestly: a pod that lives eleven minutes is sampled ~44 times at 15s, and the true
peak is above the highest sample. Record `samples: n` beside each peak so the reader knows how
much confidence the number carries, and record the pod's lifetime. A peak from three samples is
a different claim from a peak from four hundred.

If `metrics.k8s.io` is absent (no metrics-server), record the memory marks as **NOT MEASURED**
with the reason. Not zero, not omitted. This project has been bitten five times by an absence
reading as health. And distinguish the two ways a class ends up with no peak: **no pod of that
class ran** (an idle class) versus **pods ran and none was ever sampled** (a measurement
failure). Saying the first over the second is a false statement in the same object that carries
the pod count.

### Why polling cannot answer this, and what does

Measured on a real crucible cluster over four hours: **63 mover pods, every one of them 0
samples**, lifetimes from 0.9s to 17s, with metrics-server installed, healthy, and returning data
for long-lived pods throughout. The cause is structural, not a bug in the sampling loop —
metrics-server scrapes on its own ~15s cadence and exposes a pod only after it has scraped it, so
a container that lives one second never exists for it. **Sampling faster changes nothing: the
data is not there to be sampled.**

The mover therefore measures itself and reports through the channel that already exists. Its
`MoverResult` gains, and this stream prefers:

| field | what it is | what it means |
|---|---|---|
| `peakRSSBytes` | restic's peak resident set (`ru_maxrss`, RUSAGE_CHILDREN) | **the sizing number.** Anonymous + mapped, no page cache — what a memory limit must cover |
| `shimPeakRSSBytes` | the same for the crystal-mover process | resident at the same time, so a limit covers the SUM |
| `cgroupPeakBytes` | cgroup v2 `memory.peak` | **an upper bound, not a sizing target.** It is the peak of `memory.current`, which counts reclaimable PAGE CACHE; a backup streams a volume through it, so this can sit an order of magnitude above the RSS peak, and the kernel reclaims that cache long before it OOM-kills anything |
| `memoryLimitHits` | `memory.events` `max` | 0 ⇒ the limit was never pressed, so a large cgroup peak is cache the container was merely allowed to keep |
| `memoryOOMKills` | `memory.events` `oom_kill` | not redundant with the kubelet's `OOMKilled`: when restic is killed and the shim survives to report, Kubernetes records no OOM anywhere |

Constraints this has to respect, and does: the termination message is capped at **4096 bytes**
(`mover.Fit` is applied last, after the figures are stamped), **cgroup v1 is deliberately not
reported** (its `memory.max_usage_in_bytes` exists, but in the host cgroup namespace v1 uses,
nothing inside the container distinguishes its own directory from the NODE's), and an unreadable
cgroup leaves the figure absent rather than zero. The RSS peaks need no cgroup at all, so a v1
node still gets the number that sizes a limit.

Every figure in `marks.json` names its provenance in `classes[].memory.source`:
`mover-reported` or `sampled`. Never inferred from which field happens to be populated.

## 6. Redaction — at export, not at collection

**What lands on the PVC is unredacted**, deliberately, for two reasons: it is the admin's own
cluster data on the admin's own volume, and a token cannot be re-redacted — redacting at
collection time would throw away the ability to do it better later, and would make the data
useless to the admin who wants to read their own soak.

**`soak-export` redacts, once, on the way out**, using `internal/selfcheck`'s `Redactor` seeded
from the salt `--salt-method` resolved — the same construction and the same salt as the daily
self-check, so a namespace is the same token in a metrics series, in an event, in a log line and
in day 9's report. That cross-stream identity is the whole reason the salt is stable.

### Where the salt comes from — a named method, never an implicit one

| `--salt-method` | the salt | `saltSource` in every report |
|---|---|---|
| `auto` (default) | `SHA256("crystalbackup-soak-salt-v1" ‖ the operator namespace's UID)` | `namespace-uid` |
| `from-secret` | the bytes of `--redaction-salt-file`, verbatim, ≥ 32 | `caller-supplied` |
| — (one-shot `crystal-backup selfcheck`, unchanged) | 32 bytes from `crypto/rand`, per report | `random-per-report` |

`auto` is the collector's default because a soak's product IS the correlation, and a Secret is a
thing that can be lost or silently regenerated — a `helm uninstall`, a recreated namespace,
somebody redoing it "to be safe". When that happens mid-soak the series breaks with nothing to
signal it: seven days of tokens on one side, seven on the other, no way to join them. A namespace
UID is unique per cluster, constant for the life of the namespace, and **nothing generates it**,
so there is nothing to regenerate.

The UID is **not** the key. It is a 36-character string that would clear the 32-byte floor by
accident rather than by construction, and a raw UID used as a key here could not be reused for
anything else. Hashing under a versioned domain separator fixes both, and lets the derivation be
revised later without colliding with archives made under v1.

**Nothing may ever generate this Secret for the admin, chart included.**
`charts/crystal-backup/templates/webhook.yaml` already states the reason for the certificate it
does generate: "`lookup` would not fix it: Argo CD renders with `helm template`, which has no
cluster to look anything up in." Under continuous rendering the salt would change every refresh —
three minutes by default — and it would be **worse** than the certificate case, because a
regenerated certificate rolls the Deployment and gets noticed while a regenerated salt changes
nothing visible: the collector keeps running, the reports keep being written, and only the
correlation dies. This project ships Argo CD and Flux install pages, so those are its users.

**What `namespace-uid` promises, stated for somebody about to paste the file somewhere.** Against
a stranger reading a public issue, the UID is 122 bits they do not have. Against anyone who can
`get` the operator's namespace — anyone with cluster read access, now or later — the tokens are
**reversible by dictionary in seconds**, because namespace names come from a small guessable set.
Every report says exactly that in its redaction note. A report that leaves the cluster should be
re-run without a fixed salt at all.

Stated **positively** in all three cases, never inferred from an absent field, because the three
make different promises and are otherwise indistinguishable from the outside.

Two extensions to `internal/selfcheck/redact.go` are needed, and both are small:

1. `labelKinds` covers what the operator emits. A scrape adds more: `exported_namespace` (what a
   ServiceMonitor without `honorLabels` leaves behind — the chart's own comment explains how that
   silently defeated tenancy once already), `pod`, `instance`, `node`. Map them to `ns`, `pod`,
   `host`, `host`. Everything else stays passed-through-verbatim for the reason the existing
   comment gives: `origin`, `scope` and `result` carry the meaning of a series.
2. A constructor taking a caller-supplied salt AND its provenance. `NewRedactor` generates its
   own 32 random bytes; the soak needs `NewRedactorWithSource(full bool, salt []byte, source
   string)`, minimum 32 bytes, error below that. The source is reported and never branched on:
   32 bytes from a file and 32 bytes derived from a namespace UID are the same bytes to that
   package and make different promises to a reader, so it cannot be inferred and has to be
   passed.

`soak-export --full` disables redaction, exactly as `selfcheck --full` does, and the manifest
says so in a field `collect.sh` refuses to be quiet about.

**Free text** — the operator's error lines, event messages — goes through `Redactor.Detail()`
after every identifier the archive touches has been `Learn()`ed. That is the existing mechanism
and it is why `Learn` exists; the collector has seen every namespace, PVC, location and CR name
in the cluster over fourteen days, so its registry is far better than anything reconstructed
afterwards. What it still cannot catch is a name nobody enumerated: a path inside a volume, a
restic snapshot ID, a URL inside a library's error string. The manifest must SAY that, in the
words `collect.sh` prints, so the admin reads the two streams before sending.

## 7. `soak-export` — the archive

```
crystal-backup soak-export [--data-dir …] [--redaction-salt-file …] [--full] [--since 14d] [--status]
```

Writes a `tar.gz` **to stdout** — never to a file. That is what makes
`kubectl exec … > archive.tar.gz` work without `tar` in the image (`kubectl cp` would need one)
and without a shell. Nothing else may be written to stdout, ever; progress goes to stderr.

Layout:

```
MANIFEST.json                     §8 — the first thing collect.sh reads
COLLECTION-REPORT.txt             the same thing in prose, for a human who opens the tarball
selfcheck/<rfc3339>.json          the daily reports, already redacted
metrics/<day>.ndjson.gz           the downsampled series
highwater/marks.json              §5, per class, with sample counts and NOT MEASURED where true
events/events.ndjson              every Warning captured, deduplicated by UID, in arrival order
logs/operator-errors.ndjson       error-level lines with their timestamps
state/<day>.ndjson.gz             hourly CR snapshots
uptime.json                       §9
```

`--status` writes no archive: one screen on stderr saying how long the collector has been up,
what each stream holds, what the cap has dropped, and what is NOT MEASURED. This is what the
admin runs on day 1 and day 2 to find out the soak is actually running, and it is the cheapest
defence against discovering on day 14 that nothing was collected.

## 8. `MANIFEST.json` — the anti-vacuity contract

`collect.sh` derives its verdict entirely from this file. It must contain, per stream:

```json
{
  "schema": "crystalbackup.soak/v1",
  "operatorVersion": "…", "collectorStartedAt": "…", "exportedAt": "…",
  "redaction": { "mode": "hashed|full", "saltDisclosed": false, "note": "…" },
  "unredactedNote": "the sentence about what free text could not be tokenised",
  "streams": [
    { "name": "metrics", "status": "OK|EMPTY|DEGRADED|NOT_MEASURED|DISABLED",
      "items": 41, "firstSample": "…", "lastSample": "…",
      "coverage": { "requestedDays": 14, "observedDays": 13.6 },
      "drops": [ { "from": "…", "to": "…", "reason": "cap" } ],
      "note": "…" }
  ]
}
```

Rules that are not negotiable:

- a stream that was never enabled is `DISABLED`, never `EMPTY`;
- a stream that could not be measured is `NOT_MEASURED` with a reason, never `0`;
- `EMPTY` is reserved for "read successfully and there was nothing", and each stream declares
  whether empty is a possible healthy answer (`emptyIsHealthy: true` for events and error logs,
  `false` for metrics and self-checks);
- `coverage.observedDays` is computed from the data, not from the flags. The gap between what
  was asked for and what is there is the single most important number in the file.

`soak-export` **always writes an archive**, even when every stream is empty. The evidence of
nothing is evidence, and it must arrive as a file with a manifest that says so — not as a
non-zero exit and no artefact.

## 9. The collector observes itself

A collector that died on day 4 and was restarted on day 11 must not produce an archive that
reads like a fortnight. `uptime.json` carries the process's start times and every gap between
them, derived from the heartbeat file (written every loop, which is also what `--heartbeat-check`
reads for the liveness probe). `MANIFEST.json` carries the total observed fraction. `collect.sh`
turns a fraction below 0.9 into a THIN verdict.

## 10. What this deliberately does not do

- **No writes to the cluster.** Not a Lease, not an Event, not a ConfigMap. The RBAC in
  `manifests/collector.yaml` has no write verb in it, and it should stay that way: a soak kit
  that mutates the cluster it is measuring has to be argued about on every review, and the
  argument is not worth what a Lease would buy.
- **No exec into mover pods** to measure the cache from inside, though the operator's own
  ClusterRole would allow it. Running commands inside a pod during a backup is perturbation, and
  the kubelet stats endpoint answers the same question from outside.
- **No aggregation of one cluster's data with another's.** Every archive stands alone.
- **No upload.** The archive lands on the admin's machine, and what happens next is theirs.
