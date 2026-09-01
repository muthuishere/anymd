# anymd — build contract for contributors

Pure-Go any-document → Markdown. **No cgo. No external binaries. No network at
convert time.** A consumer must get a working library with `go get` and a
working binary with `go build`, on every platform Go targets. Any change that
breaks that is wrong, however good the output.

## The seam (read these three files first)

- `converter.go` — the `Converter` interface: `Accepts` + `Convert`, plus the
  `Named` / `Prioritized` optional interfaces and the `Priority*` constants.
- `streaminfo.go` — `StreamInfo` hints (`MimeType`, `Extension`, `Charset`,
  `FileName`, `URL`) and helpers `HasExt`, `HasMimePrefix`, `NormalizedMime`.
- `engine.go` — dispatch: rewind to 0, ask each converter in priority order,
  first `Accepts` wins. A converter that accepts and then fails is a hard
  error — the engine does NOT fall through to the fallback.

`conv_plaintext.go` is the worked reference. Copy its shape.

## Rules

1. **One format family, one file**, named `conv_<thing>.go`, registered from
   its own `init()` with `addBuiltin(&XConverter{})`. Never edit `builtins.go`.
2. **Never edit `go.mod` / `go.sum`.** Every dependency you need is already
   pinned. If you genuinely need one that is not, stop and report it.
3. **Render through `internal/mdutil`** — `Table`, `Heading`, `CodeBlock`,
   `Join`, `EscapeCell`, `Collapse`. Do not hand-roll pipe tables; a table from
   xlsx and a table from docx must come out byte-identical.
4. `Accepts` is cheap: hints and magic bytes only. Never parse the whole
   document there.
5. **Never panic.** Malformed input is an `error`. A converter is a parser
   pointed at hostile bytes; treat every length, index, and pointer as
   attacker-controlled. Bound recursion via `opts.Recurse`, never by calling a
   new `Engine`.
6. **Tests are mandatory** in `conv_<thing>_test.go`. Build fixtures **in Go**
   (write a real .docx/.zip with `archive/zip` at test time) rather than
   committing binaries. Assert exact markdown output, not "contains".
7. `gofmt` clean. Doc comments on every exported symbol, explaining *why* where
   the code does not already say *what*.

## Output style (GitHub-flavored Markdown)

Headings clamp to 1–6. Tables are real GFM pipe tables. Code is fenced. Blocks
join with exactly one blank line; the document ends with exactly one newline.
Empty renderings are dropped, never emitted as blank blocks.

## Concurrency note

Several agents are writing sibling `conv_*.go` files in this tree at the same
time. If `go build ./...` reports errors in a file you do not own, that is a
teammate mid-write — ignore it and re-check only the errors naming your files.
