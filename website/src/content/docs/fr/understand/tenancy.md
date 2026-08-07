---
title: Tenancy et isolation
description: Ce qu'est réellement la frontière du tenant sur chaque plan, ce qui la fait tenir, et exactement ce qu'elle ne couvre pas.
sidebar:
  order: 3
sourceFile: src/content/docs/understand/tenancy.md
sourceHash: dda5df785da9bbdb1c49232bd308ef9eb076410a
---

Les revendications de multi-tenancy ne coûtent rien. Cette page énonce ce qui est appliqué,
par quel mécanisme, et ce qui n'est explicitement pas couvert.

## Le plan namespace : l'isolation par construction

Une `BackupLocation`, c'est le bucket du tenant, ses credentials, sa clé. Il n'y a rien à
faire appliquer parce qu'il n'y a rien de partagé.

La seule propriété qui mérite d'être précise : **la plateforme ne détient aucun slot de clé
sur ce repository, et il n'existe aucun moyen d'en demander un.** Le champ qui l'aurait
réclamé a été spécifié, à moitié implémenté, puis **retiré de l'API**.

Le raisonnement mérite d'être reproduit, parce qu'il explique toute une classe de décisions
dans ce projet. Supprimer un slot de clé restic ne fait **pas** tourner la clé maître. Un
slot plateforme aurait donc été permanent — un droit que le tenant n'aurait jamais pu
reprendre. La garantie « l'accès de la plateforme prend fin quand la clé de l'utilisateur
prend fin » est obtenue par l'absence du mécanisme, plutôt que par un webhook qu'un flag, ou
un futur mainteneur, pourrait désactiver.

Les conséquences, dans les deux sens :

- La plateforme ne peut pas lire le repository d'un tenant, ne peut pas médier un restore
  depuis celui-ci, et ne peut pas le vérifier pour lui.
- **Un tenant qui perd son mot de passe perd ses backups.** Aucun recours au support, parce
  qu'aucun mécanisme.

L'operator lit bien le Secret du mot de passe **par son nom** pour faire tourner les movers
du tenant. C'est un acte visible dans le journal d'audit de l'API, et il cesse de fonctionner
dès l'instant où le tenant fait tourner la clé ou supprime le Secret.

## Le plan cluster : un repository, une clé

Le repository partagé est initialisé avec une unique clé de données aléatoire de 256 bits,
stockée wrappée sous une identité age X25519 — la **KEK du cluster** — que l'administrateur
génère et séquestre hors du cluster. Ni l'operator ni le chart ne la fabriquent jamais.

Disons-le simplement, parce que le contraire est souvent sous-entendu : **le chiffrement
n'est pas la frontière du tenant ici.** Une seule clé maître ; quiconque la détient lit tous
les namespaces. Cette clé ne quitte jamais `crystal-backup-system`, et la détenir équivaut à
l'accès au niveau d'etcd qu'un administrateur possède déjà.

La frontière du tenant sur ce plan est autre chose.

## Le filtre non forgeable

Un `Restore` namespacé **n'a aucun champ qui pourrait désigner un autre namespace**. Pas de
`locationRef`, pas de namespace cible, pas d'identifiant de cluster.

Quand le `Backup` source est d'origine cluster, l'operator résout lui-même les snapshots :

1. Il construit un filtre restic à partir du `metadata.namespace` de la custom resource —
   `--tag crystalbackup,namespace=<celui-ci>` plus le tag de run. Des tags joints par des
   virgules dans un seul flag sont combinés par ET ; des flags répétés seraient combinés par
   OU, et cette distinction est porteuse.
2. Il liste le repository avec ce filtre.
3. **Seuls les IDs de snapshot que ce listing a renvoyés** sont transmis au mover de restore.
4. Une PVC que le listing filtré ne résout pas **échoue en position fermée**. Il n'y a aucun
   repli non filtré, à aucun moment.

La frontière ne dépend donc pas d'une politique d'admission qui tient, ni de la propre
correction de l'operator dans une branche, ni de RBAC correctement configuré. Elle dépend de
l'absence du champ dans l'API, et du fait que le filtre est dérivé de données que le tenant
ne peut pas écrire.

Le coût : un Job de listing supplémentaire, quelques secondes, avant que les données ne
bougent lors d'un restore d'origine cluster. Un restore du plan namespace ne paie rien — le
repository du tenant ne contient que ses données, les IDs de snapshot présents dans
`status.volumes` sont donc utilisés directement, en confiance.

## Où le travail s'exécute

Les movers tournent **uniquement** dans `crystal-backup-system`. Jamais dans un namespace de
tenant, sur aucun des deux plans.

C'est pourquoi l'exposer générique re-lie un `VolumeSnapshotContent` de façon centralisée
plutôt que de monter le snapshot là où il a été pris : `VolumeSnapshotContent` est
cluster-scoped, l'operator peut donc le lier dans son propre namespace, et les données du
tenant ne sont jamais montées quelque part où un voisin pourrait les atteindre.

Les credentials du stockage objet et les clés de repository vivent dans le namespace de la
location — `crystal-backup-system` pour le plan cluster, celui du tenant pour le plan
namespace — et parviennent aux movers sous forme de Secrets par Job dans le namespace de
l'operator. Un namespace de tenant reçoit des PVC restaurées, et rien d'autre.

Le ServiceAccount du mover de données a **zéro RBAC** et aucun token d'API du tout. La seule
exception est le mover de manifests, qui atteint l'API server parce que lire et écrire des
objets *est* son opération — et il est lié transitoirement, par Job, à un rôle de lecteur ou
d'écrivain.

## Les hooks : le problème d'identité

Les hooks sont le point sensible du self-service pour les tenants, parce qu'un hook, c'est un
tenant qui fait exécuter une commande par la plateforme.

Sur le plan namespace, l'operator n'exécute pas l'exec en son propre nom. Il **impersonate**
un ServiceAccount que le tenant désigne, dans le namespace sauvegardé, et l'API server
autorise chaque exec au regard de cette identité. L'invariant est :

> Les utilisateurs ne peuvent faire exécuter à la plateforme que des commandes qu'ils
> peuvent déjà exécuter eux-mêmes.

Trois propriétés en découlent : le tenant décide en accordant ou non ; la révocation est
immédiate parce que le contrôle a lieu à chaque exec et que rien n'est mis en cache ; et le
*namespace* n'est jamais un champ — seul le nom du ServiceAccount l'est. Un namespace
paramétrable serait un trou inter-tenant par construction.

Une exécution du plan namespace qui déclare des hooks sans identité est **bloquée**, avec la
raison `HooksNeedServiceAccount` — et non silencieusement élevée aux privilèges propres de
l'operator.

Sur le plan cluster, les hooks sont écrits par les administrateurs et peuvent omettre
l'identité, s'exécutant alors en tant qu'operator. C'est une relation de confiance
différente, énoncée plutôt que brouillée.

## RBAC

Pas un seul verbe joker, nulle part. La lecture, c'est `get, list, watch` ; le complet, c'est
l'ensemble de huit verbes.

`crystal-backup-tenant` accorde l'ensemble complet sur `backupschedules`, `backuplocations`,
`restores` et `backupexternalsyncs`, et le **lecture seule** sur `backups` — parce que
`Backup` est une projection gérée par l'operator et la discovery, pas quelque chose qu'un
tenant écrit. Il n'accorde rien de cluster-scoped.

`crystal-backup-admin` accorde les six kinds `cluster*` et le `backuprepositories` en lecture
seule, et — notez l'asymétrie — **rien** sur les kinds namespacés. Un administrateur qui a
aussi besoin de ceux-là doit se voir lier le rôle tenant en plus.

Ni l'un ni l'autre n'est lié par le chart. C'est la plateforme qui les lie.

## Les manifests contiennent des Secrets

`includeManifests` vaut `true` par défaut, et le snapshot de manifests stocke les objets
`Secret` du namespace. Sur le repository partagé, quiconque l'ouvre les lit — ce qui revient
à la clé de plateforme réservée aux administrateurs.

L'opt-out est `manifestOptions.excludeSecretData: true` : les Secrets sont stockés avec
`data` et `stringData` retirés et annotés
`crystalbackup.io/secret-data-excluded: "true"`, et le restore les recrée **vides**, portant
la même annotation. Un workload qui a besoin des valeurs échoue alors visiblement sur une clé
manquante, au lieu de démarrer silencieusement avec les mauvaises.

C'est un opt-out d'un défaut délibéré : une reprise complète de namespace a besoin des
Secrets. Les exclure échange de la récupérabilité contre un rayon de souffle plus réduit si
la clé venait à être compromise.

## Ce qui n'est pas couvert

**Les movers détiennent les credentials du bucket de la location.** Des credentials restreints
au repository et forgés par l'opérateur ne sont **pas prévus** : un repository partagé est
dédupliqué entre namespaces, donc aucune policy de stockage ne peut y découper les données d'un
namespace, et le scoping n'aurait produit aucune isolation entre tenants, quel qu'en soit
l'effort. Un mover compromis peut atteindre tout ce que les credentials de la location
peuvent atteindre — y
compris l'objet de clé wrappée séquestré, dont la protection est la KEK et non le chemin dans
le stockage objet. Restreignez les credentials côté stockage objet, et donnez à chaque
location les siens.

**Le crypto-shredding par tenant n'existe pas**, et n'existera pas sur le plan partagé. Une
seule clé signifie que la détruire détruit les données de tout le monde.
[L'effacement](/CrystalBackup/fr/docs/guides/erasure/) est physique : `restic forget` par
tag, puis `prune`.

**Aucun partage équitable entre tenants.** Un namespace bruyant peut **retarder** les backups
d'un autre via le plafond de movers à l'échelle du cluster. Il ne peut pas les lire.

**L'application des NetworkPolicy revient à votre CNI.** Le chart livre les objets. Certains
CNI les acceptent et n'appliquent rien.

**L'admission est un garde-fou, pas la frontière.** Les contrôleurs re-dérivent l'identité du
repository, le filtre de namespace et la confirmation au moment de l'exécution. Une politique
contournée dégrade l'expérience utilisateur ; elle ne perce pas la tenancy. Cet ordre —
structurel d'abord, politique ensuite — est bien le point.

## Voir aussi

- [Règles d'admission](/CrystalBackup/fr/docs/reference/admission/)
- [Choix de conception](/CrystalBackup/fr/docs/understand/design-choices/)
- [Quand ne pas le choisir](/CrystalBackup/fr/docs/discover/when-not-to-use/)
