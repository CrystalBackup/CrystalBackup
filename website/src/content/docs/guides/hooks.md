---
title: Consistency hooks
description: Quiescing an application around the snapshot, and the ServiceAccount the operator impersonates to do it.
sidebar:
  order: 8
---

Snapshots are **crash-consistent** by default: the same state your application would see
after a power cut. Most things survive that. Some do not, and for those a hook lets you
quiesce before the snapshot and release after it.

## The two rules that shape everything

**The freeze window is the snapshot phase, not the upload.** Post hooks run as soon as every
snapshot is *cut* — not when the upload succeeds. A database held frozen for a multi-hour
upload is an outage, not a backup.

**Post hooks always run, and are retried. Pre hooks are not.** The asymmetry is deliberate:
a failed pre hook means the snapshot should not be taken, while a failed post hook means an
application may still be **quiesced**. Retrying is the difference between a transient blip
and an incident.

## Declaring hooks

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: BackupSchedule
metadata:
  name: nightly
  namespace: team-x
spec:
  locationRef: { name: my-offsite }
  schedule: "0 1 * * *"
  hooks:
    serviceAccountName: crystal-backup-hooks
    pre:
      - podSelector:
          matchLabels:
            app: postgres
        container: postgres
        command: ["psql", "-c", "CHECKPOINT"]
        timeout: 30s
        onError: Fail
    post:
      - podSelector:
          matchLabels:
            app: postgres
        container: postgres
        command: ["sh", "-c", "echo released"]
        timeout: 30s
        onError: Continue
```

| Field | Meaning |
|---|---|
| `podSelector` | Which pods. Candidates are already narrowed to **running pods in the backed-up namespace that mount one of the PVCs this run is capturing**; an empty selector means all of them. |
| `container` | Which container. Empty uses the pod's **first** container. |
| `command` | An argv, exec'd directly — **not** through a shell. For pipes or redirection use `["sh", "-c", "..."]`. |
| `timeout` | Bounds how long the application stays quiesced. Defaults to `30s`. A hook that overruns is **failed**, not waited for. |
| `onError` | `Fail` (the default) aborts the backup. `Continue` records the failure and proceeds. |

Candidacy by *mounted PVC* rather than by label alone is what confines the exec to
workloads whose data is actually being captured.

`onError: Fail` is the default because a pre-snapshot hook exists precisely to make the
snapshot trustworthy. If the quiesce did not happen, a snapshot taken anyway is a backup
that *looks* application-consistent and is not.

## The identity — required on the namespace plane

On the namespace plane the operator does **not** exec as itself. It **impersonates** a
ServiceAccount you name, in the namespace being backed up. The API server then authorises
each exec against that identity.

The consequence is the point: users can only make the platform run commands they could
already run themselves.

Create it once per namespace:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: crystal-backup-hooks
  namespace: team-x
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: crystal-backup-hooks
  namespace: team-x
rules:
  - apiGroups: [""]
    resources: ["pods/exec"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: crystal-backup-hooks
  namespace: team-x
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: crystal-backup-hooks
subjects:
  - kind: ServiceAccount
    name: crystal-backup-hooks
    namespace: team-x
```

The name is yours; `crystal-backup-hooks` is only a suggestion.

To narrow it further:

```yaml
rules:
  - apiGroups: [""]
    resources: ["pods/exec"]
    verbs: ["create"]
    resourceNames: ["postgres-0", "postgres-1"]
```

`resourceNames` matches **pod names**, so it fits StatefulSets (stable names) far better
than Deployments (generated suffixes).

Three things follow, and they are the reason it works this way:

- **You decide what the platform can do.** No grant, no exec.
- **Revocation is immediate.** Delete the RoleBinding and the next hook fails. The check
  happens at every exec; nothing is cached.
- **The namespace is never yours to choose.** Only the ServiceAccount *name* is a field. The
  namespace is always the one being backed up, derived from the pod the hook targets — a
  settable namespace would be a cross-tenant hole by construction.

### If you forget it

The backup is **gated**, not silently escalated to the operator's own privileges:

```
Conditions:
  Type    Status  Reason                    Message
  Ready   False   HooksNeedServiceAccount   hooks on a namespaced BackupLocation must set
                                            hooks.serviceAccountName — a ServiceAccount in this
                                            namespace, granted `create pods/exec`, that the
                                            operator impersonates to run them
```

If the ServiceAccount exists but lacks the grant, the hook fails and the message names the
identity: `system:serviceaccount:team-x:crystal-backup-hooks`.

### On the cluster plane

Hooks on a `ClusterBackupSchedule` are admin-authored, so `serviceAccountName` may be
omitted — they then run as the operator itself. Setting it works identically and is worth
doing where you can.

## Pod annotations

If you would rather let pod owners declare their own hooks, opt in:

```yaml
hooks:
  serviceAccountName: crystal-backup-hooks
  honorAnnotations: true
```

Then, on the pod:

```yaml
metadata:
  annotations:
    crystalbackup.io/pre-backup-command: '["psql","-c","CHECKPOINT"]'
    crystalbackup.io/pre-backup-container: postgres
    crystalbackup.io/pre-backup-timeout: 30s
    crystalbackup.io/pre-backup-on-error: Fail
    crystalbackup.io/post-backup-command: '["sh","-c","echo released"]'
    crystalbackup.io/post-backup-container: postgres
    crystalbackup.io/post-backup-timeout: 30s
    crystalbackup.io/post-backup-on-error: Continue
```

Four suffixes, on both prefixes: `-command`, `-container`, `-timeout`, `-on-error`.
`-command` is a JSON argv.

Both phases exist for the obvious reason: a `FLUSH TABLES WITH READ LOCK` needs its
`UNLOCK TABLES`, and whoever writes the first must be able to write the second in the same
place.

Two rules:

- **Annotations replace, they do not merge.** When a pod carries them, they take precedence
  and the schedule's hooks are skipped **for that pod**.
- **The annotation supplies the command, never the identity.** Hooks still run as
  `hooks.serviceAccountName`, with exactly the rights the namespace granted it.

`honorAnnotations` is opt-in and defaults to `false`, because it delegates *what the
operator execs* to anyone who can annotate a pod in the backed-up namespace.

## Reading what happened

```bash
kubectl -n team-x get backup nightly-20260730-010000 \
  -o jsonpath='{range .status.hooks[*]}{.phase}{"\t"}{.pod}{"\t"}{.container}{"\t"}{.source}{"\t"}{.result}{"\t"}{.message}{"\n"}{end}'
```

```
pre	postgres-0	postgres	spec	Succeeded	
post	postgres-0	postgres	spec	Succeeded	
```

`status.hooks` is the durable account of the freeze window: which pods were quiesced,
whether the release ran, and — when it did not — what you have to go and undo by hand.

Results are `Succeeded`, `Failed`, and `Skipped`. `Skipped` only ever appears in the **pre**
phase: an earlier hook in it failed with `onError: Fail`, so this one never ran. It is
recorded rather than omitted on purpose — a list showing three of five hooks invites the
reader to assume the missing two passed.

The **post** phase never stops. Every entry in it is a thaw owed to a different
application, so a broken release on one pod costs that pod only and the rest still run.

## When a pre hook aborts the backup

An `onError: Fail` pre hook stops the quiesce, and the run ends `Failed` with no snapshot.
The hooks **before** it, however, already succeeded, and their applications are quiesced —
so the run releases them before it reports the failure. Concretely:

- the release is scoped to the pods that were **actually** quiesced. A pod whose own hook
  failed, and a pod behind the abort marked `Skipped`, were never frozen and are never sent
  a thaw;
- while that release is in flight the Backup stays in `SnapshottingHooks`, with
  `Ready: ReleasingAfterAbortedQuiesce` naming both the abort and the attempt number;
- it is bounded by the same three attempts as any release, and ends at the same
  `UnfreezeFailed` Warning when they run out;
- the run then reports `Failed` for the pre-hook abort. A thaw that worked does not make it
  a success: no snapshot was taken.

`source` is `spec` or `annotation` — which is how you tell whether the command that ran is
the one you wrote.

`status.postHookAttempts` counts release retries. A non-zero value that stops climbing
means a release eventually succeeded. One that keeps climbing means **an application may
still be quiesced**, and that is the case to go and look at now.

## Writing hooks that are worth having

- **Checkpoint, do not dump.** A hook that runs `pg_dump` puts the dump inside the volume
  you are about to snapshot, doubling the data and the time. Flush and let the snapshot do
  its job.
- **Keep them under the timeout.** The timeout bounds the freeze, and the freeze is
  downtime.
- **Make them idempotent.** Post hooks are retried.
- **Use `onError: Continue` only when the hook's absence degrades consistency without
  invalidating the backup.** If the backup is worthless without it, `Fail` is the honest
  setting.
- **A database with its own backup operator probably does not need this.** WAL archiving
  beats a volume snapshot for point-in-time recovery. Use both, for different purposes.
