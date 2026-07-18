#!/usr/bin/env bash
# Last-resort teardown: delete every crucible resource by LABEL, without
# terraform state. Use when the tfstate is lost/corrupted — otherwise prefer
# `CONFIRM=yes mise run down`.
#
# Requires HCLOUD_TOKEN (source scripts/load-env.sh) and the hcloud CLI.
# NOTE: the S3 bucket is NOT touched (backup data safety) — delete it manually
# if needed.
set -euo pipefail

SELECTOR="${CRUCIBLE_LABEL_SELECTOR:-project=crystalbackup-crucible}"

if [[ -z "${HCLOUD_TOKEN:-}" ]]; then
  echo "FATAL: HCLOUD_TOKEN not set — source scripts/load-env.sh first." >&2
  exit 1
fi

echo "Resources labeled '${SELECTOR}':"
echo "--- servers"
hcloud server list -l "${SELECTOR}"
echo "--- volumes"
hcloud volume list -l "${SELECTOR}"
echo "--- networks"
hcloud network list -l "${SELECTOR}"
echo "--- firewalls"
hcloud firewall list -l "${SELECTOR}"
echo "--- placement groups"
hcloud placement-group list -l "${SELECTOR}"
echo

read -r -p "Type 'crucible' to DELETE everything listed above: " answer
if [[ "${answer}" != "crucible" ]]; then
  echo "Aborted."
  exit 1
fi

# Servers first (their deletion detaches volumes and frees the firewall/network).
for id in $(hcloud server list -l "${SELECTOR}" -o noheader -o columns=id); do
  echo "delete server ${id}"
  hcloud server delete "${id}"
done

for id in $(hcloud volume list -l "${SELECTOR}" -o noheader -o columns=id); do
  echo "delete volume ${id}"
  hcloud volume delete "${id}"
done

for id in $(hcloud firewall list -l "${SELECTOR}" -o noheader -o columns=id); do
  echo "delete firewall ${id}"
  hcloud firewall delete "${id}"
done

for id in $(hcloud network list -l "${SELECTOR}" -o noheader -o columns=id); do
  echo "delete network ${id}"
  hcloud network delete "${id}"
done

for id in $(hcloud placement-group list -l "${SELECTOR}" -o noheader -o columns=id); do
  echo "delete placement group ${id}"
  hcloud placement-group delete "${id}"
done

echo "Done. Reminder: the S3 bucket (if any) was left untouched."
