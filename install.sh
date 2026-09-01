#!/bin/sh
#
# anymd installer — for people who do not have Homebrew or a Go toolchain.
#
#   curl -sSfL https://raw.githubusercontent.com/muthuishere/anymd/main/install.sh | sh
#
# Pin a version:
#
#   curl -sSfL .../install.sh | VERSION=v0.1.0 sh
#   ./install.sh -v v0.1.0
#
# Choose where it lands:
#
#   curl -sSfL .../install.sh | PREFIX=$HOME/bin sh
#
# The download is ALWAYS verified against the release's checksums.txt. An
# installer that skips that is a supply-chain hole, so there is no flag to
# turn it off.
#
# This script never calls sudo. If it cannot write to /usr/local/bin it falls
# back to ~/.local/bin and tells you; re-run with PREFIX=... to decide yourself.

set -eu

REPO="muthuishere/anymd"
BIN="anymd"
VERSION="${VERSION:-}"
PREFIX="${PREFIX:-}"
TMPDIR_ANYMD=""

# ---------------------------------------------------------------- utilities

log()  { printf '%s\n' "$*" >&2; }
err()  { printf 'anymd install: %s\n' "$*" >&2; }
die()  { err "$*"; exit 1; }

cleanup() {
	if [ -n "$TMPDIR_ANYMD" ] && [ -d "$TMPDIR_ANYMD" ]; then
		rm -rf "$TMPDIR_ANYMD"
	fi
}
trap cleanup EXIT INT TERM HUP

have() { command -v "$1" >/dev/null 2>&1; }

usage() {
	cat >&2 <<'EOF'
usage: install.sh [-v VERSION] [-p PREFIX] [-h]

  -v VERSION   release tag to install, e.g. v0.1.0 (default: latest)
  -p PREFIX    directory to install into (default: /usr/local/bin if writable,
               else ~/.local/bin)
  -h           this help

Environment equivalents: VERSION=, PREFIX=
EOF
}

# ---------------------------------------------------------------- arguments

while [ $# -gt 0 ]; do
	case "$1" in
		-v|--version)
			[ $# -ge 2 ] || die "-v needs a version, e.g. -v v0.1.0"
			VERSION="$2"; shift 2 ;;
		-p|--prefix)
			[ $# -ge 2 ] || die "-p needs a directory"
			PREFIX="$2"; shift 2 ;;
		-h|--help)
			usage; exit 0 ;;
		*)
			err "unknown argument: $1"; usage; exit 2 ;;
	esac
done

# ---------------------------------------------------------------- platform

detect_os() {
	os="$(uname -s)"
	case "$os" in
		Linux)   echo linux ;;
		Darwin)  echo darwin ;;
		MINGW*|MSYS*|CYGWIN*)
			die "Windows detected ($os). Use scoop instead:
    scoop bucket add muthuishere https://github.com/muthuishere/scoop-bucket
    scoop install anymd
  or download the windows zip from https://github.com/$REPO/releases" ;;
		*)
			die "unsupported operating system: $os
  anymd publishes darwin and linux archives. On any other Go-supported OS,
  build from source:  go install github.com/$REPO/cmd/$BIN@latest" ;;
	esac
}

# Map uname's arch names onto Go's GOARCH names — these genuinely differ, and
# guessing wrong downloads a binary that will not exec.
detect_arch() {
	arch="$(uname -m)"
	case "$arch" in
		x86_64|amd64)          echo amd64 ;;
		aarch64|arm64)         echo arm64 ;;
		i386|i686|x86)         echo 386 ;;
		*)
			die "unsupported CPU architecture: $arch
  anymd publishes amd64, arm64 and (linux) 386. Build from source instead:
    go install github.com/$REPO/cmd/$BIN@latest" ;;
	esac
}

OS="$(detect_os)"
ARCH="$(detect_arch)"

case "$OS/$ARCH" in
	darwin/386)
		die "there is no 32-bit macOS build; macOS has not shipped one since 10.15" ;;
	linux/386|linux/amd64|linux/arm64|darwin/amd64|darwin/arm64)
		: ;;
	*)
		die "no published archive for $OS/$ARCH" ;;
esac

# ---------------------------------------------------------------- downloader

if have curl; then
	DL="curl"
elif have wget; then
	DL="wget"
else
	die "need either curl or wget on PATH to download anything"
fi

# fetch URL DEST
fetch() {
	if [ "$DL" = curl ]; then
		curl -sSfL --retry 3 --retry-delay 1 -o "$2" "$1"
	else
		wget -q -O "$2" "$1"
	fi
}

# fetch_stdout URL
fetch_stdout() {
	if [ "$DL" = curl ]; then
		curl -sSfL --retry 3 --retry-delay 1 "$1"
	else
		wget -q -O - "$1"
	fi
}

# ---------------------------------------------------------------- version

if [ -z "$VERSION" ]; then
	log "resolving the latest anymd release..."
	api="https://api.github.com/repos/$REPO/releases/latest"
	# Deliberately no jq dependency: one grep for the tag_name field. If the
	# API is rate-limited or the repo has no release yet, this comes back
	# empty and we say so rather than downloading a 404 page.
	VERSION="$(fetch_stdout "$api" 2>/dev/null \
		| tr ',' '\n' \
		| grep -m1 '"tag_name"' \
		| sed -e 's/.*"tag_name"[[:space:]]*:[[:space:]]*"//' -e 's/".*//' || true)"
	[ -n "$VERSION" ] || die "could not determine the latest release from $api
  GitHub may be rate-limiting you (60 requests/hour unauthenticated), or there
  may be no published release yet. Pin one explicitly:
    curl -sSfL .../install.sh | VERSION=v0.1.0 sh"
fi

case "$VERSION" in
	v*) TAG="$VERSION" ;;
	*)  TAG="v$VERSION" ;;
esac
# Archive names carry the bare version, the tag carries the leading v.
NUM="${TAG#v}"

ARCHIVE="${BIN}_${NUM}_${OS}_${ARCH}.tar.gz"
BASEURL="https://github.com/$REPO/releases/download/$TAG"

# ---------------------------------------------------------------- download

TMPDIR_ANYMD="$(mktemp -d 2>/dev/null || mktemp -d -t anymd)"
[ -d "$TMPDIR_ANYMD" ] || die "could not create a temporary directory"

log "downloading $ARCHIVE ($TAG)"
if ! fetch "$BASEURL/$ARCHIVE" "$TMPDIR_ANYMD/$ARCHIVE"; then
	die "download failed: $BASEURL/$ARCHIVE
  Check that $TAG exists at https://github.com/$REPO/releases"
fi

log "downloading checksums.txt"
fetch "$BASEURL/checksums.txt" "$TMPDIR_ANYMD/checksums.txt" \
	|| die "could not download checksums.txt from $BASEURL — refusing to install an unverified binary"

# ---------------------------------------------------------------- verify

sha256_of() {
	if have sha256sum; then
		sha256sum "$1" | cut -d' ' -f1
	elif have shasum; then
		shasum -a 256 "$1" | cut -d' ' -f1
	elif have openssl; then
		openssl dgst -sha256 "$1" | sed 's/.*= *//'
	else
		return 1
	fi
}

want="$(grep " \{1,2\}\*\{0,1\}${ARCHIVE}\$" "$TMPDIR_ANYMD/checksums.txt" | cut -d' ' -f1 || true)"
[ -n "$want" ] || die "checksums.txt has no entry for $ARCHIVE — refusing to install"

if got="$(sha256_of "$TMPDIR_ANYMD/$ARCHIVE")"; then
	if [ "$got" != "$want" ]; then
		die "CHECKSUM MISMATCH for $ARCHIVE
  expected: $want
  actual:   $got
  Do not use this file. Report it at https://github.com/$REPO/issues"
	fi
	log "checksum ok (sha256 ${want})"
else
	die "no sha256 tool found (sha256sum, shasum, or openssl) — refusing to install
  an unverified binary. Install one of those, or download and verify by hand:
    $BASEURL/$ARCHIVE"
fi

# ---------------------------------------------------------------- extract

have tar || die "need tar on PATH to unpack the archive"
tar -xzf "$TMPDIR_ANYMD/$ARCHIVE" -C "$TMPDIR_ANYMD" \
	|| die "could not unpack $ARCHIVE"
[ -f "$TMPDIR_ANYMD/$BIN" ] || die "archive did not contain a '$BIN' binary — this is a packaging bug, please report it"
chmod +x "$TMPDIR_ANYMD/$BIN"

# ---------------------------------------------------------------- install

writable_dir() { [ -d "$1" ] && [ -w "$1" ]; }

if [ -z "$PREFIX" ]; then
	if writable_dir /usr/local/bin; then
		PREFIX=/usr/local/bin
	else
		PREFIX="$HOME/.local/bin"
	fi
fi

mkdir -p "$PREFIX" 2>/dev/null || die "cannot create $PREFIX
  Pick a writable directory:  PREFIX=\$HOME/bin sh install.sh"
writable_dir "$PREFIX" || die "$PREFIX is not writable by $(id -un)
  Either pick another directory (PREFIX=\$HOME/bin) or, if you really want a
  system-wide install, re-run this script yourself under sudo. This script
  will not invoke sudo on your behalf."

# Install via a temp name + mv so a running anymd is replaced atomically
# instead of being truncated under its own feet.
cp "$TMPDIR_ANYMD/$BIN" "$PREFIX/.$BIN.new.$$"
chmod 0755 "$PREFIX/.$BIN.new.$$"
mv -f "$PREFIX/.$BIN.new.$$" "$PREFIX/$BIN"

log ""
log "installed $BIN $TAG -> $PREFIX/$BIN"

# ---------------------------------------------------------------- PATH check

case ":${PATH}:" in
	*":$PREFIX:"*)
		log ""
		log "try it:  $BIN --version   ·   printf 'a,b\\n1,2\\n' | $BIN -t csv"
		;;
	*)
		log ""
		log "WARNING: $PREFIX is not on your PATH."
		log "Add this to your shell profile (~/.zshrc, ~/.bashrc, ~/.profile):"
		log ""
		log "    export PATH=\"$PREFIX:\$PATH\""
		log ""
		log "Until then, run it by full path:  $PREFIX/$BIN --version"
		;;
esac
