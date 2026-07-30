#!/usr/bin/env bash
# Crucible — deploy the storage stack + crystal-backup onto the RKE2 cluster.
#
#   1. external-snapshotter (CSI snapshot CRDs + controller — RKE2 ships none)
#   2. rook-ceph operator + CephCluster (mon/mgr on masters, osd/mds on workers)
#      + StorageClasses/VolumeSnapshotClasses + toolbox
#   3. longhorn (snapshot-capable CSI, disks on workers only)
#   4. local-path-provisioner (NON-snapshottable class — exercises the skip path)
#   5. prometheus-operator + one Prometheus (evaluates the chart's own alert rules)
#   6. crystal-backup chart (CRDs + operator), with the ServiceMonitor and PrometheusRule ON
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
PROMETHEUS_OPERATOR_REF="${PROMETHEUS_OPERATOR_REF:-v0.93.0}"
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
# Sync image override — the THIRD image (crystal-mover + restic + rclone), run only by external-sync
# Jobs (M5, adr/0013). Same requirement as the mover for the M5 label and no requirement at all for
# anything else: an operator with no ExternalSync never pulls it, which is exactly why leaving it at
# the chart placeholder goes unnoticed until a sync Job sits in ImagePullBackOff for twenty minutes.
# The m5 suite fails fast on the placeholder digest rather than waiting for that.
SYNC_IMAGE_DIGEST="${SYNC_IMAGE_DIGEST:-}"
SYNC_IMAGE_TAG="${SYNC_IMAGE_TAG:-}"

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
# BEFORE the chart, and that ordering is load-bearing: templates/service.yaml and
# templates/prometheusrule.yaml emit monitoring.coreos.com objects, and helm refuses to install a
# manifest whose API group the cluster does not know. Installing this afterwards would need a second
# `helm upgrade` nobody would remember to run.
#
# The upstream bundle deploys the operator into `default` — it does, and that is left alone rather
# than sed-ed into another namespace: the operator watches all namespaces regardless, and rewriting a
# 75k-line generated bundle in a shell script is a maintenance liability for a cosmetic gain.
step "prometheus-operator ${PROMETHEUS_OPERATOR_REF} (CRDs + operator)"
# --server-side: the CRDs in this bundle are far past the 262144-byte annotation limit that
# client-side apply stores last-applied-configuration in, so a plain `kubectl apply` fails on them.
kubectl apply --server-side --force-conflicts \
  -f "https://github.com/prometheus-operator/prometheus-operator/releases/download/${PROMETHEUS_OPERATOR_REF}/bundle.yaml"
kubectl -n default rollout status deploy/prometheus-operator --timeout=300s
# The CRDs must be ESTABLISHED, not merely created: the Prometheus object below is rejected if the
# API server has not finished serving its type yet, and `apply` returning is not that guarantee.
for crd in prometheuses servicemonitors prometheusrules; do
  kubectl wait --for=condition=Established --timeout=120s \
    "crd/${crd}.monitoring.coreos.com"
done

step "Prometheus (scrapes crystal-backup, evaluates its shipped rules)"
kubectl apply -f "${SCRIPT_DIR}/manifests/prometheus.yaml"
# The StatefulSet is created by the operator a moment later, so wait for it to EXIST before asking
# about its rollout — `rollout status` on a missing object fails immediately and would make a slow
# operator look like a broken one. A Prometheus that never comes up must fail HERE, not forty
# minutes into the m6 alert lane.
kubectl -n monitoring wait --for=create sts/prometheus-crucible --timeout=180s
kubectl -n monitoring rollout status sts/prometheus-crucible --timeout=300s
# The crystal-backup target itself cannot be checked yet: its ServiceMonitor arrives with the chart,
# below. The alert specs assert it (m6RequirePrometheus) rather than a deploy script guessing.

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
  --set mover.image.tag="${MOVER_IMAGE_TAG}" \
  --set sync.image.digest="${SYNC_IMAGE_DIGEST}" \
  --set sync.image.tag="${SYNC_IMAGE_TAG}" \
  --set metrics.serviceMonitor.enabled=true \
  --set metrics.rules.enabled=true \
  --set networkPolicy.monitoringNamespace=monitoring \
  --set networkPolicy.apiServerPort=6443
  # metrics.serviceMonitor / metrics.rules: both default to OFF, so until M6 the crucible ran
  # against a chart whose observability half was never installed by anything. They are on here
  # for the same reason the m6 alert specs exist — a PrometheusRule nobody loads is indistinguishable
  # from one that works.
  #
  # monitoringNamespace=monitoring: the operator NetworkPolicy's metrics ingress is unrestricted
  # while this is empty and namespace-scoped as soon as it is set. Setting it means the crucible
  # tests the CLOSED configuration, which is the one an operator following the chart's own advice
  # will run; leaving it empty would have every scrape succeed for the wrong reason.
  #
  # apiServerPort=6443: RKE2's kube-apiserver Service (10.43.0.1:443) DNATs to the master
  # nodes on 6443, and Canal (the crucible CNI) evaluates NetworkPolicy egress POST-DNAT, so
  # the operator's and manifest-mover's API-server egress must name 6443, not the chart default
  # 443 (which fits a cluster whose apiserver Endpoints are themselves on 443). Without this the
  # operator crashes at startup with "dial tcp 10.43.0.1:443: i/o timeout".

echo
echo "Deployed. Storage classes:"
kubectl get storageclass
echo
echo "Next: 'mise run seed' then 'mise run test'."
