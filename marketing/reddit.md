# Reddit — draft posts

Do not post. Drafts for review. Check each subreddit's self-promotion rules
before posting; both of these are written as "here is a thing I built and what
I learned", not as an ad.

---

## r/golang

**Title:** `anymd: any document → Markdown in pure Go — 16 converters, CGO_ENABLED=0, and a benchmark I lost first`

**Body:**

I needed docx/xlsx/pdf/pptx/html → Markdown inside a Go service. The usual
answer is Microsoft's markitdown, which means a Python 3 virtualenv (318 MB with
extras) sitting beside a Go binary, at the right version, on every runner and in
every container that touches a file. So I wrote the Go one:
[github.com/muthuishere/anymd](https://github.com/muthuishere/anymd).

`CGO_ENABLED=0`. No poppler, no libmagic, no LibreOffice subprocess, no
interpreter. A 15 MB static binary, and the CLI is a thin shell over the same
public API you'd import.

**The converter registry** is markitdown's design, ported, and gladly so. A
converter is two methods:

```go
Accepts(r io.ReadSeeker, info anymd.StreamInfo, opts *anymd.Options) bool
Convert(r io.ReadSeeker, info anymd.StreamInfo, opts *anymd.Options) (anymd.Result, error)
```

Dispatch is hints (MIME, extension, filename, URL) plus magic-byte sniffing, in
priority order, first `Accepts` wins. `Accepts` must be cheap — hints and magic
bytes, never a full parse. Two rules that turned out to matter more than
expected:

1. **A converter that accepts and then fails is a hard error.** The engine does
   not fall through to the plaintext catch-all, because a catch-all that runs
   after a failed docx parse emits plausible-looking garbage at exit 0.
2. **Container formats recurse through `opts.Recurse`, never by constructing a
   new Engine.** That is the only thing that makes `--max-depth` (a zip in a zip
   in a zip) actually enforced rather than aspirational.

`anymd.New()` gives you a private registry; register at a lower priority number
and you override a built-in.

**Fuzzing.** Every converter is a parser pointed at bytes someone else chose, so
fourteen of them plus the engine dispatch have `Fuzz*` targets, and the contract
is that malformed input returns an `error` — never a panic. Third-party parsers
are wrapped in `recover()` guards. Zip entry counts, per-entry size, cumulative
decompressed size and path traversal are all checked.

**The performance bug worth stealing.** anymd parses PDFs with
`ledongthuc/pdf` (a fork of `rsc.io/pdf`), now vendored as `internal/pdf`.
Upstream ships no cache for resolved indirect objects — its own doc comment says
one "would probably help significantly" — while `Page.Content()` asks for a
glyph's width once per glyph, and each ask re-lexes the font's `/Widths` array
straight out of the file bytes. Page layout was O(glyphs × len(Widths)).
`BenchmarkConvertPdf`: 42.6 ms / 1,565,898 allocs / 75 MB → 2.59 ms / 16,605
allocs / 2.2 MB, from roughly ten lines, output byte-identical on all seven
corpus PDFs. The win tracks embedded subset fonts, not page count.

**Numbers** (M5 Pro, vs markitdown 0.1.5, `bench/run.sh` reproduces them). In
process: docx 0.59 ms vs 17.86, PDF 3.44 ms vs 74.47, xlsx 0.64 ms vs 7.35,
385 KB HTML 11.55 ms vs 104.15. Peak RSS 27 MB vs 160 MB. On the CLI, markitdown
also pays ~309 ms of interpreter startup per invocation — irrelevant in a
service, very relevant in a shell loop, so `bench/` publishes both tables.

**Quality, because speed is the easy one to win.** Scored against docling's
130-document corpus of (source, expected markdown) pairs: content 0.92 vs 0.91,
order 0.91 vs 0.88, tables 0.87 vs 0.85. anymd *lost* that benchmark the first
time it ran, and the table of defects it named — `drawingml.docx` emitting 13
bytes, Word text boxes dropped entirely, glyphs sorted by Y-then-X interleaving
the columns of a two-column paper — is still in `bench/README.md` next to the
fixes.

**Where it still loses:** PDF tables (0.43 vs 0.56) and PDF headings (0.14 vs
0.20) — it emits essentially no tables from PDF, which is a missing feature, not
a tuning problem. ODF routes through the PDF converter and scores 0.64 content
against 0.94. And markitdown genuinely transcribes audio out of the box; anymd
only does it with a model you supply, because it does it by shipping your bytes
to a remote service.

Happy to take criticism on the API shape — it's `v0` and the `Converter` /
`Options` surface can still change.

---

## r/LocalLLaMA

**Title:** `anymd — any document → Markdown for a local RAG pipeline, one Go binary, zero network calls by default`

**Body:**

If you're feeding documents into a local pipeline, the ingestion step is usually
the least local part of the stack. I got tired of that, so:
[github.com/muthuishere/anymd](https://github.com/muthuishere/anymd) — docx,
xlsx, xls, pptx, pdf, html, epub, msg, rss, csv, json, ipynb, zip and images to
clean GitHub-flavored Markdown. Pure Go, one 15 MB static binary, MIT.

**Nothing phones home, and it's structural rather than promised.** Converters
cannot open a socket, spawn a subprocess, or shell out. A document cannot make
anymd fetch a remote image, stylesheet, or linked article, no matter what is in
it. There are exactly two network paths in the whole project and both are things
you typed: a URL you passed as an argument (fetched before any converter sees a
byte), and `--llm`.

**`--llm` is off unless you pass it, and there is deliberately no environment
variable that turns it on.** Not "off unless a key is present" — a key sitting
in your shell can't flip it. When you do turn it on, point it wherever you like:

```sh
anymd --llm --llm-base-url http://localhost:11434/v1 --llm-model llava photo.jpg
```

Ollama, vLLM, anything OpenAI-shaped. That path gives you image captioning
inside docx/pptx and standalone images, plus reading scanned PDF pages with a
vision model. Cost guards are built in because a directory of decks will
otherwise ruin your day: identical images are deduplicated by sha256 (one logo
across 60 slides is one call), images under 4 KiB are skipped as furniture, and
per-document ceilings cap it at 50 captions and 20 scanned pages.

**The bit that matters most for RAG.** On a scanned PDF and on images,
markitdown returns **0 bytes with exit code 0**. Not an error — a successful
empty conversion. If that lands in your chunker, you have indexed nothing and
your index cannot tell the difference between that and a document that genuinely
had no text. anymd returns a named `ErrNoTextLayer` so you can route the file to
OCR or to a vision model instead of silently losing it. Same reasoning applies
to a transcription failure: it's a hard error, because the transcript *is* the
document and an empty success would be the same trap.

**Output is deterministic**, which matters if you're diffing chunks or caching
embeddings: a table from a spreadsheet and a table from a Word document come out
byte-identical, batch conversion emits in input order even though it runs on a
worker pool, and Markdown goes to stdout while everything else goes to stderr.

There's also `--crawl`, which walks a site to a bounded depth, converts each
page, and rewrites the links between crawled pages to relative local paths — so
you get a docs site as a browsable directory of Markdown. It reads robots.txt by
default and there is no way to request a zero delay between requests.

**Straight about the gaps,** since you'll hit them: PDF tables and headings are
weak (it emits essentially no tables from PDF — it's the largest known gap), ODF
routes through the PDF path and is poor, `.xls` formula results come back as a
placeholder, and there is no OCR, captioning or transcription at all without a
model you supply. Scanned-page reading also doesn't rasterise — there's no
pure-Go PDF renderer and a C one would end the single-binary property, so it
lifts the embedded image XObjects instead. That covers normal JPEG scans;
`CCITTFaxDecode` (most pre-2005 office scanning), JBIG2 and CMYK are skipped
rather than guessed at.

Speed, for what it's worth: docx 0.59 ms vs markitdown's 17.86, PDF 3.44 vs
74.47, peak RSS 27 MB vs 160 MB. `bench/` has the method and the runs.
