---
title: Install with Helm
description: Installing the Crystal Backup operator, the CRDs, RBAC and admission policies.
---

The chart installs the operator, the twelve CRDs, cluster-scoped RBAC, the admission
policies and default-deny NetworkPolicies for the operator namespace.

Read [Requirements](/CrystalBackup/docs/start/requirements/) first — in particular the
part about generating and escrowing the cluster KEK **before** you install.

## Install

The namespace first — the chart does not create it, and
[Requirements](/CrystalBackup/docs/start/requirements/#the-operator-namespace--create-it-before-you-install)
explains why. If you already made it for the cluster KEK, skip to the `helm install`:

```bash
kubectl create namespace crystal-backup-system

kubectl label namespace crystal-backup-system \
  pod-security.kubernetes.io/enforce=baseline \
  pod-security.kubernetes.io/enforce-version=latest \
  pod-security.kubernetes.io/audit=restricted \
  pod-security.kubernetes.io/warn=restricted \
  --overwrite
```

The chart is published as an OCI artifact on GHCR.

```bash
helm install crystal-backup \
  oci://ghcr.io/crystalbackup/charts/crystal-backup \
  --version 0.6.7 \
  --namespace crystal-backup-system
```

No `--create-namespace`, and no `namespace.create: true`. The chart deliberately does not own
the namespace: the cluster KEK Secret lives in it and has to exist before the operator does, so
by install time the namespace is already there — and a chart that rendered a `Namespace` object
would be asking Helm to adopt it, which Helm refuses with
`invalid ownership metadata; label validation error: missing key "app.kubernetes.io/managed-by"`.
Not owning it also means no prune and no `helm uninstall` can take the KEK with it.

Those PSA labels are not decoration. `helm install` and `helm upgrade` read the namespace back
and **refuse to install** onto one whose `enforce` level disagrees, printing the exact
`kubectl label` command — because the alternative is an operator that starts, locations that go
`Ready`, and a first backup that fails weeks later as a mover pod nothing will admit.

:::caution[Installing from Git instead?]
Do not translate the command above into an `Application` or a `HelmRelease` unaided. A GitOps
controller prunes, recreates and uninstalls on its own, and all three are hazardous here: a
prune can delete the namespace holding your cluster KEK — keep `namespace.create` at its
default `false` and the release cannot be the thing that deletes it — a recreated
`ClusterBackup` collides with the run it is named after, and an unordered uninstall strands
namespaces in `Terminating` permanently. The procedures handle each of those explicitly:

- [Install with Argo CD](/CrystalBackup/docs/start/install-argocd/)
- [Install with Flux](/CrystalBackup/docs/start/install-flux/)
:::

Check it came up:

```bash
kubectl -n crystal-backup-system rollout status deploy/crystal-backup
```

## What just got installed

**Nothing in `crystal-backup-system` that you did not already have.** The namespace is
yours — the chart installs *into* it and never owns it. Every platform credential, the
cluster KEK, the wrapped platform key and every mover Job live here and nowhere else.
Crystal Backup is a singleton cluster operator; do not install it twice.

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

  # Which namespace may open a connection to the metrics port. Empty — the default —
  # allows any pod in the cluster. See "Observability is opt-in" below.
  monitoringNamespace: monitoring
```

## Observability is opt-in, and off means no alerts

Three values are off by default, and a first install that leaves them off is missing the half
of this product that tells you it stopped working. They are off because each needs something
the chart cannot assume:

| Value | Default | What off actually means |
|---|---|---|
| `metrics.serviceMonitor.enabled` | `false` | Nothing scrapes the operator. Needs the `monitoring.coreos.com` CRDs. |
| `metrics.rules.enabled` | `false` | **No alert rules exist.** Nothing will tell you a backup stopped running. Same CRD requirement, and the twelve thresholds are platform policy — read them before turning them on. |
| `networkPolicy.monitoringNamespace` | `""` | Any pod in the cluster may open a connection to the metrics port. (It is HTTPS with API-server authn/authz, so an unauthorised scrape gets a 403 — but the ingress itself is open.) |

If you run the Prometheus Operator, all three:

```yaml
metrics:
  serviceMonitor:
    enabled: true
  rules:
    enabled: true
    # A Prometheus Operator only loads rules matching its `ruleSelector`. An unlabelled
    # PrometheusRule is installed, valid, and completely ignored.
    labels:
      release: kube-prometheus-stack
networkPolicy:
  monitoringNamespace: monitoring   # your Prometheus's namespace, whatever it is called
```

`monitoringNamespace` has no default because the chart cannot guess the name — `monitoring`,
`monitoring-system`, `observability` and `kube-prometheus-stack` are all real, and the wrong
one is a metrics outage that looks exactly like a working install. Read the rules first:

```bash
helm show readme oci://ghcr.io/crystalbackup/charts/crystal-backup --version 0.6.7
helm pull oci://ghcr.io/crystalbackup/charts/crystal-backup --version 0.6.7 --untar
less crystal-backup/rules/crystalbackup.rules.yaml
```

If you do not run the Prometheus Operator, the metrics are still there on port 8443 over HTTPS
— scrape them however you like. What you cannot do is nothing, and assume something is
watching. See [Alert rules](/CrystalBackup/docs/reference/alerts/).

## The soak collector, if you are evaluating

Off by default (`soak.enabled: false`), and it should stay off on a cluster you are simply
running. It is a **measurement** kit for a fortnight-long evaluation: it answers "what did this
actually cost, on my data, over two weeks" without a Prometheus anywhere.

```bash
helm upgrade crystal-backup \
  oci://ghcr.io/crystalbackup/charts/crystal-backup \
  --version 0.6.7 -n crystal-backup-system \
  --reuse-values --set soak.enabled=true
```

What it costs, stated up front: **one pod** (200m CPU / 384Mi memory, requests equal to
limits), **one 1Gi PVC** of which it uses at most `soak.maxBytes`, and **cluster-wide
read-only RBAC** held for the duration — a separate ServiceAccount from the operator's, so
revoking it is deleting bindings rather than editing the operator's. It runs the same image as
the operator, so it is by construction the build you are evaluating.

Check on day one **and** day two that it is really collecting:

```bash
kubectl -n crystal-backup-system logs deploy/crystal-backup-soak | grep soak-heartbeat
```

One line per day, each naming what it collected. A day with no line is a day with no data —
which is the whole reason to look on day two rather than on day fourteen. Full protocol,
including how to export and what redaction promises: `hack/soak/README.md`.

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

**Uninstalling is ordered, and the order is not a preference.** Six of the twelve kinds
carry a finalizer — `crystalbackup.io/location`, `/repository`, `/backup`,
`/restore-teardown`, `/cluster-restore-teardown` — and the operator is the only process
that removes one. Delete the operator while any such object is still alive and that object
can never be deleted: its namespace stops at `Terminating` **permanently**, and a later
`kubectl delete crd` waits on it forever. `helm uninstall` will not warn you; it succeeds,
and the damage appears afterwards.

Every command below is bounded with `--timeout` on purpose. An unbounded `kubectl delete`
in this sequence is a terminal you end up killing, with nothing to show for it.

If what you need is not the sequence but what each of these deletions actually removes — in the
cluster, in the CSI layer and in the repository — that is a table in
[Removing Crystal Backup](/CrystalBackup/docs/operations/uninstall/).

**1. Stop what creates new work.**

```bash
kubectl delete clusterbackupschedule --all --timeout=2m
kubectl delete clusterbackupexternalsync --all --timeout=2m
kubectl delete backupschedule --all --all-namespaces --timeout=2m
kubectl delete backupexternalsync --all --all-namespaces --timeout=2m
```

**2. Delete the finalized objects, with the operator still running** — restores and backups
before the locations that address their repository:

```bash
kubectl delete restore        --all --all-namespaces --timeout=5m
kubectl delete clusterrestore --all --timeout=5m
kubectl delete clusterbackup  --all --timeout=5m
kubectl delete backup         --all --all-namespaces --timeout=5m
kubectl delete backuplocation --all --all-namespaces --timeout=5m
kubectl delete clusterbackuplocation --all --timeout=5m
```

Nothing in the object storage is touched — deleting a location never erases repository
objects. That is deliberate: erasure is an explicit, confirmed operation. See
[The right to erasure](/CrystalBackup/docs/guides/erasure/).

**3. Verify before going further.** This is the gate; do not continue while it prints
anything:

```bash
for r in restores clusterrestores backups clusterbackups backuplocations \
         clusterbackuplocations backuprepositories; do
  kubectl get "$r.crystalbackup.io" --all-namespaces --no-headers 2>/dev/null
done
```

Silence means every finalizer has been cleared by the operator that owns it. Output means
something is still finalizing — investigate it **now**, while the operator that can still
fix it is running (`kubectl logs -n crystal-backup-system deploy/crystal-backup`).

**4. Remove the operator.**

```bash
helm uninstall crystal-backup -n crystal-backup-system
```

Helm does **not** delete the CRDs, so your `Backup` projections survive. It also does not
delete the namespace, because it never owned it — that is the point of `namespace.create:
false`. Keep `crystal-backup-system` unless you also mean to destroy the cluster KEK and the
wrapped DEKs it holds; deleting those makes every repository they protect permanently
unreadable.

(If you installed with `namespace.create: true`, `helm uninstall` **does** delete the
namespace, and the keys with it. Move the Secrets out first, or do not use that value.)

**5. Remove the CRDs, only if you mean it.** This deletes every remaining object of those
kinds:

```bash
kubectl get crd -o name | grep crystalbackup.io | xargs -r kubectl delete --timeout=5m
```

After step 3 passes, this returns in seconds. Run it before, and it blocks forever.

### Already stuck in Terminating?

```bash
kubectl get ns <ns> -o jsonpath='{.status.phase}{"\n"}'
kubectl get backup -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{" "}{.metadata.finalizers}{"\n"}{end}'
```

**Reinstall the operator — that is the fix.** The objects are still served (a CRD stuck in
`Terminating` keeps serving its instances), so an operator brought back at the same version
picks up the pending deletions, runs the teardown it owed, and clears the finalizers. Then
restart the sequence above, in order.

```bash
helm install crystal-backup oci://ghcr.io/crystalbackup/charts/crystal-backup \
  --version <the version you removed> -n crystal-backup-system
```

Only if you cannot reinstall, strip the finalizer by hand — this unblocks the namespace and
**leaks** the mover Jobs and the `Retain`-parked `VolumeSnapshotContent` objects the
teardown would have collected, which you then have to sweep yourself:

```bash
kubectl patch backup <name> -n <ns> --type=merge -p '{"metadata":{"finalizers":null}}'
kubectl -n crystal-backup-system delete job -l app.kubernetes.io/managed-by=crystal-backup
kubectl get volumesnapshotcontent -l app.kubernetes.io/managed-by=crystal-backup
```

Do **not** blanket-delete Secrets by that label in the operator namespace: the wrapped DEKs
carry it too, and deleting one makes its repository unreadable for good.

Next: [Quickstart](/CrystalBackup/docs/start/quickstart/).
