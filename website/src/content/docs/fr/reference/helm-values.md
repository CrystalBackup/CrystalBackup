---
title: Values Helm
description: Les values configurables du chart, groupées par ce qu'elles affectent réellement.
sourceFile: src/content/docs/reference/helm-values.md
sourceHash: b698001ae439fd1c226f3dbb65a72f45bc3b1b1f
---

Les défauts viennent du `values.yaml` du chart lui-même. Seules les values que vous avez des
chances de changer sont annotées ; les autres sont listées par souci d'exhaustivité.

```bash
helm show values oci://ghcr.io/crystalbackup/charts/crystal-backup --version 0.6.2
```

## Namespace et nommage

| Value | Défaut | Notes |
|---|---|---|
| `namespace.create` | `false` | **À off**, parce que le Secret de la cluster KEK entre dans ce namespace avant le chart — il existe donc déjà, et rendre un `Namespace` demande à Helm de l'adopter, ce qu'il refuse (`invalid ownership metadata`). Créez et labellisez le namespace vous-même. `true` est pour un cluster vierge, et donne la propriété à Helm : un prune ou un `helm uninstall` supprime alors le namespace, et les clés avec. |
| `namespace.name` | `crystal-backup-system` | Chaque credential de la plateforme, la cluster KEK, la clé de plateforme wrappée et chaque Job de mover vivent ici et nulle part ailleurs. |
| `namespace.podSecurityLabels` | `enforce: baseline`, `audit`/`warn: restricted` | La posture que le namespace **doit** avoir, pas seulement des labels que le chart estampille. `baseline` plutôt que `restricted` parce que les data movers tournent en `runAsUser: 0` avec `DAC_OVERRIDE` pour préserver la propriété des fichiers au restore ; l'assouplissement ne s'applique qu'à ce namespace. Estampillés quand `create: true` ; quand `create: false`, le chart relit le namespace vivant sur `helm install`/`upgrade` et **refuse** un niveau `enforce` divergent, avec la commande `kubectl label` exacte. `restricted`, ou l'absence de clé `enforce`, est refusé au rendu sur tous les chemins. Videz la map pour couper le contrôle. |
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

## Où tournent les Jobs de mover

Le bloc ci-dessus place le **pod de l'operator**. Celui-ci place chaque **Job de mover** —
backup et restore par PVC, capture des manifests, discovery, rétention, prune, check, unlock et
sync externe, sur les deux plans. Nouveau en 0.6.3 ; vide, le défaut, planifie les movers
n'importe où, exactement comme toutes les releases précédentes.

| Value | Défaut | Notes |
|---|---|---|
| `mover.placement.nodeSelector` | `{}` | Une exigence **dure**, sans forme souple. Pointé sur un label que seuls quelques nœuds portent, il ne fait pas *préférer* ces nœuds aux movers : il sérialise tous les backups du cluster à travers eux, et transforme leur absence en un cluster sans aucun backup. Les deux côtés sont rendus entre guillemets, si bien que `--set mover.placement.nodeSelector.zone=3` reste la chaîne `"3"` plutôt qu'un entier sur lequel l'operator refuse de démarrer. |
| `mover.placement.tolerations` | `[]` | Pour un pool de nœuds retenu par un taint. La seule des trois values conservée sur un Job **épinglé à un nœud** : un taint `NoExecute` s'applique aux pods en cours d'exécution quel qu'ait été leur placement, et évincerait un restore en pleine copie. |
| `mover.placement.affinity` | `{}` | Transmise au pod telle quelle. `nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution` est le champ vers lequel se tourner quand c'est une préférence, et non une exigence, que vous voulez exprimer. La node affinity est validée au démarrage ; la pod affinity et l'anti-affinity sont transmises à l'API server, et une règle écrite à la main a toutes les chances d'entrer en conflit avec la répartition souple par hostname que l'operator pose déjà pour le fan-out, plutôt que de s'y ajouter. |

C'est un réglage d'**administrateur** et rien d'autre : il n'y a délibérément aucune surcharge
par namespace ni par schedule. Un placement que l'operator n'arrive pas à interpréter **l'arrête
au démarrage** au lieu d'être ignoré, et changer la value redémarre le pod de l'operator — le
fichier est lu une seule fois au démarrage et le deployment en porte un checksum.

Voir [Où tournent les
movers](/CrystalBackup/fr/docs/guides/cluster-plane/#où-tournent-les-movers) pour la raison de
le poser et le seul Job qui y échappe.

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
| `networkPolicy.create` | `true` | Activé par défaut. Un mover détient nécessairement des credentials donnant un accès complet à son repository — un repository partagé ne peut pas être découpé par une policy de stockage — donc ce confinement d'egress est l'un des deux seuls vrais contrôles sur un mover compromis. Des credentials restreints par tenant ne viendront pas : traitez cette valeur comme portante. |
| `networkPolicy.dnsNamespace` | `kube-system` | Sélectionné par `kubernetes.io/metadata.name`. |
| `networkPolicy.clusterInternalCIDRs` | RFC1918 + link-local + loopback | Les plages que les movers ne doivent **pas** atteindre sur le 443. C'est ce qui empêche un mover compromis de pivoter vers les services internes au cluster. |
| `networkPolicy.extraMoverEgress` | `[]` | **Un endpoint S3 on-premises sur une adresse privée a besoin d'une entrée ici.** Le défaut est fermé et l'exception est visible. |
| `networkPolicy.extraOperatorEgress` | `[]` | |
| `networkPolicy.apiServerCIDRs` | `[]` | Vide autorise les ports largement — ça marche, mais ce n'est pas étroit. Posez-y l'adresse de votre API server. Réduit **à la fois** la policy du manifest-mover et l'egress de l'operator vers l'API server ; avant 0.6.3, seule la première l'était, ce dont le nom ne disait rien. La règle de stockage objet de l'operator garde son propre `0.0.0.0/0` non réduit. |
| `networkPolicy.apiServerPorts` | `[443, 6443]` | Un sur-ensemble délibéré. Le Service `kubernetes` écoute sur 443 et kube-proxy fait le DNAT vers le vrai port des Endpoints de l'API server — 6443 sur k3s, RKE2, kubeadm — **avant** que le CNI n'évalue l'egress ; une policy qui ne nomme que 443 ne matche donc rien là-bas et l'operator ne démarre jamais (`dial tcp 10.43.0.1:443: i/o timeout`). Réduisez-le au seul port de votre cluster si vous le souhaitez. |
| `networkPolicy.apiServerPort` | `null` | **Déprécié.** Un scalaire ici remplace entièrement `apiServerPorts`, ce qui garde à une installation qui l'avait réduit exactement la posture qu'elle demandait. Utilisez `apiServerPorts`. |
| `networkPolicy.webhookPort` | `9443` | |
| `networkPolicy.metricsPort` | `8443` | |
| `networkPolicy.monitoringNamespace` | `""` | Vide autorise **n'importe quelle source** sur le port des metrics — le seul défaut permissif de ce bloc. L'endpoint est en HTTPS avec authn/authz de l'API server, donc un scrape non autorisé prend un 403, mais l'ingress est ouvert. Pas de défaut parce que le nom est indevinable (`monitoring`, `monitoring-system`, `observability`, `kube-prometheus-stack`) et que le mauvais donne une panne de métriques qui ressemble à une installation qui marche. Posez-le. |
| `networkPolicy.moverManagedByValue` | `crystal-backup` | Doit correspondre à ce que l'operator estampille sur les pods de mover. |

:::caution[L'application, c'est le travail de votre CNI]
Certains CNI acceptent les objets NetworkPolicy et n'appliquent rien — le `kindnet` par
défaut de Kind en fait partie. Leur présence n'est pas en soi la preuve que le confinement
tient. Vérifiez-le sur votre CNI.
:::

## Collecteur de soak

Désactivé par défaut. Une fois activé il ajoute **un pod résident** (200m CPU / 384Mi mémoire,
requests égales aux limits) et **un PVC**, et accorde un ServiceAccount **en lecture seule,
cluster-wide**, distinct de celui de l'operator. Il existe pour répondre à des questions que la CI
ne peut pas trancher — ce que devraient être les profils mémoire des movers sur de vraies données,
ce que quinze jours d'ordonnancement réel produisent — et il se met en route délibérément, se
laisse tourner deux semaines, puis s'exporte et s'éteint.

Le protocole, et ce qu'il faut vérifier dès le premier jour, sont dans
[`hack/soak/README.md`](https://github.com/CrystalBackup/CrystalBackup/blob/main/hack/soak/README.md).

| Value | Défaut | Notes |
|---|---|---|
| `soak.enabled` | `false` | Rend le Deployment du collecteur, son PVC et sa RBAC. Rien n'est créé tant que c'est `false`. |
| `soak.saltMethod` | `auto` | `auto` dérive le sel de rédaction de l'UID du namespace de l'operator et ne crée aucun Secret ; `fromSecret` utilise celui que vous avez créé. Les deux produisent des archives aux **garanties de réversibilité différentes** — lisez le bloc `redaction` de l'archive avant de l'envoyer où que ce soit. |
| `soak.saltSecret` | `""` | Exigé par `saltMethod: fromSecret`, et par lui seul. Régler les deux, ou aucun, est refusé au rendu du template plutôt qu'en CrashLoopBackOff. |
| `soak.storage` | `1Gi` | La demande du PVC. Si votre StorageClass par défaut est adossée au nœud (local-path), ce PVC **est** du disque de nœud. |
| `soak.maxBytes` | `512Mi` | Le plafond dur que le collecteur respecte : il fait tourner les données les plus anciennes plutôt que de grossir. Volontairement sous la taille du PVC. |
| `soak.kubeletStats` | `false` | Lie le ClusterRole `nodes/proxy`, seule source du high-water du cache restic. Le rôle est rendu dans tous les cas, et laissé non lié tant que vous ne réglez pas ceci. |
| `soak.metricsInterval` | `60s` | Cadence de scrape de l'operator. |
| `soak.metricsResolution` | `5m` | Fenêtre d'agrégation des scrapes. |
| `soak.moverSampleInterval` | `15s` | Fréquence d'**échantillonnage** des pods movers (metrics.k8s.io, stats de cache du kubelet). Cela ne décide pas si un mover est vu : les chiffres exacts par mover arrivent par un watch, car un Job mover vit dix à vingt secondes et aucun intervalle de sondage ne l'attrape de façon fiable. |
| `soak.selfcheckInterval` | `24h` | Le self-check quotidien d'installation. |
| `soak.stateInterval` | `1h` | Instantanés du statut des CR. |

Vérifiez qu'il collecte **au jour 1 et au jour 2**, pas au jour 14 :

```sh
kubectl -n crystal-backup-system logs deploy/crystal-backup-soak | grep soak-heartbeat | tail -7
```

Une ligne par jour. `silent=none` est ce que vous voulez ; `movers_by_class=` doit afficher un
compte non nul pour chaque classe que vos schedules exercent réellement — une classe à zéro alors
que des sauvegardes tournent signifie que l'instrument est aveugle, pas que votre charge de
travail était au repos.

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
