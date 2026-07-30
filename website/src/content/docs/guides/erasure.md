---
title: The right to erasure
description: ClusterErasure — physically deleting a tenant, a namespace or a single PVC from a location.
sidebar:
  order: 6
---

`ClusterErasure` physically deletes data from a repository: `restic forget` filtered by
tag, then `prune`. It is an administrator operation, cluster-scoped, and it is not
reversible.

## What it is not

**It is not crypto-shredding.** The shared cluster repository has one master key. Destroying
that key would destroy every tenant's data, so per-tenant key destruction is impossible
here — it was dropped from the design rather than deferred. If your compliance regime
requires per-tenant cryptographic destruction, the shared cluster plane is not the
mechanism; give each tenant a namespace-plane location with a key of their own instead.

**It is not decommissioning a repository.** Retiring a whole repository by destroying its
key is a runbook, not a custom resource. `ClusterErasure` is a CRD precisely because it is
*bounded* — a desired state a controller can converge to, on a scope you can name.

## Erasing

Exactly one scope per object:

```yaml
# Everything tagged tenant=acme.
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterErasure
metadata:
  name: erase-acme
spec:
  locationRef:
    name: dr-primary
  target:
    tenant: acme
  confirmation: acme
```

```yaml
# Everything tagged namespace=team-x.
spec:
  locationRef: { name: dr-primary }
  target:
    namespace: team-x
  confirmation: team-x
```

```yaml
# One PVC inside one namespace.
spec:
  locationRef: { name: dr-primary }
  target:
    namespace: team-x
    pvc: uploads
  confirmation: team-x/uploads
```

`confirmation` must equal the target identity: the tenant name, the namespace name, or
`<namespace>/<pvc>`.

The tenant a namespace resolves to is its `crystalbackup.io/tenant` label if it has one,
and otherwise the namespace name. So `target.tenant` erases every namespace belonging to
that tenant in one operation — check what that covers before you run it:

```bash
kubectl get ns -l crystalbackup.io/tenant=acme
```

## The two-step

Like a restore, `confirmation` is optional in the schema so that leaving it out **parks**
the object rather than rejecting it:

```bash
kubectl apply -f erase-acme.yaml     # without confirmation
kubectl get clustererasure erase-acme
```

```
NAME         PHASE                  FORGOTTEN   AGE
erase-acme   AwaitingConfirmation   0           5s
```

Read what it is about to do. Then type the identity in:

```bash
kubectl patch clustererasure erase-acme --type=merge -p '{"spec":{"confirmation":"acme"}}'
```

A **wrong** value is rejected at admission and the object is never created. An **absent**
one parks. That asymmetry is deliberate: the typo path fails fast, the deliberate path
gives you a moment to look.

## Watching it

```bash
kubectl get clustererasure erase-acme -w
```

```
NAME         PHASE       FORGOTTEN   AGE
erase-acme   Running     0           8s
erase-acme   Completed   412         6m22s
```

```bash
kubectl get clustererasure erase-acme \
  -o jsonpath='{.status.snapshotsForgotten}{" snapshots, "}{.status.reclaimedBytes}{" bytes reclaimed\n"}'
```

Phases: `Pending`, `AwaitingConfirmation`, `Running`, `Completed`, `Blocked`, `Failed`.

## `Blocked`

On an `Immutable` location the erasure cannot proceed until Object Lock expires. The phase
is `Blocked` and `status.blockedUntil` carries the date.

Nothing is lost — the object stays and converges when the lock expires. But there is no way
to force it, which is what Object Lock means.

(Object Lock support is not implemented in this release; see
[When not to choose it](/CrystalBackup/docs/discover/when-not-to-use/).)

## How long it takes, and what it blocks

`prune` is the expensive half. On the shared cluster repository it takes the
**cluster-wide exclusive window** — no namespace can start a backup while it runs, and its
memory use scales with total repository size rather than with what you are erasing.

Erasing one small PVC still pays for a full prune. Batch erasures where you can, and run
them in the same off-peak window as scheduled maintenance.

Backups do not fail during the window; they **wait**. A `Backup` simply stays in `Pending`
until a slot opens, silently and self-resolvingly.

## What survives

The `Backup` projections for the erased snapshots disappear on the next discovery pass —
projection lifetime follows data lifetime, so `kubectl get backups` keeps telling the
truth.

What does **not** disappear: copies you made elsewhere. An external sync destination holds
its own snapshots under its own key, and erasing the source does not touch them. If the
erasure is a compliance obligation, enumerate every destination first:

```bash
kubectl get clusterbackupexternalsync,backupexternalsync -A \
  -o jsonpath='{range .items[*]}{.kind}{" "}{.metadata.namespace}/{.metadata.name}{" src="}{.spec.sourceLocationRef.name}{" dst="}{.spec.destinationLocationRef.name}{"\n"}{end}'
```

and erase at each of them too.

## Erasing on the namespace plane

There is no namespaced erasure resource. A tenant's own repository is theirs: they delete
snapshots with upstream restic, using their own key and their own credentials.

```bash
restic -r s3:https://s3.other.example/team-x-backups/crystal/prod-eu-1 \
  forget --tag namespace=team-x --prune
```

That is not a gap in the API. It is the same property as everywhere else on that plane —
the platform has no key, so the platform cannot do it for them.
