# Roadmap — milestones & task breakdown

Status: agreed direction (two-plane cascade rework, 2026-07-12); estimates refined per
milestone kickoff. Naming contract: [02-api.md](02-api.md); model rationale:
[adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md).
Priorities per [00-requirements.md §7](00-requirements.md): core two-plane path (incl.
**cluster DR** + discovery) → namespace-plane locations/keys + **external sync** → observability
→ **reach** (storage without snapshots, file-level restore, notifications) → **proof** (restore
drills and alerting) → immutable mode. Cluster DR is now **core** (M1), no longer deferred;
coexistence with other backup tools is a standing guarantee, **not** a replacement project
([adr/0006](adr/0006-coexistence-with-backup-tools.md)).

**Re-prioritised 2026-08-02** (M7–M9). The CLI and the UI leave this repository entirely
([adr/0020](adr/0020-cli-and-ui-as-separate-repositories.md)) — R8 reversibility is already met by
the repo being plain restic, so neither is a guarantee this repository owes. M7 becomes reach, M8
becomes proof, M9 becomes immutability, and coexistence hardening is retired as a deliverable
because it is already structural.

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
      melange-built restic + SBOM + cosign sign + SLSA Build Level 3 provenance + container CVE-scan gate,
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
SLSA Build Level 3 provenance attested, pushed to GHCR**.

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
      `ClusterBackup` captures selected cluster-scoped objects (curated default allowlist —
      canonical in [adr/0011 §1](adr/0011-cluster-scoped-dr.md): CRDs, StorageClasses,
      VolumeSnapshotClasses, IngressClasses, PriorityClasses, RuntimeClasses,
      ClusterRoles/Bindings excl. `system:*`, PersistentVolumes; admin-tunable
      include/exclude) as a `kind=cluster-manifests` snapshot at restic path
      `/cluster-manifests` — inside the shared repo that already lives at
      `s3://<bucket>/<prefix>/<clusterID>/`, per the layout table of
      [02-api.md](02-api.md) — via a privileged-read capture Job; capture **ON**
      by default. `ClusterRestore` restores them **selectively** (opt-in, admin-only, apply-ordered
      CRDs→cluster-scoped→namespaces→namespaced), with sanitization + confirmation.
- [ ] Never dump via exec/stdout — the manifest mover writes to the repo directly (delta 8).
- [ ] e2e: full namespace backup, restore into a fresh namespace and into kind (different
      CIDR, different storage class) — workloads come back Ready.

## M3.1 — Operator throughput & discovery scalability audit (post-M3 hardening) — DONE (0.3.1)

**Root-cause audit, NOT speculative fixes.** Surfaced by the M3 **full-suite** crucible run (all of
m0–m3 driving the ONE shared "dr" repository): under sustained multi-namespace load several specs
timed out **differently on every run** (m1 OOM crash-detection convergence `m1_reliability`, m1
discovery GC of a forgotten projection `m1_discovery`, m1 `restic check` `m1_repository`, m2
Recreate restore), and the whole `go test` overran its **60-min** budget (panic). This was **NOT an
M3 regression and NOT a correctness bug** — M3 was functionally validated (unit + envtest, `make
e2e` kind 25/25 with real mover Jobs, crucible **m3-only 11/11**, every M3 `It` green even inside
the full suite), and the cause was pre-existing and orthogonal to M3.

**What the measurement actually found** (the working hypothesis below was wrong, which is why the
milestone was chartered as measure-first): the bottleneck was **not** an O(snapshots) re-scan and
**not** operator resources. Discovery re-triggered itself — it wrote its own inventory status onto
the object it watches, with no predicate on that watch — so it never rested at `discovery.interval`
and spun back-to-back, each pass blocking its single worker on a cold `restic snapshots` Job.
The O(snapshots) term is real but secondary, and is pure S3 per-object latency rather than restic
compute. Details and numbers: [docs/audit-m3.1-throughput.md](../docs/audit-m3.1-throughput.md).

**Shipped as `0.3.1`** (audit: [docs/audit-m3.1-throughput.md](../docs/audit-m3.1-throughput.md)).

- [x] **Measured the real bottleneck FIRST.** The released 0.3.0 operator was deployed unmodified
      on a live crucible and scraped over a whole full-suite run. The hypothesis below (O(snapshots)
      re-scan) was **not** the dominant cost: discovery's single reconcile worker was **~100 %
      saturated for an entire run at THREE snapshots** — `reconcile_time_seconds{discovery}` summed
      to 1991 s inside a 1980 s window, every other controller combined under 100 s, operator CPU
      ~0.01 core / RSS ~290 MB. The cost was wall-time on blocking reconciles; the multiplier was a
      **self-trigger loop** (discovery wrote its own inventory status onto the object it watches,
      with no predicate on the watch).
- [x] **Fixes, driven by that data**: the self-trigger predicate; a plain (non-controller) owner
      reference on the inventory Job so `backuprepository`'s `Owns(Job)` stops waking on every
      listing Job (~2700 → 56 reconciles); the inventory moved **off the reconcile worker**
      (single-flight per repository, re-enqueued via `source.Channel`); and a terminating namespace
      skipped like an absent one. Re-measured: **discovery 1991 s → 9.9 s across a run 1.8× longer**,
      worker never blocked.
- [x] **Discovery scalability, quantified**: restic's own listing compute is flat (~0.8 s from N=10
      to N=2000 on a local repo) — the O(N) is entirely **S3 per-object latency on a cold cache**
      (~27 ms/snapshot; N=350 → 10 s). It is now paid off the worker and once per interval, so it
      starves nothing. A warm/incremental inventory (cold O(N) → warm O(Δ)) stays available as the
      next lever if a repository grows into the thousands of snapshots.
      NOTE — `--no-lock` for discovery is deliberately deferred to **M4** (it lands WITH `prune`, the
      first exclusive lock; until then discovery's non-exclusive `restic` lock contends with nobody,
      so `--no-lock` buys nothing yet and is best done alongside the maintenance controller).
- [x] **Crucible budget** raised 60m → 90m (`CRUCIBLE_TIMEOUT`); the M3 run had overrun it and the
      panic took the report with it. Full-suite result on 0.3.1: **37 passed / 2 failed / 4 skipped
      in 58.6 min**, with `m1_discovery` and `m1_repository` now passing.

## M3.2 — Restore safety (post-M3.1 hardening) — DONE (0.3.2)

M3.1's four deferred items, taken in one patch. The first of them was deliberately shipped as a
**reproduction plan rather than a patch** — two explanations, opposite fixes, so guessing was the
one thing not to do. Reproducing it took five minutes on a fresh crucible (`mise run test m2`
alone) and answered the question by finding **two** bugs, not one.

- [x] **The leak that wedged a namespace: it was ours.** `m2-restore` sat in `Terminating` on a PVC
      carrying `snapshot.storage.kubernetes.io/pvc-as-source-protection` with zero VolumeSnapshots
      in the cluster. The snapshot-controller's own log showed it ADDING that finalizer at 10:35:44
      and REMOVING it at 10:35:46; the PVC's `managedFields` then named the culprit —
      `manager: crystalbackup-restore, operation: Apply` at 10:36:38, owning
      `f:metadata:f:finalizers`. The manifest capture had photographed the PVC inside that two-second
      window and the restore re-applied the finalizer onto a PVC with no snapshot behind it, where
      nothing would ever remove it. Fixed at both ends: sanitization rule **S8** strips every
      finalizer at capture, and the applier strips them again so pre-0.3.2 snapshots are safe
      ([04-manifest-backup.md §4.4](04-manifest-backup.md)).
- [x] **The RBD image that "vanished" was deleted by us — a data-loss bug.** Not a Ceph-side vanish:
      `Recreate` mode **deleted the user's PVC** as part of restoring its manifest, the released PV
      (reclaimPolicy `Delete`, the dynamic-provisioning default) had the CSI driver destroy the RBD
      image, and the data half of the same restore — mounting a twin PV built from that very
      `volumeHandle` — then hung forever on `internal RBD image not found`. Proven on the cluster:
      the twin's handle was absent from `rbd ls`, and the PVC had rebound to a brand-new empty
      volume. `Recreate` now reconciles PVCs and PVs **in place**
      ([04-manifest-backup.md §5.2](04-manifest-backup.md)).
- [x] **`Recreate` of an auto-recreated object no longer fails the restore.** The third bug the
      reproduction turned up, and the reason the m2 `Recreate` spec had never once passed: the
      control plane puts a namespace's `default` ServiceAccount back the instant it is deleted, so
      the create always hit `AlreadyExists`, that object reported `Failed`, and the whole restore
      came back `PartiallyFailed`. It now falls back to the in-place apply.
- [x] **A projection failure no longer discards the inventory**: per-group failures are accumulated
      and reported (`ProjectionIncomplete`), the inventory is recorded regardless, and the retry
      reuses the listing instead of paying for a fresh one (bounded by the discovery interval).
- [x] **The OOM spec was not testing what it claimed.** It SIGKILLed whichever
      `crystalbackup.io`-labelled mover pod was Running — another run's, or the same run's *manifest*
      mover — after which the data volume completed normally and the spec failed on its own
      assertion. Scoped to its own run's DATA movers, and asserting only about the volumes it
      actually killed (`c-db` has two PVCs, so "no volume is Completed" was never satisfiable). The
      operator's crash handling was correct all along.
- [x] **Least-privilege trim**: the capability set now follows what a Job can touch, not its name.
      A maintenance Job (no PVC mounted: `init`/`forget`/`prune`/`check`/`snapshots`/`unlock` and
      the manifest shapes) adds **nothing** on top of `drop: ALL`; `DAC_OVERRIDE` stays on data jobs,
      and the restore keeps its metadata-fidelity set (R10).
- [x] **Leak-check that fails the run that caused it**: specs restoring into a namespace tear it
      down with `deleteNamespaceAndWaitGone`, which surfaces the namespace's own
      "finalizers remaining" message instead of leaving the wedge for a later run to trip over.

## M4 — Consistency hooks, verification & maintenance (R16, R17)

- [x] Pre/post exec hooks (pod selector, container, timeout, onError), freeze window =
      snapshot phase only; **timeout truly honored** (context deadline, dedicated unit test)
      and **unconditional unfreeze** — post-hooks run even if the snapshot fails, with retries
      + critical alert (delta 8).
- [x] Maintenance controller on the per-`BackupRepository` exclusive queue: `prune` (Standard
      mode; one cluster-wide window for the shared repo), `check` schedules, jitter;
      **operator-driven `restic unlock`** of stale locks before each exclusive op (delta 3);
      `RecentMaintenance` history + consecutive-failure alert.
- [x] Repository integrity verification (R17): `restic check` (structure) + scheduled
      `check --read-data-subset` (sampled data read to catch silent bucket / bit-rot corruption);
      result in `BackupRepository.status` + metrics + `RepositoryCheckFailed` alert.
      **Restore-testing stays the administrator's job** (restore drills via the normal restore
      path); no automated per-backup canary and no offloadable/verification-index in v1 (deferred
      — [00-requirements.md §6](00-requirements.md)).
- [x] e2e: hook failure policies; controller crash between pre and post hook → **unfreeze
      still happens** (the feature's most important test); prune under concurrent backups;
      kill a prune mid-flight → the next run purges the stale lock and succeeds;
      `check --read-data-subset` catches an intentionally corrupted repo (S3 object tampering).

**Shipped as `0.4.0`**, with one addition the milestone did not plan: an intermittent
`VolumeSnapshotContent` leak, audited and fixed as **crash-only teardown by re-entry** (the pass
that wrote the terminal status used to be the only one that ever deleted the VS/VSC pair). The
window predated 0.4.0; M4 made it more probable, not possible. Validated over seven independent
crucible lanes, all with zero residual snapshot objects
([crucible M4 report](https://crystalbackup.github.io/CrystalBackup/reports/crucible-m4.html)).

## M5 — Namespace plane, external sync & right-to-erasure (R3, R5, R21, R28)

- [x] `BackupLocation` + `BackupSchedule` (namespaced): a user backs up their **own** namespace
      to their **own** object storage, **in addition to** cluster DR; `Backup` in-namespace via
      the same execution path (no fan-out).
- [x] Keys ([adr/0004](adr/0004-encryption-key-management.md)): the **user's own** restic
      password (their Secret), or an operator-generated password stored **in the user's
      namespace**. **No operator key slot on a user repository** — `platformAccess` was
      specified, never implemented, and dropped during M5 so that platform access ends when the
      user's key does (adr/0004 amendment). **No** cluster→client→namespace hierarchy.
- [x] Right-to-erasure `ClusterErasure` (R21): `restic forget --tag`
      (`tenant=` | `namespace=` | `namespace=+pvc=`) then `prune` — **physical** deletion on
      the exclusive queue; typed confirmation (R23); `Blocked` + `blockedUntil` on Immutable
      locations. Per-tenant crypto-shredding is **dropped** — impossible in a single-key
      shared repo ([adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md)).
- [x] Repository **decommission** mechanism (destroy the wrapped platform DEK/KEK —
      repo-granularity retirement, **not** GDPR erasure) + re-encryption-via-repo-copy runbook
      for a compromised DEK. Erasure/decommission are driven via the `ClusterErasure` CR / a
      **confirmation-gated, audited** admin action in M5 (not a raw Secret delete); the
      `crystalctl admin erase|decommission|reencrypt` **wrappers** ship in M7 with the CLI
      (`reencrypt` automation stays backlog).
- [x] **External sync** (R28, [adr/0013](adr/0013-external-backup-sync.md)):
      `ClusterBackupExternalSync` (admin, whole shared repo → secondary `ClusterBackupLocation`)
      and `BackupExternalSync` (user, namespace's backups → secondary `BackupLocation` in the
      same ns) via a `restic copy` mover Job — re-encrypt to the destination's own key,
      blob-incremental, tag-selective; `mode: Mirror|AppendOnly` (forced AppendOnly on Immutable
      destinations; full rotation-window handling for an Immutable destination lands with **M8**);
      sync **metrics** ([05-observability.md §2](05-observability.md)). The `ExternalSyncStale`
      **alert rule** ships with M6's alert bundle — §3 of that document is a specification as of
      0.5.0, so what M5 delivers is the series the alert will read, not the rule.
- [x] e2e: a user backs up to their own S3 (SeaweedFS) with their own key; the platform cannot read it
      once they delete their password Secret; `ClusterErasure` of a tenant/namespace/PVC physically
      removes the data (repo re-scan confirms); erasure on an Immutable location reports
      `Blocked`; **external sync** copies to a second location whose repo opens only with **its
      own** key (siloing) and per-namespace selection holds (08-testing case 18).

## M6 — Observability hardening & production readiness

- [x] Full metrics catalogue ([05-observability.md](05-observability.md)), Grafana dashboards
      (namespace-user + platform) under `charts/crystal-backup/dashboards/`, alert rules
      (backup missed/failed/aged, check failed).
- [x] OTel traces across the pipeline (schedule → snapshot → mover), exemplars.
- [ ] Mover resources by operation type (prune > backup) and the cache emptyDir `sizeLimit`
      decision — **both delivered in 0.6.1**: four sizing classes over the thirteen operations,
      overridable per operation through `mover.profiles` in the chart, defaults living ONLY in
      `internal/mover/profiles.go` and documented by generation in
      [`docs/MOVER-RESOURCES.md`](../docs/MOVER-RESOURCES.md). The `sizeLimit` is a ceiling
      against a runaway cache filling a node's disk, NOT a fitted estimate — it ships with the
      eviction made legible (`MoverEvicted` carrying the kubelet's own message, `MoverOOMKilled`
      for the memory limit), because a pod that silently disappears mid-backup is a worse failure
      than the unbounded cache it replaced.
      **Still open**: the load test on millions-of-files volumes and the restic-vs-rustic revisit
      ([adr/0001](adr/0001-repository-engine-restic-format.md)) — it needs real infrastructure and
      is what would turn these conservative ceilings into measured numbers; delta 7.
- [ ] VSC ↔ RBD-image reconciliation + trash monitoring + active pre-check before VS creation
      (VolumeSnapshotClass resolved, secret present, snapshotter sidecar reachable) — delta 9;
      S3 RGW tuning (`s3.connections`, wave test vs `rgw_max_concurrent_requests`) — delta 13.
- [x] **Restore-fidelity gate** (the beta bar for `0.6`, not a 1.0/GA claim): e2e restore +
      checksum comparison to a Rook-Ceph PVC while restic#5543 stays open (delta 14).
      **Executable as the `m6` crucible label** (`mise run test m6`) — the exit criterion is a
      command, not a paragraph. It restores into a namespace that does not exist yet, so a mode,
      an xattr or an ACL the restore fails to re-apply cannot be masked by what was already on
      the target; and it carries no enable flag and no conditional skip, because a gate that can
      disable itself reads as a pass.
- [ ] NetworkPolicies, PodSecurity review, resource limits/requests; docs (user guide, ops
      guide, DR runbooks); deploy alongside Velero on a staging cluster, soak 2+ weeks.
      **Partially delivered in 0.6.0, and the split matters.** The docs shipped in full (the
      site, the GitOps install pages, the preflight script). The NetworkPolicy, the four
      PodSecurity namespace labels and the operator's requests/limits were already in the chart
      from earlier milestones and did not change here — so what is missing is the *review*, not
      the mechanism. That review now has a known answer to start from: the operator satisfies
      every `restricted` criterion, but the mover runs as uid 0 with a per-operation capability
      set because restic has to read and restore files owned by arbitrary uids, and both live in
      `crystal-backup-system` — so `enforce: baseline` there is a constraint, not caution.
      The soak alongside Velero has **not** happened; it runs on a real build cluster after the
      0.6.0 tag, which is why 0.6.0 is offered for testing rather than for production.

**Exit criteria**: production rollout on a pilot cluster for pilot namespaces; dashboards +
alerts live; leak-check and restore-checksum gate green.

**Status as of 0.6.0 (2026-07-31).** Three of the six bullets shipped, and the crucible ran the
whole suite unfiltered — 82 of 82 checks, nothing failed, nothing skipped, the project's first
green campaign that is not label-scoped. Dashboards and alerts are live and the restore-fidelity
gate is green, so two of the three exit criteria are met. The third — a pilot rollout — is not,
and neither is the soak, which is the honest reason 0.6.0 is a testing release. The two
remaining bullets move to 0.6.1: mover resources by operation type with the cache `sizeLimit`
decision and the millions-of-files load test, and the VSC ↔ RBD reconciliation with RGW tuning.

**Status as of 0.6.1, and the plan to close it (2026-08-02).** Mover resources by operation type
shipped. What is still open splits three ways, and conflating them is what let it drift:

- **Code — scheduled as `0.6.3`, before M7 opens.** The VSC ↔ RBD reconciliation, trash monitoring
  and the active pre-check before VolumeSnapshot creation (delta 9), and the S3 RGW tuning
  (delta 13). Carrying code from milestone to milestone is the mechanism that produced the
  announced-but-inert features of the M5 lot E; it gets its own patch instead.
  *(Renumbered from 0.6.2, which went to the soak kit — see the 0.6.2 status below. The
  re-arbitration of M7–M9 itself took no version: it touched specs, the site and the promotion
  kit, and a change with no operator behaviour in it is not a release.)*
- **Measurements — needs real infrastructure.** The millions-of-files load test and the
  restic-vs-rustic revisit ([adr/0001](adr/0001-repository-engine-restic-format.md)) that depends
  on it. This is what would turn the conservative mover ceilings into measured numbers.
- **Activities — run in parallel, they block nothing.** The PodSecurity review (whose answer is
  already known: the operator satisfies `restricted`, the mover cannot, so `enforce: baseline` in
  `crystal-backup-system` is a constraint rather than caution), the two-week soak alongside an
  incumbent, and the pilot rollout. These are the two unmet exit criteria, and they are calendar
  work, not engineering work.

**Status as of 0.6.2 (2026-08-03) — the soak becomes runnable.** "Calendar work, not engineering
work" was true of the soak only if the instrument existed and worked. It did not: the collector
shipped in the kit's first form could not see two of the four mover sizing classes at all, and
reported them as NOT_MEASURED through a four-hour crucible campaign that executed dozens of
backups. 0.6.2 is that instrument, fixed and verified end to end on real infrastructure — 82 of
82 crucible checks, unfiltered — plus the first measured answer to §5's mover-sizing question:
`data` 81Mi against a 4Gi limit, `manifests` 105Mi against 2Gi, `repo-heavy` 74Mi against 8Gi,
`repo-light` 101Mi against 1Gi, no OOM kills and no evictions. **On the crucible's small
repository**, which is the caveat the fortnight on real data exists to remove.

So the remaining M6 work is now genuinely calendar: install `soak.enabled=true` on a real
cluster, leave it a fortnight, run `hack/soak/collect.sh`. The pilot rollout and the PodSecurity
review are unchanged.

## M7 — Reach: storage without snapshots, file-level restore, notifications (0.7)

**Re-scoped 2026-08-02.** M7 previously stacked the CLI and the UI under one number; both now
ship as separate repositories ([adr/0020](adr/0020-cli-and-ui-as-separate-repositories.md)) and
M7 becomes the milestone that **widens the addressable estate**. The ordering rationale: a CSI
that cannot snapshot leaves a cluster with nothing at all today, which is a larger gap than any
convenience layer, and the mover placement this milestone builds is the prerequisite for
file-level restore.

- [ ] **Degraded mode for storage without snapshots** — a `filesystem-direct` exposer registered
      in the existing cascade (`cephfs-shallow` / `csi-generic` / `rook-rbd-direct` are unchanged),
      backing up the **live** filesystem with restic instead of a snapshot. Delivered in **two
      stages, and the seam is what de-risks the milestone**:
      - **unmounted volumes first** — no writer, so the copy is genuinely consistent rather than
        merely crash-consistent, and there is no placement constraint at all. `rexposer.attachedNode`
        already returns the empty node name when a PV has no attachment, which is exactly the free-
        scheduling behaviour this needs;
      - **mounted volumes second** — this half carries the whole cost: the mover must land on the
        node holding the RWO attachment, the copy is **not** crash-consistent, and the perception
        guardrails live here. Explicit opt-in per PVC or per schedule (**never** a silent fallback
        from the snapshot path), a consistency field on `status.volumes[]` (not a doc note), a
        distinct metric and alert so an operator can see how many backups are degraded, and
        quiescence hooks documented as the only way to recover some consistency.
      A PVC in `Pending` under `WaitForFirstConsumer` has no PV and therefore nothing to back up:
      **skipped cleanly**, never waited on.
- [ ] **Mover placement** (promoted from the backlog): pin the mover to the node where the volume
      is attached, reusing the `VolumeAttachment` resolution already written for restore
      (`internal/rexposer`), including its refusals — no single attached node, or a not-ready node,
      means free scheduling rather than a guaranteed `Pending` wedge.
- [ ] **File-level restore**: restore a single file or directory into a PVC **already mounted** by
      a running application. Cheap once placement exists, but it inherits the *mounted* half's
      constraints, not the unmounted half's.
- [ ] **Notifications outside Prometheus**: a generic webhook, deliberately **not** a connector per
      destination (Slack/Discord/mail are a bottomless scope) and **no message templating** — a
      stable, versioned JSON envelope the receiver formats. Delivery is handled by a **separate
      notifier component** so the egress opens for it alone and the operator's own NetworkPolicy
      stays narrow: a tenant-supplied webhook URL otherwise lets a tenant direct operator egress,
      which points straight at the `clusterInternalCIDRs` confinement
      ([03-security-and-tenancy.md §7](03-security-and-tenancy.md)). Chart values constrain the
      notifier's own policy (default egress open, tightenable).
- [ ] **`api/` split into its own Go module** (`github.com/CrystalBackup/CrystalBackup/api`) so the
      external CLI and UI repositories can import the object definitions without inheriting the
      operator's dependency graph or Go floor — **before either repository exists**, because the
      import path becomes a breaking change for both once they do
      ([adr/0020](adr/0020-cli-and-ui-as-separate-repositories.md)). Ships with the test that keeps
      `api/` free of `internal/` imports and of dependencies beyond `k8s.io/apimachinery`.

**Decisions to take at milestone start, not now**: whether notifier delivery is durable (which
decides whether it is a stateless Deployment or needs state) and the exact webhook payload —
specifically what must never leave, since a namespace name is a customer name; and who wins when
an application starts while the mover holds an unmounted RWO volume.

## M8 — Proof: restore drills, restore alerting, cluster-scoped DR hardening (0.8)

**Why after M7 and not before**: drills *consume* the restore path, and M7 modifies it (placement,
file-level restore). Building the verification on a path that has just changed is worse than
building it once the path has settled. This milestone also reverses an M4 decision — M4 recorded
that restore-testing stays the administrator's job with no automated canary — and that reversal is
deliberate: an untested backup is not a backup, and "we tested it" is a different claim from
"it tests itself at your site".

- [ ] **`RestoreDrill`** — restore the latest backup of a namespace into a scratch namespace on a
      schedule, compare fingerprints, tear down, report. Most of the machinery exists: the m6
      fidelity gate (`test/crucible/tests/m6_fidelity_test.go`) already compares content, modes,
      setuid/setgid/sticky bits, numeric ownership, xattrs, POSIX ACLs, nanosecond mtimes, sym- and
      hard links, sparse files and non-ASCII names, and it restores into a namespace that **does
      not exist** so nothing pre-existing can mask a failure. The work is lifting it out of the test
      harness and giving it a CR, a status, metrics and an alert.
      **Same-cluster only.** No inter-cluster communication: making the operator orchestrate a
      second cluster would mean credentials to another cluster's API for what is a verification
      feature. The cross-cluster rebuild path stays a **harness activity** (the crucible), which is
      where it already lives. Restoring under a *different* StorageClass was considered as a way to
      weaken the "it worked because the identifiers were still right" effect and **rejected**: it
      changes volumeMode, filesystem and topology, so a failure would no longer distinguish the
      backup from the StorageClass — and for a verification mechanism a false failure is worse than
      a false pass, because a gate that cries wolf gets disabled.
- [ ] **Restore alerting** (the corollary, and it must ship with the drills): none of the eleven
      shipped rules covers restore, and `crystalbackup_restore_failures_total` is read by no rule
      at all. Prerequisite: `RestoreStatus` has no `completionTime` (unlike `BackupStatus`), so
      the same start-vs-end drift already fixed for `Backup` would otherwise apply; the counter
      also needs the state-derived companion that
      `crystalbackup_backup_last_failure_timestamp_seconds` has, against the materialise-at-1 flaw.
- [ ] **Cluster-scoped DR hardening** (R22, moved here from M9 — it is restore reliability, so it
      belongs with the drills): default-allowlist review, guidance on excluding GitOps-managed
      resources (ArgoCD/Flux) at restore time, coverage audit
      ([adr/0011](adr/0011-cluster-scoped-dr.md)).

**Decisions to take at milestone start**: drills by **frequency** or by **sampling** — frequency
has a constant cost and a coverage that decays as the estate grows, sampling follows the estate but
needs a rotation policy or the same volumes are always the ones proven; where the drill restores
and who pays for the scratch storage; cleanup guaranteed even if the operator dies mid-drill;
cluster-plane, namespace-plane or both; and whether a failed drill only alerts or also marks the
backup.

## M9 — Immutable locations (R18 — design done in the M0 API, implementation here)

- [ ] Immutable mode: S3 Object Lock bucket support, repo rotation (`rotationPeriod`),
      forget-only bookkeeping, expired-repo deletion, lock-file strategy (`--no-lock` reads;
      validate rest-server append-only and/or rustic lock-free writes in a POC —
      [adr/0005](adr/0005-immutability-mode.md)).
- [ ] **Erasure-on-immutable**: `ClusterErasure` stays `Blocked` until object-lock expiry, then
      completes; lifecycle of residual object versions left by retried uploads on versioned
      buckets (delta 13).
- [ ] e2e with **Ceph RGW Object Lock**: erasure blocked then completing; rotation retiring an
      expired repo.

## Coexistence — no longer a milestone

The former M9 coexistence-hardening bullet is **retired as a deliverable**, because coexistence is
structural and already shipped. [adr/0006](adr/0006-coexistence-with-backup-tools.md) states that
the interesting surface is *shared infrastructure, not CRDs*, and the mechanisms are in place: a
distinct API group with a denied-namespaces deny-list, distinct namespaces/identities/repositories,
no mutation of any `VolumeSnapshotClass` or of the cluster default, a `crystal-` prefix on every
VS/VSC the operator creates with manipulation restricted to its own objects, schedule-offset
guidance, and the `CrystalbackupPVCSnapshotPileup` alert — whose predicate counts **everyone's**
VolumeSnapshots on purpose, because during coexistence it is the incumbent's snapshots that stack
up against the shared ceph-csi headroom (`--minsnapshotsonimage` 250 → background flatten,
`--maxsnapshotsonimage` 450 → `ResourceExhausted`).

What remains is not a deliverable: **running side by side with an incumbent and observing** is the
M6 soak, which is already owed and should not be counted twice; and **coverage-diff guidance** for
teams choosing to consolidate is a documentation page. The one thing no design can fix — snapshot
headroom per RBD image is finite and shared — is instrumented and documented, not boundable.

## DR drills at fleet scale — backlog

Full-namespace fleet `ClusterRestore` onto a rebuilt cluster (repo-only bootstrap, namespaces
recreated), RTO measurement and runbook. Distinct from M8's `RestoreDrill`, which proves the
repository holds restorable bytes; this proves the **rebuild** path, needs a second cluster, and is
therefore a harness/ops activity rather than a product feature.

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
  governance-gated ([02-api.md](02-api.md)). **Not** the placement M7 builds: this one steers
  movers toward the *storage endpoint* as a policy on the location, while M7 pins a mover to the
  node holding a volume's attachment because an RWO cannot be mounted twice. Same word, unrelated
  mechanisms — if both ever exist, the M7 constraint wins, since it is a correctness requirement
  and this one is an optimisation.
- **Preferred backup window** (was R27; removed from v1 2026-07-15). If re-introduced, model it
  as **`start` + `duration`** (unambiguous across midnight), not `start`/`end`, with a skip
  Event/metric and a `WindowUnsatisfiable` controller condition
  ([00-requirements.md §6](00-requirements.md)).
- **Volume quotas per tenant** (arbitrated to the backlog 2026-08-02). Per-tenant metrics exist
  (`crystalbackup_backup_protected_bytes`); nothing bounds. The blocker is not technical but
  political and unanswered: on overrun, refuse the backup (and lose the data), accept it and
  alert, or degrade retention? Adopting it also amends [00-requirements.md §6](00-requirements.md),
  where storage quotas are explicitly out of scope for v1.
- **Opt-in telemetry and crash reporting** (arbitrated to the backlog 2026-08-02). The Ceph model:
  separately-enabled channels (`ident`, `basic`, `crash`), a command that shows the exact payload
  before it is sent, re-consent on schema change, immediate revocation, announced retention. It
  would be an **admin** decision through a Helm value and an operator flag, never a namespaced CR —
  the same asymmetry as the refusal of an operator key slot on a user repository
  ([adr/0004](adr/0004-encryption-key-management.md)). The redaction primitive already exists and
  is tested (self-check HMAC over a per-report random salt never written into the report). Two hard
  parts: **no namespace name may ever leave** — a namespace name is a customer name, so
  cluster-level aggregates only — and stack traces carry object names, paths and sometimes
  repository URLs, so a `crash` channel needs dedicated scrubbing rather than a `recover()` that
  posts. **The real cost is the receiving infrastructure**, not the client: endpoint, storage,
  retention, a published privacy policy and someone who actually reads the data. Do not start the
  client before deciding who runs the server.
- **Application hook presets** (PostgreSQL, MySQL, MongoDB). Small, high perceived value — the
  generic exec hooks are already good, but everyone rewrites the same `pg_start_backup`. The trap
  to avoid: a preset that suggests a *database-aware* backup, which the project explicitly does not
  do. The label stays "quiescence", never "Postgres backup".

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
