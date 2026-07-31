---
title: Restaurer
description: Les modes Recreate et Overwrite, le modèle de sélection, le garde-fou de confirmation, et comment répéter un restore destructeur.
sidebar:
  order: 3
sourceFile: src/content/docs/guides/restore.md
sourceHash: 9ef4cabd0b6d5c34a64ca45cf0cba9981bc32f9f
---

Un restore a deux axes orthogonaux : le **mode** — comment les objets existants sont
réconciliés — et la **sélection** — ce qui entre dans le périmètre. Réglez ces deux-là
correctement et tout le reste en découle.

## Sa forme

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: Restore
metadata:
  name: recover-uploads
  namespace: team-x
spec:
  source:
    backup: dr-daily-20260730-020000
  mode: Overwrite
  volumes:
    - names: ["uploads"]
      include: ["images/2026/**"]
  confirmation: team-x
```

Il n'existe **aucun champ pour la location ni pour un namespace cible**. Un `Restore` désigne
un `Backup` de son propre namespace, et c'est là tout le modèle d'adressage. Si la source est
une backup d'origine cluster, l'operator résout lui-même le repository partagé, filtré par
votre namespace — un filtre qu'aucun champ de cet objet ne peut influencer.

## Choisir la source

Exactement un de `backup` et `time` doit être renseigné.

```yaml
# A named Backup in this namespace.
source:
  backup: nightly-20260730-010000

# The most recent one.
source:
  time: latest

# A point in time — RFC3339. A zone-less timestamp is read as UTC.
source:
  time: "2026-07-28T02:00:00Z"

# Disambiguate when both planes have backups near that time.
source:
  time: latest
  origin: cluster        # or: namespace
```

`origin` n'est valide qu'accompagné de `time`. Une fois qu'une source temporelle est
résolue, elle est **épinglée** — le restore ne dérive pas vers une backup plus récente entre
deux réconciliations.

`source` et `mode` sont **immuables après la création**. Le contrôleur les re-dérive à chaque
passe, si bien qu'une modification en cours d'exécution mélangerait deux points dans le temps,
ou deux modes destructeurs, à l'intérieur d'un même restore. Pour changer l'un ou l'autre,
supprimez le `Restore` et créez-en un autre.

## Le mode

| | `Overwrite` (par défaut) | `Recreate` |
|---|---|---|
| **Manifests** | Server-side apply, création ou mise à jour. Les objets absents de la backup sont **conservés**. | Les objets sélectionnés qui existent sont **supprimés**, puis créés depuis la backup. |
| **Fichiers des PVC** | Les fichiers de la backup écrasent les existants ou reviennent. Les fichiers présents dans la PVC mais absents de la backup sont **conservés**. | Correspondance exacte : les fichiers absents de la backup sont **supprimés**. |
| **Une PVC manquante** | Créée. | Créée. |

Sous le capot, c'est `restic restore --overwrite always`, avec `--delete` en plus dans
`Recreate`.

Choisissez `Overwrite` quand vous remettez en place quelque chose qui a été perdu. Choisissez
`Recreate` quand vous avez besoin que la cible *soit* la backup — après une corruption, ou
quand ce sont des fichiers parasites qui posent problème. `Recreate` supprime ; assurez-vous
que c'est bien ce que vous voulez.

## La sélection

Deux listes indépendantes, `resources` (les manifests) et `volumes` (les données des PVC). À
l'intérieur d'un item les conditions sont combinées par ET ; entre items, par OU. Une chose
est restaurée si **n'importe quel** item lui correspond.

```yaml
resources:
  - selector:
      matchLabels:
        app: web
    include: ["apps/Deployment"]
    exclude: ["apps/Deployment/legacy-*"]
  - include: ["apps/StatefulSet/postgres", "Secret/db-creds"]

volumes:
  - names: ["data-postgres-0"]
  - names: ["uploads"]
    include: ["images/2026/**"]
    exclude: ["images/2026/tmp/**"]
    targetPath: "/"
```

`include` et `exclude` sur `resources` sont des globs `<group>/<Kind>[/<name>]`. Sur
`volumes`, ce sont des globs de **fichiers** à l'intérieur de la PVC, et c'est ainsi qu'un
restore partiel fonctionne.

### La règle qui piège tout le monde

Chaque liste a son défaut **indépendant** :

| Ce que vous avez écrit | Ce que cela signifie |
|---|---|
| le champ est **omis** | tout ce qui est de ce type |
| le champ est **présent mais vide** (`[]`) | **rien** de ce type |
| le champ liste des items | seulement ce que les items désignent |

Omettre les deux restaure donc le namespace entier, et `resources: []` avec `volumes`
renseigné signifie « les données seulement, pas les manifests ». Ces cas sont réellement
différents, et l'API est construite pour les garder différents — les types Go omettent
délibérément `omitempty` sur les deux listes, pour qu'un slice vide ne puisse pas être relu
silencieusement comme « tout ».

Deux autres règles à connaître :

- **Le premier item qui correspond l'emporte, pour les volumes.** Quand plusieurs items de
  `volumes` correspondent à la même PVC, c'est le premier qui s'applique — une PVC est
  restaurée par exactement une passe de mover. Un item sans `names` correspond à toutes les
  PVC, alors placez vos items spécifiques en premier.
- **Les exclusions faites à la backup sont définitives.** Tout ce qui a été exclu au moment
  de la capture des manifests ne peut pas être ré-inclus au restore. Ce n'est pas dans le
  snapshot.

`targetPath` remplace la racine du restore à l'intérieur de la PVC. Vide ou `/` désigne la
racine de la PVC, et les segments `..` sont rejetés.

## La confirmation

Tout restore susceptible de modifier des objets existants exige un `spec.confirmation` égal
au namespace cible — son propre namespace pour un `Restore`, `spec.target.namespace` pour un
`ClusterRestore`. Comme les deux seuls modes sont `Recreate` et `Overwrite`, en pratique
**tout restore en a besoin**.

Deux comportements, et la différence compte :

- **Une valeur erronée est rejetée à l'admission.** L'objet n'est jamais créé.
- **Une valeur vide ou absente est admise**, et le restore se gare en phase
  `AwaitingConfirmation` avec la condition `Ready=False`, raison `ConfirmationRequired`.

Le second cas est le double geste délibéré. Créez le restore, lisez ce qu'il s'apprête à
faire, puis tapez le namespace :

```bash
kubectl -n team-x patch restore recover-uploads --type=merge \
  -p '{"spec":{"confirmation":"team-x"}}'
```

`confirmation` est l'un des rares champs mutables, précisément pour que cela fonctionne.

## Répéter : `dryRun`

```yaml
spec:
  dryRun: true
```

Déroule tout le pipeline — l'ordonnancement, la sélection, la résolution du mode — avec des
applies en server-side dry-run, ne persiste rien, et écrit le plan dans `status.resources`.
Avant un `Recreate` contre un namespace vivant, c'est la différence entre un restore relu et
un restore plein d'espoir.

```bash
kubectl -n team-x get restore recover-uploads \
  -o jsonpath='{range .status.resources.entries[*]}{.outcome}{"\t"}{.kind}{"/"}{.name}{"\t"}{.reason}{"\n"}{end}'
```

Les issues possibles sont `Created`, `Configured` (un objet existant a été appliqué par
dessus), `Recreated` (supprimé puis créé) et `Failed`. Sous `dryRun`, ce sont des actions
**planifiées**.

:::caution[La moitié « volumes » n'a pas de dry run]
`dryRun` couvre le pipeline des manifests. Il ne simule pas le restore des données. Un dry
run vous dit quels objets changeraient ; il ne vous dit pas quels fichiers seraient supprimés
par un `Recreate`.
:::

## Le suivre

```bash
kubectl -n team-x get restore recover-uploads -w
```

```
NAME              PHASE                  AGE
recover-uploads   AwaitingConfirmation   4s
recover-uploads   Running                31s
recover-uploads   Completed              2m18s
```

Phases : `Pending`, `AwaitingConfirmation`, `Running`, `Completed`, `PartiallyFailed`,
`Failed`. Un restore rapporte les échecs par ressource et **continue** ; il ne s'interrompt
pas au premier.

```bash
kubectl -n team-x get restore recover-uploads \
  -o jsonpath='{.status.restoredVolumes}{" volumes, "}{.status.restoredBytes}{" bytes, "}{.status.restoredResources}{" resources\n"}'
```

Le rapport par ressource est plafonné à 100 entrées avec 20 chemins de champs modifiés
chacune — la limite de 1 Mio par objet dans etcd est un plafond dur, et un status qui ne peut
pas être écrit perd tout le rapport plutôt que sa fin. `status.resources.truncated` vous
indique quand c'est arrivé.

## À quoi s'attendre en exploitation

- **Les movers s'exécutent dans `crystal-backup-system`**, jamais dans votre namespace. Votre
  namespace reçoit des PVC restaurées et rien d'autre.
- **Au plus quatre Jobs de mover par restore** s'exécutent en même temps.
- **Une PVC qui n'existe pas est créée** avec la capacité, la storage class et les access
  modes enregistrés dans les tags du snapshot au moment de la backup. Créez-la vous-même au
  préalable pour outrepasser l'un ou l'autre.
- **Une PVC qui existe et est bound** est restaurée sur place. Si elle est attachée à
  exactement un nœud, le mover est épinglé sur ce nœud.
- **Une PVC restaurée est à vous.** Elle porte l'annotation
  `crystalbackup.io/restored-from: <run>` et aucun des labels de l'operator, si bien que rien
  ne viendra jamais la garbage-collecter.
- **Restaurer sous un writer vivant est déconseillé.** Réduisez d'abord le workload à zéro ;
  la manœuvre recommandée est `Recreate` accompagné d'un scale-down.
- **`volumeMode: Block` n'est pas supporté** — ces volumes échouent avec la raison
  `RestoreBlockUnsupported`.
- **Les attributs étendus `trusted.*` ne sont pas restaurés** (ils nécessitent
  `CAP_SYS_ADMIN`).

## Restaurer depuis la DR cluster en tant que tenant

Rien de différent à faire. Un `Backup` d'origine cluster est projeté dans votre namespace par
la discovery, et vous le nommez comme n'importe quel autre :

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: Restore
metadata:
  name: recover-from-dr
  namespace: team-x
spec:
  source:
    backup: dr-daily-20260730-020000
  mode: Overwrite
  confirmation: team-x
```

Ce qui se passe en dessous : l'operator lance un listing contre le repository partagé avec le
filtre `namespace=team-x` construit à partir des métadonnées de cet objet, et ne remet au
mover que les IDs de snapshot renvoyés. Une PVC que le listing filtré ne résout pas **échoue
de manière fermée** — il n'y a aucun repli non filtré. Vous ne détenez jamais la clé du
repository partagé, et aucun pod de votre namespace non plus.

Le coût est un Job de listing supplémentaire, quelques secondes, avant que les données ne
bougent.

## Les restores d'administrateur

Pour restaurer dans un autre namespace, dans un namespace qui n'existe plus, ou sur un
cluster reconstruit, voir
[Disaster recovery](/CrystalBackup/fr/docs/guides/disaster-recovery/).
