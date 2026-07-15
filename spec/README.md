# Crystal Backup — Specifications

> ⚠️ **Design / specification stage — no software yet.** These documents describe *intended*
> behaviour. The implementation is being written **with AI assistance** and, when it exists, will
> be experimental — **use it at your own risk**, and always keep independent, tested backups. Full
> disclaimer: [project README](../README.md#-project-status--disclaimer).

**Crystal Backup** is a Kubernetes operator for **multi-tenant, self-service backup and
restore of namespaces** (PVC data + Kubernetes manifests). It works across **two planes**
(cert-manager `ClusterIssuer`/`Issuer` style): a *cluster plane* for platform **disaster
recovery** — all or selected namespaces into **one shared restic repository** per location,
tenancy by restic **tags** — and a *namespace plane* for users' **off-platform** backups to
their **own** bucket with their **own** key.

Execution is a CronJob-style **cascade** — `ClusterBackupSchedule → ClusterBackup → Backup →
mover Jobs`, and `BackupSchedule → Backup` — where the **namespaced `Backup`** is the single
unit of execution *and* a projection of the restic repository, which is the **source of
truth** (a discovery controller rebuilds `Backup` objects from the repo, so DR survives total
cluster loss). API group **`crystalbackup.io/v1alpha1`**; operator namespace
**`crystal-backup-system`**; standalone CLI **`crystalctl`**.

## Reading order

| Doc | Content |
|---|---|
| [00-requirements.md](00-requirements.md) | Requirements R1–R28, personas (namespace user / platform administrator), scope, priorities |
| [01-architecture.md](01-architecture.md) | Components, two-plane repository model, the cascade, backup/restore & discovery flows, concurrency |
| [02-api.md](02-api.md) | **Naming & field contract** (v3 cascade model): CRDs across both planes, validation, RBAC |
| [03-security-and-tenancy.md](03-security-and-tenancy.md) | Threat model, isolation invariants, server-side `namespace=` tag-filter mediation, secrets & keys |
| [04-manifest-backup.md](04-manifest-backup.md) | Manifest dump, sanitization rules, restore transformations |
| [05-observability.md](05-observability.md) | Metrics catalogue (`crystalbackup_`), logging, tracing, alerting |
| [06-cli.md](06-cli.md) | `crystalctl` standalone CLI (R8) |
| [07-ui.md](07-ui.md) | UI strategy: local thick client v1, Rancher/Headlamp later |
| [08-testing-and-dod.md](08-testing-and-dod.md) | Test strategy (unit/envtest/e2e), fidelity suite, benches |
| [90-roadmap.md](90-roadmap.md) | Milestones M0–M9, task breakdown, Definition of Done |

## Decision records

Architecture Decision Records live in [adr/](adr/). Key ones:

- [0001 — Repository engine: restic format, upstream restic CLI in movers](adr/0001-repository-engine-restic-format.md)
- [0002 — Operator language: Go (kubebuilder/controller-runtime)](adr/0002-operator-language-go.md)
- [0003 — Snapshot exposure: generic CSI path first](adr/0003-snapshot-exposure-csi-generic-first.md)
- [0004 — Encryption: restic native crypto, two-tier envelope keys (platform key + user key)](adr/0004-encryption-key-management.md)
- [0005 — Immutability as a location mode](adr/0005-immutability-mode.md)
- [0006 — Coexistence with third-party backup tools (no replacement goal)](adr/0006-coexistence-with-backup-tools.md)
- [0007 — Manifest sanitization: rules-based engine in Go](adr/0007-manifest-sanitization.md)
- [0008 — UI strategy: thick client first, hosted console later](adr/0008-ui-strategy.md)
- [0009 — Shared cluster repository, tag tenancy, cascade & repository-as-source-of-truth](adr/0009-shared-cluster-repo-tag-tenancy.md)
- [0010 — Admission: ValidatingAdmissionPolicy first, webhook only for dynamic checks](adr/0010-admission-vap-first.md)
- [0011 — Cluster-scoped disaster recovery: capture & selective restore](adr/0011-cluster-scoped-dr.md)
- [0012 — Container images: apko + Wolfi base, SLSA L3+ provenance](adr/0012-container-images-apko-wolfi-slsa.md)
- [0013 — External backup synchronization to a secondary location](adr/0013-external-backup-sync.md)
- [0014 — Versioning & release scheme (SemVer; milestone → minor on major 0)](adr/0014-versioning-and-release.md)

## Research

The design is grounded in a 2026-07-11 state-of-the-art survey (why build, what to reuse).
Its conclusions are folded, in English, into the ADRs above; the raw survey material is
kept as internal project documentation and is not part of this repository.
