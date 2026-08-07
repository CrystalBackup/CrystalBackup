---
title: État du projet
description: Ce qui est livré, ce qui ne l'est pas, comment le projet est versionné, et comment il est construit.
sourceFile: src/content/docs/discover/status.md
sourceHash: 238a0282c37fb09b713fd0a860b74c4cad71c27d
---

## Où en est le projet

La version courante est **`v0.6.3`**. Les jalons M0 à M6 sont livrés : le moteur de backup,
le disaster recovery du cluster, le restore, les manifests et le DR cluster-scoped, les
hooks de cohérence, la maintenance et la vérification du repository, le plan namespace, la
synchronisation externe, le droit à l'effacement, et la couche d'observabilité posée devant
l'ensemble.

Trois jalons restent. L'API des CRD est en `v1alpha1` et **changera encore** avant `1.0.0`.
M6 **était** la passe de durcissement pour la production, et elle est livrée — mais deux de
ses propres critères de sortie ne sont pas remplis : un soak de deux semaines aux côtés d'un
outil en place, et un déploiement pilote. C'est pourquoi **0.6.3 est proposée pour être
testée en conditions réelles, pas pour la production** — une affirmation plus étroite que
« pas durci », et plus utile. Le résumé honnête est : **précoce, mais ce n'est plus
hypothétique.** Les chemins livrés sont testés contre de l'infrastructure réelle ; ce n'est
pas la même chose que vous demander de lui confier des données que vous ne pouvez pas
recréer.

## Ce que livre chaque jalon, et comment il a été vérifié

Chaque jalon livré a dû passer une campagne d'acceptation sur une **plateforme réelle
jetable** — RKE2, Rook Ceph (RBD et CephFS), Longhorn, local-path, et du vrai stockage objet
S3 — avant d'être publié. Ces rapports sont publiés intégralement, check par check, y
compris les skips et les défauts trouvés à chaque tour.

| Jalon | Ce qui a été livré | Vérifié par |
|---|---|---|
| **M0** | L'échafaudage — 12 CRD, le chart Helm, le pipeline de chaîne d'approvisionnement, le harnais de test | CRD `Established` et artefacts du chart vérifiés en direct, à l'intérieur de chaque campagne crucible |
| **M1** | Moteur et DR cluster — la cascade schedule → backup → fan-out par namespace, les exposers de snapshots CSI, la discovery depuis le repository, la rétention, les métriques | [Crucible M1](/CrystalBackup/reports/crucible-m1.html) — 25 passés, 0 échec |
| **M2** | Restore — self-service dans un namespace, restore de DR cluster médié par l'operator, `ClusterRestore`, politiques d'admission | [Crucible M2](/CrystalBackup/reports/crucible-m2.html) — 31 passés, chaque volume restauré comparé octet à octet au manifeste de checksums du seed · [reprise de durcissement 0.2.1](/CrystalBackup/reports/crucible-m2.1.html) |
| **M3** | Manifests et DR cluster-scoped — le moteur de sanitisation, l'apply conscient du mode, la capture des ressources cluster avec opt-in et restore sélectif | [Crucible M3](/CrystalBackup/reports/crucible-m3.html) — 11 critères d'acceptation · audits de durcissement [3.1](/CrystalBackup/reports/audit-m3.1.html) et [3.2](/CrystalBackup/reports/audit-m3.2.html) |
| **M4** | Hooks de cohérence, vérification du repository (`restic check`) et maintenance planifiée | [Crucible M4](/CrystalBackup/reports/crucible-m4.html) — la suite complète exécutée **sept fois indépendamment**, parce que le bug le plus dur du jalon était une fuite de snapshots qui se reproduisait environ une fois sur trois. Sept lanes, zéro objet snapshot résiduel |
| **M5** | Plan namespace, synchronisation externe, droit à l'effacement | [Crucible M5](/CrystalBackup/reports/crucible-m5.html) — 14 critères d'acceptation, plus une passe de non-régression de 60 checks sur la suite complète, sur le build qui a été publié |

Deux choses au sujet de ces rapports méritent d'être connues avant d'en lire un.

**L'oracle est indépendant.** Chaque affirmation portant sur un repository est vérifiée par
un Job jetable qui exécute la CLI `restic` upstream contre le même repository, les mêmes
credentials et la même clé que ceux qu'a utilisés l'operator. Un contrôleur qui écrirait et
rapporterait la même chose fausse ne peut pas faire passer un check.

**Ils consignent ce qui a mal tourné, pas seulement ce qui est passé.** L'écriture de la
seule suite M5 a trouvé six défauts, dont trois laissaient une fonctionnalité annoncée
totalement **inerte** sur de l'infrastructure réelle — une synchronisation du plan namespace
qui ne pouvait jamais s'achever, un mode `Mirror` qui ne prunait rien, et un effacement qui
ne supprimait aucun snapshot. Aucun n'était atteignable sans un vrai repository. C'est
l'argument en faveur de l'exécution de la suite, et la raison pour laquelle les rapports
sont publiés plutôt que résumés.

## Ce qui n'est pas livré

| Jalon | État | Ce qu'il ajoutera |
|---|---|---|
| **M6** | livré en `v0.6.0` | Le catalogue complet des métriques, deux dashboards Grafana et onze règles d'alerte observées *en firing* contre un vrai Prometheus, les traces OTel à travers le pipeline, un self-check exportable, et un gate de fidélité du restore qui compare fichier par fichier un restore à sa source. Le réglage des ressources des movers par type d'opération et la revue PodSecurity passent en `0.6.1` ; le soak et le déploiement pilote n'ont pas eu lieu |
| **M7** | pas commencé | **La portée.** Sauvegarder le stockage incapable de faire des snapshots — aujourd'hui une PVC sur `local-path`, hostPath ou NFS simple est *sautée*, ce qui laisse sans rien la plupart des petites installations k3s/RKE2 — plus la restauration d'un fichier unique dans le volume d'une application qui tourne, et des notifications par webhook générique pour les équipes sans stack Prometheus |
| **M8** | pas commencé | **La preuve.** Un `RestoreDrill` qui restaure périodiquement votre dernier backup dans un namespace jetable, le compare fichier par fichier et rapporte — la machinerie existe déjà, c'est le propre gate de fidélité du projet, mais c'est un test de CI aujourd'hui, pas quelque chose qui tourne chez vous. Plus les alertes de restauration : aucune des douze règles livrées ne surveille l'échec d'un restore |
| **M9** | pas commencé | Les locations immuables. `spec.mode: Immutable` est accepté par l'API, mais S3 Object Lock, la rotation de repository et l'expiration **ne sont pas implémentés** — cela ne vous donne pas de WORM |

**Deux choses ont quitté cette liste au lieu d'y descendre.** La CLI `crystalctl` et l'UI de
navigation ne sont plus des jalons ici : elles deviennent des projets séparés, la CLI sous forme
de plugin `kubectl` et l'UI dans son propre dépôt. Rien n'est perdu — le repository est au format
restic standard, donc tout reste atteignable avec `restic` upstream, et aucune capacité ne sera
jamais accessible *uniquement* par elles. Mais cela veut dire qu'il n'y a **aucun outil en ligne
de commande destiné aux utilisateurs aujourd'hui**, et qu'il n'en viendra pas de ce dépôt. Et le
*durcissement* de la coexistence a été retiré des jalons parce que la coexistence est structurelle
et fonctionne déjà : groupe d'API et namespaces distincts, aucune mutation des classes de snapshot
d'autrui, et une alerte qui compte délibérément les VolumeSnapshots de *tous* les outils, parce
qu'en coexistence ce sont ceux de l'outil en place qui remplissent la réserve partagée par volume.
Ce qu'il restait de ce jalon, c'était le soak déjà dû par M6, compté une seconde fois.

Les conséquences pratiques de ces manques — et les coûts que la conception livrée vous
impose, que ces manques soient comblés ou non — sont sur
[Quand ne pas le choisir](/CrystalBackup/fr/docs/discover/when-not-to-use/), qui est la page
à lire si vous êtes en phase d'évaluation.

## Versionnage

Le projet suit [SemVer](https://semver.org/). En majeur `0`, chaque jalon est une version
**mineure** (`M_n` → `0.n.z`) et les itérations de durcissement sont des **patches**. L'API
des CRD est en `v1alpha1` et **peut encore changer** — `1.0.0` est une décision délibérée de
stabilité d'API prise après M9, pas une date.

Une seule chaîne de version couvre l'image de l'operator, l'image du mover, l'image de sync,
l'`appVersion` du chart Helm et la future CLI.

## Comment il est testé

Quatre couches, toutes à découvert :

- **unitaires et envtest** — le comportement des contrôleurs contre un vrai API server ;
- **bout en bout sur Kind** — de vrais Jobs de mover, de vrais snapshots CSI, du vrai
  stockage objet, sur un cluster éphémère ;
- **le crucible** — une suite en conditions réelles sur de l'infrastructure cloud
  provisionnée, avec du stockage Rook Ceph, Longhorn et local-path, des workloads de tenants
  amorcés, et des tests étiquetés par jalon. Ses rapports sont publiés sur la
  [page Qualité](/CrystalBackup/quality/), et quiconque dispose d'un projet Hetzner Cloud
  peut l'exécuter lui-même — [`test/crucible/`](https://github.com/CrystalBackup/CrystalBackup/tree/main/test/crucible) ;
- **les audits** — des revues adverses périodiques des jalons livrés, publiées elles aussi.

Le propre historique du projet sur ce point mérite d'être énoncé : plusieurs de ces tours
d'audit ont trouvé des fonctionnalités documentées et inertes, et l'un d'eux a trouvé une
commande de vérification de la chaîne d'approvisionnement qui ne fonctionnait plus depuis
quatre versions. C'est pourquoi cette documentation marque explicitement ce qui n'est pas
livré, au lieu de décrire la conception comme si elle était le produit.

## Construit avec l'assistance de l'IA

Crystal Backup est écrit avec un usage intensif d'assistants de code IA, sous direction et
revue humaines. Les spécifications, les décisions d'architecture et l'implémentation sont
produites de cette façon délibérément — le projet est en partie une expérience d'ingénierie
logicielle assistée par IA.

Pour être franc : c'est une raison de plus de tester dans un bac à sable, et une raison de
plus pour que les suites de tests ci-dessus soient aussi lourdes qu'elles le sont.

## Chaîne d'approvisionnement

Les images sont construites avec [apko](https://github.com/chainguard-dev/apko) sur une base
Wolfi (glibc), pour une surface de CVE connues quasi nulle, et publiées en multi-arch
(`linux/amd64` et `linux/arm64`) sur GHCR derrière un **gate de scan à 0 CVE connue qui
s'exécute avant le push**. L'**index** multi-arch est ensuite signé par cosign en keyless,
son SBOM SPDX est attesté, la provenance de build SLSA est attachée, et un document OpenVEX
est attesté après publication pour les advisories qui atterrissent sur une image déjà
immuable. Les manifests de production référencent les images **par digest**, jamais par un
tag flottant.

Ne prenez pas cela pour argent comptant. Les
[commandes de vérification](https://github.com/CrystalBackup/CrystalBackup/blob/main/docs/DEVELOPMENT.md#7-container-images)
sont écrites, et les exécuter est bien le but : pendant quatre versions, la signature était
attachée au **manifest enfant amd64** plutôt qu'à l'index, si bien que le `cosign verify` de
chaque consommateur échouait pendant que le pipeline restait vert. Cela a été trouvé en
vérifiant un artefact au lieu de faire confiance à une coche verte, corrigé en `0.5.1`, et
le workflow refuse désormais de signer quoi que ce soit qui ne soit pas un index.

Trois images sont livrées : l'operator, le mover (restic), et une image de sync séparée
(restic plus rclone). Ce découpage est délibéré — rclone n'est nécessaire qu'à la
synchronisation externe, et en faire une troisième image garde sa surface de dépendances
hors du chemin de backup et de restore.

## Licence et garantie

Apache-2.0. Fourni **en l'état, sans garantie d'aucune sorte**. Vous l'utilisez à vos
propres risques et les auteurs déclinent toute responsabilité. Voir
[LICENSE](https://github.com/CrystalBackup/CrystalBackup/blob/main/LICENSE).

## Pour suivre

Les spécifications, les dix-huit décisions d'architecture et la feuille de route sont
publiques et précèdent le code :

- [Spécifications](https://github.com/CrystalBackup/CrystalBackup/tree/main/spec)
- [Décisions d'architecture](https://github.com/CrystalBackup/CrystalBackup/tree/main/spec/adr)
- [Feuille de route](https://github.com/CrystalBackup/CrystalBackup/blob/main/spec/90-roadmap.md)
- [Qualité et rapports de test](/CrystalBackup/quality/)
