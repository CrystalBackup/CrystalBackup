#!/usr/bin/env bash
# Destroy ALL crucible infrastructure with terraform. Guarded — requires:
#   CONFIRM=yes mise run down
set -euo pipefail
CRUCIBLE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/load-env.sh
source "${CRUCIBLE_DIR}/scripts/load-env.sh"

if [[ "${CONFIRM:-}" != "yes" ]]; then
  echo "Refusing to destroy the crucible."
  echo "Re-run with:  CONFIRM=yes mise run down"
  echo "(tfstate lost? use 'mise run nuke' — label-based teardown.)"
  exit 1
fi

tofu -chdir="${CRUCIBLE_DIR}/terraform" destroy -auto-approve
rm -f "${CRUCIBLE_DIR}/artifacts/kubeconfig" "${CRUCIBLE_DIR}/artifacts/crucible.env"
echo
echo "Crucible destroyed. If the S3 bucket held backups, terraform emptied and removed it too;"
echo "verify in the Hetzner console that no Object Storage bucket lingers."
