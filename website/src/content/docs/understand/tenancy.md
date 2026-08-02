---
title: Tenancy and isolation
description: What the tenant boundary actually is on each plane, what enforces it, and exactly what it does not cover.
sidebar:
  order: 3
---

Multi-tenancy claims are cheap. This page states what is enforced, by what mechanism, and
what is explicitly not covered.

## The namespace plane: isolation by construction

A `BackupLocation` is the tenant's own bucket, their own credentials, their own key. There
is nothing to enforce because there is nothing shared.

The one property worth being precise about: **the platform holds no key slot on that
repository, and there is no way to ask for one.** The field that would have requested it
was specified, half-implemented, and then **removed from the API**.

The reasoning is worth reproducing, because it explains a whole class of decisions in this
project. Removing a restic key slot does **not** rotate the master key. A platform slot
would therefore have been permanent — a grant the tenant could never take back. The
guarantee "platform access ends when the user's key does" is bought by the mechanism not
existing, rather than by a webhook that a flag, or a future maintainer, could switch off.

Consequences, both directions:

- The platform cannot read a tenant's repository, cannot mediate a restore from it, and
  cannot verify it for them.
- **A tenant who loses their password loses their backups.** No support path, because no
  mechanism.

The operator does read the password Secret **by name** to run the tenant's own movers. That
is a visible act in the API audit log, and it stops working the moment the tenant rotates
the key or deletes the Secret.

## The cluster plane: one repository, one key

The shared repository is initialized with a single random 256-bit data key, stored wrapped
under an age X25519 identity — the **cluster KEK** — which the administrator generates and
escrows outside the cluster. Neither the operator nor the chart ever mints it.

State this plainly, because the opposite is often implied: **encryption is not the tenant
boundary here.** One master key; whoever holds it reads every namespace. That key never
leaves `crystal-backup-system`, and holding it is equivalent to the etcd-level access an
administrator already has.

The tenant boundary on this plane is something else.

## The non-forgeable filter

A namespaced `Restore` has **no field that could name another namespace**. No
`locationRef`, no target namespace, no cluster identifier.

When the source `Backup` is cluster-origin, the operator resolves the snapshots itself:

1. It builds a restic filter from the custom resource's own `metadata.namespace` —
   `--tag crystalbackup,namespace=<that>` plus the run tag. Comma-joined tags in a single
   flag are ANDed; repeated flags would be ORed, and that distinction is load-bearing.
2. It lists the repository with that filter.
3. **Only the snapshot IDs that listing returned** are handed to the restore mover.
4. A PVC the filtered listing does not resolve **fails closed**. There is no unfiltered
   fallback, at any point.

So the boundary does not depend on an admission policy holding, on the operator's own
correctness in a branch, or on RBAC being configured right. It depends on the API not
having the field, and on the filter being derived from data the tenant cannot write.

The cost: one extra listing Job, a few seconds, before data moves on a cluster-origin
restore. A namespace-plane restore pays nothing — the tenant's repository holds only their
data, so the snapshot IDs in `status.volumes` are trusted directly.

## Where the work runs

Movers run **only** in `crystal-backup-system`. Never in a tenant namespace, on either
plane.

That is why the generic exposer re-binds a `VolumeSnapshotContent` centrally rather than
mounting the snapshot where it was taken: `VolumeSnapshotContent` is cluster-scoped, so the
operator can bind it into its own namespace, and the tenant's data is never mounted
somewhere a neighbour could reach.

Object-storage credentials and repository keys live in the location's namespace —
`crystal-backup-system` for the cluster plane, the tenant's own for the namespace plane —
and reach movers as per-Job Secrets in the operator namespace. A tenant namespace receives
restored PVCs and nothing else.

The data mover ServiceAccount has **zero RBAC** and no API token at all. The one exception
is the manifest mover, which reaches the API server because reading and writing objects
*is* its operation — and it is bound transiently, per Job, to a reader or a writer role.

## Hooks: the identity problem

Hooks are the sharp edge of tenant self-service, because a hook is a tenant making the
platform run a command.

On the namespace plane the operator does not exec as itself. It **impersonates** a
ServiceAccount the tenant names, in the namespace being backed up, and the API server
authorises each exec against that identity. The invariant is:

> Users can only make the platform run commands they can already run themselves.

Three properties follow: the tenant decides by granting or not granting; revocation is
immediate because the check happens at every exec and nothing is cached; and the
*namespace* is never a field — only the ServiceAccount name is. A settable namespace would
be a cross-tenant hole by construction.

A namespace-plane run declaring hooks with no identity is **gated**, with reason
`HooksNeedServiceAccount` — not silently escalated to the operator's own privileges.

On the cluster plane hooks are admin-authored and may omit the identity, running as the
operator. That is a different trust relationship, stated rather than blurred.

## RBAC

Not one wildcard verb anywhere. Read is `get, list, watch`; full is the eight-verb set.

`crystal-backup-tenant` grants the full set on `backupschedules`, `backuplocations`,
`restores` and `backupexternalsyncs`, and **read-only** on `backups` — because `Backup` is
an operator- and discovery-managed projection, not something a tenant authors. It grants
nothing cluster-scoped.

`crystal-backup-admin` grants the six `cluster*` kinds and read-only `backuprepositories`,
and — note the asymmetry — **nothing** on the namespaced kinds. An administrator who also
needs those must be bound the tenant role as well.

Neither is bound by the chart. The platform binds them.

## Manifests contain Secrets

`includeManifests` defaults to `true`, and the manifest snapshot stores the namespace's
`Secret` objects. On the shared repository, whoever opens it reads them — which is the
admin-only platform key.

The opt-out is `manifestOptions.excludeSecretData: true`: Secrets are stored with `data` and
`stringData` stripped and annotated `crystalbackup.io/secret-data-excluded: "true"`, and
restore recreates them **empty** carrying the same annotation. A workload that needs the
values then fails visibly on a missing key rather than silently starting with the wrong
ones.

It is an opt-out from a deliberate default: a full namespace recovery needs the Secrets.
Excluding them trades recoverability for a smaller blast radius if the key is ever
compromised.

## What is not covered

**Movers hold the location's bucket credentials.** Operator-minted, repository-scoped
credentials are **not planned** — a shared repository is deduplicated across namespaces, so no
storage policy can carve out one namespace's data, and scoping would not have produced tenant
isolation at any effort. A compromised mover can reach everything the location's
credentials can reach — including the escrowed wrapped key object, whose protection is the
KEK and not the object-storage path. Scope credentials at the object-storage side, and give
each location its own.

**Per-tenant crypto-shredding does not exist**, and will not on the shared plane. One key
means destroying it destroys everyone's data. [Erasure](/CrystalBackup/docs/guides/erasure/)
is physical: `restic forget` by tag, then `prune`.

**No fair-share between tenants.** A noisy namespace can **delay** another's backups through
the cluster-wide mover cap. It cannot read them.

**NetworkPolicy enforcement is your CNI's.** The chart ships the objects. Some CNIs accept
them and enforce nothing.

**Admission is a gate, not the boundary.** Controllers re-derive repository identity, the
namespace filter and the confirmation at execution. A bypassed policy degrades the user
experience; it does not breach tenancy. That ordering — structural first, policy second —
is the point.

## See also

- [Admission rules](/CrystalBackup/docs/reference/admission/)
- [Design choices](/CrystalBackup/docs/understand/design-choices/)
- [When not to choose it](/CrystalBackup/docs/discover/when-not-to-use/)
