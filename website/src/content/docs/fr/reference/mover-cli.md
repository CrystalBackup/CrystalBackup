---
title: Outils en ligne de commande
description: L'état de crystalctl, l'entrypoint du container crystal-mover, et comment piloter un repository avec restic upstream aujourd'hui.
sourceFile: src/content/docs/reference/mover-cli.md
sourceHash: b071c2a15c7db00ee39c3f73b2556fb940967fd5
---

## Il n'y a pas de CLI destinée aux utilisateurs dans cette release

`crystalctl` — le binaire autonome qui listera, parcourra, extraira et exportera depuis un
repository sans aucune dépendance à Kubernetes, plus des helpers façon kubectl — est spécifié
et **non implémenté**. Il n'y a pas de `cmd/crystalctl` dans l'arbre, et rien à télécharger.

Il ne sortira pas non plus de ce dépôt : la CLI devient un plugin `kubectl` dans son propre
dépôt, distribué via krew, et l'UI de navigation devient un projet à part. C'est une décision
d'empaquetage, pas un recul — la spécification est inchangée, et la contrainte qui l'accompagne
est qu'**aucune capacité ne sera jamais accessible uniquement par la CLI**. Tout reste
exprimable en custom resource, et tout reste lisible avec `restic` upstream.

Cette page est là pour que ce soit sans ambiguïté, et pour que vous sachiez quoi utiliser à
la place.

## Quoi utiliser à la place : restic upstream

Ce n'est pas un contournement. Lire un repository Crystal Backup avec `restic` upstream est
la garantie de réversibilité autour de laquelle le projet est construit, et cela coûte deux
variables d'environnement.

### Obtenir le mot de passe

**Un repository du plan namespace** — le mot de passe est le vôtre :

```bash
export RESTIC_PASSWORD=$(kubectl -n team-x get secret offsite-key \
  -o jsonpath='{.data.password}' | base64 -d)
```

Si c'est l'operator qui l'a généré, le Secret est `crystal-repo-password-<location>` dans
votre namespace.

**Un repository du plan cluster** — la clé est wrappée sous la KEK age, il faut donc la
déwrapper :

```bash
kubectl -n crystal-backup-system get secret cluster-kek \
  -o jsonpath='{.data.identity}' | base64 -d > /tmp/kek.txt

export RESTIC_PASSWORD=$(kubectl -n crystal-backup-system get secret crystal-dek-<location> \
  -o jsonpath='{.data.dek}' | base64 -d | age -d -i /tmp/kek.txt)
```

Supprimez `/tmp/kek.txt` ensuite. Sur un cluster reconstruit, vous déwrappez depuis votre
séquestre hors bande à la place, et la clé wrappée vient du bucket, à
`<prefix>/<clusterID>.crystal-meta/wrapped-dek.age`.

### L'URL du repository

```
s3:<endpoint>/<bucket>/<prefix>/<clusterID>
```

Un prefix vide fait sauter ce segment. L'operator publie la chaîne exacte :

```bash
kubectl get backuprepository <name> -o jsonpath='{.status.repositoryURL}{"\n"}'
```

### Ce qu'il vaut la peine de savoir faire

```bash
export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
R=s3:https://s3.example.com/crystal-backups/dr/prod-eu-1

# What is in there.
restic -r $R snapshots --tag crystalbackup

# One namespace only. Comma-joined tags in ONE --tag flag are ANDed;
# repeating --tag would OR them.
restic -r $R snapshots --tag crystalbackup,namespace=team-x

# One run.
restic -r $R snapshots --tag crystalbackup,namespace=team-x,run=dr-daily-20260730-020000

# Browse a snapshot's tree.
restic -r $R ls <snapshot-id>

# Pull one file out, without restoring anything into the cluster.
restic -r $R dump <snapshot-id> /data/team-x/uploads/images/2026/photo.jpg > photo.jpg

# Restore to local disk.
restic -r $R restore <snapshot-id> --target ./recovered

# Verify the repository.
restic -r $R check --read-data-subset 1%
```

Rien dans cette liste ne fait intervenir un composant de Crystal Backup. C'est tout l'intérêt.

:::caution[N'écrivez pas à la main dans un repository vivant]
`forget`, `prune` et `unlock` prennent des locks exclusifs contre lesquels la queue de
l'operator est conçue pour sérialiser. Les lancer vous-même, hors bande, sort de l'hypothèse
d'écrivain unique sur laquelle la queue est bâtie et peut entrer en collision avec un mover
en vol. Les lectures sont sans danger. Pour les écritures, utilisez `ClusterErasure` ou le
schedule de `maintenance` de la location.
:::

## `crystal-mover`

`crystal-mover` est l'**entrypoint du container des Jobs de mover**, pas un outil que vous
lancez. Il est documenté ici parce que vous le verrez dans les specs de Job et les logs de
pod pendant un diagnostic, et parce que connaître sa forme rend ces logs lisibles.

C'est une fine couche autour de restic. Il prend deux flags et transmet tout ce qui suit `--`
tel quel :

```
crystal-mover --operation <op> -- <restic argv...>
```

| Flag | Défaut | Signification |
|---|---|---|
| `--operation` | *(requis)* | Quelle opération est ce Job. |
| `--termination-log` | `/dev/termination-log` | Où le JSON de résultat est écrit. |

Opérations acceptées :

| Valeur | Ce que fait le Job |
|---|---|
| `backup` | backuper une PVC |
| `restore` | restaurer une PVC |
| `init` | initialiser le repository (idempotent — un échec « already initialized » est traité comme un succès) |
| `forget` | appliquer la rétention |
| `prune` | récupérer de l'espace |
| `check` | vérifier le repository |
| `snapshots` | inventorier le repository pour la discovery |
| `unlock` | lever les locks périmés |
| `sync` | `restic copy` pour la sync externe |
| `manifests-backup` | dumper et uploader les manifests d'un namespace |
| `manifests-restore` | restaurer les manifests d'un namespace |
| `cluster-manifests-backup` | capturer les ressources cluster-scoped |
| `cluster-manifests-restore` | restaurer les ressources cluster-scoped |

Deux propriétés bonnes à connaître pendant un diagnostic :

- **Les secrets n'apparaissent jamais dans argv.** L'URL du repository, le chemin du fichier
  de mot de passe et les credentials S3 arrivent comme variables d'environnement depuis un
  mount de Secret en lecture seule. Une spec de Job que vous lisez avec
  `kubectl get job -o yaml` ne contient donc aucune matière de clé.
- **Le résultat est un JSON sur le message de terminaison**, pas seulement le chemin de
  sortie. Si un pod de mover a disparu avant que vous ayez pu lire ses logs, la trace durable
  est dans le status de l'objet propriétaire — `status.volumes[]` sur un `Backup`,
  `status.recentMaintenance[]` sur un `BackupRepository`.

```bash
kubectl -n crystal-backup-system get jobs -l app.kubernetes.io/managed-by=crystal-backup
kubectl -n crystal-backup-system logs job/<name>
```

Les Jobs de mover ont des **noms déterministes** — une fonction pure de ce qu'ils font,
jamais aléatoire — si bien qu'un operator redémarré ré-adopte un Job en cours plutôt que d'en
démarrer un second.

## Ce que sera `crystalctl`

Pour être complet, d'après la spécification. Rien de tout cela n'existe encore.

- Un binaire statique autonome pour linux, windows et darwin en amd64 et arm64, **sans
  dépendance à Kubernetes** — il ouvre un repository à partir de credentials S3 et d'une clé.
- `list`, `browse`, `dump`, `export tar`, et un restore local.
- Des helpers façon kubectl quand une kubeconfig est présente : déclencher un backup,
  surveiller le status.
- Des wrappers d'administration au-dessus de l'erasure et du décommissionnement de
  repository.
- Une UI de navigation locale, en sous-commande.

Suivez-le sur la
[roadmap](https://github.com/CrystalBackup/CrystalBackup/blob/main/spec/90-roadmap.md).
