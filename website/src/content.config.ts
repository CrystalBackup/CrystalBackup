import { defineCollection } from 'astro:content';
import { docsLoader } from '@astrojs/starlight/loaders';
import { docsSchema } from '@astrojs/starlight/schema';

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
 * Consequences to remember when editing:
 *   - sidebar `autogenerate.directory` values are ids, so they carry the prefix
 *     too (`docs/guides`, not `guides`) — see astro.config.mjs;
 *   - a page that sets `slug:` in its frontmatter opts OUT of the prefix and must
 *     therefore spell it out itself.
 */
const stripExtension = (entry: string) =>
	entry.replace(/\.(md|mdx|mdoc|markdown|mdown|mkdn|mkd|mdwn)$/i, '');

export const collections = {
	docs: defineCollection({
		loader: docsLoader({
			generateId: ({ entry, data }) => {
				if (typeof data.slug === 'string' && data.slug.length > 0) return data.slug;
				const id = stripExtension(entry)
					.split('/')
					.filter(Boolean)
					.join('/')
					.replace(/(^|\/)index$/, '');
				return id ? `docs/${id}` : 'docs';
			},
		}),
		schema: docsSchema(),
	}),
};
