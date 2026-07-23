#!/usr/bin/env bash
#
# Install the e2e data-path infrastructure into the current-context (Kind) cluster:
#   1. external-snapshotter: VolumeSnapshot CRDs + snapshot-controller
#   2. csi-driver-host-path: a CSI driver with VolumeSnapshot support (spec/08 §4)
#   3. a VolumeSnapshotClass for that driver
#   4. SeaweedFS (S3) as the object-storage backend, with its buckets bootstrapped
#
# Versions are pinned and overridable via env vars so CI is reproducible. This script is
# invoked by `make install-test-e2e-infra` (and transitively by `make test-e2e`); it is
# intentionally idempotent so it can be re-run against an existing cluster.
set -euo pipefail

KUBECTL="${KUBECTL:-kubectl}"
EXTERNAL_SNAPSHOTTER_VERSION="${EXTERNAL_SNAPSHOTTER_VERSION:-v8.2.0}"
CSI_DRIVER_HOSTPATH_VERSION="${CSI_DRIVER_HOSTPATH_VERSION:-v1.15.0}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "==> external-snapshotter ${EXTERNAL_SNAPSHOTTER_VERSION}: VolumeSnapshot CRDs"
${KUBECTL} apply -k \
  "https://github.com/kubernetes-csi/external-snapshotter/client/config/crd?ref=${EXTERNAL_SNAPSHOTTER_VERSION}"

echo "==> external-snapshotter ${EXTERNAL_SNAPSHOTTER_VERSION}: snapshot-controller"
${KUBECTL} apply -k \
  "https://github.com/kubernetes-csi/external-snapshotter/deploy/kubernetes/snapshot-controller?ref=${EXTERNAL_SNAPSHOTTER_VERSION}"

echo "==> waiting for the VolumeSnapshot CRDs to be Established"
for crd in \
  volumesnapshotclasses.snapshot.storage.k8s.io \
  volumesnapshotcontents.snapshot.storage.k8s.io \
  volumesnapshots.snapshot.storage.k8s.io; do
  ${KUBECTL} wait --for=condition=Established --timeout=90s "crd/${crd}"
done

echo "==> waiting for the snapshot-controller to roll out"
# The upstream kustomize deploys the controller to the 'kube-system' namespace in recent
# releases; fall back to 'default' for older layouts.
${KUBECTL} -n kube-system rollout status deploy/snapshot-controller --timeout=180s 2>/dev/null \
  || ${KUBECTL} rollout status deploy/snapshot-controller --timeout=180s 2>/dev/null \
  || echo "   (snapshot-controller Deployment not found in kube-system/default — continuing)"

echo "==> csi-driver-host-path ${CSI_DRIVER_HOSTPATH_VERSION}"
WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT
git clone --quiet --depth 1 --branch "${CSI_DRIVER_HOSTPATH_VERSION}" \
  https://github.com/kubernetes-csi/csi-driver-host-path "${WORKDIR}/csi-driver-host-path"
# deploy.sh is the upstream-supported installer; it applies the driver StatefulSet with its
# provisioner/attacher/resizer/snapshotter/registrar sidecars, the CSIDriver object and the
# 'csi-hostpath-sc' StorageClass. It reads `kubectl` from PATH and targets the current context.
"${WORKDIR}/csi-driver-host-path/deploy/kubernetes-latest/deploy.sh"

echo "==> waiting for the csi-hostpathplugin to roll out"
${KUBECTL} -n default rollout status statefulset/csi-hostpathplugin --timeout=240s 2>/dev/null \
  || ${KUBECTL} -n default rollout status daemonset/csi-hostpathplugin --timeout=240s 2>/dev/null \
  || echo "   (csi-hostpathplugin workload name differs for this version — continuing)"

echo "==> VolumeSnapshotClass for hostpath.csi.k8s.io"
${KUBECTL} apply -f "${SCRIPT_DIR}/csi-hostpath-snapshotclass.yaml"

echo "==> StorageClass csi-hostpath-sc (deploy.sh does not create it; data-path PVCs need it)"
${KUBECTL} apply -f "${SCRIPT_DIR}/csi-hostpath-storageclass.yaml"

echo "==> SeaweedFS S3 backend"
${KUBECTL} apply -f "${SCRIPT_DIR}/seaweedfs.yaml"
${KUBECTL} -n crystalbackup-e2e rollout status deploy/seaweedfs --timeout=240s
echo "==> waiting for the SeaweedFS bucket bootstrap Job"
${KUBECTL} -n crystalbackup-e2e wait --for=condition=complete job/seaweedfs-bucket-init --timeout=240s \
  || echo "   (bucket-init Job not complete yet — inspect: kubectl -n crystalbackup-e2e logs job/seaweedfs-bucket-init)"

echo "==> e2e infrastructure is ready"
