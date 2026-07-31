---
title: Maintenance et vérification
description: Retention, prune, restic check, la file exclusive, et le changement de clé d'un repository.
sidebar:
  order: 7
sourceFile: src/content/docs/guides/maintenance.md
sourceHash: 7cd0c454c22f9cb4a3e9c5c35038f01bf4ce12ff
---

Trois opérations maintiennent un repository en bonne santé : `forget` récupère de la place
selon la politique, `prune` la récupère physiquement, et `check` vous dit si ce qui s'y
trouve est toujours lisible. Seule la troisième est une vérification ; les deux premières
sont de l'entretien.

## La retention vit sur la location

```yaml
spec:
  retention:
    keepLast: 3
    keepHourly: 24
    keepDaily: 7
    keepWeekly: 4
    keepMonthly: 6
    keepYearly: 2
```

Ces six champs sont tout le vocabulaire. Il n'y a pas de `keepWithinDuration`.

Elle est sur la **location**, pas sur les schedules ni sur les exécutions, parce que
`restic forget` opère sur le repository entier et qu'une location adosse exactement un
repository. Une politique unique et faisant autorité par location est le seul agencement dans
lequel deux schedules ne peuvent pas se disputer les mêmes snapshots.

Elle est appliquée **par PVC** — regroupées par `host,paths` restic — après chaque backup
réussie, mise en file sur la file de maintenance du repository plutôt qu'exécutée en ligne.
Une exécution qui se termine n'attend pas sa propre passe de retention.

Une politique entièrement à zéro est un no-op sûr : rien n'est oublié.

Sur une location `Immutable`, la retention est signalée comme ignorée, via une condition
`RetentionIgnored` sur la location. C'est Object Lock qui y gouverne l'expiration.

## Prune

`forget` supprime des références de snapshots. `prune` supprime les données dont ces
snapshots étaient les derniers détenteurs. Tant que vous ne prunez pas, oublier ne libère
rien.

```yaml
spec:
  maintenance:
    pruneSchedule: "0 3 * * 0"
    pruneMaxRepackSize: "50G"
    checkSchedule: "0 4 * * 0"
    checkReadDataSubset: "1%"
    timezone: Europe/Paris
```

`timezone` compte plus qu'il n'y paraît. Tout l'intérêt de `pruneSchedule` est de placer une
fenêtre exclusive à l'échelle du cluster à une heure creuse, et « heure creuse » est une
notion d'heure locale — `"0 3 * * 0"` lu comme de l'UTC tombe en pleine journée de travail
pour la moitié du monde. Vide signifie UTC.

`pruneMaxRepackSize` plafonne le repacking par exécution et constitue la borne pratique sur
la durée de la fenêtre. Vide signifie le défaut de restic : repacker tout ce dont l'exécution
a besoin, aussi longtemps qu'il le faut. Sur un gros repository partagé, fixez-le.

:::caution[Le prune est la seule fenêtre exclusive à l'échelle du cluster]
Pendant son exécution, aucun namespace ne peut démarrer une backup sur ce repository. Son
usage mémoire croît avec la taille **totale** du repository, pas avec la taille par
namespace. Planifiez-le en heure creuse, plafonnez-le, et laissez-lui de la place.
:::

Les backups n'échouent pas pendant la fenêtre ; elles **attendent**. Un `Backup` reste en
`Pending` (ou un volume en `Snapshotting`) jusqu'à ce qu'un créneau se libère. Il n'y a pas
de phase `Queued` — l'attente est silencieuse et se résout d'elle-même.

Les locations `Immutable` ne prunent jamais, et fixer `pruneSchedule` sur l'une d'elles est
rejeté à l'admission.

## Check — la seule vérification

```yaml
maintenance:
  checkSchedule: "0 4 * * 0"
  checkReadDataSubset: "1%"
```

Sans `checkReadDataSubset`, `restic check` est un contrôle **structurel** : il détecte un
objet manquant ou tronqué, et ne détecte jamais un objet silencieusement corrompu dont les
octets ont pourri alors que son nom et sa longueur sont restés justes. C'est pourtant bien
ce mode de défaillance qu'a le stockage objet.

`checkReadDataSubset` fait que chaque check **lit** réellement des données de packs. Il
accepte :

| Forme | Signification |
|---|---|
| `"1%"`, `"2.5%"` | ce pourcentage de packs, un échantillon différent à chaque exécution |
| `"1/20"` | un vingtième précis — faites tourner le numérateur d'une exécution à l'autre pour tout couvrir |
| `"5G"`, `"500M"` | cette quantité de données |

Un `1%` hebdomadaire couvre tout le repository en environ deux ans, ce qui ne suffit pour
rien de ce à quoi vous tenez ; un `5%` hebdomadaire le couvre en cinq mois. Choisissez en
fonction de la valeur de vos données, pas de votre budget CPU.

Les résultats atterrissent sur le repository :

```bash
kubectl get backuprepository dr-primary \
  -o jsonpath='{.status.lastCheckTime}{"\t"}{.status.lastCheckResult}{"\n"}'
```

`lastCheckResult` vaut `Passed` ou `Failed`. `Failed` signifie que restic a trouvé des dégâts
dans le repository — c'est un incident, pas une erreur transitoire.

Notez l'asymétrie entre les deux horodatages, car elle est porteuse :

- `lastCheckTime` est mis à jour **que le check ait réussi ou échoué**. Associé au résultat,
  il distingue « vérifié récemment et c'était mauvais » de « pas vérifié depuis des
  semaines », qui sont deux incidents différents.
- `lastMaintenanceTime` n'est mis à jour **que lorsqu'un prune a réussi**. Un prune en échec
  le laisse délibérément tranquille, pour qu'une alerte de péremption continue de sonner.

## Les répétitions de restore sont à vous

C'est dit clairement plutôt qu'enfoui : l'outil ne peut pas restaurer chaque backup en canari
tous les jours, et ne prétend pas le faire. `restic check` vérifie que le repository est
lisible. Il ne vérifie pas qu'un restore produit une application qui fonctionne.

**Une backup que vous ne restaurez jamais n'est pas une backup.** Planifiez de vraies
répétitions de restore dans un namespace jetable, à une cadence réelle, et traitez une
répétition qui échoue comme vous traiteriez un incident de production. `ClusterRestore` avec
`createNamespace: true` et un `storageClassMapping` rend cela peu coûteux.

## La santé du repository

```bash
kubectl get backuprepository
```

```
NAME         SCOPE     INITIALIZED   URL                                                      SNAPSHOTS   AGE
dr-primary   Cluster   true          s3:https://s3.example.com/crystal-backups/dr/prod-eu-1   1284        41d
```

```bash
kubectl get backuprepository dr-primary -o jsonpath='{.status.approximateSizeBytes}{" bytes, "}{.status.staleLocks}{" stale locks, "}{.status.namespacesPresent}{" namespaces\n"}'
```

`approximateSizeBytes` est la taille **physique** — les objets réellement stockés sous le
prefix, après déduplication et compression. Pour le repository partagé, c'est l'empreinte de
tout le cluster dans ce bucket.

`staleLocks` compte les objets de verrou du repository plus vieux que le seuil de 30 minutes
de restic. Normalement zéro ; le verrou d'un mover tué brutalement est nettoyé par une
opération d'unlock. **Une valeur non nulle persistante est un vrai problème** : les verrous
s'accumulent plus vite qu'ils ne sont récupérés, et toute opération exclusive finira par
caler derrière eux.

### Pourquoi une exécution de maintenance a échoué

Le Job de maintenance et son pod sont supprimés dès qu'une opération se termine, si bien que
le status du repository est la seule trace durable :

```bash
kubectl get backuprepository dr-primary \
  -o jsonpath='{range .status.recentMaintenance[*]}{.operation}{"\t"}{.startTime}{"\t"}{.result}{"\t"}{.message}{"\n"}{end}'
```

Du plus récent au plus ancien, plafonné à dix. `startTime` est le moment où l'opération a été
**mise en file**, pas celui où elle a commencé à s'exécuter — il inclut délibérément
l'attente de son tour, parce que « le prune a pris trois heures » est le chiffre dont vous
avez besoin et que la file est exactement là où la contention se voit.

## La file exclusive, en bref

Chaque opération restic mutante — `init`, `forget`, `prune`, `check`, `unlock`, effacement —
s'exécute une à la fois par repository, en FIFO. Des repositories différents s'exécutent
pleinement en parallèle.

**Les lectures ne sont pas mises en file.** `snapshots`, `stats` et `ls` passent
`--no-lock`, si bien qu'une passe de discovery ou la résolution de la source d'un restore
n'attend jamais derrière une fenêtre de maintenance. Les data movers sont eux aussi des
lecteurs : plusieurs peuvent s'exécuter contre un même repository en même temps.

Deux opérations **drainent** en outre les movers avant de s'exécuter : `unlock`, parce que
retirer tous les verrous arracherait celui d'une backup en cours ; et `prune`, pour une autre
raison — le débit. Le verrou exclusif propre à restic empêche déjà toute corruption dans ce
cas, mais sans drain le prune et toute la flotte de movers se regardent en chiens de faïence
sur des retries de verrou jusqu'à ce que l'un abandonne. Le drain convertit une tempête de
contention en une seule fenêtre sérialisée et courte.

`forget` et `check` ne drainent pas. Leur tour dans la file suffit.

## Changer la clé d'un repository

`restic key remove` révoque un *mot de passe d'accès*. Chaque mot de passe déchiffre la même
**master key**, et celle-ci ne change jamais. Une clé qui a fuité ne peut donc pas être
révoquée — la seule vraie réponse est de copier vers un repository neuf.

**1 — Créez la destination** avec une nouvelle clé :

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupLocation
metadata:
  name: dr-primary-rekeyed
spec:
  clusterID: prod-eu-1
  s3:
    endpoint: https://s3.example.com
    bucket: crystal-backups-rekeyed
    prefix: dr
    forcePathStyle: true
    credentialsSecretRef: { name: dr-s3 }
  encryption:
    clusterKEKSecretRef: { name: cluster-kek-v2 }
```

Si elle doit aussi recevoir des backups natives, initialisez-la d'abord avec les paramètres
de chunker de la source (`restic init --from-repo <old> --copy-chunker-params`) —
l'initialisation faite par l'operator ne le fait pas.

**2 — Synchronisez en `AppendOnly`.** Rien ne doit être oublié à la destination pendant un
changement de clé.

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupExternalSync
metadata:
  name: rekey
spec:
  sourceLocationRef: { name: dr-primary }
  destinationLocationRef: { name: dr-primary-rekeyed }
  mode: AppendOnly
```

**3 — Vérifiez avant de détruire quoi que ce soit.** `lagSnapshots: 0` dit que chaque
snapshot de la source a une copie ; cela ne dit pas que les copies sont lisibles.

```bash
kubectl get backuprepository dr-primary-rekeyed \
  -o jsonpath='{.status.lastCheckTime}{"\t"}{.status.lastCheckResult}{"\n"}'
```

Puis lancez un vrai `ClusterRestore` depuis la nouvelle location vers un namespace jetable.

**4 — Basculez**, laissez un cycle complet de backup s'achever contre la nouvelle location,
et seulement ensuite retirez l'ancienne du service.

## Retirer un repository du service

Supprimer une location supprime son `BackupRepository` et **ne touche jamais au bucket**.
Pour rendre illisible un repository retiré du service, vous détruisez sa clé :

```bash
kubectl -n crystal-backup-system delete secret crystal-dek-<location>
```

Cela ne suffit que si aucune copie de la clé déballée n'existe ailleurs — et c'est par nature
du best-effort, ce qui explique que ce soit un runbook plutôt qu'une custom resource.

Sur le plan namespace, un mot de passe fourni par le tenant est **le sien** : l'operator ne
l'a jamais généré et ne le supprimera pas. Celui que l'operator a généré vit à
`crystal-repo-password-<location>` dans son namespace, délibérément sans ownerReference, si
bien qu'il survit à la suppression de la location jusqu'à ce que quelqu'un en décide
autrement.

Consignez ce que vous détruisez avant de le détruire — nom de la location, bucket, prefix,
cluster ID, nombre de snapshots et taille à l'instant t — parce qu'ensuite ce relevé sera le
seul artefact qui existe.
