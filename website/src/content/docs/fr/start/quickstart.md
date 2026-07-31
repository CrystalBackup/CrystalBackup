---
title: Démarrage rapide
description: Un premier backup du plan cluster et un premier restore, sous forme de commandes exactes accompagnées de la sortie que chacune doit produire.
sourceFile: src/content/docs/start/quickstart.md
sourceHash: d45723165d3c56b6c52bcde8c08f3b30aa230668
---

<!-- UNVERIFIED: à exécuter au lot I -->

:::caution[Pas encore exécuté de bout en bout]
Cette page a été écrite d'après l'API livrée, mais elle n'a **pas** encore été rejouée
telle quelle sur une infrastructure réelle. Elle le sera avant publication. D'ici là,
traitez les sorties attendues comme une intention et non comme un fait observé.
:::

Ce parcours prend un backup du plan cluster pour un namespace, puis y restaure un fichier.
Il suppose que l'operator est installé, que le Secret `cluster-kek` existe et que vous êtes
cluster-admin.

Partout : remplacez `s3.example.com`, `crystal-backups` et `prod-eu-1` par les vôtres.

## 0. Un namespace avec quelque chose dedans

```bash
kubectl create namespace demo
kubectl label namespace demo crystalbackup.io/protect=true
```

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
  namespace: demo
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: writer
  namespace: demo
spec:
  containers:
    - name: sh
      image: busybox:1.36
      command: ["sh", "-c", "echo hello-from-the-backup > /data/canary.txt && sleep infinity"]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: data
EOF
```

Attendu :

```
namespace/demo created
namespace/demo labeled
persistentvolumeclaim/data created
pod/writer created
```

Attendez le pod, puis vérifiez que le canari est bien là :

```bash
kubectl -n demo wait --for=condition=Ready pod/writer --timeout=120s
kubectl -n demo exec writer -- cat /data/canary.txt
```

Attendu :

```
pod/writer condition met
hello-from-the-backup
```

## 1. Les credentials du stockage objet

```bash
kubectl -n crystal-backup-system create secret generic dr-s3 \
  --from-literal=AWS_ACCESS_KEY_ID="$AWS_ACCESS_KEY_ID" \
  --from-literal=AWS_SECRET_ACCESS_KEY="$AWS_SECRET_ACCESS_KEY"
```

Attendu :

```
secret/dr-s3 created
```

## 2. La backup location du cluster

`clusterID`, `s3.endpoint`, `s3.bucket`, `s3.prefix` et `mode` sont **immuables après la
création** — ensemble, ils composent le chemin du repository, si bien que modifier l'un
d'eux re-pointerait silencieusement la location vers un autre repository. Choisissez-les
correctement dès maintenant ; pour en changer un plus tard, créez une nouvelle location.

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupLocation
metadata:
  name: dr-primary
spec:
  default: true
  mode: Standard
  clusterID: prod-eu-1
  s3:
    endpoint: https://s3.example.com
    bucket: crystal-backups
    prefix: dr
    forcePathStyle: true
    credentialsSecretRef:
      name: dr-s3
  encryption:
    clusterKEKSecretRef:
      name: cluster-kek
  retention:
    keepDaily: 7
    keepWeekly: 4
  maintenance:
    pruneSchedule: "0 3 * * 0"
    checkSchedule: "0 4 * * 0"
    checkReadDataSubset: "1%"
    timezone: Europe/Paris
EOF
```

Attendu :

```
clusterbackuplocation.crystalbackup.io/dr-primary created
```

Regardez-la initialiser le repository :

```bash
kubectl get clusterbackuplocation dr-primary -w
```

Attendu, en une ou deux minutes (un Job d'init de repository doit récupérer l'image du
mover et lancer `restic init`) :

```
NAME         MODE       DEFAULT   PROTECTED   PHASE   AGE
dr-primary   Standard   true      0                   5s
dr-primary   Standard   true      0           Ready   47s
```

Si elle n'atteint pas `Ready`, les conditions en donnent la raison :

```bash
kubectl get clusterbackuplocation dr-primary -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\t"}{.message}{"\n"}{end}'
```

L'objet repository porte le détail :

```bash
kubectl get backuprepository
```

Attendu :

```
NAME         SCOPE     INITIALIZED   URL                                                      SNAPSHOTS   AGE
dr-primary   Cluster   true          s3:https://s3.example.com/crystal-backups/dr/prod-eu-1   0           1m
```

Un repository du plan cluster reprend le nom de sa propre location. Celui du plan namespace
s'appelle `<namespace>--<location>`, car `BackupRepository` est cluster-scoped et deux
namespaces peuvent utiliser une location du même nom.

## 3. Un schedule, et une exécution tout de suite

Le schedule est le chemin normal. Créez-le :

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupSchedule
metadata:
  name: dr-daily
spec:
  schedule: "0 2 * * *"
  timezone: Europe/Paris
  jitter: true
  concurrencyPolicy: Forbid
  template:
    spec:
      locationRef:
        name: dr-primary
      namespaces:
        matchLabels:
          crystalbackup.io/protect: "true"
      includeManifests: true
      clusterResources:
        enabled: true
      maxConcurrentMovers: 4
EOF
```

Attendu :

```
clusterbackupschedule.crystalbackup.io/dr-daily created
```

Plutôt que d'attendre 02:00, créez directement une exécution. Un `ClusterBackup` porte la
même configuration d'exécution, en ligne :

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackup
metadata:
  name: quickstart-run
spec:
  locationRef:
    name: dr-primary
  namespaces:
    matchNames: ["demo"]
  includeManifests: true
  clusterResources:
    enabled: false
EOF
```

Attendu :

```
clusterbackup.crystalbackup.io/quickstart-run created
```

Suivez l'exécution :

```bash
kubectl get clusterbackup quickstart-run -w
```

Attendu :

```
NAME             PHASE       MATCHED   SUCCEEDED   FAILED   AGE
quickstart-run   Pending     0         0           0        2s
quickstart-run   Running     1         0           0        6s
quickstart-run   Completed   1         1           0        1m12s
```

Le `Backup` enfant a bien atterri dans le namespace :

```bash
kubectl -n demo get backups
```

Attendu :

```
NAME             PHASE       LOCATION     BACKUP-TIME   AGE
quickstart-run   Completed   dr-primary   1m            1m
```

Le détail par volume :

```bash
kubectl -n demo get backup quickstart-run -o jsonpath='{range .status.volumes[*]}{.pvc}{"\t"}{.phase}{"\t"}{.sizeBytes}{"\t"}{.reason}{"\n"}{end}'
```

Attendu :

```
data	Completed	4096	
```

Une phase `Skipped` avec `CSISnapshotUnsupported` signifie ici que le driver CSI de la PVC
ne gère pas les snapshots — voir [Prérequis](/CrystalBackup/fr/docs/start/requirements/).

## 4. Cassez quelque chose

```bash
kubectl -n demo exec writer -- sh -c 'rm /data/canary.txt && ls /data'
```

Attendu : aucune sortie (le répertoire est vide).

## 5. Restore

Un `Restore` namespacé désigne un `Backup` de son propre namespace. Il n'existe aucun champ
pour la location ni pour un namespace cible — cette absence *est* le confinement du tenant.

Répétez d'abord. `dryRun` déroule tout le pipeline avec des applies en server-side dry-run
et ne persiste rien :

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: crystalbackup.io/v1alpha1
kind: Restore
metadata:
  name: recover-canary
  namespace: demo
spec:
  source:
    backup: quickstart-run
  mode: Overwrite
  volumes:
    - names: ["data"]
  dryRun: true
  confirmation: demo
EOF
```

Attendu :

```
restore.crystalbackup.io/recover-canary created
```

Notez `confirmation: demo` — la valeur doit être égale au nom du namespace. Omettez-la et le
restore reste en `AwaitingConfirmation` jusqu'à ce que vous l'ajoutiez ; mettez-y la
*mauvaise* valeur et l'admission rejette l'objet purement et simplement.

Maintenant, le vrai :

```bash
kubectl -n demo delete restore recover-canary
```

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: crystalbackup.io/v1alpha1
kind: Restore
metadata:
  name: recover-canary
  namespace: demo
spec:
  source:
    backup: quickstart-run
  mode: Overwrite
  volumes:
    - names: ["data"]
  confirmation: demo
EOF
```

```bash
kubectl -n demo get restore recover-canary -w
```

Attendu :

```
NAME             PHASE       AGE
recover-canary   Pending     2s
recover-canary   Running     8s
recover-canary   Completed   48s
```

:::note
`Overwrite` restaure dans un volume attaché à un pod en cours d'exécution. Le mover est
épinglé sur le nœud qui porte l'attachement, donc cela fonctionne — mais restaurer sous un
writer vivant reste déconseillé en général. Pour quoi que ce soit de sérieux, réduisez
d'abord le workload à zéro réplica.
:::

Vérifiez que le canari est de retour :

```bash
kubectl -n demo exec writer -- cat /data/canary.txt
```

Attendu :

```
hello-from-the-backup
```

## 6. Le lire avec restic seul

C'est la promesse de réversibilité, et elle coûte une commande à vérifier. Déchiffrez la clé
de la plateforme :

```bash
kubectl -n crystal-backup-system get secret cluster-kek -o jsonpath='{.data.identity}' | base64 -d > /tmp/kek.txt
kubectl -n crystal-backup-system get secret -l app.kubernetes.io/managed-by=crystal-backup -o name | grep crystal-dek
```

Attendu :

```
secret/crystal-dek-dr-primary
```

```bash
export RESTIC_PASSWORD=$(kubectl -n crystal-backup-system get secret crystal-dek-dr-primary \
  -o jsonpath='{.data.dek}' | base64 -d | age -d -i /tmp/kek.txt)
export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY
restic -r s3:https://s3.example.com/crystal-backups/dr/prod-eu-1 snapshots
```

Attendu :

```
ID        Time                 Host        Tags                                                 Paths
------------------------------------------------------------------------------------------------------------
a1b2c3d4  2026-07-30 12:04:11  prod-eu-1   crystalbackup,tenant=demo,namespace=demo,pvc=data,   /data/demo/data
                                           kind=data,run=quickstart-run
------------------------------------------------------------------------------------------------------------
1 snapshots
```

Aucun composant de Crystal Backup n'intervient dans cette commande. Supprimez `/tmp/kek.txt`
quand vous en avez terminé.

## Nettoyage

```bash
kubectl delete namespace demo
kubectl delete clusterbackup quickstart-run
kubectl delete clusterbackupschedule dr-daily
kubectl delete clusterbackuplocation dr-primary
```

Supprimer la location supprime son `BackupRepository` et **laisse le bucket intact**. Pour
supprimer les données, voir [Le droit à l'effacement](/CrystalBackup/fr/docs/guides/erasure/).

## Ensuite

- [Le plan cluster](/CrystalBackup/fr/docs/guides/cluster-plane/) — schedules, sélection des
  namespaces, capture des ressources cluster-scoped.
- [Le plan namespace](/CrystalBackup/fr/docs/guides/namespace-plane/) — donner aux tenants
  leur propre bucket et leur propre clé.
- [Restaurer](/CrystalBackup/fr/docs/guides/restore/) — les modes, la sélection, et ce que
  `Recreate` supprime réellement.
