---
title: Runbook DR
description: La checklist pour se remettre d'une perte totale du cluster, et l'exercice que vous devriez faire avant d'en avoir besoin.
sidebar:
  order: 1
sourceFile: src/content/docs/operations/dr-runbook.md
sourceHash: 7396427a1e35eb5acdad3a42fa5ed2c69fce1936
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

**2 — Créez et labellisez le namespace, puis installez l'operator.**

Pas de `--create-namespace`. Helm crée le namespace *après* le rendu : il ne porte donc aucun
label Pod Security — or `crystal-backup-system` doit imposer `baseline`, parce que les data movers
tournent en uid 0 avec `DAC_OVERRIDE` pour préserver les propriétaires de fichiers à la
restauration, ce que `restricted` interdit. Un namespace sans labels s'installe proprement, puis
refuse le premier mover à l'admission. **En pleine restauration de secours, c'est le pire moment
possible pour le découvrir** — d'où ces deux commandes en tête plutôt qu'en note de bas de page.

```bash
kubectl create namespace crystal-backup-system

kubectl label namespace crystal-backup-system \
  pod-security.kubernetes.io/enforce=baseline \
  pod-security.kubernetes.io/enforce-version=latest \
  pod-security.kubernetes.io/audit=restricted \
  pod-security.kubernetes.io/warn=restricted --overwrite

helm install crystal-backup \
  oci://ghcr.io/crystalbackup/charts/crystal-backup \
  --version 0.6.4 \
  --namespace crystal-backup-system

kubectl -n crystal-backup-system rollout status deploy/crystal-backup
```

L'operator le vérifie lui-même au démarrage et émet un Event `Warning`
`PodSecurityPostureWrong` sur son propre namespace si la posture est mauvaise : une erreur ici est
donc visible dans `kubectl -n crystal-backup-system get events` avant toute tentative de
sauvegarde.

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

## Perte partielle : le namespace de l'operator

Le namespace a disparu, et `ClusterBackupLocation`, `ClusterBackupSchedule` et
`BackupRepository` sont toujours là — ils sont cluster-scoped, donc supprimer un namespace ne
les emporte pas. Ce que vous avez perdu, c'est l'operator, la cluster KEK et les DEK wrappées.

**Ce cas est plus dangereux que la perte totale du cluster, et la raison est l'ordre.** Sur une
perte totale, vous créez le namespace, vous restaurez les deux Secrets, et ensuite seulement
vous créez la location : l'operator ne voit donc jamais une location dont il ne peut pas
résoudre les credentials. Ici la location existe déjà, ce qui veut dire que l'operator la
réconcilie à l'instant où il démarre — avec ce que vous aurez réussi à remettre en place à ce
moment-là.

En `0.6.3` et avant, cet ordre décidait de votre sort. Si la cluster KEK arrivait avant les
credentials S3, la passe d'escrow ne pouvait pas lire les credentials, ne pouvait donc pas
demander au bucket s'il existait une DEK wrappée récupérable, et ne bloquait pas le repository.
Le repository était provisionné, une DEK **neuve** était générée par-dessus celle qui était
séquestrée, et la location affichait `Ready` pendant que chaque mover échouait sur
`wrong password or no key found` contre un repository plein de snapshots. La `0.6.4` referme
cela : tout état d'escrow que l'operator ne peut pas prouver sûr bloque désormais le
provisionnement et rapporte `Ready=False` avec la raison `DEKEscrowUnresolved`, et la condition
`DEKEscrowed` dit de quel cas il s'agit.

Ne comptez pas sur la version. **Créez et labellisez le namespace, restaurez les deux Secrets,
et ensuite seulement laissez l'operator démarrer.** Dans cet ordre :

```bash
kubectl create namespace crystal-backup-system

kubectl label namespace crystal-backup-system \
  pod-security.kubernetes.io/enforce=baseline \
  pod-security.kubernetes.io/enforce-version=latest \
  pod-security.kubernetes.io/audit=restricted \
  pod-security.kubernetes.io/warn=restricted --overwrite

# BOTH Secrets, before the operator exists.
kubectl -n crystal-backup-system create secret generic cluster-kek \
  --from-file=identity=cluster-kek.txt

kubectl -n crystal-backup-system create secret generic dr-s3 \
  --from-literal=AWS_ACCESS_KEY_ID=... \
  --from-literal=AWS_SECRET_ACCESS_KEY=...

# Only now.
helm install crystal-backup \
  oci://ghcr.io/crystalbackup/charts/crystal-backup \
  --version 0.6.4 \
  --namespace crystal-backup-system
```

Si c'est un contrôleur GitOps qui va réinstaller l'operator pour vous, et que vous ne maîtrisez
pas le moment, suspendez-le jusqu'à ce que les deux Secrets soient en place. Une Application
Argo CD ou un `HelmRelease` Flux qui reprend de lui-même à son propre rythme est exactement
l'acteur qui démarrera l'operator deux minutes avant que vous ayez fini de restaurer les
credentials.

Puis regardez ce que l'escrow a conclu, avant toute autre chose :

```bash
kubectl get clusterbackuplocation dr-primary \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\n"}{end}'
```

`DEKEscrowed` avec la raison `Recovered`, c'est le bon dénouement : la DEK wrappée est revenue
du bucket. `Escrowed` va aussi — cela veut dire qu'une DEK en cluster était déjà là et que la
copie du bucket lui correspond. Pour tout le reste, lisez le message avant de toucher à quoi que
ce soit : `EscrowConflict`, `EscrowUnreadableUnderKEK` et `ClusterDEKUnreadableUnderKEK` sont
trois urgences différentes et une seule d'entre elles concerne votre KEK.

**Ne supprimez pas la `ClusterBackupLocation` pour repartir propre.** Elle porte
`crystalbackup.io/location`, et la supprimer alors qu'aucun operator ne tourne la laisse en
`Terminating` sans que rien ne puisse libérer le finalizer. Si c'est déjà fait, réinstallez
l'operator et il terminera la suppression —
[Retirer Crystal Backup](/CrystalBackup/fr/docs/operations/uninstall/) a le reste.

## Adopter à la main la DEK séquestrée

L'operator récupère tout seul la DEK wrappée depuis le bucket, et c'est ce chemin automatique
que vous voulez. Ceci est pour quand il ne peut pas s'exécuter : le bucket est joignable depuis
votre poste mais pas depuis le cluster, les credentials ne sont pas ceux que la location nomme,
ou vous voulez établir que la KEK trouvée en séquestre est la bonne *avant* de laisser une
réconciliation toucher à quoi que ce soit.

L'objet de séquestre est un frère du prefix du repository, jamais dedans :

```
<prefix>/<clusterID>.crystal-meta/wrapped-dek.age
```

Avec un prefix vide, cela dégénère en `<clusterID>.crystal-meta/wrapped-dek.age`. Cette clé fait
partie du contrat de DR et ne change pas d'une version à l'autre.

**1 — Descendez l'operator à 0 replica.** Ce n'est pas optionnel. Pendant qu'il tourne, le
chemin du repository peut générer une DEK avant que la passe d'escrow ait l'occasion d'en
adopter une, et c'est une course que vous perdez.

```bash
kubectl -n crystal-backup-system scale deploy/crystal-backup --replicas=0
```

**2 — Récupérez l'objet** avec n'importe quel client S3.

```bash
aws s3 cp s3://crystal-backups/dr/prod-eu-1.crystal-meta/wrapped-dek.age . \
  --endpoint-url https://s3.example.com
```

**3 — Prouvez que votre KEK l'ouvre.** Le fichier est du ciphertext age et ne vaut rien sans la
cluster KEK, ce qui est précisément pourquoi le séquestrer dans le bucket est sans risque.
Tester une KEK candidate contre lui tient en une commande, et c'est le test le moins cher de
tout ce runbook :

```bash
age -d -i cluster-kek.txt wrapped-dek.age > /dev/null
```

Un code de sortie 0 veut dire que cette KEK est bien celle qui a wrappé la clé de ce repository.
Un échec veut dire qu'elle ne l'est pas, et aucune adoption n'y changera rien — allez chercher la
bonne KEK.

**4 — Créez le Secret.** Un Secret par location, nommé d'après la location, avec les octets
wrappés sous la clé de données `dek` :

```bash
kubectl -n crystal-backup-system create secret generic crystal-dek-dr-primary \
  --from-file=dek=wrapped-dek.age

# Cosmetic, but it makes the Secret discoverable alongside the operator's own.
kubectl -n crystal-backup-system label secret crystal-dek-dr-primary \
  app.kubernetes.io/managed-by=crystal-backup app.kubernetes.io/name=crystal-backup
```

**5 — Remontez l'operator** et vérifiez que `DEKEscrowed` passe à True.

```bash
kubectl -n crystal-backup-system scale deploy/crystal-backup --replicas=1
```

Adopter le mauvais fichier n'est pas un moyen de perdre des données : l'operator valide que les
octets se déwrappent sous la KEK courante avant de les accepter, si bien qu'un blob corrompu ou
étranger est refusé plutôt que pris silencieusement, et un Secret de DEK qui existe déjà avec
des octets différents n'est jamais écrasé — une DEK pour la vie d'une location, et c'est celle
qui existe qui gagne.

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
