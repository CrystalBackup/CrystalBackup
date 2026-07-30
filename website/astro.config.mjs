// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

// GitHub Pages project site served at https://crystalbackup.github.io/CrystalBackup/
// If a custom domain is configured later, set `site` to it and set `base` to '/'.
//
// Starlight is mounted UNDER /docs rather than at the root: `src/pages/index.astro`
// (the landing page) and `src/pages/quality.astro` keep their routes, and the docs
// routes are pushed under a prefix by the custom `generateId` in src/content.config.ts.
// The prefix lives in the entry IDS, so every `slug:` below carries it (`docs/…`).
// `autogenerate.directory` does NOT: Starlight matches those against the file path
// relative to the collection root, which is unprefixed. Hence `guides`, not `docs/guides`.
export default defineConfig({
  site: 'https://crystalbackup.github.io',
  base: '/CrystalBackup',
  integrations: [
    starlight({
      title: 'Crystal Backup',
      description:
        'Documentation for Crystal Backup — a multi-tenant Kubernetes backup and disaster-recovery operator built on plain restic repositories.',
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
          items: [
            { label: 'What Crystal Backup is', slug: 'docs' },
            { label: 'The two planes', slug: 'docs/discover/two-planes' },
            { label: 'How it compares', slug: 'docs/discover/comparison' },
            { label: 'When not to choose it', slug: 'docs/discover/when-not-to-use' },
            { label: 'Project status', slug: 'docs/discover/status' },
          ],
        },
        {
          label: 'Get started',
          items: [
            { label: 'Requirements', slug: 'docs/start/requirements' },
            { label: 'Install with Helm', slug: 'docs/start/install' },
            { label: 'Quickstart', slug: 'docs/start/quickstart' },
          ],
        },
        {
          label: 'Guides',
          autogenerate: { directory: 'guides' },
        },
        {
          label: 'Reference',
          items: [
            { label: 'API reference', slug: 'docs/reference/api' },
            { label: 'Helm values', slug: 'docs/reference/helm-values' },
            { label: 'Command-line tools', slug: 'docs/reference/mover-cli' },
            { label: 'Labels and annotations', slug: 'docs/reference/labels' },
            { label: 'Admission rules', slug: 'docs/reference/admission' },
            { label: 'Metrics', slug: 'docs/reference/metrics' },
            { label: 'Alerts', slug: 'docs/reference/alerts' },
          ],
        },
        {
          label: 'Understand',
          autogenerate: { directory: 'understand' },
        },
        {
          label: 'Operations',
          autogenerate: { directory: 'operations' },
        },
      ],
    }),
  ],
});
