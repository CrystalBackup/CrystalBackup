---
title: Requirements
description: What your cluster, your storage and your object storage need to provide before installing Crystal Backup.
---

## Kubernetes

**Version 1.30 or later.** This is a hard floor, not a recommendation: the admission model
is built on `ValidatingAdmissionPolicy`, which reached GA in 1.30. The Helm chart declares
`kubeVersion: ">= 1.30.0-0"` and will refuse to install below it.

You will need cluster-admin to install: the chart creates CRDs, cluster-scoped RBAC,
admission policies and a namespace.

## Storage — the CSI snapshot path

Backups are taken from a **read-only snapshot**, not from the live volume. That requires:

- the **`snapshot.storage.k8s.io/v1` API** — the external-snapshotter CRDs and the
  snapshot controller, installed in the cluster;
- at least one **`VolumeSnapshotClass`** for the CSI drivers backing the PVCs you want to
  protect;
- a CSI driver that supports snapshots.

A PVC on a driver that cannot snapshot is **skipped**, not silently dropped: the volume is
reported with `status.volumes[].phase: Skipped` and `reason: CSISnapshotUnsupported`. This
is worth checking before you assume a namespace is covered.

Storage-aware paths exist for CephFS (shallow `backingSnapshot`, a zero-copy read) and RBD
(copy-on-write clone). Any other snapshot-capable CSI takes the generic path: the
`VolumeSnapshotContent` is re-bound into `crystal-backup-system` as a static pair with a
temporary copy-on-write PVC.

**`volumeMode: Block` PVCs are not supported.** They are reported as per-volume failures
with reason `RestoreBlockUnsupported`.

## Object storage

S3-compatible object storage, reachable from the cluster. Tested against AWS S3, MinIO,
SeaweedFS and Ceph RGW.

Per location you need:

- an **endpoint**, a **bucket** and optionally a **prefix**;
- **credentials** with read and write on the prefix. Most non-AWS gateways also need
  `forcePathStyle: true`;
- if the endpoint uses a private CA, its PEM bundle for `spec.s3.caBundle`.

:::caution[Movers currently hold root bucket credentials]
Repository-scoped, short-lived mover credentials are not implemented in this release.
Every mover Job receives the location's credentials verbatim, so a compromised mover can
reach everything those credentials can reach. Scope the credentials to the bucket — or to
the prefix — at the object-storage side, and give each location its own.
:::

## Network

The chart installs default-deny NetworkPolicies for `crystal-backup-system` with narrow
allowances. Two things follow:

- **Enforcement is your CNI's job.** Some CNIs accept NetworkPolicy objects and enforce
  nothing (Kind's default `kindnet` among them). Their presence is not evidence the
  confinement holds — verify on your CNI.
- **An S3 endpoint on a private address needs an explicit rule.** Movers are denied egress
  to `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `169.254.0.0/16` and `127.0.0.0/8`
  on port 443 by default, to stop a compromised mover pivoting to in-cluster services. An
  on-premises S3 endpoint inside those ranges needs an entry in
  `networkPolicy.extraMoverEgress`.

## The cluster KEK — provision it before you install

For the cluster plane, the platform repository's key is a random restic password wrapped
by an **age X25519 identity**: the *cluster KEK*.

**Neither the chart nor the operator ever generates it.** A key born inside the cluster
would be lost with the cluster, and every backup with it. You generate it out of band,
escrow it **outside** the cluster, and provision it as a Secret yourself.

```bash
age-keygen -o cluster-kek.txt
```

Escrow `cluster-kek.txt` wherever you keep root secrets — a password manager, an HSM, a
sealed envelope. **It is the input to disaster recovery.** Without it, a bucket full of
backups is a bucket full of ciphertext.

The namespace plane needs nothing equivalent: each tenant's repository password is theirs,
either supplied by them or generated into their own namespace.

## Pod Security Admission

The operator namespace is labelled `enforce: baseline` (with `audit` and `warn` at
`restricted`). The operator itself is restricted-compliant, but data movers run as
`runAsUser: 0` with `DAC_OVERRIDE` — they have to preserve file ownership on restore —
which `restricted` would deny. That relaxation applies to `crystal-backup-system` only;
nothing changes in tenant namespaces.

## Sizing

The operator is small: `10m` CPU and `64Mi` memory requested, `500m`/`256Mi` limits.

The work happens in mover Jobs, one per PVC, and the number running at once is capped
cluster-wide by `maxConcurrentMovers`. Plan node capacity for that concurrency, not for
the operator.

The one thing that scales with total data rather than per namespace is `restic prune` on
the shared repository — it is memory-hungry and holds an exclusive window. Give it
off-peak hours and bound it with `pruneMaxRepackSize`. See
[Maintenance and verification](/CrystalBackup/docs/guides/maintenance/).

## Optional

- **Prometheus Operator** — for `metrics.serviceMonitor.enabled: true`. Without it,
  metrics are still served on port 8443 over HTTPS with API-server authn/authz; scrape
  them however you like.
- **An existing backup tool.** Coexistence is a design goal, not an afterthought. Add its
  namespace to `admission.deniedNamespaces` so Crystal Backup's tenant-facing resources
  cannot be created there.

Next: [Install with Helm](/CrystalBackup/docs/start/install/).
