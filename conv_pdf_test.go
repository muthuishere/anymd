package anymd

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// buildPDF writes a real, structurally valid single- or multi-page PDF: a
// catalog, a page tree, one Type1 font with explicit glyph widths (so the text
// layout code has real advances to work with) and one content stream per page.
// Building the fixture in Go keeps the repo free of committed binaries and
// makes every byte of the input auditable from the test.
func buildPDF(contents []string, extraTrailer string) []byte {
	n := len(contents)
	widths := strings.TrimSpace(strings.Repeat("500 ", 95)) // codes 32..126

	var kids strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&kids, "%d 0 R ", 4+2*i)
	}

	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [ %s] /Count %d >>", kids.String(), n),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding" +
			" /FirstChar 32 /LastChar 126 /Widths [ " + widths + " ] >>",
	}
	for i, c := range contents {
		objs = append(objs, fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [ 0 0 612 792 ]"+
				" /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", 5+2*i))
		objs = append(objs, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(c), c))
	}

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs)+1)
	for i, o := range objs {
		offsets[i+1] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(objs)+1)
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i <= len(objs); i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R %s>>\nstartxref\n%d\n%%%%EOF\n",
		len(objs)+1, extraTrailer, xref)
	return buf.Bytes()
}

// textPage returns a content stream that draws each item at an absolute
// position. Absolute Tm matrices (not relative Td) are what let the test pin
// down the exact geometry the layout code sees.
func textPage(items ...[3]string) string {
	var b strings.Builder
	b.WriteString("BT\n/F1 12 Tf\n")
	for _, it := range items {
		fmt.Fprintf(&b, "1 0 0 1 %s %s Tm\n(%s) Tj\n", it[0], it[1], it[2])
	}
	b.WriteString("ET\n")
	return b.String()
}

func TestPDFConverterAccepts(t *testing.T) {
	c := &PDFConverter{}
	pdfBytes := buildPDF([]string{textPage([3]string{"72", "720", "Hi"})}, "")

	cases := []struct {
		name string
		body []byte
		info StreamInfo
		want bool
	}{
		{"magic alone", pdfBytes, StreamInfo{}, true},
		{"mime hint", []byte("not a pdf"), StreamInfo{MimeType: "application/pdf"}, true},
		{"extension hint", []byte("not a pdf"), StreamInfo{Extension: ".pdf"}, true},
		{"plain text", []byte("hello"), StreamInfo{Extension: ".txt"}, false},
		{"empty", nil, StreamInfo{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.Accepts(bytes.NewReader(tc.body), tc.info, &Options{}); got != tc.want {
				t.Fatalf("Accepts = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPDFConvertPagesAndSpacing(t *testing.T) {
	// Page 1 exercises all three layout decisions at once:
	//   - two text objects on the same baseline with a pen jump between them
	//     (must become a space, which GetPlainText would have lost),
	//   - a nearby baseline below (a line break),
	//   - a distant baseline below (a paragraph break).
	page1 := textPage(
		[3]string{"72", "720", "Hello"},
		[3]string{"120", "720", "World"},
		[3]string{"72", "706", "Second line"},
		[3]string{"72", "640", "New paragraph"},
	)
	page2 := textPage([3]string{"72", "720", "Page two."})

	res, err := (&PDFConverter{}).Convert(
		bytes.NewReader(buildPDF([]string{page1, page2}, "")),
		StreamInfo{Extension: ".pdf"}, &Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	want := "Hello World\nSecond line\n\nNew paragraph\n\n---\n\nPage two.\n"
	if res.Markdown != want {
		t.Fatalf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
}

func TestPDFSkipsEmptyPages(t *testing.T) {
	// A blank page between two text pages must not produce a stray rule or an
	// empty block: exactly one `---` separates the two pages that have text.
	blank := "0 0 1 rg\n10 10 100 100 re\nf\n"
	res, err := (&PDFConverter{}).Convert(
		bytes.NewReader(buildPDF([]string{
			textPage([3]string{"72", "720", "One"}),
			blank,
			textPage([3]string{"72", "720", "Two"}),
		}, "")),
		StreamInfo{Extension: ".pdf"}, &Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	want := "One\n\n---\n\nTwo\n"
	if res.Markdown != want {
		t.Fatalf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
}

func TestPDFNoTextLayer(t *testing.T) {
	// A pure scan: geometry only, no text-showing operator anywhere.
	scan := "q\n612 0 0 792 0 0 cm\n0 0 0 rg\n0 0 1 1 re\nf\nQ\n"
	res, err := (&PDFConverter{}).Convert(
		bytes.NewReader(buildPDF([]string{scan}, "")),
		StreamInfo{Extension: ".pdf"}, &Options{})
	if !errors.Is(err, ErrNoTextLayer) {
		t.Fatalf("err = %v, want ErrNoTextLayer", err)
	}
	if res.Markdown != "" {
		t.Fatalf("markdown = %q, want empty alongside the sentinel", res.Markdown)
	}
}

func TestPDFEncrypted(t *testing.T) {
	trailer := "/ID [ (0123456789abcdef) (0123456789abcdef) ] " +
		"/Encrypt << /Filter /Standard /V 1 /R 2 /P -1 " +
		"/O (01234567890123456789012345678901) /U (01234567890123456789012345678901) >> "
	_, err := (&PDFConverter{}).Convert(
		bytes.NewReader(buildPDF([]string{textPage([3]string{"72", "720", "Secret"})}, trailer)),
		StreamInfo{Extension: ".pdf"}, &Options{})
	if !errors.Is(err, ErrEncryptedPDF) {
		t.Fatalf("err = %v, want ErrEncryptedPDF", err)
	}
}

func TestPDFMalformedIsAnErrorNotAPanic(t *testing.T) {
	good := buildPDF([]string{textPage([3]string{"72", "720", "Hi"})}, "")
	cases := map[string][]byte{
		"truncated":   good[:len(good)/2],
		"header only": []byte("%PDF-1.4\n"),
		"garbage":     append([]byte("%PDF-1.4\n"), bytes.Repeat([]byte{0xFF, 0x00, 0x7F}, 400)...),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := (&PDFConverter{}).Convert(bytes.NewReader(body), StreamInfo{Extension: ".pdf"}, &Options{}); err == nil {
				t.Fatal("want an error, got success")
			}
		})
	}
}

func TestPDFThroughEngine(t *testing.T) {
	// The registry must route a bare %PDF- stream with no hints at all here.
	res, err := New().ConvertBytes(
		buildPDF([]string{textPage([3]string{"72", "720", "Routed"})}, ""),
		StreamInfo{}, nil)
	if err != nil {
		t.Fatalf("ConvertBytes: %v", err)
	}
	if res.Markdown != "Routed\n" {
		t.Fatalf("markdown = %q", res.Markdown)
	}
}
