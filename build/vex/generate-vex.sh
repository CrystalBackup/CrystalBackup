#!/usr/bin/env bash
# Generate an OpenVEX document for one Crystal Backup image, from govulncheck.
#
# spec/adr/0012. Every statement in the output is produced by `govulncheck -format openvex`
# and rewritten only for identity (who authored it, which product it is about). No statement
# is ever written, edited or re-classified by hand: a `not_affected` is a signed security
# claim about a product that holds every tenant's data, and the only thing that earns one
# here is a symbol-level reachability analysis.
#
# Why source mode and not `-mode=binary`: binary mode answers "which symbols are LINKED into
# the artifact", which is a presence analysis. Source mode answers "which symbols are CALLED
# from this main package", which is a reachability analysis — the claim we actually sign.
# The two disagree in practice: on GO-2026-5932 (a package-wide advisory on
# x/crypto/openpgp) binary mode reports `affected` because openpgp symbols survive linking,
# while source mode proves nothing calls them. We assert only what reachability proves.
#
# Two rewrites are MANDATORY for the result to be usable by a scanner; each is useless
# without the other (measured against trivy: raw doc suppresses nothing, product-only
# suppresses nothing, both together suppress correctly):
#   1. products[].@id  — govulncheck emits the literal "Unknown Product". A scanner matches
#      statements to findings by product identity, so this must become the image's OCI purl.
#   2. subcomponents[].@id — govulncheck percent-encodes the module path
#      (pkg:golang/golang.org%2Fx%2Fnet@v0.53.0); trivy emits it unescaped
#      (pkg:golang/golang.org/x/net@v0.53.0). Without unescaping, nothing matches.
#
# Usage:
#   generate-vex.sh --component <operator|mover> --output <file> [--image-ref <ref>]
#
#   --image-ref  A product identifier the statements apply to. REPEATABLE, and in practice it
#                must be repeated: a scanner keys an image by purl, and that purl carries an
#                `arch=` qualifier, so one identifier only ever covers one architecture. Pass
#                the purl of every architecture in the index (and let the caller obtain them
#                from the scanner itself rather than hand-building the string — the exact purl
#                shape is the scanner's business, not ours).
#                Absent => products keep a placeholder and the document is NOT usable by a
#                scanner yet (the generate-before-publish case).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPONENT=""
OUTPUT=""
IMAGE_REFS=()

while [ $# -gt 0 ]; do
  case "$1" in
    --component) COMPONENT="$2"; shift 2 ;;
    --output)    OUTPUT="$2";    shift 2 ;;
    --image-ref) IMAGE_REFS+=("$2"); shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[ -n "$COMPONENT" ] || { echo "--component is required" >&2; exit 2; }
[ -n "$OUTPUT" ]    || { echo "--output is required" >&2; exit 2; }
command -v govulncheck >/dev/null || { echo "govulncheck not on PATH" >&2; exit 2; }
command -v jq >/dev/null          || { echo "jq not on PATH" >&2; exit 2; }

case "$COMPONENT" in
  operator) MAIN_PKG="./cmd" ;;
  # sync ships the SAME binary as mover, plus rclone — which gets its own analysis in step 2b.
  mover|sync) MAIN_PKG="./cmd/crystal-mover" ;;
  *) echo "--component must be 'operator', 'mover' or 'sync', got: $COMPONENT" >&2; exit 2 ;;
esac

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# --- 1. Our own Go binary ----------------------------------------------------------------
echo "govulncheck: ${COMPONENT} (${MAIN_PKG})" >&2
( cd "$REPO_ROOT" && govulncheck -format openvex "$MAIN_PKG" ) > "${WORK}/component.json"

DOCS=("${WORK}/component.json")

# --- 2. restic, for the images that ship it (mover, sync) ---------------------------------
# Those images ship a restic built by melange from a pinned source tarball
# (build/melange/restic.yaml). Analyse exactly that tarball, at exactly that version and
# checksum, so the VEX statements describe the restic that actually ships rather than
# whatever restic HEAD happens to be. The version and digest are read from the melange
# recipe so there is ONE source of truth for the pin; a drift fails the build loudly.
#
# rclone — which only the sync image carries — used to be excluded here, on the grounds that it
# came from Wolfi as a maintained apk with its own CVE feed, so trivy's image scan already covered
# it against a better source of truth than ours. THAT PREMISE NO LONGER HOLDS: since adr/0013's
# arbitration we build rclone ourselves from a pinned tarball with a module override
# (build/melange/rclone.yaml), exactly as we do restic. Nobody upstream tracks the binary that
# ships, so it gets the same analysis, below, for the same reason.
#
# What has NOT changed is the no-suppression rule: neither block exists to explain a finding away.
# They analyse the OVERRIDDEN source so the VEX describes the binary that actually ships instead of
# the vanilla tarball — a reachable advisory still fails the gate.
if [ "$COMPONENT" = "mover" ] || [ "$COMPONENT" = "sync" ]; then
  RECIPE="${REPO_ROOT}/build/melange/restic.yaml"
  RESTIC_VERSION="$(sed -n 's/^  version: *"\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' "$RECIPE" | head -n1)"
  RESTIC_SHA256="$(sed -n 's/^ *expected-sha256: *\([0-9a-f]*\).*$/\1/p' "$RECIPE" | head -n1)"
  [ -n "$RESTIC_VERSION" ] && [ -n "$RESTIC_SHA256" ] || {
    echo "could not read restic version/sha256 from ${RECIPE}" >&2; exit 1; }

  echo "govulncheck: restic ${RESTIC_VERSION} (pinned source from ${RECIPE})" >&2
  TARBALL="${WORK}/restic.tar.gz"
  curl -sSLf -o "$TARBALL" \
    "https://github.com/restic/restic/releases/download/v${RESTIC_VERSION}/restic-${RESTIC_VERSION}.tar.gz"

  # sha256sum on Linux/CI, shasum on a macOS dev box — the script is meant to be runnable
  # locally too (docs/DEVELOPMENT.md), not only inside the runner.
  if command -v sha256sum >/dev/null; then
    ACTUAL_SHA="$(sha256sum "$TARBALL" | awk '{print $1}')"
  else
    ACTUAL_SHA="$(shasum -a 256 "$TARBALL" | awk '{print $1}')"
  fi
  [ "$ACTUAL_SHA" = "$RESTIC_SHA256" ] || {
    echo "restic tarball checksum mismatch" >&2
    echo "  expected (melange pin): ${RESTIC_SHA256}" >&2
    echo "  actual:                 ${ACTUAL_SHA}" >&2
    exit 1; }

  mkdir -p "${WORK}/restic-src"
  tar xzf "$TARBALL" -C "${WORK}/restic-src" --strip-components=1
  # restic ${RESTIC_VERSION} still pins modules below ten REACHABLE advisories (x/text, grpc,
  # otel/sdk, x/net, klauspost/compress — the full table is in build/melange/restic.yaml). The
  # mover image builds restic with the Wolfi r14 module set overridden past those fixes; analyse
  # the SAME overrides here so the VEX describes the restic that actually ships, not the vanilla
  # tarball. Every version below is in LOCKSTEP with the melange recipe's `go get`.
  ( cd "${WORK}/restic-src" \
      && GOFLAGS=-mod=mod go get \
           go.opentelemetry.io/otel@v1.47.0 \
           go.opentelemetry.io/otel/metric@v1.47.0 \
           go.opentelemetry.io/otel/sdk@v1.47.0 \
           go.opentelemetry.io/otel/sdk/metric@v1.47.0 \
           go.opentelemetry.io/otel/trace@v1.47.0 \
           google.golang.org/grpc@v1.85.0 \
           golang.org/x/net@v0.62.0 \
           golang.org/x/sync@v0.24.0 \
           golang.org/x/sys@v0.49.0 \
           golang.org/x/text@v0.43.0 \
           golang.org/x/crypto@v0.58.0 \
           golang.org/x/term@v0.47.0 \
           github.com/go-jose/go-jose/v4@v4.1.9 \
           github.com/klauspost/compress@v1.20.3 \
      && GOFLAGS=-mod=mod go mod tidy \
      && govulncheck -format openvex ./cmd/restic ) > "${WORK}/restic.json"
  DOCS+=("${WORK}/restic.json")
fi

# --- 2b. rclone, for the ONE image that ships it (sync) -----------------------------------
# Same shape as the restic block and for the same reason: we build this binary, so we analyse the
# source we build. At rclone 1.76.0 there is NO override, and this block mirrors that: the x/image
# and x/crypto overrides of the 1.75.0 line (GO-2026-6222, and GO-2026-6303 — reachable through the
# SFTP server, the advisory that took vex-refresh red on the published sync 0.6.7) are obsolete,
# because 1.76.0 pins x/image v0.47.0 and x/crypto v0.58.0 on its own. Forcing them would assert a
# difference this build no longer makes. The restic and rclone blocks are allowed to diverge; each
# must mirror its OWN melange recipe — re-add an override here in the same commit as in
# build/melange/rclone.yaml, never in one of the two alone.
if [ "$COMPONENT" = "sync" ]; then
  RECIPE="${REPO_ROOT}/build/melange/rclone.yaml"
  RCLONE_VERSION="$(sed -n 's/^  version: *"\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' "$RECIPE" | head -n1)"
  RCLONE_SHA256="$(sed -n 's/^ *expected-sha256: *\([0-9a-f]*\).*$/\1/p' "$RECIPE" | head -n1)"
  [ -n "$RCLONE_VERSION" ] && [ -n "$RCLONE_SHA256" ] || {
    echo "could not read rclone version/sha256 from ${RECIPE}" >&2; exit 1; }

  echo "govulncheck: rclone ${RCLONE_VERSION} (pinned source from ${RECIPE})" >&2
  TARBALL="${WORK}/rclone.tar.gz"
  curl -sSLf -o "$TARBALL" \
    "https://github.com/rclone/rclone/releases/download/v${RCLONE_VERSION}/rclone-v${RCLONE_VERSION}.tar.gz"

  if command -v sha256sum >/dev/null; then
    ACTUAL_SHA="$(sha256sum "$TARBALL" | awk '{print $1}')"
  else
    ACTUAL_SHA="$(shasum -a 256 "$TARBALL" | awk '{print $1}')"
  fi
  [ "$ACTUAL_SHA" = "$RCLONE_SHA256" ] || {
    echo "rclone tarball checksum mismatch" >&2
    echo "  expected (melange pin): ${RCLONE_SHA256}" >&2
    echo "  actual:                 ${ACTUAL_SHA}" >&2
    exit 1; }

  mkdir -p "${WORK}/rclone-src"
  tar xzf "$TARBALL" -C "${WORK}/rclone-src" --strip-components=1
  ( cd "${WORK}/rclone-src" \
      && govulncheck -format openvex . ) > "${WORK}/rclone.json"
  DOCS+=("${WORK}/rclone.json")
fi

# --- 3. Merge, re-key, and normalise ------------------------------------------------------
# `statements` is null (not []) when govulncheck has nothing to say, which is the expected
# steady state for a clean image — hence `.statements[]?` everywhere rather than `.statements[]`.
if [ "${#IMAGE_REFS[@]}" -eq 0 ]; then
  IMAGE_REFS=("urn:crystalbackup:unpublished")
fi
PRODUCTS_JSON="$(printf '%s\n' "${IMAGE_REFS[@]}" | jq -R . | jq -s .)"

jq -s \
  --argjson products "$PRODUCTS_JSON" \
  --arg component "$COMPONENT" '
  {
    "@context": "https://openvex.dev/ns/v0.2.0",
    "@id": ("https://github.com/CrystalBackup/CrystalBackup/vex/" + $component),
    author: "CrystalBackup (github.com/CrystalBackup/CrystalBackup)",
    role: "Project maintainers",
    timestamp: (.[0].timestamp // "1970-01-01T00:00:00Z"),
    version: 1,
    tooling: (.[0].tooling // "https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck"),
    statements: [
      .[] | .statements[]? |
      # Fan out: one product entry per supplied identifier, each carrying the subcomponent
      # purls of the product govulncheck reported it against, unescaped.
      .products = [
        .products[]? as $p |
        $products[] |
        {
          "@id": .,
          subcomponents: [ $p.subcomponents[]? | { "@id": (.["@id"] | gsub("%2F"; "/")) } ]
        }
      ]
    ]
  }
  # Deduplicate: the mover document merges two analyses that can legitimately reach the same
  # advisory through different modules. Keep the first occurrence per (vulnerability, status).
  | .statements |= (group_by(.vulnerability.name + "|" + .status) | map(.[0]))
  | .statements |= sort_by(.vulnerability.name)
  ' "${DOCS[@]}" > "$OUTPUT"

TOTAL="$(jq -r '.statements | length' "$OUTPUT")"
AFFECTED="$(jq -r '[.statements[] | select(.status == "affected")] | length' "$OUTPUT")"

echo "${COMPONENT}: ${TOTAL} statement(s), ${AFFECTED} affected -> ${OUTPUT}" >&2

# An `affected` statement means govulncheck proved the vulnerable code IS reachable. That is
# not something to VEX away — it is a bug report against this release. Surface it loudly; the
# caller decides whether it blocks (it should).
if [ "$AFFECTED" != "0" ]; then
  echo "::warning title=Reachable vulnerability in ${COMPONENT}::${AFFECTED} advisory/advisories are reachable and must be fixed, not VEXed"
  jq -r '.statements[] | select(.status == "affected") | "  AFFECTED  \(.vulnerability.name)  \(.vulnerability.aliases // [] | join(", "))"' "$OUTPUT" >&2
fi
