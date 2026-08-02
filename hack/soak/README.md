# The soak — running 0.6.1 for two weeks and telling us how it went

0.6.1 is offered for testing rather than production for one reason: nobody has yet run it
alongside an incumbent backup tool, on real data, for two weeks. Every other M6 exit criterion is
met. This one is not, and it cannot be met by a test suite — a suite runs what someone thought
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

Three things go in:

| | what | footprint |
|---|---|---|
| CrystalBackup 0.6.1 | the operator itself, per the install docs | one pod |
| `manifests/collector.yaml` | the soak collector: one pod, one 1Gi PVC, read-only cluster RBAC | one pod, 1Gi |

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
#    prints usage  -> apply manifests/collector.yaml
#    "unknown"     -> apply manifests/fallback-selfcheck-cronjob.yaml instead, and read the
#                     header of that file: you get the daily self-checks and nothing else.

# 3. install the collector (edit the image to the digest your operator runs, first)
kubectl apply -f manifests/collector.yaml

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
To hold the salt yourself instead, create the Secret and switch the collector to
`--salt-method=from-secret` (both blocks are in `manifests/collector.yaml`, commented, ready to
uncomment):

```sh
openssl rand -out soak-salt.bin 32
kubectl -n crystal-backup-system create secret generic crystal-backup-soak-salt \
  --from-file=salt=soak-salt.bin
```

Passing `--redaction-salt-file` without `--salt-method=from-secret` is refused at startup rather
than silently ignored: the two produce archives with different guarantees, and a running
collector looks identical either way.

## During the soak — how to check it in ten seconds

The collector writes **one line a day** to its own log, plus one the moment it starts. That is
the whole health check:

```sh
kubectl -n crystal-backup-system logs deploy/crystal-backup-soak | grep soak-heartbeat | tail -7
```

A week of history, one line per day:

```
INFO soak-heartbeat at=2026-06-04T00:00:00Z day=4 up=100.0% span=3.0d sessions=1 \
  metrics=3 state=3 events=3 logs=3 selfchecks=3 movers=412 footprint=71Mi/512Mi \
  degraded=false drops=0 silent=none
```

What to look at, in order:

- **`silent=`** — the one field that is an alarm. It names any stream that is empty when empty is
  not a healthy answer. `silent=none` is what you want. `silent=metrics` on day 3 means the
  scrape has never worked and the fortnight is being wasted; go and look now, not in eleven days.
  `events=0` and `logs=0` are never named there, because a fortnight with no Warning event and no
  operator error line is a *good* fortnight.
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

## Files

| | |
|---|---|
| `README.md` | this — the protocol |
| `manifests/collector.yaml` | the resident collector: Deployment, PVC, read-only RBAC, NetworkPolicies |
| `manifests/fallback-selfcheck-cronjob.yaml` | the degraded mode, for a build with no `soak-collect` |
| `collect.sh` | export, verify, leak-check, and tell you what to read |
| `restore-drill.md` | the end-of-soak restore drill |
| `fidelity-manifest.sh` | the manifest capture the drill runs on both sides |
| `SPEC.md` | what the collector's Go side must do — for maintainers, not for you |
