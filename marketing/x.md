# X / Twitter — draft thread and standalone posts

Character counts are stated after each post and were measured, not estimated.
Limit is 280. Do not post; these are drafts.

---

## Thread (6 posts)

**1/6** — hook

```
A document converter returned exit 0 and an empty string.

The file was a scanned PDF. No text layer, nothing to extract — fine.

But "nothing to extract" and "extraction succeeded" came back identical to the caller, and my pipeline indexed the empty string.
```
*(258 chars)*

**2/6**

```
This is markitdown on its own test corpus. Three files come back as 0 bytes with exit 0: the scanned medical report and both test images.

For an ingestion pipeline that's the worst outcome there is. A loud failure gets retried. A quiet one becomes a doc your index swears it has.
```
*(280 chars)*

**3/6**

```
So I wrote anymd. Same job — any document to Markdown — in pure Go.

On a scan with no text layer it returns a named error, ErrNoTextLayer, and the caller routes it to OCR. Want the page read anyway? Pass --llm and supply a model. Explicitly, per run.
```
*(251 chars)*

**4/6**

```
The deployment part is the actual pitch.

CGO_ENABLED=0. No Python, no poppler, no LibreOffice subprocess.

One 15 MB static binary vs a 318 MB venv. Cross-compiles anywhere Go does. Same code as a go get-able library.
```
*(218 chars)*

**5/6**

```
Speed, in-process vs markitdown 0.1.5 on its corpus:

docx  0.59ms / 17.86ms  → 30x
pdf   3.44ms / 74.47ms  → 22x
385K HTML 11.55ms / 104.15ms → 9x

Peak RSS 27MB vs 160MB. And markitdown pays ~309ms of interpreter startup on every CLI invocation.
```
*(247 chars)*

**6/6** — quality + link

```
Speed is the easy number. Quality is measured too, on docling's 130-doc corpus: content .92/.91, order .91/.88, tables .87/.85.

anymd still loses on ODF and PDF tables. bench/ prints that in the same table as the wins.

MIT, Go: github.com/muthuishere/anymd
```
*(258 chars)*

---

## Standalone posts

**S1 — the benchmark-honesty one**

```
I wrote a quality benchmark for my own converter and it lost.

Then it named the bugs: drawingml.docx emitting 13 bytes at exit 0, two-column PDFs scoring 0.82 content but 0.50 reading order, 8 of 22 HTML tables silently flattened.

Fixed. That's what it's for.
```
*(261 chars)*

**S2 — the Go performance one**

```
A vendored PDF parser got 16.8x faster from ~10 lines.

ledongthuc/pdf has no cache for resolved indirect objects — its own doc comment says one "would probably help significantly". Without it, laying out a page re-lexes the font /Widths array once per glyph.

Quadratic.
```
*(271 chars)*

**S3 — the local-pipeline one**

```
anymd converts any document to Markdown and, without --llm, makes zero network calls at convert time. Not "no calls unless a key is set" — none, by construction.

Which means you can point it at a file a stranger emailed you.

Pure Go, one binary: github.com/muthuishere/anymd
```
*(276 chars)*
