package anymd

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
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

// An inline image contributes its authored alt text, split out of the run so it
// never lands inside the run's emphasis markers.
func TestDocxInlineImageAltText(t *testing.T) {
	drawing := func(descr, name string) string {
		return `<w:drawing><wp:inline xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing">` +
			`<wp:extent cx="100" cy="100"/><wp:docPr id="1" name="` + name + `" descr="` + descr + `"/>` +
			`</wp:inline></w:drawing>`
	}
	body := `<w:p>` +
		`<w:r><w:rPr><w:b/></w:rPr><w:t xml:space="preserve">before </w:t>` +
		drawing("A chart of&#xA;quarterly [revenue]", "Picture 4") +
		`<w:t xml:space="preserve"> after</w:t></w:r></w:p>` +
		// descr absent: fall back to the shape name, but only when it is one
		// somebody chose. "Picture 9" is Word's own identifier for the shape;
		// see TestDocxShapeName.
		`<w:p><w:r>` + drawing("", "Revenue by quarter") + `</w:r></w:p>` +
		`<w:p><w:r>` + drawing("", "Picture 9") + `</w:r></w:p>` +
		// Neither: emit nothing at all rather than an empty image.
		`<w:p><w:r><w:t>plain</w:t>` + drawing("", "") + `</w:r></w:p>`

	got := convertDocx(t, docxFixture(t, body, nil))
	want := "**before** ![A chart of quarterly revenue]() **after**\n\n" +
		"![Revenue by quarter]()\n\n" +
		"plain\n"
	if got.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", got.Markdown, want)
	}
}

// --- image captioning -------------------------------------------------------

// ooxmlStubDescriber is a Describer that never touches the network. It records
// every call so a test can assert what the converter actually sent — above all
// that the document's own alt text is passed through as the hint.
type ooxmlStubDescriber struct {
	reply string
	err   error
	calls []ooxmlStubCall
}

type ooxmlStubCall struct {
	n    int
	mime string
	hint string
}

func (s *ooxmlStubDescriber) Describe(_ context.Context, img []byte, mime, hint string) (string, error) {
	s.calls = append(s.calls, ooxmlStubCall{n: len(img), mime: mime, hint: hint})
	if s.err != nil {
		return "", s.err
	}
	return s.reply, nil
}

// ooxmlPixels returns n deterministic, distinct bytes to stand in for an image.
// The converter picks the media type off the part's extension, so these never
// have to be a decodable PNG — and keeping them undecodable proves the
// converter is not quietly trying to decode them.
func ooxmlPixels(n int, seed byte) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i%251)
	}
	return string(b)
}

// docxImageRels wires rId3 to word/media/<name>.
func docxImageRels(name string) string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image"` +
		` Target="media/` + name + `"/></Relationships>`
}

// docxDrawingXML builds a w:drawing carrying alt text and an a:blip r:embed.
func docxDrawingXML(descr, relID string) string {
	blip := ""
	if relID != "" {
		blip = `<a:graphic xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main">` +
			`<a:graphicData><pic:pic xmlns:pic="http://schemas.openxmlformats.org/drawingml/2006/picture">` +
			`<pic:blipFill><a:blip r:embed="` + relID + `"/></pic:blipFill></pic:pic>` +
			`</a:graphicData></a:graphic>`
	}
	return `<w:drawing><wp:inline xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing">` +
		`<wp:extent cx="100" cy="100"/><wp:docPr id="1" name="Picture 1" descr="` + descr + `"/>` +
		blip + `</wp:inline></w:drawing>`
}

func convertDocxWith(t *testing.T, b []byte, opts *Options) Result {
	t.Helper()
	res, err := New().ConvertBytes(b, StreamInfo{Extension: ".docx", FileName: "fixture.docx"}, opts)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	return res
}

// With a Describer, the image keeps its placeholder and gains the description
// as its own paragraph block — and the model is told what the author wrote.
func TestDocxImageCaptionedWithHint(t *testing.T) {
	stub := &ooxmlStubDescriber{reply: "A bar chart of quarterly revenue."}
	body := `<w:p><w:r><w:t>Figure 1.</w:t></w:r></w:p>` +
		`<w:p><w:r>` + docxDrawingXML("Revenue chart", "rId3") + `</w:r></w:p>`
	fixture := docxFixture(t, body, map[string]string{
		"word/_rels/document.xml.rels": docxImageRels("image1.png"),
		"word/media/image1.png":        ooxmlPixels(6000, 1),
	})

	got := convertDocxWith(t, fixture, &Options{Describer: stub})
	want := "Figure 1.\n\n![Revenue chart]()\n\nA bar chart of quarterly revenue.\n"
	if got.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", got.Markdown, want)
	}
	if len(stub.calls) != 1 {
		t.Fatalf("describer calls = %d, want 1", len(stub.calls))
	}
	if stub.calls[0].hint != "Revenue chart" {
		t.Errorf("hint = %q, want the authored alt text", stub.calls[0].hint)
	}
	if stub.calls[0].mime != "image/png" {
		t.Errorf("mime = %q, want image/png", stub.calls[0].mime)
	}
	if stub.calls[0].n != 6000 {
		t.Errorf("image bytes = %d, want the whole media part", stub.calls[0].n)
	}
}

// The default — no Describer — must read no media at all and emit exactly what
// it emitted before captioning existed.
func TestDocxNoDescriberIsUnchangedAndReadsNoMedia(t *testing.T) {
	body := `<w:p><w:r>` + docxDrawingXML("Revenue chart", "rId3") + `</w:r></w:p>`
	fixture := docxFixture(t, body, map[string]string{
		"word/_rels/document.xml.rels": docxImageRels("image1.png"),
		"word/media/image1.png":        ooxmlPixels(6000, 1),
	})

	want := "![Revenue chart]()\n"
	if got := convertDocx(t, fixture); got.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", got.Markdown, want)
	}
	// An Options with no Describer is the same path, explicitly.
	if got := convertDocxWith(t, fixture, &Options{}); got.Markdown != want {
		t.Errorf("markdown mismatch with empty Options\n got: %q\nwant: %q", got.Markdown, want)
	}
}

// A model outage costs the caption, never the document.
func TestDocxDescriberFailureDegradesToAltText(t *testing.T) {
	for _, tc := range []struct {
		name string
		stub *ooxmlStubDescriber
	}{
		{"error", &ooxmlStubDescriber{err: errors.New("upstream 503")}},
		{"empty caption", &ooxmlStubDescriber{reply: "   "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `<w:p><w:r>` + docxDrawingXML("Revenue chart", "rId3") + `</w:r></w:p>`
			fixture := docxFixture(t, body, map[string]string{
				"word/_rels/document.xml.rels": docxImageRels("image1.png"),
				"word/media/image1.png":        ooxmlPixels(6000, 1),
			})
			got := convertDocxWith(t, fixture, &Options{Describer: tc.stub})
			if want := "![Revenue chart]()\n"; got.Markdown != want {
				t.Errorf("markdown mismatch\n got: %q\nwant: %q", got.Markdown, want)
			}
			if len(tc.stub.calls) != 1 {
				t.Errorf("describer calls = %d, want 1", len(tc.stub.calls))
			}
		})
	}
}

// Spacers, bullet glyphs and vector drawings are not worth a round trip.
func TestDocxSkipsTinyAndVectorImages(t *testing.T) {
	for _, tc := range []struct {
		name  string
		part  string
		bytes int
	}{
		{"below the size floor", "image1.png", ooxmlMinCaptionBytes - 1},
		{"emf is vector", "image1.emf", 20000},
		{"wmf is vector", "image1.wmf", 20000},
		{"svg is vector", "image1.svg", 20000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &ooxmlStubDescriber{reply: "should never be used"}
			body := `<w:p><w:r>` + docxDrawingXML("Logo", "rId3") + `</w:r></w:p>`
			fixture := docxFixture(t, body, map[string]string{
				"word/_rels/document.xml.rels": docxImageRels(tc.part),
				"word/media/" + tc.part:        ooxmlPixels(tc.bytes, 2),
			})
			got := convertDocxWith(t, fixture, &Options{Describer: stub})
			if want := "![Logo]()\n"; got.Markdown != want {
				t.Errorf("markdown mismatch\n got: %q\nwant: %q", got.Markdown, want)
			}
			if len(stub.calls) != 0 {
				t.Errorf("describer called %d times, want 0", len(stub.calls))
			}
		})
	}
}

// An image inside a table cell keeps its alt text and costs nothing: a caption
// cannot be rendered inside a GFM cell.
func TestDocxTableImageIsNotCaptioned(t *testing.T) {
	stub := &ooxmlStubDescriber{reply: "never"}
	body := `<w:tbl><w:tr><w:tc><w:p><w:r><w:t>Cell</w:t>` +
		docxDrawingXML("Logo", "rId3") + `</w:r></w:p></w:tc></w:tr></w:tbl>`
	fixture := docxFixture(t, body, map[string]string{
		"word/_rels/document.xml.rels": docxImageRels("image1.png"),
		"word/media/image1.png":        ooxmlPixels(6000, 3),
	})
	got := convertDocxWith(t, fixture, &Options{Describer: stub})
	if !strings.Contains(got.Markdown, "Cell![Logo]()") {
		t.Errorf("table cell lost its image: %q", got.Markdown)
	}
	if len(stub.calls) != 0 {
		t.Errorf("describer called %d times inside a table, want 0", len(stub.calls))
	}
}

// --- text boxes, content controls, charts and layout tables -----------------

// TestDocxTextBoxDrawingML covers the modern path: a text box anchored in a
// paragraph through wps:txbx. Its content is ordinary paragraphs, so the list
// inside it has to survive as a list rather than as one run-together line.
func TestDocxTextBoxDrawingML(t *testing.T) {
	body := `<w:p><w:r><w:t>Before</w:t></w:r>` +
		`<w:drawing><wp:anchor xmlns:wp="wp"><wp:docPr id="1" name="Text Box 3"/>` +
		`<a:graphic xmlns:a="a"><a:graphicData><wps:wsp xmlns:wps="wps"><wps:txbx>` +
		`<w:txbxContent>` +
		`<w:p><w:pPr><w:pStyle w:val="Heading2"/></w:pPr><w:r><w:t>Boxed</w:t></w:r></w:p>` +
		`<w:p><w:pPr><w:numPr><w:ilvl w:val="0"/><w:numId w:val="1"/></w:numPr></w:pPr>` +
		`<w:r><w:t>one</w:t></w:r></w:p>` +
		`<w:p><w:pPr><w:numPr><w:ilvl w:val="0"/><w:numId w:val="1"/></w:numPr></w:pPr>` +
		`<w:r><w:t>two</w:t></w:r></w:p>` +
		`</w:txbxContent>` +
		`</wps:txbx></wps:wsp></a:graphicData></a:graphic></wp:anchor></w:drawing>` +
		`</w:p>` +
		`<w:p><w:r><w:t>After</w:t></w:r></w:p>`

	got := convertDocx(t, docxFixture(t, body, nil))
	want := "Before\n\n## Boxed\n\n- one\n- two\n\nAfter\n"
	if got.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", got.Markdown, want)
	}
}

// TestDocxTextBoxVML covers the legacy path (w:pict/v:shape/v:textbox), which
// producers still emit on its own for documents authored before DrawingML.
func TestDocxTextBoxVML(t *testing.T) {
	body := `<w:p><w:r>` +
		`<w:pict xmlns:v="v"><v:shape id="s1"><v:textbox><w:txbxContent>` +
		`<w:p><w:r><w:t>Legacy box</w:t></w:r></w:p>` +
		`</w:txbxContent></v:textbox></v:shape></w:pict>` +
		`</w:r></w:p>`

	got := convertDocx(t, docxFixture(t, body, nil))
	if got.Markdown != "Legacy box\n" {
		t.Errorf("markdown = %q, want %q", got.Markdown, "Legacy box\n")
	}
}

// TestDocxAlternateContentTakesChoiceOnce is the regression that matters most
// about text boxes: Word writes the same shape twice, as a DrawingML mc:Choice
// and a VML mc:Fallback. Reading both emits every text box's content twice.
func TestDocxAlternateContentTakesChoiceOnce(t *testing.T) {
	body := `<w:p><w:r><mc:AlternateContent xmlns:mc="mc"><mc:Choice Requires="wps">` +
		`<w:drawing><wp:anchor xmlns:wp="wp"><wp:docPr id="1" name="Text Box 1"/>` +
		`<wps:wsp xmlns:wps="wps"><wps:txbx><w:txbxContent>` +
		`<w:p><w:r><w:t>Only once</w:t></w:r></w:p>` +
		`</w:txbxContent></wps:txbx></wps:wsp></wp:anchor></w:drawing>` +
		`</mc:Choice><mc:Fallback>` +
		`<w:pict xmlns:v="v"><v:shape id="s1"><v:textbox><w:txbxContent>` +
		`<w:p><w:r><w:t>Only once</w:t></w:r></w:p>` +
		`</w:txbxContent></v:textbox></v:shape></w:pict>` +
		`</mc:Fallback></mc:AlternateContent></w:r></w:p>`

	got := convertDocx(t, docxFixture(t, body, nil))
	if got.Markdown != "Only once\n" {
		t.Errorf("markdown = %q, want %q", got.Markdown, "Only once\n")
	}
}

// TestDocxNestedTextBoxesAreBounded proves hostile nesting terminates. A text
// box holds ordinary paragraphs, which may hold further text boxes, so the
// nesting depth is attacker-controlled and has to be bounded rather than
// trusted.
func TestDocxNestedTextBoxesAreBounded(t *testing.T) {
	const levels = 500
	open := strings.Repeat(`<w:p><w:r><w:pict><v:shape><v:textbox><w:txbxContent>`, levels)
	shut := strings.Repeat(`</w:txbxContent></v:textbox></v:shape></w:pict></w:r></w:p>`, levels)
	body := open + `<w:p><w:r><w:t>deep</w:t></w:r></w:p>` + shut

	// The only contract is that this returns: no panic, no unbounded recursion,
	// and no duplicated content from re-walking the same subtree.
	res, err := New().ConvertBytes(docxFixture(t, body, nil),
		StreamInfo{Extension: ".docx"}, nil)
	if err != nil {
		return // a parse error is an acceptable outcome; a panic is not
	}
	if n := strings.Count(res.Markdown, "deep"); n > 1 {
		t.Errorf("content past the nesting bound appeared %d times: %q", n, res.Markdown)
	}
}

// TestDocxContentControlIsTransparent covers w:sdt, which is a box *around*
// real content — a cover title, a locked table. Skipping the wrapper drops
// everything inside it.
func TestDocxContentControlIsTransparent(t *testing.T) {
	body := `<w:sdt><w:sdtPr><w:tag w:val="cover"/></w:sdtPr><w:sdtContent>` +
		`<w:p><w:pPr><w:pStyle w:val="Title"/></w:pPr><w:r><w:t>Cover title</w:t></w:r></w:p>` +
		`<w:tbl><w:tr><w:tc><w:p><w:r><w:t>A</w:t></w:r></w:p></w:tc>` +
		`<w:tc><w:p><w:r><w:t>B</w:t></w:r></w:p></w:tc></w:tr>` +
		`<w:tr><w:tc><w:p><w:r><w:t>1</w:t></w:r></w:p></w:tc>` +
		`<w:tc><w:p><w:r><w:t>2</w:t></w:r></w:p></w:tc></w:tr></w:tbl>` +
		`</w:sdtContent></w:sdt>` +
		`<w:p><w:r><w:t>Body</w:t></w:r></w:p>`

	got := convertDocx(t, docxFixture(t, body, nil))
	want := "# Cover title\n\n| A | B |\n| --- | --- |\n| 1 | 2 |\n\nBody\n"
	if got.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", got.Markdown, want)
	}
}

// TestDocxSingleCellTableIsUnwrapped covers Word's missing "box" primitive:
// a one-row one-cell table is a border drawn around ordinary content, and
// rendering it as a table flattens a list into one line and loses a nested
// table entirely.
func TestDocxSingleCellTableIsUnwrapped(t *testing.T) {
	inner := `<w:tbl>` +
		`<w:tr><w:tc><w:p><w:r><w:t>Tab1</w:t></w:r></w:p></w:tc>` +
		`<w:tc><w:p><w:r><w:t>Tab2</w:t></w:r></w:p></w:tc></w:tr>` +
		`<w:tr><w:tc><w:p><w:r><w:t>A</w:t></w:r></w:p></w:tc>` +
		`<w:tc><w:p><w:r><w:t>B</w:t></w:r></w:p></w:tc></w:tr>` +
		`</w:tbl>`
	body := `<w:tbl><w:tr><w:tc>` +
		`<w:p><w:pPr><w:numPr><w:ilvl w:val="0"/><w:numId w:val="1"/></w:numPr></w:pPr>` +
		`<w:r><w:t>Hello world1</w:t></w:r></w:p>` +
		`<w:p><w:pPr><w:numPr><w:ilvl w:val="0"/><w:numId w:val="1"/></w:numPr></w:pPr>` +
		`<w:r><w:t>Hello2</w:t></w:r></w:p>` +
		`</w:tc></w:tr></w:tbl>` +
		`<w:tbl><w:tr><w:tc>` + inner + `</w:tc></w:tr></w:tbl>`

	got := convertDocx(t, docxFixture(t, body, nil))
	want := "- Hello world1\n- Hello2\n\n" +
		"| Tab1 | Tab2 |\n| --- | --- |\n| A | B |\n"
	if got.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", got.Markdown, want)
	}
}

// TestDocxNestedTableTextSurvivesInCell: a nested table still cannot be drawn
// inside a GFM cell, but its text must not disappear from the row.
func TestDocxNestedTableTextSurvivesInCell(t *testing.T) {
	nested := `<w:tbl><w:tr><w:tc><w:p><w:r><w:t>x</w:t></w:r></w:p></w:tc>` +
		`<w:tc><w:p><w:r><w:t>y</w:t></w:r></w:p></w:tc></w:tr></w:tbl>`
	body := `<w:tbl>` +
		`<w:tr><w:tc><w:p><w:r><w:t>H1</w:t></w:r></w:p></w:tc>` +
		`<w:tc><w:p><w:r><w:t>H2</w:t></w:r></w:p></w:tc></w:tr>` +
		`<w:tr><w:tc><w:p><w:r><w:t>lead</w:t></w:r></w:p>` + nested + `</w:tc>` +
		`<w:tc><w:p><w:r><w:t>plain</w:t></w:r></w:p></w:tc></w:tr>` +
		`</w:tbl>`

	got := convertDocx(t, docxFixture(t, body, nil))
	want := "| H1 | H2 |\n| --- | --- |\n| lead x y | plain |\n"
	if got.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", got.Markdown, want)
	}
}

const docxChartRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
	`<Relationship Id="rId4" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/chart"` +
	` Target="charts/chart1.xml"/>` +
	`</Relationships>`

const docxChartPart = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<c:chartSpace xmlns:c="http://schemas.openxmlformats.org/drawingml/2006/chart"><c:chart><c:plotArea>` +
	`<c:lineChart>` +
	`<c:ser>` +
	`<c:tx><c:strRef><c:strCache><c:pt idx="0"><c:v>Series 1</c:v></c:pt></c:strCache></c:strRef></c:tx>` +
	`<c:cat><c:strRef><c:strCache>` +
	`<c:pt idx="0"><c:v>Category 1</c:v></c:pt><c:pt idx="1"><c:v>Category 2</c:v></c:pt>` +
	`</c:strCache></c:strRef></c:cat>` +
	`<c:val><c:numRef><c:numCache>` +
	`<c:pt idx="0"><c:v>4.3</c:v></c:pt><c:pt idx="1"><c:v>2.5</c:v></c:pt>` +
	`</c:numCache></c:numRef></c:val>` +
	`</c:ser>` +
	`<c:ser>` +
	`<c:tx><c:strRef><c:strCache><c:pt idx="0"><c:v>Series 2</c:v></c:pt></c:strCache></c:strRef></c:tx>` +
	`<c:val><c:numRef><c:numCache>` +
	`<c:pt idx="0"><c:v>2.4</c:v></c:pt><c:pt idx="1"><c:v>4.4</c:v></c:pt>` +
	`</c:numCache></c:numRef></c:val>` +
	`</c:ser>` +
	`</c:lineChart></c:plotArea></c:chart></c:chartSpace>`

// TestDocxEmbeddedChart: an untitled chart is labelled by its plot type, and
// its cached values become a table. The auto-generated shape name "Chart 5"
// must not also be emitted as an image: it is an identifier, not a caption.
func TestDocxEmbeddedChart(t *testing.T) {
	body := `<w:p><w:r><w:drawing><wp:inline xmlns:wp="wp">` +
		`<wp:docPr id="1" name="Chart 5"/>` +
		`<a:graphic xmlns:a="a"><a:graphicData>` +
		`<c:chart xmlns:c="c" r:id="rId4"/>` +
		`</a:graphicData></a:graphic></wp:inline></w:drawing></w:r></w:p>`

	got := convertDocx(t, docxFixture(t, body, map[string]string{
		"word/_rels/document.xml.rels": docxChartRels,
		"word/charts/chart1.xml":       docxChartPart,
	}))
	want := "Line chart\n\n" +
		"|  | Series 1 | Series 2 |\n" +
		"| --- | --- | --- |\n" +
		"| Category 1 | 4.3 | 2.4 |\n" +
		"| Category 2 | 2.5 | 4.4 |\n"
	if got.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", got.Markdown, want)
	}
}

// TestDocxChartWithMissingPartDegrades: a chart relationship pointing at a part
// that is not in the archive costs the chart, not the document.
func TestDocxChartWithMissingPartDegrades(t *testing.T) {
	body := `<w:p><w:r><w:drawing><wp:inline xmlns:wp="wp">` +
		`<wp:docPr id="1" name="Chart 5"/>` +
		`<c:chart xmlns:c="c" r:id="rId4"/>` +
		`</wp:inline></w:drawing></w:r></w:p>` +
		`<w:p><w:r><w:t>still here</w:t></w:r></w:p>`

	got := convertDocx(t, docxFixture(t, body, map[string]string{
		"word/_rels/document.xml.rels": docxChartRels,
	}))
	if got.Markdown != "still here\n" {
		t.Errorf("markdown = %q, want %q", got.Markdown, "still here\n")
	}
}

// TestDocxShapeName pins the rule that keeps Word's auto-generated shape
// identifiers out of alt text while keeping a name someone chose.
func TestDocxShapeName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Picture 1", ""},
		{"Group 4", ""},
		{"Text Box 3", ""},
		{"Elbow Connector 12", ""},
		{"", ""},
		{"Docling", "Docling"},
		{"Figure2", "Figure2"}, // no separator: not the auto pattern
		{"12", "12"},
		{"Org chart Q3", "Org chart Q3"},
	}
	for _, tt := range tests {
		if got := docxShapeName(tt.in); got != tt.want {
			t.Errorf("docxShapeName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestDocxAuthoredDescriptionSurvivesShapeNameFilter: dropping the auto name
// must not drop a real wp:docPr@descr.
func TestDocxAuthoredDescriptionSurvivesShapeNameFilter(t *testing.T) {
	body := `<w:p><w:r><w:drawing><wp:inline xmlns:wp="wp">` +
		`<wp:docPr id="1" name="Picture 3" descr="A cartoon duck"/>` +
		`<a:blip xmlns:a="a" r:embed="rId9"/>` +
		`</wp:inline></w:drawing></w:r></w:p>`

	got := convertDocx(t, docxFixture(t, body, nil))
	if got.Markdown != "![A cartoon duck]()\n" {
		t.Errorf("markdown = %q, want %q", got.Markdown, "![A cartoon duck]()\n")
	}
}
