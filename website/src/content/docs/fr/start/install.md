---
title: Installer avec Helm
description: Installation de l'operator Crystal Backup, des CRDs, du RBAC et des policies d'admission.
sourceFile: src/content/docs/start/install.md
sourceHash: 71bed39ed6e6bab8132eb2eb0393ca7fbfe6b748
---

Le chart installe l'operator, les douze CRDs, le RBAC cluster-scoped, les policies
d'admission et des NetworkPolicies default-deny pour le namespace de l'operator.

Lisez d'abord [Prérequis](/CrystalBackup/fr/docs/start/requirements/) — en particulier la
partie sur la génération et la mise sous séquestre de la cluster KEK **avant** d'installer.

## Installer

Le chart est publié comme artefact OCI sur GHCR.

```bash
helm install crystal-backup \
  oci://ghcr.io/crystalbackup/charts/crystal-backup \
  --version 0.6.1 \
  --namespace crystal-backup-system \
  --create-namespace
```

Le chart crée lui-même le namespace par défaut (`namespace.create: true`) ;
`--create-namespace` est donc une ceinture en plus des bretelles.

:::caution[Vous installez plutôt depuis Git ?]
Ne transposez pas la commande ci-dessus en `Application` ou en `HelmRelease` sans aide. Un
contrôleur GitOps prune, recrée et désinstalle de son propre chef, et les trois sont
dangereux ici : un prune peut supprimer le namespace qui contient votre cluster KEK, un
`ClusterBackup` recréé entre en collision avec le run dont il porte le nom, et une
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

**Le namespace `crystal-backup-system`.** Chaque credential de la plateforme, la cluster
KEK, la clé de plateforme wrappée et chaque Job de mover vivent ici et nulle part ailleurs.
Crystal Backup est un operator cluster singleton ; ne l'installez pas deux fois.

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

# Prometheus Operator present?
metrics:
  serviceMonitor:
    enabled: true
```

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

Helm ne supprime **pas** les CRDs, donc vos projections `Backup` survivent. Conservez le
namespace `crystal-backup-system` sauf si vous entendez aussi détruire la cluster KEK et les
DEKs wrappées qu'il contient — les supprimer rend définitivement illisible chaque repository
qu'elles protègent.

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
  --version <the version you removed> -n crystal-backup-system --create-namespace
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
