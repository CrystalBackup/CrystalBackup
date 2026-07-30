---
title: Alerts
description: The shipped Prometheus alerting rules — catalogue pending.
---

:::caution[TODO — rules pending]
The alerting rules are being finalised in M6, which is in progress and has not been
released, alongside the
[metrics catalogue](/CrystalBackup/docs/reference/metrics/). This page is a placeholder
with the structure it will take.

**No alert rule bundle ships in `v0.5.1`.** Until it does, the checks under
[Observability](/CrystalBackup/docs/guides/observability/) are the manual equivalents, and
they are worth wiring up yourself in the meantime.
:::

## What each alert will document

For every rule: the series it reads, the threshold and duration, the severity, what it
actually means, and what to do about it. An alert whose runbook is "look at the dashboard"
is not worth shipping.

## Repository integrity

<!-- TODO(M6): RepositoryCheckFailed — restic found damage. Not a transient error. -->
<!-- TODO(M6): RepositoryCheckStale — nothing verified in too long. A different incident
     from the above, which is why lastCheckTime updates on failure as well as success. -->
<!-- TODO(M6): RepositoryStaleLocks — locks accumulating faster than they are reaped;
     every exclusive operation will eventually stall behind them. -->

## Backup freshness

<!-- TODO(M6): BackupTooOld — a namespace has no recent successful backup. -->
<!-- TODO(M6): BackupFailing — repeated failures on one schedule. -->
<!-- TODO(M6): VolumeSkipped — a PVC is being skipped run after run, most often
     CSISnapshotUnsupported. The backup reports Completed and that PVC is not in it. -->

## Maintenance

<!-- TODO(M6): MaintenanceStale — prune has not succeeded in too long. lastMaintenanceTime
     is deliberately not refreshed by a failed prune, so this keeps firing. -->
<!-- TODO(M6): MaintenanceFailing. -->

## External sync

<!-- TODO(M6): ExternalSyncStale — specified, ships with this bundle. -->
<!-- TODO(M6): ExternalSyncLagGrowing. -->

## Hooks

<!-- TODO(M6): PostHookRetrying — a release hook is still failing, so an application may
     still be quiesced. The one worth paging on. -->

## Coexistence

<!-- TODO(M6): PVCSnapshotPileup — more than ~20 VolumeSnapshots on one source PVC. It
     counts other tools' snapshots too, because ceph-csi's per-image thresholds
     (background flatten at 250, ResourceExhausted at 450) do not care who created them. -->

## Operator

<!-- TODO(M6): OperatorDown, ReconcileErrors. -->

## A note on what alerts cannot tell you

None of these verify that a **restore works**. `restic check` verifies the repository is
readable; it does not verify that restoring it produces a working application.

Restore drills are the administrator's job, on a real cadence, and no alert will ever
substitute for one. See
[Maintenance and verification](/CrystalBackup/docs/guides/maintenance/#restore-drills-are-yours).
