# ADR 0018 — Hook execution identity: impersonate a tenant ServiceAccount

Status: **Accepted** (2026-07-28, product owner + tech lead) — resolves the question
[adr/0017 §5](0017-cascade-materialization-backup-carries-identity.md) deferred to M5.

## Context

M4 gave the operator consistency hooks (R16): commands it runs inside a tenant's containers via
`pods/exec`, to quiesce an application around the snapshot. On the **cluster plane** that is
uncontroversial — the hooks are declared on a `ClusterBackupSchedule`, which only an admin can
write.

M5 opens the **namespace plane**, and with it `BackupSchedule.spec.hooks`, which a tenant writes.
That turns a benign feature into a privilege-escalation question, stated plainly in
[03-security-and-tenancy.md §5](../03-security-and-tenancy.md):

> users can only make the platform run commands they can already run themselves

Nothing enforced it. A user granted `create backupschedules` in their namespace could make the
operator — holder of cluster-wide `pods/exec` — run any command in any pod of that namespace,
whether or not they held `pods/exec` themselves. `honorAnnotations` widens it further: the command
then comes from a pod annotation, so anyone who can *patch a pod* chooses what the operator execs.

[adr/0017 §5](0017-cascade-materialization-backup-carries-identity.md) recorded the problem and
named a candidate — a `SubjectAccessReview` on the CR's creator — explicitly deferring the
decision to M5.

### What the ecosystem actually does

Surveyed before deciding, because being stricter than every comparable product is a claim worth
checking:

- **Velero** — hooks come from `Backup.spec.hooks` *or* from pod annotations
  (`pre.hook.backup.velero.io/command` and friends). The documentation describes **no permission
  check on either path**. Velero survives this because it is not a self-service product: `Backup`
  objects live in the Velero install namespace, so there is no tenant plane to escalate *from*.
  The pod-annotation path is a genuine hole and is simply accepted.
- **Kanister** (CNCF sandbox) — Blueprints live in the **controller's** namespace; tenants cannot
  author them. Kanister additionally removed its default `edit` ClusterRoleBinding, pushing admins
  to grant its ServiceAccount a narrow Role per application namespace. Confinement by **placement**
  plus least privilege, not by checking the requester.
- **Stash / KubeStash** — `BackupConfiguration.spec.hooks` *is* namespaced and tenant-authorable;
  no creator check found. Same posture as Velero.
- **Argo Workflows** — the closest analogue (a submitted CR makes the controller create pods under
  a chosen `serviceAccountName`), and a long CVE history on exactly this. Their answer is not a
  check on the submitter: the workload runs under **a ServiceAccount in the workflow's own
  namespace**, so its reach is that namespace's RBAC. Argo CD's newer sync design goes further and
  has the controller **impersonate** a per-project ServiceAccount rather than use its own.
- **Kubernetes itself** — the canonical answer to this class: you cannot create a `Role`/
  `RoleBinding` granting permissions you do not hold, unless you have the `escalate`/`bind` verb.
  Enforced **in the API server**.

Two conclusions. Nobody in the backup space performs the creator check, so the SAR would have been
stricter than the state of the art — at the cost of a blocking webhook. And the direction the
adjacent projects converged on is not "verify the requester" but **"act under an identity the
namespace itself authorised"**.

## Decision

**The operator IMPERSONATES a tenant-named ServiceAccount, in the backed-up namespace, to run
hooks.** `HooksSpec.serviceAccountName` names it; the namespace is always the target pod's and is
not a field.

- **The name is the tenant's choice.** Any ServiceAccount in their own namespace, created by them
  or by an admin, granted `create pods/exec`. No naming convention is imposed.
- **The namespace is not negotiable.** It is derived from the pod the hook targets. A configurable
  namespace would be a cross-tenant hole by construction, so the only degree of freedom is *which*
  ServiceAccount inside the tenant's own namespace.
- **Required on the namespace plane, optional on the cluster plane.** A namespace-plane run that
  declares hooks without a `serviceAccountName` is gated (`HooksNeedServiceAccount`) rather than
  executed — falling back to the operator's identity *is* the escalation. An admin-authored
  cluster-plane run may leave it empty and keep M4's behaviour.
- **It governs annotation-sourced hooks too.** A pod annotation supplies the *command*, never the
  *identity*. This is the one point where this design is strictly stronger than Velero's.
- RBAC: the operator gains `impersonate` on `serviceaccounts`, and keeps `pods/exec` for the
  cluster plane.

### Why impersonation beats the SubjectAccessReview

- **It is checked at every exec, not once at admission.** A SAR authorises the CR; the hook then
  runs for as long as the schedule exists. Revoke the creator's `pods/exec` and the hooks keep
  running. Impersonation is authorised by the API server *at the moment of each call*, so
  revocation takes effect immediately.
- **No blocking webhook.** The SAR needs `request.userInfo`, which only admission has. That means a
  second `ValidatingWebhookConfiguration`, and — for it to be a real boundary — `failurePolicy:
  Fail`, so an operator outage would block tenants from writing `BackupSchedule`s. It also
  contradicts [adr/0010](0010-admission-vap-first.md)'s stance that the webhook is never a safety
  boundary. Impersonation needs neither.
- **Nothing can lie or be absent.** No annotation, no marker, no cached verdict — the failure mode
  of a marker that says one thing while reality says another is a lesson this project already paid
  for in M4.
- **It is legible.** "The operator runs your hooks as the ServiceAccount you named" is a sentence a
  tenant can act on; "your hooks were admitted because you held pods/exec at creation time" is not.

## Consequences

### Positive
- The §5 confinement invariant becomes **enforced by the API server**, not asserted in prose.
- Tenants gain an explicit, auditable capability boundary they control: grant the ServiceAccount
  `pods/exec` on the pods they choose, and the platform can do exactly that and nothing more.
- The `honorAnnotations` hole is closed on both planes.
- A hook failure names the identity, so the missing grant is findable.

### Negative / costs
- **One-time setup per namespace.** Hooks do not work until a tenant creates a ServiceAccount and
  grants it `pods/exec`. Documented in the user guide; the gate condition says exactly what to do.
- **The `impersonate` grant is unrestricted by name.** Because the name is a user-chosen field, it
  cannot be pinned with `resourceNames` without imposing a convention. What bounds it is the code:
  the impersonated namespace is derived from the target pod and is not a field anywhere in the API.
  Administrators who standardise on one name may narrow the rule in their overlay. This trades a
  broad-but-code-bounded grant for a narrow-but-tenant-hostile one, deliberately.
- **The operator still holds `pods/exec`** for admin-authored cluster-plane hooks. Dropping it
  entirely would require the cluster plane to name identities too — possible, and worth revisiting.

## Alternatives considered

1. **SubjectAccessReview on the creator, blocking webhook** — the adr/0017 candidate. Rejected on
   the four points above; strictly weaker than impersonation and more machinery.
2. **Webhook `Ignore` + a controller-side approval annotation.** Closes the outage bypass without
   blocking writes, but reintroduces a marker that can disagree with reality.
3. **Accept the ecosystem posture and document it** (Velero/Stash). Rejected: it means abandoning
   an invariant our own spec asserts.
4. **Forbid hooks on the namespace plane in v1.** Safe and simple; rejected because the capability
   is legitimate and the identity model makes it safe.
5. **Impersonate the CR's creator** rather than a named ServiceAccount. Strongest semantics, but it
   needs the creator recorded at admission — a mutating webhook whose annotation must be
   trustworthy, which puts the `failurePolicy` dilemma straight back.

## Revisit triggers

- The cluster plane wants the same treatment (admin hooks naming an identity) → the operator could
  then drop `pods/exec` entirely, which would be a material reduction of the largest privilege in
  the backup path.
- A tenant needs hooks in pods the ServiceAccount cannot reach (cross-namespace quiesce) → do NOT
  add a namespace field; revisit the whole model.
- Kubernetes gains a native "act as" for controllers → replace the impersonation plumbing.

## References

- [adr/0017 §5](0017-cascade-materialization-backup-carries-identity.md) — where this question was
  raised and deferred.
- [adr/0010](0010-admission-vap-first.md) — VAP-first admission, and why the webhook is not a
  safety boundary (which this decision avoids contradicting).
- [03-security-and-tenancy.md §5](../03-security-and-tenancy.md) — the confinement invariant.
- Mechanism: `internal/hooks/executor.go` (`PodExecutor.Exec`, rest impersonation),
  `internal/hooks/hooks.go` (`Resolved.ServiceAccountName`, stamped for both sources),
  `internal/controller/backup_controller.go` (the `HooksNeedServiceAccount` gate).
- Velero hook annotations: `velero.io/docs/main/backup-hooks/`. Kanister RBAC:
  `docs.kanister.io/rbac.html`. Argo Workflows service accounts:
  `argo-workflows.readthedocs.io/en/latest/service-accounts/`. Kubernetes RBAC escalation
  prevention: `kubernetes.io/docs/reference/access-authn-authz/rbac/`.
