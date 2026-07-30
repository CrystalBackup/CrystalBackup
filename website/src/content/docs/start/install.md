---
title: Install with Helm
description: Installing the Crystal Backup operator, the CRDs, RBAC and admission policies.
---

The chart installs the operator, the twelve CRDs, cluster-scoped RBAC, the admission
policies and default-deny NetworkPolicies for the operator namespace.

Read [Requirements](/CrystalBackup/docs/start/requirements/) first — in particular the
part about generating and escrowing the cluster KEK **before** you install.

## Install

The chart is published as an OCI artifact on GHCR.

```bash
helm install crystal-backup \
  oci://ghcr.io/crystalbackup/charts/crystal-backup \
  --version 0.5.1 \
  --namespace crystal-backup-system \
  --create-namespace
```

The chart creates the namespace itself by default (`namespace.create: true`), so
`--create-namespace` is belt and braces.

Check it came up:

```bash
kubectl -n crystal-backup-system rollout status deploy/crystal-backup
```

## What just got installed

**Namespace `crystal-backup-system`.** Every platform credential, the cluster KEK, the
wrapped platform key and every mover Job live here and nowhere else. Crystal Backup is a
singleton cluster operator; do not install it twice.

**Twelve CRDs**, packaged under the chart's `crds/`. Helm installs them on first install
and — this is Helm's behaviour, not a choice of this chart — **does not upgrade them**.
See [Upgrading](/CrystalBackup/docs/guides/upgrading/).

**Three ClusterRoles**, with stable names:

| Name | For | Grants |
|---|---|---|
| `crystal-backup-operator` | the operator's ServiceAccount | bound by the chart |
| `crystal-backup-tenant` | namespace users | full verbs on `backupschedules`, `backuplocations`, `restores`, `backupexternalsyncs`; read-only on `backups` |
| `crystal-backup-admin` | platform administrators | full verbs on the six `cluster*` kinds; read-only on `backuprepositories` |

**Neither the tenant nor the admin role is bound by the chart.** You bind them.

The tenant role carries `crystalbackup.io/aggregate-to-namespace-user: "true"` always, and
— when `rbac.aggregateToDefaultRoles` is true, which is the default — also the standard
`rbac.authorization.k8s.io/aggregate-to-edit` and `-admin` labels. With aggregation on,
anyone who already has `edit` in a namespace gains the tenant permissions there
automatically.

Note the asymmetry: `crystal-backup-admin` grants **nothing** on the namespaced kinds. An
administrator who also needs to read tenants' `Backup` objects needs the tenant role too.

**Admission policies.** Seven `ValidatingAdmissionPolicy` objects plus one small webhook.
See [Admission rules](/CrystalBackup/docs/reference/admission/).

## Provision the cluster KEK

Nothing on the cluster plane works without it. Take the age identity you generated and
escrowed:

```bash
kubectl -n crystal-backup-system create secret generic cluster-kek \
  --from-file=identity=cluster-kek.txt
```

A `ClusterBackupLocation` whose KEK Secret is missing does not fail silently — it reports
condition `EncryptionValid=False` with reason `KEKMissing`, and nothing is ever generated
in its place.

## Provision the object-storage credentials

For the cluster plane, in the operator namespace:

```bash
kubectl -n crystal-backup-system create secret generic dr-s3 \
  --from-literal=AWS_ACCESS_KEY_ID=... \
  --from-literal=AWS_SECRET_ACCESS_KEY=...
```

For a namespace-plane location, the equivalent Secret lives in **the tenant's own
namespace**, and is referenced by name only. That name-only reference is one of the
admission rules: a `BackupLocation` cannot point at a Secret in another namespace.

## Values worth setting

The full list is in [Helm values](/CrystalBackup/docs/reference/helm-values/). The ones
that usually need attention on a first install:

```yaml
# Add your incumbent backup tool's namespace, so tenant-facing Crystal Backup
# resources cannot be created there.
admission:
  deniedNamespaces:
    - "kube-*"
    - crystal-backup-system
    - velero

# An on-premises S3 endpoint on a private address: movers are denied those
# ranges by default, so it needs an explicit exception.
networkPolicy:
  extraMoverEgress:
    - to:
        - ipBlock:
            cidr: 10.20.30.40/32
      ports:
        - protocol: TCP
          port: 443

# Prometheus Operator present?
metrics:
  serviceMonitor:
    enabled: true
```

## Verify the install

```bash
# The operator is running.
kubectl -n crystal-backup-system get pods

# The CRDs are registered.
kubectl get crd -l app.kubernetes.io/managed-by=Helm | grep crystalbackup.io

# The admission policies are bound.
kubectl get validatingadmissionpolicybinding | grep crystalbackup
```

If the pod is stuck pulling its image, check that `image.digest` is a real published
digest. The chart carries a placeholder digest in source; the release pipeline substitutes
the real one at publish time, so a chart installed from a source checkout rather than from
GHCR will not pull.

## Uninstall

```bash
helm uninstall crystal-backup -n crystal-backup-system
```

Helm does **not** delete CRDs on uninstall, so your `Backup` projections and locations
survive. Nothing in the object storage is touched — deleting a location never erases
repository objects. That is deliberate: erasure is an explicit, confirmed operation. See
[The right to erasure](/CrystalBackup/docs/guides/erasure/).

Next: [Quickstart](/CrystalBackup/docs/start/quickstart/).
