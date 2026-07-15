# ADR 0006 — Coexistence with third-party backup tools (no replacement goal)

Status: **Accepted** (2026-07-11; reframed from "Velero coexistence and replacement" to
generic, no-replacement coexistence on 2026-07-15, in line with the project's
de-branding/generalization — Velero is now treated as one example among other backup tools)

## Context

Crystal Backup is a generic, open-source operator. It will be installed on clusters that
**already run other backup tooling** — most commonly Velero, but also K8up, Veeam Kasten,
Stash/KubeStash, or a plain CSI-snapshot scheduler. The design goal is to **coexist without
interference**: installing Crystal Backup must never degrade, disturb, or require removing an
incumbent tool.

Crystal Backup's cluster plane (`ClusterBackupSchedule` → `ClusterBackup` → `Backup` into the
shared repo, [adr/0009](0009-shared-cluster-repo-tag-tenancy.md)) *is* a full platform-DR
capability — all namespaces (selector with exclusions) plus cluster-scoped resource manifests
([adr/0011](0011-cluster-scoped-dr.md)). So Crystal Backup **can** be a cluster's sole DR mechanism. But **whether an operator retires
another tool is the operator's decision, made on their own criteria and outside this project.**
The project ships the DR capability and the coexistence guarantees — it does **not** pursue
"replace tool X", and nothing here is written against a specific competitor.

The interesting coexistence surface is **shared infrastructure**, not CRDs: several tools take
CSI `VolumeSnapshot`s of the same underlying storage (Rook Ceph RBD via ceph-csi), write to S3,
and run on a cron. The concrete example worth engineering against is Velero, because it is the
most common incumbent and it snapshots the same Ceph RBD images through ceph-csi.

ceph-csi headroom matters: it forces background flatten from `--minsnapshotsonimage`
(default 250) and returns `ResourceExhausted` at `--maxsnapshotsonimage` (default 450)
snapshots per image
([ceph-csi cmd/cephcsi.go defaults](https://github.com/ceph/ceph-csi/blob/devel/cmd/cephcsi.go));
simultaneous snapshot+clone bursts also load the ceph-mgr `rbd_support` task queue.

## Decision

**Crystal Backup installs and runs strictly side-by-side with any incumbent backup tool,
indefinitely, and never interferes with it. There is no "replacement phase" and no
tool-specific parity checklist in this project.**

### Non-interference rules

1. **Distinct API surface**: API group `crystalbackup.io/v1alpha1`; no shared CRDs. Tenant-facing
   CRs are denied in platform namespaces via the configurable denied-namespaces deny-list
   (rule in [02-api.md](../02-api.md); default `kube-*`, `crystal-backup-system`, and any
   incumbent backup tool's install namespace — a Helm value, e.g. the Velero namespace).
2. **Distinct namespaces and identities**: operator and movers live in `crystal-backup-system`;
   the incumbent keeps its own namespace, ServiceAccounts, RBAC and Secrets. No shared
   credentials, no shared repositories (restic format in distinct buckets/prefixes).
3. **No shared VolumeSnapshotClass mutation**: the operator selects its `VolumeSnapshotClass`
   from its own configuration. It never mutates existing `VolumeSnapshotClass` objects, never
   changes the cluster default, and never touches another tool's snapshot-class labels (e.g.
   `velero.io/csi-volumesnapshot-class`). Every VolumeSnapshot/VSC object it creates carries the
   `crystal-` name prefix and the `crystalbackup.io/backup` label; the static VS/VSC re-bind
   ([01-architecture.md §5](../01-architecture.md)) only ever manipulates objects the operator
   created itself.
4. **Schedule-offset guidance**: if the incumbent owns a fixed slot (Velero's typical 22:00),
   Crystal Backup's defaults, docs and Helm sample schedules steer away from it through the cron
   `schedule` itself — admins and users pick an off-peak hour. This is defaults/guidance, not
   enforcement (tenants may have legitimate reasons to pick another hour); the cluster-wide
   `maxConcurrentMovers` semaphore bounds the worst case regardless. In nominal operation the
   transient snapshot count per image stays at 1–3 (both systems delete their snapshots after
   upload), two orders of magnitude below the 250/450 ceph-csi thresholds.
5. **Monitoring of per-image snapshot counts**: gauge
   `crystalbackup_pvc_volumesnapshot_count{namespace, pvc}` counts `VolumeSnapshot` objects per
   source PVC (so it sees the incumbent's snapshots too); alert at >20 per PVC — an order of
   magnitude below ceph-csi's flatten thresholds — catching leaks from either system early. The
   orphan reaper ([01-architecture.md §5](../01-architecture.md)) keeps Crystal Backup's own
   count bounded. Exact rules in [05-observability.md](../05-observability.md) (alert
   `CrystalbackupPVCSnapshotPileup`).

Crystal Backup failures must never degrade an incumbent safety net; the reverse must also hold.
This is verified in the M6 staging soak, which runs Crystal Backup and Velero side-by-side for
2+ weeks.

### On "becoming the primary DR tool"

An operator who wants Crystal Backup to be their cluster's primary or only DR mechanism can do
so — the cluster plane covers it. That is an **operational choice**, exercised with the
operator's own acceptance criteria (fleet-scale nightly run, DR drill RTO, coverage audit, a
double-run overlap, a rollback runbook). Those are good practices we document as **guidance for
operators** (see [90-roadmap.md](../90-roadmap.md) and the docs), **not** a milestone of this
project and **not** a promise to displace any named tool.

## Consequences

### Positive

- **No DR gap, ever**: an incumbent safety net is never touched, so adopting Crystal Backup is
  risk-free with respect to existing backups.
- **Drop-in on real clusters**: the common case (a cluster already running Velero) is a
  first-class, tested configuration rather than an afterthought.
- Where Crystal Backup does serve DR, its restic format preserves xattrs/ACLs/hardlinks that
  Velero's kopia mover does not ([kopia#544](https://github.com/kopia/kopia/issues/544)) — a
  metadata-fidelity gain (R10), offered as a capability, not as a sales pitch against Velero.

### Negative

- If an operator **keeps** both tools running, they pay for two snapshot pipelines and double
  backup traffic to S3 during any overlap (bounded: both delete snapshots after upload). This
  is the operator's cost/benefit to weigh — the project neither forces the overlap nor forces
  its end.
- Coexistence rules (deny-lists, prefixes, snapshot-count alerting) are permanent surface area
  the operator must keep configured, even on clusters where no other backup tool exists.

### Risks & mitigations

- **Snapshot storms / flatten pressure on Ceph** when schedules from two tools cluster around
  the same hour → off-peak cron scheduling away from the incumbent's slot, `maxConcurrentMovers`
  semaphore, `crystalbackup_pvc_volumesnapshot_count` alerting at >20 per PVC.
- **Silent interference** (either tool deleting the other's VS/VSC or repo objects) → strict
  object ownership: `crystal-` prefixes + labels, operator garbage-collects only objects
  carrying its own labels; distinct buckets/prefixes; e2e coexistence test in the M6 soak.

## Alternatives considered

1. **Target Velero replacement as a project goal (parity checklist, cutover milestone).**
   Rejected with the generalization: Crystal Backup is a generic tool, and singling out one
   competitor for removal is both off-brand and unnecessary — the cluster plane already
   provides DR for operators who *choose* to consolidate. Replacement is an operator decision,
   documented as guidance, not a deliverable.
2. **Build tenant self-service on top of an existing tool instead of coexisting.** Rejected in
   the state-of-the-art survey: mono-namespace admin-only control planes (Velero's
   multi-tenancy epic [#2587](https://github.com/vmware-tanzu/velero/issues/2587) iceboxed since
   2020; single static repo key) cannot provide R2/R3/R7/R9/R14/R21. Not an anchor point.
3. **Assume Crystal Backup is the only backup tool (skip coexistence engineering).** Rejected:
   real clusters carry incumbent tooling; interfering with it (shared snapshot classes,
   snapshot storms) would make Crystal Backup unsafe to install.

## Revisit triggers

- ceph-csi flatten/threshold defaults change, or the platform storage changes away from Ceph
  RBD — re-derive the snapshot-count headroom and alert thresholds.
- A common incumbent (e.g. Velero) changes its snapshot object ownership or labels in a way that
  affects the non-interference rules.
- An operator asks for tooling to *assist* a consolidation onto Crystal Backup (coverage-diff
  report, DR-drill harness) — that would be a **docs/tooling** addition for operators, still not
  a "replace tool X" project goal.
