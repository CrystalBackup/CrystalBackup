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
| 23:30 – 31:30 | **La preuve** | Le crucible (~1 € les 2 h), l'oracle restic indépendant, 3 war stories, l'IA assumée |
| 31:30 – 43:00 | **La démo** | backup → `delete namespace` → restore → checksum → restic upstream → triches refusées |
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
