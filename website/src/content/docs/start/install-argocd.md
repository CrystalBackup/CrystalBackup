---
title: Install with Argo CD
description: Managing Crystal Backup from Git with Argo CD — what belongs in Git, what must never, and the prune that destroys your keys.
---

This is the [Helm install](/CrystalBackup/docs/start/install/) driven from Git. The chart is
the same, the values are the same, and everything that page says about the KEK, the RBAC and
the ordered uninstall still applies.

What is different is that a controller now applies, re-applies and — if you let it — deletes
these objects on its own. Three of Crystal Backup's properties interact badly with that, and
they are the reason this page exists rather than a one-line "point an `Application` at the
chart".

## Read this first

**1 — A run is not a desired state.** `ClusterBackup`, `Backup`, `Restore`, `ClusterRestore`
and `ClusterErasure` are *executions*, closer to a `Job` than to a `Deployment`. Putting one
in Git makes a controller recreate it, and a recreated run is a different run wearing the same
name. See [What goes in Git](#what-goes-in-git).

**2 — A prune can destroy your keys.** The `crystal-backup-system` namespace holds the cluster
KEK and every wrapped DEK. Any prune that removes it removes the keys, and every repository
they protect becomes permanently unreadable — that is a
[decommission](https://github.com/CrystalBackup/CrystalBackup/blob/main/docs/DECOMMISSION.md#14-the-key-itself),
executed by accident. The chart no longer renders that Namespace (`namespace.create` defaults
to `false`), which removes it from the operator `Application`'s prunable set entirely; put it
in its own Application, and keep prune off there too. See
[The namespace](#the-namespace--yours-not-the-charts) and
[Why prune is off](#why-prune-is-off-and-there-is-no-finalizer).

**3 — Removing the operator is ordered, and Argo CD does not know the order.** Six kinds carry
a finalizer that only the operator removes. Delete the operator while one of those objects is
alive and its namespace stays in `Terminating` forever. See [Removing it](#removing-it).

None of this makes Crystal Backup a bad fit for GitOps. The declarations — locations,
schedules, syncs — belong in Git and benefit from it. It is the executions and the deletions
that do not.

## What goes in Git

| Kind | In Git? | Why |
|---|---|---|
| `ClusterBackupLocation`, `BackupLocation` | **yes** | desired state: where backups go |
| `ClusterBackupSchedule`, `BackupSchedule` | **yes** | desired state: when they happen |
| `ClusterBackupExternalSync`, `BackupExternalSync` | **yes** | desired state: what replicates where |
| `ClusterBackup`, `Backup` | **no** | a run; `Backup` is also a projection the operator owns |
| `Restore`, `ClusterRestore` | **no** | a one-shot recovery, with a typed confirmation |
| `ClusterErasure` | **no** | an irreversible deletion, with a typed confirmation |
| `BackupRepository` | **no** | operator-internal |

### Why a `ClusterBackup` in Git is a trap

A `ClusterBackup` fans one child `Backup`, **named after the run**, into every matched
namespace. The run name is therefore a coordinate in the restic repository, not an identity —
and several different producers can land on the same coordinate: a discovery projection, an
earlier run of the same name, or the namespace plane's own `BackupSchedule` firing on the same
cron.

Argo CD's prune-and-recreate does exactly the thing that breaks this. It deletes the
`ClusterBackup` and creates a new object with the same name and a **new UID**, while the
previous run's child `Backup` objects — deliberately not owned by the run, because they are
restore points — stay where they are.

Every `Backup` created by a run carries the annotation `crystalbackup.io/parent-uid`, the
`metadata.uid` of the object that created it. The second run finds a `Backup` at its coordinate
with somebody else's parent UID and fails that namespace with `RunNameCollision`: a
`FailureRecord`, counted in `namespacesFailed`, taking the run to `Failed` or
`PartiallyFailed`.

That failure is the fix, not the problem. Before it existed, the second run *skipped* the
namespace and aggregated the occupant's completed volumes as its own — `namespacesSucceeded`
up, phase `Completed`, over snapshots written days earlier, with `addedBytes: 0` as the only
visible difference. A loud failure on every sync is still a bad experience, though. Do not put
runs in Git.

### Doing the same things without Git

- **Recurring backups** — a `ClusterBackupSchedule` in Git. That is the normal path and it
  stamps unique run names for you (`<schedule>-<YYYYMMDD-HHMMSS>`, UTC).
- **A one-off run** — out of band, with a name that cannot repeat:

  ```bash
  kubectl create -f - <<'EOF'
  apiVersion: crystalbackup.io/v1alpha1
  kind: ClusterBackup
  metadata:
    generateName: pre-upgrade-
  spec:
    locationRef:
      name: dr-primary
    namespaces:
      matchLabels:
        crystalbackup.io/protect: "true"
  EOF
  ```

  `generateName` requires `kubectl create` (not `apply`) and the server appends a random
  suffix, which is precisely the property you want. A convenience name like `pre-upgrade`,
  reused for the second upgrade, is the same collision by hand.
- **A restore** — out of band, always. See [Restoring](/CrystalBackup/docs/guides/restore/).

## 1. Register the chart repository

The chart is published as an OCI artifact to `ghcr.io/crystalbackup/charts` by the release
pipeline, and cosign-signed there.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: crystalbackup-charts
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: repository
stringData:
  name: crystalbackup-charts
  type: helm
  url: ghcr.io/crystalbackup/charts
  enableOCI: "true"
```

:::note
Argo CD's handling of the `oci://` scheme in a repository URL has changed across releases —
some versions want the bare `host/path` with `enableOCI: "true"`, newer ones accept or expect
the `oci://` prefix. Register it, then confirm with `argocd repo list` and
`argocd app manifests crystal-backup` that the chart actually resolves before debugging
anything else.
:::

The published packages are public, so no credentials are needed. Add `username`/`password` to
the Secret if your Argo CD reaches GHCR through a registry proxy that requires them.

:::danger[Do not point Argo CD at the Git repository's `charts/` directory]
Two things in the source tree are only filled in at release time. `charts/crystal-backup/crds/`
is **git-ignored** — the CRDs are copied in when the chart is packaged — so a chart rendered
from a Git checkout installs **no CRDs at all**. And `image.digest`, `mover.image.digest` and
`sync.image.digest` carry an all-zeroes placeholder that the release pipeline replaces, so the
operator pod would never pull. Always use the published OCI chart.
:::

## The namespace — yours, not the chart's

The chart does not create `crystal-backup-system` (`namespace.create: false`), and under Argo
CD that is worth more than it is on the Helm path: an object the chart does not render is an
object no prune of the operator `Application` can ever reach.

It still has to exist, and it still has to carry the Pod Security Admission labels — data
movers run as uid 0 with `DAC_OVERRIDE` in it to preserve file ownership on restore, and
`restricted` denies them. On `helm install` the chart reads the namespace back and refuses a
wrong `enforce` level; Argo CD renders with `helm template`, which has no cluster to read, so
**under Argo CD there is no such check.** That is why the labels are written out here rather
than cross-referenced.

Put it in its own Application, or in the same one as the Secrets, sourced from your Git
repository — with prune off:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: crystal-backup-system
  annotations:
    # Belt and braces on top of `prune: false`: never prune this object, whatever the
    # Application says. It holds the cluster KEK.
    argocd.argoproj.io/sync-options: Prune=false
  labels:
    # `baseline`, not `restricted`: the operator is restricted-compliant, but data movers run
    # as uid 0 with DAC_OVERRIDE in this namespace to preserve file ownership on restore.
    # Getting this wrong costs nothing until the first backup, which then never starts.
    pod-security.kubernetes.io/enforce: baseline
    pod-security.kubernetes.io/enforce-version: latest
    pod-security.kubernetes.io/audit: restricted
    pod-security.kubernetes.io/warn: restricted
```

Give it `argocd.argoproj.io/sync-wave: "-10"` so it lands before the Secrets (wave 0) and the
operator (wave 10).

## 2. The operator Application

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: crystal-backup
  namespace: argocd
  # No `resources-finalizer.argocd.argoproj.io` finalizer, on purpose.
  # See "Removing it" below — deleting this Application must NOT cascade.
  annotations:
    argocd.argoproj.io/sync-wave: "0"
spec:
  project: platform
  destination:
    server: https://kubernetes.default.svc
    namespace: crystal-backup-system
  source:
    repoURL: ghcr.io/crystalbackup/charts
    chart: crystal-backup
    targetRevision: 0.6.3          # the pin. Bumping this IS the upgrade.
    helm:
      # Keep the release name fixed. The chart stamps
      # `app.kubernetes.io/instance: <release name>` on every object, and Argo CD
      # otherwise derives it from the Application name.
      releaseName: crystal-backup
      values: |
        admission:
          deniedNamespaces:
            - "kube-*"
            - crystal-backup-system
        # Observability is opt-in and off means NO ALERT RULES AT ALL — nothing
        # would tell you a backup stopped running. Both need the
        # monitoring.coreos.com CRDs; drop this block if you have no Prometheus
        # Operator, and wire port 8443 into whatever you do have.
        metrics:
          serviceMonitor:
            enabled: true
          rules:
            enabled: true
            labels:
              release: kube-prometheus-stack   # your Prometheus's ruleSelector
        networkPolicy:
          # Empty — the default — lets any pod in the cluster reach the metrics
          # port. There is no default because the name is unguessable.
          monitoringNamespace: monitoring
  syncPolicy:
    automated:
      selfHeal: true
      prune: false                 # deliberate. See below.
    syncOptions:
      - ServerSideApply=true
      - CreateNamespace=false      # the namespace is a separate Application, above
  ignoreDifferences:
    # The chart mints the admission webhook's CA and serving cert at RENDER time
    # (genCA/genSignedCert, no `lookup`). Argo CD re-renders the chart on every
    # refresh, so both change every time. Without these two entries the Application
    # is permanently OutOfSync and self-heal rewrites the pair every cycle.
    - group: ""
      kind: Secret
      name: crystal-backup-webhook-certs
      namespace: crystal-backup-system
      jsonPointers:
        - /data
    - group: admissionregistration.k8s.io
      kind: ValidatingWebhookConfiguration
      name: crystal-backup
      jqPathExpressions:
        - .webhooks[].clientConfig.caBundle
```

Then provision the [Secrets](#3-secrets) and the [resources](#4-locations-and-schedules).

### Why the object names in `ignoreDifferences` are not release-prefixed

Crystal Backup is a singleton cluster operator, so the chart's `fullname` helper deliberately
does **not** prefix with the release name: the cluster-scoped RBAC objects have to be
predictable for platform binding and aggregation. So the Secret is
`crystal-backup-webhook-certs` and the `ValidatingWebhookConfiguration` is `crystal-backup`
regardless of what you call the release, unless you set `fullnameOverride`.

The alternative to ignoring the diff is `admission.webhook.enabled: false`. That is a real
option — the webhook is fail-open and enforces one rule (a second default
`ClusterBackupLocation`), with the controller's `MultipleDefaults` condition as the backstop —
but it is a reduction in checking, so make it a decision rather than a workaround.

### Why `ServerSideApply=true`

Field ownership, not size. The often-cited
`metadata.annotations: Too long: must have at most 262144 bytes` failure comes from
client-side apply stuffing the whole object into `kubectl.kubernetes.io/last-applied-configuration`,
and Crystal Backup's CRDs do not get near it — the largest, `clusterbackupschedules`, is about
20 KB of JSON against a 256 KiB limit. If you hit that error, it is not coming from here.

Use server-side apply anyway, because the CRDs and the RBAC are also written by the operator
and by Helm at various points, and SSA is the only mode that resolves that with recorded field
managers instead of silently clobbering. `Replace=true` is the sledgehammer for the annotation
problem and you do not need it; on a CRD it is also a `replace`, which is a heavier operation
than the situation calls for.

### Why prune is off, and there is no finalizer

Two separate switches, both defaulting to "destructive", both turned off here.

**`prune: false`** stops auto-sync from deleting anything that disappears from the rendered
chart. What it protects, chiefly, is the **CRDs** — see below — whose deletion takes every
`Backup` object in the cluster with it.

It used to protect one more thing: the `crystal-backup-system` **Namespace**, which the chart
rendered. Deleting that namespace destroys the cluster KEK and every wrapped DEK, and a
repository whose DEK is gone cannot be read by you, by us, or by anyone who obtains the bucket.
The chart no longer renders it, so this Application cannot prune it however you set this — but
the namespace still has to live somewhere, and wherever you put it, put `prune: false` there
too. A hazard moved is not a hazard removed.

**No `resources-finalizer.argocd.argoproj.io`** means deleting the Application is
non-cascading: Argo CD stops managing the objects and leaves them running. That is what you
want, because removing the operator is an [ordered procedure](#removing-it) and a cascading
delete does it in the wrong order by definition.

You lose real GitOps convenience this way, and that is the trade. A value removed from the
chart values does still get reconciled by self-heal; what does not happen automatically is
*deletion*, which for this operator is never a routine operation.

## The CRDs, and how Argo CD differs from Helm

[Upgrading](/CrystalBackup/docs/guides/upgrading/) says Helm installs CRDs on first install and
never upgrades them. **That is true of `helm install`/`helm upgrade`, and Argo CD does neither.**
Argo CD renders the chart with `helm template` and applies the result; `crds/` is part of that
rendered set by default, which is why `source.helm.skipCrds` exists to exclude it. So under Argo
CD the CRDs are ordinary managed resources and a chart version bump **does** update the schemas.

Confirm it on your installation rather than trusting the paragraph above — the flag has a
default and defaults move:

```bash
argocd app manifests crystal-backup | grep -c "kind: CustomResourceDefinition"
```

Twelve is the expected answer. Zero means the CRDs are not in your rendered set, and you must
apply them yourself before every upgrade:

```bash
helm pull oci://ghcr.io/crystalbackup/charts/crystal-backup --version 0.6.3 --untar
kubectl apply -f crystal-backup/crds/
```

The flip side of Argo CD managing them is that they are prunable, which is the other half of
why `prune: false` is not negotiable. Deleting the twelve CRDs deletes every `Backup`,
`BackupLocation` and `Restore` object in the cluster — and if the operator went first, blocks
forever on the finalizers nobody is left to clear.

## 3. Secrets

**The chart never creates a Secret** — not the cluster KEK, not a DEK, not object-storage
credentials. That is a design position, not an omission: a key generated inside the cluster is
lost with the cluster, which would make every backup unrecoverable.

Two Secrets are needed before the cluster plane works, both in `crystal-backup-system`:

| Secret | Key | Contents |
|---|---|---|
| `cluster-kek` | `identity` | the age identity that wraps every platform DEK |
| e.g. `dr-s3` | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | object-storage credentials |

Namespace-plane locations reference a Secret **in the tenant's own namespace**, by name only —
admission rejects a cross-namespace reference.

Crystal Backup does not care how they get there. Pick one:

- **External Secrets Operator** — an `ExternalSecret` in Git, the material in Vault / AWS Secrets
  Manager / GCP Secret Manager. The Git repository holds a reference, never a key.
- **SOPS** — Argo CD has no native SOPS support, so this means a config-management plugin
  (KSOPS or similar) on the repo-server. Note where the trust ends up: the ciphertext is in Git
  and the SOPS key becomes the thing that must be escrowed.
- **Sealed Secrets** — a `SealedSecret` in Git, decryptable only by the controller in the
  target cluster. Which also means a cluster you have lost cannot decrypt them, so this is not
  an escrow either.

:::danger[Git is not a KEK escrow]
Whatever you choose, **the cluster KEK must be escrowed outside the cluster** — that is the
point of generating it out of band. And be deliberate about a copy landing in Git: a
decommission destroys CrystalBackup's copy of a key, so a copy that survives in a Git history
means the repository is still readable and nothing has actually been destroyed.
:::

### Ordering

A `ClusterBackupLocation` whose KEK Secret is missing reports `EncryptionValid=False` with
reason `KEKMissing`. It is not fatal — the controller re-checks every 30 seconds and the
location goes `Ready` once the Secret appears — but it is noise you can avoid with a sync wave
(next section).

## 4. Locations and schedules

Put these in a **second** Application, sourced from your own Git repository. Separating them
from the chart is what gives you a deletion order: you can remove the resources and let them
finalize while the operator is still running.

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: crystal-backup-config
  namespace: argocd
  annotations:
    argocd.argoproj.io/sync-wave: "10"   # after the operator Application
spec:
  project: platform
  destination:
    server: https://kubernetes.default.svc
    namespace: crystal-backup-system
  source:
    repoURL: https://github.com/example/platform-gitops
    path: clusters/prod-eu-1/crystal-backup
    targetRevision: main
  syncPolicy:
    automated:
      selfHeal: true
      prune: false
    syncOptions:
      - ServerSideApply=true
```

In `clusters/prod-eu-1/crystal-backup/`, ordered with sync waves so the location does not spend
its first minutes reporting `KEKMissing`:

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupLocation
metadata:
  name: dr-primary
  annotations:
    argocd.argoproj.io/sync-wave: "10"   # after the Secrets, which are wave 0
spec:
  default: true
  mode: Standard
  clusterID: prod-eu-1
  s3:
    endpoint: https://s3.example.com
    bucket: crystal-backups
    prefix: dr
    forcePathStyle: true
    credentialsSecretRef:
      name: dr-s3
  encryption:
    clusterKEKSecretRef:
      name: cluster-kek
  retention:
    keepDaily: 7
    keepWeekly: 4
  maintenance:
    pruneSchedule: "0 3 * * 0"
    checkSchedule: "0 4 * * 0"
    checkReadDataSubset: "1%"
    timezone: Europe/Paris
---
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupSchedule
metadata:
  name: dr-daily
  annotations:
    argocd.argoproj.io/sync-wave: "20"
spec:
  schedule: "0 2 * * *"
  timezone: Europe/Paris
  jitter: true
  concurrencyPolicy: Forbid
  template:
    spec:
      locationRef:
        name: dr-primary
      namespaces:
        matchLabels:
          crystalbackup.io/protect: "true"
      includeManifests: true
      clusterResources:
        enabled: true
      maxConcurrentMovers: 4
```

:::caution[Sync waves order the apply, not the readiness]
Argo CD has no health check for Crystal Backup's custom resources, so they report `Healthy` the
moment they are applied and the next wave starts immediately. The waves buy you apply ordering,
which is enough here because every controller retries. Do not read a green Application as "the
location is `Ready`" — check that separately.
:::

`clusterID`, `s3.endpoint`, `s3.bucket`, `s3.prefix` and `mode` are **immutable after
creation**: together they compose the repository path. Editing one in Git produces a rejected
apply and a permanently `OutOfSync` Application, which is the correct outcome — the alternative
would be silently re-pointing the location at a different repository. To change one, create a
new location.

### One in-cluster edit that Git will undo

`admission.deniedNamespaces` renders into a ConfigMap
(`crystal-backup-denied-namespaces`) that the `ValidatingAdmissionPolicy` reads through a
`paramRef`, so it can be edited in the cluster to change the deny-list without touching the
policy. Under self-heal, that edit is reverted on the next sync. Change it in the chart values
instead.

## The soak collector, if you are evaluating

Off by default (`soak.enabled: false`) and it should stay off on a cluster you are simply
running. It is a **measurement** kit for a fortnight-long evaluation, and under Argo CD it is
one more value on the operator Application:

```yaml
  source:
    helm:
      values: |
        soak:
          enabled: true
```

What it costs: **one pod** (200m CPU / 384Mi memory, requests equal to limits), **one 1Gi
PVC**, and **cluster-wide read-only RBAC** held for the duration — its own ServiceAccount, not
the operator's, so revoking it is deleting bindings. It runs the same image as the operator,
resolved from the same digest, so it is by construction the build you are evaluating.

Two Argo CD specifics. The PVC is `ReadWriteOnce` and the Deployment uses the `Recreate`
strategy, so a sync that rolls the collector will show it `Progressing` while the old pod
releases the volume — that is normal and not a stuck sync. And turning the value back off
**deletes nothing** while `prune: false` is set: the collector keeps running and holding its
RBAC. Remove it deliberately:

```bash
kubectl -n crystal-backup-system delete deploy,pvc,sa -l crystalbackup.io/soak=collector
kubectl delete clusterrole,clusterrolebinding -l crystalbackup.io/soak=collector
```

Check on day one **and** day two that it is really collecting:

```bash
kubectl -n crystal-backup-system logs deploy/crystal-backup-soak | grep soak-heartbeat
```

One line per day, each naming what it collected. A day with no line is a day with no data —
which is why you look on day two and not on day fourteen. Protocol: `hack/soak/README.md`.

## Upgrading

**Bump `targetRevision`. That is the whole upgrade.**

The chart pins the operator, mover and sync images **by digest**, and the release pipeline
stamps the real index digests into the published chart's `values.yaml`. So there is no image
tag anywhere in your Git repository, and nothing for Argo CD Image Updater to update — the
chart version *is* the image pin. One version string covers the three images and the chart.

Two consequences:

- Do **not** set `image.tag` in your Helm values expecting it to change anything. `tag` is used
  only when `digest` is empty, and the published chart's digest is never empty. You would get a
  value that renders into nothing and an upgrade that did not happen.
- Overriding `image.digest` by hand pins the operator without moving the mover and sync
  digests, which are passed to every mover Job. A partial upgrade is not something to try.

The API is `v1alpha1` and each milestone is a minor release: read the release notes before a
minor bump, go one minor at a time, and let a backup cycle complete in between. Full detail in
[Upgrading](/CrystalBackup/docs/guides/upgrading/).

## Removing it

**Do not remove Crystal Backup by deleting things from Git.** Six kinds carry a finalizer that
only the operator removes — `crystalbackup.io/location`, `/repository`, `/backup`,
`/restore-teardown`, `/cluster-restore-teardown`. Delete the operator while one of those objects
is alive and it becomes unfinalizable: the object stays, its namespace never leaves
`Terminating`, and a later CRD deletion never returns either. A prune that takes out the
operator and the CRDs in one pass produces exactly that.

The order:

1. **Delete the config Application first** (`crystal-backup-config`), then delete the
   `ClusterBackupSchedule`, `ClusterBackupLocation` and the rest **with the operator still
   running**, following the ordered sequence in
   [Uninstall](/CrystalBackup/docs/start/install/#uninstall). Every finalizer clears while its
   owner is alive.
2. **Verify nothing is left.** This is the gate; do not continue while it prints anything:

   ```bash
   for r in restores clusterrestores backups clusterbackups backuplocations \
            clusterbackuplocations backuprepositories; do
     kubectl get "$r.crystalbackup.io" --all-namespaces --no-headers 2>/dev/null
   done
   ```
3. **Only then** delete the operator Application. Without the cascade finalizer this leaves the
   objects running, so remove them explicitly:

   ```bash
   argocd app delete crystal-backup            # non-cascading
   helm uninstall crystal-backup -n crystal-backup-system
   ```

   Keep the `crystal-backup-system` namespace unless you also mean to destroy the KEK and the
   wrapped DEKs it holds.
4. **The CRDs, only if you mean it** — this deletes every remaining object of those kinds:

   ```bash
   kubectl get crd -o name | grep crystalbackup.io | xargs -r kubectl delete --timeout=5m
   ```

### Already stuck in `Terminating`?

**Reinstall the operator. That is the fix.** A CRD stuck in `Terminating` keeps serving its
instances, so an operator brought back at the same version picks up the pending deletions, runs
the teardown it owed and clears the finalizers. Re-create the Application at the version you
removed, or install the chart directly, then restart the sequence above in order. The manual
finalizer strip is a last resort and it leaks the mover Jobs and `Retain`-parked
`VolumeSnapshotContent` objects the teardown would have collected — the full recovery, including
what to sweep afterwards, is in
[Uninstall](/CrystalBackup/docs/start/install/#uninstall).

## Verify

```bash
# Argo CD's own view.
argocd app get crystal-backup

# The operator is up, on the digest the chart pinned.
kubectl -n crystal-backup-system get deploy crystal-backup \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'

# Twelve CRDs are registered.
kubectl get crd -o name | grep -c crystalbackup.io

# The admission policies are bound.
kubectl get validatingadmissionpolicybinding | grep crystalbackup

# The location reached Ready — a green Application does not tell you this.
kubectl get clusterbackuplocations
```

Then let one scheduled run complete. An install you did not verify with a real backup is an
install you have not verified.

## Next

- [Install with Flux](/CrystalBackup/docs/start/install-flux/) — the same operator, the other
  controller, and a genuinely different CRD story.
- [Quickstart](/CrystalBackup/docs/start/quickstart/) — a first backup and restore by hand.
- [The cluster plane](/CrystalBackup/docs/guides/cluster-plane/) — schedules, namespace
  selection, cluster-scoped capture.
