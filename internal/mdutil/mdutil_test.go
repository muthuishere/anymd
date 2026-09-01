package mdutil

import (
	"strings"
	"testing"
)

func TestEscapeCell(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain", "hello", "hello"},
		{"pipe escaped", "a|b", `a\|b`},
		{"every pipe escaped", "|a|b|", `\|a\|b\|`},
		{"newline flattened", "a\nb", "a b"},
		{"crlf flattened", "a\r\nb", "a b"},
		{"mixed line endings flattened", "a\r\nb\nc", "a b c"},
		{"pipes and newlines together", "a|b\nc|d", `a\|b c\|d`},
		{"outer space trimmed", "  padded  ", "padded"},
		{"leading newline trimmed after flattening", "\nvalue\n", "value"},
		{"inner spacing preserved", "two  spaces", "two  spaces"},
		{"tab preserved", "a\tb", "a\tb"},
		{"already escaped pipe is double escaped", `a\|b`, `a\\|b`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EscapeCell(tt.in); got != tt.want {
				t.Errorf("EscapeCell(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestTable is the rectangularity gate. Whatever the caller hands in, the
// emitted table must have exactly len(header) columns on every line and no
// blank line anywhere inside it — a blank line terminates a GFM table, so one
// stray newline silently splits the table into unrelated paragraphs.
func TestTable(t *testing.T) {
	tests := []struct {
		name   string
		header []string
		rows   [][]string
		want   string
	}{
		{
			name: "no header and no rows renders nothing",
			want: "",
		},
		{
			name:   "header only",
			header: []string{"A", "B"},
			want:   "| A | B |\n| --- | --- |",
		},
		{
			name:   "single column, single row",
			header: []string{"A"},
			rows:   [][]string{{"1"}},
			want:   "| A |\n| --- |\n| 1 |",
		},
		{
			name:   "two rows",
			header: []string{"A", "B"},
			rows:   [][]string{{"1", "2"}, {"3", "4"}},
			want:   "| A | B |\n| --- | --- |\n| 1 | 2 |\n| 3 | 4 |",
		},
		{
			name:   "short row is padded",
			header: []string{"A", "B", "C"},
			rows:   [][]string{{"1"}},
			want:   "| A | B | C |\n| --- | --- | --- |\n| 1 |  |  |",
		},
		{
			name:   "empty row is padded",
			header: []string{"A", "B"},
			rows:   [][]string{{}},
			want:   "| A | B |\n| --- | --- |\n|  |  |",
		},
		{
			name:   "long row is truncated",
			header: []string{"A", "B"},
			rows:   [][]string{{"1", "2", "3", "4"}},
			want:   "| A | B |\n| --- | --- |\n| 1 | 2 |",
		},
		{
			name:   "ragged rows all come out rectangular",
			header: []string{"A", "B", "C"},
			rows:   [][]string{{"1"}, {"1", "2", "3", "4"}, {"1", "2", "3"}},
			want: "| A | B | C |\n| --- | --- | --- |\n" +
				"| 1 |  |  |\n| 1 | 2 | 3 |\n| 1 | 2 | 3 |",
		},
		{
			name: "empty header promotes the first row",
			rows: [][]string{{"A", "B"}, {"1", "2"}},
			want: "| A | B |\n| --- | --- |\n| 1 | 2 |",
		},
		{
			name: "empty header with a single row leaves a header-only table",
			rows: [][]string{{"A", "B"}},
			want: "| A | B |\n| --- | --- |",
		},
		{
			name: "empty header and a zero-width first row renders nothing",
			rows: [][]string{{}, {"1"}},
			want: "",
		},
		{
			name:   "cells are escaped",
			header: []string{"a|b"},
			rows:   [][]string{{"x\ny"}},
			want:   `| a\|b |` + "\n| --- |\n| x y |",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Table(tt.header, tt.rows)
			if got != tt.want {
				t.Errorf("Table() =\n%q\nwant\n%q", got, tt.want)
			}
			assertRectangular(t, got)
		})
	}
}

// assertRectangular is the property behind every Table case: same pipe count on
// every line, and never a blank line inside the block.
func assertRectangular(t *testing.T, table string) {
	t.Helper()
	if table == "" {
		return
	}
	lines := strings.Split(table, "\n")
	want := countDelims(lines[0])
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			t.Errorf("line %d is blank; a blank line ends a GFM table:\n%q", i, table)
			continue
		}
		if n := countDelims(ln); n != want {
			t.Errorf("line %d has %d delimiters, want %d:\n%q", i, n, want, table)
		}
	}
}

// countDelims counts the pipes that actually delimit cells, i.e. the ones not
// escaped by a preceding backslash.
func countDelims(line string) int {
	n := 0
	for i := 0; i < len(line); i++ {
		if line[i] == '|' && (i == 0 || line[i-1] != '\\') {
			n++
		}
	}
	return n
}

func TestHeading(t *testing.T) {
	tests := []struct {
		name  string
		level int
		text  string
		want  string
	}{
		{"level 1", 1, "Title", "# Title"},
		{"level 6", 6, "Title", "###### Title"},
		{"level 3", 3, "Title", "### Title"},
		{"clamped below 1", 0, "Title", "# Title"},
		{"clamped from negative", -5, "Title", "# Title"},
		{"clamped above 6", 7, "Title", "###### Title"},
		{"clamped from far above 6", 99, "Title", "###### Title"},
		{"empty text renders nothing", 2, "", ""},
		{"whitespace text renders nothing", 2, "   \n\t ", ""},
		{"empty text at a clamped level still renders nothing", 0, "", ""},
		{"text is trimmed", 2, "  Title  ", "## Title"},
		{"inner text is left alone", 2, "A | B", "## A | B"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Heading(tt.level, tt.text); got != tt.want {
				t.Errorf("Heading(%d, %q) = %q, want %q", tt.level, tt.text, got, tt.want)
			}
		})
	}
}

func TestCodeBlock(t *testing.T) {
	tests := []struct {
		name string
		lang string
		body string
		want string
	}{
		{"plain", "", "x := 1", "```\nx := 1\n```"},
		{"with language", "go", "x := 1", "```go\nx := 1\n```"},
		{"empty body", "go", "", "```go\n\n```"},
		{"trailing newlines trimmed to one", "go", "x := 1\n\n\n", "```go\nx := 1\n```"},
		{"inner blank lines kept", "go", "a\n\nb", "```go\na\n\nb\n```"},
		{
			name: "fence widens past a triple backtick in the body",
			lang: "md",
			body: "see ``` here",
			want: "````md\nsee ``` here\n````",
		},
		{
			name: "fence widens past a quadruple backtick in the body",
			lang: "",
			body: "````",
			want: "`````\n````\n`````",
		},
		{
			name: "one and two backticks do not widen the fence",
			lang: "",
			body: "a `b` and ``c``",
			want: "```\na `b` and ``c``\n```",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CodeBlock(tt.lang, tt.body)
			if got != tt.want {
				t.Errorf("CodeBlock(%q, %q) = %q, want %q", tt.lang, tt.body, got, tt.want)
			}
			// The fence must always be longer than the longest backtick run
			// inside, or the block closes early and the rest leaks as prose.
			fence := got[:strings.IndexByte(got, '\n')]
			fence = strings.TrimSuffix(fence, tt.lang)
			if strings.Contains(tt.body, fence) {
				t.Errorf("body contains the fence %q, so the block closes early", fence)
			}
		})
	}
}

func TestJoin(t *testing.T) {
	tests := []struct {
		name   string
		blocks []string
		want   string
	}{
		{"nothing", nil, ""},
		{"only empties", []string{"", "", ""}, ""},
		{"only whitespace", []string{"  ", "\n\n", "\t"}, ""},
		{"one block", []string{"a"}, "a\n"},
		{"one block already newline-terminated", []string{"a\n"}, "a\n"},
		{"one block with many trailing newlines", []string{"a\n\n\n"}, "a\n"},
		{"two blocks", []string{"a", "b"}, "a\n\nb\n"},
		{"empties dropped between blocks", []string{"a", "", "b"}, "a\n\nb\n"},
		{"leading and trailing empties dropped", []string{"", "a", "b", ""}, "a\n\nb\n"},
		{"trailing newlines normalised to one blank line", []string{"a\n\n", "b\n"}, "a\n\nb\n"},
		{"multi-line blocks keep their inner newlines", []string{"a\nb", "c"}, "a\nb\n\nc\n"},
		{"leading whitespace is preserved", []string{"    indented"}, "    indented\n"},
		{"three blocks", []string{"a", "b", "c"}, "a\n\nb\n\nc\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Join(tt.blocks...)
			if got != tt.want {
				t.Errorf("Join(%q) = %q, want %q", tt.blocks, got, tt.want)
			}
			if got != "" {
				if !strings.HasSuffix(got, "\n") || strings.HasSuffix(got, "\n\n") {
					t.Errorf("Join(%q) = %q, want exactly one trailing newline", tt.blocks, got)
				}
			}
		})
	}
}

func TestCollapse(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"whitespace only", " \t\n ", ""},
		{"plain", "hello", "hello"},
		{"runs squeezed", "a    b", "a b"},
		{"newlines squeezed", "a\n\n\tb", "a b"},
		{"outer trimmed", "  a b  ", "a b"},
		{"xml indentation", "\n      Some\n      text\n    ", "Some text"},
		{"non-breaking space is whitespace", "a\u00a0b", "a b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Collapse(tt.in); got != tt.want {
				t.Errorf("Collapse(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
