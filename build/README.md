# Building the operator, mover & sync images

The authoritative, multi-arch, signed build is CI: [`.github/workflows/images.yml`](../.github/workflows/images.yml)
(apko/Wolfi/SLSA, [adr/0012](../spec/adr/0012-container-images-apko-wolfi-slsa.md)). **This page is the
_local_ equivalent** — how to build a throwaway `:dev` image on a laptop and deploy it onto the
[crucible](../test/crucible/) to iterate before a release exists.

## The model (read this first)

melange here **does not compile anything**. The Go binary is cross-compiled with `go build`, then
melange only **wraps that pre-built binary into a signed apk**; apko assembles the apk(s) onto a
Wolfi/glibc base into an OCI image. So every build is three steps: `go build` → `melange build`
(wrap) → `apko publish` (assemble + push).

Three images:

| Image | melange wraps | extra |
|-------|---------------|-------|
| **operator** (`build/{melange,apko}/operator.yaml`) | the `manager` binary (`./cmd`) | — |
| **mover** (`build/{melange,apko}/mover.yaml`) | the `crystal-mover` binary (`./cmd/crystal-mover`) | **also needs `restic` built from source** (`build/melange/restic.yaml`), which apko pins as `restic=0.19.1-r3` |
| **sync** (`build/{melange,apko}/sync.yaml`) | the **same** `crystal-mover` binary | the same pinned `restic`, **plus `rclone`** built from source too (`build/melange/rclone.yaml`), which apko pins as `rclone=1.75.0-r2` |

> **The mover and sync are the slow ones** (they compile restic from source under emulation). They
> change rarely — the operator computes the restic arguments, the mover just runs `restic`. **Build
> them once, reuse their digests across operator iterations.** Only rebuild when
> `cmd/crystal-mover` or `internal/mover` changes.

**Why sync is a separate image and not a bigger mover.** External sync is the one operation that
opens two repositories with two different sets of object-storage credentials, and restic cannot do
that through its own s3 backend — one process, one credential set — so both repositories are
addressed as `rclone:<remote>:…` ([adr/0013](../spec/adr/0013-external-backup-sync.md)). That makes
rclone a hard requirement of sync and of nothing else. Folding it into the mover would put its
vulnerability surface in front of every backup and restore; this project's release gate has already
blocked once on a transitive dependency of restic. The two images share a binary and a recipe
shape, so the third leg costs one apko file and one melange file.

Since sync and mover carry the same binary, a change to `cmd/crystal-mover` or `internal/mover`
invalidates **both** digests. A change to *only* rclone or the sync assembly invalidates just sync.

### melange can skip the rebuild and still exit 0

While preparing 0.6.3 the melange build step was run with its output silenced (`>/dev/null 2>&1`)
to keep the transcript readable. melange decided the package was already up to date, skipped the
rebuild, and reused an `.apk` from three days earlier — exit code 0, and no output left to
contradict it. apko then assembled and pushed an image from that stale package, and the published
`:dev` digest came out byte-identical to 0.6.2's (`sha256:46706810…`). Nothing failed anywhere in
the chain. The next step would have been a two-hour, ~€1/h crucible campaign run against the
*previous* release's operator, and its green report would have been filed as validation of the new
one. Deleting the stale `.apk` and rebuilding with the output visible produced `sha256:8d470029…`.

Three rules come out of that:

1. **Never silence a build step whose staleness is invisible in its exit code.** melange reports
   "up to date" as success, so a silenced melange cannot be told apart from a melange that did the
   work.
2. **Check the `.apk` mtime after the build, before you push.** It is the cheapest possible proof
   that the thing you are about to ship was built from the code you just changed.
3. **An unchanged image digest after a code change is an ALERT, not a convenience.** Two builds of
   the same source are reproducible and *should* match; a build after an edit that matches is a
   build that did not happen.

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

# 5. resolve the digest to DEPLOY — the INDEX the :dev tag points to.
#    Use imagetools, NOT `head image-refs.txt` (which can be a per-arch child digest).
#    Parse the PLAIN output: `--format '{{.Manifest.Digest}}'` is not honoured by every buildx
#    version — some print the whole inspect instead, which is non-empty and therefore slips past
#    a "did I get something?" check. The first Digest line is the index's.
OPERATOR_DIGEST="$(docker buildx imagetools inspect "$REG/operator:dev" | awk '/^Digest:/{print $2; exit}')"
echo "operator@$OPERATOR_DIGEST"
```

## Build the mover image (only when the mover changed)

```bash
# 1. cross-compile crystal-mover
mkdir -p stage-x86_64
GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  mise exec -- go build -trimpath -ldflags="-s -w" -o stage-x86_64/crystal-mover ./cmd/crystal-mover

# 2. build restic FROM SOURCE into the local apk repo — the slow step (minutes under QEMU).
#    Produces packages/x86_64/restic-0.19.1-r3.apk, which the apko pin restic=0.19.1-r3 selects.
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

MOVER_DIGEST="$(docker buildx imagetools inspect "$REG/mover:dev" | awk '/^Digest:/{print $2; exit}')"
echo "mover@$MOVER_DIGEST"
```

## Build the sync image (only when the mover binary or rclone changed)

Steps 1–3 of the mover build produce everything this one needs — the same `crystal-mover` binary and
the same restic apk — so if you have just built the mover, **skip straight to the lock+publish
below**, after the rclone step. rclone IS a melange build here — Wolfi's package carried
advisories with no published fix, so the recipe tracks the newest upstream release and overrides
what upstream has not fixed yet ([adr/0013](../spec/adr/0013-external-backup-sync.md)).

```bash
# Steps 1–3 are the mover's, unchanged. Then build rclone from source — same shape as restic, and
# for the same reason: we ship this binary, so we pin and patch its source (adr/0013).
melange build build/melange/rclone.yaml \
  --arch x86_64 --runner docker \
  --signing-key melange.rsa --out-dir ./packages

apko lock build/apko/sync.yaml \
  --arch x86_64 -r ./packages -k "$PWD/melange.rsa.pub" --output apko.lock.json
apko publish build/apko/sync.yaml "$REG/sync:dev" \
  --arch x86_64 --lockfile apko.lock.json \
  -r ./packages -k "$PWD/melange.rsa.pub" \
  --sbom-path ./sbom --image-refs image-refs.txt

SYNC_DIGEST="$(docker buildx imagetools inspect "$REG/sync:dev" | awk '/^Digest:/{print $2; exit}')"
echo "sync@$SYNC_DIGEST"
```

## Deploy onto the crucible

`test/crucible/deploy/deploy.sh` reads all three digests from the environment and passes them to the
chart (`--set image.digest`, `--set mover.image.digest`, `--set sync.image.digest`; the chart's
`_helpers.tpl` prefers digest over tag):

```bash
OPERATOR_IMAGE_DIGEST="$OPERATOR_DIGEST" \
MOVER_IMAGE_DIGEST="$MOVER_DIGEST" \
SYNC_IMAGE_DIGEST="$SYNC_DIGEST" \
  test/crucible/deploy/deploy.sh
```

Leaving `SYNC_IMAGE_DIGEST` empty is only safe when nothing on the cluster syncs: the chart falls
back to a **placeholder digest**, and because a sync image is pulled by nothing until an
`ExternalSync` exists, the mistake is silent right up to the moment a copy Job sits in
`ImagePullBackOff`. The crucible's `m5` suite fails fast on the placeholder rather than waiting it
out.

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
| `apko lock` can't resolve `restic=0.19.1-r3` | the restic apk isn't in `./packages` (or is a different version). Re-run the restic melange build (mover step 2); the apko pin in `build/apko/mover.yaml` must equal `restic.yaml`'s `version`-r`epoch`. |
| mover Jobs `ImagePullBackOff`, or the operator runs old code | you deployed `head image-refs.txt` instead of the tag's INDEX digest. Always resolve it with `docker buildx imagetools inspect …:dev \| awk '/^Digest:/{print $2; exit}'` — and not the `--format '{{.Manifest.Digest}}'` template, which some buildx versions ignore, printing the entire inspect instead. The release workflow signed an amd64 child manifest for four releases because of exactly this. |
| `denied` on `apko publish` | `docker login ghcr.io` with a `write:packages` token (packages are public to pull, but pushing needs auth). |
| stale `./packages` after a version bump | `rm -rf ./packages apko.lock.json` and rebuild; melange appends to the local apk index, so a leftover old version can shadow the new one. |

## Notes

- `stage-*/`, `packages/`, `sbom/`, `apko.lock.json`, `image-refs.txt`, `melange.rsa*` are build
  artifacts — keep them out of commits (they're git-ignored).
- The `:dev` tag is a moving pointer; the crucible always deploys by **digest**, so a new `:dev`
  push never disturbs a running cluster until you `helm upgrade` to the new digest.
- Everything here is repo-relative and location-independent; only `DOCKER_HOST` is machine-specific
  (your Rancher Desktop socket).
