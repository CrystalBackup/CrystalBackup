#!/usr/bin/env sh
# Every version a reader is told to INSTALL must be the version the chart ships.
#
# WHY THIS EXISTS. Three releases running — 0.6.5, 0.6.6 and 0.6.7 — shipped with the documentation
# telling readers to install the PREVIOUS release. Each time it was found by hand, after the tag, and
# each time the fix was the same 30 edits: 15 sites per language, across six pages, two of them
# outside the install section, which is why a per-page sweep kept missing them. A defect that recurs
# every release and costs 30 identical edits is a missing gate, not a series of oversights.
#
# WHAT IT CHECKS. `helm ... --version X`, `targetRevision: X`, `tag: "X"` and image references of the
# form `crystal-backup:X` under website/src/content/docs/, against appVersion in the chart. It reads
# the CHART as the authority rather than a constant here: the chart is what is published, and a
# number maintained in two places is a number that will disagree.
#
# WHAT IT DELIBERATELY DOES NOT CHECK. Prose. "on 0.6.6 a pre hook failed", "fixed in 0.6.2", the
# `0.6.5 -> 0.6.6` upgrade headings — those are facts about the past and rewriting them would be the
# mirror defect. Only the four command/manifest shapes above are load-bearing for a reader following
# instructions, and only those are enforced.
set -eu
cd "$(dirname "$0")/.."
want="$(awk -F'"' '/^appVersion:/{print $2}' charts/crystal-backup/Chart.yaml)"
[ -n "$want" ] || { echo "cannot read appVersion from charts/crystal-backup/Chart.yaml" >&2; exit 2; }

bad=0
# Each pattern captures a version; anything that is not $want is a finding.
found="$(grep -rnoE -- "--version[[:space:]]+[0-9]+\.[0-9]+\.[0-9]+|targetRevision:[[:space:]]*[0-9]+\.[0-9]+\.[0-9]+|tag:[[:space:]]*\"?[0-9]+\.[0-9]+\.[0-9]+\"?|crystal-backup:[0-9]+\.[0-9]+\.[0-9]+" \
  website/src/content/docs/ 2>/dev/null || true)"

echo "$found" | while IFS= read -r line; do
  [ -n "$line" ] || continue
  # strip a trailing quote first: `tag: "0.6.7"` ends in " and an anchored match misses it,
  # which made the gate's own first run flag a CORRECT line — the defect it exists to prevent.
  ver="$(printf '%s' "$line" | tr -d '"' | grep -oE '[0-9]+\.[0-9]+\.[0-9]+$')"
  [ "$ver" = "$want" ] && continue
  echo "STALE  $line   (chart appVersion is $want)"
done > /tmp/doc-pins.$$ || true

if [ -s /tmp/doc-pins.$$ ]; then
  cat /tmp/doc-pins.$$
  n="$(wc -l < /tmp/doc-pins.$$ | tr -d ' ')"
  rm -f /tmp/doc-pins.$$
  echo
  echo "$n install pin(s) name a version other than the chart's $want."
  echo "A reader following the documentation would install the wrong release."
  exit 1
fi
rm -f /tmp/doc-pins.$$
echo "doc install pins: all match the chart's appVersion ($want)"
