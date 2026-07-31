---
title: External sync
description: Répliquer un repository vers une seconde location avec restic copy, re-chiffré sous la clé propre de la destination.
sidebar:
  order: 5
sourceFile: src/content/docs/guides/external-sync.md
sourceHash: 4abd0c4f28990cbb7e86f3bb597d81592eaef305
---

L'external sync maintient un **second repository, indépendant**, au pas d'un premier. Ce
n'est pas un clone octet pour octet : les snapshots sont déchiffrés depuis la source et
**re-chiffrés sous la clé propre de la destination**, si bien que la destination est un
véritable repository sous une clé que la source ne détient pas.

C'est ce qui le rend utilisable entre fournisseurs, entre comptes, et — sur le plan
namespace — entre deux locations d'un même tenant sans que la clé de la plateforme
n'intervienne jamais.

## Deux kinds, un par plan

```yaml
# Cluster plane, admin.
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupExternalSync
metadata:
  name: offsite
spec:
  sourceLocationRef:
    name: dr-primary
  destinationLocationRef:
    name: dr-secondary
  schedule: "0 5 * * *"
  timezone: Europe/Paris
  paused: false
  mode: Mirror
  selection:
    namespaces:
      matchLabels:
        tier: production
```

```yaml
# Namespace plane, tenant. Both refs are BackupLocations in THIS namespace.
apiVersion: crystalbackup.io/v1alpha1
kind: BackupExternalSync
metadata:
  name: to-my-second-provider
  namespace: team-x
spec:
  sourceLocationRef:
    name: my-offsite
  destinationLocationRef:
    name: my-offsite-b
  schedule: "0 6 * * *"
  mode: Mirror
```

Un `schedule` vide signifie à la demande uniquement — la synchronisation s'exécute quand vous
créez ou modifiez l'objet, et pas autrement.

`selection.namespaces` n'existe que sur le plan cluster, et restreint la copie par tag de
namespace. Omis, c'est le repository entier qui est répliqué.

La source et la destination doivent toutes deux être des locations du **même plan**, et elles
doivent **différer**. Un `Mirror` auto-référent ferait un `forget` puis un `prune` sur sa
propre source ; c'est une règle d'admission, pas un avertissement.

Sur le plan namespace, les deux refs se résolvent dans le namespace de la CR elle-même, et
aucune ne peut nommer une `ClusterBackupLocation`. Le cloisonnement du tenant est préservé :
la clé de la plateforme n'intervient jamais dans la synchronisation d'un tenant.

## Les modes

| | `Mirror` (par défaut) | `AppendOnly` |
|---|---|---|
| Copie les snapshots manquants | oui | oui |
| Supprime à la destination les snapshots disparus de la source | **oui** — `forget` puis `prune` | non |
| La destination grossit sans borne | non | oui |

`Mirror` réconcilie la destination avec l'ensemble *courant* des snapshots de la source. Le
choix des snapshots à oublier à la destination est décidé par le champ `original` de restic,
qui enregistre l'ID complet du snapshot source — les tags et les horodatages ne permettent
pas de distinguer deux exécutions d'un même schedule, c'est donc la seule clé qui
fonctionne.

Utilisez `AppendOnly` quand la destination est censée survivre à la retention de la source :
un secondaire qui conserve un historique que le primaire a déjà élagué. Et utilisez-le quand
vous changez la clé d'un repository, cas où rien ne doit être oublié à la destination pendant
que la copie est en vol.

## La déduplication à la destination

Si la destination doit **aussi** recevoir des backups natives, initialisez-la avec les
paramètres de chunker de la source, faute de quoi les deux ensembles de blobs ne se
dédupliqueront pas entre eux :

```bash
restic -r <destination> init --from-repo <source> --copy-chunker-params
```

L'initialisation faite par l'operator ne le fait pas. Faites-le avant de créer la location,
ou acceptez qu'une destination mixte stocke certaines données deux fois.

## La suivre

```bash
kubectl get clusterbackupexternalsync offsite
```

```
NAME      MODE     PHASE       LAG   AGE
offsite   Mirror   Completed   0     6d
```

`LAG`, c'est `status.lagSnapshots` : les snapshots de la source sans copie à la destination.
Zéro est l'état stable. Un nombre qui croît d'exécution en exécution signifie que la
synchronisation ne suit pas — généralement la bande passante, parfois une destination
injoignable.

```bash
kubectl get clusterbackupexternalsync offsite \
  -o jsonpath='{.status.phase}{" lag="}{.status.lagSnapshots}{" copied="}{.status.snapshotsCopied}{" bytes="}{.status.bytesCopied}{"\n"}'
```

:::caution[`lagSnapshots: 0` n'est pas une vérification]
Cela dit que chaque snapshot de la source a une copie. Cela ne dit pas que ces copies sont
lisibles. Avant de vous appuyer sur une destination — et à plus forte raison avant de retirer
une source du service — lancez un `restic check` contre elle, puis faites un vrai restore
depuis elle. Voir
[Maintenance et vérification](/CrystalBackup/fr/docs/guides/maintenance/).
:::

## Ce que cela coûte

La première synchronisation déplace à peu près le volume que vous avez sélectionné. Ensuite,
seul le delta de blobs se déplace, contre une destination `Standard`.

Cette bande passante est le prix des deux propriétés que ce mécanisme achète : une
destination sous sa propre clé, et une sélectivité par namespace. Une réplication brute du
stockage objet coûterait moins cher et ne vous donnerait ni l'une ni l'autre — elle porte la
master key de la source jusqu'à la destination, ce qui, sur le plan namespace, mettrait la
clé de la plateforme à l'intérieur du silo d'un tenant, et elle ne fonctionne qu'au niveau
du repository entier.

## Planifier autour de la maintenance

Une synchronisation prend un verrou de lecture partagé sur la source et écrit la destination
sous un verrou non exclusif — exactement comme une backup. Seuls le `forget` et le `prune`
finaux de `Mirror` ont besoin de la file exclusive de la destination.

En pratique : planifiez la synchronisation pour qu'elle ne chevauche pas la fenêtre de prune
de la *destination*. Celle de la source importe moins, puisqu'une synchronisation lit au
travers.

## Les snapshots copiés à la destination

Ils conservent leurs `host`, `paths` et tags, si bien que la discovery à la destination les
projette exactement comme des snapshots natifs. Ils obtiennent de **nouveaux ID** — restic
les adresse par contenu sous la clé de la destination — ce qui explique l'existence du champ
`original` et pourquoi `Mirror` s'en sert.

Si la location de destination est enregistrée dans un cluster, un `kubectl get backups`
là-bas liste les copies comme restaurables, et elles le sont.

## Pas encore : les destinations immuables

`AppendOnly` est imposé quand la destination est `Immutable`. Le support d'Object Lock n'est
pas implémenté dans cette release, cette combinaison n'est donc pas quelque chose sur quoi
bâtir un plan — voir
[Quand ne pas le choisir](/CrystalBackup/fr/docs/discover/when-not-to-use/).

## Changer la clé d'un repository

L'external sync est aussi le mécanisme pour changer la clé d'un repository, parce que
`restic key remove` révoque un mot de passe d'accès mais ne fait jamais tourner la master
key. La procédure est : créer une destination avec la nouvelle clé, synchroniser en
`AppendOnly`, vérifier, basculer les schedules, puis retirer l'ancien repository du service.

Voir
[Maintenance et vérification](/CrystalBackup/fr/docs/guides/maintenance/#changer-la-clé-dun-repository).
