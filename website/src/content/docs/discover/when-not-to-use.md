---
title: When not to choose it
description: The cases Crystal Backup does not cover, the costs it imposes, and the situations where another tool is the better answer.
---

Read this before the quickstart. Every item is a real limitation of the current release,
not a caveat added for form.

## It is not production-hardened yet

Milestones M0 through M6 have shipped and are tested — unit and envtest suites, a Kind
end-to-end suite, and a real-infrastructure suite on provisioned clusters, which for
`v0.6.0` ran unfiltered: 82 of 82 checks, nothing failed and nothing skipped. But the CRD
API is `v1alpha1` and will still move before `1.0.0`, and two of M6's own exit criteria are
unmet — nobody has run this alongside an incumbent tool for two weeks, and there has been no
pilot rollout. The honest summary is: **early, but no longer hypothetical.** That is not the
same as a *"run it unattended on data you cannot recreate"* release.

Try it in a sandbox. **Keep your existing backups.** Test your restores — which is good
practice with any backup tool, and here it is the practice the project itself relies on.

## Things it does not do at all

**etcd and the control plane.** Crystal Backup captures application resources and PVC
data. It does not back up etcd. A "full platform DR" story that omits the control plane
has to say so, and this is it saying so: you need a separate answer for your cluster's own
state.

**Database-aware backups.** There are exec hooks, and they are enough to quiesce a
filesystem or issue a checkpoint. There is no database agent, no log shipping, no
point-in-time recovery between snapshots. If your Postgres already has an operator with
WAL archiving, keep it — that is a better backup of that database than a volume snapshot
will ever be.

**Storage quotas or chargeback.** Per-tenant metrics are exposed; no accounting or billing
is done with them, and there is no quota mechanism. A namespace generating far more data
than expected is visible, not bounded.

**Cross-cluster self-service restore.** A namespaced `Restore` is same-cluster,
same-namespace by construction. Restoring one cluster's namespace into another is an
administrator operation via `ClusterRestore`. There is no delegation mechanism for a
tenant to do it themselves.

**Block-mode volumes.** A PVC with `volumeMode: Block` is reported as a per-volume failure
with reason `RestoreBlockUnsupported`. It is not restored.

## Things that are specified but not shipped

Do not plan around these. They exist in the API surface or in the design documents, and
the implementation is not in this release.

| Feature | State |
|---|---|
| **Immutable locations** (S3 Object Lock) | `spec.mode: Immutable` is accepted and a few guards exist around it, but Object Lock support, window rotation and expiry are **not implemented**. Do not use `Immutable` expecting WORM. |
| **`crystalctl` CLI and the browse UI** | Not written. There is no user-facing command-line tool in this release. |
| **Repository-scoped mover credentials** | Movers currently receive the location's **root** object-storage credentials. A compromised mover can reach the whole bucket. Scoped, short-lived credentials are planned. |
| **Metrics catalogue and alert rules** | Metrics are emitted; the documented catalogue and the shipped alert rules are still being finalised. |
| **Namespace manifests through `ClusterRestore`** | A `ClusterRestore` restores cluster-scoped objects and volume data. Restoring the namespace's own workload manifests through that path is a follow-up. |

## Costs you are accepting

**One cluster-wide prune window.** The shared repository has a single exclusive
maintenance window during which no namespace can start a backup. Its memory use scales
with total cluster data, not per namespace. Schedule it off-peak and bound it with
`pruneMaxRepackSize`.

**No fair-share between tenants.** Mover concurrency is capped cluster-wide by
`maxConcurrentMovers`. A namespace with a great deal of churn can **delay** other
namespaces' backups. It cannot read them, but it can make them late.

**Erasure is physical, not cryptographic.** `ClusterErasure` runs `restic forget` by tag
followed by `prune`. There is no per-tenant crypto-shredding and there will not be: one
shared repository has one master key, so destroying a key destroys everyone's data. If
your compliance regime requires per-tenant key destruction, the shared cluster plane is
not the mechanism — give each tenant a namespace-plane location instead.

**A lost namespace-plane key is unrecoverable.** By design the platform holds no key slot
on a tenant's repository. If a tenant loses their `BackupLocation` password, their backups
are gone. There is no support path, because there is no mechanism.

**Coexistence surface is permanent.** Deny-lists, prefixes and snapshot-count alerting
exist even on a cluster running no other backup tool.

## You probably want something else if…

- **You only need admin cluster-wide DR.** Velero is more mature, more widely deployed and
  has a far larger operational corpus behind it. Use it.
- **Your namespaces share almost no data.** The whole point of one shared repository is
  cluster-wide deduplication. Without shared data you pay the coordination cost — one
  prune window, one key — and get none of the benefit. K8up's one-repository-per-namespace
  model is simpler and fits better.
- **You need WORM immutability today.** Object Lock is not implemented. Use a tool that
  has shipped it.
- **You need a graphical browse-and-download experience.** There is no UI. Kasten K10 has
  a good one, at the cost of being proprietary.
- **Your cluster is below Kubernetes 1.30.** The chart's supported floor is 1.30, because
  the admission model is built on `ValidatingAdmissionPolicy`, which is GA there.
- **Your PVCs are on a CSI driver with no snapshot support.** Such a PVC is *skipped*, with
  `status.volumes[].phase: Skipped` and `reason: CSISnapshotUnsupported`. It is reported
  rather than silently dropped, but it is not backed up.

## Still here?

Then [check the requirements](/CrystalBackup/docs/start/requirements/) and
[install it](/CrystalBackup/docs/start/install/). In a sandbox.
