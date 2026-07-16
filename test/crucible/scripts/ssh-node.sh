#!/usr/bin/env bash
# SSH into a crucible node by inventory name, e.g.:
#   mise run ssh crucible-master-1
#   mise run ssh crucible-worker-2
set -euo pipefail
CRUCIBLE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/load-env.sh
source "${CRUCIBLE_DIR}/scripts/load-env.sh"

host="${1:-}"
inv="${CRUCIBLE_DIR}/ansible/inventory/hosts.ini"
if [[ -z "${host}" ]]; then
  echo "usage: mise run ssh <node>   (names are in ${inv})"
  [[ -f "${inv}" ]] && { echo "known nodes:"; awk '/^crucible-/{print "  "$1}' "${inv}"; }
  exit 2
fi
[[ -f "${inv}" ]] || { echo "no inventory yet (${inv}) — run 'mise run infra' first."; exit 1; }

ip="$(awk -v h="${host}" '$1==h{for(i=2;i<=NF;i++) if($i ~ /^ansible_host=/){sub("ansible_host=","",$i); print $i}}' "${inv}")"
[[ -n "${ip}" ]] || { echo "unknown node '${host}' (see ${inv})"; exit 1; }

exec ssh -i "${ANSIBLE_PRIVATE_KEY_FILE}" -o StrictHostKeyChecking=no root@"${ip}"
