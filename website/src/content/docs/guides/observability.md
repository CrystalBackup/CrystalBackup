---
title: Observability
description: Scraping the metrics endpoint, reading logs, and the conditions and events that actually tell you what happened.
sidebar:
  order: 9
---

:::note
This page covers how to *get* the signals. The exhaustive list lives on
[Metrics](/CrystalBackup/docs/reference/metrics/) and
[Alerts](/CrystalBackup/docs/reference/alerts/) — both generated from the operator's own
registry and rule table, so they describe what this build publishes rather than what was
planned for it.
:::

## The metrics endpoint

The operator serves Prometheus metrics on port **8443**, over **HTTPS with API-server
authentication and authorisation**. That is the kubebuilder default and it is deliberate:
an unauthenticated metrics port on a backup operator leaks the shape of every tenant's
data.

All series carry the `crystalbackup_` prefix. Every one that is per-tenant carries a
`namespace` label and a `cluster` label whose value is the location's `clusterID` — so one
Prometheus can hold several clusters' fleets without collision.

### With the Prometheus Operator

```yaml
metrics:
  serviceMonitor:
    enabled: true
    interval: 30s
    scrapeTimeout: 10s
    labels:
      release: kube-prometheus-stack   # match your Prometheus' selector
```

Requires the `monitoring.coreos.com` CRDs to be present.

### Without it

The chart ships an unbound ClusterRole named `crystal-backup-metrics-reader`, granting
`get` on the non-resource URL `/metrics`. Bind it to whatever identity scrapes:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: crystal-backup-metrics-reader
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: crystal-backup-metrics-reader
subjects:
  - kind: ServiceAccount
    name: prometheus
    namespace: monitoring
```

If your CNI enforces the shipped NetworkPolicies, also set
`networkPolicy.monitoringNamespace` to the namespace your scraper runs in. Left empty, any
source is allowed on the metrics port.

## Health probes

Port **8081**, unauthenticated: `/healthz` and `/readyz`. Used by the pod's own probes;
also the quickest answer to "is the operator alive at all".

## Logs

JSON lines on stdout.

```bash
kubectl -n crystal-backup-system logs deploy/crystal-backup -f
```

Mover Jobs log separately — they are short-lived pods in the same namespace:

```bash
kubectl -n crystal-backup-system get jobs -l app.kubernetes.io/managed-by=crystal-backup
kubectl -n crystal-backup-system logs job/<name>
```

A finished mover's Job is deleted once its `ttlSecondsAfterFinished` elapses. If you are
diagnosing an intermittent failure, collect the logs while it is still there — or read the
durable record instead, which is what the next section is about.

## The signals that actually matter

Metrics tell you a fleet is healthy. Conditions and status tell you *why* one thing is not.

### Did the backup work?

```bash
kubectl get backups -A
kubectl -n <ns> get backup <run> \
  -o jsonpath='{range .status.volumes[*]}{.pvc}{"\t"}{.phase}{"\t"}{.sizeBytes}{"\t"}{.addedBytes}{"\t"}{.reason}{"\n"}{end}'
```

A `Skipped` volume with `reason: CSISnapshotUnsupported` is the one to watch for: the
backup reports `Completed`, and that PVC is not in it. It is reported rather than silently
dropped, but it is only reported if someone looks.

`addedBytes` is the deduplicated bytes this run actually added. Watching it is how you spot
a workload that has started rewriting its whole dataset nightly.

### Is the repository healthy?

```bash
kubectl get backuprepository <name> -o jsonpath='{.status.lastCheckTime}{"\t"}{.status.lastCheckResult}{"\t"}{.status.staleLocks}{"\t"}{.status.lastMaintenanceTime}{"\n"}'
```

The three signals to read here, each covered by a shipped rule if you enable the bundle —
`CrystalbackupRepositoryCheckFailed`, `CrystalbackupMaintenanceStalled` and
`CrystalbackupStaleLocks`. Without Prometheus, `crystal-backup selfcheck` evaluates the same
three from the same state:

- `lastCheckResult: Failed` — restic found repository damage. An incident.
- `lastCheckTime` far in the past — nothing has been verified in a long time. A different
  incident.
- `staleLocks` persistently non-zero — locks are accumulating faster than they are reaped,
  and every exclusive operation will eventually stall behind them.

`lastMaintenanceTime` is updated only when a prune **succeeded**, so a staleness check
against it keeps firing through repeated failures rather than being reset by them.

Why a maintenance run failed:

```bash
kubectl get backuprepository <name> \
  -o jsonpath='{range .status.recentMaintenance[*]}{.operation}{"\t"}{.startTime}{"\t"}{.result}{"\t"}{.message}{"\n"}{end}'
```

The Job and its pod are deleted as soon as the operation finishes, so this capped
newest-first history is the only durable trace of what ran and why it failed.

### Is the second copy keeping up?

```bash
kubectl get clusterbackupexternalsync,backupexternalsync -A
```

`lagSnapshots` growing run after run means the sync is falling behind. Zero means every
source snapshot has a copy — and not that the copies are readable. See
[External sync](/CrystalBackup/docs/guides/external-sync/).

### Did a hook leave something frozen?

```bash
kubectl -n <ns> get backup <run> -o jsonpath='{.status.postHookAttempts}{"\n"}'
```

A climbing count means a release hook is still failing and an application may still be
quiesced. This is the one to page on.

## Events

The operator emits Events for the transitions that need a human: confirmation required,
confirmation accepted, a volume skipped, a hook failure.

```bash
kubectl -n <ns> get events --field-selector involvedObject.kind=Restore
kubectl get events -A --field-selector reason=ConfirmationRequired
```

## Tracing

The operator honours the standard `OTEL_*` environment variables. Set them through
`extraArgs` or the deployment's environment if you have a collector.

## See also

- [Metrics](/CrystalBackup/docs/reference/metrics/)
- [Alerts](/CrystalBackup/docs/reference/alerts/)
- [Diagnosis](/CrystalBackup/docs/operations/troubleshooting/)
