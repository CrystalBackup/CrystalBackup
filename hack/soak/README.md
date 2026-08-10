# The soak — running this build for two weeks and telling us how it went

<!-- No version in the title or the first line, on purpose. Both said "0.6.1" for five releases
     after 0.6.1 shipped, which is the same staleness this kit exists to prevent in other people's
     measurements. The release you are installing is the one `helm upgrade` gave you, and the
     archive records it in MANIFEST.json and on every heartbeat line. -->

The release you are installing is offered for testing rather than production for one reason: nobody
has yet run it alongside an incumbent backup tool, on real data, for two weeks. Every other M6 exit
criterion is met. This one is not, and it cannot be met by a test suite — a suite runs what someone thought
of, and the findings a soak produces are the ones nobody thought of. A pod evicted at 3am. A
prune that ran forty minutes and blocked the window. A repository that stopped deduplicating on
day six. A metric that drifted for ten days.

The value is that **nothing is scripted**. Real sizes, real cadence, real S3 latency, real
contention with your other workloads. This kit does not change any of that; it only makes sure
that when something happens at 3am on day nine, there is still a record of it on day fourteen.

**We get no access to your cluster.** At the end you hand back one archive, and everything in it
that could identify you has been replaced by a pseudonym first.

## What you are installing, and alongside what

**Alongside your existing backup tool. Never instead of it.** For fourteen days you have two
backup systems covering the same data, and that is the point: the incumbent is what you actually
rely on, and CrystalBackup is what is being measured against it. If at any moment the honest
answer to "could I restore this today?" depends on CrystalBackup, the scope is too large — see
below.

Two things go in, and they are one install:

| | what | footprint |
|---|---|---|
| CrystalBackup | the operator itself, per the install docs | one pod |
| `soak.enabled=true` | the soak collector, from the same chart: one pod, one 1Gi PVC, read-only cluster RBAC | one pod, 1Gi |

The collector is **part of the chart**, turned on with one value. It is not a separate manifest to
apply, and deliberately so: it runs `crystal-backup soak-collect`, a subcommand of the operator
binary, and the chart gives it *the image the operator is actually running*. A collector built
from a different commit than the operator under test describes a cluster that does not exist, for
a fortnight, with nothing to signal it.

The redaction salt — what makes the same namespace the same token on day 2 and on day 13 — is
**derived from your operator namespace's UID** and needs no Secret. `--salt-method=from-secret`
is there if you would rather hold the salt yourself; see below.

Nothing else. No Prometheus is required — the collector scrapes the operator's `/metrics`
itself, so this works on a cluster whose monitoring keeps 24 hours of history, or none.

## Picking the scope

Pick namespaces where **all three** are true:

1. **The data is real.** A synthetic corpus measures nothing a test already covers. It should be
   volumes whose size, file count and churn you did not choose for this exercise.
2. **Losing the CrystalBackup copy would cost you nothing.** The incumbent is still the backup of
   record for every namespace in scope, for the whole fortnight.
3. **They are not all the same shape.** One large slow-churn volume and one small high-churn one
   tell you more in two weeks than four copies of either. If you have a namespace with many small
   files, include it: that is where the M6 load test that was never run would have looked.

Three to six namespaces is a good soak. Twenty is not five times better — it is five times more
data to redact and one more reason for something to be evicted.

Set a schedule you would actually run in production. Daily is the default for a reason; hourly on
a large volume for two weeks is a different experiment (a valid one — just say so when you send
the archive, because it changes how every duration in it reads).

## Before you start: the baseline

Do these in order. The first two take a minute and save the fortnight.

```sh
# 1. reproduce the salt LOCALLY, so you can verify the archive at the end. Nothing is created:
#    this recomputes what the collector derives, SHA256("crystalbackup-soak-salt-v1" || the
#    namespace UID). Keep the file; it is not needed to RUN the soak, only to check it.
{ printf 'crystalbackup-soak-salt-v1'
  kubectl get ns crystal-backup-system -o jsonpath='{.metadata.uid}'
} | openssl dgst -sha256 -binary > soak-salt.bin

# 2. does this build have the collector?
kubectl -n crystal-backup-system exec deploy/crystal-backup -- /manager soak-collect --help
#    prints usage  -> turn on soak.enabled, below
#    "unknown"     -> your chart predates the collector: apply
#                     manifests/fallback-selfcheck-cronjob.yaml instead, and read the header of
#                     that file: you get the daily self-checks and nothing else.

# 3. install the collector. Nothing to edit, nothing to look up: the chart already knows the
#    image, the namespace, the metrics Service and the metrics-reader ClusterRole.
helm upgrade --install crystal-backup crystal-backup/crystal-backup \
  --namespace crystal-backup-system --reuse-values --set soak.enabled=true

# 4. the baseline self-check, kept as day zero. It has to be salted the SAME way as the soak or
#    day zero shares no tokens with day one, so hand it the file you just derived.
kubectl -n crystal-backup-system cp soak-salt.bin \
  "$(kubectl -n crystal-backup-system get pod -l app.kubernetes.io/name=crystal-backup \
     -o jsonpath='{.items[0].metadata.name}')":/tmp/soak-salt.bin
kubectl -n crystal-backup-system exec deploy/crystal-backup -- \
  /manager selfcheck --redaction-salt-file=/tmp/soak-salt.bin > soak-day0.json

# 5. write down, in a text file you will send with the archive:
#      - the incumbent tool and its version
#      - the namespaces in scope, their approximate size and file count
#      - the storage classes and CSI drivers behind them
#      - the object store: provider, region, whether it is on the same network
#      - anything already unusual about the cluster today
```

**Then check on day one and again on day two** that it is really collecting:

```sh
kubectl -n crystal-backup-system exec deploy/crystal-backup-soak -- /manager soak-export --status
```

One screen, no archive. This is the cheapest possible defence against discovering on day fourteen
that nothing was collected, and it is the single step most worth not skipping.

On day one that screen is mostly zeros, and **most of those zeros are correct** — a fresh collector
has collected nothing yet, and `highwater` stays `NOT MEASURED` until your first backup runs, which
on a nightly schedule can be a day away. Which zeros are expected and which are a finding is
written out under [what a fresh collector legitimately looks
like](#5-what-a-fresh-collector-legitimately-looks-like--which-zeros-are-expected). Read it before
you conclude anything from an empty screen, and in particular before you restart the collector.

**KEEP `soak-salt.bin`.** You need it at the end, and it must never be in the archive: the tokens
are HMACs under it, and the value space (`production`, `staging`, your customer's name) is small
enough that anyone holding the salt reverses the whole archive in seconds.

With the **derived** default, be clear-eyed about what that sentence means. The salt's only input
is your operator namespace's UID, so **anyone who can `get` that namespace can recompute it and
reverse every token** — step 1 above is the whole attack, in one command. Against a stranger
reading a public issue it is 122 bits they do not have; against anyone with read access to your
cluster it is nothing. Every exported report says exactly this in its own redaction block. If an
archive is going anywhere wider than the maintainer, take it with no fixed salt at all.

**Why derived rather than a Secret you create.** A Secret can be deleted by a `helm uninstall`,
lost with a recreated namespace, or redone "to be safe" — and when that happens mid-soak the
correlation breaks with nothing to signal it: seven days of tokens on one side, seven on the
other, and no way to join them. Nothing generates a namespace UID, so nothing can regenerate it.
To hold the salt yourself instead, create the Secret and point the chart at it:

```sh
openssl rand -out soak-salt.bin 32
kubectl -n crystal-backup-system create secret generic crystal-backup-soak-salt \
  --from-file=salt=soak-salt.bin

helm upgrade --install crystal-backup crystal-backup/crystal-backup \
  --namespace crystal-backup-system --reuse-values \
  --set soak.enabled=true \
  --set soak.saltMethod=fromSecret \
  --set soak.saltSecret=crystal-backup-soak-salt
```

**The chart never generates that Secret**, and never will. A generated salt would be re-generated
on every Argo CD refresh — three minutes by default — and unlike a regenerated certificate, which
rolls the Deployment and gets noticed, a regenerated salt changes nothing visible at all: the
collector keeps running, the reports keep being written, and only the correlation dies.

Setting `saltSecret` without `saltMethod: fromSecret` — or the reverse — is refused when the chart
renders, and `--redaction-salt-file` without `--salt-method=from-secret` is refused again by the
collector at startup rather than silently ignored: the two methods produce archives with different
guarantees, and a running collector looks identical either way.

## During the soak — how to check it in ten seconds

The collector writes **one line a day** to its own log, plus one the moment it starts. That is
the whole health check:

```sh
kubectl -n crystal-backup-system logs deploy/crystal-backup-soak | grep soak-heartbeat | tail -7
```

A week of history, one line per day:

```
INFO soak-heartbeat at=2026-06-04T00:00:00Z day=4 up=100.0% span=3.0d sessions=1 \
  metrics=3 state=3 events=3 logs=3 selfchecks=3 movers=412 \
  movers_by_class=data:96,manifests:48,repo-heavy:12,repo-light:256 \
  footprint=71Mi/512Mi degraded=false drops=0 silent=none
```

What to look at, in order:

- **`silent=`** — the one field that is an alarm. It names any stream that is empty when empty is
  not a healthy answer. `silent=none` is what you want. `silent=metrics` on day 3 means the
  scrape has never worked and the fortnight is being wasted; go and look now, not in eleven days.
  `events=0` and `logs=0` are never named there, because a fortnight with no Warning event and no
  operator error line is a *good* fortnight.
- **`movers_by_class=`** — read this one against what your schedules actually do, because
  `silent=` cannot. A class at zero while backups have been running is not an idle workload, it
  is a blind instrument: that is the exact failure this field was added for, after a run reported
  `movers=87 silent=none` while `data` and `manifests` sat at zero through dozens of backups.
  **`data:0` on day 2 with a nightly schedule firing means go and look now.** The classes are the
  ones in [`docs/MOVER-RESOURCES.md`](../../docs/MOVER-RESOURCES.md); `data` is backup and
  restore, `manifests` is the Kubernetes-object dumps, `repo-heavy` is prune/check/sync and
  `repo-light` is the short repository operations.
- **`up=`** — the fraction of the elapsed span the collector was actually running. Anything below
  ~90% and the archive will be graded THIN.
- **`footprint=`** — used against the cap. If it is climbing towards the cap far faster than
  fourteen days' worth, the sizing was wrong for your cluster and `drops=` will start moving.
- **`degraded=true`** — the volume is nearly full and raw sampling has stopped. The marks and the
  events are still being kept, but act on it.

**And the absence is the strongest signal of all.** No heartbeat line for two days means the
collector is not running, whatever anything else says — no tool-specific knowledge required, and
it shows up in any log pipeline the cluster already has. Nothing else is logged between the daily
lines, so a quiet log is a healthy one and a *stale* log is not.

`soak-export --status` still gives the long form (per-stream detail, gaps, drops with reasons) —
this is the version you can check without thinking about it.

**And one block, once, whenever the collector is asked to stop.** It is the only other thing in that
log:

```sh
kubectl -n crystal-backup-system logs deploy/crystal-backup-soak | grep soak-shutdown
```

Everything the collector has measured is on its PVC and nowhere else, so on `SIGTERM` — a chart
upgrade, a node drain, a reconciler refreshing the Deployment — it writes the figures that cannot be
reconstructed (the per-class peak memory table above all) into its own log on the way out. If the
replacement pod then fails to get the volume, those lines are what you still have. They are not an
archive and they do not replace exporting one: a pod's log dies with the pod, so this only reaches
you through whatever log pipeline the cluster already has. Treat it as the thing that tells you
something happened, and see the next section for the order that keeps it from mattering.

## During the soak — the rule that matters

> **Do not quietly fix anything.**

If a backup fails at 3am and you restart the operator, the incident is gone and so is the finding.
An incident quietly repaired is worse than no soak at all, because it leaves us believing the
fortnight was clean.

So:

- **Something breaks?** Write down what you saw, what time, and what you did. Then do whatever
  your production judgement says — including fixing it. The record is what we need, not your
  forbearance.
- **Tempted to tune a knob?** Note the before value, the after value, and the timestamp. A
  mid-soak change is fine and is often itself the finding ("the default was wrong for us on day
  4"); a mid-soak change nobody wrote down turns a fortnight of metrics into noise.
- **A pod is stuck?** Capture it before you delete it: `kubectl describe`, `kubectl logs`, and
  `kubectl get <kind> -o yaml`. Paste them into your notes file. Deleting a stuck object is often
  right; deleting the evidence of why it was stuck never is.
- **Restarting the operator** loses in-memory state and resets counters. Note it. The collector
  will see the gap; your note is what explains it.

Keep a single plain-text notes file for the fortnight. Two lines a day is plenty. It is the one
stream this kit cannot collect for you, and in every soak that has ever been run it turns out to
be the most valuable one.

## At the end: collect

```sh
./collect.sh --salt-file soak-salt.bin
```

It exports the archive from the collector, reads its manifest, and then does the check nobody
else can do: it takes every namespace, PVC, location and bucket name **on your cluster** and
searches the archive for them verbatim. A hit means the redaction did not cover something, and
you are told before you send it rather than after.

Its verdict is on stderr and its reasoning is inside the archive:

| exit | verdict | means |
|---|---|---|
| 0 | COLLECTED | every stream present and plausible |
| 1 | COLLECTED, THIN | worth sending, but something covers materially less than the soak |
| 2 | INCOMPLETE | a stream is empty where empty is not a healthy answer, or a name leaked |
| 3 | NOT COLLECTED | nothing ran; no archive was written |

**A thin or incomplete archive is still worth sending.** The gap is recorded in the manifest and
travels with the file. What is not worth sending is a small tidy archive that looks complete and
is not, which is why this script goes out of its way to be loud.

## What leaves your cluster, precisely

**Tokenised** — replaced by a stable pseudonym, in every stream, so the same namespace is the same
token in a metric, in an event, in a log line and in day 9's self-check:

namespace, tenant, location, repository, schedule, external-sync, PVC, pod, node, cluster ID,
bucket, endpoint host, prefix.

**Not tokenised, deliberately** — because these carry the meaning and identify nobody:

phases, conditions, reasons, cron expressions, image tags and digests, versions, counts, byte
sizes, durations, timestamps, and the metric label names that are API enums (`origin`, `scope`,
`result`, …).

**Not tokenised, and not tokenisable** — the residual, and the reason `collect.sh` tells you to
read two directories before sending:

free text nobody enumerated. A path inside a volume that appears in an error message. A restic
snapshot ID. A URL inside a library's error string. An object created after the collector last
listed. These live in `logs/` and `events/` and nowhere else in the archive.

**Never present, in any mode:** repository passwords, S3 credentials, KEK or DEK material, CA
bundles, Secret contents. Those are not redacted — they are not read. The collector's RBAC has no
access to Secrets at all.

`--full` disables redaction entirely. It is a decision you may legitimately make; the script says
so in red, the manifest records it, and `collect.sh` reports it as a finding so it cannot happen
by accident.

## Sending it

Attach the `.tar.gz`, your notes file, and the day-zero baseline, to the issue you open — or send
them privately if you prefer. **Not the salt file.** Say in the issue:

- the incumbent tool you ran alongside;
- anything you fixed, tuned or restarted during the fortnight, with dates;
- the result of the restore drill (next section) — this is the part that matters most;
- whether `collect.sh` exited 0, 1 or 2, and what it said.

## And then the restore drill

A soak that ends without a restore has proved that the program does not crash. It has proved
nothing about your data. **[restore-drill.md](restore-drill.md)** is the runbook: restore one
namespace into a scratch namespace that does not exist yet, and compare it against the source,
property by property.

Do not skip it. It is the only part of this fortnight that answers the question the product
exists for.

## Ending a series and starting another

This kit was written for one fortnight, and for a long time that was all it said. If you are
starting a second series — a new release, a different scope, a rerun of a fortnight that went
wrong — you are starting on a volume that still holds the previous one, and the order matters.

**Read all five steps before you run any of them.** Step 2 is the one that has actually cost an
archive: the obvious way to reset a collector is reverted within minutes on most clusters, and it
takes the archive with it.

### 1. Export first, because deleting the PVC destroys the archive

The collector's data lives on `crystal-backup-soak-data`, a PersistentVolumeClaim this chart
renders. Nothing is copied off it: not to the operator, not to your object store, not into the
archive you already sent. When that PVC goes, the series goes with it.

So, in this order:

```sh
# 1a. the cheap insurance, thirty seconds, and it works even when there is no archive to make.
#     --status reads the volume and prints what is on it, including the per-class peak memory
#     figures that are the whole point of §5. SAVE THE OUTPUT — a file, a paste into your notes,
#     a screenshot. Every soak that has ended badly ended with somebody glad they had this.
kubectl -n crystal-backup-system exec deploy/crystal-backup-soak -- \
  /manager soak-export --status 2>&1 | tee soak-series1-status.txt

# 1b. the real archive.
./collect.sh --salt-file soak-salt.bin
```

If `collect.sh` cannot reach the collector at all — it answers `NOT COLLECTED: … exists but has no
Running pod` and exits 3 — then the pod is not running and its volume is not readable through it.
Do **not** proceed to step 2; deleting the PVC at that point is deleting the only copy. `kubectl -n
crystal-backup-system describe pod -l crystalbackup.io/soak=collector` will usually say why, and if
it is a volume that will not attach or will not mount, the figures in `soak-shutdown` in the
previous pod's log (see the section above) are what is left. Send those and say what happened; a
lost archive with an explanation is worth more to us than a silent gap.

### 2. The GitOps trap: `kubectl scale` and `kubectl delete` are undone

If this cluster is reconciled by Argo CD, Flux or anything similar, the collector's Deployment and
PVC are objects that reconciler owns. `kubectl scale deploy/crystal-backup-soak --replicas=0` is
reverted at the next sync — three minutes by default in Argo CD — and so is `kubectl delete pvc`.
What you get is not a stopped collector. It is a few minutes of confusion, then a collector back on
its old volume; and if the timing is unlucky, a PVC deletion racing a pod that is still using it.

That race is not hypothetical. On the morning this section was written, a pod replacement on a
ReadWriteOnce volume produced, in order: `Multi-Attach error for volume … already exclusively
attached to one node`, then a successful attach on the new node, then `rbd: map failed: (22) Invalid
argument` and a pod in `ContainerCreating` for good. The archive was intact and unreachable, on the
volume that was about to be deleted.

**Do it through the values your reconciler reads**, not with `kubectl`:

1. **Set `soak.enabled: false`** in the values file your reconciler syncs from — the Application,
   the HelmRelease, the values in your repository. Commit it and let it sync. Do not use
   `--set`; a value your reconciler does not know about is a value it will overwrite.

2. **Verify that both objects are gone**, and verify it rather than assuming it. This is the step
   people skip and it is the whole point of doing it this way:

   ```sh
   kubectl -n crystal-backup-system get deploy,pvc,pod -l crystalbackup.io/soak=collector
   # want: "No resources found in crystal-backup-system namespace."
   ```

   If the Deployment went and the PVC did not, your reconciler is not pruning removed resources
   (Argo CD: `syncPolicy.automated.prune`; Flux: `prune: true`), and the volume still holds the old
   series. Nothing in this chart holds that PVC back — it carries no
   `helm.sh/resource-policy: keep` and no finalizer of ours — so if something is holding it, it came
   from your cluster's own policy and you will have to decide what to do about it. A PVC stuck
   `Terminating` is usually a pod still mounting it: check the `pod` line in the output above.

3. **Only then set `soak.enabled: true`** again and let it sync. A fresh PVC, a fresh collector, a
   fresh `uptime.json`.

**Installing with Helm directly, no reconciler?** The same three steps, with
`helm upgrade --install … --reuse-values --set soak.enabled=false`, the same verification, then
`--set soak.enabled=true`. Helm deletes resources that a new revision no longer renders, so the PVC
does go — which is exactly why step 1 comes first.

### 3. A new day zero

The baseline is per-series. Take it again, with **the same salt file**, and keep the old one:

```sh
kubectl -n crystal-backup-system cp soak-salt.bin \
  "$(kubectl -n crystal-backup-system get pod -l app.kubernetes.io/name=crystal-backup \
     -o jsonpath='{.items[0].metadata.name}')":/tmp/soak-salt.bin
kubectl -n crystal-backup-system exec deploy/crystal-backup -- \
  /manager selfcheck --redaction-salt-file=/tmp/soak-salt.bin > soak-day0-series2.json
```

Start a new notes file too, and write in its first line what changed between the two series — the
release, the scope, the schedule, whatever you are rerunning for. That sentence is what makes two
archives a comparison instead of two unrelated fortnights.

### 4. Why the two series are still comparable

This is worth knowing rather than discovering: **the two archives share their tokens.**

With the default `saltMethod: auto`, the salt is `SHA256("crystalbackup-soak-salt-v1" || the
operator namespace's UID)`. It is derived, not generated — there is no state anywhere, so the same
namespace produces the same salt today, next month, and on a collector that has been deleted and
recreated in between. The token for `production` in series 1 is the same token in series 2, and the
two can be read as one series with a gap in the middle.

Two things break that, and only two:

- **Recreating the operator namespace.** A new namespace has a new UID, so it has a new salt, and
  the two archives then share no tokens at all. If you do that between series — a full uninstall
  and reinstall, a cluster rebuild — **say so when you send them.** Nothing in the archives reveals
  it; we would read them as two different clusters and compare nothing.
- **Changing salt method between the series.** `auto` and `fromSecret` produce different salts by
  construction. If you are on `fromSecret`, keep the same `soak-salt.bin`, and do not "regenerate it
  to be safe" — that is the failure this whole design is arranged around.

Deleting and recreating the *collector*, its PVC and its Deployment breaks nothing. That is the
point of a derived salt.

### 5. What a fresh collector legitimately looks like — which zeros are expected

Eleven seconds after the collector starts, `soak-export --status` looks alarming and is fine. This
is the screen that gets a working collector restarted, and every restart costs a session, a gap in
`uptime.json`, and another volume handover.

**Expected on a brand-new collector:**

| what you see | why it is correct |
|---|---|
| `up 0.0% of the 0.0 day(s) … across 1 session(s)` | `up` is a fraction of elapsed time, and almost no time has elapsed. It climbs towards 100% over the first day. |
| `metrics 0 day segment(s) (+0 core)` | a metrics segment is written when the first resolution window *closes* — five minutes by default. Nothing is on disk before that. |
| `state 0 day segment(s)` | the CR snapshot runs on its own interval, an hour by default. |
| `events 0 day segment(s)`, `logs 0 day segment(s)` | and these may stay at zero for the whole fortnight. No Warning event and no operator error line is a **good** fortnight; the daily heartbeat never names them as silent. |
| `NOTHING COLLECTED` next to any of the above | it is a statement about the volume, not a diagnosis. On a new collector it is the truth. |
| `highwater NOT MEASURED — no marks file yet` | **the big one.** The high-water table needs a mover pod to exist, which needs a backup to run. On a nightly schedule that is up to a day away. It will say `NOT MEASURED` until then, and that is not a fault. |
| `cache high-water NOT MEASURED` | permanent unless you set `soak.kubeletStats=true`, which grants `nodes/proxy`. Deliberate; nothing else is affected. |
| `on disk` a few tens of KiB | there is nothing on it yet. |

**Not expected, at any age:**

- `!! the collector has never recorded its configuration in /var/lib/crystal-backup-soak` — this is
  not a young collector, it is one that never started. Look at the pod now.
- `metrics 0 day segment(s)` still there after fifteen minutes, or `selfcheck 0 report(s)` after an
  hour. Both of those run on the collector's first round.
- `silent=metrics` on any heartbeat line. The daily line only names a stream once it has had time to
  write something, so if it is named, it is a finding.
- `data:0` or `manifests:0` in `movers_by_class=` once your schedules have actually fired. A class at
  zero while backups are running is a blind instrument, not an idle one.
- `up` below about 90% on any day after the first, or a `GAP` line you cannot explain from your notes.

**And the rule that follows from all of it:** do not restart the collector to make it start
collecting. It is already collecting; the first hour looks empty because it *is* empty. If you
genuinely need to replace the pod, go back to step 1 and export first.

## Files

| | |
|---|---|
| `README.md` | this — the protocol |
| the chart's `soak.enabled` | the resident collector: Deployment, PVC, read-only RBAC, NetworkPolicies. `charts/crystal-backup/templates/soak.yaml`, and there is no second copy of it |
| `manifests/fallback-selfcheck-cronjob.yaml` | the degraded mode, for a build with no `soak-collect` |
| `collect.sh` | export, verify, leak-check, and tell you what to read |
| `restore-drill.md` | the end-of-soak restore drill |
| `fidelity-manifest.sh` | the manifest capture the drill runs on both sides |
| `SPEC.md` | what the collector's Go side must do — for maintainers, not for you |
