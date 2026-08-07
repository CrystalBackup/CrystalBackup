---
title: Retirer Crystal Backup
description: L'ordre de suppression et pourquoi s'en écarter bloque le cluster, et un tableau, objet par objet, de ce que chaque suppression retire dans le cluster, dans la couche CSI et dans le repository.
sidebar:
  order: 4
sourceFile: src/content/docs/operations/uninstall.md
sourceHash: 1f47fcd97309cf1e480f859a6b2f735ccb248485
---

La séquence de commandes se trouve dans
[Désinstaller](/CrystalBackup/fr/docs/start/install/#désinstaller), sur la page d'installation,
là où quelqu'un qui défait une installation ira la chercher. Cette page est l'autre moitié :
pourquoi cet ordre est *l'*ordre, et ce que chaque suppression retire réellement.

Commençons par la réponse à la question que tout le monde pose en deuxième et devrait poser en
premier. **Rien dans une désinstallation ne supprime vos backups.** Ni supprimer un `Backup`, ni
supprimer une location, ni supprimer un repository, ni `helm uninstall`, ni supprimer les CRD.
Retirer des données d'un repository demande un
[`ClusterErasure`](/CrystalBackup/fr/docs/guides/erasure/) avec une confirmation typée, et il n'y
a pas d'autre chemin — aucun `restic forget` ne tourne nulle part sur un chemin de suppression,
délibérément, parce que les buckets Immutable et object-lock l'interdisent et parce que
l'erasure doit être quelque chose que vous avez demandé plutôt que quelque chose que vous avez
déclenché.

Ce qu'une désinstallation *peut* vous coûter, c'est la capacité à **lire** ces backups, et elle
ne le coûte qu'à une seule étape : supprimer le namespace de l'operator, qui contient la cluster
KEK et les DEK wrappées. C'est l'étape sur laquelle il faut être prudent, et elle fait ses
dégâts sans supprimer un seul octet de données de backup.

## Pourquoi l'ordre est obligatoire

Cinq finalizers, répartis sur six kinds, et l'operator est le seul processus du cluster qui en
retire un :

| Kind | Finalizer |
|---|---|
| `ClusterBackupLocation`, `BackupLocation` | `crystalbackup.io/location` |
| `BackupRepository` | `crystalbackup.io/repository` |
| `Backup` | `crystalbackup.io/backup` |
| `Restore` | `crystalbackup.io/restore-teardown` |
| `ClusterRestore` | `crystalbackup.io/cluster-restore-teardown` |

L'operator doit donc être encore **en train de tourner** quand ces objets sont supprimés.
Retirez-le d'abord et chacun d'eux s'arrête en `Terminating` sans plus personne pour libérer le
finalizer — définitivement, pas lentement. Un namespace qui en contient un ne finit jamais de se
supprimer, et un `kubectl delete crd` ultérieur l'attendra pour toujours. `helm uninstall`
signale un succès dans les deux cas ; les dégâts apparaissent après.

D'où la séquence, chaque étape existant à cause de celle qui la suit :

1. **Les schedules et les syncs d'abord**, pour que rien de nouveau ne se déclenche dans une
   désinstallation déjà en cours.
2. **Les restores, puis les enregistrements `ClusterBackup`, puis les objets `Backup`**, pendant
   que l'operator est là pour démonter leurs movers et leurs exposures.
3. **Les locations, puis les repositories** — après les objets qui les adressent.
4. **Vérifiez que rien n'est en `Terminating`.** C'est une barrière, pas une formalité : c'est le
   dernier moment où le processus capable de réparer un finalizer bloqué tourne encore.
5. **Ensuite seulement retirez l'installation**, et ensuite seulement envisagez le namespace.

**Ne supprimez jamais les CRD comme raccourci.** Un `kubectl delete crd` sur ce groupe supprime
toutes les custom resources du groupe, à l'échelle du cluster, dans tous les namespaces — y
compris les projections `Backup` qui sont la vue qu'ont vos tenants de ce qu'ils peuvent
restaurer. C'est la dernière étape d'une désinstallation délibérée, jamais un moyen de se
débloquer.

Si vous êtes déjà bloqué, la solution est de **réinstaller l'operator dans la même version** et
de le laisser terminer les suppressions qu'il doit ; c'est écrit sous
[Déjà bloqué en Terminating ?](/CrystalBackup/fr/docs/start/install/#déjà-bloqué-en-terminating).

## Ce que chaque suppression supprime

Lisez la dernière colonne d'abord. Elle dit `non` partout sauf sur une ligne, et cette ligne
demande une confirmation typée.

| Vous supprimez | Dans le cluster | Snapshot CSI | Snapshots du repository |
|---|---|---|---|
| `ClusterBackupSchedule` | le schedule, et les enregistrements `ClusterBackup` qu'il possède (ce sont ses enfants par `ownerReference`) | — | **non** |
| `BackupSchedule` | le schedule seul. Il ne possède délibérément rien : les objets `Backup` qu'il a estampillés *sont* des points de restauration, et cascader dedans supprimerait la vue qu'a un tenant de snapshots qui existent toujours | — | **non** |
| `ClusterBackup` | l'enregistrement du run. Ses enfants `Backup` par namespace sont liés par label, jamais par `ownerReference` : aucun d'eux n'est touché | — | **non** |
| `Backup` | le Job de mover et son Secret de credentials, le Job de capture de manifests et le RoleBinding transitoire dont il avait besoin, et toute exposure encore vivante — PVC temporaire, paire statique `VolumeSnapshot`/`VolumeSnapshotContent`, `VolumeSnapshot` d'origine et son content garé en `Retain`. Sur une backup déjà terminée, tout cela a été démonté quand le volume s'est achevé : il ne reste en général rien à retirer | le snapshot propre à l'exposure, oui — le teardown remet le `deletionPolicy` du content d'origine à `Delete` pour que le snapshot du stockage soit récupéré plutôt que fuité. Un `VolumeSnapshot` que vous avez créé vous-même n'est jamais touché | **non** — et la discovery recrée le `Backup` sous forme de projection à sa passe suivante, puisque les snapshots qu'il décrit sont toujours là |
| `Restore` / `ClusterRestore` | les Jobs de mover de restore et leurs Secrets, les PVC de staging, les PV jumeaux et tout volume de transplant en cours de handover. Les données déjà restaurées dans le namespace cible restent où elles sont | — (un restore n'en crée aucun) | **non** |
| `BackupLocation` | la location, et le `BackupRepository` qu'elle a créé — seulement quand les labels prouvent que ce repository est le sien. Le Secret du mot de passe du repository est **conservé**, même celui que l'operator a généré : c'est la seule chose qui puisse encore lire ces backups | — | **non** |
| `ClusterBackupLocation` | la location et, par garbage collection d'`ownerReference`, son `BackupRepository`. Le Secret de DEK wrappée `crystal-dek-<location>` est **conservé** | — | **non** |
| `BackupRepository` | l'objet. Le Secret de DEK est conservé pour la même raison, et recréer la location ré-adopte le même repository au lieu d'en fabriquer un neuf | — | **non** |
| la release Helm | le Deployment de l'operator, son ServiceAccount et son RBAC, les bindings de politique d'admission, et ceux des objets d'observabilité optionnels que vous avez activés. Pas les CRD — Helm ne les supprime jamais — et pas le namespace, qu'il ne possède pas sous `namespace.create: false` | — | **non** |
| le namespace de l'operator | la cluster KEK et toutes les DEK wrappées qu'il contient | — | **non**, et c'est la ligne dangereuse : les données survivent intactes et deviennent définitivement illisibles, parce que la clé qui les ouvre a disparu. Déplacez ces Secrets d'abord, ou laissez le namespace tranquille |
| les CRD | toutes les custom resources de `crystalbackup.io`, à l'échelle du cluster, dans tous les namespaces | — | **non** |
| un `ClusterErasure` confirmé | rien qui soit à vous | — | **oui** — `restic forget` filtré par tag, puis `prune`. C'est le seul chemin du produit qui supprime des données du repository, il est irréversible, et il ne tournera pas sans la confirmation typée |

Deux conséquences méritent d'être énoncées séparément, parce que ce sont celles que les
administrateurs se trompent dans des directions opposées :

**Un repository survit à ses objets Kubernetes.** Supprimez la location, l'objet repository et
tous les `Backup`, réinstallez de zéro, recréez la location avec le même `clusterID` et le même
`prefix`, et la discovery reprojette les mêmes backups. C'est tout l'intérêt du fait que le
repository fasse foi plutôt qu'etcd.

**Une clé ne survit pas à son Secret.** Il n'en existe pas de deuxième copie dans le cluster :
le séquestre dans le bucket et votre séquestre de KEK hors cluster sont les seules choses entre
un namespace supprimé et des backups illisibles.
