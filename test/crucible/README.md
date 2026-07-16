# Crucible — real-conditions e2e for Crystal Backup

The crucible provisions a **real, disposable Kubernetes platform on Hetzner
Cloud**, seeds it with tenant workloads covering the storage case matrix, and
runs milestone-labeled acceptance tests against it. Inspired by OpenStack's
[Tempest](https://docs.openstack.org/tempest/): the suite is the contract a
milestone must honor on *real* infrastructure — non-regression gate at each
delivery, and a reproducible arena for bug reports.

Anyone with a Hetzner Cloud project can run it — see
[secrets.example/](secrets.example/README.md).

> A [Claude Code skill](../../.claude/skills/crucible/SKILL.md) wraps this
> workflow — `/crucible` in a Claude session drives the same mise tasks.

## What gets built

```
                        Hetzner Cloud (fsn1, private net 10.42.0.0/16)
   ┌──────────────────────────────────────────────────────────────────┐
   │  crucible-master-1..3 (cx32)        crucible-worker-1..3 (cx42)  │
   │  ─ RKE2 servers (HA etcd)           ─ RKE2 agents                │
   │  ─ ceph MON + MGR                   ─ ceph OSD (raw 40G volume)  │
   │                                     ─ ceph MDS + RGW + toolbox   │
   │                                     ─ longhorn disks             │
   └──────────────────────────────────────────────────────────────────┘
        + S3 bucket on Hetzner Object Storage (backup target)
```

Storage classes exercised by the seed and the tests:

| class             | provisioner              | snapshots | why it's here                        |
| ----------------- | ------------------------ | --------- | ------------------------------------ |
| `ceph-block` *(default)* | rook-ceph RBD      | ✅        | main platform storage (RWO)          |
| `ceph-filesystem` | rook-ceph CephFS         | ✅        | RWX volumes                          |
| `longhorn`        | longhorn                 | ✅        | snapshot-capable CSI ≠ Ceph          |
| `local-path`      | rancher local-path       | ❌        | the "no snapshot support" skip path  |

## Prerequisites

1. [mise](https://mise.jdx.dev) — then `mise install` **in this directory**
   (opentofu, ansible-core, kubectl, helm, awscli, hcloud, jq).
2. Credentials in `<repo>/.secrets/` — layout in
   [secrets.example/](secrets.example/README.md).
3. An SSH key named `crystalbackup` registered in the Hetzner project
   (override: `TF_VAR_ssh_key_name`).

## 💶 Cost & lifetime

Defaults (3× cx32 + 3× cx42 + 3× 40 GB volumes + 6 IPv4) run **≈ €0.15/hour ≈
€3.50/day** (≈ €105/month if forgotten!). The crucible is built to be
**created, used, destroyed** — always finish with:

```sh
CONFIRM=yes mise run down    # terraform destroy
# tfstate lost? label-based fallback:
mise run nuke                # asks for typed confirmation
```

## Quickstart

```sh
cd test/crucible
mise install

mise run up      # ~15-25 min: servers -> RKE2 -> ceph/longhorn/local-path -> crystal-backup
mise run seed    # tenant namespaces + checksummed data
mise run test    # full suite        (mise run test m0  for one milestone)

CONFIRM=yes mise run down
```

`mise run` with no task lists them all. Granular phases: `mise run infra`
(tofu) → `mise run cluster` (ansible/RKE2) → `mise run components` (deploy.sh)
→ `mise run seed`. All idempotent — re-run any phase after fixing something.
`mise run status`, `mise run ssh crucible-master-1`, `mise run kubeconfig`
help while debugging.

`mise run test` ends with a **plain-language report** (verdict, per-area
checks, failures with a next step, and an interpretation) — also saved to
`artifacts/crucible-report.md`. Filter to one area with `mise run test infra`
or `mise run test m0`; add full Ginkgo output with `mise run test-verbose`.

## The test suite

Go/Ginkgo, in [tests/](tests/), build-tagged `crucible` so `go test ./...`
from the repo root never touches a live cluster. Specs carry **milestone
labels**:

| label   | asserts                                                                        |
| ------- | ------------------------------------------------------------------------------ |
| `infra` | nodes/roles, 4 storage classes, snapshot classes, Ceph `HEALTH_OK`, PVC provisioning + CSI snapshot smoke on ceph-block & longhorn, local-path (no snapshots), S3 bucket reachability |
| `m0`    | 12 CRDs `Established`, chart artifacts (namespace/PSA, deployment, RBAC, SA), live create→get→delete round-trip of every kind |

Each milestone adds a `tests/m<N>_test.go` with its own label — e.g. M1 will
assert the cascade `ClusterBackupSchedule → ClusterBackup → Backup → mover
Jobs` against the seeded namespaces, restore fidelity against the recorded
`MANIFEST.sha256` checksums, and so on. Enrich, never rewrite: old labels stay
green forever (non-regression).

> The `m0` operator-readiness spec self-skips until a released image exists on
> GHCR (the chart pins by digest; M0 predates the first release). Set
> `CRUCIBLE_EXPECT_OPERATOR_READY=true` once `v0.0.x` is published.

## The seed matrix (tenant namespaces)

| namespace  | archetype                                                                                     |
| ---------- | --------------------------------------------------------------------------------------------- |
| `c-web`    | manifests only — Deployment/Service/Ingress/ConfigMap/Secret/NetworkPolicy, **no PVC**        |
| `c-db`     | StatefulSet ×2 + `volumeClaimTemplates` on `ceph-block`, checksummed data                     |
| `c-media`  | **RWX** cephfs shared by 2 pods on different nodes + one **unmounted** block PVC              |
| `c-legacy` | PVC on `local-path` — storage **without** snapshot support                                    |
| `c-edge`   | longhorn PVC with **exotic data** (hardlinks, symlinks incl. broken, sparse, unicode, xattrs, odd perms) + a **scaled-to-zero** Deployment with a detached PVC |
| `c-empty`  | policy objects only (quota/limits/RBAC), no workload                                          |

Every data volume carries a `MANIFEST.sha256` written at seed time — future
restore tests verify integrity against it, byte for byte.

## Version pins

| what                 | where                            | pin        |
| -------------------- | -------------------------------- | ---------- |
| RKE2                 | `terraform/variables.tf`         | channel `stable` (override `rke2_version`) |
| rook chart + ceph    | `deploy/deploy.sh` + `deploy/manifests/ceph-*.yaml` | `v1.19.0` / ceph `v19.2.2` |
| longhorn chart       | `deploy/deploy.sh`               | `1.10.0`   |
| external-snapshotter | `deploy/deploy.sh`               | `v8.2.0`   |
| local-path           | `deploy/deploy.sh`               | `v0.0.30`  |
| CLI tools            | `mise.toml`                      | fuzzy      |

## Troubleshooting

- **Ceph stuck short of `HEALTH_OK`** — `mise run ssh crucible-worker-1`,
  check `/dev/sdb` exists and is raw; then
  `kubectl -n rook-ceph logs -l app=rook-ceph-operator --tail=100` and
  `kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph -s`.
- **A phase failed mid-way** — every phase is idempotent; fix and re-run it.
- **Orphaned cloud resources** (lost tfstate) — `mise run nuke` deletes everything
  labeled `project=crystalbackup-crucible`. The S3 bucket is never auto-deleted.
- **SSH refused right after `mise run infra`** — cloud-init may still be running;
  retry `mise run cluster` after a minute.
