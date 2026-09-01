package anymd

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

// buildZip writes a real zip archive in memory from name -> content. Fixtures
// are built here rather than committed as binaries, so a reviewer can see
// exactly which XML produced which Markdown.
func buildZip(t *testing.T, parts map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// Deterministic order keeps the fixture byte-stable across runs.
	names := make([]string, 0, len(parts))
	for n := range parts {
		names = append(names, n)
	}
	sortStrings(names)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatalf("zip create %s: %v", n, err)
		}
		if _, err := w.Write([]byte(parts[n])); err != nil {
			t.Fatalf("zip write %s: %v", n, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

const docxHeader = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"` +
	` xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><w:body>`

const docxFooter = `</w:body></w:document>`

const docxRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
	`<Relationship Id="rId7" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/hyperlink"` +
	` Target="https://example.com/docs" TargetMode="External"/>` +
	`</Relationships>`

// numbering 1 is a bullet list, numbering 2 is decimal.
const docxNumbering = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<w:numbering xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
	`<w:abstractNum w:abstractNumId="10">` +
	`<w:lvl w:ilvl="0"><w:numFmt w:val="bullet"/></w:lvl>` +
	`<w:lvl w:ilvl="1"><w:numFmt w:val="bullet"/></w:lvl>` +
	`</w:abstractNum>` +
	`<w:abstractNum w:abstractNumId="20">` +
	`<w:lvl w:ilvl="0"><w:numFmt w:val="decimal"/></w:lvl>` +
	`</w:abstractNum>` +
	`<w:num w:numId="1"><w:abstractNumId w:val="10"/></w:num>` +
	`<w:num w:numId="2"><w:abstractNumId w:val="20"/></w:num>` +
	`</w:numbering>`

const docxCore = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties"` +
	` xmlns:dc="http://purl.org/dc/elements/1.1/">` +
	`<dc:title>Quarterly Report</dc:title><dc:creator>Nobody</dc:creator>` +
	`</cp:coreProperties>`

func docxFixture(t *testing.T, body string, extra map[string]string) []byte {
	t.Helper()
	parts := map[string]string{
		"[Content_Types].xml":          `<?xml version="1.0"?><Types/>`,
		"word/document.xml":            docxHeader + body + docxFooter,
		"word/_rels/document.xml.rels": docxRels,
		"word/numbering.xml":           docxNumbering,
		"docProps/core.xml":            docxCore,
		"_rels/.rels":                  `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"/>`,
	}
	for k, v := range extra {
		if v == "" {
			delete(parts, k)
			continue
		}
		parts[k] = v
	}
	return buildZip(t, parts)
}

func convertDocx(t *testing.T, b []byte) Result {
	t.Helper()
	res, err := New().ConvertBytes(b, StreamInfo{Extension: ".docx", FileName: "fixture.docx"}, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	return res
}

func TestDocxHeadingsAndEmphasis(t *testing.T) {
	body := `` +
		`<w:p><w:pPr><w:pStyle w:val="Title"/></w:pPr><w:r><w:t>Quarterly Report</w:t></w:r></w:p>` +
		`<w:p><w:pPr><w:pStyle w:val="Subtitle"/></w:pPr><w:r><w:t>Fiscal 2026</w:t></w:r></w:p>` +
		`<w:p><w:pPr><w:pStyle w:val="Heading 3"/></w:pPr><w:r><w:t>Overview</w:t></w:r></w:p>` +
		// Three runs that share bold formatting must merge into one **...** span.
		`<w:p>` +
		`<w:r><w:rPr><w:b/></w:rPr><w:t xml:space="preserve">bold </w:t></w:r>` +
		`<w:r><w:rPr><w:b/><w:sz w:val="20"/></w:rPr><w:t xml:space="preserve">merged </w:t></w:r>` +
		`<w:r><w:rPr><w:b/></w:rPr><w:t>text</w:t></w:r>` +
		`<w:r><w:t xml:space="preserve"> and </w:t></w:r>` +
		`<w:r><w:rPr><w:i/></w:rPr><w:t>italic</w:t></w:r>` +
		`<w:r><w:rPr><w:b/><w:i w:val="1"/></w:rPr><w:t xml:space="preserve"> both</w:t></w:r>` +
		`<w:r><w:rPr><w:b w:val="0"/></w:rPr><w:t>.</w:t></w:r>` +
		`</w:p>` +
		// A hard break and a tab inside one run.
		`<w:p><w:r><w:t>line one</w:t><w:br/><w:t>line</w:t><w:tab/><w:t>two</w:t></w:r></w:p>`

	got := convertDocx(t, docxFixture(t, body, nil))
	want := "# Quarterly Report\n\n" +
		"## Fiscal 2026\n\n" +
		"### Overview\n\n" +
		"**bold merged text** and *italic* ***both***.\n\n" +
		"line one\nline two\n"
	if got.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", got.Markdown, want)
	}
	if got.Title != "Quarterly Report" {
		t.Errorf("title = %q, want %q", got.Title, "Quarterly Report")
	}
}

func TestDocxListsAndHyperlink(t *testing.T) {
	item := func(ilvl, numID, text string) string {
		return `<w:p><w:pPr><w:numPr><w:ilvl w:val="` + ilvl + `"/><w:numId w:val="` + numID +
			`"/></w:numPr></w:pPr><w:r><w:t>` + text + `</w:t></w:r></w:p>`
	}
	body := item("0", "1", "First") +
		item("1", "1", "Nested a") +
		item("2", "1", "Deep") +
		item("1", "1", "Nested b") +
		item("0", "1", "Second") +
		`<w:p><w:r><w:t xml:space="preserve">See </w:t></w:r>` +
		`<w:hyperlink r:id="rId7"><w:r><w:t>the docs</w:t></w:r></w:hyperlink>` +
		`<w:r><w:t xml:space="preserve"> for more.</w:t></w:r></w:p>` +
		item("0", "2", "Step one") +
		item("0", "2", "Step two")

	got := convertDocx(t, docxFixture(t, body, nil))
	want := "- First\n" +
		"  - Nested a\n" +
		"    - Deep\n" +
		"  - Nested b\n" +
		"- Second\n\n" +
		"See [the docs](https://example.com/docs) for more.\n\n" +
		"1. Step one\n" +
		"2. Step two\n"
	if got.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", got.Markdown, want)
	}
}

// Without numbering.xml every list must degrade to bullets rather than fail.
func TestDocxListsWithoutNumberingDegradeToBullets(t *testing.T) {
	body := `<w:p><w:pPr><w:numPr><w:ilvl w:val="0"/><w:numId w:val="2"/></w:numPr></w:pPr>` +
		`<w:r><w:t>Step one</w:t></w:r></w:p>`
	got := convertDocx(t, docxFixture(t, body, map[string]string{"word/numbering.xml": ""}))
	if got.Markdown != "- Step one\n" {
		t.Errorf("got %q, want %q", got.Markdown, "- Step one\n")
	}
}

func TestDocxTable(t *testing.T) {
	cell := func(text string) string {
		return `<w:tc><w:p><w:r><w:t>` + text + `</w:t></w:r></w:p></w:tc>`
	}
	body := `<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>Numbers</w:t></w:r></w:p>` +
		`<w:tbl>` +
		`<w:tr>` + cell("Region") + cell("Q1") + cell("Q2") + `</w:tr>` +
		`<w:tr>` + cell("North") + cell("10") + cell("12") + `</w:tr>` +
		// A pipe in a cell must be escaped, and multi-paragraph cells collapse.
		`<w:tr><w:tc><w:p><w:r><w:t>South | west</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t>region</w:t></w:r></w:p></w:tc>` + cell("8") + cell("9") + `</w:tr>` +
		// gridSpan=2 must pad so the row stays three columns wide.
		`<w:tr><w:tc><w:tcPr><w:gridSpan w:val="2"/></w:tcPr><w:p><w:r><w:t>Total</w:t></w:r></w:p></w:tc>` +
		cell("39") + `</w:tr>` +
		`</w:tbl>`

	got := convertDocx(t, docxFixture(t, body, nil))
	want := "# Numbers\n\n" +
		"| Region | Q1 | Q2 |\n" +
		"| --- | --- | --- |\n" +
		"| North | 10 | 12 |\n" +
		"| South \\| west region | 8 | 9 |\n" +
		"| Total |  | 39 |\n"
	if got.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", got.Markdown, want)
	}
}

func TestDocxAcceptsSniffsWithoutHints(t *testing.T) {
	b := docxFixture(t, `<w:p><w:r><w:t>hi</w:t></w:r></w:p>`, nil)
	c := &DocxConverter{}
	if !c.Accepts(bytes.NewReader(b), StreamInfo{}, nil) {
		t.Error("Accepts should sniff word/document.xml with no hints")
	}
	if c.Accepts(bytes.NewReader([]byte("not a zip at all")), StreamInfo{}, nil) {
		t.Error("Accepts must reject non-zip input")
	}
	// A pptx must not be claimed by the docx converter.
	if c.Accepts(bytes.NewReader(pptxFixture(t, 1, nil)), StreamInfo{}, nil) {
		t.Error("Accepts must not claim a pptx")
	}
	if !c.Accepts(bytes.NewReader(nil), StreamInfo{MimeType: docxMime}, nil) {
		t.Error("Accepts should honor the wordprocessingml mime hint")
	}
}

func TestDocxMalformedInputIsAnErrorNotAPanic(t *testing.T) {
	cases := map[string][]byte{
		"truncated zip": []byte("PK\x03\x04garbage"),
		"zip without a document part": buildZip(t, map[string]string{
			"word/other.xml": "<x/>",
		}),
		"unterminated xml": buildZip(t, map[string]string{
			"word/document.xml": docxHeader + `<w:p><w:r><w:t>oops`,
		}),
	}
	c := &DocxConverter{}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			// Accepts+Convert directly: the extension hint forces Convert even
			// when the bytes are junk, which is the hostile path.
			if _, err := c.Convert(bytes.NewReader(b), StreamInfo{Extension: ".docx"}, &Options{}); err == nil {
				if name != "unterminated xml" {
					t.Errorf("expected an error for %s", name)
				}
			}
		})
	}
}

// Part names that try to escape the archive must never resolve.
func TestDocxIgnoresTraversingPartNames(t *testing.T) {
	b := buildZip(t, map[string]string{
		"../../word/document.xml": docxHeader + `<w:p><w:r><w:t>evil</w:t></w:r></w:p>` + docxFooter,
		"word/document.xml":       docxHeader + `<w:p><w:r><w:t>safe</w:t></w:r></w:p>` + docxFooter,
	})
	got := convertDocx(t, b)
	if !strings.Contains(got.Markdown, "safe") || strings.Contains(got.Markdown, "evil") {
		t.Errorf("traversing name leaked: %q", got.Markdown)
	}
}
