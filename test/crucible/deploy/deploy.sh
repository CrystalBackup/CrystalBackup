#!/usr/bin/env bash
# Crucible — deploy the storage stack + crystal-backup onto the RKE2 cluster.
#
#   1. external-snapshotter (CSI snapshot CRDs + controller — RKE2 ships none)
#   2. rook-ceph operator + CephCluster (mon/mgr on masters, osd/mds on workers)
#      + StorageClasses/VolumeSnapshotClasses + toolbox
#   3. longhorn (snapshot-capable CSI, disks on workers only)
#   4. local-path-provisioner (NON-snapshottable class — exercises the skip path)
#   5. crystal-backup chart (CRDs + operator)
#
# Idempotent: everything is `helm upgrade --install` / `kubectl apply`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CRUCIBLE_DIR="$(dirname "${SCRIPT_DIR}")"
REPO_ROOT="$(cd "${CRUCIBLE_DIR}/../.." && pwd)"

# ---------------------------------------------------------------------------
# Version pins — bump deliberately, together with the manifests.
# ---------------------------------------------------------------------------
SNAPSHOTTER_REF="${SNAPSHOTTER_REF:-v8.2.0}"
ROOK_CHART_VERSION="${ROOK_CHART_VERSION:-v1.19.0}"
LONGHORN_CHART_VERSION="${LONGHORN_CHART_VERSION:-1.10.0}"
LOCAL_PATH_REF="${LOCAL_PATH_REF:-v0.0.30}"
CEPH_HEALTH_TIMEOUT="${CEPH_HEALTH_TIMEOUT:-1800}" # seconds

# Operator image override (no released image exists before the first v0.0.x tag;
# the deployment then stays Unavailable — the m0 tests know and tolerate that).
OPERATOR_IMAGE_DIGEST="${OPERATOR_IMAGE_DIGEST:-}"
OPERATOR_IMAGE_TAG="${OPERATOR_IMAGE_TAG:-}"
# Mover image override — the image every mover Job runs (crystal-mover + restic). REQUIRED for the
# M1 data path (repository init, backup, discovery snapshots, retention forget, unlock): a run that
# leaves this empty falls back to the chart's placeholder and every mover Job ImagePullBackOffs.
MOVER_IMAGE_DIGEST="${MOVER_IMAGE_DIGEST:-}"
MOVER_IMAGE_TAG="${MOVER_IMAGE_TAG:-}"

export KUBECONFIG="${KUBECONFIG:-${CRUCIBLE_DIR}/artifacts/kubeconfig}"

step() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }

if [[ ! -f "${KUBECONFIG}" ]]; then
  echo "FATAL: kubeconfig not found at ${KUBECONFIG} — run 'mise run cluster' first." >&2
  exit 1
fi

step "Cluster reachability"
kubectl get nodes -o wide

# ---------------------------------------------------------------------------
# CSI snapshot support: VolumeSnapshot CRDs + a snapshot-controller. Some
# distros already ship these (RKE2 bundles rke2-snapshot-controller and the
# CRDs) — re-applying them clashes, so install external-snapshotter only when
# the cluster has no VolumeSnapshot CRD yet.
step "CSI snapshot support (VolumeSnapshot CRDs + controller)"
if kubectl get crd volumesnapshots.snapshot.storage.k8s.io >/dev/null 2>&1; then
  echo "  VolumeSnapshot CRDs already present — using the cluster's snapshot-controller (external-snapshotter ${SNAPSHOTTER_REF} skipped)"
  # Ready-check whichever controller the distro provides (RKE2: rke2-snapshot-controller).
  ctrl="$(kubectl -n kube-system get deploy -o name 2>/dev/null | grep -iE 'snapshot-controller' | head -1)"
  [[ -n "${ctrl}" ]] && kubectl -n kube-system rollout status "${ctrl}" --timeout=300s || true
else
  kubectl apply -k "https://github.com/kubernetes-csi/external-snapshotter/client/config/crd?ref=${SNAPSHOTTER_REF}"
  kubectl apply -k "https://github.com/kubernetes-csi/external-snapshotter/deploy/kubernetes/snapshot-controller?ref=${SNAPSHOTTER_REF}"
  kubectl -n kube-system rollout status deploy/snapshot-controller --timeout=300s
fi

# ---------------------------------------------------------------------------
step "rook-ceph operator ${ROOK_CHART_VERSION}"
helm repo add rook-release https://charts.rook.io/release --force-update >/dev/null
helm upgrade --install rook-ceph rook-release/rook-ceph \
  --namespace rook-ceph --create-namespace \
  --version "${ROOK_CHART_VERSION}" \
  --wait --timeout 10m

step "CephCluster + pools + filesystem + toolbox"
kubectl apply -f "${SCRIPT_DIR}/manifests/ceph-cluster.yaml"
kubectl apply -f "${SCRIPT_DIR}/manifests/ceph-toolbox.yaml"

step "Waiting for Ceph HEALTH_OK (OSD prepare takes a few minutes)"
deadline=$((SECONDS + CEPH_HEALTH_TIMEOUT))
while true; do
  health="$(kubectl -n rook-ceph get cephcluster rook-ceph -o jsonpath='{.status.ceph.health}' 2>/dev/null || true)"
  phase="$(kubectl -n rook-ceph get cephcluster rook-ceph -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  echo "  cephcluster phase=${phase:-<none>} health=${health:-<none>}"
  [[ "${health}" == "HEALTH_OK" ]] && break
  if ((SECONDS > deadline)); then
    echo "FATAL: Ceph did not reach HEALTH_OK within ${CEPH_HEALTH_TIMEOUT}s" >&2
    kubectl -n rook-ceph get cephcluster rook-ceph -o yaml | tail -40 >&2
    exit 1
  fi
  sleep 20
done
kubectl -n rook-ceph rollout status deploy/rook-ceph-tools --timeout=300s

step "Ceph StorageClasses + VolumeSnapshotClasses"
kubectl apply -f "${SCRIPT_DIR}/manifests/ceph-storage.yaml"

# ---------------------------------------------------------------------------
step "longhorn ${LONGHORN_CHART_VERSION}"
helm repo add longhorn https://charts.longhorn.io --force-update >/dev/null
helm upgrade --install longhorn longhorn/longhorn \
  --namespace longhorn-system --create-namespace \
  --version "${LONGHORN_CHART_VERSION}" \
  --values "${SCRIPT_DIR}/manifests/longhorn-values.yaml" \
  --wait --timeout 15m
kubectl apply -f "${SCRIPT_DIR}/manifests/longhorn-snapclass.yaml"

# ---------------------------------------------------------------------------
step "local-path-provisioner ${LOCAL_PATH_REF} (storage WITHOUT snapshot support)"
kubectl apply -f "https://raw.githubusercontent.com/rancher/local-path-provisioner/${LOCAL_PATH_REF}/deploy/local-path-storage.yaml"
kubectl -n local-path-storage rollout status deploy/local-path-provisioner --timeout=300s

# ---------------------------------------------------------------------------
step "crystal-backup (CRDs + operator chart)"
# Sync the generated CRDs into the chart's crds/ dir (the chart keeps them
# git-ignored). Equivalent to the repo's `chart-crds` make target, inlined so the
# crucible has no build-time dependency on make.
mkdir -p "${REPO_ROOT}/charts/crystal-backup/crds"
rm -f "${REPO_ROOT}/charts/crystal-backup/crds"/*.yaml
cp "${REPO_ROOT}/config/crd/bases"/*.yaml "${REPO_ROOT}/charts/crystal-backup/crds/"
# Create the namespace ourselves (with the chart's PSA labels) so helm's release
# secret has a home, then tell the chart not to create it a second time.
kubectl create namespace crystal-backup-system --dry-run=client -o yaml | kubectl apply -f -
kubectl label namespace crystal-backup-system \
  pod-security.kubernetes.io/enforce=baseline \
  pod-security.kubernetes.io/audit=restricted \
  pod-security.kubernetes.io/warn=restricted \
  --overwrite
helm upgrade --install crystal-backup "${REPO_ROOT}/charts/crystal-backup" \
  --namespace crystal-backup-system \
  --set namespace.create=false \
  --set image.digest="${OPERATOR_IMAGE_DIGEST}" \
  --set image.tag="${OPERATOR_IMAGE_TAG}" \
  --set mover.image.digest="${MOVER_IMAGE_DIGEST}" \
  --set mover.image.tag="${MOVER_IMAGE_TAG}"

echo
echo "Deployed. Storage classes:"
kubectl get storageclass
echo
echo "Next: 'mise run seed' then 'mise run test'."
