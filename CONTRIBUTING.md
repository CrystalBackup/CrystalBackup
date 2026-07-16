# Contributing to Crystal Backup

Thanks for your interest in Crystal Backup — a Kubernetes backup & DR operator built on the
restic repository format. This guide covers how to get set up, the local development loop, and
the branch/PR conventions we follow. For a deeper tour of the toolchain, envtest/e2e, and CI,
see [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md).

> The authoritative product contract lives under [`spec/`](spec/). When code and prose disagree,
> the spec wins — start with [spec/02-api.md](spec/02-api.md) (the naming & API contract) and
> [spec/08-testing-and-dod.md](spec/08-testing-and-dod.md) (the Definition of Done).

## Prerequisites

The project pins its toolchain with [mise](https://mise.jdx.dev/) (see
[`mise.toml`](mise.toml)). From a clone:

```bash
mise install          # installs the pinned Go, kubebuilder, node, pnpm
```

| Tool | Provided by | Notes |
|---|---|---|
| Go **1.25** | mise (`go = "1.25.6"`) | the operator toolchain |
| kubebuilder 4.15 | mise | scaffolding CLI |
| controller-gen, kustomize, setup-envtest, golangci-lint | the Makefile (`go install` into `./bin`) | pinned in the Makefile; installed on demand, no manual step |
| **docker**, **kind**, **helm** | must be on your `PATH` | e2e + chart lint |
| **apko**, **cosign** | must be on your `PATH` | only needed to build/verify container images locally (see below) |

You do **not** install controller-gen / kustomize / golangci-lint yourself — the relevant
`make` targets download the pinned versions into `./bin` (`make lint` even builds a *custom*
golangci-lint binary with the `logcheck` plugin from [`.custom-gcl.yml`](.custom-gcl.yml)).

## The development loop

Everything goes through the Makefile so local runs match CI. Run tools through `mise exec` so
you get the pinned versions:

```bash
mise exec -- make manifests generate fmt vet build test
```

What each target does:

- `manifests` — regenerate CRDs & RBAC (`config/…`) from the `+kubebuilder` markers.
- `generate` — regenerate `DeepCopy` code (`api/**/zz_generated.deepcopy.go`).
- `fmt` / `vet` — `go fmt` / `go vet`.
- `build` — compile the manager binary.
- `test` — regenerate, provision the envtest apiserver/etcd binaries, and run the unit suite
  with coverage written to `./cover.out`.

Fast inner loop while editing controllers:

```bash
mise exec -- make lint-fix   # auto-fix style
mise exec -- make test       # unit + envtest
```

### Regenerated code must be committed

`api/**/zz_generated.*` and `config/crd/bases/*` are **generated** — never hand-edit them (nor
`config/rbac/role.yaml`, `config/webhook/manifests.yaml`, or `PROJECT`). After changing any
`*_types.go` or `+kubebuilder` marker, run `make manifests generate` and commit the result.
CI fails the PR if the tree is out of date:

```bash
make manifests generate && git diff --exit-code -- api config
```

See [AGENTS.md](AGENTS.md) for the full API/controller/logging conventions (status via
`metav1.Condition`, idempotent reconciliation, Kubernetes log message style, etc.).

### End-to-end tests

e2e runs against a throwaway [kind](https://kind.sigs.k8s.io/) cluster (needs docker + kind on
your `PATH`) and installs the operator from the packaged chart `charts/crystal-backup`:

```bash
mise exec -- make test-e2e
```

It creates and tears down the `bs-k8s-backup-test-e2e` kind cluster. Never point it at a real
cluster. Useful toggles: `CERT_MANAGER_INSTALL_SKIP=true`, `KIND_CLUSTER=<name>`. More detail and
the full test pyramid (unit → envtest integration → e2e → fidelity) is in
[docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) and [spec/08-testing-and-dod.md](spec/08-testing-and-dod.md).

## Branch & pull-request flow

1. Branch off `main` (e.g. `feat/backup-schedule`, `fix/discovery-projection`).
2. Make focused commits with clear, imperative subjects.
3. Keep generated code committed and the spec in sync (see below).
4. Open a PR against `main`. All required checks must be green:
   - **Lint** ([`.github/workflows/lint.yml`](.github/workflows/lint.yml)) — golangci-lint,
     the "generated code & CRDs up to date" check, and `helm lint charts/crystal-backup`.
   - **Unit tests** ([`.github/workflows/test.yml`](.github/workflows/test.yml)) — `make test`
     with a coverage artifact.
   - **E2E** ([`.github/workflows/test-e2e.yml`](.github/workflows/test-e2e.yml)) — kind suite.
5. Satisfy the **Definition of Done** ([spec/08-testing-and-dod.md §8](spec/08-testing-and-dod.md)) —
   it applies to every task. Highlights:
   - Unit tests for new logic; envtest for controller behaviour; e2e when you touch the data path.
   - **CRD/API change** ⇒ validation (VAP/CEL-first) + regenerated docs + **`spec/02-api.md`
     updated in the same PR** (CI enforces a `config/crd/` ↔ `spec/02-api.md` consistency check).
   - Anything touching **credentials, keys, or cross-namespace/tenancy logic** gets a two-person
     security review (CODEOWNERS enforces it); no widening of the tenant RBAC without an ADR.
   - A **CHANGELOG entry** unless the PR is labelled `no-changelog`.

## Versioning & releases

Crystal Backup follows [Semantic Versioning](https://semver.org/) with a milestone-driven scheme —
see [spec/adr/0014-versioning-and-release.md](spec/adr/0014-versioning-and-release.md):

- Development stays on **major `0`**; each roadmap milestone `Mn` ships as the **minor** `0.n.z`
  (M0 → `0.0.z` scaffolding, M1 → `0.1.z`, …), and iterations within a milestone bump the **patch**.
- On `0.x` the CRD API (`crystalbackup.io/v1alpha1`) may change between minors — read the changelog.
- **`1.0.0` = API freeze**, a deliberate decision expected **after M9**, not a feature count.
- One release train: the operator image, the `crystal-mover` image, the chart `appVersion`, and
  (from M7) `crystalctl` share one version string; releases are git tags `vX.Y.Z`.

## Container images (supply-chain policy)

Images are a **product property**, not a build detail — see
[spec/adr/0012-container-images-apko-wolfi-slsa.md](spec/adr/0012-container-images-apko-wolfi-slsa.md).
The release pipeline builds every image (operator + mover) with **apko on a Wolfi (glibc) base**,
and each published image is:

- **multi-arch** — `linux/amd64` + `linux/arm64` (one signed index digest covers both);
- **0-known-CVE** — a grype/trivy scan gates release (documented, time-boxed exceptions only);
- **cosign-signed (keyless)** with an **SPDX SBOM** and **SLSA L3+ provenance**;
- published to **GHCR** (`ghcr.io/crystalbackup/*`) and referenced **by digest** in the chart —
  no floating tags, no user-facing image field.

You normally don't build these locally; the release workflow does. If you do need to (apko/cosign
on your `PATH`), see [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md).

## Reporting issues

Please include the operator version (or commit), Kubernetes version, and — for data-path issues —
the relevant CR YAML (redacted) and operator logs (JSON lines). Do **not** file security-sensitive
reports as public issues — use GitHub's private vulnerability reporting (or the process in
`SECURITY.md` once it lands).
