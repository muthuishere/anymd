# Benchmarks

Reproducible comparison of `anymd` against Microsoft's
[markitdown](https://github.com/microsoft/markitdown), the Python tool this
project is an alternative to.

Run it yourself: `./bench/run.sh` (needs `uv`, `hyperfine`, and Go).

Every number below came from that script. Where markitdown wins, it says so.

## Environment

| | |
|---|---|
| Machine | Apple Silicon (darwin/arm64), macOS 25.4 |
| anymd | v0.1.0, Go 1.26 |
| markitdown | 0.1.5, Python 3.12.13, `markitdown[all]` |
| Corpus | the 31 files in markitdown's own test suite |

## Speed — in-process (the fair library comparison)

Mean of 10 runs after a warm-up. This excludes interpreter startup, so it is
the honest number for a long-running service.

| file | anymd | markitdown | speedup |
|---|---:|---:|---:|
| test.epub | 0.14 ms | 2.74 ms | **20×** |
| test.xlsx | 0.56 ms | 7.44 ms | **13×** |
| test.docx | 0.56 ms | 17.29 ms | **31×** |
| test.pptx | 0.97 ms | 8.00 ms | **8.2×** |
| test_wikipedia.html (385K) | 12.04 ms | 101.58 ms | **8.4×** |
| test.pdf | 45.69 ms | 77.27 ms | **1.7×** |

**PDF is the closest race** — 1.7×, not 30×. Both tools are dominated by the
PDF parser itself (pdfium here, pdfminer.six there), and that is where nearly
all of anymd's own runtime goes too. Do not expect order-of-magnitude wins on
PDF-heavy corpora.

## Speed — CLI end-to-end

markitdown pays **299 ms** of Python import and init on *every* invocation.
That cost is real for scripts and shell pipelines, and irrelevant to a
long-running service — which is why both tables are here.

| file | anymd | markitdown | speedup |
|---|---:|---:|---:|
| test.docx | 5.8 ms | 368.4 ms | **63×** |
| test.xlsx | 6.5 ms | 368.7 ms | **57×** |
| test_wikipedia.html | 18.4 ms | 463.1 ms | **25×** |
| test.pdf | 53.2 ms | 422.8 ms | **7.9×** |

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
