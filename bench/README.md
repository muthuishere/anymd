# Benchmarks

Reproducible comparison of `anymd` against Microsoft's
[markitdown](https://github.com/microsoft/markitdown), the Python tool this
project is an alternative to.

Run it yourself: `./bench/run.sh` (needs `uv`, `hyperfine`, and Go).

Every number below came from that script. Where markitdown wins, it says so.

## Environment

| | |
|---|---|
| Machine | Apple M5 Pro (darwin/arm64), macOS 25.4 |
| anymd | v0.1.0, Go 1.26 |
| markitdown | 0.1.5, Python 3.12.13, `markitdown[all]` (pdfminer.six 20260107) |
| Corpus | the 31 files in markitdown's own test suite |

## Speed — in-process (the fair library comparison)

Mean of 10 runs after a warm-up. This excludes interpreter startup, so it is
the honest number for a long-running service.

| file | anymd | markitdown | speedup |
|---|---:|---:|---:|
| test.epub | 0.09 ms | 3.06 ms | **34×** |
| test.docx | 0.47 ms | 17.26 ms | **37×** |
| test.xlsx | 0.53 ms | 7.61 ms | **14×** |
| test.pptx | 0.90 ms | 8.68 ms | **9.6×** |
| test.pdf | 3.22 ms | 73.65 ms | **23×** |
| test_wikipedia.html (385K) | 11.18 ms | 102.24 ms | **9.1×** |

### PDF used to be the exception — it isn't any more

An earlier revision of this file reported PDF at **45.69 ms, a 1.7× win**, and
explained it away as "both tools are dominated by the PDF parser itself". That
explanation was wrong twice over, and it is worth writing down why, because it
is the shape of mistake a benchmark exists to catch.

It named the wrong engine: anymd has never used pdfium. It parses PDFs with
`ledongthuc/pdf`, a fork of Russ Cox's `rsc.io/pdf`, now vendored as
`internal/pdf`. And it treated the cost as inherent when it was self-inflicted.
That library ships with no cache for resolved indirect objects — its own doc
comment says "the library makes no attempt at efficiency; a value cache
maintained in the Reader would probably help significantly" — while
`Page.Content()` asks for a glyph's width once per glyph, and each of those
asks re-reads and re-lexes the font's `/Widths` array from the file bytes.
Laying out a page was `O(glyphs x len(Widths))` trips through the lexer: for a
93 KB document, **1,565,898 allocations and 75 MB**.

Adding the cache upstream describes is ~10 lines and leaves output
byte-identical on all seven corpus PDFs. Same in-process harness, before and
after:

| file | before | after | |
|---|---:|---:|---:|
| test.pdf | 43.77 ms | 2.60 ms | **16.8×** |
| REPAIR-2022-INV-001_multipage.pdf | 13.34 ms | 1.34 ms | **10.0×** |
| RECEIPT-2024-TXN-98765_retail_purchase.pdf | 0.90 ms | 0.51 ms | 1.8× |
| movie-theater-booking-2024.pdf | 0.50 ms | 0.46 ms | — |
| SPARSE-2024-INV-1234_borderless_table.pdf | 0.48 ms | 0.44 ms | — |
| masterformat_partial_numbering.pdf | 0.12 ms | 0.11 ms | — |

The win tracks embedded fonts, not page count: documents with subset fonts and
large `/Widths` arrays were paying the quadratic, and documents drawing text in
a base-14 font never were. `BenchmarkConvertPdf` fell from 42.6 ms /
1,565,898 allocs / 75 MB to 2.59 ms / 16,605 allocs / 2.2 MB.

The decision and its trade-offs are recorded in
[ADR 0002](../docs/adr/0002-vendor-the-pdf-parser.md).

For the record, the parser choice is still real and still costs us something:
CiteNexus, which needs word-level bounding boxes, uses pdfium (`pdfium-render`
in Rust, `pypdfium2` in Python — PyMuPDF was ruled out as AGPL). pdfium extracts
better than we do on hard layouts. It is also a C library, and anymd's whole
proposition is one static `CGO_ENABLED=0` binary, so it is not free to adopt.
`klippa/go-pdfium` running pdfium under a pure-Go WASM runtime is the option
that would keep that property; it is not on the table while pure Go is this
fast, but it is the door out if fidelity ever beats speed.

## Speed — CLI end-to-end

markitdown pays **290 ms** of Python import and init on *every* invocation.
That cost is real for scripts and shell pipelines, and irrelevant to a
long-running service — which is why both tables are here.

| file | anymd | markitdown | speedup |
|---|---:|---:|---:|
| test.docx | 6.1 ms | 376.7 ms | **62×** |
| test.xlsx | 6.5 ms | 372.0 ms | **57×** |
| test.pdf | 8.8 ms | 430.8 ms | **49×** |
| test_wikipedia.html | 18.4 ms | 465.9 ms | **25×** |

At this end of the scale anymd is close to the floor: ~6 ms of it is process
start and exit, which is why the four numbers cluster and why small PDFs show
no CLI speedup at all even where the library got 10× faster.

## Memory and footprint

| | anymd | markitdown |
|---|---:|---:|
| Peak RSS (test_wikipedia.html) | **26 MB** | 162 MB |
| Install footprint | **17 MB** (one binary) | 318 MB (venv + deps) |

## Coverage — the number that is easy to report dishonestly

A naive count says markitdown converts **30/31** files and anymd **26/31**.
That comparison is wrong, because exit code 0 is not the same as output.

Counting files that produce **substantive output (>200 bytes)**:

| | anymd | markitdown |
|---|---:|---:|
| substantive conversions | **19/31** | **19/31** |

Dead even. The five files anymd refuses break down as:

| file | anymd | markitdown |
|---|---|---|
| scanned medical PDF | `ErrNoTextLayer` (exit 1) | **0 bytes, exit 0** |
| test.jpg, test_llm.jpg | 131 / 60 bytes of EXIF | **0 bytes, exit 0** |
| test.mp3/.m4a/.wav | error (exit 1) | 29-byte transcript |
| random.bin | error | error |

Two things worth being precise about:

1. **markitdown genuinely transcribes audio** — that is a real capability we do
   not have. It does it by sending the audio to a remote speech-recognition
   service, which is exactly the kind of convert-time network call anymd
   deliberately does not make. Different trade-off, not a defect on either side.
2. **On the scanned PDF and both images, markitdown returns an empty string and
   exit 0.** For an ingestion pipeline that is the worst outcome: silently
   indexing nothing is indistinguishable from a document that genuinely had no
   text. anymd returns `ErrNoTextLayer` so a caller can route the file to OCR.

## What this does not measure

Output *quality* is not benchmarked here — byte counts are not fidelity. The
two tools produce different markdown for the same input (see the corpus check
in the README). Where output size differs sharply, such as RSS feeds, it is
because the tools make different choices about how much of an entry's HTML body
to keep, not because one is failing.
