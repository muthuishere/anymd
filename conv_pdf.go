package anymd

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/ledongthuc/pdf"

	"github.com/muthuishere/anymd/internal/mdutil"
)

func init() { addBuiltin(&PDFConverter{}) }

// ErrNoTextLayer reports that a PDF parsed cleanly but carries no text layer at
// all — the classic pure-scan document, where every page is a single image.
//
// This is a distinct, documented outcome rather than an empty success on
// purpose: emitting "" would be indistinguishable from a genuinely blank
// document, and the caller could not tell that the bytes it needs are locked
// inside a raster image. anymd is pure Go with no OCR engine, so recovering
// that text is out of scope unless the caller supplies an Options.Describer —
// with one, the page's embedded image is lifted out of the object graph and
// read by a vision model instead, and this error is returned only when even
// that produced nothing.
var ErrNoTextLayer = errors.New("pdf has no text layer (scanned images only); OCR is out of scope for anymd")

// ErrEncryptedPDF reports that a PDF is encrypted and could not be opened with
// an empty password. We surface this instead of emitting empty output, so an
// encrypted file is never mistaken for an empty one.
var ErrEncryptedPDF = errors.New("pdf is encrypted")

// pdfMagic is the header every PDF starts with, at offset 0.
const pdfMagic = "%PDF-"

// maxPDFBytes bounds how much we will buffer for a stream that cannot be read
// randomly. Hostile input must never be able to make us allocate without limit.
const maxPDFBytes = 256 << 20

// maxPDFGlyphs bounds the number of positioned glyphs we will lay out per
// document. A crafted content stream can emit glyphs in a loop; this keeps the
// layout pass linear and bounded.
const maxPDFGlyphs = 4 << 20

// PDFConverter extracts a PDF's text layer as Markdown.
//
// Pages are separated by a `---` horizontal rule rather than a heading: a page
// number is pagination, not document structure, and injecting it as a heading
// would corrupt the outline of every document it touches.
type PDFConverter struct{}

// Name identifies the converter in errors and in `anymd --list`.
func (c *PDFConverter) Name() string { return "pdf" }

// Accepts sniffs the %PDF- magic first — it is the only signal that cannot be
// faked by a wrong filename — and falls back to the mime and extension hints so
// that a mislabelled or truncated PDF still reaches Convert and produces a real
// error instead of being swallowed by the plaintext fallback.
func (c *PDFConverter) Accepts(r io.ReadSeeker, info StreamInfo, opts *Options) bool {
	var head [len(pdfMagic)]byte
	if n, _ := io.ReadFull(r, head[:]); n == len(pdfMagic) && string(head[:]) == pdfMagic {
		return true
	}
	if info.NormalizedMime() == "application/pdf" || info.NormalizedMime() == "application/x-pdf" {
		return true
	}
	return info.HasExt(".pdf")
}

// Convert extracts the text layer page by page.
//
// The whole body runs under a recover: ledongthuc/pdf reports malformed
// structure by panicking (see its errorf), and this package's contract is that
// hostile bytes produce an error, never a crash in the caller's process.
func (c *PDFConverter) Convert(r io.ReadSeeker, info StreamInfo, opts *Options) (res Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			res = Result{}
			err = fmt.Errorf("malformed pdf: %v", p)
		}
	}()

	// The raw bytes are only materialized when a Describer is present: image
	// extraction needs the undecoded stream payloads, and with no Describer
	// nothing will ever read them, so we keep the cheaper ReaderAt path and the
	// exact behavior this converter has always had.
	var raw []byte
	var ra io.ReaderAt
	var size int64
	if opts.HasDescriber() {
		if raw, err = pdfReadAll(r); err != nil {
			return Result{}, err
		}
		ra, size = bytes.NewReader(raw), int64(len(raw))
	} else if ra, size, err = pdfReaderAt(r); err != nil {
		return Result{}, err
	}
	if size == 0 {
		return Result{}, errors.New("empty pdf")
	}

	doc, err := pdf.NewReader(ra, size)
	if err != nil {
		if pdfIsEncryptionError(err) {
			return Result{}, ErrEncryptedPDF
		}
		return Result{}, err
	}

	encrypted := !doc.Trailer().Key("Encrypt").IsNull()

	n := doc.NumPage()
	if n < 0 {
		n = 0
	}
	blocks := make([]string, 0, 2*n)
	budget := maxPDFGlyphs
	var idx *pdfImageIndex            // built lazily, on the first text-free page
	captions := map[[32]byte]string{} // sha256 -> caption, deduped per document
	captioned, capped := 0, false
	for i := 1; i <= n; i++ {
		page := doc.Page(i)
		if page.V.IsNull() {
			continue
		}
		text := pdfPageText(page, &budget)
		if text == "" && opts.HasDescriber() && !capped {
			// No text layer on this page. The pixels a scanner produced are
			// still in the file as an embedded image; read those instead.
			if idx == nil {
				idx = pdfScanImages(raw)
			}
			if captioned >= maxPDFCaptionedPages {
				capped = true
			} else if c := pdfDescribePage(idx.pageImages(page), i, captions, opts); c != "" {
				text = c
				captioned++
			}
		}
		if text == "" {
			// A page with no text (a scan, a full-bleed figure, a spacer) is
			// skipped rather than emitted as an empty block.
			continue
		}
		if len(blocks) > 0 {
			blocks = append(blocks, "---")
		}
		blocks = append(blocks, text)
	}
	if capped && len(blocks) > 0 {
		blocks = append(blocks, pdfCaptionCapNote)
	}

	if len(blocks) == 0 {
		if encrypted {
			// Decryption "succeeded" but produced nothing readable; that is an
			// encryption problem, not a missing-text-layer one.
			return Result{}, ErrEncryptedPDF
		}
		return Result{}, ErrNoTextLayer
	}
	return Result{Markdown: mdutil.Join(blocks...)}, nil
}

// pdfReadAll buffers the whole document, bounded, for the image-extraction
// pass, which needs to look at raw stream bytes anywhere in the file.
func pdfReadAll(r io.ReadSeeker) ([]byte, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(r, maxPDFBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxPDFBytes {
		return nil, fmt.Errorf("pdf too large: over %d bytes", maxPDFBytes)
	}
	return b, nil
}

// pdfDescribePage captions a text-free page's images and renders them as that
// page's content.
//
// Identical images are described once per document and the caption reused:
// a repeated letterhead, a scanned form printed on every page, or the same
// blank sheet must not be paid for twice. A Describer failure yields "", which
// leaves the page empty — never a fabricated stand-in.
func pdfDescribePage(imgs []pdfPageImage, page int, seen map[[32]byte]string, opts *Options) string {
	var parts []string
	hint := fmt.Sprintf("page %d of a scanned PDF document", page)
	for _, im := range imgs {
		sum := sha256.Sum256(im.data)
		caption, ok := seen[sum]
		if !ok {
			caption = describeImageWithHint(im.data, im.mime, hint, opts)
			seen[sum] = caption
		}
		if caption != "" {
			parts = append(parts, pdfCaptionMarker+"\n\n"+caption)
		}
	}
	return strings.Join(parts, "\n\n")
}

// pdfReaderAt gives the pdf package the random access it needs. A file or a
// bytes.Reader is used in place; anything else is buffered, under a hard cap so
// a hostile stream cannot exhaust memory.
func pdfReaderAt(r io.ReadSeeker) (io.ReaderAt, int64, error) {
	if ra, ok := r.(io.ReaderAt); ok {
		if size, err := r.Seek(0, io.SeekEnd); err == nil {
			if _, err := r.Seek(0, io.SeekStart); err == nil {
				if size > maxPDFBytes {
					return nil, 0, fmt.Errorf("pdf too large: %d bytes", size)
				}
				return ra, size, nil
			}
		}
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			return nil, 0, err
		}
	}
	b, err := io.ReadAll(io.LimitReader(r, maxPDFBytes+1))
	if err != nil {
		return nil, 0, err
	}
	if len(b) > maxPDFBytes {
		return nil, 0, fmt.Errorf("pdf too large: over %d bytes", maxPDFBytes)
	}
	return bytes.NewReader(b), int64(len(b)), nil
}

// pdfIsEncryptionError distinguishes "we cannot decrypt this" from "this file
// is broken". The pdf package reports both as plain errors.
func pdfIsEncryptionError(err error) bool {
	if errors.Is(err, pdf.ErrInvalidPassword) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "encrypt")
}

// pdfPageText lays the page's positioned glyphs back out into reading order.
//
// We deliberately use the positioned Content() API rather than GetPlainText:
// GetPlainText concatenates show-text operands, so two adjacent text objects
// separated only by a Td offset — which is how most producers emit a space
// between words, and how every column layout works — come out run together.
// With X, Y, width and font size per glyph we can put the spaces back.
func pdfPageText(p pdf.Page, budget *int) (out string) {
	defer func() {
		// The content-stream interpreter panics on malformed operands; a bad
		// page degrades to no text rather than killing the document.
		if recover() != nil {
			out = ""
		}
	}()

	if p.V.IsNull() || p.V.Key("Contents").IsNull() {
		return ""
	}
	chars := p.Content().Text
	if len(chars) == 0 {
		return ""
	}
	if *budget <= 0 {
		return ""
	}
	if len(chars) > *budget {
		chars = chars[:*budget]
	}
	*budget -= len(chars)

	// Reading order: top to bottom, then left to right. SliceStable keeps the
	// producer's own order for glyphs the geometry cannot separate (fonts with
	// no Widths never advance X, so every glyph shares a coordinate).
	sort.SliceStable(chars, func(i, j int) bool {
		if chars[i].Y != chars[j].Y {
			return chars[i].Y > chars[j].Y
		}
		return chars[i].X < chars[j].X
	})

	var paras []string
	var lines []string
	var line strings.Builder

	flushLine := func() {
		if s := mdutil.Collapse(line.String()); s != "" {
			lines = append(lines, s)
		}
		line.Reset()
	}
	flushPara := func() {
		flushLine()
		if len(lines) > 0 {
			paras = append(paras, strings.Join(lines, "\n"))
			lines = nil
		}
	}

	var prev pdf.Text
	var prevEnd float64
	first := true
	for _, ch := range chars {
		size := ch.FontSize
		if size <= 0 {
			size = prev.FontSize
		}
		if size <= 0 {
			size = 1
		}
		if first {
			first = false
		} else if ch.Y != prev.Y {
			// New line. A gap much larger than a line height reads as a
			// paragraph break; anything smaller is just the next line.
			if prev.Y-ch.Y > 1.8*size {
				flushPara()
			} else {
				flushLine()
			}
			prevEnd = ch.X
		} else if ch.X-prevEnd > 0.25*size {
			// Same line, but the pen jumped: that jump was a space.
			line.WriteString(" ")
		}
		line.WriteString(ch.S)
		prev = ch
		if end := ch.X + ch.W; end > prevEnd {
			prevEnd = end
		}
	}
	flushPara()

	return strings.Join(paras, "\n\n")
}

// --- Scanned pages: reading the page image with a vision model -------------
//
// A scanned PDF has no text layer at all: every page is one large embedded
// image XObject. anymd cannot rasterize a page — there is no pure-Go PDF
// renderer, and a cgo one (pdfium, MuPDF) would break this package's central
// promise — but it does not have to. The pixels a scanner produced are already
// sitting in the file as a JPEG; we can lift that image straight out of the
// object graph and hand it to the caller's Describer.
//
// This is strictly opt-in: with no Describer, not a byte of this code runs and
// a text-free PDF still returns ErrNoTextLayer exactly as before.

// maxPDFCaptionedPages bounds how many pages of one document we will send to a
// Describer. Every caption is a network call, so a 500-page scan must not turn
// one Convert into 500 requests; past the cap we stop and say so in the output.
const maxPDFCaptionedPages = 20

// minPDFImageBytes skips images too small to be a scanned page. A scan of a
// sheet of paper is tens of kilobytes at minimum; anything under this is a
// logo, a rule, a signature stamp or a compression artifact, and captioning it
// costs a request to be told "a small blue square".
const minPDFImageBytes = 4096

// minPDFImageEdge is the same filter in pixels: a page scan is never 64px on a
// side, so a small icon is rejected even if its encoding happens to be bulky.
const minPDFImageEdge = 64

// maxPDFImageBytes bounds a single extracted image, and maxPDFImageTotalBytes
// bounds every image we hold for one document. Both are hard allocation caps —
// the file's own length fields are attacker-controlled and are never trusted to
// size a buffer.
const (
	maxPDFImageBytes      = 32 << 20
	maxPDFImageTotalBytes = 128 << 20
)

// maxPDFScannedImages bounds how many image objects we will index for one
// document, so a file made of a million tiny image objects stays linear.
const maxPDFScannedImages = 512

// maxPDFImagePixels bounds the pixel count we will allocate when re-encoding
// raw samples to PNG.
const maxPDFImagePixels = 50 << 20

// pdfCaptionMarker prefixes every model-written page description, so a reader
// (or a downstream index) can always tell generated prose from text that was
// actually in the document. It is one italic line, deliberately unobtrusive.
const pdfCaptionMarker = "*Page image described by a vision model:*"

// pdfCaptionCapNote is emitted once, at the end, when maxPDFCaptionedPages cut
// the document short. Silently describing the first 20 pages of a 500-page scan
// and saying nothing would be a lie by omission.
const pdfCaptionCapNote = "*Vision descriptions stopped after " +
	"20 pages; the remaining pages of this document were not described.*"

// pdfRawImage is one image XObject as it sits in the file: its declared
// geometry plus the undecoded stream bytes.
type pdfRawImage struct {
	width  int64
	height int64
	bpc    int64
	filter string // filters joined with "," in application order
	cs     string // ColorSpace, when it is a plain name
	parms  bool   // stream carries /DecodeParms (predictors etc.)
	data   []byte
	used   bool
}

// pdfPageImage is one decoded image from a page, ready for a Describer.
type pdfPageImage struct {
	data []byte
	mime string
}

// pdfImageIndex is every image object in the document, in file order.
type pdfImageIndex struct {
	images []*pdfRawImage
}

// pdfPageImages returns the decodable images referenced by one page's resource
// dictionary, as (bytes, mime) pairs ready for a Describer.
//
// The page's /Resources /XObject dictionary is read through the pdf package,
// which resolves inheritance and indirect references; the stream bytes come
// from the raw index, because the pdf package's Value.Reader panics on the
// filters that matter here (it supports Flate and ASCII85 only, and a scan is
// DCTDecode). The two halves are joined on the image's declared geometry.
func (idx *pdfImageIndex) pageImages(p pdf.Page) (out []pdfPageImage) {
	defer func() {
		// Resource resolution walks attacker-controlled pointers; a bad page
		// yields no images rather than killing the document.
		if recover() != nil {
			out = nil
		}
	}()
	if idx == nil || len(idx.images) == 0 || p.V.IsNull() {
		return nil
	}
	xo := p.Resources().Key("XObject")
	if xo.Kind() != pdf.Dict {
		return nil
	}
	for _, name := range xo.Keys() { // Keys is sorted: deterministic output
		v := xo.Key(name)
		if v.Kind() != pdf.Stream || v.Key("Subtype").Name() != "Image" {
			continue
		}
		raw := idx.take(v.Key("Width").Int64(), v.Key("Height").Int64(),
			v.Key("BitsPerComponent").Int64(), v.Key("Length").Int64(),
			pdfFilterOf(v))
		if raw == nil {
			continue
		}
		if b, mime, ok := pdfDecodeImage(raw); ok {
			out = append(out, pdfPageImage{data: b, mime: mime})
		}
	}
	return out
}

// take finds the unconsumed raw image matching a page resource's geometry and
// marks it used, so two pages holding same-sized scans get their own image
// rather than both getting the first one.
func (idx *pdfImageIndex) take(w, h, bpc, length int64, filter string) *pdfRawImage {
	for _, im := range idx.images {
		if im.used || im.width != w || im.height != h || im.filter != filter {
			continue
		}
		if bpc != 0 && im.bpc != 0 && bpc != im.bpc {
			continue
		}
		// The raw length is recovered from the stream itself and may differ
		// from the declared /Length by the end-of-line before `endstream`.
		if length > 0 {
			if d := length - int64(len(im.data)); d > 2 || d < -2 {
				continue
			}
		}
		im.used = true
		return im
	}
	return nil
}

// pdfFilterOf renders a stream's /Filter as a comma-joined chain, matching how
// pdfScanImages renders the same entry out of the raw bytes.
func pdfFilterOf(v pdf.Value) string {
	f := v.Key("Filter")
	switch f.Kind() {
	case pdf.Name:
		return f.Name()
	case pdf.Array:
		names := make([]string, 0, f.Len())
		for i := 0; i < f.Len(); i++ {
			names = append(names, f.Index(i).Name())
		}
		return strings.Join(names, ",")
	}
	return ""
}

// pdfDecodeImage turns one raw image stream into bytes a vision model can read.
//
// Supported, in the order they actually occur in scans:
//
//	DCTDecode  — the stream bytes ARE a JPEG; emitted verbatim. This is the
//	             overwhelmingly common case and costs nothing.
//	JPXDecode  — JPEG 2000, passed through as image/jp2 for the model to try.
//	FlateDecode/none with 8-bit DeviceRGB or DeviceGray — raw samples, re-encoded
//	             to PNG with the standard library.
//
// Everything else is skipped, never guessed at: CCITTFaxDecode (fax-coded
// bilevel scans), JBIG2Decode, LZWDecode, RunLengthDecode, indexed and CMYK
// colorspaces, and any bit depth other than 8. A skipped image is not an error;
// the page simply produces no caption.
func pdfDecodeImage(im *pdfRawImage) (data []byte, mime string, ok bool) {
	if im == nil || len(im.data) < minPDFImageBytes ||
		im.width < minPDFImageEdge || im.height < minPDFImageEdge {
		return nil, "", false
	}
	switch im.filter {
	case "DCTDecode":
		return im.data, "image/jpeg", true
	case "JPXDecode":
		return im.data, "image/jp2", true
	case "FlateDecode", "":
		if im.parms {
			return nil, "", false // a predictor we are not going to guess at
		}
		samples := im.data
		if im.filter == "FlateDecode" {
			var err error
			if samples, err = pdfInflate(im.data); err != nil {
				return nil, "", false
			}
		}
		return pdfSamplesToPNG(samples, im.width, im.height, im.bpc, im.cs)
	}
	return nil, "", false
}

// pdfInflate decompresses a FlateDecode stream under a hard output cap, so a
// zip bomb costs one bounded allocation instead of the machine's memory.
func pdfInflate(b []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, maxPDFImageBytes+1))
	if err != nil && len(out) == 0 {
		return nil, err
	}
	if len(out) > maxPDFImageBytes {
		return nil, errors.New("inflated image too large")
	}
	return out, nil
}

// pdfSamplesToPNG re-encodes uncompressed 8-bit RGB or grayscale samples as a
// PNG. Other bit depths and colorspaces return false rather than a wrong image:
// sending a model a mis-decoded picture is worse than sending it nothing.
func pdfSamplesToPNG(samples []byte, w, h, bpc int64, cs string) ([]byte, string, bool) {
	if bpc != 8 || w <= 0 || h <= 0 || w*h > maxPDFImagePixels {
		return nil, "", false
	}
	width, height := int(w), int(h)
	var img image.Image
	switch cs {
	case "DeviceRGB", "CalRGB":
		if int64(len(samples)) < w*h*3 {
			return nil, "", false
		}
		rgba := image.NewRGBA(image.Rect(0, 0, width, height))
		for i, n := 0, width*height; i < n; i++ {
			copy(rgba.Pix[i*4:], samples[i*3:i*3+3])
			rgba.Pix[i*4+3] = 0xFF
		}
		img = rgba
	case "DeviceGray", "CalGray":
		if int64(len(samples)) < w*h {
			return nil, "", false
		}
		gray := image.NewGray(image.Rect(0, 0, width, height))
		copy(gray.Pix, samples[:width*height])
		img = gray
	default:
		return nil, "", false
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, "", false
	}
	return buf.Bytes(), "image/png", true
}

// pdfScanImages indexes every image XObject in the file by walking the raw
// bytes object by object.
//
// It is a deliberate second, tiny parser rather than a use of the pdf package:
// Value.Reader() applies the stream's filter chain and panics on DCTDecode, so
// the one encoding that carries a scanned page is exactly the one the library
// cannot hand back. Walking `N G obj … stream … endstream` gets the bytes
// untouched, and skipping past each `endstream` keeps the scan from ever
// interpreting binary image data as PDF syntax.
func pdfScanImages(b []byte) *pdfImageIndex {
	idx := &pdfImageIndex{}
	total := 0
	for i := 0; i+3 <= len(b); {
		j := bytes.Index(b[i:], []byte("obj"))
		if j < 0 {
			break
		}
		pos := i + j
		if !pdfObjHeaderBefore(b, pos) {
			i = pos + 3
			continue
		}
		body := b[pos+3:]
		sIdx := bytes.Index(body, []byte("stream"))
		eIdx := bytes.Index(body, []byte("endobj"))
		if sIdx < 0 || (eIdx >= 0 && eIdx < sIdx) {
			i = pos + 3 // no stream in this object
			continue
		}
		dict := body[:sIdx]
		start := sIdx + len("stream")
		if start < len(body) && body[start] == '\r' {
			start++
		}
		if start < len(body) && body[start] == '\n' {
			start++
		}
		end := bytes.Index(body[start:], []byte("endstream"))
		if end < 0 {
			break // truncated: nothing further is trustworthy
		}
		end += start
		i = pos + 3 + end + len("endstream") // skip the payload wholesale

		if !bytes.Contains(dict, []byte("/Image")) || len(idx.images) >= maxPDFScannedImages {
			continue
		}
		data := body[start:end]
		// Trim the end-of-line the writer put before `endstream`; a declared
		// /Length, when it is a direct integer we can trust, is authoritative.
		if n := pdfDictInt(dict, "Length"); n > 0 && n <= int64(len(data)) {
			data = data[:n]
		} else {
			data = bytes.TrimRight(data, "\r\n")
		}
		if len(data) > maxPDFImageBytes || total+len(data) > maxPDFImageTotalBytes {
			continue
		}
		total += len(data)
		idx.images = append(idx.images, &pdfRawImage{
			width:  pdfDictInt(dict, "Width"),
			height: pdfDictInt(dict, "Height"),
			bpc:    pdfDictInt(dict, "BitsPerComponent"),
			filter: strings.Join(pdfDictFilters(dict), ","),
			cs:     pdfDictName(dict, "ColorSpace"),
			parms:  bytes.Contains(dict, []byte("/DecodeParms")),
			data:   data,
		})
	}
	return idx
}

// pdfObjHeaderBefore reports whether the `obj` keyword at pos is a real object
// header — `<num> <gen> obj` — and not the tail of some other token.
func pdfObjHeaderBefore(b []byte, pos int) bool {
	if pos+3 < len(b) && isPDFAlnum(b[pos+3]) {
		return false
	}
	i := pos
	back := func(pred func(byte) bool) bool {
		start := i
		for i > 0 && pred(b[i-1]) {
			i--
		}
		return i < start
	}
	return back(isPDFSpace) && back(isPDFDigit) && back(isPDFSpace) && back(isPDFDigit)
}

func isPDFDigit(c byte) bool { return c >= '0' && c <= '9' }
func isPDFSpace(c byte) bool {
	return c == ' ' || c == '\n' || c == '\r' || c == '\t' || c == '\f' || c == 0
}
func isPDFAlnum(c byte) bool {
	return isPDFDigit(c) || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// pdfDictKeyValue returns the bytes just after /key in a stream's header
// dictionary, or nil when the key is absent.
func pdfDictKeyValue(dict []byte, key string) []byte {
	k := []byte("/" + key)
	for off := 0; ; {
		j := bytes.Index(dict[off:], k)
		if j < 0 {
			return nil
		}
		at := off + j + len(k)
		if at < len(dict) && isPDFAlnum(dict[at]) {
			off = at // /Length1 is not /Length
			continue
		}
		for at < len(dict) && isPDFSpace(dict[at]) {
			at++
		}
		return dict[at:]
	}
}

// pdfDictInt reads a direct integer entry. An indirect reference (`5 0 R`)
// reads as 0, which every caller treats as "unknown" rather than as a value.
func pdfDictInt(dict []byte, key string) int64 {
	v := pdfDictKeyValue(dict, key)
	end := 0
	for end < len(v) && isPDFDigit(v[end]) {
		end++
	}
	if end == 0 || end > 18 {
		return 0
	}
	// `12 0 R` is a reference, not the number 12.
	rest := v[end:]
	for len(rest) > 0 && isPDFSpace(rest[0]) {
		rest = rest[1:]
	}
	if len(rest) > 0 && isPDFDigit(rest[0]) {
		return 0
	}
	n, err := strconv.ParseInt(string(v[:end]), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// pdfDictName reads a direct name entry (`/ColorSpace /DeviceRGB`), returning
// "" for anything else — an array, a reference, or a missing key.
func pdfDictName(dict []byte, key string) string {
	v := pdfDictKeyValue(dict, key)
	if len(v) == 0 || v[0] != '/' {
		return ""
	}
	end := 1
	for end < len(v) && isPDFAlnum(v[end]) {
		end++
	}
	return string(v[1:end])
}

// pdfDictFilters reads /Filter as either one name or an array of names.
func pdfDictFilters(dict []byte) []string {
	v := pdfDictKeyValue(dict, "Filter")
	if len(v) == 0 {
		return nil
	}
	if v[0] == '/' {
		if n := pdfDictName(dict, "Filter"); n != "" {
			return []string{n}
		}
		return nil
	}
	if v[0] != '[' {
		return nil
	}
	var names []string
	for i := 1; i < len(v) && len(names) < 8; {
		switch {
		case v[i] == ']':
			return names
		case isPDFSpace(v[i]):
			i++
		case v[i] == '/':
			j := i + 1
			for j < len(v) && isPDFAlnum(v[j]) {
				j++
			}
			names = append(names, string(v[i+1:j]))
			i = j
		default:
			return names
		}
	}
	return names
}
