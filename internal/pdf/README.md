# internal/pdf

Vendored copy of `github.com/ledongthuc/pdf`
(commit `5959a4027728`, itself a fork of Russ Cox's `rsc.io/pdf`), BSD-3-Clause —
see `LICENSE`.

## Why vendored rather than imported

Upstream is explicitly not optimized. Its own doc comment says so:

> BUG(rsc): The library makes no attempt at efficiency. A value cache
> maintained in the Reader would probably help significantly.

That is not a micro-optimization. `Page.Content()` calls `Font.Width` once per
glyph, and every call re-reads and re-lexes the font's `/Widths` array out of
the file bytes, because resolving an indirect object goes back to the reader
each time. Text extraction is therefore `O(glyphs x len(Widths))` passes
through the lexer, and PDF became by far anymd's slowest converter — the one
format where we were barely faster than the Python tool we replace.

The fix is the cache upstream describes. It is a change to a hot internal path,
not something an importer can do from outside, so the package lives here.

The full reasoning — including why pdfium, which the sibling CiteNexus project
uses, was the wrong answer here — is
[ADR 0002](../../docs/adr/0002-vendor-the-pdf-parser.md).

## Local changes

| file | change |
|---|---|
| `read.go` | `Reader.objCache`: memoize resolved indirect objects by `objptr`, consulted and filled in `resolve()`. |
| `read.go`, `name.go` | `gofmt -w` only — upstream predates the Go 1.19 doc-comment reformatting, and this repo's CI runs `gofmt -l .` over the whole tree. No code changed. |

Measured on markitdown's `test.pdf` (93 KB), `BenchmarkConvertPdf`:

| | time/op | allocs/op | bytes/op |
|---|---:|---:|---:|
| upstream | 42.6 ms | 1,565,898 | 75 MB |
| vendored | 3.2 ms | 16,168 | 1.9 MB |

Keep this list current: anything else we change goes in the table, so a future
rebase onto upstream is a diff and not an excavation.
