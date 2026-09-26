package anymd

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/ascii85"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
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

	return assemblePDF(objs, extraTrailer)
}

// assemblePDF writes numbered objects, the xref table and the trailer. It is
// split out of buildPDF so the scanned-PDF fixtures can supply their own object
// list (pages plus image XObjects) without duplicating the file plumbing.
func assemblePDF(objs []string, extraTrailer string) []byte {
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

	// The two baselines are one wrapped paragraph, so they are joined: a
	// single newline inside a Markdown paragraph is not a line break, and
	// keeping one would leave every wrapped word pair unjoinable.
	want := "Hello World Second line\n\nNew paragraph\n\n---\n\nPage two.\n"
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

// --- Scanned pages read by a Describer -------------------------------------

// scanPage describes one page of a fixture: an optional content stream and an
// optional embedded image XObject, which is how a real scan is built.
type scanPage struct {
	content string // page content stream (may be empty)
	img     []byte // encoded image stream bytes, or nil for no image
	w, h    int
	filter  string // /DCTDecode, /FlateDecode, …
	cs      string // /DeviceRGB, /DeviceGray, …
	bpc     int
}

// buildScanPDF assembles a PDF whose pages carry real image XObjects, so the
// extraction path is exercised against bytes laid out exactly as a scanner's
// output would be — including binary payloads sitting between PDF objects.
func buildScanPDF(pages []scanPage) []byte {
	// Objects 1-3 are the catalog, the page tree and the font; each page then
	// takes a page object, a contents object and (optionally) an image object.
	num := 4
	type nums struct{ page, content, img int }
	ids := make([]nums, len(pages))
	for i, p := range pages {
		ids[i] = nums{page: num, content: num + 1}
		num += 2
		if p.img != nil {
			ids[i].img = num
			num++
		}
	}

	var kids strings.Builder
	for _, id := range ids {
		fmt.Fprintf(&kids, "%d 0 R ", id.page)
	}
	widths := strings.TrimSpace(strings.Repeat("500 ", 95))
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [ %s] /Count %d >>", kids.String(), len(pages)),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding" +
			" /FirstChar 32 /LastChar 126 /Widths [ " + widths + " ] >>",
	}
	for i, p := range pages {
		xobj := ""
		if p.img != nil {
			xobj = fmt.Sprintf(" /XObject << /Im0 %d 0 R >>", ids[i].img)
		}
		objs = append(objs, fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [ 0 0 612 792 ]"+
				" /Resources << /Font << /F1 3 0 R >>%s >> /Contents %d 0 R >>",
			xobj, ids[i].content))
		objs = append(objs, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream",
			len(p.content), p.content))
		if p.img != nil {
			objs = append(objs, fmt.Sprintf(
				"<< /Type /XObject /Subtype /Image /Width %d /Height %d"+
					" /ColorSpace %s /BitsPerComponent %d /Filter %s /Length %d >>\nstream\n%s\nendstream",
				p.w, p.h, p.cs, p.bpc, p.filter, len(p.img), p.img))
		}
	}
	return assemblePDF(objs, "")
}

// testJPEG encodes a deterministic, noisy image. Noise (not a flat fill) is
// what keeps the JPEG comfortably over minPDFImageBytes, the same way a real
// page scan is; seed varies the bytes so two pages can hold *different* images.
func testJPEG(t *testing.T, w, h, seed int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	r := rand.New(rand.NewSource(int64(seed)))
	for i := range img.Pix {
		img.Pix[i] = byte(r.Intn(256))
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	if buf.Len() < minPDFImageBytes {
		t.Fatalf("fixture jpeg is %d bytes, under the %d threshold", buf.Len(), minPDFImageBytes)
	}
	return buf.Bytes()
}

// scanJPEGPage is a page that is nothing but one large DCTDecode image — the
// shape of every page in a scanned document.
func scanJPEGPage(t *testing.T, seed int) scanPage {
	t.Helper()
	return scanPage{
		content: "q\n612 0 0 792 0 0 cm\n/Im0 Do\nQ\n",
		img:     testJPEG(t, 200, 260, seed),
		w:       200, h: 260, filter: "/DCTDecode", cs: "/DeviceRGB", bpc: 8,
	}
}

// countingDescriber records every call so the tests can assert on the number of
// network requests a document would have cost, which is the whole point of the
// dedup and page-cap guards.
type countingDescriber struct {
	calls    int
	mimes    []string
	hints    []string
	sizes    []int
	reply    string
	failWith error
}

func (d *countingDescriber) describer() Describer {
	return DescriberFunc(func(_ context.Context, img []byte, mime, hint string) (string, error) {
		d.calls++
		d.mimes = append(d.mimes, mime)
		d.hints = append(d.hints, hint)
		d.sizes = append(d.sizes, len(img))
		if d.failWith != nil {
			return "", d.failWith
		}
		return d.reply, nil
	})
}

func TestPDFTextPDFUnchangedByDescriber(t *testing.T) {
	// A PDF that has a text layer must produce byte-identical output whether or
	// not a Describer is configured, and must not cost a single model call.
	body := buildPDF([]string{
		textPage([3]string{"72", "720", "Alpha"}),
		textPage([3]string{"72", "720", "Beta"}),
	}, "")

	plain, err := (&PDFConverter{}).Convert(bytes.NewReader(body), StreamInfo{Extension: ".pdf"}, &Options{})
	if err != nil {
		t.Fatalf("Convert without describer: %v", err)
	}

	d := &countingDescriber{reply: "a picture"}
	withLLM, err := (&PDFConverter{}).Convert(bytes.NewReader(body),
		StreamInfo{Extension: ".pdf"}, &Options{Describer: d.describer()})
	if err != nil {
		t.Fatalf("Convert with describer: %v", err)
	}
	if plain.Markdown != "Alpha\n\n---\n\nBeta\n" {
		t.Fatalf("baseline markdown = %q", plain.Markdown)
	}
	if withLLM.Markdown != plain.Markdown {
		t.Fatalf("describer changed a text PDF\n got: %q\nwant: %q", withLLM.Markdown, plain.Markdown)
	}
	if d.calls != 0 {
		t.Fatalf("describer called %d times on a PDF with a text layer", d.calls)
	}
}

func TestPDFScannedPageCaptioned(t *testing.T) {
	body := buildScanPDF([]scanPage{scanJPEGPage(t, 1)})

	// Without a Describer the honest sentinel is unchanged.
	if _, err := (&PDFConverter{}).Convert(bytes.NewReader(body),
		StreamInfo{Extension: ".pdf"}, &Options{}); !errors.Is(err, ErrNoTextLayer) {
		t.Fatalf("err without describer = %v, want ErrNoTextLayer", err)
	}

	d := &countingDescriber{reply: "An invoice from Acme Corp for 3 widgets."}
	res, err := (&PDFConverter{}).Convert(bytes.NewReader(body),
		StreamInfo{Extension: ".pdf"}, &Options{Describer: d.describer()})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	want := pdfCaptionMarker + "\n\nAn invoice from Acme Corp for 3 widgets.\n"
	if res.Markdown != want {
		t.Fatalf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
	if d.calls != 1 {
		t.Fatalf("calls = %d, want 1", d.calls)
	}
	// The stream bytes must have reached the model as an untouched JPEG.
	if d.mimes[0] != "image/jpeg" {
		t.Fatalf("mime = %q, want image/jpeg", d.mimes[0])
	}
	if d.hints[0] != "page 1 of a scanned PDF document" {
		t.Fatalf("hint = %q", d.hints[0])
	}
	if got, want := d.sizes[0], len(testJPEG(t, 200, 260, 1)); got != want {
		t.Fatalf("image sent = %d bytes, want the whole %d-byte jpeg", got, want)
	}
}

func TestPDFMixedTextAndScannedPages(t *testing.T) {
	// The common real-world shape: a born-digital cover page followed by a
	// scanned insert. Text wins where there is text; the model fills the gap.
	d := &countingDescriber{reply: "A handwritten delivery note."}
	res, err := (&PDFConverter{}).Convert(bytes.NewReader(buildScanPDF([]scanPage{
		{content: textPage([3]string{"72", "720", "Cover page"})},
		scanJPEGPage(t, 7),
		{content: textPage([3]string{"72", "720", "Back matter"})},
	})), StreamInfo{Extension: ".pdf"}, &Options{Describer: d.describer()})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	want := "Cover page\n\n---\n\n" + pdfCaptionMarker +
		"\n\nA handwritten delivery note.\n\n---\n\nBack matter\n"
	if res.Markdown != want {
		t.Fatalf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
	if d.calls != 1 {
		t.Fatalf("calls = %d, want 1 (only the text-free page)", d.calls)
	}
	if d.hints[0] != "page 2 of a scanned PDF document" {
		t.Fatalf("hint = %q, want the page number of the scanned page", d.hints[0])
	}
}

func TestPDFDescriberFailureFallsBackToSentinel(t *testing.T) {
	// A model outage must not turn into a half-empty success: with nothing
	// recovered from any page, the caller gets the same honest error as before.
	d := &countingDescriber{failWith: errors.New("rate limited")}
	res, err := (&PDFConverter{}).Convert(
		bytes.NewReader(buildScanPDF([]scanPage{scanJPEGPage(t, 2), scanJPEGPage(t, 3)})),
		StreamInfo{Extension: ".pdf"}, &Options{Describer: d.describer()})
	if !errors.Is(err, ErrNoTextLayer) {
		t.Fatalf("err = %v, want ErrNoTextLayer", err)
	}
	if res.Markdown != "" {
		t.Fatalf("markdown = %q, want empty", res.Markdown)
	}
	if d.calls != 2 {
		t.Fatalf("calls = %d, want one attempt per page", d.calls)
	}
}

func TestPDFSkipsTinyImages(t *testing.T) {
	// A 16x16 logo is not a page. It must never cost a request, and the
	// document must still report that it has no readable text.
	tiny := testTinyJPEG(t)
	d := &countingDescriber{reply: "should never be asked"}
	_, err := (&PDFConverter{}).Convert(bytes.NewReader(buildScanPDF([]scanPage{{
		content: "q\n16 0 0 16 0 0 cm\n/Im0 Do\nQ\n",
		img:     tiny,
		w:       16, h: 16, filter: "/DCTDecode", cs: "/DeviceRGB", bpc: 8,
	}})), StreamInfo{Extension: ".pdf"}, &Options{Describer: d.describer()})
	if !errors.Is(err, ErrNoTextLayer) {
		t.Fatalf("err = %v, want ErrNoTextLayer", err)
	}
	if d.calls != 0 {
		t.Fatalf("describer called %d times on a 16x16 logo", d.calls)
	}
}

// testTinyJPEG is the anti-fixture: an image far below both size thresholds.
func testTinyJPEG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 16, 16)), nil); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

func TestPDFDedupesIdenticalImages(t *testing.T) {
	// The same scanned sheet appearing on two pages is one image: it is
	// described once and the caption reused, so the second page is free.
	same := scanJPEGPage(t, 11)
	d := &countingDescriber{reply: "The same form, twice."}
	res, err := (&PDFConverter{}).Convert(
		bytes.NewReader(buildScanPDF([]scanPage{same, same})),
		StreamInfo{Extension: ".pdf"}, &Options{Describer: d.describer()})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	block := pdfCaptionMarker + "\n\nThe same form, twice."
	if want := block + "\n\n---\n\n" + block + "\n"; res.Markdown != want {
		t.Fatalf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
	if d.calls != 1 {
		t.Fatalf("calls = %d, want 1 — identical bytes must be described once", d.calls)
	}
}

func TestPDFCapsCaptionedPages(t *testing.T) {
	// A long scan must not fire one request per page. Past the cap the
	// remaining pages are dropped and the output says so out loud.
	n := maxPDFCaptionedPages + 5
	pages := make([]scanPage, 0, n)
	for i := 0; i < n; i++ {
		pages = append(pages, scanJPEGPage(t, 100+i)) // every page a distinct image
	}
	d := &countingDescriber{reply: "A page."}
	res, err := (&PDFConverter{}).Convert(bytes.NewReader(buildScanPDF(pages)),
		StreamInfo{Extension: ".pdf"}, &Options{Describer: d.describer()})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if d.calls != maxPDFCaptionedPages {
		t.Fatalf("calls = %d, want the cap of %d", d.calls, maxPDFCaptionedPages)
	}
	if got := strings.Count(res.Markdown, pdfCaptionMarker); got != maxPDFCaptionedPages {
		t.Fatalf("captioned pages = %d, want %d", got, maxPDFCaptionedPages)
	}
	if !strings.HasSuffix(res.Markdown, pdfCaptionCapNote+"\n") {
		t.Fatalf("output does not end with the cap note: %q", res.Markdown[len(res.Markdown)-120:])
	}
}

func TestPDFFlateGrayImageReencodedAsPNG(t *testing.T) {
	// Not every scan is a JPEG: uncompressed 8-bit samples are re-encoded to
	// PNG so the model gets something it can actually open.
	const w, h = 100, 100
	samples := make([]byte, w*h)
	for i := range samples {
		samples[i] = byte(i * 7 % 251)
	}
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	if _, err := zw.Write(samples); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	zw.Close()
	// Pad the stream so it clears minPDFImageBytes the way a real page would;
	// zlib on synthetic data compresses far below a scan's size.
	for z.Len() < minPDFImageBytes {
		z.WriteByte(0) // trailing bytes after the zlib stream are ignored
	}

	d := &countingDescriber{reply: "A grayscale scan."}
	res, err := (&PDFConverter{}).Convert(bytes.NewReader(buildScanPDF([]scanPage{{
		content: "q\n612 0 0 792 0 0 cm\n/Im0 Do\nQ\n",
		img:     z.Bytes(),
		w:       w, h: h, filter: "/FlateDecode", cs: "/DeviceGray", bpc: 8,
	}})), StreamInfo{Extension: ".pdf"}, &Options{Describer: d.describer()})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if res.Markdown != pdfCaptionMarker+"\n\nA grayscale scan.\n" {
		t.Fatalf("markdown = %q", res.Markdown)
	}
	if d.calls != 1 || d.mimes[0] != "image/png" {
		t.Fatalf("calls = %d, mimes = %v, want one image/png call", d.calls, d.mimes)
	}
}

func TestPDFSkipsUndecodableEncodings(t *testing.T) {
	// CCITTFaxDecode (older fax-coded bilevel scans) and JBIG2 are deliberately
	// not implemented. They must be skipped silently, never half-decoded into
	// garbage handed to a model.
	for _, filter := range []string{"/CCITTFaxDecode", "/JBIG2Decode", "/LZWDecode"} {
		t.Run(filter, func(t *testing.T) {
			d := &countingDescriber{reply: "should never be asked"}
			_, err := (&PDFConverter{}).Convert(bytes.NewReader(buildScanPDF([]scanPage{{
				content: "q\n612 0 0 792 0 0 cm\n/Im0 Do\nQ\n",
				img:     bytes.Repeat([]byte{0x5A}, minPDFImageBytes*2),
				w:       800, h: 1000, filter: filter, cs: "/DeviceGray", bpc: 1,
			}})), StreamInfo{Extension: ".pdf"}, &Options{Describer: d.describer()})
			if !errors.Is(err, ErrNoTextLayer) {
				t.Fatalf("err = %v, want ErrNoTextLayer", err)
			}
			if d.calls != 0 {
				t.Fatalf("describer called %d times for %s", d.calls, filter)
			}
		})
	}
}

func TestPDFMalformedStillErrorsWithDescriber(t *testing.T) {
	// The image path must not turn hostile bytes into a panic or a success.
	good := buildScanPDF([]scanPage{scanJPEGPage(t, 5)})
	d := &countingDescriber{reply: "x"}
	opts := &Options{Describer: d.describer()}
	for name, body := range map[string][]byte{
		"truncated":   good[:len(good)/2],
		"no xref":     bytes.ReplaceAll(good, []byte("startxref"), []byte("startxrfe")),
		"header only": []byte("%PDF-1.4\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := (&PDFConverter{}).Convert(bytes.NewReader(body), StreamInfo{Extension: ".pdf"}, opts); err == nil {
				t.Fatal("want an error, got success")
			}
		})
	}
}

func TestPDFScanImagesSurvivesHostileBytes(t *testing.T) {
	// The raw object walker runs on attacker-controlled bytes before any
	// structure has been validated. It must always terminate and never panic,
	// whatever lies the file tells about lengths and object headers.
	r := rand.New(rand.NewSource(42))
	corpus := [][]byte{
		nil,
		[]byte("obj"),
		[]byte("1 0 obj\n<< /Subtype /Image /Length 999999999999 >>\nstream\n"),
		[]byte("1 0 obj\n<< /Subtype /Image /Width -5 /Height 0 /Length 4 >>\nstream\nAB\nendstream\nendobj\n"),
		[]byte("9 9 obj<< /Subtype/Image/Filter[/A/B/C/D/E/F/G/H/I]/Length 1>>stream\nx\nendstream"),
		bytes.Repeat([]byte("1 0 obj stream endstream "), 500),
	}
	for i := 0; i < 200; i++ {
		b := make([]byte, r.Intn(4096))
		r.Read(b)
		corpus = append(corpus, b)
	}
	for i, b := range corpus {
		idx := pdfScanImages(b)
		if len(idx.images) > maxPDFScannedImages {
			t.Fatalf("corpus[%d]: indexed %d images, over the cap", i, len(idx.images))
		}
	}
}

// pdfPad makes every line of a synthetic column the same width, so the column
// looks like set prose rather than a stack of ragged fragments. The fixture
// font is Helvetica with every glyph 500/1000 em, so at 12pt one character is
// exactly 6 points and a 24-character line is 144 points wide.
func pdfPad(s string) string {
	if len(s) > 24 {
		return s[:24]
	}
	return s + strings.Repeat("x", 24-len(s))
}

// TestPDFTwoColumnReadingOrder is the regression test for the defect the
// quality benchmark named: glyphs sorted by Y then X interleave a two-column
// page line by line, keeping every token and scrambling the sequence.
//
// The fixture is a real two-column page — two 144pt columns either side of a
// 124pt gutter, six shared baselines — so the whole column must be emitted
// before the second one starts.
func TestPDFTwoColumnReadingOrder(t *testing.T) {
	left := []string{"Left one", "Left two", "Left three", "Left four", "Left five", "Left six"}
	right := []string{"Right one", "Right two", "Right three", "Right four", "Right five", "Right six"}

	var items [][3]string
	for i := range left {
		y := fmt.Sprint(700 - 14*i)
		// Interleaved in the content stream exactly as a producer emits them:
		// left cell, right cell, next baseline.
		items = append(items, [3]string{"72", y, pdfPad(left[i])})
		items = append(items, [3]string{"340", y, pdfPad(right[i])})
	}

	res, err := (&PDFConverter{}).Convert(
		bytes.NewReader(buildPDF([]string{textPage(items...)}, "")),
		StreamInfo{Extension: ".pdf"}, &Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	// Each column is one wrapped paragraph, and the left column is emitted in
	// full before the right one begins.
	var want strings.Builder
	want.WriteString(strings.Join(pdfPadAll(left), " "))
	want.WriteString("\n\n")
	want.WriteString(strings.Join(pdfPadAll(right), " "))
	want.WriteString("\n")
	if res.Markdown != want.String() {
		t.Fatalf("two-column reading order wrong\n got: %q\nwant: %q", res.Markdown, want.String())
	}
}

// TestPDFSingleColumnUnchangedByColumnDetection pins the other half of the
// contract: a page with no column structure is left in document order, and its
// lines are joined into the one paragraph they were set as.
func TestPDFSingleColumnUnchangedByColumnDetection(t *testing.T) {
	lines := []string{
		"The first line of a single",
		"column page which runs on",
		"for several lines so that",
		"the column detector has a",
		"tall region to look at and",
		"still finds no gutter here",
		"because there is only one",
		"column of text on the page",
	}
	var items [][3]string
	for i, s := range lines {
		items = append(items, [3]string{"72", fmt.Sprint(700 - 14*i), s})
	}

	res, err := (&PDFConverter{}).Convert(
		bytes.NewReader(buildPDF([]string{textPage(items...)}, "")),
		StreamInfo{Extension: ".pdf"}, &Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	want := strings.Join(lines, " ") + "\n"
	if res.Markdown != want {
		t.Fatalf("single-column output moved\n got: %q\nwant: %q", res.Markdown, want)
	}
}

// pdfPadAll pads every string in a slice, so a test can state the padded text
// once as a paragraph rather than line by line.
func pdfPadAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, pdfPad(s))
	}
	return out
}

// TestPDFNarrowColumnsAreNotSplit guards the expensive failure mode of a
// column cut: a table's inter-cell whitespace is a real gutter, and reading it
// as columns would emit every left cell then every right cell, scrambling rows
// that were in the right order to begin with. Narrow, short cells fail the
// column test, so the rows are left alone.
func TestPDFNarrowColumnsAreNotSplit(t *testing.T) {
	rows := [][2]string{
		{"Widget", "12"}, {"Gadget", "34"}, {"Sprocket", "56"},
		{"Flange", "78"}, {"Bracket", "90"}, {"Grommet", "11"},
	}
	var items [][3]string
	for i, r := range rows {
		y := fmt.Sprint(700 - 14*i)
		items = append(items, [3]string{"72", y, r[0]})
		items = append(items, [3]string{"300", y, r[1]})
	}

	res, err := (&PDFConverter{}).Convert(
		bytes.NewReader(buildPDF([]string{textPage(items...)}, "")),
		StreamInfo{Extension: ".pdf"}, &Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	// The rows are read across, never down the two columns. They are also
	// recognised for what they are — a two-column table — and emitted as one.
	var want strings.Builder
	want.WriteString("| " + rows[0][0] + " | " + rows[0][1] + " |\n| --- | --- |\n")
	for _, r := range rows[1:] {
		want.WriteString("| " + r[0] + " | " + r[1] + " |\n")
	}
	if res.Markdown != want.String() {
		t.Fatalf("table rows were split into columns\n got: %q\nwant: %q",
			res.Markdown, want.String())
	}
}

// TestPDFHeadingAboveTwoColumnsStaysFirst checks the cut's other axis: a
// full-width line above a two-column body is separated by the horizontal band
// cut before the vertical cut runs, so it is emitted first rather than being
// torn in half by the gutter beneath it.
func TestPDFHeadingAboveTwoColumnsStaysFirst(t *testing.T) {
	heading := "A full width heading across the whole page"
	items := [][3]string{{"72", "740", heading}}
	left := []string{"Left one", "Left two", "Left three", "Left four", "Left five", "Left six"}
	right := []string{"Right one", "Right two", "Right three", "Right four", "Right five", "Right six"}
	for i := range left {
		y := fmt.Sprint(690 - 14*i)
		items = append(items, [3]string{"72", y, pdfPad(left[i])})
		items = append(items, [3]string{"340", y, pdfPad(right[i])})
	}

	res, err := (&PDFConverter{}).Convert(
		bytes.NewReader(buildPDF([]string{textPage(items...)}, "")),
		StreamInfo{Extension: ".pdf"}, &Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	var want strings.Builder
	want.WriteString(heading + "\n\n")
	for i, s := range left {
		if i > 0 {
			want.WriteString(" ")
		}
		want.WriteString(pdfPad(s))
	}
	want.WriteString("\n\n")
	for i, s := range right {
		if i > 0 {
			want.WriteString(" ")
		}
		want.WriteString(pdfPad(s))
	}
	want.WriteString("\n")
	if res.Markdown != want.String() {
		t.Fatalf("heading above columns misplaced\n got: %q\nwant: %q",
			res.Markdown, want.String())
	}
}

// textPageSized is textPage with a font size per item, so a fixture can set a
// heading larger than its body text.
func textPageSized(items ...[4]string) string {
	var b strings.Builder
	b.WriteString("BT\n")
	for _, it := range items {
		fmt.Fprintf(&b, "/F1 %s Tf\n1 0 0 1 %s %s Tm\n(%s) Tj\n", it[3], it[0], it[1], it[2])
	}
	b.WriteString("ET\n")
	return b.String()
}

func convertPDFPages(t *testing.T, pages ...string) string {
	t.Helper()
	res, err := (&PDFConverter{}).Convert(
		bytes.NewReader(buildPDF(pages, "")),
		StreamInfo{Extension: ".pdf"}, &Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	return res.Markdown
}

// TestPDFHeadingsFromSizeAndNumbering pins both routes into a heading. A PDF
// records no heading of any kind, so the only evidence is that a line is set
// larger than the body, or that it is numbered and stands alone — and the
// number's depth is what sets the level.
func TestPDFHeadingsFromSizeAndNumbering(t *testing.T) {
	got := convertPDFPages(t, textPageSized(
		[4]string{"72", "740", "Annual Report", "24"},
		[4]string{"72", "700", "1. Overview", "16"},
		[4]string{"72", "670", "1.2 Regional detail", "13"},
		[4]string{"72", "640", "Body text that is long enough to be a real paragraph here.", "10"},
		[4]string{"72", "628", "It continues onto a second line of the same paragraph.", "10"},
	))
	want := "# Annual Report\n\n" +
		"## 1. Overview\n\n" +
		"### 1.2 Regional detail\n\n" +
		"Body text that is long enough to be a real paragraph here. " +
		"It continues onto a second line of the same paragraph.\n"
	if got != want {
		t.Fatalf("headings\n got: %q\nwant: %q", got, want)
	}
}

// TestPDFHeadingRejectsLeadIn guards the rule that costs the most when it is
// wrong. A bold, short, body-size line ending in a colon is a lead-in to the
// text beneath it, not a section: promoting it to a heading splits a rule away
// from the condition it governs.
func TestPDFHeadingRejectsLeadIn(t *testing.T) {
	got := convertPDFPages(t, textPageSized(
		[4]string{"72", "740", "Conditions:", "10"},
		[4]string{"72", "710", "The following conditions apply to every fee listed above.", "10"},
	))
	if strings.Contains(got, "#") {
		t.Fatalf("lead-in promoted to a heading: %q", got)
	}
}

// TestPDFTableFromAlignedRows is the borderless-table case: no rules are drawn
// anywhere, and the only evidence of a table is that every row splits at the
// same X.
func TestPDFTableFromAlignedRows(t *testing.T) {
	rows := [][3]string{
		{"Region", "Nodes", "Uptime"},
		{"eu-west", "48", "99.98%"},
		{"us-east", "96", "99.95%"},
		{"ap-south", "24", "99.90%"},
	}
	var items [][3]string
	for i, r := range rows {
		y := fmt.Sprint(700 - 20*i)
		items = append(items, [3]string{"72", y, r[0]})
		items = append(items, [3]string{"220", y, r[1]})
		items = append(items, [3]string{"330", y, r[2]})
	}
	got := convertPDFPages(t, textPage(items...))
	want := "| Region | Nodes | Uptime |\n" +
		"| --- | --- | --- |\n" +
		"| eu-west | 48 | 99.98% |\n" +
		"| us-east | 96 | 99.95% |\n" +
		"| ap-south | 24 | 99.90% |\n"
	if got != want {
		t.Fatalf("borderless table\n got: %q\nwant: %q", got, want)
	}
}

// TestPDFTableShortRowKeepsItsColumn is the reason cells are placed by
// geometry rather than in order. A totals line holds two cells; written left to
// right they would file the amount under "Description", which is a wrong number
// in a plausible place — the worst kind of extraction error.
func TestPDFTableShortRowKeepsItsColumn(t *testing.T) {
	var items [][3]string
	rows := [][3]string{
		{"Item", "Qty", "Amount"},
		{"Widgets", "12", "120.00"},
		{"Gadgets", "4", "80.00"},
	}
	for i, r := range rows {
		y := fmt.Sprint(700 - 20*i)
		items = append(items, [3]string{"72", y, r[0]})
		items = append(items, [3]string{"220", y, r[1]})
		items = append(items, [3]string{"330", y, r[2]})
	}
	// The totals row has no quantity: a label, then an amount under "Amount".
	items = append(items, [3]string{"72", "640", "Total"})
	items = append(items, [3]string{"330", "640", "200.00"})

	got := convertPDFPages(t, textPage(items...))
	if !strings.Contains(got, "| Total |  | 200.00 |") {
		t.Fatalf("short row was not filed under its own columns: %q", got)
	}
}

// TestPDFListMarkersSurvive covers both shapes of list marker a PDF carries: a
// literal bullet glyph, and the private-use code point Word writes when the
// bullet comes from Symbol or Wingdings.
func TestPDFListMarkersSurvive(t *testing.T) {
	got := convertPDFPages(t, textPageSized(
		[4]string{"72", "740", "\\267 First item from a Symbol bullet", "10"},
		[4]string{"72", "726", "\\267 Second item from a Symbol bullet", "10"},
		[4]string{"72", "700", "1. First numbered item", "10"},
		[4]string{"72", "686", "2. Second numbered item", "10"},
	))
	for _, want := range []string{
		"- First item from a Symbol bullet\n- Second item from a Symbol bullet",
		"1. First numbered item\n2. Second numbered item",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("list marker lost\n got: %q\nwant substring: %q", got, want)
		}
	}
}

// TestPDFHyphenJoinUsesWitnesses is the rule that a bare join-if-lowercase
// heuristic gets wrong. Whether a line-final hyphen was the typesetter's or the
// author's cannot be read off the two lines; it is settled by whether the
// document spells the compound out intact somewhere else.
func TestPDFHyphenJoinUsesWitnesses(t *testing.T) {
	got := convertPDFPages(t, textPageSized(
		[4]string{"72", "740", "The system keeps dura-", "10"},
		[4]string{"72", "728", "bility guarantees. Write by e-", "10"},
		[4]string{"72", "716", "mail, and mind the Noord-", "10"},
		[4]string{"72", "704", "Holland clause.", "10"},
		[4]string{"72", "680", "Elsewhere the document writes e-mail intact.", "10"},
	))
	for _, want := range []string{"durability", "e-mail,", "Noord-Holland"} {
		if !strings.Contains(got, want) {
			t.Fatalf("hyphen handling\n got: %q\nwant substring: %q", got, want)
		}
	}
	if strings.Contains(got, "email") || strings.Contains(got, "e- mail") {
		t.Fatalf("author's hyphen was joined away: %q", got)
	}
}

// TestPDFRunningFurnitureKeptOnce checks that a running header is recognised
// across pages and reported once. Deleting it outright would lose the only
// copy of a fact like a revision date; repeating it on every page is
// pagination leaking into the text.
func TestPDFRunningFurnitureKeptOnce(t *testing.T) {
	var pages []string
	for p := 1; p <= 4; p++ {
		pages = append(pages, textPageSized(
			[4]string{"72", "770", "ACME CONFIDENTIAL — revised 2026-09-24", "8"},
			[4]string{"72", "700", fmt.Sprintf("Body text on page %d of the document.", p), "10"},
			[4]string{"72", "40", fmt.Sprintf("Page %d", p), "8"},
		))
	}
	got := convertPDFPages(t, pages...)
	if n := strings.Count(got, "ACME CONFIDENTIAL"); n != 1 {
		t.Fatalf("running header appears %d times, want 1: %q", n, got)
	}
	if n := strings.Count(got, "Page "); n != 1 {
		t.Fatalf("page number appears %d times, want 1: %q", n, got)
	}
	for p := 1; p <= 4; p++ {
		if !strings.Contains(got, fmt.Sprintf("page %d of", p)) {
			t.Fatalf("body of page %d lost: %q", p, got)
		}
	}
}

// TestPDFStandardFontWidthsAdvance is the regression test for a failure that
// hid in plain sight: a font from the standard 14 carries no /Widths array, so
// every glyph was reported at the same X. The text still read correctly in
// document order, which is why it went unnoticed — but every word, column and
// cell boundary this package finds is a gap between X coordinates, and there
// were none.
func TestPDFStandardFontWidthsAdvance(t *testing.T) {
	page := "BT\n/F1 12 Tf\n1 0 0 1 72 700 Tm\n(Hello) Tj\nET\n"
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [ 4 0 R ] /Count 1 >>",
		// No /Widths, no /FontDescriptor: the metrics must come from the
		// Core 14 tables.
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [ 0 0 612 792 ]" +
			" /Resources << /Font << /F1 3 0 R >> >> /Contents 5 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(page), page),
	}
	res, err := (&PDFConverter{}).Convert(
		bytes.NewReader(assemblePDF(objs, "")), StreamInfo{Extension: ".pdf"}, &Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if got := strings.TrimSpace(res.Markdown); got != "Hello" {
		t.Fatalf("text = %q, want %q", got, "Hello")
	}
}

// TestPDFTableSpanningPagesIsOneTable covers a table longer than a page, which
// is how real inventories, invoices and financial statements are set.
//
// Three separate failures meet here, and all of them silently lose or corrupt
// data rather than looking wrong:
//   - the producer repeats the header on every page, which is exactly the shape
//     the running-header pass looks for; suppressing it leaves the delimiter
//     row to promote the first data row of that page into a header;
//   - rendering page by page emits one table per page with a horizontal rule
//     between them, and Markdown cannot resume a table after a rule;
//   - the table's own rows outnumber the prose on the page, so measuring the
//     body size over them makes every real paragraph larger than "the body"
//     and turns it into a heading.
func TestPDFTableSpanningPagesIsOneTable(t *testing.T) {
	header := [][3]string{{"72", "720", "ID"}, {"200", "720", "Component"}, {"400", "720", "Amount"}}
	var pages []string
	row := 0
	for p := 0; p < 3; p++ {
		items := append([][3]string{}, header...)
		if p == 0 {
			items = append([][3]string{{"72", "760", "Service Inventory"}}, items...)
		}
		for r := 0; r < 8; r++ {
			row++
			y := fmt.Sprint(700 - 16*r)
			items = append(items,
				[3]string{"72", y, fmt.Sprintf("SVC-%03d", row)},
				[3]string{"200", y, fmt.Sprintf("node-%d", row)},
				[3]string{"400", y, fmt.Sprintf("%d.00", row*10)})
		}
		pages = append(pages, textPage(items...))
	}

	got := convertPDFPages(t, pages...)

	// Count delimiter LINES: a three-column delimiter row contains the
	// substring "| --- |" more than once.
	delims := 0
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "| --- |") {
			delims++
		}
	}
	if delims != 1 {
		t.Fatalf("table was split into %d pieces, want 1\n%s", delims, got)
	}
	if n := strings.Count(got, "| ID | Component | Amount |"); n != 1 {
		t.Fatalf("header row appears %d times, want 1\n%s", n, got)
	}
	if strings.Contains(got, "\n---\n") {
		t.Fatalf("a page rule was emitted inside the table\n%s", got)
	}
	// Every row must survive, and none may have been eaten by a header.
	for r := 1; r <= 24; r++ {
		want := fmt.Sprintf("| SVC-%03d | node-%d | %d.00 |", r, r, r*10)
		if !strings.Contains(got, want) {
			t.Fatalf("row %d missing or reshaped, want %q\n%s", r, want, got)
		}
	}
}

// pdfJPEG returns a real JPEG of the given size, large enough to clear the
// converter's "this is a rule or a logo" floors.
func pdfJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			im.Set(x, y, color.RGBA{uint8(x), uint8(y), 0x40, 0xff})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, im, nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

// pdfWithFigure builds a one-page PDF holding two lines of text with an image
// drawn between them.
//
// The image is stored ASCII85-then-DCT encoded and reached through a Form
// XObject, because that is what real producers emit: a chain rather than a
// single filter, and a wrapper rather than a bare image.
func pdfWithFigure(t *testing.T, jpg []byte, w, h int) []byte {
	t.Helper()
	var a85 bytes.Buffer
	enc := ascii85.NewEncoder(&a85)
	if _, err := enc.Write(jpg); err != nil {
		t.Fatalf("ascii85: %v", err)
	}
	enc.Close()
	payload := a85.String() + "~>"

	content := "BT\n/F1 12 Tf\n1 0 0 1 72 700 Tm\n(Above the figure) Tj\nET\n" +
		"q 400 0 0 300 72 350 cm /Fig Do Q\n" +
		"BT\n/F1 12 Tf\n1 0 0 1 72 300 Tm\n(Below the figure) Tj\nET\n"

	widths := strings.TrimSpace(strings.Repeat("500 ", 95))
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [ 4 0 R ] /Count 1 >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding" +
			" /FirstChar 32 /LastChar 126 /Widths [ " + widths + " ] >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [ 0 0 612 792 ]" +
			" /Resources << /Font << /F1 3 0 R >> /XObject << /Fig 6 0 R >> >>" +
			" /Contents 5 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
		fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d"+
			" /ColorSpace /DeviceRGB /BitsPerComponent 8"+
			" /Filter [ /ASCII85Decode /DCTDecode ] /Length %d >>\nstream\n%s\nendstream",
			w, h, len(payload), payload),
	}
	return assemblePDF(objs, "")
}

// TestPDFFigureIsInlinedInReadingOrder is the regression test for a figure that
// used to vanish without trace.
//
// Images were only ever looked at on a page with NO text, the pure-scan case,
// so a chart sitting in a report was dropped along with any sign that it had
// been there — the caption beneath it was left pointing at nothing.
func TestPDFFigureIsInlinedInReadingOrder(t *testing.T) {
	jpg := pdfJPEG(t, 200, 150)
	res, err := (&PDFConverter{}).Convert(
		bytes.NewReader(pdfWithFigure(t, jpg, 200, 150)),
		StreamInfo{Extension: ".pdf"}, &Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	// It must land between the two lines, which is where the page draws it.
	want := "Above the figure\n\n![](data:…)\n\nBelow the figure\n"
	if got := stripDataURIs(res.Markdown); got != want {
		t.Fatalf("figure misplaced or missing\n got: %q\nwant: %q", got, want)
	}
	if !strings.Contains(res.Markdown, "![](data:image/jpeg;base64,") {
		t.Errorf("figure not inlined as a jpeg data URI: %.80q", res.Markdown)
	}

	// DropImages is the way out, and it must not cost the text.
	res, err = (&PDFConverter{}).Convert(
		bytes.NewReader(pdfWithFigure(t, jpg, 200, 150)),
		StreamInfo{Extension: ".pdf"}, &Options{DropImages: true})
	if err != nil {
		t.Fatalf("Convert with DropImages: %v", err)
	}
	if strings.Contains(res.Markdown, "data:") {
		t.Errorf("DropImages still inlined the payload: %.80q", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "Above the figure") ||
		!strings.Contains(res.Markdown, "Below the figure") {
		t.Errorf("text lost with DropImages: %q", res.Markdown)
	}
}

// TestPDFSimulatedBoldIsNotDoubled covers a document asking for a weight its
// font does not have, by painting the same glyphs twice a hair apart.
//
// Both paintings are real glyphs in the text layer, so taking the layer at face
// value spells "Important" as "IImmppoorrttaanntt". The guard has to be a
// positional one: a genuine double letter is a whole advance away, and
// collapsing by sequence instead would turn "bookkeeper" into "bokeper".
func TestPDFSimulatedBoldIsNotDoubled(t *testing.T) {
	got := convertPDFPages(t, textPage(
		[3]string{"72", "700", "Important"},
		[3]string{"72.3", "700", "Important"}, // the overprint
		[3]string{"72", "680", "A bookkeeper committee will assess all offers."},
	))
	want := "Important A bookkeeper committee will assess all offers.\n"
	if got != want {
		t.Fatalf("overprint handling\n got: %q\nwant: %q", got, want)
	}
}

// TestPDFMultiLineRunningHeaderIsSuppressed covers a masthead of more than one
// line — a name over a strapline — set in two different sizes, which is the
// usual way.
//
// Allowing only the single outermost line to be furniture suppresses the first
// line and leaves the second repeating on every page, which is the more
// visible half of the failure: the part that survives is the part the reader
// keeps seeing.
func TestPDFMultiLineRunningHeaderIsSuppressed(t *testing.T) {
	var pages []string
	for p := 1; p <= 4; p++ {
		// The masthead is two sizes, a name over a strapline, which is how
		// they are usually set — and a real page of body text beneath it, so
		// the document's body size is the body's.
		items := [][4]string{
			{"72", "770", "ACME RESEARCH INSTITUTE", "10"},
			{"72", "758", "Journal of Layout Studies", "8"},
		}
		for i := 0; i < 15; i++ {
			items = append(items, [4]string{"72", fmt.Sprint(700 - 16*i), fmt.Sprintf(
				"Body line %d on page %d of the document.", i+1, p), "10"})
		}
		pages = append(pages, textPageSized(items...))
	}
	got := convertPDFPages(t, pages...)
	for _, line := range []string{"ACME RESEARCH INSTITUTE", "Journal of Layout Studies"} {
		if n := strings.Count(got, line); n != 1 {
			t.Errorf("%q appears %d times, want 1", line, n)
		}
	}
	// Suppressing furniture must never cost body text.
	for p := 1; p <= 4; p++ {
		for _, n := range []int{1, 15} {
			if want := fmt.Sprintf("Body line %d on page %d of the document.", n, p); !strings.Contains(got, want) {
				t.Errorf("body lost: %q", want)
			}
		}
	}
}

// TestPDFBodyInsideTheBandIsNotFurniture is the vector a sibling project's
// corpus turned up: a document set with ordinary one-inch margins puts its
// first line of body text inside any band wide enough to catch a real running
// head, and its last line inside the footer band.
//
// A band test alone therefore takes the opening and closing lines of every
// page. Because a page number or a date makes those lines differ only in their
// digits, the repeat test — which normalises digits so "Page 3" matches
// "Page 4" — then matches them across pages and deletes them. What keeps them
// is that body text runs on at the body's own pitch instead of standing
// detached the way a running head does.
func TestPDFBodyInsideTheBandIsNotFurniture(t *testing.T) {
	var pages []string
	for p := 1; p <= 4; p++ {
		var items [][4]string
		// A full page of body text at a one-inch margin: first line at 720 on
		// a 792pt page is 9% from the edge, and the last is 9% from the foot.
		for i := 0; i < 40; i++ {
			items = append(items, [4]string{"72", fmt.Sprint(720 - 16*i), fmt.Sprintf(
				"Paragraph line %d on page %d of the document.", i+1, p), "10"})
		}
		pages = append(pages, textPageSized(items...))
	}
	got := convertPDFPages(t, pages...)
	for p := 1; p <= 4; p++ {
		for _, n := range []int{1, 40} { // the first and last line of each page
			want := fmt.Sprintf("Paragraph line %d on page %d of the document.", n, p)
			if !strings.Contains(got, want) {
				t.Errorf("body line deleted as furniture: %q", want)
			}
		}
	}
}

// TestPDFDetachedFooterInsideTheBandIsFurniture is the other half: narrowing
// the band until body text falls outside it would throw away real footers,
// which sit further into the page than a header does. This one is at 10% of
// the page height, and it is caught because it stands detached from the text
// above it — not because of where it sits.
func TestPDFDetachedFooterInsideTheBandIsFurniture(t *testing.T) {
	var pages []string
	for p := 1; p <= 4; p++ {
		var items [][4]string
		for i := 0; i < 30; i++ {
			items = append(items, [4]string{"72", fmt.Sprint(700 - 16*i), fmt.Sprintf(
				"Body line %d on page %d.", i+1, p), "10"})
		}
		// Detached, and at 10% of the page height: inside a 12% band, outside
		// an 8% one.
		items = append(items, [4]string{"72", "80", fmt.Sprintf("Confidential draft — page %d", p), "8"})
		pages = append(pages, textPageSized(items...))
	}
	got := convertPDFPages(t, pages...)
	if n := strings.Count(got, "Confidential draft"); n != 1 {
		t.Errorf("detached footer appears %d times, want 1", n)
	}
	for p := 1; p <= 4; p++ {
		if want := fmt.Sprintf("Body line 30 on page %d.", p); !strings.Contains(got, want) {
			t.Errorf("last body line deleted as furniture: %q", want)
		}
	}
}

// pdfGarbledPage builds a page whose font maps every code to U+FFFD, which is
// what an absent or broken ToUnicode produces.
func pdfGarbledPage(t *testing.T, text string) []byte {
	t.Helper()
	var cmap strings.Builder
	cmap.WriteString("/CIDInit /ProcSet findresource begin 12 dict begin begincmap\n" +
		"1 begincodespacerange <00> <FF> endcodespacerange\n")
	fmt.Fprintf(&cmap, "%d beginbfchar\n", len(text))
	for i := 0; i < len(text); i++ {
		fmt.Fprintf(&cmap, "<%02X> <FFFD>\n", text[i])
	}
	cmap.WriteString("endbfchar\nendcmap CMapName currentdict /CMap defineresource pop end end\n")

	content := "BT\n/F1 12 Tf\n1 0 0 1 72 700 Tm\n(" + text + ") Tj\nET\n"
	widths := strings.TrimSpace(strings.Repeat("500 ", 95))
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [ 4 0 R ] /Count 1 >>",
		"<< /Type /Font /Subtype /TrueType /BaseFont /Garbled /FirstChar 32 /LastChar 126" +
			" /Widths [ " + widths + " ] /ToUnicode 6 0 R >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [ 0 0 612 792 ]" +
			" /Resources << /Font << /F1 3 0 R >> >> /Contents 5 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", cmap.Len(), cmap.String()),
	}
	return assemblePDF(objs, "")
}

// TestPDFUnreadableTextLayerIsAnError covers the most dangerous outcome a
// converter can have: not a crash and not an empty result, but a confident one
// that is wrong.
//
// A font with no usable ToUnicode still draws glyphs in the right places, so
// the extraction "succeeds" — correct reading order, correct line breaks, and
// every character meaningless. Nothing downstream can tell that from real
// text, so it has to be named rather than returned.
func TestPDFUnreadableTextLayerIsAnError(t *testing.T) {
	text := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 3)
	_, err := (&PDFConverter{}).Convert(
		bytes.NewReader(pdfGarbledPage(t, text)),
		StreamInfo{Extension: ".pdf"}, &Options{})
	if !errors.Is(err, ErrGarbledTextLayer) {
		t.Fatalf("err = %v, want ErrGarbledTextLayer", err)
	}
}

// TestPDFSoundTextLayerIsNotRejected is the other side of the gate. A few
// private-use code points are normal — Word writes its list bullets there — so
// the test is a share of the page, not a presence check.
func TestPDFSoundTextLayerIsNotRejected(t *testing.T) {
	var items [][3]string
	for i := 0; i < 12; i++ {
		// One Symbol bullet (U+F0B7 arrives as the byte 0267 in WinAnsi) per
		// line of ordinary prose.
		items = append(items, [3]string{"72", fmt.Sprint(700 - 14*i),
			fmt.Sprintf("\\267 Item %d with a good deal of perfectly readable text on it", i+1)})
	}
	res, err := (&PDFConverter{}).Convert(
		bytes.NewReader(buildPDF([]string{textPage(items...)}, "")),
		StreamInfo{Extension: ".pdf"}, &Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if !strings.Contains(res.Markdown, "Item 1 with a good deal") {
		t.Fatalf("sound page was rejected: %q", res.Markdown)
	}
}

// pdfOutlinePDF builds a document whose bookmarks name two of its lines and
// nest one under the other, while the page itself sets every line at the same
// size so the font rules cannot tell them apart.
func pdfOutlinePDF(t *testing.T) []byte {
	t.Helper()
	page := textPage(
		[3]string{"72", "740", "Scope of Works"},
		[3]string{"72", "710", "Materials and Handling"},
		[3]string{"72", "680", "All materials shall be delivered to the site in their original packaging."},
	)
	widths := strings.TrimSpace(strings.Repeat("500 ", 95))
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R /Outlines 6 0 R >>",
		"<< /Type /Pages /Kids [ 4 0 R ] /Count 1 >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding" +
			" /FirstChar 32 /LastChar 126 /Widths [ " + widths + " ] >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [ 0 0 612 792 ]" +
			" /Resources << /Font << /F1 3 0 R >> >> /Contents 5 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(page), page),
		"<< /Type /Outlines /First 7 0 R /Last 7 0 R /Count 2 >>",
		"<< /Title (Scope of Works) /Parent 6 0 R /First 8 0 R /Last 8 0 R /Count 1 >>",
		"<< /Title (Materials and Handling) /Parent 7 0 R >>",
	}
	return assemblePDF(objs, "")
}

// TestPDFOutlineSetsHeadingLevels covers the source anymd was ignoring: the
// document's own bookmarks.
//
// The outline is the author's table of contents — it names the headings and
// says how they nest. Here nothing else can recover that: both headings are
// set at body size, so the size rule sees no heading at all, and neither is
// numbered, so the numbering rule has nothing to read either.
func TestPDFOutlineSetsHeadingLevels(t *testing.T) {
	res, err := (&PDFConverter{}).Convert(
		bytes.NewReader(pdfOutlinePDF(t)), StreamInfo{Extension: ".pdf"}, &Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	for _, want := range []string{"# Scope of Works", "## Materials and Handling"} {
		if !strings.Contains(res.Markdown, want) {
			t.Errorf("outline level not applied, want %q\n got: %q", want, res.Markdown)
		}
	}
	// A line the outline does not name stays prose, however close it sits to
	// one that it does: the outline refines headings, it does not invent them.
	if strings.Contains(res.Markdown, "# All materials") {
		t.Errorf("body text promoted to a heading: %q", res.Markdown)
	}
}

// TestPDFFlatOutlineDoesNotFlattenHeadings guards the way an outline is
// trusted. Plenty of producers list every heading as a top-level bookmark: a
// long licence agreement met in testing had "1. Accepting this Agreement" and
// "1.1 Acceptance" as siblings in its outline, while the numbering on the page
// states the nesting plainly. Taking such an outline's depths at face value
// flattens a real hierarchy, and a wrong level is worse than none.
func TestPDFFlatOutlineDoesNotFlattenHeadings(t *testing.T) {
	page := textPage(
		[3]string{"72", "740", "1. Scope"},
		[3]string{"72", "710", "1.1 Materials"},
		[3]string{"72", "680", "All materials shall be delivered to the site as specified."},
	)
	widths := strings.TrimSpace(strings.Repeat("500 ", 95))
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R /Outlines 6 0 R >>",
		"<< /Type /Pages /Kids [ 4 0 R ] /Count 1 >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding" +
			" /FirstChar 32 /LastChar 126 /Widths [ " + widths + " ] >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [ 0 0 612 792 ]" +
			" /Resources << /Font << /F1 3 0 R >> >> /Contents 5 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(page), page),
		// A flat outline: both headings are siblings at the top level.
		"<< /Type /Outlines /First 7 0 R /Last 8 0 R /Count 2 >>",
		"<< /Title (1. Scope) /Parent 6 0 R /Next 8 0 R >>",
		"<< /Title (1.1 Materials) /Parent 6 0 R /Prev 7 0 R >>",
	}
	res, err := (&PDFConverter{}).Convert(
		bytes.NewReader(assemblePDF(objs, "")), StreamInfo{Extension: ".pdf"}, &Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	// Both are recognised as headings by the outline, but the numbering is
	// what says one sits under the other.
	if !strings.Contains(res.Markdown, "# 1. Scope") {
		t.Errorf("outline heading lost: %q", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "## 1.1 Materials") {
		t.Errorf("flat outline flattened the hierarchy: %q", res.Markdown)
	}
}
