# ADR 0019 — No scoped object-storage credentials for movers; credential handling stays backend-neutral

Status: **Accepted** (2026-08-02)

## Context

Since 0.6.0 the project has published, in
[`when-not-to-use`](../../website/src/content/docs/discover/when-not-to-use.md), a line
promising work it had not designed:

> **Repository-scoped mover credentials.** Movers currently receive the location's **root**
> object-storage credentials. A compromised mover can reach the whole bucket. Scoped,
> short-lived credentials are planned.

The intended implementation was S3 STS — `AssumeRole` or `GetFederationToken` — minting a
short-lived, policy-narrowed credential per mover Job instead of copying the location's
credential pair into every Job Secret.

The feature was re-examined during the 0.7 prioritisation, where it had been ranked highly on
the grounds that it was "the only direct contradiction between the multi-tenancy promise and
the implementation". That framing did not survive contact with the code. Three findings, in
increasing order of how much they matter.

### 1. The threat model implied by that sentence does not exist

Mover Jobs, their per-Job credential Secrets and the temporary clone PVCs all live in the
**operator namespace**, never in the tenant's — stated in the reconciler's own field
documentation ([`backup_controller.go`](../../internal/controller/backup_controller.go), the
`OperatorNamespace` field) and true on both planes: `ensureMoverCredsSecret` varies only the
namespace it *reads* the source credential from (operator namespace on the cluster plane, the
tenant's own namespace on the namespace plane) and always *writes* the per-Job Secret beside
the Job, in the operator namespace.

A namespace user has no RBAC in that namespace. They cannot read the Secret, exec into the
mover pod, or edit the Job. "A compromised mover" therefore does not describe a tenant
escaping their namespace; it describes a remote code execution inside restic or the mover shim,
triggered by attacker-controlled *file content* in a backed-up volume. That is a real risk and
a real thing to defend against — but it is not a tenancy defect, and scoping bucket credentials
is not the control that addresses it.

### 2. Scoping cannot produce tenant isolation, at any effort

This is the decisive finding, and it is a direct consequence of
[adr/0009](0009-shared-cluster-repo-tag-tenancy.md): one shared restic repository per
`[Cluster]BackupLocation`, tenancy carried by tags.

A restic repository is a content-addressed pack store with a shared index. Writing a snapshot
requires reading the index and writing into `data/`, `index/`, `snapshots/` and `locks/`, and
packs are **deduplicated across namespaces** — cross-namespace dedup being the main storage win
that justified the shared repo in the first place. There is consequently **no S3 object of
which one can say "this belongs to namespace A"**. An S3 policy has nothing to bite on.

**The specification already said this.** Invariant I4 in
[03-security-and-tenancy.md §3](../03-security-and-tenancy.md) scopes the planned session policy
to the repository prefix `<bucket>/<prefix>/<clusterID>/*` and states, in its own words, that it
is "**not** per namespace, because the shared repo is content-addressed and dedup-shared" — a
direct consequence of invariant I2. The error was never in the spec; it was in the published
page, which compressed a prefix-scoping invariant into a sentence readers could only take as a
tenancy fix. That distinction matters for what this ADR is: not a discovery, but a decision to
stop carrying a design whose honest description no longer justifies its cost.

Tenant-scoped credentials would require per-tenant repository sharding — deferred in
[adr/0009](0009-shared-cluster-repo-tag-tenancy.md) and out of scope in
[00-requirements.md §6](../00-requirements.md) — and would cost exactly the dedup the shared
repo exists to buy.

What scoping *could* still deliver on S3 is what I4 actually specified: a prefix-bounded token
(blocking reach to other clusters' repositories, to unrelated data on the same account, and to
the wrapped-DEK escrow object that sits beside the repository prefix), a short-lived token
(bounding the window of a leaked credential), and a read-only token for restore, discovery and
`check`. Useful hardening. Not tenant isolation.

### 3. STS is an S3 mechanism, and building on it would bake S3 into the credential layer

The project is S3-only today, and the coupling is structural rather than incidental:
`ClusterBackupLocationSpec.S3` is a required `S3Spec` with no backend union or discriminator;
`S3Spec` requires `endpoint` and `bucket`; the repository URL is always built as `s3:…`; and
the CEL rules that make repository identity immutable name `self.s3.endpoint`,
`self.s3.bucket` and `self.s3.prefix` field by field.

That is a constraint the project may want to lift. restic supports `sftp:`, `rest:`, `local:`
(the honest answer to "a repository on NFS" — a filesystem backend over a mounted export) and,
through `rclone:`, roughly everything rclone reaches. The small-installation audience that
motivates the degraded no-snapshot mode is the same audience most likely to have an SSH target
or an NFS export rather than an object store. And part of the machinery already exists: the
external-sync work ([adr/0013](0013-external-backup-sync.md)) built `rclone:` repository URLs
and per-remote `RCLONE_CONFIG_<REMOTE>_*` configuration in `internal/mover`, deliberately
restricted to `type = s3`. Widening that restriction is a far cheaper door to other backends
than adding restic backends one by one.

**Scoping does not generalise across those backends — it changes meaning entirely.** On S3 with
STS it is a session policy. On `sftp:` there is no such concept: confinement is a restricted key
and a chroot, configured on the server, outside the operator's reach. On a `rest:` server the
equivalent is `--append-only`, which is *better* than anything STS offers for the operation-scoping
case and is native (already noted for immutability in [adr/0005](0005-immutability-mode.md)).
On `local:` it is filesystem permissions.

So "scoped credentials" is not a feature. It is a per-backend matrix, and committing to the STS
shape now would put S3-specific concepts — an STS endpoint field on the location, role ARNs,
`AWS_SESSION_TOKEN` handling, a credential-minting step in the Job build path — into precisely
the layer that must stay neutral for any of those backends to be added later.

## Decision

**1. No scoped or short-lived object-storage credential feature.** Mover Jobs continue to
receive the location's credentials as supplied, via the existing per-Job Secret.

**2. The credential path stays backend-neutral.** No STS endpoint or role field on `S3Spec`, no
`AWS_SESSION_TOKEN` plumbing, no credential-minting step in the Job build path. The mover
consumes an opaque credential set from a Secret; what a credential *is* remains the backend's
business. This is the property that keeps `sftp:`, `rest:` and `rclone:` reachable later
without unwinding an S3-shaped abstraction first.

**3. Invariant I4 is retired**, not merely left unimplemented. It is rewritten in
[03-security-and-tenancy.md §3](../03-security-and-tenancy.md) to state the standing property —
movers hold credentials with full repository access, confined by no-ServiceAccount-token,
NetworkPolicy egress and Job lifetime — and open question §13 Q1 (STS availability on the
platform RGW) is closed by this decision. An invariant that describes an unbuilt future is a
worse artefact than one that describes the present accurately.

**4. The documentation states the actual position** instead of promising a remedy that would be
partial, backend-dependent and — on the point readers care about — ineffective. It records
that movers run in the operator namespace and are out of tenant reach; that the shared
repository requires full repository access by construction; and that a compromised mover's
blast radius is the repository, bounded by egress confinement rather than by credential scope.

### What remains as the actual controls

- **NetworkPolicy egress confinement** on the operator namespace, on by default.
- **Per-Job credential Secrets**, created with the Job and deleted with it (`deleteJobAndSecret`).
  Worth naming precisely: the *Kubernetes object* is already short-lived; only the underlying
  credential is long-lived. If short-lived credentials are ever revisited, the lifecycle around
  them is already the right shape and only the minting step is missing.
- **Repository key custody** — the repository key is admin-only; tenant access to
  cluster-origin data is always operator-mediated behind a non-forgeable `namespace=` tag
  filter ([adr/0009](0009-shared-cluster-repo-tag-tenancy.md)).

## Consequences

### Positive

- **A promise the project cannot keep is withdrawn** rather than carried forward. The published
  wording implied that scoping would fix cross-tenant exposure; nothing built on the shared-repo
  model could have delivered that.
- **The credential layer stays free of S3 concepts**, which is the precondition for opening
  `sftp:` / `rest:` / `rclone:` backends without an unwinding step first.
- **No dependency on uneven vendor support.** STS coverage across the S3-compatible ecosystem is
  inconsistent: AWS and MinIO implement it, Ceph RGW implements it but only when explicitly
  configured, and several targets — including the SeaweedFS backend the e2e harness runs on —
  offer no usable equivalent. Every one of those would have needed a documented degradation path.
- **Effort returns to work with a larger effect**, notably the degraded no-snapshot mode and
  automated restore verification.

### Negative / costs

- **A leaked location credential stays valid until rotated by hand.** There is no automatic
  expiry, and rotation is an operator procedure, not an operator-controller behaviour.
- **A compromised mover still reaches the whole repository** — and, if the credential carries
  wider rights than the repository prefix, whatever else those rights cover. Bounding that is
  now explicitly the deployer's job: supply a credential already scoped to the bucket or prefix.
  The documentation says so instead of implying the operator will handle it.
- **The prefix-scoping and read-only-token wins are forgone too**, not just the tenant-isolation
  claim that was never achievable. They were the defensible part of the proposal.
- **The wrapped-DEK escrow object stays reachable by a mover credential.** It sits at
  `<prefix>/<clusterID>.crystal-meta/wrapped-dek.age`, deliberately outside the repository
  prefix a scoped token would have been bounded to. Its protection therefore rests permanently
  on the KEK — the object is ciphertext useless without it — rather than on the S3 path being
  unreachable. That was always the situation in practice; this decision makes it the designed
  one, and [03-security-and-tenancy.md §5](../03-security-and-tenancy.md) is amended to stop
  describing it as temporary.
- **`when-not-to-use` loses a line that read as a roadmap commitment**, which some readers may
  have been counting on. Stating the real constraint is the honest trade.

### Risks & mitigations

| Risk | Mitigation |
|---|---|
| Readers infer that mover compromise is unbounded | Documentation states the actual blast radius (the repository) and the actual controls (egress confinement, deployer-supplied scoped credentials) |
| Deployers hand the operator an over-privileged account | Documented as a deployment responsibility, with prefix/bucket scoping recommended at credential creation |
| The decision is read as "credential hygiene does not matter" | It is scoped narrowly: no *operator-minted* scoping. Deployer-side scoping is encouraged and works today |

## Alternatives considered

### A. Implement STS on S3, document degradation elsewhere — **rejected**

The shape originally planned: probe the backend at location reconcile (a `GetFederationToken`
call distinguishes "not implemented" from "denied" from "supported" by error code), mint a
per-Job token when available, fall back to the raw credential with a condition on the location
when not.

The probe design is sound and the tri-state condition would have been genuinely useful — a
deployer would at last *see* which of their locations run on raw credentials. It is rejected
anyway because what it buys on the isolation question is nothing (finding 2), and what it costs
is S3-specific fields and minting logic in the credential path (finding 3). The visibility
benefit does not require STS: the documentation change delivers it for every deployment at once.

### B. Shard the repository per tenant so scoping has something to bite on — **rejected here**

This is the only construction in which tenant-scoped credentials are meaningful. It is a
much larger decision than credential handling — it trades away cross-namespace deduplication
and multiplies init, maintenance and discovery by the number of tenants. It remains deferred
under [adr/0009 Alternative A](0009-shared-cluster-repo-tag-tenancy.md) with its own revisit
triggers, and is not re-litigated by this ADR.

### C. Adopt `rest:` with `--append-only` as the operation-scoping mechanism — **not now, but noted**

A rest-server in append-only mode gives, natively, the write-restriction that STS session
policies would have approximated: backup can add, and cannot delete. It is already on the table
for immutability ([adr/0005](0005-immutability-mode.md)). It is not adopted here because it
presupposes the multi-backend work this ADR is preserving the *option* for, rather than
scheduling. If that work happens, this is where the operation-scoping question should be
reopened — on a backend that answers it properly, rather than on S3 where it does not.

## Revisit triggers

- **Per-tenant repository sharding is adopted** ([adr/0009](0009-shared-cluster-repo-tag-tenancy.md)
  Alternative A). Tenant-scoped credentials become meaningful the moment a tenant's data occupies
  its own repository, and this ADR should be reopened in the same change.
- **A non-S3 backend is added.** If `rest:` with `--append-only` becomes reachable, the
  operation-scoping half of the question returns on a backend where it has a native answer.
- **A compliance or contractual requirement demands credential expiry** independently of
  isolation — for instance a policy forbidding long-lived static object-storage keys. The
  per-Job Secret lifecycle already fits; only the minting step would be missing, so the cost
  is bounded and known.
- **An incident where mover compromise is the demonstrated vector.** The relevant response is
  most likely mover hardening and egress controls rather than credential scoping, but the
  balance of this decision should be re-examined against real evidence rather than reasoning.
