# Roadmap — milestones & task breakdown

Status: agreed direction (two-plane cascade rework, 2026-07-12); estimates refined per
milestone kickoff. Naming contract: [02-api.md](02-api.md); model rationale:
[adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md).
Priorities per [00-requirements.md §7](00-requirements.md): core two-plane path (incl.
**cluster DR** + discovery) → namespace-plane locations/keys + **external sync** → **CLI + UI
together** (lower priority; R8 reversibility is already met by the repo being standard restic) →
immutable mode. Cluster DR is now **core** (M1), no longer deferred; coexistence with other
backup tools is a standing guarantee, **not** a replacement project
([adr/0006](adr/0006-coexistence-with-backup-tools.md)).

Each milestone ends **releasable**: tagged image + Helm chart, e2e suite green, docs
updated. Definition of Done at the bottom applies to every task. Lessons folded in from
upstream backup-operator GitHub issues are tagged `delta N`.

**Versioning** ([adr/0014](adr/0014-versioning-and-release.md)): [SemVer 2.0.0](https://semver.org/).
Each milestone is a **minor** release on major 0 — `M_n` → **`0.n.z`** (M0 → `0.0.z`, M1 →
`0.1.z`, … M9 → `0.9.z`); iterations *within* a milestone bump the **patch** `z`. While on `0.x`
the CRD/CLI contract may still change between minors. **`1.0.0` is a deliberate API-stability
decision expected after M9** — not any milestone's "GA"; M6 reaches a production-usable **beta**
but stays `0.6.z`. Images publish to **GHCR** (`ghcr.io/crystalbackup`) as multi-arch
(`linux/amd64` + `linux/arm64`) indexes, signed with SLSA provenance
([adr/0012](adr/0012-container-images-apko-wolfi-slsa.md)).

## M0 — Project scaffolding (foundation)

- [ ] kubebuilder project layout, API group `crystalbackup.io/v1alpha1`; CRD skeletons for
      the **full cascade set** — cluster plane `ClusterBackupLocation`,
      `ClusterBackupSchedule`, `ClusterBackup`, `ClusterRestore`, `ClusterErasure`,
      `ClusterBackupExternalSync`; namespace plane `BackupLocation`, `BackupSchedule`, `Backup`,
      `Restore`, `BackupExternalSync`; internal `BackupRepository` — deepcopy/CRD generation,
      `make` targets ([02-api.md](02-api.md)).
- [ ] CI (GitHub Actions): lint (golangci-lint), unit tests + coverage gate, **multi-arch
      (`linux/amd64` + `linux/arm64`) image build with apko on Wolfi (glibc, 0-known-CVE) +
      melange-built restic + SBOM + cosign sign + SLSA L3+ provenance + container CVE-scan gate,
      published to GHCR** ([adr/0012](adr/0012-container-images-apko-wolfi-slsa.md)),
      chart packaging (`crystal-backup`), e2e stage skeleton.
- [ ] Observability plumbing: zap JSON-lines on stdout, controller-runtime metrics endpoint
      (`crystalbackup_*`), OTel SDK wired behind `OTEL_*` env vars (no-op when unset).
- [ ] envtest harness + kind-based e2e harness with csi-driver-hostpath (snapshot support) +
      SeaweedFS (S3 test backend); make target `make e2e`; label-filtered informers scaffolded (delta 10).
- [ ] Helm chart skeleton `crystal-backup` (operator Deployment in `crystal-backup-system`,
      RBAC, webhook certs via cert-manager or chart-generated); dashboards path
      `charts/crystal-backup/dashboards/`.

**Exit criteria**: `make test && make e2e` green in CI on an empty-logic operator; JSON logs
and `/metrics` verified; every CRD installs and round-trips; **multi-arch images
(`linux/amd64` + `linux/arm64`) build via apko (Wolfi), signed + SBOM + 0-known-CVE gate green +
SLSA L3+ provenance attested, pushed to GHCR**.

## M1 — Core engine & cluster DR (R1, R2 partial, R11, R12, R13, R20, R24 partial, R25, R26)

Cluster disaster recovery is **core**: the cascade writes all/selected namespaces into ONE
shared restic repository, and discovery makes them restorable with no prior CRs.

- [ ] `ClusterBackupLocation` controller: S3 reachability probe, single-default election,
      conditions; **one shared repo** at `s3://<bucket>/<prefix>/<clusterID>/`.
- [ ] `BackupRepository` provisioning: lazy `restic init` **serialized through the per-repo
      exclusive queue** (init-race fix — delta 2, cf. K8up #1055); **one platform DEK**
      (random, wrapped by the cluster KEK, age X25519) stored as a `crystal-dek-*` Secret in
      `crystal-backup-system` — no per-namespace DEK ([adr/0004](adr/0004-encryption-key-management.md)).
- [ ] Cascade controllers: `ClusterBackupSchedule` → `ClusterBackup` (a run) → **fan-out** a
      `Backup` into each matching namespace (label `crystalbackup.io/cluster-backup`) →
      per-PVC mover Jobs; `ClusterBackup.status` = aggregate counters + **capped failures**
      (no `perNamespace` map — [adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md));
      run-history limits + GC (delta 10).
- [ ] Snapshot orchestration (R11): per-PVC **exposer auto-selection** by CSI
      ([adr/0003](adr/0003-snapshot-exposure-csi-generic-first.md)) — `cephfs-shallow` (ROX
      `backingSnapshot`, zero copy) for CephFS, `csi-generic` (`VolumeSnapshot` + static VS/VSC
      re-bind + temp COW PVC mounted **readOnly**) for RBD/other CSIs; `ReadyToUse` wait, cleanup,
      orphan reaper. A CSI that **cannot snapshot** → volume **skipped** (`status.volumes[].phase:
      Skipped`, `reason: CSISnapshotUnsupported`, Event); `rook-rbd-direct` is opt-in
      (`exposure.rbdDirect`). (delta 6)
- [ ] `crystal-mover` image (restic + shim): back up a mounted path, structured JSON result
      via termination message; metadata fidelity (xattrs stored unconditionally, restore
      passes no xattr filter so ACLs travel, `--sparse`); `--pack-size 32–64MiB`,
      `GOMEMLIMIT`, `--retry-lock`; **pin restic ≥ 0.19.1** (stale-lock fixes — delta 3,
      [adr/0001](adr/0001-repository-engine-restic-format.md)).
- [ ] Reliability spine (delta 1): **periodic requeue of every non-terminal phase** +
      per-phase timeout + effective cancel (foreground Job delete) + **re-adoption of
      in-flight Jobs at operator restart**; OOMKilled mover → explicit CR failure + lock
      check (delta 7).
- [ ] Schedules (delta 4): deterministic **jitter** (hash of namespace/CR),
      `concurrencyPolicy: Forbid` (skip + event), `startingDeadlineSeconds` bounding
      post-downtime catch-up to one run, **no backup on apply**.
- [ ] Discovery controller (R26): per `BackupRepository`, on location add + every
      `discovery.interval` — `restic snapshots`, group by `(namespace, run)`, project
      `Backup` objects into **existing** namespaces (skip non-existent), remove projections
      whose snapshots are gone → CR lifetime = data lifetime.
- [ ] Retention (R24, partial): `forget --group-by host,paths` per PVC, **enqueued on the
      per-repo exclusive queue** (never inline); two-phase forget/purge, idempotent
      "already absent" = success (delta 11).
- [ ] Global snapshot/clone concurrency semaphore (per cluster, not just per repo) + per-node
      topology spread to aggregate bandwidth (R12; delta 12).
- [ ] Metrics v1 (R19): `crystalbackup_backup_last_success_timestamp_seconds{namespace,cluster}`,
      duration, size, added bytes, per-namespace failures — gauges derived from CR state
      (restart-insensitive).
- [ ] e2e: a `ClusterBackupSchedule` backs up several namespaces into one shared SeaweedFS repo;
      discovery projects `Backup`s; **leak-check invariant** (zero residual VS/VSC/PVC after
      every scenario incl. injected failures — delta 5); kill the operator mid-run → the run
      converges (re-adoption), never stuck.

**Exit criteria**: a daily `ClusterBackupSchedule` backs up a multi-namespace demo (RWO+RWX
PVCs) into ONE shared repo in SeaweedFS; `kubectl get backups -n <ns>` lists exactly what is
restorable; `restic snapshots` with the platform DEK shows per-namespace tags;
operator/node kill mid-transfer converges; leak-check green.

## M2 — Restore (R2 cornerstone, R6, R7, R14, R23)

- [ ] `Restore` controller (namespaced, user): consumes a `Backup` **in its own namespace**
      (no `locationRef`, no target-namespace field — structural confinement, R14); **mode**
      (`Recreate` | `Overwrite`) × **selection** (NetworkPolicy-style `resources[]` /
      `volumes[]` lists, partial-PVC via `include`, R7); `AwaitingConfirmation` flow.
- [ ] **Operator-mediated cluster-DR restore** (R2/R14 cornerstone): when the referenced
      `Backup` is `crystalbackup.io/origin: cluster`, serve it from the shared repo under a
      **non-forgeable server-side tag filter `namespace=<the CR's namespace>`**; cluster-origin
      `Backup`s are read-only to users (admission).
- [ ] `ClusterRestore` controller (admin): addresses a **repo coordinate** (location + origin
      namespace + run/time), **creates** the target namespace, maps storageClasses — works
      when the source namespace is gone.
- [ ] Restore mover (`crystal-mover`): mounts the target PVC read-write (`restic restore`;
      `Recreate` = `--overwrite always --delete`, `Overwrite` = `--overwrite always` without
      `--delete`); independent of application pods; topology inherited from the backup path.
- [ ] Admission (**VAP-first**, [adr/0010](adr/0010-admission-vap-first.md)): static rules as
      `ValidatingAdmissionPolicy` — R23 confirmation (conservative superset: **every**
      `Recreate`/`Overwrite` needs `confirmation == target`; target identity for erasure),
      user-isolation (binding **excludes the operator SA**), immutable-forbids-prune,
      same-namespace Secret refs, denied-namespaces (ConfigMap `paramRef`), selector shape; the
      **dynamic** single-default-location check stays a webhook (`failurePolicy: Ignore`); the
      retention-vs-`Immutable` advisory is **controller-side** (`RetentionIgnored` condition +
      Warning), not admission.
- [ ] e2e (R14 negatives): a user restores their own backup (both modes, selection,
      confirmation); a user **cannot** restore another namespace's backup — at the API level
      (cluster-origin read-only) **and** the storage level (tag filter cannot be forged);
      admin `ClusterRestore` into a **recreated** namespace.

**Exit criteria**: R14 negative tests pass at API and storage level; `ClusterRestore`
reconstitutes a deleted namespace from the shared repo.

## M3 — Manifests & cluster-scoped backup & restore (R15, R22)

- [ ] Namespace resource dump via a `crystal-manifest-mover` Job (ServiceAccount
      `crystal-manifest-mover`, transiently bound to ClusterRole `crystal-manifest-reader`)
      into a `kind=manifests` snapshot at `/manifests/<namespace>`.
- [ ] Sanitization engine (`internal/sanitize`) + golden-file corpus
      ([04-manifest-backup.md](04-manifest-backup.md), [adr/0007](adr/0007-manifest-sanitization.md)):
      neat-like stripping, Service `clusterIP` stripped / `nodePort` **preserved**,
      PVC→storageClass mapping hooks, ownerReferences policy.
- [ ] Manifest restore under ClusterRole `crystal-manifest-writer`, folded into the
      `Restore`/`ClusterRestore` **mode × selection** model: `Recreate` = delete-then-create,
      `Overwrite` = server-side apply keeping extras; apply ordering; storageClass mapping.
- [ ] **Cluster-scoped resource capture & restore** (R22, [adr/0011](adr/0011-cluster-scoped-dr.md)):
      `ClusterBackup` captures selected cluster-scoped objects (curated default allowlist — CRDs,
      StorageClasses, IngressClasses, PriorityClasses, ClusterRoles/Bindings excl. `system:*`,
      PersistentVolumes; admin-tunable include/exclude) as a `kind=cluster-manifests` snapshot at
      `<prefix>/<clusterID>/cluster-manifests/`, via a privileged-read capture Job; capture **ON**
      by default. `ClusterRestore` restores them **selectively** (opt-in, admin-only, apply-ordered
      CRDs→cluster-scoped→namespaces→namespaced), with sanitization + confirmation.
- [ ] Never dump via exec/stdout — the manifest mover writes to the repo directly (delta 8).
- [ ] e2e: full namespace backup, restore into a fresh namespace and into kind (different
      CIDR, different storage class) — workloads come back Ready.

## M4 — Consistency hooks, verification & maintenance (R16, R17)

- [ ] Pre/post exec hooks (pod selector, container, timeout, onError), freeze window =
      snapshot phase only; **timeout truly honored** (context deadline, dedicated unit test)
      and **unconditional unfreeze** — post-hooks run even if the snapshot fails, with retries
      + critical alert (delta 8).
- [ ] Maintenance controller on the per-`BackupRepository` exclusive queue: `prune` (Standard
      mode; one cluster-wide window for the shared repo), `check` schedules, jitter;
      **operator-driven `restic unlock`** of stale locks before each exclusive op (delta 3);
      `RecentMaintenance` history + consecutive-failure alert.
- [ ] Repository integrity verification (R17): `restic check` (structure) + scheduled
      `check --read-data-subset` (sampled data read to catch silent bucket / bit-rot corruption);
      result in `BackupRepository.status` + metrics + `RepositoryCheckFailed` alert.
      **Restore-testing stays the administrator's job** (restore drills via the normal restore
      path); no automated per-backup canary and no offloadable/verification-index in v1 (deferred
      — [00-requirements.md §6](00-requirements.md)).
- [ ] e2e: hook failure policies; controller crash between pre and post hook → **unfreeze
      still happens** (the feature's most important test); prune under concurrent backups;
      kill a prune mid-flight → the next run purges the stale lock and succeeds;
      `check --read-data-subset` catches an intentionally corrupted repo (S3 object tampering).

## M5 — Namespace plane, external sync & right-to-erasure (R3, R5, R21, R28)

- [ ] `BackupLocation` + `BackupSchedule` (namespaced): a user backs up their **own** namespace
      to their **own** object storage, **in addition to** cluster DR; `Backup` in-namespace via
      the same execution path (no fan-out).
- [ ] Keys ([adr/0004](adr/0004-encryption-key-management.md)): the **user's own** restic
      password (their Secret), or an operator-generated password stored **in the user's
      namespace**; optional `platformAccess` slot (default `false`) via `restic key add` for
      mediated restore/verify. **No** cluster→client→namespace hierarchy.
- [ ] Right-to-erasure `ClusterErasure` (R21): `restic forget --tag`
      (`tenant=` | `namespace=` | `namespace=+pvc=`) then `prune` — **physical** deletion on
      the exclusive queue; typed confirmation (R23); `Blocked` + `blockedUntil` on Immutable
      locations. Per-tenant crypto-shredding is **dropped** — impossible in a single-key
      shared repo ([adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md)).
- [ ] Repository **decommission** mechanism (destroy the wrapped platform DEK/KEK —
      repo-granularity retirement, **not** GDPR erasure) + re-encryption-via-repo-copy runbook
      for a compromised DEK. Erasure/decommission are driven via the `ClusterErasure` CR / a
      **confirmation-gated, audited** admin action in M5 (not a raw Secret delete); the
      `crystalctl admin erase|decommission|reencrypt` **wrappers** ship in M7 with the CLI
      (`reencrypt` automation stays backlog).
- [ ] **External sync** (R28, [adr/0013](adr/0013-external-backup-sync.md)):
      `ClusterBackupExternalSync` (admin, whole shared repo → secondary `ClusterBackupLocation`)
      and `BackupExternalSync` (user, namespace's backups → secondary `BackupLocation` in the
      same ns) via a `restic copy` mover Job — re-encrypt to the destination's own key,
      blob-incremental, tag-selective; `mode: Mirror|AppendOnly` (forced AppendOnly on Immutable
      destinations; full rotation-window handling for an Immutable destination lands with **M8**);
      sync metrics + `ExternalSyncStale` alert.
- [ ] e2e: a user backs up to their own S3 (SeaweedFS) with their own key; the platform cannot read it
      unless `platformAccess: true`; `ClusterErasure` of a tenant/namespace/PVC physically
      removes the data (repo re-scan confirms); erasure on an Immutable location reports
      `Blocked`; **external sync** copies to a second location whose repo opens only with **its
      own** key (siloing) and per-namespace selection holds (08-testing case 18).

## M6 — Observability hardening & production readiness

- [ ] Full metrics catalogue ([05-observability.md](05-observability.md)), Grafana dashboards
      (namespace-user + platform) under `charts/crystal-backup/dashboards/`, alert rules
      (backup missed/failed/aged, check failed).
- [ ] OTel traces across the pipeline (schedule → snapshot → mover), exemplars.
- [ ] Mover resources by operation type (prune > backup), cache emptyDir `sizeLimit` decision,
      load test on millions-of-files volumes (restic vs rustic revisit —
      [adr/0001](adr/0001-repository-engine-restic-format.md)); delta 7.
- [ ] VSC ↔ RBD-image reconciliation + trash monitoring + active pre-check before VS creation
      (VolumeSnapshotClass resolved, secret present, snapshotter sidecar reachable) — delta 9;
      S3 RGW tuning (`s3.connections`, wave test vs `rgw_max_concurrent_requests`) — delta 13.
- [ ] **Restore-fidelity gate** (the beta bar for `0.6`, not a 1.0/GA claim): e2e restore +
      checksum comparison to a Rook-Ceph PVC while restic#5543 stays open (delta 14).
- [ ] NetworkPolicies, PodSecurity review, resource limits/requests; docs (user guide, ops
      guide, DR runbooks); deploy alongside Velero on a staging cluster, soak 2+ weeks.

**Exit criteria**: production rollout on a pilot cluster for pilot namespaces; dashboards +
alerts live; leak-check and restore-checksum gate green.

## M7 — CLI & UI v1 (R8, R9 — lower priority, agreed)

- [ ] `crystalctl` CLI (R8): `repo snapshots|ls|dump|export --tar|restore|stats` against S3 +
      key without Kubernetes (wraps restic; documented upstream-command equivalence for
      reversibility — [06-cli.md](06-cli.md)); the `backup`/`restore`/`admin` kubeconfig subtrees
      (incl. the `admin erase|decommission` wrappers over the M5 CRs); e2e byte-compares the
      `repo export --tar` output against source.
- [ ] `crystalctl ui`: local Go binary serving the browse SPA on localhost (list backups,
      browse trees, download file/dir as stream/zip) via `internal/browse` — packages designed
      for reuse by a future Rancher extension / Headlamp plugin ([07-ui.md](07-ui.md),
      [adr/0008](adr/0008-ui-strategy.md)).
- [ ] Later (post-v1, separate decision): hosted multi-tenant backend + Rancher extension or
      Headlamp plugin with OIDC.

## M8 — Immutable locations (R18 — design done in the M0 API, implementation here)

- [ ] Immutable mode: S3 Object Lock bucket support, repo rotation (`rotationPeriod`),
      forget-only bookkeeping, expired-repo deletion, lock-file strategy (`--no-lock` reads;
      validate rest-server append-only and/or rustic lock-free writes in a POC —
      [adr/0005](adr/0005-immutability-mode.md)).
- [ ] **Erasure-on-immutable**: `ClusterErasure` stays `Blocked` until object-lock expiry, then
      completes; lifecycle of residual object versions left by retried uploads on versioned
      buckets (delta 13).
- [ ] e2e with **Ceph RGW Object Lock**: erasure blocked then completing; rotation retiring an
      expired repo.

## M9 — Coexistence hardening & DR drills (R22)

Cluster DR — including cluster-scoped capture & selective restore
([adr/0011](adr/0011-cluster-scoped-dr.md)) — already ships in M1/M3; this milestone hardens the
side-by-side coexistence guarantees and the fleet DR drills. **Replacing another backup tool is an
operator decision, not a project deliverable** ([adr/0006](adr/0006-coexistence-with-backup-tools.md)).

- [ ] Cluster-scoped DR **hardening**: default-allowlist review, guidance on excluding
      GitOps-managed resources (ArgoCD/flux) at restore time, coverage audit
      ([adr/0011](adr/0011-cluster-scoped-dr.md)).
- [ ] DR restore drills: full-namespace fleet `ClusterRestore` onto a rebuilt cluster
      (repo-only bootstrap, namespaces recreated), RTO measurement, runbook.
- [ ] Coexistence validation: run side-by-side with an incumbent tool (e.g. Velero) with no
      interference (distinct snapshots/repos/namespaces; snapshot-count headroom). **Operator
      guidance** (coverage-diff, DR-drill harness) for teams who *choose* to consolidate onto
      Crystal Backup — no forced decommission, no tool-specific parity gate.

## Backlog / future (not scheduled)

Recorded in [00-requirements.md §6](00-requirements.md); no milestone yet.

- **Namespace-plane backup as a partial repo copy** (`restic copy` from the cluster DR repo
  into the user's bucket instead of an independent re-backup) — could supersede independent
  namespace backups if the feasibility/cost trade-off wins
  ([adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md)).
- **PVC shrink via backup/restore CRD** (recreate a PVC at a smaller size; requires it
  unmounted).
- **Mover placement on `[Cluster]BackupLocation`** (`nodeAffinity`/`tolerations` near the S3
  endpoint — bandwidth / IO cost / network segmentation); admin-unrestricted, tenant
  governance-gated ([02-api.md](02-api.md)).
- **Preferred backup window** (was R27; removed from v1 2026-07-15). If re-introduced, model it
  as **`start` + `duration`** (unambiguous across midnight), not `start`/`end`, with a skip
  Event/metric and a `WindowUnsatisfiable` controller condition
  ([00-requirements.md §6](00-requirements.md)).

## Global Definition of Done (every task)

- Unit tests written and passing; integration (envtest) tests for controller behaviour; e2e
  coverage when the task touches the data path; the **leak-check invariant** (zero residual
  VS/VSC/PVC) holds after every e2e scenario (delta 5).
- Structured logs for new code paths; metrics for new user-visible outcomes (namespace-labelled,
  R19); traces on new pipeline spans.
- CRD/API changes: validation (VAP/CEL first; webhook or controller-side advisory only for
  cross-object checks) + generated docs + [02-api.md](02-api.md) updated in the same PR.
- Security review for anything touching credentials, keys, or cross-namespace logic (two-person
  review).
- No widening of **namespace-user** RBAC without an ADR.
- Docs updated (user or ops guide); CHANGELOG entry.
- CI green (lint, unit, e2e); image + chart publishable from the PR pipeline.
