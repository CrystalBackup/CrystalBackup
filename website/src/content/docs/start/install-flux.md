---
title: Install with Flux
description: Managing Crystal Backup from Git with Flux — the CRD upgrade you have to ask for, the prune that destroys your keys, and what must never be reconciled.
---

This is the [Helm install](/CrystalBackup/docs/start/install/) driven from Git. The chart is
the same, the values are the same, and everything that page says about the KEK, the RBAC and
the ordered uninstall still applies.

What is different is that a controller now installs, upgrades and — if you let it — uninstalls
the release on its own. Three of Crystal Backup's properties interact badly with that, and they
are the reason this page exists rather than a one-line "point a `HelmRelease` at the chart".

## Read this first

**1 — A run is not a desired state.** `ClusterBackup`, `Backup`, `Restore`, `ClusterRestore`
and `ClusterErasure` are *executions*, closer to a `Job` than to a `Deployment`. Reconciling one
means recreating it, and a recreated run is a different run wearing the same name. See
[What goes in Git](#what-goes-in-git).

**2 — Removing a `HelmRelease` runs `helm uninstall`.** The `crystal-backup-system` namespace
holds the cluster KEK and every wrapped DEK, and every repository they protect becomes
permanently unreadable if it goes — a
[decommission](https://github.com/CrystalBackup/CrystalBackup/blob/main/docs/DECOMMISSION.md#14-the-key-itself)
executed by accident, starting with someone deleting a file from Git. The chart no longer
renders that Namespace, so `helm uninstall` no longer deletes it; the rest of the ordering
problem is unchanged. See [Protecting the namespace](#protecting-the-namespace).

**3 — Removing the operator is ordered, and Flux does not know the order.** Six kinds carry a
finalizer that only the operator removes. Delete the operator while one of those objects is
alive and its namespace stays in `Terminating` forever. See [Removing it](#removing-it).

None of this makes Crystal Backup a bad fit for GitOps. The declarations — locations, schedules,
syncs — belong in Git and benefit from it. It is the executions and the deletions that do not.

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

Any delete-and-recreate does the thing that breaks this: the object comes back with the same
name and a **new UID**, while the previous run's child `Backup` objects — deliberately not owned
by the run, because they are restore points — stay where they are. In Flux you get there two
ways: pruning the resource out and back in, or a `Kustomization` with `spec.force: true`
recreating it on an immutable-field conflict.

Every `Backup` created by a run carries the annotation `crystalbackup.io/parent-uid`, the
`metadata.uid` of the object that created it. The second run finds a `Backup` at its coordinate
with somebody else's parent UID and fails that namespace with `RunNameCollision`: a
`FailureRecord`, counted in `namespacesFailed`, taking the run to `Failed` or `PartiallyFailed`.

That failure is the fix, not the problem. Before it existed, the second run *skipped* the
namespace and aggregated the occupant's completed volumes as its own — `namespacesSucceeded` up,
phase `Completed`, over snapshots written days earlier, with `addedBytes: 0` as the only visible
difference. A loud failure on every reconcile is still a bad experience, though. Do not put runs
in Git.

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

  `generateName` requires `kubectl create` (not `apply`) and the server appends a random suffix,
  which is precisely the property you want. A convenience name like `pre-upgrade`, reused for
  the second upgrade, is the same collision by hand.
- **A restore** — out of band, always. See [Restoring](/CrystalBackup/docs/guides/restore/).

## 1. The chart source

The chart is published as an OCI artifact to `ghcr.io/crystalbackup/charts` by the release
pipeline, and cosign-signed there.

```yaml
apiVersion: source.toolkit.fluxcd.io/v1beta2
kind: OCIRepository
metadata:
  name: crystal-backup
  namespace: flux-system
spec:
  interval: 1h
  url: oci://ghcr.io/crystalbackup/charts/crystal-backup
  ref:
    tag: "0.6.7"        # the pin. Bumping this IS the upgrade.
```

:::note
`OCIRepository` graduated from `v1beta2` to `v1` in recent Flux releases. Use whichever your
cluster serves — `kubectl api-resources | grep ocirepositories` — the fields above are the same
in both.
:::

The published packages are public, so no `secretRef` is needed. Pin harder with `ref.digest`
instead of `ref.tag` if your policy calls for it.

### Optionally, verify the signature

The release workflow cosign-signs the pushed chart keyless, so Flux can refuse to reconcile an
unsigned one:

```yaml
spec:
  verify:
    provider: cosign
    matchOIDCIdentity:
      - issuer: "^https://token\\.actions\\.githubusercontent\\.com$"
        subject: "^https://github\\.com/CrystalBackup/CrystalBackup/\\.github/workflows/images\\.yml@refs/tags/v.*$"
```

Confirm the identity against the release you are actually pinning **before** you enable this —
a `verify` block that does not match turns a working install into a stalled `OCIRepository`:

```bash
cosign verify ghcr.io/crystalbackup/charts/crystal-backup:0.6.7 \
  --certificate-oidc-issuer=https://token.actions.githubusercontent.com \
  --certificate-identity-regexp='^https://github.com/CrystalBackup/CrystalBackup/'
```

:::danger[Do not point Flux at the Git repository's `charts/` directory]
Two things in the source tree are only filled in at release time.
`charts/crystal-backup/crds/` is **git-ignored** — the CRDs are copied in when the chart is
packaged — so a chart rendered from a Git checkout installs **no CRDs at all**. And
`image.digest`, `mover.image.digest` and `sync.image.digest` carry an all-zeroes placeholder
that the release pipeline replaces, so the operator pod would never pull. Always use the
published OCI chart.
:::

## 2. The HelmRelease

```yaml
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: crystal-backup
  namespace: flux-system
spec:
  interval: 1h
  releaseName: crystal-backup          # keep it fixed: the chart stamps
                                       # app.kubernetes.io/instance from it
  targetNamespace: crystal-backup-system
  chartRef:
    kind: OCIRepository
    name: crystal-backup
  install:
    # Helm installs crds/ on first install. `Create` is the default; it is spelled
    # out here so the pair with `upgrade.crds` below reads as one decision.
    crds: Create
  upgrade:
    # NOT the default. Helm's default is Skip: it installs CRDs once and never
    # touches them again, so a version bump would give you a new operator against
    # old schemas — fields you set are silently pruned by the API server and the
    # operator reconciles as though you never set them.
    crds: CreateReplace
  # driftDetection stays DISABLED (the default). See "The webhook certificate" below.
  values:
    admission:
      deniedNamespaces:
        - "kube-*"
        - crystal-backup-system
    # Observability is opt-in, and off means NO ALERT RULES AT ALL — nothing would tell
    # you a backup stopped running. Both values need the monitoring.coreos.com CRDs, which
    # is why they are off by default; drop this block if you have no Prometheus Operator
    # and wire port 8443 into whatever you do have.
    metrics:
      serviceMonitor:
        enabled: true
      rules:
        enabled: true
        labels:
          release: kube-prometheus-stack     # your Prometheus's ruleSelector
    networkPolicy:
      # Empty — the default — lets any pod in the cluster reach the metrics port. There is
      # no default because `monitoring`, `monitoring-system`, `observability` and
      # `kube-prometheus-stack` are all real names and the wrong one is a silent outage.
      monitoringNamespace: monitoring
```

If your Prometheus Operator lives in another cluster-management repository, add its
`Kustomization` to this `HelmRelease`'s `dependsOn` — a `HelmRelease` that renders a
`ServiceMonitor` before the CRD exists fails the install, not just that object.

`CreateReplace` creates missing CRDs and replaces existing ones. It never deletes one, which is
the property that matters here: deleting the twelve CRDs deletes every `Backup`,
`BackupLocation` and `Restore` object in the cluster.

### This is where Flux and Argo CD genuinely differ

[Upgrading](/CrystalBackup/docs/guides/upgrading/) says Helm installs CRDs on first install and
never upgrades them. Flux's helm-controller runs **real Helm install and upgrade operations**,
so that rule applies to it exactly, and `upgrade.crds: CreateReplace` is how you opt out of it.
Argo CD renders with `helm template` and applies the output, so its CRD behaviour is a different
problem with a different answer — see [Install with Argo CD](/CrystalBackup/docs/start/install-argocd/).

If you would rather manage the CRDs yourself — reasonable, since it makes the schema change a
visible commit — set `install.crds: Skip` and `upgrade.crds: Skip`, vendor the CRDs into your Git
repository, and apply them from a `Kustomization` the `HelmRelease` `dependsOn`. Extract them
with:

```bash
helm pull oci://ghcr.io/crystalbackup/charts/crystal-backup --version 0.6.7 --untar
ls crystal-backup/crds/
```

## The webhook certificate

The chart mints the admission webhook's CA and serving certificate at **render** time
(`genCA`/`genSignedCert`, with no `lookup`), so every render produces a new pair. helm-controller
only renders on install and upgrade, so under Flux this is not continuous churn — a `helm upgrade`
re-issues the cert and rolls the pod, which the chart accepts for a fail-open webhook that
enforces one rule.

It becomes churn the moment you turn on drift detection:

```yaml
spec:
  driftDetection:
    mode: enabled          # <- this will fight the chart
```

With drift detection enabled, helm-controller compares the cluster against a fresh render and
sees the `crystal-backup-webhook-certs` Secret and the `caBundle` on the
`ValidatingWebhookConfiguration` differ every time. Leave it disabled, or exclude those two
paths:

```yaml
spec:
  driftDetection:
    mode: enabled
    ignore:
      - paths: ["/data"]
        target:
          kind: Secret
          name: crystal-backup-webhook-certs
      - paths: ["/webhooks/0/clientConfig/caBundle"]
        target:
          kind: ValidatingWebhookConfiguration
          name: crystal-backup
```

Those names are not release-prefixed, and that is deliberate: Crystal Backup is a singleton
cluster operator, so the chart's `fullname` helper keeps cluster-scoped names predictable for
platform binding and aggregation. They are `crystal-backup-*` whatever you call the release,
unless you set `fullnameOverride`.

The alternative is `admission.webhook.enabled: false`. That is a real option — the webhook is
fail-open and enforces one rule (a second default `ClusterBackupLocation`), with the controller's
`MultipleDefaults` condition as the backstop — but it is a reduction in checking, so make it a
decision rather than a workaround.

## Protecting the namespace

This section used to open by telling you to set `namespace.create: false`. **That is now the
chart's default** — the release does not render the `crystal-backup-system` Namespace and
`helm uninstall` therefore does not delete it. The namespace has to come from somewhere all the
same, and where you put it decides whether a deleted file can still become an unreadable
repository.

Two guards, and you want both:

**Manage the Namespace from a `Kustomization` with pruning disabled**, so nothing in the
delivery path can remove it:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: crystal-backup-system
  annotations:
    kustomize.toolkit.fluxcd.io/prune: disabled
  labels:
    # Required, and the chart cannot apply them: it does not own this object. `baseline`,
    # not `restricted` — the operator is restricted-compliant, but data movers run as uid 0
    # with DAC_OVERRIDE in this namespace to preserve file ownership on restore, which
    # restricted would deny. Nothing fails at install if these are wrong; the FIRST BACKUP
    # fails, as a mover pod that never gets admitted.
    pod-security.kubernetes.io/enforce: baseline
    pod-security.kubernetes.io/enforce-version: latest
    pod-security.kubernetes.io/audit: restricted
    pod-security.kubernetes.io/warn: restricted
```

Flux's helm-controller runs real Helm operations, so unlike Argo CD you do get the chart's own
check here: it reads the namespace back at install and upgrade, and refuses an `enforce` level
that disagrees, printing the `kubectl label` command. The labels above are what makes that
check pass rather than something it duplicates.

**Do not prune the operator's own `Kustomization`.** Set `prune: false` on the `Kustomization`
that carries the `HelmRelease`. Removing the operator is an
[ordered procedure](#removing-it), and an automatic uninstall does it in the wrong order by
definition.

:::caution[Do not set `namespace.create: true` to save a file]
It would give the release ownership of the namespace again, and then a pruned `HelmRelease` is
a `helm uninstall` that deletes the cluster KEK. The separate `Kustomization` is three lines of
YAML against a class of failure with no recovery.
:::

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

- **SOPS** — Flux decrypts it natively, no plugin: `spec.decryption.provider: sops` on the
  `Kustomization`, with the age or KMS key in a Secret in `flux-system`. Note where the trust
  ends up: the ciphertext is in Git and the SOPS key becomes the thing that must be escrowed.
- **External Secrets Operator** — an `ExternalSecret` in Git, the material in Vault / AWS Secrets
  Manager / GCP Secret Manager. The Git repository holds a reference, never a key.
- **Sealed Secrets** — a `SealedSecret` in Git, decryptable only by the controller in the target
  cluster. Which also means a cluster you have lost cannot decrypt them, so this is not an escrow
  either.

:::danger[Git is not a KEK escrow]
Whatever you choose, **the cluster KEK must be escrowed outside the cluster** — that is the point
of generating it out of band. And be deliberate about a copy landing in Git: a decommission
destroys CrystalBackup's copy of a key, so a copy that survives in a Git history means the
repository is still readable and nothing has actually been destroyed.
:::

## 4. Wiring it together

Three `Kustomization` objects, ordered by `dependsOn`. Unlike sync waves, `dependsOn` waits for
the dependency to become **Ready**, so this really does sequence.

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: crystal-backup-namespace
  namespace: flux-system
spec:
  interval: 1h
  sourceRef: { kind: GitRepository, name: platform-gitops }
  path: ./clusters/prod-eu-1/crystal-backup/namespace
  prune: false          # the namespace holds the KEK
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: crystal-backup-secrets
  namespace: flux-system
spec:
  interval: 1h
  dependsOn:
    - name: crystal-backup-namespace
  sourceRef: { kind: GitRepository, name: platform-gitops }
  path: ./clusters/prod-eu-1/crystal-backup/secrets
  prune: false          # pruning the KEK Secret is a decommission
  decryption:
    provider: sops
    secretRef: { name: sops-age }
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: crystal-backup-operator
  namespace: flux-system
spec:
  interval: 1h
  dependsOn:
    - name: crystal-backup-secrets
  sourceRef: { kind: GitRepository, name: platform-gitops }
  path: ./clusters/prod-eu-1/crystal-backup/operator   # OCIRepository + HelmRelease
  prune: false          # removing the operator is a runbook, not a prune
```

Then the declarations, in a fourth `Kustomization` that depends on the operator — kept separate
precisely so you can remove *them* first, and let their finalizers clear while the operator is
still running:

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: crystal-backup-config
  namespace: flux-system
spec:
  interval: 1h
  dependsOn:
    - name: crystal-backup-operator
  sourceRef: { kind: GitRepository, name: platform-gitops }
  path: ./clusters/prod-eu-1/crystal-backup/config
  prune: true
  # force: false — the default, and leave it there. `force` recreates a resource
  # when a patch fails on an immutable field, which is delete-and-recreate: on a
  # ClusterBackupLocation that re-points the repository, and on any run object it
  # is the collision described at the top of this page.
```

`clusters/prod-eu-1/crystal-backup/config/` holds the declarations:

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupLocation
metadata:
  name: dr-primary
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

`clusterID`, `s3.endpoint`, `s3.bucket`, `s3.prefix` and `mode` are **immutable after creation**:
together they compose the repository path. Editing one in Git gives you a `Kustomization` that
will not reconcile, which is the correct outcome — the alternative would be silently re-pointing
the location at a different repository. To change one, create a new location. (This is also the
concrete reason `force: true` is dangerous here: it would resolve that conflict by deleting the
location.)

A `ClusterBackupLocation` whose KEK Secret is missing is not fatal, incidentally: it reports
`EncryptionValid=False` with reason `KEKMissing`, and the controller re-checks every 30 seconds,
so it goes `Ready` once the Secret lands. The `dependsOn` chain above just spares you the noise.

### One in-cluster edit that Flux will undo

`admission.deniedNamespaces` renders into a ConfigMap (`crystal-backup-denied-namespaces`) that
the `ValidatingAdmissionPolicy` reads through a `paramRef`, so it can be edited in the cluster to
change the deny-list without touching the policy. With drift detection on, that edit is reverted.
Change it in the `HelmRelease` values instead.

## The soak collector, if you are evaluating

Off by default (`soak.enabled: false`) and it should stay off on a cluster you are simply
running. It is a **measurement** kit for a fortnight-long evaluation, and under Flux it is one
more value on the `HelmRelease`:

```yaml
spec:
  values:
    soak:
      enabled: true
```

What it costs: **one pod** (200m CPU / 384Mi memory, requests equal to limits), **one 1Gi
PVC**, and **cluster-wide read-only RBAC** held for the duration — its own ServiceAccount, not
the operator's, so revoking it is deleting bindings. It runs the same image as the operator,
resolved from the same digest, so it is by construction the build you are evaluating.

Two Flux specifics. Setting the value back to `false` on a real Helm upgrade **does** remove
the objects, unlike a pruneless Argo CD — helm-controller runs a genuine `helm upgrade` and
the rendered set shrinks. And the collector's PVC is `ReadWriteOnce` with a `Recreate`
strategy, so an upgrade that rolls it shows a moment of unavailability while the old pod
releases the volume; do not read that as a failed release.

Check on day one **and** day two that it is really collecting:

```bash
kubectl -n crystal-backup-system logs deploy/crystal-backup-soak | grep soak-heartbeat
```

One line per day, each naming what it collected. A day with no line is a day with no data —
which is why you look on day two and not on day fourteen. Protocol: `hack/soak/README.md`.

## Upgrading

**Bump `ref.tag` on the `OCIRepository`. That is the whole upgrade** — provided
`upgrade.crds: CreateReplace` is set, which is the one thing on this page that is easy to leave
out and expensive to discover later.

The chart pins the operator, mover and sync images **by digest**, and the release pipeline stamps
the real index digests into the published chart's `values.yaml`. So there is no image tag
anywhere in your Git repository, and nothing for Flux's image-automation controllers to
update — the chart version *is* the image pin. One version string covers the three images and
the chart.

Two consequences:

- Do **not** set `image.tag` in your `HelmRelease` values expecting it to change anything. `tag`
  is used only when `digest` is empty, and the published chart's digest is never empty. You would
  get a value that renders into nothing and an upgrade that did not happen.
- Overriding `image.digest` by hand pins the operator without moving the mover and sync digests,
  which are passed to every mover Job. A partial upgrade is not something to try.

The API is `v1alpha1` and each milestone is a minor release: read the release notes before a
minor bump, go one minor at a time, and let a backup cycle complete in between. Full detail in
[Upgrading](/CrystalBackup/docs/guides/upgrading/).

## Removing it

**Do not remove Crystal Backup by deleting files from Git.** Six kinds carry a finalizer that
only the operator removes — `crystalbackup.io/location`, `/repository`, `/backup`,
`/restore-teardown`, `/cluster-restore-teardown`. Delete the operator while one of those objects
is alive and it becomes unfinalizable: the object stays, its namespace never leaves
`Terminating`, and a later CRD deletion never returns either. A prune that removes the
`HelmRelease` while `Backup` objects are alive produces exactly that.

The order:

1. **Delete the config `Kustomization` first**, then delete the `ClusterBackupSchedule`,
   `ClusterBackupLocation` and the rest **with the operator still running**, following the
   ordered sequence in [Uninstall](/CrystalBackup/docs/start/install/#uninstall). Every finalizer
   clears while its owner is alive.
2. **Verify nothing is left.** This is the gate; do not continue while it prints anything:

   ```bash
   for r in restores clusterrestores backups clusterbackups backuplocations \
            clusterbackuplocations backuprepositories; do
     kubectl get "$r.crystalbackup.io" --all-namespaces --no-headers 2>/dev/null
   done
   ```
3. **Only then** remove the operator. With `prune: false` on its `Kustomization`, deleting the
   files leaves the release running, so do it explicitly:

   ```bash
   flux delete kustomization crystal-backup-operator
   flux delete helmrelease crystal-backup -n flux-system   # runs helm uninstall
   ```

   Keep the `crystal-backup-system` namespace unless you also mean to destroy the KEK and the
   wrapped DEKs it holds. If you followed [Protecting the namespace](#protecting-the-namespace),
   `helm uninstall` leaves it alone, which is the point of that section.
4. **The CRDs, only if you mean it.** Helm does not delete them and neither does `CreateReplace`;
   this deletes every remaining object of those kinds:

   ```bash
   kubectl get crd -o name | grep crystalbackup.io | xargs -r kubectl delete --timeout=5m
   ```

### Already stuck in `Terminating`?

**Reinstall the operator. That is the fix.** A CRD stuck in `Terminating` keeps serving its
instances, so an operator brought back at the same version picks up the pending deletions, runs
the teardown it owed and clears the finalizers. Re-create the `HelmRelease` at the version you
removed, or install the chart directly, then restart the sequence above in order. The manual
finalizer strip is a last resort and it leaks the mover Jobs and `Retain`-parked
`VolumeSnapshotContent` objects the teardown would have collected — the full recovery, including
what to sweep afterwards, is in [Uninstall](/CrystalBackup/docs/start/install/#uninstall).

## Verify

```bash
# Flux's own view.
flux get helmreleases -n flux-system
flux get kustomizations -n flux-system

# The operator is up, on the digest the chart pinned.
kubectl -n crystal-backup-system get deploy crystal-backup \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'

# Twelve CRDs are registered — and, after an upgrade, at the new schema.
kubectl get crd -o name | grep -c crystalbackup.io

# The admission policies are bound.
kubectl get validatingadmissionpolicybinding | grep crystalbackup

# The location reached Ready — a Ready HelmRelease does not tell you this.
kubectl get clusterbackuplocations
```

Then let one scheduled run complete. An install you did not verify with a real backup is an
install you have not verified.

## Next

- [Install with Argo CD](/CrystalBackup/docs/start/install-argocd/) — the same operator, the
  other controller, and a genuinely different CRD story.
- [Quickstart](/CrystalBackup/docs/start/quickstart/) — a first backup and restore by hand.
- [The cluster plane](/CrystalBackup/docs/guides/cluster-plane/) — schedules, namespace
  selection, cluster-scoped capture.
