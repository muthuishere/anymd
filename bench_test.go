package anymd

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Benchmarks exist so a throughput claim can be checked rather than believed —
// by us, and by anyone comparing anymd against markitdown on the same document.
// Every benchmark reports allocations and sets SetBytes to the input size, so
// `go test -bench .` prints MB/s directly and a regression shows up as a number
// instead of a feeling.
//
// Each one prefers a real file from markitdown's corpus (same input as the
// other implementation, so the comparison is honest) and falls back to an
// in-memory fixture built here, so `go test -bench .` still measures something
// in a bare checkout. Fixture construction always happens before b.ResetTimer.

// bmInput returns the named corpus file, or nil when the corpus is not on this
// machine. It shares ANYMD_CORPUS with the fuzz targets.
func bmInput(name string) []byte {
	b, err := os.ReadFile(filepath.Join(fzCorpusDir(), name))
	if err != nil {
		return nil
	}
	return b
}

// bmRun is the benchmark body: convert the same bytes repeatedly through the
// default registry, failing on the first error so a benchmark can never report
// the speed of an early return.
func bmRun(b *testing.B, ext string, data []byte) {
	b.Helper()
	e := New()
	info := StreamInfo{Extension: ext, FileName: "bench" + ext}
	opts := &Options{}
	// Prove it converts once before timing anything: a benchmark of a failing
	// conversion measures the error path, not the parser.
	if _, err := e.ConvertBytes(data, info, opts); err != nil {
		b.Fatalf("convert %s: %v", ext, err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.ConvertBytes(data, info, opts); err != nil {
			b.Fatalf("convert %s: %v", ext, err)
		}
	}
}

// bmZip writes a real archive in memory, so the fallback fixtures are genuine
// container files rather than something the converters treat specially.
func bmZip(b *testing.B, parts [][2]string) []byte {
	b.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range parts {
		w, err := zw.Create(p[0])
		if err != nil {
			b.Fatalf("zip create %s: %v", p[0], err)
		}
		if _, err := w.Write([]byte(p[1])); err != nil {
			b.Fatalf("zip write %s: %v", p[0], err)
		}
	}
	if err := zw.Close(); err != nil {
		b.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

const bmContentTypes = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
	`<Default Extension="xml" ContentType="application/xml"/></Types>`

// BenchmarkConvertDocx measures Word extraction: unzip, then walk the document
// part's paragraph/run tree.
func BenchmarkConvertDocx(b *testing.B) {
	data := bmInput("test.docx")
	if data == nil {
		var body strings.Builder
		for i := 0; i < 400; i++ {
			body.WriteString(`<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>Section</w:t></w:r></w:p>`)
			body.WriteString(`<w:p><w:r><w:t>The quick brown fox jumps over the lazy dog.</w:t></w:r></w:p>`)
		}
		doc := `<?xml version="1.0" encoding="UTF-8"?>` +
			`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
			`<w:body>` + body.String() + `</w:body></w:document>`
		data = bmZip(b, [][2]string{
			{"[Content_Types].xml", bmContentTypes},
			{"word/document.xml", doc},
		})
	}
	bmRun(b, ".docx", data)
}

// BenchmarkConvertXlsx measures spreadsheet extraction, which is dominated by
// cell-value resolution and shared-string lookups rather than by unzipping.
func BenchmarkConvertXlsx(b *testing.B) {
	data := bmInput("test.xlsx")
	if data == nil {
		b.Skip("no corpus xlsx and no pure-XML fallback fixture: excelize rejects a hand-rolled minimal workbook")
	}
	bmRun(b, ".xlsx", data)
}

// BenchmarkConvertPdf measures the text layer walk: xref resolution plus
// content-stream decoding, the slowest converter in the set.
func BenchmarkConvertPdf(b *testing.B) {
	data := bmInput("test.pdf")
	if data == nil {
		b.Skip("no corpus pdf: a hand-built PDF would benchmark our fixture, not a real document")
	}
	bmRun(b, ".pdf", data)
}

// BenchmarkConvertHTML measures the HTML→Markdown path, the one converter most
// users will hit in bulk.
func BenchmarkConvertHTML(b *testing.B) {
	data := bmInput("test_blog.html")
	if data == nil {
		var sb strings.Builder
		sb.WriteString("<!doctype html><html><head><title>Bench</title></head><body>")
		for i := 0; i < 300; i++ {
			sb.WriteString("<h2>Heading</h2><p>Some <b>bold</b> and a <a href=\"https://example.com\">link</a>.</p>")
			sb.WriteString("<ul><li>one</li><li>two</li></ul>")
			sb.WriteString("<table><tr><th>a</th><th>b</th></tr><tr><td>1</td><td>2</td></tr></table>")
		}
		sb.WriteString("</body></html>")
		data = []byte(sb.String())
	}
	bmRun(b, ".html", data)
}

// BenchmarkConvertZip measures the container path end to end: every member is
// dispatched and converted, so this is the closest thing to a whole-registry
// benchmark.
func BenchmarkConvertZip(b *testing.B) {
	data := bmInput("test_files.zip")
	if data == nil {
		parts := make([][2]string, 0, 40)
		for i := 0; i < 10; i++ {
			parts = append(parts,
				[2]string{"notes" + string(rune('a'+i)) + ".txt", strings.Repeat("plain text line\n", 200)},
				[2]string{"data" + string(rune('a'+i)) + ".csv", "a,b,c\n" + strings.Repeat("1,2,3\n", 200)},
				[2]string{"page" + string(rune('a'+i)) + ".html", "<h1>t</h1>" + strings.Repeat("<p>x</p>", 200)},
				[2]string{"obj" + string(rune('a'+i)) + ".json", `{"k":[1,2,3]}`},
			)
		}
		data = bmZip(b, parts)
	}
	bmRun(b, ".zip", data)
}
