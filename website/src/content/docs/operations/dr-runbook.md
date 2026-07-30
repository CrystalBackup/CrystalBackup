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

**2 — Install the operator.**

```bash
helm install crystal-backup \
  oci://ghcr.io/crystalbackup/charts/crystal-backup \
  --version 0.5.1 \
  --namespace crystal-backup-system --create-namespace

kubectl -n crystal-backup-system rollout status deploy/crystal-backup
```

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

## Recovering into a different cluster

Same procedure, with two changes:

- Use the **original** `clusterID` in the location. It is a path segment, not a statement
  about where you are running. Getting creative here points you at an empty repository.
- Expect `storageClassMapping` to be necessary.

## What this runbook does not cover

**etcd and the control plane.** Crystal Backup recovers application resources and data. The
cluster's own state is a separate problem, and you need a separate answer for it — which
should be written down next to this page.
