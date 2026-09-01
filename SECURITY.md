# Security policy

## Reporting a vulnerability

**Do not open a public issue.** Report privately through GitHub Security
Advisories:

**<https://github.com/muthuishere/anymd/security/advisories/new>**

Please include:

- what happens (panic, hang, unbounded allocation, path written outside the
  intended location, data leaving the process, …);
- the `anymd --version` output and your OS/arch;
- a reproducer. **A generator is worth more than an attachment** — a small Go
  program or shell snippet that constructs the malicious document, rather than
  the document itself. It reviews, it diffs, and it becomes a fuzz seed.

Expect an acknowledgement within a few days. Fixes ship in the next patch
release, with an advisory once a fixed version is available.

## Supported versions

While the project is pre-1.0, only the **latest released version** gets
security fixes. There is no backporting to older tags.

---

## Threat model

This is the part that actually matters, so it is specific rather than generic.

**The entire attack surface is a document you did not write.** `anymd` is a
stack of parsers — ZIP, OOXML, OLE2/BIFF, PDF, HTML, XML, JSON, EXIF — pointed
at bytes an attacker chose. The realistic scenario is: your service accepts an
upload and runs it through `anymd` to feed an LLM or a docs pipeline. The
attacker fully controls those bytes and wants your process to crash, exhaust
memory, hang, read a file it should not, or make a network call.

The **trust boundary is the input stream**. Every length, offset, index, count,
and name read out of a document is attacker-controlled and treated as such.

### What we defend against

- **Panics escaping into your process.** Malformed input is an `error`, not a
  crash. `anymd` is a library first, so a panic is somebody's production server
  going down. Third-party parsers we do not control (`ledongthuc/pdf`,
  `extrame/xls`, the EXIF and HTML decoders) are wrapped in `recover()` at the
  converter boundary and their panic is converted into an ordinary error.
- **Length-field-driven allocation.** No buffer is ever sized from a number
  read out of the document. Reads go through `io.LimitReader` with a cap we
  chose, reading one byte *past* the cap so an overflow is detectable rather
  than silently truncating.
- **Zip bombs — both kinds.** Entry count, per-entry decompressed size, and
  **cumulative** decompressed size across the archive are all capped. The
  cumulative cap is the one that actually stops a bomb: a per-entry limit alone
  still permits 10,000 entries of 64 MiB each to expand to 640 GiB.
- **Zip path traversal.** Member names containing `..` components, absolute
  paths, or Windows drive letters are rejected. `anymd` never extracts members
  to disk at all — but those names reach the output as headings, so they are
  validated rather than trusted.
- **Recursion / decompression nesting.** A zip inside a zip inside a zip is
  bounded centrally by `Options.MaxDepth` (default 8), enforced by
  `Options.Recurse`. Converters are required to recurse through that function
  rather than constructing their own `Engine`, which is what makes the bound
  real instead of advisory.
- **Exfiltration and SSRF via a document.** Converters **never touch the
  network**. A document cannot make `anymd` fetch a remote image, stylesheet,
  DTD, or linked article, so it cannot phone home and cannot be used to probe
  your internal network. The single network call in the whole project is the
  CLI fetching an `http(s)` URL **you** passed as an argument, which happens
  before any converter sees a byte.
- **Command injection.** There is no shell-out, no subprocess, no plugin
  loading, no `os/exec` anywhere. No LibreOffice, no poppler, no `unzip`.
  Nothing in a document can become an argument to a program.
- **Memory-unsafe native code.** `CGO_ENABLED=0`, so there is no C parser in
  the dependency graph and no class of exploitable heap corruption that comes
  with one. A parser bug here is a Go panic or an allocation, not arbitrary
  code execution.
- **Output injection into your files.** Cell and text content is escaped
  through `internal/mdutil` before it reaches a pipe table, so a crafted cell
  cannot break the table structure of the emitted Markdown.

### What we explicitly do NOT promise

Be honest with yourself about these before pointing `anymd` at the internet.

- **`anymd` is not a sandbox.** It is a library in your address space. It
  reduces the blast radius of a malicious document; it does not contain one.
- **Resource exhaustion is bounded, not eliminated.** A document crafted to be
  pathological can still burn significant CPU and allocate up to the caps
  before failing. Quadratic behaviour inside a third-party parser is possible
  and we cannot see it coming. **If a hostile upload must not be able to slow
  you down, run the conversion in a separate process with an OS-level memory
  limit, a CPU limit, and a wall-clock timeout, and kill it if it overruns.**
  That is the only real defence, and it is yours to apply.
- **No timeout inside the library.** Conversion is not cancellable mid-parse
  through a third-party decoder. A `context` deadline in your code will not
  interrupt one.
- **The output is untrusted content.** The Markdown `anymd` produces contains
  whatever the document said, including links, and it is structurally valid but
  not *safe*. If you render it as HTML, sanitize it. If you feed it to an LLM,
  a document can contain prompt-injection text — that is a property of every
  document extractor, and treating extracted text as data rather than
  instructions is your pipeline's job.
- **Correctness is not a security guarantee.** A converter may silently omit
  content a format hides (a tracked change, a hidden sheet, white-on-white
  text). Do not use `anymd` as a redaction or content-inspection control.
- **Dependency parsers are the most likely source of a real memory-safety or
  DoS issue.** `github.com/ledongthuc/pdf`, `github.com/extrame/xls`,
  `github.com/xuri/excelize/v2`, `github.com/dsoprea/go-exif/v3`,
  `github.com/richardlehane/mscfb`, the HTML and feed parsers, and Go's own
  `archive/zip` and `encoding/xml` all parse attacker-controlled bytes. They
  are memory-safe Go, so the ceiling is a panic or an allocation rather than
  RCE — but a hang or an OOM originating in one of them is the failure we would
  most expect. Report those here anyway; we will fix the guard on our side and
  route it upstream.

### Hardening the caller

If you process untrusted uploads:

1. Run `anymd` as a **separate short-lived process**, not in-library, so the
   OS can enforce limits you can actually rely on.
2. Apply a **memory limit** (cgroups / `ulimit -v` / a container limit), a
   **CPU limit**, and a **wall-clock timeout**. Kill on overrun.
3. Drop the network entirely — the converters never need it. A network
   namespace with no route proves the property rather than trusting it.
4. Lower `--max-depth` from the default 8 if you have no legitimate nested
   archives.
5. Cap the input size before it ever reaches `anymd`.
6. Never pass `--insecure`. It disables TLS verification on the CLI's URL
   fetch and exists only for self-signed internal hosts.
7. Treat the output as untrusted text: sanitize before rendering, and never
   write it to a path derived from a name found inside the document.

---

## A note on scope

Found a way to **panic** a converter with a crafted document? That is a bug we
want, and a report is welcome even if you think it is "only" a crash — under
this threat model, a panic in a library *is* the vulnerability.

Found that a converter uses a lot of memory on a large legitimate file? That is
a performance issue; open a normal issue for it.
