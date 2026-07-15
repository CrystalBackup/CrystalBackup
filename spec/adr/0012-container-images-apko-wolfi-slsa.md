# ADR 0012 — Container images: apko + Wolfi base, SLSA L3+ provenance

Status: **Accepted** (2026-07-15, product owner + tech lead)

## Context

The operator and mover images are deployed into customer clusters, next to tenant workloads
and holding — transiently — storage credentials and encryption keys. Their supply-chain
posture is therefore a **product** property, not a build detail:

- a **0-known-CVE** baseline is required ([00-requirements.md §5](../00-requirements.md)): a
  backup tool that ships CVEs undermines the trust it exists to provide;
- the build must produce **verifiable provenance** (who/what/how built this exact digest),
  targeting **SLSA Build L3+**;
- images must stay **minimal** (no shell, no package manager at runtime) to shrink attack
  surface, and be **reproducible** so rebuilding a tag yields the same content.

The prior spec said only "distroless/static where possible" — under-specified and
Debian-cadence. Three base strategies were compared: Debian-based **distroless** (Google),
**Alpine** (musl libc), and **Wolfi** (glibc, Chainguard) built with **apko**/**melange**.

## Decision

**Build every image (operator, mover) declaratively with `apko` on a Wolfi (glibc) base,
package anything compiled from source with `melange`, and emit SLSA L3+ provenance from GitHub
Actions.**

- **Wolfi, glibc — not Alpine/musl.** restic + our shim are data-critical; musl's DNS/NSS and
  assorted libc behavioural differences are an avoidable risk for a tool whose one job is
  fidelity. Wolfi is glibc, rolling, minimal, and engineered for a near-zero CVE window with
  fast remediation. The size cost over musl/scratch is accepted.
- **apko (declarative OCI) + melange (source→apk).** No Dockerfile: the image is a reproducible
  assembly of apk packages. `melange` builds our pinned **restic** (ADR 0001) into a signed apk
  so the mover's engine version is reproducible and SBOM-attributed, not pulled ad hoc.
- **Minimal + non-root.** Runtime images carry no shell/package manager; the operator runs as
  non-root. The mover keeps the baseline-legal capability set of
  [03-security-and-tenancy.md §6](../03-security-and-tenancy.md) (it needs `runAsUser: 0` +
  `DAC_OVERRIDE` to read arbitrary-uid files — a securityContext concern, unrelated to the base
  image).
- **Signed, SBOM'd, digest-pinned.** apko emits an SPDX **SBOM** per image; images are signed
  with **cosign** (keyless — Fulcio/Rekor, ephemeral OIDC identity) and referenced **by digest**
  in the Helm chart (no floating tags, no user-facing image field —
  [03-security-and-tenancy.md §11](../03-security-and-tenancy.md)).
- **SLSA L3+ in GitHub Actions.** Build on GitHub-hosted runners with an **isolated builder** and
  **non-falsifiable provenance** — the attestation is generated with the SLSA GitHub generator /
  `cosign attest` using the workflow's ephemeral OIDC identity (Fulcio/Rekor) → **Build L3** (L3
  requires builder isolation + unforgeable provenance, **not** full hermeticity). `slsa-verifier`
  / `cosign verify-attestation` gate the release.
- **CI gate.** A container CVE scan (grype/trivy) blocks release above a **0-known-CVE**
  threshold (documented, time-boxed exceptions only); signature + provenance verification run in
  the release job; a **scheduled rebuild** re-bakes images against the rolling Wolfi apk set so
  fixes land without waiting for a code change ([08-testing-and-dod.md §7](../08-testing-and-dod.md)).

## Consequences

### Positive
- Near-zero CVE surface with fast remediation (Wolfi rolling + scheduled rebuild); glibc
  correctness for a fidelity-critical tool; declarative, reproducible builds with native SBOM
  and SLSA L3 provenance — a genuine supply-chain differentiator for a backup product.
- No Dockerfile drift; image content is reviewable as a package list.

### Negative / costs
- apko/melange are less ubiquitous than Dockerfiles (team ramp-up); a **melange** recipe is
  needed to pin restic if the Wolfi apk lags our required version.
- Rolling Wolfi means images must be **rebuilt and re-validated regularly** (scheduled pipeline
  + a re-run of the metadata-fidelity gate), not built once per release.
- glibc images are larger than musl/scratch (bytes, not a functional cost).

### Alternatives considered
- **Debian distroless** — minimal and popular, but slower CVE cadence and not declaratively
  composable with extra apks; rejected on remediation speed.
- **Alpine / musl** — smallest with a package manager, but musl behavioural edge cases are an
  unacceptable risk for a data-fidelity tool; rejected.
- **scratch + fully-static** — smallest possible; rejected as the default because it drops CA
  certs / tzdata / nsswitch and the apk supply-chain tooling; we still build static Go binaries
  but place them on a Wolfi base for certs + glibc + SBOM lineage.
- **ko** — excellent for the Go operator image, but does not cover the **restic** binary in the
  mover and gives less control over base packages; apko covers both uniformly.

## Revisit triggers
- Wolfi rolling churn becomes operationally heavy → pin to a Chainguard stable stream.
- A surfaced need proves the operator can be pure-`scratch` static with no glibc/cert/tz
  dependency → reconsider a scratch operator image (the mover still needs the restic apk).
- SLSA **L4** tooling matures on GitHub Actions → raise the provenance target.
- restic ships an official, version-matched Wolfi apk → drop the melange recipe.
