#!/usr/bin/env node
/**
 * The translation-staleness guard.
 *
 * WHY THIS EXISTS AT ALL
 *
 * A translated page that has silently fallen behind its English source is worse than
 * no translation: a reader trusts a stale page exactly as much as a fresh one, and
 * nothing in a normal review catches it — the French file is untouched, so it does
 * not appear in the diff that broke it. The damage is one-directional and invisible.
 * On a documentation site whose subject is losing data, a French page still promising
 * a field that was renamed two releases ago is a real incident waiting for a reader.
 *
 * So the contract is mechanical rather than cultural. Every translated page records,
 * in its frontmatter, the git blob hash of the exact English bytes it was translated
 * from:
 *
 *     sourceFile: src/content/docs/start/quickstart.md
 *     sourceHash: 3f1a…            # git hash-object src/content/docs/start/quickstart.md
 *
 * A blob hash changes if and only if the file's bytes change. Editing the English page
 * therefore breaks every translation of it, loudly, in the same pull request that made
 * the edit — which is the only moment at which somebody knows what changed and why.
 *
 * WHAT IT REFUSES TO DO
 *
 * It refuses to pass by finding nothing. This project has been bitten repeatedly by an
 * absence reading as health; `make check-alert-rules` once opened with
 * `command -v promtool || exit 0`, so on every CI runner it ever ran on, it verified
 * nothing and said so in green. Five of nine shipped alert rules were unable to fire.
 * Every exit path below that could be reached by "there was nothing to look at" is
 * therefore a FAILURE:
 *
 *   - no locale declared in src/i18n/locales.mjs                          → fail
 *   - a declared locale with zero pages under its directory               → fail
 *   - zero English pages found                                            → fail
 *   - zero translated pages found overall                                 → fail
 *   - git not available, or `git hash-object` failing                     → fail (never skip)
 *
 * COVERAGE: WARNING BY DESIGN, AND WHY
 *
 * An English page with no French counterpart is reported as a WARNING by default, not
 * an error. That is a deliberate choice for one narrow reason: the site has ~36 pages
 * and exactly one of them is translated today, on purpose — this lot ships the
 * mechanism, the bulk translation is the next one. A hard failure would mean the guard
 * is red from the minute it lands, and a permanently-red check is a check people learn
 * to ignore, which is the same silence in a louder costume.
 *
 * The moment French coverage is complete, flip it: pass `--require-full-coverage`
 * (or set I18N_REQUIRE_FULL_COVERAGE=1) and the gap becomes an error. The CI job in
 * .github/workflows/lint.yml is where to add the flag. Every OTHER finding — drift, a
 * vanished source, a missing or malformed key — is an error today.
 *
 * USAGE
 *
 *   node website/tools/check-translations.mjs                  # the check
 *   node website/tools/check-translations.mjs --require-full-coverage
 *   node website/tools/check-translations.mjs --refresh        # rubber-stamp AFTER re-translating
 *   node website/tools/check-translations.mjs --self-test      # prove the guard can fail
 *
 * Stdlib only, no dependency on `npm install` — it has to run in CI without building
 * the site, and a guard that needs a working node_modules is a guard that stops running
 * the first time the lockfile breaks.
 */

import { execFileSync } from 'node:child_process';
import { mkdtempSync, mkdirSync, readdirSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, relative, resolve, sep } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const WEBSITE_ROOT = resolve(HERE, '..');
const DOCS_ROOT = join(WEBSITE_ROOT, 'src', 'content', 'docs');
const LOCALES_MODULE = join(WEBSITE_ROOT, 'src', 'i18n', 'locales.mjs');
const PAGE_EXTENSIONS = ['.md', '.mdx', '.mdoc', '.markdown'];

/** Paths are reported relative to the website directory, which is how a human types them. */
const display = (absolute) => relative(WEBSITE_ROOT, absolute).split(sep).join('/');

// ---------------------------------------------------------------------------
// git
// ---------------------------------------------------------------------------

/**
 * The blob hash of a file's CURRENT bytes on disk.
 *
 * `git hash-object` needs no repository and no index — it hashes the working-tree
 * bytes — so this also works inside the self-test's temporary fixture directory, and
 * it reports a hash for an English page that is new and not yet committed.
 */
function gitHashObject(absolutePath) {
	return execFileSync('git', ['hash-object', '--', absolutePath], {
		encoding: 'utf8',
		stdio: ['ignore', 'pipe', 'pipe'],
	}).trim();
}

/**
 * Fail, do not skip. A missing git is the exact shape of the accident this project
 * keeps having: the check that quietly decides it cannot run, and reports success.
 */
function requireGit() {
	try {
		execFileSync('git', ['--version'], { stdio: ['ignore', 'ignore', 'ignore'] });
	} catch {
		console.error('ERROR: git is not on PATH, so no source hash can be recomputed.');
		console.error('       This check verifies nothing without it, and refuses to report success.');
		process.exit(1);
	}
}

// ---------------------------------------------------------------------------
// Page discovery and frontmatter
// ---------------------------------------------------------------------------

function listPages(root) {
	const found = [];
	const walk = (dir) => {
		let entries;
		try {
			entries = readdirSync(dir, { withFileTypes: true });
		} catch {
			return;
		}
		for (const entry of entries.sort((a, b) => a.name.localeCompare(b.name))) {
			const full = join(dir, entry.name);
			if (entry.isDirectory()) walk(full);
			else if (PAGE_EXTENSIONS.some((ext) => entry.name.endsWith(ext))) found.push(full);
		}
	};
	walk(root);
	return found;
}

/**
 * The first `---`-delimited block, parsed for the two keys this guard owns and nothing
 * else. Astro already validates the whole frontmatter against the collection schema at
 * build time (src/content.config.ts declares both keys), so this deliberately does not
 * carry a YAML parser: it only has to find two scalars, and a dependency here would mean
 * this check cannot run before `npm install`.
 */
function readTranslationKeys(absolutePath) {
	const text = readFileSync(absolutePath, 'utf8');
	const match = /^---\r?\n([\s\S]*?)\r?\n---/.exec(text);
	if (!match) return { sourceFile: null, sourceHash: null, hasFrontmatter: false };
	const block = match[1];
	const scalar = (key) => {
		const found = new RegExp(`^${key}:[ \\t]*["']?([^"'\\r\\n]+)["']?[ \\t]*$`, 'm').exec(block);
		return found ? found[1].trim() : null;
	};
	return { sourceFile: scalar('sourceFile'), sourceHash: scalar('sourceHash'), hasFrontmatter: true };
}

// ---------------------------------------------------------------------------
// The check itself
// ---------------------------------------------------------------------------

/**
 * @param {object} options
 * @param {string} options.docsRoot      absolute path of the docs collection root
 * @param {string[]} options.locales     directory names holding translations
 * @param {(p: string) => string} options.pathLabel  how to render a path in a message
 * @returns {{errors: string[], warnings: string[], stats: object, drifted: object[]}}
 *
 * Pure with respect to the filesystem it is pointed at, so the self-test can run the
 * SAME function against a fixture tree. A self-test that exercises a reimplementation
 * proves nothing about what CI runs.
 */
function inspect({ docsRoot, locales, pathLabel }) {
	const errors = [];
	const warnings = [];
	const drifted = [];

	if (locales.length === 0) {
		errors.push(
			'no translated locale is declared in src/i18n/locales.mjs. Either the site is ' +
				'monolingual and this check should be removed, or the locale table was emptied ' +
				'by accident — it will not pass by having nothing to verify.'
		);
		return { errors, warnings, drifted, stats: { english: 0, translated: 0, perLocale: {} } };
	}

	const localeSet = new Set(locales);
	const allPages = listPages(docsRoot);

	/** English source pages: everything NOT under a locale directory. */
	const englishPages = [];
	/** Translated pages, grouped by locale. */
	const perLocale = Object.fromEntries(locales.map((locale) => [locale, []]));

	for (const page of allPages) {
		const relativePath = relative(docsRoot, page).split(sep).join('/');
		const head = relativePath.split('/')[0];
		if (localeSet.has(head)) perLocale[head].push(page);
		else englishPages.push(page);
	}

	// --- vacuity guards -----------------------------------------------------
	//
	// Each of these is a way for the check to "succeed" by looking at nothing.

	if (englishPages.length === 0) {
		errors.push(
			`no English page found under ${pathLabel(docsRoot)}. Either the docs collection ` +
				'moved, or this check is pointed at the wrong directory; it is not evidence ' +
				'that the translations are current.'
		);
	}

	const translatedCount = locales.reduce((n, locale) => n + perLocale[locale].length, 0);
	if (translatedCount === 0) {
		errors.push(
			`no translated page found for any of the declared locales (${locales.join(', ')}). ` +
				'A staleness check with zero pages to check verifies nothing, so it fails rather ' +
				'than reporting green.'
		);
	}

	for (const locale of locales) {
		if (perLocale[locale].length === 0 && translatedCount > 0) {
			errors.push(
				`locale "${locale}" is declared in src/i18n/locales.mjs — Starlight is serving ` +
					`/${locale}/ and the language switcher offers it — but ${pathLabel(join(docsRoot, locale))}/ ` +
					'contains no page. Every URL under it is an English fallback that nothing here ' +
					'is watching. Translate at least one page, or remove the locale.'
			);
		}
	}

	// --- per-page checks ----------------------------------------------------

	const englishByRelative = new Map(
		englishPages.map((p) => [relative(docsRoot, p).split(sep).join('/'), p])
	);
	/** Per locale, the English relative paths that have a translation. */
	const covered = Object.fromEntries(locales.map((locale) => [locale, new Set()]));

	for (const locale of locales) {
		for (const page of perLocale[locale]) {
			const label = pathLabel(page);
			const relativePath = relative(docsRoot, page).split(sep).join('/');
			/** The English page this file is, by its own location, a translation of. */
			const expectedRelative = relativePath.slice(locale.length + 1);
			const expectedSource = join(docsRoot, ...expectedRelative.split('/'));
			const expectedSourceLabel = pathLabel(expectedSource);

			const { sourceFile, sourceHash, hasFrontmatter } = readTranslationKeys(page);

			if (!hasFrontmatter) {
				errors.push(`${label}: no YAML frontmatter block, so it declares no translation source.`);
				continue;
			}
			if (!sourceFile) {
				errors.push(
					`${label}: missing \`sourceFile\` in the frontmatter. Add:\n` +
						`           sourceFile: ${expectedSourceLabel}`
				);
				continue;
			}
			if (!sourceHash) {
				errors.push(
					`${label}: missing \`sourceHash\` in the frontmatter. Without it, this page is ` +
						'a translation of nothing in particular and can never be detected as stale.'
				);
				continue;
			}
			if (!/^[0-9a-f]{40}$/.test(sourceHash)) {
				errors.push(
					`${label}: \`sourceHash: ${sourceHash}\` is not a 40-character git blob hash.`
				);
				continue;
			}

			// `sourceFile` must be the page's own mirror position. Allowing it to name any
			// file at all would let a translation point its hash at something that never
			// changes — a green check attached to the wrong bytes, which is the failure mode
			// this guard exists to remove, reintroduced through its own configuration.
			if (sourceFile !== expectedSourceLabel) {
				errors.push(
					`${label}: \`sourceFile: ${sourceFile}\` does not match this page's own position.\n` +
						`           A translated page mirrors its source path exactly, so it must be:\n` +
						`           sourceFile: ${expectedSourceLabel}`
				);
				continue;
			}

			covered[locale].add(expectedRelative);

			let exists = true;
			try {
				statSync(expectedSource);
			} catch {
				exists = false;
			}
			if (!exists) {
				errors.push(
					`${label}: its English source ${expectedSourceLabel} no longer exists.\n` +
						'           The English page was renamed or deleted and the translation was left ' +
						'behind, still being served. Move or delete this file to match.'
				);
				continue;
			}

			const actual = gitHashObject(expectedSource);
			if (actual !== sourceHash) {
				drifted.push({ page, source: expectedSource, recorded: sourceHash, actual });
				errors.push(
					`${label}: STALE. ${expectedSourceLabel} has changed since this page was translated.\n` +
						`           recorded: ${sourceHash}\n` +
						`           current:  ${actual}`
				);
			}
		}
	}

	// --- coverage (warning by default; see the header comment) ---------------

	for (const locale of locales) {
		for (const relativePath of englishByRelative.keys()) {
			if (covered[locale].has(relativePath)) continue;
			warnings.push(`${locale}: no translation of ${pathLabel(englishByRelative.get(relativePath))}`);
		}
	}

	return {
		errors,
		warnings,
		drifted,
		stats: {
			english: englishPages.length,
			translated: translatedCount,
			perLocale: Object.fromEntries(locales.map((l) => [l, perLocale[l].length])),
		},
	};
}

// ---------------------------------------------------------------------------
// --refresh
// ---------------------------------------------------------------------------

/**
 * Rewrite `sourceHash` on every drifted page to the English file's current hash.
 *
 * This is the button that says "yes, I have re-translated it". Pressing it without
 * re-translating is precisely the accident the guard exists to catch, so it prints
 * what it stamped rather than doing it quietly.
 */
function refresh(drifted) {
	if (drifted.length === 0) {
		console.log('nothing to refresh: no page records a stale source hash.');
		return;
	}
	for (const { page, source, recorded, actual } of drifted) {
		const text = readFileSync(page, 'utf8');
		const updated = text.replace(/^sourceHash:[ \t]*.*$/m, `sourceHash: ${actual}`);
		if (updated === text) {
			console.error(`ERROR: could not rewrite sourceHash in ${display(page)}.`);
			process.exit(1);
		}
		writeFileSync(page, updated);
		console.log(`${display(page)}`);
		console.log(`  source ${display(source)}`);
		console.log(`  ${recorded} -> ${actual}`);
	}
	console.log('');
	console.log(
		`refreshed ${drifted.length} page(s). This asserts the French text now says what the ` +
			'English text says.'
	);
	console.log('If it does not, you have just certified a stale page as current.');
}

// ---------------------------------------------------------------------------
// --self-test
// ---------------------------------------------------------------------------

/**
 * Prove the guard can fail.
 *
 * A check nobody has watched fail is a check nobody has tested; `promtool check rules`
 * passed for nine rules, five of which could never match a series. So this builds
 * fixture trees on disk and runs the REAL `inspect()` against them, asserting one
 * failure mode at a time — and asserting first that a correct tree produces exactly
 * zero errors, without which every other assertion below is satisfiable by a function
 * that always fails.
 */
function selfTest() {
	const root = mkdtempSync(join(tmpdir(), 'crystal-i18n-selftest-'));
	const cases = [];
	let failures = 0;

	const scenario = (name, build, assertion) => cases.push({ name, build, assertion });

	const write = (dir, relativePath, body) => {
		const full = join(dir, ...relativePath.split('/'));
		mkdirSync(dirname(full), { recursive: true });
		writeFileSync(full, body);
		return full;
	};

	/** An English page, and a French page correctly stamped with its hash. */
	const buildConsistent = (dir) => {
		const english = write(dir, 'start/quickstart.md', '---\ntitle: Quickstart\n---\n\nHello.\n');
		write(
			dir,
			'fr/start/quickstart.md',
			`---\ntitle: Démarrage rapide\nsourceFile: start/quickstart.md\nsourceHash: ${gitHashObject(english)}\n---\n\nBonjour.\n`
		);
		return english;
	};

	scenario(
		'a consistent tree passes',
		buildConsistent,
		(r) => (r.errors.length === 0 ? null : `expected no error, got: ${r.errors.join(' | ')}`)
	);

	scenario(
		'a drifted English source fails',
		(dir) => {
			const english = buildConsistent(dir);
			writeFileSync(english, '---\ntitle: Quickstart\n---\n\nHello, and one more sentence.\n');
		},
		(r) => (r.errors.some((e) => e.includes('STALE')) ? null : 'expected a STALE error')
	);

	scenario(
		'a vanished English source fails',
		(dir) => {
			const english = buildConsistent(dir);
			rmSync(english);
			// Keep a second English page so the "no English page" guard is not what fires.
			write(dir, 'start/install.md', '---\ntitle: Install\n---\n');
		},
		(r) => (r.errors.some((e) => e.includes('no longer exists')) ? null : 'expected a missing-source error')
	);

	scenario(
		'a missing sourceHash fails',
		(dir) => {
			buildConsistent(dir);
			write(
				dir,
				'fr/start/quickstart.md',
				'---\ntitle: Démarrage rapide\nsourceFile: start/quickstart.md\n---\n\nBonjour.\n'
			);
		},
		(r) => (r.errors.some((e) => e.includes('missing `sourceHash`')) ? null : 'expected a missing-key error')
	);

	scenario(
		'a sourceFile pointing somewhere else fails',
		(dir) => {
			buildConsistent(dir);
			const other = write(dir, 'start/stable.md', '---\ntitle: Stable\n---\n');
			write(
				dir,
				'fr/start/quickstart.md',
				`---\ntitle: Démarrage rapide\nsourceFile: start/stable.md\nsourceHash: ${gitHashObject(other)}\n---\n`
			);
		},
		(r) =>
			r.errors.some((e) => e.includes("does not match this page's own position"))
				? null
				: 'expected a mismatched-sourceFile error'
	);

	scenario(
		'a locale directory with no page at all fails',
		(dir) => {
			buildConsistent(dir);
			rmSync(join(dir, 'fr'), { recursive: true });
		},
		(r) =>
			r.errors.some((e) => e.includes('no translated page found'))
				? null
				: 'expected a vacuity error for an empty locale'
	);

	scenario(
		'an untranslated English page warns but does not fail',
		(dir) => {
			buildConsistent(dir);
			write(dir, 'start/install.md', '---\ntitle: Install\n---\n');
		},
		(r) => {
			if (r.errors.length !== 0) return `expected no error, got: ${r.errors.join(' | ')}`;
			return r.warnings.some((w) => w.includes('start/install.md'))
				? null
				: 'expected a coverage warning';
		}
	);

	try {
		cases.forEach(({ name, build, assertion }, index) => {
			const dir = join(root, `case-${index}`);
			mkdirSync(dir, { recursive: true });
			build(dir);
			const result = inspect({
				docsRoot: dir,
				locales: ['fr'],
				pathLabel: (p) => relative(dir, p).split(sep).join('/'),
			});
			const problem = assertion(result);
			if (problem) {
				failures += 1;
				console.error(`  FAIL  ${name}\n        ${problem}`);
			} else {
				console.log(`  ok    ${name}`);
			}
		});
	} finally {
		rmSync(root, { recursive: true, force: true });
	}

	if (failures > 0) {
		console.error('');
		console.error(`ERROR: ${failures} self-test case(s) failed. The staleness guard is not trustworthy.`);
		process.exit(1);
	}
	console.log('');
	console.log(`self-test: ${cases.length}/${cases.length} — the guard fails on drift, on a vanished`);
	console.log('source, on a missing key, on a redirected sourceFile and on an empty locale.');
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

const argv = process.argv.slice(2);
const wantSelfTest = argv.includes('--self-test');
const wantRefresh = argv.includes('--refresh');
const requireFullCoverage =
	argv.includes('--require-full-coverage') || process.env.I18N_REQUIRE_FULL_COVERAGE === '1';

requireGit();

if (wantSelfTest) {
	selfTest();
	process.exit(0);
}

const { TRANSLATED_LOCALES } = await import(pathToFileURL(LOCALES_MODULE).href);

const result = inspect({
	docsRoot: DOCS_ROOT,
	locales: TRANSLATED_LOCALES,
	pathLabel: (p) => display(p),
});

if (wantRefresh) {
	refresh(result.drifted);
	process.exit(0);
}

const { errors, warnings, stats } = result;

if (warnings.length > 0) {
	const severity = requireFullCoverage ? 'ERROR' : 'warning';
	console.log(`${severity}: ${warnings.length} English page(s) have no translation:`);
	for (const warning of warnings) console.log(`  - ${warning}`);
	console.log('');
	if (!requireFullCoverage) {
		console.log('  Coverage is a warning until French is complete, so that this check is not red');
		console.log('  from the day it lands; a permanently-red check is one people stop reading.');
		console.log('  Add --require-full-coverage to the CI job once the gap is closed.');
		console.log('');
	}
}

if (errors.length > 0) {
	console.error(`ERROR: ${errors.length} translation problem(s):`);
	for (const error of errors) console.error(`  - ${error}`);
	console.error('');
	if (result.drifted.length > 0) {
		console.error('  A stale page is served with the same confidence as a fresh one, which is why');
		console.error('  this fails the build. Re-read the English diff, update the French text, then:');
		console.error('');
		console.error('      make translations-refresh');
		console.error('');
		console.error('  That command only stamps the new hash. It does not translate anything, and');
		console.error('  running it without re-translating certifies a page that is now wrong.');
	}
	process.exit(1);
}

if (requireFullCoverage && warnings.length > 0) {
	console.error('ERROR: --require-full-coverage is set and pages above have no translation.');
	process.exit(1);
}

console.log(
	`translations are current: ${stats.translated} translated page(s) across ` +
		`${Object.entries(stats.perLocale)
			.map(([locale, n]) => `${locale}=${n}`)
			.join(', ')}, each matching the current bytes of its English source ` +
		`(${stats.english} English pages).`
);
