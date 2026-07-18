# ADR 0010 — ValidatingAdmissionPolicy first; webhook only for dynamic checks

Status: **Accepted** (2026-07-15)

## Context

Crystal Backup needs blocking admission validation for safety, tenant isolation and
destructive-operation confirmation — the rules catalogued in
[02-api.md § Validation](../02-api.md#validation--admission-vap-first) and
[03-security-and-tenancy.md §8](../03-security-and-tenancy.md).

The classic implementation is a **validating webhook** served by the operator. That couples
admission correctness to the operator's availability, and both failure modes are bad:

- `failurePolicy: Fail` — if the operator is down, unreachable, or its cert expires, the API
  server **blocks every matching write**. For our own CRDs that can wedge backups/restores
  cluster-wide precisely when the operator is already unhealthy.
- `failurePolicy: Ignore` — the same outage **silently disables the safety checks**
  (confirmation, isolation), which is worse: destructive operations pass unguarded.

Kubernetes ships **`ValidatingAdmissionPolicy`** (VAP) — GA since 1.30 — evaluating **CEL in
the API server**, with **no external call and no operator dependency**. It supports
`validationActions: [Deny, Warn, Audit]`, parameter resources (`paramKind` / `paramRef`, e.g. a
ConfigMap), and `messageExpression`. Most of our rules are **static per-object** checks (field
equality, field presence, a field constrained by another field) — directly expressible in CEL.
A few need **cross-object or live-cluster state** — single-default-`ClusterBackupLocation`
uniqueness, "does the target namespace/PVC already exist", and "is the referenced location
`Immutable`" — which per-object CEL **cannot** evaluate (VAP sees only the request object and a
bound `paramRef`, never other cluster objects).

## Decision

**Prefer `ValidatingAdmissionPolicy` for every blocking, static rule; keep the operator webhook
only for the genuinely dynamic ones, minimized and fail-open.**

1. **Static, blocking rules → VAP** (CEL, in-API-server), shipped as
   `ValidatingAdmissionPolicy` + `ValidatingAdmissionPolicyBinding` in the Helm chart,
   versioned with the CRDs. This covers [02-api.md](../02-api.md) validation rules 1–2 and 5–8:
   R23 destructive confirmation — enforced as the **conservative superset** "**every** `Recreate`
   **and every** `Overwrite` requires `confirmation == target`" (a purely static field equality;
   VAP cannot test whether the target already exists, so it asks for confirmation unconditionally
   in those modes — a safe over-approximation), user-isolation structural constraints,
   immutable-forbids-prune, same-namespace Secret refs, and the `namespaces`-selector shape. The
   **denied-namespaces** deny-list (rule 7) is a VAP `paramRef` to a **ConfigMap**, so operators
   tune it without editing policy. The VAP **binding excludes the operator's own ServiceAccount**
   (`matchConditions`), so the operator's cluster-origin fan-out `Backup`s — which legitimately
   reference a `ClusterBackupLocation` — are not denied by the user-isolation rule.
2. **Dynamic / cross-object rules → operator webhook or controller.** The
   **single-default-location** uniqueness check (rule 4) stays a **webhook** with
   **`failurePolicy: Ignore`** and object/namespace selectors scoped strictly to `crystalbackup.io`
   CRDs, so an operator outage never wedges the API server (`Ignore` is safe because a transient
   second default is also caught by the operator's `MultipleDefaults` reconcile condition — not a
   safety property). The **retention-vs-`Immutable`-mode** advisory (rule 3) is now a **same-object**
   check — `spec.retention` lives on the location itself, so its mode is right there — emitted as an
   advisory rather than a denial: the ClusterBackupLocation controller sets a `Warning` Event + a
   `RetentionIgnored` status condition when an `Immutable` location carries a `keep*` policy
   (retention there is repository rotation). No blocking; it could be tightened to same-object CEL
   later if a hard reject is ever wanted.
3. **Defaulting / mutation** (e.g. `clusterID` from the default location, the generated
   repository-password Secret) stays in the operator's mutating webhook / reconcile path,
   `failurePolicy: Ignore` — it is convenience, not a safety gate.
4. **RBAC, not admission, carries** "cluster-origin `Backup` objects are read-only to users"
   (invariant I7) — a role/verb concern, not a CEL rule.

Controllers still **re-derive** repository identity and the `namespace=` tag filter and
**re-check** destructive confirmations at execution time (defense in depth,
[03-security-and-tenancy.md §3](../03-security-and-tenancy.md)); admission is a fast gate, never
the sole boundary.

## Consequences

### Positive

- **Safety survives operator outage**: confirmation and isolation checks live in the API
  server, so they cannot be silently disabled by an unhealthy operator, and cannot wedge
  unrelated writes either.
- **Less operator surface**: little webhook code, one narrow webhook cert instead of a broad
  one; static rules are declarative and auditable as cluster objects.
- **Operator availability is decoupled from admission** for everything except a non-safety
  uniqueness check.
- Observability is clean: VAP denials appear in the apiserver's
  `apiserver_validating_admission_policy_check_total`; the operator's
  `crystalbackup_webhook_denials_total` now reflects only the dynamic rule
  ([05-observability.md §2.8](../05-observability.md)).

### Negative / limits

- **Requires Kubernetes ≥ 1.30** (VAP GA). Stated as the minimum supported version; a
  chart-toggled fallback can re-express the same CEL inside a webhook policy set for older
  clusters (see Risks).
- CEL cannot do cross-object or live-cluster lookups — hence the retained webhook for
  single-default, the controller-side retention-vs-mode advisory (rule 3), and the
  conservative-superset form of the confirmation rule (it cannot test target existence, so it
  always asks).
- VAP cannot mutate/default — defaulting stays operator-side.
- **Two mechanisms to keep in sync** with the CRD schema (VAP CEL + the CRD's own structural
  schema + the webhook).

### Risks & mitigations

| Risk | Mitigation |
|---|---|
| A target cluster runs Kubernetes < 1.30 | Documented minimum; Helm value `admission.mode: vap\|webhook` enables an equivalent webhook policy set carrying the same CEL logic as a fallback. |
| Param ConfigMap (deny-list) tampered or drifted | Shipped by the chart, RBAC-locked to admins; the binding pins `paramRef`. |
| VAP and the fallback webhook diverge over time | Conformance tests (envtest, [08-testing-and-dod.md](../08-testing-and-dod.md)) assert both reject/accept the same fixture set. |
| CEL expression bug lets a bad object through | Controllers re-check at execution (defense in depth); fixtures include the negative cases (R14/R23 e2e, [90-roadmap.md M2](../90-roadmap.md)). |

## Alternatives considered

- **All-webhook (the usual operator pattern).** Rejected: couples operator availability to
  admission correctness — `Fail` wedges the API for our CRDs, `Ignore` disables the safety
  checks. The whole point of the decision is to remove that coupling for the blocking rules.
- **All-VAP.** Impossible: single-default-location uniqueness is inherently cross-object and not
  expressible in per-object CEL.
- **OPA/Gatekeeper or Kyverno.** Rejected as a *dependency*: a generic open-source drop-in
  operator should not require an external policy engine. VAP is built into Kubernetes and
  sufficient. (Operators who already run Kyverno/Gatekeeper may of course layer their own
  policies on top — nothing here prevents it.)
- **No blocking admission, rely solely on controllers.** Rejected: controllers do re-enforce at
  execution, but a fast pre-execution gate on confirmation and isolation is valuable both as UX
  (immediate, clear rejection) and as defense in depth.

## Revisit triggers

- The minimum supported Kubernetes falls below VAP GA, or a target platform pins < 1.30 →
  activate the fallback webhook policy set.
- CEL / VAP gains cross-object or parameter features that let the single-default check move to
  VAP too → retire the operator webhook entirely.
- A new rule needs live-cluster state → weigh adding it to the existing dynamic webhook versus a
  controller-side check, keeping the webhook minimal and fail-open.
