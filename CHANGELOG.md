# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Versioning note: while the major version is `0`, the Go API (`Converter`,
`StreamInfo`, `Options`, `Result`) may change in a minor release. The **CLI
contract** — stdout is Markdown, stderr is diagnostics, exit `0`/`1`/`2` — is
treated as stable from `0.1.0` onward.

## [Unreleased]

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
