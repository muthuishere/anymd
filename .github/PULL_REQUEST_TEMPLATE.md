<!--
Thanks for the PR. The checklist below is not ceremony — every item is a rule
that has a reason behind it in CONTRACT.md or CONTRIBUTING.md.
Keep a PR to one thing: a converter plus an engine refactor is two PRs.
-->

## What this changes

<!-- One or two sentences. What is different for a user of the CLI or the library? -->

## Why

<!-- The reason, not the diff. What was broken, missing, or wrong? -->

Closes #

## Type of change

- [ ] `conv:` new or reworked converter
- [ ] `feat:` new capability
- [ ] `fix:` bug fix
- [ ] `perf:` performance, no behaviour change
- [ ] `security:` hardening
- [ ] `docs:` / `test:` / `chore:` / `ci:`
- [ ] **Breaking change** (describe the migration below)

---

## Checklist

**Correctness**

- [ ] `go build ./...` clean
- [ ] `go vet ./...` clean
- [ ] `gofmt -l .` prints nothing
- [ ] `go test ./... -race -count=1` green

**Tests**

- [ ] Tests added or updated for this change
- [ ] They assert **exact** Markdown output, not `strings.Contains`
- [ ] Hostile/malformed input is covered — truncated, oversized, wrong magic
      bytes — and produces an **error, never a panic**
- [ ] A new converter ships its own fuzz target

**The rules that get PRs rejected**

- [ ] **No committed binary fixtures.** Fixtures are built in Go at test time
      (`archive/zip` writes the real `.docx`), per CONTRACT.md rule 6
- [ ] **`go.mod` / `go.sum` untouched.** A new dependency needs an issue and a
      conversation first — it is another parser inside the trust boundary and a
      cgo risk
- [ ] **`builtins.go` untouched** — the converter registers from its own `init()`
      with `addBuiltin(...)`
- [ ] **Still pure Go**: builds with `CGO_ENABLED=0`, no shell-out, no
      subprocess, no external binary
- [ ] **No network at convert time.** The CLI's explicit URL argument remains
      the only network call in the project

**Converter specifics** (skip if not a converter)

- [ ] One format family, one `conv_<thing>.go` file
- [ ] `Accepts` is cheap — hints and magic bytes only, never a full parse
- [ ] Output rendered through `internal/mdutil` (`Table`, `Heading`,
      `CodeBlock`, `Join`) so every emitter agrees byte-for-byte
- [ ] Container recursion goes through `opts.Recurse`, never a new `Engine`
- [ ] Third-party parsers wrapped in `recover()` at the converter boundary
- [ ] No buffer sized from a length field read out of the document

**Docs**

- [ ] `README.md` format table updated (extracted **and** not-extracted columns)
- [ ] `CHANGELOG.md` entry added under `## [Unreleased]`
- [ ] Doc comments on every exported symbol, explaining **why** where the code
      already says what

---

## How to verify by hand

```sh
# the command a reviewer should run to see the change
```

## Notes for the reviewer

<!-- Anything you are unsure about, deliberately left out, or want argued with. -->
