---
title: Comment il se compare
description: Ce qui distingue Crystal Backup de Velero et de K8up — par les mécanismes, pas par les adjectifs.
sourceFile: src/content/docs/discover/comparison.md
sourceHash: 1249c1fb44cd61e8732f403068fcb7e3f6cdc1a0
---

Velero et K8up sont matures, largement déployés et bons dans ce qu'ils se sont donné pour
mission de faire. Cette page n'est ni un benchmark, ni un tableau de scores. Elle décrit
**quatre mécanismes** qui diffèrent, ainsi que ce que chacun rapporte et ce qu'il coûte,
pour que vous puissiez déterminer si la différence compte pour vous.

Les fonctionnalités évoluent. Vérifiez tout ce qui est écrit ici dans la documentation de
chaque projet avant de décider.

## 1. Deux plans plutôt qu'un seul public

**Velero** est orienté administrateur. `Backup` et `Restore` sont cluster-scoped : le
propriétaire d'un namespace ne peut pas en créer un pour son propre namespace sans se voir
accorder un pouvoir qui couvre aussi celui de tous les autres.

**K8up** est orienté namespace. `Schedule` et `Restore` sont namespacés, si bien qu'un
tenant est véritablement en self-service — mais il n'existe pas de contrepartie
cluster-scoped qui donnerait à une équipe plateforme un seul repository, une seule politique
de rétention et une seule posture de DR sur l'ensemble de la flotte.

**Crystal Backup** livre les deux, au-dessus du même moteur d'exécution.
`ClusterBackupSchedule` fait un fan-out dans les namespaces pour le DR de la plateforme ;
`BackupSchedule` appartient au tenant. Le même objet `Backup` est l'unité d'exécution quel
que soit le plan qui l'a créé, il n'y a donc qu'un seul chemin de code à qui faire
confiance, et non deux.

*Le coût :* deux plans, c'est davantage de surface d'API, et une équipe plateforme doit
décider quels namespaces sont couverts par lequel.

## 2. Un repository partagé, avec une tenancy portée par les tags

**K8up** donne à chaque namespace son propre repository. L'isolation est trivialement
parfaite, et la déduplication s'arrête à chaque frontière de namespace — cinquante
namespaces faisant tourner la même image de base la stockent cinquante fois.

**Velero**, avec son backup au niveau système de fichiers, écrit lui aussi dans un
repository par namespace.

**Crystal Backup**, sur le plan cluster, écrit chaque namespace dans **un seul** repository,
les tags restic `tenant=`, `namespace=`, `pvc=` portant la tenancy. La déduplication est à
l'échelle du cluster.

*Le coût, et il est réel :* un seul repository signifie une seule fenêtre exclusive de
`prune` pour tout le cluster, et une mémoire de prune qui croît avec le volume total du
cluster plutôt que par namespace. Planifiez-la hors des heures de pointe et bornez-la avec
`pruneMaxRepackSize`. Cela signifie aussi que la clé du repository est un secret à l'échelle
du cluster — voir la section suivante pour ce qui en est fait.

## 3. Le confinement du tenant est structurel, pas un contrôle de politique

C'est le mécanisme le plus utile à comprendre, parce que c'est là que « multi-tenant »
devient d'ordinaire une promesse plutôt qu'une propriété.

Un `Restore` namespacé dans Crystal Backup **n'a aucun champ qui pourrait désigner un autre
namespace**. Pas de `locationRef`. Pas de namespace cible. Pas d'identifiant de cluster.
Lorsque la source est un backup de DR cluster, l'operator liste lui-même le repository avec
un filtre restic construit à partir du `metadata.namespace` de la custom resource, et ne
transmet au mover que les IDs de snapshot que ce listing a renvoyés. Une PVC que le listing
filtré ne résout pas **échoue en position fermée** ; il n'existe aucun repli non filtré.

La frontière du tenant ne dépend donc pas d'une politique d'admission qui tient, ni de
l'operator qui tourne, ni de RBAC correctement configuré. Elle dépend de l'absence du champ
dans l'API.

Ce que cela ne prétend **pas** : le chiffrement n'est pas la frontière du tenant sur le
repository partagé. Il y a une seule clé maître, et quiconque la détient lit tous les
namespaces. Cette clé est réservée aux administrateurs et ne quitte jamais
`crystal-backup-system` — équivalente à l'accès qu'un administrateur a déjà via etcd. S'il
vous faut une séparation cryptographique entre tenants, utilisez le plan namespace, où le
repository de chaque tenant a une clé différente que la plateforme ne détient pas.

## 4. Un disaster recovery qui part du bucket

**Velero** sait restaurer depuis un bucket vers un cluster neuf, et le fait bien ; ses
métadonnées de backup vivent dans le stockage objet aux côtés des données.

**K8up** et **VolSync** sont des outils de réplication de volumes ; reconstruire un
namespace à partir de leurs repositories est un exercice manuel.

**Crystal Backup** traite le repository comme la source de vérité et les objets Kubernetes
comme une projection de celui-ci. Concrètement :

- Pointez un operator neuf sur un bucket existant : une passe de discovery l'inventorie et
  projette des objets `Backup` dans les namespaces qui existent. Aucune custom resource
  préexistante n'est nécessaire.
- `ClusterRestore` s'adresse à une **coordonnée de repository** — location, namespace
  d'origine, run — et fonctionne donc quand le namespace, le schedule et tous les objets
  `Backup` ont disparu.
- La capacité, la storage class et les access modes des PVC sont récupérés depuis des tags
  restic enregistrés au moment du backup (`pvcsize`, `pvcclass`, `pvcmodes`), si bien que
  les PVC reviennent correctement dimensionnées sans que rien n'ait survécu pour les
  décrire.
- La clé de plateforme wrappée est séquestrée **dans le bucket**, à
  `<prefix>/<clusterID>.crystal-meta/wrapped-dek.age` — un chiffré sous votre propre KEK,
  inutile à quiconque ne détient que le bucket.

L'entrée nécessaire à la reprise est donc : le bucket, plus la KEK age que vous avez
séquestrée hors du cluster. Rien d'autre.

*Ce qui est hors périmètre :* etcd et le control plane. Crystal Backup restaure les
ressources et les données applicatives, pas le cluster lui-même.

## Réversibilité

Le mode restic/kopia de Velero, K8up, VolSync et Crystal Backup écrivent tous des formats
que vous pouvez ouvrir avec un outil upstream. La différence de Crystal Backup est l'absence
d'enveloppe : le repository est un simple repository restic, sans catalogue annexe, et
l'agencement est documenté. Avec les credentials du bucket et la clé,
`restic -r s3://bucket/prefix/<clusterID> snapshots` liste vos backups sans qu'aucun
composant de Crystal Backup n'intervienne.

C'est aussi une contrainte délibérée sur le projet : tout ce qui exigerait son propre index
à côté du repository n'est pas construit.

## Coexistence

Crystal Backup est conçu pour tourner **à côté** d'un outil déjà en place, indéfiniment. Il
n'y a pas de phase de migration ni de checklist de parité. Concrètement : un groupe d'API
distinct (`crystalbackup.io`), un namespace distinct (`crystal-backup-system`), des
credentials et des repositories distincts, sa propre sélection de `VolumeSnapshotClass`
qu'il ne modifie jamais, et chaque `VolumeSnapshot` qu'il crée porte un préfixe de nom
`crystal-` et son propre label, de sorte que le garbage collection ne touche jamais que ses
propres objets.

*Le coût :* faire tourner deux outils signifie deux pipelines de snapshot et à peu près le
double de trafic d'upload pendant toute période de recouvrement.

## Le résumé honnête

Si vous avez besoin d'un DR cluster-wide piloté par les administrateurs et de rien d'autre,
Velero est plus mature et c'est lui que vous devriez utiliser. Si vous avez besoin de
backups restic par namespace et que vos namespaces ne partagent pas grand-chose, K8up est
plus simple et c'est lui que vous devriez utiliser.

Crystal Backup est pour le cas où vous avez besoin des *deux publics à la fois* — une
posture de DR de plateforme et un vrai self-service pour les tenants — et où vous voulez que
la frontière du tenant soit une propriété de l'API plutôt qu'une configuration que vous avez
à maintenir juste.

Voir aussi [Quand ne pas le choisir](/CrystalBackup/fr/docs/discover/when-not-to-use/).
