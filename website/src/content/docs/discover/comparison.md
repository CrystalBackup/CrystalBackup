---
title: How it compares
description: Where Crystal Backup differs from Velero and K8up — by mechanism, not by adjective.
---

Velero and K8up are mature, widely deployed and good at what they set out to do. This
page is not a benchmark and not a scorecard. It describes **four mechanisms** that differ,
and what each one buys and costs, so you can tell whether the difference matters for you.

Capabilities move. Verify anything here against each project's own documentation before
you decide.

## 1. Two planes rather than one audience

**Velero** is administrator-oriented. `Backup` and `Restore` are cluster-scoped: a
namespace owner cannot create one for their own namespace without being granted an
ability that also covers everyone else's.

**K8up** is namespace-oriented. `Schedule` and `Restore` are namespaced, so a tenant is
genuinely self-service — but there is no cluster-scoped counterpart that gives a platform
team one repository, one retention policy and one DR posture over the fleet.

**Crystal Backup** ships both, over the same execution engine. `ClusterBackupSchedule`
fans out into namespaces for platform DR; `BackupSchedule` is the tenant's own. The same
`Backup` object is the unit of execution whichever plane created it, so there is one
code path to trust rather than two.

*The cost:* two planes is more API surface, and a platform team has to decide which
namespaces are covered by which.

## 2. A shared repository with tag-carried tenancy

**K8up** gives each namespace its own repository. Isolation is trivially perfect, and
deduplication stops at every namespace boundary — fifty namespaces running the same base
image store it fifty times.

**Velero** with its file-system backup writes into one repository per namespace as well.

**Crystal Backup**, on the cluster plane, writes every namespace into **one** repository,
with restic tags `tenant=`, `namespace=`, `pvc=` carrying the tenancy. Deduplication is
cluster-wide.

*The cost, and it is real:* one repository means one exclusive `prune` window for the
whole cluster, and prune memory scales with total cluster data rather than per namespace.
Schedule it off-peak and bound it with `pruneMaxRepackSize`. It also means the repository
key is a cluster-wide secret — see the next section for what is done about that.

## 3. The tenant confinement is structural, not a policy check

This is the mechanism most worth understanding, because it is where "multi-tenant" usually
becomes a promise rather than a property.

A namespaced `Restore` in Crystal Backup **has no field that could name another
namespace**. No `locationRef`. No target namespace. No cluster identifier. When the source
is a cluster-DR backup, the operator lists the repository itself with a restic filter
built from the custom resource's own `metadata.namespace`, and hands the mover only the
snapshot IDs that listing returned. A PVC that the filtered listing does not resolve
**fails closed**; there is no unfiltered fallback.

So the tenant boundary does not depend on an admission policy holding, or on the operator
being up, or on RBAC being configured correctly. It depends on the API not having the
field.

What this does **not** claim: encryption is not the tenant boundary on the shared
repository. There is one master key, and whoever holds it reads every namespace. That key
is admin-only and never leaves `crystal-backup-system` — equivalent to the access an admin
already has through etcd. If you need cryptographic separation between tenants, use the
namespace plane, where each tenant's repository has a different key that the platform does
not hold.

## 4. Disaster recovery that starts from the bucket

**Velero** can restore from a bucket into a fresh cluster, and does it well; its backup
metadata lives in the object store alongside the data.

**K8up** and **VolSync** are volume-replication tools; reconstructing a namespace from
their repositories is a manual exercise.

**Crystal Backup** treats the repository as the source of truth and the Kubernetes objects
as a projection of it. Concretely:

- Point a fresh operator at an existing bucket and a discovery pass inventories it and
  projects `Backup` objects into whatever namespaces exist. No pre-existing custom
  resources are needed.
- `ClusterRestore` addresses a **repository coordinate** — location, origin namespace,
  run — so it works when the namespace, the schedule and every `Backup` object are gone.
- PVC capacity, storage class and access modes are recovered from restic tags recorded at
  backup time (`pvcsize`, `pvcclass`, `pvcmodes`), so PVCs come back correctly sized with
  nothing surviving to describe them.
- The wrapped platform key is escrowed **in the bucket** at
  `<prefix>/<clusterID>.crystal-meta/wrapped-dek.age` — ciphertext under your own KEK,
  useless to anyone holding only the bucket.

The recovery input is therefore: the bucket, plus the age KEK you escrowed outside the
cluster. Nothing else.

*What is out of scope:* etcd and the control plane. Crystal Backup recovers application
resources and data, not the cluster itself.

## Reversibility

All of Velero's restic/kopia mode, K8up, VolSync and Crystal Backup write formats you can
open with an upstream tool. Crystal Backup's difference is the absence of a wrapper: the
repository is a plain restic repository with no sidecar catalogue, and the layout is
documented. Given the bucket credentials and the key,
`restic -r s3://bucket/prefix/<clusterID> snapshots` lists your backups with no Crystal
Backup component involved.

That is also a deliberate constraint on the project: anything that would require its own
index alongside the repository does not get built.

## Coexistence

Crystal Backup is designed to run **beside** an incumbent tool indefinitely. There is no
migration phase and no parity checklist. Concretely: a distinct API group
(`crystalbackup.io`), a distinct namespace (`crystal-backup-system`), distinct credentials
and repositories, its own `VolumeSnapshotClass` selection which it never mutates, and
every `VolumeSnapshot` it creates carrying a `crystal-` name prefix and its own label so
garbage collection only ever touches its own objects.

*The cost:* running two tools means two snapshot pipelines and roughly double the upload
traffic during any overlap.

## The honest summary

If you need admin-driven cluster-wide DR and nothing else, Velero is more mature and you
should use it. If you need per-namespace restic backups and your namespaces do not share
much data, K8up is simpler and you should use it.

Crystal Backup is for the case where you need *both audiences at once* — a platform DR
posture and genuine tenant self-service — and where you want the tenant boundary to be a
property of the API rather than a configuration you have to keep right.

See also [When not to choose it](/CrystalBackup/docs/discover/when-not-to-use/).
