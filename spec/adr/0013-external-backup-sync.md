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

## Amendment (2026-07-28, M5 implementation) — rclone as the backend, and a dedicated sync image

Implementing this ADR surfaced a limitation that invalidates one of its stated properties. Recorded
here rather than silently worked around, because the property was load-bearing in the original
decision.

### The limitation

`restic copy` handles **two keys** but only **one backend configuration**. Verified against the
pinned restic 0.19.1:

- The two REPOSITORY PASSWORDS are fully independent — `--password-file` / `RESTIC_PASSWORD_FILE`
  for the destination, `--from-password-file` / `RESTIC_FROM_PASSWORD_FILE` (or
  `--from-password-command` / `RESTIC_FROM_PASSWORD`) for the source. The whole `--from-*` family
  is `--from-repo`, `--from-repository-file`, `--from-password-file`, `--from-password-command`,
  `--from-key-hint`. Re-encryption to the destination's own key therefore works exactly as this
  ADR describes.
- The S3 CREDENTIALS are not. There is no `--from-option`, and `AWS_ACCESS_KEY_ID` /
  `AWS_SECRET_ACCESS_KEY` are consumed by the backend library beneath restic, not by restic, so one
  process has one set. restic's own documentation states it: *"In case the source and destination
  repository use the same backend, the configuration options and environment variables used to
  configure the backend may apply to both repositories – for example it might not be possible to
  specify different accounts for the source and destination repository."*

So the Decision's claim that sync *"works to **any** S3, including cross-provider, because it is
client-side"* was **false as written**. Direct `s3:` addressing only works when both repositories
open with the SAME credentials — the same account, a different bucket — which is the narrow case,
not the secondary-location case R28 exists for.

### The amendment

**Both repositories are addressed through the `rclone:` backend**, which restic's documentation
names as the one way around this: *"You can avoid this limitation by using the rclone backend along
with remotes which are configured in rclone."* Each remote carries its own credentials, so the
source and destination may be different accounts, different providers, or both.

- Remotes are defined **entirely from environment**, no config file: `RCLONE_CONFIG_<REMOTE>_<KEY>`.
  The per-remote form is the load-bearing detail — the sibling `RCLONE_S3_*` form is global to the
  s3 backend and would reinstate exactly the limitation being escaped. `RCLONE_CONFIG_SRC_*` and
  `RCLONE_CONFIG_DST_*` are independent.
- Credentials still arrive in the per-Job projected Secret and are consumed as env, so the handling
  is the one already reviewed under "Not key-blind — and why that is fine". Nothing new is durable.
- **Direction is counter-intuitive and is pinned by a test**: `--repo`/`-r` is the DESTINATION,
  `--from-repo` is the SOURCE. Reading `-r` as "the repository I am working on" gets it backwards,
  and backwards means copying the secondary over the primary.

**A dedicated `sync` image.** rclone is a large Go binary, and this project's release gate has
already blocked once on a transitive dependency of restic (GO-2026-6061). Adding it to the shared
mover image would put that surface in front of every backup and restore. apko already builds two
images; a third is marginal, and it keeps the CVE surface of the sync path off the data path.

Two costs accepted knowingly: the sync image carries rclone's vulnerability surface and must be
kept current at the same cadence as restic; and restic spawns `rclone serve restic` as a CHILD
process, so the shim must propagate its death honestly — a mover that is dead while reported alive
is a failure mode this project has already paid for (M3.2).

### What did not change

The snapshot-level, re-encrypting model, the two CRDs, `Mirror`/`AppendOnly`, tag selectivity, and
the queue/lock behaviour are all unaffected. rclone changes only HOW a repository is addressed, not
what the copy means.

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
