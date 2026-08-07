---
title: Installer avec Argo CD
description: Gérer Crystal Backup depuis Git avec Argo CD — ce qui va dans Git, ce qui ne doit jamais y aller, et le prune qui détruit vos clés.
sourceFile: src/content/docs/start/install-argocd.md
sourceHash: ef44b1ce4e9ece656baf97ed2869112793e69103
---

C'est l'[install Helm](/CrystalBackup/fr/docs/start/install/) pilotée depuis Git. Le chart
est le même, les values sont les mêmes, et tout ce que cette page dit de la KEK, du RBAC et
de la désinstallation ordonnée s'applique toujours.

Ce qui change, c'est qu'un contrôleur applique désormais, ré-applique et — si vous le
laissez faire — supprime ces objets de son propre chef. Trois propriétés de Crystal Backup
interagissent mal avec cela, et elles sont la raison d'être de cette page plutôt qu'un
« pointez une `Application` sur le chart » en une ligne.

## À lire d'abord

**1 — Un run n'est pas un état désiré.** `ClusterBackup`, `Backup`, `Restore`,
`ClusterRestore` et `ClusterErasure` sont des *exécutions*, plus proches d'un `Job` que d'un
`Deployment`. En mettre un dans Git conduit un contrôleur à le recréer, et un run recréé est
un autre run portant le même nom. Voir [Ce qui va dans Git](#ce-qui-va-dans-git).

**2 — Un prune peut détruire vos clés.** Le namespace `crystal-backup-system` contient la
cluster KEK et chaque DEK wrappée. Tout prune qui le retire retire les clés, et chaque
repository qu'elles protègent devient définitivement illisible — c'est un
[decommission](https://github.com/CrystalBackup/CrystalBackup/blob/main/docs/DECOMMISSION.md#14-the-key-itself),
exécuté par accident. Le chart ne rend plus ce Namespace (`namespace.create` vaut `false` par
défaut), ce qui le sort entièrement de l'ensemble prunable de l'`Application` de l'operator ;
mettez-le dans sa propre Application, et gardez-y le prune coupé aussi. Voir
[Le namespace](#le-namespace--le-vôtre-pas-celui-du-chart) et
[Pourquoi le prune est désactivé](#pourquoi-le-prune-est-désactivé-et-pourquoi-il-ny-a-pas-de-finalizer).

**3 — Retirer l'operator est une opération ordonnée, et Argo CD ignore l'ordre.** Six kinds
portent un finalizer que seul l'operator retire. Supprimez l'operator pendant qu'un de ces
objets est vivant et son namespace reste en `Terminating` pour toujours. Voir
[Le retirer](#le-retirer).

Rien de tout cela ne fait de Crystal Backup un mauvais candidat au GitOps. Les déclarations
— locations, schedules, syncs — ont leur place dans Git et y gagnent. Ce sont les exécutions
et les suppressions qui n'y ont pas leur place.

## Ce qui va dans Git

| Kind | Dans Git ? | Pourquoi |
|---|---|---|
| `ClusterBackupLocation`, `BackupLocation` | **oui** | état désiré : où vont les backups |
| `ClusterBackupSchedule`, `BackupSchedule` | **oui** | état désiré : quand ils ont lieu |
| `ClusterBackupExternalSync`, `BackupExternalSync` | **oui** | état désiré : ce qui se réplique où |
| `ClusterBackup`, `Backup` | **non** | un run ; `Backup` est aussi une projection que l'operator possède |
| `Restore`, `ClusterRestore` | **non** | une récupération one-shot, avec une confirmation à taper |
| `ClusterErasure` | **non** | une suppression irréversible, avec une confirmation à taper |
| `BackupRepository` | **non** | interne à l'operator |

### Pourquoi un `ClusterBackup` dans Git est un piège

Un `ClusterBackup` déploie en éventail un `Backup` enfant, **nommé d'après le run**, dans
chaque namespace matché. Le nom du run est donc une coordonnée dans le repository restic,
pas une identité — et plusieurs producteurs différents peuvent atterrir sur la même
coordonnée : une projection de discovery, un run antérieur portant le même nom, ou le
`BackupSchedule` du plan namespace déclenché sur le même cron.

Le prune-and-recreate d'Argo CD fait exactement ce qui casse cela. Il supprime le
`ClusterBackup` et crée un nouvel objet avec le même nom et un **nouvel UID**, tandis que
les objets `Backup` enfants du run précédent — délibérément non possédés par le run, parce
que ce sont des points de restauration — restent où ils sont.

Chaque `Backup` créé par un run porte l'annotation `crystalbackup.io/parent-uid`, le
`metadata.uid` de l'objet qui l'a créé. Le second run trouve à sa coordonnée un `Backup`
avec le parent UID de quelqu'un d'autre et met ce namespace en échec avec
`RunNameCollision` : un `FailureRecord`, compté dans `namespacesFailed`, qui amène le run en
`Failed` ou `PartiallyFailed`.

Cet échec est le correctif, pas le problème. Avant qu'il n'existe, le second run *skippait*
le namespace et agrégeait les volumes complétés de l'occupant comme les siens —
`namespacesSucceeded` en hausse, phase `Completed`, sur des snapshots écrits des jours plus
tôt, avec `addedBytes: 0` comme seule différence visible. Un échec bruyant à chaque sync
reste une mauvaise expérience, cela dit. Ne mettez pas de runs dans Git.

### Faire les mêmes choses sans Git

- **Des backups récurrents** — un `ClusterBackupSchedule` dans Git. C'est le chemin normal
  et il estampille des noms de run uniques pour vous (`<schedule>-<YYYYMMDD-HHMMSS>`, UTC).
- **Un run ponctuel** — hors bande, avec un nom qui ne peut pas se répéter :

  ```bash
  kubectl create -f - <<'EOF'
  apiVersion: crystalbackup.io/v1alpha1
  kind: ClusterBackup
  metadata:
    generateName: pre-upgrade-
  spec:
    locationRef:
      name: dr-primary
    namespaces:
      matchLabels:
        crystalbackup.io/protect: "true"
  EOF
  ```

  `generateName` exige `kubectl create` (et non `apply`) et le serveur ajoute un suffixe
  aléatoire, ce qui est précisément la propriété recherchée. Un nom de confort comme
  `pre-upgrade`, réutilisé pour la deuxième mise à niveau, c'est la même collision à la
  main.
- **Un restore** — hors bande, toujours. Voir
  [Restaurer](/CrystalBackup/fr/docs/guides/restore/).

## 1. Enregistrer le repository de charts

Le chart est publié comme artefact OCI sur `ghcr.io/crystalbackup/charts` par la pipeline de
release, et y est signé avec cosign.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: crystalbackup-charts
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: repository
stringData:
  name: crystalbackup-charts
  type: helm
  url: ghcr.io/crystalbackup/charts
  enableOCI: "true"
```

:::note
La façon dont Argo CD traite le schéma `oci://` dans une URL de repository a changé au fil
des releases — certaines versions veulent le `host/path` nu avec `enableOCI: "true"`, les
plus récentes acceptent ou attendent le préfixe `oci://`. Enregistrez-le, puis confirmez
avec `argocd repo list` et `argocd app manifests crystal-backup` que le chart se résout
réellement avant de debugger quoi que ce soit d'autre.
:::

Les packages publiés sont publics, donc aucun credential n'est nécessaire. Ajoutez
`username`/`password` au Secret si votre Argo CD atteint GHCR via un proxy de registry qui
les exige.

:::danger[Ne pointez pas Argo CD sur le répertoire `charts/` du repository Git]
Deux choses de l'arbre source ne sont remplies qu'au moment de la release.
`charts/crystal-backup/crds/` est **git-ignoré** — les CRDs y sont copiées quand le chart est
packagé — si bien qu'un chart rendu depuis un checkout Git n'installe **aucune CRD**. Et
`image.digest`, `mover.image.digest` et `sync.image.digest` portent un placeholder tout à
zéro que la pipeline de release remplace, si bien que le pod de l'operator ne pullerait
jamais. Utilisez toujours le chart OCI publié.
:::

## Le namespace — le vôtre, pas celui du chart

Le chart ne crée pas `crystal-backup-system` (`namespace.create: false`), et sous Argo CD cela
vaut davantage que sur le chemin Helm : un objet que le chart ne rend pas est un objet qu'aucun
prune de l'`Application` de l'operator ne pourra jamais atteindre.

Il doit tout de même exister, et il doit tout de même porter les labels Pod Security Admission
— les data movers y tournent en uid 0 avec `DAC_OVERRIDE` pour préserver la propriété des
fichiers au restore, et `restricted` les refuse. Sur `helm install`, le chart relit le namespace
et refuse un mauvais niveau `enforce` ; Argo CD rend avec `helm template`, qui n'a aucun cluster
à relire, donc **sous Argo CD ce contrôle n'existe pas**. C'est pourquoi les labels sont écrits
ici en toutes lettres plutôt que renvoyés à une autre page.

Mettez-le dans sa propre Application, ou dans celle des Secrets, sourcée depuis votre dépôt
Git — avec le prune coupé :

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: crystal-backup-system
  annotations:
    # Belt and braces on top of `prune: false`: never prune this object, whatever the
    # Application says. It holds the cluster KEK.
    argocd.argoproj.io/sync-options: Prune=false
  labels:
    # `baseline`, not `restricted`: the operator is restricted-compliant, but data movers run
    # as uid 0 with DAC_OVERRIDE in this namespace to preserve file ownership on restore.
    # Getting this wrong costs nothing until the first backup, which then never starts.
    pod-security.kubernetes.io/enforce: baseline
    pod-security.kubernetes.io/enforce-version: latest
    pod-security.kubernetes.io/audit: restricted
    pod-security.kubernetes.io/warn: restricted
```

Donnez-lui `argocd.argoproj.io/sync-wave: "-10"` pour qu'il arrive avant les Secrets (wave 0)
et l'operator (wave 10).

## 2. L'Application de l'operator

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: crystal-backup
  namespace: argocd
  # No `resources-finalizer.argocd.argoproj.io` finalizer, on purpose.
  # See "Removing it" below — deleting this Application must NOT cascade.
  annotations:
    argocd.argoproj.io/sync-wave: "0"
spec:
  project: platform
  destination:
    server: https://kubernetes.default.svc
    namespace: crystal-backup-system
  source:
    repoURL: ghcr.io/crystalbackup/charts
    chart: crystal-backup
    targetRevision: 0.6.2          # the pin. Bumping this IS the upgrade.
    helm:
      # Keep the release name fixed. The chart stamps
      # `app.kubernetes.io/instance: <release name>` on every object, and Argo CD
      # otherwise derives it from the Application name.
      releaseName: crystal-backup
      values: |
        admission:
          deniedNamespaces:
            - "kube-*"
            - crystal-backup-system
        # Observability is opt-in and off means NO ALERT RULES AT ALL — nothing
        # would tell you a backup stopped running. Both need the
        # monitoring.coreos.com CRDs; drop this block if you have no Prometheus
        # Operator, and wire port 8443 into whatever you do have.
        metrics:
          serviceMonitor:
            enabled: true
          rules:
            enabled: true
            labels:
              release: kube-prometheus-stack   # your Prometheus's ruleSelector
        networkPolicy:
          # Empty — the default — lets any pod in the cluster reach the metrics
          # port. There is no default because the name is unguessable.
          monitoringNamespace: monitoring
  syncPolicy:
    automated:
      selfHeal: true
      prune: false                 # deliberate. See below.
    syncOptions:
      - ServerSideApply=true
      - CreateNamespace=false      # the namespace is a separate Application, above
  ignoreDifferences:
    # The chart mints the admission webhook's CA and serving cert at RENDER time
    # (genCA/genSignedCert, no `lookup`). Argo CD re-renders the chart on every
    # refresh, so both change every time. Without these two entries the Application
    # is permanently OutOfSync and self-heal rewrites the pair every cycle.
    - group: ""
      kind: Secret
      name: crystal-backup-webhook-certs
      namespace: crystal-backup-system
      jsonPointers:
        - /data
    - group: admissionregistration.k8s.io
      kind: ValidatingWebhookConfiguration
      name: crystal-backup
      jqPathExpressions:
        - .webhooks[].clientConfig.caBundle
```

Provisionnez ensuite les [Secrets](#3-secrets) et les
[ressources](#4-locations-et-schedules).

### Pourquoi les noms d'objets dans `ignoreDifferences` ne sont pas préfixés par la release

Crystal Backup est un operator cluster singleton, si bien que le helper `fullname` du chart
ne préfixe délibérément **pas** avec le nom de la release : les objets RBAC cluster-scoped
doivent être prévisibles pour le binding et l'agrégation côté plateforme. Le Secret est donc
`crystal-backup-webhook-certs` et la `ValidatingWebhookConfiguration` est `crystal-backup`
quel que soit le nom que vous donnez à la release, sauf si vous posez `fullnameOverride`.

L'alternative à ignorer le diff est `admission.webhook.enabled: false`. C'est une vraie
option — le webhook est fail-open et applique une seule règle (une seconde
`ClusterBackupLocation` par défaut), avec la condition `MultipleDefaults` du contrôleur en
filet — mais c'est une réduction du contrôle, alors faites-en une décision plutôt qu'un
contournement.

### Pourquoi `ServerSideApply=true`

La propriété des champs, pas la taille. L'échec souvent cité
`metadata.annotations: Too long: must have at most 262144 bytes` vient du client-side apply
qui fourre l'objet entier dans `kubectl.kubernetes.io/last-applied-configuration`, et les
CRDs de Crystal Backup n'en approchent pas — la plus grosse, `clusterbackupschedules`, fait
environ 20 Ko de JSON contre une limite de 256 Kio. Si vous rencontrez cette erreur, elle ne
vient pas d'ici.

Utilisez quand même le server-side apply, parce que les CRDs et le RBAC sont aussi écrits
par l'operator et par Helm à divers moments, et que le SSA est le seul mode qui résout cela
avec des field managers enregistrés au lieu d'écraser silencieusement. `Replace=true` est le
marteau-pilon pour le problème d'annotation et vous n'en avez pas besoin ; sur une CRD c'est
aussi un `replace`, opération plus lourde que ce que la situation demande.

### Pourquoi le prune est désactivé, et pourquoi il n'y a pas de finalizer

Deux interrupteurs distincts, tous deux « destructeurs » par défaut, tous deux coupés ici.

**`prune: false`** empêche l'auto-sync de supprimer tout ce qui disparaît du chart rendu. Ce
qu'il protège, principalement, ce sont les **CRDs** — voir plus bas — dont la suppression
emporte chaque objet `Backup` du cluster.

Il protégeait aussi une autre chose : le **Namespace** `crystal-backup-system`, que le chart
rendait. Supprimer ce namespace détruit la cluster KEK et chaque DEK wrappée, et un repository
dont la DEK a disparu ne peut être lu ni par vous, ni par nous, ni par quiconque obtient le
bucket. Le chart ne le rend plus, donc cette Application ne peut plus le pruner quoi que vous
posiez ici — mais le namespace doit bien vivre quelque part, et où que vous le mettiez,
mettez-y `prune: false` aussi. Un danger déplacé n'est pas un danger supprimé.

**Pas de `resources-finalizer.argocd.argoproj.io`** signifie que supprimer l'Application est
non cascadant : Argo CD cesse de gérer les objets et les laisse tourner. C'est ce que vous
voulez, parce que retirer l'operator est une [procédure ordonnée](#le-retirer) et qu'une
suppression en cascade la fait dans le mauvais ordre par définition.

Vous y perdez du vrai confort GitOps, et c'est le compromis. Une value retirée des values du
chart est toujours réconciliée par le self-heal ; ce qui ne se produit pas automatiquement,
c'est la *suppression*, qui pour cet operator n'est jamais une opération de routine.

## Les CRDs, et en quoi Argo CD diffère de Helm

[Mise à niveau](/CrystalBackup/fr/docs/guides/upgrading/) dit que Helm installe les CRDs à
la première installation et ne les met jamais à jour. **C'est vrai de `helm install` /
`helm upgrade`, et Argo CD ne fait ni l'un ni l'autre.** Argo CD rend le chart avec
`helm template` et applique le résultat ; `crds/` fait partie de cet ensemble rendu par
défaut, ce qui explique l'existence de `source.helm.skipCrds` pour l'exclure. Donc sous Argo
CD les CRDs sont des ressources gérées ordinaires et un bump de version du chart met **bien**
les schémas à jour.

Confirmez-le sur votre installation plutôt que de faire confiance au paragraphe ci-dessus —
le flag a un défaut et les défauts bougent :

```bash
argocd app manifests crystal-backup | grep -c "kind: CustomResourceDefinition"
```

Douze est la réponse attendue. Zéro signifie que les CRDs ne sont pas dans votre ensemble
rendu, et que vous devez les appliquer vous-même avant chaque mise à niveau :

```bash
helm pull oci://ghcr.io/crystalbackup/charts/crystal-backup --version 0.6.2 --untar
kubectl apply -f crystal-backup/crds/
```

Le revers du fait qu'Argo CD les gère, c'est qu'elles sont prunables, ce qui est l'autre
moitié de la raison pour laquelle `prune: false` n'est pas négociable. Supprimer les douze
CRDs supprime chaque objet `Backup`, `BackupLocation` et `Restore` du cluster — et si
l'operator est parti le premier, bloque pour toujours sur les finalizers que personne n'est
plus là pour lever.

## 3. Secrets

**Le chart ne crée jamais de Secret** — ni la cluster KEK, ni une DEK, ni des credentials de
stockage objet. C'est une position de conception, pas un oubli : une clé générée dans le
cluster est perdue avec le cluster, ce qui rendrait chaque backup irrécupérable.

Deux Secrets sont nécessaires avant que le plan cluster fonctionne, tous deux dans
`crystal-backup-system` :

| Secret | Clé | Contenu |
|---|---|---|
| `cluster-kek` | `identity` | l'identité age qui wrappe chaque DEK de la plateforme |
| p. ex. `dr-s3` | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | les credentials du stockage objet |

Les locations du plan namespace référencent un Secret **dans le namespace du tenant
lui-même**, par nom uniquement — l'admission rejette une référence cross-namespace.

Crystal Backup se moque de la façon dont ils arrivent là. Choisissez-en une :

- **External Secrets Operator** — un `ExternalSecret` dans Git, la matière dans Vault / AWS
  Secrets Manager / GCP Secret Manager. Le repository Git contient une référence, jamais une
  clé.
- **SOPS** — Argo CD n'a pas de support SOPS natif, cela suppose donc un plugin de gestion
  de configuration (KSOPS ou équivalent) sur le repo-server. Notez où finit la confiance : le
  chiffré est dans Git et la clé SOPS devient la chose qu'il faut mettre sous séquestre.
- **Sealed Secrets** — un `SealedSecret` dans Git, déchiffrable seulement par le contrôleur
  du cluster cible. Ce qui veut aussi dire qu'un cluster que vous avez perdu ne peut pas les
  déchiffrer : ce n'est donc pas non plus un séquestre.

:::danger[Git n'est pas un séquestre de KEK]
Quoi que vous choisissiez, **la cluster KEK doit être mise sous séquestre en dehors du
cluster** — c'est tout l'intérêt de la générer hors bande. Et soyez délibéré quant à une
copie qui atterrirait dans Git : un decommission détruit la copie de la clé détenue par
CrystalBackup, si bien qu'une copie qui survit dans un historique Git signifie que le
repository reste lisible et que rien n'a réellement été détruit.
:::

### Ordonnancement

Une `ClusterBackupLocation` dont le Secret de KEK est absent rapporte
`EncryptionValid=False` avec le reason `KEKMissing`. Ce n'est pas fatal — le contrôleur
re-vérifie toutes les 30 secondes et la location passe `Ready` dès que le Secret apparaît —
mais c'est du bruit que vous pouvez éviter avec une sync wave (section suivante).

## 4. Locations et schedules

Mettez-les dans une **seconde** Application, sourcée depuis votre propre repository Git. Les
séparer du chart est ce qui vous donne un ordre de suppression : vous pouvez retirer les
ressources et les laisser finaliser pendant que l'operator tourne encore.

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: crystal-backup-config
  namespace: argocd
  annotations:
    argocd.argoproj.io/sync-wave: "10"   # after the operator Application
spec:
  project: platform
  destination:
    server: https://kubernetes.default.svc
    namespace: crystal-backup-system
  source:
    repoURL: https://github.com/example/platform-gitops
    path: clusters/prod-eu-1/crystal-backup
    targetRevision: main
  syncPolicy:
    automated:
      selfHeal: true
      prune: false
    syncOptions:
      - ServerSideApply=true
```

Dans `clusters/prod-eu-1/crystal-backup/`, ordonné avec des sync waves pour que la location
ne passe pas ses premières minutes à rapporter `KEKMissing` :

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupLocation
metadata:
  name: dr-primary
  annotations:
    argocd.argoproj.io/sync-wave: "10"   # after the Secrets, which are wave 0
spec:
  default: true
  mode: Standard
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
  retention:
    keepDaily: 7
    keepWeekly: 4
  maintenance:
    pruneSchedule: "0 3 * * 0"
    checkSchedule: "0 4 * * 0"
    checkReadDataSubset: "1%"
    timezone: Europe/Paris
---
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupSchedule
metadata:
  name: dr-daily
  annotations:
    argocd.argoproj.io/sync-wave: "20"
spec:
  schedule: "0 2 * * *"
  timezone: Europe/Paris
  jitter: true
  concurrencyPolicy: Forbid
  template:
    spec:
      locationRef:
        name: dr-primary
      namespaces:
        matchLabels:
          crystalbackup.io/protect: "true"
      includeManifests: true
      clusterResources:
        enabled: true
      maxConcurrentMovers: 4
```

:::caution[Les sync waves ordonnent l'apply, pas la readiness]
Argo CD n'a pas de health check pour les custom resources de Crystal Backup, elles rapportent
donc `Healthy` dès qu'elles sont appliquées et la wave suivante démarre immédiatement. Les
waves vous achètent un ordre d'apply, ce qui suffit ici puisque chaque contrôleur retente. Ne
lisez pas une Application verte comme « la location est `Ready` » — vérifiez-le séparément.
:::

`clusterID`, `s3.endpoint`, `s3.bucket`, `s3.prefix` et `mode` sont **immuables après la
création** : ensemble, ils composent le chemin du repository. En éditer un dans Git produit
un apply rejeté et une Application `OutOfSync` en permanence, ce qui est le résultat correct
— l'alternative serait de re-pointer silencieusement la location vers un autre repository.
Pour en changer un, créez une nouvelle location.

### Une édition en cluster que Git défera

`admission.deniedNamespaces` se rend dans une ConfigMap
(`crystal-backup-denied-namespaces`) que la `ValidatingAdmissionPolicy` lit via un
`paramRef`, si bien qu'elle peut être éditée dans le cluster pour changer la deny-list sans
toucher à la policy. Sous self-heal, cette édition est annulée à la sync suivante. Changez-la
plutôt dans les values du chart.

## Le collecteur de soak, si vous évaluez

À off par défaut (`soak.enabled: false`), et il doit y rester sur un cluster que vous vous
contentez d'exploiter. C'est un kit de **mesure** pour une évaluation de quinze jours, et sous
Argo CD c'est une value de plus sur l'Application de l'operator :

```yaml
  source:
    helm:
      values: |
        soak:
          enabled: true
```

Ce qu'il coûte : **un pod** (200m CPU / 384Mi de mémoire, requests égales aux limits), **une
PVC de 1Gi**, et un **RBAC cluster-wide en lecture seule** tenu pour toute la durée — son
propre ServiceAccount, pas celui de l'operator, si bien que le révoquer, c'est supprimer des
bindings. Il tourne sur la même image que l'operator, résolue depuis le même digest, donc
c'est par construction le build que vous évaluez.

Deux spécificités Argo CD. La PVC est `ReadWriteOnce` et le Deployment utilise la stratégie
`Recreate` : un sync qui roule le collecteur l'affichera `Progressing` pendant que l'ancien pod
libère le volume — c'est normal, ce n'est pas un sync bloqué. Et remettre la value à off **ne
supprime rien** tant que `prune: false` est posé : le collecteur continue de tourner et de
tenir son RBAC. Retirez-le délibérément :

```bash
kubectl -n crystal-backup-system delete deploy,pvc,sa -l crystalbackup.io/soak=collector
kubectl delete clusterrole,clusterrolebinding -l crystalbackup.io/soak=collector
```

Vérifiez au jour un **et** au jour deux qu'il collecte vraiment :

```bash
kubectl -n crystal-backup-system logs deploy/crystal-backup-soak | grep soak-heartbeat
```

Une ligne par jour, chacune nommant ce qu'elle a collecté. Un jour sans ligne est un jour sans
données — d'où le contrôle au jour deux et pas au jour quatorze. Protocole :
`hack/soak/README.md`.

## Mise à niveau

**Bumpez `targetRevision`. C'est toute la mise à niveau.**

Le chart épingle les images de l'operator, du mover et de la sync **par digest**, et la
pipeline de release estampille les vrais digests d'index dans le `values.yaml` du chart
publié. Il n'y a donc aucun tag d'image dans votre repository Git, et rien à mettre à jour
pour Argo CD Image Updater — la version du chart *est* l'épinglage des images. Une seule
chaîne de version couvre les trois images et le chart.

Deux conséquences :

- Ne posez **pas** `image.tag` dans vos values Helm en espérant que cela change quoi que ce
  soit. `tag` n'est utilisé que quand `digest` est vide, et le digest du chart publié n'est
  jamais vide. Vous obtiendriez une value qui ne rend rien et une mise à niveau qui n'a pas
  eu lieu.
- Surcharger `image.digest` à la main épingle l'operator sans déplacer les digests du mover
  et de la sync, qui sont passés à chaque Job de mover. Une mise à niveau partielle n'est
  pas quelque chose à tenter.

L'API est en `v1alpha1` et chaque milestone est une release mineure : lisez les release notes
avant un bump mineur, avancez une mineure à la fois, et laissez un cycle de backup se
terminer entre les deux. Tout le détail dans
[Mise à niveau](/CrystalBackup/fr/docs/guides/upgrading/).

## Le retirer

**Ne retirez pas Crystal Backup en supprimant des choses de Git.** Six kinds portent un
finalizer que seul l'operator retire — `crystalbackup.io/location`, `/repository`,
`/backup`, `/restore-teardown`, `/cluster-restore-teardown`. Supprimez l'operator pendant
qu'un de ces objets est vivant et il devient impossible à finaliser : l'objet reste, son
namespace ne quitte jamais `Terminating`, et une suppression de CRD ultérieure ne revient
jamais non plus. Un prune qui emporte l'operator et les CRDs en une passe produit exactement
cela.

L'ordre :

1. **Supprimez d'abord l'Application de configuration** (`crystal-backup-config`), puis
   supprimez les `ClusterBackupSchedule`, `ClusterBackupLocation` et le reste **avec
   l'operator toujours en marche**, en suivant la séquence ordonnée de
   [Désinstaller](/CrystalBackup/fr/docs/start/install/#désinstaller). Chaque finalizer se
   lève pendant que son propriétaire est vivant.
2. **Vérifiez qu'il ne reste rien.** C'est le gate ; ne continuez pas tant qu'il affiche
   quoi que ce soit :

   ```bash
   for r in restores clusterrestores backups clusterbackups backuplocations \
            clusterbackuplocations backuprepositories; do
     kubectl get "$r.crystalbackup.io" --all-namespaces --no-headers 2>/dev/null
   done
   ```
3. **Seulement ensuite**, supprimez l'Application de l'operator. Sans le finalizer de
   cascade, cela laisse les objets tourner, alors retirez-les explicitement :

   ```bash
   argocd app delete crystal-backup            # non-cascading
   helm uninstall crystal-backup -n crystal-backup-system
   ```

   Conservez le namespace `crystal-backup-system` sauf si vous entendez aussi détruire la
   KEK et les DEKs wrappées qu'il contient.
4. **Les CRDs, seulement si c'est bien votre intention** — cela supprime tous les objets
   restants de ces kinds :

   ```bash
   kubectl get crd -o name | grep crystalbackup.io | xargs -r kubectl delete --timeout=5m
   ```

### Déjà bloqué en `Terminating` ?

**Réinstallez l'operator. C'est ça, le correctif.** Une CRD bloquée en `Terminating`
continue de servir ses instances, donc un operator ramené à la même version reprend les
suppressions en attente, exécute le teardown qu'il devait et lève les finalizers. Recréez
l'Application à la version que vous avez retirée, ou installez le chart directement, puis
reprenez la séquence ci-dessus dans l'ordre. Le retrait manuel du finalizer est un dernier
recours et il fait fuiter les Jobs de mover et les objets `VolumeSnapshotContent` parqués en
`Retain` que le teardown aurait collectés — la récupération complète, y compris ce qu'il
faut balayer ensuite, est dans
[Désinstaller](/CrystalBackup/fr/docs/start/install/#désinstaller).

## Vérifier

```bash
# Argo CD's own view.
argocd app get crystal-backup

# The operator is up, on the digest the chart pinned.
kubectl -n crystal-backup-system get deploy crystal-backup \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'

# Twelve CRDs are registered.
kubectl get crd -o name | grep -c crystalbackup.io

# The admission policies are bound.
kubectl get validatingadmissionpolicybinding | grep crystalbackup

# The location reached Ready — a green Application does not tell you this.
kubectl get clusterbackuplocations
```

Puis laissez un run planifié se terminer. Une installation que vous n'avez pas vérifiée avec
un vrai backup est une installation que vous n'avez pas vérifiée.

## Ensuite

- [Installer avec Flux](/CrystalBackup/fr/docs/start/install-flux/) — le même operator,
  l'autre contrôleur, et une histoire de CRDs réellement différente.
- [Démarrage rapide](/CrystalBackup/fr/docs/start/quickstart/) — un premier backup et un
  premier restore à la main.
- [Le plan cluster](/CrystalBackup/fr/docs/guides/cluster-plane/) — schedules, sélection des
  namespaces, capture cluster-scoped.
