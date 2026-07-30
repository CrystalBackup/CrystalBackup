---
title: Metrics
description: The crystalbackup_ Prometheus series — catalogue pending.
---

:::caution[TODO — catalogue pending]
The metrics catalogue is being reworked in M6, which is in progress and has not been
released. This page is a placeholder with the
structure it will take; the series tables are **not yet filled in** and nothing here should
be treated as a list of what exists.

For how to reach the endpoint and what to watch in the meantime, see
[Observability](/CrystalBackup/docs/guides/observability/).
:::

## Conventions

All series carry the `crystalbackup_` prefix.

Every per-tenant series carries:

- a **`namespace`** label — the origin namespace;
- a **`cluster`** label — the location's `clusterID`, so one Prometheus can hold several
  clusters' fleets without collision.

The endpoint is port **8443**, HTTPS, with API-server authentication and authorisation.
See [Observability](/CrystalBackup/docs/guides/observability/) for how to scrape it.

## Backup

<!-- TODO(M6): backup counters, durations, per-run outcomes. -->

## Volumes and data

<!-- TODO(M6): logical bytes protected, deduplicated bytes added, per-PVC results,
     skipped volumes by reason. -->

## Restore

<!-- TODO(M6): restore counters, durations, restored bytes. -->

## Repository

<!-- TODO(M6): snapshot counts, physical size, stale locks, last check result and age,
     last successful maintenance. -->

## External sync

<!-- TODO(M6): snapshots copied, bytes copied, lag. -->

## Hooks

<!-- TODO(M6): hook outcomes by phase and result, post-hook retry attempts. -->

## Coexistence

<!-- TODO(M6): per-PVC VolumeSnapshot count — this one deliberately counts snapshots from
     other tools too, since the ceph-csi per-image thresholds do not care who created them. -->

## Operator internals

<!-- TODO(M6): controller-runtime series, queue depth, mover concurrency. -->

## What the metrics are not for

Per-tenant usage series exist so a platform team can see who is generating what. The tool
does **no accounting and no billing** with them, and there is no quota mechanism — a
namespace generating far more data than expected is visible, not bounded.

Deduplicated bytes per tenant are **best-effort**: restic attributes deduplication at the
repository level, so per-tenant attribution is an estimate. The exact number is the
repository total.
