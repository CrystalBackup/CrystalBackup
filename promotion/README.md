# Promotion — Crystal Backup en meetup

Le kit complet pour présenter Crystal Backup en **45 minutes** (meetup, KCD, conf) :
soumission CFP, deck, démo live. En français, réutilisable, et fidèle au ton du projet —
**appétent mais sans bullshit** : chaque affirmation des slides est adossée au code, aux
specs ou aux rapports d'acceptation publiés.

## Contenu

| Dossier / fichier | Quoi |
|---|---|
| [conferencehall.md](conferencehall.md) | Sujet + abstract + description pour ConferenceHall (et variantes 35 min / lightning / titres alternatifs) |
| [slides/](slides/README.md) | Deck **Slidev** (~33 slides), identité visuelle du site, notes orateur **minutées**, export PDF/PPTX |
| [demo/](demo/README.md) | Démo live ~12 min sur kind (offline le jour J), scripts + manifests + runbook + plan B |

## Le format 45 minutes

| Temps | Acte | Contenu |
|---|---|---|
| 0:00 – 8:00 | **Le problème** | Le cluster est sauvegardé, pas ses habitants ; les 3 frustrations tenant ; le trou dans l'existant |
| 8:00 – 23:30 | **La solution** | Deux plans, la cascade, least-data-movement, le dépôt = source de vérité, réversibilité, isolation, clés, RGPD, coexistence |
| 23:30 – 32:30 | **La preuve** | Le crucible (~1 € les 2 h), l'oracle restic indépendant, 4 war stories, l'IA assumée |
| 32:30 – 43:00 | **La démo** | backup → `delete namespace` → restore → checksum → restic upstream → triches refusées |
| 43:00 – 45:00 | **L'atterrissage** | Ce qui n'existe pas encore (M7/M8/M9), CTA sandbox, questions |

Repères de coupe et version 35 min : [slides/README.md](slides/README.md) et
[conferencehall.md](conferencehall.md) § Variantes.

## Démarrage rapide

```bash
# Le deck
cd promotion/slides && npm install && npm run dev

# La démo (la veille, avec réseau) — outils gérés par mise (cf. demo/mise.toml)
cd promotion/demo && mise install && mise run prep && mise run demo && mise run reset
```

## Checklist jour J

- [ ] Répétition complète faite (slides chronométrées + démo déroulée **deux fois**)
- [ ] `kubectl config use-context kind-crystal-demo` — le cluster de démo répond
- [ ] `mise run reset` passé après la dernière répétition (canari frais, pod Ready)
- [ ] Plan B enregistré (`plan-b.cast`) et testé avec `asciinema play`
- [ ] Slides ouvertes en mode présentateur (`/presenter` — les notes portent le minutage)
- [ ] Export PDF de secours du deck sur le bureau (`npm run export`)
- [ ] Terminal : police ≥ 18 pt, thème sombre, prompt court, notifications coupées
- [ ] Mode avion Slack/mail ; « Ne pas déranger » activé
- [ ] Slide `whoami` et pied de couverture personnalisés pour l'événement
- [ ] Micro-cravate ou micro à main testé — et une bouteille d'eau

## Maintenance à chaque release ⚠️

Ce dossier porte des faits **datés à la version** — une release qui met à jour le site
doit aussi repasser ici. Les points de version à contrôler :

| Fichier | Quoi vérifier |
|---|---|
| `slides/slides.md` | Pill de couverture (`v0.6.4 · M0–M6 livrés`), disclaimer `whoami`, note du tableau comparatif (« v0.6.4 livrée »), slide « Ce que vous n'avez PAS vu » (M7/M8/M9 → recaler quand un milestone sort **ou quand la roadmap est re-arbitrée**, cf. 2026-08-02), le bloc `kubectl create namespace` + labels PSA + `--version 0.6.4` du `helm install` (CTA — **le miroir de `website/…/start/install.md`, y compris l'absence de `--create-namespace` : `namespace.create` vaut `false` par défaut depuis 0.6.3**), le compte « 90 checks » **et la durée** sur la slide crucible, le nom du rapport lié (`crucible-m6-4.html`) et « 20 ADRs » (compter avec `ls spec/adr/`, ne pas croire la slide). **Chercher aussi les mentions de milestone dans le CORPS des slides** : la re-arbitration a recalé les panneaux « PAS vu » mais laissé « Object Lock, M8 » dans la slide effacement, alors que l'immutabilité est passée en M9 — un `grep -n 'M[789]'` sur tout le fichier, pas seulement sur la slide de fin |
| `conferencehall.md` | Mentions de version, « M0–M6 », « 90 checks » dans l'abstract et la description (au 0.6.4 : aucun numéro de version n'y figure, seul « 90 checks » est daté) |
| `demo/00-prep.sh` | Le commentaire « releases ≤ 0.6.1 » sur l'extraction de la KEK — **le fix `age.ParseIdentities` est embarqué à partir de 0.6.2**, donc le `grep` y est facultatif sur la release installée et le commentaire le dit (y recaler le numéro de release courant) ; `CHART_VERSION` (vide = dernière release publiée) si on veut épingler |
| `demo/README.md` + runbook | Re-dérouler `mise run prep && mise run demo` sur la nouvelle release (c'est aussi le quickstart en conditions réelles) et ré-enregistrer `plan-b.cast` |
| `README.md` (ce fichier) | Le tableau de timing si le contenu bouge |

Les **war stories** des slides sont historiques (versions et dates citées) : elles ne se
périment pas, ne pas les « corriger ».

## Principes éditoriaux (pour les futures adaptations)

- **Aucune claim sans source** : tout vient du README, des specs (`spec/`), du CHANGELOG
  ou des rapports crucible publiés. En cas de doute, la source prime sur la slide.
- **Les war stories sont des atouts, pas des aveux honteux** — elles sont déjà publiques
  dans le CHANGELOG ; le talk ne révèle rien, il met en scène l'honnêteté existante.
- **Dire ce qui n'existe pas** (M7/M8/M9, v1alpha1, « pas pour la prod ») fait partie du
  pitch : c'est la slide qui rend toutes les autres crédibles.
- **Memes** : redessinés en CSS (Drake, galaxy brain) — pas d'images sous copyright, et
  pas plus de trois par deck.
- La comparaison outils reste **non-benchmark** : capacités des autres à vérifier contre
  leurs docs, mention explicite sur la slide.
