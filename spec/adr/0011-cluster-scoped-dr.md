# ADR 0011 — Cluster-scoped disaster recovery: capture & selective restore

Status: **Accepted** (2026-07-15, product owner + tech lead)

## Context

Crystal Backup's cluster plane already backs up **all/selected namespaces** into one shared
repository ([adr/0009](0009-shared-cluster-repo-tag-tenancy.md)), and R22 calls cluster
disaster recovery a **core** capability. But a namespace's manifests + PVC data are **not
sufficient** to reconstitute a namespace on a **bare** cluster: a restored PVC may reference a
`StorageClass` that does not exist yet, a restored workload may declare a `PriorityClass` or an
`IngressClass` that is missing, and a namespace holding Custom Resources cannot come back without
its `CustomResourceDefinition`s. These are **cluster-scoped** (non-namespaced) objects, outside
the per-namespace `Backup`.

Earlier drafts left "cluster-scoped resource capture" as a bare M9 checkbox with **no design**
— a headline ("full platform DR") the implementation did not back. The product owner resolved
this on 2026-07-15: a real DR must capture cluster-scoped resources, and a `ClusterRestore` must
be able to restore them — but **restore is the admin's judgement call** (in a real DR one does
**not** restore everything: not `kube-system`-owned objects, not stacks an ArgoCD/flux GitOps
controller will re-reconcile). Capture must be broad and automatic; **restore must be selective
and opt-in**.

This ADR is scoped to **application-level** cluster-scoped resources. Backing up the Kubernetes
control-plane state itself (**etcd**) remains **out of scope** ([00-requirements.md §6](../00-requirements.md)):
platform DR covers resources + data, not the control plane.

## Decision

**A `ClusterBackup` captures cluster-scoped resources alongside its per-namespace fan-out, and a
`ClusterRestore` restores them selectively. Capture is ON by default; restore is opt-in,
admin-only, and confirmation-gated.**

### 1. Capture (cluster plane only)

- Each `ClusterBackup` run writes **one `kind=cluster-manifests` snapshot** to the shared repo at
  path `/cluster-manifests` (no `namespace` tag; `run=<backup>` as usual — see
  [02-api.md § Repository layout](../02-api.md#repository-layout--snapshot-identity)), in parallel
  with the per-namespace `Backup`s. `ClusterBackup.status.clusterResourcesCaptured` reports the
  object count.
- Controlled by `ClusterBackupSchedule.template.spec.clusterResources` (mirrored on
  `ClusterBackup.spec`): `enabled` (**default `true`**), `include`, `exclude`, `labelSelector`
  ([02-api.md](../02-api.md)).
- **Default allow-list** (when `include` is empty) — the DR-relevant kinds, curated to be useful
  without being noise: `CustomResourceDefinition`, `StorageClass`, `VolumeSnapshotClass`,
  `IngressClass`, `PriorityClass`, `RuntimeClass`, `ClusterRole`/`ClusterRoleBinding`, and
  `PersistentVolume` (PV specs, for the PV↔PVC rebinding story). **Default excludes** the
  control-plane's own objects (names matching `system:*`, and objects owned by cluster add-ons)
  so a restore does not fight the API server or an add-on operator. Admins widen/narrow via
  `include`/`exclude`/`labelSelector`.
- Capture runs as a dedicated Job bound to ClusterRole **`crystal-cluster-manifest-reader`**
  (read on the allow-listed cluster-scoped kinds), transient per run
  ([03-security-and-tenancy.md §5](../03-security-and-tenancy.md)). Sanitization reuses the
  rules engine of [adr/0007](0007-manifest-sanitization.md) with cluster-scoped additions: strip
  `status`, `managedFields`, `uid`, `resourceVersion`, `creationTimestamp`; on a `PersistentVolume`
  strip `spec.claimRef.uid`/`resourceVersion` and `status`; keep `ClusterRoleBinding` subjects
  verbatim (they are the point of a DR).

### 2. Restore (selective, opt-in, admin-only)

- `ClusterRestore.spec.clusterResources` (`include`/`exclude`) selects **which** cluster-scoped
  objects to restore. It is a **pointer**, so its three states are: **omitted ⇒ nothing
  cluster-scoped is restored** (the safe default; the admin opts in explicitly); **present with
  an empty `include` ⇒ every cluster-scoped object in the snapshot** (the snapshot is already the
  curated capture of §1, and the pointer's presence is the explicit opt-in, so there is nothing
  more to name); **present with a non-empty `include` ⇒ only the matching kinds/names**. `exclude`
  always subtracts, last. There is **no** cluster-scoped restore on the namespaced `Restore` path
  (it is structurally namespace-confined, R14).
- **storageClassMapping does not touch cluster-scoped objects.** A restored `PersistentVolume`
  keeps its captured `spec.storageClassName` — a PV represents an already-provisioned volume, so
  remapping its class would not re-provision anything; the mapping applies only to the PVCs on the
  volume-data path (04-manifest-backup.md §5.3). An explicit v1 non-goal.
- **Cross-tree ordering is by sequencing, not one Job.** The cluster-scoped objects live in a
  `kind=cluster-manifests` snapshot and the namespace's objects in `kind=manifests`/`kind=data`
  ones; the ClusterRestore runs the cluster-scoped restore Job to completion *before* it drives
  the volumes, so the StorageClasses and CRDs exist before anything binds to them. (Restoring the
  namespace's own **manifests** — its Deployments, Services — through a `ClusterRestore` reuses the
  namespaced resources[] engine and lands as a follow-up; a `ClusterRestore` today restores the
  cluster-scoped objects and the volume data.)
- **Apply order**: cluster-scoped **first**, so namespaced objects bind — `CustomResourceDefinition`s
  → other cluster-scoped (StorageClasses, PriorityClasses, IngressClasses, ClusterRoles/Bindings,
  PVs) → **namespaces** → namespaced objects. `Recreate`/`Overwrite` mode and the R23 `confirmation`
  gate apply as for any restore.
- Restore binds ClusterRole **`crystal-cluster-manifest-writer`** (create/update on the selected
  cluster-scoped kinds), **admin-only** and transient per Job. Because recreating cluster RBAC
  (`ClusterRoleBinding`s) or CRDs is privileged, it is **never implicit** — opt-in + confirmation
  + admin RBAC are all required.

### 3. Non-goals

- **Not etcd/control-plane backup** — application objects only.
- **Not a second CRD** — folded into the existing `ClusterBackup`/`ClusterRestore` rather than a
  new kind, so the cascade, discovery and status model are unchanged.
- **Not on the namespace plane** — a tenant never captures or restores cluster-scoped objects.

## Consequences

### Positive

- **Real bare-cluster DR**: a `ClusterRestore` onto a rebuilt cluster can bring back the CRDs,
  StorageClasses and PVs a namespace's workloads depend on, closing the gap between "namespace
  manifests restored" and "namespace actually runs" — the headline R22 promise is now backed by
  a mechanism.
- **Safe by default at restore**: opt-in selection means a fleet `ClusterRestore` never silently
  recreates `ClusterRoleBinding`s or fights a GitOps controller; the admin curates exactly what
  the DR needs.
- Reuses the existing snapshot/repo/discovery/sanitization machinery — one `kind` added, no new
  control-plane surface.

### Negative / costs

- The capture Job holds a **broad cluster-scoped read** (allow-listed) — a privileged read
  confined to `crystal-backup-system`, admin-plane only, transient. It is a larger read surface
  than the namespaced manifest mover; two-person review applies (roadmap DoD).
- **Overlap with GitOps**: on clusters where ArgoCD/flux own cluster-scoped objects, capturing
  them is cheap but restoring them would conflict with the GitOps controller — hence restore is
  opt-in and the ops guidance is to **exclude GitOps-managed resources at restore** (roadmap M9).
- The default allow-list is a **judgement** that will need field tuning (some clusters want
  `ValidatingWebhookConfiguration`s or `MutatingWebhookConfiguration`s captured, others not).

### Risks & mitigations

| Risk | Mitigation |
|---|---|
| Restoring `ClusterRoleBinding`s escalates privilege | Restore is opt-in per `ClusterRestore`, admin-only RBAC, R23 confirmation; never implicit |
| Restored cluster-scoped object fights the API server or an add-on operator | Default excludes `system:*` and add-on-owned objects; apply order puts CRDs/StorageClasses first; `Overwrite` mode keeps extras |
| Capturing a huge, noisy set (every default `ClusterRole`) | Curated default allow-list + `exclude`; capture is manifests-only (cheap), the cost is at restore where selection is explicit |
| Two clusters sharing one bucket mixing cluster-manifests | `host=<clusterID>` + `run` tags scope the snapshot to its cluster, like all others (R20) |

## Alternatives considered

- **Namespace-scoped DR only (leave cluster-scoped out of v1).** Rejected: it makes "full
  platform DR" false on a bare cluster (missing CRDs/StorageClasses break restore). The product
  owner explicitly pulled cluster-scoped capture into v1.
- **Capture everything cluster-scoped, including control-plane/system objects.** Rejected: noise
  and danger (restoring `system:*` ClusterRoles fights the API server); a curated allow-list +
  default excludes is safer and still complete for application DR.
- **A separate `ClusterResourceBackup`/`ClusterResourceRestore` CRD.** Rejected: duplicates the
  cascade, discovery and status model for no benefit; folding capture into `ClusterBackup` keeps
  one run = one point-in-time for the whole cluster.
- **Restore cluster-scoped by default (symmetry with capture).** Rejected: unsafe — a DR admin
  rarely wants *all* cluster-scoped objects back, and implicit `ClusterRoleBinding`/CRD recreation
  is privileged. Capture broad, restore selective.

## Revisit triggers

- Field experience tunes the default allow-list (e.g. webhook configurations, `APIService`s,
  `FlowSchema`/`PriorityLevelConfiguration`).
- A first-class **GitOps-aware** exclude (detect `argocd`/`flux` ownership labels and exclude at
  restore automatically) is requested — currently manual guidance (M9).
- etcd/control-plane backup ever comes into scope (today explicitly out — [00-requirements.md §6](../00-requirements.md)).
- `VolumeGroupSnapshot` or a cluster-wide consistency primitive changes how a point-in-time
  spanning namespaced data + cluster-scoped manifests is taken.
