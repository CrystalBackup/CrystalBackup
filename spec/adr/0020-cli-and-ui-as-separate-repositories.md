# ADR 0020 — CLI and UI ship as separate repositories; the API types become an importable module

Status: **Accepted** (2026-08-02)
Amends: [adr/0008](0008-ui-strategy.md) (UI strategy — the packaging half, not the architecture)

## Context

M7 was chartered as "CLI & UI v1" and became the largest milestone in the project by stacking
**two different products** under one number. They share almost nothing that matters:

- **Different audiences.** The CLI serves an operator or a namespace user at a terminal. The UI
  serves someone who wants to look at a file tree without learning the CRD surface.
- **Different cadences.** A UI iterates far faster than a storage operator should. Tying the two
  means either the UI is held back by the operator's release discipline, or the operator inherits
  a release rhythm it does not want.
- **Different stacks and supply chains.** The operator is Go, built with apko on Wolfi with SLSA
  provenance and a 0-known-CVE gate ([adr/0012](0012-container-images-apko-wolfi-slsa.md)). An SPA
  brings a JavaScript toolchain and an npm dependency graph into that same gate.
- **Different security surfaces.** One reconciles cluster state with high privilege. The other
  renders untrusted file names in a browser.

Meanwhile this repository is already carrying a great deal: four images, a two-plane cascade, an
admission model, a manifest sanitizer, an external-sync engine, a real-infrastructure test
harness. **Bounding its complexity is a design goal in itself**, and the cheapest way to bound it
is to not add a second product to it.

Two facts make the separation cheaper than it looked when it was first considered:

1. **`internal/browse` does not exist.** It was specified in [adr/0008](0008-ui-strategy.md) as the
   package the CLI and UI would share, and M7 never started, so it was never written. The concern
   recorded at the time — that extracting the UI would force `internal/browse` into a public module
   and freeze its API earlier than planned — is moot. There is nothing to extract.
2. **Reversibility does not depend on the CLI.** R8 is already met by the repository being plain
   restic format ([adr/0001](0001-repository-engine-restic-format.md)): a user with the bucket
   credentials and the key can do everything with upstream `restic`. The CLI is therefore
   convenience, never a guarantee — which is what makes it safe to move out of the repository that
   owns the guarantees.

## Decision

### 1. The UI ships as a separate project

It lives in its own repository in the CrystalBackup GitHub organization, with its own release
cycle, its own CI and its own supply chain. No UI work happens in this repository.

The architecture decided in [adr/0008](0008-ui-strategy.md) is **not** revisited here — repositories
are opened server-side, keys and credentials never reach the browser, a cluster-origin backup is
served under a non-forgeable `namespace=` tag filter. That reasoning stands; only its packaging
changes.

### 2. The CLI ships as a separate repository too, as a `kubectl` plugin

Distributed through krew. It remains part of the product story — it is how an operator or user
performs day-to-day operations without hand-writing CRs — but it is not part of this repository.
The `crystalctl ui` subcommand of [adr/0008](0008-ui-strategy.md) is dropped: the UI is its own
project, not a subcommand of the CLI.

### 3. Both are OPTIONAL access paths, and this is a constraint on this repository

The operator and its custom resources **are** the product. The CLI and the UI are simplified ways
to interact with something that is fully usable without them.

**No capability may be reachable only through the CLI or the UI.** Every operation must be
expressible as a custom resource or an ordinary `kubectl` action against this repository's API. A
convenience layer that becomes load-bearing is no longer a convenience layer, and it would put a
guarantee in a repository that does not ship the guarantees.

This retires the M5 plan under which `crystalctl admin erase|decommission|reencrypt` would ship as
the wrappers over those operations. Erasure and decommission stay driven by the `ClusterErasure` CR
and the confirmation-gated admin path they already use; the CLI may wrap them, but the CR is the
mechanism and remains sufficient on its own.

### 4. `api/` becomes its own Go module

Both external repositories need the object definitions. They are importable today —
`api/v1alpha1` depends on nothing but `k8s.io/apimachinery` and imports nothing from `internal/`,
and the module path `github.com/CrystalBackup/CrystalBackup` matches the repository — but a single
module means an external consumer inherits the operator's whole `go.mod` requirement graph in
version selection, and its Go version floor. A krew plugin has no reason to be constrained by the
toolchain of a storage operator.

So `api/` gets its own `go.mod` as `github.com/CrystalBackup/CrystalBackup/api`, **scheduled in
M7 and before either external repository exists**. The timing is the whole point: changing an
import path once two repositories depend on it is a breaking change for both, and it costs almost
nothing today.

**The property this depends on must be defended, not merely observed**: `api/` imports only
`k8s.io/apimachinery` and never `internal/`. That holds today by luck as much as by intent, so it
gets a test that fails when it stops holding.

## Consequences

### Positive

- **The operator's complexity stops growing sideways.** M7 becomes one product's worth of work
  instead of two.
- **The UI can iterate at its own speed** without dragging a storage operator's release discipline
  behind it, and without putting an npm dependency graph inside the operator's CVE gate.
- **The extraction is nearly free right now** — no `internal/browse` to carve out, no public API to
  freeze early, and an `api/` package that is already clean.
- **The optionality constraint is a design forcing function.** Requiring every capability to be
  reachable without the CLI keeps the CRD surface honest and keeps the reversibility story (R8)
  from quietly acquiring a dependency on a binary.

### Negative / costs

- **External consumers churn on every minor while the project is on `0.x`.** The CRD contract may
  still change between minors until `1.0.0`, expected after M9
  ([adr/0014](0014-versioning-and-release.md)). Both external repositories will need to pin an
  exact version and follow. This is the real cost of the split — not the import mechanics — and it
  belongs in their READMEs rather than being discovered.
- **A multi-module repository is more awkward to operate**: `go work` for local development, two
  sets of tags, and a release step that must publish the `api` module before anything can depend on
  the new version.
- **Three repositories to keep coherent** instead of one. The optionality constraint bounds the
  damage — a stale CLI degrades to "some conveniences are missing", never to "a capability is
  unreachable" — but coherence is now a maintenance obligation rather than a compiler guarantee.
- **R9 is no longer satisfied by this repository alone.** The requirement is met by the product as a
  whole; anyone auditing this repository against R9 will not find a UI in it, and
  [00-requirements.md](../00-requirements.md) has to say where it lives.

## Alternatives considered

### A. Keep both in this repository, just reorder the milestones — **rejected**

The option considered first: leave M7 intact and move it after immutability. It costs nothing to
decide and solves nothing — the largest milestone in the project stays the largest, the npm graph
still lands inside the operator's supply chain, and the UI still releases on the operator's clock.
Reordering addresses when the problem is paid, not that it is paid.

### B. Extract the UI, keep the CLI here — **rejected**

Superficially attractive: the CLI is Go, so it fits the existing toolchain and gate. Rejected
because the CLI has the same audience and cadence mismatch as the UI, because a `kubectl` plugin is
distributed through krew rather than through this repository's release artefacts, and because
keeping it here would preserve exactly the temptation the optionality constraint exists to remove —
making a capability convenient in the CLI first and specifying the CR afterwards.

### C. Publish the types by copying them into the consuming repositories — **rejected**

Avoids the multi-module cost and is how a surprising number of projects handle it. Rejected: two
copies of an API that is explicitly still moving on `0.x` will diverge, and the divergence will be
discovered as a runtime deserialization failure rather than a compile error.

## Revisit triggers

- **`1.0.0` is cut** (expected after M9). The version-churn cost of the split largely disappears and
  the external repositories can depend on a stable contract; their pinning guidance should be
  rewritten then.
- **The optionality constraint is violated** — a capability turns out to be genuinely impractical
  without the CLI. That is a signal the CRD surface is missing something, and the fix is on this
  side, not a relaxation of the constraint.
- **The `api` module grows a dependency beyond `k8s.io/apimachinery`.** It would mean the API
  package has acquired behaviour, which is the point at which the separate-module boundary needs
  re-examining rather than silently widening.
