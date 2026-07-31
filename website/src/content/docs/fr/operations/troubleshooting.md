---
title: Diagnostic
description: Un dépannage qui part du symptôme — ce que signifie chaque état bloqué et où vit la trace durable.
sidebar:
  order: 2
sourceFile: src/content/docs/operations/troubleshooting.md
sourceHash: 186d4833b6f5d5c49f35060e8b9818ef79702433
---

## Où sont les preuves

Les Jobs de mover et leurs pods sont supprimés à leur terme. Avant d'aller chercher des logs
qui n'existent plus, sachez que la trace durable est dans le status des objets :

| Ce qui a tourné | Trace durable |
|---|---|
| Un backup ou un restore par PVC | `status.volumes[]` sur le `Backup`, `status.resources` sur le `Restore` |
| Un hook | `status.hooks[]` et `status.postHookAttempts` sur le `Backup` |
| Un prune, un check, un forget ou un unlock | `status.recentMaintenance[]` sur le `BackupRepository` |
| La santé d'un repository | `status.lastCheckResult`, `staleLocks` sur le `BackupRepository` |

Et le premier regard passe-partout :

```bash
kubectl get <kind> <name> -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\t"}{.message}{"\n"}{end}'
```

Les conditions portent le reason. Presque tous les états bloqués ci-dessous y sont nommés.

## Une location n'atteint jamais `Ready`

```bash
kubectl get clusterbackuplocation <name> \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\t"}{.message}{"\n"}{end}'
```

| Reason | Signification |
|---|---|
| `KEKMissing` | Le Secret de `clusterKEKSecretRef` n'existe pas. Rien n'est jamais généré à sa place — provisionnez-le. |
| `MultipleDefaults` | Deux locations revendiquent `default: true`. Une seule le peut. |

Sinon, c'est généralement une question d'accessibilité. Le Job d'init du repository doit
puller l'image du mover et lancer `restic init` :

```bash
kubectl -n crystal-backup-system get jobs -l app.kubernetes.io/managed-by=crystal-backup
kubectl -n crystal-backup-system logs job/<init-job>
```

Les causes fréquentes, dans l'ordre où elles se produisent réellement :

- **Le digest de l'image du mover est un placeholder.** Un chart installé depuis un checkout
  des sources en porte un et le Job ne peut pas puller.
- **L'endpoint S3 est injoignable depuis le mover.** S'il est sur une adresse privée, les
  NetworkPolicies livrées le refusent — ajoutez `networkPolicy.extraMoverEgress`.
- **`forcePathStyle` n'est pas posé** face à une gateway non-AWS.
- **Une CA privée** et pas de `s3.caBundle`.
- **Des credentials sans accès en écriture** sur le prefix.

## Un backup dit `Completed` mais une PVC manque

```bash
kubectl -n <ns> get backup <run> \
  -o jsonpath='{range .status.volumes[*]}{.pvc}{"\t"}{.phase}{"\t"}{.reason}{"\n"}{end}'
```

`Skipped` avec `CSISnapshotUnsupported` signifie que le driver CSI de la PVC ne sait pas
faire de snapshot. Le backup est honnête — le volume est rapporté, pas abandonné
silencieusement — mais la donnée n'y est pas. Soit vous déplacez le workload vers une classe
capable de snapshot, soit vous acceptez le trou en connaissance de cause.

Une PVC totalement absente de la liste n'a pas été sélectionnée. Vérifiez `pvcSelector`.

## Un backup est bloqué en `Pending`

Le plus souvent, il **attend la queue exclusive du repository** — un prune, un check ou une
erasure tourne, et la queue n'admet qu'une opération mutante à la fois. Il n'y a pas de phase
`Queued` ; l'attente est silencieuse et se résout d'elle-même.

```bash
kubectl get backuprepository <name> \
  -o jsonpath='{range .status.recentMaintenance[*]}{.operation}{"\t"}{.startTime}{"\t"}{.result}{"\n"}{end}'
```

Si rien ne tourne, vérifiez le plafond de movers à l'échelle du cluster et les Jobs réels :

```bash
kubectl -n crystal-backup-system get jobs -l app.kubernetes.io/managed-by=crystal-backup
kubectl -n crystal-backup-system get pods -l app.kubernetes.io/managed-by=crystal-backup
```

Un pod de mover en `Pending` est un problème d'ordonnancement — capacité des nœuds, ou une
PVC temporaire qui n'arrive pas à se binder.

## Un backup est bloqué en `SnapshottingHooks`

Un hook tourne, ou a tourné et a été enregistré. Regardez ce qui s'est passé :

```bash
kubectl -n <ns> get backup <run> \
  -o jsonpath='{range .status.hooks[*]}{.phase}{"\t"}{.pod}{"\t"}{.result}{"\t"}{.message}{"\n"}{end}'
```

Si la condition `Ready` dit `HooksNeedServiceAccount`, c'est qu'un run du plan namespace a
déclaré des hooks sans identité. Posez `hooks.serviceAccountName` et accordez-lui
`create pods/exec` — voir [Hooks de cohérence](/CrystalBackup/fr/docs/guides/hooks/).

Un pre hook `Failed` avec `onError: Fail` avorte le run délibérément : la mise au repos n'a
pas eu lieu, donc un snapshot pris quand même aurait l'air application-consistent sans
l'être.

## `postHookAttempts` ne cesse de grimper

**C'est celui qui est urgent.** Un hook de libération échoue toujours, ce qui veut dire
qu'**une application est peut-être encore au repos**.

```bash
kubectl -n <ns> get backup <run> \
  -o jsonpath='{.status.postHookAttempts}{"\n"}{range .status.hooks[?(@.phase=="post")]}{.pod}{"\t"}{.result}{"\t"}{.message}{"\n"}{end}'
```

Les post hooks sont retentés dans un budget borné précisément à cause de cela. Si le budget
est épuisé, allez la libérer à la main — `status.hooks` vous dit quel pod et quelle était la
commande.

## Un restore reste en `AwaitingConfirmation`

C'est le comportement prévu. `spec.confirmation` est vide ou absent.

```bash
kubectl -n <ns> patch restore <name> --type=merge \
  -p '{"spec":{"confirmation":"<namespace>"}}'
```

Pour un `ClusterRestore` la valeur est `spec.target.namespace` ; pour une `ClusterErasure`
c'est l'identité cible.

Si à la place votre `kubectl apply` a été **rejeté d'emblée**, c'est que la valeur était
fausse, pas manquante — l'admission refuse une valeur qui ne correspond pas et admet une
valeur vide.

## Un restore échoue sur un volume

```bash
kubectl -n <ns> get restore <name> \
  -o jsonpath='{.status.phase}{"\t"}{.status.restoredVolumes}{"\t"}{.status.restoredBytes}{"\n"}'
kubectl -n <ns> get events --field-selector involvedObject.name=<restore-name>
```

| Reason | Signification |
|---|---|
| `RestoreBlockUnsupported` | `volumeMode: Block`. Non supporté. |
| Un échec de node-affinity ou d'attachement | Le volume cible est attaché ailleurs. Réduisez le workload à zéro et retentez. |

Un restore rapporte les échecs par ressource et **continue** ; il n'avorte pas au premier.
`status.resources.entries` porte le détail par objet, plafonné à 100, avec `truncated` qui
vous dit quand des entrées ont été laissées de côté.

## Les manifests d'un restore ne sont pas revenus

Vérifiez qu'ils ont été sélectionnés. La règle qui piège :

- champ **omis** → tout ce qui est de ce kind ;
- champ **présent mais vide** (`[]`) → **rien** de ce kind.

Donc `resources: []` ne restaure aucun manifest, délibérément. Voir
[La règle qui piège tout le monde](/CrystalBackup/fr/docs/guides/restore/#la-règle-qui-piège-tout-le-monde).

Vérifiez aussi qu'ils ont été **capturés** : `includeManifests` devait être à true sur le
run, et `status.manifests` sur le `Backup` devrait porter un ID de snapshot et un nombre de
ressources.

## Un objet est bloqué en `Terminating`

Un finalizer le retient pendant que le contrôleur démonte les expositions vivantes, les Jobs
de mover et les volumes de staging.

```bash
kubectl get <kind> <name> -o jsonpath='{.metadata.finalizers}{"\n"}'
kubectl -n crystal-backup-system logs deploy/crystal-backup --tail=200
```

:::danger[N'enlevez pas le finalizer]
Le retirer à la main est exactement la façon d'obtenir la fuite que le finalizer existe pour
éviter : une PVC temporaire orpheline et un `VolumeSnapshotContent` parqué en `Retain`
qu'aucun garbage collector ne pourra jamais atteindre, sur un objet cluster-scoped que
personne ne pensera à aller chercher. Cherchez plutôt ce que le contrôleur attend.
:::

## `staleLocks` est durablement non nul

Des objets de lock du repository plus vieux que le seuil de 30 minutes de restic
s'accumulent plus vite qu'ils ne sont récoltés. Chaque opération exclusive finira par caler
derrière eux.

Le lock d'un mover tué brutalement est normalement levé par une opération d'unlock. Si ce
n'est pas le cas :

```bash
kubectl get backuprepository <name> \
  -o jsonpath='{.status.staleLocks}{"\n"}{range .status.recentMaintenance[*]}{.operation}{"\t"}{.result}{"\t"}{.message}{"\n"}{end}'
```

Cherchez des opérations `unlock` en échec dans cet historique. Vérifiez qu'aucun second
écrivain ne touche le repository hors bande — la queue suppose un leader unique, et un
`restic prune` externe lancé à la main sort de cette hypothèse.

## `lastCheckResult: Failed`

restic a trouvé une corruption du repository. C'est un incident, pas une erreur transitoire.

```bash
kubectl get backuprepository <name> \
  -o jsonpath='{.status.lastCheckTime}{"\t"}{.status.lastCheckResult}{"\n"}'
```

Ne prunez pas. Lancez un check manuel plus détaillé, depuis une machine qui a la clé :

```bash
restic -r $REPO check --read-data-subset 10%
```

Si des packs sont endommagés, la récupération, c'est votre **seconde copie** — une
destination de sync externe, si vous en avez une. C'est à ça qu'elles servent.

## Le retard d'une sync ne cesse de croître

```bash
kubectl get clusterbackupexternalsync <name> \
  -o jsonpath='{.status.phase}{"\t"}{.status.lagSnapshots}{"\t"}{.status.snapshotsCopied}{"\t"}{.status.bytesCopied}{"\n"}'
```

C'est généralement la bande passante : la source produit plus vite que la sync ne déplace.
Soit vous rétrécissez `selection.namespaces`, soit vous la lancez plus souvent, pour que
chaque run ait moins à déplacer.

Si `phase` est `Failed`, regardez le Job de sync tant qu'il est encore là. Deux causes
fréquentes : les credentials de la destination, et une location de destination qui n'est pas
`Ready`.

## Un rejet d'admission que vous ne comprenez pas

Le message nomme la règle. Voir
[Règles d'admission](/CrystalBackup/fr/docs/reference/admission/).

Les deux qui surprennent le plus :

- **`spec.source is immutable`** — vous avez édité la source d'un `Restore`. Supprimez-le et
  créez-en un autre ; le contrôleur re-dérive la source à chaque passe, donc une édition en
  cours de route mélangerait deux instants dans un même restore.
- **`spec.clusterID is immutable`** — l'identité de la location compose le chemin du
  repository. Une édition re-pointerait silencieusement la location vers un autre repository
  sans qu'aucune donnée ne bouge.

## Obtenir de l'aide

Incluez, toujours : les `status.conditions` complètes de l'objet, les sous-objets de
`status` pertinents, la version de l'operator et la version de Kubernetes. Caviardez les
noms de bucket et les endpoints s'il le faut, mais gardez les reasons et les messages tels
quels — ce sont eux, le diagnostic.

[Ouvrir une issue](https://github.com/CrystalBackup/CrystalBackup/issues).
