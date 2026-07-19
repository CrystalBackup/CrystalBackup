---
name: crucible
description: Drive the real-conditions e2e suite on Hetzner Cloud (test/crucible) — provision RKE2 + rook-ceph + longhorn + local-path, seed tenant workloads, run milestone-labeled Ginkgo tests, and read the plain-language report. Use when asked to run live/field tests, validate a milestone in real conditions, check non-regressions, or reproduce an issue on real infrastructure.
---

# Crucible — run Crystal Backup's real-conditions e2e

Everything is driven from `test/crucible/` via **mise tasks** (`mise run <task>`).
Full context: `test/crucible/README.md`.

## Ground rules

1. **Money**: `mise run up` creates ~€0.52/hour of Hetzner resources (≈€12.5/day;
   a ~2 h validation session ≈ €1).
   Confirm with the user before provisioning unless they just asked for it, and
   always propose `CONFIRM=yes mise run down` when the work is done. If a session
   ends with infrastructure still up, SAY SO explicitly.
2. **Secrets**: credentials live in `<repo>/.secrets/` (git-ignored; layout in
   `test/crucible/secrets.example/`). Never print, copy, or commit their values.
   If they're missing, point the user at the example dir — do not ask for values
   in chat.
3. **Tools**: run `mise install` in `test/crucible/` once. Every tool is pinned
   there (opentofu, ansible, kubectl, helm, awscli, hcloud, jq); the Go toolchain
   is inherited from the repo-root `mise.toml`. Drive the workflow with
   `cd test/crucible && mise run <task>` (or `mise -C test/crucible run <task>`).
4. **Never `mise run nuke` or `mise run down` without the user asking for teardown.**

## Phases (all idempotent — re-run after fixing)

```sh
cd test/crucible
mise install                     # once
mise run check-env               # credentials sanity (never prints values)
mise run up                      # infra (tofu) -> cluster (ansible/RKE2) -> components (deploy.sh)
mise run seed                    # tenant namespaces + checksummed data
mise run test                    # whole suite + readable report
mise run test m0                 # one milestone (labels: infra, m0, m1, ...)
CONFIRM=yes mise run down        # ALWAYS at the end (or `mise run nuke` if tfstate lost)
```

`mise run` with no task lists them all. Granular: `mise run infra` /
`mise run cluster` / `mise run components` — prefer re-running a single failed
phase over `up`. Expect `up` to take 15–25 min (Ceph OSD prepare dominates); use
long Bash timeouts or background execution.

## Building dev images (pre-release iteration)

Before a released image exists, `components`/`deploy.sh` needs a `:dev` operator
(and mover) image built from your working tree, **pushed to GHCR** (the cluster
pulls it), and deployed **by digest**. Full recipe — with the macOS/arm64 gotchas
(`DOCKER_HOST=unix://$HOME/.rd/docker.sock`, build **x86_64 only**, and the slow
restic-from-source mover build you should **build once and reuse**) — is in
`build/README.md` at the repo root. In short: rebuild → `apko publish …:dev` →
resolve the digest (`docker buildx imagetools inspect …:dev --format
'{{.Manifest.Digest}}'`) → `OPERATOR_IMAGE_DIGEST=… MOVER_IMAGE_DIGEST=…
test/crucible/deploy/deploy.sh` (or `helm upgrade --reuse-values --set
image.digest=…` for an operator-only change) → `mise run test m1`.

## Interpreting results

`mise run test` prints a **plain-language report** last (also saved to
`test/crucible/artifacts/crucible-report.md`) — relay it to the user rather than
raw Ginkgo output. It contains:

- a one-line **verdict** (✅ PASS / ❌ FAIL / ❌ SETUP FAILED) + a pass/fail/skip tally;
- checks grouped by **area** — `Platform [infra]` and `Milestone M0 [m0]`, each
  with the plain question it answers;
- a **Failures** list with the reason and the file:line to look at;
- a **What this means** paragraph that tells the reader what to do next.

Reading rules:

- If any `infra` check fails, the **platform** is broken — the report says so and
  says to fix that first. Milestone failures on a broken platform are noise.
  Run `mise run test infra` in isolation to focus on it.
- A **skip is not a failure**: the report prints why. The `m0` operator-readiness
  check self-skips while no released operator image exists on GHCR (the chart
  pins by digest; expected pre-v0.0.1). Once a release exists:
  `CRUCIBLE_EXPECT_OPERATOR_READY=true mise run test m0`.
- Need the raw Ginkgo stream to debug a failure? `mise run test-verbose`
  (optionally with a label: `mise run test-verbose m0`).
- Other debug helpers: `mise run status`, `mise run ssh crucible-worker-1`,
  `mise run kubeconfig`, and
  `kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph -s`.

## Evolving the suite (per milestone)

Add `test/crucible/tests/m<N>_test.go` with build tag `//go:build crucible` and
`Label("m<N>")`. The report auto-discovers the new label and gives it its own
`Milestone M<N>` section (extend `areaTitle` in `report_test.go` for a nicer
heading + question). Reuse the suite helpers (`ensureNamespace`,
`startPVCConsumer`, `snapshotAndWaitReady`) and the seeded namespaces (`c-web`,
`c-db`, `c-media`, `c-legacy`, `c-edge`, `c-empty` — data volumes carry
`MANIFEST.sha256` for integrity checks). Old labels must stay green — the suite
only grows.
