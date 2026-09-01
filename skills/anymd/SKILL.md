---
name: anymd
description: >-
  Convert any document to Markdown from the command line — read this PDF, DOCX,
  PPTX, XLSX, XLS, EPUB, MSG, HTML, CSV, JSON, RSS, IPYNB or ZIP; extract the
  text of a file so an LLM can read it; turn a folder of documents into Markdown
  for a RAG index; batch-convert a directory; pull a URL down as Markdown; get a
  spreadsheet as a GFM table. Use whenever a file is not plain text and you need
  its contents as Markdown. One static binary, offline, no OCR.
---

# anymd — any document → Markdown

`anymd` turns a document into clean GitHub-flavored Markdown on stdout. Pure Go,
one static binary, **no network while converting**, 15 converters. Reach for it
before writing bespoke parsing code or asking the user to paste contents.

## Install

```sh
# macOS / Linux — one line, verifies the checksum
curl -sSfL https://raw.githubusercontent.com/muthuishere/anymd/main/install.sh | sh

# with Go already present
go install github.com/muthuishere/anymd/cmd/anymd@latest

# Homebrew / Scoop, once the tap and bucket exist
brew install muthuishere/tap/anymd
scoop bucket add muthuishere https://github.com/muthuishere/scoop-bucket && scoop install anymd
```

Check it: `anymd --version`. See what this build can do: `anymd --list`.

## Use it

```sh
# one file to stdout — the default, pipeable
anymd report.docx
anymd deck.pptx | head -60

# write to a file
anymd -o report.md report.docx

# stdin, telling it what the bytes are (there is no filename to sniff)
cat page.html | anymd -t html
curl -s https://example.com/data.csv | anymd -t csv

# a whole tree into a directory, quietly
anymd -r -d build/md --ext .md -q docs/

# several files, concatenated to stdout in input order
anymd a.pdf b.xlsx c.msg

# a URL — fetched, then converted (the only network call anymd ever makes)
anymd https://example.com/report.pdf

# prepend "# Title" when the format carried one
anymd --title minutes.docx

# what converters exist in this build
anymd --list
```

**Flags go BEFORE the file arguments.** This is Go's `flag` package: the first
positional argument stops flag parsing, so `anymd -r docs/ -d out` silently
treats `-d` as a filename and fails. Write `anymd -r -d out docs/`.

### Flags worth knowing

| flag | what it does |
|---|---|
| `-o FILE\|DIR` | write to `FILE`; if it is an existing **directory**, batch into it |
| `-d, --outdir DIR` | batch output directory, created if missing |
| `--ext .md` | output extension in batch mode |
| `-t, --type EXT` | force a format hint (`-t docx`) — essential for stdin |
| `-r, --recursive` | walk directory arguments |
| `--charset NAME` | override the detected text encoding |
| `--max-depth N` | bound container recursion (zip in a zip); default 8 |
| `--title` | prepend `# Title` when the converter found one |
| `-q, --quiet` | drop the per-file progress lines on stderr |
| `--fail-fast` | stop at the first error instead of continuing |
| `--list` / `--version` | registry / build info |

## The contract you can script against

- **stdout is Markdown and nothing else.** Progress, warnings and errors go to
  **stderr**, always, never interleaved. `anymd x.pdf | grep foo` is safe.
- **Exit codes:** `0` everything converted · `1` one or more inputs failed ·
  `2` you called it wrong (bad flag, `-o FILE` with several inputs). Branch on
  these — `1` means the document was bad, `2` means your command was.
- **Batch output is deterministic**, emitted in input order even though the
  work runs on a pool. Safe to diff and commit.
- Colliding basenames in a batch get suffixed (`report.md`, `report-1.md`),
  never silently overwritten.

## Formats

`csv/tsv` · `docx` · `epub` · `html` · images (dimensions + EXIF) · `ipynb` ·
`json` · `msg` (Outlook) · `pdf` · `pptx` (incl. speaker notes) · `rss/atom` ·
`xls` · `xlsx` · `zip` (recursive) · plaintext fallback.

Spreadsheets and tables come out as real GFM pipe tables, byte-identical
whether they came from a spreadsheet or a Word document.

`anymd --list` is always the authoritative answer for the installed build.

## Limits — say these plainly, do not work around them

- **No OCR.** A scanned PDF or a photo of a page returns a clear
  `ErrNoTextLayer` error, not empty output and not invented text. If you get
  that error, the document has no text layer — tell the user, do not guess at
  the contents.
- **No audio or video transcription.** `.mp3` / `.wav` / `.m4a` are not
  supported at all.
- **No image captioning.** Images yield pixel dimensions and EXIF metadata;
  the picture itself is never described.
- **No network at convert time.** A converter will not fetch a remote image,
  stylesheet, or linked article. Only an explicit `http(s)` argument is fetched.
- Binary input with no matching converter is an **error (exit 1)**, never
  silent garbage. Anything that decodes as UTF-8 text falls through to
  plaintext.
- `.xls` formula *results* are a placeholder; PDF column layout, figures and
  form fields are not reconstructed; encrypted zips and DRM'd EPUBs are refused.

## When to use it

Use `anymd` when you need the **text** of a non-plain-text file: reading a
user's PDF/DOCX/XLSX, feeding documents into an LLM context or a RAG index,
diffing a document in CI, or converting a folder for a docs pipeline.

## When NOT to use it

- The file is already Markdown or plain text — just read it.
- You need OCR, transcription, or an image description — anymd will not do it,
  and no flag changes that. Use a different tool for that step.
- You need pixel-faithful rendering, layout geometry, or a screenshot — anymd
  extracts text and structure, not appearance.
- You need to **write** a document (produce a `.docx`/`.xlsx`) — anymd is
  one-directional.
- You are inside Go code — import the library instead of shelling out:
  `anymd.ConvertFile("report.docx")`.

## In Go

```go
import "github.com/muthuishere/anymd"

res, err := anymd.ConvertFile("report.docx")
// res.Title    — when the format carries one
// res.Markdown — the GFM body
```

Also `anymd.ConvertBytes(b, info)`, `anymd.Convert(reader, info)`, and
`anymd.New()` for a registry you can extend with your own converter.

## If something goes wrong

- `no converter accepted this stream` → give it a hint: `-t docx`. On stdin
  there is no filename, so a hint is often required.
- `--version` prints `dev` → a `go install` or source build; that is normal and
  not a bug.
- Mojibake in the output → `--charset` overrides the detected encoding.
- Reading a hostile/untrusted document? Run it in a separate process with a
  memory limit and a timeout. anymd bounds its allocations but is not a sandbox
  — see `SECURITY.md` in the repo.
