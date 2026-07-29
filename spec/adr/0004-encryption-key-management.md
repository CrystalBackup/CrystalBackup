# ADR 0004 — Encryption: restic native crypto with envelope key management

Status: **Accepted** (2026-07-12, supersedes the per-repository-DEK / crypto-shredding
design of 2026-07-11)

## Context

Crystal Backup must provide (see [00-requirements.md](../00-requirements.md)):

- **R3** — two **independent** encryption tiers (not a hierarchy): the shared cluster-plane
  repository has one platform key; each namespace-plane repository has its own user key. See
  the Decision below.
- **R8** — reversibility: a namespace user must be able to read their backups with **upstream
  `restic`**, given only S3 credentials and a key.
- **R13** — backup storage compressed, deduplicated and encrypted.
- **R21** — right-to-erasure: a tenant's data (tenant / namespace / PVC granularity) must be
  permanently removable from the backups (GDPR), even though the cluster plane stores every
  namespace in **one shared repository** ([adr/0009](0009-shared-cluster-repo-tag-tenancy.md)).

ADR [0001](0001-repository-engine-restic-format.md) fixed the repository engine to the
**restic format** (repo v2). Relevant properties of that format, from the restic design
document and key-management docs
(<https://restic.readthedocs.io/en/stable/references/design.html>):

- All repository data is encrypted with AES-256-CTR and authenticated with Poly1305-AES.
  Encryption **cannot be disabled**: the `--insecure-no-password` option merely sets an
  empty password — data on S3 is still ciphertext.
- A repository has one **master key** (data key). Key files stored inside the repo wrap
  that master key with a scrypt-derived key from each registered **password**. `restic
  key add|remove|list` manages multiple password slots over the same master key
  (multi-key), so access passwords rotate cheaply.
- Documented limitation: "it is impossible to securely revoke a leaked key without
  re-encrypting the whole repository" — removing a key slot does not rotate the master
  key.

The two-plane model ([02-api.md](../02-api.md),
[adr/0009](0009-shared-cluster-repo-tag-tenancy.md)) replaces the earlier
one-repository-per-namespace design: the **cluster plane** writes all namespaces into a
single shared repository per `ClusterBackupLocation`, and the **namespace plane** lets a user
back up their own namespace to their own object storage with their own key. This ADR records
the key model that follows, superseding the per-repository DEK hierarchy and per-tenant
crypto-shredding of the 2026-07-11 draft.

## Decision

Use **restic native encryption** for all data at rest and implement R3/R21 in **key
management**, with a **two-tier envelope** — and **no per-namespace DEK hierarchy**.

### 1. Cluster plane — one platform DEK, wrapped by a cluster KEK

- The shared cluster repository (one per `ClusterBackupLocation`, cf.
  [adr/0009 §1](0009-shared-cluster-repo-tag-tenancy.md)) is initialized with **one** random
  **256-bit DEK** (crypto/rand, base64-encoded) used as its restic password. This single
  **platform key** protects every namespace in the repo. There is **no per-namespace DEK and
  no cluster→client→namespace KEK hierarchy**: a shared repo has exactly one restic master
  key by construction, so per-namespace DEKs inside it would be fiction.
- The DEK is stored **wrapped** with [age](https://age-encryption.org) (X25519,
  `filippo.io/age`, BSD-3-Clause): the **cluster KEK** is an age identity
  (`ClusterBackupLocation.spec.encryption.clusterKEKSecretRef`), the wrapped DEK an age
  ciphertext in a Kubernetes Secret in **`crystal-backup-system`**. age was chosen over a
  hand-rolled AES-256-GCM seal: same security level, smaller audit surface, standard
  break-glass tooling.
- Wrapping is a pure key-management operation: the operator can **re-wrap** on demand
  (decrypt with the old KEK, encrypt with the new — no repository data touched), which serves
  **KEK rotation** and KMS hygiene at O(1). The wrapping sits behind a small `Wrapper`
  interface (`Wrap(dek) / Unwrap(blob)`) so a KMS-backed KEK can replace the age identity
  without touching repository logic.
- The DEK is never shown to users; movers receive it as a short-lived projected Secret
  ([03-security-and-tenancy.md](../03-security-and-tenancy.md)).
  `BackupRepository.status.keySlots` reports `[platform]` for a cluster repo.

### 2. Namespace plane — the namespace user's own key

- A namespaced `BackupLocation` is protected by the **user's own restic password**:
  `spec.encryption.repositoryPasswordSecretRef` (a Secret in the user's namespace). It *is*
  the primary — and by default only — key slot: their key, their reversibility (R8).
- If `repositoryPasswordSecretRef` is omitted the operator **generates** a random password
  and stores it as a Secret **in the user's namespace** — still the user's key, never held
  in `crystal-backup-system`.
- There is **no operator key slot on a user repository, ever**. `keySlots` is `[tenant]`, and
  that is the only value it can take. See the 2026-07-28 amendment: `platformAccess` was
  specified here, never implemented, and then **dropped on purpose**.

### 3. restic native encryption and multi-key slots

restic multi-key is why the restic format won R3/R5 in the survey (kopia is single-password
per repo): several passwords unlock the same master key. Two uses in this model:

- **Cluster plane** — the platform DEK is the single slot; a user never gets it and reaches
  cluster-DR data only through an operator-mediated `Restore` with a non-forgeable
  `namespace=` tag filter ([adr/0009](0009-shared-cluster-repo-tag-tenancy.md)).
- **Namespace plane** — the user's password is the **only** slot (upstream-restic
  reversibility, R8). restic's multi-key capability is deliberately left unused here; the
  amendment below explains why a second slot is the one thing this plane must not have.

### 4. Right-to-erasure (R21), not per-tenant crypto-shredding

- **Per-tenant crypto-shredding is impossible in the shared cluster repo.** It has a single
  master key protecting all namespaces; there is no per-tenant key to destroy. Erasure is
  therefore **physical**: `ClusterErasure` runs `restic forget --tag`
  (`tenant=` / `namespace=` / `namespace=+pvc=`) then `prune`, deleting the tenant's data
  from the object storage (contract in
  [02-api.md](../02-api.md#repository-layout--snapshot-identity)).
- On **Immutable** locations erasure is **`Blocked` until object-lock expiry** (WORM vs.
  erasure tension — [adr/0005](0005-immutability-mode.md)); on **Standard** it reclaims
  immediately.
- **Whole-repo key destruction survives only as a repository "decommission" operation**
  (`crystalctl admin decommission`): destroy the wrapped platform DEK (and its KEK) to retire
  an entire repository at once. It is **repo-granularity, not tenant-granularity**, so it is a
  lifecycle/retirement tool, **not** a GDPR erasure mechanism. Per-tenant key destruction
  would require per-tenant repositories (sharding — deferred, see Alternatives and
  [adr/0009](0009-shared-cluster-repo-tag-tenancy.md)).

## Consequences

### Positive

- **R8 preserved intact**: repositories are plain restic repos; upstream `restic` reads them
  with a password. No Crystal Backup software needed for exit — a strong sovereignty argument.
- **R13 dedup/compression untouched**: no outer layer re-encrypting restic's packs, so
  content-defined dedup (including **cross-namespace dedup** in the shared cluster repo,
  adr/0009), zstd compression, `restic dump --archive tar` (R8 export) and partial restore
  (R7) all work natively.
- **A much simpler key model**: one platform key per cluster repo, one user key per namespace
  repo — no "most-specific-wins" KEK resolution, no per-namespace DEK bookkeeping, no
  intermediate KEK tier.
- **Cheap key lifecycle**: age KEK wrapping keeps rotation and re-wrap O(1) per repository;
  the `Wrapper` seam keeps a future KMS non-disruptive.

### Negative

- **Leaked-DEK revocation is expensive.** restic's documented limitation applies: removing a
  slot does not rotate the master key. A compromised platform DEK (e.g. exfiltrated from a
  mover) requires **re-encryption via repository copy** (`restic copy` into a freshly
  initialized repo with a new DEK, then retire the old repo). Runbook in M5;
  `crystalctl admin reencrypt` automation is backlog.
- **No per-tenant crypto-shredding.** Right-to-erasure is `forget`+`prune` (physical), which
  is **blocked on Immutable locations** until object-lock expiry
  ([adr/0005](0005-immutability-mode.md), [adr/0009](0009-shared-cluster-repo-tag-tenancy.md)).
- restic's AES-256-CTR + Poly1305-AES is encrypt-then-MAC rather than a modern AEAD; accepted
  as battle-tested (10+ years, publicly documented format, two independent implementations).
- **The cluster repo key unlocks every namespace.** It is admin-only, never leaves
  `crystal-backup-system`, and tenant reads are always operator-mediated with the
  non-forgeable `namespace=` tag filter (adr/0009,
  [03-security-and-tenancy.md](../03-security-and-tenancy.md)).

### Risks & mitigations

| Risk | Mitigation |
|---|---|
| Loss of the wrapped platform DEK Secret = loss of all platform access to the cluster repo | Wrapped-DEK Secret included in platform DR (Velero today, `ClusterBackupSchedule` at M9); cluster KEK escrowed offline (sealed, two-person) |
| Loss of the cluster KEK | Re-wrap needs the old KEK: KEK escrow above; re-wrap promptly on rotation |
| Mover compromise leaks the platform DEK (shared repo → all namespaces) | Movers confined to `crystal-backup-system`, short-lived projected Secret, NetworkPolicies; leaked DEK → repo-copy reencrypt runbook |
| User loses their own `BackupLocation` password | **Their data is gone, and this is the accepted consequence of the amendment below.** Documented in `docs/DECOMMISSION.md`; the operator has no slot to re-add a key from, by design |
| Erasure requested on an Immutable location | `ClusterErasure` reports `Blocked` + `blockedUntil`; completes after object-lock expiry (adr/0005) |

## Alternatives considered

1. **Disable restic encryption + a custom Crystal Backup encryption layer** (originally
   floated to make crypto-shredding trivial). Rejected:
   - restic cannot run unencrypted: `--insecure-no-password` is an *empty password*, not
     plaintext — the "disable" half of the idea does not exist upstream.
   - An outer layer (encrypting packs before upload or via a proxy) breaks **R8**: upstream
     restic could no longer read the repository without Crystal Backup tooling.
   - Encrypting *before* restic (per-file) destroys content-defined **dedup** and breaks
     partial restore / `dump` streaming (R7/R8).
   - Rolling our own cryptography is a liability we explicitly do not want to own.
   - Its sole motivation — trivial crypto-shredding — is moot: erasure is physical
     `forget`+`prune` regardless.
2. **Per-namespace DEK / per-tenant crypto-shredding.** Now **tied to repository sharding**
   (per-tenant repos), which is **deferred, not rejected**
   ([adr/0009 § Alternatives](0009-shared-cluster-repo-tag-tenancy.md)): a shared
   single-master-key repo cannot carry per-tenant keys, and sharding would lose
   cross-namespace dedup. Revisit if a regulator or user requires per-tenant key custody or
   crypto-shredding.
3. **kopia's native envelope crypto** (modern AEAD, password-derived master key). Rejected
   with the engine choice ([adr/0001](0001-repository-engine-restic-format.md)): single
   password per repository defeats the platform+user slot model (R3/R5), and kopia fails R10
   (xattrs/ACLs/hardlinks).
4. **Plain restic passwords per repo, no envelope** (K8up-style). Rejected: without age
   wrapping there is no cheap KEK rotation, no standard break-glass tooling, and no `Wrapper`
   seam for a future KMS.
5. **KEK in an external KMS from day 1** (Vault transit / cloud KMS). Deferred, not rejected:
   v1 keeps KEKs as Kubernetes Secrets to avoid a hard runtime dependency for every backup.
   The `Wrapper` interface (`Wrap(dek) / Unwrap(blob)`) lets a KMS-backed implementation be
   added without touching repository logic.
6. **AES-256-GCM sealing implemented in-house instead of age.** Rejected: equivalent security,
   more code to review, no standard break-glass tooling.

## Revisit triggers

- restic upstream gains true master-key rotation or per-snapshot keys → revisit the reencrypt
  runbook.
- **Repository sharding is adopted**
  (triggers in [adr/0009](0009-shared-cluster-repo-tag-tenancy.md)) → per-tenant DEKs and
  per-tenant crypto-shredding become possible again; re-open this ADR.
- An external KMS becomes a platform requirement (SecNumCloud trajectory, a user requiring an
  HSM) → implement the KMS `Wrapper`, move KEKs out of Secrets.
- The mover engine changes to rustic (`rustic_core` ≥ 1.0 exposes direct master-key / KMS
  support) → evaluate direct master-key injection, keeping the same
  envelope model.
- Volume of `reencrypt` operations becomes significant (frequent DEK compromises) →
  prioritize `crystalctl admin reencrypt` automation from backlog to milestone.


## Amendment (2026-07-28, M5 implementation) — `platformAccess` is dropped

`spec.encryption.platformAccess` is **removed from the API**. A namespace-plane repository has
exactly one key slot, the user's, and the operator has no mechanism to add a second.

### How it came up

M5 implemented the namespace plane and the field went in half-way: it was read from the spec and
reflected in `BackupRepository.status.keySlots` as `[tenant, platform]`, but **no `restic key add`
was ever written**. So a location with `platformAccess: true` advertised a key slot that did not
exist in the repository — an admin trusting that status to perform a mediated restore would have
discovered the gap at the moment they needed it. Closing that hole forced the question of whether
the field should exist at all.

### Why it is dropped rather than implemented

The first framing was that the field encodes a real and carefully-worded distinction: with
`platformAccess: false` the operator holds no **durable, independent** way in — it must reach into
the user's namespace and read their Secret, which is visible in the audit log and stops working if
the user rotates their password. That framing is accurate, and it is not the one that decides this.

The deciding frame is **revocability**, which is the right frame for access to someone else's data:

- A namespace-plane `BackupLocation` is a backup the user takes **in addition to** cluster DR. It
  is theirs. If they delete their password Secret, platform access should end. Full stop.
- A `platformAccess` slot breaks exactly that. It is a password held in `crystal-backup-system`
  that keeps working after the user rotates their key, deletes their Secret, or changes their
  mind — and §3 of this ADR is what makes it permanent: **removing a key slot does not rotate the
  master key.** Once the platform has held that password, the user can never take it back. Not by
  revoking the slot, not by rotating their own key, not by anything.
- Which means the shape of the feature, from the position of the person whose data it is, is: *the
  platform had access once, so it minted itself permanent access the customer cannot revoke.* That
  the customer opted in once does not fix it, because opting in was a one-way door they were not
  told was one-way.

### Trust by construction, not by policy

The alternative considered was to keep the field and gate it — reject `platformAccess: true` at
admission until the feature is properly designed. Rejected: a guarantee enforced by a webhook is a
guarantee that a flag, a bypass or a future maintainer can switch off. A guarantee that rests on
the mechanism **not existing** cannot be switched off. For access to a customer's data, that
difference is the whole product claim, so it is bought at the API level rather than the policy
level.

### Consequences accepted

- **Mediated restore and operator-side verification are off the table on the namespace plane.**
  A user who wants help gives their password, deliberately, in the moment — and can change it
  afterwards. That is the point.
- **A user who loses their password loses their backups.** There is no platform copy of the key
  and no recovery path. `docs/DECOMMISSION.md` and the user docs must say so plainly rather than
  leaving it to be discovered.
- **If mediated restore is ever revisited**, it needs a mechanism the user can genuinely revoke —
  which, given that slot removal does not rotate the master key, means a re-encrypting copy
  (adr/0013), not a second slot. Reintroducing `platformAccess` is not the answer to a future
  customer request; this paragraph exists so that is not rediscovered the hard way.

## Open questions

- `crystalctl admin decommission` on **Standard** locations: destroy the wrapped DEK/KEK only
  (objects age out via bucket lifecycle) or also `restic`-delete the repository objects?
- Whether the operator-generated namespace password (when `repositoryPasswordSecretRef` is
  omitted) needs an escrow option at all. Note it must NOT be an operator key slot — the
  amendment forecloses that — so any answer here is a mechanism the USER holds and can destroy,
  not one the platform holds on their behalf.
