---
title: Diagnosis
description: Symptom-first troubleshooting — what each stuck state means and where the durable evidence lives.
sidebar:
  order: 2
---

## Where the evidence is

Mover Jobs and their pods are deleted when they finish. Before you go looking for logs that
no longer exist, know that the durable record is in object status:

| What ran | Durable record |
|---|---|
| A per-PVC backup or restore | `status.volumes[]` on the `Backup`, `status.resources` on the `Restore` |
| A hook | `status.hooks[]` and `status.postHookAttempts` on the `Backup` |
| A prune, check, forget or unlock | `status.recentMaintenance[]` on the `BackupRepository` |
| A repository's health | `status.lastCheckResult`, `staleLocks` on the `BackupRepository` |

And the general-purpose first look:

```bash
kubectl get <kind> <name> -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\t"}{.message}{"\n"}{end}'
```

Conditions carry the reason. Almost every stuck state below is named there.

## A location never reaches `Ready`

```bash
kubectl get clusterbackuplocation <name> \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\t"}{.message}{"\n"}{end}'
```

| Reason | Meaning |
|---|---|
| `KEKMissing` | The `clusterKEKSecretRef` Secret does not exist. Nothing is ever generated in its place — provision it. |
| `MultipleDefaults` | Two locations claim `default: true`. Only one may. |

Otherwise it is usually reachability. The repository-init Job has to pull the mover image
and run `restic init`:

```bash
kubectl -n crystal-backup-system get jobs -l app.kubernetes.io/managed-by=crystal-backup
kubectl -n crystal-backup-system logs job/<init-job>
```

Common causes, in the order they actually occur:

- **The mover image digest is a placeholder.** A chart installed from a source checkout
  carries one and the Job cannot pull.
- **The S3 endpoint is unreachable from the mover.** If it is on a private address, the
  shipped NetworkPolicies deny it — add `networkPolicy.extraMoverEgress`.
- **`forcePathStyle` is unset** against a non-AWS gateway.
- **A private CA** and no `s3.caBundle`.
- **Credentials without write access** on the prefix.

## A backup says `Completed` but a PVC is missing

```bash
kubectl -n <ns> get backup <run> \
  -o jsonpath='{range .status.volumes[*]}{.pvc}{"\t"}{.phase}{"\t"}{.reason}{"\n"}{end}'
```

`Skipped` with `CSISnapshotUnsupported` means the PVC's CSI driver cannot snapshot. The
backup is honest — the volume is reported, not silently dropped — but the data is not in it.
Either move the workload to a snapshot-capable class, or accept the gap knowingly.

A PVC not in the list at all was not selected. Check `pvcSelector`.

## A backup is stuck in `Pending`

Most often it is **waiting for the repository's exclusive queue** — a prune, a check or an
erasure is running, and the queue admits one mutating operation at a time. There is no
`Queued` phase; the wait is silent and self-resolving.

```bash
kubectl get backuprepository <name> \
  -o jsonpath='{range .status.recentMaintenance[*]}{.operation}{"\t"}{.startTime}{"\t"}{.result}{"\n"}{end}'
```

If nothing is running, check the cluster-wide mover cap and the actual Jobs:

```bash
kubectl -n crystal-backup-system get jobs -l app.kubernetes.io/managed-by=crystal-backup
kubectl -n crystal-backup-system get pods -l app.kubernetes.io/managed-by=crystal-backup
```

A mover pod in `Pending` is a scheduling problem — node capacity, or a temporary PVC that
cannot bind.

## A backup is stuck in `SnapshottingHooks`

A hook is running or has run and been recorded. Check what happened:

```bash
kubectl -n <ns> get backup <run> \
  -o jsonpath='{range .status.hooks[*]}{.phase}{"\t"}{.pod}{"\t"}{.result}{"\t"}{.message}{"\n"}{end}'
```

If the `Ready` condition says `HooksNeedServiceAccount`, a namespace-plane run declared
hooks with no identity. Set `hooks.serviceAccountName` and grant it `create pods/exec` —
see [Consistency hooks](/CrystalBackup/docs/guides/hooks/).

A `Failed` pre hook with `onError: Fail` aborts the run deliberately: the quiesce did not
happen, so a snapshot taken anyway would look application-consistent and would not be.

## `postHookAttempts` keeps climbing

**This is the urgent one.** A release hook is still failing, which means **an application
may still be quiesced**.

```bash
kubectl -n <ns> get backup <run> \
  -o jsonpath='{.status.postHookAttempts}{"\n"}{range .status.hooks[?(@.phase=="post")]}{.pod}{"\t"}{.result}{"\t"}{.message}{"\n"}{end}'
```

Post hooks are retried within a bounded budget precisely because of this. If the budget is
exhausted, go and release it by hand — `status.hooks` tells you which pod and what the
command was.

## A restore sits in `AwaitingConfirmation`

Working as designed. `spec.confirmation` is empty or absent.

```bash
kubectl -n <ns> patch restore <name> --type=merge \
  -p '{"spec":{"confirmation":"<namespace>"}}'
```

For a `ClusterRestore` the value is `spec.target.namespace`; for a `ClusterErasure` it is
the target identity.

If instead your `kubectl apply` was **rejected outright**, the value was wrong, not
missing — admission denies a non-matching value and admits an empty one.

## A restore fails on one volume

```bash
kubectl -n <ns> get restore <name> \
  -o jsonpath='{.status.phase}{"\t"}{.status.restoredVolumes}{"\t"}{.status.restoredBytes}{"\n"}'
kubectl -n <ns> get events --field-selector involvedObject.name=<restore-name>
```

| Reason | Meaning |
|---|---|
| `RestoreBlockUnsupported` | `volumeMode: Block`. Not supported. |
| A node-affinity or attach failure | The target volume is attached elsewhere. Scale the workload down and retry. |

A restore reports per-resource failures and **continues**; it does not abort on the first
one. `status.resources.entries` carries the per-object detail, capped at 100 with
`truncated` telling you when entries were dropped.

## A restore's manifests did not come back

Check whether they were selected. The rule that catches people:

- field **omitted** → everything of that kind;
- field **present but empty** (`[]`) → **nothing** of that kind.

So `resources: []` restores no manifests at all, deliberately. See
[Restoring](/CrystalBackup/docs/guides/restore/#the-rule-that-catches-people).

Also check they were **captured**: `includeManifests` must have been true on the run, and
`status.manifests` on the `Backup` should carry a snapshot ID and a resource count.

## An object is stuck in `Terminating`

A finalizer is holding it while the controller tears down live exposures, mover Jobs and
staging volumes.

```bash
kubectl get <kind> <name> -o jsonpath='{.metadata.finalizers}{"\n"}'
kubectl -n crystal-backup-system logs deploy/crystal-backup --tail=200
```

:::danger[Do not strip the finalizer]
Removing it by hand is exactly how you get the leak the finalizer exists to prevent: an
orphaned temporary PVC and a `VolumeSnapshotContent` parked on `Retain` that no garbage
collector can ever reach, on a cluster-scoped object nobody will think to look for. Find
out what the controller is waiting on instead.
:::

## `staleLocks` is persistently non-zero

Repository lock objects older than restic's 30-minute threshold are accumulating faster
than they are reaped. Every exclusive operation will eventually stall behind them.

A hard-killed mover's lock is normally cleared by an unlock operation. If it is not:

```bash
kubectl get backuprepository <name> \
  -o jsonpath='{.status.staleLocks}{"\n"}{range .status.recentMaintenance[*]}{.operation}{"\t"}{.result}{"\t"}{.message}{"\n"}{end}'
```

Look for failing `unlock` operations in that history. Check that no second writer is
touching the repository out of band — the queue assumes a single leader, and an external
`restic prune` run by hand is outside that assumption.

## `lastCheckResult: Failed`

restic found repository damage. This is an incident, not a transient error.

```bash
kubectl get backuprepository <name> \
  -o jsonpath='{.status.lastCheckTime}{"\t"}{.status.lastCheckResult}{"\n"}'
```

Do not prune. Run a manual check with more detail, from a machine with the key:

```bash
restic -r $REPO check --read-data-subset 10%
```

If packs are damaged, the recovery is your **second copy** — an external sync destination,
if you have one. This is what they are for.

## A sync's lag keeps growing

```bash
kubectl get clusterbackupexternalsync <name> \
  -o jsonpath='{.status.phase}{"\t"}{.status.lagSnapshots}{"\t"}{.status.snapshotsCopied}{"\t"}{.status.bytesCopied}{"\n"}'
```

Usually bandwidth: the source is producing faster than the sync moves. Either narrow
`selection.namespaces` or run it more often, so each run has less to move.

If `phase` is `Failed`, look at the sync Job while it is still there. Two frequent causes:
the destination's credentials, and a destination location that is not `Ready`.

## An admission rejection you do not understand

The message names the rule. See [Admission rules](/CrystalBackup/docs/reference/admission/).

The two that surprise people most:

- **`spec.source is immutable`** — you edited a `Restore`'s source. Delete it and create
  another; the controller re-derives the source every pass, so a mid-run edit would mix two
  points in time inside one restore.
- **`spec.clusterID is immutable`** — location identity composes the repository path. An
  edit would silently re-point the location at a different repository with no data moving.

## Getting help

Include, always: the object's full `status.conditions`, the relevant `status` sub-objects,
the operator version, and the Kubernetes version. Redact bucket names and endpoints if you
must, but keep the reasons and messages verbatim — they are the diagnosis.

[Open an issue](https://github.com/CrystalBackup/CrystalBackup/issues).
