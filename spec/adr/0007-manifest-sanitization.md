# ADR 0007 — Manifest sanitization: own rules-based engine in Go

Status: **Accepted** (2026-07-11)

## Context

R15 requires backing up a namespace's Kubernetes manifests alongside PVC data and
sanitizing them so they can be re-applied in another namespace or another cluster
(R6, R14, R22): strip `metadata.uid`, `resourceVersion`, `creationTimestamp`, `status`,
`managedFields`; strip Service `spec.clusterIP(s)` but **preserve `nodePort` numbers**;
keep `storageClassName` on PVCs so it can be remapped at restore time
(`storageClassMapping` on `Restore`/`ClusterRestore`, see [02-api.md](../02-api.md)).
The sanitizer runs in step 6 of the backup flow ([01-architecture.md](../01-architecture.md)
§5): live objects are read from the API server, transformed, and uploaded as the
`/manifests` restic snapshot.

The transformer has four hard properties to satisfy:

1. **Restore-oriented, not display-oriented.** The output must survive `kubectl apply`
   / server-side apply into a *different* cluster: immutable or server-populated fields
   must go, user intent must stay. This is a different goal from cleaning YAML for
   human eyes.
2. **Per-kind rules.** The clusterIP-vs-nodePort asymmetry is only one example: PVCs
   need `spec.volumeName` and the `pv.kubernetes.io/*` / `volume.kubernetes.io/*`
   binding annotations stripped while `storageClassName` is kept; `ownerReferences`
   are dropped (controller-owned objects are excluded from the dump in the first
   place — [04-manifest-backup.md](../04-manifest-backup.md) owns that policy).
   Velero's restore pipeline confirms the pattern: it needs kind-specific
   `RestoreItemAction` plugins and a dedicated `--preserve-nodeports` flag precisely
   because generic stripping is wrong for Services
   ([velero.io/docs — restore reference](https://velero.io/docs/main/restore-reference/)).
3. **Deterministic output.** The `/manifests` snapshot is stored in a deduplicating
   repository (R13). If an unchanged Deployment serializes to identical bytes on every
   backup, daily manifest snapshots cost near-zero added bytes; nondeterministic key
   order or field churn silently defeats this.
4. **A stable, testable contract.** Sanitization decides what a tenant gets back after
   a disaster. Its behaviour must be pinned by tests, versioned, and reviewable —
   not inherited from a third-party tool's release cadence.

The obvious prior art is `kubectl-neat` ([github.com/itaysk/kubectl-neat](https://github.com/itaysk/kubectl-neat),
Apache-2.0 — license-compatible with a future open-sourcing): a kubectl plugin that
de-clutters `kubectl get -o yaml` output for **display**. It strips the same generic
metadata we do, but it is built for reading, not restoring: it also removes
system-*populated* values that we must keep (a `nodePort` the user relies on looks
exactly like server noise to neat), exposes no stable library API (it is a plugin
binary; internals manipulate JSON via gjson/sjson), and shows low maintenance activity
(single maintainer, sparse releases). Building R15 on it would mean forking it anyway.

## Decision

**Implement our own sanitization engine in Go**, inside the operator codebase — no
shelling out to `kubectl-neat`, no import of it.

Design:

- **Rules-based transformer over `unstructured.Unstructured`.** The engine never
  depends on typed Go structs or a compiled scheme, so tenant custom resources (CNPG
  clusters, cert-manager Certificates, …) flow through the generic rules without us
  vendoring their types.
- **Ordered rule pipeline.** Each rule declares a match (a GVK, or `*` for all kinds)
  and a list of field operations (`removeField`, `removeAnnotationPrefix`,
  `keepField` carve-outs). Generic rules run first, per-kind rules after; later rules
  may re-protect a field a generic rule would drop (this is how `nodePort` survives).
- **Rules are data, not code.** The ruleset is a versioned YAML document embedded in
  the binary (`go:embed`). The `rulesetVersion` is recorded in the `/manifests`
  snapshot metadata so a restore knows which rules produced its input. Per-kind
  overrides via operator configuration are a possible future extension; v1 ships the
  embedded ruleset only.
- **Deterministic serialization.** After the pipeline, resources are emitted with
  sorted map keys and a stable document order, so an unchanged object produces
  identical bytes backup-over-backup.
- **Golden-file tested.** Every rule is exercised by a corpus of real-world input
  manifests (dumped from real tenant namespaces, redacted) with committed expected
  outputs. CI gates on 100 % rule coverage (a rule with no golden case fails the build)
  and on idempotence (`sanitize(sanitize(x)) == sanitize(x)`).

Core ruleset (summary — [04-manifest-backup.md](../04-manifest-backup.md) owns the
exhaustive, normative table; this ADR records the decision and approach):

| Scope | Rule |
|---|---|
| All kinds | Drop `metadata.uid`, `resourceVersion`, `generation`, `creationTimestamp`, `managedFields`, `selfLink`, `status`, `ownerReferences`. |
| All kinds | Drop the `kubectl.kubernetes.io/last-applied-configuration` annotation and other server-bookkeeping annotations (list in 04). |
| Service | Drop `spec.clusterIP` / `spec.clusterIPs`; **keep** `spec.ports[].nodePort` and `spec.healthCheckNodePort`. |
| PersistentVolumeClaim | Drop `spec.volumeName` and `pv.kubernetes.io/*`, `volume.beta.kubernetes.io/*`, `volume.kubernetes.io/*` annotations; **keep** `spec.storageClassName` (remapped at restore). |
| Per-kind (04) | Further rules per kind as the corpus surfaces them. |

Milestone: M3 (sanitization engine + golden corpus + cross-cluster restore e2e —
[90-roadmap.md](../90-roadmap.md)).

## Consequences

### Positive

- **Exact restore semantics under our control.** The clusterIP/nodePort asymmetry and
  the PVC storageClass-mapping hook are first-class rules, not patches around a
  display tool. New per-kind requirements are a rule + a golden file, not an upstream
  PR to a dormant project.
- **Dedup-friendly by construction**: deterministic bytes mean stable manifest
  snapshots cost almost nothing in added repository bytes (R13), and diffing two
  manifest snapshots with `restic diff` is meaningful.
- **Zero third-party dependency** on the critical restore path; nothing to track for
  license or liveness. The engine is small (a pipeline over `unstructured` maps) —
  the real asset is the ruleset and its corpus, which we would have had to build for
  any alternative anyway.
- **The contract is versioned and testable**: `rulesetVersion` in snapshot metadata +
  golden files give reviewers a byte-level view of any behavioural change (a
  requirement of the DoD, [90-roadmap.md](../90-roadmap.md)).

### Negative

- **We own the maintenance burden.** New Kubernetes minor versions introduce new
  server-populated fields and annotations; controllers and admission webhooks on our
  clusters inject fields we must learn to strip (or keep). The ruleset will never be
  "done".
- **The golden corpus grows with real-world cases** and must be curated (redaction of
  tenant data, periodic refresh from live clusters). Corpus review is manual work.

### Risks & mitigations

| Risk | Mitigation |
|---|---|
| **Over-stripping**: a rule removes a value the user set intentionally (the generalized nodePort problem) → restored app silently misconfigured. | Per-kind `keepField` carve-outs; golden corpus sourced from real tenant namespaces; M3 e2e restores a full namespace into kind and asserts workloads come back Ready; rule changes need review against the corpus diff. |
| **Under-stripping**: a server-owned or immutable field survives → apply fails in the target cluster. | M3 e2e restores into a *different* cluster profile (different CIDRs, storage classes); manifest restore reports per-resource apply errors in `Restore.status` instead of aborting the whole restore ([01-architecture.md](../01-architecture.md) §6). |
| **Ruleset drift vs Kubernetes API evolution.** | Corpus regenerated against every Kubernetes version in the support matrix as part of the version-bump PR; CI rule-coverage gate ensures dead or untested rules are caught. |
| **Determinism regression** silently inflates repository growth. | Golden files are byte-exact; CI asserts idempotence and byte-stability; the `crystalbackup_backup_last_added_bytes` / `crystalbackup_backup_added_bytes_total` metrics on `/manifests` snapshots (R19) make regressions visible in Grafana. |
| A sanitized manifest cannot be reconstructed to its original form (sanitization is lossy by design). | Accepted: the platform Velero (and later `ClusterBackupSchedule`, R22) covers same-cluster full-fidelity DR; R15 targets *portable* restore. Documented in the namespace user guide. |

## Alternatives considered

- **`kubectl-neat` as a library or subprocess** — Apache-2.0, so legally usable, and
  its generic stripping overlaps ours. **Rejected**: (1) *coverage* — it is
  display-oriented; it removes system-populated values indiscriminately (our kept
  `nodePort` case), has no per-kind restore rules, no storageClass-mapping hooks, no
  deterministic-output guarantee, and no stable Go API to build on (it ships as a
  kubectl plugin binary); (2) *liveness* — single maintainer, sparse release history;
  betting the restore path on it means forking it. As a subprocess it adds all the
  same gaps plus a process boundary in the middle of the manifest pipeline.
- **Server-side-apply intent extraction via `managedFields`** (filter each object down
  to the fields owned by the user's field manager, using
  `k8s.io/client-go/applyconfigurations` `Extract*` /
  [structured-merge-diff](https://github.com/kubernetes-sigs/structured-merge-diff)) —
  genuinely attractive: it recovers *exactly what the user applied*, by construction,
  with no ruleset to maintain. **Rejected for v1**: field ownership on our clusters is
  fragmented and unreliable as an intent signal — Helm releases, ArgoCD (kubectl-apply
  mode by default), legacy client-side `kubectl apply` (ownership recorded as an
  `Update` by `kubectl-client-side-apply`), HPAs owning `spec.replicas`, controllers
  and mutating webhooks owning defaulted fields. Extracting "the user's" manager set
  requires per-namespace heuristics that are more fragile than explicit rules, and
  failure is silent (missing fields, not errors). Kept as the designated successor —
  see Revisit triggers.
- **Backing up the `kubectl.kubernetes.io/last-applied-configuration` annotation
  only** — the annotation *is* user intent when present. **Rejected**: it is only
  written by client-side `kubectl apply`; it is absent under server-side apply, absent
  on Helm-created objects, and absent on anything created by controllers or operators.
  On real tenant namespaces the coverage is a minority of objects and shrinking as SSA
  adoption grows.

## Revisit triggers

- **managedFields-based intent extraction matures on our clusters**: if SSA becomes
  the dominant write path across our clusters (ArgoCD `ServerSideApply=true` fleet-wide,
  Helm SSA) and the `Extract*` API proves robust against our corpus, re-evaluate a
  hybrid engine — intent extraction first, rules as fallback/post-filter — which would
  shrink the ruleset instead of growing it.
- **The ecosystem standardizes a portable-manifest format** (a CNCF-blessed
  sanitization spec, or Velero/KubeStash-class projects converging on a shared
  transform library): adopt or align rather than diverge.
- **Ruleset churn becomes disproportionate** (e.g. every Kubernetes minor version
  forces multi-rule updates and corpus rework): revisit the hybrid approach above
  ahead of schedule.
