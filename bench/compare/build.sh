#!/usr/bin/env bash
# Regenerate the three-pane comparison published at /anymd/compare/.
#
# report.html is a plain office document -- title, numbered sections, bulleted
# and numbered lists, a five-column table. LibreOffice exports it to PDF, which
# is how most business PDFs are actually made, and then both converters are run
# over that PDF and their output shipped next to it. Nothing here is tuned for
# anymd: the document is written once, as a document, and whatever the two
# tools make of it is what the page shows.
#
# Requires: go, soffice (LibreOffice), markitdown on PATH.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
OUT="$ROOT/site/public/compare"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

command -v soffice >/dev/null || { echo "soffice (LibreOffice) not on PATH" >&2; exit 2; }
command -v markitdown >/dev/null || { echo "markitdown not on PATH -- pip install 'markitdown[all]'" >&2; exit 2; }

echo "==> building anymd" >&2
go build -o "$TMP/anymd" "$ROOT/cmd/anymd"

echo "==> exporting report.html to PDF" >&2
cp "$ROOT/bench/compare/report.html" "$TMP/report.html"
soffice --headless --convert-to pdf "$TMP/report.html" --outdir "$TMP" >/dev/null 2>&1

echo "==> converting with both tools" >&2
mkdir -p "$OUT"
cp "$TMP/report.pdf" "$OUT/report.pdf"
"$TMP/anymd" "$TMP/report.pdf" > "$OUT/report.anymd.md"
markitdown "$TMP/report.pdf" > "$OUT/report.markitdown.md"

count() { grep -cE "$2" "$1" || true; }
cat > "$OUT/stats.json" <<JSON
{
 "anymd":      {"headings": $(count "$OUT/report.anymd.md" '^#{1,6} '),      "lists": $(count "$OUT/report.anymd.md" '^[[:space:]]*([-*]|[0-9]+\.) '),      "rows": $(count "$OUT/report.anymd.md" '^\|'),      "bytes": $(wc -c < "$OUT/report.anymd.md" | tr -d ' ')},
 "markitdown": {"headings": $(count "$OUT/report.markitdown.md" '^#{1,6} '), "lists": $(count "$OUT/report.markitdown.md" '^[[:space:]]*([-*]|[0-9]+\.) '), "rows": $(count "$OUT/report.markitdown.md" '^\|'), "bytes": $(wc -c < "$OUT/report.markitdown.md" | tr -d ' ')},
 "document":   {"headings": 8, "lists": 8, "rows": 7}
}
JSON
echo "==> wrote $OUT" >&2
cat "$OUT/stats.json"
