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
#   CRUCIBLE_TIMEOUT=3h              whole-suite go-test budget (default 90m)
#
# Exits non-zero when any spec fails (so automation and the skill can detect it).
set -uo pipefail

CRUCIBLE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "${CRUCIBLE_DIR}/../.." && pwd)"
# shellcheck source=scripts/load-env.sh
source "${CRUCIBLE_DIR}/scripts/load-env.sh"

LABELS="${1:-${CRUCIBLE_LABELS:-}}"
export KUBECONFIG="${KUBECONFIG:-${CRUCIBLE_ARTIFACTS:-${CRUCIBLE_DIR}/artifacts}/kubeconfig}"

if [[ ! -f "${KUBECONFIG}" ]]; then
  echo "FATAL: no kubeconfig at ${KUBECONFIG} — run 'mise run up' (or 'mise run cluster') first." >&2
  exit 1
fi

# Per-lane, so parallel lanes do not delete each other's report at startup.
_artifacts="${CRUCIBLE_ARTIFACTS:-${CRUCIBLE_DIR}/artifacts}"
mkdir -p "${_artifacts}"
export CRUCIBLE_REPORT_PATH="${_artifacts}/crucible-report.md"
rm -f "${CRUCIBLE_REPORT_PATH}"

# Whole-suite wall-clock budget. 60m was overrun by the M3 full-suite run (go test panicked
# mid-flight, losing the report), so it became 90m; M6's restore-fidelity gate then added the
# heaviest lane in the suite — it backs up and restores ~600 MiB of real data (plus 2.5 GiB of
# sparse apparent size) on Ceph RBD and hashes it twice — so the default is now 120m. The suite's
# own Eventually deadlines, not this, are what should fail a spec; this only exists so a slow run
# is not TRUNCATED, which is worse than no run. Override for a long debugging run:
#   CRUCIBLE_TIMEOUT=3h mise run test
CRUCIBLE_TIMEOUT="${CRUCIBLE_TIMEOUT:-120m}"

# BOTH timeouts, and the Ginkgo one is the load-bearing fix: Ginkgo enforces its OWN suite
# timeout (default 1h) independently of `go test -timeout`. Passing only the go-test budget let a
# 120m fanout lane announce "timeout 120m" and still be cut by Ginkgo at exactly 3600s — 36 of 46
# specs run, the rest reported as failed/skipped when they were merely truncated. A truncated run
# is worse than no run.
ginkgo_args=(--ginkgo.label-filter="${LABELS}" --ginkgo.timeout="${CRUCIBLE_TIMEOUT}")
[[ "${CRUCIBLE_VERBOSE:-}" == "1" ]] && ginkgo_args+=(--ginkgo.v)

echo "==> running crucible suite  (labels: '${LABELS:-<all>}', timeout ${CRUCIBLE_TIMEOUT})"
cd "${REPO_ROOT}"
# The go-test budget gets 5 extra minutes so Ginkgo's timeout always fires FIRST: Ginkgo
# interrupts gracefully (per-spec verdicts, report written), while go test panics the whole
# binary and loses the report.
go test -tags crucible ./test/crucible/tests -timeout "$(awk -v t="${CRUCIBLE_TIMEOUT}" '
  BEGIN{
    mins=0
    if (t ~ /^[0-9]+h$/)      mins=int(t)*60
    else if (t ~ /^[0-9]+m$/) mins=int(t)
    else if (t ~ /^[0-9]+h[0-9]+m$/){split(t,a,/h/); mins=int(a[1])*60+int(a[2])}
    if (mins==0) mins=90
    printf "%dm", mins+5
  }')" -args "${ginkgo_args[@]}"
rc=$?

# The readable report is the last thing the operator sees — `go test` hides a
# passing binary's stdout, so we print the file the suite always writes.
if [[ -f "${CRUCIBLE_REPORT_PATH}" ]]; then
  echo
  echo "────────────────────────────────────────────────────────────────────────"
  cat "${CRUCIBLE_REPORT_PATH}"
  echo "────────────────────────────────────────────────────────────────────────"
  echo "(report saved to ${CRUCIBLE_REPORT_PATH})"
else
  echo "WARNING: no report produced — the suite likely failed to start (see output above)." >&2
fi

exit "${rc}"
