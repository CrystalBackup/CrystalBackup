# Slides — « Chaque namespace mérite ses backups »

Deck [Slidev](https://sli.dev) pour le meetup 45 min (30 min slides + 12 min démo).
L'identité visuelle reprend celle du site (fond `#060912`, dégradé `#3fc8f5 → #2b63f0`,
panneaux verre, chips de cascade) — voir [style.css](style.css).

## Utilisation

```bash
npm install        # une fois
npm run dev        # présente sur http://localhost:3030 (mode présentateur : /presenter)
npm run export     # exporte slides-export.pdf (avec les étapes d'animation)
npm run build      # site statique dans dist/ (pour héberger le deck)
```

Export PowerPoint si un organisateur l'exige :

```bash
npx slidev export slides.md --format pptx
```

## Mode présentateur

`npm run dev` puis ouvrir `http://localhost:3030/presenter` sur l'écran du laptop
(les **notes contiennent le minutage cumulé** `[mm:ss → mm:ss]` de chaque slide)
et `http://localhost:3030/presenter/print` pour imprimer les notes en secours.

## Repères de coupe (si le temps file)

- **~1 min à gagner** : la slide « Huit étapes » se résume à ses deux encarts en 30 s.
- **~1 min** : sauter la war story n°3 (« le droit à l'oubli qui n'oubliait rien ») —
  elle est résumée en une phrase sur la slide « échelle de la confiance ».
- **Version 35 min (KCD)** : voir [../conferencehall.md](../conferencehall.md) § Variantes.

## Notes de forme

- Polices Inter / JetBrains Mono chargées via Google Fonts : présenter **une première
  fois avec réseau** pour remplir le cache, sinon le fallback système reste correct
  (le site du projet, lui, ne charge aucune webfont : il déclare Inter avec fallback
  système — l'effet est le même).
- Les « memes » (Drake, galaxy brain) sont **redessinés en CSS** — aucune image sous
  copyright dans le deck.
- Placeholders à personnaliser avant chaque événement : slide `whoami`, pied de la
  slide de couverture (nom du meetup), contact final.
