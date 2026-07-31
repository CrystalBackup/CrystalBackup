---
title: Installer avec Flux
description: Gérer Crystal Backup depuis Git avec Flux — la mise à jour des CRDs qu'il faut demander, le prune qui détruit vos clés, et ce qui ne doit jamais être réconcilié.
sourceFile: src/content/docs/start/install-flux.md
sourceHash: 2183d7cbba14df72b9eb025f2379709697c61272
---

C'est l'[install Helm](/CrystalBackup/fr/docs/start/install/) pilotée depuis Git. Le chart
est le même, les values sont les mêmes, et tout ce que cette page dit de la KEK, du RBAC et
de la désinstallation ordonnée s'applique toujours.

Ce qui change, c'est qu'un contrôleur installe désormais, met à niveau et — si vous le
laissez faire — désinstalle la release de son propre chef. Trois propriétés de Crystal
Backup interagissent mal avec cela, et elles sont la raison d'être de cette page plutôt
qu'un « pointez un `HelmRelease` sur le chart » en une ligne.

## À lire d'abord

**1 — Un run n'est pas un état désiré.** `ClusterBackup`, `Backup`, `Restore`,
`ClusterRestore` et `ClusterErasure` sont des *exécutions*, plus proches d'un `Job` que d'un
`Deployment`. En réconcilier un revient à le recréer, et un run recréé est un autre run
portant le même nom. Voir [Ce qui va dans Git](#ce-qui-va-dans-git).

**2 — Retirer un `HelmRelease` déclenche `helm uninstall`.** Ce qui supprime le Namespace
`crystal-backup-system` que le chart rend, lequel contient la cluster KEK et chaque DEK
wrappée. Chaque repository qu'elles protègent devient définitivement illisible — un
[decommission](https://github.com/CrystalBackup/CrystalBackup/blob/main/docs/DECOMMISSION.md#14-the-key-itself)
exécuté par accident, et cela commence par quelqu'un qui supprime un fichier de Git. Voir
[Protéger le namespace](#protéger-le-namespace).

**3 — Retirer l'operator est une opération ordonnée, et Flux ignore l'ordre.** Six kinds
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

N'importe quel delete-and-recreate fait ce qui casse cela : l'objet revient avec le même nom
et un **nouvel UID**, tandis que les objets `Backup` enfants du run précédent — délibérément
non possédés par le run, parce que ce sont des points de restauration — restent où ils sont.
Sous Flux, vous y arrivez de deux façons : en prunant la ressource dehors puis dedans, ou
avec une `Kustomization` en `spec.force: true` qui la recrée sur un conflit de champ
immuable.

Chaque `Backup` créé par un run porte l'annotation `crystalbackup.io/parent-uid`, le
`metadata.uid` de l'objet qui l'a créé. Le second run trouve à sa coordonnée un `Backup`
avec le parent UID de quelqu'un d'autre et met ce namespace en échec avec
`RunNameCollision` : un `FailureRecord`, compté dans `namespacesFailed`, qui amène le run en
`Failed` ou `PartiallyFailed`.

Cet échec est le correctif, pas le problème. Avant qu'il n'existe, le second run *skippait*
le namespace et agrégeait les volumes complétés de l'occupant comme les siens —
`namespacesSucceeded` en hausse, phase `Completed`, sur des snapshots écrits des jours plus
tôt, avec `addedBytes: 0` comme seule différence visible. Un échec bruyant à chaque
réconciliation reste une mauvaise expérience, cela dit. Ne mettez pas de runs dans Git.

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

## 1. La source du chart

Le chart est publié comme artefact OCI sur `ghcr.io/crystalbackup/charts` par la pipeline de
release, et y est signé avec cosign.

```yaml
apiVersion: source.toolkit.fluxcd.io/v1beta2
kind: OCIRepository
metadata:
  name: crystal-backup
  namespace: flux-system
spec:
  interval: 1h
  url: oci://ghcr.io/crystalbackup/charts/crystal-backup
  ref:
    tag: "0.6.0"        # the pin. Bumping this IS the upgrade.
```

:::note
`OCIRepository` est passé de `v1beta2` à `v1` dans les releases récentes de Flux. Utilisez
celle que votre cluster sert — `kubectl api-resources | grep ocirepositories` — les champs
ci-dessus sont les mêmes dans les deux.
:::

Les packages publiés sont publics, donc aucun `secretRef` n'est nécessaire. Épinglez plus
fort avec `ref.digest` plutôt que `ref.tag` si votre politique l'exige.

### Optionnellement, vérifier la signature

Le workflow de release signe le chart poussé avec cosign en mode keyless, si bien que Flux
peut refuser de réconcilier un chart non signé :

```yaml
spec:
  verify:
    provider: cosign
    matchOIDCIdentity:
      - issuer: "^https://token\\.actions\\.githubusercontent\\.com$"
        subject: "^https://github\\.com/CrystalBackup/CrystalBackup/\\.github/workflows/images\\.yml@refs/tags/v.*$"
```

Confirmez l'identité contre la release que vous épinglez réellement **avant** d'activer ceci
— un bloc `verify` qui ne matche pas transforme une installation qui marche en
`OCIRepository` en panne :

```bash
cosign verify ghcr.io/crystalbackup/charts/crystal-backup:0.6.0 \
  --certificate-oidc-issuer=https://token.actions.githubusercontent.com \
  --certificate-identity-regexp='^https://github.com/CrystalBackup/CrystalBackup/'
```

:::danger[Ne pointez pas Flux sur le répertoire `charts/` du repository Git]
Deux choses de l'arbre source ne sont remplies qu'au moment de la release.
`charts/crystal-backup/crds/` est **git-ignoré** — les CRDs y sont copiées quand le chart est
packagé — si bien qu'un chart rendu depuis un checkout Git n'installe **aucune CRD**. Et
`image.digest`, `mover.image.digest` et `sync.image.digest` portent un placeholder tout à
zéro que la pipeline de release remplace, si bien que le pod de l'operator ne pullerait
jamais. Utilisez toujours le chart OCI publié.
:::

## 2. Le HelmRelease

```yaml
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: crystal-backup
  namespace: flux-system
spec:
  interval: 1h
  releaseName: crystal-backup          # keep it fixed: the chart stamps
                                       # app.kubernetes.io/instance from it
  targetNamespace: crystal-backup-system
  chartRef:
    kind: OCIRepository
    name: crystal-backup
  install:
    # Helm installs crds/ on first install. `Create` is the default; it is spelled
    # out here so the pair with `upgrade.crds` below reads as one decision.
    crds: Create
  upgrade:
    # NOT the default. Helm's default is Skip: it installs CRDs once and never
    # touches them again, so a version bump would give you a new operator against
    # old schemas — fields you set are silently pruned by the API server and the
    # operator reconciles as though you never set them.
    crds: CreateReplace
  # driftDetection stays DISABLED (the default). See "The webhook certificate" below.
  values:
    admission:
      deniedNamespaces:
        - "kube-*"
        - crystal-backup-system
    metrics:
      serviceMonitor:
        enabled: true
```

`CreateReplace` crée les CRDs manquantes et remplace celles qui existent. Il n'en supprime
jamais aucune, ce qui est la propriété qui compte ici : supprimer les douze CRDs supprime
chaque objet `Backup`, `BackupLocation` et `Restore` du cluster.

### C'est ici que Flux et Argo CD diffèrent réellement

[Mise à niveau](/CrystalBackup/fr/docs/guides/upgrading/) dit que Helm installe les CRDs à
la première installation et ne les met jamais à jour. Le helm-controller de Flux exécute de
**vraies opérations Helm install et upgrade**, cette règle s'applique donc exactement à lui,
et `upgrade.crds: CreateReplace` est la façon de s'en affranchir. Argo CD rend avec
`helm template` et applique la sortie, si bien que son comportement vis-à-vis des CRDs est
un autre problème avec une autre réponse — voir
[Installer avec Argo CD](/CrystalBackup/fr/docs/start/install-argocd/).

Si vous préférez gérer les CRDs vous-même — c'est raisonnable, puisque cela fait du
changement de schéma un commit visible — posez `install.crds: Skip` et `upgrade.crds: Skip`,
vendorez les CRDs dans votre repository Git, et appliquez-les depuis une `Kustomization`
dont le `HelmRelease` `dependsOn`. Extrayez-les avec :

```bash
helm pull oci://ghcr.io/crystalbackup/charts/crystal-backup --version 0.6.0 --untar
ls crystal-backup/crds/
```

## Le certificat du webhook

Le chart frappe la CA et le certificat de service du webhook d'admission au moment du
**render** (`genCA`/`genSignedCert`, sans `lookup`), si bien que chaque render produit une
nouvelle paire. Le helm-controller ne rend qu'à l'install et à l'upgrade, donc sous Flux ce
n'est pas un roulement continu — un `helm upgrade` réémet le certificat et roule le pod, ce
que le chart accepte pour un webhook fail-open qui applique une seule règle.

Cela devient du roulement dès que vous activez la détection de dérive :

```yaml
spec:
  driftDetection:
    mode: enabled          # <- this will fight the chart
```

Avec la détection de dérive activée, le helm-controller compare le cluster à un render frais
et voit que le Secret `crystal-backup-webhook-certs` et le `caBundle` de la
`ValidatingWebhookConfiguration` diffèrent à chaque fois. Laissez-la désactivée, ou excluez
ces deux chemins :

```yaml
spec:
  driftDetection:
    mode: enabled
    ignore:
      - paths: ["/data"]
        target:
          kind: Secret
          name: crystal-backup-webhook-certs
      - paths: ["/webhooks/0/clientConfig/caBundle"]
        target:
          kind: ValidatingWebhookConfiguration
          name: crystal-backup
```

Ces noms ne sont pas préfixés par la release, et c'est délibéré : Crystal Backup est un
operator cluster singleton, si bien que le helper `fullname` du chart garde les noms
cluster-scoped prévisibles pour le binding et l'agrégation côté plateforme. Ils sont
`crystal-backup-*` quel que soit le nom que vous donnez à la release, sauf si vous posez
`fullnameOverride`.

L'alternative est `admission.webhook.enabled: false`. C'est une vraie option — le webhook
est fail-open et applique une seule règle (une seconde `ClusterBackupLocation` par défaut),
avec la condition `MultipleDefaults` du contrôleur en filet — mais c'est une réduction du
contrôle, alors faites-en une décision plutôt qu'un contournement.

## Protéger le namespace

Le chart rend le Namespace `crystal-backup-system` (`namespace.create: true`). Ce qui veut
dire que `helm uninstall` le supprime — et avec lui le Secret `cluster-kek` et chaque DEK
wrappée qu'il contient. Sous Flux, `helm uninstall` est ce qui se produit quand le
`HelmRelease` est retiré de Git et que la `Kustomization` parente le prune. Un fichier
supprimé devient un repository illisible.

Deux garde-fous, et vous voulez les deux :

**Sortez le Namespace de la release.** Posez `namespace.create: false` et gérez-le depuis
une `Kustomization` avec le pruning désactivé, pour que rien dans le chemin de livraison ne
puisse le retirer :

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: crystal-backup-system
  annotations:
    kustomize.toolkit.fluxcd.io/prune: disabled
  labels:
    # The chart's own defaults. `baseline`, not `restricted`: the operator is
    # restricted-compliant, but data movers run as uid 0 with DAC_OVERRIDE in this
    # namespace to preserve file ownership on restore, which restricted would deny.
    pod-security.kubernetes.io/enforce: baseline
    pod-security.kubernetes.io/enforce-version: latest
    pod-security.kubernetes.io/audit: restricted
    pod-security.kubernetes.io/warn: restricted
```

**Ne prunez pas la `Kustomization` de l'operator.** Posez `prune: false` sur la
`Kustomization` qui porte le `HelmRelease`. Retirer l'operator est une
[procédure ordonnée](#le-retirer), et une désinstallation automatique la fait dans le
mauvais ordre par définition.

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

- **SOPS** — Flux le déchiffre nativement, sans plugin : `spec.decryption.provider: sops`
  sur la `Kustomization`, avec la clé age ou KMS dans un Secret de `flux-system`. Notez où
  finit la confiance : le chiffré est dans Git et la clé SOPS devient la chose qu'il faut
  mettre sous séquestre.
- **External Secrets Operator** — un `ExternalSecret` dans Git, la matière dans Vault / AWS
  Secrets Manager / GCP Secret Manager. Le repository Git contient une référence, jamais une
  clé.
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

## 4. Tout câbler ensemble

Trois objets `Kustomization`, ordonnés par `dependsOn`. Contrairement aux sync waves,
`dependsOn` attend que la dépendance devienne **Ready**, donc là c'est réellement séquencé.

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: crystal-backup-namespace
  namespace: flux-system
spec:
  interval: 1h
  sourceRef: { kind: GitRepository, name: platform-gitops }
  path: ./clusters/prod-eu-1/crystal-backup/namespace
  prune: false          # the namespace holds the KEK
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: crystal-backup-secrets
  namespace: flux-system
spec:
  interval: 1h
  dependsOn:
    - name: crystal-backup-namespace
  sourceRef: { kind: GitRepository, name: platform-gitops }
  path: ./clusters/prod-eu-1/crystal-backup/secrets
  prune: false          # pruning the KEK Secret is a decommission
  decryption:
    provider: sops
    secretRef: { name: sops-age }
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: crystal-backup-operator
  namespace: flux-system
spec:
  interval: 1h
  dependsOn:
    - name: crystal-backup-secrets
  sourceRef: { kind: GitRepository, name: platform-gitops }
  path: ./clusters/prod-eu-1/crystal-backup/operator   # OCIRepository + HelmRelease
  prune: false          # removing the operator is a runbook, not a prune
```

Puis les déclarations, dans une quatrième `Kustomization` qui dépend de l'operator — gardée
à part précisément pour que vous puissiez retirer *celles-ci* d'abord, et laisser leurs
finalizers se lever pendant que l'operator tourne encore :

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: crystal-backup-config
  namespace: flux-system
spec:
  interval: 1h
  dependsOn:
    - name: crystal-backup-operator
  sourceRef: { kind: GitRepository, name: platform-gitops }
  path: ./clusters/prod-eu-1/crystal-backup/config
  prune: true
  # force: false — the default, and leave it there. `force` recreates a resource
  # when a patch fails on an immutable field, which is delete-and-recreate: on a
  # ClusterBackupLocation that re-points the repository, and on any run object it
  # is the collision described at the top of this page.
```

`clusters/prod-eu-1/crystal-backup/config/` contient les déclarations :

```yaml
apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupLocation
metadata:
  name: dr-primary
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

`clusterID`, `s3.endpoint`, `s3.bucket`, `s3.prefix` et `mode` sont **immuables après la
création** : ensemble, ils composent le chemin du repository. En éditer un dans Git vous
donne une `Kustomization` qui ne réconciliera pas, ce qui est le résultat correct —
l'alternative serait de re-pointer silencieusement la location vers un autre repository.
Pour en changer un, créez une nouvelle location. (C'est aussi la raison concrète pour
laquelle `force: true` est dangereux ici : il résoudrait ce conflit en supprimant la
location.)

Une `ClusterBackupLocation` dont le Secret de KEK est absent n'est d'ailleurs pas fatale :
elle rapporte `EncryptionValid=False` avec le reason `KEKMissing`, et le contrôleur
re-vérifie toutes les 30 secondes, si bien qu'elle passe `Ready` dès que le Secret atterrit.
La chaîne de `dependsOn` ci-dessus vous épargne simplement le bruit.

### Une édition en cluster que Flux défera

`admission.deniedNamespaces` se rend dans une ConfigMap
(`crystal-backup-denied-namespaces`) que la `ValidatingAdmissionPolicy` lit via un
`paramRef`, si bien qu'elle peut être éditée dans le cluster pour changer la deny-list sans
toucher à la policy. Avec la détection de dérive activée, cette édition est annulée.
Changez-la plutôt dans les values du `HelmRelease`.

## Mise à niveau

**Bumpez `ref.tag` sur l'`OCIRepository`. C'est toute la mise à niveau** — à condition que
`upgrade.crds: CreateReplace` soit posé, ce qui est la seule chose de cette page qu'il est
facile d'oublier et coûteux de découvrir plus tard.

Le chart épingle les images de l'operator, du mover et de la sync **par digest**, et la
pipeline de release estampille les vrais digests d'index dans le `values.yaml` du chart
publié. Il n'y a donc aucun tag d'image dans votre repository Git, et rien à mettre à jour
pour les contrôleurs d'image-automation de Flux — la version du chart *est* l'épinglage des
images. Une seule chaîne de version couvre les trois images et le chart.

Deux conséquences :

- Ne posez **pas** `image.tag` dans les values de votre `HelmRelease` en espérant que cela
  change quoi que ce soit. `tag` n'est utilisé que quand `digest` est vide, et le digest du
  chart publié n'est jamais vide. Vous obtiendriez une value qui ne rend rien et une mise à
  niveau qui n'a pas eu lieu.
- Surcharger `image.digest` à la main épingle l'operator sans déplacer les digests du mover
  et de la sync, qui sont passés à chaque Job de mover. Une mise à niveau partielle n'est
  pas quelque chose à tenter.

L'API est en `v1alpha1` et chaque milestone est une release mineure : lisez les release notes
avant un bump mineur, avancez une mineure à la fois, et laissez un cycle de backup se
terminer entre les deux. Tout le détail dans
[Mise à niveau](/CrystalBackup/fr/docs/guides/upgrading/).

## Le retirer

**Ne retirez pas Crystal Backup en supprimant des fichiers de Git.** Six kinds portent un
finalizer que seul l'operator retire — `crystalbackup.io/location`, `/repository`,
`/backup`, `/restore-teardown`, `/cluster-restore-teardown`. Supprimez l'operator pendant
qu'un de ces objets est vivant et il devient impossible à finaliser : l'objet reste, son
namespace ne quitte jamais `Terminating`, et une suppression de CRD ultérieure ne revient
jamais non plus. Un prune qui retire le `HelmRelease` pendant que des objets `Backup` sont
vivants produit exactement cela.

L'ordre :

1. **Supprimez d'abord la `Kustomization` de configuration**, puis supprimez les
   `ClusterBackupSchedule`, `ClusterBackupLocation` et le reste **avec l'operator toujours
   en marche**, en suivant la séquence ordonnée de
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
3. **Seulement ensuite**, retirez l'operator. Avec `prune: false` sur sa `Kustomization`,
   supprimer les fichiers laisse la release tourner, alors faites-le explicitement :

   ```bash
   flux delete kustomization crystal-backup-operator
   flux delete helmrelease crystal-backup -n flux-system   # runs helm uninstall
   ```

   Conservez le namespace `crystal-backup-system` sauf si vous entendez aussi détruire la
   KEK et les DEKs wrappées qu'il contient. Si vous avez suivi
   [Protéger le namespace](#protéger-le-namespace), `helm uninstall` le laisse tranquille,
   ce qui est tout l'objet de cette section.
4. **Les CRDs, seulement si c'est bien votre intention.** Helm ne les supprime pas, et
   `CreateReplace` non plus ; ceci supprime tous les objets restants de ces kinds :

   ```bash
   kubectl get crd -o name | grep crystalbackup.io | xargs -r kubectl delete --timeout=5m
   ```

### Déjà bloqué en `Terminating` ?

**Réinstallez l'operator. C'est ça, le correctif.** Une CRD bloquée en `Terminating`
continue de servir ses instances, donc un operator ramené à la même version reprend les
suppressions en attente, exécute le teardown qu'il devait et lève les finalizers. Recréez le
`HelmRelease` à la version que vous avez retirée, ou installez le chart directement, puis
reprenez la séquence ci-dessus dans l'ordre. Le retrait manuel du finalizer est un dernier
recours et il fait fuiter les Jobs de mover et les objets `VolumeSnapshotContent` parqués en
`Retain` que le teardown aurait collectés — la récupération complète, y compris ce qu'il
faut balayer ensuite, est dans
[Désinstaller](/CrystalBackup/fr/docs/start/install/#désinstaller).

## Vérifier

```bash
# Flux's own view.
flux get helmreleases -n flux-system
flux get kustomizations -n flux-system

# The operator is up, on the digest the chart pinned.
kubectl -n crystal-backup-system get deploy crystal-backup \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'

# Twelve CRDs are registered — and, after an upgrade, at the new schema.
kubectl get crd -o name | grep -c crystalbackup.io

# The admission policies are bound.
kubectl get validatingadmissionpolicybinding | grep crystalbackup

# The location reached Ready — a Ready HelmRelease does not tell you this.
kubectl get clusterbackuplocations
```

Puis laissez un run planifié se terminer. Une installation que vous n'avez pas vérifiée avec
un vrai backup est une installation que vous n'avez pas vérifiée.

## Ensuite

- [Installer avec Argo CD](/CrystalBackup/fr/docs/start/install-argocd/) — le même operator,
  l'autre contrôleur, et une histoire de CRDs réellement différente.
- [Démarrage rapide](/CrystalBackup/fr/docs/start/quickstart/) — un premier backup et un
  premier restore à la main.
- [Le plan cluster](/CrystalBackup/fr/docs/guides/cluster-plane/) — schedules, sélection des
  namespaces, capture cluster-scoped.
