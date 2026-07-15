# Testing strategy & Definition of Done

Status: draft v2 — aligned with [02-api.md](02-api.md) v3 (two-plane cascade +
repository-as-source-of-truth) and [adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md).
Naming contract: [02-api.md](02-api.md). Milestones: [90-roadmap.md](90-roadmap.md).

Backup software has one unforgivable failure mode: a restore that does not work. The test
strategy is therefore restore-centric — every backup produced in CI is restored and
verified, and the **reversibility promise (R8) is a CI gate**, checked with the upstream
`restic` binary, never with our own tooling alone.

## 1. Test pyramid

| Layer | Harness | Runs on | Gate |
|---|---|---|---|
| Unit | `go test` (pure Go, no cluster) | every push | merge |
| Integration | envtest (real apiserver + etcd, no kubelet) | every PR | merge |
| e2e core | kind + csi-driver-hostpath + SeaweedFS | every PR | merge |
| e2e full + metadata fidelity | same, extended matrix | nightly + release tags | release |
| Performance benches | dedicated runner / staging cluster | M6, then per release | ADR 0001 revisit input |

Make targets: `make test`, `make test-integration`, `make e2e` (core), `make e2e-full`,
`make bench`.

## 2. Unit tests

Pure Go, table-driven, `-race` always on. Key suites:

- **Manifest sanitization — golden files.** Every sanitization rule of
  [04-manifest-backup.md](04-manifest-backup.md) (R15) has at least one `input.yaml` /
  `expected.yaml` golden pair under `internal/sanitize/testdata/`. Mandatory
  corpus entries: `metadata.uid`/`resourceVersion`/`creationTimestamp`/`status`/
  `managedFields` stripped; Service with `spec.clusterIP(s)` stripped **and `nodePort`
  numbers preserved verbatim**; PVC storageClass mapping hook; ownerReferences policy;
  selectorless Service with manually-managed Endpoints/EndpointSlices **kept** while
  controller-managed EndpointSlices are excluded (E4).
  Golden files are the review surface for rule changes — updating one requires the
  corresponding rule change in the same PR.
- **Retention policy math (R24).** Given a synthetic snapshot timeline and a
  `spec.retention` (`keepLast/Hourly/Daily/Weekly/Monthly/Yearly`, `keepWithinDuration`),
  assert the exact `restic forget` argument vector produced (per-PVC,
  `--group-by host,paths`), and assert the webhook rule "retention must keep ≥ 1 snapshot"
  on degenerate inputs (all zeros, empty).
- **Server-side tenancy derivation — property-based (R2/R14 cornerstone).** The two
  server-derived tenancy handles — the mediated-restore **tag filter** `namespace=<ns>`
  and the snapshot **path** `/data/<ns>/<pvc>` — are computed from **only**
  `(resolved location, clusterID, CR metadata.namespace[, pvc])`, never a user-writable
  spec field. Property tests (e.g. `pgregory.net/rapid`, MIT) over generated DNS-1123
  names assert:
  1. **Injectivity**: distinct namespaces yield distinct `namespace=` filters and
     disjoint `/data/<ns>/` path subtrees.
  2. **Prefix-freedom**: no derived `/data/<ns>/` subtree is a path prefix of another
     (the trailing `/` makes `…/a/` and `…/ab/` disjoint — asserted, not assumed), so a
     subtree restore cannot leak a sibling namespace.
  3. **User-input independence**: fuzzing every user-writable field of `Restore`,
     `Backup` and `BackupSchedule` specs never changes the derived filter or path for a
     fixed namespace. Any new spec field that breaks this property fails the suite — the
     non-forgeable `namespace=` filter is the R2/R14 cornerstone.
- **Selection evaluation — table-driven.** Namespace selection
  (`ClusterBackup`: `matchNames`/`matchLabels`/`matchExpressions`/`regexp`, `exclude`
  applied last, exactly one positive form) and restore selection lists
  (`resources[]`/`volumes[]`: **OR** between items, **AND** within an item; both omitted
  ⇒ whole namespace; a present `[]` ⇒ nothing of that kind; file-path `include`/`exclude`
  globbing on a volume).
- Cron/timezone computation of `nextScheduleTime`; mover termination-message JSON
  parsing; **platform DEK wrap/unwrap + KEK re-wrap** round-trip against a fixture age
  cluster KEK (adr/0004).

## 3. Integration tests (envtest)

Controllers run against a real apiserver; Jobs never execute (no kubelet), so mover
outcomes are injected by patching Job status and the restic repository is a faked
inventory. Scenarios:

- **Cascade fan-out (R25)**: a `ClusterBackup` resolves `spec.namespaces`
  (`matchNames`/`regexp` + `exclude`) and creates one `Backup` per **existing** matching
  namespace, each labeled `crystalbackup.io/cluster-backup=<run>` and
  `crystalbackup.io/origin: cluster`. Injected per-namespace outcomes roll up into
  `ClusterBackup.status` (`namespacesMatched/Succeeded/Failed`, `pvcs*`); injecting more
  failures than the cap proves `status.failures` stays **bounded** (no `perNamespace`
  map → no 1 MiB etcd risk). Child `Backup`s are linked by **label, not `ownerReference`**.
- **Schedule fires**: a `BackupSchedule` creates a `Backup` named
  `<schedule>-<timestamp>` at the computed time (test clock); `status.lastRunName`,
  `nextScheduleTime` updated; `paused: true` suppresses firing. A `ClusterBackupSchedule`
  past `successfulRunsHistoryLimit`/`failedRunsHistoryLimit` garbage-collects old
  **`ClusterBackup` run records** while their labeled child `Backup` projections
  **survive** — pruning run history never deletes restorable backups
  ([adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md)).
- **Discovery projection (R26)**: given a faked snapshot inventory grouped by
  `(namespace, run)`, the discovery controller creates a `Backup` projection in each
  **existing** namespace, **skips** non-existent ones, **re-creates** a projection a
  client deleted, and **removes** one whose snapshots vanished (post-`forget`) — CR
  lifetime = data lifetime.
- **Status transitions**: `Backup` walks
  `Pending → SnapshottingHooks → Snapshotting → Uploading → Completed`, and per-volume
  failure injection yields `PartiallyFailed` with correct `status.volumes[].phase`.
  `Restore`/`ClusterRestore` walk `Pending → AwaitingConfirmation → Running → Completed`.
- **Admission — VAP-first + a dynamic webhook** (the full [02-api.md § Validation](02-api.md) rule list; VAP policies installed in envtest, [adr/0010](adr/0010-admission-vap-first.md)):
  - R23 (conservative superset): **any** `Recreate` **or** `Overwrite` with `confirmation`
    ≠ the **target namespace** is **denied**, on `Restore` **and** `ClusterRestore` (VAP cannot
    test target existence, so it asks unconditionally in those modes); `ClusterErasure` with
    `confirmation` ≠ target identity denied; empty confirmation is admitted and parks the CR in
    `AwaitingConfirmation`.
  - **User isolation**: a user-created `Backup`/`BackupSchedule` referencing a
    `ClusterBackupLocation` is denied (must name a namespaced `BackupLocation`), **but the
    operator's ServiceAccount is exempt** (its cluster-origin fan-out `Backup`s reference a
    `ClusterBackupLocation` and must pass); create/update/delete on a
    `crystalbackup.io/origin: cluster` `Backup` is denied (read-only projection); a `Restore`
    carries no `locationRef` or target-namespace field.
  - Exactly one `ClusterBackupLocation` with `default: true` (second one denied by the
    **dynamic webhook** — the one cross-object rule; its `failurePolicy: Ignore` path and the
    operator's `MultipleDefaults` reconcile backstop are both covered).
  - Invalid cron denied; retention keeping 0 snapshots denied on a **Standard**-mode
    location (empty retention admitted on Immutable). `keep*` on a schedule targeting an
    **Immutable** location is a **controller-side** advisory (`RetentionIgnored` condition +
    Warning event), **not** admission — it needs the location's mode (cross-object), so it is
    asserted in an envtest **controller** test, not a VAP test.
  - `mode: Immutable` + `maintenance.pruneSchedule` denied (CEL);
    `credentialsSecretRef`/`repositoryPasswordSecretRef` on a `BackupLocation` must be
    same-namespace; `namespaces` selector must set **exactly one** positive form +
    optional `exclude`.
  - Tenant-facing CRs denied in the configurable denied-namespaces list (VAP `paramRef`
    ConfigMap; default `kube-*`, `crystal-backup-system` and any incumbent backup tool's
    namespace).
  - **VAP holds without the operator**: static denials (confirmation, isolation) fire in the
    API server even when the operator/webhook is unavailable, while the dynamic webhook is
    `failurePolicy: Ignore` so its absence never blocks writes ([adr/0010](adr/0010-admission-vap-first.md)).
- **BackupRepository lifecycle**: **cluster-scoped**, **one per location** (one shared
  repo for the cluster plane's `ClusterBackupLocation`, one per user `BackupLocation`),
  created when the location is added so discovery can inventory it; restic **init
  serialized** through the per-repo exclusive queue on first use; tenant RBAC cannot
  read/write it (SubjectAccessReview-style check against the chart-rendered
  `crystal-backup-tenant` ClusterRole).

## 4. e2e (kind + csi-driver-hostpath + SeaweedFS)

Environment: pinned kind node image, external-snapshotter (snapshot-controller + CRDs),
csi-driver-hostpath with `VolumeSnapshot` support, in-cluster SeaweedFS (+ `mc` for
tampering/inspection), two namespace fixtures `tenant-a` / `tenant-b`, a shared
cluster-plane `ClusterBackupLocation` (one SeaweedFS bucket, platform DEK wrapped by an age
cluster KEK) **and** per-namespace `BackupLocation`s (each its own SeaweedFS bucket + own
restic password) so both planes are exercised, operator installed from the packaged Helm
chart (`charts/crystal-backup`, never from raw manifests — the chart is under test too).

Core suite (every PR):

1. **Backup + reversibility gate (R8)**: a `BackupSchedule` in `tenant-a` (namespace
   plane, the user's own `BackupLocation` + own password) → `Backup` `Completed`. Then,
   with a **pinned upstream restic release binary** (not the mover image) and the
   **user's password** from their Secret: `restic snapshots --json` shows one `kind=data`
   snapshot per PVC (host=`<clusterID>`, tags `crystalbackup`, `tenant=`,
   `namespace=tenant-a`, `pvc=`, `schedule=`, `run=`) plus one `kind=manifests` snapshot
   at `/manifests/tenant-a`; `restic check` passes; `restic dump latest --archive tar`
   byte-compares with the source. The **cluster plane** is verified the same way with the
   platform DEK **unwrapped from the cluster KEK** (age). This gate is what makes R8 a
   fact rather than a slogan.
2. **Cascade fan-out (R25)**: a `ClusterBackupSchedule` (`namespaces.regexp: "^tenant-.+$"`)
   stamps a `ClusterBackup` run that fans out a `Backup` into **both** `tenant-a` and
   `tenant-b`, each labeled `crystalbackup.io/cluster-backup=<run>` /
   `crystalbackup.io/origin: cluster`, all writing the **one shared** cluster repo.
   `ClusterBackup.status` aggregates `namespacesMatched/Succeeded/Failed` + `pvcs*`;
   per-namespace detail stays in each child `Backup`. Failing one namespace's mover
   yields a single **capped** `status.failures` entry and `PartiallyFailed`, never an
   unbounded map.
3. **Discovery & repository-as-source-of-truth (R26)**: point a fresh
   `ClusterBackupLocation` at a **pre-populated** SeaweedFS bucket (snapshots for `tenant-a`,
   `tenant-b`, and a non-existent `tenant-gone`). Discovery projects a `Backup` into
   `tenant-a` and `tenant-b`, **skips** `tenant-gone` (no namespace); `kubectl get backups
   -n tenant-a` lists exactly the restorable runs. Then `kubectl delete backups --all -A`
   → the next discovery pass **re-projects** them identically (the repo, not the CR, is
   the truth). `restic forget` a run out-of-band → its projection disappears.
4. **Restore mode × selection (R6/R7/R23)**: against a restored `tenant-a`,
   - **`Recreate`** (data `restic restore --overwrite always --delete`): the target PVC
     becomes an exact match of the backup (extra files removed);
   - **`Overwrite`** (`--overwrite always` **without** `--delete`): present files
     overwritten, missing files restored, **files absent from the backup kept**;
   - **selection lists**: `resources: [{selector, include, exclude}]` and
     `volumes: [{names, include, exclude, targetPath}]` — OR between items, AND within
     one; both omitted ⇒ whole namespace; a **single folder of a single PVC**
     (`volumes: [{names: [uploads], include: ["images/2026/**"]}]`) restores only that
     file subset, siblings untouched.
   Each destructive path is replayed with a **wrong `confirmation`** → admission denial
   observed client-side; the accepted `confirmation` equals the **target namespace** (R23).
5. **Operator-mediated tag filter — negative (R2/R14)**: in the **shared** cluster repo
   holding both namespaces, a cluster-origin `Backup` is projected into `tenant-a` and
   into `tenant-b`. A self-service `Restore` in `tenant-a` (referencing *its own*
   cluster-origin `Backup`) is served by the operator with the server-side filter
   `namespace=tenant-a` and restores only `tenant-a` data. Fuzzed/hand-crafted `Restore`
   specs from §2 replayed live in `tenant-a` **never** reach `tenant-b`'s snapshots — the
   `namespace=` filter is derived from the CR's namespace and is not user-writable — and a
   `Restore` cannot even name `tenant-b`'s `Backup` (it is not in `tenant-a`).
6. **Storage isolation & tenant visibility (R2)**: the two namespace-plane
   `BackupLocation`s use **separate buckets, credentials and passwords**. With `tenant-a`'s
   projected S3 credentials, `GetObject`/`ListObjects` on `tenant-b`'s bucket returns
   `AccessDenied`; opening `tenant-b`'s repo with `tenant-a`'s password fails (talking
   straight to SeaweedFS, K8s RBAC out of the loop on purpose). The shared cluster-repo key
   (platform DEK) is readable **only** in `crystal-backup-system` (a tenant ServiceAccount
   SAR is denied). Native RBAC: impersonating a `tenant-a` user, `list backups` returns
   only `tenant-a`'s, and `create/update/delete` on a `crystalbackup.io/origin: cluster`
   `Backup` is **denied** by admission (read-only projection).
7. **Hook failure policies (R16)**: pre-hook with `onError: Fail` aborts the backup
   before snapshots; `onError: Continue` proceeds and records the hook error in status;
   hook timeout enforced.
8. **Metrics smoke (R19)**: `/metrics` exposes
   `crystalbackup_backup_last_success_timestamp_seconds{namespace="tenant-a"}` (full
   catalogue asserted per [05-observability.md](05-observability.md); every series carries
   the origin `namespace` label + a `cluster` label); operator logs parse as JSON lines.

Full suite (nightly + release tags), in addition:

9. **DR bootstrap + manifest round-trip (R15/R26)**: a **fresh second kind cluster** (no
   CRs, no `tenant-x` namespace) with a different default StorageClass name, and a SeaweedFS
   bucket populated by a prior run. Creating a `ClusterBackupLocation` inventories the
   repo; a `ClusterRestore` targets a **repo coordinate** (`location + namespace=tenant-x
   + run`) with `target.createNamespace: true` and `storageClassMapping`. Workloads reach
   `Ready`; `nodePort` numbers identical; `clusterIP` freshly allocated; apply order
   respected (Secrets/ConfigMaps/PVCs before workloads). Proves DR works with **no prior
   CRs and a non-existent namespace**.
10. **Right-to-erasure (`ClusterErasure`, R21)**: in the shared repo, `spec.target`
    (`pvc`, or `namespace`, or `tenant`) + typed `confirmation` → `restic forget --tag …`
    + `prune` removes **exactly** the targeted snapshots; other namespaces' snapshots and
    `restic check` stay clean; the matching `Backup` projections disappear on the next
    discovery pass. On an **Immutable** location the same request reports `phase: Blocked`
    + `blockedUntil` and reclaims nothing until object-lock expiry.
11. **Shared-repo prune under concurrent backup**: a long-running backup on the cluster
    repo while the maintenance controller schedules its **single cluster-wide** `prune` →
    prune queues behind the per-`BackupRepository` exclusive lock, backup completes, prune
    then runs exclusively; `restic check` clean after both. (One shared repo ⇒ one prune
    window — [adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md); backups take shared
    locks, prune/check/forget/init/erasure require the exclusive lock — this test proves
    the operator serializes them per `BackupRepository`.)
12. **`check` catches tampering (R17)**: flip bytes inside a pack object under `data/` in
    SeaweedFS via `mc`; the next scheduled `restic check --read-data-subset` fails, surfacing
    `BackupRepository.status.lastCheckResult: Failed`, the `RepositoryCheckFailed` condition and
    alert metric. (Restore-testing itself is the admin's job — no automated canary in v1, R17.)
13. **Orphan reaper**: kill a `crystal-mover` pod mid-upload (grace 0) and cordon+delete
    its node (node-drain simulation on a multi-node kind cluster) → temp PVC, static
    VS/VSC and tenant VolumeSnapshot are garbage-collected, the stale restic lock is
    removed after the age threshold (`restic unlock` semantics), the `Backup` ends
    `Failed` or `PartiallyFailed`, and the **next scheduled run succeeds** on the same repo.
14. **Upgrade test**: install chart version n-1 (previous tag), create schedules and
    complete one backup; `helm upgrade` to n; assert no backup storm on upgrade, existing
    CRs still reconcile, repo still readable by upstream restic, CRDs upgraded additively
    (single `v1alpha1` served version in v1 — no conversion webhooks to test yet).
15. **Immutable-mode suite (from M8)**: Ceph RGW with Object Lock; append-only writes
    succeed, prune never scheduled, repo rotation per `rotationPeriod`, expired-repo
    deletion, and `ClusterErasure` reporting `Blocked` (cf.
    [adr/0005](adr/0005-immutability-mode.md)).
16. **Cluster-scoped capture & selective restore (R22, [adr/0011](adr/0011-cluster-scoped-dr.md))**:
    a `ClusterBackup` with `clusterResources.enabled` captures a `kind=cluster-manifests` snapshot
    (a fixture CRD + its `StorageClass` + a non-`system:` `ClusterRole`); `restic snapshots` shows
    it and `ClusterBackup.status.clusterResourcesCaptured` is set. On the fresh second cluster
    (case 9), a `ClusterRestore` with `clusterResources.include` **selectively** recreates the CRD
    and StorageClass **before** the namespaced objects (apply order); omitting it restores
    **nothing** cluster-scoped (opt-in); the destructive path requires R23 confirmation, and a
    cluster-scoped restore by a **non-admin is denied** (RBAC).
17. **Exposer selection & unsupported-CSI skip (R11, [adr/0003](adr/0003-snapshot-exposure-csi-generic-first.md))**:
    with a StorageClass whose provisioner advertises **no** `VolumeSnapshotClass`, a PVC is
    **skipped** — `Backup.status.volumes[].phase: Skipped`, `reason: CSISnapshotUnsupported`, an
    Event and log line — the `Backup` ends `PartiallyCompleted` (never a false success or hard
    failure) and its manifests are still captured. Ceph exposers are covered on the staging run (§5).
18. **External sync — snapshot copy, re-keyed, siloed (R28, [adr/0013](adr/0013-external-backup-sync.md))**:
    give `tenant-a` a **second** `BackupLocation` (`my-offsite-2`, its **own** bucket + **own**
    password) and a `BackupExternalSync` (`sourceLocationRef: my-offsite`,
    `destinationLocationRef: my-offsite-2`). After a run, with the **destination's own password**
    and a **pinned upstream restic**: `restic snapshots` on the second bucket shows `tenant-a`'s
    runs (copied), `restic check` passes, and a `restic restore` byte-matches the source; opening
    the destination with the **source** password **fails** — `restic copy` re-encrypted, so the
    keys differ. A `ClusterBackupExternalSync` to a second `ClusterBackupLocation` copies the
    whole shared repo; the destination opens with the **secondary's own DEK** and the **primary
    DEK does not open it** (siloing: keys differ per repo). `mode: Mirror` propagates a source
    `forget` to the destination on the next run; an **Immutable** destination forces `AppendOnly`
    (no `forget`). Admission: a `BackupExternalSync` naming a `ClusterBackupLocation` or a location
    in another namespace is **denied** (rule 2), and a sync whose `sourceLocationRef ==
    destinationLocationRef` is **denied** (rule 9).

## 5. Metadata fidelity suite (R10 gate)

Runs inside the e2e full suite, and is the **acceptance gate for any mover engine
change** — the ADR 0001 revisit (restic CLI → rustic or anything else) cannot merge
unless this suite passes bit-for-bit in **all four directions** (backup A/restore A,
backup A/restore B, backup B/restore A, backup B/restore B). Known upstream caveats
justify it: rustic only restored hardlinks correctly from 0.11.2 (2026-04), and restic
stores POSIX ACLs via `system.posix_acl_*` xattrs — behavior worth pinning on our
filesystems, not assuming.

- **Corpus generator** (`test/fidelity/gencorpus`, deterministic from a seed), written
  into the source PVC by a root init-container (movers stay unprivileged; only corpus
  *generation* needs root for `security.*` xattrs and ownership):
  - xattrs: `user.*` and `security.*` namespaces, multiple per file, binary values;
  - POSIX ACLs: access + default ACLs on files and directories (`setfacl` fixtures);
  - hardlinks: link groups of 2–5 across sibling directories;
  - sparse files: multi-GiB apparent size, few allocated blocks;
  - setuid/setgid/sticky bits, plus non-root uid/gid combinations;
  - symlinks: relative, absolute, dangling;
  - unicode names in NFC **and** NFD, names with spaces/newlines/`--` prefixes;
  - deep trees producing paths > 255 bytes total, with individual components near the
    255-byte limit;
  - mtimes with sub-second precision; empty files and empty directories.
- **Differ** (`test/fidelity/fsdiff`): backup → restore to a fresh PVC → compare
  source vs restored on: SHA-256 content, size **and** allocated blocks (`st_blocks`,
  sparseness preserved via `restore --sparse`), full mode bits, uid/gid, all xattrs,
  ACLs, hardlink equivalence classes (inode grouping, `st_nlink`), symlink targets,
  mtime (ns). `atime`/`ctime` are explicitly out of scope. Output is a machine-readable
  diff; empty diff = pass.
- **From M7** (when `crystalctl` ships) the suite also runs against the tar produced by
  `crystalctl repo export --tar` (R8) — the core R8 gate itself uses upstream `restic dump
  --archive tar` (§4 case 1), independent of the CLI — with documented tar-format limitations
  (e.g. sparse representation) recorded as expected deltas, not silent passes.
- M6 adds one scheduled run of this suite on a **real Rook Ceph staging cluster** — CI's
  hostpath driver cannot vouch for Ceph xattr/ACL behavior. It exercises **both Ceph exposers**
  ([adr/0003](adr/0003-snapshot-exposure-csi-generic-first.md)): RBD via `csi-generic` (COW
  clone) and **CephFS via `cephfs-shallow`** (ROX `backingSnapshot` — asserting no full subvolume
  copy), plus the Object-Lock immutable suite against **Ceph RGW**.

## 6. Performance benches (M6)

Run on a dedicated runner or staging cluster (not shared CI runners). Datasets:

| Bench | Shape | Measured |
|---|---|---|
| B1 | 1M files, ~4 KiB avg, deep tree | wall time, mover peak RSS, restic index cost |
| B2 | 100 GiB PVC, large files | wall time, S3 throughput, snapshot-to-upload latency |
| B3 | 10 namespaces × 10 GiB concurrently into the shared cluster repo | aggregate throughput (R12), operator CPU/RAM, scheduling spread across nodes, shared-repo lock/index churn |

- **RAM ceilings**: benches establish the default mover resources shipped in the chart;
  starting hypothesis: 1M files fits under a 2 GiB limit without OOMKill (Velero's
  published bench saw restic at ~904 MiB on 2M+ files, but predates restic 0.18/0.19
  memory work — hence our own numbers). Any OOMKill at default limits is a release
  blocker. Shared-repo `prune` memory scales with total cluster data, not per-namespace
  (adr/0009) — benched separately.
- **restic vs rustic comparison protocol** (input to the ADR 0001 revisit): identical
  corpus (B1 + fidelity corpus), identical repo, both engines pinned; measure wall
  time, peak RSS, S3 request count; run the §5 four-direction fidelity matrix; exercise
  rustic's lock-free two-phase prune against concurrent restic backups. Results are
  committed alongside the ADR revisit, with exact versions.

## 7. CI mapping (GitHub Actions), flake policy, coverage gates

| Stage | Jobs | Trigger |
|---|---|---|
| lint | golangci-lint, `gofmt`, `go vet`, license scan (permissive-only: Apache-2.0/MIT/BSD) | every push |
| unit | `go test -race` + coverage gates | every push |
| build | operator + mover **multi-arch** images (`linux/amd64`+`linux/arm64`, **apko/Wolfi**, SBOM), chart package, `helm lint`; **container CVE scan** (0-known-CVE gate — [adr/0012](adr/0012-container-images-apko-wolfi-slsa.md)) | every push |
| integration | envtest suite | every PR |
| e2e-core | §4 core suite on kind | every PR |
| e2e-full | §4 full suite + §5 fidelity suite | nightly, release tags |
| bench | §6 (manual trigger; scheduled weekly from M6) | manual / schedule |
| release | **multi-arch image index + chart publish to GHCR**, **cosign sign + SLSA L3+ provenance attest & verify**, release-dry-run on PRs | tags (dry-run: PRs) |

- **Coverage gates** (enforced in the unit stage): **≥ 80 %** line coverage on
  `internal/controller/...`; **100 %** on the sanitization rules package (every rule
  must be reached by at least one golden pair — an unreferenced rule fails CI). Global
  coverage is reported but not gated.
- **Flake policy**: no automatic retries — retries hide real races in exactly the code
  we cannot afford races in. e2e tests are namespace-isolated with unique names and
  must pass under `-count=2` locally. A test failing twice within 7 days on unchanged
  code is quarantined (build tag `flaky` + tracked issue): it leaves the PR gate but
  keeps running nightly; fix SLA 14 days, after which the owning feature is considered
  regressed.
- CI includes a **consistency check**: a PR touching `config/crd/` must also touch
  `spec/02-api.md` (script in the lint stage) — DoD item 3 automated.
- A **scheduled rebuild** job re-bakes the images against the rolling Wolfi apk set, re-runs the
  container CVE gate and a fidelity smoke test, and re-attests SLSA provenance — **shrinking the
  known-CVE exposure window** between releases (base/OS CVEs; app/dependency CVEs still need a code
  bump) ([adr/0012](adr/0012-container-images-apko-wolfi-slsa.md)).

## 8. Definition of Done — canonical checklist

Reproduced from [90-roadmap.md](90-roadmap.md) (applies to **every task**), with the
verification method that makes each item checkable rather than aspirational:

| # | DoD item (90-roadmap.md) | Verified by |
|---|---|---|
| 1 | Unit tests written and passing; envtest tests for controller behaviour; e2e coverage when the task touches the data path | Coverage gates (§7); PR template asks "data path touched? → which e2e case"; reviewer blocks without one |
| 2 | Structured logs for new code paths; metrics for new user-visible outcomes; traces on new pipeline spans | e2e asserts logs parse as JSON-lines and `/metrics` contains the new series; OTel smoke test with an in-kind collector (spans present when `OTEL_*` set, no-op otherwise) |
| 3 | CRD/API changes: validation (VAP/CEL first; webhook or controller-side advisory only for cross-object checks) + generated docs + `02-api.md` updated in the same PR | CI consistency check (§7); envtest case (VAP, webhook, or controller advisory) required for each new rule |
| 4 | Security review checklist for anything touching credentials, keys, or cross-namespace logic (two-person review) | CODEOWNERS forces 2 approvals on key-management, tenancy-derivation (repo path + `namespace=` tag filter) and admission (VAP + webhook) packages; checklist embedded in PR template |
| 5 | No permission widening of tenant RBAC without an ADR | Golden-file test on the chart-rendered `crystal-backup-tenant` ClusterRole (`helm template` diff vs committed golden); changing the golden requires an ADR link in the PR |
| 6 | Docs updated (user or ops guide); CHANGELOG entry | CI changelog check (entry required unless PR labeled `no-changelog`); docs build job |
| 7 | CI green (lint, unit, e2e); **multi-arch** (`linux/amd64`+`linux/arm64`) image + chart publishable from the PR pipeline; **images pass the 0-known-CVE scan, are cosign-signed with an SBOM, and carry verified SLSA L3+ provenance** ([adr/0012](adr/0012-container-images-apko-wolfi-slsa.md)) | `release-dry-run` job runs the full packaging path (incl. CVE scan + provenance verify) on every PR |

## 9. Open questions

1. Cadence and ownership of the Rook Ceph staging run of the fidelity suite (§5) —
   weekly seems right once M6 lands; needs a staging namespace and alert wiring.
2. B2 (100 GiB) is too heavy for kind on shared runners; confirm the dedicated bench
   runner sizing (disk + network to a real S3 endpoint) at M6 kickoff.
3. Should the periodic `restic check --read-data-subset` sample deterministically (seeded)
   so consecutive failures are comparable, or randomly for coverage? e2e currently assumes
   deterministic seeding. (Restore drills are an admin/ops runbook item, not automated in v1.)
