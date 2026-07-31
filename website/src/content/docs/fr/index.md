---
title: Ce qu’est Crystal Backup
description: Un operator Kubernetes pour le backup et le restore multi-tenant en self-service de namespaces — données des PVC et manifests — dans de simples repositories restic.
sourceFile: src/content/docs/index.md
sourceHash: 9fd2e1d553738dd1dd2392a2b407dafe64b9e389
---

Crystal Backup est un operator Kubernetes qui sauvegarde et restaure des **namespaces** —
à la fois les **données des PVC** et les **manifests Kubernetes** — sur deux plans :

- un **plan cluster**, où un administrateur de plateforme protège tous les namespaces (ou
  une sélection) dans un repository partagé unique, pour le disaster recovery de la
  plateforme ;
- un **plan namespace**, où la personne qui possède un namespace le sauvegarde **une
  seconde fois**, dans **son propre bucket sous sa propre clé**, hors du périmètre de
  confiance de la plateforme.

Tout est écrit au format de repository [restic](https://restic.net) standard. Avec les
credentials du stockage objet et la clé, `restic` lui-même lit vos backups. Il n'y a aucun
catalogue propriétaire, et rien n'a besoin de survivre pour que les données restent
lisibles.

## Le problème qu'il traite

Les plateformes Kubernetes managées et multi-tenant sont généralement isolées par
namespace : une équipe possède un ou plusieurs namespaces et y est en self-service via
RBAC. Ces plateformes font couramment tourner un unique outil de backup cluster-wide, en
filet de sécurité réservé aux administrateurs.

Cet arrangement laisse les équipes qui possèdent réellement les workloads avec :

- **aucun self-service** — elles ne peuvent pas prendre un backup avant une migration
  risquée, et ne peuvent pas restaurer sans ouvrir un ticket ;
- **aucune visibilité** — elles ne peuvent pas savoir si leurs données sont protégées, ni
  ce qu'un restore ramènerait réellement ;
- **aucune sortie de la plateforme** — chaque copie de leurs données se trouve dans le même
  périmètre de confiance que le cluster contre lequel elles cherchent à être protégées.

Crystal Backup vise cette combinaison. Il n'est explicitement **pas** un remplaçant de
l'outil cluster-wide que vous faites déjà tourner ; il est conçu pour vivre à côté.

## Les quatre propriétés qui portent la conception

Tout le reste en découle, et chacune est une propriété que vous pouvez vérifier plutôt
qu'un adjectif.

### Un repository partagé unique, la tenancy portée par les tags

Le plan cluster écrit chaque namespace dans **un seul** repository restic par location. La
tenancy à l'intérieur est portée par des tags restic — `tenant=`, `namespace=`, `pvc=` — et
non par un repository par namespace. C'est ce qui permet à la déduplication de fonctionner à
l'échelle du cluster entier, au lieu de repartir de zéro à chaque frontière de namespace.

### Un filtre de namespace qu'un tenant ne peut pas forger

Un `Restore` namespacé désigne un `Backup` **de son propre namespace**. Il n'a pas de
`locationRef`, pas de champ pour le namespace cible et pas d'identifiant de cluster — il
n'existe aucun champ d'API par lequel un autre namespace pourrait être nommé. Lorsque la
source est un backup de DR cluster, l'operator résout les snapshots lui-même, avec un filtre
restic construit à partir du `metadata.namespace` de la custom resource, et seuls les IDs de
snapshot que ce filtre renvoie sont un jour transmis à un mover.

Le confinement est structurel : il tient parce que le moyen d'exprimer l'autre cas n'existe
pas, et non parce qu'un contrôle le rejette.

### Une discovery issue du repository, qui survit au cluster

Les objets `Backup` sont une **projection** du repository restic, pas la source de vérité.
Pointez un operator neuf sur un bucket existant : il inventorie ce qui s'y trouve et en
projette des objets `Backup` — sans aucune custom resource préexistante, et sans qu'aucun
cluster n'ait eu besoin de survivre. `kubectl get backups -n <ns>` liste donc exactement ce
qui est restaurable dans ce namespace : supprimez un `Backup` et la discovery le recrée ;
laissez un snapshot expirer et la projection disparaît.

### La plateforme ne détient aucune clé sur le repository d'un tenant

Un repository du plan namespace a exactement **un** slot de clé : celui de l'utilisateur.
Aucun champ d'API ne peut réclamer un slot pour la plateforme, parce que le champ a été
supprimé plutôt que gardé. Supprimer un slot de clé restic ne fait pas tourner la clé
maître, si bien qu'un slot plateforme aurait été permanent — et une garantie obtenue par
l'absence d'un mécanisme vaut plus qu'une garantie obtenue par un webhook que quelqu'un peut
désactiver.

La conséquence directe, dite simplement : **un utilisateur qui perd le mot de passe de sa
`BackupLocation` perd ses backups.** Il n'existe aucune copie côté plateforme.

## Où aller ensuite

- [Les deux plans](/CrystalBackup/fr/docs/discover/two-planes/) — qui possède quoi, et
  quelle custom resource appartient à qui.
- [Comment il se compare](/CrystalBackup/fr/docs/discover/comparison/) — face à Velero et
  K8up, sur les axes où ils diffèrent réellement.
- [Quand ne pas le choisir](/CrystalBackup/fr/docs/discover/when-not-to-use/) — à lire avant
  le démarrage rapide.
- [État du projet](/CrystalBackup/fr/docs/discover/status/) — ce qui est livré, ce qui ne
  l'est pas, et jusqu'où lui faire confiance aujourd'hui.
