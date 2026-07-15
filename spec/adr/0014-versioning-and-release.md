# ADR 0014 — Versioning and release scheme

Status: **Accepted** (2026-07-16, product owner + tech lead)

## Context

Crystal Backup ships as a set of co-released artifacts — the operator image, the
`crystal-mover` image, the `crystal-backup` Helm chart, and (from M7) the `crystalctl`
CLI/UI binaries. Users need a predictable, standard contract for what a version number means,
especially while the CRD API (`crystalbackup.io/v1alpha1`) is still evolving. The roadmap is
organised into milestones M0–M9 ([90-roadmap.md](../90-roadmap.md)); those milestones need a
clear mapping to released versions.

## Decision

**Follow [Semantic Versioning 2.0.0](https://semver.org/). During initial development the
project stays on major `0`; each roadmap milestone is a **minor** release, and iterations
within a milestone are **patch** releases.**

- **Milestone → minor.** `M_n` ships as **`0.n.z`**: M0 → `0.0.z` (scaffolding), M1 →
  `0.1.z`, …, M9 → `0.9.z`.
- **Iteration → patch.** Fixes and increments *within* a milestone bump the **patch** `z`
  (`0.1.0`, `0.1.1`, …).
- **`0.x` semantics (SemVer §4).** While on major 0 the public API — CRD schemas and the CLI
  contract — may change between minors; each minor documents its breaking changes. This is
  honest: the API is `v1alpha1` and deliberately not yet frozen.
- **`1.0.0` = API freeze, not a feature count.** `1.0.0` is declared when the CRD API and CLI
  contract are considered **stable**, expected **after M9** — not at any earlier milestone.
  M6 delivers a **production-usable beta** (`0.6.z`) but remains `0.x`; the term "GA" is
  avoided before `1.0.0` so we never imply an API-stability commitment we have not made.
- **CRD API version is independent of the release version.** The Kubernetes API group stays
  `crystalbackup.io/v1alpha1` until an explicit graduation to `v1beta1`/`v1` (its own
  conversion-webhook work), regardless of the `0.x`/`1.x` release number.
- **One release train.** The operator image, mover image, chart `appVersion` and `crystalctl`
  share **one** version string per release ([06-cli.md §7](../06-cli.md)); the chart's own
  `version` may bump independently for packaging-only changes.
- **Tags, registry & provenance.** Releases are git tags `vX.Y.Z`; images publish **by digest**
  to **GHCR** (`ghcr.io/crystalbackup/*`) with the release version as an added tag, as a
  **multi-arch index** (`linux/amd64` + `linux/arm64`) carrying cosign signature + SBOM + SLSA
  L3+ provenance ([adr/0012](0012-container-images-apko-wolfi-slsa.md)). CLI/UI binaries are
  cross-platform (linux/windows/darwin × amd64/arm64, [06-cli.md §7](../06-cli.md)).
  Pre-releases use SemVer pre-release suffixes (`0.1.0-rc.1`).

## Consequences

### Positive
- Standard and tooling-friendly (`go install`, Helm, Renovate/Dependabot all understand SemVer).
- The `0.x` prefix sets correct expectations — usable, but the API can still move — matching
  the `v1alpha1` reality.
- Milestone ↔ minor makes the roadmap legible as a release plan.

### Negative / costs
- Users must read minor-version changelogs for breaking changes while on `0.x`.
- "Milestone = minor" means minors are not reset for marketing: M9 is `0.9`, and `1.0.0` is a
  deliberate, separate decision.

## Alternatives considered
- **GA / `1.0.0` at M6.** Rejected: it would put the project at `1.x` mid-roadmap while the CRD
  API is still `v1alpha1` and evolving — a SemVer stability promise we cannot yet keep.
- **CalVer.** Rejected: hides the API-compatibility information users need for an operator whose
  CRDs are a contract.
- **Independent versions per artifact.** Rejected for v1: one release train is simpler to reason
  about and to support (skew policy in [06-cli.md §7](../06-cli.md)).

## Revisit triggers
- The CRD API is frozen and graduated to `v1` → cut `1.0.0` and switch to strict `1.x`
  compatibility rules.
- Artifacts need to diverge (e.g. the CLI matures faster than the operator) → reconsider
  independent version trains with a documented compatibility matrix.
