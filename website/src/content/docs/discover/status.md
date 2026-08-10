---
title: Project status
description: What has shipped, what has not, how the project is versioned, and how it is built.
---

## Where the project is

The current release is **`v0.6.5`**. Milestones M0 through M6 have shipped: the core
backup engine, cluster disaster recovery, restore, manifest and cluster-scoped DR,
consistency hooks, repository maintenance and verification, the namespace plane, external
sync, the right to erasure, and the observability layer in front of all of it.

Three milestones remain. The CRD API is `v1alpha1` and **will still change** before
`1.0.0`. M6 was the production-hardening pass and it has shipped, but two of its own exit
criteria have not been met: a two-week soak alongside an incumbent tool, and a pilot
rollout. That is why **0.6.5 is offered for testing in real conditions, not for
production** — a narrower claim than "not hardened", and a more useful one. The honest
summary is: **early, but no longer hypothetical.** The shipped paths
are tested against real infrastructure; that is not the same as asking you to trust it
with data you cannot recreate.

## What each milestone delivers, and how it was verified

Every shipped milestone had to pass an acceptance run on a **disposable real platform** —
RKE2, Rook Ceph (RBD and CephFS), Longhorn, local-path, and real S3 object storage —
before it was released. Those reports are published in full, check by check, including the
skips and the defects each round found.

| Milestone | What shipped | Verified by |
|---|---|---|
| **M0** | Scaffolding — 12 CRDs, the Helm chart, the supply-chain pipeline, the test harness | CRDs `Established` and chart artifacts asserted live, inside every crucible run |
| **M1** | Core engine and cluster DR — the schedule → backup → per-namespace fan-out cascade, CSI snapshot exposers, discovery from the repository, retention, metrics | [Crucible M1](/CrystalBackup/reports/crucible-m1.html) — 25 passed, 0 failed |
| **M2** | Restore — self-service in a namespace, operator-mediated cluster-DR restore, `ClusterRestore`, admission policies | [Crucible M2](/CrystalBackup/reports/crucible-m2.html) — 31 passed, every restored volume byte-compared against the seed's checksum manifest · [0.2.1 hardening re-run](/CrystalBackup/reports/crucible-m2.1.html) |
| **M3** | Manifests and cluster-scoped DR — the sanitization engine, mode-aware apply, cluster-resource capture with opt-in and selective restore | [Crucible M3](/CrystalBackup/reports/crucible-m3.html) — 11 acceptance criteria · hardening audits [3.1](/CrystalBackup/reports/audit-m3.1.html) and [3.2](/CrystalBackup/reports/audit-m3.2.html) |
| **M4** | Consistency hooks, repository verification (`restic check`) and scheduled maintenance | [Crucible M4](/CrystalBackup/reports/crucible-m4.html) — the full suite run **seven independent times**, because the milestone's hardest bug was a snapshot leak that reproduced roughly one run in three. Seven lanes, zero residual snapshot objects |
| **M5** | Namespace plane, external sync, right to erasure | [Crucible M5](/CrystalBackup/reports/crucible-m5.html) — 14 acceptance criteria, plus a 60-check full-suite non-regression pass on the build that shipped |

Two things about those reports are worth knowing before you read one.

**The oracle is independent.** Every claim about a repository is checked by a throwaway Job
running the plain upstream `restic` CLI against the same repository, credentials and key
the operator used. A controller that both writes and reports the same wrong thing cannot
make a check pass.

**They record what went wrong, not only what passed.** Writing the M5 suite alone found
six defects, three of which left an advertised feature completely **inert** on real
infrastructure — a namespace-plane sync that could never complete, a `Mirror` mode that
pruned nothing, and an erasure that removed no snapshot. None was reachable without a real
repository. That is the argument for running the suite, and the reason the reports are
published rather than summarised.

## What has not shipped

| Milestone | State | What it will add |
|---|---|---|
| **M6** | shipped in `v0.6.0` | The full metrics catalogue, two Grafana dashboards and eleven alert rules watched *firing* against a real Prometheus, OTel traces across the pipeline, an exportable self-check, and a restore-fidelity gate that compares a restore to its source file by file. Mover resource tuning by operation type and the PodSecurity review moved to `0.6.1`; the soak and the pilot rollout have not happened |
| **M7** | not started | **Reach.** Backing up storage that cannot snapshot — today a PVC on `local-path`, hostPath or plain NFS is *skipped*, which leaves most small k3s/RKE2 installations with nothing — plus restoring a single file into a running application's volume, and notifications over a generic webhook for teams without a Prometheus stack |
| **M8** | not started | **Proof.** A `RestoreDrill` that restores your latest backup into a scratch namespace on a schedule, compares it file by file and reports — the machinery already exists as the project's own fidelity gate, but it is a CI test today, not something that runs at your site. Plus restore alerting: none of the twelve shipped rules watches a restore failing |
| **M9** | not started | Immutable locations. `spec.mode: Immutable` is accepted by the API, but S3 Object Lock, repository rotation and expiry are **not implemented** — it does not give you WORM |

**Two things left this list rather than moving down it.** The `crystalctl` CLI and the browse
UI are no longer milestones here: they become separate projects, the CLI as a `kubectl` plugin
and the UI as its own repository. Nothing is lost — the repository is plain restic format, so
everything remains reachable with upstream `restic`, and no capability will ever be reachable
*only* through them. But it does mean there is **no user-facing command-line tool today**, and
none is coming from this repository. And coexistence *hardening* has been retired as a
milestone because coexistence is structural and already works: distinct API group and
namespaces, no mutation of anyone else's snapshot classes, and an alert that deliberately
counts *every* tool's VolumeSnapshots, because during coexistence it is the incumbent's that
fill the shared per-volume headroom. What remained of that milestone was the soak already owed
by M6, counted a second time.

The practical consequences of those gaps — and the costs the shipped design imposes on you
whether or not the gaps close — are on
[When not to choose it](/CrystalBackup/docs/discover/when-not-to-use/), which is the page
to read if you are evaluating.

## Versioning

The project follows [SemVer](https://semver.org/). On major `0`, each milestone is a
**minor** release (`M_n` → `0.n.z`) and hardening iterations are **patches**. The CRD API
is `v1alpha1` and **can still change** — `1.0.0` is a deliberate API-stability decision
taken after M9, not a date.

One version string covers the operator image, the mover image, the sync image, the Helm
chart's `appVersion` and the future CLI.

## How it is tested

Four layers, all in the open:

- **unit and envtest** — controller behaviour against a real API server;
- **Kind end-to-end** — real mover Jobs, real CSI snapshots, real object storage, on an
  ephemeral cluster;
- **the crucible** — a real-conditions suite on provisioned cloud infrastructure with
  Rook Ceph, Longhorn and local-path storage, seeded tenant workloads, and milestone-labelled
  tests. Its reports are published on the
  [Quality page](/CrystalBackup/quality/), and anyone with a Hetzner Cloud project can run
  it themselves — [`test/crucible/`](https://github.com/CrystalBackup/CrystalBackup/tree/main/test/crucible);
- **audits** — periodic adversarial reviews of shipped milestones, also published.

The project's own record on this is worth stating: several of those audit rounds found
features that were documented and inert, and one found a supply-chain verification command
that had not worked for four releases. That is why this documentation marks unshipped
things explicitly rather than describing the design as though it were the product.

## Built with AI assistance

Crystal Backup is written with heavy use of AI coding assistants under human direction and
review. The specifications, the decision records and the implementation are produced this
way deliberately — the project is partly an experiment in AI-assisted software engineering.

Being candid about it: that is one more reason to test in a sandbox, and one more reason
the test suites above are as heavy as they are.

## Supply chain

Images are built with [apko](https://github.com/chainguard-dev/apko) on a Wolfi (glibc)
base for a near-zero known-CVE surface and published multi-arch (`linux/amd64` and
`linux/arm64`) to GHCR behind a **0-known-CVE scan gate that runs before the push**. The
multi-arch **index** is then cosign keyless-signed, its SPDX SBOM is attested, SLSA build
provenance is attached, and an OpenVEX document is attested after publish for advisories
that land on an image that is already immutable. Production manifests reference images
**by digest**, never by a floating tag.

Do not take that on faith. The
[verification commands](https://github.com/CrystalBackup/CrystalBackup/blob/main/docs/DEVELOPMENT.md#7-container-images)
are written down, and running them is the point: for four releases the signature was
attached to the **amd64 child manifest** rather than the index, so every consumer's
`cosign verify` failed while the pipeline stayed green. It was found by verifying an
artefact instead of trusting a green tick, fixed in `0.5.1`, and the workflow now refuses
to sign anything that is not an index.

Three images ship: the operator, the mover (restic), and a separate sync image (restic
plus rclone). The split is deliberate — rclone is needed only by external sync, and
keeping it a third image keeps its dependency surface off the backup and restore path.

## Licence and warranty

Apache-2.0. Provided **as is, without warranty of any kind**. You use it at your own risk
and the authors accept no liability. See [LICENSE](https://github.com/CrystalBackup/CrystalBackup/blob/main/LICENSE).

## Following along

The specifications, the eighteen architecture decision records and the roadmap are public
and lead the code:

- [Specifications](https://github.com/CrystalBackup/CrystalBackup/tree/main/spec)
- [Decision records](https://github.com/CrystalBackup/CrystalBackup/tree/main/spec/adr)
- [Roadmap](https://github.com/CrystalBackup/CrystalBackup/blob/main/spec/90-roadmap.md)
- [Quality and test reports](/CrystalBackup/quality/)
