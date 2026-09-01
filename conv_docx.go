package anymd

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
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
		rels:     pkg.RelTargets(docxDocumentPart),
		numbered: docxNumberFormats(pkg.OptionalPart(docxNumberingPar)),
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
	rels     map[string]string // r:id -> hyperlink target
	numbered map[string]bool   // "numId:ilvl" and "numId" -> is an ordered list
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

	var (
		blocks   []string
		list     []string
		counters []int
	)
	flushList := func() {
		if len(list) > 0 {
			blocks = append(blocks, strings.Join(list, "\n"))
			list = nil
			counters = counters[:0]
		}
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
				if p.list {
					list = append(list, dc.listItem(p, &counters))
					continue
				}
				flushList()
				if b := dc.block(p); b != "" {
					blocks = append(blocks, b)
				}
			case "tbl":
				rows, err := dc.table(d)
				if err != nil {
					return nil, fmt.Errorf("docx: %w", err)
				}
				flushList()
				if len(rows) > 0 {
					if tbl := mdutil.Table(rows[0], rows[1:]); tbl != "" {
						blocks = append(blocks, tbl)
					}
				}
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
				seg, err := dc.run(d)
				if err != nil {
					return p, err
				}
				p.segs = append(p.segs, seg)
			case "hyperlink":
				seg, err := dc.hyperlink(d, t)
				if err != nil {
					return p, err
				}
				p.segs = append(p.segs, seg)
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

// run consumes one w:r and returns its text plus its bold/italic state.
func (dc *docxCtx) run(d *xml.Decoder) (docxSeg, error) {
	var seg docxSeg
	var sb strings.Builder
	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return seg, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "rPr":
				b, i, err := runProps(d)
				if err != nil {
					return seg, err
				}
				seg.bold, seg.ital = b, i
			case "t":
				preserve := ooxml.Attr(t, "space") == "preserve"
				s, err := ooxml.TextOf(d)
				if err != nil {
					return seg, err
				}
				if !preserve {
					s = strings.TrimSpace(s)
				}
				sb.WriteString(s)
			case "br", "cr":
				if err := ooxml.SkipElement(d); err != nil && err != io.EOF {
					return seg, err
				}
				sb.WriteString("\n")
			case "tab":
				if err := ooxml.SkipElement(d); err != nil && err != io.EOF {
					return seg, err
				}
				sb.WriteString(" ")
			case "delText":
				// Deleted text from a tracked change is not part of the document.
				if err := ooxml.SkipElement(d); err != nil && err != io.EOF {
					return seg, err
				}
			default:
				if err := ooxml.SkipElement(d); err != nil {
					if err == io.EOF {
						depth = 0
						continue
					}
					return seg, err
				}
			}
		case xml.EndElement:
			depth--
		}
	}
	seg.text = sb.String()
	return seg, nil
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
				seg, err := dc.run(d)
				if err != nil {
					return docxSeg{}, err
				}
				segs = append(segs, seg)
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

// table consumes one w:tbl and returns its cells as a rectangular grid.
func (dc *docxCtx) table(d *xml.Decoder) ([][]string, error) {
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
	return padRectangular(rows), nil
}

// tableRow consumes one w:tr, expanding w:gridSpan into padding cells so the
// row keeps the grid's real column count.
func (dc *docxCtx) tableRow(d *xml.Decoder) ([]string, error) {
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
				text, span, err := dc.tableCell(d)
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

// tableCell consumes one w:tc, returning its collapsed paragraph text and its
// horizontal span.
func (dc *docxCtx) tableCell(d *xml.Decoder) (string, int, error) {
	span := 1
	var paras []string
	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", span, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "gridSpan":
				if n, err := strconv.Atoi(strings.TrimSpace(ooxml.Attr(t, "val"))); err == nil && n > 1 && n <= 1000 {
					span = n
				}
				depth++
			case "p":
				p, err := dc.paragraph(d)
				if err != nil {
					return "", span, err
				}
				if s := mdutil.Collapse(plainDocxSegs(p.segs)); s != "" {
					paras = append(paras, s)
				}
			case "tbl":
				// A nested table cannot be rendered inside a GFM cell; drop it
				// rather than emit a table that is no longer rectangular.
				if err := ooxml.SkipElement(d); err != nil {
					if err == io.EOF {
						depth = 0
						continue
					}
					return "", span, err
				}
			default:
				depth++
			}
		case xml.EndElement:
			depth--
		}
	}
	return mdutil.Collapse(strings.Join(paras, " ")), span, nil
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
