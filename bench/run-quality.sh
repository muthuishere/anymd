#!/usr/bin/env bash
# Reproduce the quality numbers in bench/README.md.
#
# Scores anymd's markdown against docling's ground truth, with markitdown
# scored on the same corpus as a control. bench/run.sh measures speed; this
# measures fidelity.
#
# Requires: go, uv (https://docs.astral.sh/uv/).
# Usage: ./bench/run-quality.sh <path to a docling checkout> [--format docx ...]
set -euo pipefail

DOCLING="${1:-}"
if [ -z "$DOCLING" ]; then
  echo "usage: $0 <path to a docling checkout> [extra args]" >&2
  echo "clone https://github.com/docling-project/docling to get the corpus" >&2
  exit 2
fi
shift
[ -d "$DOCLING/tests/data" ] || {
  echo "no tests/data in $DOCLING -- is that a docling checkout?" >&2; exit 2; }

BENCH="${TMPDIR:-/tmp}/anymd-bench"
mkdir -p "$BENCH"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

echo "==> building anymd" >&2
go build -o "$BENCH/anymd" "$ROOT/cmd/anymd"

if [ ! -x "$BENCH/.venv/bin/python" ]; then
  echo "==> installing markitdown into a venv (the control)" >&2
  uv venv --python=3.12 "$BENCH/.venv" >/dev/null
  uv pip install --python "$BENCH/.venv/bin/python" 'markitdown[all]' >/dev/null
fi

exec "$BENCH/.venv/bin/python" "$ROOT/bench/quality.py" \
  "$DOCLING" "$BENCH/anymd" --json "$BENCH/quality.json" "$@"
