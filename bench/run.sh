#!/usr/bin/env bash
# Reproduce the numbers in bench/README.md.
#
# Requires: go, uv (https://docs.astral.sh/uv/), hyperfine.
# Usage: ./bench/run.sh [path-to-markitdown-test_files]
set -euo pipefail

CORPUS="${1:-}"
if [ -z "$CORPUS" ]; then
  echo "usage: $0 <path to markitdown's packages/markitdown/tests/test_files>" >&2
  echo "clone https://github.com/microsoft/markitdown to get it" >&2
  exit 2
fi
[ -d "$CORPUS" ] || { echo "no such directory: $CORPUS" >&2; exit 2; }

BENCH="${TMPDIR:-/tmp}/anymd-bench"
mkdir -p "$BENCH"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

echo "==> building anymd"
go build -o "$BENCH/anymd" "$ROOT/cmd/anymd"

echo "==> installing markitdown into a venv"
uv venv --python=3.12 "$BENCH/.venv" >/dev/null
uv pip install --python "$BENCH/.venv/bin/python" 'markitdown[all]' >/dev/null

echo
echo "==> coverage: substantive output (>200 bytes) per tool"
a=0; m=0; total=0
for f in "$CORPUS"/*; do
  [ -f "$f" ] || continue
  total=$((total+1))
  ab=$("$BENCH/anymd" "$f" 2>/dev/null | wc -c | tr -d ' ')
  mb=$("$BENCH/.venv/bin/markitdown" "$f" 2>/dev/null | wc -c | tr -d ' ')
  [ "$ab" -gt 200 ] && a=$((a+1))
  [ "$mb" -gt 200 ] && m=$((m+1))
done
echo "    anymd $a/$total    markitdown $m/$total"

echo
echo "==> CLI end-to-end (includes markitdown's ~300ms interpreter startup)"
for f in test.docx test.xlsx test_wikipedia.html test.pdf; do
  [ -f "$CORPUS/$f" ] || continue
  hyperfine --warmup 3 --runs 15 --style basic \
    -n "anymd $f"      "$BENCH/anymd $CORPUS/$f" \
    -n "markitdown $f" "$BENCH/.venv/bin/markitdown $CORPUS/$f" 2>/dev/null \
    | grep -E "Time|faster"
done

echo
echo "==> in-process (excludes startup; the fair library comparison)"
"$BENCH/.venv/bin/python" - "$CORPUS" <<'PY'
import sys, time
t0 = time.perf_counter()
from markitdown import MarkItDown
md = MarkItDown(enable_plugins=False)
print(f"    markitdown startup: {(time.perf_counter()-t0)*1000:.0f} ms")
T = sys.argv[1]
for f in ["test.docx","test.pdf","test.xlsx","test_wikipedia.html","test.pptx","test.epub"]:
    p = f"{T}/{f}"
    try:
        md.convert(p)
    except Exception:
        continue
    n, t = 10, time.perf_counter()
    for _ in range(n):
        md.convert(p)
    print(f"    markitdown {f:<24}{(time.perf_counter()-t)/n*1000:8.2f} ms")
PY

echo
echo "==> peak RSS on test_wikipedia.html"
if [ "$(uname)" = "Darwin" ]; then TIMEFLAG=-l; else TIMEFLAG=-v; fi
for c in "$BENCH/anymd" "$BENCH/.venv/bin/markitdown"; do
  printf "    %-12s " "$(basename "$c")"
  { /usr/bin/time $TIMEFLAG "$c" "$CORPUS/test_wikipedia.html" >/dev/null; } 2>&1 \
    | awk '/maximum resident|Maximum resident/{printf "%.0f MB\n", ($1>1000000 ? $1/1048576 : $NF/1024)}'
done
