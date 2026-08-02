---
title: Quand ne pas le choisir
description: Les cas que Crystal Backup ne couvre pas, les coûts qu'il impose, et les situations où un autre outil est la meilleure réponse.
sourceFile: src/content/docs/discover/when-not-to-use.md
sourceHash: cfcd9a6d3d5944a564bb2935dc42141f54965e4b
---

Lisez ceci avant le démarrage rapide. Chaque point est une limitation réelle de la version
courante, pas une réserve ajoutée pour la forme.

## Il n'est pas encore durci pour la production

Les jalons M0 à M6 sont livrés et testés — suites unitaires et envtest, une suite de bout en
bout sur Kind, et une suite sur infrastructure réelle sur des clusters provisionnés, qui pour
`v0.6.0` a tourné sans filtre : 82 checks sur 82, aucun échec et aucun saut. Mais l'API des
CRD est en `v1alpha1` et bougera encore avant `1.0.0`, et deux des critères de sortie de M6
ne sont pas remplis — personne n'a fait tourner ceci deux semaines aux côtés d'un outil en
place, et il n'y a pas eu de déploiement pilote. Le résumé honnête est : **précoce, mais ce
n'est plus hypothétique.** Ce n'est pas la même chose qu'une version *« à faire tourner sans
surveillance sur des données que vous ne pouvez pas recréer »*.

Essayez-le dans un bac à sable. **Gardez vos backups existants.** Testez vos restores — ce
qui est une bonne pratique avec n'importe quel outil de backup, et qui ici est la pratique
sur laquelle le projet lui-même s'appuie.

## Ce qu'il ne fait pas du tout

**etcd et le control plane.** Crystal Backup capture les ressources applicatives et les
données des PVC. Il ne sauvegarde pas etcd. Un discours de « DR complet de la plateforme »
qui omet le control plane doit le dire, et le voici en train de le dire : il vous faut une
réponse séparée pour l'état propre de votre cluster.

**Les backups conscients de la base de données.** Il existe des hooks exec, et ils suffisent
à quiescer un système de fichiers ou à déclencher un checkpoint. Il n'y a pas d'agent base de
données, pas de log shipping, pas de point-in-time recovery entre deux snapshots. Si votre
Postgres a déjà un operator avec archivage des WAL, gardez-le — c'est un meilleur backup de
cette base que ne le sera jamais un snapshot de volume.

**Les quotas de stockage ou la refacturation.** Des métriques par tenant sont exposées ;
aucune comptabilité ni facturation n'en est faite, et il n'existe aucun mécanisme de quota.
Un namespace qui génère beaucoup plus de données que prévu est visible, pas borné.

**Le restore cross-cluster en self-service.** Un `Restore` namespacé est, par construction,
même cluster et même namespace. Restaurer le namespace d'un cluster dans un autre est une
opération d'administrateur via `ClusterRestore`. Il n'existe aucun mécanisme de délégation
permettant à un tenant de le faire lui-même.

**Les volumes en mode bloc.** Une PVC avec `volumeMode: Block` est rapportée comme un échec
par volume avec la raison `RestoreBlockUnsupported`. Elle n'est pas restaurée.

## Ce qui est spécifié mais pas livré

Ne bâtissez pas de plan autour de ces points. Ils existent dans la surface d'API ou dans les
documents de conception, et l'implémentation n'est pas dans cette version.

| Fonctionnalité | État |
|---|---|
| **Locations immuables** (S3 Object Lock) | `spec.mode: Immutable` est accepté et quelques garde-fous existent autour, mais le support d'Object Lock, la rotation de fenêtre et l'expiration **ne sont pas implémentés**. N'utilisez pas `Immutable` en attendant du WORM. |
| **La CLI `crystalctl` et l'UI de navigation** | Pas écrites. Il n'y a aucun outil en ligne de commande destiné aux utilisateurs dans cette version. |
| **Manifests de namespace via `ClusterRestore`** | Un `ClusterRestore` restaure les objets cluster-scoped et les données de volume. Restaurer par ce chemin les manifests de workload propres au namespace viendra plus tard. |

## Les coûts que vous acceptez

**Une seule fenêtre de prune à l'échelle du cluster.** Le repository partagé a une unique
fenêtre de maintenance exclusive pendant laquelle aucun namespace ne peut démarrer un
backup. Son usage mémoire croît avec le volume total du cluster, pas par namespace.
Planifiez-la hors des heures de pointe et bornez-la avec `pruneMaxRepackSize`.

**Aucun partage équitable entre tenants.** La concurrence des movers est plafonnée à
l'échelle du cluster par `maxConcurrentMovers`. Un namespace très mouvant peut **retarder**
les backups des autres namespaces. Il ne peut pas les lire, mais il peut les mettre en
retard.

**Les movers détiennent les credentials du repository, et rien ne les restreint.** Un Job de
mover s'exécute dans le namespace de l'opérateur, jamais dans celui d'un tenant : un
utilisateur de namespace ne peut ni lire son Secret, ni exec dans son pod, ni modifier le Job.
Ce qu'il détient, en revanche, ce sont les credentials de stockage objet de la location, avec
un accès complet au repository — et c'est structurel, pas un oubli : un repository restic est
adressé par contenu et dédupliqué entre namespaces, donc aucun objet du bucket n'appartient à
un namespace donné et aucune policy de stockage ne peut en découper un. Des credentials
restreints par tenant ne sont donc **pas prévus** : ils ne sont pas exprimables face à un
repository partagé. Le rayon de souffle d'un mover compromis est le repository, borné par la
NetworkPolicy d'egress du namespace opérateur. Donnez à l'opérateur des credentials déjà
limités à son bucket ou à son préfixe — rien en aval ne les restreindra pour vous.

**L'effacement est physique, pas cryptographique.** `ClusterErasure` exécute
`restic forget` par tag, suivi d'un `prune`. Il n'y a pas de crypto-shredding par tenant, et
il n'y en aura pas : un repository partagé a une seule clé maître, donc détruire une clé
détruit les données de tout le monde. Si votre régime de conformité exige la destruction
d'une clé par tenant, le plan cluster partagé n'est pas le mécanisme — donnez plutôt à chaque
tenant une location du plan namespace.

**Une clé du plan namespace perdue est irrécupérable.** Par conception, la plateforme ne
détient aucun slot de clé sur le repository d'un tenant. Si un tenant perd le mot de passe de
sa `BackupLocation`, ses backups sont perdus. Il n'y a aucun recours au support, parce qu'il
n'y a aucun mécanisme.

**La surface de coexistence est permanente.** Les deny-lists, les préfixes et l'alerting sur
le nombre de snapshots existent même sur un cluster qui ne fait tourner aucun autre outil de
backup.

## Vous voulez probablement autre chose si…

- **Vous n'avez besoin que d'un DR cluster-wide d'administrateur.** Velero est plus mature,
  plus largement déployé et dispose d'un corpus opérationnel bien plus vaste. Utilisez-le.
- **Vos namespaces ne partagent presque aucune donnée.** Tout l'intérêt d'un repository
  partagé unique est la déduplication à l'échelle du cluster. Sans données partagées, vous
  payez le coût de coordination — une fenêtre de prune, une clé — sans en tirer aucun
  bénéfice. Le modèle un repository par namespace de K8up est plus simple et convient mieux.
- **Il vous faut de l'immuabilité WORM aujourd'hui.** Object Lock n'est pas implémenté.
  Utilisez un outil qui l'a livré.
- **Il vous faut une expérience graphique de navigation et de téléchargement.** Il n'y a pas
  d'UI. Kasten K10 en a une bonne, au prix d'être propriétaire.
- **Votre cluster est en deçà de Kubernetes 1.30.** Le plancher supporté par le chart est
  1.30, parce que le modèle d'admission repose sur `ValidatingAdmissionPolicy`, qui y est GA.
- **Vos PVC sont sur un driver CSI sans support des snapshots.** Une telle PVC est *skipped*,
  avec `status.volumes[].phase: Skipped` et `reason: CSISnapshotUnsupported`. Elle est
  rapportée plutôt que silencieusement abandonnée, mais elle n'est pas sauvegardée.

## Toujours là ?

Alors [vérifiez les prérequis](/CrystalBackup/fr/docs/start/requirements/) et
[installez-le](/CrystalBackup/fr/docs/start/install/). Dans un bac à sable.
