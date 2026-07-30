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
if [[ -n "${SYNC_IMAGE_DIGEST:-}" ]]; then
  echo "fanout: sync     ${SYNC_IMAGE_DIGEST}"
fi

# LANE_PREFIX lets a rerun sidestep a poisoned lane name: a Hetzner-side volume lock from a
# wedged backend action blocks BOTH the destroy and any re-create under the same name, and the
# lock's lifetime is the provider's, not ours. Fresh names cost nothing; waiting costs hours.
LANE_PREFIX="${LANE_PREFIX:-l}"
lanes=()
for i in $(seq 1 "${N}"); do lanes+=("${LANE_PREFIX}${i}"); done

log_dir="${CRUCIBLE_DIR}/artifacts/fanout"
mkdir -p "${log_dir}"
# Purge the PREVIOUS run's measurements. A run that aborts before its suites (a lane failing to
# provision) must leave EMPTY slots, not last run's numbers: stale residual files were once read
# as this round's "0/0/0" during an incident review.
rm -f "${log_dir}"/*-residual.txt "${log_dir}"/*-test.log "${log_dir}"/*-up.log "${log_dir}"/*-down.log

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
      MOVER_IMAGE_DIGEST="${MOVER_IMAGE_DIGEST:-}" SYNC_IMAGE_DIGEST="${SYNC_IMAGE_DIGEST:-}" \
      mise run up \
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
  # 30m of headroom over run-tests.sh's own default, kept as that default moved from 120m to 180m
  # when M6's alert lane added an hour of unavoidable waiting on the rules' `for:` holds.
  CRUCIBLE_LANE="${lane}" CRUCIBLE_TIMEOUT="${CRUCIBLE_TIMEOUT:-210m}" \
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

  # ── Coverage, not just colour ───────────────────────────────────────────────
  # Counting ✅ and ❌ answers "did anything fail in this lane". It does NOT answer "did this lane
  # run the suite" — and seven lanes each passing the two specs a label selected would have come
  # out of the block below as "CLEAN — every lane green". Same mistake the per-run report used to
  # make, one level up, in the tool we reach for precisely when we no longer know what to believe.
  #
  # The report states both facts in machine-readable form; this reads them rather than re-deriving
  # them. The contract with test/crucible/tests/report_test.go is these two lines:
  #   **Coverage: <ran> of <total> checks ran (<pct>% of the suite)**
  #   **Result: <p> passed · <f> failed · [<n> skipped with a stated reason] ·
  #             [<n> never selected | <n> never reached]**
  # "never selected" = a filter left them out; "never reached" = the run was cut short. Different
  # events with different verdicts, so they are counted apart. A report whose Coverage line cannot
  # be read yields "? ?" and is treated as unreadable — never as full coverage.
  awk '
    /^\*\*Coverage: / {
      if (match($0, /Coverage: [0-9]+ of [0-9]+/)) {
        split(substr($0, RSTART, RLENGTH), a, " "); ran = a[2]; total = a[4]
      }
    }
    /^\*\*Result: / {
      if (match($0, /[0-9]+ never selected/)) { split(substr($0, RSTART, RLENGTH), b, " "); nsel = b[1] }
      if (match($0, /[0-9]+ never reached/))  { split(substr($0, RSTART, RLENGTH), c, " "); nrch = c[1] }
      if (match($0, /[0-9]+ skipped with a stated reason/)) {
        split(substr($0, RSTART, RLENGTH), d, " "); nskp = d[1]
      }
    }
    END {
      if (total == "") { print "? ? 0 0 0" } else { printf "%s %s %d %d %d\n", ran, total, nsel, nrch, nskp }
    }' "${report}" > "${tmp}/${lane}.cov"

  # `|| true`: read returns 1 at EOF, and under `set -e` that is an abort, not a value.
  c_ran='?' c_total='?' c_nsel=0 c_nrch=0 c_nskp=0
  read -r c_ran c_total c_nsel c_nrch c_nskp < "${tmp}/${lane}.cov" || true
  if [[ "${c_total}" == "?" ]]; then
    cov_note="coverage UNREADABLE — the report format changed"
  elif (( c_nrch > 0 )); then
    cov_note="${c_ran}/${c_total} checks — TRUNCATED, ${c_nrch} never reached"
  elif (( c_nsel > 0 )); then
    cov_note="${c_ran}/${c_total} checks — FILTERED, ${c_nsel} never selected"
  elif (( c_nskp > 0 )); then
    cov_note="${c_ran}/${c_total} checks — whole suite (${c_nskp} skipped with a reason)"
  else
    cov_note="${c_ran}/${c_total} checks — whole suite"
  fi
  printf '  %-6s %s passed, %s failed  ·  %s\n' "${lane}" \
    "$(grep -c '^✅' "${tmp}/${lane}" || true)" "$(grep -c '^❌' "${tmp}/${lane}" || true)" \
    "${cov_note}"
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
# `|| true` on the grep, and an `if` rather than `[[ … ]] &&`, because BOTH return 1 in the good
# case — no lane has a ❌ — and `set -euo pipefail` at the top of this file turned that into an
# abort. The effect was the exact inversion of what this script promises: a campaign where every
# lane was GREEN died right here, printed neither the residual measurement nor the verdict, and
# exited 1; a campaign WITH failures ran to the end and reported properly. The one outcome the
# tool exists to certify was the one it could not report. Anything below this line is unreachable
# on a green run until this stays fixed — including the coverage verdict.
common_failures="$(
  for lane in "${lanes[@]}"; do
    if [[ -f "${tmp}/${lane}" ]]; then grep '^❌' "${tmp}/${lane}" | cut -f2- || true; fi
  done | sort | uniq -c | awk -v n="${N}" '$1 == n { $1=""; print "  " substr($0,2) }'
)"
if [[ -n "${common_failures}" ]]; then
  printf '%s\n' "${common_failures}"
else
  # Said out loud, like the flake block above it. A bare heading with nothing under it is
  # ambiguous in exactly the wrong direction: no common failures, or a parser that broke?
  echo "  (none — no spec failed in all ${N} lanes)"
fi

echo
echo "Coverage per lane (the whole suite everywhere is what makes this a release gate):"
# Printed as its own measurement, next to the residuals, because it is one: "every lane agreed"
# is a claim about the specs that RAN, and it is worth exactly as much as the number of specs
# that ran. Lanes disagreeing on the TOTAL is worse still — it means they did not execute the
# same suite, which invalidates the comparison this whole tool exists to make.
cov_totals=""
for lane in "${lanes[@]}"; do
  if [[ -f "${tmp}/${lane}.cov" ]]; then
    c_ran='?' c_total='?'
    read -r c_ran c_total _ _ _ < "${tmp}/${lane}.cov" || true
    printf '  %-6s %s of %s checks ran\n' "${lane}" "${c_ran}" "${c_total}"
    # Only READABLE totals feed the drift check. A lane with no report is already reported as
    # such below; letting its "?" also trip "lanes disagree on the suite size" would explain one
    # problem twice and send the reader looking for a second one that does not exist.
    [[ "${c_total}" != "?" ]] && cov_totals="${cov_totals}${c_total}"$'\n'
  else
    printf '  %-6s ? (no report)\n' "${lane}"
  fi
done
cov_drift=0
if [[ "$(printf '%s' "${cov_totals}" | sort -u | grep -c . || true)" -gt 1 ]]; then
  cov_drift=1
  echo "  ⚠  lanes disagree on the suite SIZE — they did not run the same code, so the" >&2
  echo "     agreement analysis above is meaningless until that is explained." >&2
fi

echo
echo "Residual exposure objects per lane (0 everywhere is the release gate):"
for lane in "${lanes[@]}"; do
  printf '  %-6s %s\n' "${lane}" "$(cat "${log_dir}/${lane}-residual.txt" 2>/dev/null || echo '?')"
done

echo
echo "Per-lane reports: artifacts/lanes/<lane>/crucible-report.md"
echo "Lanes are destroyed as each suite finishes. Verify nothing lingers:"
for i in $(seq 1 "${N}"); do echo "  CRUCIBLE_LANE=${LANE_PREFIX}${i} mise run status"; done

# ─── Machine-readable verdict ─────────────────────────────────────────────────
# Exit 0 ONLY when every lane produced a parsed report with zero ❌ AND measured zero residuals
# AND actually ran what it was asked to run. Anything else — a missing report, an unparsed one, a
# failed spec, a residual, an unreadable residual ('?'), an unreadable or truncated coverage —
# exits 1, so an automation chaining rounds stops on the first dirty one instead of spending the
# next round's money on a gate that is already red.
#
# Coverage splits the outcome three ways rather than two, and the middle one is new:
#
#   TRUNCATED  a lane reports checks "never reached" — the suite was cut off. Hard NOT clean,
#              whatever the labels were: this file's own header says a truncated run is worse
#              than no run, because it reports specs as failed when they were merely cut off.
#   FILTERED   a lane reports checks "never selected". Deliberate when `scripts/fanout.sh N m4`
#              asked for a label — that is the documented flake hunt, and failing it would break
#              the loop the tool exists for. So it still exits 0, but the word CLEAN is withheld
#              and the verdict says in full what it settles and what it does not. A filter with
#              NO label passed is nobody's intent, so that one is hard NOT clean.
#   FULL       every check selected. The only shape entitled to "CLEAN".
#
# The exit code answers "did something go wrong"; the WORDS answer "can I ship this". Conflating
# the two is how "every lane green" came to be printed over two specs out of eighty-three.
verdict=0
partial=0
cov_summary=""
cov_skipped=0
if (( cov_drift != 0 )); then
  echo "verdict: lanes disagree on the suite size — they did not run the same suite — NOT clean" >&2
  verdict=1
fi
for lane in "${lanes[@]}"; do
  if [[ ! -s "${tmp}/${lane}" ]]; then
    echo "verdict: ${lane} has no parsed specs — NOT clean" >&2; verdict=1; continue
  fi
  if grep -q '^❌' "${tmp}/${lane}"; then
    echo "verdict: ${lane} has failed specs — NOT clean" >&2; verdict=1
  fi
  res="$(cat "${log_dir}/${lane}-residual.txt" 2>/dev/null || echo '?')"
  if [[ "${res}" != "0" ]]; then
    echo "verdict: ${lane} residual='${res}' — NOT clean" >&2; verdict=1
  fi

  if [[ ! -f "${tmp}/${lane}.cov" ]]; then
    echo "verdict: ${lane} has no coverage line — NOT clean" >&2; verdict=1; continue
  fi
  c_ran='?' c_total='?' c_nsel=0 c_nrch=0 c_nskp=0
  read -r c_ran c_total c_nsel c_nrch c_nskp < "${tmp}/${lane}.cov" || true
  cov_summary="${c_ran} of ${c_total}"
  (( c_nskp > cov_skipped )) && cov_skipped="${c_nskp}"
  if [[ "${c_total}" == "?" ]]; then
    echo "verdict: ${lane} coverage unreadable — NOT clean (an unparsed report is not consensus)" >&2
    verdict=1
  elif (( c_nrch > 0 )); then
    echo "verdict: ${lane} ran only ${c_ran} of ${c_total} checks, ${c_nrch} never reached (truncated) — NOT clean" >&2
    verdict=1
  elif (( c_nsel > 0 )); then
    if [[ -z "${LABELS}" ]]; then
      echo "verdict: ${lane} left ${c_nsel} of ${c_total} checks unselected with no label requested — NOT clean" >&2
      verdict=1
    else
      partial=1
    fi
  elif (( 10 * c_nskp > c_total )); then
    # The same 10% tolerance the per-run report applies (fullRunSkipTolerance in
    # tests/report_test.go). One rule, stated once, enforced in both places: a suite that
    # abstains on more than a tenth of itself is green for what it covered, not for the suite.
    echo "verdict: ${lane} selected the whole suite but ${c_nskp} of ${c_total} checks skipped themselves" >&2
    partial=1
  fi
done

if (( verdict != 0 )); then
  :
elif (( partial != 0 )); then
  if [[ -n "${LABELS}" ]]; then
    echo "verdict: PARTIAL — every lane green and residual-free, but each ran only ${cov_summary} checks (label '${LABELS}')."
    echo "verdict: that settles the specs it ran across ${N} independent lanes; it is NOT a release gate."
    echo "verdict: re-run without a label — 'scripts/fanout.sh ${N}' — before shipping on this evidence."
  else
    echo "verdict: PARTIAL — every lane green and residual-free, but too much of the suite skipped itself"
    echo "verdict: (${cov_summary} checks ran). Green for what it covered, not for the suite; resolve what"
    echo "verdict: those checks are waiting on before shipping on this evidence."
  fi
else
  # "the whole suite" means every check was SELECTED. Say the skips out loud rather than let
  # "ran the whole suite (78 of 81 checks)" contradict itself in its own sentence.
  if (( cov_skipped > 0 )); then
    echo "verdict: CLEAN — every lane selected the whole suite, all green, zero residuals everywhere"
    echo "verdict: (${cov_summary} checks ran; ${cov_skipped} skipped with a stated reason — see the per-lane reports)"
  else
    echo "verdict: CLEAN — every lane ran the whole suite (${cov_summary} checks), all green, zero residuals everywhere"
  fi
fi
exit "${verdict}"
