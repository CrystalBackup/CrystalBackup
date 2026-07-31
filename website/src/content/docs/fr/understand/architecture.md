---
title: Architecture
description: Les composants, le déroulement d'un backup de bout en bout, la façon dont un restore expose sa cible, et pourquoi chaque unité de travail est un Job.
sidebar:
  order: 1
sourceFile: src/content/docs/understand/architecture.md
sourceHash: 1374e975c3accd6b0357f209be685a2973b8b28c
---

## Les composants

**L'operator** — un processus Go controller-runtime dans `crystal-backup-system`. Il
réconcilie chaque custom resource des deux plans et fait tourner les contrôleurs de
schedule, de backup, de restore, de discovery, de maintenance et de synchronisation externe,
plus un ramasseur d'orphelins. Il sert le webhook d'admission dynamique et l'endpoint de
métriques.

**Il ne touche jamais aux octets des données de backup.** Pas un seul. Chaque octet se
déplace à l'intérieur d'un Job.

**L'image du mover** — restic plus une fine surcouche Go, dans deux rôles :

- `crystal-mover` sauvegarde ou restaure **une PVC** : il monte un chemin de snapshot en
  lecture seule, exécute restic, et rapporte un résultat structuré. Il exécute aussi `prune`,
  `forget`, `check`, `unlock` et l'inventaire de discovery.
- `crystal-manifest-mover` exporte et restaure les manifests sanitisés d'un namespace. C'est
  la **seule** identité de mover qui atteint l'API server, liée transitoirement, par Job, à
  un rôle de lecteur ou d'écrivain.

**L'image de sync** — la même surcouche et le même restic épinglé, plus rclone. Une
troisième image plutôt qu'un mover plus gros, pour que la surface de dépendances de rclone
reste hors du chemin de backup et de restore. Elle n'est tirée que lorsqu'une
synchronisation externe existe.

**`BackupRepository`** — un objet interne à l'operator, un par repository, portant son
inventaire, ses résultats de check et son historique de maintenance, et propriétaire de sa
file exclusive. Ce n'est pas quelque chose que vous écrivez.

Les movers tournent **uniquement** dans `crystal-backup-system`, sur les deux plans, sans
privilèges. Jamais dans un namespace de tenant.

## La cascade

```
ClusterBackupSchedule ──cron──▶ ClusterBackup ──fan-out──▶ Backup (one per namespace) ──▶ movers
BackupSchedule ────────cron─────────────────────────────▶ Backup (same namespace) ─────▶ movers
```

`Backup` est l'unique unité d'exécution, pilotée à l'identique quel que soit le plan qui l'a
créée — et c'est *aussi* la projection d'un backup restaurable. Ce double emploi est le
centre de gravité de la conception ; voir
[La cascade](/CrystalBackup/fr/docs/understand/cascade/).

## Un backup, de bout en bout

**1 — Résoudre et initialiser.** Trouver la location et son `BackupRepository`, initialiser
le repository restic à la première utilisation (sérialisé via la file exclusive du
repository, pour que deux premiers backups concurrents ne puissent pas se courir dessus),
lister les PVC ciblées.

**2 — Pre hooks.** Exec dans les pods **qui montent les PVC capturées par cette exécution**.
Choisir les candidats par PVC montée plutôt que par label est ce qui confine l'exec aux
workloads dont les données sont réellement prises. L'exécution **s'arrête et persiste le
compte rendu avant qu'aucun snapshot n'existe** : un contrôleur qui meurt entre le gel et le
snapshot doit revenir en sachant qu'il a gelé quelque chose.

**3 — Snapshot.** Un `VolumeSnapshot` par PVC, dans le namespace d'origine, attendu jusqu'à
`ReadyToUse`. Crash-consistent, à un instant donné.

**4 — Post hooks.** Exécutés dès que chaque snapshot est **pris** — pas sur le fait qu'il ait
réussi, et pas après l'upload. Les hooks bornent la fenêtre de gel, pas le transfert. La
libération est inconditionnelle et retentée ; c'est le gel qui coûte de la disponibilité.

**5 — Exposer et déplacer.** Par PVC, l'operator choisit l'exposition la moins coûteuse pour
ce CSI :

| Exposer | Utilisé pour | Ce qu'il fait |
|---|---|---|
| `cephfs-shallow` | CephFS | Un montage `backingSnapshot` en lecture seule. Zéro copie. |
| `csi-generic` | RBD et autres CSI capables de snapshot | Re-lie le `VolumeSnapshotContent` dans `crystal-backup-system` sous forme de paire VS/VSC statique, avec une PVC temporaire en copy-on-write. |
| `rook-rbd-direct` | opt-in uniquement | Le seul chemin privilégié, confiné au namespace de l'operator. |

Une PVC dont le CSI ne sait pas faire de snapshot est **skipped** :
`status.volumes[].phase: Skipped`, `reason: CSISnapshotUnsupported`, plus un Event. Jamais
silencieusement abandonnée.

Ensuite, un Job `crystal-mover` monte le volume exposé **en lecture seule** et exécute
`restic backup` avec les tags. Les movers sont répartis entre les nœuds par des contraintes
de topology-spread pour agréger la bande passante, sous un plafond de concurrence à l'échelle
du cluster.

Le re-liage dans `csi-generic` est le mécanisme qui garde les données du tenant hors de sa
portée : le snapshot est pris dans le namespace du tenant, et monté de façon centralisée,
parce que `VolumeSnapshotContent` est cluster-scoped et peut être re-lié.

**6 — Manifests.** Un Job `crystal-manifest-mover` exporte les ressources du namespace, les
sanitise et les envoie comme snapshot `kind=manifests`.

**7 — Nettoyer.** Supprimer la PVC temporaire, la paire VS/VSC statique et le
`VolumeSnapshot` d'origine ; écrire le statut par PVC ; **mettre en file** le `forget` de
rétention sur la file de maintenance du repository — jamais en ligne à la fin du backup.

**8 — Échecs.** Statut par PVC avec une phase `PartiallyFailed`, `backoffLimit` du Job, et un
ramasseur d'orphelins qui collecte les PVC temporaires restantes, les paires VS/VSC et les
locks de repository périmés.

## Un restore, de bout en bout

Le restore est **générique** : un mover monte le volume cible en lecture-écriture et exécute
`restic restore`. Il n'y a aucune dépendance à Ceph sur ce chemin, et c'est ce qui permet de
restaurer un namespace adossé à Ceph sur un cluster qui n'a jamais entendu parler de Ceph.

Les movers tournent dans le namespace de l'operator, le restore doit donc franchir un pont
vers un namespace de tenant d'une manière ou d'une autre. Ce pont est le
`PersistentVolume`, cluster-scoped, via deux mécanismes :

**`pvc-transplant`** — la PVC cible n'existe pas. L'operator provisionne une PVC temporaire
dans `crystal-backup-system`, dimensionnée et classée d'après les tags `pvcsize`, `pvcclass`
et `pvcmodes` du snapshot. Le mover en est le premier consommateur, si bien que les classes
`WaitForFirstConsumer` se lient naturellement. En cas de succès, la reclaim policy du PV est
basculée sur `Retain`, la PVC temporaire est supprimée, le `claimRef` est re-pointé, et la
PVC finale est créée **pré-liée** dans le namespace cible, sous son nom d'origine.

**`pv-twin`** — la PVC cible existe et est liée. Un PV jumeau clone la source CSI du PV lié
avec `Retain`, pré-lié à une PVC temporaire dans le namespace de l'operator. Si le volume est
attaché à exactement un nœud, le Job du mover est épinglé sur ce nœud. Le RWX n'a besoin
d'aucun épinglage.

Dans les deux cas, la PVC restaurée finit par porter `crystalbackup.io/restored-from` et
**aucun** des labels du ramasseur de l'operator — c'est votre objet, et rien ne viendra le
collecter.

## Discovery

Par repository, à l'ajout d'une location et à chaque `discovery.interval` :

1. `restic snapshots --json --tag crystalbackup`, groupé par `(namespace, run)`.
2. Pour chaque groupe dont le namespace **existe**, s'assurer qu'un `Backup` nommé d'après le
   run s'y projette, avec un `status.volumes` dérivé des snapshots. Les namespaces qui
   n'existent pas sont passés — ils restent atteignables via `ClusterRestore`, qui lit le
   repository directement.
3. Supprimer les projections dont les snapshots ont disparu.

La conséquence est celle qu'il faut retenir : **la durée de vie de la projection est égale à
la durée de vie des données**, si bien que `kubectl get backups -n X` liste exactement ce qui
est restaurable dans X.

Les listings de discovery passent `--no-lock` : ils ne font donc jamais la queue derrière une
fenêtre de maintenance.

## Maintenance

Tout ce qui est exclusif s'exécute sur une **file exclusive par repository**, une opération à
la fois, en FIFO, jamais en ligne. Des repositories différents s'exécutent totalement en
parallèle.

Les lectures ne sont pas mises en file. `snapshots`, `stats` et `ls` passent `--no-lock`, et
les movers de données comptent comme des lecteurs — plusieurs peuvent tourner contre un même
repository en même temps.

Deux opérations **drainent** en plus les movers au préalable :

- `unlock`, parce que retirer tous les locks arracherait le lock d'un backup en cours.
  Celle-là est une question de correction.
- `prune`, pour une autre raison — le **débit**. Le lock exclusif propre à restic empêche
  déjà la corruption ; sans drain, le prune et toute la flotte de movers se regardent en
  chiens de faïence sur des retries de lock jusqu'à ce que l'un cède. Le drain convertit une
  tempête de contention en une seule courte fenêtre sérialisée.

`forget` et `check` ne drainent pas ; leur tour dans la file suffit.

Le drain a son propre délai, plus court que celui de l'opération qu'il précède : le budget de
l'opération est une question de correction (un plafond dimensionné pour `forget` tuerait
chaque prune avant sa convergence, définitivement), tandis que celui du drain est une
question de disponibilité, puisqu'il maintient l'admission des movers fermée à l'échelle du
cluster.

:::note[Un seul écrivain]
La file est un single-flight en processus, par repository, limité à un **unique leader**.
Ce n'est délibérément pas un lock distribué. Exécuter `prune` ou `forget` vous-même, hors
bande, sort de ses hypothèses.
:::

## Chaque unité de travail est un Job

Pas une goroutine. Quatre règles en découlent, et elles font la différence entre un operator
qui survit à un redémarrage et un operator qui fuit :

**Des noms déterministes.** Le nom d'un Job est une fonction pure de ce qu'il fait — jamais
aléatoire. Au redémarrage, l'operator ré-adopte en créant et en tolérant `AlreadyExists`,
plutôt qu'en démarrant un second mover sur le même volume.

**Poller à travers un `NotFound` transitoire.** Le retard du cache signifie « pas encore
prêt », pas « disparu ». Le traiter comme un échec, c'est ainsi qu'une exécution saine est
déclarée morte.

**Une propagation de suppression explicite**, pour qu'un Job de même nom puisse être recréé
proprement.

**Un filet de sécurité auto-nettoyant.** Les Jobs positionnent `ttlSecondsAfterFinished`,
pour qu'un contrôleur qui ne revient jamais ne laisse pas de pods derrière lui.

La même discipline se retrouve dans le teardown : un `Backup` terminal n'est marqué
`crystalbackup.io/exposures-cleaned` qu'une fois que le balayage a vérifié que chaque objet
d'exposition a bien été collecté. Tant que cette annotation n'est pas présente, le contrôleur
**rejoue** le balayage idempotent au lieu de rendre la main. Un teardown interrompu à
n'importe quel instant est retenté par la passe suivante ou le processus suivant, au lieu
d'être scellé pour toujours.

## Agencement du repository

```
s3://<bucket>/<prefix>/<clusterID>/
```

| Champ restic | Valeur |
|---|---|
| `host` | le `clusterID` |
| `paths` | `/data/<namespace>/<pvc>`, `/manifests/<namespace>`, `/cluster-manifests` |
| `tags` | `crystalbackup`, `tenant=`, `namespace=`, `pvc=`, `kind=`, `schedule=`, `run=`, et `pvcsize=`/`pvcclass=`/`pvcmodes=` sur les snapshots de données |

Que `clusterID` soit à la fois le host restic et un segment de chemin est ce qui permet à un
seul bucket de servir plusieurs clusters sans collision.

## Voir aussi

- [La cascade](/CrystalBackup/fr/docs/understand/cascade/)
- [Tenancy et isolation](/CrystalBackup/fr/docs/understand/tenancy/)
- [Choix de conception](/CrystalBackup/fr/docs/understand/design-choices/)
