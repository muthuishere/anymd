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
		body, err := pptxSlideBlocks(data)
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
// holding an a:tbl becomes a GFM table.
func pptxSlideBlocks(data []byte) ([]string, error) {
	d := ooxml.NewDecoder(data)
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
