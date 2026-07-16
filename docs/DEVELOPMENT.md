# Development guide

A hands-on reference for working on the Crystal Backup operator: the pinned toolchain, the
build/test loop, testing tiers, linting, the Helm chart, container images, and what CI runs.
For the contribution process (branch/PR flow, DoD, versioning) start with
[../CONTRIBUTING.md](../CONTRIBUTING.md); for API/controller coding conventions see
[../AGENTS.md](../AGENTS.md). The product contract is the [`spec/`](../spec/) tree.

## 1. Toolchain

The toolchain is pinned so local runs reproduce CI. Tools come from three places:

### mise (repo-pinned) — [`../mise.toml`](../mise.toml)

```bash
mise install                 # install every pinned tool
mise exec -- <cmd>           # run <cmd> with the pinned versions on PATH
mise run doc                 # serve the docs site (Astro dev server, website/)
```

| Tool | Version | Used for |
|---|---|---|
| Go | 1.26.5 | operator toolchain |
| kubebuilder | 4.15.0 | `kubebuilder create api/webhook` scaffolding |
| node / pnpm | 24.18.0 / 10.33.2 | the docs website (`website/`) |

> **Go version note.** The project targets **Go 1.26** (mise pin `1.26.5`); `go.mod` pins
> `toolchain go1.26.5` and CI resolves the version from it (`actions/setup-go` with
> `go-version-file: go.mod`). The Kubernetes 1.36 client libraries require Go ≥ 1.26, so 1.26 is a
> hard floor. Keep the mise pin and the `go.mod` `toolchain` directive consistent — with
> `GOTOOLCHAIN=auto`, any host Go ≥ 1.26.0 resolves to the pinned 1.26.5 at build time.

### Makefile-managed (installed into `./bin` on demand)

You never install these by hand — the relevant `make` target `go install`s the pinned version
into `./bin` the first time it's needed (see the `## Tool Versions` block in
[`../Makefile`](../Makefile)):

| Tool | Version | Installed by |
|---|---|---|
| controller-gen | v0.21.0 | `make manifests`, `make generate` |
| kustomize | v5.8.1 | `make build-installer`, `make deploy` |
| setup-envtest | derived from `go.mod` | `make test` (downloads the envtest apiserver/etcd) |
| golangci-lint | v2.12.2 | `make lint` — **custom-built** with the `logcheck` plugin from [`../.custom-gcl.yml`](../.custom-gcl.yml) |

### Must be on your `PATH` (not managed by the repo)

| Tool | Needed for |
|---|---|
| docker | building images, running kind |
| kind | e2e tests |
| helm | `helm lint`, installing the chart |
| apko / cosign | building & verifying container images locally (§7) |

## 2. Repository layout

Kubebuilder single-group layout (module `github.com/CrystalBackup/CrystalBackup`, API group
`crystalbackup.io/v1alpha1`). See [../AGENTS.md](../AGENTS.md) for the full map and the "never edit"
list. The short version:

```
cmd/main.go                         Manager entry point
api/v1alpha1/*_types.go             CRD schemas (+kubebuilder markers)  ← edit
api/v1alpha1/zz_generated.*.go      DeepCopy code                        ← generated, DO NOT EDIT
internal/controller/*               Reconcilers                          ← edit
internal/webhook/*                  Admission (if present)               ← edit
config/crd/bases/*                  Generated CRDs                       ← generated, DO NOT EDIT
config/rbac/role.yaml               Generated RBAC                       ← generated, DO NOT EDIT
config/samples/*                    Example CRs                          ← edit
charts/crystal-backup/              Packaged Helm chart (operator + CRDs + VAP)
spec/                               Authoritative specs & ADRs
```

The 12 CRDs (per [../spec/02-api.md](../spec/02-api.md)): cluster-scoped `ClusterBackupLocation`,
`ClusterBackupSchedule`, `ClusterBackup`, `ClusterRestore`, `ClusterErasure`,
`ClusterBackupExternalSync`; namespaced `BackupLocation`, `BackupSchedule`, `Backup`, `Restore`,
`BackupExternalSync`; plus the internal cluster-scoped `BackupRepository`.

## 3. Build & test loop

```bash
mise exec -- make manifests generate fmt vet build test
```

| Target | What it does |
|---|---|
| `make manifests` | regenerate CRDs & RBAC under `config/` from `+kubebuilder` markers |
| `make generate` | regenerate `DeepCopy` methods (`api/**/zz_generated.deepcopy.go`) |
| `make fmt` / `make vet` | `go fmt` / `go vet` |
| `make build` | compile the manager to `bin/manager` |
| `make test` | regenerate, provision envtest, run unit tests → coverage in `./cover.out` |
| `make lint` / `make lint-fix` | run / auto-fix golangci-lint |
| `make lint-config` | validate the golangci-lint config |
| `make run` | run the manager against your current kubeconfig |
| `make test-e2e` | spin up kind and run the e2e suite |

**Regenerate after touching types.** Any change to a `*_types.go` file or a `+kubebuilder`
marker requires `make manifests generate`, and the regenerated files must be committed — CI
enforces it (§6). Reproduce the CI check locally:

```bash
make manifests generate && git diff --exit-code -- api config
```

## 4. Tests

The full strategy — unit → envtest integration → e2e → metadata-fidelity — is in
[../spec/08-testing-and-dod.md](../spec/08-testing-and-dod.md). Backup software has one
unforgivable failure mode (a restore that doesn't work), so the strategy is restore-centric and
the **reversibility promise is a CI gate** checked with the upstream `restic` binary.

### Unit + integration (envtest)

```bash
mise exec -- make test
```

`make test` provisions a real apiserver + etcd via `setup-envtest` (no kubelet — Jobs don't run,
so mover outcomes are injected). It writes `./cover.out`, which the **Unit tests** workflow
uploads as the `coverage` artifact.

Per [../spec/08-testing-and-dod.md §7](../spec/08-testing-and-dod.md) the target coverage gates
are **≥ 80 %** on `internal/controller/…` and **100 %** on the sanitization-rules package (every
golden rule reached). These gates are wired into CI as those packages land; today the workflow
publishes the coverage artifact for inspection rather than failing on a threshold.

### End-to-end (kind)

```bash
mise exec -- make test-e2e
```

Runs against an **isolated** kind cluster `bs-k8s-backup-test-e2e` (created and torn down for
you) and installs the operator **from the packaged chart** `charts/crystal-backup` — the chart is
under test too, never raw manifests. Requirements & toggles:

- needs `docker` + `kind` on `PATH`;
- `KIND_CLUSTER=<name>` to use a different cluster name;
- `CERT_MANAGER_INSTALL_SKIP=true` to skip the cert-manager install;
- `KUBECTL_KUBERC=true` to re-enable kubectl kuberc (disabled by default for test isolation).

Never run e2e against a real dev/prod cluster. e2e is namespace-isolated with unique names and
must pass under `-count=2`; there are **no automatic retries** (retries hide the races we can't
afford — [../spec/08-testing-and-dod.md §7](../spec/08-testing-and-dod.md)).

### Real conditions (Hetzner crucible)

[`test/crucible/`](../test/crucible/README.md) provisions a **real disposable platform on
Hetzner Cloud** (RKE2 + rook-ceph + longhorn + local-path + S3 bucket), seeds tenant
workloads, and runs milestone-labeled Ginkgo specs (build tag `crucible`, so `make test`
never touches it):

```bash
cd test/crucible && mise install
make up seed test          # ≈ €0.15/h while it lives
make down CONFIRM=yes      # ALWAYS tear down
```

Bring your own Hetzner project — credentials layout in
[`test/crucible/secrets.example/`](../test/crucible/secrets.example/README.md).

## 5. Linting

```bash
mise exec -- make lint-config   # verify .golangci.yml
mise exec -- make lint          # build custom golangci-lint + run
mise exec -- make lint-fix      # auto-fix
```

Config: [`../.golangci.yml`](../.golangci.yml). Because [`../.custom-gcl.yml`](../.custom-gcl.yml)
is present, `make lint` first builds a **custom** golangci-lint binary bundling the
[`logcheck`](https://sigs.k8s.io/logtools) module plugin (it checks Kubernetes structured-logging
conventions — balanced key/value pairs, message style). The first `make lint` is therefore slower;
later runs reuse `./bin/golangci-lint`.

## 6. Helm chart

The operator ships as the `charts/crystal-backup` chart (CRDs + RBAC + the VAP admission policies +
the manager Deployment). Lint it exactly as CI does:

```bash
helm lint charts/crystal-backup
```

The chart references container images **by digest** — there is no user-facing image tag/field
([../spec/03-security-and-tenancy.md §11](../spec/03-security-and-tenancy.md),
[../spec/adr/0012-container-images-apko-wolfi-slsa.md](../spec/adr/0012-container-images-apko-wolfi-slsa.md)).
The chart also renders the `crystal-backup-tenant` ClusterRole; a golden-file test guards it, and
changing that golden requires an ADR (DoD item 5).

## 7. Container images

Images are a **product property**, built declaratively — see
[../spec/adr/0012-container-images-apko-wolfi-slsa.md](../spec/adr/0012-container-images-apko-wolfi-slsa.md).
Both the operator and the `crystal-mover` images are:

- built with **apko** on a **Wolfi (glibc, not musl)** base — minimal, no shell/package manager
  at runtime, reproducible; the mover's pinned `restic` is packaged from source with **melange**;
- **multi-arch** — `linux/amd64` + `linux/arm64`, assembled as one image index (no QEMU); the
  **index digest** is what gets signed/attested;
- gated at **0-known-CVE** (grype/trivy), **cosign-signed keyless**, shipped with an **SPDX SBOM**,
  and carrying **SLSA L3+ provenance**;
- published to **GHCR** `ghcr.io/crystalbackup/*` and pinned by digest.

The build/sign/scan/publish pipeline lives in the release workflow
(`.github/workflows/images.yml`, owned by the supply-chain/release track) plus a **scheduled
rebuild** that re-bakes against rolling Wolfi to shrink the CVE window. You rarely build images
locally, but you can **verify** a published one (cosign on `PATH`):

```bash
IMG=ghcr.io/crystalbackup/operator@sha256:<digest>
cosign verify "$IMG" \
  --certificate-identity-regexp '^https://github.com/CrystalBackup/CrystalBackup/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
cosign verify-attestation "$IMG" --type slsaprovenance \
  --certificate-identity-regexp '^https://github.com/CrystalBackup/CrystalBackup/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
cosign download sbom "$IMG"
```

## 8. What CI runs

| Workflow | File | Triggers | Does |
|---|---|---|---|
| Lint | [`../.github/workflows/lint.yml`](../.github/workflows/lint.yml) | push→main, PR | golangci-lint; **generated code & CRDs up-to-date** check (`make manifests generate` + `git diff`); `helm lint charts/crystal-backup` |
| Unit tests | [`../.github/workflows/test.yml`](../.github/workflows/test.yml) | push→main, PR | `make test`, uploads the `coverage` artifact |
| E2E tests | [`../.github/workflows/test-e2e.yml`](../.github/workflows/test-e2e.yml) | push→main, PR | kind-based e2e suite |
| Security | [`../.github/workflows/security.yml`](../.github/workflows/security.yml) | push→main, PR | **gitleaks over the full git history** + the pre-commit hook suite (`pre-commit run --all-files`) |
| Images / release | `.github/workflows/images.yml` | (release track) | apko multi-arch build, CVE scan, cosign sign, SBOM, SLSA provenance, GHCR publish |
| Docs site | [`../.github/workflows/deploy-pages.yml`](../.github/workflows/deploy-pages.yml) | push→main | build & deploy the Astro site (`website/`) |

All workflows start from `permissions: {}` and grant each job only `contents: read`; third-party
actions are pinned by commit SHA (with a `# vX.Y.Z` comment).

## 9. Versioning

SemVer with milestone→minor mapping on major `0`
([../spec/adr/0014-versioning-and-release.md](../spec/adr/0014-versioning-and-release.md)): milestone
`Mn` → `0.n.z`, iterations bump the patch, `appVersion` is `0.0.0` at **M0**, and `1.0.0` (API
freeze) is a deliberate post-M9 decision. The operator image, mover image, chart `appVersion`, and
`crystalctl` (from M7) share one version per release; releases are git tags `vX.Y.Z`.

## 10. Known gaps at M0

M0 is scaffolding — the operator has no reconcile logic yet ([../spec/90-roadmap.md](../spec/90-roadmap.md)).
A few CI/toolchain items intentionally trail the spec until the code they guard exists:

- **Coverage gates** (§4) are not yet enforced in CI (no `internal/controller` / sanitization
  packages yet); the artifact is published for inspection.
- The spec calls for `go test -race`; enabling `-race` in the Makefile `test` target is a follow-up
  for the Makefile owner.
- `charts/crystal-backup` is landed by the chart track; until it exists the `helm lint` job will
  report the chart as missing.
