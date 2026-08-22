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
- [x] Active pre-check before VS creation (VolumeSnapshotClass resolved, secret present) —
      delta 9; S3 RGW tuning (`s3.connections`, wave test vs `rgw_max_concurrent_requests`) —
      delta 13. **Both delivered in 0.6.3**, and the bullet is closed rather than carried because
      the two halves that are missing from it are a DECISION, not a remainder:
      - **Delivered.** The snapshotter Secret named by the VolumeSnapshotClass parameters is
        checked before Expose — missing → the volume fails `SnapshotPrecheckFailed` with the
        Secret named in the Event; CSI-templated (`${volumesnapshotcontent.name}`) → the verdict
        is `NOT_CHECKABLE` with the reason, never "pass". A progress deadline bounds a snapshot
        nobody acknowledged (15m) and, from 0.6.3, one acknowledged and never ready (2h).
        `spec.s3.connections` reaches restic as `-o s3.connections=N`, bounded by a CRD `Maximum`
        because `BackupLocation` is tenant-writable against one shared gateway (adr/0009).
      - **Decided against: "snapshotter sidecar reachable", VSC ↔ RBD-image reconciliation and
        trash monitoring.** All three need `rbd trash ls` / `rbd snap ls` or an equivalent
        Ceph-side read, and [adr/0003](adr/0003-snapshot-exposure-csi-generic-first.md) accepts as
        a consequence of its whole design that there are **no Ceph credentials anywhere in the
        backup system**. Acquiring them to close a monitoring gap would trade the design's central
        property for an observation, and adr/0003's own risk table already assigns that ground to
        a platform alert on ceph-mgr. This is therefore not deferred work and should not be
        re-opened as a task; re-opening it means re-opening adr/0003. The reasoning is written into
        `internal/exposer/precheck.go` so it is read by whoever next reaches for it.
      **Still open, and it is the residue of the pre-check rather than of this bullet**: a healthy
      snapshot-controller with a dead per-driver sidecar binds a VolumeSnapshotContent within
      seconds and never sets `readyToUse`, which from the object alone is indistinguishable from a
      slow driver. The 2h deadline ends the hang; it does not diagnose it. Diagnosing it needs
      either the Ceph reads above or a maximum legitimate snapshot duration on storage we do not
      own, and neither is attempted.
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

**Status as of 0.6.3 (2026-08-07) — the crucible was testing a chart nobody installs.** The code
scheduled here for `0.6.3` shipped: delta 9's API side and delta 13. But that is not what the
release is about. **Four defects blocked one user's first hour with 0.6.2** on their own RKE2 +
Rook-Ceph cluster, and not one of the four was reachable by any test in this repository. Two of
them were hidden by a single file: `test/crucible/deploy/deploy.sh` set `namespace.create=false`,
so the campaign never met the Helm ownership error that killed the documented install order on its
first command, and it set `networkPolicy.apiServerPort=6443` with a comment quoting the operator's
startup failure verbatim — the workaround had been understood, written down and shipped here for
longer than the bug had been visible to anyone outside. The third is a documentation gap the
crucible had no reason to notice: the soak, 0.6.2's headline, appeared in none of the six install
pages, so no reader following the documentation could reach it at all.

Both overrides are removed and the rule that replaces them is written into that file: every
`--set` is either something the documentation tells a user to set, or it is a bug in the defaults.
`test/chart/install_test.go` holds the DEFAULT render, which nothing in this repository had ever
looked at. This is the same failure mode as the M5 lot E's announced-but-inert features and as
0.6.2's collector reporting NOT_MEASURED beside a campaign that ran dozens of backups: **the
instrument agreeing with itself.** Three rounds, three different shapes of it.

The fourth is not a chart bug and is the one worth keeping. A nightly cascade whose mover
pods could not map an RBD clone left a backup unfinished for **thirty-six hours**; nothing failed,
so none of the eleven alert rules could fire, and `concurrencyPolicy: Forbid` then meant one
backup in fifteen days behind a green dashboard. The per-phase timeout had been marked "deferred
to task #22" since M1. It now exists in two forms with predicates that make their numbers safe (a
mover pod that never reached Running at 30m; a snapshot acknowledged and never ready at 2h), the
kubelet's own Warning travels into `status.volumes[].reason`, and a twelfth rule
`CrystalbackupBackupStalled` reads a state-derived series so it covers the phases no in-controller
clock can see. A mover Job that has *vanished* still requeues forever — there is no durable clock
left to measure it against — and the alert is what covers that.

`preflight.sh` would have reported that user's cluster READY, because a resolvable
VolumeSnapshotClass proves snapshot **availability** and not **usability**. It now reports every
resolvable class as USABILITY NOT ASSESSED, counted as a reservation with no code path to a pass,
and `website/public/snapshot-probe.sh` answers the other half by actually snapshotting, restoring
and mounting per StorageClass.

**And the fifth, which the incident exposed rather than caused.** Diagnosing that user's cluster
established the fact with a control — an identical pod on a 5.15 node mounted the RBD clone, on a
5.4 node it failed `rbd: map failed: (22)` — and then established that the operator had no way to
act on it. Nothing in the product could say *where* a mover may run: the chart's `nodeSelector` is
the operator pod's, and `mover.JobRequest` carried only `NodeName`, set by the same-node restore
path alone. The advice available was "upgrade every kernel first", which is not advice anybody can
act on this afternoon. `mover.placement` (nodeSelector, tolerations, affinity, applied to **every**
mover Job, admin-only) closes that, and the case generalises well beyond one kernel bug — a tainted
pool reserved for I/O, a zone with cheap egress. Its sharp edge is documented where the decision is
made: a `nodeSelector` few nodes match does not make movers *prefer* those nodes, it serialises
every backup in the cluster through them.

**One more instrument found agreeing with itself, in the campaign for this very release.** The new
`m6/stall` spec injects its fault by parking the RBD node plugin DaemonSet, and it timed out at
300s waiting for the pods to go away. The failure read like a slow cluster and invited a bigger
timeout. It was not: rook ≥ 1.17 hands the CSI driver plane to a ceph-csi-operator that OWNS that
DaemonSet, and the park was reverted in the same second it was applied — six `SuccessfulDelete` and
six `SuccessfulCreate` events with the same timestamp, and "node plugin daemonset updated
successfully" in the reconciler's log between them. No timeout would ever have worked. **A
reconciler is not a race you can wait out**, and a fault injection that is silently undone is the
same defect class as a green campaign against a chart nobody installs. The spec now stops the
reconciler before it touches the object, which is the pattern `m6_precheck_test.go` already used on
`rook-ceph-operator` for the same reason.

**Validated: 90 of 90 crucible checks, 0 failed, 0 skipped, in 2h43m24s** — the whole suite
unfiltered on a six-node RKE2 v1.35.7 + Rook-Ceph cluster with real S3, against the operator digest
this release ships. Eight checks more than 0.6.2, and the eight are the point: the `m6/stall` and
`m6/placement` lanes are new, and the first of them took three runs to become evidence rather than
decoration.

The two unmet exit criteria are unchanged: the two-week soak and the pilot rollout. The
PodSecurity review has moved from "answer known, review not done" to partly executed — the posture
`crystal-backup-system` must carry is now enforced by the chart in three places and checked by the
operator at startup — but the review as a document is still not written.

**Status as of 0.6.4 (2026-08-07) — 0.6.3 created the trap it warned about, and the same evening
walked into it.** 0.6.3 stopped rendering the chart's `Namespace` object, which is correct; under an
Argo CD Application with automated prune, an object that stops being rendered is an object to
delete. The operator namespace went, and with it the cluster KEK. `install-argocd.md` had warned in
general terms since M3 that "a prune can delete the namespace holding your cluster KEK";
`upgrading.md`, the page an upgrader actually reads, said nothing.

What that exposed is the release's subject and is worth recording as a shape rather than a bug. The
escrow reconcile knew the rule — its own comment said `EnsureDEK` must not mint over a recoverable
DEK, and its caller already converted a block into `Ready: False` — and it returned "do not block"
from all six branches that failed *before it could ask the question*. Uncertainty was encoded as
safety. The operator minted a DEK four seconds after the KEK was restored, then reported `Ready` for
an hour while every mover failed against 38 snapshots it could no longer open. Only the conflict
guard's refusal to overwrite the bucket object kept it recoverable.

The fix is an invariant instead of a case list, and the lasting part is the test that guards the
invariant rather than the cases: it reaches each branch, reads whichever reason it lands on, and
fails on any reason outside a five-entry allow-list whose entries each argue why minting cannot fork
the repository. **The escrow had no tests at all before this** — the code with the worst failure
mode in the product, and the least visible when wrong.

**Validated: 90 of 90 crucible checks, 0 failed, 0 skipped, in 2h44m1s** — the whole suite
unfiltered on a freshly provisioned six-node RKE2 v1.35.7 + Rook-Ceph cluster with real S3. It took
three campaigns: the first was killed mid-run by a session interruption, the second found the
over-blocking described above, and the third is this one. The check that decided it is
`increments the failure counter and pages, with no hold to wait out` — 17m11s green, having timed
out at 300s while the gate refused too much.

Worth recording as a limit rather than a caveat: the suite has NO lane that drives the escrow into
an unresolved state, so the campaign measures that the tightened gate did not break the working
paths — the right risk after narrowing a guard — and says nothing about the behaviour under
uncertainty, which lives in fourteen unit cases and the invariant guard against a stubbed S3.

Deliberately NOT in 0.6.4, and both are named in the CHANGELOG so they are not mistaken for
oversights: a chart-side guard that makes Argo CD refuse to prune the operator namespace (that is a
change to how the chart is installed, not a patch), and any claim that the escrow is now
bulletproof — an administrator who restores the wrong KEK still has an unreadable repository, and
0.6.4's contribution is that it says so in a reason of its own.

**Status as of 0.6.5 (2026-08-10) — one unanswerable question about one volume became its whole
namespace's verdict, and every number around it was confident.** A nightly schedule on a live
cluster produced nothing for **thirty-one hours** behind a dashboard reading *0% backup success*.
The cause was one `PersistentVolumeClaim` out of thirty-three: a hand-made NFS volume naming
`storageClassName: slow`, a class that exists as no object. That is legal — for a static binding the
class name is only a matching label between claim and volume — so the administrator had done nothing
wrong. The operator resolved snapshot capability **through the class**, errored, and because
`Reconcile` advances one volume per pass by position it re-drove that same volume forever: the five
volumes behind it were never attempted at all, which is why none of 0.6.3's three phase deadlines
could fire on them. A deadline needs a phase, and they had never left `Pending`. The run never went
terminal, so `concurrencyPolicy: Forbid` skipped every following night.

The fix is that the question was being asked of the wrong object — the CSI driver now comes from the
bound PVC's `PersistentVolume`, and the StorageClass is consulted only for a claim bound to nothing
— and nothing judges permanence any more: an unresolvable volume records its cause, stays `Pending`,
loses its turn to volumes never tried, and is bounded by a fourth deadline
(`pendingResolveDeadline`, 1 h) whose clock lives on the volume rather than on the run. `Forbid`
gained a predicate that distinguishes a run that is working from one that is wedged, derived from
the deadline ladder rather than picked.

What makes this an M6 release rather than a bug fix is the class underneath, found **three times in
three lots** in the same family of functions: **a pass that computed everything and persisted
nothing**, because the error returned upstream of the single status write — so the enumeration never
reached etcd and the bounds that exist to end exactly this were unreachable. `advancePending` no
longer has an `error` result at all, which makes the signature the invariant. Eleven lots then
closed the reporting half, and the numbers are the point: `namespacesFailed: 32` for a run whose
children were 29 `Completed` and 3 `PartiallyFailed`, now one classification with a total checked on
every write; a reaper that logged "reaped" **186 times** for three objects it never deleted, now
allowed to call only a confirmed absence a reap; `snapshotsForgotten` asserting destroyed data after
a failed erasure, now measured after the work or claiming nothing; thirty-one hours without a backup
rated `warning`, now escalating on a bound derived per schedule; and `selfcheck --format text`
answering the question a fresh installer actually asks, per PVC, in `5 + k` requests at any estate
size. The recurring shape across all of them: the components that only **observe** were right, and
the components that **act** were wrong.

**Validated: 93 of 93 crucible checks, 0 failed, 0 skipped, in 2h48m54s** — the whole suite
unfiltered, three checks more than 0.6.4, the new ones reproducing this incident against real bytes
in the repository while the unsnapshottable volume sits first in the queue. **It took two attempts,
and the first was red.** Run 1 on this same tree: 62 passed, 7 failed, 24 skipped, and **21 of 93
checks never ran** because the suite hit its four-hour timeout — five of the seven failures were the
**same** leaked `VolumeSnapshotContent`, one object sitting out five 600-second deadlines across
four milestones, about 95 minutes of budget spent watching it not disappear. That leak was a real
defect found only by the campaign: three code paths were blind to the same object class because
`mergeLabels` treats an empty desired value as equal to a missing key, so on the namespace plane —
where there is no run — the deleting path, the verifying path and the backstop all missed it. Fixed
at the source: no key is ever stamped with an empty value again. The two timed-out checks now pass
in 50.9s and 15.8s.

What the campaign does **not** establish: it injects an unsnapshottable static volume, a missing
snapshotter Secret, an unclaimed snapshot and a mover that never starts, but not a durably failing
`Expose`, an unreadable source PVC or a hooks-resolution failure — those live in envtest and
mutation testing against a stub. Deliberately not closed, and named so they are not read as
oversights: two hook-chain defects (an aborted quiesce leaves applications frozen and says nothing;
a `Fail`-policy post-hook failure thaws the wrong pod), the collision trigger behind the miscounted
namespaces, and the errored-pass sweep, which was verified **in the Backup controller only** — the
honest statement about the rest is that nobody has looked, not that they are clean. The two
unmet exit criteria are unchanged: the two-week soak and the pilot rollout.

**Status as of 0.6.6 (2026-08-10) — a check that cannot fail is not a check, and the tree was full
of them.** The release started as one complaint about CI mail and the search for the cause became
its subject. Measured before touching anything, because "CI is flaky" is a claim and 9-of-10 is a
fact: on `main`, **Lint had failed 9 of its last 10 runs and Unit tests 9 of 10**, red on **every
push since 2026-08-03** while Security, E2E, vex-refresh and the site deploy stayed green. Three
findings, always the same three, and each of them **cannot fail on the maintainer's machine**: a
`unix.Statfs_t.Bsize` cast that is redundant on linux and required on darwin inside one `//go:build
unix` file; the last user of a deprecated events API; and a test that passed only because the
machine has a kubeconfig, so `soak-collect` exited about the cluster instead of about the path the
operator typo'd. And the reason all three hid: `rm -f bin/golangci-lint` rebuilds the linter but
does not clear `~/.cache/golangci-lint`, so a local `make lint` can report zero issues from stale
results while CI, cold, rejects the same tree. `lint` now purges that cache by default and gained a
`GOOS=linux` pass, because a darwin-only run structurally cannot see a linux-only finding in a
build-constrained file. Eighteen failures over eight days is not a signal, it is furniture — the
same defect as a counter nobody can believe.

That shape then appeared five more times. The errored-pass class was swept through the four
controllers 0.6.5 admitted nobody had looked at, by both methods again — control flow from each
`Reconcile`'s first status mutation, and a mechanical classification of every error-carrying
`return` — and the two agreed everywhere: `ClusterErasure` has **zero** instances, shown rather than
asserted, and the other three had **seven**. Two of the seven cost real answers: a clean
namespace-wide manifest apply re-read as *"may have been applied"* with `failedCount 1` once the
mover pod was gone, and a counter that would have published `plannedVolumes: 0` over a real
four-of-six in front of somebody deciding whether to abandon a restore. The sweep settled a rule —
per-item failures are recorded and the pass continues, whole-object failures persist then propagate.
Two dead protections fell out on the way: `ClusterErasure`'s one-hour Immutable recheck was **never
once used** (`park` always answers a non-zero `RequeueAfter`, so the guard returned 15 s every pass)
and its Warning sat before the park, emitting **four Warnings a minute for weeks** on the most
sensitive compliance path there is; and the external-sync controllers' single `write` helper
returned without writing whenever an error was present, under a comment promising it persists once
per reconcile. No live defect today, verified — but it is the funnel every exit passes through, and
it aliases figures that are not recomputable, so it now persists whenever the pass actually changed
the status. The two hook-chain defects 0.6.5 left open are closed here: the release runs before the
termination and the terminal write is held while a thaw is owed, and only `Succeeded` pre entries
are thawed, because a thaw against a pod nothing froze is a command its owner never asked for.

The other half of the release is prediction. A production cluster had **never once** backed up a
CephFS volume — eight volumes across four namespaces, every one `SnapshotReadyDeadlineExceeded`, for
as long as the cluster had existed. The cause was outside the product and the operator's verdict was
exactly right, but `preflight.sh`, whose whole job is to tell an administrator before installing
what will and will not be backed up, called that class perfectly usable, because a
`VolumeSnapshotClass` for the driver exists. A static predictor, confidently predicting the wrong
thing. The symptom needs one `LIST` and no new permission — VolumeSnapshots **bound** to a content
and still not ready past a one-hour grace — and it now appears on the three surfaces that had the
information and were silent: the preflight script, `selfcheck`, and 0.6.5's per-PVC census.
Deliberately an **observation, not a verdict**: no phase moves, and a class with no snapshots at all
is not maligned.

**The soak has started.** The last lot of this release is what made that safe: a GitOps autosync had
replaced the collector's pod at the moment the fortnight's archive was to be exported, the archive
sat on a ReadWriteOnce volume the new pod could not map, and only a figure somebody had transcribed
by hand minutes earlier survived. `strategy: Recreate` did what it promises and the archive was lost
anyway — the comment claiming it prevents this is corrected rather than deleted, because a
protection that was believed in is worth recording as one that was not. The collector now writes its
own per-class high-water table to **stderr** on `SIGTERM`, the one channel a terminating pod has
that is not the volume it is about to release, and `hack/soak/` gained the reset procedure it had
never had. With that in place, the soak is now **running on a real build cluster on 0.6.5, for a
15-day window** — started, not completed, and it is the first time either of M6's two remaining exit
criteria has moved from calendar work not yet begun to calendar work in progress. The pilot rollout
has not started. The PodSecurity review as a written document is still not written.

**Validated: 93 of 93 crucible checks, 0 failed, 0 skipped, in 2h50m16s** — the whole suite
unfiltered on a six-node RKE2 v1.35.7 + Rook-Ceph v1.19.0 cluster with real S3. The same 93 as
0.6.5, and that is measured rather than assumed: exactly one file under `test/crucible/` changed
since the tag — a wait bound in an M1 helper — and no check was added or removed. It took two
attempts, and the first was red **on the harness**: that helper timed out at 300.001s against a
repository the operator had initialised in the same second, its own measurement being 462ms. A bound
with no margin for a cold first pass is a bound that fires again, so it is ten minutes now. The red
attempt is published anyway, which is the rule these reports follow whether the failure lands on the
product or on the instrument.

**Status as of 0.6.7 (2026-08-22) — three artefacts reported a fact when all they had was a refusal,
a blind spot or a discarded string, and the soak found every one of them.** Not one finding in this
release came from the crucible, and not one was reachable by any test in this repository: all three
came out of a fortnight's archive exported from somebody's real cluster and read. That is the first
time the soak kit has produced **findings** rather than merely uptime, and it is worth saying
plainly — M6's remaining exit criterion began paying off before it had finished. The longest-lived
of the three: the resident collector's daily self-check reported **30 of 30 PVCs** as
`ExposerUnresolvable`, under a headline saying they would NOT be backed up, on **nine consecutive
days**, while 28 of those volumes completed successfully every one of those nights. Every number in
the report was arithmetically correct and its meaning was false. The cause is one missing line of
RBAC — since 0.6.5 the exposer resolves a bound PVC's driver from its `PersistentVolume`, the
collector's ClusterRole was never extended to match, the read came back `Forbidden`, and **the
refusal was recorded as a fact about the storage**. The grant is the smaller half: any cluster that
denies that permission would have gone on reading the same false headline, so a refused or failed
read now has a class of its own, `ExposerUndetermined`, kept out of the count of volumes that will
not be backed up. The line is *did the apiserver answer* — a `NotFound` is a finding about the
storage, a `Forbidden` or a timeout is a finding about the observer — and the same conflation was
closed in two further places. What keeps it closed is not the fix: the reads the self-check performs
are now declared in **one place** that both the code and a chart test consume, so adding a read
without extending the role fails a gate on the maintainer's machine. Writing that declaration
immediately found a read no hand-audit had seen, which is exactly how the missing grant shipped.

The second was a leak the kit's own check caught: a customer's PVC name verbatim in an exported
Event, with the run name and the namespace in the same sentence properly tokenised. **The PVC had
never been registered.** PVC names entered the redactor only through a metric label that exists once
a volume has a `VolumeSnapshot`, and the volumes a schedule complains about by name are precisely
the ones stuck before they ever get one — a class structurally invisible to the redactor, which no
amount of care in the substitution pass could have covered. Both obvious repairs are rejected in
comments: dropping the name destroys the message's only diagnostic value, on the administrator's own
cluster, where their PVC name is not a secret from them; and substituting every known identifier
inside free text is worse, because on the cluster that produced this archive the PVCs are named
`data` and `backups`, so a blind pass would rewrite the English word and the CRD plural throughout
while still not being complete. The rule is bounded instead — identifiers we control are written
**quoted** at the source, and the exporter substitutes only quoted exact matches through a second
registry the general free-text pass never sees. Two written claims that the leak disproved, in
`manifest.go` and in the kit's SPEC, are corrected rather than deleted.

The third is instrumentation and nothing else. **Ten nights out of ten** on that cluster, every
`ClusterBackup` came back `PartiallyFailed` with `namespacesBlocked: 32` while its namespace-level
children read `Completed` over 28 of 29 volumes — the backlog entry below, reproduced on demand
rather than observed once. Because that entry says the trigger is an unenumerated set whose first
job is to find the other paths, **nothing classificatory was changed here**: the diff over
`classifyCoordinate`'s decision lines touches only its second return value. What that value
discarded is the finding — all four foreign branches rendered one reason and differed only in prose,
naming each branch's conclusion and never the facts behind it, and the run object's
`creationTimestamp`, the quantity the entry's own candidate fix turns on, **was not a parameter of
the function at all**, so no volume of production evidence could ever have confirmed or refuted that
hypothesis. `ClusterBackupStatus.blockedReasons` now groups blocked namespaces by cause — five
codes whatever the estate size, where the ten-entry failure list could only sample 10 of 32 and
where a per-namespace map is what [adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md) refuses —
with `withDataAtCoordinate` and `stampedByThisRun`, the second turning the falsified hypothesis into
a measurement. **There is a CRD step in this release**, unlike 0.6.6: `blockedReasons` is a new
optional field on `ClusterBackup.status` and the CRDs must be applied before or with the chart.

Neither exit criterion closed. The two-week soak is **running** on a real build cluster, started on
0.6.5 — and the operator there is still **0.6.5**, deliberately: this release is not being deployed
onto it until that fortnight ends, because restarting the window to pick up the fixes the window
itself produced is how a two-week measurement never completes one. The pilot rollout has not
started. The PodSecurity review as a written document is still not written.

**Validated: 93 of 93 crucible checks, 0 failed, 0 skipped, in 2h40m25s** — the whole suite
unfiltered on a freshly provisioned six-node RKE2 v1.35.7 + Rook-Ceph v1.19.0 cluster with real S3,
against the digests this release ships: an operator on go1.26.6, a mover carrying restic built on
go1.26.7, and a sync carrying rclone 1.75.0.

**It took NINE campaigns, six of them red, and not one of the reds was the product.** Two of the
greens are deliberately not the verdict and are named here rather than dropped: one validated the
go1.26.5 tree the seven reachable advisories disqualified, the other a tree whose mover and sync
still carried the unpatched restic and rclone — a real verdict about images nobody would publish.
Beyond the two harness defects below, a cold cluster's first repository initialisation was measured
at 11m08s and 11m02s against a 600s budget and 31s warm; one campaign is disqualified by an operator
error, having been relaunched on a cluster still carrying a deliberately corrupted repository from an
aborted lane; and the crash-path leak invariant could never observe the orphan reaper it depended on,
so every past green of that spec was a run where the ordinary teardown won. Run 1
gave a teardown **0.272s** on a leak check that fails fast on residue predating it — a premise that
is false for a spec whose subject is a thirty-minute deadline, and the operator's log settles it,
the sweep having logged in the same second the check entered that the residue was still draining.
Run 2 failed behind Ginkgo's unpinned container shuffle, and fixing that found the larger half: the
shared `dr` location was not merely created late but **deleted mid-suite** by six sites, each
deletion garbage-collecting the `BackupRepository` the other lanes poll. Both had been winning that
coin flip for two releases, which makes 0.6.6's green on the first of them luck rather than a
verdict. The seed stays unpinned on purpose — the shuffle is this suite's only instrument for
finding cross-container coupling, and it has now found two real defects that a fixed seed would have
frozen into permanent green.

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

      **A mount is bounded before it is attempted, and the bound is not optional.** Raised during
      0.6.5 and recorded here because it constrains the design rather than the implementation: a
      volume that cannot be SNAPSHOTTED and a volume that cannot be MOUNTED are different axes, and
      this exposer is the first one to depend on the second. The vocabulary already exists and must
      be reused rather than re-invented — `ErrUnsupported` means "never, on this storage",
      `ErrPrecheckFailed` means "not on this cluster today" (`internal/exposer/registry.go`), and
      every mount failure belongs on the second axis. Three shapes an administrator will actually
      hit, all **indistinguishable at mount time**: a backend that is gone for good (a PVC restored
      from an old backup), an unused PVC still pointing at a decommissioned backend, and a live
      backend under maintenance that is back tomorrow. Only TIME separates them, so the product must
      not guess: the failure is retryable, the history is what tells an operator whether it is
      chronic or a blip, and "no pod mounts it" must never become a silent exclusion — a detached
      PVC is precisely a volume somebody wants backed up.

      The hazard that decides the mechanism: a dead NFS server does not FAIL a mount, it HANGS it.
      A hard mount blocks uninterruptibly in the kernel, the mover pod sits in `ContainerCreating`,
      and deleting it can wedge on the node — so the maintenance window of a volume nobody uses
      becomes a degraded node. It cannot be fixed with mount options, because for a PVC-backed volume
      they come from `pv.spec.mountOptions`, which belongs to the user's PersistentVolume and is not
      ours to mutate. Therefore: a **bounded, out-of-band reachability probe before the Job is
      created**, plus `activeDeadlineSeconds` on the Job as the backstop. Never an unbounded mount
      attempt, and never a probe whose own timeout is the mount's timeout.
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
specifically what must never leave, since a namespace name is a customer name; who wins when
an application starts while the mover holds an unmounted RWO volume; and whether a volume on a
backend this cluster can no longer reach is worth backing up in degraded mode at all, or whether a
named `Skipped` is the honest final answer for it — the question a statically provisioned NFS volume
on a live cluster put on the table in 0.6.5, and one that decides how much of the probe above is
even needed.

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
      buckets (**delta 15** — *renumbered in 0.6.3. This carried `delta 13`, which M6 §
      "S3 RGW tuning" also carries. Two unrelated lessons under one tag is a tag that resolves to
      the wrong issue; `delta 13` now means the RGW tuning only, and 15 was the next free number
      after the restore-fidelity gate's 14.*).
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

- **`PVCSelector.matchLabels` with an empty value diverges from Kubernetes semantics** (found 0.6.5
  while consolidating the predicate; pinned as the behaviour that ships, not corrected). Kubernetes
  reads `matchLabels: {key: ""}` as *the label must be present and empty*; this rule compares
  `labels[k]` against the required value, and a missing key reads as `""`, so an empty required value
  is satisfied by a PVC that does not carry the key at all. Correcting it would change which PVCs an
  existing schedule covers, which is not something to do as a side effect of moving a function —
  it needs its own note in the release that changes it. Pinned by `TestPVCSelectorMatches`.
- **A run cannot recognise its own unstamped terminal child** (found 0.6.5, deliberately not fixed
  there). `classifyCoordinate` admits into its adoption window only a child holding no result of any
  kind, so a run whose children were fanned out before `crystalbackup.io/parent-uid` existed — an
  operator upgraded while a run was in flight — declares every one of its own terminal children a
  foreign occupant of that coordinate. On the cluster that surfaced this, all 32 terminal children of
  one run were classified that way.

  0.6.5 made the *reporting* honest instead: `namespacesBlocked` is now a separate counter from
  `namespacesFailed`, so "this run never backed up this namespace" no longer masquerades as "it tried
  and failed". That is the whole of the fix, and it is deliberately partial — 32 namespaces that were
  in fact protected still read as blocked.

  **The unstamped child is not the only way in, and the belief that it was is now falsified.** After
  0.6.5's counters were rewritten, the same disagreement was observed live on a FRESH run whose
  children carried the correct stamp, matching the run's UID exactly: 31 children reading `Completed`
  beside `namespacesFailed: 31`. So this entry is not "one upgrade path" — it is an unenumerated set,
  and the first task of the lot that takes it is to find the other paths rather than to fix the one
  that is written down.

  Not fixed in 0.6.5 for two reasons. The first is now WEAKER than it looked: an unstamped child is a
  **migration artefact that self-heals** once the in-flight run drains, but the live observation above
  shows the classifier can misjudge a correctly stamped child too, so self-healing does not cover the
  whole entry. What still holds is that 0.6.5's counters no longer depend on the answer — a collision
  cannot reach `namespacesFailed`, and a `Completed` child counts as succeeded unconditionally. And
  the plausible fix — widening the window to also accept a child whose `creationTimestamp` is at or
  after the run object's — reopens the guard installed by commit `d3d2659` against *a run reporting
  success for data it never wrote*. That discriminator looks sound (run names carry a timestamp, so a
  same-named earlier run's child is necessarily older than the current run object), and "looks sound"
  is precisely the standard that should not be enough for this guard. It deserves its own lot and its
  own campaign, not a slot in a release already carrying six.

  **0.6.7 instrumented it; the defect is unchanged and this entry stays open.** A nine-day archive
  from a production cluster made the disagreement the normal path rather than an occasional one —
  every `ClusterBackup`, **ten nights out of ten**, `PartiallyFailed` with `namespacesBlocked: 32`
  beside namespace `Backup`s reading `Completed` over 28 of 29 volumes — and the archive answered
  nothing, because the run object recorded a verdict and never the facts behind it. It now records
  both. `classifyCoordinate` returns a stable code per branch plus the discriminators it decided on
  (the occupant's own stamp as `mine`/`other`/`none`, its phase, whether it held results, and its
  `creationTimestamp` relative to the run object's). That last discriminator is exactly the quantity
  the candidate fix above turns on, and it **was not a parameter of `classifyCoordinate` at all**
  before this release — so no volume of production evidence, however many nights it spans, could
  ever have confirmed or refuted that hypothesis; it was not being looked at.

  `ClusterBackupStatus` gained `blockedReasons`: the blocked namespaces broken down BY CAUSE, so it
  is bounded by a closed set of codes rather than by the namespace count, with
  `withDataAtCoordinate` — the counter-side honesty, since the whole difficulty is that the run says
  "blocked" over coordinates that hold a backup —
  and `stampedByThisRun`, which counts the coordinates the run itself demonstrably created and the
  fan-out still refused. That last counter is the falsifier above turned into a measurement: the
  next lot reads it off a night's objects instead of arguing about it.

  Not one classification moved. No branch condition, no ordering, no adoption window — the codes are
  attached to branches that already existed, and the guard's tests are unchanged.

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
- **The orphan reaper's cadence is not exposed, so no campaign can reach the backstop** (found
  0.6.7; the test-side tolerance shipped there, the product change deliberately did not).
  `internal/controller/reaper.go` sets `defaultReaperInterval = 10 * time.Minute` (line 46) and
  `defaultReaperMinAge = 30 * time.Minute` (line 53). `MinAge` and `Interval` are struct fields
  (lines 167-168) whose defaults `Start` applies, so the product is one step from configurable —
  and **nothing exposes them**. There is no CLI flag (`cmd/main.go` line 728 constructs
  `OrphanReaper` with `Client`/`APIReader`/`Recorder`/`OperatorNamespace` and leaves both zero) and
  no Helm value. An orphan is therefore eligible at creation + 30m and swept no later than +40m —
  the sweep just before eligibility misses it — which puts the collection outside any budget a
  crucible spec can sanely hold.

  The consequence is the point of this entry, and it is worse than a slow test. Until 0.6.7 the two
  crash-path specs (`test/crucible/tests/m1_reliability_test.go` line 252,
  `m4_teardown_test.go` line 151) were green **only when the ordinary teardown won the race**. The
  campaign that broke the tie recorded it exactly: a `VolumeSnapshotContent` created 21:26:10 with
  `deletionTimestamp <nil>` and `ownerRefs []` — nothing had asked for its deletion and nothing
  would garbage-collect it, so it was waiting for the reaper — while the leak check entered at
  21:31:16 and gave up at 21:41:16, twenty-five minutes before the object became eligible. So the
  reaper was never exercised by any campaign, ever.

  0.6.7 bought the observation with patience instead: `m1CrashPathLeakCheckBudget = 50 * time.Minute`
  on those two sites, derived rather than rounded (30m eligibility floor + one 10m interval for a
  sweep that just missed + 10m for the sweep's own delete and read-back plus the external
  snapshot-controller's Delete-policy cascade, measured at just over five minutes under load). That
  is up to 100 minutes of waiting added to a suite that has measured 2h50m16s green and 3h10m55s
  red, so a pathological run now lands between roughly four and a half and five hours against the
  330m timeout in `test/crucible/scripts/run-tests.sh` — inside the ceiling, with less headroom than
  any other raise in the suite has left.

  The fix is a product change and a small one: flag + Helm value + defaulting on the two fields, so
  a crash-path spec sets `MinAge` to a minute and **asserts** the collection in minutes rather than
  enduring forty, giving that wall clock back. Two alternatives are rejected. Leaving it test-side
  and living with the 50m budget buys the assertion but never the backstop — the spec still cannot
  distinguish "the reaper collected it" from "the reaper would have, eventually". And shortening the
  production defaults themselves trades away what the 30m floor exists to be: a race guard, since an
  exposure's temp clone PVC or mover Job can exist for a few reconciles before the owning Backup's
  status catches up, and reaping one out from under a slow-but-live reconcile corrupts an in-flight
  backup. Say the finding plainly, because it generalises past the reaper: **a backstop no campaign
  can reach is a backstop nobody has verified.**
- **Pre-pull the mover image at provisioning time** (found 0.6.7, after two campaigns died on the
  same bound). Measured on two independently provisioned clusters, and the second measurement is the
  one worth noting: `ClusterBackupLocation dr` created 07:29:56Z and `BackupRepository dr
  Initialized` 07:41:04Z — **11m08s** (668s), missing a 600s budget by 68 seconds; then, on the next
  cluster, **11m02s**, reported by the wait itself through the `repository.init` report entry the
  same lot introduced (`initialized after 11m2s (budget 20m0s; at entry: NO repository had yet
  initialised on this cluster …)`). Six seconds apart on two clusters is what turned this from a
  hypothesis into a figure — and it is the instrument reporting its own cost, which is the whole
  reason that entry exists rather than a log line nobody reads. In the same campaign every later
  repository initialised without trouble (m4-killed 10:08, m4-corrupt 10:13, m5-erase 10:28) and a
  manual probe on a warm cluster measured a fresh repository at 31 seconds. So the 637-second
  difference is neither provisioning nor the object store: `restic init` runs in a Job built from the
  mover image, which carries restic **and** rclone, and on a node with an empty cache that Job
  cannot start until the pull finishes. It is paid once per *node*, not once per cluster —
  `pullPolicy: IfNotPresent` (`charts/crystal-backup/values.yaml` line 84) against three workers
  (`test/crucible/terraform/servers.tf`), so `dr`'s init warms exactly one of them and the first real
  backup schedules movers onto the other two, each paying the pull again.

  0.6.7 raised two bounds to tolerate it — the shared-repository init to 20m
  (`m1ColdRepositoryInitBudget`, with `m1WarmRepositoryInitBudget` deliberately left at 10m for the
  other eighteen call sites) and m6/headline's run wait to 20m — and the survey that came with it
  named the bounds still exposed. That survey is **not repeated here**: it lives at the constants it
  argues about, in `test/crucible/tests/m1_helpers_test.go` (`m1ColdRepositoryInitBudget` and
  `m1RepositoryInitBudgetFor`) and in the comment above the 20-minute `m1WaitClusterBackupTerminal`
  in `test/crucible/tests/m6_headline_test.go`. Read those before raising a third one, because the
  cold-start comment says what a third raise would mean: the bound is the wrong instrument.

  The fix is to pull the mover image at `mise run seed` time (`test/crucible/mise.toml`), so that no
  test bound carries it and the cost is paid once, visibly, during provisioning. The honest
  counter-argument belongs with the work rather than after it: **a pre-pull hides a genuine
  first-pull regression from every campaign** — a mover image that doubled in size, or a registry
  serving at a quarter of the bandwidth, would stop being observable anywhere. So whatever does the
  pre-pull must *report* its duration rather than swallow it, into somewhere a campaign actually
  reads, in the same spirit as the self-measuring wait 0.6.7 added.
  **The bounds this cost measured, and which are still exposed.** 0.6.7's commit message promised
  this list lives here, so here it is rather than in a comment nobody greps. Raised: the shared
  repository's init (10m → 20m, `m1ColdRepositoryInitBudget`) and `m6/headline`'s wait for the first
  real backup (10m → 20m), the latter because ten minutes *was the pull alone*. Still exposed, each
  needing its own measurement rather than a speculative raise: `m6/precheck`'s two 10-minute run
  waits (lower exposure — both are runs where a mover is expected not to complete normally); the
  six 15-minute run waits (m4 maintenance ×3, m5, `m6/s3tuning`, `m6/stall`), which leave ~4.4
  minutes of real backup after a cold pull; and the `sync` image, a third image no mover ever warms,
  whose first pull is paid by whichever external sync runs first — `m5/externalsync` already sets 20
  minutes and its comment already names the reason, chosen independently and arriving at the same
  number. Not exposed, checked: the 3-minute operator-pod wait (`IfNotPresent`, and the image is on
  the node the pod just left) and the two 1-minute waits that re-wait an already-terminal run.

- **Triage leak-check hits by provenance instead of grepping the archive verbatim** (found 0.6.7 in
  the soak export; the narrow quoted-match rule shipped there, this did not). `hack/soak/collect.sh`
  searches the unpacked archive for every namespace, PVC, location and bucket name of the cluster,
  **verbatim**. On the cluster that produced the archive the PVCs are named `data` and `backups`, so
  the check reports the CRD plural `backups.crystalbackup.io` and the English word "data" as leaks —
  92 hits in the log stream alone, every one of that shape — and it therefore fails on every export
  from that cluster, forever. A check that always fails is a check nobody reads, which is the same
  defect class 0.6.x spent twelve lots removing.

  Three classes have to be separated, and they are already named in the CHANGELOG's 0.6.7 entry.
  The product's own vocabulary — a CRD plural, a sizing-class name, an ordinary English word — is a
  false positive and nothing about the archive should change for it. Third-party free text that
  reconstructs an identifier without quoting it is a real exposure the shipped rule does not reach:
  a ceph-csi message of the form `<namespace>-<run>-<pvc>-restore`, a path inside a volume, a restic
  snapshot ID, a URL inside a library's error string — 0.6.7's rule substitutes only *quoted exact
  matches*, which works because this project writes its own identifiers quoted and says nothing
  about anyone else's. And a genuine leak in a field that should have been tokenised is the only one
  of the three that is a defect. Today all three read identically, which is why the count is
  useless rather than merely noisy.

  The residual 0.6.7 deliberately left, recorded here because it is the half a narrowing would
  break: a PVC name that reaches the redactor through a **metric label** still lands in the general
  (blind) registry rather than the quoted one, so on a cluster with a PVC named `data` that has
  snapshots, ordinary prose is still rewritten. Narrowing that — moving the metric-label path to the
  quoted registry as well — would regress a documented guarantee of the selfcheck report, so it is
  not a patch to the release that found it; it is part of this entry, and whoever takes it decides
  both halves at once or neither.
- **Nothing durable records the timings that decide an attribution** (0.6.7 hit this twice and
  answered it neither time). When a campaign or a soak window fails, the evidence needed to say
  *why* is gone within the hour: Kubernetes Events have expired, and the operator pod has been
  recreated — routinely, because several lanes kill it deliberately — so `kubectl logs --previous`
  finds nothing. Both times 0.6.7 could reconstruct the timings only from
  `status.conditions[].lastTransitionTime`, which happened to be enough once (the cold repository
  init, 07:29:56Z created against 07:41:04Z Initialized) and impossible the other time, where the
  question was what the dying operator had and had not managed to record.

  This is stated as a question rather than a design, because it is not settled: **what is the
  smallest durable, bounded record of the few timings that decide an attribution, and who writes
  it?** Not more logging — the logs were written, and then the pod holding them was replaced. Three
  candidates worth costing rather than adopting: the operator persisting a handful of per-run
  transition timestamps where the CR outlives the pod; the harness capturing them at the moment it
  observes them, which is what `m1WaitRepositoryInitialized`'s self-measurement now does for exactly
  one bound and is the cheapest evidence 0.6.7 produced; or the collector's export carrying an
  event and log tail with a retention it declares. Each moves the cost somewhere different — CR
  size, harness complexity, archive size — and the mistake to avoid is the one the soak kit already
  made once: building the instrument before stating what it must be able to answer.

- **The supply-chain gate will refuse this project on a cadence, not as an incident** (measured 0.6.7,
  which it refused twice). Our melange packages carry a frozen epoch while Wolfi's advance: Wolfi moved
  `restic` from `-r0` to `-r5` in six weeks, three of those epochs carrying security fixes. trivy does
  not read our binaries — its own output says `language-specific files num=0` — it matches the apk's
  NAME AND VERSION against the Wolfi security database. So every Go security batch, which lands roughly
  every four to six weeks, re-reds the gate whether or not anything of ours changed, and the fix each
  time is two things that must move together: a patched toolchain, which makes the claim true, and an
  epoch bump, which makes it visible. Neither alone is acceptable — the epoch alone is a lie that
  silences a scanner, which [adr/0013](adr/0013-external-backup-sync.md) already refused once.

  Two further facts make this worse than a chore. **Wolfi's security database runs ahead of its own
  index**: at 0.6.7 it named `rclone 1.75.0-r1` and `-r2` as carrying fixes while no 1.75.0 package
  existed in the APKINDEX at all — the same shape adr/0013 recorded for an unpublished `1.74.3-r5`, so
  it is now twice. And **two findings currently sit below the gate's `CRITICAL,HIGH` threshold** —
  CVE-2026-42505 (MEDIUM) and CVE-2026-46603 (UNKNOWN, fixed by the `x/image` override 0.6.7 added) —
  so a re-scoring by NVD reddens the gate with no change of ours at all.

  What to decide, rather than what to do: whether this stays a manual per-release chore that costs a
  day when it lands mid-release, or becomes a scheduled dependency sweep that meets the batch before a
  tag does. `vex-refresh` already runs nightly and is the natural place — but it failed silently for
  three consecutive nights during 0.6.7 and nobody was watching it, which is its own finding. A canary
  nobody reads is not a canary.

- **An envtest bound that fails only on a shared runner** (seen once, 0.6.7, on the tagged commit).
  `Unit tests` went red in CI on `restore_resources_job_test.go`: `waitForResourcesJob` timed out
  after 90s with `jobs.batch "rst-rr-blank-recover-blank-resources-mover" not found`, while the same
  suite was green locally on that exact tree, repeatedly, and the crucible campaign exercised real
  restores end to end. It passed on rerun. **That is not proof of innocence** — 0.6.6 spent a whole
  release on gates that were red for reasons nobody looked at — but it is not a reproduction either,
  and one observation does not distinguish "the runner was slow" from "there is a race here that a
  fast machine hides".

  What makes it worth an entry rather than a shrug: it is the same shape as the three bounds this
  release had to move — a patience chosen against a mechanism whose worst case nobody measured. The
  cheap first step is not to raise it but to make it SAY something: report the elapsed time and what
  the controller had done by then, the way `m1WaitRepositoryInitialized` now does, so the second
  occurrence arrives with evidence instead of a name. Raising it blind would convert a possible race
  into a slower possible race.

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
