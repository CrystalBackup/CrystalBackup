---
title: Admission rules
description: The validating admission policies the chart installs, what each denies, and why admission is a gate rather than the isolation boundary.
---

Blocking static validations ship as `ValidatingAdmissionPolicy` objects — CEL evaluated
inside the API server — so they hold **even when the operator is down**. The webhook is
reserved for the one genuinely dynamic check and runs `failurePolicy: Ignore`.

Rule numbers are stable; other documents cite them.

:::note[Admission is a gate, not the boundary]
Controllers re-derive repository identity, the `namespace=` filter and the confirmation
value at execution time. A bypassed policy degrades the user experience — you get a
confusing failure later instead of a clear rejection now — it does not breach tenancy. The
tenant boundary is structural; see
[Tenancy and isolation](/CrystalBackup/docs/understand/tenancy/).
:::

## The rules

| # | Enforced by | What it denies |
|---|---|---|
| 1 | VAP | **Destructive confirmation.** Every `Restore`/`ClusterRestore` in `Recreate` or `Overwrite`, and every `ClusterErasure`, needs `spec.confirmation` equal to the target. |
| 2 | VAP | **User isolation.** A user-created `Backup`/`BackupSchedule` must reference a namespaced `BackupLocation`, never a `ClusterBackupLocation`. A `BackupExternalSync`'s source *and* destination are both same-namespace `BackupLocation`s. |
| 3 | controller | **Retention placement.** Advisory, not a denial — `keep*` on an `Immutable` location is reported ignored via a `RetentionIgnored` condition. |
| 4 | webhook | **Single default `ClusterBackupLocation`.** Cross-object uniqueness is not expressible in per-object CEL. |
| 5 | VAP | **Same-namespace Secret references.** `credentialsSecretRef` and `repositoryPasswordSecretRef` on a `BackupLocation` are name-only, resolved in that namespace. |
| 6 | VAP | **`Immutable` forbids prune.** `mode: Immutable` may not set `maintenance.pruneSchedule`. |
| 7 | VAP | **Denied namespaces.** Tenant-facing resources are rejected in a configurable deny-list. |
| 8 | VAP | **Namespace-selector shape.** `namespaces` must set exactly one non-empty positive form, plus an optional `exclude`. |
| 9 | VAP | **External sync distinctness.** `sourceLocationRef.name != destinationLocationRef.name`, on both sync kinds. |

Only rules 1, 2, 5, 6, 7, 8 and 9 produce an admission rejection. Rule 3 is a status
condition and rule 4 is the webhook.

## Rule 1 — confirmation, and its deliberate asymmetry

The policy is a **conservative superset**. CEL cannot ask whether the target namespace
already exists, so confirmation is required unconditionally in both modes — and since
`Recreate` and `Overwrite` are the only two modes, in practice every restore needs it.

The policy admits an **empty or absent** value and denies only a non-matching non-empty
one. That is why `spec.confirmation` is `+optional` in the schema rather than required: a
required field with `MinLength=1` would be rejected by the API server's structural schema
*before* the policy ran, making the `AwaitingConfirmation` phase unreachable.

So:

- **wrong value** → denied at admission, the object is never created;
- **absent value** → admitted, and the controller parks the object in
  `AwaitingConfirmation` until you edit it in.

The controller checks the same equality independently, before it resolves the source.

## Rule 2 — and why the operator is exempt

The binding carries a `matchConditions` clause excluding the operator's own ServiceAccount.
Without it, the operator's cluster-DR fan-out — which legitimately creates `Backup` objects
in tenant namespaces referencing a `ClusterBackupLocation` — would be denied by its own
policy.

Rules 7 and 8 carry the same exemption, for the same reason.

Note what rule 2 does **not** do: cluster-origin `Backup` objects being read-only to users
is **RBAC**, not admission. The shipped `crystal-backup-tenant` ClusterRole grants only
`get`, `list` and `watch` on `backups`.

## Rule 7 — the deny-list

Configured through Helm, rendered into a ConfigMap bound to the policy by `paramRef` — so
it can also be edited in-cluster after install.

```yaml
admission:
  deniedNamespaces:
    - "kube-*"
    - crystal-backup-system
    - velero
```

Plain names or `*`-suffixed prefixes. Add any incumbent backup tool's namespace: it is one
of the coexistence guarantees, and it costs nothing.

## Rule 8 — selector shape

```yaml
# Valid.
namespaces:
  matchLabels: { crystalbackup.io/protect: "true" }
  exclude: ["kube-*"]

# Denied — no positive form.
namespaces:
  exclude: ["kube-*"]

# Denied — an empty map counts as unset.
namespaces:
  matchLabels: {}
```

The positive forms are `matchNames`, `matchLabels`, `matchExpressions` and `regexp`.
Exactly one must be non-empty. The engine re-validates at execution.

## CRD-level validation

Not numbered, but these produce rejections too. They are CEL expressions on the CRDs
themselves.

**Immutability after creation**

| Object | Immutable fields |
|---|---|
| `Restore` | `spec.source`, `spec.mode` |
| `ClusterRestore` | `spec.source`, `spec.mode`, `spec.target.namespace` |
| `ClusterBackupLocation` | `spec.mode`, `spec.clusterID`, `spec.s3.endpoint`, `spec.s3.bucket`, `spec.s3.prefix` |
| `BackupLocation` | the same five |

Location identity is immutable because those fields compose the repository path: an edit
would silently re-point the location at a *different* repository, orphaning every backup
taken so far, with no data moving and no error. Restore identity is immutable because the
controller re-derives it every pass, so a mid-run edit would mix two points in time inside
one restore.

`confirmation` and the selection lists stay mutable — that is how you unpark a restore, and
an edit applies to volumes not yet started.

**Source shape**

- `exactly one of source.backup and source.time must be set`
- `source.origin is only valid together with source.time` (on `Restore`)
- `time must be "latest" or an RFC3339 timestamp`

**Bounds and paths**

- `resources` and `volumes`: at most 128 items each
- `targetPath`: at most 256 characters, and `targetPath must not contain '..' segments`
- `source.backup`: at most 253 characters; `source.time`: at most 64

**Value grammars**

- `pruneMaxRepackSize`: `^[0-9]+(\.[0-9]+)?[kKmMgGtT]?$`
- `checkReadDataSubset`: `^([0-9]+/[0-9]+|[0-9]+(\.[0-9]+)?%|[0-9]+(\.[0-9]+)?[kKmMgGtT]?)$`

Those two carry restic's own grammars, pinned here so a typo is rejected at apply time
rather than becoming a maintenance Job that starts, pulls an image, opens the repository
and only then dies on a flag parse error.

## Turning it off

```yaml
admission:
  vap:
    enabled: true      # requires Kubernetes >= 1.30
  webhook:
    enabled: true
```

Disabling the VAP set means every check above becomes a controller-side failure instead of
an API-server rejection — a worse experience, and not a tenancy hole.

Disabling the webhook leaves the controller's `MultipleDefaults` condition as the only
guard against a second default `ClusterBackupLocation`.

## Seeing what is installed

```bash
kubectl get validatingadmissionpolicy | grep crystalbackup
kubectl get validatingadmissionpolicybinding | grep crystalbackup
kubectl -n crystal-backup-system get configmap | grep denied
```
