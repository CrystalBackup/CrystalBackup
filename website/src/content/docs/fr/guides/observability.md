---
title: Observabilité
description: Scraper l'endpoint de métriques, lire les logs, et les conditions et Events qui disent vraiment ce qui s'est passé.
sidebar:
  order: 9
sourceFile: src/content/docs/guides/observability.md
sourceHash: a2c59eed3f9c5859ef5f95ab9863c2dea4aec44d
---

:::note
Cette page traite de la façon d'*obtenir* les signaux. La liste exhaustive vit sur
[Métriques](/CrystalBackup/fr/docs/reference/metrics/) et
[Alertes](/CrystalBackup/fr/docs/reference/alerts/) — toutes deux générées depuis le registre
et la table de règles de l'operator lui-même, si bien qu'elles décrivent ce que ce build
publie plutôt que ce qui avait été prévu pour lui.
:::

## L'endpoint de métriques

L'operator sert les métriques Prometheus sur le port **8443**, en **HTTPS avec
authentification et autorisation par l'API server**. C'est le défaut de kubebuilder et c'est
délibéré : un port de métriques non authentifié sur un operator de backup laisse fuir la
forme des données de tous les tenants.

Toutes les séries portent le préfixe `crystalbackup_`. Chacune de celles qui sont par tenant
porte un label `namespace` et un label `cluster` dont la valeur est le `clusterID` de la
location — si bien qu'un seul Prometheus peut héberger les flottes de plusieurs clusters sans
collision.

### Avec le Prometheus Operator

```yaml
metrics:
  serviceMonitor:
    enabled: true
    interval: 30s
    scrapeTimeout: 10s
    labels:
      release: kube-prometheus-stack   # match your Prometheus' selector
```

Nécessite que les CRD `monitoring.coreos.com` soient présentes.

### Sans lui

Le chart livre une ClusterRole non liée, nommée `crystal-backup-metrics-reader`, accordant
`get` sur l'URL non-ressource `/metrics`. Liez-la à l'identité qui scrape :

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: crystal-backup-metrics-reader
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: crystal-backup-metrics-reader
subjects:
  - kind: ServiceAccount
    name: prometheus
    namespace: monitoring
```

Si votre CNI applique les NetworkPolicies livrées, renseignez également
`networkPolicy.monitoringNamespace` avec le namespace où tourne votre scraper. Laissé vide,
toute source est autorisée sur le port des métriques.

## Les sondes de santé

Port **8081**, sans authentification : `/healthz` et `/readyz`. Utilisées par les probes du
pod lui-même ; c'est aussi la réponse la plus rapide à « l'operator est-il seulement
vivant ».

## Les logs

Des lignes JSON sur stdout.

```bash
kubectl -n crystal-backup-system logs deploy/crystal-backup -f
```

Les Jobs de mover loguent séparément — ce sont des pods éphémères dans le même namespace :

```bash
kubectl -n crystal-backup-system get jobs -l app.kubernetes.io/managed-by=crystal-backup
kubectl -n crystal-backup-system logs job/<name>
```

Le Job d'un mover terminé est supprimé une fois son `ttlSecondsAfterFinished` écoulé. Si vous
diagnostiquez une défaillance intermittente, collectez les logs tant qu'il est encore là — ou
lisez plutôt l'enregistrement durable, ce qui est l'objet de la section suivante.

## Les signaux qui comptent vraiment

Les métriques vous disent qu'une flotte est saine. Les conditions et le status vous disent
*pourquoi* une chose ne l'est pas.

### La backup a-t-elle fonctionné ?

```bash
kubectl get backups -A
kubectl -n <ns> get backup <run> \
  -o jsonpath='{range .status.volumes[*]}{.pvc}{"\t"}{.phase}{"\t"}{.sizeBytes}{"\t"}{.addedBytes}{"\t"}{.reason}{"\n"}{end}'
```

Un volume `Skipped` avec `reason: CSISnapshotUnsupported` est celui à guetter : la backup
rapporte `Completed`, et cette PVC n'y est pas. C'est signalé plutôt que silencieusement
abandonné, mais ce n'est signalé que si quelqu'un regarde.

`addedBytes` est le nombre d'octets dédupliqués que cette exécution a réellement ajoutés. Le
surveiller est la façon de repérer un workload qui s'est mis à réécrire tout son jeu de
données chaque nuit.

### Le repository est-il sain ?

```bash
kubectl get backuprepository <name> -o jsonpath='{.status.lastCheckTime}{"\t"}{.status.lastCheckResult}{"\t"}{.status.staleLocks}{"\t"}{.status.lastMaintenanceTime}{"\n"}'
```

Les trois signaux à lire ici, chacun couvert par une règle livrée si vous activez le bundle —
`CrystalbackupRepositoryCheckFailed`, `CrystalbackupMaintenanceStalled` et
`CrystalbackupStaleLocks`. Sans Prometheus, `crystal-backup selfcheck` évalue les trois mêmes
depuis le même état :

- `lastCheckResult: Failed` — restic a trouvé des dégâts dans le repository. Un incident.
- `lastCheckTime` très ancien — rien n'a été vérifié depuis longtemps. Un incident différent.
- `staleLocks` durablement non nul — les verrous s'accumulent plus vite qu'ils ne sont
  récupérés, et toute opération exclusive finira par caler derrière eux.

`lastMaintenanceTime` n'est mis à jour que lorsqu'un prune a **réussi**, si bien qu'un
contrôle de péremption qui s'appuie dessus continue de sonner à travers les échecs répétés au
lieu d'être remis à zéro par eux.

Pourquoi une exécution de maintenance a échoué :

```bash
kubectl get backuprepository <name> \
  -o jsonpath='{range .status.recentMaintenance[*]}{.operation}{"\t"}{.startTime}{"\t"}{.result}{"\t"}{.message}{"\n"}{end}'
```

Le Job et son pod sont supprimés dès que l'opération se termine, si bien que cet historique
plafonné et trié du plus récent au plus ancien est la seule trace durable de ce qui s'est
exécuté et de la raison de son échec.

### La seconde copie suit-elle ?

```bash
kubectl get clusterbackupexternalsync,backupexternalsync -A
```

Un `lagSnapshots` qui croît d'exécution en exécution signifie que la synchronisation prend du
retard. Zéro signifie que chaque snapshot de la source a une copie — et non que ces copies
sont lisibles. Voir [External sync](/CrystalBackup/fr/docs/guides/external-sync/).

### Un hook a-t-il laissé quelque chose gelé ?

```bash
kubectl -n <ns> get backup <run> -o jsonpath='{.status.postHookAttempts}{"\n"}'
```

Un compteur qui grimpe signifie qu'un hook de relâchement échoue toujours et qu'une
application peut être restée quiescée. C'est celui sur lequel il faut paginer.

## Les Events

L'operator émet des Events pour les transitions qui appellent un humain : confirmation
requise, confirmation acceptée, volume ignoré, échec de hook.

```bash
kubectl -n <ns> get events --field-selector involvedObject.kind=Restore
kubectl get events -A --field-selector reason=ConfirmationRequired
```

## Le tracing

L'operator honore les variables d'environnement standard `OTEL_*`. Renseignez-les via
`extraArgs` ou l'environnement du deployment si vous avez un collecteur.

## Voir aussi

- [Métriques](/CrystalBackup/fr/docs/reference/metrics/)
- [Alertes](/CrystalBackup/fr/docs/reference/alerts/)
- [Diagnostic](/CrystalBackup/fr/docs/operations/troubleshooting/)
