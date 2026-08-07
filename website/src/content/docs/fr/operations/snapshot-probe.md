---
title: La sonde de faisabilité des snapshots
description: Quand lancer snapshot-probe.sh, ce qu'il crée exactement dans votre cluster, pourquoi il laisse les débris sur place en cas d'échec, et comment lire ses verdicts.
sidebar:
  order: 3
sourceFile: src/content/docs/operations/snapshot-probe.md
sourceHash: 628e4eb72604063e45b6103eec2e3029c25b0a34
---

`preflight.sh` est en lecture seule. Il ne crée rien, et c'est cette promesse qui vous a décidé à
le pointer vers la production. C'est aussi pour cela qu'il reste une question à laquelle il ne
peut pas répondre : cette page parle de cette question, et du script qui y répond.

## L'écart

Crystal Backup lit vos données depuis un **snapshot CSI**, pas depuis le volume vivant. La chaîne
qui doit fonctionner est donc :

1. prendre un `VolumeSnapshot` du PVC
2. provisionner un PVC temporaire **à partir de ce snapshot**
3. **monter** ce PVC temporaire sur un nœud
4. **lire** les données dessus

`preflight.sh` sait établir que l'étape 1 est possible : il existe une `VolumeSnapshotClass` dont
le `.driver` correspond au provisioner de votre StorageClass, donc un snapshot peut être
*demandé*. C'est la **disponibilité** du snapshot.

Il ne peut pas établir les étapes 2 à 4. Celles-là relèvent de l'**utilisabilité** du snapshot, et
rien dans l'API Kubernetes n'en rend compte. Le seul moyen de savoir est de le faire.

### Ce n'est pas une distinction théorique

Un administrateur a installé 0.6.2 sur RKE2 avec Rook-Ceph. Sa StorageClass `ceph-block-rwo` avait
bien une VolumeSnapshotClass `ceph-block` correspondante. Les snapshots ont été créés, et
correctement. Les clones en ont été provisionnés, et correctement. Puis chaque sauvegarde est
restée bloquée, trente-six heures, sur ceci :

```
rbd: map failed with error (exit status 22) ... rbd: sysfs write failed
rbd: map failed: (22) Invalid argument
```

Le clone portait `op_features: clone-child` — un clone RBD format v2 — et le client krbd de leurs
nœuds refusait de le mapper. Rien de tout cela n'est visible depuis l'API Kubernetes. Rien n'en
est visible sans les identifiants Ceph. Cela ne s'observe **qu'en restaurant réellement un
snapshot et en montant le résultat**, ce que fait précisément la sonde, en deux minutes environ.

## Ce que preflight dit désormais à la place

Une StorageClass dont la classe de snapshot se résout n'est plus rapportée comme propre. Elle
apparaît en `NOT ASSESSED` dans une colonne `USABILITY`, elle compte comme une réserve et non
comme un succès, et elle fait basculer tout le run en code de sortie **1** :

```
WHAT WOULD BE BACKED UP
  STORAGECLASS           PROVISIONER                    SNAPSHOT CLASS   USABILITY      PVCs
  ceph-block-rwo (default) rook-ceph.rbd.csi.ceph.com     ceph-block       NOT ASSESSED   2 in 2 ns
  local-path             rancher.io/local-path          none             DATA SKIPPED   1 in 1 ns
```

Aucune entrée ne fait dire mieux à cette colonne. Un script en lecture seule n'a aucun moyen
d'atteindre la réponse, et « nous n'avons pas pu vérifier » n'a pas le droit d'être arrondi au
vert.

En `--json` (schéma `crystalbackup.preflight/v2`) le même fait apparaît en
`"usability": "NOT_ASSESSED"` et `"dataBackedUp": null` — jamais `true`.

## Le contrat de la sonde

`snapshot-probe.sh` est l'exact inverse de `preflight.sh`, et il le dit dans son propre en-tête. Il
**crée des objets dans votre cluster**. Précisément ceux-ci :

| # | Objet | Nom | Notes |
|---|-------|-----|-------|
| — | Namespace | `crystalbackup-probe-<runid>` | créé par le script, supprimé par lui. `--namespace NS` en utilise un des vôtres à la place, et alors le namespace lui-même n'est jamais touché |
| 1 | PersistentVolumeClaim | `cbprobe-<runid>-<n>-src` | `ReadWriteOnce`, sur la StorageClass testée, `--size` (par défaut `1Gi`) |
| 2 | Pod | `cbprobe-<runid>-<n>-write` | écrit un motif d'octets connu, appelle `sync`, sort |
| 3 | VolumeSnapshot | `cbprobe-<runid>-<n>-snap` | de l'objet 1, avec la VolumeSnapshotClass que l'**opérateur lui-même** résoudrait |
| 4 | PersistentVolumeClaim | `cbprobe-<runid>-<n>-restored` | `dataSource` = objet 3 |
| 5 | Pod | `cbprobe-<runid>-<n>-read` | monte l'objet 4 **en lecture seule**, relit le motif, le redérive depuis la même graine, compare |

Rien n'est jamais créé hors de ce seul namespace, et le script ne modifie aucun objet qu'il n'a
pas créé.

Deux détails sont délibérés, pas accidentels :

- **Il choisit la même VolumeSnapshotClass que l'opérateur.** Quand plusieurs classes partagent un
  driver, l'opérateur les trie octet par octet et prend la première ; la sonde reproduit ce tri.
  Une sonde qui testerait une autre classe répondrait à une question que personne n'a posée.
- **Le PVC restauré porte le même access mode que l'exposer de l'opérateur** — `ReadWriteOnce` en
  général, `ReadOnlyMany` sur un driver CephFS. Les deux règles viennent du même bloc généré
  depuis `internal/exposer` que porte aussi `preflight.sh`, et la CI échoue si l'un des deux
  dérive.

### Relire les données est le cœur du test

Monter prouve le map. **Lire prouve le chemin de données.** Un clone qui se monte proprement et
revient vide est pire qu'un clone qui refuse de se monter, parce qu'un test qui se contente de
monter le rapporte comme un succès et vous ne le découvrez qu'à la restauration. Le pod lecteur
redérive donc les octets attendus depuis la graine du run, compare les empreintes dans le pod, et
le script recroise cette empreinte avec celle que le pod écrivain a déclaré avoir écrite — deux
comparaisons indépendantes.

### Ce qu'il ne fait *pas*

Il n'installe pas Crystal Backup, ne lit aucun de ses objets et n'a pas besoin de sa présence.
C'est un test de fumée de votre pile de stockage, réduit depuis le gate de fidélité de
restauration que ce projet exécute contre du vrai Rook-Ceph (`test/crucible`, M6) : la même
chaîne, avec un petit fichier au lieu d'un corpus travaillé, et sans l'opérateur dans le chemin.
Une sonde verte n'est **pas** la promesse que chaque xattr, chaque ACL et chaque trou creux
survivent à une vraie restauration. C'est la réponse à « le chemin snapshot → restauration →
montage → lecture fonctionne-t-il, sur ce cluster, tout court ? », qui était la question ouverte.

Il affiche les versions de noyau des nœuds **uniquement à l'intérieur d'un rapport d'échec**, à
titre de contexte. Il ne les vérifie jamais. Aucun seuil de noyau fiable n'est connu de ce projet
pour cette classe de panne, et une heuristique qui répondrait « probablement bon » serait pire que
le silence.

## Le nettoyage, et pourquoi l'échec laisse des débris

**Pour une classe qui ressort FEASIBLE**, tout ce que cette classe a créé est supprimé, puis la
suppression est *vérifiée* en interrogeant jusqu'à ce que chaque objet ait réellement disparu. Une
suppression que l'API a acceptée n'est pas une suppression qui a eu lieu ; un finalizer bloqué
mérite d'être nommé, donc il est nommé et il devient une réserve.

**Dans tous les autres cas, rien n'est supprimé. Pas un objet.** Le pod qui n'a pas voulu se
monter, ses events, son volume — c'est la preuve, elle n'est pas reproductible une fois effacée,
et dans l'incident ci-dessus elle a coûté trente-six heures à obtenir. Le script indique où sont
les objets et la commande unique qui les enlève quand vous avez fini :

```bash
kubectl delete namespace crystalbackup-probe-<runid>
```

`--keep` laisse tout en place, même en cas de succès.

## Quand le lancer

- **Avant d'installer**, sur tout cluster où `preflight.sh` sort en 1 avec un constat
  d'utilisabilité `NOT ASSESSED` — c'est-à-dire tout cluster ayant une StorageClass exploitable.
- **Après une évolution du stockage** : version majeure de Ceph, montée de version d'un driver
  CSI, changement d'OS ou de noyau sur les nœuds. La panne ci-dessus est une propriété du couple
  driver/nœud : changer l'un des deux invalide la réponse précédente.
- **Quand vous ajoutez une StorageClass**, avec `--storage-class NOM`.
- **Pas de façon périodique.** Il crée et détruit des volumes ; c'est un point de contrôle, pas
  une supervision. Pour une assurance continue, utilisez les règles d'alerte et un exercice de
  restauration.

## L'exécuter

```bash
BASE=https://crystalbackup.github.io/CrystalBackup
curl -fsSLO "$BASE/snapshot-probe.sh"
curl -fsSLO "$BASE/snapshot-probe.sh.sha256"
curl -fsSLO "$BASE/snapshot-probe.sh.cosign.bundle"

# 1. la somme de contrôle
sha256sum -c snapshot-probe.sh.sha256     # macOS : shasum -a 256 -c snapshot-probe.sh.sha256

# 2. la signature — keyless, la même racine de confiance Sigstore que nos images
cosign verify-blob snapshot-probe.sh \
  --bundle snapshot-probe.sh.cosign.bundle \
  --certificate-identity-regexp '^https://github\.com/CrystalBackup/CrystalBackup/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# 3. lisez-le. Celui-ci crée des objets dans votre cluster ; ne prenez pas cela sur parole
#    depuis une URL.
less snapshot-probe.sh

# 4. voyez exactement ce qu'il créerait, sans rien créer
sh snapshot-probe.sh --dry-run

# 5. lancez-le
sh snapshot-probe.sh
```

Il n'y a délibérément **aucun one-liner `curl … | sh`** sur cette page. `preflight.sh` en a un
parce qu'il ne crée rien. Ce script, si.

Options utiles : `--storage-class NOM` (répétable) pour restreindre le run, `--namespace NS` pour
travailler dans un namespace que vous avez déjà, `--size` si votre StorageClass impose un minimum
supérieur à `1Gi`, `--timeout SECONDES` (300 par défaut) si votre provisioner est lent — Longhorn
a déjà demandé bien plus d'une minute — `--image` si `busybox:1.36` n'est pas joignable depuis
votre cluster, `--json`, `--no-color`, `--keep`, `--help`.

## Lire les verdicts

Par StorageClass, le vocabulaire tient en trois mots, pas un de plus.

### FEASIBLE

```
ceph-block-rwo: snapshot OK · restore OK · mount OK · read OK
  → a snapshot of this StorageClass can be restored, mounted and read back exactly
```

Les quatre maillons ont tenu, et les octets sont revenus identiques. Objets supprimés, suppression
vérifiée.

### NOT FEASIBLE

```
ceph-block-rwo: snapshot OK · restore OK · MOUNT FAILED
  rbd: map failed: (22) Invalid argument
  → backups of this StorageClass cannot work on this cluster
```

Un maillon a cassé, et la chaîne montre lequel. La ligne en dessous est l'**event Warning le plus
récent du pod en échec, verbatim** — cette chaîne de caractères est ce que vous envoyez à votre
éditeur de stockage, et c'est tout le diagnostic. En dessous, le script affiche les noyaux des
nœuds à titre de contexte et vous dit que les objets sont toujours là.

N'installez pas en espérant que ça passe. Une sauvegarde de cette StorageClass restera bloquée en
exposition.

### NOT ASSESSED

```
ceph-block-rwo: NOT ASSESSED
  the VolumeSnapshot did not become ready within 300s and reported no error.
  → NOT ASSESSED. This is not a pass: the question is still open.
```

La sonde n'a pas obtenu de réponse. Une attente a expiré sans event Warning pour l'expliquer, un
pod est resté non planifié, le volume source ne s'est jamais lié, ou la classe n'a aucune
VolumeSnapshotClass et il n'y avait rien à sonder au départ. Rien n'est affirmé dans un sens ni
dans l'autre — augmentez `--timeout`, levez l'obstacle, relancez. Là aussi les objets restent en
place.

### Codes de sortie

La même discipline que `preflight.sh` : jamais de vert sur une absence.

| Code | Signification |
|------|---------------|
| `0` | **FEASIBLE** — chaque classe évaluée a fait snapshot → restauration → montage → lecture et a rendu le motif octet pour octet |
| `1` | **FEASIBLE, RÉSERVES** — au moins une classe n'a pas pu être évaluée, ou un nettoyage n'a pas pu être vérifié. Rien n'a échoué, et rien n'est affirmé |
| `2` | **NOT FEASIBLE** — au moins une classe a cassé la chaîne |
| `3` | **NOT ASSESSED** — la sonde n'a pas pu s'exécuter du tout, ou c'était un `--dry-run` |

Un `--dry-run` sort en **3**, pas en 0. Il n'a rien évalué, et un dry run qui sortirait en 0 serait
un vert sur une absence.

## Voir aussi

- [Prérequis](/CrystalBackup/fr/docs/start/requirements/) — `preflight.sh`, que vous lancez d'abord
- [Compatibilité du stockage](/CrystalBackup/fr/docs/reference/storage-compatibility/) — la règle
  qui décide si les données d'un volume sont sauvegardées, tout court
- [Diagnostic](/CrystalBackup/fr/docs/operations/troubleshooting/) — à quoi ressemble une
  sauvegarde bloquée en exposition, côté opérateur
