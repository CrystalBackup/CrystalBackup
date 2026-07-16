#!/usr/bin/env bash
# Crucible — run the Ginkgo suite against the live cluster and print a readable
# report last. Usage (via mise):
#
#   mise run test              # whole suite
#   mise run test m0           # one milestone label (infra, m0, m1, ...)
#   mise run test 'infra || m0'
#
# Env toggles:
#   CRUCIBLE_VERBOSE=1               stream full Ginkgo output (debugging)
#   CRUCIBLE_EXPECT_OPERATOR_READY=1 require the operator Deployment to be Available
#
# Exits non-zero when any spec fails (so automation and the skill can detect it).
set -uo pipefail

CRUCIBLE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "${CRUCIBLE_DIR}/../.." && pwd)"
# shellcheck source=scripts/load-env.sh
source "${CRUCIBLE_DIR}/scripts/load-env.sh"

LABELS="${1:-${CRUCIBLE_LABELS:-}}"
export KUBECONFIG="${KUBECONFIG:-${CRUCIBLE_DIR}/artifacts/kubeconfig}"

if [[ ! -f "${KUBECONFIG}" ]]; then
  echo "FATAL: no kubeconfig at ${KUBECONFIG} — run 'mise run up' (or 'mise run cluster') first." >&2
  exit 1
fi

mkdir -p "${CRUCIBLE_DIR}/artifacts"
export CRUCIBLE_REPORT_PATH="${CRUCIBLE_DIR}/artifacts/crucible-report.md"
rm -f "${CRUCIBLE_REPORT_PATH}"

ginkgo_args=(--ginkgo.label-filter="${LABELS}")
[[ "${CRUCIBLE_VERBOSE:-}" == "1" ]] && ginkgo_args+=(--ginkgo.v)

echo "==> running crucible suite  (labels: '${LABELS:-<all>}')"
cd "${REPO_ROOT}"
go test -tags crucible ./test/crucible/tests -timeout 60m -args "${ginkgo_args[@]}"
rc=$?

# The readable report is the last thing the operator sees — `go test` hides a
# passing binary's stdout, so we print the file the suite always writes.
if [[ -f "${CRUCIBLE_REPORT_PATH}" ]]; then
  echo
  echo "────────────────────────────────────────────────────────────────────────"
  cat "${CRUCIBLE_REPORT_PATH}"
  echo "────────────────────────────────────────────────────────────────────────"
  echo "(report saved to test/crucible/artifacts/crucible-report.md)"
else
  echo "WARNING: no report produced — the suite likely failed to start (see output above)." >&2
fi

exit "${rc}"
