# Building the operator & mover images

The authoritative, multi-arch, signed build is CI: [`.github/workflows/images.yml`](../.github/workflows/images.yml)
(apko/Wolfi/SLSA, [adr/0012](../spec/adr/0012-container-images-apko-wolfi-slsa.md)). **This page is the
_local_ equivalent** — how to build a throwaway `:dev` image on a laptop and deploy it onto the
[crucible](../test/crucible/) to iterate before a release exists.

## The model (read this first)

melange here **does not compile anything**. The Go binary is cross-compiled with `go build`, then
melange only **wraps that pre-built binary into a signed apk**; apko assembles the apk(s) onto a
Wolfi/glibc base into an OCI image. So every build is three steps: `go build` → `melange build`
(wrap) → `apko publish` (assemble + push).

Two images:

| Image | melange wraps | extra |
|-------|---------------|-------|
| **operator** (`build/{melange,apko}/operator.yaml`) | the `manager` binary (`./cmd`) | — |
| **mover** (`build/{melange,apko}/mover.yaml`) | the `crystal-mover` binary (`./cmd/crystal-mover`) | **also needs `restic` built from source** (`build/melange/restic.yaml`), which apko pins as `restic=0.19.1-r0` |

> **The mover is the slow one** (it compiles restic from source under emulation). It changes rarely
> — the operator computes the restic arguments, the mover just runs `restic`. **Build the mover once,
> reuse its digest across operator iterations.** Only rebuild it when `cmd/crystal-mover` or
> `internal/mover` changes.

## Prerequisites (macOS, Apple Silicon / arm64)

1. **Docker via Rancher Desktop.** melange runs its build in a container (`--runner docker`); Rancher
   Desktop's QEMU/binfmt emulates x86_64 on arm64.
2. **`DOCKER_HOST` — the #1 gotcha.** Rancher Desktop's socket is **not** at the default path, so
   melange fails with *"Cannot connect to the Docker daemon at unix:///var/run/docker.sock"* unless
   you point it there:
   ```bash
   export DOCKER_HOST="unix://$HOME/.rd/docker.sock"
   ```
3. **Go 1.26.5**, via `mise` (pinned in `mise.toml` / `go.mod`). A stray older `/usr/local/go` on
   `PATH` triggers a `GOTOOLCHAIN` mismatch, so the reliable invocation is
   `GOTOOLCHAIN=local mise exec -- go …` (a plain `go build` is fine only if your host `go` is
   ≥ 1.26.0 and first on `PATH`).
4. **apko + melange**, pinned to the versions CI uses, on `PATH`:
   ```bash
   go install chainguard.dev/apko@v1.2.25
   go install chainguard.dev/melange@v0.56.2
   export PATH="$HOME/go/bin:$PATH"
   ```
5. **A melange signing key** (one-time, writes `melange.rsa` + `melange.rsa.pub` to the repo root):
   ```bash
   melange keygen           # skip if melange.rsa already exists
   ```
6. **Logged in to GHCR** with a token that has `write:packages` (the crucible cluster pulls the image
   from GHCR, so it must be **pushed**, not just built): `docker login ghcr.io`.

### Only build x86_64

The crucible cluster is Hetzner `cpx*` (x86_64), so a dev image needs **only `--arch x86_64`**. That
halves the operator build and — crucially — avoids building restic-from-source for aarch64 under
QEMU. CI builds both arches; local dev must not.

## Build the operator image (the usual iteration)

Run from the repo root:

```bash
export DOCKER_HOST="unix://$HOME/.rd/docker.sock"
export PATH="$HOME/go/bin:$PATH"
REG=ghcr.io/crystalbackup

# 1. cross-compile the manager binary for the cluster arch
mkdir -p stage-x86_64
GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  mise exec -- go build -trimpath -ldflags="-s -w" -o stage-x86_64/manager ./cmd

# 2. wrap it into a signed apk (installs, does not compile)
melange build build/melange/operator.yaml \
  --arch x86_64 --runner docker \
  --source-dir stage-x86_64 \
  --signing-key melange.rsa --out-dir ./packages

# 3. lock package versions
apko lock build/apko/operator.yaml \
  --arch x86_64 -r ./packages -k "$PWD/melange.rsa.pub" \
  --output apko.lock.json

# 4. assemble + PUSH to GHCR under a :dev tag
mkdir -p ./sbom
apko publish build/apko/operator.yaml "$REG/operator:dev" \
  --arch x86_64 --lockfile apko.lock.json \
  -r ./packages -k "$PWD/melange.rsa.pub" \
  --sbom-path ./sbom --image-refs image-refs.txt

# 5. resolve the digest to DEPLOY — the manifest the :dev tag points to.
#    Use imagetools, NOT `head image-refs.txt` (which can be a per-arch child digest).
OPERATOR_DIGEST="$(docker buildx imagetools inspect "$REG/operator:dev" --format '{{.Manifest.Digest}}')"
echo "operator@$OPERATOR_DIGEST"
```

## Build the mover image (only when the mover changed)

```bash
# 1. cross-compile crystal-mover
mkdir -p stage-x86_64
GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  mise exec -- go build -trimpath -ldflags="-s -w" -o stage-x86_64/crystal-mover ./cmd/crystal-mover

# 2. build restic FROM SOURCE into the local apk repo — the slow step (minutes under QEMU).
#    Produces packages/x86_64/restic-0.19.1-r0.apk, which the apko pin restic=0.19.1-r0 selects.
melange build build/melange/restic.yaml \
  --arch x86_64 --runner docker \
  --signing-key melange.rsa --out-dir ./packages

# 3. wrap crystal-mover
melange build build/melange/mover.yaml \
  --arch x86_64 --runner docker \
  --source-dir stage-x86_64 \
  --signing-key melange.rsa --out-dir ./packages

# 4. lock + publish
apko lock build/apko/mover.yaml \
  --arch x86_64 -r ./packages -k "$PWD/melange.rsa.pub" --output apko.lock.json
apko publish build/apko/mover.yaml "$REG/mover:dev" \
  --arch x86_64 --lockfile apko.lock.json \
  -r ./packages -k "$PWD/melange.rsa.pub" \
  --sbom-path ./sbom --image-refs image-refs.txt

MOVER_DIGEST="$(docker buildx imagetools inspect "$REG/mover:dev" --format '{{.Manifest.Digest}}')"
echo "mover@$MOVER_DIGEST"
```

## Deploy onto the crucible

`test/crucible/deploy/deploy.sh` reads both digests from the environment and passes them to the chart
(`--set image.digest`, `--set mover.image.digest`; the chart's `_helpers.tpl` prefers digest over tag):

```bash
OPERATOR_IMAGE_DIGEST="$OPERATOR_DIGEST" \
MOVER_IMAGE_DIGEST="$MOVER_DIGEST" \
  test/crucible/deploy/deploy.sh
```

**Shortened loop for an operator-only change** (mover unchanged): rebuild the operator (steps above),
then just re-point the running deployment and re-test — no full redeploy:

```bash
helm upgrade crystal-backup charts/crystal-backup \
  --namespace crystal-backup-system --reuse-values \
  --set image.digest="$OPERATOR_DIGEST"
mise run test            # in test/crucible/  (e.g. `mise run test m1`)
```

## Troubleshooting (macOS arm64)

| Symptom | Cause / fix |
|---------|-------------|
| `Cannot connect to the Docker daemon at unix:///var/run/docker.sock` | `DOCKER_HOST` not set to the Rancher Desktop socket → `export DOCKER_HOST="unix://$HOME/.rd/docker.sock"`. |
| `melange … unable to populate workspace: open build/melange/test-dirfs-0: no such file` | melange's multi-arch test-workspace race. **Build one arch at a time** (`--arch x86_64` only, as above). CI hit this with combined `--arch x86_64,aarch64` and fixes it with a per-arch loop. |
| `go: downloading go1.26.5` / toolchain version mismatch | a stray older `/usr/local/go`. Use `GOTOOLCHAIN=local mise exec -- go …` and keep the mise go first on `PATH`. |
| `apko lock` can't resolve `restic=0.19.1-r0` | the restic apk isn't in `./packages` (or is a different version). Re-run the restic melange build (mover step 2); the apko pin in `build/apko/mover.yaml` must equal `restic.yaml`'s `version`-r`epoch`. |
| mover Jobs `ImagePullBackOff`, or the operator runs old code | you deployed `head image-refs.txt` instead of the tag's manifest digest. Always deploy `docker buildx imagetools inspect …:dev --format '{{.Manifest.Digest}}'`. |
| `denied` on `apko publish` | `docker login ghcr.io` with a `write:packages` token (packages are public to pull, but pushing needs auth). |
| stale `./packages` after a version bump | `rm -rf ./packages apko.lock.json` and rebuild; melange appends to the local apk index, so a leftover old version can shadow the new one. |

## Notes

- `stage-*/`, `packages/`, `sbom/`, `apko.lock.json`, `image-refs.txt`, `melange.rsa*` are build
  artifacts — keep them out of commits (they're git-ignored).
- The `:dev` tag is a moving pointer; the crucible always deploys by **digest**, so a new `:dev`
  push never disturbs a running cluster until you `helm upgrade` to the new digest.
- Everything here is repo-relative and location-independent; only `DOCKER_HOST` is machine-specific
  (your Rancher Desktop socket).
