# LinkedIn — draft posts

Three alternatives. Pick one. Do not post all three.

---

## Post A — the silent-failure post (recommended)

*~245 words. This is the one that teaches something whether or not the reader
ever installs anything.*

A document converter returned exit code 0 and an empty string. If that lands in
an ingestion pipeline, you have indexed nothing, and nothing about the result
says so.

The file was a scanned PDF — a page image, no text layer. There is nothing to
extract, which is fine. What is not fine is that "nothing to extract" and
"extraction succeeded" reach the caller looking identical.

I found this while benchmarking Microsoft's markitdown to see whether I should
just use it. On its own test corpus, three files come back as 0 bytes at exit 0:
the scanned medical report and both test images. It is not a bug in
markitdown so much as a missing distinction. But for anything ingesting
documents at volume, silently indexing nothing is the worst available outcome.
A loud failure gets retried or routed to OCR. A quiet one becomes a document
your search index swears it has.

So anymd returns a named error, `ErrNoTextLayer`, and the caller decides. If you
want the page read, you pass `--llm` and supply a model — explicitly, per
invocation, so it can never be a surprise on someone else's document.

The general rule I keep relearning: in an ingestion path, measure substantive
output, not exit codes. Counting exit codes said markitdown converted 30 of 31
files and anymd 26. Counting files that produced more than 200 bytes said 19 and
19.

anymd is MIT, pure Go, one binary: github.com/muthuishere/anymd

#golang #dataengineering

---

## Post B — the deployment-constraint post

*~190 words.*

The document converter in our ingestion path was a 318 MB Python virtualenv
that had to be present, at the right version, on every runner and in every
container that touched a file.

That is what anymd exists to remove. Any document to Markdown — docx, xlsx, pdf,
pptx, html, epub, msg, zip and eight more — in pure Go. `CGO_ENABLED=0`, no
Python, no poppler, no LibreOffice subprocess. One 15 MB static binary that
cross-compiles anywhere Go does, and the same code as a `go get`-able library.

The speed follows from the deployment model rather than from cleverness.
In-process against markitdown 0.1.5 on its own corpus: docx 0.59 ms vs 17.86,
PDF 3.44 ms vs 74.47, a 385 KB HTML page 11.55 ms vs 104.15. Peak RSS 27 MB
against 160 MB. And markitdown pays about 309 ms of interpreter startup on every
CLI invocation, which is free to ignore in a long-running service and impossible
to ignore in a shell loop.

Quality is the number that actually decides this, so it is measured too, against
docling's 130-document corpus. Full method, the script, and the formats where
anymd still loses are in bench/.

github.com/muthuishere/anymd

#golang

---

## Post C — the "I lost my own benchmark" post

*~220 words.*

I wrote a quality benchmark for my own document converter and it lost.

The setup: docling ships 130 documents as (source, expected markdown) pairs,
named for the feature each one exercises. So a low score does not just assert
that something is broken, it names the broken thing. I scored anymd and
markitdown against those pairs on content, reading order, tables, headings and
lists.

What it found was specific and humbling. `drawingml.docx` emitted 13 bytes at
exit 0 — Word text boxes and DrawingML canvas text dropped entirely. Two-column
arXiv PDFs scored 0.82 on content and 0.50 on reading order — nearly all the
text, half the sequence — because glyphs were sorted by Y then X, which
interleaves the columns line by line. Eight of the
corpus's 22 HTML tables came out as loose paragraphs, because the table plugin
I depended on bails when a cell has block content.

All three are fixed now, and the aggregate went from behind to ahead: content
0.92 against 0.91, order 0.91 against 0.88, tables 0.87 against 0.85. On
spreadsheets it is not close — xlsx content 0.98 against 0.68.

anymd still loses on ODF and on PDF tables, and bench/ says so in the same table
as the wins. A benchmark you only publish when you win is marketing.

github.com/muthuishere/anymd

#golang #opensource
