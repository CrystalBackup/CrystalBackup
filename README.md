# Crystal Backup

> **Early, real code** — **M0 through M6 have shipped (v0.6.2)**: the core backup engine,
> cluster disaster recovery, **restore**, **manifest & cluster-scoped DR**, the
> **namespace plane** (a user's own repository under their own key), **external sync** and the
> **right to erasure** are implemented, tested and released. Every milestone is accepted on a
> **real RKE2 + Rook-Ceph cluster** before it ships, and the acceptance reports are
> [published, check by check](https://crystalbackup.github.io/CrystalBackup/quality/).
> Three milestones (M7–M9) are still ahead — [what is *not* here yet](#how-it-compares) is
> listed explicitly. Built in the open with AI assistance.
> **[Documentation](https://crystalbackup.github.io/CrystalBackup/docs/)** ·
> [Project status & disclaimer](#-project-status--disclaimer)

**Crystal Backup** is a Kubernetes operator that provides **multi-tenant,
self-service backup and restore of namespaces** — both **PVC data and Kubernetes manifests** —
across **two planes**:

- a **cluster plane** where platform administrators back up all (or selected) namespaces into
  **one shared restic repository** per location (tenancy carried by restic **tags**) for
  platform **disaster recovery**; and
- a **namespace plane** where namespace users additionally back up **their own** namespace to
  **their own object storage, with their own key**, off-platform.

Backups are stored in the plain **restic repository format**, so anyone can read their data
with upstream `restic` — **reversibility by design, no lock-in**. Discovery is
**disaster-recovery-first**: point the operator at an existing bucket and it inventories what
is restorable, with no pre-existing custom resources and no surviving cluster required.

---

## ⚠️ Project status & disclaimer

**M0 through M6 have shipped (v0.6.2)** — the core engine, cluster disaster recovery, restore,
manifest & cluster-scoped DR, the namespace plane, external sync and the right to erasure are
real, tested code, and M6 has now put instrumentation in front of all of it: a metrics
catalogue, eleven alert rules with unit tests, traces, an exportable self-check and a
restore-fidelity gate that compares a restore to its source file by file. The CRD API is
`v1alpha1` and **will still move** before `1.0.0`, and three milestones remain
([roadmap](#roadmap)).

**0.6.2 is offered for testing in real conditions, not for production.** The difference is
specific rather than rhetorical: the milestone's own exit criteria call for a two-week soak
alongside Velero and a pilot rollout, and neither has happened yet. Run it on a cluster whose
loss you can absorb, alongside — not instead of — whatever you back up with today. So:
**early, no longer hypothetical, and honest about which parts are which.**

**How each shipped milestone is verified.** Beyond unit, envtest and kind e2e suites, every
milestone is accepted on a disposable **real platform** — RKE2, Rook-Ceph (RBD + CephFS),
Longhorn, local-path, and real S3 object storage — by the
[crucible suite](test/crucible/README.md). Repository claims are checked by an **independent
`restic` oracle** (a throwaway Job running the plain upstream CLI against the same repository),
so a controller that both writes and reports the same wrong thing cannot make a check pass.
The reports are published in full, per check, pass and skip:

| Milestone | Acceptance report |
|---|---|
| M1 — core engine & cluster DR | [crucible-m1](https://crystalbackup.github.io/CrystalBackup/reports/crucible-m1.html) |
| M2 — restore | [crucible-m2](https://crystalbackup.github.io/CrystalBackup/reports/crucible-m2.html) · [0.2.1 hardening](https://crystalbackup.github.io/CrystalBackup/reports/crucible-m2.1.html) |
| M3 — manifests & cluster-scoped DR | [crucible-m3](https://crystalbackup.github.io/CrystalBackup/reports/crucible-m3.html) |
| M4 — hooks, verification & maintenance | [crucible-m4](https://crystalbackup.github.io/CrystalBackup/reports/crucible-m4.html) |
| M5 — namespace plane, sync & erasure | [crucible-m5](https://crystalbackup.github.io/CrystalBackup/reports/crucible-m5.html) |
| M6 — observability & production readiness | [crucible-m6](https://crystalbackup.github.io/CrystalBackup/reports/crucible-m6.html) — **the full 82-check suite, M0 through M6** |
| 0.6.1 — mover sizing | [crucible-m6.1](https://crystalbackup.github.io/CrystalBackup/reports/crucible-m6-1.html) — the full 82-check suite again, on the sizing defaults |
| 0.6.2 — the soak kit | [crucible-m6.2](https://crystalbackup.github.io/CrystalBackup/reports/crucible-m6-2.html) — the full 82-check suite a third time, on the release that makes the two-week soak runnable |

The reports include the defects each round found — writing the M5 suite alone turned up three
features that were documented and completely **inert** on real infrastructure. That is the point
of running them, and the reason they are published rather than summarised.

Please still treat the project accordingly:

- Try it in a **sandbox**, not on data you can't recreate — and keep your existing backups.
- **Test your restores** — good practice with any backup tool, and the practice this project
  runs on.
- Provided **"AS IS", without warranty of any kind**; you use it **at your own risk**, and the
  authors accept **no liability**. See [LICENSE](LICENSE) (Apache-2.0).

None of this is meant to scare you off — it's an honest "we're early". Specs, ADRs and the
roadmap are public and lead the code. If the direction resonates, **star or watch the repo**,
read [when *not* to choose it](https://crystalbackup.github.io/CrystalBackup/docs/discover/when-not-to-use/),
and try the shipped milestones in a sandbox.

## 🤖 Built with AI assistance

This project is **written with heavy use of AI coding assistants**, under human direction and
review. The specifications, the ADRs and the implementation are produced this way on purpose —
Crystal Backup is partly an **experiment in AI-assisted software engineering**, not only a
backup tool.

Being candid about it: AI-assisted work still benefits from **human review and real testing**
before anyone relies on it — which is exactly how it's being built, and one more reason to test
in a sandbox first.

**Supply chain.** The three images (`operator`, `mover`, `sync`) are built with
[apko](https://github.com/chainguard-dev/apko) on a **Wolfi (glibc)** base, published
**multi-arch** (`linux/amd64` + `linux/arm64`) to GHCR behind a **0-known-CVE trivy gate that
runs before the push**. The multi-arch **index** is then cosign keyless-signed, its SPDX **SBOM**
is attested, **SLSA build provenance** is attached, and an **OpenVEX** document is attested after
publish for advisories that land on an already-immutable image
([ADR 0012](spec/adr/0012-container-images-apko-wolfi-slsa.md)). Don't take that on faith —
`cosign verify` it yourself, [the commands are written down](docs/DEVELOPMENT.md#7-container-images).
The insistence is earned: for four releases the signature was attached to the **amd64 child
manifest** instead of the index, so every consumer's `cosign verify` failed while the pipeline
stayed green. Fixed in 0.5.1, with the workflow now refusing to sign anything that is not an
index. Verify artefacts, not pipelines.

## Why this project exists

Managed, multi-tenant Kubernetes platforms are typically **tenant-isolated by namespace**: a
user owns one or more namespaces and is self-service inside them via RBAC. Such platforms
commonly run a **cluster-wide backup tool** (e.g. Velero, daily, short retention) as an
**admin-only** safety net.

That leaves namespace users with:

- **no self-service** backup or restore of their own data;
- **no visibility** into whether (or what) is backed up;
- **no way to back up off-platform**, under their own key, outside the operator's trust.

A survey of existing open-source and commercial tools found that **none covers this combination**
on the discriminating axes — multi-tenant *self-service*, per-namespace *isolation*, *off-platform*
user backups, *reversibility*, snapshot *least-data-movement*, and *disaster recovery straight
from the repository*. Each tool solves part of the problem; the gaps differ. Crystal Backup
covers **that specific combination**, while **coexisting** with whatever cluster-wide backup tool
is already in place — it is **not** a "rip and replace" project.

Full requirements (R1–R28) and rationale: [spec/00-requirements.md](spec/00-requirements.md).

## What makes it different — the main design choices

- **Two planes, cert-manager style** (R2/R5) — a cluster-scoped `ClusterBackupLocation` drives
  DR into one shared repo (tenancy by restic tags `tenant`/`namespace`/`pvc`); a namespaced
  `BackupLocation` lets a user back up to **their own bucket with their own key**, *in addition*
  to cluster DR.
- **Reversibility by design** (R8) — repositories are plain **restic** repos; you can always read
  your backups with upstream `restic` given the S3 credentials and key. No proprietary catalog,
  no lock-in.
- **Admin-held root key, escrowed off-cluster** — the cluster **KEK** that unwraps the platform key
  is generated by the administrator and kept *outside* the cluster; the operator and the Helm chart
  **never mint it**. A key born inside the cluster would be lost with the cluster — and every backup
  with it — so custody of the one root secret stays deliberately in the admin's hands ([spec/03](spec/03-security-and-tenancy.md)).
- **The repository is the source of truth** (R26) — `Backup` objects are a *projection* of the
  restic repository, which survives total cluster loss. A discovery controller inventories a
  location and projects `Backup`s into existing namespaces, so `kubectl get backups` lists exactly
  what is restorable — and DR works with **no pre-existing custom resources**.
- **Server-side tenant isolation** (R2/R14) — a namespaced `Restore` is structurally confined to
  its own namespace; access to the shared DR repo is mediated by a **non-forgeable server-side
  `namespace=` tag filter**. On the namespace plane, isolation is by construction (the user's own
  bucket, credentials and key — the platform holds no key slot on it, by design).
- **Least-data-movement, Ceph-aware snapshots** (R11) — back up from a read-only snapshot with the
  cheapest path per CSI driver (CephFS shallow `backingSnapshot`, RBD copy-on-write clone); a CSI
  that cannot snapshot is **skipped with a reason in status**, never silently dropped.
- **PVC data *and* sanitized manifests** (R15) — manifests are cleaned for cross-cluster restore
  (`uid`/`resourceVersion`/`status` stripped, `clusterIP` dropped but `nodePort` preserved,
  storageClass remapping); **cluster-scoped resources** are captured too for real bare-cluster DR
  ([ADR 0011](spec/adr/0011-cluster-scoped-dr.md)).
- **Right to erasure** (R21) — `ClusterErasure` *physically* deletes a tenant / namespace / PVC
  (`restic forget --tag …` + `prune`); blocked on immutable locations until object-lock expiry.
- **Immutability as a location mode** (R18) — *designed, not shipped*: a location is `Standard`
  or `Immutable` (S3 Object Lock; no prune, retention by repository rotation). The field is
  accepted and a few guards exist around it; Object Lock support, repository rotation and expiry
  land in **M8**. Do not set `Immutable` expecting WORM today.
- **External sync to a secondary location** (R28) — replicate a repository to a second location
  via `restic copy`, **re-encrypted to the destination's own key** (independent repo, per-namespace
  selective, tenant siloing preserved) ([ADR 0013](spec/adr/0013-external-backup-sync.md)).
- **Coexistence, not replacement** (R22) — distinct API group, namespace, credentials, repositories
  and snapshot objects; runs **alongside** Velero (or any tool) without interference.
- **Hardened supply chain** — images built with **apko** on a **Wolfi (glibc)** base for a
  near-zero CVE surface, gated at 0 known CVEs before push, with the multi-arch index signed,
  an attested SBOM and SLSA build provenance ([details above](#-built-with-ai-assistance)).

## How it compares

The Crystal Backup column is **v0.6.2 as shipped** — every ✅ below is code you can install
today, and each one is exercised by the published
[acceptance reports](#-project-status--disclaimer). The other columns are those tools'
**current** capabilities to the best of our knowledge. Capabilities evolve, these tools have
**different goals**, and this is **not** a benchmark or an endorsement — verify against each
project's own docs.

Legend: ✅ yes / core goal · 🟡 partial or possible with effort · ❌ no / not a goal ·
🚧 **not shipped yet** (milestone in the cell).

| Capability | Crystal Backup *(v0.6.2)* | Velero | K8up | VolSync | Kasten K10 |
|---|:--:|:--:|:--:|:--:|:--:|
| Open source | ✅ | ✅ | ✅ | ✅ | ❌ (commercial; limited free tier) |
| Namespace-user **self-service** (own schedules/restores) | ✅ | ❌ (admin-oriented) | ✅ | 🟡 | 🟡 |
| Per-namespace **tenant isolation** (can't read others') | ✅ | ❌ | 🟡 | 🟡 | 🟡 |
| **Off-platform** backup to the user's own bucket + own key | ✅ | 🟡 | ✅ | ✅ | ❌ |
| PVC data **+ Kubernetes manifests** | ✅ | ✅ | ❌ (volume data) | ❌ (volumes) | ✅ |
| **Least-data-movement** CSI snapshots (Ceph-aware) | ✅ | ✅ | 🟡 | ✅ | ✅ |
| **DR from the repository alone** (no pre-existing CRs) | ✅ | 🟡 | ❌ | ❌ | 🟡 |
| **Reversibility** — read backups with a standard tool | ✅ (restic) | 🟡 (restic/kopia, wrapped) | ✅ (restic) | ✅ (restic option) | ❌ (proprietary catalog) |
| **Right to erasure** (physical, per tenant/ns/PVC) | ✅ | 🟡 | 🟡 | ❌ | 🟡 |
| **Coexistence** with another backup tool (stated goal) | ✅ | 🟡 | 🟡 | 🟡 | 🟡 |
| **Immutability** (S3 Object Lock) | 🚧 **M8** | 🟡 | 🟡 | ❌ | ✅ |
| Browse / file-level download **UI** | 🚧 **M7** | ❌ | 🟡 (via Backrest) | ❌ | ✅ (rich UI) |

### The line, drawn explicitly

Three things this README describes are **design, not code**, and the roadmap is the honest
answer to "when":

- **`crystalctl` and the browse UI — M7.** There is no user-facing CLI in this release. Every
  operation goes through custom resources and `kubectl`, or through upstream `restic` directly.
- **Immutable locations — M8.** `spec.mode: Immutable` is accepted by the API but S3 Object Lock,
  repository rotation and expiry are not implemented. It does not give you WORM.
- **Coexistence *hardening* — M9.** Coexistence is structural today (distinct API group,
  namespace, credentials, repositories, prefixed and labelled snapshot objects) and that is the
  ✅ above. What M9 adds is the *validated* side-by-side soak against an incumbent tool, the
  coverage-diff guidance and the fleet DR drills.

A fuller list — including the costs you accept and the cases where another tool is simply the
better answer — is on
[When not to choose it](https://crystalbackup.github.io/CrystalBackup/docs/discover/when-not-to-use/).

The one-line reading: mature tools like **Velero** excel at **admin cluster-wide DR**, and
**K8up/VolSync** at **restic/replication per namespace** — but the combination of *multi-tenant
self-service + reversibility + DR-straight-from-the-repository* is the gap Crystal Backup closes,
and all three of those are shipped.
Commercial suites like **Kasten K10** are feature-rich but proprietary (reversibility and cost are
the trade-offs). A mechanism-by-mechanism version of this comparison is in the docs:
[How it compares](https://crystalbackup.github.io/CrystalBackup/docs/discover/comparison/).

## Roadmap

Milestones are sequenced so the **core two-plane path + cluster DR** comes first; each milestone
ends releasable. Full task breakdown and Definition of Done: [spec/90-roadmap.md](spec/90-roadmap.md).

**Versioning** follows [SemVer](https://semver.org/): each milestone is a **minor** release on
major 0 (`M_n` → `0.n.z`), iterations are **patches**, and `1.0.0` is a deliberate API-stability
decision **after M9** — the CRD API is `v1alpha1` and can still move until then
([spec/adr/0014](spec/adr/0014-versioning-and-release.md)). The three images ship **multi-arch**
(`linux/amd64` + `linux/arm64`) on GHCR; when the CLI/UI arrive in M7 they will target linux,
windows and darwin on amd64 + arm64.

| Milestone | Theme | State |
|---|---|---|
| **M0** | Scaffolding — kubebuilder layout, CRD skeletons, CI (multi-arch apko/Wolfi + SLSA), test harness | shipped |
| **M1** | Core engine & cluster DR — cascade, snapshot exposers, discovery, retention, metrics | shipped |
| **M2** | Restore — self-service, operator-mediated cluster-DR restore, `ClusterRestore`, admission (VAP) | shipped |
| **M3** | Manifests & cluster-scoped DR — sanitization engine, cluster-scoped capture & selective restore | shipped |
| **M4** | Consistency hooks, repository verification (`restic check`) & maintenance | shipped |
| **M5** | Namespace plane, **external sync** & right-to-erasure | shipped |
| **M6** | Observability hardening & production readiness — metrics catalogue, dashboards, alert rules, traces, restore-fidelity gate | shipped — **v0.6.2**, current |
| **M7** | `crystalctl` CLI & local browse UI | planned |
| **M8** | Immutable locations (S3 Object Lock) | planned |
| **M9** | Coexistence hardening & DR drills | planned |

*(An editable dependency diagram of the roadmap is kept as a local planning artifact and is not
published in this repository.)*

## Architecture in one paragraph

A Go operator in `crystal-backup-system` reconciles the CRDs of both planes
(`ClusterBackupLocation`, `ClusterBackupSchedule`, `ClusterBackup`, `ClusterRestore`,
`ClusterErasure`, `ClusterBackupExternalSync`; `BackupLocation`, `BackupSchedule`, `Backup`,
`Restore`, `BackupExternalSync`), runs the schedule / backup / restore / discovery / maintenance /
external-sync controllers, orchestrates CSI `VolumeSnapshot`s, and fans out one **unprivileged
mover Job per PVC** (`crystal-mover` / `crystal-manifest-mover`) running `restic` against
`s3://<bucket>/<prefix>/<clusterID>/`. Movers run **only** in `crystal-backup-system` (never in
tenant namespaces); the shared DR repository's key never leaves that namespace. Architecture and
flows: [spec/01-architecture.md](spec/01-architecture.md); the naming & field contract:
[spec/02-api.md](spec/02-api.md); tenancy & threat model:
[spec/03-security-and-tenancy.md](spec/03-security-and-tenancy.md).

## Documentation

Start here: **[crystalbackup.github.io/CrystalBackup/docs](https://crystalbackup.github.io/CrystalBackup/docs/)**
— what it is, the two planes, requirements, install, quickstart, guides for each plane and each
operation, the API and Helm-values reference, and the DR runbook. The
[Quality page](https://crystalbackup.github.io/CrystalBackup/quality/) collects the test strategy
and every published acceptance report.

In-repo, the specification index is **[spec/README.md](spec/README.md)** — the specs and ADRs
lead the code and are worth reading if you want the reasoning rather than the instructions.

| Doc | Content |
|---|---|
| [docs/RESTORE.md](docs/RESTORE.md) | Restore guide: self-service, cluster DR, bare-cluster runbook |
| [docs/HOOKS.md](docs/HOOKS.md) | Consistency hooks: the tenant ServiceAccount the operator impersonates, and how to grant it |
| [docs/DECOMMISSION.md](docs/DECOMMISSION.md) | Runbook: retiring a repository by destroying its key, re-encrypting one after a key leak, and uninstalling the operator in the order that does not strand a namespace |
| [spec/00-requirements.md](spec/00-requirements.md) | Requirements R1–R28, personas, scope, priorities |
| [spec/01-architecture.md](spec/01-architecture.md) | Components, two-plane model, cascade, flows, concurrency |
| [spec/02-api.md](spec/02-api.md) | CRD naming & field contract, validation, RBAC |
| [spec/03-security-and-tenancy.md](spec/03-security-and-tenancy.md) | Threat model, isolation invariants, keys, supply chain |
| [spec/04-manifest-backup.md](spec/04-manifest-backup.md) | Manifest dump, sanitization rules, restore transforms |
| [spec/05-observability.md](spec/05-observability.md) | Metrics, logging, tracing, alerting |
| [spec/06-cli.md](spec/06-cli.md) | `crystalctl` standalone CLI |
| [spec/07-ui.md](spec/07-ui.md) | UI strategy |
| [spec/08-testing-and-dod.md](spec/08-testing-and-dod.md) | Test strategy, fidelity suite, Definition of Done |
| [spec/90-roadmap.md](spec/90-roadmap.md) | Milestones M0–M9, task breakdown |
| [spec/adr/](spec/adr/) | 18 Architecture Decision Records (0001–0018) |
| [test/crucible/](test/crucible/README.md) | Real-conditions e2e on Hetzner Cloud (bring your own project) — [published reports](https://crystalbackup.github.io/CrystalBackup/quality/) |
| [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) | Toolchain, build & test loop, image verification |

## License

Licensed under the **Apache License 2.0** — see [LICENSE](LICENSE). All dependencies must remain
permissive-license compatible (Apache-2.0 / MIT / BSD).
