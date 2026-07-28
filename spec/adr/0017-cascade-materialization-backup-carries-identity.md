# ADR 0017 — Cascade materialization: `Backup` carries identity, pulls its run configuration

Status: **Accepted** (2026-07-25, M4 kickoff)

## Context

The cascade is modelled on `CronJob → Job → Pod`
([adr/0009 §2](0009-shared-cluster-repo-tag-tenancy.md)), and the top hop really is a template
stamp: `ClusterBackupSchedule.spec.template.spec` is copied wholesale into the `ClusterBackup`
it creates (`internal/controller/clusterbackupschedule_controller.go`). The hop below it is
**not**. `ClusterBackup` → `Backup` copies **two** of the run spec's nine fields
(`scheduleRef`, `locationRef`) and leaves the rest to be *pulled* at reconcile time, by
following the `crystalbackup.io/cluster-backup` label back to the parent
(`backup_controller.go`, `resolveRun`).

Read against the Kubernetes idiom this looks like an omission, and M4 is where it stops being
academic. `HooksSpec` sits on `ClusterBackupRunSpec` **and** on `BackupScheduleSpec`, and
`BackupStatus.Phase` already enumerates `SnapshottingHooks` — but `BackupSpec` has no field
that could carry a hook, so the object whose status advertises the phase cannot express what
to run. The same gap is load-bearing one milestone out: `BackupScheduleSpec` carries
`pvcSelector`, `includeManifests`, `manifestOptions`, `hooks` and `backoffLimit` **flat, with
no template wrapper**, there is no field on `BackupSpec` to receive them, and a
`BackupSchedule`-stamped `Backup` has no `ClusterBackup` parent to pull from. M5 cannot be
written until this is settled.

The question is therefore not "is the cascade tidy" but **what is a `Backup` an
authoritative record of** — and the answer is already constrained by something outside the
cascade entirely.

## Decision

### 1. `Backup.spec` carries identity, not intent

`Backup.spec` holds exactly what makes the object addressable and restorable — which
repository it lives in, and which schedule stamped it. Run configuration (what to select, how
to move it, what to exec) is **resolved at reconcile time**, never copied in.

### 2. The reason is that two producers write one kind

`Backup` is written from two directions: the execution fan-out, and **discovery**, which
projects `Backup` objects out of restic snapshots by server-side apply with `ForceOwnership`
(`discovery_controller.go`). SSA ownership is what keeps those two producers from fighting —
the `crystalbackup.io/projected` annotation guards execution, and each writer owns the fields
it sets (`internal/apiconst/apiconst.go`).

A field manager that **owns** a field must be able to **reproduce** it. Discovery's only input
is the repository. It can reconstruct `locationRef` — the repository *is* the location — and
it can reconstruct `status.volumes` from snapshot tags. It cannot reconstruct a `pvcSelector`,
a `manifestOptions`, or a hook command: that information was never written to restic and never
will be. Give `BackupSpec` a run-configuration field and discovery has two bad options: own it
and force it empty on every pass, permanently fighting the execution controller over the same
object; or leave it unowned, at which point SSA ownership is no longer the boundary between
the two producers and the `projected` guard is carrying the whole invariant alone.

This is what [adr/0009 §3](0009-shared-cluster-repo-tag-tenancy.md) means by *materialized
view*, and why **CR lifetime = data lifetime**: a `Backup` must stay reconstructible from the
repository alone, because after total cluster loss the repository is all there is.

### 3. The costs, recorded rather than hidden

The pull is a trade, not a free win. Four consequences are accepted knowingly:

- **The configuration-bearing link can dangle.** `ClusterBackup` run records are
  history-limited and garbage-collected (`successfulRunsHistoryLimit`), while their children
  live as long as their snapshots — and the link is a label, deliberately not an
  `ownerReference`, so GC never cascades. A `Backup` whose parent is gone resolves nothing and
  gates to `Pending` with reason `NoParent`. Today this is unreachable in practice (a run
  reaches a terminal phase long before history GC), but it is a latent hazard, **not** a
  designed property. Anything that widens the window between a run finishing and its children
  finishing must revisit this line.
- **Editing a parent retroactively changes the apparent configuration of finished runs.**
- **A `Backup` is not a self-describing audit record of what ran.** Whatever must be auditable
  per run belongs in `status`, written at execution time — never inferred by re-reading a
  parent's current `spec`.
- **`crystalctl backup trigger --schedule`** as specified in [06-cli.md](../06-cli.md) cannot
  mirror a schedule's hooks into an ad-hoc `Backup`. That contract is deferred to M7 and must
  be reconciled with whatever M5 decides (§5).

### 4. M4 does not force the question

M4's hooks are declared **primarily as pod annotations**, resolved against the pods that mount
the PVCs in the backup's selection, with Velero's precedence rule (an annotated pod's
annotations win, and the spec-declared hooks are skipped for that pod). The spec-declared path
stays admin-only on the cluster plane and rides the existing pull. Almost nothing about hooks
travels down the cascade, so M4 neither needs nor forecloses a change of direction.

### 5. M5 direction — decided here, implemented there

When the namespace plane lands, **do not extend the pull to a second parent kind**. Split
`ClusterBackupRunSpec` into a shared `BackupRunSpec` (`pvcSelector`, `includeManifests`,
`manifestOptions`, `hooks`, `maxConcurrentMovers`, `backoffLimit`) plus the cluster-only
remainder (`namespaces`, `clusterResources`, `locationRef`), and **materialize the shared part
into `Backup.spec.run` at creation**, keeping the parent pull as a fallback for objects created
before the change. Two constraints must hold, and both come from §2 and §3:

- **Discovery must never own `spec.run`.** Projections leave it absent; the `projected`
  annotation already stops them from executing. If discovery ever needs to own it, §2 says the
  design is wrong.
- **The admission surface grows.** A tenant-submittable run spec re-opens a question the
  cluster plane never had to answer: a user who can create a `BackupSchedule` makes the
  *operator* exec commands in their namespace. [03-security-and-tenancy.md
  §5](../03-security-and-tenancy.md) asserts the confinement invariant ("users can only make
  the platform run commands they can already run themselves") but nothing enforces it. Decide
  it in M5, with a `SubjectAccessReview` on the CR's creator as the candidate mitigation.

## Alternatives considered

- **Materialize the full run spec into `Backup` now (M4).** Rejected on §2, not on effort:
  the discovery-ownership question has to be answered first, and answering it is the whole
  content of this ADR. Doing the refactor inside the project's largest milestone, to unblock a
  plane that ships one milestone later, adds risk where there is no deadline.
- **Extend the pull to `BackupSchedule` via `scheduleRef`.** Implementable — `scheduleRef`
  exists for exactly this — and rejected as the M5 answer: it doubles down on retroactive
  mutation and on orphaning, for a plane where the parent is tenant-owned and therefore far
  more likely to be edited or deleted mid-flight than an admin's cluster schedule.
- **Make run records immortal so the link cannot dangle.** Rejected: run history is bounded on
  purpose, and the object-count argument in [adr/0009 §4](0009-shared-cluster-repo-tag-tenancy.md)
  is the reason.
- **Give discovery its own kind** (e.g. `RestorableBackup`) so `Backup` could be a pure
  execution record. Rejected in the original design and not reopened: it would split
  `kubectl get backups` into "what ran" and "what is restorable", which is precisely the
  confusion [adr/0009 §3](0009-shared-cluster-repo-tag-tenancy.md) set out to remove.

## Consequences

- The `CronJob → Job → Pod` analogy holds for the top hop and **stops** at `ClusterBackup`;
  the spec says so explicitly rather than leaving readers to infer a template that is not there.
- `SnapshottingHooks` becomes reachable in M4 without `BackupSpec` growing a field.
- M5 inherits a decision instead of a discovery, and inherits the escalation question with a
  named candidate answer.
- The dangling-parent hazard is now written down, so the next change that lengthens a run's
  tail has something to trip over.

## References

- [adr/0009](0009-shared-cluster-repo-tag-tenancy.md) §2 (the cascade), §3 (repository is the
  source of truth; CR lifetime = data lifetime), §4 (bounded object counts).
- [02-api.md](../02-api.md) — cascade diagram and design invariants.
- [90-roadmap.md](../90-roadmap.md) — M4 (hooks), M5 (namespace plane).
- Mechanism: `internal/controller/backup_controller.go` (`resolveRun`, the `NoParent` gate),
  `internal/controller/clusterbackup_controller.go` (`ensureChildBackup`, `childBackupLabels`),
  `internal/controller/discovery_controller.go` (`projectGroup`, SSA with `ForceOwnership`),
  `internal/apiconst/apiconst.go` (the `projected` annotation as the two-producer guard).
- Velero's precedence rule, mirrored in §4: `vmware-tanzu/velero`,
  `internal/hook/item_hook_handler.go` — "If the pod has the hook specified via annotations,
  that takes priority."
