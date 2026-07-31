---
title: Values Helm
description: Les values configurables du chart, groupées par ce qu'elles affectent réellement.
sourceFile: src/content/docs/reference/helm-values.md
sourceHash: 407e4952c331e2db2f8eee839892d1e30b4eafeb
---

Les défauts viennent du `values.yaml` du chart lui-même. Seules les values que vous avez des
chances de changer sont annotées ; les autres sont listées par souci d'exhaustivité.

```bash
helm show values oci://ghcr.io/crystalbackup/charts/crystal-backup --version 0.6.0
```

## Namespace et nommage

| Value | Défaut | Notes |
|---|---|---|
| `namespace.create` | `true` | Le chart crée lui-même le namespace de l'operator. |
| `namespace.name` | `crystal-backup-system` | Chaque credential de la plateforme, la cluster KEK, la clé de plateforme wrappée et chaque Job de mover vivent ici et nulle part ailleurs. |
| `namespace.podSecurityLabels` | `enforce: baseline`, `audit`/`warn: restricted` | `baseline` plutôt que `restricted` parce que les data movers tournent en `runAsUser: 0` avec `DAC_OVERRIDE` pour préserver la propriété des fichiers au restore. L'assouplissement ne s'applique qu'à ce namespace. |
| `fullnameOverride`, `nameOverride` | `""` | Les noms RBAC cluster-scoped dérivent du nom de base. **Gardez-le stable** — un test golden-file épingle le ClusterRole tenant rendu. |

Crystal Backup est un operator cluster **singleton**. Ne l'installez pas deux fois.

## Images

| Value | Défaut | Notes |
|---|---|---|
| `image.repository` | `ghcr.io/crystalbackup/operator` | |
| `image.digest` | placeholder dans les sources | En production, les images sont référencées **par digest**. La pipeline de release y substitue le vrai digest d'index au moment de la publication, si bien qu'un chart installé depuis un checkout des sources ne pullera pas. |
| `image.tag` | `""` | Utilisé **uniquement** quand `image.digest` est vide. Vaut `.Chart.AppVersion` par défaut. |
| `image.pullPolicy` | `IfNotPresent` | |
| `mover.image.repository` | `ghcr.io/crystalbackup/mover` | Le mover restic. **Requis pour de vrais backups** — l'operator le passe à chaque Job de mover. |
| `mover.image.digest` / `.tag` | comme ci-dessus | |
| `sync.image.repository` | `ghcr.io/crystalbackup/sync` | restic **plus rclone**, une troisième image plutôt qu'un mover plus gros, pour que la surface de dépendances de rclone reste hors du chemin de backup et de restore. Pullée seulement quand une sync externe existe. |
| `sync.image.digest` / `.tag` | comme ci-dessus | |
| `imagePullSecrets` | `[]` | Les images GHCR sont publiques. |

## Déploiement de l'operator

| Value | Défaut | Notes |
|---|---|---|
| `replicaCount` | `1` | Plus d'un est sans danger : l'élection de leader garde exactement un actif, les autres sont des standbys chauds. |
| `resources.requests` | `10m` CPU, `64Mi` | Le travail se passe dans les Jobs de mover, pas ici. |
| `resources.limits` | `500m` CPU, `256Mi` | |
| `extraArgs` | `[]` | Flags supplémentaires du manager. |
| `nodeSelector`, `tolerations`, `affinity` | `{}` / `[]` | Utilisez `affinity` pour répartir les standbys entre nœuds ou zones. |
| `podAnnotations`, `podLabels` | `{}` | |
| `priorityClassName` | `""` | Mérite d'être posé : un operator de backup évincé sous pression cesse de prendre des backups. |
| `terminationGracePeriodSeconds` | `10` | |
| `podSecurityContext` | non-root `65532`, seccomp `RuntimeDefault` | |
| `securityContext` | `readOnlyRootFilesystem`, toutes les capabilities retirées | |
| `livenessProbe`, `readinessProbe` | standard | |

## ServiceAccount et RBAC

| Value | Défaut | Notes |
|---|---|---|
| `serviceAccount.create` | `true` | |
| `serviceAccount.name` | `""` → `<fullname>-operator` | |
| `serviceAccount.annotations` | `{}` | C'est là que vont les annotations IRSA ou Workload Identity. |
| `serviceAccount.automount` | `true` | |
| `rbac.create` | `true` | Installe les ClusterRoles operator, tenant et admin. |
| `rbac.aggregateToDefaultRoles` | `true` | Estampille aussi `rbac.authorization.k8s.io/aggregate-to-edit` et `-admin` sur le ClusterRole tenant, si bien que quiconque a `edit` dans un namespace y gagne les permissions tenant. Un label custom stable, `crystalbackup.io/aggregate-to-namespace-user`, est présent quoi qu'il arrive. |
| `manifestMover.serviceAccountName` | `""` → `<fullname>-manifest-mover` | La seule identité de mover qui atteint l'API server. |

Ni le ClusterRole tenant ni le ClusterRole admin ne sont liés par le chart. C'est vous qui
les liez.

## Metrics et health

| Value | Défaut | Notes |
|---|---|---|
| `metrics.port` | `8443` | HTTPS avec authn/authz de l'API server. |
| `metrics.service.create` | `true` | |
| `metrics.service.type` | `ClusterIP` | |
| `metrics.serviceMonitor.enabled` | `false` | Nécessite les CRDs `monitoring.coreos.com`. |
| `metrics.serviceMonitor.interval` | `30s` | |
| `metrics.serviceMonitor.scrapeTimeout` | `10s` | |
| `metrics.serviceMonitor.labels` | `{}` | À faire correspondre au sélecteur de votre Prometheus. |
| `health.port` | `8081` | `/healthz` et `/readyz`. |

## Admission

| Value | Défaut | Notes |
|---|---|---|
| `admission.vap.enabled` | `true` | Le jeu de `ValidatingAdmissionPolicy`. Nécessite Kubernetes ≥ 1.30. |
| `admission.webhook.enabled` | `true` | La vérification de la location par défaut unique, `failurePolicy: Ignore`, avec un certificat généré par le chart. |
| `admission.deniedNamespaces` | `["kube-*", "crystal-backup-system"]` | **Ajoutez le namespace de votre outil de backup en place.** Rendu dans une ConfigMap liée par `paramRef`, il peut donc aussi être édité en cluster. Noms simples ou préfixes suffixés par `*`. |

Voir [Règles d'admission](/CrystalBackup/fr/docs/reference/admission/).

## Network policy

Default-deny pour le namespace de l'operator, plus des autorisations étroites — pour qu'une
forme de pod ajoutée plus tard démarre sans connectivité plutôt qu'en héritant de tout.

| Value | Défaut | Notes |
|---|---|---|
| `networkPolicy.create` | `true` | Activé par défaut. Tant que les credentials de mover limités au repository n'ont pas atterri, ce confinement d'egress est l'un des deux seuls vrais contrôles sur un mover compromis détenant des credentials root de stockage objet. |
| `networkPolicy.dnsNamespace` | `kube-system` | Sélectionné par `kubernetes.io/metadata.name`. |
| `networkPolicy.clusterInternalCIDRs` | RFC1918 + link-local + loopback | Les plages que les movers ne doivent **pas** atteindre sur le 443. C'est ce qui empêche un mover compromis de pivoter vers les services internes au cluster. |
| `networkPolicy.extraMoverEgress` | `[]` | **Un endpoint S3 on-premises sur une adresse privée a besoin d'une entrée ici.** Le défaut est fermé et l'exception est visible. |
| `networkPolicy.extraOperatorEgress` | `[]` | |
| `networkPolicy.apiServerCIDRs` | `[]` | Vide autorise le port largement — ça marche, mais ce n'est pas étroit. Posez-y l'adresse de votre API server. |
| `networkPolicy.apiServerPort` | `443` | |
| `networkPolicy.webhookPort` | `9443` | |
| `networkPolicy.metricsPort` | `8443` | |
| `networkPolicy.monitoringNamespace` | `""` | Vide autorise n'importe quelle source sur le port des metrics. |
| `networkPolicy.moverManagedByValue` | `crystal-backup` | Doit correspondre à ce que l'operator estampille sur les pods de mover. |

:::caution[L'application, c'est le travail de votre CNI]
Certains CNI acceptent les objets NetworkPolicy et n'appliquent rien — le `kindnet` par
défaut de Kind en fait partie. Leur présence n'est pas en soi la preuve que le confinement
tient. Vérifiez-le sur votre CNI.
:::

## Réservé

| Value | Défaut | Notes |
|---|---|---|
| `clusterID` | `""` | Réservé. Pas encore câblé dans les flags du manager ; un milestone ultérieur le consommera. L'identité de cluster qui compte aujourd'hui est `spec.clusterID` sur la location. |

## Ce que le chart ne fait jamais

**Il ne crée jamais de Secret.** Ni la cluster KEK, ni une clé de données, ni des
credentials.

La cluster KEK est votre racine de confiance : générez-la hors bande avec `age-keygen`,
mettez-la sous séquestre **en dehors** du cluster, et provisionnez-la vous-même avant de
créer une `ClusterBackupLocation`. Une clé générée dans le cluster serait perdue avec le
cluster, et chaque backup avec elle.
