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

# TF_WORKSPACE, NOT `workspace select`, and that distinction is the whole reason lanes can run
# concurrently at all.
#
# `workspace select` persists the choice in .terraform/environment — a file SHARED by every
# process using this configuration. Three lanes selecting at once race on it and on the state
# lock, which is exactly how the first parallel fanout died: two lanes failed with "Error
# acquiring the state lock" while the third quietly provisioned under whichever workspace won the
# write. TF_WORKSPACE is per-process and touches no shared file.
export TF_WORKSPACE="${workspace}"

# `init` must run before a workspace can exist, and TF_WORKSPACE pointing at a missing workspace
# is an error rather than an implicit create — so init also materialises it. Serial by
# construction: fanout creates every lane's workspace before it forks.
if [[ "${1:-}" == "init" ]]; then
  tofu -chdir="${TF_DIR}" init "${@:2}"
  if [[ "${workspace}" != "default" ]]; then
    # `workspace new` is the one command that must NOT see TF_WORKSPACE set to the workspace it is
    # about to create, or tofu refuses with "workspace does not exist".
    TF_WORKSPACE="" tofu -chdir="${TF_DIR}" workspace new "${workspace}" 2>/dev/null \
      || echo "==> workspace '${workspace}' already exists" >&2
  fi
  exit 0
fi

echo "==> tofu ${1:-} in workspace '${workspace}'" >&2
exec tofu -chdir="${TF_DIR}" "$@"
