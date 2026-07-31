import { defineCollection, z } from 'astro:content';
import { docsLoader } from '@astrojs/starlight/loaders';
import { docsSchema } from '@astrojs/starlight/schema';
import { TRANSLATED_LOCALES } from './i18n/locales.mjs';

/**
 * Starlight normally owns the site root. Here it does not: `/` is the hand-built
 * landing page and `/quality/` is its own Astro page, both of which predate the
 * documentation and must keep working untouched.
 *
 * So the docs collection is loaded with a custom `generateId` that prefixes every
 * entry with `docs/`. The FILES stay where they read naturally
 * (`src/content/docs/guides/…`) while the ROUTES all land under `/docs/…`, which
 * is what keeps them from colliding with `src/pages/`.
 *
 * i18n adds a second prefix, and the ORDER of the two is not free. Starlight reads
 * the locale off the FIRST segment of an entry id (`slugToLocale` splits on `/` and
 * looks the head segment up in `locales`), so a French page must be `fr/docs/…` and
 * never `docs/fr/…` — the latter is an English page that happens to live in a
 * directory called `fr`. The files still read naturally, at
 * `src/content/docs/fr/guides/…`; only the id is reordered:
 *
 *   src/content/docs/start/quickstart.md     → `docs/start/quickstart`    → /docs/start/quickstart/
 *   src/content/docs/fr/start/quickstart.md  → `fr/docs/start/quickstart` → /fr/docs/start/quickstart/
 *   src/content/docs/index.md                → `docs`                     → /docs/
 *   src/content/docs/fr/index.md             → `fr/docs`                  → /fr/docs/
 *
 * English ids come out byte-for-byte what they were before French existed, which is
 * the point: adding a language moved no English URL.
 *
 * Consequences to remember when editing:
 *   - sidebar `slug:` values are ids, so they carry the `docs/` prefix — see
 *     astro.config.mjs. Starlight prepends the locale itself when it builds the
 *     French sidebar, so they must NOT carry `fr/`;
 *   - sidebar `autogenerate.directory` values are file paths relative to the
 *     collection root, which carry neither prefix (`guides`), and Starlight likewise
 *     prepends the locale itself;
 *   - a page that sets `slug:` in its frontmatter opts OUT of both prefixes and must
 *     spell out whatever it wants, locale included.
 */
const stripExtension = (entry: string) =>
	entry.replace(/\.(md|mdx|mdoc|markdown|mdown|mkdn|mkd|mdwn)$/i, '');

const localeDirectories: string[] = TRANSLATED_LOCALES;

export const collections = {
	docs: defineCollection({
		loader: docsLoader({
			generateId: ({ entry, data }) => {
				if (typeof data.slug === 'string' && data.slug.length > 0) return data.slug;
				const path = stripExtension(entry)
					.split('/')
					.filter(Boolean)
					.join('/')
					.replace(/(^|\/)index$/, '');
				const [head, ...rest] = path.split('/');
				if (head && localeDirectories.includes(head)) {
					const tail = rest.join('/');
					return tail ? `${head}/docs/${tail}` : `${head}/docs`;
				}
				return path ? `docs/${path}` : 'docs';
			},
		}),
		/**
		 * `sourceFile` and `sourceHash` are the translation-staleness contract, declared
		 * here so that a mistyped key is a build error rather than a page the guard
		 * quietly treats as having nothing to check.
		 *
		 * `sourceHash` is the git blob hash of the English file the page was translated
		 * from — literally `git hash-object <sourceFile>`. It changes if and only if the
		 * English bytes change, which is exactly the event that turns a translation into
		 * a confident lie. tools/check-translations.mjs recomputes it in CI.
		 *
		 * Both are optional in the schema because English pages have neither. It is the
		 * guard, not the schema, that makes them mandatory on a page under a locale
		 * directory, and it names the file and the missing key when they are absent.
		 */
		schema: docsSchema({
			extend: z.object({
				sourceFile: z.string().optional(),
				sourceHash: z
					.string()
					.regex(/^[0-9a-f]{40}$/, 'sourceHash must be a 40-character git blob hash')
					.optional(),
			}),
		}),
	}),
};
