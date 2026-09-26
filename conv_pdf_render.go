package anymd

// Turning classified PDF lines into Markdown.
//
// The order of the passes is load-bearing. Tables are found before headings,
// because a bold, short, isolated cell looks exactly like a heading until you
// know it is a cell. Furniture is stripped before either, because a running
// header is short and often bold too. Hyphen joining happens last, on text that
// is already inside its final block, so a word broken across a column break is
// never stitched to the wrong continuation.

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/muthuishere/anymd/internal/mdutil"
	"github.com/muthuishere/anymd/internal/pdf"
)

// pdfRenderDoc turns every page into Markdown, with the document-wide passes
// (furniture, hyphen witnesses) resolved first.
// pdfDoc is a rendered document: the Markdown blocks of each page, plus the
// page boundaries a table runs straight through.
type pdfDoc struct {
	pages [][]string
	// joined[i] is true when page i continues page i-1 without a break,
	// because a table spans the two. A `---` there would split one table into
	// two, and Markdown has no way to resume a table after a rule.
	joined []bool
}

// pdfOutlineMaxEntries bounds how much of a document's outline is read. The
// tree comes from the file and a malformed one can be made to loop.
const pdfOutlineMaxEntries = 4096

// pdfOutlineLevels flattens a document outline into normalised title -> depth.
//
// The outline is the author's own table of contents: it names the headings and
// says how they nest, which is precisely what the visual rules have to infer.
// It is used to settle the LEVEL of a heading the page already supports, and
// to recognise a short line whose text matches a title exactly — never to
// invent a heading somewhere the page shows none, because an outline entry can
// point anywhere and a heading conjured out of nothing is worse than a missing
// one.
// A depth of 0 in the returned map means "this line is a heading, but the
// outline does not say at what level" — which is what a flat outline, listing
// every heading as a top-level bookmark, actually tells you.
func pdfOutlineLevels(root pdf.Outline) map[string]int {
	out := map[string]int{}
	deepest := 0
	var walk func(o pdf.Outline, depth int)
	walk = func(o pdf.Outline, depth int) {
		if len(out) >= pdfOutlineMaxEntries || depth > 6 {
			return
		}
		if t := pdfNormalizeTitle(o.Title); t != "" {
			if depth > deepest {
				deepest = depth
			}
			// Keep the shallowest occurrence: a title repeated deeper in the
			// tree is a sub-entry of the same name, not a promotion.
			if lvl, seen := out[t]; !seen || depth < lvl {
				out[t] = depth
			}
		}
		for _, c := range o.Child {
			walk(c, depth+1)
		}
	}
	// The root carries no title of its own; its children are level one.
	walk(root, 0)
	delete(out, "")

	// A flat outline says nothing about depth. Plenty of producers emit every
	// heading as a top-level bookmark — a long licence agreement met in
	// testing lists "1. Accepting this Agreement" and "1.1 Acceptance" as
	// siblings — and taking that at face value flattens a hierarchy the
	// numbering states plainly. Keep the recognition, drop the depths.
	if deepest <= 1 {
		for t := range out {
			out[t] = 0
		}
	}
	return out
}

// pdfNormalizeTitle reduces a heading or outline title to the form the two are
// compared in: case-folded, whitespace collapsed, and stripped of the
// punctuation a producer adds on one side but not the other.
func pdfNormalizeTitle(s string) string {
	s = mdutil.Collapse(s)
	s = strings.TrimSpace(s)
	s = strings.Trim(s, ".:;,\u2013\u2014-")
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func pdfRenderDoc(pages []pdfPage, images *imageBudget, opts *Options, outline map[string]int) pdfDoc {
	// Tables are located before anything else, on the raw lines, because two
	// later passes are wrong without knowing where they are.
	tables := make([]map[int]pdfTable, len(pages))
	inTable := make([]map[int]bool, len(pages))
	for i := range pages {
		tables[i] = pdfFindTables(pages[i].lines, pages[i].hasRuling)
		inTable[i] = map[int]bool{}
		for at, t := range tables[i] {
			for k := at; k < at+t.rows && k < len(pages[i].lines); k++ {
				inTable[i][k] = true
			}
		}
	}

	// The body size is needed before furniture is marked, because what ends a
	// masthead is the document's own body type appearing.
	var all []pdfTextLine
	for i, pg := range pages {
		for j, l := range pg.lines {
			if !inTable[i][j] {
				all = append(all, l)
			}
		}
	}
	if len(all) == 0 {
		for _, pg := range pages {
			all = append(all, pg.lines...)
		}
	}
	body := pdfBodySize(all)

	pdfMarkFurniture(pages, inTable, body)
	witness := pdfHyphenWitnesses(pages)

	// The body size is measured once for the whole document, not per page. A
	// page that happens to be all table, or all title, has no body text of its
	// own, and measuring it in isolation makes its own largest size the
	// baseline — so the same heading is an H1 on one page and an H3 on the
	// next.
	//
	// Table rows were left out of that measurement. A table is usually set
	// smaller than the prose around it, and a long one holds far more
	// characters than the prose does, so counting its rows makes the table's
	// own size "the body" — and then every real paragraph on the page is
	// larger than the body and becomes a heading.
	top := pdfHeadingOffset(all)

	doc := pdfDoc{pages: make([][]string, 0, len(pages)), joined: make([]bool, len(pages))}
	for i := range pages {
		doc.pages = append(doc.pages, pdfRenderPage(pages[i], body, top, witness, images, opts, outline))
	}
	pdfJoinPageTables(&doc)
	return doc
}

// pdfJoinPageTables stitches a table that runs across a page break back into
// one table.
//
// A table longer than a page is not two tables, but rendering page by page
// produces two — with a horizontal rule between them, which in Markdown is not
// something a table can resume after. The continuation also arrives carrying
// its own header row: the delimiter line makes Markdown read that first row as
// a header, so the pieces after the first would each lose a row of data to a
// heading that is really a repeat of the original.
func pdfJoinPageTables(doc *pdfDoc) {
	// prevAt tracks the page that still holds the growing table. A merge
	// empties the page it consumed, so the next page must attach to the last
	// page that still has blocks — otherwise a table crossing three pages
	// joins the first two and leaves the third stranded.
	prevAt := 0
	for i := 1; i < len(doc.pages); i++ {
		cur := doc.pages[i]
		if len(doc.pages[prevAt]) == 0 {
			prevAt = i
			continue
		}
		if len(cur) == 0 {
			continue
		}
		prev := doc.pages[prevAt]
		tail, head := prev[len(prev)-1], cur[0]
		if !pdfIsTableBlock(tail) || !pdfIsTableBlock(head) {
			prevAt = i
			continue
		}
		tailRows, headRows := strings.Split(tail, "\n"), strings.Split(head, "\n")
		if len(headRows) < 2 || pdfPipeCount(tailRows[0]) != pdfPipeCount(headRows[0]) {
			prevAt = i
			continue
		}
		// Drop the continuation's own header and delimiter. When the producer
		// repeated the header row, that row is the repeat and goes with them;
		// when it did not, the first row is data and only the delimiter goes.
		body := headRows[2:]
		if headRows[0] != tailRows[0] {
			body = append([]string{headRows[0]}, body...)
		}
		if len(body) == 0 {
			prevAt = i
			continue
		}
		doc.pages[prevAt][len(prev)-1] = tail + "\n" + strings.Join(body, "\n")
		doc.pages[i] = cur[1:]
		doc.joined[i] = true
		if len(doc.pages[i]) > 0 {
			prevAt = i
		}
	}
}

// pdfIsTableBlock reports whether a rendered block is a pipe table.
func pdfIsTableBlock(b string) bool {
	return strings.HasPrefix(b, "| ") && strings.Contains(b, "\n| --- |")
}

// pdfPipeCount is a table row's column count, used to tell whether two pieces
// belong to the same table.
func pdfPipeCount(row string) int { return strings.Count(row, "|") }

// pdfRenderPage emits one page.
func pdfRenderPage(pg pdfPage, body, top float64, witness map[string]bool, images *imageBudget, opts *Options, outline map[string]int) []string {
	lines := make([]pdfTextLine, 0, len(pg.lines))
	for i, l := range pg.lines {
		if pg.furniture[i] {
			continue
		}
		lines = append(lines, l)
	}
	if len(lines) == 0 {
		return nil
	}

	tables := pdfFindTables(lines, pg.hasRuling)
	leading := pdfLeading(lines)
	figures := pdfPlaceFigures(pg, lines, images, opts)

	var blocks []string
	var para []string
	flush := func() {
		if len(para) > 0 {
			blocks = append(blocks, pdfJoinWrapped(para, witness))
			para = nil
		}
	}

	for i := 0; i < len(lines); {
		if fig := figures[i]; len(fig) > 0 {
			flush()
			blocks = append(blocks, fig...)
		}
		if t, ok := tables[i]; ok {
			flush()
			blocks = append(blocks, pdfRenderTable(lines[i:i+t.rows], t))
			i += t.rows
			continue
		}
		l := lines[i]

		// A bullet glyph set on its own — its own text frame, as PowerPoint
		// and many DTP engines emit it — belongs to the line that follows.
		if marker, ok := pdfBulletOnly(l); ok && i+1 < len(lines) {
			flush()
			blocks = append(blocks, marker+" "+pdfJoinWrapped([]string{lines[i+1].text}, witness))
			i += 2
			continue
		}
		if lvl, ok := pdfHeadingLevel(l, body, top, lines, i, outline); ok {
			flush()
			blocks = append(blocks, strings.Repeat("#", lvl)+" "+l.text)
			i++
			continue
		}
		if item, ok := pdfListItem(l); ok {
			flush()
			// Consecutive items are emitted as one block so the list is tight:
			// separating them by blank lines makes every item its own loose
			// paragraph, and a reader of the Markdown loses the list.
			base := l.x0
			items := []string{item}
			indents := []float64{l.x0}
			for i+1 < len(lines) {
				if _, isHead := pdfHeadingLevel(lines[i+1], body, top, lines, i+1, outline); isHead {
					break
				}
				nx, ok := pdfListItem(lines[i+1])
				if !ok {
					break
				}
				items = append(items, nx)
				indents = append(indents, lines[i+1].x0)
				if lines[i+1].x0 < base {
					base = lines[i+1].x0
				}
				i++
			}
			// Indentation is the only record a PDF keeps of a nested list, so
			// it is measured against the shallowest item in the run and
			// rendered back as Markdown nesting.
			step := l.size * 1.5
			if step <= 0 {
				step = 1
			}
			for k := range items {
				if d := int((indents[k] - base) / step); d > 0 {
					if d > 4 {
						d = 4
					}
					items[k] = strings.Repeat("    ", d) + items[k]
				}
			}
			blocks = append(blocks, strings.Join(items, "\n"))
			i++
			continue
		}
		para = append(para, l.text)
		// A wide vertical gap, or a short line that does not reach the right
		// edge the block otherwise holds, ends the paragraph.
		if i+1 >= len(lines) || pdfBreaksParagraph(lines, i, leading) {
			flush()
		}
		i++
	}
	flush()
	// Anything drawn below the last line of text — a figure at the foot of the
	// page, or the whole content of a page that is one picture.
	blocks = append(blocks, figures[len(lines)]...)
	return blocks
}

// pdfPlaceFigures renders each image the page painted and files it under the
// index of the line it belongs above, so it lands in the reading order rather
// than in a heap at the end.
//
// Anything without a text layer under it — a full-page scan — is handled
// elsewhere; this is the ordinary case of a figure sitting in a document.
func pdfPlaceFigures(pg pdfPage, lines []pdfTextLine, images *imageBudget, opts *Options) map[int][]string {
	out := map[int][]string{}
	if len(pg.images) == 0 {
		return out
	}
	captions := 0
	for _, im := range pg.images {
		uri := images.dataURI(im.data, im.mime)
		hint := ""
		if uri == "" && !opts.HasDescriber() {
			// Nothing to show and nothing to say: a decorative rule or a
			// payload over budget, with no reader for it either way.
			continue
		}
		alt := ""
		// The captioning floor, which is far above the inlining one: a model
		// call to be told "a small blue square" is a call wasted.
		if opts.HasDescriber() && captions < maxPDFCaptionedPages && len(im.data) >= minPDFImageBytes {
			if c := describeImageWithHint(im.data, im.mime, hint, opts); c != "" {
				alt = c
				captions++
			}
		}
		// The first line whose baseline sits below the image's top edge is the
		// line the image comes before.
		at := len(lines)
		for i, l := range lines {
			if l.y < im.rect.Max.Y {
				at = i
				break
			}
		}
		out[at] = append(out[at], markdownImage(alt, uri))
	}
	return out
}

// pdfLeading is the page's prevailing line spacing: the median gap between
// consecutive baselines in the same column.
//
// A paragraph break has to be measured against this rather than against the
// font size. The two are not proportional — a document sets its leading and its
// paragraph spacing independently — so a fixed multiple of the font size is
// either too eager on a tightly-led page or, as it was here, too reluctant on a
// loosely-led one, silently running consecutive paragraphs together.
func pdfLeading(lines []pdfTextLine) float64 {
	var gaps []float64
	for i := 0; i+1 < len(lines); i++ {
		if lines[i+1].col != lines[i].col {
			continue
		}
		if g := lines[i].y - lines[i+1].y; g > 0 {
			gaps = append(gaps, g)
		}
	}
	if len(gaps) == 0 {
		return 0
	}
	sort.Float64s(gaps)
	// The lower quartile, not the median: the gaps being measured are a
	// mixture of two populations — line spacing within a paragraph and the
	// larger spacing between them — and it is the smaller one that defines
	// what "the next line" looks like. On a page that is mostly headings and
	// short blocks, the median lands in the middle of the paragraph gaps and
	// the rule stops firing altogether.
	return gaps[len(gaps)/4]
}

// pdfBreaksParagraph reports whether the paragraph ends after line i.
func pdfBreaksParagraph(lines []pdfTextLine, i int, leading float64) bool {
	cur, next := lines[i], lines[i+1]
	if next.col != cur.col {
		return true
	}
	size := cur.size
	if size <= 0 {
		size = 1
	}
	// Either measure is enough. The leading-relative rule catches the ordinary
	// case; the size-relative one is the backstop for a page with too few
	// lines to have a prevailing leading at all.
	gap := cur.y - next.y
	if gap > 1.8*size {
		return true
	}
	if leading > 0 && gap > 1.35*leading {
		return true
	}
	// An indented first line of the next paragraph.
	if next.x0-cur.x0 > 2*size {
		return true
	}
	return false
}

// --- headings --------------------------------------------------------------

// pdfSectionNumber matches the numbering a document uses to mark a section:
// decimal ("2.", "1.1", "3.2.4"), roman ("IV."), or a labelled article in one
// of the forms legal and contractual documents use.
var pdfSectionNumber = regexp.MustCompile(`^(?:(?:\d+\.)*\d+\.?|[IVXLC]+\.|(?:Article|Artikel|Section|Clause|Chapter|Appendix|Annex)\s+[\dIVXLC]+\.?)(?:\s|$)`)

// pdfBodySize is the modal font size on the page: the size most glyphs are set
// in, which is the baseline every heading rule is relative to.
func pdfBodySize(lines []pdfTextLine) float64 {
	weight := map[float64]int{}
	for _, l := range lines {
		weight[l.size] += len([]rune(l.text))
	}
	best, size := 0, 0.0
	for s, w := range weight {
		if w > best || (w == best && s < size) {
			best, size = w, s
		}
	}
	if size <= 0 {
		size = 1
	}
	return size
}

// pdfHeadingOffset reports how far the document's numbered headings should be
// pushed down the outline: one level when something larger than any of them is
// set somewhere, which is the unnumbered title, and none when there is not.
//
// It is decided once for the whole document rather than per heading, because
// that is the only way the result stays a hierarchy. Judged line by line, a
// paper whose title and first-level headings are set at the same size — which
// is the reportlab and LaTeX default — pushes "1.1" down a level while leaving
// "1." where it is, and emits an H1 followed directly by an H3.
func pdfHeadingOffset(lines []pdfTextLine) float64 {
	numbered, top := 0.0, 0.0
	for _, l := range lines {
		if len([]rune(l.text)) > pdfHeadingMaxChars {
			continue
		}
		if l.size > top {
			top = l.size
		}
		if pdfSectionNumber.MatchString(l.text) && l.size > numbered {
			numbered = l.size
		}
	}
	if numbered > 0 && top > numbered {
		return numbered
	}
	return 0
}

// pdfHeadingLevel decides whether a line is a heading, and at what depth.
//
// Two independent routes qualify, because neither is sufficient alone. Size
// works for design-led documents and fails on Word output, where most tagged
// headings are set at body size and carry their rank in a number instead;
// numbering works there and fails on a paper whose sections are unnumbered.
// Boldness alone qualifies for neither: a bold lead-in like "Conditions:" is
// not a section, and promoting it splits a rule away from the text it governs.
// top is the document-wide offset from pdfHeadingOffset: the size at or below
// which a numbered heading sits one level under the title.
func pdfHeadingLevel(l pdfTextLine, body, top float64, lines []pdfTextLine, i int, outline map[string]int) (int, bool) {
	text := l.text
	if text == "" || len([]rune(text)) > pdfHeadingMaxChars {
		return 0, false
	}
	if strings.HasSuffix(text, ":") || strings.HasSuffix(text, ";") || strings.HasSuffix(text, ",") {
		return 0, false
	}
	// A line that is one sentence of several is prose, however it is set.
	if strings.Count(text, ". ") > 1 {
		return 0, false
	}

	// The outline names the headings, so a line it lists is one — even set at
	// body size, where the size rule can see nothing. Its depth is used only
	// when the outline actually nests; see pdfOutlineLevels.
	outlined, isOutlined := outline[pdfNormalizeTitle(text)]
	if isOutlined && outlined > 0 {
		return pdfClampLevel(outlined), true
	}

	num := pdfSectionNumber.FindString(text)
	big := l.size >= pdfHeadingRatio*body

	switch {
	case big:
		// Rank by how far above the body the line is set, so a title outranks
		// the sections beneath it without needing to be counted.
		lvl := 3
		switch r := l.size / body; {
		case r >= 1.8:
			lvl = 1
		case r >= 1.35:
			lvl = 2
		}
		// A section number is authored hierarchy: "1.1" is a child of "1." and
		// says so. Size is only a proxy for it, and a weak one — Word sets H1
		// and H2 a point or two apart, which rounds to the same level and
		// flattens the outline. So when a number is present it decides the
		// depth, offset by one if something larger is set above it, which is
		// the unnumbered title the numbering cannot see.
		if d := pdfNumberDepth(num); d > 0 {
			if top > 0 && l.size <= top {
				d++
			}
			lvl = d
		}
		return pdfClampLevel(lvl), true
	case num != "" && (l.bold || pdfLineIsolated(lines, i)):
		d := pdfNumberDepth(num)
		if d == 0 {
			d = 2
		} else if top > 0 && l.size <= top {
			d++
		}
		return pdfClampLevel(d), true
	case isOutlined:
		// Named by a flat outline: a heading for certain, at a level nothing
		// has stated. Numbering is the next best authority, and failing that
		// it sits one below the title.
		if d := pdfNumberDepth(num); d > 0 {
			if top > 0 && l.size <= top {
				d++
			}
			return pdfClampLevel(d), true
		}
		return pdfClampLevel(2), true
	}
	return 0, false
}

// pdfNumberDepth reads the depth out of a section number: "2." is depth one,
// "1.1" two, "3.2.4" three. A labelled or roman number carries no depth of its
// own and reports zero, leaving the caller's default in place.
func pdfNumberDepth(num string) int {
	s := strings.TrimSpace(num)
	s = strings.TrimSuffix(s, ".")
	parts := strings.Split(s, ".")
	for _, p := range parts {
		if _, err := strconv.Atoi(p); err != nil {
			return 0
		}
	}
	return len(parts)
}

func pdfClampLevel(l int) int {
	if l < 1 {
		return 1
	}
	if l > 6 {
		return 6
	}
	return l
}

// pdfLineIsolated reports whether a line stands apart from its neighbours —
// more air above and below it than the lines around it leave between
// themselves. That is how an unbolded, unenlarged heading announces itself.
func pdfLineIsolated(lines []pdfTextLine, i int) bool {
	size := lines[i].size
	if size <= 0 {
		return false
	}
	above := i == 0 || lines[i-1].col != lines[i].col || lines[i-1].y-lines[i].y > 1.5*size
	below := i+1 >= len(lines) || lines[i+1].col != lines[i].col || lines[i].y-lines[i+1].y > 1.5*size
	return above && below
}

// --- lists -----------------------------------------------------------------

var (
	pdfBulletGlyphs  = "•▪◦‣∙·–—*"
	pdfOrderedMarker = regexp.MustCompile(`^(\d{1,3}|[a-zA-Z]|[ivxIVX]{1,4})[.)]\s+`)
)

// pdfIsBullet reports whether a rune is a list bullet.
//
// The private-use range is in here because that is where Word puts its bullets.
// A Word list marker is a glyph from Symbol or Wingdings, and its ToUnicode
// entry maps it to U+F0B7 — a code point with no meaning outside that font.
// Read literally it is an invisible character, which is why bulleted lists used
// to arrive as a run of orphan sentences with a stray space in front of each.
func pdfIsBullet(r rune) bool {
	if strings.ContainsRune(pdfBulletGlyphs, r) {
		return true
	}
	return r >= 0xE000 && r <= 0xF8FF
}

// pdfBulletOnly reports a line that is nothing but a bullet glyph, and the
// Markdown marker it should become. Slide and DTP engines set the marker in its
// own text frame, so it arrives as a line of its own.
func pdfBulletOnly(l pdfTextLine) (string, bool) {
	t := strings.TrimSpace(l.text)
	r := []rune(t)
	if len(r) != 1 || !pdfIsBullet(r[0]) {
		return "", false
	}
	return "-", true
}

// pdfListItem turns a line that begins with a list marker into a Markdown list
// item, preserving the distinction between bulleted and numbered lists.
func pdfListItem(l pdfTextLine) (string, bool) {
	t := strings.TrimSpace(l.text)
	if t == "" {
		return "", false
	}
	r := []rune(t)
	// The space after the marker is optional: a bullet is positioned by the
	// text matrix, not by a space, so whether one was emitted at all depends
	// on the producer.
	if len(r) > 1 && pdfIsBullet(r[0]) {
		if rest := strings.TrimSpace(string(r[1:])); rest != "" {
			return "- " + rest, true
		}
	}
	if m := pdfOrderedMarker.FindString(t); m != "" {
		// A date or a decimal opening a sentence is not a list marker.
		rest := strings.TrimSpace(t[len(m):])
		if rest == "" {
			return "", false
		}
		return strings.TrimSpace(m) + " " + rest, true
	}
	return "", false
}

// --- paragraphs and hyphenation --------------------------------------------

// pdfJoinWrapped joins the lines of one paragraph back into a single Markdown
// paragraph, repairing words the typesetter broke across the line.
func pdfJoinWrapped(lines []string, witness map[string]bool) string {
	var b strings.Builder
	for i, l := range lines {
		if i == 0 {
			b.WriteString(l)
			continue
		}
		cur := b.String()
		if head, tail, ok := pdfHyphenJoin(cur, l, witness); ok {
			b.Reset()
			b.WriteString(head)
			b.WriteString(tail)
			continue
		}
		if strings.HasSuffix(cur, "-") {
			// The hyphen is the author's, so the word still runs across the
			// break: "e-" + "mail" is "e-mail", never "e- mail".
			b.WriteString(l)
			continue
		}
		b.WriteString(" ")
		b.WriteString(l)
	}
	return b.String()
}

// pdfHyphenJoin decides whether a line-final hyphen was the typesetter breaking
// a word, or a hyphen the author wrote.
//
// The distinction cannot be made from the two lines alone: "sys-" + "tem" and
// "long-" + "term" look identical. What separates them is the rest of the
// document. If the hyphenated form appears intact somewhere else — "long-term"
// in a line that did not wrap — then the hyphen is the author's and must stay.
// Guessing instead, as a bare join-if-lowercase rule does, silently produces
// "longterm" and "email", which then fail to match any quotation of the source.
func pdfHyphenJoin(cur, next string, witness map[string]bool) (string, string, bool) {
	if !strings.HasSuffix(cur, "-") && !strings.HasSuffix(cur, "­") {
		return "", "", false
	}
	i := strings.LastIndexFunc(strings.TrimRight(cur, "-­"), unicode.IsSpace)
	head := strings.TrimRight(cur, "-­")[i+1:]
	tail := next
	if j := strings.IndexFunc(next, unicode.IsSpace); j >= 0 {
		tail = next[:j]
	}
	if head == "" || tail == "" {
		return "", "", false
	}
	r := []rune(tail)
	// A capital or a digit after the break is a proper compound, not a wrap.
	if unicode.IsUpper(r[0]) || unicode.IsDigit(r[0]) {
		return "", "", false
	}
	// A conjunction after the break means the hyphen is a suspended one:
	// "in- en verkoop", "pre- and post-flight".
	switch strings.ToLower(tail) {
	case "en", "and", "or", "of", "und", "oder", "et", "ou":
		return "", "", false
	}
	// The continuation carries whatever punctuation followed it on the page,
	// and "e-mail," is not how the witness was recorded.
	if witness[strings.ToLower(head+"-"+strings.Trim(tail, ".,;:!?()[]\"'"))] {
		return "", "", false
	}
	return strings.TrimSuffix(strings.TrimRight(cur, "­"), "-"), next, true
}

// pdfHyphenWitnesses collects every hyphenated compound that appears intact on
// a single line anywhere in the document. Those are the hyphens the author
// wrote, and they are never joined away.
func pdfHyphenWitnesses(pages []pdfPage) map[string]bool {
	out := map[string]bool{}
	for _, pg := range pages {
		for _, l := range pg.lines {
			for _, w := range strings.Fields(l.text) {
				w = strings.Trim(w, ".,;:()[]\"'")
				if strings.Count(w, "-") == 1 && !strings.HasPrefix(w, "-") && !strings.HasSuffix(w, "-") {
					out[strings.ToLower(w)] = true
				}
			}
		}
	}
	return out
}

// --- running headers and footers -------------------------------------------

var pdfDigits = regexp.MustCompile(`\d+`)

// pdfMaxFurnitureLines caps how many lines at each edge of a page may be
// furniture. A running header is a line or three — a name, an issue, a date —
// and a cap keeps a data-bearing letterhead from being swallowed whole.
const pdfMaxFurnitureLines = 3

// pdfEdgeLines returns the indices that a running header or footer could
// occupy: the block of lines chained inward from the top and bottom of the
// page.
//
// Chaining is what makes a multi-line header work. Allowing only the single
// outermost line suppresses the first line of a two-line masthead and leaves
// the second repeating on every page — which is the more visible half of the
// failure, since the part that survives is the part the reader keeps seeing.
func pdfEdgeLines(lines []pdfTextLine, top, bot, band, body float64) map[int]bool {
	order := make([]int, len(lines))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return lines[order[a]].y > lines[order[b]].y })
	pitch := pdfPitch(lines, order)

	out := map[int]bool{}
	for _, i := range pdfEdgeRun(lines, order, pitch, band, top, body, true) {
		out[i] = true
	}
	for _, i := range pdfEdgeRun(lines, order, pitch, band, bot, body, false) {
		out[i] = true
	}
	return out
}

// pdfEdgeRun returns the lines at one edge of the page that could be running
// furniture, or nothing when that edge is simply where the body text starts.
//
// The band alone cannot decide this. A document set with one-inch margins puts
// its first line of body text inside any band wide enough to catch a real
// header, so a band test on its own takes the opening line of every page — and
// because a page number or a date makes those lines differ only in their
// digits, the repeat test then matches them across pages and deletes them.
//
// What separates the two is the gap, not the position. A running head is
// detached: there is white space between it and the text, far more than one
// line of leading. Body text that merely begins high on the page runs on at
// the body's own pitch, straight through the band and down the page. So the
// chain is followed to wherever it ends, and if it never breaks stride it is
// not furniture at all, however close to the edge it started.
func pdfEdgeRun(lines []pdfTextLine, order []int, pitch, band, edge, body float64, fromTop bool) []int {
	at := func(n int) pdfTextLine {
		if fromTop {
			return lines[order[n]]
		}
		return lines[order[len(order)-1-n]]
	}
	idx := func(n int) int {
		if fromTop {
			return order[n]
		}
		return order[len(order)-1-n]
	}
	depth := func(l pdfTextLine) float64 {
		if fromTop {
			return edge - l.y
		}
		return l.y - edge
	}

	var run []int
	for n := 0; n < len(order); n++ {
		if depth(at(n)) > band {
			break
		}
		// Where the document's own body type appears, the running head has
		// ended and the document proper has begun. That is the signal, rather
		// than any drift within the masthead: a masthead is often two sizes —
		// a name over a strapline — and cutting the block at the first change
		// of size splits it, leaving the smaller half to repeat on every page
		// while the detachment test below measures against the wrong line.
		if n > 0 && body > 0 && at(n).size >= body*(1-pdfFurnitureSizeTol) {
			break
		}
		run = append(run, idx(n))
	}
	if len(run) == 0 || len(run) > pdfMaxFurnitureLines {
		// A band filled with more lines than any masthead holds is body text
		// that happens to start near the edge.
		return nil
	}
	if len(run) == len(order) {
		// The page holds nothing else; there is no body to be detached from,
		// and calling all of it furniture would empty the page.
		return nil
	}
	// The line just past the band must be a real step away. If it follows at
	// the body's own pitch, the chain is one continuous flow of text and the
	// part inside the band is its first line, not a header.
	gap := depth(at(len(run))) - depth(at(len(run)-1))
	if pitch > 0 && gap < 1.6*pitch {
		return nil
	}
	return run
}

// pdfFurnitureSizeTol is the slack allowed when comparing a line against the
// body size, so a masthead set a hair under the body type is not mistaken for
// the body itself.
const pdfFurnitureSizeTol = 0.15

// pdfPitch is the page's prevailing line spacing, measured over the lines in
// geometric order. It is the yardstick the detachment test uses: a gap is only
// meaningful relative to how far apart this page sets its lines.
func pdfPitch(lines []pdfTextLine, order []int) float64 {
	var gaps []float64
	for n := 0; n+1 < len(order); n++ {
		if g := lines[order[n]].y - lines[order[n+1]].y; g > 0 {
			gaps = append(gaps, g)
		}
	}
	// Too few gaps to measure anything. A page with a header, one line and a
	// footer has no two consecutive body lines, so every gap it offers is one
	// of the very gaps the caller is trying to judge — and measuring the
	// yardstick from the thing being measured makes a detached header look
	// like ordinary line spacing. Fall back to the typographic estimate:
	// leading runs about 1.2 times the type size.
	if len(gaps) < 4 {
		var sizes []float64
		for _, l := range lines {
			if l.size > 0 {
				sizes = append(sizes, l.size)
			}
		}
		if len(sizes) == 0 {
			return 0
		}
		sort.Float64s(sizes)
		return 1.2 * sizes[len(sizes)/2]
	}
	sort.Float64s(gaps)
	// The lower quartile, for the same reason pdfLeading uses it: the gaps are
	// a mixture of line spacing and the larger spaces between blocks, and it
	// is the line spacing that defines "the next line".
	return gaps[len(gaps)/4]
}

// pdfMarkFurniture finds the lines that are page furniture — a running header,
// a footer, a page number — and marks every repeat of them.
//
// The first occurrence is deliberately kept. Furniture is not noise: a footer
// reading "Last updated 2026-09-24" or a header naming the journal is the only
// place that fact appears, and deleting it outright loses it. Repeating it on
// every page, on the other hand, is pagination bleeding into the text.
func pdfMarkFurniture(pages []pdfPage, inTable []map[int]bool, body float64) {
	if len(pages) < 2 {
		return
	}
	type hit struct{ page, line int }
	seen := map[string][]hit{}
	for p := range pages {
		pg := &pages[p]
		height := pg.top - pg.bot
		if height <= 0 {
			continue
		}
		edge := pdfEdgeLines(pg.lines, pg.top, pg.bot, pdfFurnitureBand*height, body)
		for i, l := range pg.lines {
			// A table row is never furniture. A long table repeats its header
			// on every page, which is exactly the shape this pass looks for —
			// and suppressing it leaves the delimiter row to promote the first
			// data row into a header, losing a row of real data.
			if inTable[p][i] {
				continue
			}
			// Both halves of the test matter. The band alone would take a
			// first paragraph that happens to start high on a short page, and
			// the digit normalisation that lets "Page 3" match "Page 4" would
			// then let "...on page 3 of..." match "...on page 4 of..." and
			// delete the body of every page but one. A running header is also
			// always the topmost line on its page, and a footer the bottom-
			// most, so requiring that costs nothing and closes the hole.
			// Both halves of the test matter. The band alone would take a
			// first paragraph that happens to start high on a short page, and
			// the digit normalisation that lets "Page 3" match "Page 4" would
			// then let "...on page 3 of..." match "...on page 4 of..." and
			// delete the body of every page but one.
			if !edge[i] {
				continue
			}
			_ = l
			// Page numbers differ per page by construction, so compare the
			// shape of the line rather than its text.
			key := pdfDigits.ReplaceAllString(l.text, "#")
			seen[key] = append(seen[key], hit{p, i})
		}
	}
	need := (len(pages) + 1) / 2
	if need < 2 {
		need = 2
	}
	for _, hits := range seen {
		pageSet := map[int]bool{}
		for _, h := range hits {
			pageSet[h.page] = true
		}
		if len(pageSet) < need {
			continue
		}
		for _, h := range hits[1:] {
			pages[h.page].furniture[h.line] = true
		}
	}
}

// --- tables ----------------------------------------------------------------

// pdfTable is a located table: the lines it covers and the column tracks its
// cells are assigned to.
type pdfTable struct {
	rows   int
	tracks []pdfTrack
}

// pdfTrack is one column of a table, as an X interval.
type pdfTrack struct{ x0, x1 float64 }

// pdfFindTables locates the table regions on a page.
//
// The method is deterministic: a table is a run of consecutive lines whose
// cells fall into the same vertical tracks. Nothing is rendered and no model is
// asked — the tracks are read off the cell edges, and every cell is filled with
// the glyphs that sit inside it, so a wrong track can misplace a value but can
// never invent one.
//
// ruled says the page draws rules somewhere. It does not say where, so it only
// relaxes the evidence required rather than placing a grid: a page that went to
// the trouble of drawing cell borders is a page whose aligned runs are far more
// likely to be tables than coincidentally justified prose.
func pdfFindTables(lines []pdfTextLine, ruled bool) map[int]pdfTable {
	// Cells are split once per line and reused. Splitting is cheap, but the
	// search below reconsiders a line as part of several candidate runs, and
	// re-splitting inside that loop makes the pass quadratic in the page's
	// glyph count for no gain.
	cells := make([][]pdfCell, len(lines))
	for i, l := range lines {
		cells[i] = pdfRowCells(l)
	}

	out := map[int]pdfTable{}
	for i := 0; i < len(lines); {
		if len(cells[i]) < pdfMinTableCols {
			i++
			continue
		}
		// A table can only be as long as the unbroken stretch of split lines
		// it starts. Bounding the candidates this way is what keeps the search
		// linear in the page: without it, every line on the page starts a
		// candidate of every possible length.
		end := i
		for end < len(lines) && lines[end].col == lines[i].col {
			if len(cells[end]) >= pdfMinTableCols {
				end++
				continue
			}
			// A ruled page may hold a spanning row — a total, a section label
			// — inside the table. An unruled one may not: there, a line that
			// does not split is where the table stops.
			if ruled && len(cells[end]) == 1 && end > i {
				end++
				continue
			}
			break
		}
		matched := 0
		for n := end - i; n >= pdfMinTableRows; n-- {
			if t, ok := pdfTableOf(lines[i:i+n], cells[i:i+n], ruled); ok {
				out[i] = t
				matched = n
				break
			}
		}
		if matched == 0 {
			i++
			continue
		}
		i += matched
	}
	return out
}

// pdfTableOf tests one run of lines and, if it is a table, returns its tracks.
//
// Tracks come from overlap, not from left edges. A column of right-aligned
// figures — which is most of the numbers in most tables — shares no left edge
// at all: "1", "16" and "1,504,000" start at three different X and end at one.
// Clustering their left edges splits one column into three and the table falls
// apart, so cells are grouped by whether their X intervals overlap instead,
// which is the same thing a reader does.
func pdfTableOf(rows []pdfTextLine, cells [][]pdfCell, ruled bool) (pdfTable, bool) {
	if len(rows) < pdfMinTableRows {
		return pdfTable{}, false
	}
	multi := 0
	for i, r := range rows {
		if _, isList := pdfListItem(r); isList {
			// A bulleted list aligns into tracks as readily as a table does —
			// every item starts at the same X, and a two-line item splits at
			// the same place. It is still a list.
			return pdfTable{}, false
		}
		if len(cells[i]) > 1 {
			multi++
		}
	}
	// Every row may not be split — a spanning total line is one cell — but a
	// run that is mostly unsplit lines is a paragraph, not a table.
	if multi*3 < len(rows)*2 {
		return pdfTable{}, false
	}

	tracks := pdfBuildTracks(cells)
	if len(tracks) < pdfMinTableCols {
		return pdfTable{}, false
	}
	// If two cells of one row land in the same track, the tracks do not
	// describe this run: it is prose whose wide word gaps happened to line up.
	filled := 0
	for _, row := range cells {
		used := map[int]bool{}
		for _, c := range row {
			t := pdfTrackOf(tracks, c)
			if used[t] {
				return pdfTable{}, false
			}
			used[t] = true
			filled++
		}
	}
	// A real grid is mostly full. A sparse one is alignment by coincidence —
	// an indented block, a table of contents, a column of footnotes.
	want := 0.55
	if ruled {
		want = 0.4
	}
	if float64(filled) < want*float64(len(rows)*len(tracks)) {
		return pdfTable{}, false
	}
	return pdfTable{rows: len(rows), tracks: tracks}, true
}

// pdfCell is one cell: its words and the X interval they occupy.
type pdfCell struct {
	words  []pdfWord
	x0, x1 float64
}

func (c pdfCell) text() string {
	parts := make([]string, 0, len(c.words))
	for _, w := range c.words {
		parts = append(parts, w.s)
	}
	return strings.Join(parts, " ")
}

// pdfRowCells splits one line into cells on the wide gaps.
func pdfRowCells(l pdfTextLine) []pdfCell {
	size := l.size
	if size <= 0 {
		size = 1
	}
	var out []pdfCell
	var cur pdfCell
	for i, w := range l.words {
		if i > 0 && w.x0-l.words[i-1].x1 > pdfCellGapFrac*size {
			out = append(out, cur)
			cur = pdfCell{}
		}
		if len(cur.words) == 0 {
			cur.x0 = w.x0
		}
		cur.words = append(cur.words, w)
		cur.x1 = w.x1
	}
	if len(cur.words) > 0 {
		out = append(out, cur)
	}
	return out
}

// pdfBuildTracks merges every cell interval on the run into the smallest set of
// columns that no cell straddles.
func pdfBuildTracks(rows [][]pdfCell) []pdfTrack {
	var all []pdfTrack
	for _, row := range rows {
		for _, c := range row {
			all = append(all, pdfTrack{c.x0, c.x1})
		}
	}
	if len(all) == 0 {
		return nil
	}
	sort.Slice(all, func(i, j int) bool { return all[i].x0 < all[j].x0 })
	out := []pdfTrack{all[0]}
	for _, t := range all[1:] {
		last := &out[len(out)-1]
		if t.x0 <= last.x1 {
			if t.x1 > last.x1 {
				last.x1 = t.x1
			}
			continue
		}
		out = append(out, t)
	}
	return out
}

// pdfTrackOf places a cell in the track it overlaps most. A cell that overlaps
// none — which the merge makes impossible for the cells the tracks were built
// from — falls back to the nearest one.
func pdfTrackOf(tracks []pdfTrack, c pdfCell) int {
	best, bestOv := 0, -1.0
	for i, t := range tracks {
		lo, hi := c.x0, c.x1
		if t.x0 > lo {
			lo = t.x0
		}
		if t.x1 < hi {
			hi = t.x1
		}
		ov := hi - lo
		if ov > bestOv {
			best, bestOv = i, ov
		}
	}
	return best
}

// pdfRenderTable emits a run of rows as a GitHub-flavored pipe table, placing
// every cell in the column its geometry puts it in.
//
// Placing by track rather than by order is what keeps a short row honest. A
// totals line holds two cells — a label and an amount — and writing them into
// the first two columns would file the amount under "Description". Filed by
// position, it lands under "Amount", where the page put it.
func pdfRenderTable(rows []pdfTextLine, t pdfTable) string {
	width := len(t.tracks)
	grid := make([][]string, 0, len(rows))
	for _, r := range rows {
		cells := make([]string, width)
		for _, c := range pdfRowCells(r) {
			i := pdfTrackOf(t.tracks, c)
			if cells[i] != "" {
				cells[i] += " "
			}
			cells[i] += mdutil.EscapeCell(c.text())
		}
		grid = append(grid, cells)
	}

	var b strings.Builder
	writeRow := func(cells []string) {
		b.WriteString("|")
		for i := 0; i < width; i++ {
			b.WriteString(" ")
			b.WriteString(cells[i])
			b.WriteString(" |")
		}
		b.WriteString("\n")
	}
	writeRow(grid[0])
	b.WriteString("|")
	for i := 0; i < width; i++ {
		b.WriteString(" --- |")
	}
	b.WriteString("\n")
	for _, row := range grid[1:] {
		writeRow(row)
	}
	return strings.TrimRight(b.String(), "\n")
}
