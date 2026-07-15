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
- `spec.encryption.platformAccess` (default `false`) registers an **optional operator key
  slot** (`restic key add`) so the operator can mediate restore/verification. Default `false`
  keeps off-platform backups **private to the user**; `keySlots` is `[tenant]`, or
  `[tenant, platform]` when `platformAccess: true`.

### 3. restic native encryption and multi-key slots

restic multi-key is why the restic format won R3/R5 in the survey (kopia is single-password
per repo): several passwords unlock the same master key. Two uses in this model:

- **Cluster plane** — the platform DEK is the single slot; a user never gets it and reaches
  cluster-DR data only through an operator-mediated `Restore` with a non-forgeable
  `namespace=` tag filter ([adr/0009](0009-shared-cluster-repo-tag-tenancy.md)).
- **Namespace plane** — the user's password is the primary slot (upstream-restic
  reversibility, R8); the optional `platformAccess` slot is a second password over the same
  repo.

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
| User loses their own `BackupLocation` password | Their responsibility (documented); with `platformAccess: true` the operator slot can re-add a user key |
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

## Open questions

- `crystalctl admin decommission` on **Standard** locations: destroy the wrapped DEK/KEK only
  (objects age out via bucket lifecycle) or also `restic`-delete the repository objects?
- Whether the operator-generated namespace password (when `repositoryPasswordSecretRef` is
  omitted) needs any escrow option beyond the explicit `platformAccess` slot.
