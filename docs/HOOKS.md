# Consistency hooks — and the ServiceAccount they run as

A **hook** is a command Crystal Backup runs inside one of your containers around the snapshot, so
that what lands in the repository is a coherent state rather than a torn one. A `CHECKPOINT` before
the snapshot and a resume after it; a `FLUSH TABLES WITH READ LOCK` and its release; whatever your
application needs.

This page is mostly about **who runs that command**, because on the namespace plane the answer is
"an identity you control", and it takes one ServiceAccount to set up.

---

## The short version

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
    serviceAccountName: crystal-backup-hooks   # ← required for hooks in your namespace
    pre:
      - podSelector: { matchLabels: { app: postgres } }
        container: postgres
        command: ["psql", "-c", "CHECKPOINT"]
        timeout: 30s
        onError: Fail
```

Plus, once per namespace:

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
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: Role, name: crystal-backup-hooks }
subjects:
  - kind: ServiceAccount
    name: crystal-backup-hooks
    namespace: team-x
```

That is the whole setup. The name is yours to choose — `crystal-backup-hooks` is only a
suggestion.

---

## Why a ServiceAccount at all

The operator holds the ability to `exec` into containers. Without a rule, anyone who could write a
`BackupSchedule` in a namespace could make the operator run **any command in any pod of that
namespace** — including someone who was never given `pods/exec` themselves. Writing a backup
schedule would have been a way to get shell access.

So the operator does not use its own identity for your hooks. It asks the Kubernetes API server to
run the command **as the ServiceAccount you named**, in your namespace. The API server then applies
that ServiceAccount's permissions, exactly as it would if you ran `kubectl exec` yourself.

The consequences are worth stating plainly:

- **You decide what the platform can do.** Grant the ServiceAccount `pods/exec` on everything, or
  narrow it with `resourceNames` to just your database pods. The platform can do that and no more.
- **Revocation is immediate.** Delete the RoleBinding and the next hook fails — there is no cached
  approval from when the schedule was created.
- **The namespace is never yours to choose.** Only the *name* is configurable. The namespace is
  always the one being backed up, so no schedule, anywhere, can reach into another team's pods.

The same rule covers hooks declared by **pod annotations** (`honorAnnotations: true`): the
annotation supplies the command, never the identity.

---

## Narrowing the grant

`pods/exec` on the whole namespace is the simple form. If you want the platform to be able to exec
into your database pods and nothing else, name them:

```yaml
rules:
  - apiGroups: [""]
    resources: ["pods/exec"]
    verbs: ["create"]
    resourceNames: ["postgres-0", "postgres-1"]
```

`resourceNames` matches **pod names**, so this suits StatefulSets (stable names) better than
Deployments (generated suffixes). For a Deployment, either keep the namespace-wide grant or point
your hooks at a StatefulSet.

---

## When it is not set up

A backup whose schedule declares hooks without `serviceAccountName` does not run the hooks with the
operator's privileges — it stops and tells you:

```
$ kubectl describe backup nightly-20260728-010000 -n team-x
...
  Conditions:
    Type    Status  Reason                    Message
    Ready   False   HooksNeedServiceAccount   hooks on a namespaced BackupLocation must set
                                              hooks.serviceAccountName — a ServiceAccount in this
                                              namespace, granted `create pods/exec`, that the
                                              operator impersonates to run them
```

If the ServiceAccount exists but lacks the grant, the hook itself fails and names the identity:

```
  Hooks:
    Phase:   pre
    Pod:     postgres-0
    Result:  Failed
    Message: exec [psql -c CHECKPOINT] in team-x/postgres-0 [postgres]
             as system:serviceaccount:team-x:crystal-backup-hooks: pods "postgres-0" is
             forbidden: User "system:serviceaccount:team-x:crystal-backup-hooks" cannot create
             resource "pods/exec" in API group "" in the namespace "team-x"
```

The identity in that message is the one to grant, not the operator's.

---

## Hook semantics (both planes)

| field | meaning |
|---|---|
| `podSelector` | which pods this hook applies to. Candidates are the **Running pods in the backed-up namespace that mount one of the PVCs this run captures** — a pod holding none of the backed-up data is not consistency-relevant. |
| `container` | which container to exec in. Defaults to the pod's **first** container. |
| `command` | argv, never a shell string. Need a shell? Make it explicit: `["/bin/sh", "-c", "..."]`. |
| `timeout` | how long the command may take. A hook that overruns is **failed**, not waited for — it would otherwise hold your database frozen indefinitely. |
| `onError` | `Fail` (default) aborts the backup; `Continue` records the error and proceeds. |

**Post hooks always run.** If the snapshot fails, the release still fires — otherwise the one
workload whose backup just went wrong would be the one left quiesced. They are also retried, which
pre hooks are not: a failed pre hook means the snapshot should not be taken, while a failed post
hook means an application may still be frozen.

The freeze window is the **snapshot phase only**, not the upload. Your database is held for the
seconds it takes to cut a CSI snapshot, not for the minutes it takes to move the data.

---

## Pod annotations

With `hooks.honorAnnotations: true`, a pod can declare its own hook and it **replaces** the
spec-declared ones for that pod (never merges — the same precedence rule Velero uses):

```yaml
metadata:
  annotations:
    crystalbackup.io/pre-backup-command: '["psql","-c","CHECKPOINT"]'
    crystalbackup.io/pre-backup-container: postgres
    crystalbackup.io/pre-backup-timeout: 30s
    crystalbackup.io/pre-backup-on-error: Fail
```

It is **opt-in** because it delegates *what gets run* to anyone who can annotate a pod. The
identity is still `hooks.serviceAccountName`, so the blast radius stays exactly what you granted —
but the choice of command moves.

---

## The cluster plane

Hooks on a `ClusterBackupSchedule` are admin-authored and may omit `serviceAccountName`, in which
case they run as the operator itself. That is the pre-existing behaviour and the reason the
requirement is namespace-plane-only: an admin writing a cluster schedule already holds the
privilege they would be using.

An admin who wants the tighter model can set `serviceAccountName` there too; it works identically.

---

## See also

- [adr/0018](../spec/adr/0018-hook-execution-identity.md) — why impersonation rather than an
  admission check, and what the rest of the ecosystem does.
- [03-security-and-tenancy.md](../spec/03-security-and-tenancy.md) §5 — the confinement invariant
  this implements.
- [02-api.md](../spec/02-api.md) — the full `BackupSchedule` reference.
