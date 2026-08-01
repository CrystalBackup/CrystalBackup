#!/usr/bin/env bash
# SC2016: the single-quoted printf bodies below are markdown — the backticks are
# code spans for the reader, not command substitution waiting to happen.
# shellcheck disable=SC2016
#
# csi-report.sh — aggregate the per-StorageClass artifacts written by csi-probe.sh
# into one markdown matrix.
#
#   scripts/csi-report.sh [artifacts-dir]
#
# Reads every csi-probe-*.json (schema csi-probe/v1) and writes
# artifacts/csi-compat-report.md, also echoing it to stdout.
#
# The report deliberately separates three things a reader keeps conflating:
#   - the VERDICT (did CrystalBackup's exposure path work at all),
#   - the COST (COW clone vs full copy per backup — an estimate, never a measurement),
#   - the ANOMALIES (checksum mismatches, leaked objects, probe crashes), which
#     invalidate a verdict rather than qualifying it.
#
# Exit status is 0 even when drivers came back INCOMPATIBLE: a driver refusing the
# path is a RESULT. Non-zero is reserved for "the report itself cannot be trusted"
# — no artifacts at all, or a PROBE_ERROR / cleanup failure among them.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CRUCIBLE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
ARTIFACTS="${1:-${CRUCIBLE_DIR}/artifacts}"
REPORT="${ARTIFACTS}/csi-compat-report.md"

command -v jq >/dev/null || {
  printf 'csi-report: jq is required\n' >&2
  exit 2
}

shopt -s nullglob
FILES=("${ARTIFACTS}"/csi-probe-*.json)
shopt -u nullglob

if ((${#FILES[@]} == 0)); then
  printf 'csi-report: no csi-probe-*.json found in %s — nothing to aggregate\n' "${ARTIFACTS}" >&2
  exit 1
fi

# Every downstream view is derived from this one slurped array, so a malformed
# artifact fails once, here, instead of halfway through the markdown.
if ! ALL="$(jq -sc '.' "${FILES[@]}" 2>/dev/null)"; then
  printf 'csi-report: at least one artifact is not valid JSON; check %s\n' "${ARTIFACTS}" >&2
  exit 1
fi

# A verdict is only as good as its cleanup and its checksum: surface both as
# first-class columns rather than burying them in the JSON.
render() {
  jq -r '
    def dur(r; k): (.rounds[r].durations_seconds[k] // null)
                   | if . == null then "—" else "\(.)s" end;
    # "non mesuré" and "indéterminé" are NOT the same answer and must not collapse:
    # the first means the copy round never ran (or could not fit on this bench), the
    # second means it ran and the two timings did not separate cleanly. A SKIPPED
    # driver never reaches the point where the question applies at all.
    def cost:
      if .verdict == "SKIPPED" then "—"
      elif (.copy_probe.enabled // false) != true then "non mesuré"
      elif .copy_probe.classification == "COW" then "COW / zéro-copie"
      elif .copy_probe.classification == "FULL_COPY" then "copie complète probable"
      elif .copy_probe.classification == null then "non mesuré"
      else "indéterminé" end;
    def rank:
      {"COMPATIBLE":0, "COMPATIBLE_COPIE_COMPLETE":1, "SKIPPED":2,
       "INCOMPATIBLE":3, "PROBE_ERROR":4}[.verdict] // 9;

    sort_by(rank, .storageclass)
    | "| StorageClass | Provisioner | Exposer | VolumeSnapshotClass | Verdict | Snapshot prêt | PVC temp lié | Coût | Données |",
      "|---|---|---|---|---|---|---|---|---|",
      (.[] |
        "| `\(.storageclass)` | `\(.provisioner)` "
      + "| \(.exposer // "—") | \(.volume_snapshot_class // "_aucune_") "
      + "| **\(.verdict)** | \(dur("base";"snapshot_ready")) | \(dur("base";"temp_pvc_bound")) "
      + "| \(cost) "
      + "| \(if .rounds.base.checksum.match then "✅ vérifiées"
             elif (.verdict | startswith("COMPATIBLE")) then "⚠️ NON vérifiées"
             else "—" end) |")
  ' <<<"${ALL}"
}

summary() {
  jq -r '
    group_by(.verdict) | map("\(length) × \(.[0].verdict)") | join(" · ")
  ' <<<"${ALL}"
}

# Anomalies are the reason to distrust the table above, so they are printed even
# when the list is empty (an explicit "none" beats a missing section).
anomalies() {
  jq -r '
    map(select(
          .verdict == "PROBE_ERROR"
          or (.cleanup_failures // 0) > 0
          or .kept == true
          or (.verdict == "COMPATIBLE" and (.rounds.base.checksum.match | not))))
    | if length == 0 then "_Aucune._"
      else (.[] | "- `\(.storageclass)` — "
        + ([ (if .verdict == "PROBE_ERROR" then "sonde sans conclusion au stade \(.failed_step // "?") : \(.reason // "sans raison")" else empty end),
             (if (.cleanup_failures // 0) > 0 then "\(.cleanup_failures) objet(s) non nettoyé(s) — vérifier les snapshots côté stockage" else empty end),
             (if .kept == true then "exécutée avec --keep : un VolumeSnapshotContent reste en Retain" else empty end),
             (if .verdict == "COMPATIBLE" and (.rounds.base.checksum.match | not) then "verdict COMPATIBLE mais empreinte des données NON concordante — à ne pas publier tel quel" else empty end)
           ] | join(" ; "))) end
  ' <<<"${ALL}"
}

{
  printf '# Compatibilité CSI — résultats de sonde\n\n'
  printf 'Généré par `scripts/csi-report.sh` à partir de %d artefact(s) `csi-probe/v1`.\n\n' "${#FILES[@]}"
  printf 'Chaque ligne est le résultat d'"'"'une exécution réelle de `csi-probe.sh`, qui rejoue le\n'
  printf 'chemin d'"'"'exposition de CrystalBackup — VolumeSnapshot, re-bind statique du\n'
  printf 'VolumeSnapshotContent, PVC temporaire recréé **dans un autre namespace**, montage et\n'
  printf 'relecture des données. `SKIPPED` est un résultat attendu et légitime : le driver\n'
  printf 'n'"'"'expose aucune VolumeSnapshotClass, donc CrystalBackup marque le volume\n'
  printf '`Skipped/CSISnapshotUnsupported` et sauvegarde tout de même les manifests.\n\n'
  printf '**Bilan :** %s\n\n' "$(summary)"
  render
  printf '\n## Anomalies\n\n'
  anomalies
  printf '\n## Limites\n\n'
  printf 'La colonne « Coût » est une **estimation** dérivée du temps de provisionnement du PVC\n'
  printf 'temporaire à deux tailles de données, pas une mesure du stockage. Elle peut se tromper\n'
  printf 'dans les deux sens (une baie rapide imite le COW ; un backend throttlé imite la copie).\n'
  printf 'Les durées sous la seconde sont du bruit de sondage. Un verdict `COMPATIBLE` ne parle\n'
  printf 'que du chemin d'"'"'exposition : ni la restauration, ni la tenue en charge, ni les quotas\n'
  printf 'de snapshots ne sont couverts.\n'
} | tee "${REPORT}"

printf '\n\033[1;36m==> rapport écrit : %s\033[0m\n' "${REPORT}" >&2

# Anything that makes the table itself untrustworthy is worth a non-zero status so
# a caller in a pipeline stops and looks. Driver verdicts never are.
if jq -e 'map(select(.verdict == "PROBE_ERROR" or (.cleanup_failures // 0) > 0)) | length > 0' <<<"${ALL}" >/dev/null; then
  printf '\033[1;33m==> au moins une sonde n'"'"'a pas conclu ou n'"'"'a pas nettoyé — voir « Anomalies »\033[0m\n' >&2
  exit 3
fi
