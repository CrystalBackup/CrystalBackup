---
title: What Crystal Backup is
description: A Kubernetes operator for multi-tenant, self-service backup and restore of namespaces — PVC data and manifests — into plain restic repositories.
---

Crystal Backup is a Kubernetes operator that backs up and restores **namespaces** —
both the **PVC data** and the **Kubernetes manifests** — across two planes:

- a **cluster plane**, where a platform administrator protects all (or selected)
  namespaces into one shared repository, for platform disaster recovery;
- a **namespace plane**, where the person who owns a namespace backs it up **again**,
  to **their own bucket under their own key**, off the platform's trust.

Everything is written in the plain [restic](https://restic.net) repository format. Given
the object-storage credentials and the key, `restic` itself reads your backups. There is
no proprietary catalogue, and nothing has to survive for the data to stay readable.

## The problem it addresses

Managed, multi-tenant Kubernetes platforms are usually isolated by namespace: a team
owns one or more namespaces and is self-service inside them through RBAC. Those
platforms commonly run one cluster-wide backup tool as an admin-only safety net.

That arrangement leaves the teams who actually own the workloads with:

- **no self-service** — they cannot take a backup before a risky migration, and they
  cannot restore without filing a ticket;
- **no visibility** — they cannot tell whether their data is protected, or what a
  restore would actually bring back;
- **no way off the platform** — every copy of their data sits inside the same trust
  boundary as the cluster they are trying to be protected from.

Crystal Backup targets that combination. It is explicitly **not** a replacement for the
cluster-wide tool you already run; it is designed to sit next to it.

## The four properties that carry the design

Everything else follows from these, and each one is a property you can check rather than
an adjective.

### One shared repository, tenancy carried by tags

The cluster plane writes every namespace into **one** restic repository per location.
Tenancy inside it is carried by restic tags — `tenant=`, `namespace=`, `pvc=` — not by
one repository per namespace. That is what lets deduplication work across the whole
cluster instead of resetting at every namespace boundary.

### A namespace filter a tenant cannot forge

A namespaced `Restore` names a `Backup` **in its own namespace**. It has no
`locationRef`, no target-namespace field and no cluster identifier — there is no API
field through which another namespace could be named. When the source is a cluster-DR
backup, the operator resolves the snapshots itself, with a restic filter built from the
custom resource's own `metadata.namespace`, and only the snapshot IDs that filter
returns are ever handed to a mover.

The confinement is structural: it holds because the way to express the other case does
not exist, not because a check rejects it.

### Discovery from the repository, which outlives the cluster

`Backup` objects are a **projection** of the restic repository, not the source of truth.
Point a fresh operator at an existing bucket and it inventories what is there and
projects `Backup` objects for it — with no pre-existing custom resources, and no
surviving cluster required. `kubectl get backups -n <ns>` therefore lists exactly what
is restorable in that namespace: delete a `Backup` and discovery recreates it; let a
snapshot expire and the projection disappears.

### The platform holds no key on a tenant's repository

A namespace-plane repository has exactly **one** key slot: the user's. There is no API
field that could request a platform slot, because the field was removed rather than
guarded. Removing a restic key slot does not rotate the master key, so a platform slot
would have been permanent — and a guarantee bought by a mechanism not existing is worth
more than one bought by a webhook someone can switch off.

The direct consequence, stated plainly: **a user who loses their `BackupLocation`
password loses their backups.** There is no platform copy.

## Where to go next

- [The two planes](/CrystalBackup/docs/discover/two-planes/) — who owns what, and which
  custom resource belongs to whom.
- [How it compares](/CrystalBackup/docs/discover/comparison/) — against Velero and K8up,
  on the axes where they actually differ.
- [When not to choose it](/CrystalBackup/docs/discover/when-not-to-use/) — worth reading
  before the quickstart.
- [Project status](/CrystalBackup/docs/discover/status/) — what has shipped, what has
  not, and how far to trust it today.
