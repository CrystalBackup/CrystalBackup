---
title: Règles d'admission
description: Les validating admission policies que le chart installe, ce que chacune refuse, et pourquoi l'admission est un gate plutôt que la frontière d'isolation.
sourceFile: src/content/docs/reference/admission.md
sourceHash: e8be3727fd6efb6c5daf47095e1f5d8aff9b88e4
---

Les validations statiques bloquantes sont livrées sous forme d'objets
`ValidatingAdmissionPolicy` — du CEL évalué à l'intérieur de l'API server — si bien qu'elles
tiennent **même quand l'operator est à terre**. Le webhook est réservé à la seule
vérification réellement dynamique et tourne en `failurePolicy: Ignore`.

Les numéros de règle sont stables ; d'autres documents les citent.

:::note[L'admission est un gate, pas la frontière]
Les contrôleurs re-dérivent l'identité du repository, le filtre `namespace=` et la valeur de
confirmation au moment de l'exécution. Une policy contournée dégrade l'expérience
utilisateur — vous obtenez un échec confus plus tard au lieu d'un refus clair maintenant —
elle ne perce pas la tenancy. La frontière de tenant est structurelle ; voir
[Tenancy et isolation](/CrystalBackup/fr/docs/understand/tenancy/).
:::

## Les règles

| # | Appliquée par | Ce qu'elle refuse |
|---|---|---|
| 1 | VAP | **Confirmation destructive.** Chaque `Restore`/`ClusterRestore` en `Recreate` ou `Overwrite`, et chaque `ClusterErasure`, a besoin d'un `spec.confirmation` égal à la cible. |
| 2 | VAP | **Isolation des utilisateurs.** Un `Backup`/`BackupSchedule` créé par un utilisateur doit référencer une `BackupLocation` namespacée, jamais une `ClusterBackupLocation`. La source *et* la destination d'un `BackupExternalSync` sont toutes deux des `BackupLocation`s du même namespace. |
| 3 | contrôleur | **Placement de la rétention.** Consultatif, pas un refus — les `keep*` sur une location `Immutable` sont signalés ignorés via une condition `RetentionIgnored`. |
| 4 | webhook | **`ClusterBackupLocation` par défaut unique.** L'unicité entre objets n'est pas exprimable en CEL par objet. |
| 5 | VAP | **Références de Secret dans le même namespace.** `credentialsSecretRef` et `repositoryPasswordSecretRef` sur une `BackupLocation` sont par nom seul, résolues dans ce namespace. |
| 6 | VAP | **`Immutable` interdit le prune.** `mode: Immutable` ne peut pas poser `maintenance.pruneSchedule`. |
| 7 | VAP | **Namespaces refusés.** Les ressources destinées aux tenants sont rejetées dans une deny-list configurable. |
| 8 | VAP | **Forme du sélecteur de namespaces.** `namespaces` doit poser exactement une forme positive non vide, plus un `exclude` optionnel. |
| 9 | VAP | **Distinction de la sync externe.** `sourceLocationRef.name != destinationLocationRef.name`, sur les deux kinds de sync. |

Seules les règles 1, 2, 5, 6, 7, 8 et 9 produisent un rejet à l'admission. La règle 3 est une
condition de status et la règle 4 est le webhook.

## Règle 1 — la confirmation, et son asymétrie délibérée

La policy est un **sur-ensemble conservateur**. CEL ne peut pas demander si le namespace
cible existe déjà, la confirmation est donc requise inconditionnellement dans les deux modes
— et comme `Recreate` et `Overwrite` sont les deux seuls modes, en pratique tout restore en
a besoin.

La policy admet une valeur **vide ou absente** et ne refuse qu'une valeur non vide qui ne
correspond pas. C'est pour cela que `spec.confirmation` est `+optional` dans le schéma
plutôt que requis : un champ requis avec `MinLength=1` serait rejeté par le schéma structurel
de l'API server *avant* que la policy ne tourne, rendant la phase `AwaitingConfirmation`
inatteignable.

Donc :

- **mauvaise valeur** → refusée à l'admission, l'objet n'est jamais créé ;
- **valeur absente** → admise, et le contrôleur gare l'objet en `AwaitingConfirmation`
  jusqu'à ce que vous l'éditiez pour l'y mettre.

Le contrôleur vérifie la même égalité indépendamment, avant de résoudre la source.

## Règle 2 — et pourquoi l'operator en est exempté

Le binding porte une clause `matchConditions` qui exclut le ServiceAccount de l'operator
lui-même. Sans elle, le fan-out de DR cluster de l'operator — qui crée légitimement des
objets `Backup` dans les namespaces des tenants en référençant une `ClusterBackupLocation` —
serait refusé par sa propre policy.

Les règles 7 et 8 portent la même exemption, pour la même raison.

Notez ce que la règle 2 ne fait **pas** : le fait que les objets `Backup` d'origine cluster
soient en lecture seule pour les utilisateurs relève du **RBAC**, pas de l'admission. Le
ClusterRole `crystal-backup-tenant` livré n'accorde que `get`, `list` et `watch` sur
`backups`.

## Règle 7 — la deny-list

Configurée via Helm, rendue dans une ConfigMap liée à la policy par `paramRef` — si bien
qu'elle peut aussi être éditée en cluster après l'installation.

```yaml
admission:
  deniedNamespaces:
    - "kube-*"
    - crystal-backup-system
    - velero
```

Des noms simples ou des préfixes suffixés par `*`. Ajoutez le namespace de tout outil de
backup déjà en place : c'est une des garanties de coexistence, et cela ne coûte rien.

## Règle 8 — la forme du sélecteur

```yaml
# Valid.
namespaces:
  matchLabels: { crystalbackup.io/protect: "true" }
  exclude: ["kube-*"]

# Denied — no positive form.
namespaces:
  exclude: ["kube-*"]

# Denied — an empty map counts as unset.
namespaces:
  matchLabels: {}
```

Les formes positives sont `matchNames`, `matchLabels`, `matchExpressions` et `regexp`.
Exactement une doit être non vide. Le moteur revalide à l'exécution.

## Validation au niveau des CRDs

Non numérotées, mais elles produisent aussi des rejets. Ce sont des expressions CEL sur les
CRDs elles-mêmes.

**Immuabilité après création**

| Objet | Champs immuables |
|---|---|
| `Restore` | `spec.source`, `spec.mode` |
| `ClusterRestore` | `spec.source`, `spec.mode`, `spec.target.namespace` |
| `ClusterBackupLocation` | `spec.mode`, `spec.clusterID`, `spec.s3.endpoint`, `spec.s3.bucket`, `spec.s3.prefix` |
| `BackupLocation` | les cinq mêmes |

L'identité d'une location est immuable parce que ces champs composent le chemin du
repository : une édition re-pointerait silencieusement la location vers un *autre*
repository, orphelinant chaque backup pris jusque-là, sans qu'aucune donnée ne bouge et sans
aucune erreur. L'identité d'un restore est immuable parce que le contrôleur la re-dérive à
chaque passe, si bien qu'une édition en cours de route mélangerait deux instants dans un
même restore.

`confirmation` et les listes de sélection restent mutables — c'est ainsi qu'on dégare un
restore, et une édition s'applique aux volumes pas encore démarrés.

**Forme de la source**

- `exactly one of source.backup and source.time must be set`
- `source.origin is only valid together with source.time` (sur `Restore`)
- `time must be "latest" or an RFC3339 timestamp`

**Bornes et chemins**

- `resources` et `volumes` : au plus 128 éléments chacun
- `targetPath` : au plus 256 caractères, et `targetPath must not contain '..' segments`
- `source.backup` : au plus 253 caractères ; `source.time` : au plus 64

**Grammaires de valeurs**

- `pruneMaxRepackSize` : `^[0-9]+(\.[0-9]+)?[kKmMgGtT]?$`
- `checkReadDataSubset` : `^([0-9]+/[0-9]+|[0-9]+(\.[0-9]+)?%|[0-9]+(\.[0-9]+)?[kKmMgGtT]?)$`

Ces deux-là reprennent les grammaires propres à restic, épinglées ici pour qu'une faute de
frappe soit rejetée au moment de l'apply plutôt que de devenir un Job de maintenance qui
démarre, pulle une image, ouvre le repository et ne meurt qu'ensuite sur une erreur de
parsing de flag.

## Le désactiver

```yaml
admission:
  vap:
    enabled: true      # requires Kubernetes >= 1.30
  webhook:
    enabled: true
```

Désactiver le jeu de VAP signifie que chaque vérification ci-dessus devient un échec côté
contrôleur au lieu d'un rejet par l'API server — une moins bonne expérience, et pas un trou
de tenancy.

Désactiver le webhook laisse la condition `MultipleDefaults` du contrôleur comme seul
garde-fou contre une seconde `ClusterBackupLocation` par défaut.

## Voir ce qui est installé

```bash
kubectl get validatingadmissionpolicy | grep crystalbackup
kubectl get validatingadmissionpolicybinding | grep crystalbackup
kubectl -n crystal-backup-system get configmap | grep denied
```
