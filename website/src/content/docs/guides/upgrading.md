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
# 0.6.2 is the current release.
helm pull oci://ghcr.io/crystalbackup/charts/crystal-backup --version 0.6.2 --untar
kubectl apply -f crystal-backup/crds/

# Then upgrade the operator.
helm upgrade crystal-backup \
  oci://ghcr.io/crystalbackup/charts/crystal-backup \
  --version 0.6.2 \
  --namespace crystal-backup-system
```

`kubectl apply` on CRDs is additive and safe: it adds new fields and never drops stored
objects.

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
