// Package mdutil holds the shared GitHub-flavored-Markdown emitters.
//
// The one rule that matters: every converter renders tables, headings and code
// through these functions and never hand-rolls markup. That is what makes the
// output byte-identical regardless of source format — a table lifted out of an
// .xlsx, a .docx, a .csv and an .html page all come out as the same bytes, so
// converted documents are safe to commit and diff, and a regression in one
// format cannot quietly change the shape of another.
//
// Two contracts are load-bearing and easy to break by accident:
//
// [Table] emits its lines joined by exactly one newline, with no trailing
// newline and no blank line anywhere inside the block. A blank line terminates
// a GFM table: one stray newline between rows does not produce a slightly odd
// table, it silently splits one table into a truncated table followed by
// unrelated paragraphs of pipe characters. This was a real bug. Build a table
// by joining lines, never by appending a newline per row.
//
// [Join] assembles blocks into a finished document: empty blocks are dropped
// rather than emitted as blank space, blocks are separated by exactly one blank
// line, and the result ends with exactly one trailing newline (or is empty, if
// every block was). Converters therefore hand Join whatever they produced,
// including empties, and never trim or pad themselves.
//
// [Heading] clamps to GFM's 1..6 and returns "" for empty text (which Join then
// drops). [CodeBlock] widens its fence past any backtick run in the body.
// [EscapeCell] flattens newlines and escapes pipes for table cells, and
// [Collapse] squeezes whitespace runs in inline text pulled out of XML.
package mdutil
