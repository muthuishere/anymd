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
	var b strings.Builder
	writeRow(&b, header, n)
	b.WriteString("|")
	for i := 0; i < n; i++ {
		b.WriteString(" --- |")
	}
	for _, r := range rows {
		b.WriteString("\n")
		writeRow(&b, r, n)
	}
	return strings.TrimRight(b.String(), "\n")
}

func writeRow(b *strings.Builder, cells []string, n int) {
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
	b.WriteString("\n")
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
