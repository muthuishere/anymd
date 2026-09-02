#!/usr/bin/env python3
"""Output-quality benchmark for anymd, scored against docling's ground truth.

Speed is measured by bench/run.sh. This measures fidelity: how close the
markdown each tool produces is to a known-good target.

The corpus is docling's own test suite, which ships ~150 (source document,
expected markdown) pairs under tests/data/<format>/{sources,groundtruth}.
Two properties make it worth borrowing: the pairs are machine-matchable, and
the files are named for the feature they exercise (docx_lists.docx,
xlsx_07_gap_tolerance_.xlsx), so a low score names the broken feature.

Ground truth is docling's own frozen output, so docling would score 1.0 by
construction and is not run. It is the target, not a contestant. The honest
head-to-head is anymd vs markitdown, both of which are third parties to it.

Scores are style-insensitive by design -- `| --- |` vs `| - |` and `#` vs `##`
are conventions, not defects -- so everything is normalized before comparison.

Usage: quality.py <docling-checkout> <anymd-binary> [--format docx] [--json out]
"""

import argparse
import json
import re
import subprocess
import sys
import unicodedata
from collections import Counter
from difflib import SequenceMatcher
from pathlib import Path

# Extensions anymd claims to convert. Anything else in docling's corpus
# (audio, latex, jats, uspto...) is out of scope and skipped rather than
# counted as a failure.
SUPPORTED = {
    ".docx", ".docm", ".pptx", ".xlsx", ".xlsm", ".xls", ".csv", ".html",
    ".htm", ".md", ".pdf", ".epub", ".json", ".ipynb", ".msg", ".txt",
}

# ---------------------------------------------------------------------------
# normalization
# ---------------------------------------------------------------------------

_EMPH = re.compile(r"(\*\*|__|\*|_|`|~~)")
_LINK = re.compile(r"\[([^\]]*)\]\([^)]*\)")     # keep link text, drop target
_IMG = re.compile(r"!\[([^\]]*)\]\([^)]*\)")     # keep alt text, drop target
_HTML_COMMENT = re.compile(r"<!--.*?-->", re.S)  # docling's <!-- image -->
_HTML_TAG = re.compile(r"<[^>]+>")
_WS = re.compile(r"\s+")
_TOKEN = re.compile(r"[a-z0-9]+")

_HEADING = re.compile(r"^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$")
_LIST = re.compile(r"^\s*(?:[-*+]|\d+[.)])\s+(.*)$")
_TABLE_ROW = re.compile(r"^\s*\|.*\|\s*$")
_TABLE_SEP = re.compile(r"^\s*\|(?:\s*:?-+:?\s*\|)+\s*$")
_FENCE = re.compile(r"^\s*(```|~~~)")


def normalize(text: str) -> str:
    """Strip everything that is a formatting choice rather than content."""
    text = unicodedata.normalize("NFKC", text)
    text = _HTML_COMMENT.sub(" ", text)
    text = _IMG.sub(r"\1", text)
    text = _LINK.sub(r"\1", text)
    text = _HTML_TAG.sub(" ", text)
    text = _EMPH.sub("", text)
    # Unicode punctuation that differs purely by extractor.
    text = text.replace("‘", "'").replace("’", "'")
    text = text.replace("“", '"').replace("”", '"')
    text = text.replace("–", "-").replace("—", "-")
    text = text.replace(" ", " ").replace("​", "")
    return _WS.sub(" ", text).strip().lower()


def tokens(text: str) -> list[str]:
    return _TOKEN.findall(normalize(text))


# ---------------------------------------------------------------------------
# markdown skeleton
# ---------------------------------------------------------------------------

class Skeleton:
    """The structural content of a markdown document, style stripped."""

    def __init__(self, md: str):
        self.headings: list[str] = []
        self.lists: list[str] = []
        self.tables: list[list[list[str]]] = []
        self.code_blocks: int = 0
        self.text: str = ""

        parts: list[str] = []
        table: list[list[str]] = []
        in_fence = False

        for line in md.splitlines():
            if _FENCE.match(line):
                in_fence = not in_fence
                if in_fence:
                    self.code_blocks += 1
                continue
            if in_fence:
                parts.append(line)
                continue

            if _TABLE_ROW.match(line):
                if not _TABLE_SEP.match(line):
                    cells = [normalize(c) for c in line.strip().strip("|").split("|")]
                    table.append(cells)
                    parts.append(" ".join(cells))
                continue
            if table:
                self.tables.append(table)
                table = []

            if m := _HEADING.match(line):
                h = normalize(m.group(2))
                if h:
                    self.headings.append(h)
                    parts.append(h)
                continue
            if m := _LIST.match(line):
                item = normalize(m.group(1))
                if item:
                    self.lists.append(item)
                    parts.append(item)
                continue

            parts.append(line)

        if table:
            self.tables.append(table)

        self.text = normalize(" ".join(parts))
        self.tokens = _TOKEN.findall(self.text)


# ---------------------------------------------------------------------------
# metrics
# ---------------------------------------------------------------------------

def f1(gold: list[str], pred: list[str]) -> float:
    """Order-insensitive token F1. Catches dropped and invented content."""
    if not gold and not pred:
        return 1.0
    if not gold or not pred:
        return 0.0
    overlap = sum((Counter(gold) & Counter(pred)).values())
    if not overlap:
        return 0.0
    p, r = overlap / len(pred), overlap / len(gold)
    return 2 * p * r / (p + r)


def order_similarity(gold: list[str], pred: list[str]) -> float:
    """Order-SENSITIVE similarity over the token sequence.

    This is the metric that catches scrambled reading order. A two-column PDF
    read straight down the page keeps every token -- content F1 stays high --
    but interleaves them, which collapses this number. The gap between the two
    is the reading-order defect, isolated.

    autojunk must stay off: it treats tokens appearing in >1% of a long
    sequence as noise, which for prose means discarding ordinary words.
    """
    if not gold and not pred:
        return 1.0
    if not gold or not pred:
        return 0.0
    # Cap for tractability; SequenceMatcher is superlinear on long inputs.
    cap = 12000
    return SequenceMatcher(None, gold[:cap], pred[:cap], autojunk=False).ratio()


def table_score(gold: list, pred: list) -> tuple[float, int, int]:
    """Mean best-match cell fidelity over the ground-truth tables.

    Each ground-truth table is matched to its best candidate by cell-token F1,
    so table ordering does not matter but missing tables score 0. Full TEDS is
    the literature standard; cell F1 captures the failures that actually bite
    -- a dropped table, a wrong column count, a mishandled merged cell.
    """
    if not gold:
        return (1.0 if not pred else 1.0), len(gold), len(pred)
    scores = []
    for g in gold:
        g_tokens = [t for row in g for cell in row for t in _TOKEN.findall(cell)]
        best = 0.0
        for p in pred:
            p_tokens = [t for row in p for cell in row for t in _TOKEN.findall(cell)]
            best = max(best, f1(g_tokens, p_tokens))
        scores.append(best)
    return sum(scores) / len(scores), len(gold), len(pred)


def recall(gold: list[str], pred: list[str]) -> float:
    """Fraction of ground-truth items present in the candidate.

    Matching is fuzzy: an item counts as found if some candidate item is a
    close string match, so minor extraction noise does not zero out a hit.
    """
    if not gold:
        return 1.0
    pred_set = set(pred)
    found = 0
    for g in gold:
        if g in pred_set:
            found += 1
            continue
        if any(SequenceMatcher(None, g, p).ratio() > 0.9 for p in pred):
            found += 1
    return found / len(gold)


def score(gt_md: str, out_md: str, status: str = "ok") -> dict:
    gold, pred = Skeleton(gt_md), Skeleton(out_md)
    tbl, n_gold_tbl, n_pred_tbl = table_score(gold.tables, pred.tables)
    return {
        "status": status,
        "content_f1": f1(gold.tokens, pred.tokens),
        "order_sim": order_similarity(gold.tokens, pred.tokens),
        "table": tbl,
        "heading_recall": recall(gold.headings, pred.headings),
        "list_recall": recall(gold.lists, pred.lists),
        "gt_tokens": len(gold.tokens),
        "out_tokens": len(pred.tokens),
        "gt_tables": n_gold_tbl,
        "out_tables": n_pred_tbl,
    }


# ---------------------------------------------------------------------------
# corpus + runners
# ---------------------------------------------------------------------------

def find_pairs(docling: Path) -> list[tuple[str, Path, Path]]:
    """Yield (format, source, ground-truth markdown) for every usable pair."""
    pairs = []
    for src_dir in sorted(docling.glob("tests/data/*/sources")):
        fmt = src_dir.parent.name
        gt_dir = src_dir.parent / "groundtruth"
        if not gt_dir.is_dir():
            continue
        for src in sorted(src_dir.iterdir()):
            if not src.is_file() or src.suffix.lower() not in SUPPORTED:
                continue
            # Two naming conventions in the corpus: <name.ext>.md, and for
            # pdf/, <name>.md with the extension dropped.
            for gt in (gt_dir / f"{src.name}.md", gt_dir / f"{src.stem}.md"):
                if gt.is_file():
                    pairs.append((fmt, src, gt))
                    break
    return pairs


def run_anymd(binary: Path, src: Path) -> tuple[str, str]:
    try:
        r = subprocess.run([str(binary), str(src)], capture_output=True,
                           timeout=120)
    except Exception:
        return "", "declined"
    out = r.stdout.decode("utf-8", "replace")
    if r.returncode != 0:
        return out, "declined"
    return out, "ok" if out.strip() else "empty"


def make_markitdown():
    try:
        from markitdown import MarkItDown
    except ImportError:
        return None
    md = MarkItDown(enable_plugins=False)

    def run(src: Path) -> tuple[str, str]:
        try:
            out = md.convert(str(src)).text_content or ""
        except Exception:
            return "", "declined"
        return out, "ok" if out.strip() else "empty"
    return run


# ---------------------------------------------------------------------------
# reporting
# ---------------------------------------------------------------------------

METRICS = ["content_f1", "order_sim", "table", "heading_recall", "list_recall"]
LABELS = {
    "content_f1": "content",
    "order_sim": "order",
    "table": "tables",
    "heading_recall": "headings",
    "list_recall": "lists",
}


def converted(rows: list[dict]) -> list[dict]:
    """Rows the tool actually attempted.

    An explicit refusal (nonzero exit, raised exception) is withheld from the
    fidelity means and counted on its own: it is a caller-actionable signal,
    not bad output, and averaging it in as 0.00 would understate a tool that
    fails loudly instead of quietly. Exit-0-with-empty-output is NOT withheld
    -- that is the silent-failure mode, and it scores 0 like any other miss.
    """
    return [r for r in rows if r.get("status") != "declined"]


def mean(rows: list[dict], key: str) -> float:
    vals = [r[key] for r in rows if key in r]
    return sum(vals) / len(vals) if vals else 0.0


def _fidelity_table(results: list[dict], formats: list[str], tool: str) -> None:
    print("| format | n | declined | " + " | ".join(
        LABELS[m] for m in METRICS) + " |")
    print("|---|---:|---:|" + "---:|" * len(METRICS))
    for fmt in formats + [None]:
        all_rows = [r[tool] for r in results
                    if tool in r and (fmt is None or r["format"] == fmt)]
        if not all_rows:
            continue
        rows = converted(all_rows)
        dec = len(all_rows) - len(rows)
        cells = " | ".join(f"{mean(rows, m):.2f}" for m in METRICS)
        name = "**all**" if fmt is None else fmt
        print(f"| {name} | {len(all_rows)} | {dec} | {cells} |")


def report(results: list[dict], tools: list[str]) -> None:
    formats = sorted({r["format"] for r in results})

    for tool in tools:
        print(f"\n## {tool}\n")
        _fidelity_table(results, formats, tool)

    if len(tools) > 1:
        a, b = tools[0], tools[1]
        print(f"\n## head-to-head ({a} vs {b})\n")
        print("| format | n | " + " | ".join(
            f"{LABELS[m]} {a}/{b}" for m in METRICS) + " |")
        print("|---|---:|" + "---:|" * len(METRICS))
        for fmt in formats + [None]:
            rows = [r for r in results
                    if a in r and b in r and (fmt is None or r["format"] == fmt)]
            if not rows:
                continue
            ra, rb = converted([r[a] for r in rows]), converted([r[b] for r in rows])
            cells = " | ".join(
                f"{mean(ra, m):.2f} / {mean(rb, m):.2f}" for m in METRICS)
            name = "**all**" if fmt is None else fmt
            print(f"| {name} | {len(rows)} | {cells} |")

    print("\n## worst files for anymd\n")
    print("| file | status | content | order | tables | headings | lists |")
    print("|---|---|---:|---:|---:|---:|---:|")
    scored = [r for r in results if "anymd" in r]
    scored.sort(key=lambda r: (r["anymd"]["content_f1"] + r["anymd"]["order_sim"]) / 2)
    for r in scored[:25]:
        s = r["anymd"]
        print(f"| {r['file']} | {s['status']} | " + " | ".join(
            f"{s[m]:.2f}" for m in METRICS) + " |")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("docling", type=Path, help="path to a docling checkout")
    ap.add_argument("anymd", type=Path, help="path to the anymd binary")
    ap.add_argument("--format", action="append", help="limit to these formats")
    ap.add_argument("--json", type=Path, help="also write raw per-file scores")
    args = ap.parse_args()

    pairs = find_pairs(args.docling)
    if args.format:
        pairs = [p for p in pairs if p[0] in args.format]
    if not pairs:
        print("no (source, ground-truth) pairs found -- is that a docling "
              "checkout with tests/data?", file=sys.stderr)
        return 2

    markitdown = make_markitdown()
    tools = ["anymd"] + (["markitdown"] if markitdown else [])
    print(f"corpus: {len(pairs)} documents from {args.docling}", file=sys.stderr)
    if not markitdown:
        print("markitdown not importable -- scoring anymd only", file=sys.stderr)

    results = []
    for i, (fmt, src, gt) in enumerate(pairs, 1):
        print(f"\r  {i}/{len(pairs)} {src.name[:50]:<50}", end="", file=sys.stderr)
        gt_md = gt.read_text(encoding="utf-8", errors="replace")
        if not gt_md.strip():
            continue
        row = {"format": fmt, "file": src.name}
        row["anymd"] = score(gt_md, *run_anymd(args.anymd, src))
        if markitdown:
            row["markitdown"] = score(gt_md, *markitdown(src))
        results.append(row)
    print("\r" + " " * 70 + "\r", end="", file=sys.stderr)

    report(results, tools)

    if args.json:
        args.json.write_text(json.dumps(results, indent=2))
        print(f"\nraw scores: {args.json}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
