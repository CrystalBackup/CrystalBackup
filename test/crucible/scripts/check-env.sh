#!/usr/bin/env bash
# Verify the crucible credentials load from <repo>/.secrets/ — never prints values.
set -euo pipefail
CRUCIBLE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/load-env.sh
source "${CRUCIBLE_DIR}/scripts/load-env.sh"

fail=0
[[ -n "${HCLOUD_TOKEN:-}" ]]          || { echo "MISSING: HCLOUD_TOKEN        (.secrets/HETZNER_TOKEN)"; fail=1; }
[[ -n "${TF_VAR_s3_access_key:-}" ]]  || { echo "MISSING: S3 access/secret     (.secrets/HETZNER_S3)"; fail=1; }
[[ -f "${ANSIBLE_PRIVATE_KEY_FILE:-/nonexistent}" ]] \
                                      || { echo "MISSING: SSH private key       (.secrets/crystalbackup.key)"; fail=1; }

if (( fail )); then
  echo
  echo "Credentials incomplete — see test/crucible/secrets.example/README.md for the expected layout."
  exit 1
fi
echo "credentials OK (HCLOUD_TOKEN, S3 keys, SSH key all present)"
