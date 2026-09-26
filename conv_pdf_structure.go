package anymd

// Structure recovery for PDF.
//
// A PDF has no paragraphs, no headings, no lists and no tables. It has glyphs
// with coordinates, and every structure a reader sees on the page is something
// the reader infers from where those glyphs landed. Emitting the text in
// reading order — which is all this converter used to do — throws that
// inference away: a table becomes space-separated prose, a heading becomes a
// short paragraph, and a bulleted list becomes a sequence of orphan sentences.
// For a Markdown target, which exists to carry exactly that structure, this is
// the whole of the job rather than a refinement of it.
//
// Everything here is deterministic geometry over the text layer. No model is
// consulted, nothing is generated: every character emitted came out of the
// PDF, and the analysis only decides where the characters go. That matters for
// tables especially — a cell is filled from the glyphs that sit inside its
// column, never rewritten — so a wrong grid can misplace a value but can never
// invent one.

import (
	"sort"
	"strings"
	"unicode"

	"github.com/muthuishere/anymd/internal/mdutil"
	"github.com/muthuishere/anymd/internal/pdf"
)

// pdfWord is one run of glyphs with no internal gap wide enough to be a space.
// Its edges are what the borderless-table pass aligns into column tracks.
type pdfWord struct {
	s      string
	x0, x1 float64
}

// pdfTextLine is one assembled line of a page: the text a reader sees on one
// baseline, plus the geometry and font statistics the classifiers need.
type pdfTextLine struct {
	words  []pdfWord
	text   string
	x0, x1 float64
	y      float64
	size   float64 // the dominant font size on the line
	bold   bool    // every glyph came from a font whose name says bold
	col    int     // which reading-order run (column) the line came from

	glyphs     int // glyphs kept on this line
	badGlyphs  int // of those, ones that decoded to nothing readable
	overprints int // duplicates dropped as simulated bold
}

// pdfPage is one page's assembled lines plus the page geometry the furniture
// pass needs to know what counts as a margin.
type pdfPage struct {
	lines     []pdfTextLine
	top, bot  float64
	furniture map[int]bool // line index -> suppressed as a repeated header/footer
	rects     []pdf.Rect
	hasRuling bool
	// glyphs, badGlyphs and overprints are the text-layer soundness counts:
	// how many glyphs the page drew, how many of them decoded to nothing a
	// reader can use, and how many were duplicates painted on top of others.
	glyphs     int
	badGlyphs  int
	overprints int
	// imageRefs is every XObject the page actually painted, in the order the
	// content stream painted them.
	imageRefs []pdf.ImageRef
	// images is those refs resolved to bytes, filled in later and only when
	// the caller wants images at all.
	images []pdfPlacedImage
}

// --- tuning constants ------------------------------------------------------

const (
	// pdfSpaceFrac is the gap, as a fraction of font size, that separates two
	// words. It matches the value the glyph loop has always used to decide
	// where to write a space, so words and rendered text cannot disagree.
	pdfSpaceFrac = 0.25

	// pdfCellGapFrac is the gap that separates two table cells on one row. It
	// is far wider than a word space: prose never leaves this much air mid-line
	// unless it is justified to a column edge, which the validators catch.
	pdfCellGapFrac = 1.2

	// pdfHeadingRatio is how much larger than the body text a line must be
	// before its size alone makes it a heading. Tagged headings in Word output
	// are 84% set at body size, so this rule is deliberately not the only one:
	// a numbered, bold, short line qualifies at body size too.
	pdfHeadingRatio = 1.15

	// pdfHeadingMaxChars caps how long a line can be and still be a heading. A
	// sentence that happens to be set large is a pull quote, not a section.
	pdfHeadingMaxChars = 150

	// pdfSentenceWords is how many words after a list marker make the line a
	// sentence rather than a numbered title. Six clears the longest headings
	// that end in a full stop while catching the shortest real list items.
	pdfSentenceWords = 6

	// pdfFurnitureBand is the fraction of page height, top and bottom, inside
	// which a repeated line is treated as a running header or footer. It is
	// measured from the edge of the paper, so it is the printing margin: a
	// running head sits within about an inch of the edge. The band is
	// deliberately generous rather than tight: a footer can sit well inside
	// the page, and narrowing the band to exclude body text loses those
	// instead. What keeps body text out is the detachment test in
	// pdfEdgeRun, not the width of the band.
	pdfFurnitureBand = 0.12

	// pdfMinTableRows is the fewest aligned rows that may be called a table.
	// Two rows of two columns is how an ordinary indented paragraph looks.
	pdfMinTableRows = 3

	// pdfMinTableCols is the fewest columns a table may have. A single column
	// of aligned lines is a list or a paragraph.
	pdfMinTableCols = 2
)

// pdfBuildPage assembles one page's glyphs into lines, in reading order.
//
// The reading-order pass (XY-cut over column fragments) already exists and is
// reused unchanged; this adds the step after it, turning each run's glyphs into
// lines whose words, edges and font statistics survive into the classifiers.
func pdfBuildPage(p pdf.Page, budget *int) (pg pdfPage, ok bool) {
	defer func() {
		if recover() != nil {
			pg, ok = pdfPage{}, false
		}
	}()

	if p.V.IsNull() || p.V.Key("Contents").IsNull() {
		return pdfPage{}, false
	}
	content := p.Content()
	chars := content.Text
	if len(chars) == 0 || *budget <= 0 {
		return pdfPage{}, false
	}
	if len(chars) > *budget {
		chars = chars[:*budget]
	}
	*budget -= len(chars)

	sort.SliceStable(chars, func(i, j int) bool {
		if chars[i].Y != chars[j].Y {
			return chars[i].Y > chars[j].Y
		}
		return chars[i].X < chars[j].X
	})

	pg.rects = content.Rect
	pg.hasRuling = pdfHasRuling(content.Rect)
	pg.imageRefs = content.Image
	pg.top, pg.bot = pdfPageBounds(p, chars)

	for col, run := range pdfReadingOrder(chars) {
		for _, l := range pdfAssembleLines(run) {
			l.col = col
			pg.glyphs += l.glyphs
			pg.badGlyphs += l.badGlyphs
			pg.overprints += l.overprints
			pg.lines = append(pg.lines, l)
		}
	}
	pg.furniture = map[int]bool{}
	return pg, len(pg.lines) > 0
}

// pdfPageBounds returns the page's vertical extent, from its MediaBox.
//
// It has to be the declared page, not the ink. A margin is measured from the
// edge of the paper, and the whole idea of a "top band" is the strip of paper
// a running header is printed in. Measured against the ink instead, the band
// is relative to wherever the text happens to start and stop — so the last
// line of body text on any page is, by construction, at distance zero from the
// "bottom" of the page and lands in the footer band every time.
//
// The ink extent is the fallback for a page whose MediaBox is missing or
// nonsensical, where a band relative to the text is still better than none.
func pdfPageBounds(p pdf.Page, chars []pdf.Text) (top, bot float64) {
	if box := pdfMediaBox(p); box != nil {
		return box[1], box[0]
	}
	if len(chars) == 0 {
		return 0, 0
	}
	top, bot = chars[0].Y, chars[0].Y
	for _, ch := range chars {
		if ch.Y > top {
			top = ch.Y
		}
		if ch.Y < bot {
			bot = ch.Y
		}
	}
	return top, bot
}

// pdfMediaBox returns {bottom, top} from the page's MediaBox, walking up the
// page tree because the attribute is inheritable and most producers set it once
// on the root rather than on every page.
func pdfMediaBox(p pdf.Page) []float64 {
	for v := p.V; !v.IsNull(); v = v.Key("Parent") {
		box := v.Key("MediaBox")
		if box.Kind() != pdf.Array || box.Len() != 4 {
			continue
		}
		lo, hi := box.Index(1).Float64(), box.Index(3).Float64()
		if hi < lo {
			lo, hi = hi, lo
		}
		if hi-lo > 1 && hi-lo < 1e6 {
			return []float64{lo, hi}
		}
	}
	return nil
}

// pdfAssembleLines groups one run's glyphs into lines and words.
//
// Glyphs arrive sorted top-to-bottom then left-to-right, so a line ends when
// the baseline steps down by more than a fraction of the font size, or when the
// pen jumps backwards — the signature of a new line whose first glyph happens
// to share a rounded baseline with the last one.
func pdfAssembleLines(chars []pdf.Text) []pdfTextLine {
	var out []pdfTextLine
	var cur []pdf.Text
	flush := func() {
		if l, ok := pdfMakeLine(cur); ok {
			out = append(out, l)
		}
		cur = nil
	}
	var prevY, prevEnd float64
	for i, ch := range chars {
		size := ch.FontSize
		if size <= 0 {
			size = 1
		}
		if i > 0 {
			if prevY-ch.Y > pdfLineTol*size || ch.Y-prevY > pdfLineTol*size {
				flush()
			} else if ch.X+ch.W < prevEnd-size {
				flush()
			}
		}
		cur = append(cur, ch)
		prevY = ch.Y
		if e := ch.X + ch.W; e > prevEnd || len(cur) == 1 {
			prevEnd = e
		}
	}
	flush()
	return out
}

// pdfMakeLine turns one baseline's glyphs into a line, splitting it into words
// on the same gap rule the renderer uses for spaces.
func pdfMakeLine(chars []pdf.Text) (pdfTextLine, bool) {
	var l pdfTextLine
	var w pdfWord
	var b strings.Builder
	var prevEnd float64
	sizes := map[float64]int{}
	bold, glyphs := 0, 0

	flushWord := func() {
		if s := strings.TrimSpace(w.s); s != "" {
			w.s = s
			l.words = append(l.words, w)
		}
		w = pdfWord{}
	}

	var kept pdf.Text
	var haveKept bool
	for i, ch := range chars {
		size := ch.FontSize
		if size <= 0 {
			size = 1
		}
		// Simulated bold: the same glyph painted twice a hair apart, which is
		// how a document asks for a weight its font does not have. Both
		// paintings are real glyphs in the text layer, so taking them at face
		// value spells "Important" as "IImmppoorrttaanntt".
		//
		// The test is position, not sequence: a genuine double letter sits a
		// full advance away, while an overprint sits within a rounding error
		// of the glyph it is thickening.
		if haveKept && ch.S == kept.S && ch.Y == kept.Y &&
			pdfAbs(ch.X-kept.X) < pdfOverprintFrac*size {
			l.overprints++
			continue
		}
		if pdfIsBlank(ch.S) {
			flushWord()
			if i > 0 {
				b.WriteString(" ")
			}
			prevEnd = ch.X + ch.W
			continue
		}
		if i > 0 && ch.X-prevEnd > pdfSpaceFrac*size {
			flushWord()
			b.WriteString(" ")
		}
		if w.s == "" {
			w.x0 = ch.X
		}
		w.s += ch.S
		w.x1 = ch.X + ch.W
		b.WriteString(ch.S)
		if end := ch.X + ch.W; end > prevEnd {
			prevEnd = end
		}
		kept, haveKept = ch, true
		sizes[size]++
		glyphs++
		l.glyphs++
		if pdfUnreadable(ch.S) {
			l.badGlyphs++
		}
		if pdfFontIsBold(ch.Font) {
			bold++
		}
		if l.y == 0 || ch.Y > l.y {
			l.y = ch.Y
		}
	}
	flushWord()

	l.text = mdutil.Collapse(b.String())
	if l.text == "" || len(l.words) == 0 {
		return pdfTextLine{}, false
	}
	l.x0, l.x1 = l.words[0].x0, l.words[len(l.words)-1].x1
	for _, wd := range l.words {
		if wd.x0 < l.x0 {
			l.x0 = wd.x0
		}
		if wd.x1 > l.x1 {
			l.x1 = wd.x1
		}
	}
	best := 0
	for size, n := range sizes {
		if n > best || (n == best && size > l.size) {
			best, l.size = n, size
		}
	}
	l.bold = glyphs > 0 && bold == glyphs
	return l, true
}

// pdfOverprintFrac is how close, as a fraction of font size, two identical
// glyphs must be before the second is treated as an overprint rather than as
// text. It is deliberately tight: a real double letter is a whole advance away,
// so there is a wide margin between the two cases.
const pdfOverprintFrac = 0.2

// pdfAbs is math.Abs without the import.
func pdfAbs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// pdfUnreadable reports whether a decoded glyph carries no meaning a reader
// could recover: the replacement character, a private-use code point, or a
// stray control code.
//
// These are what a broken or absent ToUnicode map produces. The text still
// arrives, in the right order and the right places, and looks like a
// successful extraction — which is exactly why it has to be counted. A
// document that comes back as fluent-looking rubbish is worse than one that
// comes back as an error.
func pdfUnreadable(s string) bool {
	for _, r := range s {
		switch {
		case r == 0xFFFD:
			return true
		case r >= 0xE000 && r <= 0xF8FF:
			// Private use. Word writes its list bullets here, so a handful per
			// page is normal and only the overall share is meaningful.
			return true
		case unicode.IsControl(r) && !unicode.IsSpace(r):
			return true
		}
	}
	return false
}

// pdfFontIsBold reports whether a font's name claims weight. The name is the
// only weight signal a text layer carries — a simple font dictionary has no
// /FontWeight — so this is a hint, never a decision on its own.
func pdfFontIsBold(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "bold") || strings.Contains(n, "black") ||
		strings.Contains(n, "heavy") || strings.Contains(n, "semib")
}

// pdfHasRuling reports whether the page draws rules: rectangles thin enough in
// one dimension to be lines rather than boxes. Word and Excel exports draw cell
// borders this way, as thin filled rectangles rather than strokes, which is why
// counting stroked segments alone misses most ruled tables.
func pdfHasRuling(rects []pdf.Rect) bool {
	n := 0
	for _, r := range rects {
		w := r.Max.X - r.Min.X
		h := r.Max.Y - r.Min.Y
		if (h <= 2 && w >= 20) || (w <= 2 && h >= 10) {
			n++
		}
	}
	return n >= 3 || pdfHasShadedBand(rects)
}

// pdfHasShadedBand reports a row of filled cells sharing one horizontal band —
// a shaded header, which is a table's other way of drawing itself.
//
// A rule is a thin rectangle, and looking only for those misses every document
// whose borders are stroked paths rather than filled boxes: LibreOffice and
// Word both emit a bordered table that way, so a perfectly ordinary report
// reaches the table pass with no ruling evidence at all. The shading is still
// there, though, because a header band is a fill, and three or more fills
// standing side by side in the same band are a header row and nothing else.
//
// Three, not pdfMinTableCols: two boxes abreast are a diagram as often as they
// are a table, and this is the same bar the thin-rule count above sets.
func pdfHasShadedBand(rects []pdf.Rect) bool {
	bands := map[int]int{}
	for _, r := range rects {
		w := r.Max.X - r.Min.X
		h := r.Max.Y - r.Min.Y
		// A cell, not a rule, not the page: wide enough to hold a word, short
		// enough to be one row, and not the full width of the sheet.
		if w < 15 || w > 400 || h < 4 || h > 60 {
			continue
		}
		// Bands are keyed on the rounded top edge, so cells that differ by a
		// fraction of a point — which they do, once a border width is added
		// to one of them — still land together.
		bands[int(r.Max.Y+0.5)]++
	}
	for _, n := range bands {
		if n >= 3 {
			return true
		}
	}
	return false
}
