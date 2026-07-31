---
title: Disaster recovery
description: ClusterRestore — restaurer un namespace par coordonnée de repository, restaurer les ressources cluster-scoped, et repartir d'un cluster où il ne reste rien.
sidebar:
  order: 4
sourceFile: src/content/docs/guides/disaster-recovery.md
sourceHash: 941b737ed266d8dac6a333fc635f9877ae533ba4
---

Un `ClusterRestore` s'adresse à une **coordonnée de repository** — une location, un namespace
d'origine et une exécution — plutôt qu'à un objet du cluster. Il n'exige donc la survie de
rien : ni du namespace, ni du schedule, ni d'un `Backup`. C'est la propriété sur laquelle
repose toute l'histoire de la DR.

C'est une opération d'administrateur. Elle requiert le rôle `crystal-backup-admin`.

## Restaurer un namespace ailleurs

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterRestore
metadata:
  name: recover-team-x
spec:
  source:
    locationRef:
      name: dr-primary
    namespace: team-x                        # as it was named at backup time
    backup: dr-daily-20260730-020000         # or: time: latest
  target:
    namespace: team-x-restored
    createNamespace: true
    storageClassMapping:
      fast-rbd: standard
  mode: Recreate
  confirmation: team-x-restored              # the TARGET namespace
```

`source`, `mode` et `target.namespace` sont immuables après la création. `confirmation`, les
listes de sélection, `createNamespace` et `storageClassMapping` restent mutables.

`storageClassMapping` réécrit `storageClassName` sur les **PVC** au fur et à mesure de leur
restauration. C'est ainsi que vous récupérez un namespace adossé à Ceph sur un cluster qui
n'a pas Ceph. Il ne touche **pas** aux objets cluster-scoped : un `PersistentVolume` restauré
conserve la classe avec laquelle il a été capturé, parce qu'un PV représente un volume qui
existe déjà et que remapper sa classe ne re-provisionnerait rien.

Suivez-le :

```bash
kubectl get clusterrestore recover-team-x -w
```

```
NAME             PHASE       TARGET            AGE
recover-team-x   Running     team-x-restored   12s
recover-team-x   Completed   team-x-restored   3m41s
```

## Comment les PVC reviennent

Chaque snapshot de données porte la forme de la PVC sous forme de tags restic, enregistrés au
moment de la backup : `pvcsize`, `pvcclass` et `pvcmodes`. Un `ClusterRestore` les lit et
recrée la PVC avec sa capacité, sa storage class (après `storageClassMapping`) et ses access
modes d'origine — sans qu'aucun objet survivant du cluster n'en décrive quoi que ce soit.

Pour les snapshots pris avant la 0.2, qui n'ont pas ces tags, le repli est : la taille des
données arrondie au Gio supérieur avec 20 % de marge (minimum 1Gi), la storage class **par
défaut** du cluster, et `ReadWriteOnce`. Créez la PVC vous-même au préalable pour outrepasser
l'un ou l'autre.

## Restaurer les ressources cluster-scoped

La capture est large et automatique ; le restore est **opt-in et étroit**. Le champ
`clusterResources` a trois états significatifs :

```yaml
# 1. Omitted — nothing cluster-scoped is restored. The safe default.

# 2. Present with an empty include — everything in the snapshot. The snapshot
#    is already the curated capture set, so the field's mere presence is the
#    explicit opt-in.
clusterResources:
  include: []

# 3. Present with an include — only what matches.
clusterResources:
  include:
    - "storage.k8s.io/StorageClass"
    - "apiextensions.k8s.io/CustomResourceDefinition"
  exclude:
    - "storage.k8s.io/StorageClass/legacy-*"
```

`exclude` est appliqué en dernier, toujours.

:::caution[Le RBAC cluster et les CRD sont privilégiés]
Restaurer des `ClusterRoleBinding` recrée des habilitations à l'échelle du cluster
exactement telles qu'elles étaient — les subjects sont préservés mot pour mot, parce que
c'est tout l'objet d'une DR. Restaurer des CRD peut entrer en collision avec des operators
déjà installés. Ni l'un ni l'autre n'est jamais implicite : cela exige l'opt-in ci-dessus, le
champ de confirmation, et le rôle admin. Utilisez `dryRun` d'abord.
:::

Sur un cluster où ArgoCD ou Flux est propriétaire des objets cluster-scoped, capturez-les
sans hésiter — mais les restaurer ira se battre avec le contrôleur GitOps. Excluez-les au
moment du restore.

Il n'existe **aucun restore cluster-scoped sur le chemin du `Restore` namespacé**. Un tenant
ne capture ni ne restaure quoi que ce soit en dehors de son namespace.

## L'ordre d'application

L'ordre est fixe, et c'est lui qui fait qu'un restore à froid fonctionne :

1. les `CustomResourceDefinition`
2. les autres objets cluster-scoped — StorageClasses, PriorityClasses, IngressClasses,
   ClusterRoles et bindings, PersistentVolumes
3. les namespaces
4. les objets namespacés

Le Job de restore cluster-scoped s'exécute **jusqu'à son terme** avant que les volumes ne
soient traités, si bien que les StorageClasses et les CRD existent avant que quoi que ce soit
n'essaie de s'y lier.

## Le runbook du cluster nu

Tout a disparu : le cluster, l'etcd, les custom resources. Ce qu'il vous reste, c'est le
bucket et la KEK age que vous avez mise en séquestre hors du cluster.

La clé de plateforme enveloppée est mise en séquestre **dans le bucket lui-même**, à
`<prefix>/<clusterID>.crystal-meta/wrapped-dek.age` — voisine du prefix du repository,
invisible pour restic. C'est du chiffré sous votre KEK, et cela ne sert à rien à qui ne
détient que le bucket.

**1 — Installez l'operator** sur le nouveau cluster. Voir
[Installer avec Helm](/CrystalBackup/fr/docs/start/install/).

**2 — Restaurez les deux Secrets** depuis votre séquestre hors bande :

```bash
kubectl -n crystal-backup-system create secret generic cluster-kek \
  --from-file=identity=cluster-kek.txt

kubectl -n crystal-backup-system create secret generic dr-s3 \
  --from-literal=AWS_ACCESS_KEY_ID=... \
  --from-literal=AWS_SECRET_ACCESS_KEY=...
```

**3 — Créez la location**, en pointant vers le bucket existant avec **les mêmes** `clusterID`
et `prefix`. Ils composent le chemin du repository ; une faute de frappe ici vous emmène vers
un repository vide plutôt que vers une erreur.

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupLocation
metadata:
  name: dr-primary
spec:
  default: true
  clusterID: prod-eu-1
  s3:
    endpoint: https://s3.example.com
    bucket: crystal-backups
    prefix: dr
    forcePathStyle: true
    credentialsSecretRef:
      name: dr-s3
  encryption:
    clusterKEKSecretRef:
      name: cluster-kek
```

L'operator récupère la clé enveloppée depuis le séquestre du bucket — condition `DEKEscrowed`
avec la raison `Recovered` — et la discovery inventorie le repository.

```bash
kubectl get clusterbackuplocation dr-primary \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\n"}{end}'
kubectl get backuprepository dr-primary \
  -o jsonpath='{.status.snapshotCount}{" snapshots, "}{.status.namespacesPresent}{" namespaces\n"}'
```

**4 — Regardez ce qu'il y a.** La discovery projette des objets `Backup` dans les namespaces
qui **existent**. Sur un cluster nu, il n'y en a aucun — ce qui n'est pas grave, parce qu'un
`ClusterRestore` lit directement le repository et n'a pas besoin de la projection.

Pour énumérer ce que contient le repository avant que le moindre namespace n'existe, demandez
à restic :

```bash
export RESTIC_PASSWORD=$(kubectl -n crystal-backup-system get secret crystal-dek-dr-primary \
  -o jsonpath='{.data.dek}' | base64 -d | age -d -i cluster-kek.txt)
restic -r s3:https://s3.example.com/crystal-backups/dr/prod-eu-1 snapshots --tag crystalbackup
```

**5 — Restaurez d'abord les ressources cluster-scoped**, depuis n'importe quelle exécution :

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterRestore
metadata:
  name: dr-cluster-scoped
spec:
  source:
    locationRef: { name: dr-primary }
    namespace: team-x
    time: latest
  target:
    namespace: team-x
    createNamespace: true
  mode: Overwrite
  resources: []
  volumes: []
  clusterResources:
    include:
      - "storage.k8s.io/StorageClass"
      - "snapshot.storage.k8s.io/VolumeSnapshotClass"
      - "apiextensions.k8s.io/CustomResourceDefinition"
  confirmation: team-x
```

Notez `resources: []` et `volumes: []` — présents mais vides, ce qui signifie *rien de ce
type*. Ce restore ne touche que l'ensemble cluster-scoped.

Lancez-le avec `dryRun: true` d'abord. Sur un cluster reconstruit, cette étape peut recréer
des CRD et du RBAC cluster, et voir le plan est la différence entre une DR relue et une DR
pleine d'espoir.

**6 — Restaurez chaque namespace :**

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterRestore
metadata:
  name: dr-team-x
spec:
  source:
    locationRef: { name: dr-primary }
    namespace: team-x
    time: latest
  target:
    namespace: team-x
    createNamespace: true
  mode: Overwrite
  confirmation: team-x
```

Avec `resources` et `volumes` tous deux omis, ceci restaure tout ce que l'exécution a capturé
pour ce namespace.

**7 — Vérifiez.** Pas « la phase dit `Completed` » — vérifiez réellement. Contrôlez que les
workloads tournent, et qu'un fichier dont vous connaissez le contenu a bien ce contenu.

:::note[Une lacune connue]
Un `ClusterRestore` restaure aujourd'hui les objets cluster-scoped et les données des
volumes. Ramener les manifests des workloads **propres** au namespace par ce chemin réutilise
le moteur `resources[]` namespacé et fera l'objet d'un lot ultérieur. En attendant,
redéployez les workloads via votre mécanisme de livraison habituel, par-dessus les volumes
restaurés.
:::

## Ce qui n'est pas couvert

**L'etcd et le control plane.** Crystal Backup récupère les ressources et les données
applicatives. Le cluster lui-même est un problème distinct qui appelle une réponse distincte,
et prétendre le contraire serait exactement le genre d'affirmation que ce projet s'efforce de
ne pas faire.

## Voir aussi

- [Restaurer](/CrystalBackup/fr/docs/guides/restore/) — les modes, la sélection, le garde-fou
  de confirmation
- [Le runbook de DR](/CrystalBackup/fr/docs/operations/dr-runbook/) — la forme checklist
- [Diagnostic](/CrystalBackup/fr/docs/operations/troubleshooting/)
