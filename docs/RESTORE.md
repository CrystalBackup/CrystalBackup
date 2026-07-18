# Restore guide (M2)

How to get data back with Crystal Backup: self-service restores for namespace users, and
disaster-recovery restores — up to and including a namespace (or a whole cluster's worth of
namespaces) that no longer exists. The authoritative contract is
[spec/02-api.md](../spec/02-api.md); the execution design is
[spec/adr/0016](../spec/adr/0016-restore-execution-and-target-exposure.md).

Scope note: through M2 a restore covers **PVC data**. Kubernetes manifests (Deployments,
Services, Secrets, …) are captured and restored from **M3** — until then, `spec.resources`
is accepted but restores nothing, and a `ClusterRestore` reconstitutes namespaces + volumes,
not workloads.

## Self-service: `Restore` (namespace users)

A `Restore` names a `Backup` **in its own namespace** — that is the whole security model
(R14): there is no location field and no target-namespace field, so a restore can only ever
bring your own namespace's history back into your own namespace. Cluster-DR backups
(`kubectl get backups` shows them with `crystalbackup.io/origin: cluster`) are restorable
the same way: the operator mediates against the shared repository with a server-side
`namespace=<your namespace>` filter that no field of the CR can influence.

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: Restore
metadata:
  name: recover-uploads
  namespace: c-team-x
spec:
  source:
    backup: dr-daily-20260711-020000   # a Backup in this namespace; or time: latest
  mode: Overwrite                      # or Recreate
  volumes:                             # omit for every volume
    - names: ["uploads"]
      include: ["images/2026/**"]      # partial restore (R7)
  confirmation: c-team-x               # must equal YOUR namespace (R23)
```

### Modes

| Mode | Effect on the PVC's files |
|---|---|
| `Overwrite` | Files in the backup overwrite/return; files **absent from the backup are kept**. |
| `Recreate` | Exact match: extras are **deleted** (`restic restore --delete`). A missing PVC is recreated. |

### Confirmation (R23)

Every `Recreate`/`Overwrite` requires `spec.confirmation` to equal the target namespace.
Admission rejects a **wrong** value outright; an **empty** one is admitted and the restore
parks in phase `AwaitingConfirmation` until you edit the field — a deliberate two-step for
the destructive path.

### Selection

`spec.volumes` is a NetworkPolicy-style list: a PVC is restored iff **any** item matches
(an item without `names` matches every PVC), and the **first** matching item's
`include`/`exclude`/`targetPath` apply. Omit the list entirely to restore every volume;
an explicitly empty list restores none.

### What to expect

- The restore runs as mover Jobs in `crystal-backup-system` — never in your namespace, and
  your namespace never receives credentials or keys (only restored PVCs).
- Restoring into a **live, actively-written** volume is discouraged (as with any in-place
  restore tool): quiesce or scale down first for consistent results. An RWO volume attached
  to one node is handled — the mover is pinned to that node.
- `volumeMode: Block` PVCs are not restorable (restic restores files); the volume fails
  with reason `RestoreBlockUnsupported`.
- Terminal state: `Completed`, `PartiallyFailed` or `Failed`, with `restoredVolumes` /
  `restoredBytes` counters and per-volume detail in Events.

## Disaster recovery: `ClusterRestore` (admins)

A `ClusterRestore` addresses a **repository coordinate** — location + origin namespace +
run (or `time: latest` / RFC3339) — so it needs no surviving object in the cluster: not the
namespace, not a `Backup`, nothing.

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterRestore
metadata:
  name: recover-team-x
spec:
  source:
    locationRef: { name: dr-primary }
    namespace: c-team-x                 # as it was named at backup time
    backup: dr-daily-20260711-020000    # or time: latest
  target:
    namespace: c-team-x-restored
    createNamespace: true
    storageClassMapping: { fast-rbd: standard }
  mode: Recreate
  confirmation: c-team-x-restored       # the TARGET namespace (R23)
```

Recreated PVCs get their **original capacity, storage class and access modes** from the
snapshot's `pvcsize`/`pvcclass`/`pvcmodes` tags (recorded since 0.2). For pre-0.2 snapshots
the fallback is the data size rounded up to the next GiB (+20% headroom, min 1Gi), the
cluster's default class, and RWO — pre-create the PVC yourself to override anything.
Restored PVCs carry the `crystalbackup.io/restored-from: <run>` annotation.

## Bare-cluster DR runbook (nothing survived)

The repository is the source of truth (R26); since 0.2 the wrapped platform DEK is also
**escrowed in the bucket** (`<prefix>/<clusterID>.crystal-meta/wrapped-dek.age` — ciphertext
under your KEK, useless alone). With the KEK escrowed out-of-band by your organization, a
cluster that burned to the ground recovers like this:

1. Install the operator (Helm chart) on the new cluster.
2. Re-create the **cluster KEK Secret** (`cluster-kek`) in `crystal-backup-system` from your
   out-of-band escrow, and the S3 credentials Secret.
3. Create the `ClusterBackupLocation` pointing at the existing bucket (same
   `clusterID`/`prefix`). The operator **recovers the wrapped DEK from the bucket escrow**
   (condition `DEKEscrowed: Recovered`), and discovery inventories the repository —
   `kubectl get backups -A` fills up with what is restorable.
4. For each namespace to bring back: a `ClusterRestore` with `createNamespace: true`
   (step 3's inventory is not even required — a `ClusterRestore` reads the repo directly).
5. From M3, the same restores also reapply the namespaces' manifests; through M2, redeploy
   workloads via your usual delivery (GitOps, Helm) on top of the restored volumes.

Verify with upstream restic at any point (reversibility, R8): the repository opens with the
unwrapped DEK and standard `restic snapshots` / `restore` — no Crystal Backup tooling
required.
