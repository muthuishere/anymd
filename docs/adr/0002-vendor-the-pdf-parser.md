# 0002 — Vendor the PDF parser as `internal/pdf` and cache resolved objects

Status: **Accepted** · 2026-09-02

## Context

PDF was, by a wide margin, anymd's worst format. Against markitdown it won every
other format by 8×–37× and PDF by **1.7×** — 45.69 ms against 77.27 ms on a
93 KB document. `bench/README.md` recorded that as a fact of life:

> Both tools are dominated by the PDF parser itself (pdfium here, pdfminer.six
> there) … Do not expect order-of-magnitude wins on PDF-heavy corpora.

That sentence was wrong in both halves, and the way it was wrong is the point of
this record.

**It named an engine anymd does not use.** anymd has never linked pdfium — it
could not, under [0001](0001-pure-go-no-cgo-no-network.md). It parses PDFs with
`github.com/ledongthuc/pdf`, a fork of Russ Cox's `rsc.io/pdf`. The claim was
never measured; it was assumed, and then it was published.

**It treated a self-inflicted cost as an inherent one.** `rsc.io/pdf` is a
readable reference implementation that deliberately skips optimization. Its own
doc comment says so:

> BUG(rsc): The library makes no attempt at efficiency. A value cache maintained
> in the Reader would probably help significantly.

Resolving an indirect object therefore goes back to the file bytes and re-lexes
it, every time. That interacts badly with one specific line in `Page.Content()`:
it asks the current font for a glyph's width **once per glyph**, and
`Font.Width` reads `/Widths` — normally an indirect reference to an array of a
few hundred numbers. Laying out a page was `O(glyphs × len(Widths))` passes
through the lexer. For a 93 KB document: **1,565,898 allocations and 75 MB**.

Investigating this started from a sibling project, CiteNexus, which needs
word-level bounding boxes and settled on **pdfium** (`pdfium-render` in Rust,
`pypdfium2` in Python; PyMuPDF ruled out as AGPL against an Apache-2.0 core).
Its port spike had already assessed the pure-Go field and written it off —
"`ledongthuc/pdf` is thin, `pdfcpu` is not layout-faithful, `unipdf` is
commercial, `go-fitz` is cgo and AGPL". That assessment is fair about output
fidelity. It is not an explanation for 45 ms, and adopting its conclusion would
have meant paying [0001](0001-pure-go-no-cgo-no-network.md)'s price to fix a
problem that was never about the parser's design.

The options, then:

1. **Adopt pdfium via `go-pdfium`/Wazero.** Better extraction on hard layouts,
   keeps `CGO_ENABLED=0`. Costs a multi-megabyte WASM blob in the binary, a WASM
   runtime's startup on every conversion, and a rewrite of a working converter —
   to solve a quadratic that pdfium merely does not have.
2. **Upstream the cache.** Correct, and we should still try. But it is a fork of
   an archived `rsc.io` package; the fix cannot be waited on.
3. **Vendor the package and add the cache.**

## Decision

**`github.com/ledongthuc/pdf` is vendored into `internal/pdf` (BSD-3, license
retained), with one local change: a `Reader.objCache` memoizing resolved
indirect objects by `objptr`.**

It is a change to a hot internal path — `resolve()` is unexported — so no
importer could make it from outside. Correctness rests on the reader being
read-only, an `objptr` identifying exactly one object in one file, and
`resolve()` being deterministic: a cache hit returns what a miss would have
parsed. The cache is bounded by the object count the xref table already bounds.

Verified byte-identical output on all seven corpus PDFs, before and after.

`internal/pdf/README.md` carries the provenance and a table of every local
change, so a future rebase onto upstream is a diff rather than an excavation.

## Consequences

Measured on markitdown's corpus, same in-process harness:

| | before | after |
|---|---:|---:|
| `BenchmarkConvertPdf` | 42.6 ms · 1,565,898 allocs · 75 MB | **2.59 ms · 16,605 allocs · 2.2 MB** |
| test.pdf | 43.77 ms | **2.60 ms** (16.8×) |
| REPAIR-…_multipage.pdf | 13.34 ms | **1.34 ms** (10.0×) |
| vs markitdown, in-process | 1.7× | **22×** |
| vs markitdown, CLI | 7.9× | **46×** |

The win tracks embedded subset fonts, not page count: documents whose text is
drawn in a base-14 font never had a large `/Widths` array to re-lex and are
unchanged. Binary size is unchanged at 16.6 MB; `go.mod` loses a dependency.

What this costs:

- **We now maintain a parser.** Upstream's bug fixes arrive by rebase, not by
  `go get -u`, and a CVE in it is ours to notice. The mitigating factor is that
  upstream is a fork of an archived package and was not shipping fixes anyway.
- **`internal/pdf` is excluded from `golangci-lint`.** Linting vendored code
  reports upstream's decisions as our defects and turns every fix into a rebase
  conflict. The code we own there is the cache, and it is reviewed as a diff.
- **Fidelity is unchanged.** This bought speed, nothing else. pdfium still
  extracts better on hard layouts, and 0001's escape hatch is still the answer
  if that ever becomes the binding constraint.

The process lesson is cheaper than the fix was: a benchmark that publishes an
*explanation* alongside a number needs the explanation checked as carefully as
the number. The 45.69 ms was measured and correct. The sentence beside it was
neither, and it shipped in the first release — turning a ten-line bug into a
documented limitation of the language.
