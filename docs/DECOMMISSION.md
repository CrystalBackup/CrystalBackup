# Runbook — decommissioning a repository, re-encrypting one, uninstalling the operator

Three operations that are not CRDs and never will be, because each is **irreversible in a way no
reconcile loop should own**: one destroys the only key that can read a repository, one rewrites
where a fleet's backups live, and one removes the controller that every finalizer in the cluster
depends on. They are runbooks an admin executes deliberately, with the operator's own machinery
doing the work.

- **[Decommission](#1-decommission)** — retire a repository by destroying its key. The bytes stay
  in the bucket and become permanently unreadable. This is the *only* surviving form of
  crypto-shredding ([adr/0004](../spec/adr/0004-encryption-key-management.md)).
- **[Re-encrypt](#2-re-encrypt)** — move a repository's contents under a **new** key, because the
  old one leaked. Since M5 this is not a special procedure: it is an
  [external sync](../spec/adr/0013-external-backup-sync.md) into a fresh location, followed by a
  decommission of the old one.
- **[Uninstall the operator](#3-uninstall-the-operator)** — take CrystalBackup off a cluster
  without stranding a namespace in `Terminating` forever. An ordered procedure, for the same
  reason: the finalizers the operator owns outlive it, and nothing else can clear them.

> **Neither is right-to-erasure.** Erasing one tenant's data is `ClusterErasure` (R21) — a CRD,
> with a typed confirmation and a compliance record. Decommission destroys **everything** in the
> repository at once. If a GDPR request brought you here, you are in the wrong document.

---

## 0. Before either operation

**Know what you are about to make unreadable.** A repository holds every namespace that ever
backed up to it, including ones whose owners have moved on and whose `Backup` objects were deleted
long ago. The objects in your cluster are not the inventory — the repository is:

```bash
kubectl get backuprepository <name> -o jsonpath='{.status.snapshotCount}{"\n"}{.status.approximateSizeBytes}{"\n"}'
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

**Cluster plane.** Pause the schedules and syncs pointing at this location:

```bash
kubectl patch clusterbackupschedule <name> --type=merge -p '{"spec":{"paused":true}}'
kubectl patch clusterbackupexternalsync <name> --type=merge -p '{"spec":{"paused":true}}'
```

A paused sync no longer trips `CrystalbackupExternalSyncStale`. That guard arrived in M6; before it,
following this step paged you 26 hours later about the copy you had just deliberately stopped. It is
silence with a deadline, not silence forever: if the pause is still there in seven days,
`CrystalbackupExternalSyncPausedTooLong` says so, because a secondary that quietly stopped being fed
is only discovered on the day you need it.

**Namespace plane.** `spec.paused` is on `BackupSchedule` and `BackupExternalSync` as well since M6,
so a tenant schedule is suspended exactly like a cluster one — and, unlike deleting it, the pause
keeps `status.lastSuccessTime` and `status.lastRunName`, which is what someone will want if the
decommission is called off:

```bash
kubectl patch backupschedule <name> -n <ns> --type=merge -p '{"spec":{"paused":true}}'
kubectl patch backupexternalsync <name> -n <ns> --type=merge -p '{"spec":{"paused":true}}'
```

If the namespace is being retired along with the repository, delete the schedule (and the sync)
instead — that is the difference the two operations are there to express, and the monitoring reads
it: `CrystalbackupSchedulePausedTooLong` and `CrystalbackupExternalSyncPausedTooLong` fire on an
object left paused for more than seven days, and never on a deleted one. Finish the decommission
here and neither will ever be heard from.

```bash
# retiring the namespace as well:
kubectl delete backupschedule <name> -n <ns>
# or keeping the tenant backed up somewhere else:
kubectl patch backupschedule <name> -n <ns> --type=merge \
  -p '{"spec":{"locationRef":{"name":"<another-location>"}}}'
```

Leaving it alone does not endanger the decommission: once [§1.2](#12-delete-the-location) removes the
`BackupLocation`, every run it stamps fails `LocationNotFound` before it ever opens the repository,
so nothing writes. It is noise rather than risk — the schedule keeps firing on its cron and each
failing run counts against your backup alerting. Settle it here instead of explaining the pages
afterwards.

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
- `status.snapshotCount` and `status.approximateSizeBytes` at decommission time — the size is the
  post-dedup physical footprint under the prefix, and it is *approximate* by name; record it as
  what it is rather than as an exact byte count;
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
    clusterKEKSecretRef: { name: cluster-kek }
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
kubectl get backuprepository dr-primary-rekeyed \
  -o jsonpath='{.status.lastCheckResult}{" at "}{.status.lastCheckTime}{"\n"}'
```

The green light is `Passed` **and** a `lastCheckTime` after the sync completed. Both halves matter:
`lastCheckResult` is refreshed on failure as well as success, so `Passed` on its own can be a stale
verdict from before the copy existed — and an empty pair means the new repository has never been
checked at all, which is not the same as healthy.

Then run an actual `Restore` against the new location. A rekey verified only by counters is a rekey
verified only by the thing that would also be wrong if the copy were broken.

### 2.4 Cut over, then decommission

Point the schedules at the new location, let a full backup cycle complete against it, and only then
run [§1](#1-decommission) on the old one. The gap between "the new repository works" and "the old
key is destroyed" costs you storage; closing it early costs you the ability to go back.

---

## 3. Uninstall the operator

Removing CrystalBackup from a cluster is an **ordered** operation, and the order is not a
preference. Get it wrong and namespaces stop in `Terminating` permanently — no timeout, no
eventual consistency, nothing that resolves it on its own. The way back is
[§3.3](#33-if-something-is-already-stuck), and it is more work than doing this in order.

### 3.1 Why the order exists

Six of the twelve kinds carry a finalizer, and **the operator is the only process that removes
one**:

| Finalizer | Carried by |
|---|---|
| `crystalbackup.io/location` | `ClusterBackupLocation`, `BackupLocation` |
| `crystalbackup.io/repository` | `BackupRepository` |
| `crystalbackup.io/backup` | `Backup` |
| `crystalbackup.io/restore-teardown` | `Restore` |
| `crystalbackup.io/cluster-restore-teardown` | `ClusterRestore` |

Delete the operator while one of those objects is alive and it becomes **unfinalizable**: the
object stays, the namespace holding it never leaves `Terminating`, and a later
`kubectl delete crd` — which waits for every instance — never returns either. `helm uninstall`
and `make undeploy` do not warn you; both simply succeed, and the damage shows up later.

The finalizers are not ceremony. They tear down live mover Jobs and their credential Secrets, the
transient RoleBinding a manifest capture holds in the tenant namespace, and above all the
`VolumeSnapshotContent` objects parked with `Retain`, which are cluster-scoped and which no
ownerReference GC will ever collect. Stripping a finalizer by hand leaks exactly those — which is
why it is [§3.3](#33-if-something-is-already-stuck) and not the procedure.

### 3.2 The order

Every command below is bounded with `--timeout`. That is deliberate: an unbounded
`kubectl delete` in this sequence is a terminal you will have to kill, and it tells you nothing
about which object is holding it.

**Step 1 — stop what creates new work.** Schedules and syncs carry no finalizer and go
immediately; what matters is that nothing stamps a new run while you are draining:

```bash
kubectl delete clusterbackupschedule --all --timeout=2m
kubectl delete clusterbackupexternalsync --all --timeout=2m
kubectl delete backupschedule --all --all-namespaces --timeout=2m
kubectl delete backupexternalsync --all --all-namespaces --timeout=2m
```

**Step 2 — let in-flight movers finish.** A mover killed mid-run is not a correctness problem
(nothing in the repository is ever half-written), but it leaves residue the finalizers below are
about to collect, so it is cheaper to wait:

```bash
kubectl get jobs -n crystal-backup-system -l app.kubernetes.io/managed-by=crystal-backup
```

**Step 3 — delete the finalized objects, operator still running.** Restores and backups before
the locations that address their repository:

```bash
kubectl delete restore        --all --all-namespaces --timeout=5m
kubectl delete clusterrestore --all --timeout=5m
kubectl delete clusterbackup  --all --timeout=5m
kubectl delete backup         --all --all-namespaces --timeout=5m
kubectl delete backuplocation --all --all-namespaces --timeout=5m
kubectl delete clusterbackuplocation --all --timeout=5m
kubectl delete clustererasure --all --timeout=2m
```

None of this touches object storage. Deleting a location removes its `BackupRepository`; the
bucket and every snapshot in it are untouched, and a reinstall re-discovers them.

**Step 4 — verify the cluster is empty of CrystalBackup objects.** This is the gate. Do not
proceed while it prints anything:

```bash
for r in restores clusterrestores backups clusterbackups backuplocations \
         clusterbackuplocations backuprepositories; do
  kubectl get "$r.crystalbackup.io" --all-namespaces --no-headers 2>/dev/null
done
```

Silence here means every finalizer has been cleared by the operator that owns them, and nothing
that follows can wedge. Output here means an object is still finalizing — wait, and if it does
not move, find out why *now*, while the operator that can still fix it is running:

```bash
kubectl get backup <name> -n <ns> -o jsonpath='{.metadata.finalizers}{"\n"}'
kubectl logs -n crystal-backup-system deploy/crystal-backup --tail=100
```

**Step 5 — remove the operator:**

```bash
helm uninstall crystal-backup -n crystal-backup-system
kubectl delete namespace crystal-backup-system --timeout=5m
```

The namespace holds the cluster KEK and the wrapped DEKs. Deleting it destroys the keys — that is
[§1.4](#14-the-key-itself) territory, i.e. every repository they protect becomes unreadable. If you
are uninstalling the operator but keeping the backups, **keep the namespace**, or escrow its
Secrets first.

**Step 6 — remove the CRDs, only if you mean it.** Helm does not delete them, on purpose: the
`Backup` projections are the readable inventory of what is in the repositories. Deleting the CRDs
deletes every remaining object of those kinds:

```bash
kubectl get crd -o name | grep crystalbackup.io | xargs -r kubectl delete --timeout=5m
```

After step 4 this returns in seconds. Run it before step 4 passes and it blocks forever.

### 3.3 If something is already stuck

The symptom is a namespace that will not go:

```bash
kubectl get ns <ns> -o jsonpath='{.status.phase}{"\n"}'          # Terminating
kubectl get backup -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{" "}{.metadata.finalizers}{"\n"}{end}'
```

**Reinstall the operator. That is the fix.** The objects are still served — a CRD stuck in
`Terminating` keeps serving reads and patches for its instances — so an operator brought back at
the same chart version picks up the pending deletions, runs the teardown it was supposed to run,
and clears the finalizers. Then restart at [§3.2](#32-the-order), in order this time.

```bash
# Not --create-namespace: Helm creates the namespace after rendering, so it gets no Pod Security
# labels, and crystal-backup-system must enforce `baseline` or the first mover is refused at
# admission. Here that would strand the very deletions you came to unblock.
kubectl create namespace crystal-backup-system --dry-run=client -o yaml | kubectl apply -f -
kubectl label namespace crystal-backup-system \
  pod-security.kubernetes.io/enforce=baseline \
  pod-security.kubernetes.io/enforce-version=latest \
  pod-security.kubernetes.io/audit=restricted \
  pod-security.kubernetes.io/warn=restricted --overwrite

helm install crystal-backup oci://ghcr.io/crystalbackup/charts/crystal-backup \
  --version <the version you removed> -n crystal-backup-system
```

**Only if you cannot reinstall**, strip the finalizer by hand. This unblocks the namespace and
**leaks** what the teardown would have collected:

```bash
kubectl patch backup <name> -n <ns> --type=merge -p '{"metadata":{"finalizers":null}}'
```

Then sweep the residue yourself, because nothing else will:

```bash
# mover Jobs and their pods
kubectl -n crystal-backup-system delete job -l app.kubernetes.io/managed-by=crystal-backup
# Retain-parked snapshot contents — cluster-scoped, never garbage-collected
kubectl get volumesnapshotcontent -l app.kubernetes.io/managed-by=crystal-backup
```

> Do **not** blanket-delete Secrets by that label in the operator namespace. The wrapped DEKs
> carry it too, and deleting one is a decommission ([§1.4](#14-the-key-itself)) — irreversible,
> and not what you came here for.

---

## 4. What is deliberately not automated

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
