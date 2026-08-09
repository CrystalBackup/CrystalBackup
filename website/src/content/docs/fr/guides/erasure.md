---
title: Le droit à l'effacement
description: ClusterErasure — supprimer physiquement un tenant, un namespace ou une seule PVC d'une location.
sidebar:
  order: 6
sourceFile: src/content/docs/guides/erasure.md
sourceHash: c27db3a6e828a23e8ec185c9cfe1553359999917
---

`ClusterErasure` supprime physiquement des données d'un repository : un `restic forget`
filtré par tag, puis un `prune`. C'est une opération d'administrateur, cluster-scoped, et
elle est irréversible.

## Ce que ce n'est pas

**Ce n'est pas du crypto-shredding.** Le repository cluster partagé a une seule master key.
Détruire cette clé détruirait les données de tous les tenants, la destruction de clé par
tenant est donc impossible ici — elle a été abandonnée dès la conception plutôt que
reportée. Si votre régime de conformité exige une destruction cryptographique par tenant, le
plan cluster partagé n'est pas le mécanisme ; donnez plutôt à chaque tenant une location du
plan namespace avec une clé à lui.

**Ce n'est pas le démantèlement d'un repository.** Retirer du service un repository entier en
détruisant sa clé est un runbook, pas une custom resource. `ClusterErasure` est une CRD
précisément parce qu'elle est *bornée* — un état désiré vers lequel un contrôleur peut
converger, sur un périmètre que vous pouvez nommer.

## Effacer

Exactement un périmètre par objet :

```yaml
# Everything tagged tenant=acme.
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterErasure
metadata:
  name: erase-acme
spec:
  locationRef:
    name: dr-primary
  target:
    tenant: acme
  confirmation: acme
```

```yaml
# Everything tagged namespace=team-x.
spec:
  locationRef: { name: dr-primary }
  target:
    namespace: team-x
  confirmation: team-x
```

```yaml
# One PVC inside one namespace.
spec:
  locationRef: { name: dr-primary }
  target:
    namespace: team-x
    pvc: uploads
  confirmation: team-x/uploads
```

`confirmation` doit être égal à l'identité de la cible : le nom du tenant, le nom du
namespace, ou `<namespace>/<pvc>`.

Le tenant auquel un namespace se rattache est son label `crystalbackup.io/tenant` s'il en a
un, et sinon le nom du namespace. Donc `target.tenant` efface en une seule opération tous les
namespaces appartenant à ce tenant — vérifiez ce que cela couvre avant de le lancer :

```bash
kubectl get ns -l crystalbackup.io/tenant=acme
```

## Le double geste

Comme pour un restore, `confirmation` est facultatif dans le schéma, afin que son omission
**gare** l'objet plutôt que de le rejeter :

```bash
kubectl apply -f erase-acme.yaml     # without confirmation
kubectl get clustererasure erase-acme
```

```
NAME         PHASE                  TARGETED   FORGOTTEN   REMAINING   AGE
erase-acme   AwaitingConfirmation   0          0           0           5s
```

Lisez ce qu'il s'apprête à faire. Puis tapez l'identité :

```bash
kubectl patch clustererasure erase-acme --type=merge -p '{"spec":{"confirmation":"acme"}}'
```

Une **mauvaise** valeur est rejetée à l'admission et l'objet n'est jamais créé. Une valeur
**absente** met en attente. Cette asymétrie est délibérée : le chemin de la faute de frappe
échoue vite, le chemin délibéré vous laisse un instant pour regarder.

## La suivre

```bash
kubectl get clustererasure erase-acme -w
```

```
NAME         PHASE       TARGETED   FORGOTTEN   REMAINING   AGE
erase-acme   Running     412        0           412         8s
erase-acme   Completed   412        412         0           6m22s
```

```bash
kubectl get clustererasure erase-acme \
  -o jsonpath='{.status.snapshotsForgotten}{" snapshots, "}{.status.reclaimedBytes}{" bytes reclaimed\n"}'
```

Phases : `Pending`, `AwaitingConfirmation`, `Running`, `Completed`, `Blocked`, `Failed`.

`snapshotsForgotten` est une **attestation, pas une intention**. Ce champ ne compte que ce dont
la suppression est établie : il vaut donc `0` pendant toute la durée de l'effacement —
`snapshotsTargeted` étant le périmètre en cours de traitement. Si l'effacement **échoue**,
l'opérateur relit le dépôt sous le même filtre et publie ce qu'il y trouve : un effacement ayant
retiré 4 snapshots sur 10 avant d'échouer affiche `snapshotsForgotten: 4` et
`snapshotsRemaining: 6`, et un effacement dont le résidu n'a même pas pu être listé affiche `0`
oublié et le périmètre entier restant. Un enregistrement qui sur-déclare une destruction clôt une
conversation de conformité qui devait continuer : ce champ ne suppose jamais à la hausse.

## `Blocked`

Sur une location `Immutable`, l'effacement ne peut pas avancer tant qu'Object Lock n'a pas
expiré. La phase est `Blocked` et `status.blockedUntil` porte la date.

Rien n'est perdu — l'objet demeure et converge à l'expiration du verrou. Mais il n'y a aucun
moyen de le forcer, et c'est précisément ce que signifie Object Lock.

(Le support d'Object Lock n'est pas implémenté dans cette release ; voir
[Quand ne pas le choisir](/CrystalBackup/fr/docs/discover/when-not-to-use/).)

## Combien de temps cela prend, et ce que cela bloque

`prune` est la moitié coûteuse. Sur le repository cluster partagé, il prend la **fenêtre
exclusive à l'échelle du cluster** — aucun namespace ne peut démarrer une backup pendant son
exécution, et son usage mémoire croît avec la taille totale du repository plutôt qu'avec ce
que vous effacez.

Effacer une petite PVC paie quand même un prune complet. Regroupez les effacements quand vous
le pouvez, et lancez-les dans la même fenêtre creuse que la maintenance planifiée.

Les backups n'échouent pas pendant la fenêtre ; elles **attendent**. Un `Backup` reste
simplement en `Pending` jusqu'à ce qu'un créneau se libère, silencieusement et en se
résolvant tout seul.

## Ce qui survit

Les projections `Backup` des snapshots effacés disparaissent à la prochaine passe de
discovery — la durée de vie de la projection suit celle des données, si bien que
`kubectl get backups` continue de dire la vérité.

Ce qui ne disparaît **pas** : les copies que vous avez faites ailleurs. Une destination
d'external sync détient ses propres snapshots sous sa propre clé, et effacer la source n'y
touche pas. Si l'effacement est une obligation de conformité, énumérez d'abord toutes les
destinations :

```bash
kubectl get clusterbackupexternalsync,backupexternalsync -A \
  -o jsonpath='{range .items[*]}{.kind}{" "}{.metadata.namespace}/{.metadata.name}{" src="}{.spec.sourceLocationRef.name}{" dst="}{.spec.destinationLocationRef.name}{"\n"}{end}'
```

et effacez à chacune d'elles également.

## Effacer sur le plan namespace

Il n'existe pas de ressource d'effacement namespacée. Le repository d'un tenant est le sien :
il supprime les snapshots avec restic en amont, avec sa propre clé et ses propres
credentials.

```bash
restic -r s3:https://s3.other.example/team-x-backups/crystal/prod-eu-1 \
  forget --tag namespace=team-x --prune
```

Ce n'est pas une lacune de l'API. C'est la même propriété que partout ailleurs sur ce plan —
la plateforme n'a pas la clé, donc la plateforme ne peut pas le faire à sa place.
