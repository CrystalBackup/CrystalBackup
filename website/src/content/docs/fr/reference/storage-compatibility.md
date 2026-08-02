---
title: Compatibilité du stockage
description: Quelles solutions de stockage Crystal Backup sait sauvegarder, la règle mécanique qui en décide, ce qu'une campagne réelle a mesuré, et ce qui n'est que déduit.
sourceFile: src/content/docs/reference/storage-compatibility.md
sourceHash: 7c0dd34f81b4bc90f08f5811fecdb933a7402f01
---

Que Crystal Backup sache sauvegarder les *données* d'un volume est décidé par une seule règle
mécanique, appliquée par PVC. Il n'y a pas de liste blanche de fournisseurs, pas de cas
particulier par driver au-delà d'un unique test de sous-chaîne, et aucune configuration qui
change le résultat.

Cette page énonce la règle, puis rapporte ce qu'une campagne réelle a mesuré face à elle, puis —
tenu strictement à part — ce que la règle *implique* pour du stockage sur lequel ce projet n'a
jamais tourné.

## La règle

Implémentée dans `Registry.For` (`internal/exposer/registry.go`), spécifiée par
[l'ADR 0003](https://github.com/CrystalBackup/CrystalBackup/blob/main/spec/adr/0003-snapshot-exposure-csi-generic-first.md).

1. Lire le `spec.storageClassName` de la PVC, puis le `provisioner` de cette StorageClass.
2. Lister toutes les `VolumeSnapshotClass` du cluster et en chercher une dont le `driver` est
   **strictement égal** à ce provisioner. Plusieurs correspondances sont légales ; le nom le plus
   petit lexicographiquement l'emporte, si bien que le choix est déterministe.
3. **Aucune correspondance → le volume est skipped.** `status.volumes[].phase: Skipped`,
   `reason: CSISnapshotUnsupported`, plus un Event. Le `Backup` se termine tout de même et les
   manifests du namespace sont tout de même capturés — un volume skipped est neutre dans
   l'agrégation de phase, jamais maquillé en succès et jamais un échec dur.
4. Nom du provisioner **contenant la sous-chaîne `.cephfs.csi.`** → `cephfs-shallow` : une PVC
   `ReadOnlyMany` adossée directement au snapshot (`backingSnapshot`), zéro copie.
5. **Tout le reste** → `csi-generic` : `VolumeSnapshot` dans le namespace d'origine → le
   `VolumeSnapshotContent` lié est épinglé en `Retain` et re-lié statiquement dans
   `crystal-backup-system` → une PVC temporaire est créée **dans le namespace de l'operator** à
   partir de ce snapshot statique → un mover non privilégié la lit.

Deux conséquences qu'il vaut mieux énoncer clairement :

- **Égalité de chaînes, pas détection de fournisseur.** Un driver dont la `VolumeSnapshotClass`
  existe sous une chaîne `driver` différente du `provisioner` de la StorageClass ne sera pas
  reconnu. C'est aussi pourquoi le test CephFS porte sur une sous-chaîne du *nom*, et pourquoi un
  driver qui se trouve simplement reposer sur du stockage CephFS n'est pas routé vers
  `cephfs-shallow` (voir [le piège `ceph-nfs`](#le-piège-ceph-nfs)).
- **La règle prédit le routage, pas le succès.** Elle répond à « quel chemin sera pris », pas à
  « le driver l'honorera-t-il ». Un driver peut se résoudre proprement vers `csi-generic` puis
  refuser de créer la PVC temporaire. Ce volume n'est *pas* `Skipped` — `Skipped` est réservé au
  seul cas que l'operator peut trancher d'avance, avant de toucher au stockage : aucune
  `VolumeSnapshotClass` du tout.

La question qui détermine tout le reste est toujours la même :
**le driver sait-il créer un volume *à partir* d'un snapshot, et à quel coût ?**

## Résultats vérifiés

Le tableau ci-dessous n'est pas une affirmation de compatibilité écrite depuis de la
documentation. Chaque ligne est une exécution de `test/crucible/scripts/csi-probe.sh`, qui rejoue
le chemin d'exposition étape par étape — `VolumeSnapshot` dynamique, épinglage `Retain`, re-bind
statique du `VolumeSnapshotContent` **dans un autre namespace**, PVC temporaire à partir du
snapshot statique, montage en lecture seule, checksum des données relues comparé aux données
écrites.

Banc : RKE2 sur Hetzner Cloud, 3 masters + 3 workers, 2026-08-01. 13 StorageClass sondées,
**zéro anomalie**. Artefacts bruts : `test/crucible/artifacts/csi-probe-*.json` ; agrégat :
`test/crucible/artifacts/csi-compat-report.md`.

| StorageClass | Provisioner | Exposer | Verdict | PVC temp liée (50 Mio → 500 Mio) |
|---|---|---|---|---|
| `ceph-block` | `rook-ceph.rbd.csi.ceph.com` | `csi-generic` | **COMPATIBLE** | 1,7 s → non mesuré |
| `ceph-filesystem` | `rook-ceph.cephfs.csi.ceph.com` | `cephfs-shallow` | **COMPATIBLE** | 1,7 s → 0,4 s |
| `ceph-nfs` | `rook-ceph.nfs.csi.ceph.com` | `csi-generic` | **COMPATIBLE** | 8,6 s → 18,3 s |
| `csi-nfs` | `nfs.csi.k8s.io` | `csi-generic` | **COMPATIBLE** | 4,5 s → 14,1 s |
| `longhorn` | `driver.longhorn.io` | `csi-generic` | **COMPATIBLE** | 3,1 s → 3,1 s |
| `openebs-lvm-thin` | `local.csi.openebs.io` | `csi-generic` | **COMPATIBLE** | 1,7 s → non mesuré |
| `openebs-zfs` | `zfs.csi.openebs.io` | `csi-generic` | **COMPATIBLE** | 1,7 s → 1,7 s |
| `topolvm-thin` | `topolvm.io` | `csi-generic` | **COMPATIBLE** | 1,7 s → 1,7 s |
| `openebs-lvm` (VG épais) | `local.csi.openebs.io` | `csi-generic` | **INCOMPATIBLE** | jamais liée |
| `csi-smb` | `smb.csi.k8s.io` | — | **SKIPPED** | — |
| `hcloud-volumes` | `csi.hetzner.cloud` | — | **SKIPPED** | — |
| `local-path` | `rancher.io/local-path` | — | **SKIPPED** | — |
| `openebs-hostpath` | `openebs.io/local` | — | **SKIPPED** | — |

:::note[Ce que les durées sont et ne sont pas]
« PVC temp liée » est le temps de provisionnement de la PVC temporaire depuis le snapshot, à
50 Mio puis à 500 Mio de données source. Plat signifie que le driver n'a pas copié ; croissant
signifie qu'il l'a probablement fait. C'est une **heuristique de temps**, pas une mesure du
backend de stockage, et elle peut se tromper dans les deux sens — une baie rapide peut copier
500 Mio assez vite pour ressembler à du copy-on-write, et un backend throttlé peut faire passer
un vrai clone pour linéaire. Les écarts sous la seconde sont du bruit de sondage. Un verdict
`COMPATIBLE` ne couvre que le chemin d'exposition : ni le restore, ni la tenue en charge, ni les
quotas de snapshots.
:::

### CephFS : le zéro-copie est mesuré

`ceph-filesystem` route vers `cephfs-shallow`, et la PVC temporaire s'est liée en **1,7 s pour
50 Mio et 0,4 s pour 500 Mio**. Dix fois les données, pas plus de temps — et moins, ce qui est du
bruit autour d'une constante. C'est le seul endroit où l'affirmation de zéro-copie est une mesure
plutôt qu'un argument de conception.

Ça compte parce que le chemin générique sur CephFS ne serait pas gratuit : une PVC normale et
inscriptible créée depuis un snapshot CephFS est une **copie complète du subvolume**.
`cephfs-shallow` existe précisément pour l'éviter, et il lui faut à la fois `ReadOnlyMany` et
`backingSnapshot` pour cela.

### Le piège `ceph-nfs`

`ceph-nfs`, c'est l'export NFS de Rook — le même filesystem Ceph en dessous, atteint via NFS. Son
provisioner est `rook-ceph.nfs.csi.ceph.com`.

**Ce nom ne contient pas `.cephfs.csi.`.** Le test de sous-chaîne échoue, donc le volume est routé
vers `csi-generic`, pas vers `cephfs-shallow`.

Le coût de ce routage a été mesuré : PVC temporaire liée en **8,6 s à 50 Mio et 18,3 s à
500 Mio**, là où `ceph-filesystem` restait plat sur la même multiplication par dix.

Même stockage sous-jacent. Coût radicalement différent, décidé entièrement par le driver par
lequel on y accède. Si vous avez le choix entre monter un filesystem Ceph via ceph-csi et via
l'export NFS, ce choix est une décision de coût de backup, et le côté NFS est le côté cher.

### OpenEBS LVM : épais et thin sont deux réponses différentes

Même provisioner (`local.csi.openebs.io`), même `VolumeSnapshotClass`, résultats opposés :

- **`openebs-lvm` sur un volume group épais — INCOMPATIBLE.** La PVC temporaire n'a jamais atteint
  `Bound`. Le message du driver lui-même : `snapshot ... is not of thin type`. Reproduit trois
  fois.
- **`openebs-lvm-thin` sur un thin pool — COMPATIBLE.** Snapshot prêt en 1,7 s, PVC temporaire liée
  en 1,7 s.

C'est l'illustration la plus nette de « la règle prédit le routage, pas le succès ». La résolution
a réussi dans les deux cas — une `VolumeSnapshotClass` correspondante existe — et le cas épais a
échoué plus tard, à l'intérieur du driver. Un tel volume n'est pas rapporté `Skipped` ; il ne se
termine pas.

Si vous faites tourner OpenEBS LocalPV LVM, vérifiez que votre volume group est thin-provisionné
avant de compter ces PVC comme protégées.

### Longhorn : compatible, avec un péage fixe de 140 secondes

`longhorn` est COMPATIBLE, et son provisionnement est constant : la PVC temporaire s'est liée en
3,1 s à 50 Mio comme à 500 Mio. Mais **monter** cette PVC a pris **139,6 s et 140,1 s** — contre
5 à 10 s pour les classes Ceph.

C'est un coût fixe par volume, pas un coût de copie de données, et il n'apparaît pas du tout dans
les chiffres de provisionnement. Sur un namespace à 30 PVC, c'est le terme qui domine le run.
Dimensionnez vos fenêtres de backup et votre `maxConcurrentMovers` en le gardant à l'esprit.

### `csi.hetzner.cloud` : pas de snapshot du tout

`hcloud-volumes` est SKIPPED, et la raison est en amont de Crystal Backup : le driver
n'implémente pas `CreateSnapshot`, donc aucune `VolumeSnapshotClass` n'existe pour lui. Vérifié en
comptant les objets `VolumeSnapshotClass` dont le `driver` est `csi.hetzner.cloud` sur le
cluster : zéro.

Les manifests sont tout de même capturés pour ces namespaces. Les *données* du volume, non.

### `csi-nfs` : compatible, coût indéterminé mais croissant

`csi-nfs` (csi-driver-nfs, `nfs.csi.k8s.io`) est COMPATIBLE. La PVC temporaire s'est liée en 4,5 s
à 50 Mio et 14,1 s à 500 Mio — classé « indéterminé », mais clairement croissant, ce qui est
cohérent avec une implémentation qui archive le volume dans un fichier tar. Considérez qu'il paie
pour les données, pas qu'il clone.

### `piraeus-thin` : non qualifié

LINSTOR/Piraeus **n'apparaît pas** dans le tableau parce qu'il n'a pas pu être monté sur ce banc :
le module noyau DRBD9 n'était disponible sur aucun worker. Le loader de Piraeus le compile et
exige `linux-headers-$(uname -r)`, que l'image des nœuds ne portait pas.

C'est un **échec de prérequis de notre côté, pas un verdict sur le driver**. Crystal Backup n'a
aucun résultat pour LINSTOR, dans un sens comme dans l'autre.

## Deux pièges d'exploitation découverts pendant la campagne

Aucun des deux n'est propre à Crystal Backup. Tous deux cassent `VolumeSnapshot` pour tout ce qui
s'en sert.

### Plusieurs `snapshot-controller` dans le cluster

Les charts OpenEBS LVM et ZFS embarquent chacun leur propre `snapshot-controller`, *en plus* de
celui que votre distribution livre déjà (RKE2 et k3s le font ; la plupart des distributions
managées aussi).

Le snapshot controller est un **singleton cluster-scoped**. Deux exemplaires ou plus réconcilient
chaque `VolumeSnapshot` du cluster et se battent en écriture optimiste. Le symptôme est que le
`VolumeSnapshot` n'atteint jamais `readyToUse`, avec :

```
Operation cannot be fulfilled on volumesnapshots.snapshot.storage.k8s.io "…":
the object has been modified; please apply your changes to the latest version and try again
```

Ça ne casse pas seulement le driver dont le chart a apporté l'exemplaire en trop. **Ça peut casser
les snapshots de tout le cluster** — y compris sur du stockage par ailleurs parfaitement sain.

Détectez-le en listant tous les workloads portant une image `snapshot-controller` :

```bash
kubectl get deploy,statefulset,daemonset -A -o json | jq -r '
  .items[]
  | . as $w
  | $w.spec.template.spec.containers[]
  | select(.image | contains("snapshot-controller"))
  | "\($w.kind)/\($w.metadata.namespace)/\($w.metadata.name)  \(.image)"'
```

Plus d'une ligne, c'est la panne. Ces charts n'exposent aucune valeur pour le désactiver ; le
correctif est donc de retirer le conteneur `snapshot-controller` du workload après installation,
en gardant celui que votre distribution possède.

### Modules noyau device-mapper manquants

OpenEBS LocalPV LVM prend ses snapshots avec `lvcreate --snapshot`, qui a besoin de la cible
device-mapper `dm_snapshot` — **y compris sur un LV thin**.

Sans elle, la custom resource `LVMSnapshot` reste `Pending` et **rien ne remonte à l'API
Kubernetes** : ni statut, ni Event, ni condition. La seule trace est dans le log de l'agent de
nœud :

```
Required device-mapper target(s) not detected in your kernel
```

Diagnostiquez dans cet ordre :

```bash
# 1. la CR est bloquée sans explication
kubectl get lvmsnapshots.local.openebs.io -A

# 2. le seul endroit où la raison existe — le node plugin du nœud portant le volume
kubectl -n openebs get pods -o wide | grep -i lvm.*node
kubectl -n openebs logs <ce-pod> --all-containers --tail=200 | grep -i device-mapper

# 3. sur le nœud lui-même
lsmod | grep -E 'dm_snapshot|dm_thin_pool'
```

Le correctif est sur les nœuds (`modprobe dm_snapshot`, rendu persistant), pas dans un objet
Kubernetes.

## Déduit de la règle — jamais testé ici

:::caution[Rien sous cette ligne n'a été mesuré]
Tout ce qui suit est **déduit** de la règle en haut de cette page, plus de la connaissance
générale des drivers. Rien de tout cela n'a tourné sur notre banc. C'est proposé pour vous aider à
vous former une attente et à savoir quoi vérifier — pas comme une déclaration de compatibilité.
Vérifiez avec [`csi-probe.sh`](#qualifier-une-storageclass-pour-de-vrai) sur votre propre cluster
avant de vous fier à une ligne quelconque.

Pour chaque entrée, la question qui tranche est inchangée : **le driver sait-il créer un volume à
partir d'un snapshot, et à quel coût ?**
:::

### Baies et stockage propriétaire

Toutes celles-ci sont en `csi-generic` par la règle : leurs noms de provisioner ne contiennent
aucun `.cephfs.csi.`.

| Driver | Provisioner | Attente |
|---|---|---|
| NetApp Trident | `csi.trident.netapp.io` | Devrait fonctionner. FlexClone rend la création depuis un snapshot quasi zéro-copie. |
| Pure Storage | `csi.purestorage.com` | Devrait fonctionner. |
| Portworx | `pxd.portworx.com` | Devrait fonctionner. |
| Dell PowerStore / PowerFlex / PowerMax / Unity | drivers CSI Dell | Devraient fonctionner. |
| HPE | `csi.hpe.com` | Devrait fonctionner. |
| IBM Storage Scale | CSI IBM Spectrum Scale | Volumes fileset uniquement. |
| vSphere CSI | `csi.vsphere.vmware.com` | **Volumes block uniquement.** Les volumes vSAN File n'ont pas de support de snapshot. Et attention : la création d'un volume *depuis* un snapshot est arrivée bien après la simple création de snapshot — une version de driver qui snapshote n'est pas nécessairement une version capable de restaurer dans une PVC temporaire. |

### Clouds

| Stockage | Provisioner | Attente |
|---|---|---|
| AWS EBS | `ebs.csi.aws.com` | Fonctionne, mais le volume temporaire est **hydraté paresseusement depuis S3**, et une sauvegarde lit tout le volume. C'est le pire cas de la lecture paresseuse : chaque bloc est chargé à la première lecture. |
| AWS EFS | `efs.csi.aws.com` | Pas de `VolumeSnapshot` → **Skipped**. |
| AWS FSx (Lustre, OpenZFS) | drivers CSI FSx | Pas de `VolumeSnapshot` → **Skipped**. |
| GCP PD / Hyperdisk | `pd.csi.storage.gke.io` | Devrait fonctionner. |
| GCP Filestore | `filestore.csi.storage.gke.io` | Supporté, mais restaurer crée une **nouvelle instance Filestore** (1 Tio minimum) par volume et par sauvegarde. Techniquement fonctionnel, économiquement absurde. |
| Azure Disk | `disk.csi.azure.com` | Devrait fonctionner. |
| Azure File | `file.csi.azure.com` | Les snapshots fonctionnent, mais créer un share depuis un snapshot est une **copie complète**. |
| Azure Blob | `blob.csi.azure.com` | Pas de `VolumeSnapshot` → **Skipped**. |
| OpenStack Cinder | `cinder.csi.openstack.org` | Devrait fonctionner. |
| OpenStack Manila | `manila.csi.openstack.org` | Dépend entièrement du backend du share. |
| DigitalOcean, Scaleway, OCI Block, IBM VPC Block, Alibaba Disk | drivers CSI respectifs | Devraient fonctionner. |
| OCI File Storage | `fss.csi.oraclecloud.com` | Probablement **Skipped**. |

### Autre stockage open-source

| Stockage | Attente |
|---|---|
| Mayastor (OpenEBS Replicated) | Le support des snapshots est récent — à vérifier selon votre version. |
| democratic-csi / TrueNAS | ZFS en dessous, donc les snapshots devraient être très bon marché. |
| JuiceFS | Pas de `VolumeSnapshot` → **Skipped**. |
| SeaweedFS CSI | Pas de `VolumeSnapshot` → **Skipped**. |
| `nfs-subdir-external-provisioner` | **Ce n'est pas un driver CSI du tout.** Aucune `VolumeSnapshotClass` ne peut pointer dessus → **Skipped**. |

## Limitations connues

### `volumeMode: Block`

**Le restore le refuse explicitement.** Une cible en `volumeMode: Block` produit
`rexposer.ErrBlockUnsupported`, et le volume est rapporté en échec avec la raison
`RestoreBlockUnsupported` (`internal/rexposer/rexposer.go`,
`internal/controller/restore_engine.go`). restic restaure des fichiers dans un filesystem ; un
device brut n'en a pas.

**La sauvegarde ne le conserve pas.** La PVC temporaire est construite sans aucun champ
`volumeMode` (`newTempPVCFromSnapshot`, `internal/exposer/snapshot.go`), donc elle vaut
`Filesystem` par défaut — à partir d'un snapshot de device brut. `internal/exposer` ne lit ni
n'écrit `VolumeMode` nulle part.

C'est un écart entre la spécification et le code : l'ADR 0003 dit que les PVC block sont
« exposées à l'identique » avec un chemin `volumeDevices` fourni au mover, et le code ne le fait
pas. Considérez `volumeMode: Block` comme non supporté de bout en bout, en notant que le côté
sauvegarde échoue moins clairement que le côté restore.

### Le snapshot controller doit être installé

Sans les CRD `snapshot.storage.k8s.io/v1` et le contrôleur external-snapshotter, il n'y a aucun
objet `VolumeSnapshotClass` à faire correspondre, donc **tous** les volumes sont `Skipped` — y
compris sur du Ceph parfaitement sain. Voir
[Prérequis](/CrystalBackup/fr/docs/start/requirements/).

### Il n'y a aucune soupape de configuration

L'ADR 0003 décrit deux réglages d'operator, `exposure.rbdDirect` et
`exposure.readOnlyManyStorageClasses`. **Aucun des deux n'est implémenté.** Les deux chaînes
n'apparaissent que dans `spec/` ; elles n'existent dans aucun fichier Go, aucun champ de CRD et
aucune valeur Helm. Il n'y a aucun moyen supporté de forcer un autre exposer, d'ajouter une
StorageClass à une liste blanche pour une PVC temporaire `ReadOnlyMany`, ni d'activer le chemin
`rook-rbd-direct`.

La sélection décrite en haut de cette page est tout ce qu'il y a.

## Le restore est plus permissif que la sauvegarde

La sauvegarde a besoin d'un snapshot. **Le restore, non.**

Le mécanisme `pvc-transplant` ([ADR 0016](https://github.com/CrystalBackup/CrystalBackup/blob/main/spec/adr/0016-restore-execution-and-target-exposure.md))
provisionne une PVC temporaire ordinaire dans `crystal-backup-system`, laisse le mover en être le
premier consommateur, puis transplante le PV sous-jacent dans le namespace cible. Il n'exige
qu'une StorageClass capable de provisionner un volume — pas de `VolumeSnapshotClass`, pas de
capacité de snapshot.

Conséquences :

- Des données sauvegardées depuis Ceph peuvent être restaurées sur du `local-path`, via
  `storageClassMapping`.
- Une StorageClass `Skipped` est une limitation côté sauvegarde uniquement. Rien ne l'empêche
  d'être une *cible* de restore.
- `pv-twin` (restore dans une PVC existante et liée) a la même propriété.

Voir [Restaurer](/CrystalBackup/fr/docs/guides/restore/).

## Le côté cible est S3 uniquement

La compatibilité de stockage ci-dessus concerne la *source*. La **destination** — là où vit le
repository restic — est une question distincte et bien plus étroite.

Une `BackupLocation` et une `ClusterBackupLocation` portent chacune exactement un champ de
stockage, `s3` (`api/v1alpha1/backuplocation_types.go`,
`api/v1alpha1/clusterbackuplocation_types.go`, de type `S3Spec` avec un `endpoint` requis). La
seule URL de repository que Crystal Backup génère est `s3:` (`internal/restic/restic.go`). restic
lui-même sait adresser GCS, Azure Blob, B2, SFTP et des repositories REST ; **Crystal Backup, non.**

Cela vaut aussi pour `ExternalSync` : son `destinationLocationRef` nomme une autre
`BackupLocation`, elle-même S3 uniquement. Il n'existe aucun chemin vers une destination non-S3.

Google Cloud Storage et Azure Blob offrent tous deux des options compatibles S3 ou une passerelle
S3 — c'est la porte d'entrée, et sa validation vous incombe ; ce n'est pas quelque chose que ce
projet teste.

## Vérifier votre propre cluster

### En 30 secondes

```bash
# every StorageClass and its provisioner
kubectl get storageclass \
  -o custom-columns=NAME:.metadata.name,PROVISIONER:.provisioner

# every VolumeSnapshotClass and its driver
kubectl get volumesnapshotclass \
  -o custom-columns=NAME:.metadata.name,DRIVER:.driver
```

Comparez les deux listes par égalité exacte de chaînes. **Chaque provisioner sans `driver` qui lui
soit égal signifie que les volumes de cette StorageClass seront `Skipped` avec
`CSISnapshotUnsupported`** — manifests capturés, données non.

Une deuxième liste vide (ou `error: the server doesn't have a resource type
"volumesnapshotclass"`) signifie que le snapshot controller est absent et que rien du tout ne sera
sauvegardé.

Le [script de préflight](/CrystalBackup/fr/docs/start/requirements/#vérifiez-votre-cluster-avant-dinstaller)
fait cette même résolution et compte en plus combien de PVC reposent sur chaque StorageClass. Il
ne crée rien.

### Qualifier une StorageClass pour de vrai

`csi-probe.sh` va plus loin que la résolution : il exécute réellement le chemin d'exposition — y
compris le re-bind statique inter-namespace, qui est la partie sur laquelle la plupart des drivers
cassent — et vérifie un checksum des données relues.

```bash
test/crucible/scripts/csi-probe.sh <storageclass> --copy-probe
```

Il lui faut `bash` ≥ 4, `kubectl` et `jq` ; rien n'est installé côté cluster. Il crée deux
namespaces jetables et ses propres objets `VolumeSnapshotContent`, nettoie en sortie y compris en
cas d'échec, et écrit un artefact JSON dans
`$CRUCIBLE_ARTIFACTS/csi-probe-<storageclass>.json`. Le seul objet préexistant qu'il touche est le
`VolumeSnapshotContent` d'origine, dont il bascule le `deletionPolicy` en `Retain` le temps du
handover et qu'il rétablit ensuite — exactement comme le fait l'operator.

`--copy-probe` rejoue tout le flux avec dix fois les données pour que les temps de
provisionnement soient comparables. Verdicts : `COMPATIBLE` (0), `SKIPPED` (0),
`COMPATIBLE_COPIE_COMPLETE` (4), `INCOMPATIBLE` (1), `PROBE_ERROR` (3) — le dernier signifie que
*la sonde* n'a pas pu répondre et n'est jamais la faute du driver.
