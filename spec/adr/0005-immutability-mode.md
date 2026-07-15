# ADR 0005 — Immutability as a location mode, retention by repository rotation

Status: **Accepted** (2026-07-11; retention/erasure model aligned to the shared cluster
repository 2026-07-12, [adr/0009](0009-shared-cluster-repo-tag-tenancy.md))

## Context

R18 requires ransomware-grade protection for backups: once written, backup data must not
be deletable or modifiable — not by the tenant, not by a compromised mover credential,
and (in the strictest mode) not even by a platform admin. The natural storage primitive
is S3 Object Lock (WORM), supported by AWS S3, Ceph RGW (our production target) and other
S3 implementations.

Object Lock is however fundamentally incompatible with the restic-format lifecycle we
use everywhere else (ADR 0001):

1. **Prune/forget require deletions.** `restic forget` deletes snapshot files;
   `restic prune` rewrites and deletes pack and index files. On an object-locked bucket
   every one of these deletions fails until the per-object retain-until date passes.
   Dedup-based space reclamation is therefore impossible on a locked bucket.
2. **Worse: lock files.** restic writes a lock file under `/locks/` at the start of
   every operation — including plain `restic backup` — and deletes it on completion
   (stale locks are normally purgeable after 30 min via `restic unlock`). Under
   Compliance-mode Object Lock even those deletions fail: stale locks accumulate, cannot
   be purged by anyone (Compliance mode binds the bucket owner too), and operations that
   need an exclusive lock eventually wedge the repository permanently. A naive
   "restic + object-locked bucket" deployment is not degraded — it is broken.

Both facts were confirmed during the 2026-07-11 research pass (restic locking model,
rest-server append-only mode, rustic lock-free design) and match the product owner's own
findings.

At the same time, `Standard` locations depend on prune for cost control (R24 retention +
dedup) **and** on `forget`+`prune` for **right-to-erasure** (R21,
[adr/0009](0009-shared-cluster-repo-tag-tenancy.md)). Neither lifecycle can coexist with
Object Lock in one repository, and neither can run against locked objects before their
retain-until — so immutability also **defers physical erasure**, not only prune.

## Decision

**Immutability is a mode of the backup location, mutually exclusive with the prune-based
lifecycle.** `ClusterBackupLocation` and `BackupLocation` carry
`spec.mode: Standard | Immutable` (exact fields in [02-api.md](../02-api.md)). The API
ships day 1 (M1 CRDs); the implementation lands in **M8** per
[90-roadmap.md](../90-roadmap.md).

### Immutable mode design

- **Append-only writes, no prune ever, no in-repo deletion.** `maintenance.pruneSchedule`
  is forbidden on Immutable locations (CEL validation rule, [02-api.md](../02-api.md)
  §Validation #6). `restic forget` is not executed against locked objects either — in
  the base design it is never run at all: snapshots are enumerable per rotation window,
  so bookkeeping does not require deleting snapshot files. `BackupSchedule.spec.retention`
  (R24) is not enforced on Immutable locations; the admission policy emits a **VAP `Warn`**
  when a schedule targeting an Immutable location sets `keep*` fields
  ([02-api.md](../02-api.md) §Validation #3, [adr/0010](0010-admission-vap-first.md)).
- **Retention by repository rotation** (`spec.immutable.rotationPeriod`, e.g. `720h`):
  the operator opens a **new restic repository per time window** at
  `s3://<bucket>/<prefix>/<clusterID>/<windowStart>/` (windowStart formatted
  `20260801T000000Z`), one level below the R20 repo root. Rotation is at the
  **repository** granularity of each plane: the cluster plane's *one shared repo for all
  namespaces* rotates as a unit — tenancy stays tag-based, **not** per-namespace repos
  ([adr/0009](0009-shared-cluster-repo-tag-tenancy.md)) — and a namespace-plane location
  rotates its own single repo. Each window repo is initialized and keyed exactly like a
  Standard repo of its plane ([adr/0004](0004-encryption-key-management.md)): one platform
  DEK wrapped by the cluster KEK (cluster plane) or the user's own password (namespace
  plane), one key per window — **no per-namespace DEK, no crypto-shredding**.
- **Expiry = whole-window deletion.** Objects inherit the bucket's default Object Lock
  retention `D`. A window that closed at time `T` becomes fully deletable at `T + D`;
  the operator then deletes the **entire window prefix** (every namespace in it at once).
  Guaranteed restore horizon is `[D, D + rotationPeriod)`. The location controller reads
  the bucket's `GetObjectLockConfiguration` and refuses `Ready` if `D < rotationPeriod`
  (which would create deletable-before-rotation gaps) or if lock is absent while
  `objectLockMode: Governance|Compliance` is declared.
- **Right-to-erasure defers to lock expiry.** `ClusterErasure` (R21) is **physical**
  `forget`+`prune` ([adr/0004](0004-encryption-key-management.md),
  [adr/0009](0009-shared-cluster-repo-tag-tenancy.md)); per-tenant crypto-shredding does
  not exist here (a shared window repo has a single platform key). On an Immutable
  location that deletion cannot execute before the objects' retain-until, so
  `ClusterErasure` reports `phase: Blocked` with `status.blockedUntil` = the retain-until
  of the youngest window still holding the target. It is then satisfied as those windows
  age out and are deleted whole (the base design never issues an early `forget`).
  Whole-repo key destruction stays a repository **decommission** tool only, never a
  per-tenant erasure ([adr/0004](0004-encryption-key-management.md)).
- **`objectLockMode`** selects the enforcement mechanism: `Compliance` (nobody can
  delete before retain-until, bucket owner included), `Governance` (privileged bypass
  possible with `s3:BypassGovernanceRetention`), or `AppendOnlyProxy` (no bucket lock;
  `rclone serve restic --append-only` fronts the S3 bucket and denies deletions at the
  HTTP layer).
- **Lock-file strategy** — the central M8 POC question, candidates ranked:
  1. **rustic as the mover engine for Immutable locations.** rustic is lock-free by
     design ("lock-free operations, two-phase pruning" — official comparison docs): it
     writes **no** lock files, and writes/reads the same restic repo v2 format.
     Precondition: rustic must first pass the metadata-fidelity suite (xattrs, ACLs,
     hardlinks, sparse — cf. ADR 0001, which keeps restic CLI as the v1 mover and names
     rustic the designated challenger). Cleanest option: works even under Compliance
     mode with zero infrastructure added.
  2. **`rclone serve restic --append-only`.** A proxy fronting any rclone remote —
     including our S3 bucket — that permits the lock-file lifecycle (restic may create
     *and delete* objects under `/locks/`) while denying deletion of data, index and
     snapshot files. Restic CLI then works unmodified. Costs a proxy in the data path
     and its own S3 credential; exact append-only semantics (notably lock deletion and
     the config file) must be verified in the M8 POC — the research pass recorded the
     mode's existence, not its byte-level rules. Upstream restic/rest-server is *not*
     an option here: it has no S3 backend (it serves a local `--path`), so
     rest-server-with-its-own-storage would silently move backup data out of the S3
     location — rejected.
  3. **Governance mode + narrowly-scoped `/locks/` bypass.** In theory: allow
     `s3:DeleteObject` + `s3:BypassGovernanceRetention` on
     `arn:…:<bucket>/<prefix>/…/locks/*` only. In practice restic does not send the
     `x-amz-bypass-governance-retention` header, and per-prefix *exemption from default
     retention at write time* is not expressible in standard S3 Object Lock — documented
     here as **unlikely to work**; kept only as a POC data point.
- **Verification (R17) still applies**: read-only operations (`snapshots`, `ls`, `dump`,
  `restore`) accept `--no-lock`; `restic check` wants an exclusive lock, so the check
  path must go through whichever lock strategy the POC validates (rustic `check` being
  the natural fit).
- **Metrics (R19)**:
  `crystalbackup_immutable_window_start_timestamp_seconds{location,scope,cluster}`,
  `crystalbackup_immutable_expired_repos_deleted_total{location,scope,cluster}` — window
  rotation is a per-repository event, so these are location-scoped, not per-namespace —
  plus the standard per-backup metrics (namespace-labeled). `BackupRepository.status`
  gains window bookkeeping (`activeWindowStart`, per-window snapshot counts) at M8.

### Standard vs Immutable trade-offs

| | `Standard` | `Immutable` |
|---|---|---|
| Deletion protection | Credentials/RBAC only; compromised creds can delete | Storage-level WORM; Compliance mode resists even the platform admin |
| Retention | R24 `keep*` via `forget` + prune | Rotation window + bucket lock duration; `keep*` ignored |
| Right-to-erasure (R21) | Immediate `forget`+`prune` | **Blocked** until object-lock expiry; served by whole-window age-out |
| Dedup horizon | Whole repo lifetime | **Resets every rotation window** — first backup of a window re-uploads everything |
| Storage cost | Baseline | Higher: ≥ 1 full copy per live window + deltas; space freed only per whole window |
| Space reclamation | `prune` (serialized per repo) | Whole-prefix deletion after lock expiry; no prune |
| Restore horizon | Exactly the R24 policy | `[D, D + rotationPeriod)` — coarse-grained |
| Moving parts | restic CLI only | Lock strategy (rustic / rclone proxy / bypass) + rotation controller |

## Consequences

### Positive

- Ransomware and insider-deletion protection with **standard upstream semantics**: an
  Immutable window repo remains a plain restic repo, readable with upstream `restic`
  (R8 reversibility intact — this is why a custom WORM-ish format was never considered).
- Clean mental model for users and admins: a location is either prunable or immutable,
  never a hybrid; CEL makes invalid combinations unrepresentable from day 1, so no API
  break when M8 lands (R18 as a v1 *design* constraint is honored).
- Whole-window expiry avoids every restic-on-WORM failure mode: no prune, no forget, no
  lock-file deletion required on locked objects (strategy a), and bounded growth.
- Rotation windows double as blast-radius bounds: index size, check duration and restore
  enumeration are all per-window.

### Negative

- Storage cost: dedup resets per window. A namespace with 100 GiB and 1% daily churn on
  a 720h window stores ~1 full copy + deltas per live window instead of one copy total.
- Restore-horizon granularity is coarse (window-sized), and snapshots near a window
  boundary live up to `D + rotationPeriod` — users pay for that tail.
- **Right-to-erasure is not immediate.** On an Immutable location a tenant's
  `ClusterErasure` cannot physically delete before lock expiry and stays `Blocked` until
  the covering windows age out ([adr/0004](0004-encryption-key-management.md),
  [adr/0009](0009-shared-cluster-repo-tag-tenancy.md)) — a WORM-vs-GDPR tension that the
  tenant contract must state (erasure horizon bounded below by `D`).
- Immutable mode likely introduces a second engine binary (rustic) or a proxy
  (rclone) into an otherwise restic-CLI-only data plane.

### Risks & mitigations

- **rustic fails the metadata-fidelity suite** (hardlink restore only landed in rustic
  0.11.2, 2026-04 — young code path): fall back to strategy (b) `rclone serve restic
  --append-only`; both are POC-gated in M8 before any GA claim.
- **rclone append-only semantics differ from our assumption** (e.g. denies lock
  deletion too): detected by the M8 e2e suite against Ceph RGW object-lock; strategy (a)
  remains the primary.
- **Wedged repo despite precautions** (stale locks on a Governance bucket): Governance
  mode allows an admin break-glass bypass to purge `/locks/`; document in the ops
  runbook. Compliance mode offers no break-glass by definition — say so in the docs and
  default `objectLockMode` examples to `Governance`.
- **Operator bug deletes a live window**: expired-repo deletion requires
  `now > windowEnd + D` *and* is double-checked against the bucket's own retain-until
  metadata before issuing deletes; under Compliance the storage layer would refuse
  anyway.

## Alternatives considered

- **kopia's native Object Lock support.** kopia maintains locks-free repositories and
  has upstream object-lock awareness (retention extension during maintenance). Moot: the
  engine was disqualified in ADR 0001 (R10 metadata gaps — no xattrs/ACLs/hardlinks,
  kopia#544 — and single-password repos breaking R3/R21 multi-key).
- **Single ever-growing append-only repository** (no rotation). Simple, maximal dedup —
  and unbounded: no deletion path ever, index and storage grow forever, `check` cost
  grows forever. Rejected.
- **Per-snapshot Object Lock tagging / selective retention.** Impossible with a
  deduplicating format: restic pack files are shared between snapshots, so there is no
  1:1 snapshot→objects mapping to hang a per-snapshot retain-until on. Rejected as
  structurally infeasible, not merely hard.
- **Immutability as a separate CRD or per-schedule flag.** Mixing immutable and pruned
  data in one repository is exactly the broken hybrid described in Context; the property
  belongs to the storage target, hence a mode on the location CR.

## Revisit triggers

- Results of the rustic metadata-fidelity suite (ADR 0001 gate): a pass makes strategy
  (a) the default and may retire the rclone proxy option before it ships.
- restic upstream gains native Object Lock support (retain-until–aware writes, lock-file
  avoidance, or an official no-lock mode for all operations).
- Repository **sharding** is adopted (per-tenant repos —
  [adr/0009](0009-shared-cluster-repo-tag-tenancy.md)): per-tenant keys would let
  crypto-shredding erase a tenant *inside* a live Immutable window, reopening the erasure
  story above.
- e2e findings on Ceph RGW Object Lock behavior diverging from AWS semantics
  (the target production S3 is Ceph RGW — the M8 POC must run against it, not only a
  lightweight S3 mock).
