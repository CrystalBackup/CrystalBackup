---
title: Les hooks de cohérence
description: Quiescer une application autour du snapshot, et le ServiceAccount que l'operator impersonate pour le faire.
sidebar:
  order: 8
sourceFile: src/content/docs/guides/hooks.md
sourceHash: 0064b0999b62fec19fd3330b892e74e3ffe60d1e
---

Les snapshots sont **crash-consistent** par défaut : le même état que votre application
verrait après une coupure de courant. La plupart des choses y survivent. Certaines non, et
pour celles-là un hook vous permet de quiescer avant le snapshot et de relâcher après.

## Les deux règles qui structurent tout

**La fenêtre de gel est la phase de snapshot, pas l'upload.** Les post hooks s'exécutent dès
que chaque snapshot est *pris* — pas quand l'upload a réussi. Une base de données maintenue
gelée pendant un upload de plusieurs heures est une panne, pas une backup.

**Les post hooks s'exécutent toujours, et sont retentés. Les pre hooks non.** L'asymétrie est
délibérée : un pre hook en échec signifie que le snapshot ne doit pas être pris, tandis qu'un
post hook en échec signifie qu'une application peut être restée **quiescée**. Le retry est la
différence entre un raté passager et un incident.

## Déclarer des hooks

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: BackupSchedule
metadata:
  name: nightly
  namespace: team-x
spec:
  locationRef: { name: my-offsite }
  schedule: "0 1 * * *"
  hooks:
    serviceAccountName: crystal-backup-hooks
    pre:
      - podSelector:
          matchLabels:
            app: postgres
        container: postgres
        command: ["psql", "-c", "CHECKPOINT"]
        timeout: 30s
        onError: Fail
    post:
      - podSelector:
          matchLabels:
            app: postgres
        container: postgres
        command: ["sh", "-c", "echo released"]
        timeout: 30s
        onError: Continue
```

| Champ | Signification |
|---|---|
| `podSelector` | Quels pods. Les candidats sont déjà restreints aux **pods en cours d'exécution, dans le namespace sauvegardé, qui montent l'une des PVC que cette exécution capture** ; un sélecteur vide les désigne tous. |
| `container` | Quel container. Vide utilise le **premier** container du pod. |
| `command` | Un argv, exécuté directement — **pas** via un shell. Pour des pipes ou des redirections, utilisez `["sh", "-c", "..."]`. |
| `timeout` | Borne la durée pendant laquelle l'application reste quiescée. Vaut `30s` par défaut. Un hook qui déborde est **mis en échec**, pas attendu. |
| `onError` | `Fail` (le défaut) avorte la backup. `Continue` enregistre l'échec et poursuit. |

La candidature par *PVC montée* plutôt que par label seul est ce qui confine l'exec aux
workloads dont les données sont effectivement capturées.

`onError: Fail` est le défaut parce qu'un hook pré-snapshot existe précisément pour rendre le
snapshot digne de confiance. Si la quiescence n'a pas eu lieu, un snapshot pris quand même
est une backup qui *paraît* cohérente au niveau applicatif sans l'être.

## L'identité — obligatoire sur le plan namespace

Sur le plan namespace, l'operator ne fait **pas** l'exec en son propre nom. Il
**impersonate** un ServiceAccount que vous nommez, dans le namespace en cours de sauvegarde.
L'API server autorise alors chaque exec au regard de cette identité.

La conséquence est tout l'objet du dispositif : les utilisateurs ne peuvent faire exécuter à
la plateforme que des commandes qu'ils pourraient déjà exécuter eux-mêmes.

Créez-le une fois par namespace :

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: crystal-backup-hooks
  namespace: team-x
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: crystal-backup-hooks
  namespace: team-x
rules:
  - apiGroups: [""]
    resources: ["pods/exec"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: crystal-backup-hooks
  namespace: team-x
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: crystal-backup-hooks
subjects:
  - kind: ServiceAccount
    name: crystal-backup-hooks
    namespace: team-x
```

Le nom vous appartient ; `crystal-backup-hooks` n'est qu'une suggestion.

Pour le restreindre davantage :

```yaml
rules:
  - apiGroups: [""]
    resources: ["pods/exec"]
    verbs: ["create"]
    resourceNames: ["postgres-0", "postgres-1"]
```

`resourceNames` s'applique aux **noms de pods**, ce qui convient bien mieux aux StatefulSets
(noms stables) qu'aux Deployments (suffixes générés).

Trois choses en découlent, et ce sont elles qui justifient ce fonctionnement :

- **C'est vous qui décidez de ce que la plateforme peut faire.** Pas d'habilitation, pas
  d'exec.
- **La révocation est immédiate.** Supprimez le RoleBinding et le hook suivant échoue. La
  vérification a lieu à chaque exec ; rien n'est mis en cache.
- **Le namespace n'est jamais le vôtre à choisir.** Seul le *nom* du ServiceAccount est un
  champ. Le namespace est toujours celui en cours de sauvegarde, dérivé du pod que le hook
  vise — un namespace paramétrable serait un trou entre tenants par construction.

### Si vous l'oubliez

La backup est **bloquée en amont**, et non silencieusement escaladée vers les privilèges
propres de l'operator :

```
Conditions:
  Type    Status  Reason                    Message
  Ready   False   HooksNeedServiceAccount   hooks on a namespaced BackupLocation must set
                                            hooks.serviceAccountName — a ServiceAccount in this
                                            namespace, granted `create pods/exec`, that the
                                            operator impersonates to run them
```

Si le ServiceAccount existe mais n'a pas l'habilitation, le hook échoue et le message nomme
l'identité : `system:serviceaccount:team-x:crystal-backup-hooks`.

### Sur le plan cluster

Les hooks d'un `ClusterBackupSchedule` sont écrits par un administrateur, si bien que
`serviceAccountName` peut être omis — ils s'exécutent alors en tant que l'operator lui-même.
Le renseigner fonctionne à l'identique et vaut la peine partout où c'est possible.

## Les annotations de pod

Si vous préférez laisser les propriétaires des pods déclarer leurs propres hooks, activez-le
explicitement :

```yaml
hooks:
  serviceAccountName: crystal-backup-hooks
  honorAnnotations: true
```

Puis, sur le pod :

```yaml
metadata:
  annotations:
    crystalbackup.io/pre-backup-command: '["psql","-c","CHECKPOINT"]'
    crystalbackup.io/pre-backup-container: postgres
    crystalbackup.io/pre-backup-timeout: 30s
    crystalbackup.io/pre-backup-on-error: Fail
    crystalbackup.io/post-backup-command: '["sh","-c","echo released"]'
    crystalbackup.io/post-backup-container: postgres
    crystalbackup.io/post-backup-timeout: 30s
    crystalbackup.io/post-backup-on-error: Continue
```

Quatre suffixes, sur les deux préfixes : `-command`, `-container`, `-timeout`, `-on-error`.
`-command` est un argv JSON.

Les deux phases existent pour la raison évidente : un `FLUSH TABLES WITH READ LOCK` a besoin
de son `UNLOCK TABLES`, et quiconque écrit le premier doit pouvoir écrire le second au même
endroit.

Deux règles :

- **Les annotations remplacent, elles ne fusionnent pas.** Quand un pod en porte, elles
  priment et les hooks du schedule sont ignorés **pour ce pod**.
- **L'annotation fournit la commande, jamais l'identité.** Les hooks s'exécutent toujours en
  tant que `hooks.serviceAccountName`, avec exactement les droits que le namespace lui a
  accordés.

`honorAnnotations` est opt-in et vaut `false` par défaut, parce qu'il délègue *ce que
l'operator exécute* à quiconque peut annoter un pod dans le namespace sauvegardé.

## Lire ce qui s'est passé

```bash
kubectl -n team-x get backup nightly-20260730-010000 \
  -o jsonpath='{range .status.hooks[*]}{.phase}{"\t"}{.pod}{"\t"}{.container}{"\t"}{.source}{"\t"}{.result}{"\t"}{.message}{"\n"}{end}'
```

```
pre	postgres-0	postgres	spec	Succeeded	
post	postgres-0	postgres	spec	Succeeded	
```

`status.hooks` est le compte rendu durable de la fenêtre de gel : quels pods ont été
quiescés, si le relâchement a bien eu lieu, et — quand ce n'est pas le cas — ce que vous avez
à défaire à la main.

Les résultats sont `Succeeded`, `Failed` et `Skipped`. `Skipped` signifie qu'un hook
antérieur de la même phase a échoué avec `onError: Fail`, si bien que celui-ci ne s'est
jamais exécuté. Il est enregistré plutôt qu'omis, et c'est délibéré : une liste montrant
trois hooks sur cinq invite le lecteur à supposer que les deux manquants sont passés.

`source` vaut `spec` ou `annotation` — c'est ainsi que vous savez si la commande qui s'est
exécutée est bien celle que vous avez écrite.

`status.postHookAttempts` compte les retries de relâchement. Une valeur non nulle qui cesse
de grimper signifie qu'un relâchement a fini par réussir. Une valeur qui continue de grimper
signifie qu'**une application peut être restée quiescée**, et c'est le cas où il faut aller
regarder tout de suite.

## Écrire des hooks qui valent la peine

- **Faites un checkpoint, pas un dump.** Un hook qui lance `pg_dump` place le dump à
  l'intérieur du volume que vous êtes sur le point de snapshotter, doublant les données et le
  temps. Flushez et laissez le snapshot faire son travail.
- **Gardez-les sous le timeout.** Le timeout borne le gel, et le gel est de l'indisponibilité.
- **Rendez-les idempotents.** Les post hooks sont retentés.
- **N'utilisez `onError: Continue` que lorsque l'absence du hook dégrade la cohérence sans
  invalider la backup.** Si la backup ne vaut rien sans lui, `Fail` est le réglage honnête.
- **Une base de données dotée de son propre operator de backup n'en a probablement pas
  besoin.** L'archivage de WAL bat un snapshot de volume pour la restauration à un instant
  donné. Utilisez les deux, à des fins différentes.
