package anymd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"

	"github.com/muthuishere/anymd/internal/mdutil"
	"github.com/muthuishere/anymd/internal/ooxml"
)

func init() { addBuiltin(&DocxConverter{}) }

const (
	docxMime         = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	docxDocumentPart = "word/document.xml"
	docxNumberingPar = "word/numbering.xml"
	docxCorePart     = "docProps/core.xml"
)

// DocxConverter renders a WordprocessingML document (.docx) as Markdown using
// nothing but archive/zip and encoding/xml.
//
// It is deliberately a streaming token walk rather than a struct unmarshal:
// paragraph content is an *ordered* mix of runs, hyperlinks and change-tracking
// wrappers, and only a token walk preserves that order without recursing on
// attacker-controlled nesting.
type DocxConverter struct{}

// Name identifies the converter in errors and in `anymd --list`.
func (c *DocxConverter) Name() string { return "docx" }

// Accepts recognizes .docx by extension, by the WordprocessingML mime type, or
// by sniffing the zip central directory for word/document.xml. It never parses
// XML.
func (c *DocxConverter) Accepts(r io.ReadSeeker, info StreamInfo, opts *Options) bool {
	if info.HasExt(".docx") {
		return true
	}
	if info.NormalizedMime() == docxMime {
		return true
	}
	return ooxml.HasAnyPart(r, docxDocumentPart)
}

// Convert renders the document body to Markdown and lifts dc:title, when
// present, into Result.Title.
func (c *DocxConverter) Convert(r io.ReadSeeker, info StreamInfo, opts *Options) (Result, error) {
	pkg, err := ooxml.Open(r)
	if err != nil {
		return Result{}, err
	}
	data, err := pkg.Part(docxDocumentPart)
	if err != nil {
		return Result{}, err
	}

	dc := &docxCtx{
		pkg:       pkg,
		rels:      pkg.RelTargets(docxDocumentPart),
		numbered:  docxNumberFormats(pkg.OptionalPart(docxNumberingPar)),
		captioner: newOOXMLCaptioner(pkg, opts),
	}
	blocks, err := dc.document(data)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Markdown: mdutil.Join(blocks...),
		Title:    docxCoreTitle(pkg.OptionalPart(docxCorePart)),
	}, nil
}

// docxCtx carries the package-level lookups a body walk needs.
type docxCtx struct {
	pkg      *ooxml.Package    // for parts a body reference points at (charts)
	rels     map[string]string // r:id -> hyperlink target AND r:embed -> media part
	numbered map[string]bool   // "numId:ilvl" and "numId" -> is an ordered list

	// captioner is nil unless the caller supplied a Describer; see
	// ooxmlCaptioner for why nil is the interesting case.
	captioner *ooxmlCaptioner

	// pending holds captions produced while walking the current paragraph.
	// An image lives *inline* in a w:p, but its description is prose and
	// belongs in its own block, so the paragraph walk queues it here and
	// document() emits it after the paragraph the image sat in.
	pending []string

	// inTable suppresses captioning inside a w:tbl: a caption cannot be added
	// to a GFM cell without either breaking the row or burying the prose, so a
	// table image keeps its alt text and costs no model call.
	inTable bool

	// cell is the w:tc currently being walked, or nil outside a table. A cell
	// needs two renderings of the same content — the flat text a GFM cell can
	// hold, and the block rendering used when the table turns out to be a
	// single-cell layout wrapper — and hanging it off the context is what lets
	// one pass produce both.
	cell *docxCellCtx

	// tbDepth bounds text-box recursion. A text box holds ordinary paragraphs,
	// which may hold further text boxes; the nesting is attacker-controlled, so
	// it is bounded rather than trusted.
	tbDepth int
}

// docxMaxTextboxDepth is how deep text boxes may nest before their content is
// dropped. Real documents never exceed two.
const docxMaxTextboxDepth = 8

// docxCellCtx accumulates, while a w:tc is walked, the two things the cell's
// two renderings need that the block walk would otherwise discard.
type docxCellCtx struct {
	span int
	text []string
}

// takePending returns and clears the captions queued by the last paragraph.
func (dc *docxCtx) takePending() []string {
	if len(dc.pending) == 0 {
		return nil
	}
	out := dc.pending
	dc.pending = nil
	return out
}

// image renders one w:drawing: the placeholder exactly as it has always been
// rendered, plus — only when a Describer produced one — a caption queued for
// emission as the following block. ok is false when there is nothing to emit at
// all, which is the pre-existing behaviour for an image with no alt text.
func (dc *docxCtx) image(alt, embedID string) (docxSeg, bool) {
	caption := ""
	if !dc.inTable {
		caption = dc.captioner.caption(docxDocumentPart, embedID, alt)
	}
	if caption == "" && alt == "" {
		return docxSeg{}, false
	}
	if caption != "" {
		dc.pending = append(dc.pending, caption)
	}
	// Rendered without a destination: the media part is not inlined, but the
	// authored description is often the only prose describing the figure.
	return docxSeg{raw: "![" + alt + "]()"}, true
}

// docxSeg is one stretch of inline content. raw is set for content that is
// already Markdown (a hyperlink) and must not be re-wrapped in emphasis.
type docxSeg struct {
	text string
	bold bool
	ital bool
	raw  string
}

// docxPara is everything a body walk learns about one w:p.
type docxPara struct {
	segs  []docxSeg
	style string
	list  bool
	ilvl  int
	numID string
}

// document walks word/document.xml and returns the top-level Markdown blocks.
func (dc *docxCtx) document(data []byte) ([]string, error) {
	d := ooxml.NewDecoder(data)

	// Descend to w:body. Anything before it (w:document attributes, mc gunk) is
	// uninteresting.
	found := false
	for !found {
		tok, err := d.Token()
		if err == io.EOF {
			return nil, errors.New("docx: no <w:body> in word/document.xml")
		}
		if err != nil {
			return nil, fmt.Errorf("docx: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "body" {
			found = true
		}
	}
	return dc.blocks(d)
}

// blocks consumes the children of the block container the caller has just
// entered and returns the Markdown blocks they produce.
//
// Four different elements are that container — w:body, a content control's
// w:sdtContent, a text box's w:txbxContent and a table cell's w:tc — and all
// four hold the same grammar. Walking them through one function is what makes a
// list inside a text box inside a table cell come out as a list: a simplified
// second path for "the nested case" is exactly how that content gets lost.
func (dc *docxCtx) blocks(d *xml.Decoder) ([]string, error) {
	var (
		blocks      []string
		list        []string
		counters    []int
		listCaption []string
	)
	// A caption belonging to an image inside a list item cannot interrupt the
	// list, so it is held until the list closes and emitted after it.
	flushList := func() {
		if len(list) > 0 {
			blocks = append(blocks, strings.Join(list, "\n"))
			list = nil
			counters = counters[:0]
		}
		blocks = append(blocks, listCaption...)
		listCaption = nil
	}

	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("docx: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p":
				p, err := dc.paragraph(d)
				if err != nil {
					return nil, fmt.Errorf("docx: %w", err)
				}
				if dc.cell != nil {
					if s := mdutil.Collapse(plainDocxSegs(p.segs)); s != "" {
						dc.cell.text = append(dc.cell.text, s)
					}
				}
				if p.list {
					list = append(list, dc.listItem(p, &counters))
					listCaption = append(listCaption, dc.takePending()...)
					continue
				}
				flushList()
				if b := dc.block(p); b != "" {
					blocks = append(blocks, b)
				}
				blocks = append(blocks, dc.takePending()...)
			case "tbl":
				tbl, err := dc.table(d)
				if err != nil {
					return nil, fmt.Errorf("docx: %w", err)
				}
				flushList()
				blocks = append(blocks, tbl...)
				blocks = append(blocks, dc.takePending()...)
			case "tcPr":
				// A cell's properties sit among its blocks. w:gridSpan is the
				// only one that changes the rendering, and it has to be read
				// here because this loop owns the cell's children.
				if err := dc.cellProps(d); err != nil {
					if err == io.EOF {
						depth = 0
						continue
					}
					return nil, fmt.Errorf("docx: %w", err)
				}
			case "sdt", "sdtContent", "AlternateContent", "Choice":
				// Transparent wrappers. A content control (w:sdt) is a *box
				// around* real body content — a cover title, a locked table —
				// so skipping it drops everything inside; and mc:Choice is the
				// modern half of a Word shape, which is the half we read.
				depth++
			case "Fallback":
				// The legacy VML twin of the mc:Choice above, carrying the same
				// text. Taking both emits every text box twice.
				if err := ooxml.SkipElement(d); err != nil {
					if err == io.EOF {
						depth = 0
						continue
					}
					return nil, fmt.Errorf("docx: %w", err)
				}
			case "txbxContent":
				inner, err := dc.textbox(d)
				if err != nil {
					return nil, fmt.Errorf("docx: %w", err)
				}
				flushList()
				blocks = append(blocks, inner...)
			default:
				if err := ooxml.SkipElement(d); err != nil {
					if err == io.EOF {
						depth = 0
						continue
					}
					return nil, fmt.Errorf("docx: %w", err)
				}
			}
		case xml.EndElement:
			depth--
		}
	}
	flushList()
	return blocks, nil
}

// textbox renders one w:txbxContent — the body of a Word text box, reached
// either through a DrawingML shape (wps:txbx) or the legacy VML path
// (w:pict/v:shape/v:textbox). Its content is ordinary paragraphs, so it goes
// through the same block walk as the body and keeps its lists, headings and
// tables.
//
// The blocks come back to the caller rather than being spliced inline: a text
// box is anchored *in* a paragraph but reads as its own prose, and putting a
// list inside a paragraph would not be Markdown.
func (dc *docxCtx) textbox(d *xml.Decoder) ([]string, error) {
	if dc.tbDepth >= docxMaxTextboxDepth {
		err := ooxml.SkipElement(d)
		if err == io.EOF {
			err = nil
		}
		return nil, err
	}
	dc.tbDepth++
	savedPending := dc.pending
	dc.pending = nil
	blocks, err := dc.blocks(d)
	dc.pending = savedPending
	dc.tbDepth--
	return blocks, err
}

// cellProps consumes a w:tcPr, recording the horizontal span on the cell being
// walked. Outside a cell it is an ordinary skip.
func (dc *docxCtx) cellProps(d *xml.Decoder) error {
	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "gridSpan" && dc.cell != nil {
				if n, err := strconv.Atoi(strings.TrimSpace(ooxml.Attr(t, "val"))); err == nil && n > 1 && n <= 1000 {
					dc.cell.span = n
				}
			}
			depth++
		case xml.EndElement:
			depth--
		}
	}
	return nil
}

// block renders a non-list paragraph: a heading when its style says so,
// otherwise a plain paragraph.
func (dc *docxCtx) block(p docxPara) string {
	text := renderDocxSegs(p.segs)
	text = strings.TrimRight(strings.TrimLeft(text, " \t"), " \t\n")
	if text == "" {
		return ""
	}
	if lvl := docxHeadingLevel(p.style); lvl > 0 {
		return mdutil.Heading(lvl, mdutil.Collapse(text))
	}
	return text
}

// listItem renders one numbered or bulleted paragraph, indenting two spaces per
// w:ilvl. counters holds the running ordinal for each level so that a nested
// ordered list restarts when it re-opens.
func (dc *docxCtx) listItem(p docxPara, counters *[]int) string {
	lvl := p.ilvl
	if lvl < 0 {
		lvl = 0
	}
	if lvl > 8 {
		lvl = 8
	}
	marker := "- "
	if dc.ordered(p.numID, lvl) {
		c := *counters
		for len(c) <= lvl {
			c = append(c, 0)
		}
		c = c[:lvl+1]
		c[lvl]++
		*counters = c
		marker = strconv.Itoa(c[lvl]) + ". "
	}
	text := mdutil.Collapse(renderDocxSegs(p.segs))
	return strings.Repeat("  ", lvl) + marker + text
}

// ordered reports whether a numbering definition is an ordered list. An absent
// or unreadable numbering.xml degrades to bullets rather than failing.
func (dc *docxCtx) ordered(numID string, ilvl int) bool {
	if numID == "" || dc.numbered == nil {
		return false
	}
	if v, ok := dc.numbered[numID+":"+strconv.Itoa(ilvl)]; ok {
		return v
	}
	return dc.numbered[numID]
}

// paragraph consumes one w:p (its start tag already read) and returns its
// properties plus its inline segments, in document order.
func (dc *docxCtx) paragraph(d *xml.Decoder) (docxPara, error) {
	var p docxPara
	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err == io.EOF {
			return p, nil
		}
		if err != nil {
			return p, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "pPr":
				if err := dc.paraProps(d, &p); err != nil {
					return p, err
				}
			case "r":
				segs, err := dc.run(d)
				if err != nil {
					return p, err
				}
				p.segs = append(p.segs, segs...)
			case "hyperlink":
				seg, err := dc.hyperlink(d, t)
				if err != nil {
					return p, err
				}
				p.segs = append(p.segs, seg)
			case "drawing":
				alt, embed, err := dc.drawing(d)
				if err != nil {
					return p, err
				}
				if seg, ok := dc.image(alt, embed); ok {
					p.segs = append(p.segs, seg)
				}
			case "Fallback":
				// See blocks(): the VML twin of a shape already read from
				// mc:Choice, whose text would otherwise be emitted twice.
				if err := ooxml.SkipElement(d); err != nil && err != io.EOF {
					return p, err
				}
			case "txbxContent":
				inner, err := dc.textbox(d)
				if err != nil {
					return p, err
				}
				dc.pending = append(dc.pending, inner...)
			default:
				// Descend. Revision marks (w:ins), structured document tags and
				// smart tags all wrap runs we still want, and descending costs
				// nothing for elements that hold none.
				depth++
			}
		case xml.EndElement:
			depth--
		}
	}
	return p, nil
}

// paraProps consumes a w:pPr, recording the style name and any numbering.
func (dc *docxCtx) paraProps(d *xml.Decoder, p *docxPara) error {
	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "pStyle":
				p.style = ooxml.Attr(t, "val")
			case "numPr":
				p.list = true
			case "ilvl":
				if p.list {
					if n, err := strconv.Atoi(strings.TrimSpace(ooxml.Attr(t, "val"))); err == nil {
						p.ilvl = n
					}
				}
			case "numId":
				if p.list {
					p.numID = strings.TrimSpace(ooxml.Attr(t, "val"))
				}
			}
			depth++
		case xml.EndElement:
			depth--
		}
	}
	// numId="0" means "no numbering" in WordprocessingML.
	if p.numID == "0" {
		p.list = false
	}
	return nil
}

// run consumes one w:r and returns its content as segments.
//
// It returns a slice rather than a single segment because a run may embed a
// w:drawing between two stretches of text, and an image must not end up inside
// the run's emphasis markers.
func (dc *docxCtx) run(d *xml.Decoder) ([]docxSeg, error) {
	var (
		out  []docxSeg
		bold bool
		ital bool
		sb   strings.Builder
	)
	flush := func() {
		if sb.Len() == 0 {
			return
		}
		out = append(out, docxSeg{text: sb.String(), bold: bold, ital: ital})
		sb.Reset()
	}
	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "rPr":
				b, i, err := runProps(d)
				if err != nil {
					return nil, err
				}
				bold, ital = b, i
			case "t":
				preserve := ooxml.Attr(t, "space") == "preserve"
				s, err := ooxml.TextOf(d)
				if err != nil {
					return nil, err
				}
				if !preserve {
					s = strings.TrimSpace(s)
				}
				sb.WriteString(s)
			case "br", "cr":
				if err := ooxml.SkipElement(d); err != nil && err != io.EOF {
					return nil, err
				}
				sb.WriteString("\n")
			case "tab":
				if err := ooxml.SkipElement(d); err != nil && err != io.EOF {
					return nil, err
				}
				sb.WriteString(" ")
			case "drawing":
				alt, embed, err := dc.drawing(d)
				if err != nil {
					return nil, err
				}
				if seg, ok := dc.image(alt, embed); ok {
					flush()
					out = append(out, seg)
				}
			case "AlternateContent", "Choice", "pict", "object",
				"group", "shape", "shapetype", "rect", "roundrect",
				"oval", "line", "polyline", "textbox":
				// A run's default is to *skip* what it does not know, because
				// most of what a run holds is metadata. These are the
				// exception: they are the containers a Word shape is built
				// from, and a text box's paragraphs sit at the bottom of them.
				// This is the whole reason textbox.docx converted to two lines.
				depth++
			case "txbxContent":
				inner, err := dc.textbox(d)
				if err != nil {
					return nil, err
				}
				dc.pending = append(dc.pending, inner...)
			case "Fallback":
				if err := ooxml.SkipElement(d); err != nil && err != io.EOF {
					return nil, err
				}
			case "delText":
				// Deleted text from a tracked change is not part of the document.
				if err := ooxml.SkipElement(d); err != nil && err != io.EOF {
					return nil, err
				}
			default:
				if err := ooxml.SkipElement(d); err != nil {
					if err == io.EOF {
						depth = 0
						continue
					}
					return nil, err
				}
			}
		case xml.EndElement:
			depth--
		}
	}
	flush()
	return out, nil
}

// runProps consumes a w:rPr and reports the toggles we render.
func runProps(d *xml.Decoder) (bold, ital bool, err error) {
	depth := 1
	for depth > 0 {
		tok, e := d.Token()
		if e == io.EOF {
			return bold, ital, nil
		}
		if e != nil {
			return false, false, e
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "b", "bCs":
				if ooxml.OnOff(t) {
					bold = true
				}
			case "i", "iCs":
				if ooxml.OnOff(t) {
					ital = true
				}
			}
			depth++
		case xml.EndElement:
			depth--
		}
	}
	return bold, ital, nil
}

// hyperlink consumes a w:hyperlink, resolving r:id through the document's
// relationships part. An unresolvable link degrades to its text.
func (dc *docxCtx) hyperlink(d *xml.Decoder, se xml.StartElement) (docxSeg, error) {
	var segs []docxSeg
	id := ooxml.Attr(se, "id")
	anchor := ooxml.Attr(se, "anchor")

	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return docxSeg{}, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "r" {
				rs, err := dc.run(d)
				if err != nil {
					return docxSeg{}, err
				}
				segs = append(segs, rs...)
				continue
			}
			depth++
		case xml.EndElement:
			depth--
		}
	}

	text := strings.TrimSpace(mdutil.Collapse(renderDocxSegs(segs)))
	url := ""
	if id != "" && dc.rels != nil {
		url = dc.rels[id]
	}
	if url == "" && anchor != "" {
		url = "#" + anchor
	}
	if text == "" {
		return docxSeg{raw: ""}, nil
	}
	if url == "" {
		return docxSeg{raw: text}, nil
	}
	return docxSeg{raw: "[" + text + "](" + escapeLinkURL(url) + ")"}, nil
}

// docxCell is one w:tc in both of the renderings a table might need: the flat
// text a GFM cell can hold, and the full block rendering used when the table
// turns out to be a layout wrapper rather than tabular data.
type docxCell struct {
	text   string
	blocks []string
	span   int
}

// table consumes one w:tbl and returns the blocks it renders to.
//
// It returns blocks rather than a grid because not every w:tbl is a table. Word
// has no "box" primitive, so authors draw one as a table with a single row and
// a single cell wrapped around ordinary body content — a list, a nested table,
// a run of paragraphs. Rendering that as a one-cell GFM table flattens whatever
// was inside it into one line and loses a nested table entirely, so a
// single-cell table is unwrapped and its content emitted at this level.
func (dc *docxCtx) table(d *xml.Decoder) ([]string, error) {
	var rows [][]docxCell
	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "tr" {
				row, err := dc.tableRow(d)
				if err != nil {
					return nil, err
				}
				rows = append(rows, row)
				continue
			}
			if err := ooxml.SkipElement(d); err != nil {
				if err == io.EOF {
					depth = 0
					continue
				}
				return nil, err
			}
		case xml.EndElement:
			depth--
		}
	}

	if len(rows) == 1 && len(rows[0]) == 1 {
		return rows[0][0].blocks, nil
	}

	grid := make([][]string, 0, len(rows))
	for _, row := range rows {
		cells := make([]string, 0, len(row))
		for _, c := range row {
			cells = append(cells, c.text)
			for i := 1; i < c.span; i++ {
				cells = append(cells, "")
			}
		}
		grid = append(grid, cells)
	}
	grid = padRectangular(grid)
	if len(grid) == 0 {
		return nil, nil
	}
	if tbl := mdutil.Table(grid[0], grid[1:]); tbl != "" {
		return []string{tbl}, nil
	}
	return nil, nil
}

// tableRow consumes one w:tr and returns its cells.
func (dc *docxCtx) tableRow(d *xml.Decoder) ([]docxCell, error) {
	row := []docxCell{}
	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "tc" {
				cell, err := dc.tableCell(d)
				if err != nil {
					return nil, err
				}
				row = append(row, cell)
				continue
			}
			if err := ooxml.SkipElement(d); err != nil {
				if err == io.EOF {
					depth = 0
					continue
				}
				return nil, err
			}
		case xml.EndElement:
			depth--
		}
	}
	return row, nil
}

// tableCell consumes one w:tc through the ordinary block walk, so a cell keeps
// its lists, its content controls and its nested tables, and hands back both
// renderings of what it found.
func (dc *docxCtx) tableCell(d *xml.Decoder) (docxCell, error) {
	// Captions cannot live in a GFM cell, so no image inside a table is sent
	// to the model at all. Restored on the way out because tables nest.
	wasInTable := dc.inTable
	dc.inTable = true
	parent := dc.cell
	cur := &docxCellCtx{span: 1}
	dc.cell = cur
	defer func() { dc.inTable = wasInTable; dc.cell = parent }()

	blocks, err := dc.blocks(d)
	// A nested table's text belongs to the cell that contains it as well as to
	// its own cells, or it would vanish from the outer row.
	if parent != nil {
		parent.text = append(parent.text, cur.text...)
	}
	return docxCell{
		text:   mdutil.Collapse(strings.Join(cur.text, " ")),
		blocks: blocks,
		span:   cur.span,
	}, err
}

// padRectangular widens every row to the widest one, so mdutil.Table never
// silently truncates a spanned row.
func padRectangular(rows [][]string) [][]string {
	width := 0
	for _, r := range rows {
		if len(r) > width {
			width = len(r)
		}
	}
	for i, r := range rows {
		for len(r) < width {
			r = append(r, "")
		}
		rows[i] = r
	}
	return rows
}

// docxHeadingLevel maps a w:pStyle value to a Markdown heading level, or 0 for
// body text. Producers write both "Heading1" and "Heading 1", so spaces and
// case are normalized away.
func docxHeadingLevel(style string) int {
	s := strings.ToLower(strings.Join(strings.Fields(style), ""))
	switch s {
	case "":
		return 0
	case "title":
		return 1
	case "subtitle":
		return 2
	}
	if rest, ok := strings.CutPrefix(s, "heading"); ok {
		if n, err := strconv.Atoi(rest); err == nil && n >= 1 && n <= 9 {
			return n
		}
	}
	return 0
}

// mergeDocxSegs joins adjacent runs that share formatting, so a sentence Word
// happened to split across three runs emits "**one phrase**" rather than
// "**one** **phrase**".
func mergeDocxSegs(segs []docxSeg) []docxSeg {
	out := make([]docxSeg, 0, len(segs))
	for _, s := range segs {
		if s.raw == "" && s.text == "" {
			continue
		}
		if n := len(out); n > 0 && s.raw == "" && out[n-1].raw == "" &&
			out[n-1].bold == s.bold && out[n-1].ital == s.ital {
			out[n-1].text += s.text
			continue
		}
		out = append(out, s)
	}
	return out
}

// renderDocxSegs merges then wraps the inline segments into Markdown.
func renderDocxSegs(segs []docxSeg) string {
	var sb strings.Builder
	for _, s := range mergeDocxSegs(segs) {
		if s.raw != "" {
			sb.WriteString(s.raw)
			continue
		}
		sb.WriteString(emphasize(s.text, s.bold, s.ital))
	}
	return sb.String()
}

// plainDocxSegs is renderDocxSegs without emphasis, for table cells.
func plainDocxSegs(segs []docxSeg) string {
	var sb strings.Builder
	for _, s := range segs {
		if s.raw != "" {
			sb.WriteString(s.raw)
			continue
		}
		sb.WriteString(s.text)
	}
	return sb.String()
}

// emphasize wraps text in bold/italic markers, keeping any surrounding
// whitespace *outside* the markers — "** bold**" does not render.
func emphasize(text string, bold, ital bool) string {
	if text == "" || (!bold && !ital) {
		return text
	}
	core := strings.Trim(text, " \t\n")
	if core == "" {
		return text
	}
	i := strings.Index(text, core)
	lead, trail := text[:i], text[i+len(core):]
	marker := ""
	if bold {
		marker += "**"
	}
	if ital {
		marker += "*"
	}
	return lead + marker + core + marker + trail
}

// escapeLinkURL keeps a URL from breaking out of a Markdown link destination.
func escapeLinkURL(u string) string {
	r := strings.NewReplacer(" ", "%20", "(", "%28", ")", "%29", "\n", "", "\r", "")
	return r.Replace(strings.TrimSpace(u))
}

// docxNumberFormats builds the "is this numId ordered?" lookup from
// word/numbering.xml. A missing or malformed part yields nil, and every list
// then degrades to bullets.
func docxNumberFormats(data []byte) map[string]bool {
	if len(data) == 0 {
		return nil
	}
	var doc struct {
		AbstractNums []struct {
			ID   string `xml:"abstractNumId,attr"`
			Lvls []struct {
				ILvl   string `xml:"ilvl,attr"`
				NumFmt struct {
					Val string `xml:"val,attr"`
				} `xml:"numFmt"`
			} `xml:"lvl"`
		} `xml:"abstractNum"`
		Nums []struct {
			NumID         string `xml:"numId,attr"`
			AbstractNumID struct {
				Val string `xml:"val,attr"`
			} `xml:"abstractNumId"`
		} `xml:"num"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil
	}

	abstract := make(map[string]map[string]bool, len(doc.AbstractNums))
	for _, an := range doc.AbstractNums {
		lv := make(map[string]bool, len(an.Lvls))
		for _, l := range an.Lvls {
			lv[strings.TrimSpace(l.ILvl)] = isOrderedNumFmt(l.NumFmt.Val)
		}
		abstract[strings.TrimSpace(an.ID)] = lv
	}

	out := make(map[string]bool)
	for _, n := range doc.Nums {
		numID := strings.TrimSpace(n.NumID)
		lv, ok := abstract[strings.TrimSpace(n.AbstractNumID.Val)]
		if !ok || numID == "" {
			continue
		}
		for ilvl, ordered := range lv {
			out[numID+":"+ilvl] = ordered
		}
		if ordered, ok := lv["0"]; ok {
			out[numID] = ordered
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// isOrderedNumFmt treats everything that is not a bullet or "none" as ordered:
// decimal, letters, roman numerals and the enclosed variants all number.
func isOrderedNumFmt(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "bullet", "none":
		return false
	default:
		return true
	}
}

// docxCoreTitle extracts dc:title from docProps/core.xml. Its absence is normal.
func docxCoreTitle(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	var props struct {
		Title string `xml:"title"`
	}
	if err := xml.Unmarshal(data, &props); err != nil {
		return ""
	}
	return strings.TrimSpace(props.Title)
}

// drawing consumes a w:drawing and returns the normalized alt text of its
// wp:docPr (falling back to the shape name) plus the r:embed relationship id of
// the first a:blip, which is what names the media part holding the pixels.
//
// A w:drawing is not only ever a picture. It is also how Word anchors a shape
// with a text box in it and how it anchors a chart, and both of those carry
// content that no alt text describes. Those are queued as blocks, and a drawing
// that turned out to be one of them reports no image at all: "![Chart 5]()"
// next to the chart's own numbers is noise, not a figure.
func (dc *docxCtx) drawing(d *xml.Decoder) (alt, embed string, err error) {
	var extra []string
	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch {
			case t.Name.Local == "txbxContent":
				inner, err := dc.textbox(d)
				if err != nil {
					return "", "", err
				}
				extra = append(extra, inner...)
				continue
			case t.Name.Local == "docPr" && alt == "":
				alt = ooxmlAltText(ooxml.Attr(t, "descr"), docxShapeName(ooxml.Attr(t, "name")))
			case t.Name.Local == "chart":
				extra = append(extra, dc.chart(ooxml.Attr(t, "id"))...)
			case t.Name.Local == "blip" && embed == "":
				// r:embed is the internal part; r:link points outside the
				// package and is deliberately ignored.
				embed = ooxml.Attr(t, "embed")
			default:
				depth++
				continue
			}
			if err := ooxml.SkipElement(d); err != nil {
				if err == io.EOF {
					depth = 0
					continue
				}
				return "", "", err
			}
		case xml.EndElement:
			depth--
		}
	}
	dc.pending = append(dc.pending, extra...)
	if len(extra) > 0 && embed == "" {
		return "", "", nil
	}
	return alt, embed, nil
}

// docxShapeName drops the identifier Word generates for every shape it creates
// — "Picture 1", "Group 4", "Text Box 3", "Elbow Connector 12" — and keeps a
// name someone actually chose.
//
// wp:docPr@name is a fallback for wp:docPr@descr, and a fallback is only worth
// having when it says something. An auto-generated name describes nothing, and
// putting it in an image's alt text invents words the document never contained:
// it reads as content to anything downstream that indexes the Markdown. The
// tell is that Word always suffixes the shape's ordinal, which an authored
// caption almost never ends with.
func docxShapeName(name string) string {
	trimmed := strings.TrimRight(name, "0123456789")
	if trimmed == name || strings.TrimSpace(trimmed) == "" {
		return name
	}
	if rest := strings.TrimRight(trimmed, " \t"); rest != trimmed {
		return ""
	}
	return name
}

// chart follows a c:chart relationship into word/charts/chartN.xml and renders
// what a reader would actually see: the chart's label, and the category x
// series grid from the cached values the producer embedded beside the plot.
//
// The cache is the only readable source — the live data lives in an embedded
// workbook — so an uncached chart degrades to its label alone rather than
// failing the document.
func (dc *docxCtx) chart(relID string) []string {
	if relID == "" || dc.pkg == nil || dc.rels == nil {
		return nil
	}
	part := ooxml.ResolveTarget(docxDocumentPart, dc.rels[relID])
	if part == "" || !dc.pkg.Has(part) {
		return nil
	}
	data := dc.pkg.OptionalPart(part)
	if len(data) == 0 {
		return nil
	}
	ch, err := ooxml.ParseChart(data)
	if err != nil {
		return nil
	}

	var blocks []string
	if label := docxChartLabel(ch); label != "" {
		blocks = append(blocks, label)
	}
	if len(ch.Series) == 0 {
		return blocks
	}
	// The first column holds the categories, which belong to the chart rather
	// than to any one series, so its header is empty.
	header := []string{""}
	rowCount := 0
	for _, s := range ch.Series {
		header = append(header, s.Name)
		if len(s.Cats) > rowCount {
			rowCount = len(s.Cats)
		}
		if len(s.Vals) > rowCount {
			rowCount = len(s.Vals)
		}
	}
	if rowCount == 0 {
		return blocks
	}
	rows := make([][]string, 0, rowCount)
	for i := 0; i < rowCount; i++ {
		row := []string{""}
		for _, s := range ch.Series {
			if row[0] == "" && i < len(s.Cats) {
				row[0] = s.Cats[i]
			}
			v := ""
			if i < len(s.Vals) {
				v = s.Vals[i]
			}
			row = append(row, v)
		}
		rows = append(rows, row)
	}
	if t := mdutil.Table(header, rows); t != "" {
		blocks = append(blocks, t)
	}
	return blocks
}

// docxChartLabel names a chart: its authored title when it has one, and
// otherwise the plot type, which is the only other thing the part says about
// it ("Line chart"). An untitled chart with no label at all would leave the
// table below it unexplained.
func docxChartLabel(ch ooxml.Chart) string {
	if ch.Title != "" {
		return ch.Title
	}
	kind := strings.TrimSuffix(ch.Kind, "3D")
	if kind == "" {
		return ""
	}
	return strings.ToUpper(kind[:1]) + kind[1:] + " chart"
}

// ooxmlAltText normalizes an authored image description into something safe to
// put between the brackets of a Markdown image: descr wins over name, the
// bracket characters that would terminate the label early are neutralized, and
// the embedded newlines Word writes into descr collapse to single spaces.
//
// Shared by the docx and pptx converters so a figure reads the same either way.
func ooxmlAltText(descr, name string) string {
	alt := descr
	if strings.TrimSpace(alt) == "" {
		alt = name
	}
	alt = strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', '[', ']':
			return ' '
		}
		return r
	}, alt)
	return mdutil.Collapse(alt)
}

// --- shared OOXML image captioning -----------------------------------------
//
// docx and pptx both embed images the same way: a drawing carries an r:embed
// relationship id that resolves, through the *containing part's* rels, to a
// media part in the archive. Neither converter reads those pixels by default —
// there is no model in the box, and the placeholder plus the authored alt text
// is all a pure-Go parse can honestly say. When the caller supplies a
// Describer, the bytes become worth reading, and everything below exists to
// read them at the lowest possible cost.

// ooxmlMinCaptionBytes is the size below which an embedded image is assumed to
// be furniture — a spacer, a bullet glyph, a rule, a 1x1 tracking pixel. A
// model call on those buys nothing and costs a round trip, so they keep their
// alt text and nothing more. 4 KiB is comfortably below any real photograph or
// screenshot and comfortably above the decorative PNGs Office scatters through
// a template.
const ooxmlMinCaptionBytes = 4 << 10

// ooxmlMaxCaptionsPerDoc caps how many Describer calls one document may make.
// A 300-slide deck must not silently fire 300 network requests because someone
// passed an Options with a Describer; past the cap every further image degrades
// to its alt text, exactly as if no Describer had been supplied. Deduplicated
// repeats (the logo on every slide) do not count against it — they cost
// nothing.
const ooxmlMaxCaptionsPerDoc = 50

// ooxmlCaptioner resolves embedded images and captions them, once per distinct
// image, for the lifetime of a single conversion.
//
// A nil *ooxmlCaptioner is the "no Describer" case and every method on it is a
// no-op returning "", so the call sites stay branch-free and — importantly —
// never read a media part out of the zip when nothing will look at it.
type ooxmlCaptioner struct {
	pkg  *ooxml.Package
	opts *Options
	// rels caches id -> target per source part, so a 60-slide deck parses each
	// slide's rels once rather than once per picture.
	rels map[string]map[string]string
	// cache maps a sha256 of the image bytes to its caption, which is what
	// turns "the same logo on 60 slides" into one model call. An empty value
	// is cached too: a failed or refused image must not be retried 59 times.
	cache map[string]string
	calls int
}

// newOOXMLCaptioner returns nil when the caller supplied no Describer, which is
// the default and the only mode in which anymd makes no network calls.
func newOOXMLCaptioner(pkg *ooxml.Package, opts *Options) *ooxmlCaptioner {
	if pkg == nil || !opts.HasDescriber() {
		return nil
	}
	return &ooxmlCaptioner{
		pkg:   pkg,
		opts:  opts,
		rels:  map[string]map[string]string{},
		cache: map[string]string{},
	}
}

// caption resolves embedID through sourcePart's relationships, reads the media
// part, and asks the Describer to caption it, passing the document's own alt
// text as the hint. It returns "" for every reason a caption might not be
// available — no Describer, an unresolvable id, a vector format, an image below
// the size floor, the per-document cap, or a Describer error — because the
// caller's fallback in all of those cases is identical: emit what it emits
// today.
func (c *ooxmlCaptioner) caption(sourcePart, embedID, hint string) string {
	if c == nil || embedID == "" {
		return ""
	}
	rels, ok := c.rels[sourcePart]
	if !ok {
		rels = c.pkg.RelTargets(sourcePart)
		c.rels[sourcePart] = rels
	}
	part := ooxml.ResolveTarget(sourcePart, rels[embedID])
	if part == "" || !c.pkg.Has(part) {
		return ""
	}
	mime := ooxmlImageMime(part)
	if mime == "" {
		return "" // unknown or vector: no vision model can read it
	}

	// Bounded by ooxml.MaxPartSize; a media part that trips the cap comes back
	// as an error and simply yields no caption.
	b, err := c.pkg.Part(part)
	if err != nil || len(b) < ooxmlMinCaptionBytes {
		return ""
	}

	sum := sha256.Sum256(b)
	key := hex.EncodeToString(sum[:])
	if cached, ok := c.cache[key]; ok {
		return cached
	}
	if c.calls >= ooxmlMaxCaptionsPerDoc {
		return ""
	}
	c.calls++
	caption := describeImageWithHint(b, mime, hint, c.opts)
	c.cache[key] = caption
	return caption
}

// ooxmlImageMime maps a media part's extension to an image media type, and
// returns "" for anything a vision model cannot read. EMF, WMF and SVG are
// deliberately excluded: they are vector drawings, no hosted vision model
// accepts them, and sending one is a wasted call.
func ooxmlImageMime(part string) string {
	switch strings.ToLower(strings.TrimPrefix(path.Ext(part), ".")) {
	case "png":
		return "image/png"
	case "jpg", "jpeg", "jpe":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "tif", "tiff":
		return "image/tiff"
	case "bmp":
		return "image/bmp"
	default:
		return ""
	}
}
