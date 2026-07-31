// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import { LOCALES } from './src/i18n/locales.mjs';

// GitHub Pages project site served at https://crystalbackup.github.io/CrystalBackup/
// If a custom domain is configured later, set `site` to it and set `base` to '/'.
//
// Starlight is mounted UNDER /docs rather than at the root: `src/pages/index.astro`
// (the landing page) and `src/pages/quality.astro` keep their routes, and the docs
// routes are pushed under a prefix by the custom `generateId` in src/content.config.ts.
// The prefix lives in the entry IDS, so every `slug:` below carries it (`docs/…`).
// `autogenerate.directory` does NOT: Starlight matches those against the file path
// relative to the collection root, which is unprefixed. Hence `guides`, not `docs/guides`.
//
// The site is bilingual as of the i18n foundation lot. English is the `root` locale and
// is served with NO prefix, so every English URL is unchanged — that is a constraint, not
// a preference: the README and the published crucible reports deep-link into
// /CrystalBackup/docs/…, and an `/en/` prefix would 404 all of them at once. French is
// served under /fr/, and the locale segment goes in FRONT of the docs prefix
// (/fr/docs/…), because Starlight reads the locale off the first path segment.
//
// Neither `slug:` nor `autogenerate.directory` below carries `fr/`: Starlight prepends
// the locale itself when it builds a localized sidebar. What the entries DO carry is a
// `translations` map, so the French sidebar reads in French even where the page behind it
// is still an English fallback. An untranslated page reached under /fr/ is not a 404 —
// Starlight serves the English content with a "not yet available in your language"
// notice, which is the honest state and is exactly what the next lot removes page by page.
export default defineConfig({
  site: 'https://crystalbackup.github.io',
  base: '/CrystalBackup',
  integrations: [
    starlight({
      title: 'Crystal Backup',
      description:
        'Documentation for Crystal Backup — a multi-tenant Kubernetes backup and disaster-recovery operator built on plain restic repositories.',
      // Declared once in src/i18n/locales.mjs, because tools/check-translations.mjs reads
      // the same table: a staleness guard that learns the language list somewhere other
      // than the site does is a guard that can pass while covering nothing that shipped.
      defaultLocale: 'root',
      locales: LOCALES,
      logo: {
        src: './src/assets/crystal.svg',
        alt: 'Crystal Backup',
      },
      favicon: '/favicon.svg',
      customCss: ['./src/styles/docs.css'],
      // The landing page is dark-only, so the docs are too. A light theme would be a
      // second design to keep honest, and a half-maintained one reads as a bug.
      components: {
        ThemeSelect: './src/components/docs/ThemeSelect.astro',
      },
      social: [
        {
          icon: 'github',
          label: 'GitHub',
          href: 'https://github.com/CrystalBackup/CrystalBackup',
        },
      ],
      editLink: {
        baseUrl: 'https://github.com/CrystalBackup/CrystalBackup/edit/main/website/',
      },
      lastUpdated: false,
      pagination: true,
      tableOfContents: { minHeadingLevel: 2, maxHeadingLevel: 3 },
      sidebar: [
        {
          label: 'Discover',
          translations: { fr: 'Découvrir' },
          items: [
            {
              label: 'What Crystal Backup is',
              translations: { fr: 'Ce qu’est Crystal Backup' },
              slug: 'docs',
            },
            {
              label: 'The two planes',
              translations: { fr: 'Les deux plans' },
              slug: 'docs/discover/two-planes',
            },
            {
              label: 'How it compares',
              translations: { fr: 'Comparaison' },
              slug: 'docs/discover/comparison',
            },
            {
              label: 'When not to choose it',
              translations: { fr: 'Quand ne pas le choisir' },
              slug: 'docs/discover/when-not-to-use',
            },
            {
              label: 'Project status',
              translations: { fr: 'État du projet' },
              slug: 'docs/discover/status',
            },
          ],
        },
        {
          label: 'Get started',
          translations: { fr: 'Démarrer' },
          items: [
            {
              label: 'Requirements',
              translations: { fr: 'Prérequis' },
              slug: 'docs/start/requirements',
            },
            {
              label: 'Install with Helm',
              translations: { fr: 'Installer avec Helm' },
              slug: 'docs/start/install',
            },
            {
              label: 'Install with Argo CD',
              translations: { fr: 'Installer avec Argo CD' },
              slug: 'docs/start/install-argocd',
            },
            {
              label: 'Install with Flux',
              translations: { fr: 'Installer avec Flux' },
              slug: 'docs/start/install-flux',
            },
            {
              label: 'Quickstart',
              translations: { fr: 'Démarrage rapide' },
              slug: 'docs/start/quickstart',
            },
          ],
        },
        {
          label: 'Guides',
          translations: { fr: 'Guides' },
          autogenerate: { directory: 'guides' },
        },
        {
          label: 'Reference',
          translations: { fr: 'Référence' },
          items: [
            {
              label: 'API reference',
              translations: { fr: 'Référence de l’API' },
              slug: 'docs/reference/api',
            },
            {
              label: 'Helm values',
              translations: { fr: 'Valeurs Helm' },
              slug: 'docs/reference/helm-values',
            },
            {
              label: 'Command-line tools',
              translations: { fr: 'Outils en ligne de commande' },
              slug: 'docs/reference/mover-cli',
            },
            {
              label: 'Labels and annotations',
              translations: { fr: 'Labels et annotations' },
              slug: 'docs/reference/labels',
            },
            {
              label: 'Admission rules',
              translations: { fr: 'Règles d’admission' },
              slug: 'docs/reference/admission',
            },
            { label: 'Metrics', translations: { fr: 'Métriques' }, slug: 'docs/reference/metrics' },
            { label: 'Alerts', translations: { fr: 'Alertes' }, slug: 'docs/reference/alerts' },
          ],
        },
        {
          label: 'Understand',
          translations: { fr: 'Comprendre' },
          autogenerate: { directory: 'understand' },
        },
        {
          label: 'Operations',
          translations: { fr: 'Exploitation' },
          autogenerate: { directory: 'operations' },
        },
      ],
    }),
  ],
});
