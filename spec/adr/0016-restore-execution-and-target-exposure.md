# ADR 0016 — Restore execution: movers stay in the operator namespace, PV-level target exposure (transplant & twin), server-side snapshot resolution

Status: **Accepted** (2026-07-18, M2 kickoff)

## Context

M2 delivers restore ([90-roadmap.md](../90-roadmap.md)): a namespaced `Restore` consumes a
`Backup` in its own namespace, a cluster-scoped `ClusterRestore` addresses a repo coordinate,
and a `crystal-mover` Job runs `restic restore` against the **target PVC mounted read-write**
([01-architecture.md §6](../01-architecture.md)). Three constraints collide:

1. **The repository key must never enter a user namespace.** A cluster-DR restore needs the
   platform DEK as the restic password; invariant I3
   ([03-security-and-tenancy.md §3](../03-security-and-tenancy.md)) confines that DEK — and
   every per-Job projected Secret carrying it — to `crystal-backup-system`. A mover pod's
   projected Secret lives in the pod's own namespace, so a restore mover in a tenant
   namespace would hand the tenant the key to every namespace's DR data.
2. **A pod can only mount a PVC of its own namespace.** The restore target PVC lives in the
   tenant (or `ClusterRestore` target) namespace; a mover confined to `crystal-backup-system`
   cannot mount it directly. Backup solved the read direction with the cluster-scoped
   `VolumeSnapshotContent` re-bind ([adr/0003](0003-snapshot-exposure-csi-generic-first.md));
   restore needs the write-direction equivalent.
3. **Restore must be generic (R6)** — no Ceph dependency, and it must work on
   `WaitForFirstConsumer` StorageClasses, where a PVC binds no volume until its first pod.

I5 additionally pins the allowed operator-created objects in user namespaces (VolumeSnapshots,
transient manifest RoleBindings, **restored PVCs/manifests**, projected `Backup`s) — mover pods
are deliberately not on that list, and the PSA posture ("Crystal Backup schedules no pods in
user namespaces") is a hardening guarantee worth keeping.

## Decision

### 1. Restore movers run ONLY in `crystal-backup-system`

Same image, same wire protocol (`internal/mover`), same reader semantics as backup movers
(a `restic restore` holds a non-exclusive repo lock: restore movers count in the
mover-quiescence census and are gated by `queue.QuiescenceRequired` exactly like backup
movers — [adr/0015](0015-per-repository-exclusive-queue-serialization.md)). The restore
container adds the capabilities metadata fidelity requires
(`CHOWN, DAC_OVERRIDE, FOWNER, SETFCAP, MKNOD` — [03-security-and-tenancy.md §6](../03-security-and-tenancy.md));
everything else (root uid, seccomp, read-only rootfs, no SA token) is unchanged.

### 2. PV-level target exposure, chosen by the target PVC's state

The bridge between the operator-namespace mover and the target-namespace PVC is the
**cluster-scoped PersistentVolume**, in two mechanisms behind one seam
(`internal/rexposer`, the write-direction sibling of `internal/exposer`):

- **`pvc-transplant`** — the target PVC **does not exist** (typical `ClusterRestore` into a
  recreated namespace; `Restore` of a deleted PVC). The operator provisions a **temp PVC in
  `crystal-backup-system`** (capacity/StorageClass/accessModes from the snapshot's PVC-meta
  tags, StorageClass run through `storageClassMapping`); the mover is its **first consumer**,
  so `WaitForFirstConsumer` classes bind naturally. After a successful restore the volume is
  **transplanted**: reclaimPolicy→`Retain`, temp PVC deleted, `claimRef` re-pointed, final
  PVC created **pre-bound** (`spec.volumeName`) in the target namespace under the original
  PVC name, reclaimPolicy restored to the StorageClass's policy. The restored PVC carries the
  informational annotation `crystalbackup.io/restored-from: <run>` and **none** of the
  operator's reaper-selected labels (a restored PVC is the user's object; the reaper must
  never garbage-collect it).
- **`pv-twin`** — the target PVC **exists and is bound** (in-place `Overwrite`, or `Recreate`
  into a kept PVC). The operator creates a **twin PV**: a second PV object cloning the bound
  PV's CSI source (driver, `volumeHandle`, attributes, secret refs, `nodeAffinity`,
  volumeMode) with **reclaimPolicy `Retain` always** (deleting the twin must delete the PV
  *object*, never the underlying volume), pre-bound to a temp PVC in
  `crystal-backup-system`. The mover mounts that temp PVC read-write; the kubelet stages CSI
  volumes by `(driver, volumeHandle)`, so on one node the twin resolves to the same staged
  filesystem. **Attach conflict rule**: if a `VolumeAttachment` exists for the underlying
  volume on exactly one node, the mover Job is pinned to that node (`nodeName`) so an RWO
  volume is never asked to attach twice; multi-attach (RWX) volumes need no pin. Cleanup
  (success or failure) deletes the temp PVC then the twin PV object.
- An existing but **unbound** target PVC (a `WaitForFirstConsumer` claim no pod ever used)
  holds no data: the operator deletes it and follows `pvc-transplant`, recreating it
  pre-bound with the spec (capacity/class/modes) copied from the deleted claim. This is
  destructive only in the letter — an unbound claim has no volume — and sits behind the same
  R23 confirmation as every Recreate/Overwrite.

Mode is orthogonal to mechanism: `Recreate` vs `Overwrite` select restic flags
(`--overwrite always --delete` vs `--overwrite always`), never the exposure path.

### 3. Snapshot resolution is server-side; cluster-origin restores resolve through the tag filter

The R2/R14 cornerstone is enforced **end-to-end at execution**, not only at admission:

- A `Restore` whose source `Backup` is `origin=cluster` is resolved by a repository listing
  scoped with the literal restic filter `--tag crystalbackup,namespace=<CR.metadata.namespace>`
  (AND-combined in one `--tag` flag) plus `run=<backup>`; **only snapshot IDs returned by
  that listing are ever passed to the restore mover**. The filter value derives from
  `metadata.namespace` alone — no user-writable spec field feeds it — so even a corrupted
  `Backup` projection cannot address another namespace's snapshots.
- A namespace-plane restore (`origin=namespace`) trusts the `Backup.status.volumes`
  snapshot IDs directly: the user's own repository holds only their namespace's data
  (isolation by construction, I2), so there is no cross-tenant surface to re-derive against
  and no listing Job to pay for.
- A `ClusterRestore` (admin) resolves its repo coordinate through the same listing path with
  `namespace=<spec.source.namespace>`; the admin's identity is the authorization (R14).

### 4. PVC-meta tags make volumes reconstructible from the repository alone

`ClusterRestore` must recreate PVCs when the source namespace — and its `Backup` CRs — are
gone. From M2 the backup mover stamps three additional, **informational** tags on every
`kind=data` snapshot: `pvcsize=<bytes>` (the PVC's requested capacity),
`pvcclass=<storageClassName>` (omitted when the PVC had none) and
`pvcmodes=<RWO[+ROX][+RWX][+RWOP]>` (sorted, `+`-joined). They extend the
[02-api.md tag table](../02-api.md#repository-layout--snapshot-identity) additively — the
identity tags and discovery grouping are untouched. For pre-M2 snapshots that lack them,
restore falls back to capacity = the snapshot's logical size rounded up to the next GiB
(minimum 1 GiB, +20% headroom), the target's default StorageClass, and `RWO` — documented,
best-effort, and always overridable by pre-creating the PVC before restoring into it.

## Consequences

### Positive

- **I3/I5 hold verbatim**: no mover pod, no key material, no projected Secret ever enters a
  user namespace; the only restore-created objects there are the restored PVCs themselves.
- **One uniform execution path** for both planes and both restore kinds (adr/0009's
  "uniform restore" goal); mode never forks the mechanism.
- **WFFC-safe by construction**: the mover is the first consumer of every volume it fills.
- **DR-complete**: with PVC-meta tags, a repository plus a KEK is sufficient to rebuild
  namespaces with correctly-sized PVCs — no surviving CR needed (R26).
- The tag filter is enforced in the **restic invocation itself** for cluster-origin
  restores — defense in depth beyond admission and RBAC (adr/0010's "controllers re-derive").

### Negative / costs

- **Transplant topology**: a volume provisioned for the mover's node inherits that
  provisioning topology (zonal CSIs); app pods schedule within the PV's nodeAffinity, as with
  any pre-provisioned volume. Cluster-wide storage (Ceph, Longhorn) is unaffected.
- **Twin-PV is a controlled aliasing of a volumeHandle.** Two PV objects reference one CSI
  volume for the duration of a restore. The same-node pin plus staging-by-volumeHandle keeps
  a single filesystem instance; restoring into a PVC that a live workload is actively
  writing remains discouraged (documented) — crash-consistent semantics are the user's
  responsibility, exactly as with any in-place restore tool.
- **Cluster-origin restores pay one listing Job** (~seconds) before data moves; the price of
  end-to-end mediation.
- A failure between transplant steps leaves a `Retain`ed PV pending the next reconcile; the
  steps are idempotent and re-derivable from live objects (deterministic names), and the
  orphan reaper backstops the temp objects.

### Risks & mitigations

| Risk | Mitigation |
|---|---|
| Twin PV deletion triggers CSI `DeleteVolume` on the real volume | Twin reclaimPolicy is **always `Retain`**; asserted by unit test; cleanup deletes the PV object only |
| RWO volume attached on node A, mover scheduled on node B | `VolumeAttachment` census → `nodeName` pin; attach-after-check races surface as attach timeout, never corruption (CSI refuses the second attach) |
| Transplant interrupted mid-handover (operator restart) | Every step keyed on deterministic names; reconcile re-derives the step from live object state (final PVC exists→done; PV Released→re-point; temp PVC exists→wait mover) |
| `restic restore` into a live-mounted volume under active writers | Documented limitation (as for every in-place restore engine); `Recreate` exact-match plus scale-down is the recommended drill |
| PVC-meta tags absent (pre-M2 snapshots) | Deterministic fallback (rounded size, default class, RWO) + pre-create-the-PVC override; fallback covered by unit tests |

## Alternatives considered

1. **Run restore movers in the target namespace** (K8up-style). Mounts the PVC trivially and
   is WFFC-safe — but the projected repo-credentials Secret would live in the user
   namespace. Fatal for the cluster plane (the platform DEK opens every namespace, violating
   I3), and even namespace-plane-only use would fork the execution path per plane and break
   the "no backup pods in user namespaces" PSA guarantee. Rejected.
2. **Restore into a staging PVC and copy across namespaces at the file level** (operator- or
   Job-driven `cp` through two mounts). Requires *some* pod that mounts both — which is the
   same cross-namespace problem again, plus a full second data copy. Rejected.
3. **Node-agent DaemonSet with hostPath access** (Velero's model). Solves every mount problem
   with one privileged daemon on every node — the exact privilege posture this project
   refuses (no hostPath, no privileged pods, movers confined to one namespace). Rejected.
4. **Always transplant (never twin): delete-and-recreate every target PVC.** Simpler, but
   `Overwrite` semantics ("keep files absent from the backup") are impossible once the volume
   is discarded, and recreating the PVC object breaks volume identity (PV name, uid) for
   workloads that reference it. Rejected.
5. **Ephemeral CSI volumes or cross-namespace data sources** (`AnyVolumeDataSource`,
   `CrossNamespaceVolumeDataSource`). The cross-namespace data-source API remains alpha,
   gated, and populator-dependent — unusable as the generic v1 path (R6). Revisit trigger
   below.

## Revisit triggers

- `CrossNamespaceVolumeDataSource` (or an equivalent) reaches GA on the supported Kubernetes
  floor → re-evaluate replacing the twin/transplant pair with native cross-namespace
  provisioning.
- A CSI driver in real use stages by PV name rather than volumeHandle (breaking same-node
  twin semantics) → gate the twin on a driver allowlist and fall back to documented
  scale-down + transplant.
- Restore volume throughput becomes a bottleneck (single mover per PVC) → parallel per-PVC
  movers need no design change (they already parallelize across PVCs like backup).
- Block-mode (`volumeMode: Block`) restore demand: restic restores files, not raw devices;
  block PVCs are surfaced as per-volume failures (`RestoreBlockUnsupported`) in v1.
