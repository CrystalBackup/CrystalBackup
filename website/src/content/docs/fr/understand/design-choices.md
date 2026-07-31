---
title: Choix de conception
description: Les décisions qui déterminent ce que Crystal Backup peut et ne peut pas faire, et ce que chacune a coûté.
sidebar:
  order: 4
sourceFile: src/content/docs/understand/design-choices.md
sourceHash: 09e815b245c1dc85319dda7bcd3e8993e5bb8d00
---

Chacune de ces décisions est un arbitrage. Le raisonnement est public dans son intégralité —
dix-huit
[décisions d'architecture](https://github.com/CrystalBackup/CrystalBackup/tree/main/spec/adr)
précèdent le code. Cette page est la version courte de celles que vous allez ressentir.

## restic comme format de repository

**Choisi parce que** c'est un format simple, documenté, largement déployé, avec une
déduplication à découpage défini par le contenu, de la compression et du chiffrement AES-256,
que n'importe qui peut lire avec un binaire upstream. La réversibilité cesse d'être une
promesse et devient une propriété.

**Le coût.** Le projet hérite du modèle de restic, y compris de son verrouillage. Chaque
opération exclusive prend un lock sur le repository, et la forme du `prune` — une fenêtre
exclusive, une mémoire proportionnelle à la taille du repository — est la forme de restic,
pas un choix. Cela contraint aussi ce qui peut être construit : tout ce qui exigerait un
index privé à côté du repository est exclu, parce que cet index serait le verrouillage
propriétaire.

## Un repository partagé par location cluster

**Choisi parce que** la déduplication à l'échelle du cluster entier vaut très cher —
cinquante namespaces faisant tourner la même image de base la stockent une fois — et parce
que des repositories par namespace multiplient la surface de maintenance par le nombre de
namespaces.

**Le coût, et c'est le plus grand de la conception.** Un seul repository signifie une seule
clé, donc le chiffrement ne peut pas être la frontière du tenant et le crypto-shredding par
tenant est impossible. Cela signifie une unique fenêtre de prune exclusive à l'échelle du
cluster, dont la mémoire croît avec le volume total de données. Et cela signifie de nombreux
movers en contention sur les locks d'un même repository, bornés par un plafond de
concurrence.

Le sharding par tenant est **différé, pas rejeté** — la conception le garde ajoutable
derrière une clé de shard, sans changer la surface d'API.

## Une tenancy portée par les tags, filtrée côté serveur

**Choisi parce que** c'est ce qui rend le repository partagé exposable aux tenants tout
court. Le filtre est dérivé du namespace propre à la custom resource, et l'API n'a aucun champ
par lequel un autre namespace pourrait être nommé.

**Le coût.** Un Job de listing supplémentaire — quelques secondes — avant qu'un restore
d'origine cluster ne déplace des données. Et la discipline voulant que tout ce qui est sur la
surface exposée au tenant soit re-dérivable côté serveur, ce qui explique que plusieurs
champs de confort n'existent pas.

## Le repository est la source de vérité

**Choisi parce qu'**un disaster recovery qui exige un cluster survivant n'est pas un disaster
recovery. Pointez l'operator sur un bucket, et il inventorie ce qui est restaurable.

**Le coût.** `Backup.spec` ne peut contenir que ce qu'une projection peut reconstruire à
partir de restic seul. La configuration d'exécution est matérialisée à la création plutôt que
maintenue vivante, et la discovery ne doit jamais revendiquer un champ qu'elle ne peut pas
reproduire — sinon les deux écrivains se battent pour toujours au sujet de l'objet sous le
server-side apply. Voir [La cascade](/CrystalBackup/fr/docs/understand/cascade/).

## Une clé racine détenue par l'admin, séquestrée hors du cluster

**Choisi parce qu'**une clé générée dans le cluster est perdue avec le cluster, et tous les
backups avec elle. Ni l'operator ni le chart n'en fabriquent jamais.

**Le coût.** Une charge opérationnelle qu'on ne peut pas automatiser : vous devez générer la
KEK, la séquestrer à l'extérieur, et tenir ce séquestre à jour. Perdez-la et le bucket n'est
plus que du chiffré.

La contrepartie est le séquestre de la clé wrappée **dans le bucket**, si bien que l'entrée
nécessaire à la reprise est de deux choses — le bucket et votre KEK — plutôt que trois.

## Aucun slot de clé de l'operator sur le repository d'un tenant

**Choisi parce que** supprimer un slot de clé restic ne fait pas tourner la clé maître. Un
slot plateforme aurait été permanent et irrévocable : une porte à sens unique.

Le champ existait, était à moitié implémenté — il annonçait un slot plateforme dans le statut
sans jamais en ajouter un — et il a été **supprimé** plutôt que corrigé.

**Le coût.** La plateforme ne peut pas aider un tenant qui perd sa clé, ne peut pas médier un
restore depuis un repository de tenant, et ne peut pas en vérifier un pour lui. Certaines
demandes de support n'ont pas de réponse, et c'est le résultat recherché.

## Les movers comme Jobs, jamais en processus

**Choisi parce qu'**un operator qui déplace des octets dans son propre processus ne peut pas
être redémarré en sécurité, ne peut pas être ordonnancé sur plusieurs nœuds, et ne peut pas
être borné en ressources par unité de travail.

**Le coût.** Une discipline d'orchestration des Jobs partout : des noms déterministes pour
qu'un redémarrage ré-adopte au lieu de dupliquer ; un polling *à travers* un `NotFound`
transitoire, parce que le retard du cache n'est pas une absence ; une propagation de
suppression explicite ; et des filets de sécurité par TTL. Chacune de ces règles existe parce
que son absence a produit une vraie fuite.

## Les movers uniquement dans le namespace de l'operator

**Choisi parce que** cela garde les clés de repository et les credentials du stockage objet
hors de tout namespace qu'un tenant contrôle, sur les deux plans.

**Le coût.** Un restore doit franchir un pont vers un namespace de tenant d'une manière ou
d'une autre, et ce pont est le `PersistentVolume` cluster-scoped — les mécanismes de
transplant et de jumeau, qui sont la machinerie la plus complexe du projet. Plus simple aurait
été un mover dans le namespace du tenant, et cela y aurait mis des credentials.

## Deux chemins d'exposition, conscients du stockage

**Choisi parce que** la lecture la moins coûteuse diffère selon le CSI : CephFS peut servir un
snapshot shallow en lecture seule sans aucune copie, RBD veut un clone en copy-on-write, et
tout le reste passe par le re-liage générique.

**Le coût.** Davantage de chemins de code, et une surface de correction par driver. La
compensation honnête est qu'un driver incapable de faire un snapshot est **skipped, avec une
raison dans le statut** plutôt que silencieusement abandonné — un trou visible valant mieux
qu'un trou invisible.

## Les hooks bornent le gel, pas l'upload

**Choisi parce qu'**une application maintenue quiescée pendant un upload de plusieurs heures
est une panne. Les post hooks s'exécutent dès que les snapshots sont *pris*, pas quand ils ont
réussi, et la libération est inconditionnelle et retentée.

**Le coût.** Le backup n'est cohérent qu'à hauteur de l'instant du snapshot, ce qui est la
garantie correcte mais est plus faible que ce que certains attendent du mot « hook ».

## L'impersonation pour l'identité des hooks

**Choisi parce que** cela fait de « les utilisateurs ne peuvent faire exécuter à la plateforme
que des commandes qu'ils peuvent déjà exécuter eux-mêmes » quelque chose que l'API server
applique, plutôt que quelque chose qu'un document affirme.

**Le coût.** Chaque tenant qui veut des hooks doit d'abord créer un ServiceAccount et un
RoleBinding. C'est de la friction, et c'est le mécanisme.

## L'admission en VAP, pas en webhook

**Choisi parce qu'**une `ValidatingAdmissionPolicy` s'exécute à l'intérieur de l'API server et
tient donc **quand l'operator est à terre**. Un webhook qui échoue en position ouverte, non.

**Le coût.** Kubernetes 1.30 comme plancher dur, et les limites de CEL — il ne peut pas
demander si une cible existe, ce qui explique que la règle de confirmation soit un
sur-ensemble conservateur qui demande de manière inconditionnelle. Un contrôle véritablement
dynamique (l'unicité de la location par défaut) reste un webhook, fail-open, avec une
condition côté contrôleur derrière lui.

## L'immuabilité comme mode de location, pas comme politique

**Choisi parce qu'**Object Lock est une propriété fixée à la création d'un repository, pas un
réglage à basculer. `Standard` prune ; `Immutable` ne le peut pas, et expire en faisant
tourner les repositories à la place.

**Le coût, dit simplement :** ce n'est **pas implémenté**. Le champ est accepté et quelques
garde-fous existent ; Object Lock, la rotation et l'expiration, non. Il vaut la peine de
savoir pourquoi c'est difficile : restic écrit un fichier de lock au début de *chaque*
opération et le supprime à la fin. Sous un Object Lock en mode compliance, ces suppressions
échouent, les locks périmés s'accumulent, personne ne peut les purger, et le repository finit
par se bloquer définitivement. Un déploiement naïf « restic plus un bucket avec Object Lock »
n'est pas dégradé — il est cassé. C'est le problème qui reste à résoudre.

## La coexistence, pas le remplacement

**Choisi parce que** personne n'arrache un outil de backup qui fonctionne pour en essayer un
nouveau, et qu'un projet qui l'exige ne sera pas essayé.

**Le coût.** Une surface permanente — deny-lists, préfixes distincts, surveillance du nombre
de snapshots — sur chaque cluster, y compris ceux qui ne font tourner aucun autre outil. Et,
pendant toute période de recouvrement, deux pipelines de snapshot et à peu près le double de
trafic d'upload.

## apko et Wolfi pour les images

**Choisi parce qu'**une base à surface de CVE connues quasi nulle, un SBOM et une provenance
de build coûtent moins cher à maintenir dès le départ qu'à ajouter après coup.

**Le coût.** Une chaîne de build que la plupart des contributeurs n'ont jamais vue, et des
overrides de dépendances à tenir en lockstep sur trois images quand une advisory upstream
tombe.

## Ce qui a été essayé puis abandonné

Cela vaut la peine d'être consigné, parce que les idées rejetées d'un projet en disent autant
que celles qui ont été retenues.

- **Le crypto-shredding par tenant** — impossible dans un repository partagé à clé unique.
  Remplacé par l'effacement physique.
- **Un slot de clé plateforme sur les repositories des tenants** — irrévocable par
  construction. Supprimé.
- **Une fenêtre de backup préférée** (`start`–`end`) — la forme s'est avérée ambiguë au
  passage de minuit. L'orientation hors des heures de pointe se fait par l'expression cron à
  la place.
- **La réplication brute au niveau du stockage objet pour la synchronisation externe** — elle
  transporte la clé maître de la source vers la destination, ce qui, sur le plan namespace,
  mettrait la clé de la plateforme à l'intérieur du silo d'un tenant, et elle ne fonctionne
  que sur un repository entier. Remplacée par `restic copy`, qui re-chiffre.
- **Une custom resource `ClusterDecommission`** — une CRD est un état désiré vers lequel un
  contrôleur converge, elle se redéclencherait donc après une restauration d'etcd ou un
  ré-apply égaré. Détruire une clé relève du runbook. `ClusterErasure` est une CRD précisément
  parce qu'elle est *bornée*.
