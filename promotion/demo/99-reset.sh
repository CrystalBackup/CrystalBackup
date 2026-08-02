#!/usr/bin/env bash
# Remise à zéro entre deux répétitions (garde le cluster, l'opérateur et la location).
# Les vieux runs restent dans le dépôt : la discovery les re-projettera dans `demo` —
# c'est voulu (« le dépôt est la source de vérité »), et les noms horodatés de 10-demo.sh
# évitent toute RunNameCollision au run suivant.
set -euo pipefail
DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLUSTER="${CLUSTER:-crystal-demo}"

if [[ "$(kubectl config current-context)" != "kind-${CLUSTER}" ]]; then
  echo "✗ contexte kubectl = '$(kubectl config current-context)', attendu 'kind-${CLUSTER}'" >&2
  exit 1
fi

echo "▶ Purge des objets de démo (CRs de run, restores, tentatives de triche)"
kubectl delete clusterrestore --all --ignore-not-found --wait=false
kubectl get clusterbackup -o name 2>/dev/null | grep '/demo-' | xargs -r kubectl delete --wait=false
kubectl delete namespace demo --ignore-not-found

echo "▶ Re-seed de l'appli témoin"
kubectl apply -f "$DEMO_DIR/manifests/00-seed-app.yaml"
kubectl -n demo wait --for=condition=Ready pod/writer --timeout=5m
kubectl -n demo exec writer -- cat /data/canary.txt

echo "✅ Prêt à rejouer : ./10-demo.sh"
