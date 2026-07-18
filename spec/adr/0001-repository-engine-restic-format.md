# ADR 0001 — Repository engine: restic format, upstream restic CLI in movers

Status: **Accepted** (2026-07-11)

## Context

Every byte Crystal Backup stores passes through a repository engine that must provide
compression, content-defined deduplication and client-side encryption (R13), partial
restore (R7), offline export without Kubernetes (R8), full POSIX metadata fidelity —
uid/gid/mode, xattrs, ACLs, hardlinks, sparse files (R10) — and a key model compatible
with the two-plane key tiers (multi-key) and tag-based right-to-erasure (R3, R21). The
engine choice is a **format-level contract**: the mover Job, the `crystalctl` CLI and the
future UI backend (R9) all read and write the same repositories, and the user-facing
reversibility promise (00-requirements §5) states that a tenant can always read their
backups with standard upstream tooling.

The engine is deliberately decoupled from the operator: R11/R12 force the data path into
per-PVC mover Jobs spread across nodes, so the engine never runs inside the operator
process (see [01-architecture.md](../01-architecture.md) §1 and
[adr/0002](0002-operator-language-go.md)). The operator orchestrates; movers move bytes.

Candidates surveyed in the 2026-07-11 repository-engine research pass:
restic, kopia, rustic/rustic_core, plakar/kloset, borg, bupstash. Two orthogonal
questions were answered:

1. **Which repository FORMAT?** — determines crypto, dedup, metadata fidelity,
   reversibility, and the tenant/platform key model.
2. **Which implementation executes in the mover?** — a packaging question, revisitable
   at any time if the format has more than one implementation.

## Decision

1. **Standardize on the restic repository format, version 2** (zstd compression,
   content-defined chunking via Rabin fingerprints with a per-repository random
   polynomial, AES-256-CTR + Poly1305-AES authenticated encryption). The format is
   publicly specified and stable for ~10 years
   ([restic design document](https://restic.readthedocs.io/en/stable/design.html)) and
   has **two independent, actively maintained implementations**: restic (Go,
   BSD-2-Clause) and rustic (Rust, Apache-2.0/MIT). Every repository created by
   Crystal Backup is a plain restic repository readable by upstream `restic`.

2. **v1 movers invoke the upstream restic CLI** — a pinned version, vendored as a static
   binary in the mover image, wrapped by a thin Go shim that builds arguments, parses
   `--json` output and emits the structured termination message consumed by the
   operator. Not a Go library: restic has none **by design** (all code under
   `internal/`; [restic#4406](https://github.com/restic/restic/issues/4406) requesting
   a public API is open with no maintainer commitment). Not embedded `rustic_core`
   either — see Alternatives. The CLI-in-Jobs pattern is proven: K8up has run restic
   CLI in Kubernetes Jobs in production for ~7 years (since 2019).

3. **The engine choice inside the mover is an implementation detail behind the format
   contract.** Because rustic reads and writes the same format, the mover engine can be
   swapped (globally or per-operation, e.g. prune only) without any repository
   migration. This ADR fixes the format permanently and the mover engine for v1 only.

The decision powers the requirements as follows:

| Requirement | How the restic format serves it |
|---|---|
| R3 / R5 | **restic multi-key**: one repository accepts several passwords, each wrapping (scrypt) the same master key (`restic key add/remove`). The shared cluster repo carries a **single platform slot**; a namespace-plane repo carries the **user's own key** plus, when `platformAccess` is set, an **optional operator slot** for mediated restore/verification — no duplicated data, no cross-repo key hierarchy ([adr/0004](0004-encryption-key-management.md)). |
| R7 | `restic restore --include`, `restic dump <snap> <path>` streams a single file to stdout reading only the needed blobs. |
| R8 | Free by construction: `restic -r s3:… dump latest --archive tar` produces a tar with only S3 credentials + key. `crystalctl repo export --tar` is a thin wrapper; the upstream equivalent is documented as the reversibility guarantee. |
| R10 | Complete: xattrs (including ACLs as `system.posix_acl_*` xattrs) are stored unconditionally at backup; restores pass **no** `--include-xattr`/`--exclude-xattr` filter so all xattrs travel, hardlinks preserved, sparse files via `restore --sparse`. Verified by the metadata-fidelity e2e suite (M1). |
| R13 | Repo v2: zstd + CDC dedup + AES-256/Poly1305, all client-side before S3. |
| R17 | `restic check --read-data-subset` as the integrity-check primitive. |
| R21 | `restic forget --tag` (`tenant=` / `namespace=` / `namespace=+pvc=`) then `prune` → **physical** right-to-erasure from the shared repo; a single-master-key repo cannot carry per-tenant keys, so crypto-shredding is dropped ([adr/0009](0009-shared-cluster-repo-tag-tenancy.md), [adr/0004](0004-encryption-key-management.md)). |
| R24 | `restic forget --keep-last/--keep-hourly/…/--keep-within` maps 1:1 to `[Cluster]BackupLocation.spec.retention` (retention lives on the location — one shared repo, one policy — not the schedule; [adr/0009](0009-shared-cluster-repo-tag-tenancy.md)). |

## Consequences

### Positive

- **Sovereignty and reversibility**: a namespace user (or the platform operator, even
  after a hypothetical disappearance of the project) restores with upstream `restic` —
  two independent implementations of a public format are the strongest reversibility
  argument available in this space, a strong sovereignty guarantee.
- **Multi-key enables the two-plane key model**: several restic passwords over one master
  key let the operator add a slot without duplicating data — a **single platform slot** on
  the shared cluster repo, and the **user's key plus an optional operator slot**
  (`platformAccess`) on a namespace repo. With the envelope of
  [adr/0004](0004-encryption-key-management.md) (the platform DEK is a restic password
  wrapped by an age KEK, O(1) re-wrap) this delivers R3/R5 with **zero custom
  cryptography** and without breaking R8.
- **Nothing custom in the data format**: dedup, compression and crypto are battle-tested
  (restic ~30k stars, massive deployment, 0.19.x current); we write no storage-format
  code, only orchestration.
- **Engine swap-ability**: rustic is a drop-in replacement inside the mover image if
  benchmarks or the immutable mode ever justify it (see Revisit triggers).

### Negative

- **Prune, forget and check require a per-repository exclusive lock** (restic locking
  model). The operator serializes `forget`/`prune`/`check` against backups per
  `BackupRepository` via the maintenance controller's per-repo exclusive-operation
  queue (maintenance windows, jitter —
  [01-architecture.md](../01-architecture.md) §7–8). Because the cluster plane is **one
  shared repo**, its `prune` is a **single cluster-wide exclusive window** (scheduled
  off-peak, `--max-repack-size`; [adr/0009](0009-shared-cluster-repo-tag-tenancy.md));
  namespace-plane repos are independent. restic 0.18+ resumable prune bounds the cost.
- **Lock files are objects in the bucket**: on an object-locked bucket, restic can
  neither prune nor delete its own lock files — confirmed blocker for `Immutable` mode,
  which is why immutable locations use repository rotation and never prune
  ([adr/0005](0005-immutability-mode.md)).
- **No library API**: the `crystalctl` CLI and the UI backend must shell out to restic
  (`ls --json`, `dump`) or later embed `rustic_core`; one subprocess per request and
  repeated index loads are a known cost for interactive browse (R9 — accepted, UI is
  the lowest priority).
- **RAM footprint of movers**: restic keeps its index in memory (~3–4× theoretical index
  size). Repos on millions of files need generous mover memory limits; sizing guidance
  and a bench are an M6 deliverable. Recent releases (0.18.0 prune memory, 0.19.0 index
  loading) substantially improved this, but published Velero-era benchmarks predate
  those versions and must not be reused.

### Risks & mitigations

| Risk | Mitigation |
|---|---|
| Leaked repository key: restic cannot securely revoke a key without re-encrypting — `key remove` deletes the wrapped copy but the master key never rotates (documented restic limitation). | Re-encryption procedure = repo copy into a fresh repository with a new DEK; runbook is an M5 deliverable. Threat model documented in [adr/0004](0004-encryption-key-management.md). |
| Stale locks after mover crashes block subsequent operations. A hard-killed (OOMKilled/SIGKILL) mover's lock is NOT stale to restic — created on a since-gone pod (a different host, so restic cannot probe the PID) and under the 30-min age window — so a bare `unlock` leaves it and it then blocks the next `forget`/`prune`. | On detecting a hard-killed mover (blank termination message) the Backup controller enqueues an `unlock --remove-all` on the per-repo exclusive queue. Force-removal is made safe by a per-repo **backup⇄unlock mutex**: the mover admission holds new backups back while an unlock is pending/in-flight (`queue.QuiescenceRequired`), and the unlock drains the in-flight movers before it runs — so no live backup lock exists when it force-removes. `prune`/`erase` join as writers in M2/M4. An age-based `restic unlock` sweep stays a future backstop for locks orphaned while the operator itself was down. |
| restic CLI flag/JSON output changes across versions. | Version pinned and vendored in the mover image; shim has contract tests against the pinned binary; upgrades are deliberate, tested PRs. |
| Mover OOM on pathological file counts. | Configurable mover resources per size class; M6 load test defines defaults. The shared cluster repo's index scales with total cluster data (not per-namespace); sharding, which would re-bound it per tenant, is deferred ([adr/0009](0009-shared-cluster-repo-tag-tenancy.md)). |
| restic bus factor (~1–2 core maintainers). | The format outlives the implementation: public spec + rustic as second implementation; worst case we maintain a fork of a stable CLI, not a storage format. |

## Alternatives considered

- **kopia** (Go, own format, Apache-2.0) — the strongest challenger: best-in-class
  public Go API (`repo`, `snapshot/snapshotfs`; embedded by Velero and Kasten K10),
  lock-free maintenance (no exclusive prune lock), modern AEAD crypto, and a native
  browse UI. **Rejected** on three structural grounds: (1) R10 failure —
  [kopia#544](https://github.com/kopia/kopia/issues/544), open since August 2020,
  documents no xattrs, no ACLs, no hardlinks, and setuid/setgid/sticky bits lost on
  restore; a six-year-open metadata issue on an explicit requirement is disqualifying.
  (2) Single password per repository — no way to hold a user key and an operator slot
  on the same repo, which guts the R3/R5 platform-plus-user slot design
  (`repository change-password` only rotates the envelope, and mid-failure can lock the
  repo, kopia#3049). (3) Mono-implementation format — a weaker reversibility guarantee.
- **rustic / rustic_core** (Rust, restic format) — attractive: lock-free two-phase
  prune concurrent with backups, more compact in-memory index, native Prometheus
  metrics, `dump`/single-file restore, direct master-key support (KMS-friendly).
  **Rejected as the v1 primary engine**: the project self-declares "early development
  stage, API subject to change" (rustic_core pre-1.0) and the CLI "beta … not
  recommended for production backups, yet"; team of ~2 (bus factor); hardlink
  preservation on restore only landed in 0.11.2 (April 2026), a maturity signal.
  **Kept as the designated second implementation** — format-identical, so it can be
  adopted per-operation (e.g. lock-free prune, immutable-mode writes) or wholesale
  without migration, once it passes our metadata-fidelity suite.
- **plakar / kloset** (Go, own format, ISC, Plakar Korp — French) — embeddable
  library, audited crypto, integrated browse UI, sympathetic sovereignty story.
  **Rejected**: format is young with no third-party implementation, the company is a
  pre-seed startup, and the open-core boundary (IAM/KMS/multi-store in the proprietary
  Enterprise edition) sits exactly where our multi-tenant needs live. Re-evaluate in
  ~18 months as a market watch item, not a foundation.
- **borg / borgbackup** (Python) — no stable S3 backend (borg 2 with `s3:` still beta,
  "do not use in production"); Python is not embeddable in a Go toolchain;
  mono-implementation. **Eliminated.**
- **bupstash** (Rust) — dormant (last release November 2022), self-qualified beta, no
  S3 backend. **Eliminated.**
- **Custom outer encryption over any engine** — rejected in the requirements phase:
  wrapping repositories in our own crypto layer breaks R8 (upstream tooling could no
  longer read the data) and destroys cross-snapshot deduplication. R21 is met by
  tag-scoped `forget`+`prune` (physical erasure) and R3/R13 by native restic crypto plus
  envelope key management instead ([adr/0004](0004-encryption-key-management.md)).

## Revisit triggers

- **rustic reaches 1.0** (or earlier: passes our metadata-fidelity e2e suite —
  xattrs/ACLs/hardlinks/sparse — on representative data): evaluate rustic for
  (a) lock-free prune replacing the serialized prune queue, (b) `Immutable`-mode
  writes at M8 ([adr/0005](0005-immutability-mode.md)), (c) `rustic_core` in the UI
  backend for R9.
- **restic upstream ships a public library API** (restic#4406): reconsider embedding
  for the `crystalctl` CLI and UI backend, removing the subprocess cost.
- **M6 benchmark results** (restic vs rustic on our S3, millions of files, TB scale,
  post-0.19 restic): revisit the mover engine and default resource sizing.
- **kopia closes #544 with full metadata support**: does not reopen this decision by
  itself (single-key and mono-implementation objections stand), but note it in the
  next architecture review.
