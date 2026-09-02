# 0001 — Pure Go, and no network the caller did not ask for

Status: **Accepted** · recorded retrospectively 2026-09-02. The constraint
itself is not new: it arrived with `CONTRACT.md` in the project's second commit
and has bound every converter since. Only the record is late, written because
[0002](0002-vendor-the-pdf-parser.md) needed a documented premise to lean on.

## Context

anymd converts documents to Markdown. It is aimed squarely at the job Microsoft's
`markitdown` does, and the reason to prefer it is not that Go is faster than
Python — it is what a consumer has to accept in order to use it.

markitdown's install is a 318 MB virtualenv, and its audio support works by
sending your bytes to a remote speech-recognition service — not as a flag, but
as what `markitdown file.mp3` does. Both are defensible choices for a Python
tool. Both are disqualifying for the two consumers anymd is built for: an
ingestion pipeline that must not phone home with a customer document, and a
binary that must drop into a scratch container and run.

The forces:

- **Distribution.** A single static binary and one `go get`-able library.
  `CGO_ENABLED=0`, cross-compiled to every platform Go targets, is the whole
  proposition. A C dependency turns one artifact into a per-platform build
  matrix and a runtime linkage problem.
- **Trust.** A converter runs on documents the caller did not write. Any
  convert-time network call is an exfiltration path, and one that is invisible
  in the function signature.
- **Fidelity, which loses here.** The best extractors for the hardest formats
  are C libraries — pdfium, MuPDF, Tesseract. Refusing them means accepting
  worse output on hard PDFs.
- **The capabilities people actually want.** OCR, captions and transcription are
  the converters markitdown is judged on, and a scanned PDF is a real document
  someone needs read. "We do not do that" is a defensible answer exactly once.

## Decision

**anymd is pure Go: no cgo, no external binaries, no subprocesses. The default
build makes no network call at convert time; anything that needs a model is
opt-in behind an explicit flag, and off unless asked for.**

The two halves are not the same promise, and separating them is the whole
decision. *Pure Go* is an invariant — there is no flag that links a C library,
because the artifact is the product. *No network* is a *default*, because the
capability is genuinely valuable and the objection was never "a model is wrong",
it was "a model must not be a surprise".

So the shape is: `anymd file.pdf` cannot phone home, and that is checkable from
the command line rather than from our documentation. `anymd --llm file.pdf`
can, because you said so. `ErrNoTextLayer` is what the default returns on a
scan — a hard error the caller can route onward, never an empty string that
silently indexes nothing.

A default that flips itself on when a key happens to be in the environment would
forfeit this entirely: the guarantee is worth having only if it holds without
the operator having to audit their own shell.

## Consequences

- Install is one 17 MB binary; peak RSS is ~26 MB against markitdown's ~162 MB.
- The default build declines what it cannot do honestly: `ErrNoTextLayer` on a
  scan, and audio not accepted at all rather than accepted and returned empty.
- OCR, image captioning and transcription exist, behind `--llm` /
  `--llm-transcribe` and the `Describer` / `Transcriber` interfaces. The library
  takes an interface rather than an SDK, so a local Ollama satisfies it as well
  as a hosted API and the "no data leaves the box" property is recoverable by
  the caller's own choice of implementation.
- Benchmarks measure the default build only. With a model in the loop the wall
  clock is the model's, and the comparison stops being about parsers.
- PDF extraction is weaker than pdfium's on hard layouts. See
  [0002](0002-vendor-the-pdf-parser.md).
- The escape hatch, if fidelity ever outweighs the binary: `klippa/go-pdfium`
  runs pdfium under a pure-Go WASM runtime, which keeps `CGO_ENABLED=0`. It is
  on the table only when a real corpus shows the current parser failing, and
  never merely because it would be faster.
- **This record was superseded in part by its own repository within a day.** It
  was written retrospectively on 2026-09-02, and the `--llm` work landed on
  `main` in the same session, which is why the text above distinguishes the
  invariant from the default. A record that had simply said "no network" would
  have been false on the day it was committed — worth remembering the next time
  a constraint feels permanent enough not to write the reason down.
