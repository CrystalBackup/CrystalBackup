---
title: The snapshot feasibility probe
description: When to run snapshot-probe.sh, exactly what it creates in your cluster, why it leaves the wreckage behind when it fails, and how to read its verdicts.
sidebar:
  order: 3
---

`preflight.sh` is read-only. It creates nothing, and that promise is why you were willing to
point it at production. It is also why there is a question it cannot answer, and this page is
about that question and the script that does answer it.

## The gap

Crystal Backup reads your data out of a **CSI snapshot**, not out of the live volume. So the
chain that has to work is:

1. take a `VolumeSnapshot` of the PVC
2. provision a temporary PVC **from that snapshot**
3. **mount** that temporary PVC on a node
4. **read** the data off it

`preflight.sh` can establish that step 1 is possible: a `VolumeSnapshotClass` exists whose
`.driver` matches your StorageClass's provisioner, so a snapshot can be *requested*. That is
snapshot **availability**.

It cannot establish steps 2 to 4. Those are snapshot **usability**, and nothing in the Kubernetes
API reports on them. The only way to know is to do it.

### This is not a hypothetical distinction

An administrator installed 0.6.2 on RKE2 with Rook-Ceph. Their `ceph-block-rwo` StorageClass had
a matching `ceph-block` VolumeSnapshotClass. Snapshots were created, and created correctly.
Clones were provisioned from them, and provisioned correctly. Every backup then hung, for
thirty-six hours, on this:

```
rbd: map failed with error (exit status 22) ... rbd: sysfs write failed
rbd: map failed: (22) Invalid argument
```

The clone carried `op_features: clone-child` — an RBD format-v2 clone — and the krbd client on
their nodes refused to map it. Nothing about that is visible from the Kubernetes API. Nothing
about it is visible without Ceph credentials. It is observable **only by actually restoring a
snapshot and mounting the result**, which is exactly what the probe does, in about two minutes.

## What preflight now says instead

A StorageClass whose snapshot class resolves is no longer reported as clean. It appears as
`NOT ASSESSED` in a `USABILITY` column, it is counted as a reservation rather than a pass, and
it drags the whole run into exit code **1**:

```
WHAT WOULD BE BACKED UP
  STORAGECLASS           PROVISIONER                    SNAPSHOT CLASS   USABILITY      PVCs
  ceph-block-rwo (default) rook-ceph.rbd.csi.ceph.com     ceph-block       NOT ASSESSED   2 in 2 ns
  local-path             rancher.io/local-path          none             DATA SKIPPED   1 in 1 ns
```

There is no input that makes that column say anything better. A read-only script has no way to
reach the answer, and "we could not check" is not allowed to round up to green.

In `--json` (schema `crystalbackup.preflight/v2`) the same fact appears as
`"usability": "NOT_ASSESSED"` and `"dataBackedUp": null` — never `true`.

## The probe's contract

`snapshot-probe.sh` is the opposite of `preflight.sh` and states so in its own header. It
**creates objects in your cluster**. Precisely these:

| # | Object | Name | Notes |
|---|--------|------|-------|
| — | Namespace | `crystalbackup-probe-<runid>` | created by the script, deleted by it. `--namespace NS` uses one of yours instead, and then the namespace itself is never touched |
| 1 | PersistentVolumeClaim | `cbprobe-<runid>-<n>-src` | `ReadWriteOnce`, on the StorageClass under test, `--size` (default `1Gi`) |
| 2 | Pod | `cbprobe-<runid>-<n>-write` | writes a known byte pattern, calls `sync`, exits |
| 3 | VolumeSnapshot | `cbprobe-<runid>-<n>-snap` | of object 1, using the VolumeSnapshotClass the **operator itself** would resolve |
| 4 | PersistentVolumeClaim | `cbprobe-<runid>-<n>-restored` | `dataSource` is object 3 |
| 5 | Pod | `cbprobe-<runid>-<n>-read` | mounts object 4 **read-only**, reads the pattern back, re-derives it from the same seed, compares |

Nothing is ever created outside that one namespace, and the script modifies no object it did not
create.

Two details are deliberate rather than incidental:

- **It picks the same VolumeSnapshotClass the operator would.** When several classes share a
  driver, the operator sorts them byte-wise and takes the first; the probe reproduces that. A
  probe that tested a different class would be answering a question nobody asked.
- **The restored PVC has the same access mode the operator's exposer uses** — `ReadWriteOnce`
  normally, `ReadOnlyMany` on a CephFS driver. Both rules come from the same block generated out
  of `internal/exposer` that `preflight.sh` carries, and CI fails if either drifts.

### Reading the data back is the point

Mounting proves the map. **Reading proves the data path.** A clone that mounts cleanly and comes
back empty is worse than one that refuses to mount, because a mount-only test reports it as a
pass and you find out at restore time. So the reader pod re-derives the expected bytes from the
run's seed, compares digests in the pod, and the script cross-checks that digest against what the
writer pod reported writing — two independent comparisons.

### What it does *not* do

It does not install Crystal Backup, read any of its objects, or need it to be present. It is a
smoke test of your storage stack, reduced from the restore-fidelity gate this project runs
against real Rook-Ceph (`test/crucible`, M6): the same chain, with one small file instead of an
engineered corpus, and without the operator in the path. A green probe is **not** a promise that
every xattr, ACL and sparse hole survives a real restore. It is the answer to "does this
cluster's snapshot → restore → mount → read path work at all", which is the question that was
open.

It prints node kernel versions **only inside a failure report**, as context. It never checks
them. No reliable kernel threshold for this failure class is known to this project, and a
heuristic that answered "probably fine" would be worse than saying nothing.

## Cleanup, and why failure leaves a mess

**On a class that comes back FEASIBLE**, everything that class created is deleted, and the
removal is then *verified* by polling until each object is actually gone. A delete the API
accepted is not a delete that happened; a stuck finalizer is worth naming, so it is named and it
becomes a reservation.

**On anything else, nothing is deleted. Not one object.** The pod that would not mount, its
events, its volume — that is the evidence, it is not reproducible once deleted, and in the
incident above it took thirty-six hours to obtain. The script prints where the objects are and
the single command that removes them when you are done:

```bash
kubectl delete namespace crystalbackup-probe-<runid>
```

`--keep` leaves everything behind even on success.

## When to run it

- **Before you install**, on any cluster where `preflight.sh` exits 1 with a `NOT ASSESSED`
  usability finding — which is every cluster that has a usable StorageClass.
- **After a storage upgrade**: a Ceph major version, a CSI driver bump, a node OS or kernel roll.
  The failure above is a property of the pairing between the driver and the node, so changing
  either invalidates the earlier answer.
- **When you add a StorageClass**, with `--storage-class NAME`.
- **Not on a schedule.** It creates and destroys volumes; it is a checkpoint, not a monitor.
  For continuous assurance use the alerting rules and a restore drill instead.

## Running it

```bash
BASE=https://crystalbackup.github.io/CrystalBackup
curl -fsSLO "$BASE/snapshot-probe.sh"
curl -fsSLO "$BASE/snapshot-probe.sh.sha256"
curl -fsSLO "$BASE/snapshot-probe.sh.cosign.bundle"

# 1. the checksum
sha256sum -c snapshot-probe.sh.sha256     # macOS: shasum -a 256 -c snapshot-probe.sh.sha256

# 2. the signature — keyless, the same Sigstore trust root as our container images
cosign verify-blob snapshot-probe.sh \
  --bundle snapshot-probe.sh.cosign.bundle \
  --certificate-identity-regexp '^https://github\.com/CrystalBackup/CrystalBackup/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# 3. read it. This one creates objects in your cluster; do not take that on trust from a URL.
less snapshot-probe.sh

# 4. see exactly what it would create, without creating any of it
sh snapshot-probe.sh --dry-run

# 5. run it
sh snapshot-probe.sh
```

There is deliberately **no `curl … | sh` one-liner** on this page. `preflight.sh` has one because
it creates nothing. This script does.

Useful flags: `--storage-class NAME` (repeatable) to narrow the run, `--namespace NS` to work in
a namespace you already have, `--size` if your StorageClass has a minimum above `1Gi`,
`--timeout SECONDS` (default 300) if your provisioner is slow — Longhorn has needed well over a
minute — `--image` if `busybox:1.36` is not reachable from your cluster, `--json`, `--no-color`,
`--keep`, `--help`.

## Reading the verdicts

Per StorageClass, the vocabulary is three words and no more.

### FEASIBLE

```
ceph-block-rwo: snapshot OK · restore OK · mount OK · read OK
  → a snapshot of this StorageClass can be restored, mounted and read back exactly
```

All four links held, and the bytes came back identical. Objects removed, removal verified.

### NOT FEASIBLE

```
ceph-block-rwo: snapshot OK · restore OK · MOUNT FAILED
  rbd: map failed: (22) Invalid argument
  → backups of this StorageClass cannot work on this cluster
```

Something in the chain broke, and the chain shows where. The line under it is the **most recent
Warning event on the failing pod, verbatim** — that string is what you send to your storage
vendor, and it is the whole diagnosis. Below it the script prints the node kernels as context and
tells you the objects are still there.

Do not install and hope. A backup of this StorageClass will hang in exposure.

### NOT ASSESSED

```
ceph-block-rwo: NOT ASSESSED
  the VolumeSnapshot did not become ready within 300s and reported no error.
  → NOT ASSESSED. This is not a pass: the question is still open.
```

The probe did not get an answer. A wait expired with no Warning event to explain it, a pod stayed
unscheduled, the source volume never bound, or the class has no VolumeSnapshotClass and there was
nothing to probe in the first place. Nothing is claimed either way — raise `--timeout`, fix the
obstacle, and run it again. Objects are left in place here too.

### Exit codes

The same discipline as `preflight.sh`: never green on an absence.

| Code | Meaning |
|------|---------|
| `0` | **FEASIBLE** — every class assessed went snapshot → restore → mount → read and gave the pattern back byte for byte |
| `1` | **FEASIBLE, RESERVES** — at least one class could not be assessed, or a cleanup could not be verified. Nothing failed, and nothing is claimed |
| `2` | **NOT FEASIBLE** — at least one class broke the chain |
| `3` | **NOT ASSESSED** — the probe could not run at all, or it was a `--dry-run` |

A `--dry-run` exits **3**, not 0. It assessed nothing, and a dry run that exited 0 would be a
green on an absence.

## See also

- [Requirements](/CrystalBackup/docs/start/requirements/) — `preflight.sh`, which you run first
- [Storage compatibility](/CrystalBackup/docs/reference/storage-compatibility/) — the rule that
  decides whether a volume's data is backed up at all
- [Diagnosis](/CrystalBackup/docs/operations/troubleshooting/) — what a backup stuck in exposure
  looks like from the operator's side
