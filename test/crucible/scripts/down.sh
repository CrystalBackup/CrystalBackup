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

"${CRUCIBLE_DIR}/scripts/tofu-lane.sh" destroy -auto-approve
rm -f "${CRUCIBLE_DIR}/artifacts/kubeconfig" "${CRUCIBLE_DIR}/artifacts/crucible.env"
echo
echo "Crucible destroyed."

# Verify rather than reassure. This used to print "terraform emptied and removed the
# bucket too; verify in the Hetzner console" — and terraform's destroy provisioner
# swallowed its own failure, so the sentence was routinely false: 32 buckets had piled
# up by 2026-08-02, 29 of them empty. Anything this script cannot confirm, it now
# checks; anything it cannot check, it says plainly.
# crucible.env stores S3_ENDPOINT WITH its scheme, and is gone once a previous `down` ran —
# so normalise both. The old check prefixed a second "https://", aws failed, the failure was
# piped into `grep -c` and read as "0 buckets left": it reassured on every run, including
# with a bucket still there.
endpoint="${S3_ENDPOINT:-${TF_VAR_s3_endpoint:-fsn1.your-objectstorage.com}}"
endpoint="https://${endpoint#https://}"

if ! listing="$(aws s3 ls --endpoint-url "${endpoint}" 2>&1)"; then
  echo
  echo "WARNING: could not list buckets on ${endpoint} — leftover crucible-* buckets NOT checked:"
  echo "${listing}" | tail -3 | sed 's/^/  /'
  exit 0
fi
leftovers="$(echo "${listing}" | awk '{print $3}' | grep '^crucible-' || true)"

if [[ -z "${leftovers}" ]]; then
  echo "No crucible-* bucket left on ${endpoint}."
else
  echo
  echo "WARNING: $(echo "${leftovers}" | wc -l | tr -d ' ') crucible-* bucket(s) still on ${endpoint}:"
  echo "${leftovers}" | sed 's/^/  /'
  echo
  echo "Some may belong to OTHER lanes still running — check before deleting."
  echo "Hetzner is eventually consistent about a bucket's object count, so a"
  echo "DeleteBucket right after emptying can fail with BucketNotEmpty and succeed"
  echo "~30s later. To clear one:"
  echo "  aws s3 rm s3://<bucket> --endpoint-url ${endpoint} --recursive"
  echo "  aws s3api delete-bucket --bucket <bucket> --endpoint-url ${endpoint}"
fi
