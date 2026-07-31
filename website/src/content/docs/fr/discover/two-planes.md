---
title: Les deux plans
description: Quelles custom resources appartiennent à l'administrateur de plateforme, lesquelles appartiennent au propriétaire du namespace, et ce que chaque côté peut ou ne peut pas atteindre.
sourceFile: src/content/docs/discover/two-planes.md
sourceHash: 84fb6d83ffa1a24f2a39685fb1077060b7bde2d2
---

Crystal Backup découpe son API comme cert-manager sépare `ClusterIssuer` de `Issuer` : des
kinds cluster-scoped pour la plateforme, des kinds namespacés pour le tenant, pilotant le
même moteur d'exécution en dessous.

Les deux plans sont **additifs**. Un namespace protégé par le DR cluster peut aussi faire
tourner son propre schedule vers son propre bucket ; cette seconde copie ne remplace pas la
première.

## En un coup d'œil

| | Plan cluster | Plan namespace |
|---|---|---|
| Propriétaire | Administrateur de plateforme | Propriétaire du namespace |
| Portée | CRD cluster-scoped | CRD namespacés |
| Stockage | Un repository partagé par location | Un repository par location, dans le bucket de l'utilisateur |
| Clé | DEK de plateforme, wrappée par une KEK age détenue par l'admin | Le mot de passe restic de l'utilisateur |
| Isolation | Tags restic + un filtre `namespace=` dérivé par l'operator | Par construction — bucket, credentials et clé séparés |
| Finalité | Disaster recovery de la plateforme | Copie hors plateforme, restore en self-service |

## Les ressources du plan cluster

Toutes cluster-scoped, toutes réservées aux administrateurs.

| Kind | Nom court | Ce que c'est |
|---|---|---|
| `ClusterBackupLocation` | `cbl` | Le stockage objet et la clé de la plateforme ; adosse un repository partagé. |
| `ClusterBackupSchedule` | `cbs` | Un schedule cron qui frappe des exécutions `ClusterBackup` à partir d'un template. |
| `ClusterBackup` | `cb` | Une exécution de DR : fait un fan-out d'un `Backup` dans chaque namespace correspondant, et capture les ressources cluster-scoped. |
| `ClusterRestore` | `crst` | Un restore administrateur adressé par coordonnée de repository — fonctionne quand le namespace source n'existe plus. |
| `ClusterErasure` | `cer` | Suppression physique d'un tenant, d'un namespace ou d'une PVC dans une location. |
| `ClusterBackupExternalSync` | `cbes` | Réplication du repository partagé vers une seconde location. |
| `BackupRepository` | `br` | État interne à l'operator et inventaire d'un repository restic. Ce n'est pas quelque chose que vous écrivez. |

## Les ressources du plan namespace

Toutes namespacées. Un tenant disposant du ClusterRole `crystal-backup-user` livré obtient
l'ensemble complet des verbes sur les quatre premières, et le **lecture seule** sur
`Backup`.

| Kind | Nom court | Ce que c'est |
|---|---|---|
| `BackupLocation` | `bl` | Le stockage objet de l'utilisateur et sa propre clé. |
| `BackupSchedule` | `bs` | Un schedule cron qui frappe des objets `Backup` dans ce namespace. |
| `Backup` | `bk` | L'unité d'exécution **et** la projection d'un backup restaurable. En lecture seule pour les utilisateurs. |
| `Restore` | `rst` | Un restore en self-service de ce namespace. |
| `BackupExternalSync` | `bes` | Réplication entre deux `BackupLocation` de ce namespace. |

`Backup` est en lecture seule pour les tenants à dessein : c'est une projection du
repository, et la discovery en est propriétaire. En supprimer un ne supprime aucune donnée —
la discovery le projette à nouveau à la passe suivante.

## Ce que chaque côté peut atteindre

Un utilisateur de namespace :

- crée des schedules, des locations, des restores et des syncs **dans son propre
  namespace** ;
- peut restaurer depuis un backup de DR cluster, mais seulement via un `Restore` désignant
  un `Backup` d'origine cluster déjà projeté **dans son namespace** — l'operator fait l'accès
  au repository pour son compte ;
- ne détient jamais la clé du repository partagé, et ne fait jamais tourner de pod qui la
  détienne ;
- n'a accès à aucun kind cluster-scoped de Crystal Backup.

Un administrateur de plateforme :

- possède le repository partagé, sa clé et ses fenêtres de maintenance ;
- peut restaurer n'importe quel namespace n'importe où avec `ClusterRestore`, y compris dans
  un namespace qui n'existe pas encore ;
- détient la KEK age qui déwrappe la clé de plateforme — et la détient **hors** du cluster.

## Où le travail se fait réellement

Les movers tournent **uniquement** dans `crystal-backup-system`, jamais dans un namespace de
tenant, sur les deux plans. C'est ce qui garde les clés de repository et les credentials du
stockage objet hors des namespaces que le tenant contrôle, et c'est pourquoi un snapshot pris
dans un namespace de tenant est re-lié de façon centralisée plutôt que monté là où il a été
pris.

La mécanique est dans [Architecture](/CrystalBackup/fr/docs/understand/architecture/) ; le
raisonnement est dans
[Tenancy et isolation](/CrystalBackup/fr/docs/understand/tenancy/).

## Lequel voulez-vous ?

- **Vous exploitez la plateforme.** Commencez par le plan cluster : il protège des
  namespaces dont les propriétaires n'ont rien demandé, c'est-à-dire la plupart. Voir
  [Le plan cluster](/CrystalBackup/fr/docs/guides/cluster-plane/).
- **Vous possédez un namespace.** Le plan cluster vous protège déjà, mais la copie vit dans
  le bucket de la plateforme sous la clé de la plateforme. Si vous en voulez une qui n'y soit
  pas, voir [Le plan namespace](/CrystalBackup/fr/docs/guides/namespace-plane/).
- **Les deux.** C'est l'arrangement prévu.
