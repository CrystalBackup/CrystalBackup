#!/usr/bin/env bash
# Crucible — seed the tenant namespaces with the workload/storage case matrix,
# then wait until every seed is fully materialized (data written + checksummed).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CRUCIBLE_DIR="$(dirname "${SCRIPT_DIR}")"
export KUBECONFIG="${KUBECONFIG:-${CRUCIBLE_DIR}/artifacts/kubeconfig}"

step() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }

step "Applying seed manifests"
kubectl apply -f "${SCRIPT_DIR}/manifests/"

step "Waiting for workloads"
kubectl -n c-web rollout status deploy/web --timeout=300s
kubectl -n c-db rollout status statefulset/db --timeout=600s
kubectl -n c-media wait job/media-writer --for=condition=Complete --timeout=600s
kubectl -n c-media rollout status deploy/media-readers --timeout=300s
kubectl -n c-legacy rollout status deploy/legacy-app --timeout=300s
kubectl -n c-edge rollout status deploy/edge-app --timeout=600s
# c-edge/edge-dormant is scaled to zero on purpose — nothing to wait for.

step "Waiting for every PVC to be Bound"
for ns in c-db c-media c-legacy c-edge; do
  kubectl -n "${ns}" wait pvc --all --for=jsonpath='{.status.phase}'=Bound --timeout=300s
done

step "Seed summary"
for ns in c-web c-db c-media c-legacy c-edge c-empty; do
  echo "--- ${ns}"
  kubectl -n "${ns}" get pods,pvc 2>/dev/null | sed 's/^/    /' || true
done

echo
echo "Seeded. Data manifests (sha256) live at /data/MANIFEST.sha256 (or /media) inside each volume."
