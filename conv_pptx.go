package anymd

import (
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/muthuishere/anymd/internal/mdutil"
	"github.com/muthuishere/anymd/internal/ooxml"
)

func init() { addBuiltin(&PptxConverter{}) }

const (
	pptxMime             = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	pptxPresentationPart = "ppt/presentation.xml"
	pptxSlideDir         = "ppt/slides/"
	pptxNotesRelType     = "notesSlide"
)

// PptxConverter renders a PresentationML deck (.pptx) as Markdown using nothing
// but archive/zip and encoding/xml.
type PptxConverter struct{}

// Name identifies the converter in errors and in `anymd --list`.
func (c *PptxConverter) Name() string { return "pptx" }

// Accepts recognizes .pptx by extension, by the PresentationML mime type, or by
// sniffing the zip central directory for ppt/presentation.xml.
func (c *PptxConverter) Accepts(r io.ReadSeeker, info StreamInfo, opts *Options) bool {
	if info.HasExt(".pptx") {
		return true
	}
	if info.NormalizedMime() == pptxMime {
		return true
	}
	return ooxml.HasAnyPart(r, pptxPresentationPart)
}

// Convert renders every slide in presentation order as "## Slide n" followed by
// its shape text, tables, and speaker notes.
func (c *PptxConverter) Convert(r io.ReadSeeker, info StreamInfo, opts *Options) (Result, error) {
	pkg, err := ooxml.Open(r)
	if err != nil {
		return Result{}, err
	}

	slides := pptxSlideParts(pkg.Names())
	var blocks []string
	for i, part := range slides {
		data, err := pkg.Part(part)
		if err != nil {
			return Result{}, err
		}
		// The heading numbers slides by position in the deck, not by the digits
		// in the file name: a deleted slide leaves a gap in slideN.xml but not
		// in what a reader sees.
		blocks = append(blocks, mdutil.Heading(2, "Slide "+strconv.Itoa(i+1)))
		body, err := pptxSlideBlocks(data, pkg, part)
		if err != nil {
			return Result{}, fmt.Errorf("pptx: %s: %w", part, err)
		}
		blocks = append(blocks, body...)
		if n := pptxNotes(pkg, part); n != "" {
			blocks = append(blocks, n)
		}
	}
	return Result{Markdown: mdutil.Join(blocks...)}, nil
}

// pptxSlideParts returns ppt/slides/slideN.xml sorted NUMERICALLY. Sorting the
// names as strings is the classic pptx bug: it puts slide10 between slide1 and
// slide2.
func pptxSlideParts(names []string) []string {
	type slide struct {
		n    int
		name string
	}
	var found []slide
	for _, name := range names {
		rest, ok := strings.CutPrefix(name, pptxSlideDir)
		if !ok || strings.Contains(rest, "/") {
			continue
		}
		digits, ok := strings.CutPrefix(rest, "slide")
		if !ok {
			continue
		}
		digits, ok = strings.CutSuffix(digits, ".xml")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(digits)
		if err != nil || n < 0 {
			continue
		}
		found = append(found, slide{n: n, name: name})
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].n != found[j].n {
			return found[i].n < found[j].n
		}
		return found[i].name < found[j].name
	})
	out := make([]string, len(found))
	for i, s := range found {
		out[i] = s.name
	}
	return out
}

// pptxSlideBlocks walks one slide part and returns its Markdown blocks in shape
// order. A shape carrying a title placeholder becomes an H3; a graphic frame
// holding an a:tbl becomes a GFM table; a p:pic becomes an image with its
// authored alt text; and a c:chart reference is followed into the chart part.
//
// pkg and slidePart are needed because a chart is not inline: the slide only
// carries an r:id that has to be resolved through the slide's relationships.
func pptxSlideBlocks(data []byte, pkg *ooxml.Package, slidePart string) ([]string, error) {
	d := ooxml.NewDecoder(data)
	rels := pkg.RelTargets(slidePart)
	var blocks []string
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "sp":
			title, paras, err := pptxShape(d)
			if err != nil {
				return nil, err
			}
			for _, p := range paras {
				if title {
					if h := mdutil.Heading(3, p); h != "" {
						blocks = append(blocks, h)
					}
					continue
				}
				blocks = append(blocks, p)
			}
		case "tbl":
			rows, err := pptxTable(d)
			if err != nil {
				return nil, err
			}
			if len(rows) > 0 {
				if t := mdutil.Table(rows[0], rows[1:]); t != "" {
					blocks = append(blocks, t)
				}
			}
		case "pic":
			alt, err := pptxPictureAlt(d)
			if err != nil {
				return nil, err
			}
			if alt != "" {
				// No destination: the media part is not extracted, but the
				// authored description is often the only prose describing the
				// figure, so losing it loses real content.
				blocks = append(blocks, "!["+alt+"]()")
			}
		case "chart":
			id := ooxml.Attr(se, "id")
			if err := ooxml.SkipElement(d); err != nil && err != io.EOF {
				return nil, err
			}
			blocks = append(blocks, pptxChartBlocks(pkg, slidePart, rels[id])...)
		}
		// Anything else is descended into, which is how a:tbl inside a
		// p:graphicFrame and shapes inside a p:grpSp are reached.
	}
	return blocks, nil
}

// pptxShape consumes one p:sp, reporting whether it is a title placeholder and
// returning its non-empty paragraphs.
func pptxShape(d *xml.Decoder) (bool, []string, error) {
	var (
		title bool
		paras []string
	)
	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "ph":
				switch strings.ToLower(ooxml.Attr(t, "type")) {
				case "title", "ctrtitle":
					title = true
				}
				if err := ooxml.SkipElement(d); err != nil {
					if err == io.EOF {
						depth = 0
						continue
					}
					return false, nil, err
				}
			case "p":
				s, err := pptxParagraph(d)
				if err != nil {
					return false, nil, err
				}
				if s != "" {
					paras = append(paras, s)
				}
			default:
				depth++
			}
		case xml.EndElement:
			depth--
		}
	}
	return title, paras, nil
}

// pptxParagraph consumes one a:p and returns its collapsed text. a:br becomes a
// space, because a soft break inside a slide bullet is not a Markdown block.
func pptxParagraph(d *xml.Decoder) (string, error) {
	var sb strings.Builder
	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				s, err := ooxml.TextOf(d)
				if err != nil {
					return "", err
				}
				sb.WriteString(s)
			case "br":
				if err := ooxml.SkipElement(d); err != nil {
					if err == io.EOF {
						depth = 0
						continue
					}
					return "", err
				}
				sb.WriteString(" ")
			default:
				depth++
			}
		case xml.EndElement:
			depth--
		}
	}
	return mdutil.Collapse(sb.String()), nil
}

// pptxTable consumes one a:tbl and returns its cells as a rectangular grid.
func pptxTable(d *xml.Decoder) ([][]string, error) {
	var rows [][]string
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
				row, err := pptxTableRow(d)
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
	return padRectangular(rows), nil
}

// pptxTableRow consumes one a:tr. gridSpan is an attribute on a:tc here (not a
// child element as in docx), and a spanned-over cell is marked hMerge.
func pptxTableRow(d *xml.Decoder) ([]string, error) {
	row := []string{}
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
				span := 1
				if n, err := strconv.Atoi(strings.TrimSpace(ooxml.Attr(t, "gridSpan"))); err == nil && n > 1 && n <= 1000 {
					span = n
				}
				text, err := pptxTableCell(d)
				if err != nil {
					return nil, err
				}
				row = append(row, text)
				for i := 1; i < span; i++ {
					row = append(row, "")
				}
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

// pptxTableCell consumes one a:tc and returns its collapsed paragraph text.
func pptxTableCell(d *xml.Decoder) (string, error) {
	var paras []string
	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "p" {
				s, err := pptxParagraph(d)
				if err != nil {
					return "", err
				}
				if s != "" {
					paras = append(paras, s)
				}
				continue
			}
			depth++
		case xml.EndElement:
			depth--
		}
	}
	return mdutil.Collapse(strings.Join(paras, " ")), nil
}

// pptxNotes renders a slide's speaker notes as a blockquote, or "" when the
// slide has none. The notes part is found through the slide's relationships,
// falling back to the conventional notesSlideN.xml name.
func pptxNotes(pkg *ooxml.Package, slidePart string) string {
	notesPart := ""
	for _, rel := range pkg.Relationships(slidePart) {
		if rel.External() || !strings.HasSuffix(rel.Type, pptxNotesRelType) {
			continue
		}
		if p := ooxml.ResolveTarget(slidePart, rel.Target); p != "" && pkg.Has(p) {
			notesPart = p
			break
		}
	}
	if notesPart == "" {
		base := strings.TrimSuffix(strings.TrimPrefix(slidePart, pptxSlideDir), ".xml")
		if n, ok := strings.CutPrefix(base, "slide"); ok {
			guess := "ppt/notesSlides/notesSlide" + n + ".xml"
			if pkg.Has(guess) {
				notesPart = guess
			}
		}
	}
	if notesPart == "" {
		return ""
	}

	data := pkg.OptionalPart(notesPart)
	if len(data) == 0 {
		return ""
	}
	paras, err := pptxNotesParagraphs(data)
	if err != nil || len(paras) == 0 {
		return ""
	}

	var lines []string
	for i, p := range paras {
		if i == 0 {
			lines = append(lines, "> **Notes:** "+p)
			continue
		}
		lines = append(lines, ">", "> "+p)
	}
	return strings.Join(lines, "\n")
}

// pptxNotesParagraphs pulls the text out of a notes slide, skipping the
// placeholder that merely re-renders the slide image.
func pptxNotesParagraphs(data []byte) ([]string, error) {
	d := ooxml.NewDecoder(data)
	var out []string
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "sp" {
			continue
		}
		shapeType, paras, err := pptxNotesShape(d)
		if err != nil {
			return nil, err
		}
		if shapeType == "sldimg" || shapeType == "sldnum" {
			continue
		}
		out = append(out, paras...)
	}
	return out, nil
}

// pptxNotesShape is pptxShape with the placeholder type returned verbatim, so
// the caller can drop the slide-image and slide-number placeholders.
func pptxNotesShape(d *xml.Decoder) (string, []string, error) {
	var (
		phType string
		paras  []string
	)
	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "ph":
				phType = strings.ToLower(ooxml.Attr(t, "type"))
				if err := ooxml.SkipElement(d); err != nil {
					if err == io.EOF {
						depth = 0
						continue
					}
					return "", nil, err
				}
			case "p":
				s, err := pptxParagraph(d)
				if err != nil {
					return "", nil, err
				}
				if s != "" {
					paras = append(paras, s)
				}
			default:
				depth++
			}
		case xml.EndElement:
			depth--
		}
	}
	return phType, paras, nil
}

// pptxPictureAlt consumes one p:pic and returns its normalized alt text, taken
// from the descr attribute of p:cNvPr and falling back to its name.
func pptxPictureAlt(d *xml.Decoder) (string, error) {
	alt := ""
	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "cNvPr" && alt == "" {
				alt = ooxmlAltText(ooxml.Attr(t, "descr"), ooxml.Attr(t, "name"))
				if err := ooxml.SkipElement(d); err != nil {
					if err == io.EOF {
						depth = 0
						continue
					}
					return "", err
				}
				continue
			}
			depth++
		case xml.EndElement:
			depth--
		}
	}
	return alt, nil
}

// pptxChartBlocks follows a c:chart relationship into ppt/charts/chartN.xml and
// renders what a reader would actually see: the chart title, and the category x
// series grid from the cached values the producer embedded.
//
// The cache is the only readable source — the live data lives in an embedded
// workbook — so an uncached chart degrades to its title alone rather than
// failing the whole deck.
func pptxChartBlocks(pkg *ooxml.Package, slidePart, target string) []string {
	if target == "" {
		return nil
	}
	part := ooxml.ResolveTarget(slidePart, target)
	if part == "" || !pkg.Has(part) {
		return nil
	}
	data := pkg.OptionalPart(part)
	if len(data) == 0 {
		return nil
	}
	title, series, err := pptxParseChart(data)
	if err != nil {
		return nil
	}

	head := "Chart"
	if title != "" {
		head += ": " + title
	}
	blocks := []string{mdutil.Heading(3, head)}

	if len(series) == 0 {
		return blocks
	}
	header := []string{"Category"}
	rowCount := 0
	for _, s := range series {
		header = append(header, s.name)
		if len(s.cats) > rowCount {
			rowCount = len(s.cats)
		}
		if len(s.vals) > rowCount {
			rowCount = len(s.vals)
		}
	}
	if rowCount == 0 {
		return blocks
	}
	rows := make([][]string, 0, rowCount)
	for i := 0; i < rowCount; i++ {
		row := []string{""}
		for _, s := range series {
			if row[0] == "" && i < len(s.cats) {
				row[0] = s.cats[i]
			}
			v := ""
			if i < len(s.vals) {
				v = s.vals[i]
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

// pptxSeries is one c:ser reduced to the strings a Markdown table needs.
type pptxSeries struct {
	name string
	cats []string
	vals []string
}

// pptxParseChart pulls the title and every series out of a chart part. The
// first c:title in document order is the chart title; the ones that follow
// belong to the axes.
func pptxParseChart(data []byte) (string, []pptxSeries, error) {
	d := ooxml.NewDecoder(data)
	var (
		title  string
		haveT  bool
		series []pptxSeries
	)
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "title":
			if haveT {
				continue
			}
			haveT = true
			t, err := pptxChartTitle(d)
			if err != nil {
				return "", nil, err
			}
			title = t
		case "ser":
			s, err := pptxChartSeries(d)
			if err != nil {
				return "", nil, err
			}
			series = append(series, s)
		}
	}
	return title, series, nil
}

// pptxChartTitle consumes a c:title and joins its a:t runs.
func pptxChartTitle(d *xml.Decoder) (string, error) {
	var sb strings.Builder
	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "t" {
				s, err := ooxml.TextOf(d)
				if err != nil {
					return "", err
				}
				sb.WriteString(s)
				sb.WriteString(" ")
				continue
			}
			depth++
		case xml.EndElement:
			depth--
		}
	}
	return mdutil.Collapse(sb.String()), nil
}

// pptxChartSeries consumes one c:ser, reading its name from c:tx and its
// categories and values from the c:cat / c:val caches.
func pptxChartSeries(d *xml.Decoder) (pptxSeries, error) {
	var s pptxSeries
	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return s, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "tx":
				pts, err := pptxCachedPoints(d)
				if err != nil {
					return s, err
				}
				if len(pts) > 0 {
					s.name = pts[0]
				}
			case "cat":
				pts, err := pptxCachedPoints(d)
				if err != nil {
					return s, err
				}
				s.cats = pts
			case "val":
				pts, err := pptxCachedPoints(d)
				if err != nil {
					return s, err
				}
				s.vals = pts
			default:
				depth++
			}
		case xml.EndElement:
			depth--
		}
	}
	return s, nil
}

// maxChartPoints bounds a cached series: the idx attribute is attacker-
// controlled, so a single <c:pt idx="4000000000"/> must not allocate a slice.
const maxChartPoints = 100000

// pptxCachedPoints consumes an element and returns its <c:pt idx><c:v> values
// in index order, with gaps left empty.
func pptxCachedPoints(d *xml.Decoder) ([]string, error) {
	byIdx := map[int]string{}
	maxIdx := -1
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
			if t.Name.Local == "pt" {
				idx, convErr := strconv.Atoi(strings.TrimSpace(ooxml.Attr(t, "idx")))
				if convErr != nil || idx < 0 || idx >= maxChartPoints {
					idx = -1
				}
				v, err := pptxPointValue(d)
				if err != nil {
					return nil, err
				}
				if idx >= 0 {
					byIdx[idx] = v
					if idx > maxIdx {
						maxIdx = idx
					}
				}
				continue
			}
			depth++
		case xml.EndElement:
			depth--
		}
	}
	if maxIdx < 0 {
		return nil, nil
	}
	out := make([]string, maxIdx+1)
	for i := range out {
		out[i] = byIdx[i]
	}
	return out, nil
}

// pptxPointValue consumes a c:pt and returns its c:v text.
func pptxPointValue(d *xml.Decoder) (string, error) {
	var sb strings.Builder
	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "v" {
				s, err := ooxml.TextOf(d)
				if err != nil {
					return "", err
				}
				sb.WriteString(s)
				continue
			}
			depth++
		case xml.EndElement:
			depth--
		}
	}
	return mdutil.Collapse(sb.String()), nil
}
