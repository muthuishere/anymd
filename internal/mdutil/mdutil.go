// Package mdutil holds the shared GitHub-flavored-Markdown emitters that every
// converter renders through, so a table from xlsx and a table from docx come
// out byte-identical.
package mdutil

import (
	"strings"
	"unicode"
)

// EscapeCell makes text safe inside a GFM pipe-table cell: newlines collapse to
// spaces and pipes are escaped. Matches the rule proven in CiteNexus's emitter.
func EscapeCell(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.TrimSpace(s)
}

// Table renders a GFM pipe table. header may be empty, in which case the first
// row is promoted to the header. Rows shorter than the header are padded and
// longer rows are truncated, so the table is always rectangular and valid.
//
// Lines are joined with exactly one newline and the result has no trailing
// newline: a blank line between rows would terminate the table in GFM, so the
// separator is built by joining, never by appending per-row.
func Table(header []string, rows [][]string) string {
	if len(header) == 0 {
		if len(rows) == 0 {
			return ""
		}
		header, rows = rows[0], rows[1:]
	}
	n := len(header)
	if n == 0 {
		return ""
	}
	lines := make([]string, 0, len(rows)+2)
	lines = append(lines, row(header, n))
	sep := make([]string, n)
	for i := range sep {
		sep[i] = "---"
	}
	lines = append(lines, row(sep, n))
	for _, r := range rows {
		lines = append(lines, row(r, n))
	}
	return strings.Join(lines, "\n")
}

// row renders one pipe-delimited line of exactly n cells, padding or truncating
// to keep the table rectangular. It returns no trailing newline.
func row(cells []string, n int) string {
	var b strings.Builder
	b.WriteString("|")
	for i := 0; i < n; i++ {
		cell := ""
		if i < len(cells) {
			cell = EscapeCell(cells[i])
		}
		b.WriteString(" ")
		b.WriteString(cell)
		b.WriteString(" |")
	}
	return b.String()
}

// Heading renders an ATX heading, clamping the level to GFM's 1..6.
func Heading(level int, text string) string {
	if level < 1 {
		level = 1
	}
	if level > 6 {
		level = 6
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return strings.Repeat("#", level) + " " + text
}

// CodeBlock renders a fenced block, widening the fence when the body itself
// contains a run of backticks.
func CodeBlock(lang, body string) string {
	fence := "```"
	for strings.Contains(body, fence) {
		fence += "`"
	}
	return fence + lang + "\n" + strings.TrimRight(body, "\n") + "\n" + fence
}

// Join assembles blocks into a document: empties dropped, exactly one blank
// line between blocks, exactly one trailing newline (none if empty).
func Join(blocks ...string) string {
	kept := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b = strings.TrimRight(b, " \t\n"); b != "" {
			kept = append(kept, b)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, "\n\n") + "\n"
}

// Collapse squeezes runs of whitespace into single spaces and trims. Use it on
// inline text pulled out of XML, where indentation is meaningless.
func Collapse(s string) string {
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
}
