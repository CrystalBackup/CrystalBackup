#!/usr/bin/env bash
# LA DÉMO (~12 min, 100 % offline) — à dérouler après 00-prep.sh (et une répétition !).
# Chaque commande s'affiche d'abord ; Entrée l'exécute. DEMO_AUTO=1 pour un déroulé
# non interactif (enregistrement asciinema du plan B).
#
# Le runbook minuté, les phrases à dire et le plan B sont dans README.md.
set -uo pipefail

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$DEMO_DIR"   # tous les chemins de manifests sont relatifs à ce dossier
KEK="${DEMO_DIR}/.kek/kek.txt"
RUN="demo-$(date +%Y%m%d-%H%M%S)"   # horodaté : rejouable sans RunNameCollision
S3_LOCAL="s3:http://localhost:8333/e2e-dr/crystal/meetup-demo"
CLUSTER="${CLUSTER:-crystal-demo}"

# Garde-fou : ne jamais dérouler la démo sur un autre cluster que le kind de démo.
if [[ "$(kubectl config current-context)" != "kind-${CLUSTER}" ]]; then
  echo "✗ contexte kubectl = '$(kubectl config current-context)', attendu 'kind-${CLUSTER}'" >&2
  echo "  → kubectl config use-context kind-${CLUSTER}" >&2
  exit 1
fi

C_CMD=$'\033[1;36m'; C_TIT=$'\033[1;33m'; C_OFF=$'\033[0m'

banner() { printf '\n%s━━ %s ━━%s\n' "$C_TIT" "$*" "$C_OFF"; }
pe() { # print & execute (Entrée pour lancer)
  printf '\n%s$ %s%s\n' "$C_CMD" "$*" "$C_OFF"
  [[ "${DEMO_AUTO:-}" == 1 ]] || read -r
  eval "$*"
}
pe_fail() { pe "$@" || true; } # pour les commandes dont l'ÉCHEC est la démo

wait_phase() { # wait_phase <kind/name> [-n ns] — affiche la phase jusqu'à un état terminal
  local args=("$@") tries=0
  while :; do
    local p
    p=$(kubectl get "${args[@]}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    printf '\r  phase: %-22s' "${p:-…}"
    case "$p" in Completed|PartiallyFailed|Failed|Ready) printf '\n'; break ;; esac
    # ~5 min max : une étape qui coince doit être SAUTABLE (plan B), pas un hang muet
    if (( ++tries > 150 )); then printf '\n  ⚠ timeout — kubectl describe %s, ou passer à la suite\n' "${args[*]}"; break; fi
    sleep 2
  done
}

trap 'kill $(jobs -p) 2>/dev/null' EXIT

# ────────────────────────────────────────────────────────────────────────────────
banner "1/6 — L'état des lieux (l'opérateur, le dépôt, l'appli du tenant)"
pe kubectl -n crystal-backup-system get pods
pe kubectl get clusterbackuplocation,backuprepository
pe kubectl -n demo exec writer -- cat /data/canary.txt
SEED_SHA=$(kubectl -n demo exec writer -- cat /data/MANIFEST.sha256)
echo "  (checksum témoin gardé de côté : ${SEED_SHA%% *})"

banner "2/6 — Un backup, maintenant (la cascade en vrai)"
pe "sed \"s/__RUN__/$RUN/\" manifests/03-backup-now.template.yaml | kubectl apply -f -"
echo "  → fan-out : ClusterBackup → Backup (ns demo) → mover Jobs (1/PVC) → restic"
wait_phase clusterbackup/"$RUN"
pe kubectl -n demo get backups
pe "kubectl -n demo get backup $RUN -o jsonpath='{range .status.volumes[*]}{.pvc}{\"\\t\"}{.phase}{\"\\t\"}{.reason}{\"\\n\"}{end}'"
echo "  → 'data' Completed ; 'legacy' Skipped/CSISnapshotUnsupported — dit, pas caché."

banner "3/6 — Le drame"
pe kubectl delete namespace demo
pe kubectl -n demo get backups
echo "  → plus de namespace, plus de CRs. Il reste... le dépôt. (= la source de vérité)"

banner "4/6 — ClusterRestore : une coordonnée de dépôt, pas un objet survivant"
pe "sed \"s/__RUN__/$RUN/\" manifests/04-clusterrestore.template.yaml | kubectl apply -f -"
wait_phase clusterrestore/"$RUN"-restore
echo "  → volumes restaurés + namespace recréé. Les manifests de workload du namespace,"
echo "    eux, reviennent par un Restore namespacé ou un re-apply — dit aussi (RESTORE.md) :"
pe kubectl apply -f manifests/00-seed-app.yaml
pe kubectl -n demo wait --for=condition=Ready pod/writer --timeout=3m
pe "kubectl -n demo exec writer -- sh -c 'sha256sum -c /data/MANIFEST.sha256 && cat /data/canary.txt'"
echo "  → byte pour byte. (le seed est idempotent : il n'écrase jamais un canari existant)"

banner "5/6 — La réversibilité : restic UPSTREAM, zéro composant Crystal Backup"
kubectl -n crystalbackup-e2e port-forward svc/seaweedfs 8333:8333 >/dev/null 2>&1 &
sleep 2
pe "export RESTIC_PASSWORD=\$(kubectl -n crystal-backup-system get secret crystal-dek-dr-primary -o jsonpath='{.data.dek}' | base64 -d | age -d -i $KEK)"
export AWS_ACCESS_KEY_ID=crystalbackup AWS_SECRET_ACCESS_KEY=crystalbackup-secret
pe "restic -r $S3_LOCAL snapshots"
pe "restic -r $S3_LOCAL dump --tag pvc=data latest /data/demo/data/canary.txt | sha256sum"
echo "  → à comparer au checksum témoin de l'étape 1 : ${SEED_SHA%% *}"
echo "  → même checksum. Le jour où l'outil vous déçoit, vous partez avec vos données."

banner "6/6 — Et si on trichait ?"
pe_fail kubectl apply -f manifests/05-forged-backup.yaml
echo "  → un Backup utilisateur ne référencera JAMAIS le dépôt DR partagé (VAP, R2)."
pe_fail kubectl apply -f manifests/06-restore-bad-confirmation.yaml
echo "  → confirmation ≠ namespace : refusé dans l'API server. Et remarquez : ce Restore"
echo "    n'a NI champ location NI target-namespace. Le champ pour tricher n'existe pas."

banner "Fin — merci ! (./99-reset.sh pour rejouer)"
