---
title: Mettre à niveau
description: Mettre à niveau le chart, le problème des CRD que Helm ne résout pas pour vous, et ce que garantit l'API v1alpha1.
sidebar:
  order: 10
sourceFile: src/content/docs/guides/upgrading.md
sourceHash: 92038ae56fba7fd8268940ded1c05b67637baabf
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
# 0.6.1 is the current release.
helm pull oci://ghcr.io/crystalbackup/charts/crystal-backup --version 0.6.1 --untar
kubectl apply -f crystal-backup/crds/

# Then upgrade the operator.
helm upgrade crystal-backup \
  oci://ghcr.io/crystalbackup/charts/crystal-backup \
  --version 0.6.1 \
  --namespace crystal-backup-system
```

Un `kubectl apply` sur des CRD est additif et sûr : il ajoute les nouveaux champs et ne
supprime jamais d'objets stockés.

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
