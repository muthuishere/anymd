# marketing/ — launch copy drafts

> **DO NOT POST.** Everything in this directory is a **draft for the owner to
> review and post manually**. Nothing here has been published, submitted,
> scheduled, or sent to any platform, and no browser or social tool was used to
> produce it. Review, edit, then post yourself.

## What's here

| file | contents |
|---|---|
| `linkedin.md` | three alternative posts (~200-230 words each). **A** is built on the silent-failure insight and is the recommended one; **B** is the deployment/footprint angle; **C** is "I lost my own benchmark". Post one, not three. |
| `x.md` | a 6-post thread plus 3 standalone posts. Every post has a measured character count stated under it; all are ≤ 280. |
| `hackernews.md` | a Show HN title (73 chars, plus two alternates) and a first comment with the technical detail and a limitations section. |
| `reddit.md` | one post for r/golang (registry design, no-cgo, fuzzing, the PDF perf bug) and one for r/LocalLLaMA (local pipelines, nothing phones home, the RAG failure mode). |

Check each subreddit's self-promotion rules before posting there.

## Where every number came from

Nothing in these drafts is estimated or rounded up. Sources, all in-repo:

**`bench/README.md`** — machine is an Apple M5 Pro (darwin/arm64), anymd v0.1.0
against markitdown 0.1.5, corpus is markitdown's own 31 test files.

- In-process ms and speedups: docx 0.59 / 17.86 (30×), xlsx 0.64 / 7.35 (11×),
  pdf 3.44 / 74.47 (22×), epub 0.10 / 3.10 (31×), pptx 0.91 / 8.52 (9.4×),
  385 KB HTML 11.55 / 104.15 (9.0×).
- markitdown's ~309 ms per-invocation interpreter startup; CLI docx 6.7 ms vs
  375.7 ms.
- Peak RSS 27 MB vs 160 MB; install footprint 15 MB binary vs 318 MB venv.
- Substantive output (>200 bytes): 19/31 both. Naive exit-code coverage: 26/31
  vs 30/31.
- The 0-bytes-exit-0 rows: scanned medical PDF, `test.jpg`, `test_llm.jpg`.
- PDF object cache before/after: `test.pdf` 43.77 → 2.60 ms (16.8×);
  `BenchmarkConvertPdf` 42.6 ms / 1,565,898 allocs / 75 MB → 2.59 ms /
  16,605 allocs / 2.2 MB; 93 KB document, byte-identical on all seven corpus
  PDFs.
- Quality on docling's 130 documents (anymd / markitdown): content 0.92/0.91,
  order 0.91/0.88, tables 0.87/0.85, headings 0.84/0.82, lists 0.83/0.82; xlsx
  content 0.98/0.68 and tables 0.99/0.60; xls content 0.95/0.68; PDF tables
  0.43/0.56 and headings 0.14/0.20; odf content 0.64/0.94 and tables 0.17/0.38.
- The named first-run defects: `drawingml.docx` content 0.08 / 13 bytes,
  `textbox.docx` 0.13, `2203.01017v2.pdf` content 0.82 order 0.50,
  `xlsx_07_gap_tolerance_.xlsx` tables 0.11.

**`CHANGELOG.md`** — HTML tables 0.69 → 0.99 and the `html-to-markdown/v2` table
plugin dropping 8 of the corpus's 22 tables; docx content 0.84 → 0.94; xlsx
tables 0.74 → 0.99; PDF order 0.70 → 0.77; the `--llm` cost guards (sha256
dedupe, 4 KiB skip, ceilings of 50 captions and 20 scanned pages); the
scanned-PDF image-XObject path and its unsupported filters (`CCITTFaxDecode`,
JBIG2, LZW, indexed, CMYK, non-8-bit); the known-limitations list; `--llm`
corpus coverage 27/31.

**`README.md`** — the format table, the stream/exit-code contract, the
converter-extension contract, the security posture, `--llm` precedence and
config.

**`docs/adr/0001`** — pure Go as an invariant vs no-network as a default, and
the explicit point that a default which flips on when a key is in the
environment would forfeit the guarantee. **`0002`** — why not pdfium.
**`0003`** — why docling is the target and not a contestant.

**Verified by running it** (2026-09-02, this repo, `main`):

- `anymd --list` prints 16 converters: audio, csv, docx, epub, image, ipynb,
  msg, pdf, pptx, rss, xls, xlsx, json, html, zip, plaintext.
- `anymd --help` confirms the flag surface used in the copy, including
  `--crawl / --depth / --max-pages / --crawl-delay` (default 500 ms) and
  `--ignore-robots` (so robots.txt is read by default).
- `CGO_ENABLED=0 go build -ldflags="-s -w"` produces a 16 MB binary here; the
  copy uses **15 MB**, which is the figure `bench/README.md` reports for the
  install footprint. If you want them to agree, re-measure and update both.
- 15 `Fuzz*` targets in `fuzz_test.go` (14 converters + engine dispatch), plus
  `FuzzCacheKey`. The copy says "fourteen converters plus the engine's
  dispatch", not "every converter".

## Claims we must not make

These are wrong, or unsupported by anything in the repo. Do not let them creep
back in during editing.

1. **"anymd does OCR."** It does not. A scanned PDF returns `ErrNoTextLayer`.
   With `--llm` and a vision model *you* supply and pay for, the page's embedded
   images are read — that is a model call, not built-in OCR, and it is off by
   default.
2. **"anymd transcribes audio."** Only with `--llm --llm-transcribe` and a
   speech endpoint you configure. Without a `Transcriber` the audio converter
   declines in `Accepts`. markitdown does this out of the box and we should say
   so — it is a real capability we do not have by default.
3. **"anymd beats markitdown on PDF."** It does not, across the board. It wins
   PDF *reading order* (0.77 vs 0.74) and speed, and loses PDF content
   (0.87 vs 0.90), tables (0.43 vs 0.56) and headings (0.14 vs 0.20). It emits
   essentially no tables or headings from PDF at all.
4. **"anymd handles ODF well."** It does not — ODF routes through the PDF
   converter and loses on every metric there (content 0.64 vs 0.94).
5. **"anymd wins on coverage."** Substantive output is 19/31 for both tools.
   The 26-vs-30 exit-code number flatters markitdown and the 19-19 number is the
   honest one; do not quote either without the other.
6. **Any user quote, testimonial, download count, star count, adoption claim, or
   "used in production at X".** There is no evidence for any of these in the
   repo and none may be invented.
7. **"Every converter is fuzzed."** Fourteen are, plus engine dispatch.
8. **Benchmark numbers without their context.** They are one machine (M5 Pro),
   one markitdown version (0.1.5), and the *default offline build*. With `--llm`
   the wall clock is whatever the model takes and the comparison is no longer
   about parsers. The quality ground truth is docling's own output, so "quality"
   means agreement with docling, including where docling is itself wrong.

## Claims that were dropped for lack of evidence

- Anything about install/adoption numbers, GitHub stars, or users.
- Any "N× faster overall" single figure — the speedups are per-format and range
  from 9× to 31× in process, and the CLI figures are different again.
- Any claim about CI runtime, container image size savings, or cost savings in a
  real pipeline; nothing in the repo measures those.
- Any comparison against docling, Unstructured, LlamaParse or Tika. Only
  markitdown 0.1.5 has been measured.
