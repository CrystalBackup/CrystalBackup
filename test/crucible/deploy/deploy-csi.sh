#!/usr/bin/env bash
# Crucible — install the ADDITIONAL CSI drivers we want to qualify against CrystalBackup's
# exposure path, one wave at a time.
#
#   deploy/deploy-csi.sh <wave> [--print]      wave in {1,2,3,4}, or "all" for 1..3
#
# This is NOT deploy.sh. deploy.sh builds the crucible's own storage stack (rook-ceph, longhorn,
# local-path, prometheus, crystal-backup) and every crucible test depends on it. This script adds
# drivers ALONGSIDE that stack and must never touch it. The one exception is wave 4's CephNFS,
# which modifies rook in place — it is gated behind an explicit confirmation and documented at
# length where it happens.
#
# Each wave installs its drivers, creates their StorageClasses, and creates a VolumeSnapshotClass
# ONLY for the drivers that actually implement CreateSnapshot. For the rest, THE ABSENCE OF A
# VolumeSnapshotClass IS THE EXPECTED RESULT, not an oversight: internal/exposer/registry.go
# returns ErrUnsupported, the Backup controller marks the volume Skipped/CSISnapshotUnsupported
# (ADR 0003), and scripts/csi-probe.sh reports SKIPPED with exit 0. Every place where one is
# deliberately not created carries a comment saying so, here and in the manifests.
#
# Waves
#   1  needs the nfsd/nfs kernel modules loaded on the nodes (prepared by ansible)
#        csi-driver-nfs + an in-cluster NFS server (ceph-block backed)      -> csi-nfs      WITH VSC
#        OpenEBS LocalPV hostpath                                           -> openebs-hostpath  no VSC
#   2  needs cifs-utils on the nodes (prepared by ansible)
#        csi-driver-smb + an in-cluster Samba server                        -> csi-smb      no VSC
#   3  needs the devices ansible prepares on /dev/sdc of every worker:
#        VG crucible-vg1 (thick), VG crucible-vg2 with thin pool "thinpool", zpool crucible-zpool
#        OpenEBS LocalPV LVM on crucible-vg1                                -> openebs-lvm  WITH VSC
#        TopoLVM on crucible-vg2/thinpool                                   -> topolvm-thin WITH VSC
#        OpenEBS LocalPV ZFS on crucible-zpool                              -> openebs-zfs  WITH VSC
#   4  DISPOSABLE LANE ONLY — drivers that damage or mutate the cluster
#        Rook CephNFS                                                       -> ceph-nfs     WITH VSC
#        Piraeus / LINSTOR on crucible-vg2                                  -> piraeus-thin WITH VSC
#        Hetzner Cloud CSI                                                  -> hcloud-volumes  no VSC
#
# "all" stops at wave 3 on purpose. Wave 4 needs CRUCIBLE_WAVE4_CONFIRM=yes and must be asked for
# by number, for two independent reasons: CephNFS adds a pool to a Ceph cluster whose HEALTH_OK
# is a hard gate in deploy.sh, and Piraeus wants the SAME thin pool TopoLVM took in wave 3.
#
# --------------------------------------------------------------------------------------------
# SECRET HANDLING (wave 4, Hetzner)
# --------------------------------------------------------------------------------------------
# The Hetzner CSI driver needs a Secret named `hcloud` holding an API token. That token is read
# ONLY from the HCLOUD_TOKEN environment variable, which scripts/load-env.sh exports from
# .secrets/HETZNER_TOKEN. It is never written to a file in this repository, never passed as a
# helm --set (which would store it in cleartext in the release Secret and in `helm get values`),
# never echoed, and never included in any artifact. Shell tracing is disabled around the one
# function that handles it. The Secret is TEMPORARY: it grants full control of the Hetzner
# project to anything that can read Secrets in its namespace, so it must disappear with the lane
# — `mise run down` / scripts/nuke.sh destroys the cluster, which is the only teardown that
# actually removes it.
#
# Idempotent: everything is `helm upgrade --install` / `kubectl apply`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CRUCIBLE_DIR="$(dirname "${SCRIPT_DIR}")"
MANIFESTS="${SCRIPT_DIR}/manifests"

# ---------------------------------------------------------------------------
# Version pins — bump deliberately, together with the manifests.
# Every one of these was verified to exist before being written down; see the
# "how each pin was checked" note at the bottom of this header block.
# ---------------------------------------------------------------------------
CSI_NFS_CHART_VERSION="${CSI_NFS_CHART_VERSION:-4.13.4}"
CSI_SMB_CHART_VERSION="${CSI_SMB_CHART_VERSION:-1.20.3}"
OPENEBS_CHART_VERSION="${OPENEBS_CHART_VERSION:-4.5.1}"
OPENEBS_LVM_CHART_VERSION="${OPENEBS_LVM_CHART_VERSION:-1.9.1}"
OPENEBS_ZFS_CHART_VERSION="${OPENEBS_ZFS_CHART_VERSION:-2.10.1}"
TOPOLVM_CHART_VERSION="${TOPOLVM_CHART_VERSION:-17.0.0}"
PIRAEUS_OPERATOR_REF="${PIRAEUS_OPERATOR_REF:-v2.10.8}"
HCLOUD_CSI_CHART_VERSION="${HCLOUD_CSI_CHART_VERSION:-2.22.1}"
HCLOUD_CCM_CHART_VERSION="${HCLOUD_CCM_CHART_VERSION:-1.34.0}"
# Only used by the wave-4 DRBD preflight Jobs; busybox because it ships nsenter as an applet.
DRBD_PROBE_IMAGE="${DRBD_PROBE_IMAGE:-busybox:1.37.0}"

HELM_TIMEOUT="${HELM_TIMEOUT:-10m}"
ROLLOUT_TIMEOUT="${ROLLOUT_TIMEOUT:-300s}"
PREFLIGHT_TIMEOUT="${PREFLIGHT_TIMEOUT:-180}" # seconds, for the DRBD probe Jobs

# Wave 4 opt-ins. Both default to OFF and both are checked, not assumed.
CRUCIBLE_WAVE4_CONFIRM="${CRUCIBLE_WAVE4_CONFIRM:-}"
CRUCIBLE_INSTALL_HCLOUD_CCM="${CRUCIBLE_INSTALL_HCLOUD_CCM:-}"

# The label the rke2_agent ansible role puts on every worker
# (ansible/roles/rke2_agent/templates/config.yaml.j2). Custom prefix because kubelets are not
# allowed to self-assign node-role.kubernetes.io/*.
WORKER_LABEL="crystalbackup.io/node-role=worker"

export KUBECONFIG="${KUBECONFIG:-${CRUCIBLE_DIR}/artifacts/kubeconfig}"

step() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
info() { printf '    %s\n' "$*"; }
warn() { printf '\033[1;33m    !!\033[0m  %s\n' "$*" >&2; }
fatal() {
  printf '\033[1;31mFATAL:\033[0m %s\n' "$*" >&2
  exit 1
}

DRY_RUN=0
# run — execute, or in --print mode just show what would have been executed. Every mutating
# command in this script goes through it.
#
# One caveat if you extend this: a redirection written on the CALL (`run helm repo add ... >
# /dev/null`) applies to run itself, so in --print mode it would swallow the echoed line too.
# That is harmless today because every waveN() returns from its own --print branch before
# reaching any run call — the plan text is written out explicitly there. Do not rely on run's
# echo for anything you redirect.
run() {
  if ((DRY_RUN)); then
    printf '\033[2m    $ %s\033[0m\n' "$*"
    return 0
  fi
  "$@"
}

usage() {
  cat <<'EOF'
Usage: deploy-csi.sh <wave> [--print]

  <wave>      1 | 2 | 3 | 4 | all   ("all" runs waves 1..3; wave 4 is never included)
  --print     list what the wave would install and the SC/VSC it would create, change nothing
  -h|--help   this text

Wave 4 additionally requires CRUCIBLE_WAVE4_CONFIRM=yes — it mutates rook-ceph in place and
competes with wave 3 for the same LVM thin pool. It belongs on a throwaway lane only.
EOF
}

# ---------------------------------------------------------------------------
# The matrix. ONE declarative table, used by --print, by the end-of-wave recap, and by nothing
# else — so the recap cannot disagree with the preview.
#
#   wave | storageclass | provisioner | volumesnapshotclass ("" = deliberately none)
# ---------------------------------------------------------------------------
CSI_MATRIX=(
  "1|csi-nfs|nfs.csi.k8s.io|csi-nfs"
  "1|openebs-hostpath|openebs.io/local|"
  "2|csi-smb|smb.csi.k8s.io|"
  "3|openebs-lvm|local.csi.openebs.io|openebs-lvm"
  "3|topolvm-thin|topolvm.io|topolvm-thin"
  "3|openebs-zfs|zfs.csi.openebs.io|openebs-zfs"
  "4|ceph-nfs|rook-ceph.nfs.csi.ceph.com|ceph-nfs"
  "4|piraeus-thin|linstor.csi.linbit.com|piraeus-thin"
  "4|hcloud-volumes|csi.hetzner.cloud|"
)

# verdict_for — the verdict scripts/csi-probe.sh should return for this driver, computed with
# EXACTLY the rule in internal/exposer/registry.go:
#
#   1. no VolumeSnapshotClass whose .driver equals the provisioner -> ErrUnsupported -> SKIPPED
#   2. provisioner contains ".cephfs.csi."                         -> cephfsShallowExposer
#   3. otherwise                                                   -> csiGenericExposer
#
# Order matters: the VSC lookup comes FIRST, so a CephFS driver with no VolumeSnapshotClass is
# SKIPPED, not cephfs-shallow. And note what rule 2 does NOT match: rook-ceph.nfs.csi.ceph.com
# contains ".nfs.csi.", so Ceph's NFS driver takes the csi-generic path like everything else.
verdict_for() {
  local provisioner="$1" vsc="$2"
  if [[ -z "${vsc}" ]]; then
    echo "SKIPPED"
    return 0
  fi
  case "${provisioner}" in
  *.cephfs.csi.*) echo "cephfs-shallow" ;;
  *) echo "csi-generic" ;;
  esac
}

# recap — end-of-wave summary. Reads the LIVE cluster rather than replaying the table, so it
# reports what is actually installed; --print falls back to the table because there is nothing
# live to read. A StorageClass the wave should have created but did not shows up as a missing
# row, which is the point.
recap() {
  local wave="$1" row w sc provisioner vsc verdict live_prov live_vsc
  step "Wave ${wave} — StorageClasses, VolumeSnapshotClasses, and the verdict csi-probe.sh should return"
  printf '    %-18s %-30s %-16s %s\n' "STORAGECLASS" "PROVISIONER" "SNAPSHOTCLASS" "EXPECTED VERDICT"
  printf '    %-18s %-30s %-16s %s\n' "------------" "-----------" "-------------" "----------------"
  for row in "${CSI_MATRIX[@]}"; do
    IFS='|' read -r w sc provisioner vsc <<<"${row}"
    [[ "${w}" == "${wave}" ]] || continue

    if ((DRY_RUN)); then
      verdict="$(verdict_for "${provisioner}" "${vsc}")"
      printf '    %-18s %-30s %-16s %s\n' "${sc}" "${provisioner}" "${vsc:-(none)}" "${verdict}"
      continue
    fi

    live_prov="$(kubectl get storageclass "${sc}" -o jsonpath='{.provisioner}' 2>/dev/null || true)"
    if [[ -z "${live_prov}" ]]; then
      printf '    %-18s %-30s %-16s %s\n' "${sc}" "<absent>" "-" "PROBE_ERROR (no such StorageClass)"
      continue
    fi
    # Same tie-break as Registry.findVolumeSnapshotClass: lexicographically smallest match.
    live_vsc="$(kubectl get volumesnapshotclasses.snapshot.storage.k8s.io \
      -o custom-columns=NAME:.metadata.name,DRIVER:.driver --no-headers 2>/dev/null |
      awk -v d="${live_prov}" '$2 == d { print $1 }' | sort | head -1)"
    verdict="$(verdict_for "${live_prov}" "${live_vsc}")"
    printf '    %-18s %-30s %-16s %s\n' "${sc}" "${live_prov}" "${live_vsc:-(none)}" "${verdict}"

    # A disagreement between the table and the cluster is worth shouting about: it means either
    # a driver shipped a VolumeSnapshotClass we did not ask for, or one we asked for is missing.
    if [[ -n "${vsc}" && -z "${live_vsc}" ]]; then
      warn "${sc}: expected a VolumeSnapshotClass for ${live_prov}, found none — the probe will say SKIPPED"
    elif [[ -z "${vsc}" && -n "${live_vsc}" ]]; then
      warn "${sc}: found VolumeSnapshotClass '${live_vsc}' for ${live_prov}, which this wave deliberately does NOT create."
      warn "  Something else installed it (a chart default?). The skip path is no longer being exercised here."
    fi
  done
  echo
  info "Qualify one with:  scripts/csi-probe.sh <storageclass> [--copy-probe]"
}

print_plan() {
  local wave="$1"
  step "Wave ${wave} — plan (--print: nothing will be created)"
}

# ---------------------------------------------------------------------------
# Shared preflight
# ---------------------------------------------------------------------------
preflight_common() {
  [[ -f "${KUBECONFIG}" ]] || fatal "kubeconfig not found at ${KUBECONFIG} — run 'mise run cluster' first."
  ((DRY_RUN)) && return 0

  step "Cluster reachability"
  kubectl get nodes -o wide

  # Every VolumeSnapshotClass this script applies needs the CRD to exist. deploy.sh installs
  # external-snapshotter only when the distro ships none; either way it must be there by now.
  # Failing here is far kinder than a wave that installs three drivers and then dies on the
  # `kubectl apply` of its last manifest.
  kubectl get crd volumesnapshotclasses.snapshot.storage.k8s.io >/dev/null 2>&1 ||
    fatal "no VolumeSnapshotClass CRD in this cluster — run deploy/deploy.sh first."

  local workers
  workers="$(kubectl get nodes -l "${WORKER_LABEL}" -o name 2>/dev/null | wc -l | tr -d ' ')"
  [[ "${workers}" -gt 0 ]] ||
    fatal "no node carries ${WORKER_LABEL} — every driver here is pinned to workers by that label."
  info "workers carrying ${WORKER_LABEL}: ${workers}"
}

# Waves 1 and 2 back their in-cluster file servers with a ceph-block PVC.
preflight_ceph_block() {
  ((DRY_RUN)) && return 0
  kubectl get storageclass ceph-block >/dev/null 2>&1 ||
    fatal "StorageClass ceph-block not found — the in-cluster NFS/SMB server needs it. Run deploy/deploy.sh first."
}

# ---------------------------------------------------------------------------
# WAVE 1 — csi-driver-nfs (+ in-cluster NFS server) and OpenEBS LocalPV hostpath
# ---------------------------------------------------------------------------
wave1() {
  if ((DRY_RUN)); then
    print_plan 1
    info "helm csi-driver-nfs/csi-driver-nfs ${CSI_NFS_CHART_VERSION} -> ns csi-driver-nfs (values: csi-nfs-values.yaml)"
    info "kubectl apply manifests/csi-nfs-server.yaml   (ns csi-nfs-server: 20Gi ceph-block PVC + nfs-server Deployment + Service)"
    info "kubectl apply manifests/csi-nfs-storage.yaml  (SC csi-nfs + VolumeSnapshotClass csi-nfs)"
    info "helm openebs/openebs ${OPENEBS_CHART_VERSION} -> ns openebs (values: openebs-hostpath-values.yaml, hostpath engine only)"
    info "  no VolumeSnapshotClass for openebs.io/local — it is not a CSI driver and has no CreateSnapshot"
    recap 1
    return 0
  fi

  preflight_common
  preflight_ceph_block

  # Same shape as wave 2's cifs-utils warning, and same reason it is a warning and not a check:
  # the API server cannot be asked what modules a node has loaded. The in-cluster NFS server runs
  # nfsd, which is a HOST kernel service — the container cannot modprobe it itself (its
  # /lib/modules comes from the image, so modprobe fails with "invalid module format"). The
  # ansible `common` role loads nfsd/nfs and persists them via /etc/modules-load.d.
  warn "wave 1 assumes the nfsd and nfs kernel modules are loaded on every worker (ansible common role)."
  warn "  Without them the nfs-server pod CrashLoopBackOffs at 'starting rpc.nfsd', and the csi-nfs"
  warn "  StorageClass that follows would provision PVCs that no pod can ever mount."

  # -------------------------------------------------------------------------
  step "csi-driver-nfs ${CSI_NFS_CHART_VERSION}"
  run helm repo add csi-driver-nfs https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/master/charts --force-update >/dev/null
  run helm upgrade --install csi-driver-nfs csi-driver-nfs/csi-driver-nfs \
    --namespace csi-driver-nfs --create-namespace \
    --version "${CSI_NFS_CHART_VERSION}" \
    --values "${MANIFESTS}/csi-nfs-values.yaml" \
    --wait --timeout "${HELM_TIMEOUT}"

  # -------------------------------------------------------------------------
  step "In-cluster NFS server (20Gi on ceph-block)"
  run kubectl apply -f "${MANIFESTS}/csi-nfs-server.yaml"
  # The StorageClass below is useless until this pod actually serves, and a driver that cannot
  # reach its server fails with a mount timeout that reads like a driver bug. Fail here instead.
  run kubectl -n csi-nfs-server rollout status deploy/nfs-server --timeout="${ROLLOUT_TIMEOUT}"

  step "csi-nfs StorageClass + VolumeSnapshotClass"
  # WITH a VolumeSnapshotClass: nfs.csi.k8s.io implements CreateSnapshot as a tar archive.
  # See the header of csi-nfs-storage.yaml for the upstream evidence and for why a
  # pre-provisioned VolumeSnapshotContent goes Ready here without anything verifying it.
  run kubectl apply -f "${MANIFESTS}/csi-nfs-storage.yaml"

  # -------------------------------------------------------------------------
  step "OpenEBS LocalPV hostpath ${OPENEBS_CHART_VERSION}"
  run helm repo add openebs https://openebs.github.io/openebs --force-update >/dev/null
  run helm upgrade --install openebs openebs/openebs \
    --namespace openebs --create-namespace \
    --version "${OPENEBS_CHART_VERSION}" \
    --values "${MANIFESTS}/openebs-hostpath-values.yaml" \
    --wait --timeout "${HELM_TIMEOUT}"
  # The chart creates the openebs-hostpath StorageClass itself (provisioner openebs.io/local),
  # so there is no openebs-hostpath-storage.yaml to apply.
  #
  # NO VolumeSnapshotClass IS CREATED HERE, AND THAT IS THE POINT.
  # openebs.io/local is not a CSI driver at all — it is an out-of-tree dynamic provisioner that
  # hands out host directories. There is no CSI controller to call CreateSnapshot on, so there is
  # nothing a VolumeSnapshotClass could point at. Registry.For therefore returns ErrUnsupported,
  # the volume is marked Skipped/CSISnapshotUnsupported, the Backup still completes, and
  # csi-probe.sh returns SKIPPED with exit 0. This class is here precisely to exercise that path
  # a second time, independently of local-path-provisioner.
  run kubectl -n openebs rollout status deploy/openebs-localpv-provisioner --timeout="${ROLLOUT_TIMEOUT}"

  recap 1
}

# ---------------------------------------------------------------------------
# WAVE 2 — csi-driver-smb (+ in-cluster Samba server)
# ---------------------------------------------------------------------------

# ensure_smb_secret — create the Samba credentials Secret, reusing the existing password on a
# replay. Regenerating it would be worse than useless: samba would start rejecting the very
# credentials the node-stage secret hands it, and every mounted csi-smb volume in the cluster
# would break on the next remount, with an EACCES that looks like a driver fault.
#
# The password never reaches stdout. `kubectl create --dry-run=client -o yaml` emits it
# base64-encoded straight into `kubectl apply` on the other side of the pipe.
ensure_smb_secret() {
  local had_xtrace=0 existing password
  case $- in *x*) had_xtrace=1 ;; esac
  set +x

  existing="$(kubectl -n csi-smb-server get secret smb-creds -o jsonpath='{.data.password}' 2>/dev/null || true)"
  if [[ -n "${existing}" ]]; then
    password="$(printf '%s' "${existing}" | base64 -d)"
    info "reusing the existing smb-creds password"
  else
    # tr -dc keeps the alphabet free of characters that would need quoting in smb.conf or in a
    # CIFS mount option string.
    #
    # NOTE ON THE SHAPE OF THIS: the obvious one-liner
    #     password="$(tr -dc 'A-Za-z0-9' </dev/urandom | head -c 32)"
    # is a guaranteed failure under this script's `set -euo pipefail`, and it took down wave 2
    # every single time on a fresh cluster. `head -c 32` exits the instant it has 32 bytes and
    # closes the pipe; `tr`, reading an INFINITE source, is still writing and dies of SIGPIPE
    # with status 141; pipefail makes that the status of the whole substitution and `set -e`
    # kills the script — right after the Deployment was applied and right before the Secret it
    # needs. The visible symptom is not a shell error but a pod stuck in
    # CreateContainerConfigError with `secret "smb-creds" not found`, which reads like a
    # manifest-ordering bug and is not one.
    #
    # So: read a BOUNDED amount of randomness (head's producer is a file read, nothing upstream
    # can get SIGPIPE), filter it, and slice in-shell. 1024 random bytes yield ~248 alphanumerics
    # on average, so the 32 we need are never in doubt — but assert it rather than trust it,
    # because a short password here would surface as a samba auth failure days later.
    local raw
    raw="$(head -c 1024 /dev/urandom | LC_ALL=C tr -dc 'A-Za-z0-9')"
    password="${raw:0:32}"
    unset raw
    [[ "${#password}" -eq 32 ]] ||
      fatal "could not generate a 32-character smb-creds password (got ${#password})"
    info "generated a new smb-creds password (never printed, never written to the repo)"
  fi

  kubectl -n csi-smb-server create secret generic smb-creds \
    --from-literal=username=crucible \
    --from-literal=password="${password}" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  unset password existing

  if ((had_xtrace)); then set -x; fi
  return 0
}

wave2() {
  if ((DRY_RUN)); then
    print_plan 2
    info "helm csi-driver-smb/csi-driver-smb ${CSI_SMB_CHART_VERSION} -> ns csi-driver-smb"
    info "kubectl apply manifests/csi-smb-server.yaml   (ns csi-smb-server: 20Gi ceph-block PVC + samba Deployment + Service)"
    info "kubectl create secret smb-creds               (generated password, reused on replay, never printed)"
    info "kubectl apply manifests/csi-smb-storage.yaml  (SC csi-smb ONLY)"
    info "  no VolumeSnapshotClass for smb.csi.k8s.io — CreateSnapshot returns codes.Unimplemented"
    recap 2
    return 0
  fi

  preflight_common
  preflight_ceph_block

  # cifs-utils on the nodes is this wave's prerequisite and ansible's job. There is no reliable
  # way to check it from the API server, and guessing would be worse than saying so: if the
  # package is missing, PVCs bind fine and then every pod that mounts one sits in
  # ContainerCreating with "mount error: cifs filesystem not supported by the system".
  warn "wave 2 assumes cifs-utils is installed on every worker (ansible). If it is not, PVCs will Bind and pods will hang in ContainerCreating."

  step "csi-driver-smb ${CSI_SMB_CHART_VERSION}"
  run helm repo add csi-driver-smb https://raw.githubusercontent.com/kubernetes-csi/csi-driver-smb/master/charts --force-update >/dev/null
  run helm upgrade --install csi-driver-smb csi-driver-smb/csi-driver-smb \
    --namespace csi-driver-smb --create-namespace \
    --version "${CSI_SMB_CHART_VERSION}" \
    --wait --timeout "${HELM_TIMEOUT}"

  step "In-cluster Samba server (20Gi on ceph-block)"
  # The namespace has to exist before the Secret, and the Deployment mounts the Secret, so the
  # manifest goes first (it creates the namespace), then the Secret, then we wait for rollout.
  run kubectl apply -f "${MANIFESTS}/csi-smb-server.yaml"
  ensure_smb_secret
  run kubectl -n csi-smb-server rollout status deploy/smb-server --timeout="${ROLLOUT_TIMEOUT}"

  step "csi-smb StorageClass (NO VolumeSnapshotClass — by design)"
  # DELIBERATELY NO VolumeSnapshotClass.
  # smb.csi.k8s.io does not implement snapshots: pkg/smb/controllerserver.go returns
  # status.Error(codes.Unimplemented, "") from CreateSnapshot, DeleteSnapshot and ListSnapshots,
  # and pkg/smb/smb.go never advertises CREATE_DELETE_SNAPSHOT among its controller capabilities.
  #   https://github.com/kubernetes-csi/csi-driver-smb/blob/master/pkg/smb/controllerserver.go
  #   https://github.com/kubernetes-csi/csi-driver-smb/blob/master/pkg/smb/smb.go
  # Creating one anyway would make Registry.For hand the volume to csiGenericExposer, which would
  # then fail at CreateSnapshot — converting a correct SKIPPED into a hard Backup failure. The
  # omission is what makes the operator behave correctly.
  run kubectl apply -f "${MANIFESTS}/csi-smb-storage.yaml"

  recap 2
}

# ---------------------------------------------------------------------------
# WAVE 3 — OpenEBS LocalPV LVM, TopoLVM, OpenEBS LocalPV ZFS
# ---------------------------------------------------------------------------
wave3() {
  if ((DRY_RUN)); then
    print_plan 3
    info "helm openebs-lvmlocalpv/lvm-localpv ${OPENEBS_LVM_CHART_VERSION} -> ns openebs-lvmlocalpv  (VG crucible-vg1)"
    info "kubectl apply manifests/openebs-lvm-storage.yaml   (SC openebs-lvm + VSC openebs-lvm)"
    info "helm topolvm/topolvm ${TOPOLVM_CHART_VERSION} -> ns topolvm-system   (VG crucible-vg2 / thinpool, POD WEBHOOK OFF)"
    info "kubectl apply manifests/topolvm-storage.yaml       (SC topolvm-thin + VSC topolvm-thin)"
    info "helm openebs-zfslocalpv/zfs-localpv ${OPENEBS_ZFS_CHART_VERSION} -> ns openebs-zfslocalpv (zpool crucible-zpool)"
    info "kubectl apply manifests/openebs-zfs-storage.yaml   (SC openebs-zfs + VSC openebs-zfs)"
    recap 3
    return 0
  fi

  preflight_common

  warn "wave 3 assumes ansible has prepared /dev/sdc on every worker: VG crucible-vg1 (thick),"
  warn "  VG crucible-vg2 with thin pool 'thinpool', and zpool crucible-zpool. None of that is"
  warn "  checkable from the API server; a missing VG shows up as PVCs stuck Pending with the"
  warn "  node plugin logging 'volume group not found'."

  # -------------------------------------------------------------------------
  step "OpenEBS LocalPV LVM ${OPENEBS_LVM_CHART_VERSION} (VG crucible-vg1, thick)"
  # Its OWN helm release, not engines.local.lvm on the wave-1 umbrella — see the header of
  # openebs-lvm-values.yaml. Flipping the umbrella's flag would make replaying wave 1 tear this
  # driver down.
  run helm repo add openebs-lvmlocalpv https://openebs.github.io/lvm-localpv --force-update >/dev/null
  run helm upgrade --install lvm-localpv openebs-lvmlocalpv/lvm-localpv \
    --namespace openebs-lvmlocalpv --create-namespace \
    --version "${OPENEBS_LVM_CHART_VERSION}" \
    --values "${MANIFESTS}/openebs-lvm-values.yaml" \
    --wait --timeout "${HELM_TIMEOUT}"
  # WITH a VolumeSnapshotClass: local.csi.openebs.io snapshots via `lvcreate -s`. On a THICK VG
  # that snapshot needs its own free extents in crucible-vg1 — see openebs-lvm-storage.yaml.
  run kubectl apply -f "${MANIFESTS}/openebs-lvm-storage.yaml"

  # -------------------------------------------------------------------------
  step "TopoLVM ${TOPOLVM_CHART_VERSION} (VG crucible-vg2 / thinpool)"
  # THE POD MUTATING WEBHOOK IS DISABLED (webhook.podMutatingWebhook.enabled: false in
  # topolvm-values.yaml, which is also the chart's own 17.x default, stated explicitly so a
  # chart bump cannot turn it on silently).
  #
  # Why that matters, in one paragraph, because it is the single most dangerous thing this
  # script could install: the chart's MutatingWebhookConfiguration hardcodes failurePolicy: Fail
  # on `CREATE pods` for apiGroups [""] — every pod in the cluster, not just TopoLVM's. If the
  # topolvm-controller Deployment is unavailable, or its serving certificate expires, or its
  # Service loses endpoints, then EVERY POD CREATE IN THE CLUSTER IS REJECTED. On the crucible
  # that takes out rook's OSDs, longhorn, prometheus, the crystal-backup operator and every
  # mover Job simultaneously — and the cluster cannot recover on its own, because recovering
  # means creating pods. A driver we are merely qualifying must not be able to do that.
  #
  # The chart DOES support restricting it, and topolvm-values.yaml is pre-wired for that case:
  # the generated namespaceSelector always excludes namespaces listed in
  # webhook.podMutatingWebhook.ignoreNamespaces (pre-populated there with every namespace the
  # crucible cannot lose) and always honours the label topolvm.io/webhook=ignore on a namespace.
  # So turning it on is a one-line change that is already scoped. Note that enabling it also
  # pulls in a cert-manager requirement (webhook.certManager defaults to true and the chart does
  # NOT install cert-manager) — and a webhook whose cert never arrives IS the outage above.
  #
  # The cost of leaving it off: the scheduler no longer sees per-node LVM capacity, so a PVC can
  # be scheduled onto a node whose volume group is too full and stay Pending. For a
  # qualification lane that is a much better failure mode.
  run helm repo add topolvm https://topolvm.github.io/topolvm --force-update >/dev/null
  run helm upgrade --install topolvm topolvm/topolvm \
    --namespace topolvm-system --create-namespace \
    --version "${TOPOLVM_CHART_VERSION}" \
    --values "${MANIFESTS}/topolvm-values.yaml" \
    --wait --timeout "${HELM_TIMEOUT}"
  # WITH a VolumeSnapshotClass — but only because the device class is thin. TopoLVM snapshots
  # exist for thin volumes only; a thick device class would provision fine and fail every
  # CreateSnapshot.
  run kubectl apply -f "${MANIFESTS}/topolvm-storage.yaml"

  # -------------------------------------------------------------------------
  step "OpenEBS LocalPV ZFS ${OPENEBS_ZFS_CHART_VERSION} (zpool crucible-zpool)"
  run helm repo add openebs-zfslocalpv https://openebs.github.io/zfs-localpv --force-update >/dev/null
  run helm upgrade --install zfs-localpv openebs-zfslocalpv/zfs-localpv \
    --namespace openebs-zfslocalpv --create-namespace \
    --version "${OPENEBS_ZFS_CHART_VERSION}" \
    --values "${MANIFESTS}/openebs-zfs-values.yaml" \
    --wait --timeout "${HELM_TIMEOUT}"
  # WITH a VolumeSnapshotClass: native ZFS snapshots, genuinely copy-on-write and constant-time.
  run kubectl apply -f "${MANIFESTS}/openebs-zfs-storage.yaml"

  recap 3
}

# ---------------------------------------------------------------------------
# WAVE 4 — disposable lane: Rook CephNFS, Piraeus/LINSTOR, Hetzner Cloud CSI
# ---------------------------------------------------------------------------

# require_drbd_module — fail loudly BEFORE installing Piraeus if DRBD9 is not available on the
# workers, rather than leaving a half-installed operator behind.
#
# There is no way to ask the API server about a kernel module, so this runs one short privileged
# Job per worker that nsenters the host PID namespace and tries `modprobe drbd`. The Jobs and
# their namespace are removed afterwards, pass or fail.
#
# What "missing" looks like if this check is skipped: the drbd-module-loader initContainer that
# piraeus injects into every linstor-satellite pod fails, the satellite pod never leaves Init:,
# the node never registers with LINSTOR, and every PVC on piraeus-thin sits Pending forever with
# nothing in the CSI logs pointing at a kernel module. The usual root cause on Hetzner is that
# the loader defaults to LB_HOW=compile and the nodes have no linux-headers-$(uname -r).
#   https://piraeus.io/docs/stable/how-to/drbd-loader/
#   https://piraeus.io/docs/stable/how-to/install-kernel-headers/
require_drbd_module() {
  local ns="csi-drbd-preflight" node nodes rc=0 deadline phase failed=()

  step "Preflight: DRBD9 kernel module on every worker"
  mapfile -t nodes < <(kubectl get nodes -l "${WORKER_LABEL}" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
  [[ ${#nodes[@]} -gt 0 ]] || fatal "no worker nodes to probe for DRBD."

  kubectl create namespace "${ns}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl label namespace "${ns}" \
    pod-security.kubernetes.io/enforce=privileged \
    pod-security.kubernetes.io/audit=privileged \
    pod-security.kubernetes.io/warn=privileged --overwrite >/dev/null

  for node in "${nodes[@]}"; do
    kubectl -n "${ns}" delete job "drbd-check-${node}" --ignore-not-found >/dev/null 2>&1 || true
    kubectl apply -f - <<EOF >/dev/null
apiVersion: batch/v1
kind: Job
metadata:
  name: drbd-check-${node}
  namespace: ${ns}
spec:
  backoffLimit: 0
  activeDeadlineSeconds: ${PREFLIGHT_TIMEOUT}
  ttlSecondsAfterFinished: 300
  template:
    spec:
      restartPolicy: Never
      nodeName: ${node}
      hostPID: true
      containers:
        - name: probe
          image: ${DRBD_PROBE_IMAGE}
          securityContext:
            privileged: true
          command: ["nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p", "--", "sh", "-c"]
          args:
            - |
              modprobe drbd 2>&1 || true
              if [ -e /proc/drbd ] || grep -q '^drbd ' /proc/modules; then
                echo "DRBD present on \$(hostname)"
                exit 0
              fi
              echo "DRBD MISSING on \$(hostname)"
              exit 1
EOF
  done

  deadline=$((SECONDS + PREFLIGHT_TIMEOUT + 60))
  for node in "${nodes[@]}"; do
    while true; do
      phase="$(kubectl -n "${ns}" get job "drbd-check-${node}" \
        -o jsonpath='{.status.conditions[?(@.status=="True")].type}' 2>/dev/null || true)"
      case "${phase}" in
      *Complete*)
        info "drbd ok on ${node}"
        break
        ;;
      *Failed*)
        failed+=("${node}")
        rc=1
        break
        ;;
      esac
      if ((SECONDS > deadline)); then
        warn "DRBD preflight timed out waiting on ${node}"
        failed+=("${node}")
        rc=1
        break
      fi
      sleep 3
    done
  done

  if ((rc != 0)); then
    warn "probe logs:"
    kubectl -n "${ns}" logs -l batch.kubernetes.io/job-name --tail=20 --prefix=true 2>/dev/null || true
  fi
  kubectl delete namespace "${ns}" --wait=false >/dev/null 2>&1 || true

  if ((rc != 0)); then
    cat >&2 <<EOF

FATAL: the DRBD9 kernel module is not available on: ${failed[*]}

Piraeus/LINSTOR cannot work without it, and installing the operator anyway would leave a
half-deployed control plane whose satellites sit in Init: forever while every piraeus-thin PVC
stays Pending with nothing in the CSI logs naming the cause.

Fix it on the nodes first — piraeus's drbd-module-loader defaults to LB_HOW=compile, so it needs
linux-headers-\$(uname -r) installed on every worker:
    https://piraeus.io/docs/stable/how-to/install-kernel-headers/
or preload the module from ansible and pin the loader image:
    https://piraeus.io/docs/stable/how-to/drbd-loader/
EOF
    exit 1
  fi
}

# ensure_hcloud_secret — create the `hcloud` Secret from HCLOUD_TOKEN.
#
# The token is handled inside this function and nowhere else. Shell tracing is disabled for its
# duration (restored on the way out) so that a run under `bash -x` cannot leak it; it is never
# echoed; it is never passed as a helm --set (which would persist it in cleartext inside the helm
# release Secret and print it back from `helm get values`); it never reaches a file in this
# repository. `kubectl create --dry-run=client -o yaml | kubectl apply -f -` gives idempotence
# without ever materialising the manifest on disk.
ensure_hcloud_secret() {
  local had_xtrace=0
  case $- in *x*) had_xtrace=1 ;; esac
  set +x

  if [[ -z "${HCLOUD_TOKEN:-}" ]]; then
    if ((had_xtrace)); then set -x; fi
    cat >&2 <<'EOF'
FATAL: HCLOUD_TOKEN is empty.

The Hetzner CSI driver needs a Secret named `hcloud` holding a Hetzner Cloud API token, and this
script reads it ONLY from the environment. Load it the way the rest of the crucible does:

    source test/crucible/scripts/load-env.sh

which exports HCLOUD_TOKEN from .secrets/HETZNER_TOKEN. Do not paste the token on the command
line and do not put it in a values file — it would end up in your shell history, in the helm
release Secret in cleartext, and in `helm get values` output.
EOF
    exit 1
  fi

  kubectl create namespace hcloud-csi --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl -n hcloud-csi create secret generic hcloud \
    --from-literal=token="${HCLOUD_TOKEN}" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  info "Secret hcloud/token created in ns hcloud-csi (value never printed)"
  info "TEMPORARY: this Secret grants full control of the Hetzner project. It must die with the lane."

  if ((had_xtrace)); then set -x; fi
  return 0
}

wave4() {
  if ((DRY_RUN)); then
    print_plan 4
    info "requires CRUCIBLE_WAVE4_CONFIRM=yes"
    info "kubectl patch cm/rook-ceph-operator-config ROOK_CSI_ENABLE_NFS=true   (MUTATES rook in place)"
    info "kubectl apply manifests/cephnfs-cluster.yaml   (CephNFS crucible-nfs -> Ceph creates the .nfs pool)"
    info "kubectl apply manifests/cephnfs-storage.yaml   (SC ceph-nfs + VSC ceph-nfs)"
    info "preflight: one privileged Job per worker checking for the DRBD9 kernel module"
    info "kubectl apply -k piraeus-operator ${PIRAEUS_OPERATOR_REF} -> ns piraeus-datastore"
    info "kubectl apply manifests/piraeus-cluster.yaml   (LinstorCluster + thin pool on crucible-vg2)"
    info "kubectl apply manifests/piraeus-storage.yaml   (SC piraeus-thin + VSC piraeus-thin)"
    info "kubectl create secret hcloud                   (from HCLOUD_TOKEN, never printed)"
    info "helm hcloud/hcloud-csi ${HCLOUD_CSI_CHART_VERSION} -> ns hcloud-csi (chart creates SC hcloud-volumes, NOT default)"
    info "  no VolumeSnapshotClass for csi.hetzner.cloud — the driver has no CreateSnapshot at all"
    info "  hcloud CCM: opt-in only, CRUCIBLE_INSTALL_HCLOUD_CCM=yes (currently: ${CRUCIBLE_INSTALL_HCLOUD_CCM:-no})"
    recap 4
    return 0
  fi

  [[ "${CRUCIBLE_WAVE4_CONFIRM}" == "yes" ]] || fatal "$(
    cat <<'EOF'
wave 4 refused: set CRUCIBLE_WAVE4_CONFIRM=yes to proceed.

It is not like the others. It:
  - patches the rook-ceph operator ConfigMap IN PLACE (ROOK_CSI_ENABLE_NFS),
  - makes Ceph create a replicated `.nfs` pool on a cluster sized 3x40GB, whose HEALTH_OK is a
    HARD GATE in deploy/deploy.sh — degrade it and the NEXT full deploy on this cluster fails,
    with an error that points at rook and never mentions NFS,
  - claims the SAME thin pool (crucible-vg2/thinpool) that wave 3 gave to TopoLVM,
  - installs a Hetzner API token into the cluster.

Run it on a lane you intend to destroy, never on a lane you intend to keep testing on.
EOF
  )"

  preflight_common

  warn "WAVE 4 — disposable lane. This mutates rook-ceph in place and installs a Hetzner API token."
  warn "  Do not run the standard crucible suite on this cluster afterwards; destroy the lane."

  # -------------------------------------------------------------------------
  step "Rook CephNFS (MUTATES the existing rook-ceph install)"
  # ROOK_CSI_ENABLE_NFS defaults to "false", and without it Rook deploys no NFS provisioner at
  # all — the StorageClass would exist and provision nothing. This ConfigMap is OWNED BY THE
  # ROOK HELM RELEASE, so this patch is reverted by the next `helm upgrade` of rook, i.e. by the
  # next deploy.sh run. That is correct for a disposable lane and is exactly why wave 4 must not
  # be mixed with a lane that will be re-deployed.
  # (CSI_ENABLE_NFS_SNAPSHOTTER is a DIFFERENT key that already defaults to "true"; it only
  # decides whether the snapshotter sidecar joins the NFS provisioner pod.)
  run kubectl -n rook-ceph patch configmap rook-ceph-operator-config --type merge \
    -p '{"data":{"ROOK_CSI_ENABLE_NFS":"true"}}'
  run kubectl -n rook-ceph rollout status deploy/rook-ceph-operator --timeout="${ROLLOUT_TIMEOUT}"
  run kubectl apply -f "${MANIFESTS}/cephnfs-cluster.yaml"
  # The Ganesha Deployment is created by the operator a moment later; wait for it to EXIST
  # before asking about its rollout, the same pattern deploy.sh uses for the Prometheus STS.
  run kubectl -n rook-ceph wait --for=create deploy/rook-ceph-nfs-crucible-nfs-a --timeout=300s
  run kubectl -n rook-ceph rollout status deploy/rook-ceph-nfs-crucible-nfs-a --timeout="${ROLLOUT_TIMEOUT}"
  # WITH a VolumeSnapshotClass. Note the provisioner is rook-ceph.nfs.csi.ceph.com: it contains
  # ".nfs.csi.", NOT ".cephfs.csi.", so registry.go routes it to csi-generic and not to
  # cephfs-shallow. A Ceph driver that must not take the Ceph path.
  run kubectl apply -f "${MANIFESTS}/cephnfs-storage.yaml"

  # -------------------------------------------------------------------------
  step "Piraeus / LINSTOR ${PIRAEUS_OPERATOR_REF} (VG crucible-vg2 / thinpool)"
  require_drbd_module
  # --server-side: the piraeus CRDs are large enough to blow past the 262144-byte
  # last-applied-configuration annotation limit that client-side apply uses, the same reason
  # deploy.sh applies the prometheus-operator bundle server-side.
  run kubectl apply --server-side --force-conflicts \
    -k "https://github.com/piraeusdatastore/piraeus-operator//config/default?ref=${PIRAEUS_OPERATOR_REF}"
  for crd in linstorclusters linstorsatelliteconfigurations; do
    run kubectl wait --for=condition=Established --timeout=120s "crd/${crd}.piraeus.io"
  done
  run kubectl -n piraeus-datastore rollout status deploy/piraeus-operator-controller-manager --timeout="${ROLLOUT_TIMEOUT}"
  run kubectl apply -f "${MANIFESTS}/piraeus-cluster.yaml"
  # WITH a VolumeSnapshotClass — and only because the pool is THIN. LINSTOR supports snapshots
  # for LVM_THIN, FILE_THIN, ZFS and ZFS_THIN; plain thick LVM is not on that list, which is why
  # this lane sits on crucible-vg2 and not crucible-vg1.
  #   https://piraeus.io/docs/stable/tutorial/snapshots/
  run kubectl apply -f "${MANIFESTS}/piraeus-storage.yaml"

  # -------------------------------------------------------------------------
  step "Hetzner Cloud CSI ${HCLOUD_CSI_CHART_VERSION}"
  #
  # THE cloud-controller-manager QUESTION, and the answer is: we do not need it.
  #
  # The usual claim is that csi.hetzner.cloud requires the hcloud CCM so nodes carry
  # spec.providerID = hcloud://<id>. That turns out not to be true for this driver: it never
  # reads providerID at all. It resolves the server it is running on in this documented order —
  # HCLOUD_VOLUME_DEFAULT_LOCATION, then HCLOUD_SERVER_ID, then KUBE_NODE_NAME looked up through
  # the Hetzner API, and finally the metadata service at
  # http://169.254.169.254/hetzner/v1/metadata/instance-id.
  #   https://github.com/hetznercloud/csi-driver/blob/main/docs/kubernetes/explanation/volume-location.md
  # So the least invasive solution is to install NOTHING extra. The realistic failure here is not
  # a missing providerID but an unreachable link-local metadata address, which shows up as
  # "failed to fetch server ID from metadata service" / connection refused on 169.254.169.254 and
  # is a CNI problem, not a CCM one.
  #
  # Installing the CCM onto this already-running cluster would be the RISKY move, not omitting
  # it, and it is opt-in for that reason. RKE2 here was not started with
  # --cloud-provider=external, so its nodes never got the
  # node.cloudprovider.kubernetes.io/uninitialized:NoSchedule taint that the CCM watches for.
  # Adding the CCM after the fact does NOT retroactively taint them — it simply never initialises
  # them, providerID stays empty, and the CCM logs "providerIDToServerID: missing prefix
  # hcloud://" forever. Making it work would mean turning --cloud-provider=external ON across
  # the cluster, which taints EVERY NODE AT ONCE and leaves most pods unschedulable until the CCM
  # is up — on a live crucible that is an outage, not a configuration change.
  #   https://github.com/hetznercloud/hcloud-cloud-controller-manager/blob/main/docs/guides/quickstart.md
  #   https://github.com/hetznercloud/hcloud-cloud-controller-manager/issues/267
  run helm repo add hcloud https://charts.hetzner.cloud --force-update >/dev/null

  if [[ "${CRUCIBLE_INSTALL_HCLOUD_CCM}" == "yes" ]]; then
    warn "CRUCIBLE_INSTALL_HCLOUD_CCM=yes — installing the hcloud cloud-controller-manager."
    warn "  The CSI driver does NOT need this. On nodes that were not started with"
    warn "  --cloud-provider=external it will never initialise them and will log"
    warn "  'providerIDToServerID: missing prefix hcloud://' indefinitely. Enabled only because"
    warn "  you asked for it."
    ensure_hcloud_secret
    # No token flag: the CCM chart's own default for env.HCLOUD_TOKEN is already
    # valueFrom.secretKeyRef {name: hcloud, key: token}, i.e. the Secret ensure_hcloud_secret
    # just created. Passing it via --set would put nothing new in place and would only add
    # another surface where a token could accidentally be spelled out.
    run helm upgrade --install hcloud-ccm hcloud/hcloud-cloud-controller-manager \
      --namespace hcloud-csi \
      --version "${HCLOUD_CCM_CHART_VERSION}" \
      --wait --timeout "${HELM_TIMEOUT}"
  else
    info "hcloud cloud-controller-manager: NOT installed (not required; set CRUCIBLE_INSTALL_HCLOUD_CCM=yes to override)"
  fi

  ensure_hcloud_secret
  run helm upgrade --install hcloud-csi hcloud/hcloud-csi \
    --namespace hcloud-csi \
    --version "${HCLOUD_CSI_CHART_VERSION}" \
    --values "${MANIFESTS}/hcloud-csi-values.yaml" \
    --wait --timeout "${HELM_TIMEOUT}"
  # The chart creates the hcloud-volumes StorageClass (explicitly NOT marked cluster-default —
  # see hcloud-csi-values.yaml), so there is nothing to apply from hcloud-csi-storage.yaml, which
  # exists only to document the omission below.
  #
  # DELIBERATELY NO VolumeSnapshotClass — and this one contradicts the brief, which asked for
  # one. It cannot be written honestly: csi.hetzner.cloud does not implement CreateSnapshot at
  # all. There is no snapshotter sidecar in the chart, no snapshot keys in its values, and the
  # feature has been an open upstream request since 2019.
  #   https://github.com/hetznercloud/csi-driver/issues/88
  #   https://github.com/hetznercloud/csi-driver/issues/849
  # Writing one anyway would make Registry.findVolumeSnapshotClass match it, route the volume to
  # csiGenericExposer, and fail at CreateSnapshot with codes.Unimplemented — converting a correct
  # SKIPPED into a hard Backup failure. Expected verdict: SKIPPED.

  recap 4
}

# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------
WAVE=""
while (($#)); do
  case "$1" in
  1 | 2 | 3 | 4 | all)
    [[ -z "${WAVE}" ]] || {
      usage >&2
      fatal "more than one wave given ('${WAVE}' then '$1') — run them one at a time."
    }
    WAVE="$1"
    ;;
  --print | --dry-run) DRY_RUN=1 ;;
  -h | --help)
    usage
    exit 0
    ;;
  *)
    usage >&2
    fatal "unknown argument: $1"
    ;;
  esac
  shift
done

[[ -n "${WAVE}" ]] || {
  usage >&2
  exit 2
}

case "${WAVE}" in
1) wave1 ;;
2) wave2 ;;
3) wave3 ;;
4) wave4 ;;
all)
  # 1..3 only. Wave 4 is excluded by design — see the header.
  wave1
  wave2
  wave3
  ;;
esac

if ((DRY_RUN)); then
  echo
  info "--print: nothing was created."
else
  echo
  info "Done. Existing crucible components (rook, longhorn, local-path, prometheus, crystal-backup) were not touched."
  info "Next: scripts/csi-probe.sh <storageclass> --copy-probe"
fi
