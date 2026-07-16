---
name: crucible
description: Drive the real-conditions e2e suite on Hetzner Cloud (test/crucible) — provision RKE2 + rook-ceph + longhorn + local-path, seed tenant workloads, run milestone-labeled Ginkgo tests, tear down. Use when asked to run live/field tests, validate a milestone in real conditions, check non-regressions, or reproduce an issue on real infrastructure.
---

# Crucible — run Crystal Backup's real-conditions e2e

Everything is driven from `test/crucible/` via its Makefile. Full context:
`test/crucible/README.md`.

## Ground rules

1. **Money**: `make up` creates ~€0.15/hour of Hetzner resources (≈€3.5/day).
   Confirm with the user before provisioning unless they just asked for it,
   and always propose `make down CONFIRM=yes` when the work is done. If a
   session ends with infrastructure still up, SAY SO explicitly.
2. **Secrets**: credentials live in `<repo>/.secrets/` (git-ignored; layout in
   `test/crucible/secrets.example/`). Never print, copy, or commit their
   values. If they're missing, point the user at the example dir — do not ask
   for values in chat.
3. **Tools**: run `mise install` in `test/crucible/` first. Invoke everything
   through mise (per project convention), e.g.
   `mise exec -- make -C test/crucible <target>` or cd + `mise exec -- make …`.
4. **Never `make nuke` or `make down` without the user asking for teardown.**

## Phases (all idempotent — re-run after fixing)

```sh
cd test/crucible
mise install                  # once
make check-env                # credentials sanity
make up                       # infra (tofu) -> cluster (ansible/RKE2) -> components (deploy.sh)
make seed                     # tenant namespaces + checksummed data
make test                     # whole suite
make test LABELS=m0           # one milestone (labels: infra, m0, m1, ...)
make down CONFIRM=yes         # ALWAYS at the end (or make nuke if tfstate lost)
```

Granular: `make infra` / `make cluster` / `make components` — prefer re-running
a single failed phase over `up`. Expect `up` to take 15–25 min (Ceph OSD
prepare dominates); use long Bash timeouts or background execution.

## Interpreting results

- Run `LABELS=infra` first when anything looks off: if infra specs fail, the
  PLATFORM is broken — fix that before reading product specs.
- The `m0` operator-readiness spec self-skips while no released operator image
  exists on GHCR (chart pins by digest; expected pre-v0.0.1). Once a release
  exists: `CRUCIBLE_EXPECT_OPERATOR_READY=true make test LABELS=m0`.
- Debug helpers: `make status`, `make ssh HOST=crucible-worker-1`,
  `make kubeconfig`, `kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph -s`.

## Evolving the suite (per milestone)

Add `test/crucible/tests/m<N>_test.go` with build tag `//go:build crucible`
and `Label("m<N>")`. Reuse the suite helpers (`ensureNamespace`,
`startPVCConsumer`, `snapshotAndWaitReady`) and the seeded namespaces
(`c-web`, `c-db`, `c-media`, `c-legacy`, `c-edge`, `c-empty` — data volumes
carry `MANIFEST.sha256` for integrity checks). Old labels must stay green —
the suite only grows.
