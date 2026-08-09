#!/bin/sh
# CrystalBackup preflight — will this cluster actually back your volumes up?
# SPDX-License-Identifier: Apache-2.0
#
# ---------------------------------------------------------------------------------------------
# THIS SCRIPT IS STRICTLY READ-ONLY.
#
#   It creates nothing, changes nothing, installs nothing, and deletes nothing. It writes no
#   file anywhere — not even a temporary one. Every cluster call it makes is a `kubectl get`,
#   `kubectl api-resources`, `kubectl version`, or `kubectl auth can-i`.
#
#   One honest footnote, because "read-only" deserves to be precise: `kubectl auth can-i` POSTs
#   a SelfSubjectAccessReview. That is an HTTP POST, and it is how the Kubernetes API answers
#   "may I?" — it stores no object, changes no state, and leaves nothing behind. Pass
#   --no-rbac-probe to skip those calls entirely; the permission checks then report as UNKNOWN,
#   never as passing.
# ---------------------------------------------------------------------------------------------
#
# WHAT IT ANSWERS
#
#   Not "will `helm install` succeed" — helm will tell you that. The question this answers is
#   the one you would otherwise discover three weeks in: WHICH OF YOUR VOLUMES WILL ACTUALLY BE
#   BACKED UP, and which will be skipped. CrystalBackup backs up from a CSI snapshot; a
#   StorageClass whose driver has no VolumeSnapshotClass cannot be snapshotted, so its volumes
#   are reported Skipped with reason CSISnapshotUnsupported — visibly, but the backup still ends
#   Completed. If you never look, you never notice.
#
#   The per-StorageClass table this prints predicts DYNAMICALLY provisioned volumes, and says so
#   where it prints. For a volume its class provisioned, the class's provisioner IS the driver
#   serving it, so the class is the right unit to answer by. For a volume you bound by hand it is
#   not: CrystalBackup reads the driver from the PersistentVolume, because a class can be deleted
#   and re-created over a different backend while every old volume keeps pointing at the name, and
#   because a statically bound PVC's storageClassName is only a matching label — Kubernetes never
#   requires the object to exist. So this script reads your PersistentVolumes too, and names the
#   PVCs the table does not describe rather than letting the table stand for them.
#
# WHAT IT CANNOT ANSWER, AND WILL NOT PRETEND TO
#
#   Half of that question. A VolumeSnapshotClass whose .driver matches your StorageClass proves
#   snapshot AVAILABILITY: a snapshot can be REQUESTED of that driver, and will very likely be
#   taken. It does NOT prove snapshot USABILITY — that a volume restored from that snapshot can
#   be mounted on your nodes and read back. Those are two different questions, and a real
#   cluster has answered yes to the first and no to the second: snapshots taken cleanly, clones
#   provisioned cleanly, every mount refused by the node's kernel client with
#
#     rbd: map failed: (22) Invalid argument
#
#   Nothing in the Kubernetes API showed it. Backups hung for thirty-six hours.
#
#   Establishing usability means creating a PVC, writing to it, snapshotting it, restoring it and
#   mounting the result. This script will not do that: the promise at the top of this file is
#   worth more than the answer, and it is why you were willing to run this against production.
#
#   So every StorageClass that resolves to an exposer is reported here as USABILITY NOT ASSESSED.
#   It counts as a reservation, never as a pass — an unanswerable question does not become a
#   green one by being important. The companion below does create those objects, states exactly
#   which in its own header, and leaves them behind when they fail so the evidence survives:
#
#     https://crystalbackup.github.io/CrystalBackup/snapshot-probe.sh
#     https://crystalbackup.github.io/CrystalBackup/docs/operations/snapshot-probe/
#
# USAGE
#
#   ./preflight.sh [--json] [--no-rbac-probe] [--no-color] [--help] [--version]
#
#   --json            machine-readable output on stdout, nothing else on stdout
#   --no-rbac-probe   skip `kubectl auth can-i` (see the footnote above)
#   --no-color        never emit ANSI colour (also honoured: NO_COLOR=1)
#
#   Needs only `kubectl`, configured for the cluster you want to assess (KUBECONFIG / --context
#   are respected through kubectl itself; set them in the environment). `jq` is used if present
#   and is not required.
#
# EXIT CODES
#
#   0  READY               every check passed.
#   1  READY WITH RESERVES at least one check raised a reservation, or could NOT BE MADE.
#                          An unanswerable check lands here, never in 0 — this script does not
#                          report green on an absence.
#   2  BLOCKING            at least one check failed in a way that stops CrystalBackup working.
#   3  NOT ASSESSED        the script could not run at all (no kubectl, no reachable cluster,
#                          bad usage). Nothing was concluded about your cluster.
#
# VERIFYING THIS SCRIPT — do this before you run it
#
#   sha256sum -c preflight.sh.sha256          # or: shasum -a 256 -c preflight.sh.sha256
#   cosign verify-blob preflight.sh \
#     --bundle preflight.sh.cosign.bundle \
#     --certificate-identity-regexp '^https://github\.com/CrystalBackup/CrystalBackup/' \
#     --certificate-oidc-issuer https://token.actions.githubusercontent.com
#
# ---------------------------------------------------------------------------------------------

# `set -e` is deliberately NOT used. This script's whole job is to run commands that are allowed
# to fail — a missing CRD, a denied RBAC check, an unreachable subresource are all findings, not
# aborts. Every command's status is inspected where it is issued. `set -u` stays on: an unset
# variable here would be a bug in the script, not a fact about the cluster.
set -u

SCRIPT_VERSION='2.1.0'

# The companion that answers the half of the headline question this script deliberately cannot.
# Named here once, printed wherever the answer is missing.
CB_PROBE_URL='https://crystalbackup.github.io/CrystalBackup/snapshot-probe.sh'
CB_PROBE_DOCS='https://crystalbackup.github.io/CrystalBackup/docs/operations/snapshot-probe/'

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

# --- static facts about the release, kept next to the checks that use them -------------------
CB_MIN_K8S_MINOR=30
CB_MIN_K8S='1.30'
CB_SKIP_REASON='CSISnapshotUnsupported'

# --- output plumbing --------------------------------------------------------------------------

OUT_JSON=no
RBAC_PROBE=yes
USE_COLOR=auto

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
# comment. This used to be a fixed line range, which is a constant that has to be kept in step
# with the prose above it — and the prose above it is where this script states what it promises
# not to do. Piped in from a URL there is no file to read ($0 is the shell), so it falls back to
# the essentials rather than printing nothing at all.
usage() {
	if [ -r "$0" ] && head -n 1 "$0" 2>/dev/null | grep -q '^#!'; then
		awk 'NR == 1 { next } /^#/ { sub(/^# ?/, ""); print; next } { exit }' "$0"
		return
	fi
	cat <<'USAGE'
CrystalBackup preflight — read-only. Creates nothing, changes nothing, writes no file.
Reports, per StorageClass, which exposer would be chosen and which volumes would be skipped. That
table predicts DYNAMICALLY provisioned volumes. For a volume bound by hand the driver comes from
the PersistentVolume, not from the class it names, so the PersistentVolumes are read as well and
the PVCs the table does not describe are named individually.

It reports snapshot AVAILABILITY. It cannot report snapshot USABILITY — whether a volume
restored from a snapshot can be mounted and read on your nodes — because answering that
requires creating objects. Resolvable classes are reported "usability NOT ASSESSED", which is a
reservation and not a pass. The probe that does answer it:
  https://crystalbackup.github.io/CrystalBackup/snapshot-probe.sh

  preflight.sh [--json] [--no-rbac-probe] [--no-color] [--help] [--version]

Exit: 0 ready | 1 ready with reservations (includes checks that could NOT be made)
      2 blocking | 3 not assessed

Full header, checksum and cosign signature:
  https://crystalbackup.github.io/CrystalBackup/preflight.sh
USAGE
}

# --- result accumulation ----------------------------------------------------------------------
#
# POSIX sh has no arrays. Findings accumulate as records in newline-separated strings, fields
# joined by US (0x1f).
#
# US rather than tab, and this is not cosmetic: tab is IFS *whitespace*, so `IFS=<tab> read`
# collapses runs of tabs into one delimiter and an empty field silently shifts every field after
# it. A StorageClass with no VolumeSnapshotClass has exactly that — an empty "chosen class" field
# — so with a tab separator the one row this whole script exists to print is the one row that
# comes out scrambled. US is not IFS whitespace, so empty fields survive; and `clean` strips
# 0x00-0x1f from everything that comes in from the cluster, so the separator cannot appear in data.
US=$(printf '\037')
RESULTS=''
SC_ROWS=''
N_PASS=0
N_WARN=0
N_FAIL=0
N_UNKNOWN=0

# clean collapses a value to a single line of printable characters. Applied to every string that
# comes from the cluster or from a command's stderr before it is stored or printed.
clean() {
	printf '%s' "$1" | tr '\n\r\t' '   ' | tr -d '\000-\037' | sed -e 's/  */ /g' -e 's/^ //' -e 's/ $//'
}

# count_lines counts the non-empty lines of its argument. `grep -c` exits 1 on no match while
# still printing 0, which is exactly the shape that turns a naive `|| printf 0` fallback into the
# string "00"; this keeps that arithmetic honest.
count_lines() {
	printf '%s\n' "$1" | grep -c '.'
}

# record ID STATUS TITLE DETAIL
#   STATUS is one of PASS WARN FAIL UNKNOWN. UNKNOWN means "this could not be determined" and is
#   counted apart from PASS on purpose: it degrades the verdict, it never flatters it.
record() {
	_r_detail=$(clean "$4")
	RESULTS="${RESULTS}${1}${US}${2}${US}$(clean "$3")${US}${_r_detail}
"
	case $2 in
	PASS) N_PASS=$((N_PASS + 1)) ;;
	WARN) N_WARN=$((N_WARN + 1)) ;;
	FAIL) N_FAIL=$((N_FAIL + 1)) ;;
	UNKNOWN) N_UNKNOWN=$((N_UNKNOWN + 1)) ;;
	esac
	unset _r_detail
}

die_unassessed() {
	if [ "$OUT_JSON" = yes ]; then
		printf '{"schema":"crystalbackup.preflight/v2","scriptVersion":%s,"verdict":"NOT_ASSESSED","reason":%s,"usabilityAssessed":false,"checks":[],"storageClasses":[]}\n' \
			"$(json_str "$SCRIPT_VERSION")" "$(json_str "$1")"
	else
		printf '%sNOT ASSESSED%s: %s\n' "$C_FAIL" "$C_RESET" "$1" >&2
	fi
	exit 3
}

# --- JSON encoding ----------------------------------------------------------------------------

HAVE_JQ=no
JSON_ENCODER='builtin'

# json_str renders its argument as a JSON string, quotes included. jq is preferred because it
# encodes every code point exactly; without it the builtin path escapes backslash and quote and
# DROPS control characters, which is a real (if small) loss of fidelity — so the JSON document
# says which encoder produced it rather than leaving the reader to guess.
json_str() {
	if [ "$HAVE_JQ" = yes ]; then
		printf '%s' "$1" | jq -Rs .
	else
		printf '"%s"' "$(printf '%s' "$1" | tr -d '\000-\037' | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g')"
	fi
}

# --- kubectl wrappers -------------------------------------------------------------------------

KUBECTL=${KUBECTL:-kubectl}

# k_get runs `kubectl get` and captures stderr into K_ERR, so a caller can tell "absent" from
# "forbidden" — the distinction between a fact about the cluster and a fact about your token.
K_ERR=''
K_OUT=''
k_get() {
	K_ERR=$("$KUBECTL" get "$@" 2>&1 >/dev/null)
	K_OUT=$("$KUBECTL" get "$@" 2>/dev/null)
	if [ -n "$K_ERR" ]; then
		return 1
	fi
	return 0
}

# forbidden? — did the last k_get fail because of RBAC rather than absence?
is_forbidden() {
	case $K_ERR in
	*orbidden* | *"cannot list"* | *"cannot get"* | *Unauthorized*) return 0 ;;
	*) return 1 ;;
	esac
}

# --- checks -----------------------------------------------------------------------------------

check_kubectl() {
	if ! command -v "$KUBECTL" >/dev/null 2>&1; then
		die_unassessed "kubectl not found on PATH. Nothing about this cluster was checked."
	fi
	_kv=$("$KUBECTL" version --client -o json 2>/dev/null |
		sed -n 's/.*"gitVersion" *: *"\([^"]*\)".*/\1/p' | head -n 1)
	if [ -z "$_kv" ]; then
		_kv=$("$KUBECTL" version --client 2>/dev/null | sed -n 's/.*[Vv]ersion:* *\(v[0-9][^ ,]*\).*/\1/p' | head -n 1)
	fi
	if [ -z "$_kv" ]; then
		record kubectl UNKNOWN "kubectl client" \
			"kubectl is on PATH but would not report its version; checks below may misbehave on a very old client."
	else
		record kubectl PASS "kubectl client" "$_kv"
	fi
	unset _kv
}

check_server_version() {
	if ! _raw=$("$KUBECTL" get --raw /version 2>&1) || [ -z "$_raw" ]; then
		die_unassessed "cannot reach the cluster API: $(clean "$_raw")"
	fi
	_gv=$(printf '%s' "$_raw" | sed -n 's/.*"gitVersion" *: *"\([^"]*\)".*/\1/p' | head -n 1)
	_maj=$(printf '%s' "$_gv" | sed -n 's/^v\{0,1\}\([0-9][0-9]*\)\.\([0-9][0-9]*\).*/\1/p')
	_min=$(printf '%s' "$_gv" | sed -n 's/^v\{0,1\}\([0-9][0-9]*\)\.\([0-9][0-9]*\).*/\2/p')

	if [ -z "$_maj" ] || [ -z "$_min" ]; then
		record k8s-version UNKNOWN "Kubernetes version" \
			"the API reported a version string this script cannot parse (${_gv:-empty}); the ${CB_MIN_K8S}+ floor was NOT verified."
	elif [ "$_maj" -gt 1 ] 2>/dev/null || { [ "$_maj" -eq 1 ] && [ "$_min" -ge "$CB_MIN_K8S_MINOR" ]; }; then
		record k8s-version PASS "Kubernetes version" "$_gv (floor is $CB_MIN_K8S)"
	else
		record k8s-version FAIL "Kubernetes version" \
			"$_gv is below the hard floor $CB_MIN_K8S. The chart declares kubeVersion '>= 1.30.0-0' and will refuse to install."
	fi
	unset _raw _gv _maj _min
}

# ValidatingAdmissionPolicy is probed as an API, not inferred from the version number: a
# distribution can serve 1.30 with the feature gate off, and the version check alone would have
# reported that cluster ready for an admission model it cannot run.
check_vap() {
	if ! _r=$("$KUBECTL" get --raw /apis/admissionregistration.k8s.io/v1 2>&1); then
		record admission UNKNOWN "ValidatingAdmissionPolicy" \
			"could not read /apis/admissionregistration.k8s.io/v1 ($(clean "$_r")); admission support was NOT verified."
	elif printf '%s' "$_r" | grep -q '"name" *: *"validatingadmissionpolicies"'; then
		record admission PASS "ValidatingAdmissionPolicy" "served at admissionregistration.k8s.io/v1"
	else
		record admission FAIL "ValidatingAdmissionPolicy" \
			"admissionregistration.k8s.io/v1 is served but does NOT list validatingadmissionpolicies. The admission model cannot be installed; check the ValidatingAdmissionPolicy feature gate on the API server."
	fi
	unset _r
}

check_snapshot_crds() {
	_missing=''
	_wrongver=''
	_versions=''
	for _crd in volumesnapshots volumesnapshotcontents volumesnapshotclasses; do
		_full="${_crd}.${CB_SNAPSHOT_GROUP}"
		if k_get crd "$_full" -o jsonpath='{range .spec.versions[*]}{.name}{","}{end}'; then
			_versions="${_versions}${_crd}=${K_OUT%,} "
			# The operator addresses these objects at a pinned group/version. A CRD that exists but
			# does not serve that version is not a working snapshot API for CrystalBackup's purposes
			# — an easy thing to be wrong about on a cluster still carrying a v1beta1 snapshotter.
			case ",${K_OUT}" in
			*",${CB_SNAPSHOT_VERSION},"*) ;;
			*) _wrongver="${_wrongver}${_full}(${K_OUT%,}) " ;;
			esac
		else
			if is_forbidden; then
				record snapshot-crds UNKNOWN "external-snapshotter CRDs" \
					"you are not allowed to read CustomResourceDefinitions ($(clean "$K_ERR")); snapshot support was NOT verified. Every StorageClass verdict below is therefore provisional."
				unset _missing _wrongver _versions _crd _full
				return
			fi
			_missing="${_missing}${_full} "
		fi
	done

	if [ -n "$_missing" ]; then
		record snapshot-crds FAIL "external-snapshotter CRDs" \
			"missing: ${_missing}- without them nothing can be backed up at all. Install the external-snapshotter CRDs and the snapshot controller."
	elif [ -n "$_wrongver" ]; then
		record snapshot-crds FAIL "external-snapshotter CRDs" \
			"present but not serving ${CB_SNAPSHOT_GROUP}/${CB_SNAPSHOT_VERSION}: ${_wrongver}- the operator addresses snapshots at that exact version. Upgrade the external-snapshotter."
	else
		record snapshot-crds PASS "external-snapshotter CRDs" \
			"serving ${CB_SNAPSHOT_GROUP}/${CB_SNAPSHOT_VERSION}; $_versions"
	fi
	unset _missing _wrongver _versions _crd _full
}

# The CRDs being present proves objects can be CREATED, not that anything acts on them. Without
# a running snapshot controller every VolumeSnapshot sits at readyToUse=false forever and every
# backup hangs in exposure — a failure that looks like a CrystalBackup bug and is not one.
check_snapshot_controller() {
	if k_get deploy -A -o jsonpath='{range .items[*]}{.metadata.namespace}{"/"}{.metadata.name}{" "}{.status.readyReplicas}{"\n"}{end}'; then
		_hits=$(printf '%s' "$K_OUT" | grep -i 'snapshot-controller' | head -n 3)
		if [ -z "$_hits" ]; then
			record snapshot-controller WARN "snapshot controller" \
				"no Deployment matching 'snapshot-controller' found in any namespace. If it runs some other way (DaemonSet, static pod, a differently named Deployment) this is a false alarm — but if it does not run, VolumeSnapshots never become ready and every backup stalls."
		else
			record snapshot-controller PASS "snapshot controller" "$(clean "$_hits")"
		fi
		unset _hits
	else
		record snapshot-controller UNKNOWN "snapshot controller" \
			"could not list Deployments ($(clean "$K_ERR")); whether a snapshot controller is running was NOT verified."
	fi
}

# VSCLASSES holds 'driver<TAB>name' lines; the StorageClass check joins against it.
VSCLASSES=''
VSCLASSES_KNOWN=no
check_snapshot_classes() {
	if k_get volumesnapshotclass -o jsonpath='{range .items[*]}{.driver}{"	"}{.metadata.name}{"	"}{.deletionPolicy}{"\n"}{end}'; then
		VSCLASSES=$K_OUT
		VSCLASSES_KNOWN=yes
		_n=$(count_lines "$VSCLASSES")
		if [ "$_n" -eq 0 ]; then
			record snapshot-classes FAIL "VolumeSnapshotClasses" \
				"none exist. With no VolumeSnapshotClass, EVERY volume in this cluster is skipped with reason $CB_SKIP_REASON and CrystalBackup will capture manifests only."
		else
			record snapshot-classes PASS "VolumeSnapshotClasses" \
				"$_n found: $(printf '%s\n' "$VSCLASSES" | while IFS='	' read -r d n p; do printf '%s(%s,%s) ' "$n" "$d" "$p"; done)"
		fi
		unset _n
	else
		record snapshot-classes UNKNOWN "VolumeSnapshotClasses" \
			"could not list them ($(clean "$K_ERR")); no StorageClass verdict below can be trusted."
	fi
}

# --- the volume inventory the checks below share -------------------------------------------------
#
# PVCs and PersistentVolumes are listed once, here, because two checks read them: the StorageClass
# table reports a skipped class with the damage it actually does, and the check after it works out
# which of your PVCs that table describes at all.
#
# Failing to list either is a finding, not an abort — the same degradation the checks above use for
# VolumeSnapshotClasses. Without the PVCs the StorageClass verdicts still hold, they just lose their
# weight; without the PersistentVolumes the check below reports UNKNOWN rather than assuming your
# volumes are served by the drivers their classes name.
#
#   PVC_ROWS  storageClassName / namespace / name / volumeName
#   PV_ROWS   name / .spec.csi.driver / pv.kubernetes.io/migrated-to / then one field per non-CSI
#             source, present only when that is what the volume is: nfs / hostPath / local / iscsi /
#             fc. A row with no driver, no migrated-to and none of those is some other non-CSI
#             source, which is the only claim that has to be right about it.
#
# Both are US-separated, for the reason set out where US is defined: spec.volumeName is EMPTY on an
# unbound PVC and spec.csi.driver is EMPTY on a hand-made NFS volume, and those are precisely the
# rows that must not scramble. kubectl's jsonpath can emit a tab but not a US, so the tabs are
# translated on arrival — no name of a Kubernetes object can contain either.
PVC_KNOWN=no
PVC_ROWS=''
PV_KNOWN=no
PV_ROWS=''
PV_ERR=''
collect_volume_inventory() {
	if k_get pvc -A -o jsonpath='{range .items[*]}{.spec.storageClassName}{"	"}{.metadata.namespace}{"	"}{.metadata.name}{"	"}{.spec.volumeName}{"\n"}{end}'; then
		PVC_ROWS=$(printf '%s\n' "$K_OUT" | tr '\t' "$US")
		PVC_KNOWN=yes
	fi
	if k_get pv -o jsonpath='{range .items[*]}{.metadata.name}{"	"}{.spec.csi.driver}{"	"}{.metadata.annotations.pv\.kubernetes\.io/migrated-to}{"	"}{.spec.nfs.server}{"	"}{.spec.hostPath.path}{"	"}{.spec.local.path}{"	"}{.spec.iscsi.iqn}{"	"}{.spec.fc.targetWWNs}{.spec.fc.wwids}{"\n"}{end}'; then
		PV_ROWS=$(printf '%s\n' "$K_OUT" | tr '\t' "$US")
		PV_KNOWN=yes
	else
		PV_ERR=$K_ERR
	fi
}

# --- the core: what happens to each StorageClass ------------------------------------------------

# SC_KNOWN distinguishes "this cluster defines no StorageClass" from "the StorageClasses could not
# be read". The check after this one needs that apart: with no class in the cluster, a bound PVC
# naming one is a class that genuinely does not exist; with the listing refused, the same PVC is a
# question that was not answered, and the two must not print the same sentence.
SC_KNOWN=no
check_storage_classes() {
	# The migrated-to annotation is read alongside the provisioner because on a CSI-migrated in-tree
	# class the provisioner names a plugin that no longer serves anything — see cb_sc_driver in the
	# generated region above, which owns that choice.
	if ! k_get storageclass -o jsonpath='{range .items[*]}{.metadata.name}{"	"}{.provisioner}{"	"}{.metadata.annotations.pv\.kubernetes\.io/migrated-to}{"	"}{.metadata.annotations.storageclass\.kubernetes\.io/is-default-class}{"\n"}{end}'; then
		record storage-classes UNKNOWN "StorageClass coverage" \
			"could not list StorageClasses ($(clean "$K_ERR")). THE MAIN QUESTION THIS SCRIPT EXISTS TO ANSWER WAS NOT ANSWERED."
		return
	fi
	SC_KNOWN=yes
	# US, not tab, from here on: the migrated-to annotation is absent on almost every class, and an
	# empty middle field is exactly what `IFS=<tab> read` collapses.
	_scs=$(printf '%s\n' "$K_OUT" | tr '\t' "$US")
	if ! printf '%s' "$_scs" | grep -q '.'; then
		record storage-classes WARN "StorageClass coverage" "this cluster defines no StorageClass at all."
		return
	fi

	_skipped_classes=0
	_skipped_pvcs=0
	_resolved_classes=0
	_resolved_pvcs=0
	_no_default=yes

	while IFS="$US" read -r _name _prov _migrated _default; do
		[ -n "$_name" ] || continue
		[ "$_default" = "true" ] && _no_default=no

		# The driver this class's volumes are actually served by. Usually the provisioner; on a
		# CSI-migrated in-tree class the annotation instead, because the provisioner then names a
		# plugin that no longer serves anything and no VolumeSnapshotClass will ever carry its name.
		# The derivation is generated, not written here — see cb_sc_driver.
		_drv=$(cb_sc_driver "$_prov" "$_migrated")

		# Candidate VolumeSnapshotClasses for that driver, resolved exactly as the operator resolves
		# them: match on .driver, then the generated tie-break picks the winner.
		_cands=''
		_ncand=0
		if [ "$VSCLASSES_KNOWN" = yes ]; then
			_cands=$(printf '%s\n' "$VSCLASSES" | awk -F'\t' -v p="$_drv" '$1==p {print $2}')
			_ncand=$(count_lines "$_cands")
		fi

		_has=no
		[ "$_ncand" -gt 0 ] && _has=yes

		if [ "$VSCLASSES_KNOWN" = no ]; then
			_verdict='unknown'
			_chosen=''
		else
			_verdict=$(cb_exposer_for "$_drv" "$_has")
			_chosen=$(printf '%s\n' "$_cands" | grep '.' | cb_pick_vsclass)
		fi

		_pvcn=0
		_pvcns=0
		if [ "$PVC_KNOWN" = yes ]; then
			_pvcn=$(count_lines "$(printf '%s\n' "$PVC_ROWS" | awk -F"$US" -v sc="$_name" '$1==sc')")
			_pvcns=$(count_lines "$(printf '%s\n' "$PVC_ROWS" | awk -F"$US" -v sc="$_name" '$1==sc {print $2}' | LC_ALL=C sort -u)")
		fi

		# _usability is deliberately never 'ASSESSED'. There is no branch of this script that can
		# produce that value, because there is no read-only call that establishes it. A column
		# whose best case is "NOT ASSESSED" reads as an omission until you notice it is the same
		# in every row — which is exactly the shape of the truth here.
		case $_verdict in
		skip)
			_skipped_classes=$((_skipped_classes + 1))
			_skipped_pvcs=$((_skipped_pvcs + _pvcn))
			_usability='NOT_APPLICABLE'
			_note="no VolumeSnapshotClass has driver '$_drv' — volumes on this class are SKIPPED, reason $CB_SKIP_REASON"
			;;
		"$CB_KIND_CEPHFS_SHALLOW")
			_resolved_classes=$((_resolved_classes + 1))
			_resolved_pvcs=$((_resolved_pvcs + _pvcn))
			_usability='NOT_ASSESSED'
			_note="exposer $_verdict: CephFS shallow (backingSnapshot) ReadOnlyMany temp PVC, zero copy. Snapshot class RESOLVED; whether the restored ReadOnlyMany volume can be mounted and read on your nodes was NOT ASSESSED — see $CB_PROBE_URL"
			;;
		"$CB_KIND_CSI_GENERIC")
			_resolved_classes=$((_resolved_classes + 1))
			_resolved_pvcs=$((_resolved_pvcs + _pvcn))
			_usability='NOT_ASSESSED'
			_note="exposer $_verdict: VolumeSnapshot + ReadWriteOnce temp PVC. Copy-on-write on RBD and most drivers; a driver whose create-from-snapshot is a FULL copy will pay that copy per backup. Snapshot class RESOLVED; whether the restored volume can be mounted and read on your nodes was NOT ASSESSED — see $CB_PROBE_URL"
			;;
		*)
			_usability='UNDETERMINED'
			_note="undetermined — VolumeSnapshotClasses could not be listed"
			;;
		esac
		if [ "$_ncand" -gt 1 ]; then
			_note="$_note; $_ncand classes match this driver, the operator would use '$_chosen'"
		fi
		# When the two differ, the row must say which string its verdict was resolved for. The
		# PROVISIONER column prints what the class DECLARES — that is what `kubectl get sc` shows and
		# what an administrator will look for — so the driver the verdict actually used is named here,
		# in the row, rather than being quietly substituted into a column labelled something else.
		if [ "$_drv" != "$_prov" ]; then
			_note="this StorageClass is CSI-MIGRATED: its declared provisioner '$_prov' is an in-tree plugin that no longer serves its volumes, and its $CB_MIGRATED_TO_ANNOTATION annotation names '$_drv', which is the driver the verdict above was resolved for. $_note"
		fi

		SC_ROWS="${SC_ROWS}${_name}${US}${_prov}${US}${_drv}${US}${_default:-false}${US}${_chosen}${US}${_verdict}${US}${_usability}${US}${_pvcn}${US}${_pvcns}${US}$(clean "$_note")
"
	done <<EOF
$_scs
EOF

	if [ "$VSCLASSES_KNOWN" = no ]; then
		record storage-classes UNKNOWN "StorageClass coverage" \
			"VolumeSnapshotClasses could not be listed, so no exposer could be resolved for any StorageClass."
	elif [ "$_skipped_classes" -eq 0 ]; then
		record storage-classes PASS "StorageClass coverage" \
			"every StorageClass resolves to a VolumeSnapshotClass and an exposer. That is snapshot AVAILABILITY only; usability is a separate finding below and was not established here."
	elif [ "$PVC_KNOWN" = yes ] && [ "$_skipped_pvcs" -gt 0 ]; then
		record storage-classes WARN "StorageClass coverage" \
			"$_skipped_classes StorageClass(es) cannot be snapshotted, and $_skipped_pvcs existing PVC(s) sit on them. Those volumes' DATA WILL NEVER BE BACKED UP — their manifests will, and the Backup will still report Completed."
	else
		record storage-classes WARN "StorageClass coverage" \
			"$_skipped_classes StorageClass(es) cannot be snapshotted. No PVC uses them today; any PVC created on them later is silently data-skipped."
	fi

	# The headline question — WHICH OF YOUR VOLUMES WILL ACTUALLY BE BACKED UP — has a half this
	# script cannot reach, and this is where it says so instead of letting the row above stand for
	# an answer. UNKNOWN, not PASS and not WARN: nothing is wrong, and nothing is established.
	# There is no code path that turns this into a PASS, by construction.
	if [ "$_resolved_classes" -gt 0 ]; then
		_usab_detail="$_resolved_classes StorageClass(es) resolve to a VolumeSnapshotClass"
		if [ "$PVC_KNOWN" = yes ]; then
			_usab_detail="$_usab_detail, carrying $_resolved_pvcs existing PVC(s)"
		fi
		record storage-usability UNKNOWN "Snapshot USABILITY (not assessed here)" \
			"$_usab_detail. That proves a snapshot can be REQUESTED. It does not prove the volume restored from it can be MOUNTED on your nodes and READ — a cluster has passed the first and failed the second, with every mount refused by the node ('rbd: map failed: (22) Invalid argument') while the Kubernetes API showed nothing wrong. Establishing it requires creating a PVC, a snapshot, a restored PVC and a pod, which this script does not do. Run the probe, which does and says so: $CB_PROBE_URL (how to read it: $CB_PROBE_DOCS)"
		unset _usab_detail
	fi

	if [ "$_no_default" = yes ]; then
		record default-sc WARN "Default StorageClass" \
			"no StorageClass is marked default. Restores that do not name a StorageClass explicitly will fail to provision."
	else
		record default-sc PASS "Default StorageClass" "set"
	fi
	unset _scs _skipped_classes _skipped_pvcs _resolved_classes _resolved_pvcs _no_default _name _prov _migrated _drv _default _cands _ncand _has _verdict _usability _chosen _pvcn _pvcns _note
}

# --- which volumes the table above is actually about ----------------------------------------------
#
# The table is per StorageClass, and for a volume that is already bound the StorageClass is not what
# decides. CrystalBackup resolves the driver from the PersistentVolume the claim names, and consults
# the class only for a PVC bound to nothing yet. Two reasons, and a real cluster has each:
#
#   A StorageClass's provisioner cannot be changed — but the class itself is not permanent. Delete
#   it, create one of the same name over a different backend, and every PersistentVolume made under
#   the old one still carries spec.storageClassName pointing at that name: a dangling string that
#   now resolves to the WRONG driver. Predicting through it does not produce an error, it produces a
#   confident wrong answer.
#
#   A statically bound volume makes the same point from the other side. There storageClassName is
#   only a matching LABEL between the claim and the volume, and Kubernetes never requires the
#   StorageClass OBJECT to exist. A PVC bound to a hand-made NFS volume whose class was never created
#   is legal, ordinary, and nothing to fix.
#
# So this check reads the PersistentVolumes and names the PVCs the table cannot speak for. It is the
# one place in this script that can say "that row is not about your volume".
PV_CLASSIFIED=''

# real_verdict_phrase DRIVER — what CrystalBackup would do with a volume served by DRIVER, in the
# same words the table uses, resolved through the same generated rule. The driver comes from the
# PersistentVolume here rather than from a class, which is the entire point: cb_exposer_for takes a
# DRIVER, so one rule answers for both paths and there is no second copy to drift.
real_verdict_phrase() {
	if [ "$VSCLASSES_KNOWN" = no ]; then
		printf 'verdict undetermined, the VolumeSnapshotClasses could not be listed'
		return
	fi
	_rvp_has=no
	if printf '%s\n' "$VSCLASSES" | awk -F'\t' -v d="$1" '$1==d {f=1} END {exit !f}'; then
		_rvp_has=yes
	fi
	_rvp=$(cb_exposer_for "$1" "$_rvp_has")
	case $_rvp in
	skip) printf 'DATA SKIPPED, reason %s: no VolumeSnapshotClass has driver %s' "$CB_SKIP_REASON" "$1" ;;
	*) printf 'exposer %s, usability NOT ASSESSED' "$_rvp" ;;
	esac
	unset _rvp _rvp_has
}

# pv_bucket KIND — the classified rows of one kind.
pv_bucket() {
	printf '%s\n' "$PV_CLASSIFIED" | awk -F"$US" -v k="$1" '$1==k'
}

# PV_NO_CLASS stands in, inside the classified rows, for a PVC that names no StorageClass at all —
# legal, and a different thing from naming one that does not exist. It is a value no StorageClass
# name can take, so it cannot collide with a real one.
PV_NO_CLASS='(none)'

# pv_examples KIND LIMIT — a readable list of one bucket's PVCs, bounded at LIMIT. Bounded because a
# cluster can hold hundreds of them, and a detail line nobody finishes is a detail nobody reads; the
# count that precedes it is always the true one.
pv_examples() {
	_pe_n=0
	_pe_out=''
	while IFS="$US" read -r _pe_kind _pe_pvc _pe_class _pe_pv _pe_drv _pe_extra; do
		[ -n "$_pe_kind" ] || continue
		_pe_n=$((_pe_n + 1))
		[ "$_pe_n" -le "$2" ] || continue
		if [ "$_pe_class" = "$PV_NO_CLASS" ]; then
			_pe_cls='naming no StorageClass'
		else
			_pe_cls="naming class '$_pe_class'"
		fi
		case $_pe_kind in
		mismatch)
			_pe_item="$_pe_pvc (its class '$_pe_class' resolves to $_pe_extra, but its volume $_pe_pv is served by $_pe_drv — $(real_verdict_phrase "$_pe_drv"))"
			;;
		noncsi)
			_pe_item="$_pe_pvc, $_pe_cls (volume $_pe_pv, source: $_pe_extra)"
			;;
		noclass)
			_pe_item="$_pe_pvc, $_pe_cls (volume $_pe_pv is served by $_pe_drv — $(real_verdict_phrase "$_pe_drv"))"
			;;
		nocmp)
			_pe_item="$_pe_pvc (volume $_pe_pv is served by $_pe_drv — $(real_verdict_phrase "$_pe_drv"))"
			;;
		*)
			_pe_item="$_pe_pvc (volume $_pe_pv)"
			;;
		esac
		_pe_out="${_pe_out}${_pe_item}; "
	done <<EOF
$(pv_bucket "$1")
EOF
	if [ "$_pe_n" -gt "$2" ]; then
		_pe_out="${_pe_out}and $((_pe_n - $2)) more; "
	fi
	# The list ends the sentence it is part of, rather than running the next one onto its last
	# semicolon.
	printf '%s' "${_pe_out%"; "}. "
	unset _pe_n _pe_out _pe_kind _pe_pvc _pe_class _pe_pv _pe_drv _pe_extra _pe_item _pe_cls
}

check_bound_volume_drivers() {
	_title='Bound volumes — the PersistentVolume decides the driver'
	if [ "$PVC_KNOWN" = no ]; then
		record bound-volumes UNKNOWN "$_title" \
			"PersistentVolumeClaims could not be listed, so which of your volumes the StorageClass table describes — and which it does not — was NOT established."
		unset _title
		return
	fi
	if [ "$PV_KNOWN" = no ]; then
		record bound-volumes UNKNOWN "$_title" \
			"could not list PersistentVolumes ($(clean "$PV_ERR")). CrystalBackup takes a bound volume's driver from its PersistentVolume, so without them the StorageClass table below is a prediction for DYNAMICALLY provisioned volumes only and nothing here can tell you which of your volumes that is. Nothing is claimed either way."
		unset _title
		return
	fi

	# One awk pass rather than a lookup per PVC: a cluster with a few hundred claims would otherwise
	# fork awk a few hundred times. The three inventories are tagged with a leading field and
	# concatenated; awk indexes the first two and classifies the third.
	PV_CLASSIFIED=$(
		{
			printf '%s\n' "$SC_ROWS" | sed "s/^/S${US}/"
			printf '%s\n' "$PV_ROWS" | sed "s/^/V${US}/"
			printf '%s\n' "$PVC_ROWS" | sed "s/^/C${US}/"
		} | awk -F"$US" -v SEP="$US" -v NOCLASS="$PV_NO_CLASS" \
			-v cmpok="$([ "$SC_KNOWN" = yes ] && printf 1 || printf 0)" '
			# Field 4 of an SC row, not field 3: the comparison below must be against the EFFECTIVE
			# driver of the class. Comparing a bound PV against the declared provisioner of a
			# CSI-migrated class would report every one of its volumes as a mismatch — the PV carries
			# the migrated driver, the class declares the retired plugin, and they agree in every way
			# that matters. (No apostrophes in here: this awk program is a single-quoted shell word.)
			$1 == "S" && $2 != "" { drvOf[$2] = $4; scknown[$2] = 1; next }
			$1 == "V" && $2 != "" {
				pvknown[$2] = 1
				drv[$2] = $3
				mig[$2] = $4
				if ($5 != "") { src[$2] = "nfs" }
				else if ($6 != "") { src[$2] = "hostPath" }
				else if ($7 != "") { src[$2] = "local" }
				else if ($8 != "") { src[$2] = "iscsi" }
				else if ($9 != "") { src[$2] = "fc" }
				else { src[$2] = "non-CSI" }
				next
			}
			$1 == "C" && $4 != "" {
				if ($5 == "") { next }   # not bound to anything: the class IS what the operator reads
				pvc = $3 "/" $4
				pv = $5
				class = ($2 == "" ? NOCLASS : $2)
				if (!(pv in pvknown)) { print "pvgone" SEP pvc SEP class SEP pv SEP "" SEP ""; next }
				d = (drv[pv] != "" ? drv[pv] : mig[pv])
				if (d == "") { print "noncsi" SEP pvc SEP class SEP pv SEP "" SEP src[pv]; next }
				if (cmpok != 1) { print "nocmp" SEP pvc SEP class SEP pv SEP d SEP ""; next }
				if ($2 == "" || !($2 in scknown)) { print "noclass" SEP pvc SEP class SEP pv SEP d SEP ""; next }
				if (drvOf[$2] != d) { print "mismatch" SEP pvc SEP class SEP pv SEP d SEP drvOf[$2]; next }
				print "agree" SEP pvc SEP class SEP pv SEP d SEP ""
			}
		'
	)

	_nb=$(count_lines "$PV_CLASSIFIED")
	_n_agree=$(count_lines "$(pv_bucket agree)")
	_n_mismatch=$(count_lines "$(pv_bucket mismatch)")
	_n_noncsi=$(count_lines "$(pv_bucket noncsi)")
	_n_noclass=$(count_lines "$(pv_bucket noclass)")
	_n_nocmp=$(count_lines "$(pv_bucket nocmp)")
	_n_pvgone=$(count_lines "$(pv_bucket pvgone)")

	_level=PASS
	_detail=''

	# A driver that is not the one its class names. WARN, because the row the reader would have
	# trusted is about a different backend — but not FAIL: the volume itself is very likely fine,
	# and its real verdict is printed here.
	if [ "$_n_mismatch" -gt 0 ]; then
		_level=WARN
		_detail="${_detail}$_n_mismatch bound PVC(s) sit on a PersistentVolume whose CSI driver is NOT the provisioner of the StorageClass they name: $(pv_examples mismatch 4)THE TABLE BELOW DOES NOT DESCRIBE THESE VOLUMES — read the verdict quoted here instead, which is the one CrystalBackup will use. A StorageClass deleted and re-created over a different backend leaves exactly this shape: the provisioner of a class can never change, but the class can be replaced, and volumes made under the old one keep naming it. "
	fi

	# No CSI driver at all. WARN, because 'the data is never backed up' is a reservation whatever
	# its cause — and worded so nobody goes looking for the setting that fixes it, because there
	# is none.
	if [ "$_n_noncsi" -gt 0 ]; then
		_level=WARN
		_detail="${_detail}$_n_noncsi bound PVC(s) sit on a PersistentVolume that is not a CSI volume at all: $(pv_examples noncsi 4)Their DATA WILL BE SKIPPED, reason $CB_SKIP_REASON — the manifests are still captured and the Backup still reports Completed. This is NOT a misconfiguration and there is no setting that changes it: a volume with no CSI driver has nothing that can be asked for a snapshot. Expected for a hand-made NFS, hostPath, local, iSCSI or FC volume; back those up by whatever means the storage behind them provides. "
	fi

	# A named class that does not exist. This is what a static binding looks like, so it is reported
	# as a fact and NEVER raises the level: the volume's driver was read from the PersistentVolume,
	# which is exactly what the operator does, and everything about it works.
	if [ "$_n_noclass" -gt 0 ]; then
		_detail="${_detail}$_n_noclass bound PVC(s) name a StorageClass that does not exist — or name none at all — and are served by a CSI driver read from their PersistentVolume: $(pv_examples noclass 4)This is normal, legal and nothing to correct: for a statically provisioned volume storageClassName is only a matching label between the claim and the volume, and Kubernetes never requires the object to exist. It is reported only because the table below has no row for these volumes; their verdict is the one quoted here. "
	fi

	if [ "$_n_nocmp" -gt 0 ]; then
		[ "$_level" = PASS ] && _level=UNKNOWN
		_detail="${_detail}$_n_nocmp bound PVC(s) are served by a CSI driver read from their PersistentVolume, but the StorageClasses could not be listed, so whether that driver agrees with the class each PVC names was NOT checked: $(pv_examples nocmp 4)"
	fi

	if [ "$_n_pvgone" -gt 0 ]; then
		[ "$_level" = PASS ] && _level=UNKNOWN
		_detail="${_detail}$_n_pvgone bound PVC(s) name a PersistentVolume that was not in the listing: $(pv_examples pvgone 4)Their driver was NOT established. The likeliest cause is a volume deleted between the two reads, in which case running this again settles it."
	fi

	if [ -z "$_detail" ]; then
		if [ "$_nb" -eq 0 ]; then
			_detail="no PVC in this cluster is bound to a PersistentVolume yet, so there is nothing the table below can misdescribe. It predicts what would happen to volumes provisioned on each class from now on."
		else
			_detail="all $_nb bound PVC(s) sit on a PersistentVolume whose CSI driver is exactly the provisioner of the StorageClass they name, so every row of the table below applies to its volumes as printed."
		fi
	elif [ "$SC_KNOWN" = yes ]; then
		# Only worth counting when there is a table to be counted against; with the StorageClasses
		# unreadable there is no row for any volume and "0 of N described" would be a statistic
		# about nothing.
		_detail="$_n_agree of $_nb bound PVC(s) are described by the table below as printed. $_detail"
	fi

	record bound-volumes "$_level" "$_title" "$_detail"
	unset _title _nb _n_agree _n_mismatch _n_noncsi _n_noclass _n_nocmp _n_pvgone _level _detail
}

# A volumeMode: Block PVC is the one case the StorageClass table above reports as covered and is
# not, so it gets its own check rather than a footnote. Block volumes are BACKED UP normally — the
# exposer treats them identically — but a RESTORE into a Block PVC is refused (restic restores
# files into a filesystem, not a device), failing that volume with reason RestoreBlockUnsupported.
# A backup that cannot be restored is worse than a missing one, because it is the one you were
# counting on.
check_block_volumes() {
	if ! k_get pvc -A -o jsonpath='{range .items[*]}{.spec.volumeMode}{"	"}{.metadata.namespace}{"/"}{.metadata.name}{"\n"}{end}'; then
		record block-volumes UNKNOWN "Block-mode volumes" \
			"could not list PVCs ($(clean "$K_ERR")); whether any volumeMode: Block PVC exists was NOT established."
		return
	fi
	_blocks=$(printf '%s\n' "$K_OUT" | awk -F'\t' '$1=="Block" {print $2}')
	_nb=$(count_lines "$_blocks")
	if [ "$_nb" -eq 0 ]; then
		record block-volumes PASS "Block-mode volumes" "none; every PVC is filesystem-mode."
	else
		record block-volumes WARN "Block-mode volumes" \
			"$_nb PVC(s) use volumeMode: Block ($(clean "$(printf '%s' "$_blocks" | tr '\n' ' ')")). They are BACKED UP normally, but RESTORE into a Block PVC is NOT supported — those volumes fail a restore with reason RestoreBlockUnsupported. Plan another recovery path for them."
	fi
	unset _blocks _nb
}

# --- permissions --------------------------------------------------------------------------------

check_rbac() {
	if [ "$RBAC_PROBE" = no ]; then
		record rbac UNKNOWN "Install permissions" \
			"--no-rbac-probe was given; your permissions were NOT checked. The install may still fail half-way."
		return
	fi

	_denied=''
	_unknown=''
	# resource:verb:scope triples the chart needs at install time. A half-permitted install leaves
	# CRDs without the RBAC that makes them useful, which is worse than a clean refusal.
	#
	# stderr is discarded rather than merged: kubectl prints "Warning: resource X is not namespace
	# scoped" for every cluster-scoped resource, and folding that into the answer turns every
	# cluster-scoped probe into an unparseable reply — reported as UNKNOWN, which is safe but
	# useless. The verdict is read from stdout ("yes"/"no") alone.
	for _triple in \
		'customresourcedefinitions.apiextensions.k8s.io:create:cluster' \
		'clusterroles.rbac.authorization.k8s.io:create:cluster' \
		'clusterrolebindings.rbac.authorization.k8s.io:create:cluster' \
		'validatingadmissionpolicies.admissionregistration.k8s.io:create:cluster' \
		'namespaces:create:cluster' \
		'deployments.apps:create:all-ns' \
		'secrets:create:all-ns' \
		'serviceaccounts:create:all-ns'; do
		_res=${_triple%%:*}
		_rest=${_triple#*:}
		_verb=${_rest%%:*}
		_scope=${_rest#*:}
		if [ "$_scope" = all-ns ]; then
			_ans=$("$KUBECTL" auth can-i "$_verb" "$_res" --all-namespaces 2>/dev/null)
		else
			_ans=$("$KUBECTL" auth can-i "$_verb" "$_res" 2>/dev/null)
		fi
		case $_ans in
		yes) : ;;
		no) _denied="${_denied}${_verb} ${_res}; " ;;
		*) _unknown="${_unknown}${_res}; " ;;
		esac
	done

	if [ -n "$_unknown" ]; then
		record rbac UNKNOWN "Install permissions" \
			"the API did not give a usable answer for: ${_unknown}(SelfSubjectAccessReview unavailable, or an authorizer that cannot answer in advance). Your permissions were NOT established."
	elif [ -n "$_denied" ]; then
		record rbac FAIL "Install permissions" \
			"you may NOT: ${_denied}the chart creates CRDs, cluster-scoped RBAC, admission policies and a namespace, so the install would stop part-way. Install as cluster-admin."
	else
		record rbac PASS "Install permissions" "may create CRDs, cluster RBAC, admission policies, namespace, workloads and secrets."
	fi
	unset _denied _unknown _triple _rest _scope _res _verb _ans
}

# --- optional integrations ------------------------------------------------------------------------

# cert-manager is checked and reported, but it is NOT a requirement and never affects the verdict:
# the chart issues the webhook's serving certificate itself (genCA/genSignedCert), deliberately, to
# stay free of a cert-manager dependency. Reporting its absence as a reservation would be a false
# alarm, and a script that cries wolf about an optional component teaches people to ignore it.
check_cert_manager() {
	if k_get crd certificates.cert-manager.io -o jsonpath='{.metadata.name}'; then
		record cert-manager PASS "cert-manager (not required)" \
			"present. CrystalBackup does not use it: the chart generates the webhook's serving certificate itself, and re-issues it on upgrade."
	else
		if is_forbidden; then
			record cert-manager UNKNOWN "cert-manager (not required)" \
				"could not check ($(clean "$K_ERR")). It is not needed either way — the chart self-signs the webhook certificate."
		else
			record cert-manager PASS "cert-manager (not required)" \
				"absent, and not needed: the chart generates the webhook's serving certificate itself."
		fi
	fi
}

check_prometheus_operator() {
	_have_sm=no
	_have_pr=no
	_unk=no
	if k_get crd servicemonitors.monitoring.coreos.com -o jsonpath='{.metadata.name}'; then _have_sm=yes; elif is_forbidden; then _unk=yes; fi
	if k_get crd prometheusrules.monitoring.coreos.com -o jsonpath='{.metadata.name}'; then _have_pr=yes; elif is_forbidden; then _unk=yes; fi

	if [ "$_unk" = yes ]; then
		record prometheus UNKNOWN "Prometheus operator CRDs (optional)" \
			"could not read CustomResourceDefinitions; whether ServiceMonitor/PrometheusRule can be created was NOT established."
	elif [ "$_have_sm" = yes ] && [ "$_have_pr" = yes ]; then
		record prometheus PASS "Prometheus operator CRDs (optional)" \
			"ServiceMonitor and PrometheusRule available; serviceMonitor.enabled and prometheusRule.enabled can be turned on."
	elif [ "$_have_sm" = no ] && [ "$_have_pr" = no ]; then
		record prometheus PASS "Prometheus operator CRDs (optional)" \
			"absent. Both are off by default, so this blocks nothing — but leave serviceMonitor.enabled and prometheusRule.enabled off, or the install will fail on unknown kinds. Metrics are still served on the operator's /metrics endpoint."
	else
		record prometheus WARN "Prometheus operator CRDs (optional)" \
			"only one of ServiceMonitor / PrometheusRule exists (servicemonitors=$_have_sm prometheusrules=$_have_pr). Enable only the matching chart value."
	fi
	unset _have_sm _have_pr _unk
}

# --- cluster shape ---------------------------------------------------------------------------------

check_nodes() {
	if ! k_get nodes -o jsonpath='{range .items[*]}{.status.nodeInfo.architecture}{"	"}{.status.allocatable.cpu}{"	"}{.status.allocatable.memory}{"\n"}{end}'; then
		record nodes UNKNOWN "Cluster resources" \
			"could not list nodes ($(clean "$K_ERR")); cluster size and node architecture were NOT established."
		return
	fi
	_n=$(count_lines "$K_OUT")
	_arches=$(printf '%s\n' "$K_OUT" | awk -F'\t' 'NF{print $1}' | LC_ALL=C sort -u | tr '\n' ' ')
	# Allocatable CPU is either an integer of cores or a milli-quantity; memory is Ki/Mi/Gi.
	_cpu=$(printf '%s\n' "$K_OUT" | awk -F'\t' '
		NF { v=$2; if (v ~ /m$/) { sub(/m$/,"",v); s+=v/1000 } else { s+=v } }
		END { printf "%.1f", s }')
	_mem=$(printf '%s\n' "$K_OUT" | awk -F'\t' '
		NF { v=$3; u=1
		     if (v ~ /Ki$/) { sub(/Ki$/,"",v); u=1024 }
		     else if (v ~ /Mi$/) { sub(/Mi$/,"",v); u=1048576 }
		     else if (v ~ /Gi$/) { sub(/Gi$/,"",v); u=1073741824 }
		     s += v*u }
		END { printf "%.1f", s/1073741824 }')

	# The published images are a multi-arch index covering linux/amd64 and linux/arm64 only. A node
	# on any other architecture cannot pull them, and the failure surfaces as an unschedulable
	# mover long after install.
	_bad=''
	for _a in $_arches; do
		case $_a in
		amd64 | arm64) ;;
		*) _bad="${_bad}${_a} " ;;
		esac
	done
	if [ -n "$_bad" ]; then
		record nodes WARN "Cluster resources" \
			"$_n node(s), ${_cpu} allocatable cores, ${_mem} GiB. Node architecture(s) '${_bad}' are NOT covered by the published multi-arch image index (linux/amd64 and linux/arm64 only); movers cannot be scheduled there."
	else
		record nodes PASS "Cluster resources" \
			"$_n node(s), ${_cpu} allocatable cores, ${_mem} GiB, arch: ${_arches}"
	fi
	unset _n _arches _cpu _mem _bad _a
}

check_existing_install() {
	if k_get crd backups.crystalbackup.io -o jsonpath='{.metadata.name}'; then
		record existing WARN "Existing installation" \
			"backups.crystalbackup.io already exists — CrystalBackup (or its CRDs) is already installed here. This script assessed the cluster, not the running release."
	else
		if is_forbidden; then
			record existing UNKNOWN "Existing installation" "could not check for an existing installation ($(clean "$K_ERR"))."
		else
			record existing PASS "Existing installation" "no CrystalBackup CRDs present; this is a clean cluster."
		fi
	fi
}

# --- reporting ---------------------------------------------------------------------------------

status_color() {
	case $1 in
	PASS) printf '%s' "$C_PASS" ;;
	WARN) printf '%s' "$C_WARN" ;;
	FAIL) printf '%s' "$C_FAIL" ;;
	*) printf '%s' "$C_UNK" ;;
	esac
}

verdict_of() {
	if [ "$N_FAIL" -gt 0 ]; then
		printf 'BLOCKING'
	elif [ "$N_WARN" -gt 0 ] || [ "$N_UNKNOWN" -gt 0 ]; then
		printf 'READY_WITH_RESERVATIONS'
	else
		printf 'READY'
	fi
}

exit_code_of() {
	case $(verdict_of) in
	BLOCKING) printf 2 ;;
	READY_WITH_RESERVATIONS) printf 1 ;;
	*) printf 0 ;;
	esac
}

report_text() {
	printf '%sCrystalBackup preflight%s  %s(read-only; nothing was created, changed or written)%s\n' \
		"$C_BOLD" "$C_RESET" "$C_DIM" "$C_RESET"
	printf '%s\n\n' "$(printf '%s' '---------------------------------------------------------------------------')"

	printf '%sCHECKS%s\n' "$C_BOLD" "$C_RESET"
	while IFS="$US" read -r _id _st _title _detail; do
		[ -n "$_id" ] || continue
		printf '  %s%-7s%s %s\n' "$(status_color "$_st")" "$_st" "$C_RESET" "$_title"
		printf '          %s%s%s\n' "$C_DIM" "$_detail" "$C_RESET"
	done <<EOF
$RESULTS
EOF

	printf '\n%sWHAT WOULD BE BACKED UP%s  %s(per StorageClass)%s\n' \
		"$C_BOLD" "$C_RESET" "$C_DIM" "$C_RESET"
	# The scope of the table, printed where the table is read and not only where it is coded. A
	# reader who takes these rows for the whole answer is wrong about every volume they bound by
	# hand, and the row would never have told them: it would just have been about a different
	# driver, confidently.
	printf '  %sThese rows predict DYNAMICALLY provisioned volumes — the ones a class created, whose%s\n' "$C_DIM" "$C_RESET"
	printf '  %sPersistentVolume is therefore served by that class'"'"'s own provisioner. They do NOT predict%s\n' "$C_DIM" "$C_RESET"
	printf '  %sa volume you bound by hand: CrystalBackup reads a bound volume'"'"'s driver from its%s\n' "$C_DIM" "$C_RESET"
	printf '  %sPersistentVolume, not from the class it names. The check "Bound volumes" above names the%s\n' "$C_DIM" "$C_RESET"
	printf '  %sPVCs this table does not describe, and gives their real verdict.%s\n\n' "$C_DIM" "$C_RESET"
	if ! printf '%s' "$SC_ROWS" | grep -q '.'; then
		printf '  %sNo StorageClass verdict could be produced — see the checks above.%s\n' "$C_UNK" "$C_RESET"
	else
		# The column is labelled "(declared)" because that is exactly what it holds: the class's own
		# .provisioner, the string `kubectl get sc` shows. On a CSI-migrated class the verdict was
		# resolved for a DIFFERENT driver, so those rows are flagged [migrated] and their note names
		# the driver used. A column that silently printed one string while the verdict came from
		# another would be the same species of lie this table was just corrected for.
		printf '  %-22s %-30s %-16s %-14s %s\n' \
			'STORAGECLASS' 'PROVISIONER (declared)' 'SNAPSHOT CLASS' 'USABILITY' 'PVCs'
		_any_resolved=no
		_any_migrated=no
		while IFS="$US" read -r _n _p _drv _d _vsc _v _u _pn _pns _note; do
			[ -n "$_n" ] || continue
			# Column 3 is the VolumeSnapshotClass the operator would resolve — a name, or the
			# absence of one. Column 4 is what is known about using it, and its best value is
			# NOT ASSESSED. Splitting them is the point: the first is a fact this script can
			# establish, the second is a fact it cannot, and merging them into a single green
			# token is how this table used to read as an answer to the headline question.
			case $_v in
			skip) _c=$C_FAIL; _label='none' ; _uc=$C_FAIL; _ul='DATA SKIPPED' ;;
			unknown) _c=$C_UNK; _label='?' ; _uc=$C_UNK; _ul='UNDETERMINED' ;;
			*) _c=$C_WARN; _label=$_vsc; _uc=$C_UNK; _ul='NOT ASSESSED'; _any_resolved=yes ;;
			esac
			_star=''
			[ "$_d" = true ] && _star=' (default)'
			_pcell=$_p
			if [ "$_drv" != "$_p" ]; then
				_pcell="$_p [migrated]"
				_any_migrated=yes
			fi
			printf '  %s%-22s%s %-30s %s%-16s%s %s%-14s%s %s in %s ns\n' \
				"$C_BOLD" "$_n$_star" "$C_RESET" "$_pcell" \
				"$_c" "$_label" "$C_RESET" "$_uc" "$_ul" "$C_RESET" "$_pn" "$_pns"
			printf '      %s%s%s\n' "$C_DIM" "$_note" "$C_RESET"
		done <<EOF
$SC_ROWS
EOF
		if [ "$_any_migrated" = yes ]; then
			printf '\n  %s[migrated] marks a StorageClass whose declared provisioner is an in-tree plugin a CSI%s\n' "$C_WARN" "$C_RESET"
			printf '  %sdriver has superseded. Its verdict was resolved for the driver in its%s\n' "$C_WARN" "$C_RESET"
			printf '  %s%s annotation, NOT for the provisioner printed above — the%s\n' "$C_WARN" "$CB_MIGRATED_TO_ANNOTATION" "$C_RESET"
			printf '  %srow'"'"'s own note names it. Resolving through the provisioner would have reported these%s\n' "$C_WARN" "$C_RESET"
			printf '  %sclasses DATA SKIPPED, which is what CrystalBackup'"'"'s own preflight did until 0.6.5.%s\n' "$C_WARN" "$C_RESET"
		fi
		unset _any_migrated _pcell _drv
		if [ "$_any_resolved" = yes ]; then
			printf '\n  %sA named SNAPSHOT CLASS means a snapshot of that StorageClass can be REQUESTED.%s\n' "$C_WARN" "$C_RESET"
			printf '  %sWhether the volume restored from it MOUNTS on your nodes and reads back is a%s\n' "$C_WARN" "$C_RESET"
			printf '  %sdifferent question, and this read-only script cannot reach it. To answer it:%s\n' "$C_WARN" "$C_RESET"
			printf '      %s%s\n' "$C_DIM" "$CB_PROBE_URL"
			printf '      %s%s%s\n' "$C_DIM" "$CB_PROBE_DOCS" "$C_RESET"
		fi
		unset _any_resolved _uc _ul
	fi

	_v=$(verdict_of)
	case $_v in
	BLOCKING) _c=$C_FAIL ;;
	READY_WITH_RESERVATIONS) _c=$C_WARN ;;
	*) _c=$C_PASS ;;
	esac
	printf '\n%sVERDICT%s  %s%s%s   (%s passed, %s reservations, %s blocking, %s could not be determined)\n' \
		"$C_BOLD" "$C_RESET" "$_c$C_BOLD" "$_v" "$C_RESET" "$N_PASS" "$N_WARN" "$N_FAIL" "$N_UNKNOWN"
	if [ "$N_UNKNOWN" -gt 0 ]; then
		printf '%s  %s checks could not be made. They are NOT counted as passing.%s\n' "$C_UNK" "$N_UNKNOWN" "$C_RESET"
	fi
	printf 'exit %s\n' "$(exit_code_of)"
}

report_json() {
	# Schema v2, and the bump is not cosmetic: in v1 a resolved StorageClass reported
	# "dataBackedUp": true, which is the same unsupported claim the text report used to make, in
	# the form a machine acts on. It is now null for every class whose usability was not
	# assessed — and no class's usability is ever assessed by this script — and false only where
	# the data is genuinely skipped. There is no input to this script that makes it true.
	printf '{"schema":"crystalbackup.preflight/v2"'
	printf ',"scriptVersion":%s' "$(json_str "$SCRIPT_VERSION")"
	printf ',"usabilityAssessed":false'
	# The same scope caveat the text report prints above the table, for the reader that is a
	# program. A machine acting on storageClasses[] alone draws the same wrong conclusion about a
	# statically bound volume as a human would, and has even less chance of noticing.
	printf ',"storageClassScope":{"appliesTo":"dynamically-provisioned","boundVolumeDriverFrom":"PersistentVolume","check":"bound-volumes"}'
	printf ',"usabilityProbe":{"script":%s,"docs":%s}' \
		"$(json_str "$CB_PROBE_URL")" "$(json_str "$CB_PROBE_DOCS")"
	printf ',"jsonEncoder":%s' "$(json_str "$JSON_ENCODER")"
	printf ',"verdict":%s' "$(json_str "$(verdict_of)")"
	printf ',"exitCode":%s' "$(exit_code_of)"
	printf ',"summary":{"pass":%s,"warn":%s,"fail":%s,"unknown":%s}' "$N_PASS" "$N_WARN" "$N_FAIL" "$N_UNKNOWN"

	printf ',"checks":['
	_first=1
	while IFS="$US" read -r _id _st _title _detail; do
		[ -n "$_id" ] || continue
		[ "$_first" = 1 ] || printf ','
		_first=0
		printf '{"id":%s,"status":%s,"title":%s,"detail":%s}' \
			"$(json_str "$_id")" "$(json_str "$_st")" "$(json_str "$_title")" "$(json_str "$_detail")"
	done <<EOF
$RESULTS
EOF
	printf ']'

	printf ',"storageClasses":['
	_first=1
	# "provisioner" keeps meaning what the class declares — changing that value under an unchanged
	# key would silently rewrite history for every consumer. The driver the verdict was actually
	# resolved for is a NEW key, so a reader that cares can tell the two apart and one that does not
	# is at least not lied to.
	while IFS="$US" read -r _n _p _drv _d _vsc _v _u _pn _pns _note; do
		[ -n "$_n" ] || continue
		[ "$_first" = 1 ] || printf ','
		_first=0
		_bk=null
		[ "$_v" = skip ] && _bk=false
		_res=true
		{ [ "$_v" = skip ] || [ "$_v" = unknown ]; } && _res=false
		printf '{"name":%s,"provisioner":%s,"effectiveDriver":%s,"csiMigrated":%s,"isDefault":%s,"volumeSnapshotClass":%s,"exposer":%s,"snapshotClassResolved":%s,"usability":%s,"dataBackedUp":%s,"skipReason":%s,"pvcCount":%s,"namespaceCount":%s,"note":%s}' \
			"$(json_str "$_n")" "$(json_str "$_p")" "$(json_str "$_drv")" \
			"$([ "$_drv" != "$_p" ] && printf true || printf false)" \
			"$([ "$_d" = true ] && printf true || printf false)" \
			"$(json_str "$_vsc")" "$(json_str "$_v")" "$_res" "$(json_str "$_u")" "$_bk" \
			"$([ "$_v" = skip ] && json_str "$CB_SKIP_REASON" || printf null)" \
			"$_pn" "$_pns" "$(json_str "$_note")"
	done <<EOF
$SC_ROWS
EOF
	printf ']}\n'
	unset _first
}

# --- main ---------------------------------------------------------------------------------------

for _arg in "$@"; do
	case $_arg in
	--json) OUT_JSON=yes ;;
	--no-rbac-probe) RBAC_PROBE=no ;;
	--no-color) USE_COLOR=no ;;
	-h | --help)
		usage
		exit 3
		;;
	--version)
		printf 'crystalbackup-preflight %s\n' "$SCRIPT_VERSION"
		exit 3
		;;
	*)
		printf 'unknown option: %s (try --help)\n' "$_arg" >&2
		exit 3
		;;
	esac
done
unset _arg

setup_color
if command -v jq >/dev/null 2>&1; then
	HAVE_JQ=yes
	JSON_ENCODER='jq'
fi

check_kubectl
check_server_version

if [ "$HAVE_JQ" = no ]; then
	record jq WARN "jq" \
		"not found. Everything still runs — kubectl's own jsonpath does the parsing — but --json output uses a conservative builtin encoder that strips control characters instead of escaping them."
fi

check_vap
check_snapshot_crds
check_snapshot_controller
check_snapshot_classes
collect_volume_inventory
check_storage_classes
check_bound_volume_drivers
check_block_volumes
check_rbac
check_cert_manager
check_prometheus_operator
check_nodes
check_existing_install

if [ "$OUT_JSON" = yes ]; then
	report_json
else
	report_text
fi
exit "$(exit_code_of)"
