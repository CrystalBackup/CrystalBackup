# Restore guide

How to get data back with Crystal Backup: self-service restores for namespace users, and
disaster-recovery restores — up to and including a namespace (or a whole cluster's worth of
namespaces) that no longer exists. The authoritative contract is
[spec/02-api.md](../spec/02-api.md); the execution design is
[spec/adr/0016](../spec/adr/0016-restore-execution-and-target-exposure.md).

Scope: a restore covers **PVC data and Kubernetes manifests** (Deployments, Services,
Secrets, …). Manifest capture and restore shipped in **0.3.0** — on a `Restore`,
`spec.resources` selects them and they are reapplied alongside the volume data. Manifests are
captured on both planes: a `ClusterBackup` fans out into a child `Backup` per namespace, and
each child captures that namespace's manifests.

One gap is worth stating outright rather than leaving to be discovered: a `ClusterRestore`
restores **cluster-scoped** objects (`spec.clusterResources`) and volume data, but *not* the
source namespace's own workload manifests. `spec.resources` is accepted on a `ClusterRestore`
and currently restores nothing — the manifests are in the repository, but that reapply path is
still a follow-up. On the cluster-DR path, redeploy workloads through your usual delivery
(GitOps, Helm) on top of the restored volumes; see step 5 of the runbook below.

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

### Choosing the source

Exactly one of `spec.source.backup` and `spec.source.time` must be set. `time` is `latest` or
an RFC3339 instant (a zone-less timestamp is read as UTC). When both planes have backups near
that instant, `spec.source.origin` — `cluster` or `namespace` — disambiguates; it is valid
**only** together with `time`, and admission rejects it alongside `backup`. Once a time-based
source resolves it is **pinned**: the restore does not drift to a newer backup between
reconciles. (A `ClusterRestore` has no `origin`: its coordinate already names the location and
the origin namespace explicitly.)

### Modes

One mode governs both halves of a restore:

| Mode | The PVC's files | The manifests |
|---|---|---|
| `Overwrite` (default) | Files in the backup overwrite/return; files **absent from the backup are kept** (`restic restore --overwrite always`). | Server-side apply, create-or-update. Objects absent from the backup are **kept**. |
| `Recreate` | Exact match: extras are **deleted** (`--delete`). A missing PVC is recreated. | Selected objects that exist are **deleted**, then created from the backup. |

Pick `Overwrite` to put back something that was lost; pick `Recreate` when the target must
*be* the backup — after a corruption, or when the stray files are themselves the problem.
`Recreate` deletes, so rehearse it with `dryRun` below.

### Confirmation (R23)

Every `Recreate`/`Overwrite` requires `spec.confirmation` to equal the target namespace.
Admission rejects a **wrong** value outright; an **empty** one is admitted and the restore
parks in phase `AwaitingConfirmation` until you edit the field — a deliberate two-step for
the destructive path.

### Rehearsing: `spec.dryRun`

```yaml
spec:
  dryRun: true
```

Present on **both** `Restore` and `ClusterRestore`. It runs the manifest pipeline for real —
ordering, selection, mode resolution, and on a `ClusterRestore` the storage-class mapping —
with **server-side dry-run** applies, persists nothing, and writes the plan to
`status.resources`. Before a `Recreate` against a live namespace, or a `ClusterRestore` that
can recreate CRDs and cluster RBAC, this is the difference between a reviewed restore and a
hopeful one — and it is the rehearsal for exactly the destructive path the confirmation gate
above exists to slow down.

```bash
kubectl -n c-team-x get restore recover-uploads \
  -o jsonpath='{range .status.resources.entries[*]}{.outcome}{"\t"}{.kind}{"/"}{.name}{"\t"}{.reason}{"\n"}{end}'
```

`entries` records **non-trivial** outcomes only — `Configured` (applied over an existing
object), `Recreated` (deleted then created) and `Failed`, with the server's error in `reason`.
A plain create is not listed; it shows up only in the `restoredResources` counter. Under
`dryRun` these are **planned** actions. The report is capped at 100 entries with 20 changed
field paths each, because a status that exceeds etcd's object-size limit loses the whole report
rather than its tail — `status.resources.truncated` tells you when that happened.

**The volume half has no dry run.** `dryRun` covers manifests only: it tells you which objects
would change, not which files a `Recreate` would delete.

The drill is to dry-run, read the plan, then **delete the object and apply the same restore
without `dryRun`** — rather than patching the field on a restore that has already gone
terminal.

### Selection

Two independent lists: `spec.resources` (manifests) and `spec.volumes` (PVC data). Each is
NetworkPolicy-style — a thing is restored iff **any** item matches — and each defaults
**independently**: omitted means *everything* of that kind, present-but-empty (`[]`) means
*nothing* of that kind, and a populated list means only what its items match. So omitting both
restores the whole namespace, while `resources: []` with `volumes` set is "data only, no
manifests".

```yaml
resources:
  - selector:
      matchLabels: { app: web }
    include: ["apps/Deployment"]
    exclude: ["apps/Deployment/legacy-*"]
  - include: ["apps/StatefulSet/postgres", "Secret/db-creds"]
```

`include`/`exclude` on `resources` are `<group>/<Kind>[/<name>]` globs; on `volumes` they are
**file** globs inside the PVC, which is how a partial restore works (R7). For volumes the
**first** matching item's `include`/`exclude`/`targetPath` apply — an item without `names`
matches every PVC, so put the specific items first. Anything excluded when the manifests were
*captured* cannot be re-included at restore time: it is not in the snapshot.

### What to expect

- The restore runs as mover Jobs in `crystal-backup-system` — never in your namespace, and
  your namespace never receives credentials or keys (only restored PVCs).
- Restoring into a **live, actively-written** volume is discouraged (as with any in-place
  restore tool): quiesce or scale down first for consistent results. An RWO volume attached
  to one node is handled — the mover is pinned to that node.
- `volumeMode: Block` PVCs are not restorable (restic restores files); the volume fails
  with reason `RestoreBlockUnsupported`.
- A many-volume restore is paced: at most **4 mover Jobs per restore** run at once (slots
  free as volumes finish), so a large `ClusterRestore` cannot stampede node attach limits
  or the S3 endpoint.
- `spec.source` and `spec.mode` are immutable once created (`target.namespace` too, on a
  `ClusterRestore`): one restore is one point in time. To change them, delete and recreate.
- A manifest restore reports per-resource failures and **continues**; it does not abort on the
  first one.
- Terminal state: `Completed`, `PartiallyFailed` or `Failed`, with `restoredVolumes` /
  `restoredBytes` / `restoredResources` counters, per-volume detail in Events and per-resource
  detail in `status.resources`.

### What file metadata survives a restore

Restored, and asserted by the `m6` restore-fidelity gate on a real Rook-Ceph volume: file
contents, mode bits **including setuid/setgid/sticky**, numeric uid/gid, `user.*` and
`security.*` extended attributes, POSIX ACLs **including a directory's default ACL**,
modification times at nanosecond precision, symlinks (stored as links, never resolved), hard
links (shared inodes stay shared), sparseness, FIFOs, empty directories, and names containing
non-ASCII characters, spaces or shell metacharacters.

**Not restored**, by design rather than oversight:

| | why |
|---|---|
| `trusted.*` xattrs | writing them requires `CAP_SYS_ADMIN`, which the mover deliberately does not hold |
| `atime` | not preserved by restic, and unmeasurable anyway — reading a file to verify it destroys it |
| `ctime` | no interface exists to set it; it records when the inode was last changed, so a restore legitimately updates it |
| device nodes | whether one can even be created depends on the target volume's mount options, which is a property of your cluster, not of the backup |

If your workload depends on any of these, it depends on something no backup of a POSIX
filesystem can return to you, and that is worth knowing before an incident rather than during
one.

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

Cluster-scoped objects are an explicit, admin-only opt-in: `spec.clusterResources` **omitted**
restores nothing cluster-scoped, and its mere presence is the opt-in. Note the arbitration
differs from the namespaced lists above — a present `clusterResources` with an empty `include`
restores the whole curated capture (the snapshot is already the curation), while `exclude`
always subtracts.

```yaml
  clusterResources:
    include:
      - "storage.k8s.io/StorageClass"
      - "apiextensions.k8s.io/CustomResourceDefinition"
```

Exclude anything a GitOps controller will own once you reconnect it — restoring those means
two writers fighting over the same objects. And dry-run this half first: it is the one that can
recreate CRDs and cluster RBAC.

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
4. Restore the cluster-scoped objects, with `dryRun: true` first — read the plan, then delete
   and apply it for real. Then, for each namespace to bring back: a `ClusterRestore` with
   `createNamespace: true` (step 3's inventory is not even required — a `ClusterRestore` reads
   the repo directly).
5. Redeploy workloads. A `ClusterRestore` brings back cluster-scoped objects and volume data;
   reapplying a namespace's own workload manifests through *that* path is still a follow-up, so
   bring them back via your usual delivery (GitOps, Helm) on top of the restored volumes. The
   manifests themselves are in the repository — once a namespace and its `Backup` objects exist
   again, a namespaced `Restore` does reapply them, which is the route for a partial loss rather
   than a bare-cluster rebuild.
6. Verify each namespace, then recreate the `ClusterBackupSchedule` and let one run complete: a
   cluster you have recovered but are not backing up is not recovered.

Verify with upstream restic at any point (reversibility, R8): the repository opens with the
unwrapped DEK and standard `restic snapshots` / `restore` — no Crystal Backup tooling
required.
