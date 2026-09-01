# anymd

**Any document → Markdown, in pure Go.** One static binary, one `go get`-able library.

```sh
anymd report.docx > report.md
```

`anymd` is a Go-native alternative to Microsoft's [markitdown](https://github.com/microsoft/markitdown).
Same idea — feed it a file, get clean GitHub-flavored Markdown suitable for an LLM
context window, a diff, or a docs pipeline — with a different set of trade-offs:

- **No cgo.** `CGO_ENABLED=0` builds. Nothing to link, nothing to `apt install`.
- **No Python.** No interpreter, no virtualenv, no wheels that need a C compiler.
- **No native library to ship.** No poppler, no libmagic, no LibreOffice subprocess.
- **Library first.** The CLI is a thin shell over the same public API you import.
- **Cross-compiles everywhere Go does.** `GOOS=windows GOARCH=arm64 go build` and you're done.

That is the whole pitch: `go install` it, drop the binary on a CI runner or into a
scratch container, and it works.

---

## Install

Binary:

```sh
go install github.com/muthuishere/anymd/cmd/anymd@latest
```

Library:

```sh
go get github.com/muthuishere/anymd
```

Or build from source:

```sh
git clone https://github.com/muthuishere/anymd && cd anymd
make build        # stamps the version from `git describe`
make release      # cross-compiles darwin/linux/windows × amd64/arm64 into dist/
```

## Quickstart

```sh
# 1. one file to stdout — the unix default, pipeable
anymd notes.docx | head -40

# 2. stdin, with a hint about what the bytes are
curl -s https://example.com/data.csv | anymd -t csv

# 3. batch a tree into a directory, quietly, in deterministic order
anymd -r docs/ -d build/md --ext .md -q
```

As a library:

```go
package main

import (
	"fmt"
	"log"

	"github.com/muthuishere/anymd"
)

func main() {
	res, err := anymd.ConvertFile("report.docx")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.Title)    // "Q3 Report", when the format carries one
	fmt.Println(res.Markdown) // the GFM body
}
```

There is also `anymd.ConvertBytes(b, info)` and `anymd.Convert(reader, info)` for
streams you already hold, plus `anymd.New()` for a private registry with your own
converters (see [Extending it](#extending-it)).

## CLI

```
anymd [flags] [file|url ...]
```

| flag | what it does |
|---|---|
| `-o FILE\|DIR` | write to `FILE`; if it exists as a **directory**, batch into it |
| `-d, --outdir DIR` | batch output directory, created if missing |
| `--ext .md` | output extension in batch mode |
| `-t, --type EXT` | force an extension hint (`-t docx`), mainly for stdin |
| `-r, --recursive` | walk directories given as inputs |
| `--charset NAME` | override the detected text encoding |
| `--max-depth N` | bound container recursion (a zip inside a zip); default 8 |
| `--keep-data-uris` | keep base64 images inline as `data:` URIs instead of dropping them |
| `--title` | prepend `# Title` when the converter found one and the body has no h1 |
| `-q, --quiet` | suppress the per-file progress lines on stderr |
| `--fail-fast` | stop at the first error (default: continue, report at the end) |
| `--insecure` | skip TLS verification when fetching a URL |
| `--list` | print the registered converters in dispatch order |
| `--version` | version, commit and Go version |

**Streams.** Markdown goes to **stdout**. Progress, warnings and errors go to
**stderr**, always — they are never interleaved into stdout. That is what makes
`anymd x.pdf | grep …` safe, and it is precisely where naive converters fail.

**Exit codes.** `0` everything converted · `1` one or more inputs failed · `2` you
called it wrong (bad flag, `-o FILE` with several inputs). Distinct codes let a CI
script branch on "the document was bad" versus "the invocation was bad".

**Batch.** Inputs are converted on a worker pool sized to `runtime.NumCPU()`, but
output is emitted in **input order** — deterministic and diffable beats marginally
faster. A batch run ends with a stderr summary: `converted 12, failed 1`.

**URLs.** An `http://` or `https://` argument is fetched with a 30-second timeout
and a real `User-Agent`; the extension and MIME hints come from the response
`Content-Type` and the final (post-redirect) URL. **This is the only place in the
entire project that touches the network** — converters are offline by construction,
so a document can never make `anymd` phone home. `--insecure` skips TLS verification
and exists for self-signed internal hosts; do not point it at the public internet.

## Supported formats

| Format | Extensions | Extracted | Not extracted |
|---|---|---|---|
| Plain text / Markdown | `.txt` `.text` `.md` `.markdown` `.log` | verbatim passthrough (last-resort fallback) | — |
| CSV / TSV | `.csv` `.tsv` `.tab` | delimiter sniffing → GFM pipe table | formulas (there are none), cell types |
| Excel | `.xlsx` `.xlsm` `.xltx` `.xltm` | every sheet as a heading + table, computed cell values | charts, images, macros, pivot tables |
| Excel (legacy) | `.xls` `.xlt` `.xlm` `.xlw` | same output as `.xlsx`, byte-identical on the same workbook | formula results (the BIFF reader returns a placeholder), Excel's own date formats |
| Word | `.docx` | headings, paragraphs, lists, tables, links, image alt text, core-properties title | image pixels, comments, tracked changes |
| PDF | `.pdf` | text layer, page by page | **no OCR** — a scanned page yields nothing; layout columns, figures, forms |
| HTML | `.html` `.htm` `.xhtml` | headings, lists, tables, links, code, `<title>` | scripts, styles, anything requiring JS execution, remote assets |
| Feeds | `.rss` `.atom` `.xml` `.rdf` | channel/feed title, per-entry title, date, link, summary | full-article fetch (that would be a network call) |
| JSON | `.json` | pretty-printed, fenced as a code block | schema inference, semantic flattening |
| Notebooks | `.ipynb` | markdown cells, code cells as fenced blocks, text outputs | rendered plots and images, widget state |
| EPUB | `.epub` | spine order, per-chapter HTML → Markdown, metadata title | cover art, footnote back-links, DRM'd books |
| ZIP | `.zip` | recursive conversion of each member, bounded by `--max-depth` | encrypted archives |
| PowerPoint | `.pptx` | slide-by-slide shape text, tables, charts, image alt text, speaker notes | image pixels, animations, themes, layout geometry |
| Images | `.jpg` `.jpeg` `.png` `.gif` `.webp` `.tiff` `.bmp` | pixel dimensions and EXIF metadata (capture time, camera, lens, exposure, GPS, description, rights) | **no OCR, no captioning** — the pixels are never described |
| Outlook mail | `.msg` | subject, From/To/Cc/Date table, HTML or plain body | attachments, RTF-compressed bodies, recipient storages |

Anything with no matching converter that still decodes as UTF-8 text falls through
to the plaintext converter; genuinely binary input with no converter is an error
(exit 1), never silent garbage. `anymd --list` prints the live registry in dispatch
order, which is always the authoritative answer for the build you have.

### Verified against markitdown's own corpus

`anymd` is checked against the 31 test files in Microsoft markitdown's own test
suite, not only against fixtures we wrote ourselves — fixtures only ever assert
what we already thought to build. **26 of 31 convert.** The five that do not are
each a deliberate scope decision, not a bug:

| file | outcome |
|---|---|
| `MEDRPT-…_medical_report_scan.pdf` | `ErrNoTextLayer` — a scan with no text layer. We return a named error instead of empty output, so a caller can tell "nothing to extract" from "extraction failed". |
| `test.m4a` `test.mp3` `test.wav` | audio: transcription needs a model, which this project does not ship |
| `random.bin` | correctly refused rather than emitted as garbage |

Output is also checked against markitdown's own canary assertions
(`PPTX_TEST_STRINGS`): all present, including the image alt text and the chart
content that live outside the slide XML.

### Deliberate omissions

markitdown's most impressive-looking converters are the ones that call something
else. `anymd` does not ship them, on purpose:

- **No OCR.** A scanned PDF or a photo of a page comes back empty, not hallucinated.
- **No audio/video transcription.** No Whisper, no speech service.
- **No LLM image captioning.** No API key, no per-file cost, no data leaving the box.
- **No network fetching at convert time.** A converter never resolves a remote
  image, stylesheet, or linked article.

Each of those would mean a model, a service, a key, or a native dependency — and
would break "one static binary that works offline". If you want them, they belong
in *your* pipeline, wrapped around `anymd`, where you control the cost and the
data boundary.

## How it differs from markitdown

|  | markitdown | anymd |
|---|---|---|
| Runtime | Python 3 + wheels | single static Go binary |
| Use as a library | Python only | Go, and the binary is a thin wrapper on it |
| Native deps | several, varying by extra | none — `CGO_ENABLED=0` |
| OCR / transcription / LLM captions | yes, via services and extras | deliberately absent |
| Network at convert time | yes (some converters) | never; only the CLI's explicit URL argument |
| Batch | one file per invocation | worker pool, deterministic input-order output |
| Exit codes | coarse | `0` / `1` / `2`, scriptable |

The *design* is a port, and gladly so: the converter registry, the `StreamInfo`
"hints plus sniffing" dispatch, and the first-`Accepts`-wins ordering are markitdown's
ideas, proven at scale. The deterministic GFM emitters (`internal/mdutil`: one
`Table`, one `Heading`, one `CodeBlock`, one `Join`) come from CiteNexus's
emit-markdown capability, where the requirement was that a table from a spreadsheet
and a table from a Word document be **byte-identical**. That property is what makes
`anymd` output safe to commit and diff.

## Extending it

A converter is two methods. Implement them, register the converter, done:

```go
package main

import (
	"io"
	"strings"

	"github.com/muthuishere/anymd"
)

type TodoConverter struct{}

func (c *TodoConverter) Name() string  { return "todo" }
func (c *TodoConverter) Priority() int { return anymd.PrioritySpecific }

// Accepts must be cheap: hints and magic bytes only, never a full parse.
func (c *TodoConverter) Accepts(r io.ReadSeeker, info anymd.StreamInfo, opts *anymd.Options) bool {
	return info.HasExt(".todo")
}

func (c *TodoConverter) Convert(r io.ReadSeeker, info anymd.StreamInfo, opts *anymd.Options) (anymd.Result, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return anymd.Result{}, err
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, "- [ ] "+line)
		}
	}
	return anymd.Result{Markdown: strings.Join(out, "\n") + "\n"}, nil
}

func main() {
	e := anymd.New()          // all built-ins
	e.Register(&TodoConverter{}) // yours, ahead of the fallback
	res, _ := e.ConvertFile("chores.todo", nil)
	print(res.Markdown)
}
```

Notes:

- `Priority()` orders dispatch — `PrioritySpecific` (0) for a unique magic number
  or extension, `PriorityGeneric` (10) for a family that would shadow a specific
  format, `PriorityFallback` (100) for the one text catch-all. Ties break by
  registration order, so registering at a lower priority overrides a built-in.
- A converter that `Accepts` and then fails is a **hard error**. The engine does
  not fall through to a catch-all that would emit plausible-looking garbage.
- In-tree converters render through `internal/mdutil` (`Table`, `Heading`,
  `CodeBlock`, `Join`) so every emitter agrees byte-for-byte. It is an internal
  package, so an out-of-tree converter emits its own GFM — match the same shape.
- Container formats must recurse via `opts.Recurse(reader, info)`, never by
  constructing a new `Engine` — that is what makes `--max-depth` real.

The full rules for in-tree converters live in [`CONTRACT.md`](CONTRACT.md).

## Security posture

Every converter is a parser pointed at bytes someone else chose. So:

- **Never panics.** Malformed input is an `error`, not a crash. Lengths, indices,
  and offsets read out of a document are treated as attacker-controlled.
- **Bounded memory and recursion.** Container nesting is capped by
  `Options.MaxDepth` (default 8) and enforced centrally by `Options.Recurse`.
- **No network, no subprocess, no shell.** Converters cannot fetch a remote asset
  or exec anything. The single exception is the CLI's explicit URL argument, which
  is a fetch you asked for, before any converter sees a byte.
- **No cgo**, so there is no memory-unsafe parser in the dependency graph.

Found a way to panic one of the converters? That is a bug worth a report.

## License

MIT © 2026 Muthukumaran Navaneethakrishnan. See [LICENSE](LICENSE).
