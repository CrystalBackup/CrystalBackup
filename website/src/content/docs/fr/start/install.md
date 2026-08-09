---
title: Installer avec Helm
description: Installation de l'operator Crystal Backup, des CRDs, du RBAC et des policies d'admission.
sourceFile: src/content/docs/start/install.md
sourceHash: e9d84d937a7958eaea09357638eef2c1c59e9aa7
---

Le chart installe l'operator, les douze CRDs, le RBAC cluster-scoped, les policies
d'admission et des NetworkPolicies default-deny pour le namespace de l'operator.

Lisez d'abord [Prérequis](/CrystalBackup/fr/docs/start/requirements/) — en particulier la
partie sur la génération et la mise sous séquestre de la cluster KEK **avant** d'installer.

## Installer

Le namespace d'abord — le chart ne le crée pas, et
[Prérequis](/CrystalBackup/fr/docs/start/requirements/#le-namespace-de-loperator--créez-le-avant-dinstaller)
explique pourquoi. Si vous l'avez déjà créé pour la cluster KEK, passez directement au
`helm install` :

```bash
kubectl create namespace crystal-backup-system

kubectl label namespace crystal-backup-system \
  pod-security.kubernetes.io/enforce=baseline \
  pod-security.kubernetes.io/enforce-version=latest \
  pod-security.kubernetes.io/audit=restricted \
  pod-security.kubernetes.io/warn=restricted \
  --overwrite
```

Le chart est publié comme artefact OCI sur GHCR.

```bash
helm install crystal-backup \
  oci://ghcr.io/crystalbackup/charts/crystal-backup \
  --version 0.6.4 \
  --namespace crystal-backup-system
```

Pas de `--create-namespace`, et pas de `namespace.create: true`. Le chart ne veut
délibérément pas posséder le namespace : le Secret de la cluster KEK y vit et doit exister
avant l'operator, donc à l'heure de l'installation le namespace est déjà là — et un chart qui
rendrait un objet `Namespace` demanderait à Helm de l'adopter, ce que Helm refuse avec
`invalid ownership metadata; label validation error: missing key "app.kubernetes.io/managed-by"`.
Ne pas le posséder veut aussi dire qu'aucun prune et aucun `helm uninstall` ne peut emporter
la KEK avec lui.

Ces labels PSA ne sont pas décoratifs. `helm install` et `helm upgrade` relisent le namespace
et **refusent d'installer** sur un namespace dont le niveau `enforce` diverge, en affichant la
commande `kubectl label` exacte — parce que l'alternative, c'est un operator qui démarre, des
locations qui passent `Ready`, et une première sauvegarde qui échoue des semaines plus tard
sous la forme d'un pod de mover que plus rien n'admet.

:::caution[Vous installez plutôt depuis Git ?]
Ne transposez pas la commande ci-dessus en `Application` ou en `HelmRelease` sans aide. Un
contrôleur GitOps prune, recrée et désinstalle de son propre chef, et les trois sont
dangereux ici : un prune peut supprimer le namespace qui contient votre cluster KEK — gardez
`namespace.create` à son défaut `false` et la release ne peut pas être ce qui le supprime —
un `ClusterBackup` recréé entre en collision avec le run dont il porte le nom, et une
désinstallation non ordonnée laisse des namespaces en `Terminating` définitivement. Les
procédures traitent chacun de ces cas explicitement :

- [Installer avec Argo CD](/CrystalBackup/fr/docs/start/install-argocd/)
- [Installer avec Flux](/CrystalBackup/fr/docs/start/install-flux/)
:::

Vérifiez que c'est bien monté :

```bash
kubectl -n crystal-backup-system rollout status deploy/crystal-backup
```

## Ce qui vient d'être installé

**Rien dans `crystal-backup-system` que vous n'ayez déjà.** Le namespace est le vôtre — le
chart s'installe *dedans* et ne le possède jamais. Chaque credential de la plateforme, la
cluster KEK, la clé de plateforme wrappée et chaque Job de mover vivent ici et nulle part
ailleurs. Crystal Backup est un operator cluster singleton ; ne l'installez pas deux fois.

**Douze CRDs**, empaquetés sous le `crds/` du chart. Helm les installe à la première
installation et — c'est le comportement de Helm, pas un choix de ce chart — **ne les met pas
à jour**. Voir [Mise à niveau](/CrystalBackup/fr/docs/guides/upgrading/).

**Trois ClusterRoles**, aux noms stables :

| Nom | Pour | Accorde |
|---|---|---|
| `crystal-backup-operator` | le ServiceAccount de l'operator | lié par le chart |
| `crystal-backup-tenant` | les utilisateurs d'un namespace | tous les verbes sur `backupschedules`, `backuplocations`, `restores`, `backupexternalsyncs` ; lecture seule sur `backups` |
| `crystal-backup-admin` | les administrateurs de la plateforme | tous les verbes sur les six kinds `cluster*` ; lecture seule sur `backuprepositories` |

**Ni le rôle tenant ni le rôle admin ne sont liés par le chart.** C'est vous qui les liez.

Le rôle tenant porte toujours `crystalbackup.io/aggregate-to-namespace-user: "true"` et —
quand `rbac.aggregateToDefaultRoles` vaut true, ce qui est le défaut — également les labels
standard `rbac.authorization.k8s.io/aggregate-to-edit` et `-admin`. Avec l'agrégation
activée, quiconque dispose déjà de `edit` dans un namespace y gagne automatiquement les
permissions tenant.

Notez l'asymétrie : `crystal-backup-admin` n'accorde **rien** sur les kinds namespacés. Un
administrateur qui a aussi besoin de lire les objets `Backup` des tenants a besoin du rôle
tenant en plus.

**Les policies d'admission.** Sept objets `ValidatingAdmissionPolicy` plus un petit webhook.
Voir [Règles d'admission](/CrystalBackup/fr/docs/reference/admission/).

## Provisionner la cluster KEK

Rien sur le plan cluster ne fonctionne sans elle. Prenez l'identité age que vous avez
générée et mise sous séquestre :

```bash
kubectl -n crystal-backup-system create secret generic cluster-kek \
  --from-file=identity=cluster-kek.txt
```

Une `ClusterBackupLocation` dont le Secret de KEK est absent n'échoue pas silencieusement —
elle rapporte la condition `EncryptionValid=False` avec le reason `KEKMissing`, et rien
n'est jamais généré à sa place.

## Provisionner les credentials du stockage objet

Pour le plan cluster, dans le namespace de l'operator :

```bash
kubectl -n crystal-backup-system create secret generic dr-s3 \
  --from-literal=AWS_ACCESS_KEY_ID=... \
  --from-literal=AWS_SECRET_ACCESS_KEY=...
```

Pour une location du plan namespace, le Secret équivalent vit dans **le namespace du tenant
lui-même**, et il est référencé par nom uniquement. Cette référence par nom seul est une des
règles d'admission : une `BackupLocation` ne peut pas pointer vers un Secret d'un autre
namespace.

## Les values qui méritent d'être posées

La liste complète est dans [Values Helm](/CrystalBackup/fr/docs/reference/helm-values/).
Celles qui demandent généralement de l'attention à la première installation :

```yaml
# Add your incumbent backup tool's namespace, so tenant-facing Crystal Backup
# resources cannot be created there.
admission:
  deniedNamespaces:
    - "kube-*"
    - crystal-backup-system
    - velero

# An on-premises S3 endpoint on a private address: movers are denied those
# ranges by default, so it needs an explicit exception.
networkPolicy:
  extraMoverEgress:
    - to:
        - ipBlock:
            cidr: 10.20.30.40/32
      ports:
        - protocol: TCP
          port: 443

  # Which namespace may open a connection to the metrics port. Empty — the default —
  # allows any pod in the cluster. See "L'observabilité est opt-in" below.
  monitoringNamespace: monitoring
```

## L'observabilité est opt-in, et « off » veut dire aucune alerte

Trois values sont à off par défaut, et une première installation qui les laisse ainsi se prive
de la moitié de ce produit qui vous dit qu'il a cessé de fonctionner. Elles sont à off parce
que chacune exige quelque chose que le chart ne peut pas présupposer :

| Value | Défaut | Ce que « off » veut dire concrètement |
|---|---|---|
| `metrics.serviceMonitor.enabled` | `false` | Rien ne scrape l'operator. Exige les CRDs `monitoring.coreos.com`. |
| `metrics.rules.enabled` | `false` | **Aucune règle d'alerte n'existe.** Rien ne vous dira qu'une sauvegarde a cessé de tourner. Même exigence de CRDs, et les douze seuils relèvent de la politique de la plateforme — lisez-les avant de les activer. |
| `networkPolicy.monitoringNamespace` | `""` | N'importe quel pod du cluster peut ouvrir une connexion vers le port des métriques. (C'est du HTTPS avec authn/authz de l'API server, donc un scrape non autorisé prend un 403 — mais l'ingress, lui, est ouvert.) |

Si vous faites tourner le Prometheus Operator, les trois :

```yaml
metrics:
  serviceMonitor:
    enabled: true
  rules:
    enabled: true
    # A Prometheus Operator only loads rules matching its `ruleSelector`. An unlabelled
    # PrometheusRule is installed, valid, and completely ignored.
    labels:
      release: kube-prometheus-stack
networkPolicy:
  monitoringNamespace: monitoring   # le namespace de votre Prometheus, quel que soit son nom
```

`monitoringNamespace` n'a pas de défaut parce que le chart ne peut pas deviner le nom —
`monitoring`, `monitoring-system`, `observability` et `kube-prometheus-stack` existent tous, et
le mauvais donne une panne de métriques qui ressemble exactement à une installation qui marche.
Lisez les règles d'abord :

```bash
helm show readme oci://ghcr.io/crystalbackup/charts/crystal-backup --version 0.6.4
helm pull oci://ghcr.io/crystalbackup/charts/crystal-backup --version 0.6.4 --untar
less crystal-backup/rules/crystalbackup.rules.yaml
```

Si vous ne faites pas tourner le Prometheus Operator, les métriques sont quand même là sur le
port 8443 en HTTPS — scrapez-les comme vous voulez. Ce que vous ne pouvez pas faire, c'est ne
rien faire en supposant que quelque chose surveille. Voir
[Règles d'alerte](/CrystalBackup/fr/docs/reference/alerts/).

## Le collecteur de soak, si vous évaluez

À off par défaut (`soak.enabled: false`), et il doit y rester sur un cluster que vous vous
contentez d'exploiter. C'est un kit de **mesure** pour une évaluation de quinze jours : il
répond à « qu'est-ce que ça a réellement coûté, sur mes données, sur deux semaines » sans le
moindre Prometheus.

```bash
helm upgrade crystal-backup \
  oci://ghcr.io/crystalbackup/charts/crystal-backup \
  --version 0.6.4 -n crystal-backup-system \
  --reuse-values --set soak.enabled=true
```

Ce qu'il coûte, annoncé d'emblée : **un pod** (200m CPU / 384Mi de mémoire, requests égales
aux limits), **une PVC de 1Gi** dont il n'utilise au plus que `soak.maxBytes`, et un **RBAC
cluster-wide en lecture seule** tenu pour toute la durée — un ServiceAccount distinct de celui
de l'operator, si bien que le révoquer, c'est supprimer des bindings et pas éditer ceux de
l'operator. Il tourne sur la même image que l'operator, donc c'est par construction le build
que vous évaluez.

Vérifiez au jour un **et** au jour deux qu'il collecte vraiment :

```bash
kubectl -n crystal-backup-system logs deploy/crystal-backup-soak | grep soak-heartbeat
```

Une ligne par jour, chacune nommant ce qu'elle a collecté. Un jour sans ligne est un jour sans
données — et c'est toute la raison de regarder au jour deux plutôt qu'au jour quatorze.
Protocole complet, y compris l'export et ce que la rédaction promet : `hack/soak/README.md`.

## Vérifier l'installation

```bash
# The operator is running.
kubectl -n crystal-backup-system get pods

# The CRDs are registered.
kubectl get crd -l app.kubernetes.io/managed-by=Helm | grep crystalbackup.io

# The admission policies are bound.
kubectl get validatingadmissionpolicybinding | grep crystalbackup
```

Si le pod reste bloqué au pull de son image, vérifiez que `image.digest` est bien un digest
réellement publié. Le chart porte un digest placeholder dans les sources ; la pipeline de
release y substitue le vrai au moment de la publication, si bien qu'un chart installé depuis
un checkout des sources plutôt que depuis GHCR ne pourra pas puller.

## Désinstaller

**La désinstallation est ordonnée, et l'ordre n'est pas une préférence.** Six des douze
kinds portent un finalizer — `crystalbackup.io/location`, `/repository`, `/backup`,
`/restore-teardown`, `/cluster-restore-teardown` — et l'operator est le seul processus qui
en retire un. Supprimez l'operator alors qu'un tel objet est encore vivant et cet objet ne
pourra plus jamais être supprimé : son namespace s'arrête en `Terminating`
**définitivement**, et un `kubectl delete crd` ultérieur l'attendra pour toujours.
`helm uninstall` ne vous préviendra pas ; il réussit, et les dégâts apparaissent après.

Chaque commande ci-dessous est bornée par un `--timeout`, volontairement. Un
`kubectl delete` non borné dans cette séquence, c'est un terminal que vous finissez par
tuer, sans rien avoir obtenu.

Si ce qu'il vous faut n'est pas la séquence mais ce que chacune de ces suppressions retire
réellement — dans le cluster, dans la couche CSI et dans le repository —, c'est un tableau dans
[Retirer Crystal Backup](/CrystalBackup/fr/docs/operations/uninstall/).

**1. Arrêtez ce qui crée du nouveau travail.**

```bash
kubectl delete clusterbackupschedule --all --timeout=2m
kubectl delete clusterbackupexternalsync --all --timeout=2m
kubectl delete backupschedule --all --all-namespaces --timeout=2m
kubectl delete backupexternalsync --all --all-namespaces --timeout=2m
```

**2. Supprimez les objets finalisés, l'operator toujours en marche** — les restores et les
backups avant les locations qui adressent leur repository :

```bash
kubectl delete restore        --all --all-namespaces --timeout=5m
kubectl delete clusterrestore --all --timeout=5m
kubectl delete clusterbackup  --all --timeout=5m
kubectl delete backup         --all --all-namespaces --timeout=5m
kubectl delete backuplocation --all --all-namespaces --timeout=5m
kubectl delete clusterbackuplocation --all --timeout=5m
```

Rien n'est touché dans le stockage objet — supprimer une location n'efface jamais les objets
du repository. C'est délibéré : l'effacement est une opération explicite et confirmée. Voir
[Le droit à l'effacement](/CrystalBackup/fr/docs/guides/erasure/).

**3. Vérifiez avant d'aller plus loin.** C'est le gate ; ne continuez pas tant qu'il affiche
quoi que ce soit :

```bash
for r in restores clusterrestores backups clusterbackups backuplocations \
         clusterbackuplocations backuprepositories; do
  kubectl get "$r.crystalbackup.io" --all-namespaces --no-headers 2>/dev/null
done
```

Le silence signifie que chaque finalizer a été levé par l'operator qui le possède. Une
sortie signifie que quelque chose est encore en train de finaliser — enquêtez
**maintenant**, pendant que l'operator qui peut encore corriger tourne
(`kubectl logs -n crystal-backup-system deploy/crystal-backup`).

**4. Retirez l'operator.**

```bash
helm uninstall crystal-backup -n crystal-backup-system
```

Helm ne supprime **pas** les CRDs, donc vos projections `Backup` survivent. Il ne supprime pas
non plus le namespace, puisqu'il ne l'a jamais possédé — c'est tout l'intérêt de
`namespace.create: false`. Conservez `crystal-backup-system` sauf si vous entendez aussi
détruire la cluster KEK et les DEKs wrappées qu'il contient ; les supprimer rend définitivement
illisible chaque repository qu'elles protègent.

(Si vous avez installé avec `namespace.create: true`, `helm uninstall` supprime **bel et bien**
le namespace, et les clés avec. Sortez-en les Secrets d'abord, ou n'utilisez pas cette value.)

**5. Retirez les CRDs, seulement si c'est bien votre intention.** Cela supprime tous les
objets restants de ces kinds :

```bash
kubectl get crd -o name | grep crystalbackup.io | xargs -r kubectl delete --timeout=5m
```

Une fois l'étape 3 passée, cela revient en quelques secondes. Lancez-la avant, et elle
bloque pour toujours.

### Déjà bloqué en Terminating ?

```bash
kubectl get ns <ns> -o jsonpath='{.status.phase}{"\n"}'
kubectl get backup -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{" "}{.metadata.finalizers}{"\n"}{end}'
```

**Réinstallez l'operator — c'est ça, le correctif.** Les objets sont toujours servis (une
CRD bloquée en `Terminating` continue de servir ses instances), donc un operator ramené à la
même version reprend les suppressions en attente, exécute le teardown qu'il devait et lève
les finalizers. Puis reprenez la séquence ci-dessus, dans l'ordre.

```bash
helm install crystal-backup oci://ghcr.io/crystalbackup/charts/crystal-backup \
  --version <the version you removed> -n crystal-backup-system
```

Seulement si vous ne pouvez pas réinstaller, retirez le finalizer à la main — cela débloque
le namespace et **fait fuiter** les Jobs de mover et les objets `VolumeSnapshotContent`
parqués en `Retain` que le teardown aurait collectés, et qu'il vous faut alors balayer
vous-même :

```bash
kubectl patch backup <name> -n <ns> --type=merge -p '{"metadata":{"finalizers":null}}'
kubectl -n crystal-backup-system delete job -l app.kubernetes.io/managed-by=crystal-backup
kubectl get volumesnapshotcontent -l app.kubernetes.io/managed-by=crystal-backup
```

Ne supprimez **pas** en masse les Secrets portant ce label dans le namespace de l'operator :
les DEKs wrappées le portent aussi, et en supprimer une rend son repository illisible pour
de bon.

Ensuite : [Démarrage rapide](/CrystalBackup/fr/docs/start/quickstart/).
