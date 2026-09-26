# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Versioning note: while the major version is `0`, the Go API (`Converter`,
`StreamInfo`, `Options`, `Result`) may change in a minor release. The **CLI
contract** — stdout is Markdown, stderr is diagnostics, exit `0`/`1`/`2` — is
treated as stable from `0.1.0` onward.

## [Unreleased]

### Fixed

- **Two different images could share one inlined payload.** The dedup key
  began with the byte length written as `string(rune(n))`, and a rune
  conversion is not injective: every length in the surrogate range
  `0xD800`-`0xDFFF`, and every length above `0x10FFFF`, collapses to the same
  replacement character. Two images whose lengths both landed there and whose
  first and last 64 bytes matched — the same logo saved twice at slightly
  different quality, say — would resolve to one `data:` URI, so one of them
  was emitted in the other's place. The length is now written as decimal.

- The dead pre-renderer (`pdfPageText` and the two run-extent helpers it
  alone used) is removed, having been superseded by the structure-aware
  renderer, and the soft hyphens in the hyphenation rules are written as
  `\u00ad` escapes rather than as invisible characters in a string literal.

## [0.3.0] - 2026-09-26

### Added

- **A text-layer soundness gate.** A PDF whose fonts carry no usable ToUnicode
  mapping still draws glyphs in the right places, so extraction "succeeds":
  correct reading order, correct line breaks, every character meaningless.
  Nothing downstream can tell that from real text. anymd now measures the share
  of glyphs that decode to the replacement character, a private-use code point
  or a stray control code, and acts on it:

  - above 2% (the figure Marker uses) the page is no longer trusted, and with
    an `Options.Describer` the pixels are read by a vision model instead;
  - above 30% there is nothing worth keeping, and a document whose every page
    is in that state returns the new `ErrGarbledTextLayer` rather than fluent
    rubbish.

  The two thresholds are separate deliberately. Marker's 2% chooses between a
  text layer and OCR, with OCR always available; anymd has no OCR, so at 2% the
  choice is between imperfect text and nothing. A real document met in testing
  was 12% unreadable — a school report whose headings came through a subset
  font as "6WXGHQW 3URJUHVV 5HSRUW" while the student's name and roll number
  were perfectly correct. Refusing that would lose the 88% that was right.

- **Heading levels from the document outline.** A PDF's bookmarks are the
  author's own table of contents: they name the headings and say how they nest,
  which is exactly what the font and numbering rules have to infer. The outline
  now settles the level where it names a line, and recognises a heading set at
  body size that the size rule cannot see. It never invents one: an outline
  entry can point anywhere, and a heading conjured where the page shows none is
  worse than a missing one. On a Word document converted to PDF, the headings
  extracted from the PDF now match those extracted from the DOCX exactly.

- **Images are inlined as base64 `data:` URIs, in every format that carries
  them.** A document's pictures are part of its content; dropping them to an
  empty `![]()` kept the position and lost the thing itself. This now covers
  PDF, DOCX, PPTX, notebooks, standalone images and images inside containers,
  so the Markdown is self-contained — no sidecar directory, no relative paths
  to keep in step.

  Size is metered rather than unbounded: `--max-image-kb` caps a single image
  (default 2 MiB of raw bytes), a document-wide budget caps the rest (16 MiB),
  and an image over either cap keeps its placeholder so it is visibly skipped
  rather than silently missing. Repeats — a letterhead on every page, a logo on
  every slide — are encoded once and charged once. `--no-images`
  (`Options.DropImages`) restores the old behaviour.

- **PDF figures are extracted and placed in reading order.** Images were only
  ever looked at on a page with *no* text, the pure-scan case, so a chart in a
  report was dropped along with any sign it had been there — its caption left
  pointing at nothing. Even `--llm` never saw it. Three things were missing and
  are now in place:

  - the content interpreter had no `Do` operator at all, so image *placement*
    was invisible; it now records each placement with the area the graphics
    state puts it in, which is what lets a figure sit between the right two
    paragraphs instead of being appended somewhere;
  - Form XObjects are followed to the image inside them, which is how
    reportlab, Word and LibreOffice all wrap a picture;
  - `/Filter` is treated as the chain it is. An ordinary
    `[/ASCII85Decode /DCTDecode]` photograph matched no case at all and was
    skipped; transport filters (ASCII85, ASCIIHex) are now undone before the
    image codec is read.

- **Composite and standard-14 font metrics**, and **notebook image outputs**
  (a notebook's charts are `display_data` payloads, and were dropped).

- **PDF structure recovery.** A PDF records glyphs and coordinates, not
  headings, lists or tables; a converter that only sorts the glyphs throws away
  every structure a reader can see. anymd now recovers, deterministically and
  with no model involved:

  - **Tables**, ruled and borderless, as GitHub-flavored pipe tables. Columns
    come from cell-interval overlap rather than left edges, so a column of
    right-aligned figures stays one column, and cells are placed in the track
    their geometry puts them in — a totals row's amount lands under "Amount",
    not under "Description". Every cell is filled from the glyphs inside it, so
    a wrong grid can misplace a value but can never invent one.
  - **Headings**, from font size relative to the document's body size and from
    section numbering (`2.`, `1.1`, `Artikel 5.`), with the numbering deciding
    depth because it is authored hierarchy. Bold alone never makes a heading: a
    body-size lead-in like "Conditions:" is not a section, and promoting it
    splits a rule from the condition it governs.
  - **Lists**, bulleted and numbered, including the private-use code points
    Word writes for a Symbol or Wingdings bullet, markers set in their own text
    frame, and nesting recovered from indentation.
  - **Wrapped lines joined into paragraphs**, with paragraph breaks measured
    against the page's own prevailing leading rather than a fixed multiple of
    the font size.
  - **Hyphenation repaired with evidence.** A line-final hyphen is joined only
    when the document does not spell the compound out intact somewhere else, so
    "sys-" + "tem" becomes "system" while "e-" + "mail" and "long-" + "term"
    stay as the author wrote them. Capitals, digits and suspended hyphens
    ("in- en verkoop") are never joined.
  - **Running headers and footers** detected across pages and reported once
    rather than on every page — kept, not deleted, because a footer reading
    "last updated …" is often the only place that fact appears.

- **A table is no longer mistaken for a two-column page.** The reading-order cut
  now rejects a gutter whose rows share baselines and whose lines stop short of
  their column edges, which is a table's signature and not prose's. Cutting
  there used to read a table down its columns, divorcing every figure from its
  row.

- **DOCX lists that are lists by style.** A paragraph can be a list item with no
  `w:numPr` of its own, with the numbering hanging off the style — which is what
  python-docx and LibreOffice produce for "List Bullet" and "List Number". Those
  lists were arriving as unmarked paragraphs.

- **PPTX outlines are lists.** Body placeholder text on a slide is a bulleted
  list; PowerPoint inherits the bullet from the layout rather than writing it
  into the slide, so it was being emitted as loose paragraphs. Nesting comes
  from `a:pPr lvl`. Titles and subtitles stay prose.

### Changed

- PDF output joins wrapped lines into paragraphs instead of emitting one
  Markdown line per PDF line. A single newline inside a Markdown paragraph is
  not a line break, and keeping one made hyphenation repair impossible.

### Fixed

- **Simulated bold doubled every character.** A document that wants a weight
  its font does not have paints the glyphs twice, a hair apart. Both paintings
  are real glyphs in the text layer, so "Important" came out as
  "IImmppoorrttaanntt". An overprint is now recognised by position — it sits
  within a fraction of the font size of the glyph it is thickening, while a
  genuine double letter is a whole advance away, so "bookkeeper" is untouched.

- **A running header of more than one line kept repeating.** Only the single
  outermost line of each band could be furniture, so a two-line masthead had
  its first line suppressed and its second left on every page — the more
  visible half of the failure. Furniture is now a block chained inward from the
  edge of the page, capped at three lines per band.

- **The furniture band was measured against the ink, not the page.** A margin
  is measured from the edge of the paper, so the last line of body text on any
  page sat, by construction, at distance zero from the "bottom" and fell inside
  the footer band every time. Bounds now come from the MediaBox, inherited up
  the page tree.

- **Body text that starts near the edge of the page is no longer taken as
  furniture.** A document set with ordinary one-inch margins puts its first
  line inside any band wide enough to catch a real running head, and its last
  line inside the footer band; the digit normalisation that lets "Page 3" match
  "Page 4" then matched those lines across pages and deleted them. Narrowing
  the band is not the answer — a real footer sits further into the page than a
  header does, and a tight band loses it. What separates the two is the gap: a
  running head stands detached from the text, while body text runs on at the
  body's own pitch straight through the band. Furniture is now the block
  chained in from the page edge, ended where the document's own body type
  appears, and kept only when it is detached from the text that follows.

  Two details matter more than they look. The block ends at the *body* size
  rather than at any change of size, because a masthead is commonly two sizes —
  a name over a strapline — and cutting it at the first change splits it,
  leaving the smaller half to repeat on every page. And on a page with too few
  lines to measure a pitch (a cover sheet, a short form), the pitch is not
  measured at all but estimated from the type size: every gap such a page
  offers is one of the gaps being judged, so measuring would compare the
  yardstick against itself and let a properly detached header through.

- **A table spanning pages came apart, and lost a row to each break.** Three
  faults met: the repeated header row is exactly the shape the running-header
  pass looks for, so suppressing it left the delimiter row to promote that
  page's first *data* row into a header; rendering page by page emitted one
  table per page separated by a rule, which Markdown cannot resume a table
  after; and the table's own rows outnumbered the prose, so the body-size
  estimate came from the table and turned every real paragraph into a heading.
  Table rows are now immune to furniture detection, excluded from body sizing,
  and stitched back into one table across the break.

- **`--keep-data-uris` never did anything.** It was declared, documented, in
  the CLI help and hashed into the cache key, but no converter read it — and
  the default was the opposite of what it documented. It is now deprecated and
  accepted as a no-op, since inlining is the default; use `--no-images` to opt
  out.

- **PDF text was silently corrupted whenever a font mapped one code to several
  characters.** A ToUnicode CMap may map a single code to a string — that is how
  a subset font spells out a ligature, so the one `ft` glyph LibreOffice emits
  for "Platform" decodes to two characters. The decoder walked the decoded runes
  and the raw codes with one index, so from the first ligature on a line every
  glyph was measured with the wrong code's width. Positions drifted, the sort by
  X reordered the line, and real documents came out as "Platofrm", "Optino",
  "Migratoin efofrt" and "Trade-ofsf". Decoding is now per code.

- **Every `TJ` operator appended a spurious glyph.** The text builder ended each
  `TJ` with a synthetic `"\n"`, which was then decoded through the current
  font. In a subset font byte `0x0A` is a real glyph, so every LibreOffice line
  ended in a stray letter — `t`, `r`, `p`, `m`, whichever the font put at that
  code.

- **Standard-14 fonts had no metrics at all.** A font from the Core 14 may be
  named without being embedded and without a `/Widths` array; a viewer is
  required to know its metrics already. anymd did not, so `Width` returned zero
  for every glyph, the text matrix never advanced, and a whole page of glyphs
  was reported at one X coordinate. The characters still came out in document
  order, which is why this went unnoticed — but every structure inferred from
  geometry was being inferred from nothing. The published AFM widths now ship
  with the parser, with the usual aliases (Arial, Liberation Sans, …) folded in.

- **Composite (Type0) font widths** are read from the descendant font's `/W`
  array and `/DW`, instead of through the simple-font `/Widths` path that
  returns zero for them.

- **Word spacing (`Tw`) was ignored**, shrinking every inter-word gap — the same
  signal the layout passes use to find word, column and cell boundaries.

## [0.2.0] - 2026-09-03

### Added

- **Site crawling.** `anymd --crawl --depth 2 -d ./site https://example.dev`
  follows links to a bounded depth, converts each page, and **rewrites internal
  links to the local files** — a mirror whose links still point at the internet
  is not a mirror. A link to a page that was not crawled stays absolute so it
  still works, and a URL inside a fenced code block is left alone, because that
  is content rather than navigation.

  Flags: `--crawl --depth --max-pages --crawl-delay --same-host --include
  --exclude --ignore-robots --sitemap`. Politeness is not optional: robots.txt
  is respected by default with `Crawl-delay` honoured, there is a default
  inter-request delay, and the crawl is same-host unless told otherwise.

  Crawling lives in its own `crawl` package and is unreachable from a
  converter. The guarantee that a converter never touches the network is what
  makes anymd safe to point at untrusted input, so crawling is something a
  caller opts into, never something a conversion can trigger.

- **Sitemap discovery** (`--sitemap auto|only|off`, `crawl.Options.Sitemap`).
  Found via robots.txt `Sitemap:` directives, then `/sitemap.xml`, then
  `/sitemap_index.xml`; handles gzipped sitemaps and sitemap-index fan-out,
  both bounded. Default `auto` seeds the crawl from a sitemap **and** still
  follows links, so it finds pages the seed does not link to. A sitemap is a
  hint, not an authority: its URLs still pass same-host, include/exclude,
  robots.txt and the page cap.

- **`anymd skills install | list | path | uninstall`.** Installs the bundled
  agent skill to `~/.claude/skills/anymd` and `~/.agents/skills/anymd`, so an
  AI coding agent can find it. Embedded with `go:embed`, so it works from a
  bare `go install` with no checkout, and there is exactly one canonical
  `SKILL.md` that cannot drift. It never overwrites silently: identical is
  "already current", modified is a refusal naming `--force`. `uninstall`
  removes only what it installed and keeps a directory holding any foreign file.

- **Flags may follow filenames.** `anymd report.docx -o report.md` — the form
  markitdown's own help documents — used to fail with "stat -o: no such file or
  directory", because Go's flag package stops at the first positional. Python's
  argparse and GNU getopt both permute; Go is the outlier.

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
- **16 converters** (the live list is always `anymd --list`):

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
- PDF figures and form fields are not reconstructed, and almost no tables or
  headings are emitted from PDF; only the text layer is extracted, in
  column-aware reading order.
- HTML that requires JavaScript execution renders as whatever is in the source.
- Encrypted zips and DRM'd EPUBs are refused.
- `.msg` attachments, RTF-compressed bodies and recipient storages are skipped.

[Unreleased]: https://github.com/muthuishere/anymd/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/muthuishere/anymd/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/muthuishere/anymd/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/muthuishere/anymd/releases/tag/v0.1.0
