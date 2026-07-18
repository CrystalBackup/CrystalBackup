# Security & Tenancy

Status: draft v2 (two-plane cascade + shared repository). Naming & field contract:
[02-api.md](02-api.md). Tenancy model: [adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md).
Key management: [adr/0004](adr/0004-encryption-key-management.md). Immutability:
[adr/0005](adr/0005-immutability-mode.md).

## 1. Principles

1. **Isolation is enforced server-side, never client-declared.** A namespaced CR contains
   no field that names another namespace's repository, credentials, or keys; the operator
   derives both the repository and the restic `namespace=` tag filter from the CR's own
   `metadata.namespace` (R2, R14).
2. **Backups must not widen a namespace compromise.** An attacker holding a namespace
   user's Kubernetes credentials gains no capability through Crystal Backup beyond what
   they already had inside their own namespace.
3. **Defense in depth.** Admission policies validate (VAP first, a minimal dynamic webhook —
   [adr/0010](adr/0010-admission-vap-first.md)), controllers re-derive repository identity and
   the tag filter, storage credentials scope down, RBAC confines. No single layer is
   load-bearing for tenancy — and on the shared cluster repository, encryption is
   explicitly **not** a tenant boundary (one master key, all namespaces), so the boundary
   is the operator-mediated `namespace=` tag filter plus the admin-only key.
4. **The platform is trusted for the cluster DR repository; users can stay off-platform.**
   Cluster disaster recovery uses one shared, admin-owned repository the platform can read
   by design. A namespace user who wants zero platform readability backs up through their
   **own** `BackupLocation` + key with `platformAccess: false` (R3, R5).

## 2. Threat model

| Threat | Vector | Primary controls | Residual risk |
|---|---|---|---|
| Malicious namespace user | Crafted CRs: point a `Restore`/`Backup` at cluster DR or another namespace, hostile hook commands, schedule spam | Invariants I1–I2, I7 (§3): the `namespace=` filter cannot be forged, cluster-origin `Backup`s are read-only, a user CR cannot reference a `ClusterBackupLocation`; hooks exec only in the CR's own namespace (§5); admission rules (§8); global/per-node mover concurrency caps | No per-user fair-share of mover slots in v1 (R24: no quotas) — a noisy user can delay others' backups, not read them; the shared-repo prune window contends with backups (serialized, never corrupts) |
| Compromised namespace user K8s credentials | Attacker creates `Restore`/`BackupSchedule` in the victim namespace | Blast radius = that namespace's own data (I1); a cluster-origin `Backup` is served only through the mediated `namespace=<that namespace>` filter, so it restores the namespace's **own** history back into itself; R23 confirmation gates accidents, not attackers; exfiltration via a user `BackupLocation` adds nothing the namespace's pod egress didn't already allow | Attacker can destroy in-namespace data via a `Recreate` restore — same power as their existing PVC access; off-platform copies (user location) survive |
| Compromised mover | Container escape attempt, credential/DEK theft from the Job | Unprivileged pod, `seccompProfile: RuntimeDefault`, caps dropped (§6); **no ServiceAccount token** (`crystal-mover`, `automountServiceAccountToken: false`, zero RBAC); short-lived repo-scoped S3 credentials (I4); NetworkPolicy egress = S3 only (§7); snapshot mounted `readOnly` — source data cannot be tampered | A leaked **cluster-repo** mover credential or DEK reads/writes the **whole shared repo** (all namespaces) — the shared-repo cost ([adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md)); bounded by TTL, confinement and Immutable mode; a leaked DEK → repo-copy reencrypt ([adr/0004](adr/0004-encryption-key-management.md)). On a **user** location the mover holds only that user's key; a **sync mover** transiently holds the **source + destination** keys and egresses to two S3 endpoints, under the same TTL/confinement (R28, I9) |
| Ransomware / deletion on S3 | Stolen S3 credentials encrypt or delete backup objects | Cluster-repo root creds exist only in `crystal-backup-system` (I3); `Immutable` location mode = S3 Object Lock / append-only (R18) — note: object-locked buckets break `restic prune` **and** lock-file deletion, hence the no-prune + rotation design in [adr/0005](adr/0005-immutability-mode.md) | In `Standard` mode a mover credential can delete objects of the **shared** cluster repo (restic needs delete for lock files and prune) — blast radius is the whole DR repo, not one namespace; `Immutable` mode (R18, M8) removes even that. Until M8, the platform Velero safety net and off-platform user locations are the fallback |
| Insider (platform administrator) | Admin reads every namespace via the platform key, or erases data via `ClusterErasure` | Cluster KEK access restricted to the operator SA + break-glass group; `ClusterRestore`/`ClusterErasure` are cluster-scoped, audited objects (§12) with typed `confirmation` (R23); two-person review on key/erasure code (roadmap DoD) | The platform can read the shared cluster DR repo **by design** (one key, same trust level as existing etcd/node access); users opt out with their own location + key, `platformAccess: false` — data the platform cannot read |

## 3. Isolation invariants

These are the non-negotiable properties; every code review of controller or webhook
logic checks against this list (cf. roadmap DoD).

- **I1 — Operator-mediated repository access, non-forgeable tag filter (R2/R14
  cornerstone).** A namespaced `Restore` names a `Backup` **in its own namespace** and has
  **no** `locationRef`, no target-namespace field, no `clusterID`. When that `Backup` is
  `crystalbackup.io/origin: cluster`, the operator opens the shared DR repo but restricts
  it with a restic tag filter `namespace=<CR.metadata.namespace>` **derived server-side** —
  a user cannot express or override it. A `run`/backup name in `Restore.spec.source`
  resolves **only** within that filter; a name belonging to another namespace does not
  resolve.
- **I2 — One isolation mechanism per plane.** *Cluster plane*: **one shared restic
  repository** per `ClusterBackupLocation`; namespaces are separated by restic **tags**
  (`tenant`, `namespace`, `pvc`), not by repositories or keys — the repo has exactly one
  master key, so isolation rests on I1 (mediated filter) + I3 (admin-only key), **not** on
  cryptographic separation ([adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md)).
  *Namespace plane*: **one repository per user `BackupLocation`** in the user's own bucket
  with the user's own key — isolation is by construction.
- **I3 — Cluster DR credentials and key never leave the platform namespace.** The
  `ClusterBackupLocation` S3 Secret (`dr-s3`), the cluster KEK (`cluster-kek`) and the
  wrapped platform DEK (`crystal-dek-<location>`) exist only in `crystal-backup-system`.
  User namespaces never contain platform credentials; user CRs reference only
  same-namespace Secrets (typed name-only refs, 02-api rule 5).
- **I4 — Movers get short-lived, repo-scoped credentials.** *Cluster plane*: the operator
  mints S3 credentials via RGW STS `AssumeRole` with a session policy scoped to the shared
  repo prefix `<bucket>/<prefix>/<clusterID>/*` — **not** per namespace, because the shared
  repo is content-addressed and dedup-shared, so a namespace's data cannot be isolated to
  an S3 subtree (a direct consequence of I2). TTL = Job `activeDeadlineSeconds` + margin,
  projected as a Job-owned Secret (`ownerReference` → GC with the Job). Fallback when STS
  is unavailable: per-repo static keys via the RGW admin ops API. *Namespace plane*: the
  mover receives the user's own Secret — already scoped to the user's bucket by
  construction.
- **I5 — No backup pods, keys, or platform Secrets in user namespaces.** Movers and temp
  PVCs live in `crystal-backup-system`. The only objects the operator creates in a user
  namespace are `VolumeSnapshot`s (transient, during a run), transient manifest-mover
  RoleBindings (I6), restored PVCs/manifests on restore, and the projected read-only
  `Backup` objects written by discovery. `BackupRepository` is cluster-scoped and
  operator-internal.
- **I6 — Data movers have zero Kubernetes API access.** ServiceAccount `crystal-mover`
  with `automountServiceAccountToken: false` and no bindings; results flow back via the
  Job termination message, read by the operator. Sole exception: the manifest-mover Job
  (SA `crystal-manifest-mover`) automounts its token and receives a **transient**
  namespace-scoped RoleBinding — ClusterRole `crystal-manifest-reader` on backup,
  `crystal-manifest-writer` on restore — created by the operator in the target namespace
  and deleted when the Job completes.
- **I7 — Cluster-origin `Backup`s are read-only; the namespace plane cannot reference the
  cluster plane.** Admission: `Backup` objects labelled `crystalbackup.io/origin: cluster`
  are get/list/watch-only for users; a user-created `Backup`/`BackupSchedule` must
  reference a **namespaced** `BackupLocation`, never a `ClusterBackupLocation`. Because the
  repository (not the CR) is the source of truth, discovery re-creates a projection a user
  deletes.
- **I8 — Cross-cluster restore is admin-only in v1** (§9).
- **I9 — External sync re-keys; it never carries a repository's key to another repository
  (R28).** `ClusterBackupExternalSync`/`BackupExternalSync` replicate with **`restic copy`**,
  which **decrypts from the source and re-encrypts to the destination's own key** — never a raw
  object clone. A `BackupExternalSync` resolves **both** its source and destination
  `BackupLocation`s **within the CR's own namespace** (admission rule 2) and re-encrypts to the
  destination's **user key**, so a client's secondary copy is under the **client's** key,
  distinct from the platform key, and holds **only that namespace's** snapshots — client siloing
  is preserved. The sync Job handles both keys **transiently** in `crystal-backup-system` (never
  persisted), the same model as the namespace-plane backup mover (§4); `platformAccess: false`
  (no durable operator key slot) is unchanged. See [adr/0013](adr/0013-external-backup-sync.md).

## 4. Secrets and key management (ADR 0004)

Two independent tiers, **no per-namespace DEK and no cluster→client→namespace hierarchy**
(R3). Restic's native crypto is the only data-encryption layer (a custom outer layer was
rejected — it would break R8 reversibility with upstream restic and R13 deduplication).

- **Cluster plane** — the shared DR repository is initialized with **one** random 256-bit
  **platform DEK** used as its restic password. That DEK is stored **wrapped** with an
  [age](https://age-encryption.org) X25519 identity (the **cluster KEK**) as an age
  ciphertext Secret in `crystal-backup-system`. A shared repo has exactly one master key by
  construction, so per-namespace DEKs inside it would be fiction.
- **Namespace plane** — the repository is protected by the **user's own** restic password
  (`repositoryPasswordSecretRef`, a Secret in their namespace), or an operator-generated
  password stored **in the user's namespace** if that ref is omitted (their key, their
  reversibility). `platformAccess` (default `false`) optionally registers a second,
  operator-owned key slot for mediated restore/verification.

What lives where:

| Material | Object | Namespace | Readable by |
|---|---|---|---|
| Cluster KEK | Secret `cluster-kek` | `crystal-backup-system` | operator SA (get by name), break-glass admins |
| Wrapped platform DEK (one per cluster repo) | Secret `crystal-dek-<location>`, label `crystalbackup.io/location` | `crystal-backup-system` | operator; useless without the KEK |
| Cluster DR S3 root creds | Secret `dr-s3` | `crystal-backup-system` | operator (STS minting, maintenance Jobs) |
| Per-mover S3 creds + unwrapped DEK | Job-owned Secret, TTL-bounded | `crystal-backup-system` | the one mover Job (volume projection) |
| User location creds / user repo password | user-named Secrets (`offsite-s3`, `offsite-key`) | user namespace | user; operator by name (to run the user's movers) |

`BackupRepository.status.keySlots` reports `[platform]` for a cluster repo, `[tenant]` for
a user repo, or `[tenant, platform]` when `platformAccess: true`.

A **sync mover** (external sync, R28) is the one Job that projects **two** key/cred sets — the
source and destination repositories' — held only for its lifetime; there is still no durable
operator key slot (invariant I9).

Rules:

- The **unwrapped DEK exists only** (a) in operator memory during wrap/unwrap, (b) in the
  mover's projected Secret for the duration of one Job. It is never logged, never in CR
  status, never in events.
- **KEK is admin-provided, never operator- or chart-generated**: the cluster KEK is generated by
  the administrator and escrowed in the platform secret store *outside* the cluster before first
  use, then handed to the operator as the `cluster-kek` Secret. Neither the operator nor the Helm
  chart ever mints it (no key generation on any operator or install path — an absent KEK leaves the
  location degraded with `EncryptionValid=False`/`KEKMissing`; it is never silently created). A key
  born inside the cluster would die with it, taking every backup with it, so the KEK is the one
  secret whose custody the platform must own out-of-band (platform DR, R22).
- **Wrapped-DEK availability for bare-cluster DR**: the wrapped DEK (`crystal-dek-<location>`) today
  lives only in `crystal-backup-system`, so a *total* cluster loss would strand it even with the KEK
  safe in escrow — the KEK alone cannot open the repository without the wrapped DEK to unwrap. Full
  "restore with no surviving cluster" therefore also escrows the wrapped DEK **in the bucket**
  (useless without the KEK): DR then = pull the wrapped DEK from S3 + the escrowed KEK → unwrap →
  open restic. Wiring that bucket escrow is DR-bootstrap work (M2); until then the wrapped DEK Secret
  is part of what an operator must back up out-of-band.
- **KEK rotation** = decrypt-and-rewrap the `crystal-dek-<location>` Secret under the new
  KEK version (`kek.version` field). No data movement, no restic involvement; the `Wrapper`
  seam ([adr/0004](adr/0004-encryption-key-management.md)) lets a KMS-backed KEK replace age
  later; runbook target < 1 h.
- **Access-password rotation**: restic multi-key (`restic key add/remove`) rotates repo
  passwords per slot without re-encryption.
- **Compromised-DEK limitation** (from restic's own threat model): a leaked key cannot be
  securely revoked without re-encrypting the repository — the restic master key does not
  rotate. Procedure: `restic copy` into a fresh repo with a new DEK, then retire the old
  repo (`crystalctl admin reencrypt`, M5 runbook).
- **Right-to-erasure, not crypto-shredding** (R21): per-tenant crypto-shredding is
  impossible in a single-key shared repo and is dropped. `ClusterErasure` deletes
  **physically** — `restic forget --tag` (`tenant=` / `namespace=` / `namespace=+pvc=`)
  then `prune`. Whole-repo key destruction survives only as a repository **decommission**
  tool (`crystalctl admin decommission`), repo-granularity, not a GDPR mechanism. On
  `Immutable` locations erasure is `Blocked` until object-lock expiry (recorded in the
  audit output).

## 5. RBAC packaging

Per the platform RBAC policy: **no `*` verbs anywhere**; read = `get, list, watch`; full
access = the uniform 8-verb set
`create, delete, deletecollection, get, list, patch, update, watch`.

**Namespace user** — ClusterRole `crystal-backup-user`, aggregated into the platform's
per-namespace user roles (02-api RBAC section):

- 8 verbs on `backupschedules`, `backuplocations`, `restores`, `backupexternalsyncs` (group
  `crystalbackup.io`).
- **read-only** (`get, list, watch`) on `backups` — they are operator/discovery-managed
  projections, never user-written.
- Nothing cluster-scoped; `clusterbackuplocations`, `clusterbackupschedules`,
  `clusterbackups`, `clusterrestores`, `clustererasures`, `clusterbackupexternalsyncs` are
  admin-only, and `backuprepositories` are operator-internal (no user access).

**Operator** (SA `crystal-backup-operator`):

- 8 verbs on all `crystalbackup.io` kinds (both planes + cluster kinds) + `update`/`patch`
  on their `status` and `finalizers` subresources.
- 8 verbs on `volumesnapshots` and `volumesnapshotcontents`; read on
  `volumesnapshotclasses` (`snapshot.storage.k8s.io`).
- `persistentvolumeclaims`: 8 verbs (temp PVCs; restored PVCs in user namespaces);
  `persistentvolumes`: read; `storageclasses` (`storage.k8s.io`): read — the exposer
  resolves a PVC's CSI provisioner from its StorageClass (adr/0003). `namespaces`: read +
  `create` (ClusterRestore targets).
- `batch/jobs`: 8 verbs **via a namespaced Role in `crystal-backup-system` only** — the
  operator cannot create workloads anywhere else. The manager's Job informer is
  namespace-scoped to `crystal-backup-system` to match: the default cluster-wide Job cache
  would otherwise demand a cluster-wide `jobs` `list`/`watch` this Role deliberately
  withholds, CrashLooping the operator (cache sync fails) against its own least-privilege RBAC.
- `pods`: read; `pods/exec`: `create` (hooks, R16). Controller invariant: a hook only ever
  execs into pods **in the same namespace as the `BackupSchedule` that declares it** —
  users can only make the platform run commands they can already run themselves (the
  platform's namespace-user roles include in-namespace exec).
- `secrets`: `get` **only** — no `list`/`watch`; the operator reads Secrets by name via a
  direct API client (cache bypass), so no cluster-wide Secret cache ever exists in its
  memory. Namespace-scoped Secret `list` exists only in the manifest mover's transient
  binding, never on the operator SA.
- `events`: `create`, `patch`.

**Data mover** (SA `crystal-mover`): zero RBAC, no token (I6).

**Manifest mover** (SA `crystal-manifest-mover`): no standing bindings; per-Job transient
namespace-scoped RoleBinding created and deleted by the operator (I6), binding:

- ClusterRole `crystal-manifest-reader` (backup, R15): read on all namespaced resource
  types (includes Secrets — see §10).
- ClusterRole `crystal-manifest-writer` (restore): additionally `create`/`update` on
  arbitrary namespaced kinds in the target namespace — the single largest grant of the
  system; it is Velero-equivalent, scoped to one namespace for one Job's lifetime, and any
  widening beyond it requires an ADR (roadmap DoD).

**Cluster-scoped capture/restore** (R22, [adr/0011](adr/0011-cluster-scoped-dr.md)) is
**cluster-plane only** — there is no namespace-user path to it. A `ClusterBackup`'s capture Job
binds ClusterRole `crystal-cluster-manifest-reader` (read on the allow-listed cluster-scoped
kinds — CRDs, StorageClasses, IngressClasses, PriorityClasses, ClusterRoles/Bindings, PVs);
`ClusterRestore` of cluster-scoped objects binds `crystal-cluster-manifest-writer`
(create/update on them). Both are **admin-only** and transient per Job; cluster-scoped restore is
**opt-in** with R23 confirmation, because recreating cluster RBAC (`ClusterRoleBinding`s) is
privileged and must never be implicit. Widening either ClusterRole requires an ADR (roadmap DoD).

## 6. Pod security

| Namespace | PSA enforce | Notes |
|---|---|---|
| User namespaces | `baseline` (platform hardening status quo) | Crystal Backup schedules **no pods** here; unchanged posture |
| `crystal-backup-system` | `baseline` | movers need root uid, see below; the operator itself is `restricted`-compliant |
| Opt-in `rook-rbd-direct` exposer (ADR 0003) | would require `privileged` | confined to `crystal-backup-system`, never a user's; off by default (`exposure.rbdDirect`). `cephfs-shallow` (v1, CephFS) stays unprivileged |

`crystal-mover` securityContext (backup): `runAsUser: 0`, `capabilities: {drop: [ALL], add:
[DAC_OVERRIDE]}` (PSA-baseline-legal; reads files of arbitrary uid/gid — the write half of
the capability is moot because the snapshot volume is mounted `readOnly`),
`allowPrivilegeEscalation: false`, `seccompProfile: RuntimeDefault`,
`readOnlyRootFilesystem: true` (scratch `emptyDir` for the restic cache).

`crystal-mover` securityContext (restore): same, with `add: [CHOWN, DAC_OVERRIDE, FOWNER,
SETFCAP, MKNOD]` to restore ownership, modes (incl. setuid bits), `security.capability`
xattrs, ACLs and device nodes (R10). Known limit: `trusted.*` xattrs need `CAP_SYS_ADMIN`
and are **not** restored — documented, covered by the metadata-fidelity suite
([08-testing-and-dod.md](08-testing-and-dod.md)).

No `hostPath`, no host namespaces, no privileged containers anywhere in v1.

## 7. NetworkPolicies

`crystal-backup-system` is default-deny (ingress and egress). Then:

- **Data movers** (`crystal-mover`): egress to DNS (kube-dns, 53) and to the S3 endpoint of
  their resolved location on 443. Where the CNI supports FQDN policies, pin the endpoint
  hostname; otherwise allow egress 443 **excluding all cluster-internal CIDRs** (pods,
  services, nodes) — a compromised mover cannot pivot to cluster services (SSRF), only talk
  to the outside on 443. A **sync mover** (external sync, R28) egresses to **both** the source
  and destination location endpoints on 443 (it copies between two repositories).
- **Manifest movers** (`crystal-manifest-mover`): same as data movers **plus** egress to
  the API server; this API-server egress is granted to `crystal-manifest-mover` pods only.
- **Operator**: egress to the API server, DNS, and location endpoints (reachability probes,
  STS); ingress on 9443 (webhooks, from the API server) and the metrics port (from the
  monitoring namespace only).
- User namespaces: no policy changes required by Crystal Backup.

## 8. Admission enforcement (VAP-first)

Static, blocking rules (02-api rules 1–2 and 5–8) are enforced by **`ValidatingAdmissionPolicy`**
(CEL, in the API server) so they hold even when the operator is unavailable; the operator's
**webhook** carries only the one dynamic, cross-object rule (single-default location), scoped to
the project's CRDs with `failurePolicy: Ignore`; the retention-vs-`Immutable` advisory (rule 3) is
**controller-side** (non-blocking), not admission ([adr/0010](adr/0010-admission-vap-first.md)).
The security-relevant rules:

1. **Destructive confirmation (R23)** — VAP on `Restore`, `ClusterRestore` **and**
   `ClusterErasure`, enforced as a **conservative superset**: **every** `Recreate` **or**
   `Overwrite` requires `confirmation == <target namespace>` (VAP cannot test whether the target
   already exists, so it asks unconditionally in those modes); a `ClusterErasure` requires
   `confirmation == <target identity>`. In-API-server evaluation cannot be silently disabled by an
   operator outage — and, unlike a `failurePolicy: Fail` webhook, never wedges unrelated writes
   when the operator is down.
2. **User isolation** (rule 2) — VAP: a user-created `Backup`/`BackupSchedule` must reference a
   same-namespace `BackupLocation`, never a `ClusterBackupLocation`; a `BackupExternalSync`'s
   `sourceLocationRef` and `destinationLocationRef` are both same-namespace `BackupLocation`s
   (and must differ — rule 9); `Restore` carries no target-namespace field. (Cluster-origin
   `Backup`s read-only to users is **RBAC**, I7.)
3. **Denied namespaces** (rule 7) — VAP with a ConfigMap `paramRef`: tenant-facing CRs denied in
   the configurable deny-list (default `kube-*`, `crystal-backup-system`, and any incumbent
   backup tool's namespace).
4. **Immutable-mode** (rule 6) — VAP: `mode: Immutable` forbids `maintenance.pruneSchedule`.

Admission is a **gate, not the isolation boundary**: controllers re-derive repository identity
and the `namespace=` tag filter (I1) and re-check destructive confirmations on execution, so a
bypassed or misconfigured policy degrades UX, never tenancy. Dynamic-webhook denials are counted
in `crystalbackup_webhook_denials_total{webhook,reason}`; VAP denials surface in the API server's
own `apiserver_validating_admission_policy_check_total{policy}` metric — both dashboarded with
per-namespace context where applicable (R19), alerting on unusual rates in M6.

## 9. Cross-cluster & cross-namespace self-service restore: excluded in v1

A namespaced `Restore` names a `Backup` **in its own namespace** — no `clusterID`, no
`locationRef`, no target-namespace field — so it can only restore what discovery projected
into that namespace, i.e. the **local cluster's** snapshots for that namespace (discovery
filters by `host=<local clusterID>`). Cross-namespace and cross-cluster restore therefore
require `ClusterRestore` (admin), which addresses a **repo coordinate** (location + origin
namespace + run/time) and whose human operator *is* the identity check (R14).

Why namespaced self-service cannot cross a cluster boundary: one shared bucket may serve
several clusters (`host=<clusterID>`, R20), and namespace names are unique only within one
cluster — namespace `acme` on `prod-eu-1` and `acme` on `prod-eu-2` may be different
tenants. Discovery only projects the local cluster's snapshots, and the mediated
`namespace=` filter is scoped to the local `clusterID`'s repo path; there is no
cross-cluster tenant-identity registry in v1.

**Future sketch — `RestoreGrant`** (post-v1, not in the 02-api contract): a cluster-scoped,
admin-created CR delegating read of one foreign repo coordinate to one local namespace:

```yaml
kind: RestoreGrant            # cluster-scoped, admin-only, time-bounded
spec:
  source: { clusterID: prod-eu-2, namespace: acme, locationRef: {...} }
  grantee: { namespace: acme-dr }        # local namespace allowed to restore from source
  expiresAt: "2026-09-01T00:00:00Z"
```

A namespaced `Restore` referencing a grant would be honored iff the grant matches its
namespace and has not expired. The admin act of creating the grant asserts "same tenant"
and is itself the audit record. Two clusters must also never share a `clusterID` (Helm
value) — uniqueness is an ops-runbook check.

## 10. Manifest snapshots contain Secrets

`includeManifests: true` (default) stores the namespace's Secret objects — required for a
full namespace recovery (R15). Implications, stated plainly:

- **Repository encryption is the control, but it is not a boundary against the platform.**
  Whoever can open the repo reads those Secrets. On the shared cluster DR repo that is the
  **admin-only platform key** (§4) — equal to the platform's existing etcd-level access, but
  a more portable ciphertext; KEK custody keeps it safe at rest. Users never hold the
  cluster key; their access is the mediated `namespace=` filter, which restores their own
  Secrets back into their own namespace where those Secrets already live.
- On a user `BackupLocation`, the control is the user's own key. With `platformAccess:
  false` (default) the operator registers **no durable key slot** of its own on that repo,
  so the platform has no standing, independent way to open the user's off-platform backups
  (e.g. to read their Secrets) — the user's key governs access, and they may hold or rotate
  it themselves.
- Opt-out: `BackupSchedule`/`ClusterBackupSchedule` `manifestOptions.excludeSecretData:
  true` stores Secret manifests with `data`/`stringData` stripped and annotation
  `crystalbackup.io/secret-data-excluded: "true"`; restore recreates them empty with the
  same annotation (workloads needing them fail visibly rather than silently). Field
  reserved in [02-api.md](02-api.md); to be folded into 02-api.md and 04-manifest-backup.md
  (see Open questions).

## 11. Supply chain

- Operator and mover images: built declaratively with **apko** on a **Wolfi (glibc)** base for
  a **0-known-CVE** posture (not Alpine/musl), **signed (cosign, keyless)**, carrying an SPDX
  **SBOM**, and referenced **by digest** in the Helm chart; no user-facing CR field selects
  images ([adr/0012](adr/0012-container-images-apko-wolfi-slsa.md)).
- **SLSA L3+ build provenance** is generated in GitHub Actions (isolated builder + ephemeral OIDC
  identity → non-falsifiable provenance) and verified in the release job; a **CVE-scan gate**
  (0-known-CVE threshold) plus a **scheduled rebuild** against the rolling Wolfi apk set
  **minimize the known-CVE exposure window** between releases (base/OS CVEs are remediated by the
  rebuild; app/dependency CVEs still require a code bump).
- The `restic` binary in the mover image is version-pinned (built from source with **melange**)
  and SHA256-verified; `crystalctl` embeds the same pinned engine (ADR 0001).
- CI license gate = permissive-only dependencies (Apache-2.0/MIT/BSD — open-source readiness,
  00-requirements §5).

## 12. Audit trail

- **CR events**: controllers emit Kubernetes Events with stable reasons
  (`BackupCompleted`, `RestoreDenied`, `ConfirmationAccepted`, `RepositoryInitialized`,
  `KeySlotAdded`, `ErasureRequested`, `ErasureBlocked`, …) on the originating CR.
- **API audit**: recommend a platform audit-policy rule logging `RequestResponse` for
  group `crystalbackup.io` — captures *who* created every `Restore`, `ClusterRestore` and
  `ClusterErasure` with its full spec (the operator itself never knows the requesting user).
- **Logs/metrics**: JSON-lines logs and metrics carry the origin `namespace` label (R19);
  `ClusterRestore` and `ClusterErasure` usage is visible on the platform dashboard (M6).
- **Storage side**: enable RGW ops log on the backup bucket for forensic correlation of
  object-level access with mover Job runs — and, for erasure, to attest that `forget`+`prune`
  actually deleted the objects.

## 13. Open questions

1. STS availability on the platform RGW (I4): confirm `AssumeRole` + session-policy support
   on the RGW version before M1; otherwise start with the static per-repo-keys fallback and
   track STS as M6 hardening.
2. Per-user fair-share queueing of mover slots (DoS residual, §2) — revisit when quota work
   (post-v1, R24 note) is scheduled.
3. `manifestOptions.excludeSecretData` (§10) to be added to 02-api.md and
   04-manifest-backup.md in the same PR that specifies manifest options.
