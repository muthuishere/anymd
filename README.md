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

# 4. opt in to a vision model to caption images and read scanned pages
anymd --llm scan.pdf
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
| `--cache` | reuse a previous conversion of identical bytes (off by default) |
| `--no-cache` | force a fresh conversion; wins over `--cache` |
| `--cache-dir DIR` | cache location (default `os.UserCacheDir()/anymd`) |
| `--llm` | enable LLM image captioning — **off by default**, see [LLM features](#llm-features) |
| `--llm-config PATH` | config file; default `~/.config/anymd/anymdconfig.json` |
| `--llm-model NAME` | override the vision model from the config file |
| `--llm-base-url URL` | override the endpoint (a local Ollama or vLLM works) |
| `--llm-timeout D` | bound one model call, e.g. `30s` (default 60s) |
| `--llm-transcribe` | also transcribe audio — a separate endpoint and a separate charge |
| `--llm-transcribe-model NAME` | speech model (default `whisper-1`) |

There are also `config` and `cache` subcommands: `anymd config path`, `anymd config show`,
`anymd config init`. See [LLM features](#llm-features).

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
`Content-Type` and the final (post-redirect) URL. **Unless you pass `--llm`, this is
the only place in the entire project that touches the network** — converters are
offline by construction, so a document can never make `anymd` phone home.
`--insecure` skips TLS verification and exists for self-signed internal hosts; do
not point it at the public internet.

## Supported formats

| Format | Extensions | Extracted | Not extracted |
|---|---|---|---|
| Plain text / Markdown | `.txt` `.text` `.md` `.markdown` `.log` | verbatim passthrough (last-resort fallback) | — |
| CSV / TSV | `.csv` `.tsv` `.tab` | delimiter sniffing → GFM pipe table | formulas (there are none), cell types |
| Excel | `.xlsx` `.xlsm` `.xltx` `.xltm` | every sheet as a heading + table, computed cell values | charts, images, macros, pivot tables |
| Excel (legacy) | `.xls` `.xlt` `.xlm` `.xlw` | same output as `.xlsx`, byte-identical on the same workbook | formula results (the BIFF reader returns a placeholder), Excel's own date formats |
| Word | `.docx` | headings, paragraphs, lists, tables, links, image alt text, core-properties title; embedded image captions with `--llm` | comments, tracked changes |
| PDF | `.pdf` | text layer, page by page, in column-aware reading order; with `--llm`, pages with no text layer are read by a vision model | tables, headings, figures, form fields |
| HTML | `.html` `.htm` `.xhtml` `.xht` | headings, lists, tables, links, code, `<title>` | scripts, styles, anything requiring JS execution, remote assets |
| Feeds | `.rss` `.atom` `.xml` `.rdf` | channel/feed title, per-entry title, date, link, summary | full-article fetch (that would be a network call) |
| JSON | `.json` | pretty-printed, fenced as a code block | schema inference, semantic flattening |
| Notebooks | `.ipynb` | markdown cells, code cells as fenced blocks, text outputs | rendered plots and images, widget state |
| EPUB | `.epub` | spine order, per-chapter HTML → Markdown, metadata title | cover art, footnote back-links, DRM'd books |
| ZIP | `.zip` | recursive conversion of each member, bounded by `--max-depth` | encrypted archives |
| PowerPoint | `.pptx` | slide-by-slide shape text, tables, charts, image alt text, speaker notes; embedded image captions with `--llm` | animations, themes, layout geometry |
| Images | `.jpg` `.jpeg` `.png` `.gif` `.webp` `.tiff` `.bmp` | pixel dimensions and EXIF metadata (capture time, camera, lens, exposure, GPS, description, rights); a caption with `--llm` | without `--llm`, the pixels are never described |
| Audio | `.mp3` `.m4a` `.wav` `.flac` `.ogg` `.opus` … | **only with `--llm-transcribe`** — the spoken content, transcribed | speaker diarization, timestamps; without the flag the format is not accepted at all |
| Outlook mail | `.msg` | subject, From/To/Cc/Date table, HTML or plain body | attachments, RTF-compressed bodies, recipient storages |

Anything with no matching converter that still decodes as UTF-8 text falls through
to the plaintext converter; genuinely binary input with no converter is an error
(exit 1), never silent garbage. `anymd --list` prints the live registry in dispatch
order, which is always the authoritative answer for the build you have.

### Benchmarks vs markitdown

Measured on the 31 files in markitdown's own test suite. Full method and the
reproducible script: [`bench/`](bench/).

| | anymd | markitdown 0.1.5 |
|---|---:|---:|
| docx (in-process) | **0.59 ms** | 17.86 ms |
| xlsx (in-process) | **0.64 ms** | 7.35 ms |
| pdf (in-process) | **3.44 ms** | 74.47 ms |
| 385K HTML (in-process) | **11.55 ms** | 104.15 ms |
| CLI startup overhead | **none** | 309 ms per invocation |
| Peak RSS (385K HTML) | **27 MB** | 160 MB |
| Install footprint | **15 MB** binary | 318 MB venv |
| Files with substantive output | 19/31 | 19/31 |

One honest caveat: **the benchmark is the default, offline build**. With
`--llm` the wall clock is whatever the model takes, and the comparison stops
being about parsers.

PDF used to be a second caveat — 45.69 ms and a 1.7× win, explained away as both
tools being parser-bound. That explanation was wrong. The cost was our own PDF
library resolving the same font `/Widths` array out of the file bytes once per
glyph, so page layout was quadratic in glyph count. It is vendored as
`internal/pdf` now, with the object cache upstream's own doc comment recommends;
output is byte-identical and the worst corpus document went from 43.77 ms to
2.60 ms. [`bench/`](bench/) has the before/after table, and
[ADR 0002](docs/adr/0002-vendor-the-pdf-parser.md) the reasoning.

Where anymd refuses a file, markitdown often returns an empty string and exit 0
— on the scanned PDF and on both test images. For an ingestion pipeline that is
the worse failure: silently indexing nothing looks exactly like a document that
had no text. anymd returns `ErrNoTextLayer` so you can route it to OCR.

### Verified against markitdown's own corpus

`anymd` is checked against the 31 test files in Microsoft markitdown's own test
suite, not only against fixtures we wrote ourselves — fixtures only ever assert
what we already thought to build. **26 of 31 convert.** The five that do not are
each a deliberate scope decision, not a bug:

| file | outcome |
|---|---|
| `MEDRPT-…_medical_report_scan.pdf` | `ErrNoTextLayer` — a scan with no text layer. We return a named error instead of empty output, so a caller can tell "nothing to extract" from "extraction failed". `--llm` reads it with a vision model. |
| `test.m4a` `test.mp3` `test.wav` | audio: needs a speech model. `--llm-transcribe` converts these; the default offline build declines them. |
| `random.bin` | correctly refused rather than emitted as garbage |

Output is also checked against markitdown's own canary assertions
(`PPTX_TEST_STRINGS`): all present, including the image alt text and the chart
content that live outside the slide XML.

### What the default build will not do

markitdown's most impressive-looking converters are the ones that call something
else. In `anymd` every one of those is **opt-in behind `--llm`**, and the default
build does none of them:

- **No OCR.** A scanned PDF comes back as `ErrNoTextLayer`, not hallucinated text.
- **No audio transcription.** An audio file is not even accepted.
- **No image captioning.** Images give dimensions and EXIF, nothing more.
- **No network of any kind at convert time.** A converter never resolves a remote
  image, stylesheet, or linked article. The CLI's explicit URL argument is the
  only fetch, and it happens before any converter sees a byte.

That is the shape of the trade, and it is why the default is off rather than
"on if a key happens to be in the environment": turning it on costs money, sends
your documents to a third party, and makes the output non-deterministic. Those
are decisions, not defaults. See [LLM features](#llm-features).

The constraint underneath all four — pure Go, and no network the caller did not
ask for — is written down with what it costs in
[ADR 0001](docs/adr/0001-pure-go-no-cgo-no-network.md). The rest of
[`docs/adr/`](docs/adr/) covers the decisions whose result you can see in the
code but whose reason you cannot.

## LLM features

Everything in this section is **off unless you pass `--llm`**. Without it anymd
makes no network calls during conversion at all — that is the guarantee that
lets you point it at a document someone emailed you.

### What `--llm` turns on

| | what it does | cost |
|---|---|---|
| Images | a prose caption under the image, in `.png`/`.jpg`/… and embedded in `.docx` and `.pptx` | **one model call per distinct image** (identical images — a logo on every slide — are captioned once) |
| Scanned PDFs | pages with no text layer are read by the vision model instead of returning `ErrNoTextLayer` | one call per page that has no text |
| Audio (`--llm-transcribe`) | `.mp3`/`.m4a`/`.wav`/… become their spoken content | **one call per file** |

```sh
# caption every image in a deck
anymd --llm deck.pptx

# read a scanned PDF
anymd --llm scan.pdf

# transcribe an interview — a separate endpoint and a separate charge,
# hence a separate flag
anymd --llm --llm-transcribe interview.m4a

# a local model: no key leaves the box, no per-call cost
anymd --llm --llm-base-url http://localhost:11434/v1 --llm-model llava photo.jpg
```

**Be deliberate about the bill.** A 60-slide deck with a logo on every slide and
40 distinct figures is 41 calls, not 100 — but a directory of 200 such decks is
several thousand. `--llm` is per-invocation for exactly this reason; there is no
environment variable that turns it on globally.

A model failure is never a document failure: a timeout, a rate limit or an
outage costs you the caption, and the rest of the output — text, tables,
dimensions, EXIF — is still emitted. `--llm-timeout` bounds a single call so a
slow model cannot stall a whole batch.

### The config file

`~/.config/anymd/anymdconfig.json` (or `$XDG_CONFIG_HOME/anymd/`). Every string
field supports `${VAR}` interpolation from the environment, which is how a key
stays out of the file:

```json
{
  "model": "openai/gpt-4o-mini",
  "base_url": "https://openrouter.ai/api/v1",
  "api_key": "${OPENROUTER_API_KEY}",
  "retries": 2,
  "timeout_ms": 60000
}
```

An unset `${VAR}` is an **error**, not an empty string — silently expanding a
missing key to `""` surfaces later as a confusing 401 from the provider. The
error names the variable, never a value.

```sh
anymd config path    # where is it
anymd config init    # write a commented starter there, mode 0600, never overwriting
anymd config show    # what does it resolve to — with every secret redacted
```

`config show` prints `api_key: <set from ${OPENROUTER_API_KEY}>`, never the key,
not even a masked prefix. (A masked key still leaks its length and its prefix,
and it teaches people that showing part of a key is fine.) The same redaction
applies to header values and to credentials embedded in a proxy URL.

### Precedence

**explicit flag > config file > environment > default.**

So `--llm-model llava` beats `"model"` in the file, which beats the built-in
default. The API key is the one thing with no flag — by design; a key on a
command line ends up in your shell history and in `ps`. It comes from
`${VAR}` in the config file, or directly from `OPENROUTER_API_KEY`,
`OPENAI_API_KEY` or `ANTHROPIC_API_KEY`. With `--llm` and no key resolvable,
anymd exits `2` and tells you which variable to set.

Transcription reads `OPENAI_API_KEY`, `OPENROUTER_API_KEY` or `LLM_API_KEY`,
and posts to `<base_url>/audio/transcriptions` — an OpenAI-shaped endpoint. If
your `base_url` points at an aggregator that does not implement it, override it
for that run with `--llm-base-url`.

### Beside markitdown

```python
# markitdown (Python)
from markitdown import MarkItDown
from openai import OpenAI

md = MarkItDown(llm_client=OpenAI(), llm_model="gpt-4o")
result = md.convert("document_with_images.pdf")
print(result.text_content)
```

```sh
# anymd (CLI)
anymd --llm --llm-model gpt-4o document_with_images.pdf
```

```go
// anymd (library)
d := llm.New(llm.Config{Model: "gpt-4o"})
res, err := anymd.Default().ConvertFile("document_with_images.pdf",
    &anymd.Options{Describer: d})
```

The library takes an interface, not an SDK object: anything that can look at
bytes and return a description satisfies `anymd.Describer`, including a local
model, a hosted API, or a stub in your tests. `github.com/muthuishere/anymd/llm`
is one implementation of it, not the only way in.

## How it differs from markitdown

|  | markitdown | anymd |
|---|---|---|
| Runtime | Python 3 + wheels | single static Go binary |
| Use as a library | Python only | Go, and the binary is a thin wrapper on it |
| Native deps | several, varying by extra | none — `CGO_ENABLED=0` |
| OCR / transcription / LLM captions | yes, via services and extras | opt-in behind `--llm`, never by default |
| Network at convert time | yes (some converters) | never, unless you pass `--llm`; otherwise only the CLI's explicit URL argument |
| Model configuration | Python SDK object in code | a config file with `${VAR}` interpolation, or flags |
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
  or exec anything. There are exactly two exceptions, and both are things you
  asked for out loud: the CLI's explicit URL argument, which is fetched before
  any converter sees a byte, and `--llm`, which sends image or audio bytes to the
  endpoint you configured. Neither can be triggered by the content of a document.
- **Keys are never printed.** Not in an error, not in `config show`, not masked.
  A key is read from the environment (or a `${VAR}` reference in the config file)
  and used only as an `Authorization` header. `anymd config init` writes mode
  `0600` and refuses to overwrite.
- **No cgo**, so there is no memory-unsafe parser in the dependency graph.

Found a way to panic one of the converters? That is a bug worth a report.

## License

MIT © 2026 Muthukumaran Navaneethakrishnan. See [LICENSE](LICENSE).
