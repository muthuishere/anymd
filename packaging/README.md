# Packaging & release runbook

Everything a maintainer needs to ship `anymd` so that **anyone, on any OS, can
install it in one line**. This file is deliberately blunt about what is
*already wired up* versus what *does not exist yet*.

---

## Status: what works today, what does not

| Install path | Status | Needs |
|---|---|---|
| `go install github.com/muthuishere/anymd/cmd/anymd@latest` | ✅ works now | nothing — it works the moment the repo is public |
| GitHub release archives + `checksums.txt` | ✅ wired | push a `v*` tag; `.github/workflows/release.yml` does the rest |
| `curl … install.sh \| sh` | ✅ wired | the repo public on `main`, plus **at least one published release** |
| `brew install muthuishere/tap/anymd` | ❌ **not set up** | the `homebrew-tap` repo + the `HOMEBREW_TAP_GITHUB_TOKEN` secret, then uncomment `brews:` in `.goreleaser.yaml` |
| `scoop install anymd` | ❌ **not set up** | the `scoop-bucket` repo + the same secret, then uncomment `scoops:` |

The `brews:` and `scoops:` blocks in `.goreleaser.yaml` are **commented out on
purpose**. GoReleaser fails the entire release if it cannot push to a tap or
bucket repo, so enabling them before those repos exist would make every tag a
red build and publish nothing at all — including the plain archives.

---

## One-time setup (the part a human must do)

### 1. Publish the repo

`github.com/muthuishere/anymd`, public. `install.sh` is fetched from
`raw.githubusercontent.com/muthuishere/anymd/main/install.sh`, so the default
branch must be `main` and the file must be at the repo root.

### 2. Create the Homebrew tap

```sh
gh repo create muthuishere/homebrew-tap --public \
  --description "Homebrew tap for muthuishere's tools"
git clone https://github.com/muthuishere/homebrew-tap && cd homebrew-tap
mkdir -p Formula && touch Formula/.gitkeep
git add -A && git commit -m "chore: tap skeleton" && git push
```

The repo name **must** be `homebrew-tap` — that literal prefix is what makes
`brew install muthuishere/tap/anymd` resolve.

### 3. Create the Scoop bucket

```sh
gh repo create muthuishere/scoop-bucket --public \
  --description "Scoop bucket for muthuishere's tools"
git clone https://github.com/muthuishere/scoop-bucket && cd scoop-bucket
mkdir -p bucket && touch bucket/.gitkeep
git add -A && git commit -m "chore: bucket skeleton" && git push
```

### 4. Create the cross-repo token

The workflow's built-in `GITHUB_TOKEN` is scoped to `anymd` only and **cannot**
write to the tap or bucket repos. A separate token is required:

- GitHub → Settings → Developer settings → Personal access tokens
- A classic token with the `repo` scope, or a fine-grained token with
  **Contents: read and write** on `homebrew-tap` *and* `scoop-bucket`
- Add it to `anymd` as a repository secret named **`HOMEBREW_TAP_GITHUB_TOKEN`**
  (the release workflow already passes this env var through):

```sh
gh secret set HOMEBREW_TAP_GITHUB_TOKEN --repo muthuishere/anymd
```

### 5. Turn the blocks on

Uncomment `brews:` and `scoops:` at the bottom of `.goreleaser.yaml`, then:

```sh
goreleaser check
```

---

## Cutting a release

```sh
# 1. Land everything on main, green.
go build ./... && go test ./... -race -count=1 && go vet ./... && gofmt -l .

# 2. Write the CHANGELOG entry for the new version (Keep a Changelog format).
$EDITOR CHANGELOG.md

# 3. Dry-run the whole pipeline locally — builds and archives, publishes nothing.
goreleaser release --snapshot --clean --skip=publish,announce

# 4. Tag and push. That is the only trigger.
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

The `release` workflow then runs `vet` + `gofmt` + `go test -race` in one job
and, only if that job is green, GoReleaser in a second job with
`contents: write`.

**Version numbers.** The tag carries the `v` (`v0.1.0`); the archive filenames
and the Homebrew/Scoop manifests carry the bare number (`0.1.0`). `install.sh`
accepts either form and normalises.

---

## Verifying every install path after a release

Do not trust the green check mark — actually install it. Run each of these on
the matching OS.

### 1. The raw archive + checksum (any OS)

```sh
V=0.1.0
base=https://github.com/muthuishere/anymd/releases/download/v$V
curl -sSfLO $base/anymd_${V}_linux_amd64.tar.gz
curl -sSfLO $base/checksums.txt
sha256sum -c checksums.txt --ignore-missing   # must print: OK
tar xzf anymd_${V}_linux_amd64.tar.gz && ./anymd --version
```

`--version` must print the real version, **not `dev`**. `dev` means the ldflags
did not reach the linker — check `builds[].ldflags` against the `version` and
`commit` vars in `cmd/anymd/main.go`.

### 2. install.sh

```sh
curl -sSfL https://raw.githubusercontent.com/muthuishere/anymd/main/install.sh | sh
anymd --version
printf 'a,b\n1,2\n' | anymd -t csv     # must print a GFM pipe table
```

Also test the pinned path and a non-default prefix:

```sh
curl -sSfL .../install.sh | VERSION=v0.1.0 PREFIX="$HOME/bin" sh
```

Expect the checksum line (`checksum ok (sha256 …)`) in the output. If it is
missing, something is wrong — the script has no way to skip verification.

### 3. Homebrew (only once the tap exists)

```sh
brew update
brew install muthuishere/tap/anymd
brew test anymd        # runs the test do block: real CSV -> real GFM table
brew audit --strict muthuishere/tap/anymd
```

### 4. Scoop (Windows)

```powershell
scoop bucket add muthuishere https://github.com/muthuishere/scoop-bucket
scoop install anymd
anymd --version
"a,b`n1,2" | anymd -t csv
```

### 5. Go

```sh
go install github.com/muthuishere/anymd/cmd/anymd@v0.1.0
anymd --list        # 15 converters
```

Note: a `go install` build reports `dev` for the version — the ldflags only
exist in the Makefile and the GoReleaser config. `--version` still resolves the
commit from the embedded VCS build info, so it is not useless.

---

## Files in this directory

| file | what it is |
|---|---|
| `homebrew/anymd.rb` | reference formula. GoReleaser generates the real one into `homebrew-tap/Formula/anymd.rb`; use this by hand if the tap is not automated yet |
| `scoop/anymd.json` | reference Scoop manifest with `checkver` + `autoupdate`. Same story: GoReleaser writes the real one into `scoop-bucket/bucket/anymd.json` |

Both carry `REPLACE_WITH_SHA256_*` placeholders. They are meant to be loud: a
manifest shipped with a placeholder still in it fails to install rather than
installing something unverified.

---

## Things that will bite you

- **`use: github` in the changelog config needs a git remote.** A local
  `goreleaser release --snapshot` in a clone with no `origin` fails with
  `scm releases: no remote configured to list refs from`. Not a config bug.
- **The macOS universal archive is `darwin_all`.** `replace: false` means the
  per-arch `darwin_amd64` / `darwin_arm64` archives are published *as well*,
  which is what `install.sh` and the Homebrew formula both download. Do not
  set `replace: true` without updating both.
- **No `main.date` variable exists** in `cmd/anymd/main.go`. GoReleaser's usual
  `-X main.date=...` is intentionally absent from `.goreleaser.yaml`: the Go
  linker silently ignores `-X` for a symbol that does not exist, so adding it
  would look like it worked while doing nothing.
- **`go install` and released binaries report different versions.** That is
  expected; see above.
