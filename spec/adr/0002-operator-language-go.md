# ADR 0002 — Operator language: Go (kubebuilder / controller-runtime)

Status: **Accepted** (2026-07-11)

## Context

Crystal Backup needs a control-plane implementation language. The operator (running in
`crystal-backup-system`) reconciles the CRDs defined in [02-api.md](../02-api.md)
(`ClusterBackupLocation`, `BackupLocation`, `BackupSchedule`, `Backup`, `Restore`,
`ClusterRestore`, `BackupRepository`, `ClusterBackupSchedule`), orchestrates
`VolumeSnapshot`/`VolumeSnapshotContent` objects, fans out mover Jobs, serves the dynamic
admission webhook + defaulting (static validation is `ValidatingAdmissionPolicy` —
[adr/0010](0010-admission-vap-first.md)) and exposes Prometheus metrics (R19)
plus JSON logs and OpenTelemetry traces
([00-requirements.md §5](../00-requirements.md)).

Two properties of the architecture ([01-architecture.md](../01-architecture.md)) frame
the decision:

1. **The operator never touches backup data bytes.** R11/R12 already force the data
   path into per-PVC mover Jobs spread across nodes; the mover invokes the repository
   engine as a separate binary (restic CLI for v1 —
   [adr/0001](0001-repository-engine-restic-format.md)). The engine choice therefore
   does **not** constrain the operator language: the boundary between them is a
   container image and a CLI invocation.
2. **Several sibling binaries share control-plane code.** The `crystalctl` CLI (R8, with
   kubectl-style helpers when a kubeconfig is present), the future UI backend
   (`crystalctl ui`, R9, [07-ui.md](../07-ui.md)) and the operator all need the API
   types, repository-naming logic (`<prefix>/<clusterID>/`), key-wrapping
   helpers (R21) and manifest sanitization (R15) as shared packages.

The candidates evaluated (2026-07-11 "Go vs Rust" research pass) were Go with
kubebuilder/controller-runtime, and Rust with kube-rs.

### Ecosystem facts (as of July 2026)

Go / controller-runtime:

- controller-runtime v0.24.x, release cadence synchronized with each Kubernetes minor
  (v0.24 ↔ k8s v1.36). kubebuilder scaffolds multi-group, multi-CRD projects with
  namespaced and cluster-scoped kinds and CEL validation markers (`XValidation`) — we
  need both scopes and CEL rules from day 1 (02-api.md §Validation).
- **Webhook server built in** (`pkg/webhook`) including `certwatcher` (watched
  certificate rotation with dedicated metrics) and cert-manager integration scaffolded
  by kubebuilder — still needed for the dynamic single-default webhook and defaulting, even
  though most validation is `ValidatingAdmissionPolicy` ([adr/0010](0010-admission-vap-first.md)).
- **Prometheus metrics registry built in**: reconcile, workqueue, rest-client,
  leader-election and webhook latency metrics come for free; our R19 per-tenant
  metrics register into the same registry.
- **envtest** (real apiserver+etcd without a cluster) is mature and standard; it is
  the backbone of our integration test layer
  ([08-testing-and-dod.md](../08-testing-and-dod.md), M0 exit criteria).
- **Official snapshot client**: `github.com/kubernetes-csi/external-snapshotter/client/v8`
  (v8.6.0, May 2026) provides types, clientset, informers and listers that register
  directly into the controller-runtime scheme. VolumeGroupSnapshot (KEP-5013,
  v1beta2 → GA in progress) lands **first** in this Go client — relevant for future
  multi-PVC consistency groups.
- **Three reference codebases implement exactly our patterns**: Velero (CNCF Sandbox
  2026 — snapshot orchestration, static VS/VSC re-bind, hooks), VolSync (per-namespace
  movers, mover permission model) and K8up (operator invoking the restic binary in
  Jobs, in production since 2019). No production PVC-backup operator of this kind
  exists in Rust.

Rust / kube-rs:

- kube v4.0.0 (June 2026), CNCF Sandbox, ~4M downloads/month, production adopters
  (AWS, Datadog, Buoyant, Kubewarden); Stackable runs a full operator suite on
  kube-rs/operator-rs in production, including conversion webhooks (March 2026). Rust
  is therefore **credible**, not disqualified on maturity.
- But it would cost us, per the kube.rs documentation itself: a hand-rolled webhook
  **server and certificate management** (kube provides `AdmissionReview` types only —
  "a non-trivial amount of certificate management" is the project's own wording);
  hand-rolled Prometheus metrics (no registry in kube-runtime; documented as "the most
  verbose part of instrumentation"); **kopium-generated snapshot bindings** to
  regenerate and maintain ourselves (no official external-snapshotter client, and new
  APIs such as VolumeGroupSnapshot arrive late); and kube-rs/envtest v0.3.0, an FFI
  wrapper around the Go envtest born in June 2026 (5 releases, requires a Go toolchain
  plus clang at build time).
- Rust's genuine advantages (memory footprint, memory safety) matter in the **data
  path** — which lives in the movers, and the mover engine is a separate binary anyway
  (restic CLI, [adr/0001](0001-repository-engine-restic-format.md)). The operator is
  I/O-bound control logic where these advantages buy nothing user-visible.

## Decision

**The operator is written in Go, scaffolded with kubebuilder on controller-runtime.**

- API group `crystalbackup.io/v1alpha1`; CRD Go types generated with
  controller-gen; CEL validation via kubebuilder markers; admission webhooks on the
  controller-runtime webhook server with certwatcher (cert-manager or chart-generated
  certs, per M0).
- Snapshot orchestration uses the official
  `kubernetes-csi/external-snapshotter/client/v8` package registered in the manager
  scheme.
- Observability: controller-runtime metrics registry (+ our `crystalbackup_*` metrics,
  [05-observability.md](../05-observability.md)), zap JSON-lines logging via logr,
  opentelemetry-go **stable 1.x** SDK activated by standard `OTEL_*` env vars (no-op
  when unset).
- **The data plane is decoupled**: movers are Kubernetes Jobs whose image contains the
  engine binary plus a thin Go shim (structured result via termination message). The
  operator↔engine contract is the CLI surface and the restic repository format, so the
  engine can change (restic CLI → rustic CLI, cf. adr/0001 revisit triggers) without
  touching the operator language decision.
- The `crystalctl` CLI and the UI backend (`crystalctl ui`, M7) are Go binaries sharing
  packages with the operator: API types, repo-identity derivation, envelope-key
  handling, manifest sanitization, restic invocation/JSON-parsing helpers.

## Consequences

### Positive

- **Everything R23/R19/testing needs is built in**: webhook server + cert rotation,
  metrics registry, envtest. Zero pioneer infrastructure to write or maintain.
- Official, current snapshot client; first-class access to upcoming
  VolumeGroupSnapshot APIs.
- Three directly relevant open-source references (Velero, VolSync, K8up) to crib
  designs and battle-tested edge-case handling from — all Apache-2.0, compatible with
  our permissive-license constraint.
- **Single language across operator, mover shim, `crystalctl` CLI and UI backend**:
  shared packages, one build toolchain, one CI pipeline, standard hiring and
  maintenance profile for a Kubernetes shop.
- restic (our v1 engine) is also Go; reading upstream source while debugging mover
  behaviour stays in one language.

### Negative

- Higher operator memory baseline than an equivalent Rust binary (informer caches +
  Go GC). Marginal in practice: the operator is one small Deployment; the memory-heavy
  work (restic index) is in the movers, not the operator.
- Go's type system catches fewer state-machine errors at compile time than Rust's;
  we compensate with envtest coverage of controller behaviour (Definition of Done,
  [90-roadmap.md](../90-roadmap.md)).

### Risks & mitigations

- **controller-runtime API churn between k8s versions** — mitigated by its
  cadence-aligned releases and the fact that every major Go operator absorbs the same
  churn; upgrade path is well documented and community-wide.
- **Temptation to smear data-path logic into the operator** (breaking the "never
  touches backup bytes" invariant and the language-independence argument) — mitigated
  by the mover contract: all engine interaction happens in Jobs via the shim; code
  review gate in 01-architecture.md §1.
- **Team Rust interest / retention** — the door stays open in the right place: the
  mover engine and a future UI backend component may adopt Rust (rustic/rustic_core)
  without any operator change, since the boundaries are a container image and the
  restic repository format.

## Alternatives considered

### Rust operator (kube-rs 4.0) — rejected

Viable in 2026 (CNCF Sandbox, Stackable in production) but strictly more expensive for
this project: hand-rolled webhook server + certificate management, hand-rolled metrics
endpoint, kopium-generated external-snapshotter bindings to maintain (lagging new
snapshot APIs), and an envtest wrapper a few weeks old at decision time. We would be
the first production PVC-backup operator in Rust — a pioneer cost with no product
benefit, because Rust's advantages live in the data path, which is in the movers, and
the mover engine is a separate binary regardless (adr/0001). It would also split the
codebase: the CLI/UI backend would either duplicate logic in Go or drag the whole
product to Rust, where the repo-browsing story (rustic_core) is explicitly pre-1.0
with an unstable API.

### Hybrid: Go operator + Rust movers — rejected for v1

The v1 mover is `restic` CLI plus a thin shim; there is no data-path code of ours
substantial enough to justify a second toolchain. If rustic CLI replaces restic CLI in
the mover image later (adr/0001), that is an engine swap, not a language decision —
the shim can stay in Go or become trivial. Revisit only if we start writing
significant custom data-path code.

### One-language-per-component free-for-all — rejected

Maximizes hiring surface and CI complexity for a team of this size; contradicts the
shared-packages benefit between operator, `crystalctl` and the UI backend.

## Revisit triggers

None expected for the operator itself. Reconsider only if:

- a Rust-only engine embedding (`rustic_core` as a library) becomes strategic for the
  UI backend (R9) — and even then, the scope of the revisit is the **UI backend
  component**, not the operator: the operator↔mover and backend↔repository boundaries
  are format- and CLI-based, so the operator remains Go regardless;
- controller-runtime or the official snapshot client were abandoned upstream (no sign
  of this; both are core Kubernetes-ecosystem projects).
