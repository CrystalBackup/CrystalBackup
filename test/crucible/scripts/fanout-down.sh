#!/usr/bin/env bash
# Destroy N lanes. The counterpart to fanout.sh, and the reason it exists separately: a fanout
# that fails partway leaves lanes standing, and the recovery must not depend on the script that
# just failed.
#
#   CONFIRM=yes scripts/fanout-down.sh 3
set -euo pipefail

CRUCIBLE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${CRUCIBLE_DIR}"

N="${1:-2}"
if [[ "${CONFIRM:-}" != "yes" ]]; then
  echo "fanout-down: refusing without CONFIRM=yes (would destroy ${N} lanes)." >&2
  exit 1
fi

# Serial, not parallel. Teardown is the step that must not be clever: N concurrent destroys
# against one Hetzner account hit rate limits, and a rate-limited destroy leaves servers behind
# while reporting success.
LANE_PREFIX="${LANE_PREFIX:-l}"
failed=()
for i in $(seq 1 "${N}"); do
  lane="${LANE_PREFIX}${i}"
  echo "==> destroying lane ${lane}"
  if ! CRUCIBLE_LANE="${lane}" CONFIRM=yes mise run down; then
    failed+=("${lane}")
  fi
done

if (( ${#failed[@]} > 0 )); then
  echo >&2
  echo "fanout-down: FAILED for: ${failed[*]}" >&2
  echo "fanout-down: those lanes may still be billing. Check the Hetzner console, or use" >&2
  echo "             CRUCIBLE_LANE=<lane> mise run nuke  (label-based, needs no state)." >&2
  exit 1
fi

echo
echo "All ${N} lanes destroyed. Verify nothing lingers:"
for i in $(seq 1 "${N}"); do echo "  CRUCIBLE_LANE=${LANE_PREFIX}${i} mise run status"; done
