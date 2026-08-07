#!/usr/bin/env bash
# Pré-bake de la démo meetup — À LANCER AVANT LE TALK (≈ 10-15 min, réseau requis).
# Monte un cluster kind + CSI snapshots + S3 in-cluster (SeaweedFS), installe l'opérateur
# depuis le chart publié, crée les secrets (KEK age + S3), la ClusterBackupLocation et
# l'appli témoin. À la fin, tout est prêt pour 10-demo.sh (100 % offline).
set -euo pipefail

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${DEMO_DIR}/../.." && pwd)"
CLUSTER="${CLUSTER:-crystal-demo}"
CHART="${CHART:-oci://ghcr.io/crystalbackup/charts/crystal-backup}"
CHART_VERSION="${CHART_VERSION:-}" # vide = dernière release publiée
KEK_DIR="${DEMO_DIR}/.kek"

step() { printf '\n\033[1;36m▶ %s\033[0m\n' "$*"; }

step "Vérification des prérequis"
# Outils gérés par mise (promotion/demo/mise.toml) — passer par `mise run prep`
# les met sur le PATH automatiquement.
for bin in kind kubectl helm age-keygen age restic; do
  command -v "$bin" >/dev/null || { echo "✗ '$bin' manquant → 'mise install' dans ${DEMO_DIR}, puis relancer via 'mise run prep'" >&2; exit 1; }
done
# Outils système (hors mise)
for bin in docker make git; do
  command -v "$bin" >/dev/null || { echo "✗ '$bin' manquant (outil système, non géré par mise)" >&2; exit 1; }
done
docker info >/dev/null 2>&1 || { echo "✗ docker ne répond pas" >&2; exit 1; }

step "Cluster kind '${CLUSTER}' (3 nœuds) + limites inotify"
make -C "$REPO_ROOT" setup-test-e2e KIND_CLUSTER="$CLUSTER"
# Si le cluster existait déjà, `kind create` est sauté et ne bascule PAS le contexte —
# on le force pour ne jamais dérouler la suite sur un autre cluster.
kubectl config use-context "kind-${CLUSTER}"

step "Infra de test : snapshot-controller + csi-hostpath + SeaweedFS S3 (cible test/e2e)"
make -C "$REPO_ROOT" install-test-e2e-infra KIND_CLUSTER="$CLUSTER"

step "Namespace crystal-backup-system (PSA baseline : les movers tournent en uid 0)"
kubectl create namespace crystal-backup-system --dry-run=client -o yaml | kubectl apply -f -
# Mêmes labels que ceux que le chart poserait s'il créait le namespace lui-même
# (charts/crystal-backup/values.yaml : enforce=baseline, audit/warn=restricted).
kubectl label namespace crystal-backup-system \
  pod-security.kubernetes.io/enforce=baseline \
  pod-security.kubernetes.io/enforce-version=latest \
  pod-security.kubernetes.io/audit=restricted \
  pod-security.kubernetes.io/warn=restricted --overwrite

step "Opérateur : chart publié (${CHART}${CHART_VERSION:+ @${CHART_VERSION}})"
helm upgrade --install crystal-backup "$CHART" \
  ${CHART_VERSION:+--version "$CHART_VERSION"} \
  -n crystal-backup-system \
  --set namespace.create=false \
  -f "$DEMO_DIR/values-demo.yaml" \
  --wait --timeout 6m

step "La KEK — générée ICI, par l'admin, jamais par l'opérateur ni le chart"
mkdir -p "$KEK_DIR"
if [[ ! -f "$KEK_DIR/kek.txt" ]]; then
  age-keygen -o "$KEK_DIR/kek.txt" 2>/dev/null
  echo "  KEK écrite dans ${KEK_DIR}/kek.txt (gitignorée — en vrai : coffre-fort HORS cluster)"
fi
# Le Secret ne contient QUE la ligne AGE-SECRET-KEY- : les releases ≤ 0.6.1 rejettent le
# fichier age-keygen complet, ses lignes de commentaires cassant le parse bech32
# (« malformed secret key: mixed case »). Corrigé à partir de 0.6.2 (internal/keys accepte
# le fichier entier), donc sur la release que cette démo installe — 0.6.3 — le grep est
# FACULTATIF. Il reste en place parce qu'il est correct partout et que la démo doit pouvoir
# se rejouer contre une release plus ancienne sans être retouchée.
# Le fichier complet reste dans .kek/ : il sert tel quel à `age -d -i` pour la démo.
kubectl -n crystal-backup-system create secret generic cluster-kek \
  --from-literal=identity="$(grep '^AGE-SECRET-KEY-' "$KEK_DIR/kek.txt")" \
  --dry-run=client -o yaml | kubectl apply -f -

step "Credentials S3 (ceux du SeaweedFS in-cluster)"
kubectl -n crystal-backup-system create secret generic dr-s3 \
  --from-literal=AWS_ACCESS_KEY_ID=crystalbackup \
  --from-literal=AWS_SECRET_ACCESS_KEY=crystalbackup-secret \
  --dry-run=client -o yaml | kubectl apply -f -

step "ClusterBackupLocation dr-primary → attente du 'restic init' (Job mover réel)"
kubectl apply -f "$DEMO_DIR/manifests/01-location.yaml"
kubectl wait clusterbackuplocation/dr-primary \
  --for=jsonpath='{.status.phase}'=Ready --timeout=5m
kubectl get backuprepository

step "Le schedule (décor du chemin normal) + l'appli témoin"
kubectl apply -f "$DEMO_DIR/manifests/02-schedule.yaml"
kubectl apply -f "$DEMO_DIR/manifests/00-seed-app.yaml"
kubectl -n demo wait --for=condition=Ready pod/writer --timeout=5m
kubectl -n demo exec writer -- cat /data/canary.txt

step "Pré-chargement des images sur les nœuds kind (zéro pull sur le réseau de la salle)"
# Chaque nœud tire lui-même les images via son containerd (`ctr`) : pas de transfert
# docker→kind (le `kind load docker-image` casse sur les digests multi-arch quand le
# docker hôte utilise le containerd image store). Busybox ET l'image mover — les Jobs
# du jour J peuvent être schedulés sur un nœud qui ne les a pas encore.
MOVER_IMG=$(kubectl -n crystal-backup-system get deploy -o yaml | grep -o 'mover-image=[^"]*' | head -1 | cut -d= -f2- || true)
for node in $(kind get nodes --name "$CLUSTER"); do
  echo "  $node : busybox"
  docker exec "$node" ctr --namespace=k8s.io images pull docker.io/library/busybox:1.36 >/dev/null
  if [[ -n "${MOVER_IMG}" ]]; then
    echo "  $node : mover"
    docker exec "$node" ctr --namespace=k8s.io images pull "$MOVER_IMG" >/dev/null
  fi
done
[[ -n "${MOVER_IMG}" ]] || echo "  (image mover introuvable dans les args du Deployment — vérifier à la main)"

cat <<EOF

✅ Prêt. Prochaines étapes :
   1. RÉPÉTER une fois en entier :   mise run demo
   2. remettre à zéro :              mise run reset
   3. le jour J, dérouler :          mise run demo   (aucun réseau requis)

   Cluster :   kind '${CLUSTER}' (kubectl config use-context kind-${CLUSTER})
   Teardown :  kind delete cluster --name ${CLUSTER}
EOF
