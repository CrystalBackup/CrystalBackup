---
title: La cascade
description: Pourquoi Backup est à la fois l'unité d'exécution et la projection d'un backup restaurable, et ce qui en découle.
sidebar:
  order: 2
sourceFile: src/content/docs/understand/cascade.md
sourceHash: 558bcc1a05397a5ef9ec440272a46b59b4cba7cd
---

La cascade a la forme d'un CronJob, et c'est délibéré :

```
ClusterBackupSchedule ──cron + template──▶ ClusterBackup ──fan-out──▶ Backup ──per PVC──▶ movers
BackupSchedule ─────────cron──────────────────────────────────────▶ Backup ──per PVC──▶ movers
```

- **`ClusterBackupSchedule`** frappe des exécutions `ClusterBackup` à partir d'un template,
  bornées par `successfulRunsHistoryLimit` et `failedRunsHistoryLimit`.
- **`ClusterBackup`** résout son sélecteur de namespaces et crée un `Backup` dans chaque
  namespace correspondant, plus un snapshot des ressources cluster-scoped.
- **`BackupSchedule`** frappe directement des objets `Backup`, sans fan-out.
- **`Backup`** est l'unique unité d'exécution, pilotée à l'identique quel que soit le plan
  qui l'a créée, et elle pilote un Job de mover par PVC.

## Un objet, deux métiers

`Backup` est l'unité d'exécution **et** la projection d'un backup restaurable. C'est
inhabituel, et c'est de là que viennent la plupart des conséquences de la conception.

Deux producteurs écrivent des objets de ce kind :

- le fan-out d'exécution, qui connaît la configuration de l'exécution ;
- la **discovery**, qui reconstruit les objets à partir des seuls snapshots restic.

La seule entrée de la discovery est le repository. Elle peut reconstruire la location dans
laquelle vit un backup (le repository *est* la location) et ses résultats par volume (depuis
les tags). Elle **ne peut pas** reconstruire un sélecteur de PVC, une option de manifests ni
une commande de hook — rien de tout cela n'a jamais été écrit dans restic, et rien de tout
cela ne le sera jamais.

## « Le backup porte l'identité, pas l'intention »

Cette contrainte donne la règle qui façonne `Backup.spec` : un champ qu'une projection ne
peut pas reproduire ne doit pas vivre à un endroit dont une projection est propriétaire.

Ainsi, `spec` porte l'**identité** — quel repository, quel schedule l'a frappé — et la
configuration de l'exécution est **matérialisée** dans `spec.run` par celui qui a créé
l'objet, une fois, à la création. Elle n'est pas relue depuis un parent à chaque
réconciliation.

Deux choses en découlent :

- **Un `Backup` s'exécute même quand son parent a disparu.** La configuration a été recopiée
  vers le bas ; le schedule peut être supprimé, le compte rendu d'exécution élagué, et le
  backup sait toujours ce qu'on lui a demandé de faire.
- **La discovery ne revendique jamais `spec.run`.** Sous le server-side apply, un
  propriétaire qui revendique un champ qu'il ne peut pas reproduire se bat pour toujours avec
  le contrôleur d'exécution au sujet de l'objet. Les projections le laissent donc absent — et
  une annotation `crystalbackup.io/projected` les rend inertes de toute façon.

La distinction du pointeur compte ici aussi : absent signifie « cet objet est antérieur à la
matérialisation, se rabattre sur le parent », tandis qu'une structure vide signifie
« matérialisé, chaque bouton à sa valeur par défaut ». Confondre les deux casserait soit les
anciens objets, soit relirait silencieusement les valeurs par défaut comme une instruction
d'aller chercher un parent.

## Des conséquences observables

**Un `Restore` ne nomme jamais une location.** Il nomme un `Backup` de son propre namespace,
et le `Backup` sait où il vit. C'est pourquoi `Restore` n'a pas de `locationRef` — cette
absence n'est pas un oubli, c'est la cascade qui fait son travail.

**L'historique des exécutions et la restaurabilité sont découplés.** Un `Backup` enfant est
lié à son `ClusterBackup` par le **label** `crystalbackup.io/cluster-backup`, pas par une
ownerReference. Élaguer les comptes rendus d'exécution à `successfulRunsHistoryLimit` ne
supprime donc jamais un backup restaurable. (Cela ne pouvait de toute façon pas être une
ownerReference : un objet namespacé ne peut pas être possédé par un objet cluster-scoped —
Kubernetes traiterait la référence comme pendante et ferait le garbage collection de
l'enfant.)

**Supprimer un `Backup` ne supprime rien.** La discovery le projette à nouveau à la passe
suivante. Le repository est la source de vérité ; l'objet en est une vue. Inversement,
l'expiration d'un snapshot fait disparaître la projection — c'est pourquoi
`kubectl get backups` continue de dire la vérité sur ce qui est restaurable.

**`ClusterBackup.status` est agrégé, et rien d'autre.** Des compteurs plus une liste
d'échecs **plafonnée**. Une map par namespace non bornée, sur un cluster de 500 namespaces,
produit un objet qui finit par ne plus pouvoir être écrit du tout, et un statut qui ne peut
pas être écrit perd tout le compte rendu, pas seulement sa fin.

**La visibilité des tenants, c'est RBAC natif.** Parce que `Backup` est namespacé, un
utilisateur qui liste les backups voit exactement les siens. Aucune couche de filtrage,
aucune vue, aucun proxy — juste RBAC faisant ce que RBAC fait.

**Un `Backup` n'est pas un enregistrement d'audit de ce qui a tourné.** Modifier un schedule
change la configuration apparente des exécutions terminées, puisqu'elles le référencent. Tout
ce qui doit être auditable par exécution vit dans `status`, écrit au moment de l'exécution :
`status.volumes`, `status.hooks`, `status.manifests`.

## Le nommage

Une exécution planifiée est nommée `<schedule>-<YYYYMMDD-HHMMSS>` en UTC. Cette chaîne
exacte est :

- le nom de l'objet `ClusterBackup`,
- le nom de chaque `Backup` enfant, dans chaque namespace,
- le tag restic `run`.

Un seul identifiant, de bout en bout. C'est ce qui permet à la discovery et au fan-out de
converger sur le même objet, indexé par `(namespace, run)`, sans coordination — et c'est ce
qui rend ceci possible :

```bash
kubectl get backups -A -l crystalbackup.io/cluster-backup=dr-daily-20260730-020000
restic -r $REPO snapshots --tag crystalbackup,run=dr-daily-20260730-020000
```

## Là où les deux plans diffèrent

La configuration d'exécution partagée — sélecteur de PVC, options de manifests, hooks,
backoff limit — est un seul type, déclaré une fois et utilisé par les deux plans. Le plan
cluster l'inline à côté de ses champs de fan-out ; le plan namespace la déclare sur le
schedule exposé au tenant.

Un champ est délibérément **absent** de la surface offerte au tenant :
`maxConcurrentMovers`. C'est un plafond à l'échelle du cluster, vérifié contre chaque Job de
mover du namespace de l'operator ; un tenant qui le fixerait fixerait donc une limite
valable pour toute la plateforme.

Cette asymétrie est la règle générale de ce qui a sa place sur une surface exposée au
tenant. Chaque champ qu'on y ajoute devient quelque chose qu'un utilisateur de namespace peut
faire faire à l'operator pour son compte — c'est bien pourquoi les hooks, en particulier, ont
exigé tout un mécanisme d'identité avant de pouvoir être exposés. Voir
[Tenancy et isolation](/CrystalBackup/fr/docs/understand/tenancy/).
