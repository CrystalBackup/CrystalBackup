---
title: Storage compatibility
description: Which storage solutions Crystal Backup can back up, the mechanical rule that decides it, what a real campaign measured, and what is only deduced.
---

Whether Crystal Backup can back up the *data* of a volume is decided by one mechanical rule,
applied per PVC. There is no allow-list of vendors, no per-driver special case beyond a single
substring test, and no configuration that changes the outcome.

This page states the rule, then reports what a real campaign measured against it, then — kept
strictly apart — what the rule *implies* for storage this project has never run on.

## The rule

Implemented in `Registry.For` (`internal/exposer/registry.go`), specified by
[ADR 0003](https://github.com/CrystalBackup/CrystalBackup/blob/main/spec/adr/0003-snapshot-exposure-csi-generic-first.md).

1. Read the PVC's `spec.storageClassName`, then that StorageClass's `provisioner`.
2. List every `VolumeSnapshotClass` in the cluster and look for one whose `driver` is
   **strictly equal** to that provisioner. Several matches are legal; the lexicographically
   smallest name wins, so the choice is deterministic.
3. **No match → the volume is skipped.** `status.volumes[].phase: Skipped`,
   `reason: CSISnapshotUnsupported`, plus an Event. The `Backup` still completes and the
   namespace's manifests are still captured — a skipped volume is neutral in the phase roll-up,
   never dressed up as a success and never a hard failure.
4. Provisioner name **contains the substring `.cephfs.csi.`** → `cephfs-shallow`: a
   `ReadOnlyMany` PVC backed directly by the snapshot (`backingSnapshot`), zero copy.
5. **Everything else** → `csi-generic`: `VolumeSnapshot` in the origin namespace → the bound
   `VolumeSnapshotContent` is pinned `Retain` and statically re-bound into
   `crystal-backup-system` → a temporary PVC is created **in the operator namespace** from that
   static snapshot → an unprivileged mover reads it.

Two consequences worth stating plainly:

- **String equality, not vendor detection.** A driver whose `VolumeSnapshotClass` exists under a
  different `driver` string than the StorageClass `provisioner` will not be matched. This is also
  why the CephFS test is a substring on the *name*, and why a driver that merely happens to sit
  on CephFS storage is not routed to `cephfs-shallow` (see [the `ceph-nfs` trap](#the-ceph-nfs-trap)).
- **The rule predicts routing, not success.** It answers "which path will be taken", not "will
  the driver honour it". A driver can resolve cleanly to `csi-generic` and then refuse to create
  the temporary PVC. That volume is *not* `Skipped` — `Skipped` is reserved for the one case the
  operator can decide up front, before touching the storage: no `VolumeSnapshotClass` at all.

The question that determines everything downstream is always the same:
**can the driver create a volume *from* a snapshot, and at what cost?**

## Verified results

The table below is not a compatibility claim written from documentation. Each row is one run of
`test/crucible/scripts/csi-probe.sh`, which replays the exposure path step for step — dynamic
`VolumeSnapshot`, `Retain` pin, static `VolumeSnapshotContent` re-bind **into another namespace**,
temporary PVC from the static snapshot, read-only mount, checksum of the data read back against
the data written.

Bench: RKE2 on Hetzner Cloud, 3 masters + 3 workers, 2026-08-01. 13 StorageClasses probed,
**zero anomalies**. Raw artifacts: `test/crucible/artifacts/csi-probe-*.json`; aggregate:
`test/crucible/artifacts/csi-compat-report.md`.

| StorageClass | Provisioner | Exposer | Verdict | Temp PVC bound (50 MiB → 500 MiB) |
|---|---|---|---|---|
| `ceph-block` | `rook-ceph.rbd.csi.ceph.com` | `csi-generic` | **COMPATIBLE** | 1.7 s → not measured |
| `ceph-filesystem` | `rook-ceph.cephfs.csi.ceph.com` | `cephfs-shallow` | **COMPATIBLE** | 1.7 s → 0.4 s |
| `ceph-nfs` | `rook-ceph.nfs.csi.ceph.com` | `csi-generic` | **COMPATIBLE** | 8.6 s → 18.3 s |
| `csi-nfs` | `nfs.csi.k8s.io` | `csi-generic` | **COMPATIBLE** | 4.5 s → 14.1 s |
| `longhorn` | `driver.longhorn.io` | `csi-generic` | **COMPATIBLE** | 3.1 s → 3.1 s |
| `openebs-lvm-thin` | `local.csi.openebs.io` | `csi-generic` | **COMPATIBLE** | 1.7 s → not measured |
| `openebs-zfs` | `zfs.csi.openebs.io` | `csi-generic` | **COMPATIBLE** | 1.7 s → 1.7 s |
| `topolvm-thin` | `topolvm.io` | `csi-generic` | **COMPATIBLE** | 1.7 s → 1.7 s |
| `openebs-lvm` (thick VG) | `local.csi.openebs.io` | `csi-generic` | **INCOMPATIBLE** | never bound |
| `csi-smb` | `smb.csi.k8s.io` | — | **SKIPPED** | — |
| `hcloud-volumes` | `csi.hetzner.cloud` | — | **SKIPPED** | — |
| `local-path` | `rancher.io/local-path` | — | **SKIPPED** | — |
| `openebs-hostpath` | `openebs.io/local` | — | **SKIPPED** | — |

:::note[What the timings are and are not]
"Temp PVC bound" is the time to provision the temporary PVC from the snapshot, at 50 MiB and then
at 500 MiB of source data. Flat means the driver did not copy; growing means it probably did. It
is a **timing heuristic**, not a measurement of the storage backend, and it can be wrong in both
directions — a fast array can copy 500 MiB quickly enough to look like copy-on-write, and a
throttled backend can make a genuine clone look linear. Sub-second differences are probe noise.
A `COMPATIBLE` verdict covers the exposure path only: not restore, not behaviour under load, not
snapshot quotas.
:::

### CephFS: the zero copy is measured

`ceph-filesystem` routes to `cephfs-shallow`, and the temporary PVC bound in **1.7 s for 50 MiB
and 0.4 s for 500 MiB**. Ten times the data, no more time — and less, which is noise around a
constant. This is the one place where the zero-copy claim is a measurement rather than a design
argument.

It matters because the generic path on CephFS would not be free: a normal writable PVC created
from a CephFS snapshot is a **full subvolume copy**. `cephfs-shallow` exists precisely to avoid
that, and it needs both `ReadOnlyMany` and `backingSnapshot` to do so.

### The `ceph-nfs` trap

`ceph-nfs` is Rook's NFS export — the same Ceph filesystem underneath, reached over NFS. Its
provisioner is `rook-ceph.nfs.csi.ceph.com`.

**That name does not contain `.cephfs.csi.`.** The substring test fails, so the volume is routed
to `csi-generic`, not `cephfs-shallow`.

The cost of that routing was measured: temporary PVC bound in **8.6 s at 50 MiB and 18.3 s at
500 MiB**, while `ceph-filesystem` stayed flat over the same tenfold increase.

Same underlying storage. Radically different cost, decided entirely by which driver you reach it
through. If you have a choice between mounting a Ceph filesystem via ceph-csi and via the NFS
export, that choice is a backup-cost decision, and the NFS side is the expensive one.

### OpenEBS LVM: thick and thin are two different answers

Same provisioner (`local.csi.openebs.io`), same `VolumeSnapshotClass`, opposite outcomes:

- **`openebs-lvm` on a thick volume group — INCOMPATIBLE.** The temporary PVC never reached
  `Bound`. The driver's own message:
  `snapshot ... is not of thin type`. Reproduced three times.
- **`openebs-lvm-thin` on a thin pool — COMPATIBLE.** Snapshot ready in 1.7 s, temporary PVC
  bound in 1.7 s.

This is the clearest illustration of "the rule predicts routing, not success". Resolution
succeeded in both cases — a matching `VolumeSnapshotClass` exists — and the thick case failed
later, inside the driver. Such a volume is not reported `Skipped`; it does not complete.

If you run OpenEBS LocalPV LVM, check whether your volume group is thin-provisioned before
counting those PVCs as protected.

### Longhorn: compatible, with a fixed 140-second toll

`longhorn` is COMPATIBLE, and its provisioning is constant: the temporary PVC bound in 3.1 s at
both 50 MiB and 500 MiB. But **mounting** that PVC took **139.6 s and 140.1 s** — against 5 to
10 s for the Ceph classes.

That is a fixed cost per volume, not a cost of copying data, and it does not appear at all in the
provisioning numbers. On a namespace with 30 PVCs it is the term that dominates the run. Size
your backup windows and your `maxConcurrentMovers` with it in mind.

### `csi.hetzner.cloud`: no snapshot at all

`hcloud-volumes` is SKIPPED, and the reason is upstream of Crystal Backup: the driver does not
implement `CreateSnapshot`, so no `VolumeSnapshotClass` exists for it. Verified by counting the
`VolumeSnapshotClass` objects whose `driver` is `csi.hetzner.cloud` on the cluster: zero.

Manifests are still captured for those namespaces. The volume *data* is not.

### `csi-nfs`: compatible, cost indeterminate but growing

`csi-nfs` (csi-driver-nfs, `nfs.csi.k8s.io`) is COMPATIBLE. The temporary PVC bound in 4.5 s at
50 MiB and 14.1 s at 500 MiB — classified "indeterminate", but clearly growing, which is
consistent with an implementation that archives the volume into a tar file. Treat it as paying
for the data, not as a clone.

### `piraeus-thin`: not qualified

LINSTOR/Piraeus does **not** appear in the table because it could not be brought up on this
bench: the DRBD9 kernel module was unavailable on every worker. Piraeus's loader compiles it and
requires `linux-headers-$(uname -r)`, which the node image did not carry.

That is a **prerequisite failure on our side, not a verdict on the driver**. Crystal Backup has
no result for LINSTOR, in either direction.

## Two operational traps found during the campaign

Neither is specific to Crystal Backup. Both break `VolumeSnapshot` for anything that uses it.

### More than one `snapshot-controller` in the cluster

The OpenEBS LVM and ZFS charts each embed their own `snapshot-controller`, *in addition to* the
one your distribution already ships (RKE2 and k3s do; so do most managed distributions).

The snapshot controller is a **cluster-scoped singleton**. Two or more instances reconcile every
`VolumeSnapshot` in the cluster and fight over optimistic writes. The symptom is that the
`VolumeSnapshot` never reaches `readyToUse`, with:

```
Operation cannot be fulfilled on volumesnapshots.snapshot.storage.k8s.io "…":
the object has been modified; please apply your changes to the latest version and try again
```

This does not only break the driver whose chart brought the extra copy. **It can break snapshots
cluster-wide** — including on storage that is otherwise perfectly healthy.

Detect it by listing every workload carrying a `snapshot-controller` image:

```bash
kubectl get deploy,statefulset,daemonset -A -o json | jq -r '
  .items[]
  | . as $w
  | $w.spec.template.spec.containers[]
  | select(.image | contains("snapshot-controller"))
  | "\($w.kind)/\($w.metadata.namespace)/\($w.metadata.name)  \(.image)"'
```

More than one line is the fault. These charts expose no value to turn it off, so the fix is to
remove the `snapshot-controller` container from the workload after installation, keeping the one
your distribution owns.

### Missing device-mapper kernel modules

OpenEBS LocalPV LVM takes its snapshots with `lvcreate --snapshot`, which needs the `dm_snapshot`
device-mapper target — **including on a thin LV**.

Without it, the `LVMSnapshot` custom resource stays `Pending` and **nothing surfaces to the
Kubernetes API**: no status, no Event, no condition. The only trace is in the node agent's log:

```
Required device-mapper target(s) not detected in your kernel
```

Diagnose it in that order:

```bash
# 1. the CR is stuck with no explanation
kubectl get lvmsnapshots.local.openebs.io -A

# 2. the only place the reason exists — the node plugin on the node holding the volume
kubectl -n openebs get pods -o wide | grep -i lvm.*node
kubectl -n openebs logs <that-pod> --all-containers --tail=200 | grep -i device-mapper

# 3. on the node itself
lsmod | grep -E 'dm_snapshot|dm_thin_pool'
```

The fix is on the nodes (`modprobe dm_snapshot`, made persistent), not in any Kubernetes object.

## Deduced from the rule — never tested here

:::caution[Nothing below this line was measured]
Everything from here on is **deduced** from the rule at the top of this page plus general
knowledge of the drivers. None of it ran on our bench. It is offered to help you form an
expectation and know what to check — not as a compatibility statement. Verify with
[`csi-probe.sh`](#qualify-a-storageclass-for-real) on your own cluster before relying on any row.

For every entry, the deciding question is unchanged: **can the driver create a volume from a
snapshot, and at what cost?**
:::

### Arrays and proprietary storage

All of these are `csi-generic` by the rule: their provisioner names contain no `.cephfs.csi.`.

| Driver | Provisioner | Expectation |
|---|---|---|
| NetApp Trident | `csi.trident.netapp.io` | Should work. FlexClone makes create-from-snapshot near zero-copy. |
| Pure Storage | `csi.purestorage.com` | Should work. |
| Portworx | `pxd.portworx.com` | Should work. |
| Dell PowerStore / PowerFlex / PowerMax / Unity | Dell CSI drivers | Should work. |
| HPE | `csi.hpe.com` | Should work. |
| IBM Storage Scale | IBM Spectrum Scale CSI | Fileset-backed volumes only. |
| vSphere CSI | `csi.vsphere.vmware.com` | **Block volumes only.** vSAN File volumes have no snapshot support. Also: creating a volume *from* a snapshot arrived considerably later than snapshot creation itself — a driver version that snapshots is not necessarily one that can restore into a temporary PVC. |

### Clouds

| Storage | Provisioner | Expectation |
|---|---|---|
| AWS EBS | `ebs.csi.aws.com` | Works, but the temporary volume is **lazily hydrated from S3**, and a backup reads the whole volume. This is the worst case for lazy hydration: every block is faulted in on first read. |
| AWS EFS | `efs.csi.aws.com` | No `VolumeSnapshot` → **Skipped**. |
| AWS FSx (Lustre, OpenZFS) | FSx CSI drivers | No `VolumeSnapshot` → **Skipped**. |
| GCP PD / Hyperdisk | `pd.csi.storage.gke.io` | Should work. |
| GCP Filestore | `filestore.csi.storage.gke.io` | Supported, but restoring creates a **new Filestore instance** (1 TiB minimum) per volume and per backup. Technically functional, economically absurd. |
| Azure Disk | `disk.csi.azure.com` | Should work. |
| Azure File | `file.csi.azure.com` | Snapshots work, but creating a share from a snapshot is a **full copy**. |
| Azure Blob | `blob.csi.azure.com` | No `VolumeSnapshot` → **Skipped**. |
| OpenStack Cinder | `cinder.csi.openstack.org` | Should work. |
| OpenStack Manila | `manila.csi.openstack.org` | Depends entirely on the share backend. |
| DigitalOcean, Scaleway, OCI Block, IBM VPC Block, Alibaba Disk | respective CSI drivers | Should work. |
| OCI File Storage | `fss.csi.oraclecloud.com` | Probably **Skipped**. |

### Other open-source storage

| Storage | Expectation |
|---|---|
| Mayastor (OpenEBS Replicated) | Snapshot support is recent — verify against your version. |
| democratic-csi / TrueNAS | ZFS underneath, so snapshots should be very cheap. |
| JuiceFS | No `VolumeSnapshot` → **Skipped**. |
| SeaweedFS CSI | No `VolumeSnapshot` → **Skipped**. |
| `nfs-subdir-external-provisioner` | **Not a CSI driver at all.** No `VolumeSnapshotClass` can point at it → **Skipped**. |

## Known limitations

### `volumeMode: Block`

**Restore refuses it explicitly.** A `volumeMode: Block` target yields
`rexposer.ErrBlockUnsupported`, and the volume is reported failed with reason
`RestoreBlockUnsupported` (`internal/rexposer/rexposer.go`,
`internal/controller/restore_engine.go`). restic restores files into a filesystem; a raw device
has none.

**Backup does not preserve it.** The temporary PVC is built without a `volumeMode` field at all
(`newTempPVCFromSnapshot`, `internal/exposer/snapshot.go`), so it defaults to `Filesystem` — from
a snapshot of a raw device. `internal/exposer` never reads or sets `VolumeMode` anywhere.

This is a gap between the specification and the code: ADR 0003 says block PVCs are "exposed
identically" with the mover receiving a `volumeDevices` path, and the code does not do that.
Treat `volumeMode: Block` as unsupported end to end, and note that the backup side fails less
clearly than the restore side.

### The snapshot controller must be installed

Without the `snapshot.storage.k8s.io/v1` CRDs and the external-snapshotter controller, there are
no `VolumeSnapshotClass` objects to match, so **every** volume is `Skipped` — including on
perfectly healthy Ceph. See [Requirements](/CrystalBackup/docs/start/requirements/).

### There are no configuration knobs

ADR 0003 describes two operator settings, `exposure.rbdDirect` and
`exposure.readOnlyManyStorageClasses`. **Neither is implemented.** Both strings appear only in
`spec/`; they exist in no Go file, no CRD field and no Helm value. There is no supported way to
force a different exposer, to allow-list a StorageClass for a `ReadOnlyMany` temporary PVC, or to
enable the `rook-rbd-direct` path.

The selection described at the top of this page is the whole of it.

## Restore is more permissive than backup

Backup needs a snapshot. **Restore does not.**

The `pvc-transplant` mechanism ([ADR 0016](https://github.com/CrystalBackup/CrystalBackup/blob/main/spec/adr/0016-restore-execution-and-target-exposure.md))
provisions an ordinary temporary PVC in `crystal-backup-system`, lets the mover be its first
consumer, and then transplants the underlying PV into the target namespace. It requires only a
StorageClass that can provision a volume — no `VolumeSnapshotClass`, no snapshot capability.

Consequences:

- Data backed up from Ceph can be restored onto `local-path`, via `storageClassMapping`.
- A `Skipped` StorageClass is a backup-side limitation only. Nothing prevents it from being a
  *restore target*.
- `pv-twin` (restoring into an existing bound PVC) has the same property.

See [Restoring](/CrystalBackup/docs/guides/restore/).

## The target side is S3-only

Storage compatibility above is about the *source*. The **destination** — where the restic
repository lives — is a separate and much narrower question.

A `BackupLocation` and a `ClusterBackupLocation` each carry exactly one storage field, `s3`
(`api/v1alpha1/backuplocation_types.go`, `api/v1alpha1/clusterbackuplocation_types.go`, typed
`S3Spec` with a required `endpoint`). The only repository URL Crystal Backup ever generates is
`s3:` (`internal/restic/restic.go`). restic itself can address GCS, Azure Blob, B2, SFTP and REST
repositories; **Crystal Backup does not**.

This applies to `ExternalSync` too: its `destinationLocationRef` names another `BackupLocation`,
which is itself S3-only. There is no path to a non-S3 destination.

Google Cloud Storage and Azure Blob both offer S3-compatible or S3-gateway options — those are
the way in, and they are your responsibility to validate, not something this project tests.

## Check your own cluster

### In 30 seconds

```bash
# every StorageClass and its provisioner
kubectl get storageclass \
  -o custom-columns=NAME:.metadata.name,PROVISIONER:.provisioner

# every VolumeSnapshotClass and its driver
kubectl get volumesnapshotclass \
  -o custom-columns=NAME:.metadata.name,DRIVER:.driver
```

Compare the two lists by exact string. **Every provisioner with no `driver` equal to it means
volumes on that StorageClass will be `Skipped` with `CSISnapshotUnsupported`** — manifests
captured, data not.

An empty second list (or `error: the server doesn't have a resource type
"volumesnapshotclass"`) means the snapshot controller is missing and nothing will be backed up at
all.

The [preflight script](/CrystalBackup/docs/start/requirements/#check-your-cluster-before-you-install)
does this same resolution and additionally counts how many PVCs sit on each StorageClass. It
creates nothing.

### Qualify a StorageClass for real

`csi-probe.sh` goes further than resolution: it actually runs the exposure path — including the
cross-namespace static re-bind, which is the part most drivers break on — and verifies a checksum
of the data read back.

```bash
test/crucible/scripts/csi-probe.sh <storageclass> --copy-probe
```

It needs `bash` ≥ 4, `kubectl` and `jq`; nothing is installed cluster-side. It creates two
throwaway namespaces and its own `VolumeSnapshotContent` objects, cleans up on exit including on
failure, and writes a JSON artifact to `$CRUCIBLE_ARTIFACTS/csi-probe-<storageclass>.json`. The
only pre-existing object it touches is the origin `VolumeSnapshotContent`, whose `deletionPolicy`
it flips to `Retain` for the handover and restores afterwards — exactly as the operator does.

`--copy-probe` re-runs the whole flow with ten times the data so the provisioning times can be
compared. Verdicts: `COMPATIBLE` (0), `SKIPPED` (0), `COMPATIBLE_COPIE_COMPLETE` (4),
`INCOMPATIBLE` (1), `PROBE_ERROR` (3) — the last means *the probe* could not answer and is never
the driver's fault.
