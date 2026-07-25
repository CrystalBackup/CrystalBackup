#!/usr/bin/env bash
# tofu, pinned to this lane's workspace.
#
# Every tofu invocation in the harness goes through here so the workspace can never be forgotten.
# That matters more than it looks: `tofu destroy` in the wrong workspace destroys a SIBLING lane's
# cluster, and the command that does it looks exactly like the one that would have been right.
#
# With no lane selected the workspace is "default", which is where a pre-lanes state already lives
# — so an existing crucible keeps working with no migration and no import.
set -euo pipefail

CRUCIBLE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TF_DIR="${CRUCIBLE_DIR}/terraform"

# lane.sh is idempotent and cheap; sourcing it here means this script is correct even when called
# directly rather than from a task that already loaded the environment.
# shellcheck disable=SC1091
source "${CRUCIBLE_DIR}/scripts/lane.sh"

workspace="${CRUCIBLE_WORKSPACE:-default}"

# `init` has to run before a workspace can be selected, so let it through untouched.
if [[ "${1:-}" == "init" ]]; then
  exec tofu -chdir="${TF_DIR}" "$@"
fi

# select -or-create. `-or-create` is what makes `mise run up` work for a brand-new lane without a
# separate "create the lane" step; for the default workspace it is a no-op.
tofu -chdir="${TF_DIR}" workspace select -or-create "${workspace}" >/dev/null

echo "==> tofu ${1:-} in workspace '${workspace}'" >&2
exec tofu -chdir="${TF_DIR}" "$@"
