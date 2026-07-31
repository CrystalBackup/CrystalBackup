---
title: Le plan namespace
description: Backup en self-service pour le propriétaire d'un namespace — votre bucket, votre clé, votre schedule.
sidebar:
  order: 2
sourceFile: src/content/docs/guides/namespace-plane.md
sourceHash: d5d3684be025dc91b6cbc87771367cb3fff2668d
---

Le plan cluster protège déjà votre namespace, dans le bucket de la plateforme et sous sa
clé. Le plan namespace vous donne une **seconde copie, indépendante**, que la plateforme ne
peut pas lire.

Rien ici ne requiert d'administrateur une fois l'operator installé et le rôle
`crystal-backup-tenant` lié à vous par votre équipe plateforme — rôle que vous avez déjà, si
elle a laissé `rbac.aggregateToDefaultRoles` actif et que vous disposez de `edit` sur votre
namespace.

## Ce que vous pouvez faire

Avec le rôle tenant, vous obtenez l'ensemble des verbes sur `BackupLocation`,
`BackupSchedule`, `Restore` et `BackupExternalSync` dans vos propres namespaces, et un accès
**en lecture seule** sur `Backup`.

`Backup` est en lecture seule parce que c'est une projection du repository, pas un objet dont
vous êtes l'auteur. Supprimez-en un et la discovery le reprojette à la passe suivante ; c'est
le repository qui fait foi, pas l'objet.

## Votre clé

Le repository d'une `BackupLocation` a exactement un slot de clé : le vôtre. Aucun champ de
l'API ne pourrait en donner un second à la plateforme.

Apportez votre propre mot de passe :

```bash
kubectl -n team-x create secret generic offsite-key \
  --from-literal=password="$(openssl rand -base64 32)"
```

Ou omettez `repositoryPasswordSecretRef` et l'operator en génère un **dans votre namespace**,
nommé `crystal-repo-password-<location>`.

:::danger[Il n'existe aucun chemin de récupération]
Perdez ce mot de passe et vos backups sont illisibles. La plateforme n'en détient pas de
copie, parce que le mécanisme permettant d'en détenir une a été supprimé plutôt qu'encadré —
retirer un slot de clé restic ne fait pas tourner la master key, si bien qu'un slot pour la
plateforme aurait été permanent.

Mettez-le en séquestre là où vous gardez vos propres secrets racine, **en dehors de ce
cluster**.
:::

## Vos credentials

```bash
kubectl -n team-x create secret generic offsite-s3 \
  --from-literal=AWS_ACCESS_KEY_ID=... \
  --from-literal=AWS_SECRET_ACCESS_KEY=...
```

Les deux Secrets sont référencés **par nom uniquement**, et doivent se trouver dans le même
namespace que la location. Une règle d'admission le fait respecter, si bien qu'une location
ne peut pas aller chercher ses credentials dans un autre namespace.

## La location

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: BackupLocation
metadata:
  name: my-offsite
  namespace: team-x
spec:
  mode: Standard
  s3:
    endpoint: https://s3.other-provider.example
    bucket: team-x-backups
    prefix: crystal
    forcePathStyle: true
    credentialsSecretRef:
      name: offsite-s3
  encryption:
    repositoryPasswordSecretRef:
      name: offsite-key
  discovery:
    enabled: true
    interval: 1h
  retention:
    keepDaily: 14
    keepWeekly: 8
```

Comme sur le plan cluster, `clusterID`, `s3.endpoint`, `s3.bucket`, `s3.prefix` et `mode`
sont **immuables après la création** : ils composent le chemin du repository. `clusterID` est
facultatif ici et prend par défaut celui de la `ClusterBackupLocation` par défaut de la
plateforme — mais une fois résolu il est enregistré dans `status.clusterID` et n'est jamais
re-dérivé, si bien qu'un administrateur qui changerait le défaut de la plateforme plus tard
ne peut pas déplacer silencieusement votre repository.

Utilisez un bucket que la plateforme ne contrôle pas. C'est tout l'intérêt ; une location
pointant vers le stockage objet de la plateforme vous donne une seconde copie mais pas une
seconde frontière de confiance.

Vérifiez qu'elle est en ligne :

```bash
kubectl -n team-x get backuplocation my-offsite
```

```
NAME         MODE       PHASE   AGE
my-offsite   Standard   Ready   38s
```

## Le schedule

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: BackupSchedule
metadata:
  name: nightly
  namespace: team-x
spec:
  locationRef:
    name: my-offsite
  schedule: "0 1 * * *"
  timezone: Europe/Paris
  jitter: true
  concurrencyPolicy: Forbid
  startingDeadlineSeconds: 3600
  pvcSelector:
    exclude: ["*-cache"]
  includeManifests: true
  backoffLimit: 2
```

`locationRef` doit nommer une `BackupLocation` **de ce namespace**. Il ne peut jamais nommer
une `ClusterBackupLocation` — c'est une règle d'admission, et c'est ce qui empêche un tenant
d'écrire dans le repository partagé de la plateforme.

Deux champs que vous ne trouverez pas ici, et pourquoi :

- **`paused`** n'existe pas sur `BackupSchedule`. Pour l'arrêter, supprimez-le ou changez
  l'expression cron.
- **`maxConcurrentMovers`** n'existe pas non plus. C'est un plafond à l'échelle du cluster,
  et un tenant qui le fixerait fixerait une limite valable pour toute la plateforme.

## Le suivre

```bash
kubectl -n team-x get backupschedules
```

```
NAME      SCHEDULE    LOCATION     LAST-SUCCESS   AGE
nightly   0 1 * * *   my-offsite   7h             3d
```

```bash
kubectl -n team-x get backups
```

```
NAME                      PHASE       LOCATION     BACKUP-TIME   AGE
nightly-20260730-010000   Completed   my-offsite   7h            7h
dr-daily-20260730-020000  Completed   dr-primary   6h            6h
```

Les backups des deux plans apparaissent dans votre namespace. Distinguez-les par leur
origine :

```bash
kubectl -n team-x get backups -l crystalbackup.io/origin=namespace   # yours
kubectl -n team-x get backups -l crystalbackup.io/origin=cluster     # platform DR
```

Vous pouvez restaurer depuis l'une comme depuis l'autre. Une backup d'origine cluster se
restaure via le même objet `Restore` ; l'operator résout le repository partagé pour votre
compte, avec un filtre de namespace dérivé du vôtre qu'aucun champ de votre objet ne peut
influencer.

Le détail par volume :

```bash
kubectl -n team-x get backup nightly-20260730-010000 \
  -o jsonpath='{range .status.volumes[*]}{.pvc}{"\t"}{.phase}{"\t"}{.sizeBytes}{"\t"}{.reason}{"\n"}{end}'
```

## Les manifests

`includeManifests` vaut `true` par défaut, et le snapshot de manifests **inclut vos objets
Secret**. Sur votre propre repository, sous votre propre clé, c'est normalement ce que vous
voulez — une récupération de namespace sans les Secrets est un namespace qui ne démarre pas.

Si vous préférez échanger de la récupérabilité contre un rayon d'impact plus réduit :

```yaml
manifestOptions:
  excludeSecretData: true
```

Les Secrets sont alors stockés avec `data` et `stringData` retirés et annotés
`crystalbackup.io/secret-data-excluded: "true"`. Le restore les recrée **vides**, portant la
même annotation — si bien qu'un workload qui a besoin des valeurs échoue visiblement sur une
clé manquante plutôt que de démarrer silencieusement avec les mauvaises.

## Les hooks de cohérence

Un snapshot est crash-consistent par défaut. Si votre application a besoin de plus, les hooks
permettent de la quiescer autour du snapshot — et sur ce plan ils exigent un ServiceAccount
que **vous** habilitez, et que l'operator impersonate. Voir
[Les hooks de cohérence](/CrystalBackup/fr/docs/guides/hooks/).

## Où le travail s'exécute

Pas dans votre namespace. Les Jobs de mover s'exécutent dans `crystal-backup-system`, sur les
deux plans, et votre namespace ne reçoit jamais de credentials ni de matériel de clé —
seulement des PVC restaurées.

C'est une propriété de la conception plutôt qu'une politesse : un snapshot pris sur votre PVC
est ré-attaché de façon centralisée, si bien que vos données ne sont jamais montées là où un
voisin pourrait les atteindre.

## Ensuite

- [Restaurer](/CrystalBackup/fr/docs/guides/restore/)
- [External sync](/CrystalBackup/fr/docs/guides/external-sync/) — une seconde location, dans
  ce namespace, sous une seconde clé à vous
- [Les hooks de cohérence](/CrystalBackup/fr/docs/guides/hooks/)
