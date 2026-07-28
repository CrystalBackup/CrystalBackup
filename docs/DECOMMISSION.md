# Runbook — decommissioning a repository, and re-encrypting one

Two operations that are not CRDs and never will be, because both are **irreversible in a way no
reconcile loop should own**: one destroys the only key that can read a repository, the other
rewrites where a fleet's backups live. They are runbooks an admin executes deliberately, with the
operator's own machinery doing the work.

- **[Decommission](#1-decommission)** — retire a repository by destroying its key. The bytes stay
  in the bucket and become permanently unreadable. This is the *only* surviving form of
  crypto-shredding ([adr/0004](../spec/adr/0004-encryption-key-management.md)).
- **[Re-encrypt](#2-re-encrypt)** — move a repository's contents under a **new** key, because the
  old one leaked. Since M5 this is not a special procedure: it is an
  [external sync](../spec/adr/0013-external-backup-sync.md) into a fresh location, followed by a
  decommission of the old one.

> **Neither is right-to-erasure.** Erasing one tenant's data is `ClusterErasure` (R21) — a CRD,
> with a typed confirmation and a compliance record. Decommission destroys **everything** in the
> repository at once. If a GDPR request brought you here, you are in the wrong document.

---

## 0. Before either operation

**Know what you are about to make unreadable.** A repository holds every namespace that ever
backed up to it, including ones whose owners have moved on and whose `Backup` objects were deleted
long ago. The objects in your cluster are not the inventory — the repository is:

```bash
kubectl get backuprepository <name> -o jsonpath='{.status.snapshotCount}{"\n"}{.status.sizeBytes}{"\n"}'
```

`status.snapshotCount` is what discovery last saw. If it is stale or empty, run a listing before
trusting it — a decommission based on "the CR said 0 snapshots" has destroyed a repository that
held years of data more than once, in more than one product.

**Check nothing is syncing FROM it.** A `ClusterBackupExternalSync` or `BackupExternalSync` whose
**source** you decommission does not fail loudly: on the next tick it finds a repository it cannot
open and reports `SyncFailed`, while its destination silently stops advancing. Find them first:

```bash
kubectl get clusterbackupexternalsync,backupexternalsync -A \
  -o jsonpath='{range .items[*]}{.kind}{" "}{.metadata.namespace}/{.metadata.name}{" src="}{.spec.sourceLocationRef.name}{" dst="}{.spec.destinationLocationRef.name}{"\n"}{end}'
```

**Confirm the retention window has actually expired.** A decommissioned repository cannot serve a
restore, an audit, or a legal hold. There is no undo, no support path, and no vendor with a copy.

---

## 1. Decommission

### What it does

Destroys the **key**, not the objects. Afterwards the bucket still bills you for every byte and not
one of them can be decrypted — by you, by us, or by anyone who obtains the bucket. That asymmetry is
the point: it is instant and complete regardless of how large the repository is, where an S3 delete
of millions of objects is neither.

### 1.1 Stop everything that writes to it

```bash
# Cluster plane: pause the schedules pointing at this location.
kubectl patch clusterbackupschedule <name> --type=merge -p '{"spec":{"paused":true}}'
# Namespace plane: same, per namespace.
kubectl patch backupschedule <name> -n <ns> --type=merge -p '{"spec":{"paused":true}}'
```

Then wait for in-flight movers to drain — a mover that starts before you destroy the key and
finishes after will report a failure that looks like a storage fault:

```bash
kubectl get jobs -n crystal-backup-system -l app.kubernetes.io/managed-by=crystal-backup
```

### 1.2 Delete the location

Deleting the `ClusterBackupLocation` (or `BackupLocation`) removes its `BackupRepository`. This
does **not** touch the bucket, and on the namespace plane it deliberately does **not** delete a
generated password Secret — see [§1.4](#14-the-key-itself).

```bash
kubectl delete clusterbackuplocation <name>
# or, namespace plane:
kubectl delete backuplocation <name> -n <ns>
```

### 1.3 Record what you are destroying

Before the key goes, write down what it protected. Afterwards nothing can reconstruct it:

- location name, bucket, prefix, cluster ID;
- `status.snapshotCount` and `status.sizeBytes` at decommission time;
- the date, and who authorised it.

This is the only artefact that will exist. Treat it as the compliance record it is.

### 1.4 The key itself

**Cluster plane.** The repository password is a platform DEK, wrapped by the cluster KEK and stored
as a Secret in the operator namespace:

```bash
kubectl -n crystal-backup-system get secret -l app.kubernetes.io/managed-by=crystal-backup | grep dek
kubectl -n crystal-backup-system delete secret <the location's wrapped DEK>
```

Deleting the wrapped DEK is sufficient **only if no copy of the plaintext DEK exists elsewhere** —
in a break-glass escrow, a password manager, or an old backup of the operator namespace. Chase
those down before declaring the repository unreadable; a decommission that leaves one copy behind
has changed nothing except your belief about it.

**Namespace plane.** The password is the tenant's, and where it lives depends on how the location
was written:

- `spec.encryption.repositoryPasswordSecretRef` set → the Secret is the **user's**, created and
  owned by them. The operator never generated it, never mutates it, and will not delete it. Tell
  the owner; deleting it is their act, in their namespace.
- No ref → the operator generated one and stored it in their namespace. It carries **no
  ownerReference**, on purpose: deleting the location must not silently make a repository
  unreadable. Deleting it is a separate, deliberate step:

```bash
kubectl -n <ns> get secret crystal-repo-password-<location>
kubectl -n <ns> delete secret crystal-repo-password-<location>
```

### 1.5 The bucket

Empty it or not, on your own schedule — the data is already unreadable, so this is a billing
decision rather than a security one. On an `Immutable` location it is not a decision at all: Object
Lock refuses the delete until expiry, and the objects stay (and bill) until then. That is expected;
the key is already gone, which is what mattered.

---

## 2. Re-encrypt

### When

A repository password has leaked, or is suspected to have. Removing a key slot is **not** enough,
and this is the single most misunderstood property of restic: `restic key remove` revokes an
*access password*, but every one of them decrypts the same **master key**, which never changes.
Anyone holding the old password AND a copy of the objects can still read everything
([adr/0004](../spec/adr/0004-encryption-key-management.md)). The only real answer is to rewrite the
data under a new master key — which is exactly what `restic copy` does.

### The shape

Since M5 there is no bespoke procedure. Re-encryption is a **sync into a fresh location**, then a
**decommission of the old one**:

```
old location ──(ClusterBackupExternalSync / BackupExternalSync)──▶ new location
     │                                                                   │
     └── decommission (§1) once verified ◀───────────────────────────────┘
```

The copy decrypts with the old key and re-encrypts with the new one, per snapshot. Tags, host and
paths are preserved, so discovery projects the new repository exactly like the old one and every
`Restore` keeps working against it.

### 2.1 Create the destination

A new location, **with its own key** — a new bucket or at minimum a new prefix, never the same
repository path:

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupLocation
metadata:
  name: dr-primary-rekeyed
spec:
  s3: { endpoint: "https://s3.example.com", bucket: "backups-rekeyed", prefix: "dr" }
  encryption:
    clusterKEKSecretRef: { name: crystal-cluster-kek }
```

> **If the new repository will ALSO receive native backups**, initialize it with the source's
> chunker parameters or the two blob sets will not deduplicate against each other and you will pay
> for the data twice:
> `restic -r <new> init --from-repo <old> --copy-chunker-params`.
> The operator's own `init` does not do this — it has no way to know you intend a copy — so it is a
> manual step, taken **before** the first sync.

### 2.2 Sync

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupExternalSync
metadata:
  name: rekey
spec:
  sourceLocationRef: { name: dr-primary }
  destinationLocationRef: { name: dr-primary-rekeyed }
  mode: AppendOnly   # nothing should be forgotten at the destination during a rekey
```

`AppendOnly`, not the `Mirror` default: a rekey copies forward and never removes. Mirror would
also be *correct* here, but it gives the operation a delete path it has no use for, and a rekey is
not the moment to hold one.

The first run moves the whole repository — plan for it. Watch it:

```bash
kubectl get clusterbackupexternalsync rekey -o jsonpath='{.status.phase}{" lag="}{.status.lagSnapshots}{" copied="}{.status.snapshotsCopied}{"\n"}'
```

### 2.3 Verify BEFORE destroying anything

`lagSnapshots: 0` says every source snapshot has a copy. It does not say those copies are readable.
Check the new repository on its own terms, and restore something real from it:

```bash
# structural integrity, under the NEW key
kubectl get backuprepository dr-primary-rekeyed -o jsonpath='{.status.lastCheckSucceeded}{"\n"}'
```

Then run an actual `Restore` against the new location. A rekey verified only by counters is a rekey
verified only by the thing that would also be wrong if the copy were broken.

### 2.4 Cut over, then decommission

Point the schedules at the new location, let a full backup cycle complete against it, and only then
run [§1](#1-decommission) on the old one. The gap between "the new repository works" and "the old
key is destroyed" costs you storage; closing it early costs you the ability to go back.

---

## 3. What is deliberately not automated

There is no `ClusterDecommission` CRD, and adding one is not on the roadmap.

The reason is not that the operation is too dangerous for a controller — it is that **a gate can
only promise what the mechanism can deliver**. A typed confirmation is worth having when the
deletion it guards has no alternative: `ClusterErasure` removes snapshots from a repository, and
once the prune completes there is no other copy to fall back on. A decommission is not like that.
It destroys *CrystalBackup's* copy of the key. If an admin kept one in a password manager, an
escrow, or an old backup of the operator namespace, the objects stay readable and nothing has
actually been destroyed — §1.4 says exactly this. Wrapping that in a confirmation dialog would
dress a best-effort act up as a guarantee.

A CRD would add a second problem on top: it is a **desired state a controller converges to**, so it
re-fires after an etcd restore, a GitOps re-apply, or a stray `kubectl apply -f` from a directory
nobody pruned. But that is the lesser objection. The first one stands even for a one-shot action.

`ClusterErasure` is a CRD precisely because it is **bounded**: a typed confirmation naming a scope
that must match, a count recorded before the deletion, and a target that selects nothing rather
than everything when it is under-specified. None of those guards translate to an operation whose
scope is "all of it".
