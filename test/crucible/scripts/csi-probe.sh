#!/usr/bin/env bash
# Crucible — CSI compatibility probe.
#
#   scripts/csi-probe.sh <storageclass> [--size 1Gi] [--keep] [--copy-probe]
#
# Qualifies ONE StorageClass against the EXACT exposure path CrystalBackup uses, and returns a
# verdict. It is a hand-rolled replay of internal/exposer (ADR 0003 §"The csi-generic flow"),
# deliberately NOT a call into the operator: the point is to answer "would this driver work?"
# on a cluster where CrystalBackup is not installed, before anyone spends a release qualifying it.
#
# What it replays, step for step (internal/exposer/{registry,snapshot,ready,cleanup}.go):
#
#   1. Resolution        — StorageClass.provisioner -> the VolumeSnapshotClass whose .driver
#                          equals it (lexicographically smallest on ties, exactly like
#                          Registry.findVolumeSnapshotClass). None -> SKIPPED, and that is a
#                          RESULT, not a failure (ErrUnsupported / CSISnapshotUnsupported).
#                          A provisioner containing ".cephfs.csi." selects cephfs-shallow,
#                          anything else csi-generic (Registry.For).
#   2. Source data       — a PVC in a throwaway SOURCE namespace, seeded by a short pod, hashed,
#                          then unmounted. The hash is what proves the exposed copy is the data.
#   3. Dynamic snapshot  — VolumeSnapshot in the SOURCE namespace, wait status.readyToUse.
#   4. Static re-bind    — patch the bound VolumeSnapshotContent to deletionPolicy: Retain, then
#                          create a pre-provisioned VSC/VS pair in a throwaway OPERATOR namespace
#                          against the SAME status.snapshotHandle, and wait for the static
#                          VolumeSnapshot to become readyToUse. This cross-namespace handover is
#                          the actual hard part and the thing most drivers break on.
#   5. Temp PVC + mount  — a PVC from the STATIC snapshot in the operator namespace
#                          (ReadWriteOnce for csi-generic, ReadOnlyMany for cephfs-shallow, per
#                          buildTempPVC / buildShallowPVC), sized max(request, restoreSize) as
#                          resolveTempPVCCapacity does, then a pod that mounts it READ-ONLY and
#                          re-verifies the hash. Times to Bound and to mounted are both measured.
#   6. Copy probe        — optional (--copy-probe): the whole thing again with a 10x dataset, so
#                          the temp-PVC provisioning times can be compared. A COW clone is
#                          constant-time; a full copy grows with the data. HEURISTIC — see the
#                          "What this probe does NOT prove" note at the bottom of this header.
#   7. Cleanup           — reverse order (temp PVC -> static pair -> restore the origin VSC's
#                          ORIGINAL deletionPolicy -> origin VolumeSnapshot -> source -> the two
#                          namespaces), on a trap EXIT so it also runs on failure, and idempotent.
#
# Verdicts — last line of stdout, and the "verdict" field of the JSON artifact:
#
#   COMPATIBLE                  exit 0   full path worked, data verified
#   SKIPPED                     exit 0   no VolumeSnapshotClass for this driver; CrystalBackup
#                                        would mark such volumes Skipped/CSISnapshotUnsupported
#                                        and still complete the Backup. A legitimate answer.
#   COMPATIBLE_COPIE_COMPLETE   exit 4   full path worked, but provisioning the temp PVC scales
#                                        with the data: this driver pays a full copy per backup
#   INCOMPATIBLE                exit 1   the DRIVER refused; the failing step and the driver's own
#                                        message are printed and recorded
#   PROBE_ERROR                 exit 3   THE PROBE could not answer: unreachable cluster, no
#                                        StorageClass by that name, no snapshot CRDs installed,
#                                        or a bug in here. Never blame the driver for this one.
#   (usage)                     exit 2   bad or missing arguments — nothing was created
#
# COMPATIBLE and SKIPPED share exit 0 on purpose: both mean "nothing to fix". Aggregation must
# read the JSON artifact, written to $CRUCIBLE_ARTIFACTS/csi-probe-<storageclass>.json (one line),
# ALWAYS — including on failure and on a probe crash.
#
# Blast radius: two namespaces it creates itself, plus the cluster-scoped VolumeSnapshotContent
# objects it creates itself (run-id suffixed). The ONLY object it touches that it did not create
# is the origin VolumeSnapshotContent the CSI driver provisioned for its own snapshot, whose
# deletionPolicy it flips to Retain for the handover and puts back to its ORIGINAL value at
# cleanup — exactly as internal/exposer/cleanup.go does.
#
# Requirements: bash >= 4, kubectl, jq. No Go, no cluster-side install.
#
# What this probe does NOT prove — read before quoting its output:
#   - COW vs full copy is a TIMING HEURISTIC, not a measurement of the storage backend. It reads
#     one number (temp-PVC provisioning time at 1x vs 10x) and it can be wrong in both directions:
#     an all-flash array can copy 500 MiB fast enough to look like COW, and a busy or
#     rate-limited backend can make a genuine COW clone look linear. It says "probable", and the
#     JSON keeps both raw times so a human can disagree with the classification.
#   - It proves nothing about a mover under load, concurrency, snapshot quotas, or long-run
#     behaviour: one volume, one snapshot, one mount, once.
#   - A COMPATIBLE verdict is about the EXPOSURE path only. Restore, retention and the rest of
#     the product are out of scope here.
#
# -E (errtrace) is not decoration: without it the ERR trap does not fire inside functions, and
# every unexpected failure inside run_round would reach the EXIT trap with no line number.
#
# SC2329 off file-wide: nearly every function here is called indirectly — the traps call on_err
# and on_exit, wait_for calls its predicates through "$@", and cleanup_one dispatches by name.
# shellcheck disable=SC2329
set -eEuo pipefail

# Numeric formatting is load-bearing (EPOCHREALTIME, awk arithmetic): a comma decimal separator
# would silently corrupt every duration measured below.
export LC_ALL=C

if ((BASH_VERSINFO[0] < 4)); then
  echo "FATAL: bash >= 4 required (macOS /bin/bash is 3.2 — 'brew install bash' and re-run)." >&2
  exit 2
fi

# ---------------------------------------------------------------------------
# Logging — same shape as deploy.sh (cyan '==>' steps), plus levels this script
# needs to keep "the driver refused" visually distinct from "the probe broke".
# ---------------------------------------------------------------------------
step() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
info() { printf '    %s\n' "$*"; }
ok() { printf '\033[1;32m    ok\033[0m  %s\n' "$*"; }
warn() { printf '\033[1;33m    !!\033[0m  %s\n' "$*" >&2; }

usage() {
  cat <<'EOF'
Usage: csi-probe.sh <storageclass> [options]

  --size <quantity>   source PVC size for the base round (default 1Gi, minimum 128Mi)
  --copy-probe        re-run the whole flow with a 10x dataset and classify COW vs full copy
  --keep              do not clean up (leaves the namespaces AND a Retain-pinned origin
                      VolumeSnapshotContent — i.e. a storage-side snapshot — behind)
  --timeout <sec>     per-wait budget in seconds (default 300)
  --poll <sec>        poll interval in seconds (default 1)
  -h, --help          this text

Verdict is the last line; the machine-readable result is written to
  <artifacts>/csi-probe-<storageclass>.json
EOF
}

usage_error() {
  echo "csi-probe: $*" >&2
  echo >&2
  usage >&2
  exit 2
}

# ---------------------------------------------------------------------------
# Arguments
# ---------------------------------------------------------------------------
SC=""
SIZE="1Gi"
KEEP=0
COPY_PROBE=0
TIMEOUT=300
POLL=1

while (($#)); do
  case "$1" in
  -h | --help)
    usage
    exit 0
    ;;
  --size)
    [[ $# -ge 2 ]] || usage_error "--size requires a value"
    SIZE="$2"
    shift 2
    ;;
  --timeout)
    [[ $# -ge 2 ]] || usage_error "--timeout requires a value"
    TIMEOUT="$2"
    shift 2
    ;;
  --poll)
    [[ $# -ge 2 ]] || usage_error "--poll requires a value"
    POLL="$2"
    shift 2
    ;;
  --keep)
    KEEP=1
    shift
    ;;
  --copy-probe)
    COPY_PROBE=1
    shift
    ;;
  -*) usage_error "unknown option: $1" ;;
  *)
    if [[ -n "${SC}" ]]; then usage_error "exactly one StorageClass expected (got '${SC}' and '$1')"; fi
    SC="$1"
    shift
    ;;
  esac
done

[[ -n "${SC}" ]] || usage_error "missing <storageclass>"
[[ "${TIMEOUT}" =~ ^[0-9]+$ ]] || usage_error "--timeout must be an integer number of seconds"
[[ "${POLL}" =~ ^[0-9]+$ ]] || usage_error "--poll must be an integer number of seconds"

# ---------------------------------------------------------------------------
# Paths / identity
# ---------------------------------------------------------------------------
CRUCIBLE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ARTIFACTS="${CRUCIBLE_ARTIFACTS:-${CRUCIBLE_DIR}/artifacts}"

# Only adopt the crucible kubeconfig when the caller has not chosen one AND it exists — otherwise
# a developer's own ~/.kube/config must keep working. This probe is meant to be pointed at any
# cluster, not just a crucible lane.
if [[ -z "${KUBECONFIG:-}" && -f "${ARTIFACTS}/kubeconfig" ]]; then
  export KUBECONFIG="${ARTIFACTS}/kubeconfig"
fi

# The dataset the source volume carries, per round. 50 MiB base / 500 MiB for the copy probe:
# small enough that the base round is a couple of minutes, large enough that a full copy of the
# 10x round cannot hide inside measurement noise on any realistic backend.
DATA_MIB_BASE=50
COPY_PROBE_FACTOR=10
PROBE_IMAGE="${CSI_PROBE_IMAGE:-busybox:1.36}" # same image the crucible seed workloads use
DUMP_BYTES=3000                               # truncation budget for on-timeout object dumps

# A run id in every object name, and in the namespace names. Two successive probes of the SAME
# StorageClass otherwise collide with each other's leftovers — and the collision that actually
# bites is a namespace still Terminating on a stuck CSI finalizer, which would make the second
# run fail as if the driver were broken. Unique names cost nothing and remove the whole class.
RUN_ID="$(date +%s)-$$"

# Namespace/object names must be DNS-1123 labels (<=63 chars): sanitise and cap the StorageClass
# name. The slug is cosmetic — RUN_ID is what guarantees uniqueness.
slug() {
  local s="${1,,}"
  s="${s//[^a-z0-9-]/-}"
  s="${s#-}"
  s="${s:0:20}"
  s="${s%-}"
  printf '%s' "${s:-sc}"
}
SLUG="$(slug "${SC}")"

NS_SRC="csiprobe-src-${SLUG}-${RUN_ID}"
NS_SYS="csiprobe-sys-${SLUG}-${RUN_ID}"

# The artifact keeps the FULL StorageClass name, not the truncated slug: two long class names can
# share a slug, and an aggregation that silently overwrote one driver's result with another's
# would be worse than no aggregation. StorageClass names are DNS-1123 subdomains, so they are
# already safe as a filename.
RESULT_JSON="${ARTIFACTS}/csi-probe-${SC}.json"

# ---------------------------------------------------------------------------
# Verdict state. VERDICT stays empty until something decides one; an empty
# VERDICT in the EXIT trap therefore means "we fell out of the script without
# concluding", which is a PROBE_ERROR and never the driver's fault.
# ---------------------------------------------------------------------------
VERDICT=""
VERDICT_RC=3
FAILED_STEP=""
REASON=""
CURRENT_STEP="startup"

PROVISIONER=""
VSCLASS=""
EXPOSER=""
BINDING_MODE=""

declare -A DUR=()      # "<round>:<name>" -> seconds (string, 1 decimal)
declare -A CHK=()      # "<round>:written" / "<round>:read"
declare -a CLEANUP_STACK=()
CLEANUP_FAILURES=0
COPY_CLASS="" # COW | FULL_COPY_LIKELY | INDETERMINATE

# ---------------------------------------------------------------------------
# Clock. EPOCHREALTIME gives millisecond resolution without forking; `date` is
# the fallback for a bash that lacks it. Measured durations are only as precise
# as the poll interval anyway — which is exactly why the copy-probe
# classification has an absolute floor and not just a ratio.
# ---------------------------------------------------------------------------
now_ms() {
  if [[ -n "${EPOCHREALTIME:-}" ]]; then
    local t=${EPOCHREALTIME} s f
    s=${t%%.*}
    f=${t#*.}
    printf '%d' $((10#${s} * 1000 + 10#${f:0:3}))
  else
    printf '%d' $(($(date +%s) * 1000))
  fi
}

secs_since() { # <start_ms> -> seconds, 1 decimal
  awk -v a="$1" -v b="$(now_ms)" 'BEGIN{printf "%.1f", (b-a)/1000}'
}

# ---------------------------------------------------------------------------
# Quantities. Only ever used to size the temp PVC (and to grow it to the
# snapshot's restoreSize, as resolveTempPVCCapacity does), so a plain byte count
# is emitted — a bare integer is a legal Kubernetes quantity and sidesteps every
# unit-rounding argument.
# ---------------------------------------------------------------------------
qty_to_bytes() {
  local q="$1" num unit
  num="${q%%[!0-9.]*}"
  unit="${q#"${num}"}"
  [[ -n "${num}" ]] || return 1
  local f
  case "${unit}" in
  "" | B) f=1 ;;
  Ki) f=1024 ;;
  Mi) f=1048576 ;;
  Gi) f=1073741824 ;;
  Ti) f=1099511627776 ;;
  k | K) f=1000 ;;
  M) f=1000000 ;;
  G) f=1000000000 ;;
  T) f=1000000000000 ;;
  *) return 1 ;;
  esac
  awk -v n="${num}" -v f="${f}" 'BEGIN{printf "%d", n*f}'
}

SIZE_BYTES="$(qty_to_bytes "${SIZE}")" || usage_error "--size: '${SIZE}' is not a quantity I understand (e.g. 1Gi, 512Mi)"
if ((SIZE_BYTES < 134217728)); then
  usage_error "--size must be at least 128Mi (the base round writes ${DATA_MIB_BASE}MiB, the copy probe ${COPY_PROBE_FACTOR}x that)"
fi

# ---------------------------------------------------------------------------
# Outcomes. Every exit that is not "the probe crashed" goes through one of
# these, so the last line of stdout, the exit code and the JSON can never
# disagree with each other.
# ---------------------------------------------------------------------------
finish_ok() {
  VERDICT="COMPATIBLE"
  VERDICT_RC=0
  exit 0
}

finish_full_copy() {
  VERDICT="COMPATIBLE_COPIE_COMPLETE"
  VERDICT_RC=4
  exit 4
}

finish_skipped() { # <reason>
  VERDICT="SKIPPED"
  VERDICT_RC=0
  REASON="$1"
  exit 0
}

# fail_driver: the driver (or the cluster's CSI stack) refused. This is a RESULT about the
# driver, printed as such, and it is the only path to INCOMPATIBLE.
fail_driver() { # <step> <reason...>
  FAILED_STEP="$1"
  shift
  REASON="$*"
  VERDICT="INCOMPATIBLE"
  VERDICT_RC=1
  exit 1
}

# fail_probe: the probe itself could not do its job — bad cluster access, an unparseable API
# reply, a bug in here. Explicitly NOT a verdict about the driver, and it says so.
fail_probe() { # <step> <reason...>
  FAILED_STEP="$1"
  shift
  REASON="$*"
  VERDICT="PROBE_ERROR"
  VERDICT_RC=3
  exit 3
}

on_err() { # <rc> <line>
  trap - ERR
  set +e
  if [[ -z "${VERDICT}" ]]; then
    FAILED_STEP="${CURRENT_STEP}"
    REASON="probe script aborted at line $2 (status $1) — this is a bug in csi-probe.sh, not a driver verdict"
    VERDICT="PROBE_ERROR"
    VERDICT_RC=3
  fi
}
trap 'on_err $? $LINENO' ERR

# ---------------------------------------------------------------------------
# Diagnostics
# ---------------------------------------------------------------------------
# dump_object prints the real observed state of whatever we gave up waiting for. A timeout
# message that says only "timed out" is worth nothing on a driver nobody here has ever run.
dump_object() { # <namespace|""> <kind> <name>
  local ns="$1" kind="$2" name="$3" out
  printf '\033[1;33m    -- observed state of %s/%s (truncated to %s bytes) --\033[0m\n' "${kind}" "${name}" "${DUMP_BYTES}" >&2
  if [[ -n "${ns}" ]]; then
    out="$(kubectl -n "${ns}" get "${kind}" "${name}" -o yaml 2>&1 || true)"
  else
    out="$(kubectl get "${kind}" "${name}" -o yaml 2>&1 || true)"
  fi
  # Truncated by parameter expansion, NOT `| head -c`: under `set -eEuo pipefail` a pipe
  # whose reader exits early kills the writer with SIGPIPE (141) and takes the whole
  # script with it. This function only ever runs when something has ALREADY failed, so
  # that turns a diagnostic into a bare exit 141 with no diagnosis at all.
  printf '%s\n' "${out:0:${DUMP_BYTES}}" >&2
  printf '\n' >&2
}

# last_warning gives the DRIVER's own words for a failure, which is what a compatibility report
# has to quote. VolumeSnapshot carries its message in status.error; everything else leaves it in
# a Warning event.
last_warning() { # <namespace|""> <kind> <name>
  local ns="$1" kind="$2" name="$3" args=()
  if [[ -n "${ns}" ]]; then args=(-n "${ns}"); else args=(-A); fi
  kubectl "${args[@]}" get events \
    --field-selector "involvedObject.kind=${kind},involvedObject.name=${name}" -o json 2>/dev/null |
    jq -r '[.items[]? | select(.type=="Warning")]
           | sort_by(.lastTimestamp // .eventTime // "")
           | last // {}
           | .message // empty' 2>/dev/null || true
}

snapshot_error() { # <namespace> <name>
  kubectl -n "$1" get volumesnapshot "$2" -o jsonpath='{.status.error.message}' 2>/dev/null || true
}

# describe_failure builds the one sentence that goes into the report: what we were waiting for,
# and what the driver said about it.
describe_failure() { # <what> <namespace|""> <kind> <name> [driver_message]
  local what="$1" ns="$2" kind="$3" name="$4" msg="${5:-}"
  [[ -n "${msg}" ]] || msg="$(last_warning "${ns}" "${kind}" "${name}")"
  if [[ -n "${msg}" ]]; then
    printf '%s after %ss; driver said: %s' "${what}" "${TIMEOUT}" "${msg}"
  else
    printf '%s after %ss; the driver reported no error at all (nothing in status, no Warning event)' "${what}" "${TIMEOUT}"
  fi
}

# ---------------------------------------------------------------------------
# Waiting
# ---------------------------------------------------------------------------
WAIT_ELAPSED=""

# wait_for polls a predicate until it holds or the budget runs out, and leaves the elapsed time
# in WAIT_ELAPSED either way (a failed wait's duration is still data worth recording).
wait_for() { # <predicate> [args...]
  local start deadline
  start="$(now_ms)"
  deadline=$((start + TIMEOUT * 1000))
  while true; do
    if "$@"; then
      WAIT_ELAPSED="$(secs_since "${start}")"
      return 0
    fi
    if (($(now_ms) > deadline)); then
      WAIT_ELAPSED="$(secs_since "${start}")"
      return 1
    fi
    sleep "${POLL}"
  done
}

vs_ready() { [[ "$(kubectl -n "$1" get volumesnapshot "$2" -o jsonpath='{.status.readyToUse}' 2>/dev/null || true)" == "true" ]]; }
vs_bound_content() { [[ -n "$(kubectl -n "$1" get volumesnapshot "$2" -o jsonpath='{.status.boundVolumeSnapshotContentName}' 2>/dev/null || true)" ]]; }
pvc_bound() { [[ "$(kubectl -n "$1" get pvc "$2" -o jsonpath='{.status.phase}' 2>/dev/null || true)" == "Bound" ]]; }
pod_phase() { kubectl -n "$1" get pod "$2" -o jsonpath='{.status.phase}' 2>/dev/null || true; }
# "Mounted" is the moment the pod stops being Pending: the kubelet has attached and mounted the
# volume. A very fast container can be Succeeded before we look, which counts just the same.
pod_started() { case "$(pod_phase "$1" "$2")" in Running | Succeeded | Failed) return 0 ;; *) return 1 ;; esac; }
pod_terminal() { case "$(pod_phase "$1" "$2")" in Succeeded | Failed) return 0 ;; *) return 1 ;; esac; }

# ---------------------------------------------------------------------------
# Cleanup — a stack, popped in reverse. Entries are pushed BEFORE the object is
# created, so an object that half-exists after a failed create is still torn
# down. Every delete tolerates absence, so re-running the trap is free.
# ---------------------------------------------------------------------------
push_cleanup() { CLEANUP_STACK+=("$*"); }

kdel() { # <human label> <kubectl args...>
  local label="$1"
  shift
  local out
  if ! out="$(kubectl "$@" 2>&1)"; then
    warn "cleanup: could not delete ${label} — ${out}"
    CLEANUP_FAILURES=$((CLEANUP_FAILURES + 1))
    return 0
  fi
  info "deleted ${label}"
}

# restore_vsc_policy puts the origin VolumeSnapshotContent back to the deletionPolicy it had
# BEFORE the probe touched it. This is the one object in the whole run that the probe did not
# create, and leaving it on Retain would strand the storage-side snapshot forever.
restore_vsc_policy() { # <vsc name> <original policy>
  local name="$1" want="${2:-Delete}" cur out
  cur="$(kubectl get volumesnapshotcontent "${name}" -o jsonpath='{.spec.deletionPolicy}' 2>/dev/null || true)"
  if [[ -z "${cur}" ]]; then
    info "origin VolumeSnapshotContent ${name} is already gone — nothing to restore"
    return 0
  fi
  if [[ "${cur}" == "${want}" ]]; then
    info "origin VolumeSnapshotContent ${name} already back to deletionPolicy=${want}"
    return 0
  fi
  if ! out="$(kubectl patch volumesnapshotcontent "${name}" --type=merge \
    -p "{\"spec\":{\"deletionPolicy\":\"${want}\"}}" 2>&1)"; then
    warn "cleanup: could not restore deletionPolicy=${want} on origin VolumeSnapshotContent ${name} — ${out}"
    warn "cleanup: that content is still Retain — its storage-side snapshot will NOT be reclaimed. Fix by hand."
    CLEANUP_FAILURES=$((CLEANUP_FAILURES + 1))
    return 0
  fi
  info "restored origin VolumeSnapshotContent ${name} to deletionPolicy=${want}"
}

cleanup_one() {
  local kind="$1"
  shift
  case "${kind}" in
  pod) kdel "pod ${1}/${2}" -n "$1" delete pod "$2" --ignore-not-found --wait=true --timeout=120s ;;
  pvc) kdel "pvc ${1}/${2}" -n "$1" delete pvc "$2" --ignore-not-found --wait=true --timeout=180s ;;
  vs) kdel "volumesnapshot ${1}/${2}" -n "$1" delete volumesnapshot "$2" --ignore-not-found --wait=true --timeout=180s ;;
  vsc) kdel "volumesnapshotcontent ${1}" delete volumesnapshotcontent "$1" --ignore-not-found --wait=true --timeout=180s ;;
  vscpolicy) restore_vsc_policy "$1" "$2" ;;
  # --wait=false on purpose: a namespace whose CSI objects are still finalising can take minutes
  # to disappear, and blocking the probe on it would turn a slow driver into a probe timeout.
  # The names are run-unique, so a lingering Terminating namespace cannot poison the next run.
  ns) kdel "namespace ${1}" delete namespace "$1" --ignore-not-found --wait=false ;;
  *) warn "cleanup: unknown entry '${kind} $*' (bug in csi-probe.sh)" ;;
  esac
}

cleanup_all() {
  if ((${#CLEANUP_STACK[@]} == 0)); then return 0; fi
  if ((KEEP)); then
    step "Cleanup SKIPPED (--keep)"
    warn "the probe's namespaces ${NS_SRC} / ${NS_SYS} and its VolumeSnapshotContents are still there."
    warn "the origin VolumeSnapshotContent is still patched to deletionPolicy=Retain: a storage-side"
    warn "snapshot is PINNED and will not be reclaimed until you undo it. Objects, newest first:"
    local i
    for ((i = ${#CLEANUP_STACK[@]} - 1; i >= 0; i--)); do
      info "  ${CLEANUP_STACK[i]}"
    done
    return 0
  fi
  step "Cleanup (reverse creation order: temp PVC -> static pair -> origin policy -> origin snapshot -> source -> namespaces)"
  local i entry
  for ((i = ${#CLEANUP_STACK[@]} - 1; i >= 0; i--)); do
    entry="${CLEANUP_STACK[i]}"
    # shellcheck disable=SC2086 # entries are our own space-separated tuples, never user input
    cleanup_one ${entry}
  done
  if ((CLEANUP_FAILURES > 0)); then
    warn "${CLEANUP_FAILURES} object(s) could not be removed — check by hand before the next run:"
    warn "  kubectl get ns,volumesnapshotcontent -l csiprobe.crystalbackup.io/run=${RUN_ID} -A"
  fi
}

# ---------------------------------------------------------------------------
# Result artifact — written on EVERY exit path, including a probe crash, because
# a missing file in an aggregation of a dozen drivers reads as "not run" when
# the truth was "ran and blew up".
# ---------------------------------------------------------------------------
jnum() { # a duration or "null"
  local v="${1:-}"
  if [[ "${v}" =~ ^[0-9]+(\.[0-9]+)?$ ]]; then printf '%s' "${v}"; else printf 'null'; fi
}

round_json() { # <round>
  local r="$1"
  jq -n \
    --argjson source_write "$(jnum "${DUR["${r}:source_write"]:-}")" \
    --argjson snapshot_ready "$(jnum "${DUR["${r}:snapshot_ready"]:-}")" \
    --argjson static_rebind_ready "$(jnum "${DUR["${r}:static_rebind_ready"]:-}")" \
    --argjson temp_pvc_bound "$(jnum "${DUR["${r}:temp_pvc_bound"]:-}")" \
    --argjson temp_mount "$(jnum "${DUR["${r}:temp_mount"]:-}")" \
    --argjson data_mib "$(jnum "${DUR["${r}:data_mib"]:-}")" \
    --arg checksum_written "${CHK["${r}:written"]:-}" \
    --arg checksum_read "${CHK["${r}:read"]:-}" \
    '{data_mib: $data_mib,
      durations_seconds: {
        source_write: $source_write,
        snapshot_ready: $snapshot_ready,
        static_rebind_ready: $static_rebind_ready,
        temp_pvc_bound: $temp_pvc_bound,
        temp_mount: $temp_mount
      },
      checksum: {written: $checksum_written, read: $checksum_read,
                 match: (($checksum_written != "") and ($checksum_written == $checksum_read))}}'
}

write_json() {
  mkdir -p "${ARTIFACTS}" 2>/dev/null || true
  local base big copy enabled kept
  if ((KEEP)); then kept=true; else kept=false; fi
  base="$(round_json base)" || base="null"
  big="null"
  if [[ -n "${DUR["big:data_mib"]:-}" ]]; then
    big="$(round_json big)" || big="null"
  fi
  if ((COPY_PROBE)); then enabled=true; else enabled=false; fi
  copy="$(jq -n \
    --arg classification "${COPY_CLASS}" \
    --argjson enabled "${enabled}" \
    --argjson small_bound "$(jnum "${DUR["base:temp_pvc_bound"]:-}")" \
    --argjson big_bound "$(jnum "${DUR["big:temp_pvc_bound"]:-}")" \
    '{enabled: $enabled,
      small_bound_seconds: $small_bound,
      big_bound_seconds: $big_bound,
      classification: (if $classification == "" then null else $classification end)}')" ||
    copy='{"enabled":null,"classification":null}'

  if ! jq -nc \
    --arg schema "csi-probe/v1" \
    --arg timestamp "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg run_id "${RUN_ID}" \
    --arg storageclass "${SC}" \
    --arg provisioner "${PROVISIONER}" \
    --arg volume_snapshot_class "${VSCLASS}" \
    --arg exposer "${EXPOSER}" \
    --arg volume_binding_mode "${BINDING_MODE}" \
    --arg size "${SIZE}" \
    --arg verdict "${VERDICT:-PROBE_ERROR}" \
    --arg failed_step "${FAILED_STEP}" \
    --arg reason "${REASON}" \
    --argjson exit_code "${VERDICT_RC}" \
    --argjson kept "${kept}" \
    --argjson cleanup_failures "${CLEANUP_FAILURES}" \
    --argjson base "${base}" \
    --argjson big "${big}" \
    --argjson copy_probe "${copy}" \
    '{schema: $schema, timestamp: $timestamp, run_id: $run_id,
      storageclass: $storageclass, provisioner: $provisioner,
      volume_snapshot_class: (if $volume_snapshot_class == "" then null else $volume_snapshot_class end),
      exposer: (if $exposer == "" then null else $exposer end),
      volume_binding_mode: (if $volume_binding_mode == "" then null else $volume_binding_mode end),
      requested_size: $size,
      verdict: $verdict, exit_code: $exit_code,
      failed_step: (if $failed_step == "" then null else $failed_step end),
      reason: (if $reason == "" then null else $reason end),
      rounds: {base: $base, big: $big},
      copy_probe: $copy_probe,
      kept: $kept, cleanup_failures: $cleanup_failures}' \
    >"${RESULT_JSON}.tmp"; then
    rm -f "${RESULT_JSON}.tmp"
    warn "could not write the result artifact ${RESULT_JSON} (jq refused the payload — probe bug)"
    return 0
  fi
  # Written via a temp file and renamed, so an aggregator can never read a half-written line.
  mv -f "${RESULT_JSON}.tmp" "${RESULT_JSON}"
}

print_verdict() {
  echo
  case "${VERDICT}" in
  COMPATIBLE)
    printf '\033[1;32m%s\033[0m — %s (%s) replays CrystalBackup'"'"'s %s exposure path end to end, data verified.\n' \
      "COMPATIBLE" "${SC}" "${PROVISIONER}" "${EXPOSER}"
    ;;
  COMPATIBLE_COPIE_COMPLETE)
    printf '\033[1;33m%s\033[0m — %s (%s) works, but the temp PVC provisioning time scales with the data: a full copy per backup is likely (%ss for %sMiB vs %ss for %sMiB).\n' \
      "COMPATIBLE_COPIE_COMPLETE" "${SC}" "${PROVISIONER}" \
      "${DUR["base:temp_pvc_bound"]:-?}" "${DUR["base:data_mib"]:-?}" \
      "${DUR["big:temp_pvc_bound"]:-?}" "${DUR["big:data_mib"]:-?}"
    ;;
  SKIPPED)
    printf '\033[1;34m%s\033[0m — %s (%s): %s. CrystalBackup would mark such volumes Skipped/CSISnapshotUnsupported and still complete the Backup.\n' \
      "SKIPPED" "${SC}" "${PROVISIONER}" "${REASON}"
    ;;
  INCOMPATIBLE)
    printf '\033[1;31m%s\033[0m — %s (%s) failed at step "%s": %s\n' \
      "INCOMPATIBLE" "${SC}" "${PROVISIONER}" "${FAILED_STEP}" "${REASON}"
    ;;
  *)
    printf '\033[1;35m%s\033[0m — the PROBE failed at step "%s", NOT the driver: %s\n' \
      "PROBE_ERROR" "${FAILED_STEP:-${CURRENT_STEP}}" "${REASON}"
    printf '           No verdict can be read from this run for %s.\n' "${SC}"
    ;;
  esac
  printf '           result: %s\n' "${RESULT_JSON}"
}

on_exit() {
  local rc=$?
  trap - EXIT ERR
  set +e
  if [[ -z "${VERDICT}" ]]; then
    FAILED_STEP="${FAILED_STEP:-${CURRENT_STEP}}"
    REASON="${REASON:-probe exited unexpectedly (status ${rc}) — a bug in csi-probe.sh, not a driver verdict}"
    VERDICT="PROBE_ERROR"
    VERDICT_RC=3
  fi
  cleanup_all
  write_json
  print_verdict
  exit "${VERDICT_RC}"
}
trap on_exit EXIT

# ---------------------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------------------
CURRENT_STEP="preflight"
step "Preflight"
for bin in kubectl jq; do
  command -v "${bin}" >/dev/null 2>&1 || fail_probe preflight "'${bin}' not found in PATH"
done
kubectl version -o json >/dev/null 2>&1 || fail_probe preflight "cannot reach a cluster (KUBECONFIG=${KUBECONFIG:-<default>})"
info "cluster: $(kubectl config current-context 2>/dev/null || echo '<unknown context>')"
info "run id : ${RUN_ID}"

# ---------------------------------------------------------------------------
# Step 1 — resolution (internal/exposer/registry.go, Registry.For)
# ---------------------------------------------------------------------------
CURRENT_STEP="resolve"
step "1/7  Resolution — StorageClass -> provisioner -> VolumeSnapshotClass -> exposer"

kubectl get storageclass "${SC}" >/dev/null 2>&1 ||
  fail_probe resolve "StorageClass '${SC}' does not exist. (Registry.For treats this as a hard error too — it is not a verdict about snapshot capability.)"

PROVISIONER="$(kubectl get storageclass "${SC}" -o jsonpath='{.provisioner}' 2>/dev/null || true)"
[[ -n "${PROVISIONER}" ]] || fail_probe resolve "StorageClass '${SC}' has no .provisioner"
BINDING_MODE="$(kubectl get storageclass "${SC}" -o jsonpath='{.volumeBindingMode}' 2>/dev/null || true)"
BINDING_MODE="${BINDING_MODE:-Immediate}"
info "provisioner       : ${PROVISIONER}"
info "volumeBindingMode : ${BINDING_MODE}"

# No snapshot CRDs at all is an ENVIRONMENT problem, not a driver verdict: qualifying anything
# here is impossible, and answering SKIPPED for a dozen drivers in a row would be a lie.
kubectl get crd volumesnapshotclasses.snapshot.storage.k8s.io >/dev/null 2>&1 ||
  fail_probe resolve "this cluster has no snapshot.storage.k8s.io CRDs — install external-snapshotter first; no driver can be qualified without it"

# Same rule as Registry.findVolumeSnapshotClass: match on .driver, and on ties take the
# lexicographically smallest name so the choice is deterministic rather than list-order dependent.
VSCLASS="$(kubectl get volumesnapshotclasses -o json 2>/dev/null |
  jq -r --arg d "${PROVISIONER}" '[.items[]? | select(.driver == $d) | .metadata.name] | sort | .[0] // empty')"

if [[ -z "${VSCLASS}" ]]; then
  info "no VolumeSnapshotClass has driver == '${PROVISIONER}'"
  finish_skipped "no VolumeSnapshotClass for driver '${PROVISIONER}' (exposer.ErrUnsupported / CSISnapshotUnsupported)"
fi
info "VolumeSnapshotClass: ${VSCLASS}"

# ADR 0003 / Registry.For: ".cephfs.csi." in the provisioner name -> cephfs-shallow (ReadOnlyMany,
# snapshot-backed, zero copy); anything else -> csi-generic (ReadWriteOnce, writable so the
# kubelet can replay a dirty journal).
if [[ "${PROVISIONER}" == *".cephfs.csi."* ]]; then
  EXPOSER="cephfs-shallow"
  TEMP_ACCESS_MODE="ReadOnlyMany"
else
  EXPOSER="csi-generic"
  TEMP_ACCESS_MODE="ReadWriteOnce"
fi
ok "CrystalBackup would expose this class with '${EXPOSER}' (temp PVC accessMode ${TEMP_ACCESS_MODE})"

# ---------------------------------------------------------------------------
# Namespaces — one standing in for the tenant namespace, one for
# crystal-backup-system. Two of them, and that is the whole point: a snapshot
# that cannot cross a namespace boundary is useless to this product.
# ---------------------------------------------------------------------------
CURRENT_STEP="namespaces"
step "2/7  Throwaway namespaces (source + 'operator')"
create_namespace() { # <name> <role>
  push_cleanup "ns $1"
  # PodSecurity baseline is set explicitly: the probe pods run as root to write to a fresh
  # volume, which baseline allows and 'restricted' does not. On a cluster that defaults every
  # namespace to restricted, not setting this would fail the probe for a reason that has nothing
  # to do with the CSI driver.
  kubectl apply -f - >/dev/null <<YAML || fail_probe namespaces "cannot create namespace $1"
apiVersion: v1
kind: Namespace
metadata:
  name: $1
  labels:
    app.kubernetes.io/part-of: csi-probe
    csiprobe.crystalbackup.io/run: "${RUN_ID}"
    csiprobe.crystalbackup.io/role: "$2"
    pod-security.kubernetes.io/enforce: baseline
YAML
  info "namespace $1 ($2)"
}
create_namespace "${NS_SRC}" source
create_namespace "${NS_SYS}" operator

# ---------------------------------------------------------------------------
# The round: everything from the source PVC to the verified read-only mount.
# Run once for the base dataset, and again at 10x when --copy-probe is on.
# ---------------------------------------------------------------------------
run_round() { # <round tag> <pvc size bytes> <data MiB>
  local round="$1" pvc_bytes="$2" data_mib="$3"
  local src_pvc="probe-src-${round}"
  local writer="probe-writer-${round}"
  local origin_vs="probe-snap-${round}"
  local static_vsc="csiprobe-${SLUG}-${RUN_ID}-${round}"
  local static_vs="probe-restore-${round}"
  local temp_pvc="probe-clone-${round}"
  local reader="probe-reader-${round}"
  local t0

  DUR["${round}:data_mib"]="${data_mib}"

  # -- 3/7 source data ------------------------------------------------------
  CURRENT_STEP="source-data (${round})"
  step "3/7  Source volume + deterministic dataset (${round}: ${data_mib}MiB in a $(awk -v b="${pvc_bytes}" 'BEGIN{printf "%.0f", b/1048576}')MiB PVC)"

  push_cleanup "pvc ${NS_SRC} ${src_pvc}"
  kubectl apply -f - >/dev/null <<YAML || fail_probe "source-data" "cannot create source PVC ${NS_SRC}/${src_pvc}"
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${src_pvc}
  namespace: ${NS_SRC}
  labels: {app.kubernetes.io/part-of: csi-probe, csiprobe.crystalbackup.io/run: "${RUN_ID}"}
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: ${SC}
  resources:
    requests:
      storage: "${pvc_bytes}"
YAML

  # The writer is the PVC's first consumer, which is what unblocks a
  # WaitForFirstConsumer class; it writes a small readable tree plus one bulk file, records a
  # per-file sha256 manifest INSIDE the volume, and prints the manifest's own hash so the probe
  # can compare it with what the exposed copy yields later.
  #
  # The bulk file is /dev/urandom rather than a repeating pattern on purpose: the copy probe
  # compares provisioning times, and compressible or dedupable filler would let a backend that
  # really does copy the data look like a COW clone. Content reproducibility across runs is not
  # needed — the hash is captured at write time and re-checked at read time within the same run.
  push_cleanup "pod ${NS_SRC} ${writer}"
  kubectl apply -f - >/dev/null <<YAML || fail_probe "source-data" "cannot create writer pod ${NS_SRC}/${writer}"
apiVersion: v1
kind: Pod
metadata:
  name: ${writer}
  namespace: ${NS_SRC}
  labels: {app.kubernetes.io/part-of: csi-probe, csiprobe.crystalbackup.io/run: "${RUN_ID}"}
spec:
  restartPolicy: Never
  containers:
    - name: writer
      image: ${PROBE_IMAGE}
      command: ["/bin/sh", "-ec"]
      args:
        - |
          mkdir -p /data/tree/a/b
          printf 'crystalbackup csi-probe %s round=%s sc=%s\n' "${RUN_ID}" "${round}" "${SC}" > /data/README.txt
          printf 'alpha\n' > /data/tree/a/one.txt
          printf 'bravo\n' > /data/tree/a/b/two.txt
          printf 'charlie\n' > /data/tree/a/b/three.txt
          dd if=/dev/urandom of=/data/bulk.bin bs=1M count=${data_mib} 2>/dev/null
          cd /data
          find . -type f ! -name MANIFEST.sha256 | sort | xargs sha256sum > /tmp/manifest
          mv /tmp/manifest /data/MANIFEST.sha256
          sync
          echo "PROBE_CHECKSUM=\$(sha256sum < /data/MANIFEST.sha256 | cut -d' ' -f1)"
      volumeMounts:
        - {name: data, mountPath: /data}
      resources:
        requests: {cpu: 50m, memory: 64Mi}
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: ${src_pvc}
YAML

  if ! wait_for pod_terminal "${NS_SRC}" "${writer}"; then
    dump_object "${NS_SRC}" pvc "${src_pvc}"
    dump_object "${NS_SRC}" pod "${writer}"
    fail_driver "source-data" "$(describe_failure "the source PVC never bound or never mounted (writer pod still Pending)" "${NS_SRC}" Pod "${writer}")"
  fi
  DUR["${round}:source_write"]="${WAIT_ELAPSED}"
  if [[ "$(pod_phase "${NS_SRC}" "${writer}")" != "Succeeded" ]]; then
    dump_object "${NS_SRC}" pod "${writer}"
    fail_driver "source-data" "the writer pod failed on this volume: $(kubectl -n "${NS_SRC}" logs "${writer}" --tail=10 2>&1 | tr '\n' ' ')"
  fi

  local written
  written="$(kubectl -n "${NS_SRC}" logs "${writer}" 2>/dev/null | awk -F= '/^PROBE_CHECKSUM=/{print $2}' | tail -1)"
  [[ -n "${written}" ]] || fail_probe "source-data" "the writer pod succeeded but printed no PROBE_CHECKSUM (probe bug or truncated logs)"
  CHK["${round}:written"]="${written}"
  ok "wrote ${data_mib}MiB + 4 files in ${WAIT_ELAPSED}s, manifest sha256 ${written:0:16}…"

  # Unmount before snapshotting. Not strictly required — CrystalBackup snapshots live, mounted
  # volumes — but it removes "the writer was still flushing" as an explanation for any later
  # checksum mismatch, which is the one failure mode this probe must never misattribute.
  kubectl -n "${NS_SRC}" delete pod "${writer}" --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1 || true
  info "writer unmounted"

  # -- 4/7 dynamic snapshot in the SOURCE namespace -------------------------
  CURRENT_STEP="snapshot (${round})"
  step "4/7  VolumeSnapshot in the source namespace (${round})"
  push_cleanup "vs ${NS_SRC} ${origin_vs}"
  kubectl apply -f - >/dev/null <<YAML || fail_driver "snapshot" "the API server refused the VolumeSnapshot for class ${VSCLASS}"
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: ${origin_vs}
  namespace: ${NS_SRC}
  labels: {app.kubernetes.io/part-of: csi-probe, csiprobe.crystalbackup.io/run: "${RUN_ID}"}
spec:
  volumeSnapshotClassName: ${VSCLASS}
  source:
    persistentVolumeClaimName: ${src_pvc}
YAML

  if ! wait_for vs_ready "${NS_SRC}" "${origin_vs}"; then
    dump_object "${NS_SRC}" volumesnapshot "${origin_vs}"
    fail_driver "snapshot" "$(describe_failure "VolumeSnapshot ${NS_SRC}/${origin_vs} never reached status.readyToUse=true" \
      "${NS_SRC}" VolumeSnapshot "${origin_vs}" "$(snapshot_error "${NS_SRC}" "${origin_vs}")")"
  fi
  DUR["${round}:snapshot_ready"]="${WAIT_ELAPSED}"
  ok "snapshot readyToUse in ${WAIT_ELAPSED}s"

  # -- 5/7 static re-bind into the 'operator' namespace ---------------------
  CURRENT_STEP="static-rebind (${round})"
  step "5/7  Static re-bind into the operator namespace (${round}) — the cross-namespace handover"
  t0="$(now_ms)"

  if ! wait_for vs_bound_content "${NS_SRC}" "${origin_vs}"; then
    dump_object "${NS_SRC}" volumesnapshot "${origin_vs}"
    fail_driver "static-rebind" "$(describe_failure "VolumeSnapshot ${NS_SRC}/${origin_vs} is readyToUse but never published status.boundVolumeSnapshotContentName" \
      "${NS_SRC}" VolumeSnapshot "${origin_vs}")"
  fi
  local origin_vsc driver handle origin_policy
  origin_vsc="$(kubectl -n "${NS_SRC}" get volumesnapshot "${origin_vs}" -o jsonpath='{.status.boundVolumeSnapshotContentName}')"
  driver="$(kubectl get volumesnapshotcontent "${origin_vsc}" -o jsonpath='{.spec.driver}' 2>/dev/null || true)"
  handle="$(kubectl get volumesnapshotcontent "${origin_vsc}" -o jsonpath='{.status.snapshotHandle}' 2>/dev/null || true)"
  origin_policy="$(kubectl get volumesnapshotcontent "${origin_vsc}" -o jsonpath='{.spec.deletionPolicy}' 2>/dev/null || true)"
  origin_policy="${origin_policy:-Delete}"

  if [[ -z "${driver}" || -z "${handle}" ]]; then
    dump_object "" volumesnapshotcontent "${origin_vsc}"
    fail_driver "static-rebind" "the bound VolumeSnapshotContent ${origin_vsc} exposes no spec.driver / status.snapshotHandle, so the snapshot cannot be re-bound into another namespace at all"
  fi
  info "bound content   : ${origin_vsc}"
  info "snapshotHandle  : ${handle}"
  info "origin policy   : ${origin_policy} (will be restored at cleanup)"

  # Retain FIRST, and record the original value so cleanup can put it back. This is exactly
  # ready.go's patchOriginVSCForHandover: for the duration of the handover, nothing may reclaim
  # the storage-side snapshot the static pair is about to point at.
  push_cleanup "vscpolicy ${origin_vsc} ${origin_policy}"
  kubectl patch volumesnapshotcontent "${origin_vsc}" --type=merge \
    -p '{"spec":{"deletionPolicy":"Retain"}}' >/dev/null ||
    fail_driver "static-rebind" "could not patch VolumeSnapshotContent ${origin_vsc} to deletionPolicy=Retain (the handover guard)"
  info "patched origin content to Retain"

  # The pre-provisioned pair, against the SAME handle (ready.go: buildStaticVolumeSnapshotContent
  # + buildStaticVolumeSnapshot). volumeSnapshotClassName is deliberately omitted on both: it is a
  # dynamic-provisioning input and a pre-provisioned object derives everything from driver+handle.
  push_cleanup "vsc ${static_vsc}"
  kubectl apply -f - >/dev/null <<YAML || fail_driver "static-rebind" "the API server refused the pre-provisioned VolumeSnapshotContent"
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotContent
metadata:
  name: ${static_vsc}
  labels: {app.kubernetes.io/part-of: csi-probe, csiprobe.crystalbackup.io/run: "${RUN_ID}"}
spec:
  deletionPolicy: Retain
  driver: ${driver}
  source:
    snapshotHandle: "${handle}"
  volumeSnapshotRef:
    apiVersion: snapshot.storage.k8s.io/v1
    kind: VolumeSnapshot
    name: ${static_vs}
    namespace: ${NS_SYS}
YAML

  push_cleanup "vs ${NS_SYS} ${static_vs}"
  kubectl apply -f - >/dev/null <<YAML || fail_driver "static-rebind" "the API server refused the static VolumeSnapshot in ${NS_SYS}"
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: ${static_vs}
  namespace: ${NS_SYS}
  labels: {app.kubernetes.io/part-of: csi-probe, csiprobe.crystalbackup.io/run: "${RUN_ID}"}
spec:
  source:
    volumeSnapshotContentName: ${static_vsc}
YAML

  if ! wait_for vs_ready "${NS_SYS}" "${static_vs}"; then
    dump_object "${NS_SYS}" volumesnapshot "${static_vs}"
    dump_object "" volumesnapshotcontent "${static_vsc}"
    fail_driver "static-rebind" "$(describe_failure "the re-bound VolumeSnapshot ${NS_SYS}/${static_vs} never became readyToUse — this driver's snapshot does not survive a cross-namespace pre-provisioned re-bind" \
      "${NS_SYS}" VolumeSnapshot "${static_vs}" "$(snapshot_error "${NS_SYS}" "${static_vs}")")"
  fi
  DUR["${round}:static_rebind_ready"]="$(secs_since "${t0}")"
  ok "snapshot re-bound into ${NS_SYS} in ${DUR["${round}:static_rebind_ready"]}s"

  # -- 6/7 temp PVC + read-only mount ---------------------------------------
  CURRENT_STEP="temp-pvc (${round})"
  step "6/7  Temp PVC from the static snapshot (${round}, ${TEMP_ACCESS_MODE}) + read-only mount"

  # resolveTempPVCCapacity: floor at what we asked for, grow to the snapshot's restoreSize when
  # that is larger — a temp PVC smaller than the snapshot simply will not provision.
  local restore_size restore_bytes temp_bytes
  restore_size="$(kubectl -n "${NS_SRC}" get volumesnapshot "${origin_vs}" -o jsonpath='{.status.restoreSize}' 2>/dev/null || true)"
  temp_bytes="${pvc_bytes}"
  if [[ -n "${restore_size}" ]] && restore_bytes="$(qty_to_bytes "${restore_size}")"; then
    if ((restore_bytes > temp_bytes)); then
      temp_bytes="${restore_bytes}"
      info "grew the temp PVC to the snapshot's restoreSize (${restore_size})"
    fi
  fi

  push_cleanup "pvc ${NS_SYS} ${temp_pvc}"
  push_cleanup "pod ${NS_SYS} ${reader}"

  local reader_yaml
  reader_yaml="$(
    cat <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: ${reader}
  namespace: ${NS_SYS}
  labels: {app.kubernetes.io/part-of: csi-probe, csiprobe.crystalbackup.io/run: "${RUN_ID}"}
spec:
  restartPolicy: Never
  containers:
    - name: reader
      image: ${PROBE_IMAGE}
      command: ["/bin/sh", "-ec"]
      args:
        - |
          cd /data
          find . -type f ! -name MANIFEST.sha256 | sort | xargs sha256sum > /tmp/manifest
          if ! cmp -s /tmp/manifest /data/MANIFEST.sha256; then
            echo "PROBE_VERIFY=MISMATCH"
            diff /data/MANIFEST.sha256 /tmp/manifest | head -20 || true
            exit 17
          fi
          echo "PROBE_CHECKSUM=\$(sha256sum < /tmp/manifest | cut -d' ' -f1)"
          echo "PROBE_VERIFY=OK"
      volumeMounts:
        - {name: data, mountPath: /data, readOnly: true}
      resources:
        requests: {cpu: 50m, memory: 64Mi}
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: ${temp_pvc}
        readOnly: true
YAML
  )"

  local pvc_yaml
  pvc_yaml="$(
    cat <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${temp_pvc}
  namespace: ${NS_SYS}
  labels: {app.kubernetes.io/part-of: csi-probe, csiprobe.crystalbackup.io/run: "${RUN_ID}"}
spec:
  accessModes: ["${TEMP_ACCESS_MODE}"]
  storageClassName: ${SC}
  resources:
    requests:
      storage: "${temp_bytes}"
  dataSource:
    apiGroup: snapshot.storage.k8s.io
    kind: VolumeSnapshot
    name: ${static_vs}
YAML
  )"

  t0="$(now_ms)"
  printf '%s\n' "${pvc_yaml}" | kubectl apply -f - >/dev/null ||
    fail_driver "temp-pvc" "the API server refused the temp PVC (dataSource VolumeSnapshot ${static_vs}, accessMode ${TEMP_ACCESS_MODE})"

  # WaitForFirstConsumer classes never bind without a consumer, so the reader has to exist
  # first — and then "time to Bound" legitimately includes scheduling. The operator hits exactly
  # the same thing (temp PVC then mover Job), so this is faithful, not a workaround; the JSON
  # records the binding mode so nobody compares an Immediate class against a WFFC one blindly.
  if [[ "${BINDING_MODE}" == "WaitForFirstConsumer" ]]; then
    printf '%s\n' "${reader_yaml}" | kubectl apply -f - >/dev/null ||
      fail_probe "temp-pvc" "cannot create reader pod ${NS_SYS}/${reader}"
    info "WaitForFirstConsumer: reader created first; the bind time below includes scheduling"
  fi

  if ! wait_for pvc_bound "${NS_SYS}" "${temp_pvc}"; then
    dump_object "${NS_SYS}" pvc "${temp_pvc}"
    fail_driver "temp-pvc" "$(describe_failure "the temp PVC ${NS_SYS}/${temp_pvc} never reached Bound — this driver cannot create a ${TEMP_ACCESS_MODE} volume from a re-bound snapshot" \
      "${NS_SYS}" PersistentVolumeClaim "${temp_pvc}")"
  fi
  DUR["${round}:temp_pvc_bound"]="${WAIT_ELAPSED}"
  ok "temp PVC Bound in ${WAIT_ELAPSED}s"

  CURRENT_STEP="mount (${round})"
  if [[ "${BINDING_MODE}" != "WaitForFirstConsumer" ]]; then
    t0="$(now_ms)"
    printf '%s\n' "${reader_yaml}" | kubectl apply -f - >/dev/null ||
      fail_probe "mount" "cannot create reader pod ${NS_SYS}/${reader}"
  fi

  if ! wait_for pod_started "${NS_SYS}" "${reader}"; then
    dump_object "${NS_SYS}" pod "${reader}"
    fail_driver "mount" "$(describe_failure "the reader pod never mounted the temp PVC read-only (still Pending)" "${NS_SYS}" Pod "${reader}")"
  fi
  DUR["${round}:temp_mount"]="$(secs_since "${t0}")"
  ok "temp PVC mounted read-only in ${DUR["${round}:temp_mount"]}s (includes scheduling and image pull)"

  CURRENT_STEP="verify (${round})"
  if ! wait_for pod_terminal "${NS_SYS}" "${reader}"; then
    dump_object "${NS_SYS}" pod "${reader}"
    fail_probe "verify" "the reader pod mounted but never finished within ${TIMEOUT}s"
  fi
  local reader_logs read_sum
  reader_logs="$(kubectl -n "${NS_SYS}" logs "${reader}" 2>&1 || true)"
  if [[ "$(pod_phase "${NS_SYS}" "${reader}")" != "Succeeded" ]]; then
    # Flattened and truncated in-shell for the same SIGPIPE reason as dump_object above.
    local flat="${reader_logs//$'\n'/ }"
    fail_driver "verify" "the exposed copy does not match the source: ${flat:0:600}"
  fi
  read_sum="$(printf '%s\n' "${reader_logs}" | awk -F= '/^PROBE_CHECKSUM=/{print $2}' | tail -1)"
  CHK["${round}:read"]="${read_sum}"
  if [[ -z "${read_sum}" || "${read_sum}" != "${CHK["${round}:written"]}" ]]; then
    fail_driver "verify" "checksum mismatch between the source volume (${CHK["${round}:written"]:-<none>}) and the exposed copy (${read_sum:-<none>})"
  fi
  ok "data verified through the exposed copy (sha256 ${read_sum:0:16}…)"
}

# ---------------------------------------------------------------------------
# Base round
# ---------------------------------------------------------------------------
run_round base "${SIZE_BYTES}" "${DATA_MIB_BASE}"

# ---------------------------------------------------------------------------
# Copy probe — the same flow at 10x, then a comparison of the ONE number that
# is a usable proxy for "did the backend copy the data": how long the temp PVC
# took to provision. Bind time, not mount time: mount time carries scheduling
# and image pull, which have nothing to do with the storage backend.
# ---------------------------------------------------------------------------
if ((COPY_PROBE)); then
  CURRENT_STEP="copy-probe"
  step "7/7  Copy probe — replaying the whole flow with ${COPY_PROBE_FACTOR}x the data"
  run_round big "$((SIZE_BYTES * COPY_PROBE_FACTOR))" "$((DATA_MIB_BASE * COPY_PROBE_FACTOR))"

  cp_small="${DUR["base:temp_pvc_bound"]}"
  cp_big="${DUR["big:temp_pvc_bound"]}"
  step "Copy-probe verdict"
  info "temp PVC bind at ${DATA_MIB_BASE}MiB  : ${cp_small}s"
  info "temp PVC bind at $((DATA_MIB_BASE * COPY_PROBE_FACTOR))MiB : ${cp_big}s"

  # Three guards, in order, and the first one is the important one: below a couple of seconds the
  # numbers are poll interval and API latency, not storage. A 0.4s -> 2.0s pair is a 5x "ratio"
  # that means nothing, and classifying it as a full copy would libel a perfectly good driver.
  COPY_CLASS="$(awk -v s="${cp_small}" -v b="${cp_big}" -v f="${COPY_PROBE_FACTOR}" '
    BEGIN{
      if (b < 3.0)                        { print "COW"; exit }        # absolute floor
      if (s <= 0)                          s = 0.1
      r = b / s
      d = b - s
      if (r >= f/2.5 && d >= 5.0)         { print "FULL_COPY_LIKELY"; exit }
      if (r <= 2.0)                       { print "COW"; exit }
      print "INDETERMINATE"
    }')"
  case "${COPY_CLASS}" in
  COW) ok "classified COW / zero-copy: provisioning is ~constant with the dataset size" ;;
  FULL_COPY_LIKELY) warn "classified FULL COPY LIKELY: provisioning grows with the dataset size" ;;
  *) warn "INDETERMINATE: the two times are neither flat nor proportional — re-run, or measure on the backend directly" ;;
  esac
fi

if [[ "${COPY_CLASS}" == "FULL_COPY_LIKELY" ]]; then
  finish_full_copy
fi
finish_ok
