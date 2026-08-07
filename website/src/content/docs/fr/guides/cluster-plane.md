---
title: Le plan cluster
description: Disaster recovery de la plateforme — ClusterBackupLocation, ClusterBackupSchedule, sélection des namespaces et capture des ressources cluster-scoped.
sidebar:
  order: 1
sourceFile: src/content/docs/guides/cluster-plane.md
sourceHash: d1f2a0a70fecf337debc134434375c759faa7003
---

Le plan cluster appartient à l'équipe plateforme : un repository partagé, une politique de
retention, une fenêtre de maintenance, couvrant tous les namespaces que vous sélectionnez —
y compris ceux dont les propriétaires n'ont jamais entendu parler de Crystal Backup. C'est
précisément l'objet de ce plan.

## La location

Une `ClusterBackupLocation`, c'est le stockage objet plus la clé de la plateforme, et elle
adosse exactement un repository restic à `s3://<bucket>/<prefix>/<clusterID>/`.

```yaml
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
    region: eu-west-1
    forcePathStyle: true
    credentialsSecretRef:
      name: dr-s3
  encryption:
    clusterKEKSecretRef:
      name: cluster-kek
  discovery:
    enabled: true
    interval: 1h
  retention:
    keepLast: 3
    keepDaily: 7
    keepWeekly: 4
    keepMonthly: 6
  maintenance:
    pruneSchedule: "0 3 * * 0"
    pruneMaxRepackSize: "50G"
    checkSchedule: "0 4 * * 0"
    checkReadDataSubset: "1%"
    timezone: Europe/Paris
```

### Les champs immuables

`clusterID`, `s3.endpoint`, `s3.bucket`, `s3.prefix` et `mode` ne peuvent pas être modifiés
après la création. Ils composent le chemin du repository, si bien qu'une modification
re-pointerait la location vers un *autre* repository sans qu'aucune donnée ne bouge — toutes
les backups prises jusque-là seraient orphelines alors que l'objet continuerait de paraître
sain. `mode` est figé parce que c'est une propriété choisie au moment où le repository est
créé.

Pour en changer un, créez une nouvelle location et, si l'historique vous est nécessaire,
répliquez dedans avec l'[external sync](/CrystalBackup/fr/docs/guides/external-sync/).

### `clusterID` n'est pas cosmétique

C'est le `host` des snapshots restic **et** un segment du chemin. Un même bucket peut donc
servir plusieurs clusters, chacun sous son propre prefix, sans collision. Choisissez un nom
que vous reconnaîtrez encore dans deux ans, pendant un incident, dans le terminal de
quelqu'un d'autre.

### `default: true`

Une seule location peut être celle par défaut. C'est d'elle qu'une `BackupLocation` hérite
son `clusterID` quand le tenant n'en fixe pas. La vérification d'unicité est assurée par le
webhook de l'operator plutôt que par une admission policy, parce que c'est une contrainte
inter-objets qu'une expression CEL par objet ne sait pas exprimer ; une race qui passerait
au travers se manifeste par une condition `MultipleDefaults`.

### `retention` vit ici, pas sur les schedules

`restic forget` opère sur le repository entier. Une location adosse un repository, donc une
politique unique et faisant autorité par location est le seul agencement dans lequel deux
schedules ne peuvent pas se disputer les mêmes snapshots. Elle est appliquée **par PVC**
(regroupés par `host,paths` restic) après chaque backup réussie.

Il n'y a pas de `keepWithinDuration`. Les champs disponibles sont exactement `keepLast`,
`keepHourly`, `keepDaily`, `keepWeekly`, `keepMonthly`, `keepYearly`.

## Le schedule

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupSchedule
metadata:
  name: dr-daily
spec:
  schedule: "0 2 * * *"
  timezone: Europe/Paris
  paused: false
  jitter: true
  concurrencyPolicy: Forbid
  startingDeadlineSeconds: 3600
  successfulRunsHistoryLimit: 10
  failedRunsHistoryLimit: 10
  template:
    spec:
      locationRef:
        name: dr-primary
      namespaces:
        matchLabels:
          crystalbackup.io/protect: "true"
        exclude: ["kube-*", "crystal-backup-system"]
      includeManifests: true
      manifestOptions:
        excludeSecretData: false
      clusterResources:
        enabled: true
      pvcSelector:
        exclude: ["*-cache", "scratch-*"]
      maxConcurrentMovers: 4
      backoffLimit: 2
```

`jitter: true` étale le fan-out par namespace de façon déterministe. Sur un cluster avec
cinquante namespaces protégés, c'est la différence entre une ruée générale à 02:00 et une
heure étalée. Activez-le.

`concurrencyPolicy: Forbid` (la valeur par défaut) signifie qu'une exécution encore en cours
à l'heure de la suivante bloque cette dernière. `Skip` abandonne l'exécution manquée à la
place.

`startingDeadlineSeconds` borne le rattrapage après une indisponibilité de l'operator : sans
lui, une longue panne produit une rafale d'exécutions manquées au redémarrage.

`maxConcurrentMovers` est un plafond **à l'échelle du cluster**, pas par exécution — il est
évalué contre tous les Jobs de mover du namespace de l'operator. C'est pour cela qu'il
n'existe que sur le plan cluster ; un tenant qui le fixerait fixerait une limite valable
pour toute la plateforme.

## Sélectionner les namespaces

`namespaces` exige **exactement une forme positive non vide**, plus un `exclude` facultatif
appliqué en dernier. Une liste ou une map vide compte comme non renseignée.

```yaml
# By opt-in label — the recommended default.
namespaces:
  matchLabels:
    crystalbackup.io/protect: "true"

# By name glob.
namespaces:
  matchNames: ["team-*", "prod-*"]
  exclude: ["team-sandbox"]

# By label expression.
namespaces:
  matchExpressions:
    - key: tier
      operator: In
      values: ["production", "staging"]

# By regexp — a power tool. Prefer one of the above.
namespaces:
  regexp: "^c-[a-z0-9]+$"
```

`crystalbackup.io/protect` est une convention, pas une clé magique : l'operator la lit parce
que votre sélecteur la nomme, et ne la pose jamais. Deux postures fonctionnent :

- **opt-in** — `matchLabels: {crystalbackup.io/protect: "true"}`. Les namespaces sont
  protégés quand quelqu'un le demande. Rien n'est sauvegardé par accident, et rien ne l'est
  par omission non plus.
- **opt-out** — `matchNames: ["*"]` avec une liste `exclude`. Tout est couvert par défaut.
  Plus sûr, et cela ramassera des namespaces pleins de caches et d'artefacts de build à
  moins d'affiner aussi `pvcSelector`.

Choisissez délibérément ; les modes de défaillance sont opposés.

## Sélectionner les PVC

```yaml
pvcSelector:
  matchLabels:
    backup: "yes"
  include: ["data-*"]
  exclude: ["*-cache", "*-tmp"]
```

Vide signifie toutes les PVC du namespace. `exclude` est appliqué après les formes
positives.

## Les ressources cluster-scoped

Une exécution capture aussi les objets qui vivent en dehors de tout namespace, sous forme
d'un snapshot unique portant `kind=cluster-manifests`. C'est actif par défaut sur le plan
cluster, et c'est ce qui rend possible la DR sur cluster nu : restaurer un namespace dans un
cluster dépourvu de la `StorageClass` correspondante ne restaure rien d'exploitable.

```yaml
clusterResources:
  enabled: true
  include: []          # empty ⇒ the curated default set
  exclude: ["system:*"]
```

Avec un `include` vide, l'ensemble par défaut est : `CustomResourceDefinition`,
`StorageClass`, `VolumeSnapshotClass`, `IngressClass`, `PriorityClass`, `RuntimeClass`, les
`ClusterRole` et `ClusterRoleBinding` non système, et `PersistentVolume`. Les objets nommés
`system:*` et ceux appartenant aux add-ons sont exclus par défaut, pour qu'un restore n'aille
pas se battre avec l'API server.

La capture est peu coûteuse et large ; **le restore est opt-in et étroit**. Voir
[Disaster recovery](/CrystalBackup/fr/docs/guides/disaster-recovery/).

## Lancer une backup tout de suite

Les schedules produisent des objets `ClusterBackup`. Vous pouvez en créer un vous-même — la
même configuration d'exécution, en ligne :

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackup
metadata:
  name: before-the-upgrade
spec:
  locationRef:
    name: dr-primary
  namespaces:
    matchNames: ["team-x"]
  includeManifests: true
  clusterResources:
    enabled: false
```

Les exécutions planifiées sont nommées `<schedule>-<YYYYMMDD-HHMMSS>` en UTC. Cette même
chaîne est le nom du `ClusterBackup`, le nom de chaque `Backup` enfant dans chaque namespace
et le tag restic `run` — une exécution est donc un seul identifiant de bout en bout.

## Suivre une exécution

```bash
kubectl get clusterbackups
kubectl -n <namespace> get backups
```

Les compteurs agrégés vivent sur le `ClusterBackup` ; le détail par namespace vit sur les
objets `Backup` enfants. Le parent ne conserve qu'une liste d'échecs **plafonnée**, parce
qu'une map par namespace sans borne sur un cluster de 500 namespaces est un objet qui finit
par ne plus pouvoir être écrit du tout.

Les enfants sont rattachés à l'exécution par le label
`crystalbackup.io/cluster-backup`, **pas** par une ownerReference. Élaguer les vieux
enregistrements d'exécution ne supprime donc jamais une backup restaurable.

```bash
kubectl get backups -A -l crystalbackup.io/cluster-backup=dr-daily-20260730-020000
```

Si une exécution rapporte `PartiallyFailed` :

```bash
kubectl get clusterbackup <run> -o jsonpath='{range .status.failures[*]}{.namespace}{"\t"}{.backup}{"\t"}{.message}{"\n"}{end}'
```

## Où tournent les movers

Par défaut, un Job de mover se planifie n'importe où, ce qui est la bonne réponse sur un cluster
dont les nœuds sont interchangeables. Ils ne le sont pas toujours. Un mover **monte le volume du
tenant** : il hérite donc de toutes les contraintes que le driver CSI impose au nœud qui fait le
montage, et ces contraintes ne sont pas uniformes. Sur Rook-Ceph, mapper un *clone* RBD — c'est
exactement ce qu'est la lecture d'un snapshot CSI — exige un noyau portant le feature bit
clone-child : un nœud en 5.4 répond `rbd: map failed: (22) Invalid argument` là où un nœud en
5.15 juste à côté monte le volume et la backup passe. Les raisons ordinaires ont la même forme :
un pool de nœuds tainté pour les travaux gourmands en I/O, une zone dont l'egress vers l'object
store est bon marché.

`mover.placement` est une value du chart et non un champ d'un objet de cette page, et c'est là
que cela se dit :

```yaml
mover:
  placement:
    nodeSelector:
      crystalbackup.io/mover: "true"
    tolerations:
      - key: dedicated
        operator: Equal
        value: backup
        effect: NoSchedule
    affinity:
      nodeAffinity:
        preferredDuringSchedulingIgnoredDuringExecution:
          - weight: 100
            preference:
              matchExpressions:
                - key: node.kubernetes.io/instance-type
                  operator: In
                  values: [storage-optimised]
```

Elle s'applique à **tous** les movers — backup et restore par PVC, capture des manifests,
discovery, rétention, prune, check, unlock, sync externe — sur les deux plans. « Les pods de
backup tournent sur les nœuds de backup » est une phrase que vous vérifiez d'un seul
`kubectl get pods -o wide` ; une règle à exceptions est une phrase que vous découvrez pour la
première fois en plein debug. Et elle n'appartient qu'à vous : il n'y a délibérément aucune
surcharge par namespace ni par schedule, parce que le choix des nœuds sur lesquels atterrissent
les pods de backup de la plateforme n'est pas une décision de tenant.

**`nodeSelector` est une exigence dure, et il n'en existe pas de forme souple.** Réfléchissez
avant de le pointer sur un label que seuls quelques nœuds portent : il ne fait pas *préférer*
ces nœuds aux movers, il sérialise tous les backups du cluster à travers eux, et il transforme
leur absence en un cluster sans aucune backup. Quand c'est une préférence que vous voulez
exprimer, le bloc `affinity.nodeAffinity` ci-dessus est le bon champ — les movers atterrissent
sur les nœuds capables tant que ceux-ci ont de la place, et ailleurs plutôt que nulle part
quand ils n'en ont plus.

**Un Job fait exception.** Un restore dans un volume RWO existant est épinglé sur le nœud auquel
le volume est attaché, parce que c'est le seul nœud depuis lequel il peut être monté. Sur ce Job,
le nodeSelector et l'affinity sont retirés et seules les tolerations sont conservées : la kubelet
revérifie les deux à l'admission, y compris pour un pod qu'elle n'a jamais planifié, et
rejetterait le pod purement et simplement au lieu de le placer mieux. Les retirer ne fait pas
réussir le restore sur un nœud incapable de monter le volume — cela fait que l'échec soit le
vrai, celui du driver CSI, plutôt qu'une erreur de scheduling à propos d'un pod qui n'a jamais
été planifié.

**Un placement que l'operator n'arrive pas à interpréter l'arrête au démarrage** : champ inconnu,
clé de label invalide, toleration que l'API server refuserait, terme d'affinity qui ne matche
aucun nœud. C'est délibéré. Un placement qu'un administrateur relit dans `helm get values` et qui
n'a jamais atteint un pod, c'est une backup qui tourne sur un nœud incapable de monter le volume,
c'est-à-dire une backup qui n'existe pas. Changer la value redémarre le pod de l'operator, parce
que le fichier est lu une seule fois au démarrage et que le deployment en porte un checksum.

Voir [Values Helm](/CrystalBackup/fr/docs/reference/helm-values/#où-tournent-les-jobs-de-mover)
pour la référence champ par champ, et [la sonde de faisabilité des
snapshots](/CrystalBackup/fr/docs/operations/snapshot-probe/) pour établir si la chaîne
snapshot → montage → lecture fonctionne tout court sur ce cluster.

## Mettre en pause

```bash
kubectl patch clusterbackupschedule dr-daily --type=merge -p '{"spec":{"paused":true}}'
```

`paused` existe sur `ClusterBackupSchedule` et sur `ClusterBackupExternalSync`. Il
n'existe **pas** sur le `BackupSchedule` namespacé — pour arrêter un schedule de tenant,
supprimez-le ou changez son expression cron.

## Ensuite

- [Le plan namespace](/CrystalBackup/fr/docs/guides/namespace-plane/)
- [Maintenance et vérification](/CrystalBackup/fr/docs/guides/maintenance/)
- [Disaster recovery](/CrystalBackup/fr/docs/guides/disaster-recovery/)
