---
title: DR runbook
description: The checklist for recovering from total cluster loss, and the drill you should be running before you need it.
sidebar:
  order: 1
---

The narrative version, with explanations, is
[Disaster recovery](/CrystalBackup/docs/guides/disaster-recovery/). This is the checklist.

## Before the incident

The recovery input is **the bucket plus the age KEK you escrowed outside the cluster**.
Everything else is reconstructible. So:

- [ ] The KEK is escrowed **outside** this cluster, and someone who is not you can reach it.
- [ ] You have tested the escrow. Retrieving it for the first time during an incident is
      how you find out it was rotated.
- [ ] You have recorded, somewhere that survives the cluster: the bucket, the prefix, the
      `clusterID`, the S3 endpoint, and where the credentials come from. A typo in
      `clusterID` at recovery time points you at an empty repository rather than at an
      error.
- [ ] You have run a restore drill in the last quarter, and it worked.

That last one is not decoration. `restic check` verifies the repository is readable; it
does not verify a restore produces a working application. Nothing but a drill does.

## The drill

Run this quarterly, into a scratch namespace, on the live cluster. It costs an hour and it
is the only thing that tells you the DR works.

```bash
kubectl create namespace dr-drill
```

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterRestore
metadata:
  name: drill-20260730          # date it, so successive drills do not collide
spec:
  source:
    locationRef: { name: dr-primary }
    namespace: team-x
    time: latest
  target:
    namespace: dr-drill
    createNamespace: true
  mode: Overwrite
  confirmation: dr-drill
```

Then check the data is the data — not that the phase says `Completed`. Time it, and write
the number down: that is your measured RTO for one namespace, and it is the only honest
input to an RTO commitment.

```bash
kubectl delete namespace dr-drill
```

## The incident: total cluster loss

**1 — Rebuild a cluster.** Kubernetes ≥ 1.30, a CSI driver with snapshot support, and the
external-snapshotter CRDs.

**2 — Create and label the namespace, then install the operator.**

Not `--create-namespace`. Helm creates the namespace *after* rendering, so it carries no Pod
Security labels — and `crystal-backup-system` must enforce `baseline`, because data movers run as
uid 0 with `DAC_OVERRIDE` to preserve file ownership on restore, which `restricted` denies. An
unlabelled namespace installs cleanly and then refuses the first mover at admission. **During a
disaster recovery that is the worst possible moment to find out**, which is why these two commands
come first here rather than being a footnote.

```bash
kubectl create namespace crystal-backup-system

kubectl label namespace crystal-backup-system \
  pod-security.kubernetes.io/enforce=baseline \
  pod-security.kubernetes.io/enforce-version=latest \
  pod-security.kubernetes.io/audit=restricted \
  pod-security.kubernetes.io/warn=restricted --overwrite

helm install crystal-backup \
  oci://ghcr.io/crystalbackup/charts/crystal-backup \
  --version 0.6.5 \
  --namespace crystal-backup-system

kubectl -n crystal-backup-system rollout status deploy/crystal-backup
```

The operator checks this itself on startup and emits a `PodSecurityPostureWrong` Warning Event on
its own namespace if the posture is wrong, so a mistake here is visible in
`kubectl -n crystal-backup-system get events` before any backup is attempted.

**3 — Restore the two Secrets** from escrow.

```bash
kubectl -n crystal-backup-system create secret generic cluster-kek \
  --from-file=identity=cluster-kek.txt

kubectl -n crystal-backup-system create secret generic dr-s3 \
  --from-literal=AWS_ACCESS_KEY_ID=... \
  --from-literal=AWS_SECRET_ACCESS_KEY=...
```

**4 — Create the location**, with the **same** `clusterID` and `prefix` as before.

```bash
kubectl apply -f - <<'EOF'
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupLocation
metadata:
  name: dr-primary
spec:
  default: true
  clusterID: prod-eu-1
  s3:
    endpoint: https://s3.example.com
    bucket: crystal-backups
    prefix: dr
    forcePathStyle: true
    credentialsSecretRef: { name: dr-s3 }
  encryption:
    clusterKEKSecretRef: { name: cluster-kek }
EOF
```

**5 — Confirm the key was recovered from the bucket escrow.**

```bash
kubectl get clusterbackuplocation dr-primary \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\n"}{end}'
```

Look for `DEKEscrowed` with reason `Recovered`, and the location reaching `Ready`.

**6 — Confirm the repository is there.**

```bash
kubectl get backuprepository dr-primary \
  -o jsonpath='{.status.initialized}{"\t"}{.status.snapshotCount}{"\t"}{.status.namespacesPresent}{"\n"}'
```

A snapshot count of zero here means you are pointed at the wrong path. Stop and check
`clusterID` and `prefix` before doing anything else.

**7 — Enumerate what you can recover.** No namespaces exist yet, so discovery has nothing
to project into. Ask restic directly:

```bash
kubectl -n crystal-backup-system get secret cluster-kek \
  -o jsonpath='{.data.identity}' | base64 -d > /tmp/kek.txt
export RESTIC_PASSWORD=$(kubectl -n crystal-backup-system get secret crystal-dek-dr-primary \
  -o jsonpath='{.data.dek}' | base64 -d | age -d -i /tmp/kek.txt)
export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...

restic -r s3:https://s3.example.com/crystal-backups/dr/prod-eu-1 \
  snapshots --tag crystalbackup --json | jq -r '.[].tags[]' | grep '^namespace=' | sort -u
```

**8 — Restore the cluster-scoped resources.** Dry-run first.

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterRestore
metadata:
  name: dr-cluster-scoped
spec:
  source:
    locationRef: { name: dr-primary }
    namespace: team-x
    time: latest
  target: { namespace: team-x, createNamespace: true }
  mode: Overwrite
  resources: []
  volumes: []
  clusterResources:
    include:
      - "storage.k8s.io/StorageClass"
      - "snapshot.storage.k8s.io/VolumeSnapshotClass"
      - "apiextensions.k8s.io/CustomResourceDefinition"
  dryRun: true
  confirmation: team-x
```

```bash
kubectl get clusterrestore dr-cluster-scoped \
  -o jsonpath='{range .status.resources.entries[*]}{.outcome}{"\t"}{.kind}{"/"}{.name}{"\n"}{end}'
```

Read the plan. Then delete it, drop `dryRun`, and apply for real.

Exclude anything a GitOps controller will own once you reconnect it — restoring those will
fight the controller.

**9 — Restore each namespace, in dependency order.** Storage-layer namespaces first, then
platform services, then applications.

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterRestore
metadata:
  name: dr-team-x
spec:
  source:
    locationRef: { name: dr-primary }
    namespace: team-x
    time: latest
  target:
    namespace: team-x
    createNamespace: true
    storageClassMapping:
      fast-rbd: standard          # if the new cluster's classes differ
  mode: Overwrite
  confirmation: team-x
```

Run several in parallel; they are independent objects. The cluster-wide mover cap bounds
the real concurrency anyway.

**10 — Redeploy workloads.** A `ClusterRestore` today restores cluster-scoped objects and
volume data. Bring the workload manifests back through your usual delivery mechanism — Helm,
Argo, Flux — on top of the restored volumes.

**11 — Verify each namespace.** Workloads running, PVCs bound, and a file whose contents
you know having the contents you know.

**12 — Re-establish protection.** Recreate the `ClusterBackupSchedule` and let one run
complete before you call the incident closed. A cluster you have recovered but are not
backing up is not recovered.

## Partial loss: one namespace

Much shorter, because the cluster and the operator are fine.

```bash
kubectl -n team-x get backups
```

If the namespace still exists, a namespaced
[`Restore`](/CrystalBackup/docs/guides/restore/) is enough, and the namespace's own owner
can do it.

If the namespace is gone, use `ClusterRestore` with `createNamespace: true` — it addresses
the repository, not the cluster, so nothing needs to have survived.

## Partial loss: the operator namespace

The namespace is gone, and `ClusterBackupLocation`, `ClusterBackupSchedule` and
`BackupRepository` are all still there — they are cluster-scoped, so deleting a namespace does
not take them with it. What you lost is the operator, the cluster KEK and the wrapped DEKs.

**This case is more dangerous than total cluster loss, and the reason is ordering.** On total
loss you create the namespace, restore both Secrets, and only then create the location, so the
operator never sees a location it cannot resolve credentials for. Here the location already
exists, which means the operator reconciles it the instant it starts — with whatever you have
managed to put back by that moment.

In `0.6.3` and earlier that ordering decided your outcome. If the cluster KEK landed before the
S3 credentials, the escrow pass could not read the credentials, could not therefore ask the
bucket whether a recoverable wrapped DEK existed, and did not block the repository. The
repository was provisioned, a **fresh** DEK was minted over the escrowed one, and the location
reported `Ready` while every mover failed `wrong password or no key found` against a repository
full of snapshots. `0.6.4` closes that: any escrow state the operator cannot positively prove
safe now blocks provisioning and reports `Ready=False` with reason `DEKEscrowUnresolved`, and
condition `DEKEscrowed` says which case it is.

Do not rely on the version. **Create and label the namespace, restore both Secrets, and only
then let the operator start.** In this order:

```bash
kubectl create namespace crystal-backup-system

kubectl label namespace crystal-backup-system \
  pod-security.kubernetes.io/enforce=baseline \
  pod-security.kubernetes.io/enforce-version=latest \
  pod-security.kubernetes.io/audit=restricted \
  pod-security.kubernetes.io/warn=restricted --overwrite

# BOTH Secrets, before the operator exists.
kubectl -n crystal-backup-system create secret generic cluster-kek \
  --from-file=identity=cluster-kek.txt

kubectl -n crystal-backup-system create secret generic dr-s3 \
  --from-literal=AWS_ACCESS_KEY_ID=... \
  --from-literal=AWS_SECRET_ACCESS_KEY=...

# Only now.
helm install crystal-backup \
  oci://ghcr.io/crystalbackup/charts/crystal-backup \
  --version 0.6.5 \
  --namespace crystal-backup-system
```

If a GitOps controller is going to reinstall the operator for you, and you cannot control when,
suspend it until the two Secrets are in place. An Argo CD Application or a Flux `HelmRelease`
that resumes on its own schedule is exactly the actor that will start the operator two minutes
before you finish restoring the credentials.

Then check what the escrow concluded, before anything else:

```bash
kubectl get clusterbackuplocation dr-primary \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\n"}{end}'
```

`DEKEscrowed` with reason `Recovered` is the good outcome: the wrapped DEK came back from the
bucket. `Escrowed` is also fine — it means an in-cluster DEK was already there and the bucket
copy matches it. Anything else, read the message before you touch anything: `EscrowConflict`,
`EscrowUnreadableUnderKEK` and `ClusterDEKUnreadableUnderKEK` are three different emergencies
and only one of them is about your KEK.

**Do not delete the `ClusterBackupLocation` to start clean.** It carries
`crystalbackup.io/location`, and deleting it while no operator is running leaves it in
`Terminating` with nothing able to release the finalizer. If you have already done it, reinstall
the operator and it will finish the deletion —
[Removing Crystal Backup](/CrystalBackup/docs/operations/uninstall/) has the rest.

## Adopting the escrowed DEK by hand

The operator recovers the wrapped DEK from the bucket on its own, and that automatic path is
the one you want. This is for when it cannot run: the bucket is reachable from your laptop but
not from the cluster, the credentials are not the ones the location names, or you want to
establish that the KEK you found in escrow is the right one *before* letting a reconcile touch
anything.

The escrow object is a sibling of the repository prefix, never inside it:

```
<prefix>/<clusterID>.crystal-meta/wrapped-dek.age
```

With an empty prefix that degenerates to `<clusterID>.crystal-meta/wrapped-dek.age`. The key is
part of the DR contract and does not change between versions.

**1 — Scale the operator to 0.** Not optional. While it runs, the repository path can mint a
DEK before the escrow pass gets a chance to adopt one, and that is a race you lose.

```bash
kubectl -n crystal-backup-system scale deploy/crystal-backup --replicas=0
```

**2 — Fetch the object** with any S3 client.

```bash
aws s3 cp s3://crystal-backups/dr/prod-eu-1.crystal-meta/wrapped-dek.age . \
  --endpoint-url https://s3.example.com
```

**3 — Prove your KEK opens it.** The file is age ciphertext and is worth nothing without the
cluster KEK, which is why escrowing it in the bucket is safe. Testing a candidate KEK against
it is one command, and it is the cheapest test in this whole runbook:

```bash
age -d -i cluster-kek.txt wrapped-dek.age > /dev/null
```

Exit 0 means that KEK is the one that wrapped this repository's key. A failure means it is not,
and no amount of adoption will help — go and find the right KEK.

**4 — Create the Secret.** One Secret per location, named from the location, with the wrapped
bytes under the data key `dek`:

```bash
kubectl -n crystal-backup-system create secret generic crystal-dek-dr-primary \
  --from-file=dek=wrapped-dek.age

# Cosmetic, but it makes the Secret discoverable alongside the operator's own.
kubectl -n crystal-backup-system label secret crystal-dek-dr-primary \
  app.kubernetes.io/managed-by=crystal-backup app.kubernetes.io/name=crystal-backup
```

**5 — Scale the operator back up** and confirm `DEKEscrowed` goes True.

```bash
kubectl -n crystal-backup-system scale deploy/crystal-backup --replicas=1
```

Adopting the wrong file is not a way to lose data: the operator validates that the bytes unwrap
under the current KEK before it accepts them, so a corrupt or foreign blob is refused rather
than silently taken, and a DEK Secret that already exists with different bytes is never
overwritten — one DEK for the life of a location, and the existing one wins.

## Recovering into a different cluster

Same procedure, with two changes:

- Use the **original** `clusterID` in the location. It is a path segment, not a statement
  about where you are running. Getting creative here points you at an empty repository.
- Expect `storageClassMapping` to be necessary.

## What this runbook does not cover

**etcd and the control plane.** Crystal Backup recovers application resources and data. The
cluster's own state is a separate problem, and you need a separate answer for it — which
should be written down next to this page.
