---
title: Helm values
description: The chart's configurable values, grouped by what they actually affect.
---

Defaults are from the chart's own `values.yaml`. Only the values you are likely to change
are annotated; the rest are listed for completeness.

```bash
helm show values oci://ghcr.io/crystalbackup/charts/crystal-backup --version 0.6.2
```

## Namespace and naming

| Value | Default | Notes |
|---|---|---|
| `namespace.create` | `true` | The chart creates the operator namespace itself. |
| `namespace.name` | `crystal-backup-system` | Every platform credential, the cluster KEK, the wrapped platform key and every mover Job live here and nowhere else. |
| `namespace.podSecurityLabels` | `enforce: baseline`, `audit`/`warn: restricted` | `baseline` rather than `restricted` because data movers run `runAsUser: 0` with `DAC_OVERRIDE` to preserve file ownership on restore. The relaxation applies to this namespace only. |
| `fullnameOverride`, `nameOverride` | `""` | The cluster-scoped RBAC names derive from the base name. **Keep it stable** — a golden-file test pins the rendered tenant ClusterRole. |

Crystal Backup is a **singleton** cluster operator. Do not install it twice.

## Images

| Value | Default | Notes |
|---|---|---|
| `image.repository` | `ghcr.io/crystalbackup/operator` | |
| `image.digest` | placeholder in source | Production references images **by digest**. The release pipeline substitutes the real index digest at publish time, so a chart installed from a source checkout will not pull. |
| `image.tag` | `""` | Used **only** when `image.digest` is empty. Defaults to `.Chart.AppVersion`. |
| `image.pullPolicy` | `IfNotPresent` | |
| `mover.image.repository` | `ghcr.io/crystalbackup/mover` | The restic mover. **Required for real backups** — the operator passes it to every mover Job. |
| `mover.image.digest` / `.tag` | as above | |
| `sync.image.repository` | `ghcr.io/crystalbackup/sync` | restic **plus rclone**, a third image rather than a bigger mover so rclone's dependency surface stays off the backup and restore path. Pulled only when an external sync exists. |
| `sync.image.digest` / `.tag` | as above | |
| `imagePullSecrets` | `[]` | GHCR images are public. |

## Operator deployment

| Value | Default | Notes |
|---|---|---|
| `replicaCount` | `1` | More than one is safe: leader election keeps exactly one active, the rest are warm standbys. |
| `resources.requests` | `10m` CPU, `64Mi` | The work happens in mover Jobs, not here. |
| `resources.limits` | `500m` CPU, `256Mi` | |
| `extraArgs` | `[]` | Extra manager flags. |
| `nodeSelector`, `tolerations`, `affinity` | `{}` / `[]` | Use `affinity` to spread standbys across nodes or zones. |
| `podAnnotations`, `podLabels` | `{}` | |
| `priorityClassName` | `""` | Worth setting: a backup operator evicted under pressure stops taking backups. |
| `terminationGracePeriodSeconds` | `10` | |
| `podSecurityContext` | non-root `65532`, `RuntimeDefault` seccomp | |
| `securityContext` | `readOnlyRootFilesystem`, all capabilities dropped | |
| `livenessProbe`, `readinessProbe` | standard | |

## ServiceAccount and RBAC

| Value | Default | Notes |
|---|---|---|
| `serviceAccount.create` | `true` | |
| `serviceAccount.name` | `""` → `<fullname>-operator` | |
| `serviceAccount.annotations` | `{}` | Where IRSA or Workload Identity annotations go. |
| `serviceAccount.automount` | `true` | |
| `rbac.create` | `true` | Installs the operator, tenant and admin ClusterRoles. |
| `rbac.aggregateToDefaultRoles` | `true` | Also stamps `rbac.authorization.k8s.io/aggregate-to-edit` and `-admin` on the tenant ClusterRole, so anyone with `edit` in a namespace gains the tenant permissions there. A stable custom label `crystalbackup.io/aggregate-to-namespace-user` is always present regardless. |
| `manifestMover.serviceAccountName` | `""` → `<fullname>-manifest-mover` | The one mover identity that reaches the API server. |

Neither the tenant nor the admin ClusterRole is bound by the chart. You bind them.

## Metrics and health

| Value | Default | Notes |
|---|---|---|
| `metrics.port` | `8443` | HTTPS with API-server authn/authz. |
| `metrics.service.create` | `true` | |
| `metrics.service.type` | `ClusterIP` | |
| `metrics.serviceMonitor.enabled` | `false` | Requires the `monitoring.coreos.com` CRDs. |
| `metrics.serviceMonitor.interval` | `30s` | |
| `metrics.serviceMonitor.scrapeTimeout` | `10s` | |
| `metrics.serviceMonitor.labels` | `{}` | Match your Prometheus' selector. |
| `health.port` | `8081` | `/healthz` and `/readyz`. |

## Admission

| Value | Default | Notes |
|---|---|---|
| `admission.vap.enabled` | `true` | The `ValidatingAdmissionPolicy` set. Requires Kubernetes ≥ 1.30. |
| `admission.webhook.enabled` | `true` | The single-default-location check, `failurePolicy: Ignore`, with a chart-generated certificate. |
| `admission.deniedNamespaces` | `["kube-*", "crystal-backup-system"]` | **Add your incumbent backup tool's namespace.** Rendered into a ConfigMap bound by `paramRef`, so it can also be edited in-cluster. Plain names or `*`-suffixed prefixes. |

See [Admission rules](/CrystalBackup/docs/reference/admission/).

## Network policy

Default-deny for the operator namespace, plus narrow allowances — so a pod shape added
later starts with no connectivity rather than inheriting everything.

| Value | Default | Notes |
|---|---|---|
| `networkPolicy.create` | `true` | On by default. A mover necessarily holds credentials with full access to its repository — a shared repository cannot be carved up by storage policy — so this egress confinement is one of only two real controls on a compromised mover. Scoped per-tenant credentials are not coming; treat this value as load-bearing. |
| `networkPolicy.dnsNamespace` | `kube-system` | Selected by `kubernetes.io/metadata.name`. |
| `networkPolicy.clusterInternalCIDRs` | RFC1918 + link-local + loopback | Ranges movers must **not** reach on 443. This is what stops a compromised mover pivoting to in-cluster services. |
| `networkPolicy.extraMoverEgress` | `[]` | **An on-premises S3 endpoint on a private address needs an entry here.** The default is closed and the exception is visible. |
| `networkPolicy.extraOperatorEgress` | `[]` | |
| `networkPolicy.apiServerCIDRs` | `[]` | Empty allows the port broadly — it works, but is not narrow. Set it to your API server's address. |
| `networkPolicy.apiServerPort` | `443` | |
| `networkPolicy.webhookPort` | `9443` | |
| `networkPolicy.metricsPort` | `8443` | |
| `networkPolicy.monitoringNamespace` | `""` | Empty allows any source on the metrics port. |
| `networkPolicy.moverManagedByValue` | `crystal-backup` | Must match what the operator stamps on mover pods. |

:::caution[Enforcement is your CNI's job]
Some CNIs accept NetworkPolicy objects and enforce nothing — Kind's default `kindnet` among
them. Their presence is not by itself evidence the confinement holds. Verify on your CNI.
:::

## Soak collector

Off by default. When enabled it adds **one resident pod** (200m CPU / 384Mi memory, requests
equal to limits) and **one PVC**, and grants a **read-only, cluster-wide** ServiceAccount that is
separate from the operator's. It exists to answer questions this project cannot answer from CI —
what the mover memory profiles should be on real data, what a fortnight of real scheduling does —
and it is meant to be turned on deliberately, left for two weeks, then exported and turned off.

The protocol, and what to check on day one, is in
[`hack/soak/README.md`](https://github.com/CrystalBackup/CrystalBackup/blob/main/hack/soak/README.md).

| Value | Default | Notes |
|---|---|---|
| `soak.enabled` | `false` | Renders the collector Deployment, its PVC and its RBAC. Nothing is created while this is false. |
| `soak.saltMethod` | `auto` | `auto` derives the redaction salt from the operator namespace's UID and creates no Secret; `fromSecret` uses one you created. The two produce archives with **different reversibility guarantees** — read the archive's own redaction block before sending it anywhere. |
| `soak.saltSecret` | `""` | Required by, and only by, `saltMethod: fromSecret`. Setting both, or neither, is refused at template time rather than in a CrashLoopBackOff. |
| `soak.storage` | `1Gi` | The PVC request. If your default StorageClass is node-backed (local-path), this **is** node disk. |
| `soak.maxBytes` | `512Mi` | The hard cap the collector honours, rotating the oldest data away rather than growing. Deliberately below the PVC size. |
| `soak.kubeletStats` | `false` | Binds the `nodes/proxy` ClusterRole, the only source for the restic cache high-water. The role is rendered either way, and left unbound until you set this. |
| `soak.metricsInterval` | `60s` | Operator scrape cadence. |
| `soak.metricsResolution` | `5m` | Window the scrapes are aggregated into. |
| `soak.moverSampleInterval` | `15s` | How often mover pods are **sampled** (metrics.k8s.io, kubelet cache stats). It does not decide whether a mover is seen at all: the exact per-mover figures arrive on a watch, because a mover Job lives ten to twenty seconds and no poll interval catches that reliably. |
| `soak.selfcheckInterval` | `24h` | The daily installation self-check. |
| `soak.stateInterval` | `1h` | CR-status snapshots. |

Check it is working on **day one and day two**, not on day fourteen:

```sh
kubectl -n crystal-backup-system logs deploy/crystal-backup-soak | grep soak-heartbeat | tail -7
```

One line a day. `silent=none` is what you want; `movers_by_class=` should show a non-zero count
for every class your schedules actually exercise — a class at zero while backups are running
means the instrument is blind, not that your workload was idle.

## Reserved

| Value | Default | Notes |
|---|---|---|
| `clusterID` | `""` | Reserved. Not yet wired into the manager flags; a later milestone consumes it. The cluster identity that matters today is `spec.clusterID` on the location. |

## What the chart never does

**It never creates a Secret.** Not the cluster KEK, not a data key, not credentials.

The cluster KEK is your root of trust: generate it out of band with `age-keygen`, escrow it
**outside** the cluster, and provision it yourself before creating a
`ClusterBackupLocation`. A key generated inside the cluster would be lost with the cluster,
and every backup with it.
