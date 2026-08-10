---
title: Labels et annotations
description: Chaque label, annotation et finalizer crystalbackup.io/*, ce qui le pose, et ceux que vous posez vous-même.
sourceFile: src/content/docs/reference/labels.md
sourceHash: 58efe3b773289dc7a62f60d1aaab851a3f235e1d
---

Tout ce qui suit est sous le domaine `crystalbackup.io`, qui est aussi le groupe d'API. La
source de vérité est `internal/apiconst/apiconst.go`.

## Celui que vous posez

| Clé | Où | Signification |
|---|---|---|
| `crystalbackup.io/protect` | sur un **Namespace** | Une convention, pas une clé magique. L'operator la lit parce que le `namespaces.matchLabels` de votre `ClusterBackupSchedule` la nomme ; il ne la pose jamais. `crystalbackup.io/protect: "true"` est l'opt-in habituel. |
| `crystalbackup.io/tenant` | sur un **Namespace** | Regroupe plusieurs namespaces sous un même tenant. En son absence, le tenant d'un namespace est son propre nom. Détermine le tag restic `tenant=`, et donc la portée d'une `ClusterErasure` avec `target.tenant`. |

## Les labels qui méritent une requête

| Clé | Sur | Signification |
|---|---|---|
| `crystalbackup.io/origin` | `Backup` | `cluster` ou `namespace` — quel plan l'a produit. Celui que les tenants utilisent le plus. |
| `crystalbackup.io/cluster-backup` | `Backup` | Le run `ClusterBackup` qui l'a déployé en éventail. Un **label, pas une ownerReference**, si bien qu'élaguer l'historique des runs ne supprime jamais un backup restaurable. |
| `crystalbackup.io/schedule` | `Backup`, `ClusterBackup` | Le schedule d'origine. Absent sur un run manuel. Reflète le tag restic `schedule=`. |
| `crystalbackup.io/namespace` | `Backup`, objets possédés par l'operator | Le namespace d'origine d'un `Backup` enfant, sous forme de label interrogeable. Reflète le tag restic `namespace=`. |
| `crystalbackup.io/tenant` | `Backup` | Le tenant résolu. Reflète le tag restic `tenant=`. |
| `crystalbackup.io/location` | `BackupRepository` | La location que ce repository sert. Avec `crystalbackup.io/namespace`, c'est le lien retour qui tient lieu d'ownerReference sur le plan namespace — un objet cluster-scoped ne peut pas être possédé par un objet namespacé. |

Requêtes utiles :

```bash
# Everything a given DR run produced.
kubectl get backups -A -l crystalbackup.io/cluster-backup=dr-daily-20260730-020000

# What a tenant produced themselves, versus what the platform did for them.
kubectl -n team-x get backups -l crystalbackup.io/origin=namespace
kubectl -n team-x get backups -l crystalbackup.io/origin=cluster

# Every namespace covered by one tenant identity.
kubectl get ns -l crystalbackup.io/tenant=acme
```

## Les annotations que vous verrez

| Clé | Sur | Signification |
|---|---|---|
| `crystalbackup.io/restored-from` | une PVC qu'un restore a **créée** | Le run dont elle vient. Délibérément une annotation et jamais accompagnée des labels propres à l'operator — une PVC restaurée est **votre** objet et ne doit jamais être ramassée par le garbage collector. |
| `crystalbackup.io/projected` | `Backup` | `"true"` quand l'objet est une projection en lecture seule reconstruite depuis le repository par la discovery. Le contrôleur traite un tel `Backup` comme inerte : il ne snapshote ni ne déplace jamais de données pour lui. C'est pourquoi certains objets `Backup` ne peuvent pas être actionnés. |
| `crystalbackup.io/secret-data-excluded` | un `Secret` restauré | `"true"` quand le Secret a été capturé sous `manifestOptions.excludeSecretData` et que ses `data`/`stringData` ont été retirés. Le Secret restauré existe et est vide, et il le dit — un workload qui a besoin des valeurs échoue visiblement au lieu de démarrer silencieusement avec les mauvaises. |

## Les annotations de hook

Honorées sur les pods uniquement quand le schedule pose `hooks.honorAnnotations: true`.
Quatre suffixes sur chacun des deux préfixes.

| Clé | Valeur |
|---|---|
| `crystalbackup.io/pre-backup-command` | argv JSON, p. ex. `'["psql","-c","CHECKPOINT"]'` |
| `crystalbackup.io/pre-backup-container` | nom du container |
| `crystalbackup.io/pre-backup-timeout` | une durée, p. ex. `30s` |
| `crystalbackup.io/pre-backup-on-error` | `Fail` ou `Continue` |
| `crystalbackup.io/post-backup-command` | argv JSON |
| `crystalbackup.io/post-backup-container` | nom du container |
| `crystalbackup.io/post-backup-timeout` | une durée |
| `crystalbackup.io/post-backup-on-error` | `Fail` ou `Continue` |

Les annotations **remplacent** les hooks du schedule pour ce pod — elles ne fusionnent
jamais. L'annotation fournit la commande, jamais l'identité : les hooks tournent toujours
sous `hooks.serviceAccountName`. Voir
[Hooks de cohérence](/CrystalBackup/fr/docs/guides/hooks/).

## Finalizers

Voilà pourquoi un objet peut rester en `Terminating`. Ils existent pour que le contrôleur
puisse démonter les expositions vivantes, les Jobs de mover et les volumes de staging
**avant** que l'objet ne disparaisse — sans eux, supprimer un `Backup` en cours de run ferait
fuiter une PVC temporaire et un `VolumeSnapshotContent` qu'aucun garbage collector ne peut
plus atteindre.

| Finalizer | Sur |
|---|---|
| `crystalbackup.io/location` | `ClusterBackupLocation`, `BackupLocation` |
| `crystalbackup.io/repository` | `BackupRepository` |
| `crystalbackup.io/backup` | `Backup` |
| `crystalbackup.io/restore-teardown` | `Restore` |
| `crystalbackup.io/cluster-restore-teardown` | `ClusterRestore` |

Supprimer une location ou un repository n'efface **jamais** les objets du repository.
L'effacement est une [`ClusterErasure`](/CrystalBackup/fr/docs/guides/erasure/) explicite et
confirmée.

Si un objet est bloqué en `Terminating`, lisez les logs de l'operator plutôt que de retirer
le finalizer à la main. Le retirer, c'est la façon d'obtenir la fuite que le finalizer existe
pour éviter.

## Interne à l'operator

Vous verrez ceux-ci sur les objets de `crystal-backup-system` pendant qu'un backup ou un
restore est en cours. Ils sont listés pour qu'ils ne soient pas un mystère ; rien à
l'extérieur de l'operator ne devrait en dépendre.

| Clé | Signification |
|---|---|
| `crystalbackup.io/backup` | Le `Backup` auquel appartient un objet d'exposition ou un Job de mover, par son nom. Présent sur les deux plans — contrairement à `crystalbackup.io/cluster-backup`, que seul un run de DR cluster possède — c'est donc avec lui que le balayage de teardown et le reaper d'orphelins résolvent le propriétaire. (Même chaîne que le finalizer `Backup` ; labels et finalizers sont des champs différents.) |
| `crystalbackup.io/pvc` | La PVC source à laquelle appartient un objet d'exposition ou un Job de mover. |
| `crystalbackup.io/restore`, `crystalbackup.io/cluster-restore` | Le restore propriétaire. |
| `crystalbackup.io/pv-role` | `twin` ou `transplant` — marque un PersistentVolume qu'un restore a créé ou adopté. |
| `crystalbackup.io/exposure-kind` | Le mécanisme d'exposition de la cible avec lequel un mover de restore a démarré. |
| `crystalbackup.io/mover-role` | `data` ou `manifest` — ce à quoi le pod de mover a le droit de parler. Les NetworkPolicies sélectionnent dessus, parce qu'une NetworkPolicy sélectionne les pods par label et non par ServiceAccount. |
| `crystalbackup.io/mover-job`, `crystalbackup.io/operator-namespace` | Sur un RoleBinding transitoire : quel Job il accompagne, et quel operator l'a créé. |
| `crystalbackup.io/exposure-node` | Sur un claim de staging : le nœud auquel le volume cible était attaché. |
| `crystalbackup.io/mover-result` | Le JSON de résultat d'un mover de restore terminé, conservé parce que le pod est supprimé avant qu'on puisse le lire. |
| `crystalbackup.io/exposures-cleaned` | `"true"` une fois que le balayage de teardown terminal a vérifié que chaque objet d'exposition a été collecté. |

## Hors du domaine, mais porteur

`app.kubernetes.io/managed-by: crystal-backup` est estampillé sur chaque objet de workload
géré par l'operator — Jobs de mover, objets d'exposition, Secrets de clés wrappées. C'est le
sélecteur unique de « tout ce que Crystal Backup a créé » :

```bash
kubectl -n crystal-backup-system get jobs,secrets -l app.kubernetes.io/managed-by=crystal-backup
```

Ce n'est délibérément **pas** `app.kubernetes.io/name=crystal-backup`, qui est le label du
pod de l'operator lui-même.

## Les tags restic

Ce ne sont pas des labels Kubernetes, mais c'est la même idée à l'intérieur du repository.
Chaque snapshot porte :

| Tag | Valeur |
|---|---|
| `crystalbackup` | le marqueur que porte chaque snapshot Crystal Backup |
| `tenant=` | le tenant résolu |
| `namespace=` | le namespace d'origine |
| `pvc=` | la PVC source (snapshots de données) |
| `kind=` | `data`, `manifests` ou `cluster-manifests` |
| `schedule=` | le schedule d'origine |
| `run=` | le nom du run — la même chaîne que le nom de l'objet `Backup` |
| `pvcsize=`, `pvcclass=`, `pvcmodes=` | la forme de la PVC, sur les snapshots `kind=data` depuis la 0.2 — c'est ce qui permet à un `ClusterRestore` de reconstruire une PVC alors que rien n'a survécu pour la décrire |

Le `host` du snapshot est le `clusterID` de la location ; les `paths` sont
`/data/<namespace>/<pvc>`, `/manifests/<namespace>` et `/cluster-manifests`.

`namespace=` est le tag sur lequel l'operator filtre quand il médie le restore d'un tenant
contre le repository partagé — construit depuis le `metadata.namespace` de la custom resource
elle-même, ce qui explique qu'il ne puisse pas être forgé. Voir
[Tenancy et isolation](/CrystalBackup/fr/docs/understand/tenancy/).
