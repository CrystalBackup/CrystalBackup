# shellcheck shell=bash
# Source me — never execute: `source scripts/lane.sh`
#
# Resolves ONE optional variable, CRUCIBLE_LANE, into the paths and the tofu workspace that
# isolate one crucible from its siblings. Everything else in the harness reads what this exports
# and needs no lane awareness of its own.
#
#   CRUCIBLE_LANE unset/empty  →  the default workspace, artifacts/, hosts.ini
#                                 IDENTICAL to the harness before lanes existed.
#   CRUCIBLE_LANE=b            →  workspace "b", artifacts/lanes/b/, hosts-b.ini
#
# Why lanes exist: running the suite on several independent clusters at once turns flakiness from
# an argument into a measurement. A spec failing in one lane of three is timing; failing in all
# three is a bug. Answering that question in one 50-minute round instead of three sequential ones
# is worth the extra euro.
#
# The lane name lands in Hetzner resource names, so keep it short and DNS-ish.

_lane="${CRUCIBLE_LANE:-}"

if [[ -n "${_lane}" && ! "${_lane}" =~ ^[a-z0-9]([a-z0-9-]{0,10}[a-z0-9])?$ ]]; then
  echo "lane: CRUCIBLE_LANE='${_lane}' is not usable — lowercase letters, digits and dashes, 1-12 chars." >&2
  echo "      It becomes part of Hetzner server names (crucible-<lane>-master-1), which are DNS labels." >&2
  return 1 2>/dev/null || exit 1
fi

# The tofu workspace IS the isolation: it is what keeps two lanes from writing each other's state.
export CRUCIBLE_WORKSPACE="${_lane:-default}"

# Where this lane's kubeconfig, crucible.env and report live. Relative paths are resolved against
# the crucible dir by whoever sources this, so keep it absolute here.
_lane_root="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")/.." && pwd)"
if [[ -n "${_lane}" ]]; then
  export CRUCIBLE_ARTIFACTS="${_lane_root}/artifacts/lanes/${_lane}"
else
  export CRUCIBLE_ARTIFACTS="${_lane_root}/artifacts"
fi
mkdir -p "${CRUCIBLE_ARTIFACTS}"

# Ansible needs the same directory, and terraform computes it independently from the workspace —
# the two MUST agree or the kubeconfig lands where nothing looks for it.
export CRUCIBLE_ARTIFACTS_DIR="${CRUCIBLE_ARTIFACTS}"

# The Ansible inventory terraform writes for this lane. Exported so the `cluster` task can point
# ansible-playbook at it without repeating the lane logic.
if [[ -n "${CRUCIBLE_WORKSPACE}" && "${CRUCIBLE_WORKSPACE}" != "default" ]]; then
  export CRUCIBLE_INVENTORY="inventory/hosts-${CRUCIBLE_WORKSPACE}.ini"
else
  export CRUCIBLE_INVENTORY="inventory/hosts.ini"
fi

# KUBECONFIG is exported here rather than left to mise.toml so that a lane shell, a fanout child
# and a hand-run kubectl all agree without anyone remembering to set it.
export KUBECONFIG="${CRUCIBLE_ARTIFACTS}/kubeconfig"

unset _lane _lane_root
