# ADR 0013 — External backup synchronization to a secondary location

Status: **Accepted** (2026-07-15, product owner + tech lead)

## Context

Beyond the primary repository (a `ClusterBackupLocation` for DR, a user `BackupLocation` for
off-platform), operators and users want a **second copy** of backups in another location
(another bucket, region or provider) as a bonus resilience layer — "sync my backups to a
secondary location distinct from the primary". Two mechanisms exist:

1. **Raw object replication** — copy the restic repository's S3 objects **byte-for-byte**
   (server-side `CopyObject` / bucket replication). Bandwidth-cheap.
2. **`restic copy`** — copy at the **snapshot** level; restic decrypts blobs from the source and
   **re-encrypts them with the destination repository's own key**.

Raw object replication (1) was the first proposal, for its low bandwidth. It has two
disqualifying properties for a multi-tenant tool, raised by the product owner on 2026-07-15:

- **It carries the source key.** A byte clone shares the source's restic master key. Cloning the
  shared **cluster** repo (platform key) into a **client** location would put the **platform key
  into the client's silo** — the client cannot read it with their own key, and the platform key
  now lives in the tenant's bucket. **Client siloing is broken.**
- **It cannot sub-select a namespace.** restic packs mix blobs from many PVCs/namespaces, so raw
  object copy is **whole-repo only** — there is no way to replicate "just this namespace's
  backups" out of the shared cluster repo.

The product owner ruled: **tenant siloing and per-namespace selectivity outrank bandwidth.**

## Decision

**External sync copies at the snapshot level with `restic copy`, re-encrypting to the
destination repository's own key. The destination is an independent repository with its **own
key**, never a byte-clone. Two CRDs express it — one per plane.**

- `restic copy` gives the three properties raw replication lacks: **snapshot selectivity**
  (`--tag namespace=…`), a **distinct destination key** (client key ≠ platform key → siloing
  preserved), and **blob-level incremental dedup** at the destination (re-runs copy only new
  blobs).
- **`ClusterBackupExternalSync`** (cluster-scoped, admin): copies the shared repo — **whole repo
  by default**, optional `selection.namespaces` — from the primary `ClusterBackupLocation` to a
  **secondary `ClusterBackupLocation` with its own platform DEK**.
- **`BackupExternalSync`** (namespaced, user): copies the namespace's backups from a source
  `BackupLocation` to a destination `BackupLocation`, **both resolved within the CR's own
  namespace** (structural confinement, like `Restore`), both under the **user's own key(s)**.
- **`mode`**: `Mirror` (default — the destination is reconciled to the source's current snapshot
  set: copy missing runs, `forget`+`prune` the extras, on the destination's exclusive queue) or
  `AppendOnly` (the destination only grows). `AppendOnly` is **forced when the destination is
  `Immutable`** (Object Lock cannot delete → a WORM secondary). An **Immutable** destination is a
  **rotating set of window-repos** ([adr/0005](0005-immutability-mode.md)), so the sync writes to
  the destination's **current** window-repo and **dedup resets each `rotationPeriod`** — the first
  sync into a new window re-copies the selected data, not a blob delta. Because external sync (M5)
  predates Immutable locations (M8), the Immutable-destination combination is **finalized with M8**.
- **Execution**: always `restic copy` (client-side, blob-incremental against a **Standard**
  destination) — **no raw object clone**. The sync Job runs in `crystal-backup-system` like every
  other mover; it takes a **shared read lock** on the source and writes the destination under a
  **non-exclusive** lock (like a backup) — only `Mirror`'s trailing `forget`+`prune` needs the
  destination's **exclusive** queue. Cron-scheduled; status tracks the last sync,
  snapshots/blobs/bytes copied and lag.

### Not key-blind — and why that is fine

`restic copy` **must** decrypt from the source and re-encrypt to the destination, so the sync Job
**transiently handles both keys** in `crystal-backup-system`. This is the **same trust model
already in force**: the namespace-plane backup mover already uses the user's key *by name* to
write their backups ([03-security-and-tenancy.md §4](../03-security-and-tenancy.md)). The
`platformAccess: false` guarantee is about **no durable / standing** operator key slot; a
**transient** use on an operation the principal **requested** (their own sync) does not change it.
What siloing preserves here is **where the data ends up** — the client's copy under the
**client's** key, holding **only their** snapshots — not a claim that the operator never touches
plaintext (it already does, to back them up).

## Consequences

### Positive
- A real second copy **under the right key**: the destination repo is independently usable with
  upstream `restic` under its **own** key (reversibility, R8), and a client secondary is opaque
  to the platform's cluster key.
- **Per-namespace selectivity** and **blob-incremental** cost; works to **any** S3, including
  cross-provider, because it is client-side.
- Reuses the exclusive-queue, discovery and tag machinery: copied snapshots keep their
  `host`/`paths`/tags, so discovery projects them at the destination like any other.

### Negative / costs
- **Not server-side** → the first sync moves ≈ the selected data volume (then only the blob delta
  **against a Standard destination**; an **Immutable** destination resets dedup per rotation
  window, [adr/0005](0005-immutability-mode.md)). Bandwidth is the accepted price of siloing +
  selectivity.
- The operator **transiently handles keys** (same as backup movers); reviewed as tenancy code
  (DoD two-person review).
- Copied snapshots get **new IDs** at the destination (content-addressed) — expected; tags
  preserve identity for discovery. Dedup is within the destination repo only; a destination that
  **also** receives native backups should be initialized with the **source's chunker parameters**
  (else the two blob sets will not dedup).

## Alternatives considered
- **Raw object clone / S3 server-side replication** — rejected: carries the source key (breaks
  client siloing), whole-repo only (no per-namespace), and would place the platform key in a
  client silo. Bandwidth was its only advantage.
- **A single generic `ExternalSync` CRD across planes** — rejected: cluster (admin, whole shared
  repo) and namespace (user, own repo, structurally confined) have different RBAC, scope and
  default selection; two CRDs mirror the `ClusterBackup`/`Backup` split.

## Revisit triggers
- An admin→admin cluster secondary on the **same** provider where a same-key byte clone is
  acceptable wants pure server-side speed → a `ClusterBackupExternalSync`-only `method: Clone`
  (raw object copy, same key) could be added behind `spec.method`; **deferred**, not v1 (it
  reintroduces the key-carrying property, so it is cluster-only by construction).
- The backlog "namespace-plane backup as a partial repo copy" (cross-plane, cluster repo → user
  bucket, re-keyed — [00-requirements.md §6](../00-requirements.md)) is scheduled: it is the
  **same `restic copy` mechanism** across planes and would reuse this controller.
- A destination provider gains a re-encrypting server-side copy primitive (none exists today).
