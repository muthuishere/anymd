package anymd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
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
// that text is out of scope; the caller must decide whether to run OCR itself.
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

	ra, size, err := pdfReaderAt(r)
	if err != nil {
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
	for i := 1; i <= n; i++ {
		page := doc.Page(i)
		if page.V.IsNull() {
			continue
		}
		text := pdfPageText(page, &budget)
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
