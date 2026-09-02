# Hacker News — Show HN draft

Do not post. Draft for review.

---

## Title

```
Show HN: Anymd – any document to Markdown, in pure Go (no cgo, no Python)
```
*(73 chars — HN's limit is 80.)*

Alternates:

```
Show HN: Anymd – markitdown in pure Go, and a benchmark where it loses
```
*(70 chars)*

```
Show HN: Anymd – document to Markdown in Go; markitdown returns 0 bytes, exit 0
```
*(79 chars)*

URL to submit: https://github.com/muthuishere/anymd

---

## First comment

Author here. anymd does the same job as Microsoft's markitdown — feed it a
file, get GitHub-flavored Markdown suitable for an LLM context window, a diff,
or a docs pipeline — with a different set of constraints. Pure Go, `CGO_ENABLED=0`,
no Python, no poppler, no LibreOffice subprocess. A 15 MB static binary against
a 318 MB virtualenv, and the same code is a `go get`-able library. 16 converters:
docx, xlsx, xls, pptx, pdf, html, epub, msg, rss, csv, json, ipynb, zip, images,
audio, plaintext.

The reason I wrote it is a failure mode, not a benchmark. On markitdown's own
test corpus, a scanned PDF and both test images come back as **0 bytes with exit
code 0**. There is genuinely nothing to extract from a page image — that part is
correct — but "nothing to extract" and "extraction succeeded" arrive at the
caller identically. In an ingestion pipeline that is the worst outcome there is:
a loud failure gets retried or routed to OCR, a quiet one becomes a document
your index insists it has. anymd returns a named `ErrNoTextLayer`.

That also breaks the naive coverage number. Counting exit codes says markitdown
converts 30/31 and anymd 26/31. Counting files that produce more than 200 bytes
says 19/31 and 19/31.

Three things that might be interesting regardless of whether you ever run it:

**There is no pure-Go PDF rasteriser, so scanned PDFs are not rasterised.**
Every good option (pdfium, MuPDF) is C, and a cgo dependency ends the
single-binary property that is the entire point. So when the optional vision
path is enabled, anymd doesn't render the page — it walks the PDF object graph
and lifts the page's image XObjects out directly. `DCTDecode` (what a scanner
almost always emits) and `JPXDecode` pass through untouched; 8-bit Flate RGB and
Gray get re-encoded to PNG. The honest gap: `CCITTFaxDecode` — most pre-2005
office scanning — plus JBIG2, LZW, indexed and CMYK colorspaces are not
supported, and are skipped rather than guessed at.

**The PDF parser got 16.8× faster from about ten lines.** anymd uses
`ledongthuc/pdf` (a fork of `rsc.io/pdf`), now vendored as `internal/pdf`. It
ships with no cache for resolved indirect objects; its own doc comment says
"the library makes no attempt at efficiency; a value cache maintained in the
Reader would probably help significantly". Meanwhile `Page.Content()` asks for a
glyph's width once per glyph, and each ask re-reads and re-lexes the font's
`/Widths` array straight out of the file bytes. Page layout was
O(glyphs × len(Widths)). For a 93 KB document: 1,565,898 allocations and 75 MB.
Adding the cache took `BenchmarkConvertPdf` from 42.6 ms / 1,565,898 allocs /
75 MB to 2.59 ms / 16,605 allocs / 2.2 MB, output byte-identical on all seven
corpus PDFs. I had previously written the slow number up in the benchmark as
"both tools are parser-bound" — an explanation that was wrong, and the reason
the before/after table is still in `bench/`.

**A table plugin was dropping tables at exit 0.** `html-to-markdown/v2`'s table
plugin bails on tables with no `<th>` or with block content in a cell and emits
the cells as loose paragraphs. Eight of the 22 tables in the quality corpus came
out as no table at all, successfully. Replaced with a grid walker; HTML tables
went 0.69 → 0.99, and since RSS, EPUB and `.msg` share the HTML path, they got
it too.

Numbers, all reproducible via `bench/run.sh` and `bench/run-quality.sh`, M5 Pro,
markitdown 0.1.5:

- In-process (the fair library comparison): docx 0.59 ms vs 17.86, PDF 3.44 ms
  vs 74.47, xlsx 0.64 ms vs 7.35, 385 KB HTML 11.55 ms vs 104.15.
- CLI: markitdown pays ~309 ms of interpreter startup per invocation, which is
  irrelevant to a long-running service and very relevant to a shell loop. Both
  tables are in `bench/` for that reason.
- Peak RSS 27 MB vs 160 MB.
- Quality, on docling's 130-document corpus (their (source, expected markdown)
  pairs; docling itself is not scored, since its own frozen output is the ground
  truth and it would trivially score 1.00): content 0.92/0.91, order 0.91/0.88,
  tables 0.87/0.85, headings 0.84/0.82, lists 0.83/0.82. Spreadsheets are the
  standout — xlsx content 0.98 vs 0.68, tables 0.99 vs 0.60.

anymd lost that quality benchmark the first time it was run. The table of
defects it named — `drawingml.docx` emitting 13 bytes, Word text boxes dropped,
two-column PDFs at 0.82 content and 0.50 order from sorting glyphs by Y then X —
is still in `bench/README.md`, because it is the evidence that the benchmark
does anything.

**Limitations, in the same spirit:**

- **PDF tables and headings are the biggest gap.** `out_tables: 0` on every PDF.
  Tables 0.43 vs markitdown's 0.56, headings 0.14 vs 0.20. That is a missing
  feature, not a tuning problem.
- **ODF is poor** — it routes through the PDF converter: content 0.64 vs 0.94,
  tables 0.17 vs 0.38.
- **No OCR, no audio transcription, no image captioning in the default build.**
  Those need a model. They exist behind `--llm`, off unless you pass it, and
  there is deliberately no environment variable that turns them on — a guarantee
  is only worth having if it holds without auditing your own shell.
- **markitdown genuinely transcribes audio out of the box** and anymd does not.
  It does it by sending your bytes to a remote speech service, which is the
  convert-time network call anymd is built not to make. Different trade-off, not
  a defect on either side.
- `.xls` formula *results* come back as a placeholder (a BIFF reader limit).
- HTML needing JS execution renders as whatever is in the source.
- The quality ground truth is docling's output, so "quality" here means
  agreement with docling — including where docling is itself wrong. Two corpus
  filenames (`table_misidentified_as_form`, `table_mislabeled_as_picture`) are
  docling's own record of getting a page wrong.

Other bits: a converter is two methods (`Accepts` + `Convert`) registered in a
priority-ordered registry, so you can add your own; batch conversion runs on a
worker pool but emits in input order, because deterministic and diffable beats
marginally faster; stdout is Markdown and stderr is everything else, always;
exit codes are 0/1/2 so a script can tell a bad document from a bad invocation;
`--crawl` walks a site, converts each page, and rewrites links between crawled
pages to relative local paths so the output directory browses offline (robots.txt
is read by default and there is no way to ask for a zero delay between requests).
Fourteen converters plus the engine's dispatch have fuzz targets, and the
contract is that no converter may panic on malformed input — it returns an
error.

MIT. Happy to be told where it is wrong — particularly if you have PDFs it
mangles.
