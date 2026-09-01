package anymd

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

const pptxSldHeader = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"` +
	` xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"` +
	` xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main">` +
	`<p:cSld><p:spTree>` +
	`<p:nvGrpSpPr><p:cNvPr id="1" name="Shape Tree"/></p:nvGrpSpPr>`

const pptxSldFooter = `</p:spTree></p:cSld></p:sld>`

// pptxShapeXML builds one p:sp; phType may be "" for a plain body shape.
func pptxShapeXML(phType string, paras ...string) string {
	ph := ""
	if phType != "" {
		ph = `<p:ph type="` + phType + `"/>`
	}
	var body strings.Builder
	for _, p := range paras {
		body.WriteString(`<a:p><a:r><a:rPr lang="en-US"/><a:t>` + p + `</a:t></a:r></a:p>`)
	}
	return `<p:sp><p:nvSpPr><p:cNvPr id="2" name="Placeholder"/><p:cNvSpPr/><p:nvPr>` + ph +
		`</p:nvPr></p:nvSpPr><p:spPr/><p:txBody><a:bodyPr/>` + body.String() + `</p:txBody></p:sp>`
}

// pptxTableXML builds a p:graphicFrame wrapping an a:tbl from rows of cells.
func pptxTableXML(rows ...[]string) string {
	var b strings.Builder
	b.WriteString(`<p:graphicFrame><p:nvGraphicFramePr><p:cNvPr id="9" name="Table"/></p:nvGraphicFramePr>` +
		`<a:graphic><a:graphicData><a:tbl><a:tblPr/><a:tblGrid/>`)
	for _, r := range rows {
		b.WriteString(`<a:tr h="370840">`)
		for _, c := range r {
			b.WriteString(`<a:tc><a:txBody><a:bodyPr/><a:p><a:r><a:t>` + c + `</a:t></a:r></a:p></a:txBody><a:tcPr/></a:tc>`)
		}
		b.WriteString(`</a:tr>`)
	}
	b.WriteString(`</a:tbl></a:graphicData></a:graphic></p:graphicFrame>`)
	return b.String()
}

func pptxNotesXML(paras ...string) string {
	var body strings.Builder
	for _, p := range paras {
		body.WriteString(`<a:p><a:r><a:t>` + p + `</a:t></a:r></a:p>`)
	}
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<p:notes xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"` +
		` xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main">` +
		`<p:cSld><p:spTree>` +
		// The slide-image placeholder carries junk text that must NOT be quoted.
		`<p:sp><p:nvSpPr><p:cNvPr id="3" name="Slide Image"/><p:nvPr><p:ph type="sldImg"/></p:nvPr></p:nvSpPr>` +
		`<p:txBody><a:p><a:r><a:t>slide image placeholder</a:t></a:r></a:p></p:txBody></p:sp>` +
		`<p:sp><p:nvSpPr><p:cNvPr id="4" name="Notes"/><p:nvPr><p:ph type="body" idx="1"/></p:nvPr></p:nvSpPr>` +
		`<p:txBody><a:bodyPr/>` + body.String() + `</p:txBody></p:sp>` +
		`</p:spTree></p:cSld></p:notes>`
}

func pptxSlideRels(notesTarget string) string {
	rel := ""
	if notesTarget != "" {
		rel = `<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/notesSlide"` +
			` Target="` + notesTarget + `"/>`
	}
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` + rel + `</Relationships>`
}

// pptxFixture builds a deck of n slides, each holding one title placeholder.
// extra overlays or (with an empty value) deletes parts.
func pptxFixture(t *testing.T, n int, extra map[string]string) []byte {
	t.Helper()
	parts := map[string]string{
		"[Content_Types].xml":  `<?xml version="1.0"?><Types/>`,
		"ppt/presentation.xml": `<?xml version="1.0"?><p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"/>`,
		"_rels/.rels":          `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"/>`,
	}
	for i := 1; i <= n; i++ {
		parts[fmt.Sprintf("ppt/slides/slide%d.xml", i)] =
			pptxSldHeader + pptxShapeXML("title", "Title "+strconv.Itoa(i)) + pptxSldFooter
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

func convertPptx(t *testing.T, b []byte) Result {
	t.Helper()
	res, err := New().ConvertBytes(b, StreamInfo{Extension: ".pptx", FileName: "fixture.pptx"}, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	return res
}

// Slide parts must sort numerically: lexical order puts slide10 before slide2.
func TestPptxSlideOrderingPastNine(t *testing.T) {
	got := convertPptx(t, pptxFixture(t, 11, nil))
	var want strings.Builder
	for i := 1; i <= 11; i++ {
		if i > 1 {
			want.WriteString("\n")
		}
		fmt.Fprintf(&want, "## Slide %d\n\n### Title %d\n", i, i)
	}
	if got.Markdown != want.String() {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", got.Markdown, want.String())
	}
}

func TestPptxSlideParts(t *testing.T) {
	in := []string{
		"ppt/slides/slide10.xml",
		"ppt/slides/slide2.xml",
		"ppt/slides/slide1.xml",
		"ppt/slides/_rels/slide1.xml.rels",
		"ppt/slides/notes/slide3.xml",
		"ppt/slides/slideLayout1.xml",
		"ppt/notesSlides/notesSlide1.xml",
	}
	got := pptxSlideParts(in)
	want := []string{"ppt/slides/slide1.xml", "ppt/slides/slide2.xml", "ppt/slides/slide10.xml"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestPptxTitleBodyTableAndNotes(t *testing.T) {
	slide := pptxSldHeader +
		pptxShapeXML("ctrTitle", "Quarterly Review") +
		pptxShapeXML("", "Revenue is up", "Costs are flat") +
		pptxTableXML(
			[]string{"Region", "Q1", "Q2"},
			[]string{"North", "10", "12"},
			[]string{"South", "8", "9"},
		) +
		pptxSldFooter

	b := pptxFixture(t, 1, map[string]string{
		"ppt/slides/slide1.xml":            slide,
		"ppt/slides/_rels/slide1.xml.rels": pptxSlideRels("../notesSlides/notesSlide1.xml"),
		"ppt/notesSlides/notesSlide1.xml":  pptxNotesXML("Open with the revenue chart.", "Then hand over to Sam."),
	})

	got := convertPptx(t, b)
	want := "## Slide 1\n\n" +
		"### Quarterly Review\n\n" +
		"Revenue is up\n\n" +
		"Costs are flat\n\n" +
		"| Region | Q1 | Q2 |\n" +
		"| --- | --- | --- |\n" +
		"| North | 10 | 12 |\n" +
		"| South | 8 | 9 |\n\n" +
		"> **Notes:** Open with the revenue chart.\n" +
		">\n" +
		"> Then hand over to Sam.\n"
	if got.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", got.Markdown, want)
	}
}

// With no rels part the converter still finds notesSlideN.xml by convention.
func TestPptxNotesFoundByConventionAndSkippedWhenEmpty(t *testing.T) {
	b := pptxFixture(t, 2, map[string]string{
		"ppt/notesSlides/notesSlide2.xml": pptxNotesXML("Only slide two has notes."),
	})
	got := convertPptx(t, b)
	want := "## Slide 1\n\n### Title 1\n\n" +
		"## Slide 2\n\n### Title 2\n\n" +
		"> **Notes:** Only slide two has notes.\n"
	if got.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", got.Markdown, want)
	}
}

func TestPptxAcceptsSniffsWithoutHints(t *testing.T) {
	c := &PptxConverter{}
	if !c.Accepts(bytes.NewReader(pptxFixture(t, 1, nil)), StreamInfo{}, nil) {
		t.Error("Accepts should sniff ppt/presentation.xml with no hints")
	}
	if c.Accepts(bytes.NewReader(docxFixture(t, `<w:p/>`, nil)), StreamInfo{}, nil) {
		t.Error("Accepts must not claim a docx")
	}
	if !c.Accepts(bytes.NewReader(nil), StreamInfo{MimeType: pptxMime + "; charset=utf-8"}, nil) {
		t.Error("Accepts should honor the presentationml mime hint")
	}
}

func TestPptxEmptyDeckIsEmptyNotAnError(t *testing.T) {
	got := convertPptx(t, pptxFixture(t, 0, nil))
	if got.Markdown != "" {
		t.Errorf("got %q, want empty", got.Markdown)
	}
}

func TestPptxMalformedIsAnErrorNotAPanic(t *testing.T) {
	c := &PptxConverter{}
	if _, err := c.Convert(bytes.NewReader([]byte("PK\x03\x04nope")), StreamInfo{Extension: ".pptx"}, &Options{}); err == nil {
		t.Error("expected an error for a truncated zip")
	}
}
