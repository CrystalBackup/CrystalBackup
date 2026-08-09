---
title: Mettre à niveau
description: Mettre à niveau le chart, le problème des CRD que Helm ne résout pas pour vous, et ce que garantit l'API v1alpha1.
sidebar:
  order: 10
sourceFile: src/content/docs/guides/upgrading.md
sourceHash: 2ce168bf10f8dc437db402d573884fc6a74a097c
---

## Ce que signifie un numéro de version ici

Le projet suit SemVer sur la majeure `0` : chaque milestone est une release **mineure**
(`M_n` → `0.n.z`), les itérations de durcissement sont des **patches**. Une seule chaîne de
version couvre l'image de l'operator, celle du mover, celle du sync et l'`appVersion` du
chart — c'est un seul train de release, et ils sont censés avancer ensemble.

L'API des CRD est en **`v1alpha1`**, et ce n'est pas une formalité. Des champs peuvent être
ajoutés, renommés ou supprimés d'une release mineure à l'autre jusqu'à la `1.0.0`, ce qui est
une décision délibérée de stabilité d'API prise après M9. Lisez les notes de release avant
chaque montée de mineure ; ne supposez pas qu'un manifest qui s'appliquait contre la `0.5`
s'applique contre la `0.6`.

Les releases de patch ne changent pas l'API.

## Le problème des CRD

**Helm installe les CRD à la première installation et ne les met jamais à niveau.** C'est le
comportement de Helm, pas un choix de ce chart, et cela veut dire qu'un `helm upgrade` seul
vous laissera avec un operator neuf sur de vieilles CRD — ce qui échoue de la façon la plus
déroutante possible : les champs que vous renseignez sont silencieusement élagués par l'API
server, et l'operator réconcilie comme si vous ne les aviez jamais renseignés.

Appliquez les CRD vous-même, avant le chart :

```bash
# Pull the chart and take its CRDs. Use the version you are upgrading *to*;
# 0.6.4 is the current release.
helm pull oci://ghcr.io/crystalbackup/charts/crystal-backup --version 0.6.4 --untar
kubectl apply -f crystal-backup/crds/

# Then upgrade the operator.
helm upgrade crystal-backup \
  oci://ghcr.io/crystalbackup/charts/crystal-backup \
  --version 0.6.4 \
  --namespace crystal-backup-system
```

Un `kubectl apply` sur des CRD est additif et sûr : il ajoute les nouveaux champs et ne
supprime jamais d'objets stockés.

## 0.6.3 → 0.6.4 : une location qui rapportait Ready peut désormais rapporter Degraded

Rien à faire avant la mise à niveau, et aucune donnée ne bouge — mais le nouvel operator peut
rapporter une location comme non-Ready là où l'ancien la rapportait saine. Lisez ceci avant d'en
conclure que la mise à niveau a cassé quelque chose.

La `0.6.4` fait en sorte que la passe d'escrow de la DEK wrappée **bloque le provisioning du
repository dans tout état qu'elle ne peut pas positivement prouver sûr**. Deux états sont sûrs :
une DEK in-cluster existe déjà, donc rien ne peut être frappé ; ou il n'y a prouvablement aucune
DEK nulle part, donc frapper une DEK est ce qui doit arriver. Tout le reste bloque désormais et
pose `Ready=False` avec la raison `DEKEscrowUnresolved`, la phase `Degraded`, et la condition
`DEKEscrowed` qui porte le cas exact :

```bash
kubectl get clusterbackuplocations
kubectl get clusterbackuplocation <name> \
  -o jsonpath='{range .status.conditions[?(@.type=="DEKEscrowed")]}{.reason}: {.message}{"\n"}{end}'
```

Si l'une passe en `Degraded` à la première réconciliation après la mise à niveau, **l'état
qu'elle nomme était déjà vrai en `0.6.3`** — il ne bloquait simplement rien, ce qui est
précisément le défaut que cette release corrige. Trois cas méritent d'être connus :

- **`EscrowConflict`** — l'objet du bucket et la DEK in-cluster sont tous deux lisibles et sont
  des clés différentes. Ce sont deux générations de repository, et la copie du bucket peut être
  la seule clé de la plus ancienne. En `0.6.3`, une telle location rapportait `Ready` tout en
  donnant une mauvaise clé à chaque mover. Ne supprimez rien : l'objet du bucket est une preuve.
- **`EscrowUnreachable`** — le bucket n'a pas pu être lu et il n'y a pas de DEK locale, donc une
  clé récupérable peut s'y trouver. En général des credentials ou un endpoint, et cela se résorbe
  tout seul dès que le bucket redevient joignable. À distinguer de **`EscrowUnverifiable`**, qui
  est la même panne d'E/S *avec* une DEK locale présente et qui, elle, ne bloque **pas**, puisque
  il n'y a rien à frapper.
- **`CredentialsUnavailable`** / **`KEKUnavailable`** — le Secret manque. Restaurez-le ; la
  location se rétablit sans intervention.

`EscrowWriteFailed` ne bloque toujours pas : la DEK in-cluster est connue bonne et seule la copie
du bucket est en retard, ce qui dégrade la DR à froid plutôt que vos backups.

## 0.6.2 → 0.6.3 sous Argo CD : un objet cesse d'être rendu

Lisez ceci avant de synchroniser, pas après. C'est arrivé sur un vrai cluster.

En `0.6.2`, le chart rendait un objet `Namespace` par défaut (`namespace.create` valait `true`
par défaut). En `0.6.3`, ce défaut est `false`, et l'objet a simplement disparu du rendu. Sous
Argo CD avec le prune automatique, **un objet qui cesse d'être rendu est un objet qui est
supprimé** — c'est ce que veut dire le prune, et il ne distingue pas « l'auteur a retiré ceci »
de « l'auteur a changé le défaut ». Une synchronisation `0.6.2` → `0.6.3` peut donc supprimer
`crystal-backup-system` et tout ce qu'il contient, y compris le Secret qui porte votre cluster
KEK et toutes les DEK wrappées. Rien n'est touché dans le stockage objet, et chaque repository
que ces clés protègent devient définitivement illisible — un
[decommission](https://github.com/CrystalBackup/CrystalBackup/blob/main/docs/DECOMMISSION.md#14-the-key-itself)
exécuté par accident, pendant une montée de patch.

Le remède consiste à sortir le namespace de l'ensemble prunable de l'Application **avant** la
mise à niveau : arrêtez de le suivre dans l'`Application` de l'operator — une Application à lui
seul, avec le prune désactivé, ou une exclusion du périmètre de synchronisation. Dès lors que le
namespace n'est plus quelque chose que cette Application rend, aucun changement de
`namespace.create` ne peut l'atteindre. Ensuite, mettez à niveau. Le raisonnement, et la forme à
employer, sont dans
[Installer avec Argo CD](/CrystalBackup/fr/docs/start/install-argocd/#le-namespace--le-vôtre-pas-celui-du-chart).

Le même danger vaut pour un `HelmRelease` Flux avec le pruning activé, et pour tout autre
réconciliateur qui traite « n'est plus rendu » comme « à supprimer ». Après la mise à niveau,
`namespace.create` devrait rester à `false` définitivement : un namespace que Helm possède est un
namespace qu'un prune ou un `helm uninstall` peut emporter, avec les clés dedans.

## Avant de mettre à niveau

**1 — Laissez le travail en cours se terminer.** Une mise à niveau redémarre l'operator, ce
qui est sûr par conception — les Jobs de mover ont des noms déterministes et sont ré-adoptés
plutôt que relancés — mais il n'y a aucune raison de le faire pendant une fenêtre de prune.

```bash
kubectl -n crystal-backup-system get jobs -l app.kubernetes.io/managed-by=crystal-backup
kubectl get clusterbackups
kubectl get restores,clusterrestores -A
```

**2 — Sachez où sont vos clés.** Une mise à niveau n'y touche pas, mais le moment où vous
découvrez que votre séquestre de KEK est périmé ne devrait pas être celui où vous en avez
besoin.

**3 — Lisez les notes de release.** Particulièrement pour une montée de mineure.

## Pendant

```bash
kubectl -n crystal-backup-system rollout status deploy/crystal-backup
```

L'operator redémarre. Si vous faites tourner plus d'un replica, l'élection de leader en
maintient exactement un actif et les autres en standby chaud, si bien que le redémarrage est
un passage de témoin plutôt qu'une interruption de service.

Rien dans le stockage objet n'est touché par une mise à niveau. Les repositories ne sont pas
migrés, les clés ne sont pas tournées, et aucune donnée ne bouge.

## Après

```bash
# The operator is up on the new version.
kubectl -n crystal-backup-system get deploy crystal-backup \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'

# Locations are still Ready.
kubectl get clusterbackuplocations
kubectl get backuplocations -A

# Repositories are still reachable.
kubectl get backuprepositories
```

Puis laissez une backup planifiée s'exécuter et vérifiez qu'elle s'est terminée. Une mise à
niveau que vous n'avez pas vérifiée avec une vraie backup est une mise à niveau que vous
n'avez pas vérifiée.

## Redescendre de version

Non supporté, et cela mérite d'être dit sans détour. Les nouveaux champs de CRD écrits par un
operator plus récent sont inconnus d'un plus ancien ; l'API server les élague à la prochaine
écriture, et l'operator plus ancien réconcilie contre un spec tronqué.

S'il vous faut vraiment revenir en arrière : désinstallez, ré-appliquez les anciennes CRD,
réinstallez l'ancien chart, et recréez vos custom resources. Les **repositories ne sont pas
affectés** — c'est tout l'intérêt du fait que le repository fasse foi. La discovery
reprojettera les backups.

Suivez la [procédure de désinstallation](/CrystalBackup/fr/docs/start/install/#désinstaller)
ordonnée pour cette première étape. Retirer l'operator alors qu'un `Backup`, un `Restore` ou
une location porte encore son finalizer laisse le namespace en `Terminating` de façon
permanente, et une descente de version est exactement le moment où vous supprimerez des
objets dans la précipitation.

## Franchir plusieurs mineures

Avancez d'une mineure à la fois (`0.3` → `0.4` → `0.5`), en appliquant les CRD de chaque
release et en laissant un cycle de backup s'achever entre deux. Sauter des mineures sur une
API alpha, c'est la manière de découvrir de quelle migration vous aviez besoin.

## Les images

En production, les images sont référencées **par digest**, jamais par tag. Les values
publiées du chart portent les vrais digests de la release ; un chart installé depuis un
checkout des sources porte un placeholder et ne pullera pas.

```bash
kubectl -n crystal-backup-system get pods \
  -o jsonpath='{range .items[*]}{.spec.containers[0].image}{"\n"}{end}'
```

Les images du mover et du sync sont épinglées séparément et sont passées à chaque Job de
mover. Elles avancent avec l'operator, si bien qu'une mise à niveau partielle — nouvel
operator, ancien digest de mover — est à éviter plutôt qu'à tenter.
