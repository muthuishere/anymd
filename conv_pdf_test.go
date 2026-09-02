package anymd

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"image"
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
