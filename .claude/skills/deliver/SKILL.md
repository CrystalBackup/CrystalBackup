---
name: deliver
description: The end-to-end loop that drives a milestone or hardening patch to green and ships it — local gates (make test/lint/e2e), build :dev images, deploy onto the crucible, iterate fix→re-test→re-crucible until green, relay the crucible report, teardown, then release (tag vX.Y.Z) and update the site. Use when asked to validate a milestone/patch in real conditions and release it, to "ship 0.x.y", to run the full crucible delivery loop, or whenever the work ends in a tagged release rather than just a test run. For the suite mechanics alone (running/reading the crucible), use the `crucible` skill instead.
---

# Deliver — validate to green, then release

This is the **delivery runbook**: the ordered loop from "code changed" to "release
tagged + site updated". It orchestrates the pieces; it does not re-explain them.

- Suite mechanics (phases, report format, evolving specs): the **`crucible` skill**
  and `test/crucible/README.md`.
- Exact `:dev` image build commands + macOS/arm64 gotchas: `build/README.md`.
- Definition of Done: `spec/08-testing-and-dod.md`.

## The loop at a glance

```
        ┌─────────────────────────────────────────────────────────────┐
        │ 1. local gates  → make test + make lint  (+ make e2e / kind) │  cheap, first
        │ 2. build :dev   → operator (always) [+ mover only if changed]│
        │ 3. deploy       → by DIGEST onto the crucible                │
        │ 4. crucible     → mise run test [label]   (FINAL gate)       │  ← ask first (€)
        │                                                             │
        │   fail? ── fix ──► re-run local gates ──► rebuild/redeploy ──┘
        │                     (test+lint locally, IN PARALLEL          │
        │                      with the slow remote crucible)          │
        │ all green ►                                                  │
        │ 5. report       → relay artifacts/crucible-report.md         │
        │ 6. teardown     → CONFIRM=yes mise run down  (verify 0 infra) │
        │ 7. release      → bump chart+CHANGELOG, tag vX.Y.Z           │  ← ask first
        │ 8. site         → reports + roadmap row, push to main        │
        └─────────────────────────────────────────────────────────────┘
```

Two human checkpoints, both because they cost or are hard to undo: **before the
crucible** (money) and **before the tag** (release). Everything else runs
unattended.

## Order rule (the user's, applied literally)

Local gates are **cheap and run first** — never spend crucible money on a tree
that fails `make test`/`make lint`. The crucible is the **last** verification.
On a crucible failure: **fix → re-run the crucible → once it's green, re-run the
non-reg tests + linter → then** teardown → release → site. Every fix re-arms both
gates; the release train only leaves when local gates AND the crucible are green
on the same tree.

## 1 — Local gates (before touching the cloud)

```sh
make test          # unit + envtest (Makefile resolves KUBEBUILDER_ASSETS to an ABSOLUTE path via LOCALBIN — use the target, not a hand-rolled `go test`)
make lint          # custom golangci-lint over the FULL tree (matches CI). NEVER a package-scoped lint — it has missed findings twice.
make e2e           # kind e2e (builds + loads images, deploys, Ginkgo). Heavier; part of the non-reg trio, can run alongside the remote crucible.
```

If `api/` or CRD validation changed: `make manifests generate` first (it runs as
part of `make test` anyway), and keep the chart CRDs under
`charts/crystal-backup/crds/` in sync with `config/crd/bases/`.

## 2 — Build the `:dev` image(s)

Full recipe + macOS/arm64 gotchas: **`build/README.md`**. The decision of *what*
to rebuild:

| changed | rebuild |
| --- | --- |
| `cmd/`, `internal/controller`, `internal/…`, `api/` | **operator** image |
| `cmd/crystal-mover`, `internal/mover` | **mover** AND **sync** images too — they carry the same binary, so one change invalidates both digests (slow: restic from source; otherwise **reuse their digests**) |
| only rclone / `build/apko/sync.yaml` | **sync** image only |
| nothing (config/chart only) | neither — `helm upgrade` is enough |

Non-negotiable prep (details in `build/README.md`): `export
DOCKER_HOST="unix://$HOME/.rd/docker.sock"`, build **`--arch x86_64` only** (the
crucible is x86_64; this avoids restic-from-source under QEMU), `docker login
ghcr.io` (the cluster pulls from GHCR, so `:dev` must be **pushed**).

Resolve the **digest** to deploy — the `:dev` tag is a moving pointer, the
crucible always deploys by digest:

```sh
REG=ghcr.io/crystalbackup
OPERATOR_DIGEST="$(docker buildx imagetools inspect "$REG/operator:dev" | awk '/^Digest:/{print $2; exit}')"
# Parse the PLAIN output. `--format '{{.Manifest.Digest}}'` is not honoured by every buildx
# version — some print the whole inspect, which is NON-EMPTY and so slips past an "is it set?"
# check. The first Digest line is the multi-arch INDEX's; the per-arch children follow it.
# Never deploy `head image-refs.txt` — it can be a per-arch child digest → ImagePullBackOff / stale code.
```

## 3 — Deploy onto the crucible (by digest)

```sh
# full redeploy (operator + mover + sync):
OPERATOR_IMAGE_DIGEST="$OPERATOR_DIGEST" MOVER_IMAGE_DIGEST="$MOVER_DIGEST" \
SYNC_IMAGE_DIGEST="$SYNC_DIGEST" \
  test/crucible/deploy/deploy.sh
# Omitting SYNC_IMAGE_DIGEST leaves the chart's PLACEHOLDER digest, which nothing pulls
# until an ExternalSync exists — so the mistake is silent until a copy Job hangs in
# ImagePullBackOff. The m5 suite's first spec fails fast on it instead.

# operator-only change (mover unchanged) — shortest loop:
helm upgrade crystal-backup charts/crystal-backup \
  --namespace crystal-backup-system --reuse-values \
  --set image.digest="$OPERATOR_DIGEST"
```

If the crucible isn't up yet: `cd test/crucible && mise run up && mise run seed`
(~15–25 min; **ask the user first — it starts ~€0.52/h of Hetzner infra**). See
the `crucible` skill for `up`/`seed` details.

## 4 — Run the crucible (final gate)

```sh
cd test/crucible
mise run test              # whole suite + readable report
mise run test m3           # one milestone label while iterating (infra, m0, m1, …)
mise run test-verbose m3   # full Ginkgo stream to debug a failure
```

**Parallelize the wait**: the crucible is remote and slow, so kick it off and run
`make test` / `make lint` locally in the meantime — that is the "unit+e2e+lint in
parallel with the crucible" the loop calls for.

Fix → the *only* things that need a fresh cluster round-trip are code changes:
rebuild the affected image (§2), redeploy by digest (§3), re-run the label. A
fix that touches only local Go gets caught by `make test`/`make lint` without
spending a cluster cycle.

## 5 — Report

`mise run test` prints a plain-language report last and saves it to
`test/crucible/artifacts/crucible-report.md` (verdict, per-area checks, failures
with a next step). **Relay that**, not raw Ginkgo. Reading rules (infra failure =
platform broken first; a skip is not a failure) are in the `crucible` skill.

When a milestone adds behavior, add its `test/crucible/tests/m<N>_test.go`
(`//go:build crucible`, `Label("m<N>")`) — old labels stay green forever
(non-regression). Details: `crucible` skill, "Evolving the suite".

## 6 — Teardown (always)

```sh
CONFIRM=yes mise run down    # terraform destroy
mise run status              # confirm 0 nodes / no outputs
# tfstate lost? mise run nuke  (label-based, typed confirmation)
```

If a session ends with infra still up, **say so explicitly**. The S3 bucket is
never auto-deleted.

## 7 — Release

Only after local gates AND the crucible are green on the same tree, and **after
the user OKs the tag**:

- bump `charts/crystal-backup/Chart.yaml` `version` + `appVersion` to `X.Y.Z`
  (milestone `M_n` → `0.n.z`; a patch within a milestone bumps `z`);
- update `CHANGELOG.md`, and version strings in `README.md`;
- commit **per lot**, and with **NO Claude attribution** (no `Co-Authored-By:
  Claude`, no "Generated with Claude Code" — hard rule);
- tag `vX.Y.Z` → `.github/workflows/images.yml` builds the signed multi-arch
  index (+ SBOM, + provenance) and publishes the chart OCI + GitHub Release.

Never commit build artifacts: `melange.rsa*` (private key), `stage-*/`,
`packages/`, `sbom/`, `apko.lock.json`, `image-refs.txt` (all git-ignored).

## 8 — Site

`website/` deploys via GitHub Pages on push to `main`. Typical update: add the
milestone's report(s) under `website/public/reports/`, and a roadmap row in
`website/src/pages/index.astro`. Verify the Pages deploy succeeded.

## Gotchas that cost time (don't rediscover)

- **envtest**: `KUBEBUILDER_ASSETS` must be an **absolute** path — a relative one
  resolves against the test package dir → `fork/exec ENOENT`. `make test` already
  does this right; only relevant if you invoke `go test` by hand.
- **controller-gen** toolchain mismatch → run it through `make` targets, or
  `GOTOOLCHAIN=local PATH="$(dirname $(mise which go)):$PATH" ./bin/controller-gen …`.
  Don't hand-run `object` gen without a copyright year — it writes a "Copyright ."
  header into `zz_generated.deepcopy.go`.
- **Image digest**: `imagetools inspect …:dev | awk '/^Digest:/{print $2; exit}'` — the FIRST
  Digest line is the multi-arch index's. Not `--format '{{.Manifest.Digest}}'` (ignored by some
  buildx versions, which then print the entire inspect — non-empty, so it passes a naive check),
  and never `head image-refs.txt` (a per-arch child). The release workflow signed an amd64 child
  for four releases on exactly that mistake.
- **kind e2e CI flake**: `kubectl` hangs → `panic: test timed out after 10m0s`.
  A kind API-server hiccup, not a regression — verify, then `gh run rerun <id>
  --failed`.
- **CEL / CRD immutability**: rules are update-only (`self.X == oldSelf.X`);
  `namespace` is a reserved word (escape `__namespace__`); optional fields need a
  `has()` guard; adding immutability can silently break existing webhook tests
  that mutate that field — grep the tests first.
- **Money**: the crucible is `mise run up` → ~€0.52/h. Ask before provisioning,
  teardown when done.
