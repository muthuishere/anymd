# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Versioning note: while the major version is `0`, the Go API (`Converter`,
`StreamInfo`, `Options`, `Result`) may change in a minor release. The **CLI
contract** — stdout is Markdown, stderr is diagnostics, exit `0`/`1`/`2` — is
treated as stable from `0.1.0` onward.

## [Unreleased]

### Added

- **Optional LLM features — off by default.** anymd can now read what it
  previously could only skip, when the caller explicitly supplies a model.
  Nothing here changes the default build: with no `Describer`/`Transcriber` and
  no `--llm`, output is byte-identical to `0.1.0` and no network call is made.
  See [ADR 0001](docs/adr/0001-pure-go-no-cgo-no-network.md) for why that line
  is drawn where it is.

  - **Scanned PDFs are readable.** A PDF with no text layer used to return
    `ErrNoTextLayer` and stop. With a `Describer` it now extracts the page's
    image XObjects and has them read. Verified end to end against a live model
    on the corpus's scanned medical report — the file markitdown returns
    0 bytes and exit 0 for. No rasterization is involved: there is no pure-Go
    PDF renderer and a cgo one would end the single-binary promise, so the
    embedded images are lifted from the object graph instead. `DCTDecode`
    (the usual scan encoding) passes through untouched; `JPXDecode` passes
    through; 8-bit Flate RGB/Gray is re-encoded to PNG. **Not supported, and
    skipped silently rather than guessed:** `CCITTFaxDecode` — which is most
    pre-2005 office scanning — plus JBIG2, LZW, indexed and CMYK colorspaces,
    and non-8-bit depths.
  - **Images in `.docx` and `.pptx` are captioned**, with the author's own alt
    text passed to the model as a hint rather than discarded.
  - **Standalone images are captioned**, alongside the existing dimensions and
    EXIF.
  - **Audio is transcribed** (`.mp3 .m4a .wav .flac .ogg .webm .mp4`), closing
    the last format gap against markitdown. Requires a `Transcriber`; without
    one the converter *declines* in `Accepts`, so the engine still returns the
    honest `ErrUnsupported` rather than accepting and then failing.
  - Corpus coverage with `--llm`: **27 of 31**, up from 26.

- `Options.Describer`, `Options.Transcriber`, `Options.LLMTimeout`, and the
  `Describer` / `Transcriber` interfaces (`llm.go`). These are interfaces, not a
  vendor SDK — a local model, a hosted API or a test stub all satisfy them.
- `llm` package: a `Describer` built on
  [toolnexus](https://github.com/muthuishere/toolnexus) (which already handles
  keys, retries, backoff and the OpenAI-vs-Anthropic wire difference) and a
  `Transcriber` written against `/audio/transcriptions` directly, since
  toolnexus drives chat completions only.
- **Config file** at `~/.config/anymd/anymdconfig.json` — `llm.Load`,
  `llm.ConfigPath`, `llm.FromConfigFile`. Every string field supports `${VAR}`
  interpolation from the environment, which is how a key stays out of the file.
  An unset `${VAR}` is a hard error naming the *variable*: expanding it to `""`
  would send an unauthenticated request and surface as a baffling 401. Errors
  never contain a value. Not `os.UserConfigDir()`, which on macOS resolves to
  `~/Library/Application Support`.
- **CLI**: `--llm`, `--llm-model`, `--llm-base-url`, `--llm-config`,
  `--llm-timeout`, `--llm-transcribe`, `--llm-transcribe-model`. Any `--llm-*`
  flag without `--llm` is a usage error (exit 2) rather than silently ignored.
  Precedence: explicit flag > config file > environment > default.
- **CLI**: `anymd config path | show | init`. `show` redacts every secret —
  it parses the raw file rather than the interpolated config, and covers the
  three leak paths (an interpolated key, a literal header, credentials inside a
  proxy URL). `init` writes `0600` with `O_EXCL`, so it cannot clobber a config
  written a moment earlier.

- `docs/adr/` — architecture decision records, with
  [0001](docs/adr/0001-pure-go-no-cgo-no-network.md) (pure Go, and no network
  the caller did not ask for — why the first half is an invariant and the
  `--llm` default is not; recorded retrospectively) and
  [0002](docs/adr/0002-vendor-the-pdf-parser.md) (why the PDF parser is
  vendored, and why the answer was not pdfium).
- `CONTRACT.md` rule 8: what vendoring third-party code requires.

### Changed

- **Output quality now leads markitdown on every aggregate metric**, measured on
  docling's 130-document corpus (see [bench/](bench/)): content 0.88 -> **0.92**
  (markitdown 0.91), order 0.86 -> **0.91** (0.88), tables 0.75 -> **0.87**
  (0.85). anymd lost this benchmark when it was first written; it named the
  defects and they were fixed.

  - **html tables 0.69 -> 0.99.** `html-to-markdown/v2`'s table plugin bails on
    tables with no `<th>` or with block content in a cell and emits the cells as
    loose paragraphs — eight of the corpus's 22 tables came out as no table at
    all, at exit 0. Replaced with our own grid walker rendering through
    `mdutil.Table`. This is the shared HTML path, so RSS, EPUB and `.msg` gain
    it too.
  - **docx content 0.84 -> 0.94, tables 0.89 -> 0.98.** Word text boxes
    (`w:txbxContent`, both DrawingML and legacy VML), DrawingML canvas text and
    embedded charts were dropped entirely — `drawingml.docx` emitted 13 bytes at
    exit 0. Content controls (`w:sdt`) are now transparent, which alone
    recovered two named defects, and single-cell wrapper tables are unwrapped.
  - **xlsx tables 0.74 -> 0.99, content 0.87 -> 0.98.** A sheet is split into
    4-connected regions instead of being dumped as one table; merged cells
    repeat across their span; section labels lift to prose; cell comments
    (legacy and threaded) and chart sheets are extracted.
  - **pdf reading order 0.70 -> 0.77.** A recursive XY-cut replaces
    sort-by-Y-then-X, which interleaved the columns of a two-column page — every
    token present, sequence scrambled. Content is unchanged on all 15 files,
    which is what distinguishes a reading-order defect from a content-loss one.

- **`--llm` costs money and sends document content to a third party.** One
  model call per captioned image, one per transcribed file. Guards are built in:
  identical images are deduplicated by sha256 (one logo across 60 slides is one
  call), images under 4 KiB are skipped as furniture, and per-document ceilings
  cap runaway documents (50 captions, 20 scanned pages). A captioning failure
  degrades to the previous output; a *transcription* failure is a hard error,
  because the transcript is the document and an empty success would be exactly
  the silent-nothing behaviour this project criticises.

- **PDF conversion is ~13x faster** (markitdown's `test.pdf`: 45.39 ms -> 3.44 ms
  in-process; `BenchmarkConvertPdf` 42.6 ms / 1,565,898 allocs / 75 MB ->
  2.59 ms / 16,605 allocs / 2.2 MB). Output is byte-identical on every file in
  the corpus, with or without a `Describer`.

  `github.com/ledongthuc/pdf` is now vendored as `internal/pdf` (BSD-3, see
  `internal/pdf/README.md`) with the one change upstream's own doc comment asks
  for: a cache of resolved indirect objects. Without it, `Page.Content()` asked
  each font for a glyph width once per glyph and every ask re-lexed the font's
  `/Widths` array straight out of the file bytes, making page layout
  `O(glyphs x len(Widths))`. The win scales with embedded subset fonts, not
  page count. The vision path added in `cca96b3` is unaffected: it walks raw
  object offsets rather than resolved values, and still extracts the same three
  JPEGs, byte for byte, from the corpus's scanned report.

### Fixed

- `bench/run.sh` is re-runnable and completes. It aborted on a second run
  (existing venv), and `set -o pipefail` made the coverage loop die on the
  first file a tool exits nonzero for — which is exactly what that table
  measures. It also now generates anymd's own in-process column
  (`bench/inproc`) instead of leaving it to be filled in by hand.

## [0.1.0] - 2026-09-01

Initial release. Any document → Markdown, in pure Go: one static binary and one
`go get`-able library, sharing the same converter registry.

### Added

- **Library API.** `anymd.ConvertFile`, `anymd.ConvertBytes`, `anymd.Convert`
  for one-shot use, and `anymd.New()` for a private registry you can extend
  with your own `Converter`. A converter is two methods, `Accepts` + `Convert`;
  the optional `Named` and `Prioritized` interfaces control dispatch order.
- **Dispatch engine.** Hints (`MimeType`, `Extension`, `Charset`, `FileName`,
  `URL`) plus magic-byte sniffing, in priority order, first `Accepts` wins.
  A converter that accepts and then fails is a hard error — the engine does not
  fall through to a catch-all that would emit plausible-looking garbage.
- **15 converters** (the live list is always `anymd --list`):

  | converter | extensions | extracts |
  |---|---|---|
  | `csv` | `.csv` `.tsv` `.tab` | delimiter sniffing → GFM pipe table |
  | `docx` | `.docx` | headings, paragraphs, lists, tables, links, image alt text, core-properties title |
  | `epub` | `.epub` | spine order, per-chapter HTML → Markdown, metadata title |
  | `html` | `.html` `.htm` `.xhtml` | headings, lists, tables, links, code, `<title>` |
  | `image` | `.jpg` `.jpeg` `.png` `.gif` `.webp` `.tiff` `.bmp` | pixel dimensions and EXIF (capture time, camera, lens, exposure, GPS, description, rights) |
  | `ipynb` | `.ipynb` | markdown cells, code cells as fenced blocks, text outputs |
  | `json` | `.json` | pretty-printed, fenced as a code block |
  | `msg` | `.msg` | subject, From/To/Cc/Date table, HTML or plain body |
  | `pdf` | `.pdf` | the text layer, page by page |
  | `pptx` | `.pptx` | slide-by-slide shape text, tables, charts, image alt text, speaker notes |
  | `rss` | `.rss` `.atom` `.xml` `.rdf` | feed title, per-entry title, date, link, summary |
  | `xls` | `.xls` `.xlt` `.xlm` `.xlw` | legacy BIFF workbooks, byte-identical output to `xlsx` |
  | `xlsx` | `.xlsx` `.xlsm` `.xltx` `.xltm` | every sheet as a heading + table, computed cell values |
  | `zip` | `.zip` | recursive conversion of each member, bounded by `--max-depth` |
  | `plaintext` | `.txt` `.text` `.md` `.markdown` `.log` | verbatim passthrough; the last-resort fallback for anything that decodes as UTF-8 text |

- **CLI** (`cmd/anymd`) — a thin shell over the same public API.
  - stdin with `-t`/`--type` hints, single files, whole directories under `-r`,
    and `http(s)` URLs as arguments.
  - Batch output with `-d`/`--outdir` and `--ext`, colliding basenames suffixed
    rather than silently clobbered.
  - Converted on a worker pool sized to `runtime.NumCPU()` but **emitted in
    input order** — deterministic and diffable beats marginally faster.
  - Flags: `-o`, `-d`/`--outdir`, `--ext`, `-t`/`--type`, `-r`/`--recursive`,
    `--charset`, `--max-depth`, `--keep-data-uris`, `--title`, `-q`/`--quiet`,
    `--fail-fast`, `--insecure`, `--list`, `--version`.
  - **Stream contract:** Markdown to stdout, progress/warnings/errors to
    stderr, always — never interleaved.
  - **Exit codes:** `0` everything converted · `1` one or more inputs failed ·
    `2` usage error. Distinct codes let a CI script tell "the document was bad"
    from "the invocation was bad".
- **Deterministic GFM emitters** (`internal/mdutil`): one `Table`, one
  `Heading`, one `CodeBlock`, one `Join`. A table from a spreadsheet and a
  table from a Word document come out byte-identical, which is what makes the
  output safe to commit and diff.
- **Pure Go, no cgo.** Builds with `CGO_ENABLED=0`, cross-compiles everywhere
  Go targets, links no native library — no poppler, no libmagic, no
  LibreOffice subprocess, no Python interpreter.
- **Hardening against hostile documents.** Converters never panic on malformed
  input; `recover()` guards wrap third-party parsers; zip entry counts, per-entry
  size and cumulative decompressed size are all capped; zip paths are checked for
  traversal; container recursion is bounded centrally by `Options.MaxDepth`
  (default 8) via `Options.Recurse`. See [SECURITY.md](SECURITY.md).
- **Release plumbing:** GoReleaser config for darwin/linux/windows across
  amd64/arm64 (plus linux/386) with a macOS universal binary, published
  archives with `checksums.txt`, a checksum-verifying `install.sh`, and
  Homebrew/Scoop manifest templates. See [packaging/README.md](packaging/README.md).

### Not included — on purpose

These are markitdown's most impressive-looking converters, and each one would
mean a model, a service, an API key, or a native dependency. Shipping them
would break "one static binary that works offline":

- **No OCR.** A scanned PDF or a photo of a page returns a named
  `ErrNoTextLayer` rather than empty output or invented text, so a caller can
  tell "nothing to extract" from "extraction failed".
- **No audio or video transcription.** No Whisper, no speech service.
- **No LLM image captioning.** Images yield dimensions and EXIF; the pixels are
  never described.
- **No network access at convert time.** A converter cannot resolve a remote
  image, stylesheet, or linked article. The single network call in the whole
  project is the CLI fetching a URL you passed as an argument, before any
  converter sees a byte.

If you want any of those, wrap `anymd` in your own pipeline where you control
the cost and the data boundary.

### Known limitations

- `.xls` formula **results** come back as a placeholder (a BIFF reader limit),
  and Excel's own date formats are not applied.
- PDF layout columns, figures and form fields are not reconstructed; only the
  text layer is extracted.
- HTML that requires JavaScript execution renders as whatever is in the source.
- Encrypted zips and DRM'd EPUBs are refused.
- `.msg` attachments, RTF-compressed bodies and recipient storages are skipped.

[Unreleased]: https://github.com/muthuishere/anymd/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/muthuishere/anymd/releases/tag/v0.1.0
