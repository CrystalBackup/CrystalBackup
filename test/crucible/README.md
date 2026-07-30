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
                        Hetzner Cloud (fsn1, private net 10.10.0.0/16)
   ┌──────────────────────────────────────────────────────────────────┐
   │  crucible-master-1..3 (cpx32)       crucible-worker-1..3 (cpx42)  │
   │  ─ RKE2 servers (HA etcd)           ─ RKE2 agents                │
   │  ─ ceph MON + MGR                   ─ ceph OSD (raw 40G volume)  │
   │                                     ─ ceph MDS + toolbox         │
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

Defaults (3× cpx32 + 3× cpx42 + 3× 40 GB volumes + 6 IPv4) run **≈ €0.52/hour ≈
€12.5/day** (≈ €370/month if forgotten!) — a ~2 h validation session is about €1.
The cheaper Intel `cx` line and the ARM `cax` line aren't creatable in fsn1 today
(`hcloud datacenter describe fsn1-dc14` → `server_types.available`); override
`TF_VAR_master_type` / `TF_VAR_worker_type` if your location offers something
cheaper. The crucible is built to be **created, used, destroyed** — always finish
with:

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
| `m1`    | the cluster-DR cascade `ClusterBackupSchedule → ClusterBackup → Backup → mover Jobs` over the seeded namespaces, discovery from the repository, shared-repository lifecycle, retention, same-named PVCs across namespaces not colliding, convergence with no orphaned snapshots, and an **off-cluster restore** driven by upstream `restic` alone |
| `m2`    | self-service `Restore` (modes × selection × mediation), operator-mediated `ClusterRestore` reconstituting a deleted namespace, admission policies; every restored volume byte-compared against `MANIFEST.sha256` |
| `m3`    | manifest backup/restore round-trip, the sanitization engine and mode-aware apply, cluster-scoped capture with opt-in + selective restore, DR bootstrap into a fresh namespace |
| `m4`    | repository maintenance and verification — prune vs. a live backup, a prune killed mid-flight, a silently corrupted pack caught by `restic check`, and crash-only teardown via terminal re-entry |
| `m5`    | the namespace plane, external sync (deployment + queue behaviour), and the right to erasure |
| `m6`    | the **restore-fidelity gate** — a corpus engineered to be hard to restore faithfully, backed up from a Rook-Ceph RBD volume and restored into a *fresh* one, then compared per file by manifest: content digests (with 16 MiB-window digests naming a corrupted byte range), modes and setuid/setgid/sticky, numeric ownership, xattrs, POSIX ACLs incl. a directory's default ACL, sparseness, symlinks, hard links, nanosecond mtimes, FIFOs, hostile file names, deep trees |

Each milestone adds a `tests/m<N>_*_test.go` carrying its own label. Enrich,
never rewrite: old labels stay green forever (non-regression), which is why
`mise run test` with no argument is the real gate and a single label is only a
debugging shortcut.

### The restore-fidelity gate (`m6`)

`m6` is the beta bar for `0.6`, and it is written to a stricter rule than the rest
of the suite: **it cannot self-disable**. No enable flag, no conditional `Skip()`,
no tunable tolerance. Missing S3 credentials, a missing `getfattr`, a container log
that came back truncated — each of those *fails* the run rather than quietly
measuring less. A backup tool that has not proven its restores is worth nothing,
and a gate that skips itself reads as a pass in every summary a human looks at.

Two consequences worth knowing before reading a red `m6` line:

- **It restores into a volume that did not exist.** The scenario drives a
  `ClusterRestore` into a namespace created on the spot, so the PVC comes from the
  snapshot's own `pvcsize`/`pvcclass` tags and the filesystem is fresh. Restoring
  in place over the source would make the whole comparison dishonest: a mode, an
  xattr or an ACL the restore failed to re-apply would still be sitting on the
  pre-existing file, and the diff would come back green.
- **A broken facet stays in the corpus.** Each property (content, permissions,
  xattrs, ACLs, timestamps, symlinks, hard links, sparseness, presence) is asserted
  by its own spec, so one regression is one red line next to the facets that still
  hold. In particular the content facet is aimed squarely at
  [restic#5543](https://github.com/restic/restic/issues/5543) — deterministic
  corruption at an offset inside a file when restoring large data sets to
  Rook-Ceph, open upstream — and the failure message names the differing 16 MiB
  window. If it fires, the fix is upstream, not a smaller corpus.

Deliberately *not* measured, so that nobody re-discovers it as a gap: `atime`
(reading the corpus to measure it destroys it), `ctime` (no interface sets it),
`trusted.*` xattrs (documented as not restored — they need `CAP_SYS_ADMIN`), and
device nodes (whether one can exist in a tenant PVC depends on the kubelet's mount
options, not on the backup).

> **The `m0` operator-readiness check runs unconditionally** (since M6). It used to
> self-skip unless `CRUCIBLE_EXPECT_OPERATOR_READY` was set to exactly `true` — a
> guard from the days before any image existed on GHCR. Releases have existed since
> `v0.1.0`, but nothing ever set it: the only place that documented the variable
> proposed `=1`, which the comparison rejects. So "is the operator `Available`"
> was skipped in **every published run**, M1 through the M4 seven-lane fanout,
> behind a message that still said "pre-v0.0.1" — and a skip reads as a pass in
> every summary anyone actually looks at. The variable is gone; if the operator
> does not come up, the run now fails, which is the single most useful thing a
> real-infrastructure run can tell you.

## The seed matrix (tenant namespaces)

| namespace  | archetype                                                                                     |
| ---------- | --------------------------------------------------------------------------------------------- |
| `c-web`    | manifests only — Deployment/Service/Ingress/ConfigMap/Secret/NetworkPolicy, **no PVC**        |
| `c-db`     | StatefulSet ×2 + `volumeClaimTemplates` on `ceph-block`, checksummed data                     |
| `c-media`  | **RWX** cephfs shared by 2 pods on different nodes + one **unmounted** block PVC              |
| `c-legacy` | PVC on `local-path` — storage **without** snapshot support                                    |
| `c-edge`   | longhorn PVC with **exotic data** (hardlinks, symlinks incl. broken, sparse, unicode, xattrs, odd perms) + a **scaled-to-zero** Deployment with a detached PVC |
| `c-empty`  | policy objects only (quota/limits/RBAC), no workload                                          |

Every data volume carries a `MANIFEST.sha256` written at seed time — the `m1`
and `m2` restore specs verify integrity against it, byte for byte.

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
