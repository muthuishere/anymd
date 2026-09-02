// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import starlightLlmsTxt from 'starlight-llms-txt';

// Project GitHub Pages: https://muthuishere.github.io/anymd
// https://astro.build/config
export default defineConfig({
	site: 'https://muthuishere.github.io',
	base: '/anymd',
	integrations: [
		starlight({
			title: 'anymd',
			description:
				'Any document → Markdown, in pure Go. A cgo-free, Python-free alternative to Microsoft markitdown: one static binary and one go get-able library.',
			plugins: [
				// /llms.txt and /llms-full.txt. This is on-thesis rather than decorative:
				// anymd exists to turn documents into Markdown an LLM can read, so its own
				// documentation being machine-readable is the product arguing for itself.
				starlightLlmsTxt({
					projectName: 'anymd',
					description:
						'A pure-Go library and CLI that converts any document — docx, xlsx, pptx, pdf, html, epub, csv, json, msg, rss, ipynb, zip, images, audio — into clean GitHub-flavored Markdown.',
					details:
						'anymd is a Go-native alternative to Microsoft markitdown. It builds with CGO_ENABLED=0, needs no Python interpreter, no poppler, no libmagic and no LibreOffice subprocess, and cross-compiles anywhere Go does. The CLI is a thin shell over the same public API you import with go get. Conversion makes no network calls unless you explicitly pass --llm, which opts in to vision-model image captioning, scanned-PDF reading, and (with --llm-transcribe) audio transcription.',
				}),
			],
			customCss: ['@fontsource-variable/inter', './src/styles/deemwar.css'],
			social: [
				{ icon: 'github', label: 'GitHub', href: 'https://github.com/muthuishere/anymd' },
			],
			editLink: {
				baseUrl: 'https://github.com/muthuishere/anymd/edit/main/site/',
			},
			// Pagefind full-text search (Starlight's default) stays on.
			pagefind: true,
			sidebar: [
				{
					label: 'Start here',
					items: [
						{ label: 'Overview', slug: '' },
						{ label: 'Getting started', slug: 'getting-started' },
					],
				},
				{
					label: 'Use it',
					items: [
						{ label: 'CLI', slug: 'cli' },
						{ label: 'Library', slug: 'library' },
						{ label: 'Formats', slug: 'formats' },
					],
				},
				{
					label: 'Going further',
					items: [
						{ label: 'LLM features', slug: 'llm' },
						{ label: 'Caching', slug: 'cache' },
						{ label: 'Extending it', slug: 'extending' },
					],
				},
				{
					label: 'Evidence',
					items: [{ label: 'Benchmarks', slug: 'benchmarks' }],
				},
			],
		}),
	],
});
