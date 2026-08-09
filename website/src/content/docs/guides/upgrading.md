---
title: Upgrading
description: Upgrading the chart, the CRD problem Helm does not solve for you, and what the v1alpha1 API guarantees.
sidebar:
  order: 10
---

## What a version number means here

The project follows SemVer on major `0`: each milestone is a **minor** release
(`M_n` → `0.n.z`), hardening iterations are **patches**. One version string covers the
operator image, the mover image, the sync image and the chart's `appVersion` — they are one
release train and are meant to move together.

The CRD API is **`v1alpha1`**, and that is not a formality. Fields can be added, renamed or
removed between minor releases until `1.0.0`, which is a deliberate API-stability decision
taken after M9. Read the release notes before every minor upgrade; do not assume a manifest
that applied against `0.5` applies against `0.6`.

Patch releases do not change the API.

## The CRD problem

**Helm installs CRDs on first install and never upgrades them.** That is Helm's behaviour,
not a choice of this chart, and it means `helm upgrade` alone will leave you running a new
operator against old CRDs — which fails in the most confusing possible way: fields you set
are silently pruned by the API server, and the operator reconciles as though you never set
them.

Apply the CRDs yourself, before the chart:

```bash
# Pull the chart and take its CRDs. Use the version you are upgrading *to*;
# 0.6.4 is the current release.
helm pull oci://ghcr.io/crystalbackup/charts/crystal-backup --version 0.6.4 --untar
kubectl apply -f crystal-backup/crds/

# Then upgrade the operator.
helm upgrade crystal-backup \
  oci://ghcr.io/crystalbackup/charts/crystal-backup \
  --version 0.6.4 \
  --namespace crystal-backup-system
```

`kubectl apply` on CRDs is additive and safe: it adds new fields and never drops stored
objects.

## 0.6.3 → 0.6.4: a location that reported Ready can now report Degraded

Nothing to do before the upgrade, and no data moves — but the new operator can report a
location as not-Ready where the old one reported it healthy, so read this before you conclude
the upgrade broke something.

`0.6.4` makes the wrapped-DEK escrow pass **block repository provisioning in every state it
cannot positively prove safe**. Two states are safe: an in-cluster DEK already exists, so
nothing can be minted; or there is provably no DEK anywhere, so minting is what should happen.
Everything else now blocks and sets `Ready=False` with reason `DEKEscrowUnresolved`, phase
`Degraded`, and condition `DEKEscrowed` carrying which case it is:

```bash
kubectl get clusterbackuplocations
kubectl get clusterbackuplocation <name> \
  -o jsonpath='{range .status.conditions[?(@.type=="DEKEscrowed")]}{.reason}: {.message}{"\n"}{end}'
```

If one goes `Degraded` on the first reconcile after the upgrade, **the state it names was
already true on `0.6.3`** — it was simply not blocking anything, which is the defect this
release fixes. Three of them are worth knowing:

- **`EscrowConflict`** — the bucket object and the in-cluster DEK are both readable and are
  different keys. That is two repository generations, and the bucket copy may be the only key to
  the older one. On `0.6.3` such a location reported `Ready` while handing a wrong key to every
  mover. Do not delete anything; the bucket object is evidence.
- **`EscrowUnreachable`** — the bucket could not be read and there is no local DEK, so a
  recoverable key may be sitting in there. Usually credentials or endpoint, and it clears on its
  own once the bucket is reachable. Distinct from **`EscrowUnverifiable`**, which is the same I/O
  failure *with* a local DEK present and does **not** block, because there is nothing to mint.
- **`CredentialsUnavailable`** / **`KEKUnavailable`** — the Secret is missing. Restore it; the
  location recovers without intervention.

`EscrowWriteFailed` still does not block: the in-cluster DEK is known-good and only the bucket
copy is behind, which degrades bare-cluster DR rather than your backups.

## 0.6.2 → 0.6.3 under Argo CD: an object stops being rendered

Read this before you sync, not after. It has happened on a real cluster.

In `0.6.2` the chart rendered a `Namespace` object by default (`namespace.create` defaulted to
`true`). In `0.6.3` that default is `false`, and the object is simply gone from the render. Under
Argo CD with automated prune, **an object that stops being rendered is an object that gets
deleted** — that is what prune means, and it does not distinguish between "the author removed
this" and "the author changed the default". So a `0.6.2` → `0.6.3` sync can delete
`crystal-backup-system` and everything inside it, including the Secret holding your cluster KEK
and every wrapped DEK. Nothing in object storage is touched, and every repository those keys
protect becomes permanently unreadable — a
[decommission](https://github.com/CrystalBackup/CrystalBackup/blob/main/docs/DECOMMISSION.md#14-the-key-itself)
executed by accident, during a patch upgrade.

The remedy is to get the namespace out of the Application's prunable set **before** the upgrade:
stop tracking it in the operator `Application` — a separate Application of its own, with prune
off, or exclude it from the sync scope. Once the namespace is not something that Application
renders, no change to `namespace.create` can reach it. Then upgrade. The reasoning, and the
shape to use, are in
[Install with Argo CD](/CrystalBackup/docs/start/install-argocd/#the-namespace--yours-not-the-charts).

The same hazard applies to a Flux `HelmRelease` with pruning enabled, and to any other
reconciler that treats "no longer rendered" as "delete". After the upgrade, `namespace.create`
should stay `false` permanently: a namespace Helm owns is a namespace a prune or a
`helm uninstall` can take, with the keys inside it.

## Before you upgrade

**1 — Let in-flight work finish.** An upgrade restarts the operator, which is safe by
design — mover Jobs have deterministic names and are re-adopted rather than restarted — but
there is no reason to do it during a prune window.

```bash
kubectl -n crystal-backup-system get jobs -l app.kubernetes.io/managed-by=crystal-backup
kubectl get clusterbackups
kubectl get restores,clusterrestores -A
```

**2 — Know where your keys are.** An upgrade does not touch them, but the moment you
discover your KEK escrow is stale should not be the moment you need it.

**3 — Read the release notes.** Particularly for a minor bump.

## During

```bash
kubectl -n crystal-backup-system rollout status deploy/crystal-backup
```

The operator restarts. If you run more than one replica, leader election keeps exactly one
active and the rest are warm standbys, so the restart is a leadership handover rather than
an outage.

Nothing in object storage is touched by an upgrade. Repositories are not migrated, keys are
not rotated, and no data moves.

## After

```bash
# The operator is up on the new version.
kubectl -n crystal-backup-system get deploy crystal-backup \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'

# Locations are still Ready.
kubectl get clusterbackuplocations
kubectl get backuplocations -A

# Repositories are still reachable.
kubectl get backuprepositories
```

Then let one scheduled backup run and check it completed. An upgrade you did not verify
with a real backup is an upgrade you have not verified.

## Downgrading

Not supported, and worth being blunt about. New CRD fields written by a newer operator are
unknown to an older one; the API server prunes them on the next write, and the older
operator reconciles against a truncated spec.

If you have to go back: uninstall, re-apply the older CRDs, reinstall the older chart, and
re-create your custom resources. The **repositories are unaffected** — that is the point of
the repository being the source of truth. Discovery will project the backups again.

Follow the ordered [uninstall procedure](/CrystalBackup/docs/start/install/#uninstall) for
that first step. Removing the operator while a `Backup`, `Restore` or location still carries
its finalizer strands the namespace in `Terminating` permanently, and a downgrade is exactly
the moment you will be deleting objects in a hurry.

## Upgrading across several minors

Go one minor at a time (`0.3` → `0.4` → `0.5`), applying each release's CRDs and letting
a backup cycle complete in between. Skipping minors on an alpha API is how you find out
which migration you needed.

## Images

Production references images **by digest**, never by tag. The chart's published values
carry the real digests for that release; a chart installed from a source checkout carries a
placeholder and will not pull.

```bash
kubectl -n crystal-backup-system get pods \
  -o jsonpath='{range .items[*]}{.spec.containers[0].image}{"\n"}{end}'
```

The mover and sync images are pinned separately and are passed to every mover Job. They
move with the operator, so a partial upgrade — new operator, old mover digest — is
something to avoid rather than something to try.
