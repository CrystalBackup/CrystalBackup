---
title: Design choices
description: The decisions that shape what Crystal Backup can and cannot do, and what each one cost.
sidebar:
  order: 4
---

Every one of these is a trade. The reasoning is public in full — eighteen
[architecture decision records](https://github.com/CrystalBackup/CrystalBackup/tree/main/spec/adr)
lead the code. This page is the short version of the ones you will feel.

## restic as the repository format

**Chosen because** it is a plain, documented, widely deployed format with content-defined
deduplication, compression and AES-256 encryption, that anyone can read with an upstream
binary. Reversibility stops being a promise and becomes a property.

**Cost.** The project inherits restic's model, including its locking. Every exclusive
operation takes a repository lock, and the shape of `prune` — one exclusive window, memory
proportional to repository size — is restic's shape, not a choice. It also constrains what
can be built: anything requiring a private index beside the repository is off the table,
because that index would be the lock-in.

## One shared repository per cluster location

**Chosen because** deduplication across the whole cluster is worth a great deal — fifty
namespaces running the same base image store it once — and because per-namespace
repositories multiply the maintenance surface by the namespace count.

**Cost, and it is the biggest one in the design.** One repository means one key, so
encryption cannot be the tenant boundary and per-tenant crypto-shredding is impossible. It
means one cluster-wide exclusive prune window whose memory scales with total data. And it
means many movers contending on one repository's locks, bounded by a concurrency cap.

Per-tenant sharding is **deferred, not rejected** — the design keeps it addable behind a
shard key without changing the API surface.

## Tag-carried tenancy, filtered server-side

**Chosen because** it is what makes the shared repository safe to expose to tenants at all.
The filter is derived from the custom resource's own namespace, and the API has no field
through which another namespace could be named.

**Cost.** One extra listing Job — a few seconds — before a cluster-origin restore moves
data. And the discipline that everything on the tenant surface must be re-derivable
server-side, which is why several convenience fields do not exist.

## The repository is the source of truth

**Chosen because** disaster recovery that requires a surviving cluster is not disaster
recovery. Point the operator at a bucket, and it inventories what is restorable.

**Cost.** `Backup.spec` may only hold what a projection can reconstruct from restic alone.
Run configuration is materialized at creation rather than kept live, and discovery must
never claim a field it cannot reproduce — otherwise the two writers fight over the object
forever under server-side apply. See [The cascade](/CrystalBackup/docs/understand/cascade/).

## Admin-held root key, escrowed off-cluster

**Chosen because** a key generated inside the cluster is lost with the cluster, and every
backup with it. Neither the operator nor the chart ever mints one.

**Cost.** An operational burden that cannot be automated away: you must generate the KEK,
escrow it outside, and keep the escrow current. Lose it and the bucket is ciphertext.

The counterpart is the wrapped-key escrow **in the bucket**, so the recovery input is two
things — the bucket and your KEK — rather than three.

## No operator key slot on a tenant's repository

**Chosen because** removing a restic key slot does not rotate the master key. A platform
slot would have been permanent and un-revocable: a one-way door.

The field existed, was half-implemented — it reported a platform slot in status while never
adding one — and was **removed** rather than fixed.

**Cost.** The platform cannot help a tenant who loses their key, cannot mediate a restore
from a tenant repository, and cannot verify one for them. Some support requests have no
answer, and that is the intended outcome.

## Movers as Jobs, never in-process

**Chosen because** an operator that moves bytes in its own process cannot be restarted
safely, cannot be scheduled across nodes, and cannot be resource-bounded per unit of work.

**Cost.** Job orchestration discipline everywhere: deterministic names so a restart
re-adopts rather than duplicating; polling *through* a transient `NotFound` because cache
lag is not absence; explicit deletion propagation; and TTL backstops. Every one of those
rules exists because its absence produced a real leak.

## Movers only in the operator namespace

**Chosen because** it keeps repository keys and object-storage credentials out of any
namespace a tenant controls, on both planes.

**Cost.** A restore has to bridge into a tenant namespace somehow, and the bridge is the
cluster-scoped `PersistentVolume` — the transplant and twin mechanisms, which are the most
intricate machinery in the project. Simpler would have been a mover in the tenant's
namespace, and it would have put credentials there.

## Two exposure paths, storage-aware

**Chosen because** the cheapest read differs per CSI: CephFS can serve a shallow read-only
snapshot with zero copy, RBD wants a copy-on-write clone, and everything else takes the
generic re-bind.

**Cost.** More code paths, and a per-driver correctness surface. The honest compensation is
that a driver which cannot snapshot is **skipped with a reason in status** rather than
silently dropped — a visible gap being better than an invisible one.

## Hooks bound the freeze, not the upload

**Chosen because** an application held quiesced for a multi-hour upload is an outage. Post
hooks run as soon as the snapshots are *cut*, not when they succeed, and the release is
unconditional and retried.

**Cost.** The backup is only as consistent as the snapshot instant, which is the correct
guarantee but is weaker than some people expect from the word "hook".

## Impersonation for hook identity

**Chosen because** it makes "users can only make the platform run commands they can already
run themselves" something the API server enforces rather than something a document asserts.

**Cost.** Every tenant wanting hooks must create a ServiceAccount and a RoleBinding first.
That is friction, and it is the mechanism.

## Admission as VAP, not a webhook

**Chosen because** a `ValidatingAdmissionPolicy` runs inside the API server and therefore
holds **when the operator is down**. A webhook that fails open does not.

**Cost.** Kubernetes 1.30 as a hard floor, and CEL's limits — it cannot ask whether a target
exists, which is why the confirmation rule is a conservative superset that asks
unconditionally. One genuinely dynamic check (single default location) remains a webhook,
fail-open, with a controller-side condition behind it.

## Immutability as a location mode, not a policy

**Chosen because** Object Lock is a property fixed when a repository is created, not a
setting to toggle. `Standard` prunes; `Immutable` cannot, and expires by rotating
repositories instead.

**Cost, stated plainly:** it is **not implemented**. The field is accepted and a few guards
exist; Object Lock, rotation and expiry are not. Worth knowing why it is hard: restic writes
a lock file at the start of *every* operation and deletes it at the end. Under compliance
Object Lock those deletions fail, stale locks accumulate, nobody can purge them, and the
repository eventually wedges permanently. A naive "restic plus an object-locked bucket"
deployment is not degraded — it is broken. That is the problem still being solved.

## Coexistence, not replacement

**Chosen because** nobody rips out a working backup tool to try a new one, and a project
that demands it will not be tried.

**Cost.** Permanent surface area — deny-lists, distinct prefixes, snapshot-count monitoring
— on every cluster, including those running no other tool. And during any overlap, two
snapshot pipelines and roughly double the upload traffic.

## apko and Wolfi for images

**Chosen because** a near-zero known-CVE base, an SBOM and build provenance are cheaper to
maintain from the start than to retrofit.

**Cost.** A build toolchain most contributors have not seen before, and dependency
overrides that must be kept in lockstep across three images when an upstream advisory
lands.

## What was tried and dropped

Worth recording, because a project's rejected ideas say as much as its accepted ones.

- **Per-tenant crypto-shredding** — impossible in a single-key shared repository. Replaced
  by physical erasure.
- **A platform key slot on tenant repositories** — un-revocable by construction. Removed.
- **A preferred backup window** (`start`–`end`) — the shape proved ambiguous across
  midnight. Off-peak steering is done through the cron expression instead.
- **Raw object-storage replication for external sync** — it carries the source's master key
  to the destination, which on the namespace plane would put the platform key inside a
  tenant's silo, and it is whole-repository only. Replaced by `restic copy`, which
  re-encrypts.
- **A `ClusterDecommission` custom resource** — a CRD is a desired state a controller
  converges to, so it would re-fire after an etcd restore or a stray re-apply. Destroying a
  key is a runbook. `ClusterErasure` is a CRD precisely because it is *bounded*.
