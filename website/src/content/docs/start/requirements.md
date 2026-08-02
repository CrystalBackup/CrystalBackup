---
title: Requirements
description: What your cluster, your storage and your object storage need to provide before installing Crystal Backup.
---

## Check your cluster before you install

Everything on this page can be checked automatically, against the cluster you actually have, by
a script that **installs nothing and changes nothing**. It creates no objects, writes no files —
not even temporary ones — and only ever issues `kubectl get`, `kubectl version` and
`kubectl auth can-i`.

It exists mainly to answer one question this page can only describe in the abstract: **which of
your StorageClasses will actually have their data backed up, and which will be skipped.** For
each StorageClass it resolves the exposer CrystalBackup would choose — `cephfs-shallow`,
`csi-generic`, or *skipped* with reason `CSISnapshotUnsupported` — and tells you how many PVCs
sit on each. Discovering three weeks in that a namespace was never protected because its CSI
driver cannot snapshot is a bad way to find out.

That routing table is not written by hand. It is **generated from the operator's own selection
code** (`internal/exposer`) and held to it by a CI guard, so the script cannot drift into
describing a version of the logic that no longer exists.

### Download it, read it, run it

```bash
BASE=https://crystalbackup.github.io/CrystalBackup
curl -fsSLO "$BASE/preflight.sh"
curl -fsSLO "$BASE/preflight.sh.sha256"
curl -fsSLO "$BASE/preflight.sh.cosign.bundle"

# 1. the checksum
sha256sum -c preflight.sh.sha256          # macOS: shasum -a 256 -c preflight.sh.sha256

# 2. the signature — keyless, the same Sigstore trust root as our container images
cosign verify-blob preflight.sh \
  --bundle preflight.sh.cosign.bundle \
  --certificate-identity-regexp '^https://github\.com/CrystalBackup/CrystalBackup/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# 3. read it — plain POSIX shell, and its header states exactly what it does
less preflight.sh

# 4. run it
sh preflight.sh
```

`--json` gives the same findings as a machine-readable document for automation. `jq` is used if
present and is not required; without it the script says so and falls back to a built-in encoder.

Exit codes: **0** ready, **1** ready with reservations, **2** blocking, **3** could not be
assessed at all. A check that could *not be made* — a permission you lack, a CRD it could not
read — is reported as such and lands in exit 1. It is never counted as a pass.

### Or, the one-liner

```bash
curl -fsSL https://crystalbackup.github.io/CrystalBackup/preflight.sh | sh
```

This is a shortcut, and it is a real trade: piping a URL into a shell runs whatever the server
returns, and neither the checksum nor the signature is checked. It is fine for a scratch cluster
and a reasonable thing to want. For anything you care about, use the four steps above — they
take about twenty seconds longer and they are the reason we publish the checksum and the
signature at all.

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

Then provision it as a Secret in the operator namespace, under the data key `identity`:

```bash
kubectl -n crystal-backup-system create secret generic cluster-kek \
  --from-file=identity=cluster-kek.txt
```

The whole `age-keygen` file is accepted as is — its `# created:` / `# public key:`
comment lines included. Releases **up to 0.6.1** parse only the bare key line, and fail
with `KEKInvalid` (`malformed secret key: mixed case`) when given the full file; on those
versions, strip it down first:

```bash
kubectl -n crystal-backup-system create secret generic cluster-kek \
  --from-literal=identity="$(grep '^AGE-SECRET-KEY-' cluster-kek.txt)"
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
