#!/bin/sh
# CrystalBackup snapshot probe — can a snapshot of this StorageClass be restored, mounted and read?
# SPDX-License-Identifier: Apache-2.0
#
# ---------------------------------------------------------------------------------------------
# THIS SCRIPT CREATES OBJECTS IN YOUR CLUSTER.
#
#   That is the opposite of preflight.sh, which creates nothing and says so in its own header.
#   The two are deliberately split along that line, because the question left over when preflight
#   finishes cannot be answered without creating something. Read this block before you run it.
#
# WHY IT EXISTS
#
#   preflight.sh can tell you that a VolumeSnapshotClass exists for your StorageClass's driver.
#   That is snapshot AVAILABILITY. It is not snapshot USABILITY, and the gap between the two is
#   not theoretical: a cluster can create snapshots correctly, restore a PVC from one correctly,
#   and then fail to MOUNT that restored PVC — the exact shape of an RBD clone whose format the
#   node's kernel client refuses. Nothing in the Kubernetes API shows it. Only mounting shows it.
#   And a clone that mounts but comes back EMPTY is worse still, because a mount-only test passes
#   it. So this probe reads the data back and compares it.
#
# WHAT IT CREATES, EXACTLY
#
#   One namespace, named crystalbackup-probe-<runid>, created by this script and deleted by it.
#   Nothing is ever created outside that namespace.
#
#     --namespace NS   use a namespace you already have instead. The script then creates and
#                      deletes only the objects below, inside it, and never creates, modifies or
#                      deletes the namespace itself.
#
#   Then, per StorageClass assessed, five objects, every one named cbprobe-<runid>-<n>-*:
#
#     1  PersistentVolumeClaim  …-src        ReadWriteOnce, on that StorageClass, --size (1Gi)
#     2  Pod                    …-write      writes a known byte pattern, calls sync, exits
#     3  VolumeSnapshot         …-snap       of …-src, using the VolumeSnapshotClass the
#                                            operator itself would resolve for that driver
#     4  PersistentVolumeClaim  …-restored   dataSource: …-snap. ReadWriteOnce, or ReadOnlyMany
#                                            on a CephFS driver — the same access mode the
#                                            operator's exposer uses for that driver
#     5  Pod                    …-read       mounts …-restored READ-ONLY, reads the pattern
#                                            back, re-derives it from the same seed, compares
#
#   It modifies no object it did not create, ever. Besides creating those, it reads:
#   StorageClasses, VolumeSnapshotClasses, PVCs, Pods, VolumeSnapshots, Events, Nodes, and the
#   two pods' logs.
#
#   The two pods pull one image (--image, default busybox:1.36) and run as UID 65532, non-root,
#   with all capabilities dropped, so they are accepted by a "restricted" Pod Security namespace.
#
# WHAT IT DELETES
#
#   On a class that comes back FEASIBLE: everything that class created, and then it VERIFIES the
#   removal by polling until each object is actually gone. A delete the API accepted is not a
#   delete that happened — a stuck finalizer is precisely the case worth naming, so it is named.
#   The namespace is deleted only if this script created it AND every class came back FEASIBLE.
#
#   On any other outcome: NOTHING. Not one object. The objects ARE the evidence — the pod that
#   would not mount, its events, its volume. This script exists because that evidence took an
#   administrator thirty-six hours to obtain the hard way; it will not throw it away to leave a
#   tidy cluster. It prints where the objects are and the single command that removes them.
#
#   --keep never deletes anything, even on success.
#
# WHAT IT DOES NOT DO
#
#   It does not install CrystalBackup, does not read or write any of its objects, and needs none
#   of them to be present. It is a smoke test of the storage stack, reduced from the restore
#   fidelity gate this project runs against real Rook-Ceph (test/crucible, M6): same chain —
#   snapshot, restore, mount, read back, compare — with one small file instead of an engineered
#   corpus, and without the operator in the path. A green here is not a promise that every
#   attribute of every file survives a real restore; it is the answer to "does this cluster's
#   snapshot-restore-mount path work at all", which is the question that was open.
#
#   It prints node kernel versions ONLY inside a failure report, as context. It never checks
#   them: no reliable kernel threshold for this failure class is known to this project, and a
#   heuristic that answered "probably fine" would be worse than saying nothing.
#
# USAGE
#
#   ./snapshot-probe.sh [--storage-class NAME]… [--namespace NS] [--size SIZE] [--image REF]
#                       [--timeout SECONDS] [--dry-run] [--keep] [--json] [--no-color]
#                       [--help] [--version]
#
#   --storage-class N  assess only this StorageClass; repeatable. Default: every StorageClass
#                      whose driver has a VolumeSnapshotClass — the ones preflight reports as
#                      resolvable, which are exactly the ones whose usability is still open.
#   --namespace NS     an existing namespace to work in (see above). Default: create one.
#   --size SIZE        source PVC request, default 1Gi. This is the smallest size that
#                      provisions on every driver this project has run against; a class with a
#                      larger minimum will fail to provision and the failure will name the
#                      minimum verbatim. Raise it then — the probe does not retry at a bigger
#                      size, because a retry would destroy the evidence of why.
#   --image REF        image for the two probe pods, default busybox:1.36. It needs sh, awk,
#                      dd, sha256sum, wc and sync.
#   --timeout SECONDS  bound on each individual wait, default 300. Never an unbounded loop.
#   --dry-run          print the exact YAML of every object it WOULD create, create nothing,
#                      and exit 3 (nothing was assessed).
#   --keep             leave every object behind, including on success.
#   --json             machine-readable output on stdout, nothing else on stdout.
#   --no-color         never emit ANSI colour (also honoured: NO_COLOR=1).
#
#   Needs only kubectl, configured for the cluster you want to assess. jq is used if present and
#   is not required.
#
# EXIT CODES — the same discipline as preflight.sh: never green on an absence.
#
#   0  FEASIBLE            every StorageClass assessed went snapshot -> restore -> mount -> read
#                          and gave the pattern back byte for byte.
#   1  FEASIBLE, RESERVES  at least one class could NOT BE ASSESSED, or cleanup could not be
#                          verified. Nothing failed outright, and nothing is claimed either.
#   2  NOT FEASIBLE        at least one class broke the chain. Backups of it cannot work here.
#   3  NOT ASSESSED        the probe could not run at all, or was a --dry-run. Nothing was
#                          concluded about your cluster.
#
# VERIFYING THIS SCRIPT — do this before you run it, and here more than anywhere
#
#   sha256sum -c snapshot-probe.sh.sha256     # or: shasum -a 256 -c snapshot-probe.sh.sha256
#   cosign verify-blob snapshot-probe.sh \
#     --bundle snapshot-probe.sh.cosign.bundle \
#     --certificate-identity-regexp '^https://github\.com/CrystalBackup/CrystalBackup/' \
#     --certificate-oidc-issuer https://token.actions.githubusercontent.com
#
#   Then read it. It creates objects in your cluster; you should not take that on trust from a
#   URL. `--dry-run` prints every byte of YAML it would send, and sends none of it.
#
#   How to read the verdicts, and when to run this at all:
#   https://crystalbackup.github.io/CrystalBackup/docs/operations/snapshot-probe/
# ---------------------------------------------------------------------------------------------

# `set -e` is deliberately NOT used. Nearly every cluster call here is allowed to fail — a PVC
# that will not bind, a pod that will not mount, a snapshot that never becomes ready are the
# findings, not aborts, and aborting mid-chain is how a script leaves objects behind without
# telling you which. Every command's status is inspected where it is issued. `set -u` stays on:
# an unset variable here would be a bug in this script, and an empty argument silently flowing
# into a `kubectl delete` is the one mistake this script must never make.
set -u

SCRIPT_VERSION='1.1.0'

# >>> BEGIN GENERATED — exposer selection (make preflight-table) >>>
# Generated from internal/exposer by `make preflight-table` — do not edit by hand.
# Sources: internal/exposer/registry.go (Registry.For), exposer.go (Kind values),
# snapshot.go (snapshot API group/version). The generator additionally executes the real
# Registry.For against a fake cluster and refuses to emit a rule that disagrees with it.

CB_SNAPSHOT_GROUP='snapshot.storage.k8s.io'
CB_SNAPSHOT_VERSION='v1'
CB_KIND_CSI_GENERIC='csi-generic'
CB_KIND_CEPHFS_SHALLOW='cephfs-shallow'
CB_CEPHFS_MARKER='.cephfs.csi.'
CB_MIGRATED_TO_ANNOTATION='pv.kubernetes.io/migrated-to'

# cb_sc_driver DECLARED_PROVISIONER MIGRATED_TO
#   The driver serving a StorageClass's volumes: its pv.kubernetes.io/migrated-to
#   annotation when it carries one, else its .provisioner. Same choice driverFor makes for a
#   PVC that is bound to nothing yet.
#
#   The annotation wins because on a CSI-migrated class .provisioner names an in-tree plugin
#   that no longer serves anything: no VolumeSnapshotClass will ever carry that name, so
#   resolving through it finds none and calls a class DATA SKIPPED that is snapshotted fine.
#   This derivation is GENERATED for the same reason the rule below is — it is part of the
#   routing, and a hand-written copy of it in each script drifted within one release.
cb_sc_driver() {
	if [ -n "$2" ]; then
		printf '%s\n' "$2"
		return 0
	fi
	printf '%s\n' "$1"
}

# cb_exposer_for DRIVER HAS_SNAPSHOT_CLASS
#   DRIVER is the driver serving the volume — cb_sc_driver's answer for an unbound PVC, or the
#   PersistentVolume's own driver for a bound one. HAS_SNAPSHOT_CLASS is 'yes' when some
#   VolumeSnapshotClass in the cluster has .driver == DRIVER. Prints the exposer kind, or 'skip'.
cb_exposer_for() {
	if [ "$2" != yes ]; then
		printf '%s\n' skip
		return 0
	fi
	case $1 in
	*"$CB_CEPHFS_MARKER"*)
		printf '%s\n' "$CB_KIND_CEPHFS_SHALLOW"
		;;
	*)
		printf '%s\n' "$CB_KIND_CSI_GENERIC"
		;;
	esac
}

# cb_pick_vsclass: reads candidate VolumeSnapshotClass names on stdin, one per line, and
#   prints the one the operator would actually resolve when several classes share a driver.
#   internal/exposer's findVolumeSnapshotClass sorts the candidates and takes the first; it
#   uses Go's slices.Sort, which on strings is a byte-wise comparison, so the sort must run
#   under LC_ALL=C to reproduce it. Under a locale collation this would silently pick a
#   different class from the one the operator will.
cb_pick_vsclass() {
	LC_ALL=C sort | head -n 1
}
# <<< END GENERATED <<<

# The access mode of the restored PVC is the ONE thing the two exposers differ in
# (internal/exposer/snapshot.go, newTempPVCFromSnapshot: csi-generic passes ReadWriteOnce,
# cephfs-shallow passes ReadOnlyMany). The probe mirrors it rather than always using RWO,
# because a CephFS clone probed as RWO is not the object the operator would mount, and a verdict
# about an object nobody creates is worth nothing.
restored_access_mode() {
	case $1 in
	"$CB_KIND_CEPHFS_SHALLOW") printf 'ReadOnlyMany\n' ;;
	*) printf 'ReadWriteOnce\n' ;;
	esac
}

# --- options ------------------------------------------------------------------------------------

OUT_JSON=no
USE_COLOR=auto
DRY_RUN=no
KEEP=no
SIZE='1Gi'
IMAGE='busybox:1.36'
TIMEOUT=300
POLL=3
NS=''
NS_OWNED=yes
WANT_SC=''

KUBECTL=${KUBECTL:-kubectl}

# The pattern's seed. Deterministic and printed, so a reader can regenerate the file by hand and
# diff it themselves; the run id keeps two concurrent probes from confirming each other's data.
RUN_ID=''
SEED=''

C_RESET='' C_PASS='' C_WARN='' C_FAIL='' C_UNK='' C_DIM='' C_BOLD=''

setup_color() {
	if [ "$USE_COLOR" = no ] || [ "$OUT_JSON" = yes ]; then
		return
	fi
	if [ "$USE_COLOR" = auto ]; then
		[ -n "${NO_COLOR:-}" ] && return
		[ -t 1 ] || return
	fi
	C_RESET=$(printf '\033[0m')
	C_PASS=$(printf '\033[32m')
	C_WARN=$(printf '\033[33m')
	C_FAIL=$(printf '\033[31m')
	C_UNK=$(printf '\033[35m')
	C_DIM=$(printf '\033[2m')
	C_BOLD=$(printf '\033[1m')
}

# usage reprints this file's own header: every line from the second up to the first that is not a
# comment. A fixed line range would be a constant to keep in step with the prose above it, and it
# is the prose above it that states what this script creates in your cluster. Piped in from a URL
# there is no file to read ($0 is the shell), so it falls back to the essentials.
usage() {
	if [ -r "$0" ] && head -n 1 "$0" 2>/dev/null | grep -q '^#!'; then
		awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"
		return
	fi
	cat <<'USAGE'
CrystalBackup snapshot probe — CREATES OBJECTS in your cluster (one namespace, and per
StorageClass: 2 PVCs, 2 Pods, 1 VolumeSnapshot). On success it removes them and verifies the
removal. On failure it removes NOTHING, so the evidence survives.

Per StorageClass: write a known pattern, snapshot, restore from the snapshot, mount the restored
volume read-only, read the pattern back, compare.

  snapshot-probe.sh [--storage-class NAME]... [--namespace NS] [--size SIZE] [--image REF]
                    [--timeout SECONDS] [--dry-run] [--keep] [--json] [--no-color]

Exit: 0 feasible | 1 feasible with reservations (includes NOT ASSESSED) | 2 not feasible
      3 not assessed (also: --dry-run, --help)

Full header, checksum, signature and documentation:
  https://crystalbackup.github.io/CrystalBackup/snapshot-probe.sh
  https://crystalbackup.github.io/CrystalBackup/docs/operations/snapshot-probe/
USAGE
}

# --- accumulation ---------------------------------------------------------------------------------
#
# Same US-separated records as preflight.sh, and for the same reason: POSIX sh has no arrays, and
# tab is IFS whitespace so an empty field would silently shift every field after it.
US=$(printf '\037')
SUMMARY=''
JSON_SC=''
LEFTOVERS=''
N_FEASIBLE=0
N_NOT_FEASIBLE=0
N_NOT_ASSESSED=0
N_RESERVATION=0
CREATED_ANY=no

# out prints one line, unless --json was asked for, in which case stdout belongs to the document.
out() {
	[ "$OUT_JSON" = yes ] || printf '%s\n' "$1"
}

# out_verbatim prints a block exactly as the cluster produced it, indented and untouched. It does
# NOT collapse or sanitise: this is the evidence, and the whole point is that it is quotable into
# a bug report against your storage vendor.
out_verbatim() {
	[ "$OUT_JSON" = yes ] && return 0
	[ -n "$1" ] || return 0
	# The trailing newline is added by THIS printf and not assumed to be in the value: `read`
	# returns non-zero on a final line with no newline, so a `printf '%s'` here would silently
	# swallow the last line — and single-line evidence, which is the shape of the event that
	# matters most, is entirely a last line.
	printf '%s\n' "$1" | while IFS= read -r _ov_line; do
		printf '    %s%s%s\n' "$C_DIM" "$_ov_line" "$C_RESET"
	done
}

die_unassessed() {
	if [ "$OUT_JSON" = yes ]; then
		printf '{"schema":"crystalbackup.snapshot-probe/v1","scriptVersion":%s,"verdict":"NOT_ASSESSED","reason":%s,"storageClasses":[],"objectsLeft":[]}\n' \
			"$(json_str "$SCRIPT_VERSION")" "$(json_str "$1")"
	else
		printf '%sNOT ASSESSED%s: %s\n' "$C_FAIL" "$C_RESET" "$1" >&2
	fi
	exit 3
}

# --- JSON ------------------------------------------------------------------------------------------

HAVE_JQ=no
JSON_ENCODER='builtin'

# json_str renders its argument as a JSON string, quotes included. jq is strongly preferred here:
# the evidence this script carries is a verbatim multi-line event message, and the builtin path
# DROPS control characters — including the newlines between the lines of that message. The
# document says which encoder produced it rather than leaving the reader to guess.
json_str() {
	if [ "$HAVE_JQ" = yes ]; then
		printf '%s' "$1" | jq -Rs .
	else
		printf '"%s"' "$(printf '%s' "$1" | tr '\n\r\t' '   ' | tr -d '\000-\037' | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g')"
	fi
}

# --- kubectl -----------------------------------------------------------------------------------------

K_ERR=''
K_OUT=''
k_get() {
	K_ERR=$("$KUBECTL" get "$@" 2>&1 >/dev/null)
	K_OUT=$("$KUBECTL" get "$@" 2>/dev/null)
	[ -n "$K_ERR" ] && return 1
	return 0
}

now() { date +%s; }

# k_create sends one object, or — under --dry-run — prints it and sends nothing. The manifest is
# passed as an ARGUMENT, deliberately, and not piped in: `yaml_pod_read … | k_create` puts the
# function on the right-hand side of a pipe, which POSIX runs in a subshell, so every variable it
# sets — the captured error, the "we have created something" flag — is discarded when the pipe
# closes. The failure that costs is the first: a rejected object would have been reported with an
# empty reason, which is the one thing this script must never do.
CREATE_ERR=''
DRY_YAML=''
k_create() {
	CREATE_ERR=''
	if [ "$DRY_RUN" = yes ]; then
		DRY_YAML="${DRY_YAML}---
$1
"
		out '---'
		out "$1"
		return 0
	fi
	if ! CREATE_ERR=$(printf '%s\n' "$1" | "$KUBECTL" create -f - 2>&1 >/dev/null); then
		[ -n "$CREATE_ERR" ] || CREATE_ERR='kubectl create failed and said nothing'
		return 1
	fi
	CREATED_ANY=yes
	return 0
}

# last_warning KIND NAME — the most recent Warning event on an object, verbatim.
#
# This one line is the reason the script exists. In the incident that motivated it, the whole
# diagnosis was inside a FailedMount event that nobody had a reason to look at, on a pod that no
# longer existed by the time anybody looked.
last_warning() {
	_lw=$("$KUBECTL" -n "$NS" get events \
		--field-selector "involvedObject.kind=$1,involvedObject.name=$2,type=Warning" \
		--sort-by=.lastTimestamp \
		-o jsonpath='{range .items[*]}{.reason}{": "}{.message}{"\n"}{end}' 2>/dev/null |
		grep '.' | tail -n 1)
	if [ -z "$_lw" ]; then
		# Some distributions do not index involvedObject.kind. Fall back to name alone rather
		# than reporting "no event" for an object that has one.
		_lw=$("$KUBECTL" -n "$NS" get events \
			--field-selector "involvedObject.name=$2,type=Warning" \
			--sort-by=.lastTimestamp \
			-o jsonpath='{range .items[*]}{.reason}{": "}{.message}{"\n"}{end}' 2>/dev/null |
			grep '.' | tail -n 1)
	fi
	printf '%s' "$_lw"
	unset _lw
}

# node_kernels is CONTEXT printed inside a failure report, and never a check. See the header.
node_kernels() {
	"$KUBECTL" get nodes \
		-o jsonpath='{range .items[*]}{.metadata.name}{"  kernel "}{.status.nodeInfo.kernelVersion}{"  "}{.status.nodeInfo.osImage}{"\n"}{end}' \
		2>/dev/null | grep '.'
}

# --- bounded waits -------------------------------------------------------------------------------
#
# Every wait in this script goes through one of these three, and every one of them has a deadline
# derived from --timeout. There is no `while true` anywhere: a probe that hangs is a probe that
# gets killed with ^C halfway through, which is the one way it can leave objects behind that it
# never got to name.

# wait_field KIND NAME JSONPATH WANT — 0 when the field equals WANT, 1 on deadline.
wait_field() {
	_wf_end=$(($(now) + TIMEOUT))
	while :; do
		if [ "$("$KUBECTL" -n "$NS" get "$1" "$2" -o jsonpath="$3" 2>/dev/null)" = "$4" ]; then
			unset _wf_end
			return 0
		fi
		if [ "$(now)" -ge "$_wf_end" ]; then
			unset _wf_end
			return 1
		fi
		sleep "$POLL"
	done
}

# wait_pod_left_pending NAME — 0 as soon as the pod is no longer Pending, 1 on deadline.
#
# Leaving Pending is exactly the moment the kubelet finished attaching and mounting the volume,
# so this is the mount test, and a deadline here with a FailedMount event is the failure the
# whole script is pointed at.
wait_pod_left_pending() {
	_wp_end=$(($(now) + TIMEOUT))
	while :; do
		_wp_phase=$("$KUBECTL" -n "$NS" get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null)
		case $_wp_phase in
		Running | Succeeded | Failed)
			unset _wp_end _wp_phase
			return 0
			;;
		esac
		if [ "$(now)" -ge "$_wp_end" ]; then
			unset _wp_end _wp_phase
			return 1
		fi
		sleep "$POLL"
	done
}

# wait_pod_terminal NAME — 0 Succeeded, 1 Failed, 2 deadline.
wait_pod_terminal() {
	_wt_end=$(($(now) + TIMEOUT))
	while :; do
		_wt_phase=$("$KUBECTL" -n "$NS" get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null)
		case $_wt_phase in
		Succeeded)
			unset _wt_end _wt_phase
			return 0
			;;
		Failed)
			unset _wt_end _wt_phase
			return 1
			;;
		esac
		if [ "$(now)" -ge "$_wt_end" ]; then
			unset _wt_end _wt_phase
			return 2
		fi
		sleep "$POLL"
	done
}

# --- the objects ------------------------------------------------------------------------------------
#
# Each of these prints one object's YAML and nothing else, so that --dry-run shows precisely the
# bytes a real run sends.

yaml_namespace() {
	cat <<YAML
apiVersion: v1
kind: Namespace
metadata:
  name: ${NS}
  labels:
    app.kubernetes.io/managed-by: crystalbackup-snapshot-probe
    crystalbackup.io/probe-run: "${RUN_ID}"
YAML
}

yaml_pvc_src() {
	# $1 name, $2 storageclass
	cat <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $1
  namespace: ${NS}
  labels:
    app.kubernetes.io/managed-by: crystalbackup-snapshot-probe
    crystalbackup.io/probe-run: "${RUN_ID}"
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: $2
  resources:
    requests:
      storage: ${SIZE}
YAML
}

yaml_pvc_restored() {
	# $1 name, $2 storageclass, $3 snapshot name, $4 access mode
	cat <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $1
  namespace: ${NS}
  labels:
    app.kubernetes.io/managed-by: crystalbackup-snapshot-probe
    crystalbackup.io/probe-run: "${RUN_ID}"
spec:
  accessModes: [$4]
  storageClassName: $2
  dataSource:
    apiGroup: ${CB_SNAPSHOT_GROUP}
    kind: VolumeSnapshot
    name: $3
  resources:
    requests:
      storage: ${SIZE}
YAML
}

yaml_snapshot() {
	# $1 name, $2 volumesnapshotclass, $3 source pvc
	cat <<YAML
apiVersion: ${CB_SNAPSHOT_GROUP}/${CB_SNAPSHOT_VERSION}
kind: VolumeSnapshot
metadata:
  name: $1
  namespace: ${NS}
  labels:
    app.kubernetes.io/managed-by: crystalbackup-snapshot-probe
    crystalbackup.io/probe-run: "${RUN_ID}"
spec:
  volumeSnapshotClassName: $2
  source:
    persistentVolumeClaimName: $3
YAML
}

# The pattern. Generated by awk from the seed alone, so the reader pod can re-derive the expected
# bytes without being told the digest — a reader handed the digest could only prove the file is
# unchanged since the write, not that the write's own content ever arrived.
PATTERN_PROG='BEGIN { for (i = 0; i < 2048; i++) printf "crystalbackup-probe %s %06d 0123456789abcdef0123456789abcdef\n", seed, i }'

yaml_pod_write() {
	# $1 name, $2 pvc
	cat <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: ${NS}
  labels:
    app.kubernetes.io/managed-by: crystalbackup-snapshot-probe
    crystalbackup.io/probe-run: "${RUN_ID}"
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    fsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: write
      image: ${IMAGE}
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: [ALL]
      command: ["/bin/sh", "-c"]
      args:
        - |
          set -eu
          awk -v seed="${SEED}" '${PATTERN_PROG}' > /data/probe.dat
          sync
          sync
          echo "PROBE-WRITE sha256=\$(sha256sum /data/probe.dat | cut -d' ' -f1) bytes=\$(wc -c < /data/probe.dat)"
      volumeMounts:
        - name: data
          mountPath: /data
      resources:
        requests:
          cpu: 50m
          memory: 64Mi
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: $2
YAML
}

yaml_pod_read() {
	# $1 name, $2 restored pvc
	#
	# readOnly is set on BOTH the volume source and the mount. Read-only is not a nicety here: the
	# probe must not be able to repair, extend or otherwise disturb the restored clone it is
	# judging, and mounting a snapshot-backed volume read-only is what the operator's exposer
	# does with it.
	cat <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: ${NS}
  labels:
    app.kubernetes.io/managed-by: crystalbackup-snapshot-probe
    crystalbackup.io/probe-run: "${RUN_ID}"
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: read
      image: ${IMAGE}
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: [ALL]
      command: ["/bin/sh", "-c"]
      args:
        - |
          set -eu
          if [ ! -f /data/probe.dat ]; then
            echo "PROBE-READ ABSENT the restored volume mounted, and /data/probe.dat is not on it"
            ls -la /data || true
            exit 1
          fi
          awk -v seed="${SEED}" '${PATTERN_PROG}' > /tmp/expected.dat
          want=\$(sha256sum /tmp/expected.dat | cut -d' ' -f1)
          got=\$(sha256sum /data/probe.dat | cut -d' ' -f1)
          bytes=\$(wc -c < /data/probe.dat)
          if [ "\$want" = "\$got" ]; then
            echo "PROBE-READ MATCH sha256=\$got bytes=\$bytes"
          else
            echo "PROBE-READ MISMATCH want=\$want got=\$got bytes=\$bytes"
            exit 1
          fi
      volumeMounts:
        - name: data
          mountPath: /data
          readOnly: true
      resources:
        requests:
          cpu: 50m
          memory: 64Mi
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: $2
        readOnly: true
YAML
}

# --- per-class result plumbing ------------------------------------------------------------------

# CHAIN is the snapshot -> restore -> mount -> read progress of the class being assessed, built as
# it goes so a broken chain prints exactly how far it got and no further.
CHAIN=''
chain_ok() {
	if [ -z "$CHAIN" ]; then
		CHAIN="$1 OK"
	else
		CHAIN="$CHAIN · $1 OK"
	fi
}
chain_failed() {
	_cf=$(printf '%s' "$1" | tr '[:lower:]' '[:upper:]')
	if [ -z "$CHAIN" ]; then
		CHAIN="$_cf FAILED"
	else
		CHAIN="$CHAIN · $_cf FAILED"
	fi
	unset _cf
}

# record_class SC VERDICT STAGE REASON EVIDENCE
record_class() {
	SUMMARY="${SUMMARY}${1}${US}${2}${US}${CHAIN}${US}${3}
"
	[ -n "$JSON_SC" ] && JSON_SC="${JSON_SC},"
	JSON_SC="${JSON_SC}{\"storageClass\":$(json_str "$1")"
	# "provisioner" keeps its meaning — what the class declares. The driver every decision was
	# actually made for is a separate key, because on a CSI-migrated class it is a different string
	# and a consumer that assumed they were the same would be wrong in silence.
	JSON_SC="${JSON_SC},\"provisioner\":$(json_str "$SC_PROV")"
	JSON_SC="${JSON_SC},\"effectiveDriver\":$(json_str "$SC_DRIVER")"
	JSON_SC="${JSON_SC},\"csiMigrated\":$([ "$SC_DRIVER" != "$SC_PROV" ] && printf true || printf false)"
	JSON_SC="${JSON_SC},\"volumeSnapshotClass\":$(json_str "$SC_VSCLASS")"
	JSON_SC="${JSON_SC},\"exposer\":$(json_str "$SC_EXPOSER")"
	JSON_SC="${JSON_SC},\"verdict\":$(json_str "$2")"
	JSON_SC="${JSON_SC},\"chain\":$(json_str "$CHAIN")"
	JSON_SC="${JSON_SC},\"failedStage\":$([ -n "$3" ] && json_str "$3" || printf null)"
	JSON_SC="${JSON_SC},\"reason\":$(json_str "$4")"
	JSON_SC="${JSON_SC},\"evidence\":$([ -n "$5" ] && json_str "$5" || printf null)}"

	case $2 in
	FEASIBLE) N_FEASIBLE=$((N_FEASIBLE + 1)) ;;
	NOT_FEASIBLE) N_NOT_FEASIBLE=$((N_NOT_FEASIBLE + 1)) ;;
	*) N_NOT_ASSESSED=$((N_NOT_ASSESSED + 1)) ;;
	esac
}

# fail_class prints the whole failure report for one class: the chain, the verbatim evidence, the
# consequence in one sentence, the kernels as context, and where the objects are.
fail_class() {
	# $1 sc, $2 verdict, $3 stage, $4 reason, $5 evidence
	if [ "$2" = NOT_FEASIBLE ]; then
		out "$(printf '%s%s%s: %s%s%s' "$C_BOLD" "$1" "$C_RESET" "$C_FAIL" "$CHAIN" "$C_RESET")"
	else
		out "$(printf '%s%s%s: %s%s%s' "$C_BOLD" "$1" "$C_RESET" "$C_UNK" "${CHAIN:-NOT ASSESSED}" "$C_RESET")"
	fi
	out "  $4"
	if [ -n "$5" ]; then
		out_verbatim "$5"
	fi
	if [ "$2" = NOT_FEASIBLE ]; then
		out "$(printf '  %s→ backups of this StorageClass cannot work on this cluster%s' "$C_FAIL" "$C_RESET")"
	else
		out "$(printf '  %s→ NOT ASSESSED. This is not a pass: the question is still open.%s' "$C_UNK" "$C_RESET")"
	fi
	out ''
	out "$(printf '  %sthe objects were left in place, on purpose — they are the evidence:%s' "$C_DIM" "$C_RESET")"
	out "$(printf '  %s  kubectl -n %s get pvc,pod,volumesnapshot -l crystalbackup.io/probe-run=%s%s' \
		"$C_DIM" "$NS" "$RUN_ID" "$C_RESET")"
	out "$(printf '  %s  kubectl -n %s describe pod -l crystalbackup.io/probe-run=%s%s' \
		"$C_DIM" "$NS" "$RUN_ID" "$C_RESET")"
	if [ "$3" = mount ] || [ "$3" = read ]; then
		out ''
		out "$(printf '  %snode kernels, as CONTEXT and not as a check — no kernel threshold is known%s' "$C_DIM" "$C_RESET")"
		out "$(printf '  %sfor this failure, so nothing here says "probably fine":%s' "$C_DIM" "$C_RESET")"
		out_verbatim "$(node_kernels)"
	fi
	out ''
	record_class "$1" "$2" "$3" "$4" "$5"
}

# --- cleanup ------------------------------------------------------------------------------------

# delete_and_verify KIND NAME — deletes, then polls until the object is actually gone. A delete
# the API accepted is not a delete that happened; a finalizer nothing will release is the case
# worth naming, and it is named.
delete_and_verify() {
	"$KUBECTL" -n "$NS" delete "$1" "$2" --wait=false >/dev/null 2>&1
	_dv_end=$(($(now) + TIMEOUT))
	while :; do
		if ! "$KUBECTL" -n "$NS" get "$1" "$2" >/dev/null 2>&1; then
			unset _dv_end
			return 0
		fi
		if [ "$(now)" -ge "$_dv_end" ]; then
			LEFTOVERS="${LEFTOVERS}${NS}/$1/$2
"
			N_RESERVATION=$((N_RESERVATION + 1))
			unset _dv_end
			return 1
		fi
		sleep "$POLL"
	done
}

cleanup_class() {
	# In reverse dependency order: the pods release the volumes, then the restored claim releases
	# the snapshot, then the snapshot releases the source. Deleting a VolumeSnapshot that a PVC is
	# still restoring from is how a VolumeSnapshotContent ends up stranded.
	delete_and_verify pod "$5"
	delete_and_verify pod "$2"
	delete_and_verify pvc "$4"
	delete_and_verify volumesnapshot "$3"
	delete_and_verify pvc "$1"
}

# --- one StorageClass, end to end ------------------------------------------------------------------

SC_PROV=''
SC_DRIVER=''
SC_VSCLASS=''
SC_EXPOSER=''

probe_class() {
	# $1 index, $2 storageclass, $3 declared provisioner, $4 effective driver,
	# $5 volumesnapshotclass, $6 exposer kind, $7 volumeBindingMode
	#
	# The provisioner and the driver are separate arguments because on a CSI-migrated class they are
	# separate strings, and every decision below belongs to the driver while the report belongs to
	# the provisioner.
	_idx=$1
	_sc=$2
	SC_PROV=$3
	SC_DRIVER=$4
	SC_VSCLASS=$5
	SC_EXPOSER=$6
	_binding=$7
	CHAIN=''

	_base="cbprobe-${RUN_ID}-${_idx}"
	_src="${_base}-src"
	_writer="${_base}-write"
	_snap="${_base}-snap"
	_restored="${_base}-restored"
	_reader="${_base}-read"
	_mode=$(restored_access_mode "$SC_EXPOSER")

	out "$(printf '%s%s%s  %svia %s, exposer %s, restored clone %s%s' \
		"$C_BOLD" "$_sc" "$C_RESET" "$C_DIM" "$SC_VSCLASS" "$SC_EXPOSER" "$_mode" "$C_RESET")"
	if [ "$SC_DRIVER" != "$SC_PROV" ]; then
		out "$(printf '  %sCSI-migrated: declared provisioner %s is a superseded in-tree plugin; the%s' \
			"$C_DIM" "$SC_PROV" "$C_RESET")"
		out "$(printf '  %sdriver serving this class — and the one resolved above — is %s%s' \
			"$C_DIM" "$SC_DRIVER" "$C_RESET")"
	fi

	if [ "$DRY_RUN" = yes ]; then
		k_create "$(yaml_pvc_src "$_src" "$_sc")"
		k_create "$(yaml_pod_write "$_writer" "$_src")"
		k_create "$(yaml_snapshot "$_snap" "$SC_VSCLASS" "$_src")"
		k_create "$(yaml_pvc_restored "$_restored" "$_sc" "$_snap" "$_mode")"
		k_create "$(yaml_pod_read "$_reader" "$_restored")"
		return 0
	fi

	# ── seed: provision a source volume and put a known pattern on it ────────────────────────
	if ! k_create "$(yaml_pvc_src "$_src" "$_sc")"; then
		fail_class "$_sc" NOT_ASSESSED provision \
			"the source PersistentVolumeClaim could not even be created, so nothing downstream was tried." \
			"$CREATE_ERR"
		return 0
	fi
	# A WaitForFirstConsumer class will not bind until a pod wants it, so waiting for Bound first
	# would time out on a perfectly healthy class. Under Immediate, waiting here buys an early and
	# far more legible failure than a pod stuck Pending for an unstated reason.
	if [ "$_binding" != WaitForFirstConsumer ]; then
		if ! wait_field pvc "$_src" '{.status.phase}' Bound; then
			fail_class "$_sc" NOT_ASSESSED provision \
				"the source volume never bound within ${TIMEOUT}s, so the snapshot chain was never started." \
				"$(last_warning PersistentVolumeClaim "$_src")"
			return 0
		fi
	fi

	if ! k_create "$(yaml_pod_write "$_writer" "$_src")"; then
		fail_class "$_sc" NOT_ASSESSED write \
			"the writer Pod could not be created." "$CREATE_ERR"
		return 0
	fi
	wait_pod_terminal "$_writer"
	case $? in
	0) : ;;
	1)
		fail_class "$_sc" NOT_ASSESSED write \
			"the writer Pod failed, so no known pattern was ever put on the source volume and nothing downstream would have meant anything." \
			"$(last_warning Pod "$_writer")
$("$KUBECTL" -n "$NS" logs "$_writer" 2>&1)"
		return 0
		;;
	*)
		fail_class "$_sc" NOT_ASSESSED write \
			"the writer Pod did not finish within ${TIMEOUT}s. Raise --timeout if this class is simply slow." \
			"$(last_warning Pod "$_writer")"
		return 0
		;;
	esac
	_wlog=$("$KUBECTL" -n "$NS" logs "$_writer" 2>/dev/null)
	_wsha=$(printf '%s' "$_wlog" | sed -n 's/.*PROBE-WRITE sha256=\([0-9a-f]*\).*/\1/p' | head -n 1)
	if [ -z "$_wsha" ]; then
		fail_class "$_sc" NOT_ASSESSED write \
			"the writer Pod succeeded but did not report a digest, so there is no pattern to compare against." \
			"$_wlog"
		return 0
	fi
	out "$(printf '  %sseeded: %s bytes of pattern, sha256 %s%s' "$C_DIM" \
		"$(printf '%s' "$_wlog" | sed -n 's/.*bytes=\([0-9]*\).*/\1/p' | head -n 1)" "$_wsha" "$C_RESET")"

	# ── snapshot ───────────────────────────────────────────────────────────────────────────────
	if ! k_create "$(yaml_snapshot "$_snap" "$SC_VSCLASS" "$_src")"; then
		chain_failed snapshot
		fail_class "$_sc" NOT_FEASIBLE snapshot \
			"the VolumeSnapshot was rejected by the API." "$CREATE_ERR"
		return 0
	fi
	if ! wait_field volumesnapshot "$_snap" '{.status.readyToUse}' true; then
		_err=$("$KUBECTL" -n "$NS" get volumesnapshot "$_snap" \
			-o jsonpath='{.status.error.message}' 2>/dev/null)
		if [ -n "$_err" ]; then
			chain_failed snapshot
			fail_class "$_sc" NOT_FEASIBLE snapshot \
				"the VolumeSnapshot reported an error instead of becoming ready." "$_err"
		else
			chain_failed snapshot
			fail_class "$_sc" NOT_ASSESSED snapshot \
				"the VolumeSnapshot did not become ready within ${TIMEOUT}s and reported no error. That is not a failure of the driver, it is an unfinished measurement — raise --timeout, or check that a snapshot controller is running." \
				"$(last_warning VolumeSnapshot "$_snap")"
		fi
		unset _err
		return 0
	fi
	chain_ok snapshot

	# ── restore ────────────────────────────────────────────────────────────────────────────────
	if ! k_create "$(yaml_pvc_restored "$_restored" "$_sc" "$_snap" "$_mode")"; then
		chain_failed restore
		fail_class "$_sc" NOT_FEASIBLE restore \
			"the restored PersistentVolumeClaim was rejected by the API." "$CREATE_ERR"
		return 0
	fi
	if [ "$_binding" != WaitForFirstConsumer ]; then
		if ! wait_field pvc "$_restored" '{.status.phase}' Bound; then
			_ev=$(last_warning PersistentVolumeClaim "$_restored")
			chain_failed restore
			if [ -n "$_ev" ]; then
				fail_class "$_sc" NOT_FEASIBLE restore \
					"a PersistentVolumeClaim could not be provisioned from the snapshot." "$_ev"
			else
				fail_class "$_sc" NOT_ASSESSED restore \
					"the restored claim did not bind within ${TIMEOUT}s and no Warning event was recorded. Raise --timeout." \
					''
			fi
			unset _ev
			return 0
		fi
	fi
	chain_ok restore

	# ── mount ──────────────────────────────────────────────────────────────────────────────────
	#
	# This is the stage the whole script exists for. A restored clone the API is perfectly happy
	# with can still be unmountable on the node, and the node is where it says so.
	if ! k_create "$(yaml_pod_read "$_reader" "$_restored")"; then
		chain_failed mount
		fail_class "$_sc" NOT_FEASIBLE mount \
			"the reader Pod was rejected by the API." "$CREATE_ERR"
		return 0
	fi
	if ! wait_pod_left_pending "$_reader"; then
		# Under WaitForFirstConsumer the restored claim was not waited on above, so a pod stuck
		# in Pending may be stuck on PROVISIONING rather than on mounting. Blaming the mount for
		# a restore that never happened would send the reader after the wrong subsystem, so the
		# claim is asked first and the stage is named after whichever one is actually stuck.
		if [ "$("$KUBECTL" -n "$NS" get pvc "$_restored" -o jsonpath='{.status.phase}' 2>/dev/null)" != Bound ]; then
			_ev=$(last_warning PersistentVolumeClaim "$_restored")
			chain_failed restore
			if [ -n "$_ev" ]; then
				fail_class "$_sc" NOT_FEASIBLE restore \
					"a PersistentVolumeClaim could not be provisioned from the snapshot (its binding mode is WaitForFirstConsumer, so it was the reader Pod that triggered the attempt)." \
					"$_ev"
			else
				fail_class "$_sc" NOT_ASSESSED restore \
					"the restored claim did not bind within ${TIMEOUT}s and no Warning event was recorded. Raise --timeout." \
					''
			fi
			unset _ev
			return 0
		fi
		_ev=$(last_warning Pod "$_reader")
		chain_failed mount
		if [ -n "$_ev" ]; then
			fail_class "$_sc" NOT_FEASIBLE mount \
				"the restored volume could not be mounted on the node. The snapshot and the restore both succeeded — this is the gap that no Kubernetes object shows:" \
				"$_ev"
		else
			fail_class "$_sc" NOT_ASSESSED mount \
				"the reader Pod was still Pending after ${TIMEOUT}s and no Warning event was recorded — it may simply be unscheduled. Raise --timeout, or check node capacity." \
				"$("$KUBECTL" -n "$NS" get pod "$_reader" -o wide 2>&1)"
		fi
		unset _ev
		return 0
	fi
	chain_ok mount

	# ── read back ──────────────────────────────────────────────────────────────────────────────
	#
	# Mounting proves the map. Reading proves the data path, and only reading catches the clone
	# that mounts cleanly and is empty — which a mount-only test would report as a pass.
	wait_pod_terminal "$_reader"
	_rc=$?
	_rlog=$("$KUBECTL" -n "$NS" logs "$_reader" 2>&1)
	if [ "$_rc" = 2 ]; then
		chain_failed read
		fail_class "$_sc" NOT_ASSESSED read \
			"the reader Pod mounted the restored volume but did not finish within ${TIMEOUT}s." \
			"$_rlog"
		return 0
	fi
	case $_rlog in
	*"PROBE-READ MATCH"*) : ;;
	*"PROBE-READ MISMATCH"*)
		chain_failed read
		fail_class "$_sc" NOT_FEASIBLE read \
			"the restored volume MOUNTED, and gave back different bytes from the ones that were written. A backup taken through this path would be silently wrong:" \
			"$_rlog"
		return 0
		;;
	*"PROBE-READ ABSENT"*)
		chain_failed read
		fail_class "$_sc" NOT_FEASIBLE read \
			"the restored volume MOUNTED and is EMPTY — the file that was written and synced before the snapshot is not on it. This is the failure a mount-only test reports as a pass:" \
			"$_rlog"
		return 0
		;;
	*)
		chain_failed read
		fail_class "$_sc" NOT_ASSESSED read \
			"the reader Pod produced no verdict line, so nothing was compared." "$_rlog"
		return 0
		;;
	esac
	# Second, independent comparison: the reader re-derived the pattern from the seed and matched
	# it; this checks that what it matched is also what the writer actually wrote.
	_rsha=$(printf '%s' "$_rlog" | sed -n 's/.*PROBE-READ MATCH sha256=\([0-9a-f]*\).*/\1/p' | head -n 1)
	if [ "$_rsha" != "$_wsha" ]; then
		chain_failed read
		fail_class "$_sc" NOT_FEASIBLE read \
			"the restored volume's digest does not match what the writer reported it had written." \
			"writer: ${_wsha}
reader: ${_rsha}"
		return 0
	fi
	chain_ok read

	out "$(printf '%s%s%s: %s%s%s' "$C_BOLD" "$_sc" "$C_RESET" "$C_PASS" "$CHAIN" "$C_RESET")"
	out "$(printf '  %s→ a snapshot of this StorageClass can be restored, mounted and read back exactly%s' \
		"$C_PASS" "$C_RESET")"
	record_class "$_sc" FEASIBLE '' \
		"snapshot, restore, read-only mount and byte-for-byte read-back all succeeded (sha256 ${_wsha})" ''

	if [ "$KEEP" = yes ]; then
		out "$(printf '  %s--keep: the objects were left in place%s' "$C_DIM" "$C_RESET")"
	else
		cleanup_class "$_src" "$_writer" "$_snap" "$_restored" "$_reader"
	fi
	out ''
	unset _idx _sc _binding _base _src _writer _snap _restored _reader _mode _wlog _wsha _rlog _rsha _rc
}

# --- discovery -----------------------------------------------------------------------------------

# select_classes prints one US-separated row per StorageClass:
#
#   name / declared provisioner / effective driver / vsclass / exposer / bindingMode
#
# The declared provisioner and the effective driver are BOTH carried because they are not always the
# same string. On a StorageClass whose in-tree plugin a CSI driver has superseded, .provisioner names
# the retired plugin and the pv.kubernetes.io/migrated-to annotation names the driver that will
# actually serve the PVC this probe is about to create — so the driver is what the VolumeSnapshotClass
# lookup and the exposer choice must use, while the provisioner is what the report should print. The
# derivation is not written here: cb_sc_driver, in the generated region, owns it, and preflight.sh
# reads it from the same place. A hand-written copy in each script is what drifted last time.
#
# US (0x1f) and not tab, for the reason preflight.sh states at length and this script proved by
# getting it wrong first: tab is IFS *whitespace*, so `IFS=<tab> read` collapses runs of tabs and
# an empty field silently shifts every field after it. The class with no VolumeSnapshotClass has
# exactly that — an empty vsclass — so with tabs the one row that must be recognised as "nothing
# to probe here" came back with the binding mode sitting in the exposer's place, and the probe
# went off to create five objects on a StorageClass that cannot be snapshotted at all. The
# migrated-to annotation, absent on almost every class, is a second empty middle field, so the
# kubectl output is translated to US before it is read.
VSCLASSES=''
select_classes() {
	if ! k_get volumesnapshotclass -o jsonpath='{range .items[*]}{.driver}{"	"}{.metadata.name}{"\n"}{end}'; then
		die_unassessed "cannot list VolumeSnapshotClasses ($(printf '%s' "$K_ERR" | tr '\n' ' ')). Without them no snapshot can be requested and nothing was created."
	fi
	VSCLASSES=$K_OUT

	if ! k_get storageclass -o jsonpath='{range .items[*]}{.metadata.name}{"	"}{.provisioner}{"	"}{.metadata.annotations.pv\.kubernetes\.io/migrated-to}{"	"}{.volumeBindingMode}{"\n"}{end}'; then
		die_unassessed "cannot list StorageClasses ($(printf '%s' "$K_ERR" | tr '\n' ' ')). Nothing was created."
	fi
	printf '%s\n' "$K_OUT" | tr '\t' "$US" | while IFS="$US" read -r _n _p _m _b; do
		[ -n "$_n" ] || continue
		if [ -n "$WANT_SC" ]; then
			case $WANT_SC in
			*"${US}${_n}${US}"*) ;;
			*) continue ;;
			esac
		fi
		_d=$(cb_sc_driver "$_p" "$_m")
		_c=$(printf '%s\n' "$VSCLASSES" | awk -F'\t' -v p="$_d" '$1==p {print $2}' | grep '.' | cb_pick_vsclass)
		_h=no
		[ -n "$_c" ] && _h=yes
		_e=$(cb_exposer_for "$_d" "$_h")
		printf '%s%s%s%s%s%s%s%s%s%s%s\n' \
			"$_n" "$US" "$_p" "$US" "$_d" "$US" "$_c" "$US" "$_e" "$US" "${_b:-Immediate}"
	done
}

# --- reporting --------------------------------------------------------------------------------------

verdict_of() {
	if [ "$N_NOT_FEASIBLE" -gt 0 ]; then
		printf 'NOT_FEASIBLE'
	elif [ "$N_NOT_ASSESSED" -gt 0 ] || [ "$N_RESERVATION" -gt 0 ]; then
		printf 'FEASIBLE_WITH_RESERVATIONS'
	elif [ "$N_FEASIBLE" -gt 0 ]; then
		printf 'FEASIBLE'
	else
		printf 'NOT_ASSESSED'
	fi
}

exit_code_of() {
	case $(verdict_of) in
	NOT_FEASIBLE) printf 2 ;;
	FEASIBLE_WITH_RESERVATIONS) printf 1 ;;
	FEASIBLE) printf 0 ;;
	*) printf 3 ;;
	esac
}

report_text_tail() {
	out "$(printf '%sSUMMARY%s' "$C_BOLD" "$C_RESET")"
	if ! printf '%s' "$SUMMARY" | grep -q '.'; then
		out "$(printf '  %sno StorageClass was assessed.%s' "$C_UNK" "$C_RESET")"
	else
		while IFS="$US" read -r _n _v _ch _st; do
			[ -n "$_n" ] || continue
			case $_v in
			FEASIBLE) _c=$C_PASS ;;
			NOT_FEASIBLE) _c=$C_FAIL ;;
			*) _c=$C_UNK ;;
			esac
			out "$(printf '  %s%-26s%s %s%-14s%s %s' "$C_BOLD" "$_n" "$C_RESET" "$_c" \
				"$(printf '%s' "$_v" | tr '_' ' ')" "$C_RESET" "${_ch:-—}")"
		done <<EOF
$SUMMARY
EOF
	fi

	if printf '%s' "$LEFTOVERS" | grep -q '.'; then
		out ''
		out "$(printf '%sNOT CLEANED UP%s' "$C_WARN" "$C_RESET")"
		out '  these objects were deleted and were still there when the wait expired:'
		out_verbatim "$(printf '%s' "$LEFTOVERS" | grep '.')"
		out '  A delete the API accepted is not a delete that happened. Look for a finalizer.'
	fi

	_v=$(verdict_of)
	case $_v in
	NOT_FEASIBLE) _c=$C_FAIL ;;
	FEASIBLE) _c=$C_PASS ;;
	*) _c=$C_UNK ;;
	esac
	out ''
	out "$(printf '%sVERDICT%s  %s%s%s   (%s feasible, %s not feasible, %s not assessed, %s cleanup reservation(s))' \
		"$C_BOLD" "$C_RESET" "$_c$C_BOLD" "$_v" "$C_RESET" \
		"$N_FEASIBLE" "$N_NOT_FEASIBLE" "$N_NOT_ASSESSED" "$N_RESERVATION")"
	if [ "$N_NOT_ASSESSED" -gt 0 ]; then
		out "$(printf '%s  %s class(es) could not be assessed. They are NOT counted as feasible.%s' \
			"$C_UNK" "$N_NOT_ASSESSED" "$C_RESET")"
	fi
	if [ "$N_NOT_FEASIBLE" -gt 0 ] || [ "$N_NOT_ASSESSED" -gt 0 ] || [ "$KEEP" = yes ]; then
		if [ "$CREATED_ANY" = yes ]; then
			out ''
			out "$(printf '%sOBJECTS LEFT BEHIND%s  in namespace %s' "$C_BOLD" "$C_RESET" "$NS")"
			if [ "$KEEP" = yes ]; then
				out '  --keep was given, so nothing was removed. When you are done:'
			else
				out '  Nothing was cleaned up for a class that did not come back FEASIBLE: the objects and'
				out '  their events are the evidence, and they are not reproducible once deleted. Read them,'
				out '  then remove everything this run made with:'
			fi
			if [ "$NS_OWNED" = yes ]; then
				out "$(printf '    kubectl delete namespace %s' "$NS")"
			else
				out "$(printf '    kubectl -n %s delete pvc,pod,volumesnapshot -l crystalbackup.io/probe-run=%s' "$NS" "$RUN_ID")"
			fi
		fi
	fi
	out "$(printf 'exit %s' "$(exit_code_of)")"
	unset _v _c
}

report_json() {
	printf '{"schema":"crystalbackup.snapshot-probe/v1"'
	printf ',"scriptVersion":%s' "$(json_str "$SCRIPT_VERSION")"
	printf ',"jsonEncoder":%s' "$(json_str "$JSON_ENCODER")"
	printf ',"runId":%s' "$(json_str "$RUN_ID")"
	printf ',"namespace":%s' "$(json_str "$NS")"
	printf ',"namespaceCreatedByProbe":%s' "$([ "$NS_OWNED" = yes ] && printf true || printf false)"
	printf ',"objectsLeftBehind":%s' "$([ "$(verdict_of)" = FEASIBLE ] && [ "$KEEP" = no ] && printf false || printf true)"
	printf ',"verdict":%s' "$(json_str "$(verdict_of)")"
	printf ',"exitCode":%s' "$(exit_code_of)"
	printf ',"summary":{"feasible":%s,"notFeasible":%s,"notAssessed":%s,"reservations":%s}' \
		"$N_FEASIBLE" "$N_NOT_FEASIBLE" "$N_NOT_ASSESSED" "$N_RESERVATION"
	printf ',"storageClasses":[%s]' "$JSON_SC"
	printf ',"notCleanedUp":['
	_first=1
	while IFS= read -r _l; do
		[ -n "$_l" ] || continue
		[ "$_first" = 1 ] || printf ','
		_first=0
		json_str "$_l"
	done <<EOF
$LEFTOVERS
EOF
	printf ']}\n'
	unset _first _l
}

# --- main -----------------------------------------------------------------------------------------

_need_value() {
	[ -n "$2" ] || {
		printf '%s needs a value (try --help)\n' "$1" >&2
		exit 3
	}
}

while [ $# -gt 0 ]; do
	case $1 in
	--json) OUT_JSON=yes ;;
	--no-color) USE_COLOR=no ;;
	--dry-run) DRY_RUN=yes ;;
	--keep) KEEP=yes ;;
	--storage-class)
		_need_value "$1" "${2:-}"
		WANT_SC="${WANT_SC}${US}${2}"
		shift
		;;
	--namespace)
		_need_value "$1" "${2:-}"
		NS=$2
		NS_OWNED=no
		shift
		;;
	--size)
		_need_value "$1" "${2:-}"
		SIZE=$2
		shift
		;;
	--image)
		_need_value "$1" "${2:-}"
		IMAGE=$2
		shift
		;;
	--timeout)
		_need_value "$1" "${2:-}"
		TIMEOUT=$2
		shift
		;;
	-h | --help)
		usage
		exit 3
		;;
	--version)
		printf 'crystalbackup-snapshot-probe %s\n' "$SCRIPT_VERSION"
		exit 3
		;;
	*)
		printf 'unknown option: %s (try --help)\n' "$1" >&2
		exit 3
		;;
	esac
	shift
done
[ -n "$WANT_SC" ] && WANT_SC="${WANT_SC}${US}"

setup_color
if command -v jq >/dev/null 2>&1; then
	HAVE_JQ=yes
	JSON_ENCODER='jq'
fi

case $TIMEOUT in
'' | *[!0-9]*) die_unassessed "--timeout must be a whole number of seconds, got '${TIMEOUT}'." ;;
esac
[ "$TIMEOUT" -ge 10 ] || die_unassessed "--timeout must be at least 10 seconds, got '${TIMEOUT}'."

command -v "$KUBECTL" >/dev/null 2>&1 ||
	die_unassessed "kubectl not found on PATH. Nothing was created and nothing about this cluster was checked."
"$KUBECTL" get --raw /version >/dev/null 2>&1 ||
	die_unassessed "cannot reach the cluster API. Nothing was created."

RUN_ID="$(date +%Y%m%d%H%M%S)-$$"
SEED="cbprobe-${RUN_ID}"
[ -n "$NS" ] || NS="crystalbackup-probe-${RUN_ID}"

out "$(printf '%sCrystalBackup snapshot probe%s  %s(this CREATES objects; see --help)%s' \
	"$C_BOLD" "$C_RESET" "$C_DIM" "$C_RESET")"
out '---------------------------------------------------------------------------'
if [ "$DRY_RUN" = yes ]; then
	out "$(printf '%sDRY RUN%s — the YAML below is exactly what would be sent, and none of it is sent.' \
		"$C_BOLD" "$C_RESET")"
	out "$(printf 'namespace: %s (%s)' "$NS" "$([ "$NS_OWNED" = yes ] && printf 'would be created and deleted' || printf 'yours; untouched')")"
	out ''
	if [ "$NS_OWNED" = yes ]; then
		k_create "$(yaml_namespace)"
	fi
else
	out "$(printf 'namespace: %s %s(%s)%s' "$NS" "$C_DIM" \
		"$([ "$NS_OWNED" = yes ] && printf 'created by this run' || printf 'yours; this run creates and deletes only the probe objects in it')" \
		"$C_RESET")"
	out ''
	if [ "$NS_OWNED" = yes ]; then
		if ! k_create "$(yaml_namespace)"; then
			die_unassessed "could not create namespace ${NS}: $(printf '%s' "$CREATE_ERR" | tr '\n' ' ')"
		fi
	elif ! "$KUBECTL" get namespace "$NS" >/dev/null 2>&1; then
		die_unassessed "namespace ${NS} does not exist. Nothing was created; --namespace names a namespace you already have."
	fi
fi

# Selection is materialised into a file-less here-doc string first: the loop below must run in
# THIS shell, not a subshell, or every counter it increments is discarded when the pipe closes —
# which would report a verdict of NOT_ASSESSED over a run that assessed everything.
SELECTED=$(select_classes)

_i=0
while IFS="$US" read -r _n _p _drv _c _e _b; do
	[ -n "$_n" ] || continue
	_i=$((_i + 1))
	if [ "$_e" = skip ]; then
		SC_PROV=$_p
		SC_DRIVER=$_drv
		SC_VSCLASS=''
		SC_EXPOSER=skip
		CHAIN=''
		out "$(printf '%s%s%s: %sNOT ASSESSED%s' "$C_BOLD" "$_n" "$C_RESET" "$C_UNK" "$C_RESET")"
		out "  no VolumeSnapshotClass has driver '${_drv}' — there is no snapshot to take, so there is"
		out '  nothing here to probe. preflight.sh already reports this class as data-skipped.'
		if [ "$_drv" != "$_p" ]; then
			out "  (that driver is the one in this class's $CB_MIGRATED_TO_ANNOTATION annotation, not its"
			out "  declared provisioner '${_p}', which names an in-tree plugin a CSI driver has superseded.)"
		fi
		out ''
		record_class "$_n" NOT_ASSESSED '' \
			"no VolumeSnapshotClass has driver '${_drv}'; volumes on this class are skipped at backup time and there is nothing to probe" ''
		continue
	fi
	probe_class "$_i" "$_n" "$_p" "$_drv" "$_c" "$_e" "$_b"
done <<EOF
$SELECTED
EOF

if [ "$DRY_RUN" = yes ]; then
	out ''
	out "$(printf '%sNOT ASSESSED%s — this was a dry run. Nothing was created, and nothing was concluded' \
		"$C_UNK" "$C_RESET")"
	out '  about your cluster. A dry run that exited 0 would be a green on an absence.'
	if [ "$OUT_JSON" = yes ]; then
		printf '{"schema":"crystalbackup.snapshot-probe/v1","scriptVersion":%s,"verdict":"NOT_ASSESSED","reason":"--dry-run: nothing was created","wouldCreate":%s,"storageClasses":[],"objectsLeft":[]}\n' \
			"$(json_str "$SCRIPT_VERSION")" "$(json_str "$DRY_YAML")"
	fi
	exit 3
fi

if [ "$_i" -eq 0 ]; then
	if [ "$NS_OWNED" = yes ]; then
		"$KUBECTL" delete namespace "$NS" --wait=false >/dev/null 2>&1
	fi
	die_unassessed "no StorageClass matched. Nothing was probed.$([ -n "$WANT_SC" ] && printf ' (--storage-class named a class this cluster does not have)')"
fi

# The namespace goes only when this script made it, nothing is being kept as evidence, and every
# object inside it was already confirmed gone. Deleting the namespace over an object that would
# not delete would erase the one interesting thing about the run.
if [ "$NS_OWNED" = yes ] && [ "$KEEP" = no ] &&
	[ "$N_NOT_FEASIBLE" -eq 0 ] && [ "$N_NOT_ASSESSED" -eq 0 ] && [ "$N_RESERVATION" -eq 0 ]; then
	"$KUBECTL" delete namespace "$NS" --wait=false >/dev/null 2>&1
	_ns_end=$(($(now) + TIMEOUT))
	while "$KUBECTL" get namespace "$NS" >/dev/null 2>&1; do
		if [ "$(now)" -ge "$_ns_end" ]; then
			LEFTOVERS="${LEFTOVERS}namespace/${NS}
"
			N_RESERVATION=$((N_RESERVATION + 1))
			break
		fi
		sleep "$POLL"
	done
	unset _ns_end
fi

if [ "$OUT_JSON" = yes ]; then
	report_json
else
	report_text_tail
fi
exit "$(exit_code_of)"
