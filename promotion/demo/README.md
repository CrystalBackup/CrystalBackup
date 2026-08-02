# Démo meetup — « backup, drame, restauration, preuve »

Démo live de **~12 minutes**, 100 % locale (kind + SeaweedFS S3 in-cluster), **zéro
dépendance au réseau de la salle**. Elle montre, dans l'ordre : la cascade de backup, la
destruction du namespace, la restauration depuis une coordonnée de dépôt, la lecture du
dépôt avec le **restic upstream**, et deux tentatives de triche refusées par l'admission.

> Le déroulé est volontairement la version « conditions réelles » du
> [quickstart du site](../../website/src/content/docs/start/quickstart.md) — le dérouler
> en répétition, c'est aussi vérifier la doc.

## Prérequis (sur le laptop)

Les outils de la démo sont gérés par **mise** ([mise.toml](mise.toml), même pattern que
`test/crucible/`) : `kind`, `kubectl`, `helm`, `age` (fournit `age-keygen`), `restic`.

```bash
mise install
```

Restent **système** (hors mise) : `docker` (Desktop/colima), `make`, `git`.

Machine : 8 Go de RAM libres pour le cluster kind 3 nœuds. K8s ≥ 1.30 (VAP GA) — le
node-image kind par défaut convient.

## La veille (avec réseau) — ~15 min

```bash
mise run prep         # cluster + infra + opérateur + secrets + location + appli témoin
```

```bash
mise run demo         # RÉPÉTER UNE FOIS EN ENTIER (et enregistrer le plan B, cf. plus bas)
```

```bash
mise run reset        # remise à zéro pour le jour J
```

(`mise run` met les outils épinglés sur le PATH même sans activation shell de mise ;
les scripts `00-prep.sh` / `10-demo.sh` / `99-reset.sh` restent appelables directement
si mise est activé dans le shell.)

`00-prep.sh` réutilise l'outillage e2e du dépôt (`make setup-test-e2e`,
`make install-test-e2e-infra`) et installe l'opérateur **depuis le chart publié**
(`oci://ghcr.io/crystalbackup/charts/crystal-backup`, images épinglées par digest).
La KEK est générée localement dans `.kek/` (gitignoré).

## Le jour J — runbook minuté

Lancer `mise run demo` : chaque commande s'affiche, **Entrée** l'exécute. Les phrases-clés
à dire sont imprimées par le script lui-même sous chaque résultat.

| # | Temps | Étape | Ce qu'on voit / ce qu'on dit |
|---|---|---|---|
| 1 | 0:00–1:30 | État des lieux | L'opérateur, `ClusterBackupLocation` **Ready**, le `BackupRepository` avec l'URL `s3:…/meetup-demo`, le canari du tenant. |
| 2 | 1:30–4:30 | Backup maintenant | `ClusterBackup` horodaté → le `Backup` enfant apparaît **dans** le namespace ; détail par volume : `data` **Completed**, `legacy` **Skipped/CSISnapshotUnsupported** — « jamais un drop silencieux ». *NB : les runs des répétitions apparaissent aussi (la discovery re-projette tout ce que le dépôt contient) — c'est un talking point gratuit, pas un bug.* |
| 3 | 4:30–5:30 | Le drame | `kubectl delete namespace demo`. Silence. « Il reste… le bucket. » |
| 4 | 5:30–8:30 | ClusterRestore | Cible une **coordonnée de dépôt** (location+namespace+run), `createNamespace: true`. Honnêteté : les volumes reviennent, les manifests de workload par re-apply (documenté dans RESTORE.md). Checksum : ✓. |
| 5 | 8:30–10:30 | restic upstream | Déballage de la DEK (age + KEK), `restic snapshots`, `restic dump … \| sha256sum` — **aucun composant Crystal Backup dans la commande**. |
| 6 | 10:30–12:00 | La triche | `Backup` forgé vers la location cluster → **refusé** (VAP R2, message à l'écran) ; `confirmation` fausse → **refusé**. « Le champ pour tricher n'existe pas. » |

Pauses naturelles pour les questions : après l'étape 4 (le checksum) et à la fin.

## Ce que la démo ne montre PAS (et qu'on assume si on vous le demande)

- Le **plan namespace** (bucket du tenant, sa clé) — même mécanique, seconde `BackupLocation` ;
  coupé pour tenir 12 min. Le montrer en bonus si le talk est en avance : voir
  `website/…/guides/namespace-plane.md`.
- Les snapshots **Ceph** least-data-movement — le laptop utilise csi-hostpath ; le chemin
  Ceph est exercé par le crucible (rapports publiés).
- `mode: Immutable` (M8), `crystalctl` (M7) — pas livrés, c'est dans les slides.

## Plan B (le démon de la démo existe)

1. **Le cast existe déjà** (`plan-b.cast`, gitignoré) — enregistré sur un vrai run :
   ~1 min 40 en mode auto, les 6 actes, les deux refus VAP en toutes lettres, le même
   sha256 du seed au `restic dump`. Le ré-enregistrer après un changement :

   ```bash
   DEMO_AUTO=1 mise x 'aqua:asciinema/asciinema@latest' -- \
     asciinema rec --overwrite -t "Crystal Backup — démo meetup (auto)" -c "./10-demo.sh" plan-b.cast
   ```

   Rejouer en salle : `asciinema play -i 2 plan-b.cast` (`-i` borne les silences).
2. **Panne partielle** : chaque étape du script est indépendante — sauter une étape qui
   coince et continuer (le runbook ci-dessus indique quoi dire).
3. **Panne de cluster** : `kind delete cluster --name crystal-demo && mise run prep`
   prend ~15 min — trop long en live ; basculer sur le replay et le dire franchement :
   c'est un talk sur l'honnêteté opérationnelle.

## Fichiers

| Fichier | Rôle |
|---|---|
| `mise.toml` | Outils épinglés (kind, kubectl, helm, age, restic) + tâches `prep`/`demo`/`reset` |
| `00-prep.sh` | Pré-bake complet (la veille, avec réseau) |
| `10-demo.sh` | Le déroulé interactif (jour J, offline) — `DEMO_AUTO=1` pour l'enregistrement |
| `99-reset.sh` | Remise à zéro entre répétitions (garde cluster + opérateur + location) |
| `manifests/00-seed-app.yaml` | Appli témoin : canari + `MANIFEST.sha256`, PVC snapshotable + PVC `Skipped` |
| `manifests/01-location.yaml` | `ClusterBackupLocation` → SeaweedFS in-cluster, discovery 1 min |
| `manifests/02-schedule.yaml` | Le cron de décor (`dr-daily`) |
| `manifests/03-backup-now.template.yaml` | Le run direct (nom horodaté par le script) |
| `manifests/04-clusterrestore.template.yaml` | La restauration DR depuis la coordonnée de dépôt |
| `manifests/05-forged-backup.yaml` | Triche n°1 — refusée par la VAP `backup-user-isolation` |
| `manifests/06-restore-bad-confirmation.yaml` | Triche n°2 — refusée par la VAP R23 |

## Nettoyage final

```bash
kind delete cluster --name crystal-demo
```
