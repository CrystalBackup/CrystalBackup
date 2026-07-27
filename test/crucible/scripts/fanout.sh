#!/usr/bin/env bash
# Run the suite on N INDEPENDENT crucibles at once, then say which specs disagreed.
#
#   scripts/fanout.sh 3            # 3 lanes, whole suite
#   scripts/fanout.sh 3 m4         # 3 lanes, one label
#
# The point is not speed, it is EVIDENCE. A single red spec is ambiguous — flake or bug, and the
# usual answer is to re-run and hope. Three lanes answer it in one round: green-green-red is
# timing, red-red-red is a bug, and the report below says which of the two you are looking at
# instead of leaving it to be argued.
#
# This is strictly opt-in. `mise run up` / `mise run test` with no lane behave exactly as they
# always have, on the default workspace.
set -euo pipefail

CRUCIBLE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${CRUCIBLE_DIR}"

N="${1:-2}"
LABELS="${2:-}"

if ! [[ "${N}" =~ ^[1-9]$ ]]; then
  echo "fanout: N must be 1-9 (got '${N}')." >&2
  exit 1
fi

# ─── The part that costs money, said out loud before it is spent ──────────────
# Each lane is a full crucible: 3 masters + 3 workers + Ceph disks + its own bucket.
SERVERS_PER_LANE=6
echo "fanout: ${N} lanes × ${SERVERS_PER_LANE} servers = $((N * SERVERS_PER_LANE)) Hetzner instances."
echo "fanout: roughly €$(awk "BEGIN{printf \"%.2f\", 0.52 * ${N}}")/h while they run."
echo
echo "⚠  Hetzner projects ship with a default limit of 10 servers. Three lanes need 18."
echo "   Raise the project limit FIRST, or provisioning will fail partway through and leave"
echo "   half-built lanes behind — which still bill."
echo
if [[ "${CONFIRM:-}" != "yes" ]]; then
  echo "Re-run with CONFIRM=yes once the quota is sorted." >&2
  exit 1
fi

# The image under test. Exported into every lane so all three exercise the SAME operator —
# comparing lanes is only meaningful if they run identical code. Empty falls back to the chart
# defaults, which is right for a plain "does the suite pass" fanout and wrong for a hunt.
if [[ -n "${OPERATOR_IMAGE_DIGEST:-}" ]]; then
  echo "fanout: operator ${OPERATOR_IMAGE_DIGEST}"
fi
if [[ -n "${MOVER_IMAGE_DIGEST:-}" ]]; then
  echo "fanout: mover    ${MOVER_IMAGE_DIGEST}"
fi

lanes=()
for i in $(seq 1 "${N}"); do lanes+=("l${i}"); done

log_dir="${CRUCIBLE_DIR}/artifacts/fanout"
mkdir -p "${log_dir}"

# ─── Bring every lane up in parallel ──────────────────────────────────────────
# Provisioning is ~20 minutes of mostly waiting, so serialising N lanes would throw away the whole
# point. Each child gets its own CRUCIBLE_LANE and therefore its own workspace, artifacts and
# inventory; nothing is shared but the Hetzner account and the local disk.
# Create every lane's workspace SERIALLY first. tofu's local backend keeps one lock per state
# directory, and `workspace new` writes there — doing it concurrently is what produced "Error
# acquiring the state lock" on the first attempt at a parallel fanout.
echo "==> creating ${N} workspaces (serial — the state lock is shared)"
for lane in "${lanes[@]}"; do
  CRUCIBLE_LANE="${lane}" scripts/tofu-lane.sh init -input=false >/dev/null
done

echo "==> bringing up ${N} lanes"
pids=()
for lane in "${lanes[@]}"; do
  ( CRUCIBLE_LANE="${lane}" OPERATOR_IMAGE_DIGEST="${OPERATOR_IMAGE_DIGEST:-}" \
      MOVER_IMAGE_DIGEST="${MOVER_IMAGE_DIGEST:-}" mise run up \
    && CRUCIBLE_LANE="${lane}" mise run seed ) \
    > "${log_dir}/${lane}-up.log" 2>&1 &
  pids+=("$!")
done

up_failed=()
for idx in "${!pids[@]}"; do
  if ! wait "${pids[$idx]}"; then up_failed+=("${lanes[$idx]}"); fi
done

if (( ${#up_failed[@]} > 0 )); then
  echo "fanout: these lanes failed to come up: ${up_failed[*]}" >&2
  echo "fanout: their logs are under ${log_dir}/; the lanes that DID come up are still running" >&2
  echo "fanout: and still billing. Tear them down with: scripts/fanout-down.sh ${N}" >&2
  exit 1
fi

# ─── Run the suites ONE AT A TIME ─────────────────────────────────────────────
# Provisioning above parallelises well — it is mostly waiting on Hetzner. The SUITES do not:
# three concurrent Ginkgo runs, three Ansible-provisioned clusters and one Hetzner account
# contend badly enough that a previous attempt had every lane hit its suite timeout, one of them
# after only 3 specs of 46. A truncated run is worse than no run: it reports specs as failed when
# they were merely cut off.
#
# Sequential suites keep what lanes are actually FOR — three independent samples of the same code
# on genuinely separate infrastructure — and drop only the wall-clock saving, which was never the
# point.
#
# Each lane is destroyed the moment its suite ends, so lanes are not billed while waiting their
# turn: the last one would otherwise pay for the whole window doing nothing.
echo "==> running the suite in ${N} lanes, SEQUENTIALLY (see the comment: concurrent suites truncate)"
for lane in "${lanes[@]}"; do
  echo "--> lane ${lane}"
  CRUCIBLE_LANE="${lane}" CRUCIBLE_TIMEOUT="${CRUCIBLE_TIMEOUT:-150m}" \
    mise run test ${LABELS:+"${LABELS}"} > "${log_dir}/${lane}-test.log" 2>&1 || true

  # Residual exposure objects, read BEFORE teardown. This is the one measurement that depends on
  # neither the report parser nor the suite running to completion. A kubectl failure records "?",
  # never "0": an unreadable cluster and a clean cluster are different claims, and conflating
  # them is how a leak hides behind a flaky API server.
  if residual_raw="$(KUBECONFIG="${CRUCIBLE_DIR}/artifacts/lanes/${lane}/kubeconfig" \
    kubectl get volumesnapshotcontent -l app.kubernetes.io/managed-by=crystal-backup \
    --no-headers --ignore-not-found 2>/dev/null)"; then
    residual="$(printf '%s' "${residual_raw}" | grep -c . || true)"
  else
    residual="?"
  fi
  echo "${residual}" > "${log_dir}/${lane}-residual.txt"
  echo "--> lane ${lane}: ${residual} residual VolumeSnapshotContent(s); destroying it now"

  CRUCIBLE_LANE="${lane}" CONFIRM=yes mise run down > "${log_dir}/${lane}-down.log" 2>&1 \
    || echo "fanout: lane ${lane} FAILED to destroy — it is still billing" >&2
done

# ─── Compare ──────────────────────────────────────────────────────────────────
# One spec per line per lane, so a plain sort|uniq tells us who disagreed. The report renders
# "- ✅  <spec text>  _(duration)_", and the duration must be stripped or identical specs would
# never match across lanes.
echo
echo "════════════════════════════════════════════════════════════════════"
echo " fanout verdict — ${N} lanes${LABELS:+, label '${LABELS}'}"
echo "════════════════════════════════════════════════════════════════════"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
for lane in "${lanes[@]}"; do
  report="${CRUCIBLE_DIR}/artifacts/lanes/${lane}/crucible-report.md"
  if [[ ! -f "${report}" ]]; then
    echo "  ${lane}: NO REPORT (see ${log_dir}/${lane}-test.log)"
    continue
  fi
  # awk, NOT sed. BSD sed (which is what macOS gives a SCRIPT, whatever the interactive shell
  # aliases to) has no \| alternation in BREs, so the previous pattern matched nothing and three
  # real reports rendered as "0 passed, 0 failed" — a parser returning empty must never read as
  # consensus. awk behaves identically on both platforms.
  awk '
    /^- ✅/ { mark="✅" }
    /^- ❌/ { mark="❌" }
    /^- (✅|❌)/ {
      line = $0
      sub(/^- (✅|❌)[ \t]*/, "", line)     # drop the bullet and the mark
      sub(/[ \t]*_\(.*\)_$/, "", line)     # drop the trailing duration, or lanes never match
      sub(/[ \t]*—.*$/, "", line)          # drop any trailing " — reason", same rationale
      print mark "\t" line
    }' "${report}" > "${tmp}/${lane}"
  if [[ ! -s "${tmp}/${lane}" ]]; then
    echo "  ${lane}: report exists but NO spec lines parsed — the report format changed" >&2
  fi
  printf '  %-6s %s passed, %s failed\n' "${lane}" \
    "$(grep -c '^✅' "${tmp}/${lane}" || true)" "$(grep -c '^❌' "${tmp}/${lane}" || true)"
done

echo
echo "Specs that did NOT agree across lanes (these are your flakes):"
# Build the union of spec names, then check whether every lane returned the same mark for it.
# The whole comparison runs inside a command substitution, NOT a pipeline-fed while loop: a
# variable set in a pipeline stage dies with its subshell, which is how a previous version
# printed "(none — every lane agreed)" right AFTER listing two disagreements.
flakes="$(cat "${tmp}"/* 2>/dev/null | cut -f2- | sort -u | while IFS= read -r spec; do
  marks=""
  for lane in "${lanes[@]}"; do
    [[ -f "${tmp}/${lane}" ]] || continue
    m="$(grep -F -m1 "	${spec}" "${tmp}/${lane}" | cut -f1 || true)"
    marks="${marks}${m:-·}"
  done
  # Disagreement = at least one ✅ and at least one ❌ for the same spec.
  if [[ "${marks}" == *"✅"* && "${marks}" == *"❌"* ]]; then
    printf '  %s  %s\n' "${marks}" "${spec}"
  fi
done)"
if [[ -n "${flakes}" ]]; then
  printf '%s\n' "${flakes}"
else
  echo "  (none — every lane agreed on every spec)"
fi

echo
echo "Specs that failed in EVERY lane (these are your bugs):"
for lane in "${lanes[@]}"; do [[ -f "${tmp}/${lane}" ]] && grep '^❌' "${tmp}/${lane}" | cut -f2-; done \
  | sort | uniq -c | awk -v n="${N}" '$1 == n { $1=""; print "  " substr($0,2) }'

echo
echo "Residual exposure objects per lane (0 everywhere is the release gate):"
for lane in "${lanes[@]}"; do
  printf '  %-6s %s\n' "${lane}" "$(cat "${log_dir}/${lane}-residual.txt" 2>/dev/null || echo '?')"
done

echo
echo "Per-lane reports: artifacts/lanes/<lane>/crucible-report.md"
echo "Lanes are destroyed as each suite finishes. Verify nothing lingers:"
for i in $(seq 1 "${N}"); do echo "  CRUCIBLE_LANE=l${i} mise run status"; done
