---
title: Disaster recovery
description: ClusterRestore — restoring a namespace by repository coordinate, restoring cluster-scoped resources, and recovering onto a cluster that has nothing left.
sidebar:
  order: 4
---

A `ClusterRestore` addresses a **repository coordinate** — a location, an origin namespace,
and a run — rather than an object in the cluster. It therefore needs nothing to have
survived: not the namespace, not the schedule, not a `Backup`. That is the property the
whole DR story rests on.

This is an administrator operation. It needs the `crystal-backup-admin` role.

## Restoring a namespace elsewhere

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterRestore
metadata:
  name: recover-team-x
spec:
  source:
    locationRef:
      name: dr-primary
    namespace: team-x                        # as it was named at backup time
    backup: dr-daily-20260730-020000         # or: time: latest
  target:
    namespace: team-x-restored
    createNamespace: true
    storageClassMapping:
      fast-rbd: standard
  mode: Recreate
  confirmation: team-x-restored              # the TARGET namespace
```

`source`, `mode` and `target.namespace` are immutable after creation. `confirmation`, the
selection lists, `createNamespace` and `storageClassMapping` stay mutable.

`storageClassMapping` rewrites `storageClassName` on **PVCs** as they are restored. It is
how you recover a Ceph-backed namespace onto a cluster that has no Ceph. It does **not**
touch cluster-scoped objects: a restored `PersistentVolume` keeps the class it was captured
with, because a PV represents a volume that already exists and remapping its class would
not re-provision anything.

Watch it:

```bash
kubectl get clusterrestore recover-team-x -w
```

```
NAME             PHASE       TARGET            AGE
recover-team-x   Running     team-x-restored   12s
recover-team-x   Completed   team-x-restored   3m41s
```

## How PVCs come back

Every data snapshot carries the PVC's shape as restic tags, recorded at backup time:
`pvcsize`, `pvcclass` and `pvcmodes`. A `ClusterRestore` reads them and recreates the PVC
with its original capacity, storage class (after `storageClassMapping`) and access modes —
with no surviving object in the cluster describing any of it.

For snapshots taken before 0.2, which have no such tags, the fallback is: the data size
rounded up to the next GiB with 20% headroom (minimum 1Gi), the cluster's **default**
storage class, and `ReadWriteOnce`. Pre-create the PVC yourself to override any of that.

## Restoring cluster-scoped resources

Capture is broad and automatic; restore is **opt-in and narrow**. The `clusterResources`
field has three meaningful states:

```yaml
# 1. Omitted — nothing cluster-scoped is restored. The safe default.

# 2. Present with an empty include — everything in the snapshot. The snapshot
#    is already the curated capture set, so the field's mere presence is the
#    explicit opt-in.
clusterResources:
  include: []

# 3. Present with an include — only what matches.
clusterResources:
  include:
    - "storage.k8s.io/StorageClass"
    - "apiextensions.k8s.io/CustomResourceDefinition"
  exclude:
    - "storage.k8s.io/StorageClass/legacy-*"
```

`exclude` is applied last, always.

:::caution[Cluster RBAC and CRDs are privileged]
Restoring `ClusterRoleBinding`s recreates cluster-wide grants exactly as they were —
subjects are preserved verbatim, because that is the point of a DR. Restoring CRDs can
collide with operators already installed. Neither is ever implicit: it needs the opt-in
above, the confirmation field, and the admin role. Use `dryRun` first.
:::

On a cluster where ArgoCD or Flux owns cluster-scoped objects, capture them by all means —
but restoring them will fight the GitOps controller. Exclude them at restore time.

There is **no cluster-scoped restore on the namespaced `Restore` path**. A tenant neither
captures nor restores anything outside their namespace.

## Apply order

The order is fixed, and it is what makes a cold restore work:

1. `CustomResourceDefinition`s
2. other cluster-scoped objects — StorageClasses, PriorityClasses, IngressClasses,
   ClusterRoles and bindings, PersistentVolumes
3. namespaces
4. namespaced objects

The cluster-scoped restore Job runs **to completion** before the volumes are driven, so
StorageClasses and CRDs exist before anything tries to bind against them.

## The bare-cluster runbook

Everything is gone: the cluster, the etcd, the custom resources. What you have is the
bucket and the age KEK you escrowed outside the cluster.

The wrapped platform key is escrowed **in the bucket itself**, at
`<prefix>/<clusterID>.crystal-meta/wrapped-dek.age` — a sibling of the repository prefix,
invisible to restic. It is ciphertext under your KEK and useless to anyone holding only the
bucket.

**1 — Install the operator** on the new cluster. See
[Install with Helm](/CrystalBackup/docs/start/install/).

**2 — Restore the two Secrets** from your out-of-band escrow:

```bash
kubectl -n crystal-backup-system create secret generic cluster-kek \
  --from-file=identity=cluster-kek.txt

kubectl -n crystal-backup-system create secret generic dr-s3 \
  --from-literal=AWS_ACCESS_KEY_ID=... \
  --from-literal=AWS_SECRET_ACCESS_KEY=...
```

**3 — Create the location**, pointing at the existing bucket with the **same** `clusterID`
and `prefix`. They compose the repository path; a typo here points you at an empty
repository rather than at an error.

```yaml
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
    credentialsSecretRef:
      name: dr-s3
  encryption:
    clusterKEKSecretRef:
      name: cluster-kek
```

The operator recovers the wrapped key from the bucket escrow — condition `DEKEscrowed`
with reason `Recovered` — and discovery inventories the repository.

```bash
kubectl get clusterbackuplocation dr-primary \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\n"}{end}'
kubectl get backuprepository dr-primary \
  -o jsonpath='{.status.snapshotCount}{" snapshots, "}{.status.namespacesPresent}{" namespaces\n"}'
```

**4 — Look at what is there.** Discovery projects `Backup` objects into namespaces that
**exist**. On a bare cluster that is none of them — which is fine, because a
`ClusterRestore` reads the repository directly and does not need the projection.

To enumerate what the repository holds before any namespace exists, ask restic:

```bash
export RESTIC_PASSWORD=$(kubectl -n crystal-backup-system get secret crystal-dek-dr-primary \
  -o jsonpath='{.data.dek}' | base64 -d | age -d -i cluster-kek.txt)
restic -r s3:https://s3.example.com/crystal-backups/dr/prod-eu-1 snapshots --tag crystalbackup
```

**5 — Restore the cluster-scoped resources first**, from any run:

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
  target:
    namespace: team-x
    createNamespace: true
  mode: Overwrite
  resources: []
  volumes: []
  clusterResources:
    include:
      - "storage.k8s.io/StorageClass"
      - "snapshot.storage.k8s.io/VolumeSnapshotClass"
      - "apiextensions.k8s.io/CustomResourceDefinition"
  confirmation: team-x
```

Note `resources: []` and `volumes: []` — present-but-empty, meaning *nothing of that kind*.
This restore touches only the cluster-scoped set.

Run it with `dryRun: true` first. On a rebuilt cluster this step can recreate CRDs and
cluster RBAC, and seeing the plan is the difference between a reviewed DR and a hopeful
one.

**6 — Restore each namespace:**

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
  mode: Overwrite
  confirmation: team-x
```

With `resources` and `volumes` both omitted, this restores everything the run captured for
that namespace.

**7 — Verify.** Not "the phase says `Completed`" — actually verify. Check the workloads are
running, and that a file whose contents you know has the contents you know.

:::note[A known gap]
A `ClusterRestore` today restores the cluster-scoped objects and the volume data. Bringing
back the namespace's **own** workload manifests through that path reuses the namespaced
`resources[]` engine and is a follow-up. Until it lands, redeploy workloads through your
usual delivery mechanism on top of the restored volumes.
:::

## What is not covered

**etcd and the control plane.** Crystal Backup recovers application resources and data. The
cluster itself is a separate problem with a separate answer, and pretending otherwise would
be exactly the kind of claim this project tries not to make.

## See also

- [Restoring](/CrystalBackup/docs/guides/restore/) — modes, selection, the confirmation gate
- [The DR runbook](/CrystalBackup/docs/operations/dr-runbook/) — the checklist form
- [Diagnosis](/CrystalBackup/docs/operations/troubleshooting/)
