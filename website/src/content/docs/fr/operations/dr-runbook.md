---
title: Runbook DR
description: La checklist pour se remettre d'une perte totale du cluster, et l'exercice que vous devriez faire avant d'en avoir besoin.
sidebar:
  order: 1
sourceFile: src/content/docs/operations/dr-runbook.md
sourceHash: 69196da8409a3f1f0fa929b355c7d7ef46d40eeb
---

La version narrative, avec les explications, c'est
[Disaster recovery](/CrystalBackup/fr/docs/guides/disaster-recovery/). Ceci est la
checklist.

## Avant l'incident

L'entrée de la récupération, c'est **le bucket plus la KEK age que vous avez mise sous
séquestre en dehors du cluster**. Tout le reste est reconstructible. Donc :

- [ ] La KEK est sous séquestre **en dehors** de ce cluster, et quelqu'un d'autre que vous
      peut l'atteindre.
- [ ] Vous avez testé le séquestre. La récupérer pour la première fois pendant un incident,
      c'est comme ça qu'on découvre qu'elle avait été tournée.
- [ ] Vous avez consigné, quelque part qui survit au cluster : le bucket, le prefix, le
      `clusterID`, l'endpoint S3, et d'où viennent les credentials. Une faute de frappe dans
      `clusterID` au moment de la récupération vous pointe vers un repository vide plutôt
      que vers une erreur.
- [ ] Vous avez fait un exercice de restore dans le dernier trimestre, et il a marché.

Le dernier point n'est pas décoratif. `restic check` vérifie que le repository est lisible ;
il ne vérifie pas qu'un restore produit une application qui fonctionne. Rien d'autre qu'un
exercice ne le fait.

## L'exercice

Faites-le tous les trimestres, dans un namespace jetable, sur le cluster vivant. Cela coûte
une heure et c'est la seule chose qui vous dit que la DR fonctionne.

```bash
kubectl create namespace dr-drill
```

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterRestore
metadata:
  name: drill-20260730          # date it, so successive drills do not collide
spec:
  source:
    locationRef: { name: dr-primary }
    namespace: team-x
    time: latest
  target:
    namespace: dr-drill
    createNamespace: true
  mode: Overwrite
  confirmation: dr-drill
```

Vérifiez ensuite que les données sont bien les données — pas que la phase dit `Completed`.
Chronométrez, et notez le chiffre : c'est votre RTO mesuré pour un namespace, et c'est la
seule entrée honnête à un engagement de RTO.

```bash
kubectl delete namespace dr-drill
```

## L'incident : perte totale du cluster

**1 — Reconstruisez un cluster.** Kubernetes ≥ 1.30, un driver CSI avec support des
snapshots, et les CRDs de l'external-snapshotter.

**2 — Installez l'operator.**

```bash
helm install crystal-backup \
  oci://ghcr.io/crystalbackup/charts/crystal-backup \
  --version 0.6.2 \
  --namespace crystal-backup-system --create-namespace

kubectl -n crystal-backup-system rollout status deploy/crystal-backup
```

**3 — Restaurez les deux Secrets** depuis le séquestre.

```bash
kubectl -n crystal-backup-system create secret generic cluster-kek \
  --from-file=identity=cluster-kek.txt

kubectl -n crystal-backup-system create secret generic dr-s3 \
  --from-literal=AWS_ACCESS_KEY_ID=... \
  --from-literal=AWS_SECRET_ACCESS_KEY=...
```

**4 — Créez la location**, avec les **mêmes** `clusterID` et `prefix` qu'avant.

```bash
kubectl apply -f - <<'EOF'
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupLocation
metadata:
  name: dr-primary
spec:
  default: true
  clusterID: prod-eu-1
  s3:
    endpoint: https://s3.example.com
    bucket: crystal-backups
    prefix: dr
    forcePathStyle: true
    credentialsSecretRef: { name: dr-s3 }
  encryption:
    clusterKEKSecretRef: { name: cluster-kek }
EOF
```

**5 — Confirmez que la clé a bien été récupérée depuis le séquestre du bucket.**

```bash
kubectl get clusterbackuplocation dr-primary \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\n"}{end}'
```

Cherchez `DEKEscrowed` avec le reason `Recovered`, et la location qui atteint `Ready`.

**6 — Confirmez que le repository est là.**

```bash
kubectl get backuprepository dr-primary \
  -o jsonpath='{.status.initialized}{"\t"}{.status.snapshotCount}{"\t"}{.status.namespacesPresent}{"\n"}'
```

Un compte de snapshots à zéro ici signifie que vous pointez vers le mauvais chemin.
Arrêtez-vous et vérifiez `clusterID` et `prefix` avant de faire quoi que ce soit d'autre.

**7 — Énumérez ce que vous pouvez récupérer.** Aucun namespace n'existe encore, donc la
discovery n'a rien où projeter. Demandez directement à restic :

```bash
kubectl -n crystal-backup-system get secret cluster-kek \
  -o jsonpath='{.data.identity}' | base64 -d > /tmp/kek.txt
export RESTIC_PASSWORD=$(kubectl -n crystal-backup-system get secret crystal-dek-dr-primary \
  -o jsonpath='{.data.dek}' | base64 -d | age -d -i /tmp/kek.txt)
export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...

restic -r s3:https://s3.example.com/crystal-backups/dr/prod-eu-1 \
  snapshots --tag crystalbackup --json | jq -r '.[].tags[]' | grep '^namespace=' | sort -u
```

**8 — Restaurez les ressources cluster-scoped.** Dry-run d'abord.

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterRestore
metadata:
  name: dr-cluster-scoped
spec:
  source:
    locationRef: { name: dr-primary }
    namespace: team-x
    time: latest
  target: { namespace: team-x, createNamespace: true }
  mode: Overwrite
  resources: []
  volumes: []
  clusterResources:
    include:
      - "storage.k8s.io/StorageClass"
      - "snapshot.storage.k8s.io/VolumeSnapshotClass"
      - "apiextensions.k8s.io/CustomResourceDefinition"
  dryRun: true
  confirmation: team-x
```

```bash
kubectl get clusterrestore dr-cluster-scoped \
  -o jsonpath='{range .status.resources.entries[*]}{.outcome}{"\t"}{.kind}{"/"}{.name}{"\n"}{end}'
```

Lisez le plan. Puis supprimez-le, enlevez `dryRun`, et appliquez pour de vrai.

Excluez tout ce qu'un contrôleur GitOps possédera une fois que vous le rebrancherez —
restaurer ces objets-là se battra avec le contrôleur.

**9 — Restaurez chaque namespace, dans l'ordre des dépendances.** Les namespaces de la
couche stockage d'abord, puis les services de plateforme, puis les applications.

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterRestore
metadata:
  name: dr-team-x
spec:
  source:
    locationRef: { name: dr-primary }
    namespace: team-x
    time: latest
  target:
    namespace: team-x
    createNamespace: true
    storageClassMapping:
      fast-rbd: standard          # if the new cluster's classes differ
  mode: Overwrite
  confirmation: team-x
```

Lancez-en plusieurs en parallèle ; ce sont des objets indépendants. Le plafond de movers à
l'échelle du cluster borne de toute façon la concurrence réelle.

**10 — Redéployez les workloads.** Un `ClusterRestore` restaure aujourd'hui les objets
cluster-scoped et les données de volume. Ramenez les manifests des workloads par votre
mécanisme de livraison habituel — Helm, Argo, Flux — par-dessus les volumes restaurés.

**11 — Vérifiez chaque namespace.** Workloads qui tournent, PVCs bound, et un fichier dont
vous connaissez le contenu qui a bien le contenu que vous connaissez.

**12 — Rétablissez la protection.** Recréez le `ClusterBackupSchedule` et laissez un run
se terminer avant de déclarer l'incident clos. Un cluster que vous avez récupéré mais que
vous ne backupez pas n'est pas récupéré.

## Perte partielle : un namespace

Beaucoup plus court, parce que le cluster et l'operator vont bien.

```bash
kubectl -n team-x get backups
```

Si le namespace existe encore, un [`Restore`](/CrystalBackup/fr/docs/guides/restore/)
namespacé suffit, et le propriétaire du namespace peut le faire lui-même.

Si le namespace a disparu, utilisez `ClusterRestore` avec `createNamespace: true` — il
adresse le repository, pas le cluster, donc rien n'a besoin d'avoir survécu.

## Récupérer dans un autre cluster

Même procédure, avec deux changements :

- Utilisez le `clusterID` **d'origine** dans la location. C'est un segment de chemin, pas
  une affirmation sur l'endroit où vous tournez. Faire preuve de créativité ici vous pointe
  vers un repository vide.
- Attendez-vous à ce que `storageClassMapping` soit nécessaire.

## Ce que ce runbook ne couvre pas

**etcd et le control plane.** Crystal Backup récupère les ressources et les données
applicatives. L'état propre du cluster est un problème séparé, et il vous faut une réponse
séparée pour lui — qui devrait être écrite noir sur blanc à côté de cette page.
