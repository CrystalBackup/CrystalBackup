---
title: Mettre à niveau
description: Mettre à niveau le chart, le problème des CRD que Helm ne résout pas pour vous, et ce que garantit l'API v1alpha1.
sidebar:
  order: 10
sourceFile: src/content/docs/guides/upgrading.md
sourceHash: 58ef3baf9da44296552a764415a19b8376da9e0c
---

## Ce que signifie un numéro de version ici

Le projet suit SemVer sur la majeure `0` : chaque milestone est une release **mineure**
(`M_n` → `0.n.z`), les itérations de durcissement sont des **patches**. Une seule chaîne de
version couvre l'image de l'operator, celle du mover, celle du sync et l'`appVersion` du
chart — c'est un seul train de release, et ils sont censés avancer ensemble.

L'API des CRD est en **`v1alpha1`**, et ce n'est pas une formalité. Des champs peuvent être
ajoutés, renommés ou supprimés d'une release mineure à l'autre jusqu'à la `1.0.0`, ce qui est
une décision délibérée de stabilité d'API prise après M9. Lisez les notes de release avant
chaque montée de mineure ; ne supposez pas qu'un manifest qui s'appliquait contre la `0.5`
s'applique contre la `0.6`.

Les releases de patch ne changent pas l'API.

## Le problème des CRD

**Helm installe les CRD à la première installation et ne les met jamais à niveau.** C'est le
comportement de Helm, pas un choix de ce chart, et cela veut dire qu'un `helm upgrade` seul
vous laissera avec un operator neuf sur de vieilles CRD — ce qui échoue de la façon la plus
déroutante possible : les champs que vous renseignez sont silencieusement élagués par l'API
server, et l'operator réconcilie comme si vous ne les aviez jamais renseignés.

Appliquez les CRD vous-même, avant le chart :

```bash
# Pull the chart and take its CRDs. Use the version you are upgrading *to*;
# 0.6.6 is the current release.
helm pull oci://ghcr.io/crystalbackup/charts/crystal-backup --version 0.6.6 --untar
kubectl apply -f crystal-backup/crds/

# Then upgrade the operator.
helm upgrade crystal-backup \
  oci://ghcr.io/crystalbackup/charts/crystal-backup \
  --version 0.6.6 \
  --namespace crystal-backup-system
```

Un `kubectl apply` sur des CRD est additif et sûr : il ajoute les nouveaux champs et ne
supprime jamais d'objets stockés.

## 0.6.5 → 0.6.6 : rien à appliquer, mais un backup en échec met désormais plus de temps à le dire

**Cette release ne change ni les CRD ni l'API.** Aucun champ n'a été ajouté, renommé ou supprimé
sur quelque type que ce soit : contrairement à toutes les sections ci-dessous, ce saut n'a
**aucune étape de schéma**. Lancez tout de même le `kubectl apply` ci-dessus s'il fait déjà partie
de votre procédure — appliquer une CRD inchangée ne change rien — mais n'allez pas chercher les
nouveaux champs : il n'y en a pas. Ce qui change, c'est le comportement, en cinq endroits, dont
deux changent un **résultat** qu'un dashboard ou un script peut être en train de surveiller.

**Un quiesce avorté libère désormais les applications qu'il a gelées avant de rapporter `Failed`.**
En `0.6.5`, un hook pre en échec sous `onError: Fail` arrêtait la chaîne — correctement — et
écrivait immédiatement le `Failed` terminal. Or les hooks qui avaient **déjà réussi** avaient gelé
leurs applications, et le court-circuit « déjà terminal » faisait que rien ne les dégelait jamais :
le run rapportait *le quiesce n'a pas marché* alors qu'une partie avait marché et était toujours en
vigueur. La `0.6.6` exécute la libération **d'abord** et retient l'écriture terminale tant qu'un
dégel est dû. Le verdict est inchangé — la raison terminale reste `PreHookFailed` — mais **le
Backup reste désormais non terminal pendant que le dégel réessaie**, dans le même budget de trois
tentatives dont disposait déjà le chemin d'unfreeze, en portant une raison `Ready` à
`ReleasingAfterAbortedQuiesce` qui nomme l'abandon et la tentative en cours. Un dégel qui continue
d'échouer finit toujours sur l'Event Warning `UnfreezeFailed` existant, et ne rapporte `Failed`
qu'ensuite. **Donc si vous alertez sur la phase terminale, cette alerte arrive désormais plus
tard** — quelques secondes à deux réconciliations, pas des minutes — et l'état intermédiaire est le
retry, pas un blocage :

```bash
kubectl get backup <name> -o \
  jsonpath='{.status.phase}{"\t"}{range .status.conditions[?(@.type=="Ready")]}{.reason}{end}{"\n"}'
```

Seules les entrées pre `Succeeded` sont dégelées. Un hook `Skipped` n'a jamais tourné, et un dégel
contre un pod que rien n'a gelé est une commande que son propriétaire n'a jamais demandée.

**Un échec sous politique `Fail` dans un hook *post* ne saute plus le reste de la chaîne.**
S'arrêter au premier échec est juste pour la phase pre et faux pour la phase post, où chaque entrée
est un dégel dû à une application **différente** : un premier hook post définitivement cassé
signifiait que les pods suivants n'étaient jamais dégelés du tout, sur les trois tentatives.
Bruyant — `UnfreezeFailed` se déclenchait bien — mais à propos du mauvais pod. Les hooks post
restants sont désormais tentés quoi qu'il arrive.

**Un restore propre ne peut plus être re-rapporté comme peut-être cassé, et ses compteurs ne
peuvent plus retomber à 0.** Deux passes des contrôleurs de restore jetaient leur travail sur un
retour d'erreur. L'une re-dérivait le verdict de l'apply des manifests depuis un pod mover déjà
disparu, transformant l'apply propre d'un namespace entier en *« did not report a result … some
resources may have been applied »* avec `failedCount 1`. L'autre publiait un décompte de volumes
vide quand elle ne pouvait pas lire le recensement du mover, écrasant un vrai quatre-sur-six par
`plannedVolumes: 0`. **Les deux étaient de mauvaises réponses, pas des réponses manquantes**,
devant quelqu'un en train de décider s'il abandonne un restore. `plannedVolumes` et `failedVolumes`
conservent désormais leurs dernières valeurs publiées quand une passe ne peut pas les recompter, et
la condition dit pourquoi — un panneau qui s'appuie sur eux cesse donc de retomber à zéro en pleine
restauration.

**Un `ClusterErasure` qui attend la levée d'un object lock cesse d'émettre quatre Warnings par
minute.** L'Event `ErasureBlocked` se déclenchait à chaque passe d'un recheck de 15 secondes —
pendant des semaines, sur le chemin de conformité le plus sensible qui existe, noyant le dossier
même que l'on montre pour affirmer que des données ont été détruites. Il se déclenche désormais sur
la **transition**, et la cadence du recheck est celle d'une heure, configurée depuis toujours et
jamais utilisée une seule fois. La décision de l'erasure n'a pas changé ; si vous avez bâti quoi que
ce soit sur le *rythme* de cet Event, c'est désormais un Event par transition.

**`selfcheck` et `preflight.sh` gagnent une observation, purement additive.** Les VolumeSnapshots
**bound** à un content et toujours pas ready au bout d'une heure sont désormais rapportés : un
objet `stuckSnapshots` à côté de `leakIndicators` dans le JSON, une qualification
`stuckSnapshotsOnStorageClass` sur le recensement de couverture par PVC apparu en `0.6.5`, et la
même constatation dans le script de preflight. Cela existe parce qu'un cluster a été trouvé qui
n'avait **jamais** sauvegardé un seul volume CephFS — le verdict du produit était juste chaque
nuit et aucun artefact ne disait que cela n'avait jamais marché, tandis que `preflight.sh`
déclarait la classe parfaitement utilisable. C'est délibérément une **observation, pas un
verdict** : rien ne devient `Skipped` ni `Failed`, aucune phase ne bouge, et une StorageClass sans
aucun snapshot n'est pas mise en cause. La seule chose à vérifier à la mise à niveau est un parser
qui rejetterait les clés JSON inconnues.

**Deux nouvelles valeurs `soak`, toutes deux réglées par défaut sur le comportement actuel.**
`soak.accessModes` (défaut `[ReadWriteOnce]`) et `soak.storageClassName` (défaut `""`, la classe
par défaut du cluster). Elles existent ensemble pour un seul cas : une classe RWX supprime le
transfert de volume exclusif qui a fait perdre l'archive d'une quinzaine lorsqu'un autosync a
remplacé le pod du collecteur — le `strategy: Recreate` du chart a fait ce qu'il promet et
l'archive est restée inatteignable quand même. Poser le mode sans une classe qui le fournit donne
un PVC qui ne se lie jamais. Le collecteur écrit également désormais sa table de high-water par
classe sur **sa propre stderr** au `SIGTERM`, seul canal dont dispose un pod en train de se
terminer et qui n'est pas le volume qu'il est sur le point de relâcher. Si `soak.enabled` vaut
`false`, rien de tout cela ne vous concerne ; s'il vaut `true`, lisez la procédure de remise à
zéro de `hack/soak/README.md` avant de remplacer ce pod exprès, et exportez l'archive d'abord.

## 0.6.4 → 0.6.5 : un panneau qui affichait un nombre peut désormais afficher 0, et un backup qui échouait peut désormais aboutir

Rien à faire avant la mise à niveau au-delà de l'application des CRD, et aucune donnée ne bouge.
Mais deux choses changent un **résultat** et pas seulement une formulation, et l'une d'elles va
faire taire un dashboard existant à propos de namespaces qui, eux, restent non protégés. Lisez
ceci avant d'en conclure que la mise à niveau a corrigé quelque chose qu'elle n'a pas corrigé.

**Un compteur qui était un seul champ en fait désormais deux.** En `0.6.4`, un namespace dont la
coordonnée de fan-out entrait en collision était compté comme *failed* — le même champ que celui
utilisé pour un namespace réellement tenté et en échec, si bien qu'aucun des deux nombres ne
pouvait être cru. La `0.6.5` donne à la collision son propre compteur,
`status.namespacesBlocked`, avec sa propre métrique
`crystalbackup_clusterbackup_namespaces_blocked`, et `namespacesFailed` ne compte plus que les
enfants qui ont réellement échoué. **Un dashboard ou une alerte qui ne s'appuie que sur
`crystalbackup_clusterbackup_namespaces_failed` affichera donc 0 là où il affichait un nombre** —
les namespaces sont tout aussi non protégés, et le panneau qui le disait a cessé de le dire.
Ajoutez la série `blocked` à côté de la série `failed`, idéalement avant de mettre à niveau :

```bash
kubectl get clusterbackup <name> -o \
  jsonpath='{.status.namespacesSucceeded}/{.status.namespacesFailed}/{.status.namespacesBlocked}{"\n"}'
```

**`onError: Continue` sur un hook pre est désormais honoré, et c'est un changement de résultat.**
La `0.6.4` enregistrait `Failed` pour tout échec de hook, quelle que soit la politique : un
utilisateur qui avait explicitement demandé que le backup continue malgré un quiesce en échec
obtenait un backup terminalement en échec et aucun snapshot du tout. Le même run en `0.6.5`
**aboutit, avec un snapshot**, et porte une nouvelle condition `ApplicationConsistent` posée à
`False` avec la raison `CrashConsistent`, qui nomme le pod, le conteneur et l'erreur. C'est le
contrat documenté et c'est ce à quoi ce champ a toujours servi — mais si vous avez posé
`onError: Continue` et que vous traitiez l'échec dur comme votre signal, le signal est désormais
une condition sur un Backup `Completed`, et le point de restauration qu'elle décrit est
crash-consistent :

```bash
kubectl get backup <name> \
  -o jsonpath='{range .status.conditions[?(@.type=="ApplicationConsistent")]}{.status} {.reason}: {.message}{"\n"}{end}'
```

La condition est délibérément à trois états : **absente** quand aucun hook pre n'a tourné, pour
qu'un backup sans hooks n'hérite pas d'un `False` sur lequel personne ne peut agir.

**Une nouvelle alerte `critical` peut se déclencher.** `CrystalbackupBackupMissedCritical`
escalade selon l'ampleur, avec une borne à trois fois la période propre du schedule plus une
heure — 4 h pour un schedule horaire, 73 h pour un schedule nocturne. **Aucun seuil existant n'a
bougé**, et le palier `warning` est inchangé et se déclenche en parallèle : rien de ce que vous
routez déjà n'est réduit. Mais si votre Alertmanager traite `critical` différemment de `warning` —
une astreinte plutôt qu'un ticket — un cluster qui n'a rien produit pendant trois périodes de
schedule réveillera désormais quelqu'un. Il y a également une nouvelle métrique
`crystalbackup_restore_volumes_failed`.

**Nouveaux champs de status, tous additifs et tous optionnels.** Rien n'est renommé et rien n'est
supprimé :

- `status.volumes[]` gagne `firstAttemptAt` et `phaseEnteredAt` ;
- `status.hooks[]` gagne `onError` — la politique qui était en vigueur pour cette exécution. Vide
  se lit comme `Fail`, et c'est précisément ce qui rend sûre une mise à niveau par-dessus un
  Backup déjà à l'intérieur de sa fenêtre de gel : les entrées écrites par l'ancien operator
  avortent exactement comme avant, au lieu de devenir tolérées par un binaire plus récent ;
- `Restore` et `ClusterRestore` gagnent `plannedVolumes` et `failedVolumes`, écrits aussi sur les
  passes non terminales, si bien qu'un restore long progresse visiblement ;
- `ClusterErasure` gagne `snapshotsTargeted` et `snapshotsRemaining` ;
- `ClusterBackup` gagne `namespacesBlocked`.

Appliquez les CRD avant le chart, comme ci-dessus. Sans cela, l'API server élague chacun de ces
champs et le nouvel operator réconcilie comme si vous ne les aviez jamais eus.

**Les objets d'exposition gagnent un label `crystalbackup.io/backup`, et les résidus antérieurs à
la mise à niveau ne l'ont pas.** Tout snapshot ou content fuité laissé par la `0.6.4` ou une
version antérieure ne porte pas ce label : le teardown en ligne ne le sélectionne donc pas. C'est
le **reaper d'orphelins** qui le ramasse, sur sa propre passe, par exclusion — il ne reape que
lorsqu'aucun Backup de ce namespace ne pourrait encore vouloir une exposition de ce PVC, et il
refuse sur une liste qu'il ne peut pas lire. Attendez-vous donc à voir les vieux résidus
disparaître au bout d'une passe ou deux plutôt qu'au teardown du backup suivant, et attendez-vous
à ce que le reaper le dise. Rien ne force la suppression du finalizer d'un autre contrôleur : un
objet réellement bloqué est désormais rapporté comme bloqué, avec les finalizers nommés, au lieu
d'être journalisé comme reapé.

**`selfcheck` gagne `--format text`** — un rapport compact en langage clair, avec un recensement
de couverture par PVC : c'est la première fois que le produit peut répondre à *ce qui sera
sauvegardé et ce qui ne le sera pas*, y compris les PVC qu'aucun schedule ne sélectionne. **JSON
reste la sortie par défaut**, donc tout ce qui parse la sortie de `selfcheck` — y compris le
CronJob non surveillé du kit de soak — n'est pas affecté.

## 0.6.3 → 0.6.4 : une location qui rapportait Ready peut désormais rapporter Degraded

Rien à faire avant la mise à niveau, et aucune donnée ne bouge — mais le nouvel operator peut
rapporter une location comme non-Ready là où l'ancien la rapportait saine. Lisez ceci avant d'en
conclure que la mise à niveau a cassé quelque chose.

La `0.6.4` fait en sorte que la passe d'escrow de la DEK wrappée **bloque le provisioning du
repository dans tout état qu'elle ne peut pas positivement prouver sûr**. Deux états sont sûrs :
une DEK in-cluster existe déjà, donc rien ne peut être frappé ; ou il n'y a prouvablement aucune
DEK nulle part, donc frapper une DEK est ce qui doit arriver. Tout le reste bloque désormais et
pose `Ready=False` avec la raison `DEKEscrowUnresolved`, la phase `Degraded`, et la condition
`DEKEscrowed` qui porte le cas exact :

```bash
kubectl get clusterbackuplocations
kubectl get clusterbackuplocation <name> \
  -o jsonpath='{range .status.conditions[?(@.type=="DEKEscrowed")]}{.reason}: {.message}{"\n"}{end}'
```

Si l'une passe en `Degraded` à la première réconciliation après la mise à niveau, **l'état
qu'elle nomme était déjà vrai en `0.6.3`** — il ne bloquait simplement rien, ce qui est
précisément le défaut que cette release corrige. Trois cas méritent d'être connus :

- **`EscrowConflict`** — l'objet du bucket et la DEK in-cluster sont tous deux lisibles et sont
  des clés différentes. Ce sont deux générations de repository, et la copie du bucket peut être
  la seule clé de la plus ancienne. En `0.6.3`, une telle location rapportait `Ready` tout en
  donnant une mauvaise clé à chaque mover. Ne supprimez rien : l'objet du bucket est une preuve.
- **`EscrowUnreachable`** — le bucket n'a pas pu être lu et il n'y a pas de DEK locale, donc une
  clé récupérable peut s'y trouver. En général des credentials ou un endpoint, et cela se résorbe
  tout seul dès que le bucket redevient joignable. À distinguer de **`EscrowUnverifiable`**, qui
  est la même panne d'E/S *avec* une DEK locale présente et qui, elle, ne bloque **pas**, puisque
  il n'y a rien à frapper.
- **`CredentialsUnavailable`** / **`KEKUnavailable`** — le Secret manque. Restaurez-le ; la
  location se rétablit sans intervention.

`EscrowWriteFailed` ne bloque toujours pas : la DEK in-cluster est connue bonne et seule la copie
du bucket est en retard, ce qui dégrade la DR à froid plutôt que vos backups.

## 0.6.2 → 0.6.3 sous Argo CD : un objet cesse d'être rendu

Lisez ceci avant de synchroniser, pas après. C'est arrivé sur un vrai cluster.

En `0.6.2`, le chart rendait un objet `Namespace` par défaut (`namespace.create` valait `true`
par défaut). En `0.6.3`, ce défaut est `false`, et l'objet a simplement disparu du rendu. Sous
Argo CD avec le prune automatique, **un objet qui cesse d'être rendu est un objet qui est
supprimé** — c'est ce que veut dire le prune, et il ne distingue pas « l'auteur a retiré ceci »
de « l'auteur a changé le défaut ». Une synchronisation `0.6.2` → `0.6.3` peut donc supprimer
`crystal-backup-system` et tout ce qu'il contient, y compris le Secret qui porte votre cluster
KEK et toutes les DEK wrappées. Rien n'est touché dans le stockage objet, et chaque repository
que ces clés protègent devient définitivement illisible — un
[decommission](https://github.com/CrystalBackup/CrystalBackup/blob/main/docs/DECOMMISSION.md#14-the-key-itself)
exécuté par accident, pendant une montée de patch.

Le remède consiste à sortir le namespace de l'ensemble prunable de l'Application **avant** la
mise à niveau : arrêtez de le suivre dans l'`Application` de l'operator — une Application à lui
seul, avec le prune désactivé, ou une exclusion du périmètre de synchronisation. Dès lors que le
namespace n'est plus quelque chose que cette Application rend, aucun changement de
`namespace.create` ne peut l'atteindre. Ensuite, mettez à niveau. Le raisonnement, et la forme à
employer, sont dans
[Installer avec Argo CD](/CrystalBackup/fr/docs/start/install-argocd/#le-namespace--le-vôtre-pas-celui-du-chart).

Le même danger vaut pour un `HelmRelease` Flux avec le pruning activé, et pour tout autre
réconciliateur qui traite « n'est plus rendu » comme « à supprimer ». Après la mise à niveau,
`namespace.create` devrait rester à `false` définitivement : un namespace que Helm possède est un
namespace qu'un prune ou un `helm uninstall` peut emporter, avec les clés dedans.

## Avant de mettre à niveau

**1 — Laissez le travail en cours se terminer.** Une mise à niveau redémarre l'operator, ce
qui est sûr par conception — les Jobs de mover ont des noms déterministes et sont ré-adoptés
plutôt que relancés — mais il n'y a aucune raison de le faire pendant une fenêtre de prune.

```bash
kubectl -n crystal-backup-system get jobs -l app.kubernetes.io/managed-by=crystal-backup
kubectl get clusterbackups
kubectl get restores,clusterrestores -A
```

**2 — Sachez où sont vos clés.** Une mise à niveau n'y touche pas, mais le moment où vous
découvrez que votre séquestre de KEK est périmé ne devrait pas être celui où vous en avez
besoin.

**3 — Lisez les notes de release.** Particulièrement pour une montée de mineure.

## Pendant

```bash
kubectl -n crystal-backup-system rollout status deploy/crystal-backup
```

L'operator redémarre. Si vous faites tourner plus d'un replica, l'élection de leader en
maintient exactement un actif et les autres en standby chaud, si bien que le redémarrage est
un passage de témoin plutôt qu'une interruption de service.

Rien dans le stockage objet n'est touché par une mise à niveau. Les repositories ne sont pas
migrés, les clés ne sont pas tournées, et aucune donnée ne bouge.

## Après

```bash
# The operator is up on the new version.
kubectl -n crystal-backup-system get deploy crystal-backup \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'

# Locations are still Ready.
kubectl get clusterbackuplocations
kubectl get backuplocations -A

# Repositories are still reachable.
kubectl get backuprepositories
```

Puis laissez une backup planifiée s'exécuter et vérifiez qu'elle s'est terminée. Une mise à
niveau que vous n'avez pas vérifiée avec une vraie backup est une mise à niveau que vous
n'avez pas vérifiée.

## Redescendre de version

Non supporté, et cela mérite d'être dit sans détour. Les nouveaux champs de CRD écrits par un
operator plus récent sont inconnus d'un plus ancien ; l'API server les élague à la prochaine
écriture, et l'operator plus ancien réconcilie contre un spec tronqué.

S'il vous faut vraiment revenir en arrière : désinstallez, ré-appliquez les anciennes CRD,
réinstallez l'ancien chart, et recréez vos custom resources. Les **repositories ne sont pas
affectés** — c'est tout l'intérêt du fait que le repository fasse foi. La discovery
reprojettera les backups.

Suivez la [procédure de désinstallation](/CrystalBackup/fr/docs/start/install/#désinstaller)
ordonnée pour cette première étape. Retirer l'operator alors qu'un `Backup`, un `Restore` ou
une location porte encore son finalizer laisse le namespace en `Terminating` de façon
permanente, et une descente de version est exactement le moment où vous supprimerez des
objets dans la précipitation.

## Franchir plusieurs mineures

Avancez d'une mineure à la fois (`0.3` → `0.4` → `0.5`), en appliquant les CRD de chaque
release et en laissant un cycle de backup s'achever entre deux. Sauter des mineures sur une
API alpha, c'est la manière de découvrir de quelle migration vous aviez besoin.

## Les images

En production, les images sont référencées **par digest**, jamais par tag. Les values
publiées du chart portent les vrais digests de la release ; un chart installé depuis un
checkout des sources porte un placeholder et ne pullera pas.

```bash
kubectl -n crystal-backup-system get pods \
  -o jsonpath='{range .items[*]}{.spec.containers[0].image}{"\n"}{end}'
```

Les images du mover et du sync sont épinglées séparément et sont passées à chaque Job de
mover. Elles avancent avec l'operator, si bien qu'une mise à niveau partielle — nouvel
operator, ancien digest de mover — est à éviter plutôt qu'à tenter.
