---
title: Quickstart
description: A first cluster-plane backup and a first restore, as exact commands with the output each one should produce.
---

<!-- UNVERIFIED: à exécuter au lot I -->

:::caution[Not yet executed end to end]
This page has been written against the shipped API but has **not** yet been run verbatim
on real infrastructure. It will be, before publication. Until then, treat the expected
outputs as intent rather than as observed fact.
:::

This takes a cluster-plane backup of one namespace and restores a file from it. It assumes
the operator is installed, the `cluster-kek` Secret exists and you are cluster-admin.

Throughout: replace `s3.example.com`, `crystal-backups` and `prod-eu-1` with your own.

## 0. A namespace with something in it

```bash
kubectl create namespace demo
kubectl label namespace demo crystalbackup.io/protect=true
```

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
  namespace: demo
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: writer
  namespace: demo
spec:
  containers:
    - name: sh
      image: busybox:1.36
      command: ["sh", "-c", "echo hello-from-the-backup > /data/canary.txt && sleep infinity"]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: data
EOF
```

Expected:

```
namespace/demo created
namespace/demo labeled
persistentvolumeclaim/data created
pod/writer created
```

Wait for the pod, then confirm the canary is there:

```bash
kubectl -n demo wait --for=condition=Ready pod/writer --timeout=120s
kubectl -n demo exec writer -- cat /data/canary.txt
```

Expected:

```
pod/writer condition met
hello-from-the-backup
```

## 1. Object-storage credentials

```bash
kubectl -n crystal-backup-system create secret generic dr-s3 \
  --from-literal=AWS_ACCESS_KEY_ID="$AWS_ACCESS_KEY_ID" \
  --from-literal=AWS_SECRET_ACCESS_KEY="$AWS_SECRET_ACCESS_KEY"
```

Expected:

```
secret/dr-s3 created
```

## 2. The cluster backup location

`clusterID`, `s3.endpoint`, `s3.bucket`, `s3.prefix` and `mode` are **immutable after
creation** — together they compose the repository path, so editing one would silently
re-point the location at a different repository. Get them right now; to change one later,
create a new location.

```bash
cat <<'EOF' | kubectl apply -f -
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
EOF
```

Expected:

```
clusterbackuplocation.crystalbackup.io/dr-primary created
```

Watch it initialise the repository:

```bash
kubectl get clusterbackuplocation dr-primary -w
```

Expected, within a minute or two (a repository-init Job has to pull the mover image and
run `restic init`):

```
NAME         MODE       DEFAULT   PROTECTED   PHASE   AGE
dr-primary   Standard   true      0                   5s
dr-primary   Standard   true      0           Ready   47s
```

If it does not reach `Ready`, the conditions say why:

```bash
kubectl get clusterbackuplocation dr-primary -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\t"}{.message}{"\n"}{end}'
```

The repository object carries the detail:

```bash
kubectl get backuprepository
```

Expected:

```
NAME         SCOPE     INITIALIZED   URL                                                      SNAPSHOTS   AGE
dr-primary   Cluster   true          s3:https://s3.example.com/crystal-backups/dr/prod-eu-1   0           1m
```

A cluster-plane repository takes the location's own name. A namespace-plane one is named
`<namespace>--<location>`, because `BackupRepository` is cluster-scoped and two namespaces
may use the same location name.

## 3. A schedule, and one run now

The schedule is the normal path. Create it:

```bash
cat <<'EOF' | kubectl apply -f -
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
EOF
```

Expected:

```
clusterbackupschedule.crystalbackup.io/dr-daily created
```

Rather than wait until 02:00, create a run directly. A `ClusterBackup` carries the same
run configuration inline:

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackup
metadata:
  name: quickstart-run
spec:
  locationRef:
    name: dr-primary
  namespaces:
    matchNames: ["demo"]
  includeManifests: true
  clusterResources:
    enabled: false
EOF
```

Expected:

```
clusterbackup.crystalbackup.io/quickstart-run created
```

Watch the run:

```bash
kubectl get clusterbackup quickstart-run -w
```

Expected:

```
NAME             PHASE       MATCHED   SUCCEEDED   FAILED   AGE
quickstart-run   Pending     0         0           0        2s
quickstart-run   Running     1         0           0        6s
quickstart-run   Completed   1         1           0        1m12s
```

The child `Backup` landed in the namespace:

```bash
kubectl -n demo get backups
```

Expected:

```
NAME             PHASE       LOCATION     BACKUP-TIME   AGE
quickstart-run   Completed   dr-primary   1m            1m
```

Per-volume detail:

```bash
kubectl -n demo get backup quickstart-run -o jsonpath='{range .status.volumes[*]}{.pvc}{"\t"}{.phase}{"\t"}{.sizeBytes}{"\t"}{.reason}{"\n"}{end}'
```

Expected:

```
data	Completed	4096	
```

A `Skipped` phase with `CSISnapshotUnsupported` here means the PVC's CSI driver has no
snapshot support — see [Requirements](/CrystalBackup/docs/start/requirements/).

## 4. Break something

```bash
kubectl -n demo exec writer -- sh -c 'rm /data/canary.txt && ls /data'
```

Expected: no output (the directory is empty).

## 5. Restore

A namespaced `Restore` names a `Backup` in its own namespace. There is no field for the
location or for a target namespace — that absence is the tenant confinement.

Rehearse first. `dryRun` runs the whole pipeline with server-side dry-run applies and
persists nothing:

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: crystalbackup.io/v1alpha1
kind: Restore
metadata:
  name: recover-canary
  namespace: demo
spec:
  source:
    backup: quickstart-run
  mode: Overwrite
  volumes:
    - names: ["data"]
  dryRun: true
  confirmation: demo
EOF
```

Expected:

```
restore.crystalbackup.io/recover-canary created
```

Note `confirmation: demo` — it must equal the namespace. Omit it and the restore parks in
`AwaitingConfirmation` until you edit it in; set it to the *wrong* value and admission
rejects the object outright.

Now the real one:

```bash
kubectl -n demo delete restore recover-canary
```

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: crystalbackup.io/v1alpha1
kind: Restore
metadata:
  name: recover-canary
  namespace: demo
spec:
  source:
    backup: quickstart-run
  mode: Overwrite
  volumes:
    - names: ["data"]
  confirmation: demo
EOF
```

```bash
kubectl -n demo get restore recover-canary -w
```

Expected:

```
NAME             PHASE       AGE
recover-canary   Pending     2s
recover-canary   Running     8s
recover-canary   Completed   48s
```

:::note
`Overwrite` restores into a volume that is attached to a running pod. The mover is pinned
to the node holding the attachment, so it works — but restoring under a live writer is
discouraged in general. For anything real, scale the workload down first.
:::

Check the canary is back:

```bash
kubectl -n demo exec writer -- cat /data/canary.txt
```

Expected:

```
hello-from-the-backup
```

## 6. Read it with plain restic

This is the reversibility claim, and it costs one command to verify. Unwrap the platform
key:

```bash
kubectl -n crystal-backup-system get secret cluster-kek -o jsonpath='{.data.identity}' | base64 -d > /tmp/kek.txt
kubectl -n crystal-backup-system get secret -l app.kubernetes.io/managed-by=crystal-backup -o name | grep crystal-dek
```

Expected:

```
secret/crystal-dek-dr-primary
```

```bash
export RESTIC_PASSWORD=$(kubectl -n crystal-backup-system get secret crystal-dek-dr-primary \
  -o jsonpath='{.data.dek}' | base64 -d | age -d -i /tmp/kek.txt)
export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY
restic -r s3:https://s3.example.com/crystal-backups/dr/prod-eu-1 snapshots
```

Expected:

```
ID        Time                 Host        Tags                                                 Paths
------------------------------------------------------------------------------------------------------------
a1b2c3d4  2026-07-30 12:04:11  prod-eu-1   crystalbackup,tenant=demo,namespace=demo,pvc=data,   /data/demo/data
                                           kind=data,run=quickstart-run
------------------------------------------------------------------------------------------------------------
1 snapshots
```

No Crystal Backup component is involved in that command. Delete `/tmp/kek.txt` when you
are done with it.

## Clean up

```bash
kubectl delete namespace demo
kubectl delete clusterbackup quickstart-run
kubectl delete clusterbackupschedule dr-daily
kubectl delete clusterbackuplocation dr-primary
```

Deleting the location removes its `BackupRepository` and **leaves the bucket alone**. To
delete the data, see [The right to erasure](/CrystalBackup/docs/guides/erasure/).

## Next

- [The cluster plane](/CrystalBackup/docs/guides/cluster-plane/) — schedules, namespace
  selection, cluster-scoped capture.
- [The namespace plane](/CrystalBackup/docs/guides/namespace-plane/) — giving tenants their
  own bucket and key.
- [Restoring](/CrystalBackup/docs/guides/restore/) — modes, selection, and what `Recreate`
  really deletes.
