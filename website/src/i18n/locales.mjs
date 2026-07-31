/**
 * The one place the site's languages are declared.
 *
 * Three consumers read this file and nothing restates it:
 *   - astro.config.mjs, for Starlight's `locales` map (the language switcher, the
 *     `lang`/`hreflang` attributes and the sitemap's alternates all come from there);
 *   - src/content.config.ts, to know which top-level directories under
 *     src/content/docs/ are a LANGUAGE rather than a section (see the id-shape note
 *     below, it is not the usual Starlight layout);
 *   - tools/check-translations.mjs, so the staleness guard fails on a locale that is
 *     served but has no translated page at all, instead of quietly checking nothing.
 *
 * That last one is why this is a module and not three copies of a string. A guard
 * that learns the locale list from a different place than the site does is a guard
 * that can pass while covering none of what shipped.
 *
 * `root` is English and is served WITHOUT a prefix: every English URL that exists
 * today keeps the exact path it has (`/CrystalBackup/docs/…`). That is a hard
 * constraint — the README, the published crucible reports and external links point
 * into those paths, and moving them to `/en/…` would 404 all of them.
 */

/** Starlight `locales` map. `root` is the unprefixed default locale. */
export const LOCALES = {
	root: { label: 'English', lang: 'en' },
	fr: { label: 'Français', lang: 'fr' },
};

/**
 * Directory names under src/content/docs/ that hold a translation.
 *
 * Everything else in that directory is English source. A new language is one entry
 * in LOCALES plus a directory of the same name; nothing else needs editing.
 */
export const TRANSLATED_LOCALES = Object.keys(LOCALES).filter((locale) => locale !== 'root');
