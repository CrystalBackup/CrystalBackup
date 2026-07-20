# Manifest backup & restore (R15)

Status: draft v2 (aligned with the two-plane cascade + Restore selection model,
2026-07-12). Implements R15; milestone [M3](90-roadmap.md). Naming & field contract:
[02-api.md](02-api.md). Prior art: `kubectl-neat` (itaysk/kubectl-neat) for field
stripping, Velero `restorePriorities` for apply ordering, Velero `--preserve-nodeports`
for the NodePort decision (here: always preserve).

## 1. Scope

Every `Backup` run with `includeManifests: true` (default) captures the **namespaced**
Kubernetes resources of the backup's namespace, sanitized for cross-cluster restore, as
one dedicated restic snapshot: path `/manifests/<namespace>`, tag `kind=manifests`,
alongside the volume snapshots' `crystalbackup`/`tenant`/`namespace`/`schedule`/`run`
tags and `host=<clusterID>`
([02-api.md § Repository layout](02-api.md#repository-layout--snapshot-identity)).
The same capture serves both planes: a namespace-plane `BackupSchedule` and a
cluster-plane `ClusterBackup` both converge on a per-namespace `Backup`, the single unit
of execution. Cluster-scoped resources (CRDs, ClusterRoles, PVs, VolumeSnapshotContents,
…) are **out of scope** here; they belong to platform DR (`ClusterBackupSchedule`, M9,
R22).

## 2. Dump pipeline

### 2.1 Discovery-driven enumeration

The dump never hardcodes a kind list. Per backup run:

1. Aggregated server-side discovery (`/api`, `/apis`).
2. Keep resources where `namespaced == true`, verbs include `list` and `get`, and the
   name contains no `/` (subresources such as `pods/log` are skipped).
3. For each API group, use only the server's **preferred version**. The captured
   `apiVersion` is stored verbatim in each manifest; no version conversion is performed
   at backup or restore time (the target API server converts if it serves that version).
4. List with pagination (`limit=500`, continue tokens). The dump is a single pass; it is
   not transactionally consistent across kinds (same guarantee class as Velero).
5. A kind that fails to list (e.g. RBAC 403, aggregated API down) is recorded as a
   warning in `index.json` and in a `ManifestsComplete=False` condition on the `Backup`;
   the dump continues. `Backup.status.manifests.resourceCount` (02-api.md) reflects the
   objects actually captured.

### 2.2 Default exclusion list

Excluded objects are transient, controller-owned, or rebuilt by the target cluster.
The list is compiled into the sanitizer (versioned with the mover image), not user
configurable in v1.

| Rule | Excluded | Rationale |
|---|---|---|
| E1 | `Pod` with a `controller: true` ownerReference | Recreated by its workload controller. Standalone (naked) Pods are kept. |
| E2 | `ReplicaSet` owned (`controller: true`) by a `Deployment` | Recreated by the Deployment. Standalone ReplicaSets are kept. |
| E3 | `Job` owned (`controller: true`) by a `CronJob` | Recreated by the CronJob. Standalone Jobs are kept. |
| E4 | `EndpointSlice` labeled `endpointslice.kubernetes.io/managed-by: endpointslice-controller.k8s.io`; `Endpoints` whose same-name `Service` has a `spec.selector` | Rebuilt by the control plane from Services. Manually-managed `Endpoints`/`EndpointSlices` (selectorless Services) are **kept** — user intent. |
| E5 | `Event` (core and `events.k8s.io`) | Operational noise. |
| E6 | `Lease` (`coordination.k8s.io`) | Leader-election/heartbeat state. |
| E7 | `Secret` of type `kubernetes.io/service-account-token` | Bound to a UID of the source cluster; re-minted on demand. |
| E8 | cert-manager transients: `Order`, `Challenge` (`acme.cert-manager.io`), `CertificateRequest` (`cert-manager.io`), and any object labeled `acme.cert-manager.io/http01-solver=true` (solver Pods/Services/Ingresses) | Reissued from the kept `Certificate` (+ its kept TLS `Secret`). |
| E9 | `VolumeSnapshot` (`snapshot.storage.k8s.io`) | Bound to cluster-local VolumeSnapshotContents; meaningless cross-cluster. Includes the transient snapshots this operator creates (PVC dataSource path, §3 of 01-architecture.md). |
| E10 | Own operational CRs `Backup` and `Restore` (`crystalbackup.io`) | Run records / operation records and discovery projections — not desired state. **`BackupSchedule` and `BackupLocation` are kept** (user configuration). |
| E11 | ConfigMap `kube-root-ca.crt` | Auto-created in every namespace by the control plane. |

### 2.3 Execution model

Dump + sanitize + upload run in a dedicated **manifest mover Job** in
`crystal-backup-system` (same mover image, subcommand `manifests-backup`), because the
operator never touches backup data bytes (01-architecture.md §1). The Job runs under
its own ServiceAccount **`crystal-manifest-mover`** (token automounted) — distinct from
the zero-RBAC data-mover SA `crystal-mover`. API read access is granted through a
transient namespace-scoped `RoleBinding` in the tenant namespace, created by the operator
before the Job and deleted at Job completion, binding the ClusterRole
`crystal-manifest-reader` (get/list on all resources) to `crystal-manifest-mover`.
[03-security-and-tenancy.md](03-security-and-tenancy.md) I6 records this as the **sole
exception** to the zero-API mover invariant; the NetworkPolicy grants API-server egress
to `crystal-manifest-mover` only — data movers (`crystal-mover`) keep zero API access and
S3-only egress. The sanitizer itself is a plain Go package (`internal/sanitize`) shared
by the mover, the restore path, and the `crystalctl` CLI, and covered by the golden
corpus (§5).

## 3. Storage format in the restic snapshot

```
/manifests/<namespace>/index.json
/manifests/<namespace>/<group>/<Kind>/<name>.yaml
```

- The snapshot's `paths` root is `/manifests/<namespace>` (mirrors the data path
  `/data/<namespace>/<pvc>`). `<group>` = API group; the legacy core group is stored as
  `core`. `<Kind>` = PascalCase kind. Examples:
  `/manifests/<namespace>/core/Service/web.yaml`,
  `/manifests/<namespace>/apps/Deployment/web.yaml`,
  `/manifests/<namespace>/postgresql.cnpg.io/Cluster/db.yaml`.
  Kubernetes object names are DNS-safe, hence valid file names.
- **One YAML document per file**, no `---` separator, trailing newline, UTF-8.
- **Deterministic serialization** for dedup-friendliness: objects are dumped as
  unstructured maps and marshaled with `sigs.k8s.io/yaml` (JSON round-trip ⇒
  lexicographically sorted map keys). Combined with the sanitization (all churn fields
  such as `resourceVersion` removed), an unchanged resource produces a byte-identical
  file across backups, so restic's content-defined dedup stores it once.
- `index.json` (deterministically sorted): `formatVersion: 1`, `clusterID`, `namespace`,
  `backupName` (= the `run` tag), `kubernetesVersion`, `capturedAt`, per-resource entries
  `{group, version, kind, name}`, and the list of enumeration warnings (§2.1.5).
- Reversibility (R8): manifests are ordinary files in a standard restic snapshot —
  `restic -r s3:… dump latest --archive tar /manifests/<namespace> > manifests.tar` works
  with upstream restic (streamed, reads only the needed blobs).

## 4. Sanitization rules

Applied at **backup** time (the stored manifest is already clean; restore-time
transformations are limited to storageClassMapping, §5.3). `kubectl-neat`-like, but
implemented in-house: neat's behavior is a reference, not a dependency.

### 4.1 Generic rules (all kinds)

| # | Field | Action |
|---|---|---|
| S1 | `metadata.uid`, `metadata.resourceVersion`, `metadata.creationTimestamp`, `metadata.generation`, `metadata.selfLink` | Strip. |
| S2 | `metadata.managedFields` | Strip. |
| S3 | `status` (entire subtree) | Strip. |
| S4 | `metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"]` | Strip. |
| S5 | `metadata.ownerReferences` | Strip — **restored objects are unowned** (see §4.3). |
| S6 | `metadata.namespace` | Strip (reinjected at apply time; enables cross-namespace `ClusterRestore`). |
| S7 | Empty `metadata.annotations` / `metadata.labels` maps after stripping | Remove the empty map. |

### 4.2 Kind-specific rules

| # | Kind | Rule |
|---|---|---|
| S10 | `Service` | Strip `spec.clusterIP`, `spec.clusterIPs`, `spec.ipFamilies` — **except the literal value `None`, which is kept** (see below). **Preserve `spec.ports[].nodePort` and `spec.healthCheckNodePort`** (agreed decision; Velero only does this behind `--preserve-nodeports`). |
| S11 | `PersistentVolumeClaim` | Strip `spec.volumeName` (PV binding is cluster-local); strip `spec.dataSource` and `spec.dataSourceRef` (source objects excluded per E9; data comes back through the volume restore path); strip annotations `pv.kubernetes.io/bind-completed`, `pv.kubernetes.io/bound-by-controller`, `volume.beta.kubernetes.io/storage-provisioner`, `volume.kubernetes.io/storage-provisioner`, `volume.kubernetes.io/selected-node`; strip finalizer `kubernetes.io/pvc-protection` (re-added by the control plane). **Keep `spec.storageClassName`** — it is the input of storageClassMapping (§5.3). |
| S12 | `Pod` (standalone) | Strip `spec.nodeName`, `spec.priority` (derived from kept `priorityClassName`), deprecated `spec.serviceAccount` alias; strip projected token volumes named `kube-api-access-*` and their volumeMounts. |
| S13 | `Deployment` | Strip annotation `deployment.kubernetes.io/revision`. |

**Headless Services (`clusterIP: None`) — corrected 2026-07-20.** The rule above originally
stripped `spec.clusterIP` unconditionally. That is wrong for exactly one value: `None` is not
an address the API server allocated, it is how a headless Service is *declared*. Stripping it
restores the Service as an ordinary ClusterIP Service with a virtual IP, which silently
removes the per-pod DNS records (`<pod>.<svc>.<ns>.svc.cluster.local`) that every StatefulSet
governed by it depends on — so a clustered database comes back unable to resolve its own
members, failing at the application layer long after the restore reported success. The
sanitizer therefore keeps `clusterIP` and `clusterIPs` when their value is `None`, and strips
them otherwise. Found by the golden corpus (§6) before the rule ever ran on real data, which
is the corpus doing its job.

NodePort / LoadBalancer notes: a preserved `nodePort` may collide in the target cluster;
the apply then fails for that Service and is reported per-resource (§5.4) — it is never
silently renumbered. `status.loadBalancer` disappears with S3; the target cluster's LB
controller re-provisions (a kept `spec.loadBalancerIP` may be unsatisfiable there —
reported the same way).

### 4.3 ownerReferences policy

All ownerReferences are dropped (S5). Rationale: cross-cluster/cross-namespace UIDs are
meaningless, and a dangling ownerReference would get restored objects garbage-collected.
Consequences: controller-managed children are mostly excluded anyway (E1–E3); kept
objects that were owned (e.g. StatefulSet PVCs under a `persistentVolumeClaimRetentionPolicy`)
come back standalone, and controllers re-adopt via selectors where they support it. This
holds in both restore modes (§5.2).

### 4.4 Webhook-injected fields caveat

The dump reads the **live** object: API-server defaulting and mutating-webhook output
(injected sidecars, volumes, env — e.g. Istio/Linkerd) are captured **as-is**; there is
no un-injection in v1. Restoring into a cluster running the same mutating webhooks may
double-apply mutations (most injectors are idempotent via their own annotations);
restoring into a cluster without them keeps the injected state frozen at backup time.
This caveat must appear in the user documentation.

## 5. Restore

Manifest restore is the **`resources[]`** half of a `Restore`/`ClusterRestore` (the
`volumes[]` half restores PVC data — 01-architecture.md §6). It obeys the shared
**mode + selection** model
([02-api.md § Restore selection model](02-api.md#restore-selection-model)). A namespaced
`Restore` targets **its own namespace** (referencing a `Backup` in that namespace, no
`locationRef`); a `ClusterRestore` targets a **repo coordinate** and may create the target
namespace. The apply runs in a manifest-mover Job under the `crystal-manifest-mover`
ServiceAccount (subcommand `manifests-restore`), with a transient namespace-scoped
RoleBinding to the ClusterRole `crystal-manifest-writer` (mirror of §2.3).

### 5.1 Apply ordering

CRD installation is never part of a namespaced restore (§1). Phases, applied
sequentially; within a phase, resources sort by `(group, Kind, name)` for determinism:

1. `ServiceAccount`
2. `Role`, `RoleBinding`
3. `ConfigMap`, `Secret`
4. `PersistentVolumeClaim` (storageClassMapping applied; no wait for `Bound` —
   `WaitForFirstConsumer` classes bind when workloads start)
5. All remaining kinds not listed in another phase (custom resources, PDBs, HPAs, …)
6. Workloads: `Deployment`, `StatefulSet`, `DaemonSet`, `ReplicaSet`, `Job`, `CronJob`, `Pod`
7. `Service`, `Ingress`, `NetworkPolicy`

A custom resource whose CRD is absent in the target cluster fails with the server's
"no matches for kind" error, reported per-resource; the restore continues. In `Recreate`
mode, the delete of an existing object happens inside its own phase, immediately before
the recreate, so ordering is preserved.

### 5.2 Existing-resource behavior: mode

Conflicts on already-present objects are resolved by `spec.mode` (02-api.md), not by a
skip rule:

- **`Overwrite`** — each selected resource is **server-side applied** (create-or-update,
  field manager `crystalbackup-restore`). Objects present in the target but **absent from
  the backup are kept** (extras preserved) — additive recovery.
- **`Recreate`** — each selected resource that already exists is **deleted, then created**
  from the backup (a clean replace); absent ones are simply created.

Both modes **modify pre-existing objects** when the target already holds them, so they
require `spec.confirmation == <target namespace>` (R23). A restore that only creates —
e.g. `ClusterRestore` into a fresh namespace via `target.createNamespace` — is
non-destructive and needs no confirmation.

Caveats: a `Recreate` delete honors finalizers — an object stuck on a finalizer is
reported `Failed` (reason surfaced), never force-deleted, and the restore continues. It
deletes with **background** propagation: `foreground` works by adding a
`foregroundDeletion` finalizer only the garbage collector removes, which would make every
`Recreate` wait on a healthy GC controller, and ownerReferences are stripped at backup
anyway (§4.3) so the replaced objects have no dependents to wait for.

Under `Overwrite`, server-side apply resolves field ownership per Kubernetes conflict
rules — that is the *merge* semantics (list keys, atomic vs granular fields), not a refusal
to take ownership. The apply is **forced** (`force: true`). Without it, an apply conflicts
with whatever manager already owns each field — `kubectl`, Helm, a controller — which in a
real namespace is nearly every pre-existing object, so `Overwrite` would fail on precisely
what it exists to reconcile; the §6 e2e (a pre-created *drifted* ConfigMap that `Overwrite`
must SSA-merge) is unsatisfiable otherwise. Force takes ownership; it does not prune, so
"objects and fields present in the target but absent from the backup are kept" still holds.
ownerReferences were stripped at backup, so restored objects are unowned in either mode.

### 5.3 storageClassMapping

`ClusterRestore.spec.target.storageClassMapping` (`map[string]string`, source class →
target class). Application points:

1. **PVC manifests** during resource restore: `spec.storageClassName` is rewritten before
   apply. A mapping to `""` removes the field (target cluster's default class); unmapped
   classes pass through unchanged.
2. The **volume-data restore path** consults the *same* map when it must materialize a
   PVC (mode `Recreate`, fresh PVC), so a PVC's class is identical whether it arrives
   through its manifest or through the data path.

The map touches **PVCs only**. A restored cluster-scoped `PersistentVolume` keeps its captured
`spec.storageClassName` unchanged: a PV represents an already-provisioned volume, so rewriting
its class would rename a label without re-provisioning anything (adr/0011 §2). An explicit v1
non-goal, not an oversight.

Both points share one implementation, so mapping semantics cannot diverge. The namespaced
`Restore` restores into **its own namespace** and exposes no `storageClassMapping`
(same-cluster, same classes) — remapping is a `ClusterRestore` (cross-cluster /
cross-namespace) concern.

### 5.4 Selection, dry-run, status

- **Selection** — the manifest set is the `resources[]` list
  ([02-api.md § Restore selection model](02-api.md#restore-selection-model)): a manifest is
  restored iff **any** item matches (OR between items); within an item, the `selector`
  (labels) **and** `include` select, `exclude` removes. `include`/`exclude` entries are
  globs over the stored path `<group>/<Kind>[/<name>]` (§3; the core group may be written
  `core` or elided) — e.g. `apps/Deployment`, `apps/StatefulSet/postgres`, `Secret/db-creds`,
  `postgresql.cnpg.io/Cluster/db`, `apps/*`. Each list defaults
  **independently**: `resources` omitted ⇒ every manifest, whatever `volumes` says (so
  "both omitted ⇒ whole namespace" is the common case of that rule, not a special one);
  `resources: []` ⇒ no manifests. The CLI is the tie-breaker that fixes this reading —
  `--data-only` writes `resources: []` explicitly, which it would not need to do if
  omitting the field already meant none ([06-cli.md](06-cli.md)). The default exclusions (§2.2) already
  happened at **backup** time and cannot be re-included.
- **Dry-run** (`spec.dryRun: true`, reserved — additive to the 02-api.md contract, lands
  at M3): runs the full pipeline (ordering, selection, mapping, mode resolution) with
  server-side dry-run applies, persists nothing, and writes the plan to status. Also
  exposed as `crystalctl restore --dry-run` (reads the snapshot + a kubeconfig). For
  `Overwrite`, the plan carries the would-change field paths (capped: 20 paths per
  resource, 100 entries per restore, `truncated: true` beyond).
- **Status** (additive to 02-api.md `Restore.status`; `restoredResources` is the applied
  count):

```yaml
status:
  restoredResources: 141         # 02-api.md contract
  resources:                     # 04-manifest detail (additive), capped as above
    failedCount: 1
    entries:                     # non-trivial outcomes only
      - { group: "",   kind: Service,    name: web,        outcome: Failed,     reason: "nodePort 30080 already allocated" }
      - { group: "",   kind: ConfigMap,  name: app-config, outcome: Configured, changed: ["data.LOG_LEVEL"] }  # Overwrite SSA
      - { group: apps, kind: Deployment, name: web,        outcome: Recreated }                                 # Recreate replace
```

Outcomes: `Created` | `Configured` (Overwrite SSA update) | `Recreated` (Recreate
delete+create) | `Failed`; dry-run reports the same outcomes as the *planned* action.
Metrics carry the origin `namespace` label per R19; the catalogue lives in
[05-observability.md](05-observability.md).

## 6. Testing (M3 gate)

- **Golden-file corpus** (mandatory, M3 exit criterion): `internal/sanitize/testdata/<case>/input.yaml`
  → `expected.yaml`, table-driven, byte-exact comparison. Minimum cases: Service of each
  type (ClusterIP, NodePort, LoadBalancer with `healthCheckNodePort`, headless,
  ExternalName); bound PVC with all S11 annotations; controller-owned Pod/ReplicaSet/Job
  (exclusion); selectorless Service + manually-managed EndpointSlice (kept, sanitized);
  standalone Pod with injected sidecar and `kube-api-access-*` volume;
  object with `last-applied-configuration`; custom resource with a `status` subresource;
  Deployment with revision annotation. Adding a sanitization rule without a corpus case
  fails review (DoD, 90-roadmap.md).
- **Determinism test**: dump the same namespace twice; assert byte-identical files.
- **e2e** (M3): backup a demo namespace, restore into a fresh namespace on the same
  cluster and into a kind cluster (different storage class, different Service CIDR);
  workloads reach `Ready`; NodePorts preserved; **mode** verified against a pre-created
  drifted ConfigMap — `Recreate` replaces it, `Overwrite` SSA-merges and keeps
  target-only extras — and the R23 `confirmation` gate is enforced on both.

## 7. Open questions

1. ~~`spec.dryRun` and `status.resources` are additive to the 02-api.md contract~~ —
   **resolved (M3)**: both are now in [02-api.md](02-api.md) as the contract, `dryRun`
   top-level rather than nested under a `manifests` block (one selection model and one mode
   serve both halves of a restore, so a per-half switch would be inventing a distinction
   that does not exist).
2. Should E9 (VolumeSnapshot exclusion) become opt-out for same-cluster restores where
   the VolumeSnapshotContents still exist? Deferred; default stays excluded.
3. Helm release Secrets (`type: helm.sh/release.v1`) are currently **kept** so `helm`
   keeps working after restore; revisit if their size becomes a dedup problem.
