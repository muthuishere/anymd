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
| test.epub | 0.10 ms | 3.10 ms | **31×** |
| test.docx | 0.59 ms | 17.86 ms | **30×** |
| test.xlsx | 0.64 ms | 7.35 ms | **11×** |
| test.pptx | 0.91 ms | 8.52 ms | **9.4×** |
| test.pdf | 3.44 ms | 74.47 ms | **22×** |
| test_wikipedia.html (385K) | 11.55 ms | 104.15 ms | **9.0×** |

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

markitdown pays **309 ms** of Python import and init on *every* invocation.
That cost is real for scripts and shell pipelines, and irrelevant to a
long-running service — which is why both tables are here.

| file | anymd | markitdown | speedup |
|---|---:|---:|---:|
| test.docx | 6.7 ms | 375.7 ms | **56×** |
| test.xlsx | 7.0 ms | 377.0 ms | **54×** |
| test.pdf | 9.4 ms | 431.1 ms | **46×** |
| test_wikipedia.html | 19.3 ms | 473.6 ms | **25×** |

At this end of the scale anymd is close to the floor: ~6 ms of it is process
start and exit, which is why the four numbers cluster and why small PDFs show
no CLI speedup at all even where the library got 10× faster.

## Memory and footprint

| | anymd | markitdown |
|---|---:|---:|
| Peak RSS (test_wikipedia.html) | **27 MB** | 160 MB |
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

## Quality — fidelity, not byte counts

Run it yourself: `./bench/run-quality.sh <path-to-a-docling-checkout>`.

Every number above is a speed number, and speed is the easy one to win. This is
the number that decides whether the output is worth having, and anymd currently
**loses it**. The decision to measure it this way is
[ADR 0003](../docs/adr/0003-quality-benchmark-against-docling.md).

### The corpus and the target

The corpus is [docling](https://github.com/docling-project/docling)'s test
suite — 130 in-scope documents shipped as (source, expected markdown) pairs
under `tests/data/<format>/{sources,groundtruth}`. Two things make it worth
borrowing: the pairs are machine-matchable, and the files are named for the
feature they exercise (`docx_lists.docx`, `xlsx_07_gap_tolerance_.xlsx`), so a
low score names the broken feature instead of merely asserting one exists.

**docling is the target, not a contestant.** Its ground truth is its own frozen
output, so it would score 1.00 by construction and reporting that would be
meaningless. It is not run. The head-to-head is anymd vs markitdown, both of
which are third parties to that ground truth.

Scoring is style-insensitive on purpose. `| --- |` against `| - |`, `#` against
`##`, `<!-- image -->` against nothing — those are conventions, not defects, and
are normalized away before anything is compared.

### The metrics

| | what it catches |
|---|---|
| **content** | token F1, order-insensitive — dropped and invented content |
| **order** | token F1, order-**sensitive** — scrambled reading order |
| **tables** | each ground-truth table matched to its best candidate, scored on cell content |
| **headings** / **lists** | fraction of ground-truth items that survive |

**content and order are a pair, and the gap between them is the point.** A
two-column PDF read straight down the page keeps every token, so content stays
high while order collapses. That gap separates a reading-order defect from a
content-loss defect, which no single similarity score can do.

`declined` counts explicit refusals (nonzero exit), which are withheld from the
averages. A tool that fails loudly hands the caller something actionable, and
scoring that 0.00 would punish the exact behaviour the coverage section above
argues for. Exit-0-with-empty-output is *not* withheld — that is the silent
failure, and it scores 0 like any other miss.

### Results

130 documents, anymd v0.1.0 against markitdown 0.1.5. Bold marks the winner
where the gap is more than a rounding difference.

| format | n | content | order | tables | headings | lists |
|---|---:|---:|---:|---:|---:|---:|
| csv | 9 | **1.00** / 0.98 | **1.00** / 0.98 | **1.00** / 0.98 | 1.00 / 1.00 | 1.00 / 1.00 |
| docx | 33 | 0.84 / **0.94** | 0.84 / **0.94** | 0.89 / **0.96** | **0.95** / 0.88 | 0.85 / 0.87 |
| epub | 1 | 1.00 / 0.99 | 1.00 / 0.99 | 1.00 / 1.00 | 1.00 / 1.00 | 1.00 / 1.00 |
| html | 32 | 0.95 / 0.96 | 0.95 / 0.96 | 0.69 / **0.98** | 1.00 / 1.00 | 0.94 / 0.94 |
| md | 10 | 0.97 / 0.97 | 0.97 / 0.97 | 0.87 / 0.87 | 0.92 / 0.92 | 0.78 / 0.78 |
| odf (as pdf) | 6 | 0.64 / **0.94** | 0.60 / **0.82** | 0.17 / **0.38** | 0.17 / 0.17 | 0.56 / 0.56 |
| pdf | 15 | 0.87 / 0.90 | 0.70 / 0.74 | 0.43 / **0.56** | 0.14 / 0.20 | 0.44 / 0.42 |
| pptx | 8 | 0.80 / 0.82 | 0.77 / 0.82 | 0.98 / 0.95 | **1.00** / 0.86 | 0.56 / 0.57 |
| xls | 1 | **0.95** / 0.68 | **0.95** / 0.68 | **0.64** / 0.41 | 1.00 / 1.00 | 1.00 / 1.00 |
| xlsx | 11 | **0.87** / 0.68 | **0.83** / 0.65 | **0.74** / 0.60 | 0.95 / 1.00 | 1.00 / 1.00 |
| **all** | **130** | 0.88 / **0.91** | 0.86 / **0.88** | 0.75 / **0.85** | **0.84** / 0.82 | 0.82 / 0.82 |

anymd wins the spreadsheet formats decisively — xlsx content **0.87 against
0.68** — and csv outright. It loses docx, html tables, pdf, and the aggregate.

These numbers are unchanged by the vendored PDF parser of
[ADR 0002](../docs/adr/0002-vendor-the-pdf-parser.md), which is the point: that
change bought 16.8× and was asserted to leave output byte-identical. Running
this benchmark either side of it is the first independent check of that claim.

### What the low scores actually are

A quality benchmark earns its keep by naming bugs, not by producing a number.
These are the ones it found on its first run:

| file | score | defect |
|---|---|---|
| `drawingml.docx` | content **0.08** | 13 bytes emitted — DrawingML canvas text and embedded chart data dropped entirely |
| `textbox.docx` | content **0.13** | Word text boxes (`w:txbxContent`) not extracted |
| `docx_rich_tables_01.docx` | tables **0.00** | rich table cells lost |
| `2203.01017v2.pdf` | content 0.82, order **0.50** | two-column arXiv paper: nearly all the text, half the order |
| `2206.01062.pdf` | content 0.71, order **0.47** | same defect |
| `table_mislabeled_as_picture.pdf` | content 0.97, order **0.49** | the textbook case — the content is *there*, the sequence is wrong |
| `xlsx_07_gap_tolerance_.xlsx` | tables **0.11** | one table dumped per sheet; separate logical tables not split on blank-row gaps |
| `hyperlink_02.html` | content 0.40 | link handling |

The three PDF rows are one bug, not three. `pdfPageText` in `conv_pdf.go` orders
glyphs by `Y` descending then `X` ascending, which on a two-column page
interleaves the columns line by line. docling solves this without a model:
`ReadingOrderPredictor` in `docling/models/postprocessing/reading_order_rb.py`
builds bounding-box adjacency between blocks, dilates horizontally to merge
column fragments, and walks the resulting graph depth-first. That is geometry
and a graph traversal, both portable to Go.

The two `docx` rows are the more serious pair. A scrambled PDF is visibly
degraded; a dropped text box is a document that silently converts to almost
nothing at exit 0, which is the failure mode this project criticises markitdown
for elsewhere in this file.

`table_misidentified_as_form.pdf` is the single declined file — a scanned PDF
with no text layer, refused with `ErrNoTextLayer`. The vision-model path added
in `cca96b3` handles it, but is opt-in and unconfigured here, which is why the
refusal still stands.

## What this still does not measure

The ground truth is docling's output, so "quality" here means agreement with
docling. Where docling is itself wrong, matching it scores well — two of the
corpus filenames (`table_misidentified_as_form`, `table_mislabeled_as_picture`)
are docling's own record of getting a page wrong. It also says nothing about
formats docling ships no pairs for: rss, msg, zip, ipynb, and images.
