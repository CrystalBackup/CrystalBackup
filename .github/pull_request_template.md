<!--
  Keep this short. The checklists exist to catch the things that are expensive to find
  later, not to be ceremony. Delete any section that genuinely does not apply.
-->

## What and why

<!-- One paragraph. What changes, and what it buys. Link the spec section or ADR that
     authorises it — in this project the specs are the contract, so a behavioural change
     with no spec change is either a bug fix or a missing spec update. -->

Spec / ADR:
Milestone:

## Definition of Done

`spec/08-testing-and-dod.md` is authoritative. For this PR:

- [ ] Tests: unit / envtest cover the new behaviour (not just the happy path)
- [ ] `make test` and the linter pass locally
- [ ] Spec, `CHANGELOG.md`, chart and docs updated **in this PR** (same-PR rule)
- [ ] Generated artefacts regenerated via the `make` targets (CRDs, deepcopy) — not by hand
- [ ] No build artefacts committed (`melange.rsa`, `stage-*/`, `packages/`, `sbom/`,
      `apko.lock.json`, `image-refs.txt`)

## Security review checklist

**Required (two approvals) when this PR touches credentials, keys, or cross-namespace
logic** — DoD 4. The CODEOWNERS paths are the tripwire; this checklist is what the second
reviewer actually checks.

- [ ] **Not applicable** — this PR touches none of the surfaces below

Otherwise:

- [ ] **Tenancy (I1)**: any `namespace=` tag filter or repository path is derived
      **server-side** from the CR's own namespace. A user cannot express, override or
      widen it. Resolution fails **closed** — no fallback to an unmediated identifier.
- [ ] **Least privilege**: no RBAC verb or resource added beyond what the change needs.
      Widening `crystal-manifest-reader`/`-writer` or
      `crystal-cluster-manifest-reader`/`-writer` requires an ADR (03-security §5).
- [ ] **Transient grants**: any RoleBinding created in a tenant namespace is deleted on
      Job completion **and** is reachable by the OrphanReaper backstop. Remember an
      `ownerReference` cannot cross namespaces here (03-security §5).
- [ ] **Mover posture**: data movers keep zero API access (`crystal-mover`, no token).
      Only `crystal-manifest-mover` automounts a token, and only it gets API-server
      egress.
- [ ] **Secrets**: no credential, key or token is logged, put in an annotation, written
      to status, or placed in a URL query string.
- [ ] **Blast radius**: if this widens what a compromised mover or a namespace user can
      reach, that is stated explicitly in the PR description — not left implicit.

## Supply chain

Only when touching `.github/workflows/images.yml`, `build/`, or the release path:

- [ ] Tool versions pinned (the `env:` block), actions pinned per the repo convention
- [ ] The 0-known-CVE gate still runs **before** push
- [ ] Signing / SBOM / VEX / provenance still attach to the **index** digest
- [ ] VEX statements are **tool-generated** (`govulncheck`), never hand-written — a
      `not_affected` is a signed security claim about a product that holds every tenant's
      data

## Verification

<!-- What you actually ran, and what you observed. "CI is green" is not verification of a
     behavioural change. If a crucible run was needed, say so and paste the result line.
     If something is untested, say that too — an honest gap beats a silent one. -->
