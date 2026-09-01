# Contributing to anymd

Thanks for wanting to help. This project has one hard promise —
**pure Go, no cgo, no external binaries, no network at convert time** — and a
small number of rules that exist to keep it.

The binding rules for in-tree code are in **[`CONTRACT.md`](CONTRACT.md)**.
Read it before writing a converter; this file is the practical wrapper around
it (how to run things, what a PR needs, what gets rejected and why).

---

## Getting set up

```sh
git clone https://github.com/muthuishere/anymd && cd anymd
go build ./...
go test ./...
```

That is the whole toolchain. Go, and nothing else. If a change makes that
untrue — a C library, a Python script, a `brew install` step in the README —
it is the wrong change, however good the output it produces.

The Go version is pinned in `go.mod`; use at least that.

---

## Running the tests

```sh
make test                      # go test ./...
go test ./... -race -count=1   # what CI runs; -count=1 defeats the test cache
go vet ./...                   # == make lint
gofmt -l .                     # must print nothing
```

**Fuzzing.** Converters are parsers pointed at bytes someone else chose, so
fuzz targets are first-class here, not an extra:

```sh
# every fuzz target's seed corpus, as ordinary unit tests (fast, in CI)
go test ./... -run Fuzz

# actually fuzz one target — run this for minutes, not seconds
go test -run '^$' -fuzz FuzzDocx -fuzztime 5m .

# the targets that exist today (fuzz_test.go): FuzzDocx FuzzPptx FuzzXlsx
# FuzzXls FuzzPdf FuzzZip FuzzEpub FuzzMsg FuzzHTML FuzzRSS — a new converter
# should arrive with its own.
```

A crash lands in `testdata/fuzz/<Target>/`. **Commit that file** — it is a
plain text corpus entry, not a binary artifact, and it becomes a permanent
regression test.

---

## Adding a converter

The short version; `CONTRACT.md` is authoritative.

1. **One format family, one file**, named `conv_<thing>.go`. Register it from
   its own `init()` with `addBuiltin(&XConverter{})`. **Never edit
   `builtins.go`** — that file only exists to hold the slice.
2. **Never edit `go.mod` / `go.sum`.** Every dependency you need is already
   pinned. If you genuinely need one that is not, open an issue *first* — see
   [Dependencies](#dependencies) below.
3. **Render through `internal/mdutil`** (`Table`, `Heading`, `CodeBlock`,
   `Join`, `EscapeCell`, `Collapse`). Do not hand-roll a pipe table. A table
   from `xlsx` and a table from `docx` must be byte-identical; that property is
   the reason anymd's output is safe to commit and diff.
4. **`Accepts` must be cheap** — hints and magic bytes only. Never parse the
   whole document there.
5. **Never panic.** See the next section; this is the rule we care most about.
6. **Tests are mandatory**, in `conv_<thing>_test.go`, asserting **exact**
   markdown output. Not `strings.Contains`.
7. **Recurse through `opts.Recurse(reader, info)`**, never by constructing a
   new `Engine`. That indirection is what makes `--max-depth` real.

`conv_plaintext.go` is the worked reference. Copy its shape.

Update the format table in `README.md` and add a line to `CHANGELOG.md` under
`## [Unreleased]` in the same PR.

---

## A converter must never panic on hostile input

Every converter is a parser aimed at a file some stranger produced. Treat every
length, offset, index, and count you read out of a document as
attacker-controlled.

- **Never size a buffer from a length field.** `make([]byte, n)` where `n` came
  out of the file is a one-line OOM. Read through an `io.LimitReader` with a
  cap you chose, and read one byte past it so overflow is *detectable*.
- **Bound the totals, not just the parts.** A per-entry cap alone still lets
  10,000 entries of 64 MiB expand to 640 GiB. Cap the cumulative budget too —
  `conv_zip.go` is the reference for this.
- **Malformed input is an `error`, never a crash.** A panic escaping a
  converter takes down the caller's process; anymd is a *library* first, so
  that is somebody's production server.
- **Wrap third-party parsers in `recover()`.** We do not control what
  `ledongthuc/pdf`, `extrame/xls`, or the EXIF decoders do on a malformed
  document. Convert their panic into an error at your converter's boundary and
  return a useful message.
- **No network, no subprocess, no shell, ever.** Not for a remote image, not
  for a stylesheet, not for a linked article. The only network call in the
  project is the CLI fetching a URL you explicitly passed as an argument.
- **Path traversal.** Anything that reads names out of a container must reject
  absolute paths, Windows drive letters, and `..` components — even though we
  never write members to disk, those names end up in headings.

If you find a way to panic a converter, that is a bug worth reporting. See
[`SECURITY.md`](SECURITY.md) for how.

---

## No committed binaries

**Do not commit a `.docx`, `.xlsx`, `.pdf`, `.zip`, or any other binary
fixture.** Build fixtures **in Go, at test time** — write a real `.docx` with
`archive/zip` in the test itself. `conv_docx_test.go` and `conv_zip_test.go`
show the pattern.

Why this is a rule and not a preference:

- A committed binary is unreviewable. Nobody diffs a zip in a PR, so nothing
  stops a fixture from quietly carrying something it should not.
- Fixtures rot silently. A generated fixture states its own intent in code:
  you can read exactly which XML element the test is about.
- It keeps the clone small and `go get` cheap forever.

The only binary in the repo is the `testdata/fuzz` corpus, which is generated
by the fuzzer and is plain text.

---

## Dependencies

`go.mod` is pinned and closed. Adding a dependency means:

- another parser in the trust boundary, which is where a memory-safety issue
  would come from;
- a risk it pulls in cgo, which breaks the entire product promise.

So: **open an issue before writing code that needs a new one.** In the issue,
say what the format is, why the standard library plus what is already pinned
cannot do it, and confirm the candidate builds with `CGO_ENABLED=0`. A PR that
touches `go.mod` without that conversation will be asked to split it out.

---

## Commit style

Conventional Commits — the release changelog is generated from them, so the
prefix decides which section your change lands in:

```
feat:     a new capability
conv:     a new or reworked converter
fix:      a bug fix
perf:     a performance change with no behaviour change
security: a hardening change
docs:     documentation only
test:     tests only
chore:    tooling, deps, housekeeping
ci:       workflow changes
```

Scope it when it helps: `conv(pptx): extract speaker notes`. Breaking changes
get a `!` (`feat!:`) and an explanation in the body.

Write the body for someone reading `git log` in two years: **why**, not what.
The diff already says what.

Do not add `Co-authored-by:` trailers.

---

## Pull requests

Before you open one:

```sh
gofmt -l .                     # prints nothing
go vet ./...                   # clean
go test ./... -race -count=1   # green
go build ./...                 # clean
```

The PR template has the full checklist. In short: tests added and asserting
exact output, gofmt clean, vet clean, no new dependency without a prior issue,
no committed binaries, `CONTRACT.md` followed, `README.md` format table and
`CHANGELOG.md` updated if user-visible behaviour changed.

Keep PRs to one thing. A converter plus a refactor of the engine is two PRs.

**Concurrency note:** several people may be writing sibling `conv_*.go` files
at once. If `go build ./...` reports an error in a file you do not own, that is
someone else mid-write — ignore it, and re-check only the errors naming your
files.

---

## Reporting a security issue

Not through a public issue. See [`SECURITY.md`](SECURITY.md).

---

## License

By contributing you agree your work is licensed under the MIT License, the same
as the rest of the project.
