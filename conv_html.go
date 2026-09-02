package anymd

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/marker"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/strikethrough"
	"github.com/muthuishere/anymd/internal/mdutil"
	"github.com/saintfish/chardet"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/net/html/charset"
	"golang.org/x/text/encoding"
	"golang.org/x/text/transform"
)

func init() { addBuiltin(&HTMLConverter{}) }

// htmlMaxBytes bounds how much of a stream we will buffer. HTML is parsed
// whole (the parser has no streaming mode we can use), so the cap is what
// stops a hostile multi-gigabyte "page" from being an OOM.
const htmlMaxBytes = 256 << 20 // 256 MiB

// HTMLConverter turns an HTML page into GitHub-flavored Markdown.
//
// It sits at PriorityGeneric because "looks like markup" is a broad claim:
// docx, xlsx and epub are all zip-of-XML and must be given first refusal.
type HTMLConverter struct{}

// Name implements Named.
func (c *HTMLConverter) Name() string { return "html" }

// Priority implements Prioritized. Generic, so specific markup-bearing
// container formats are asked first.
func (c *HTMLConverter) Priority() int { return PriorityGeneric }

// Accepts recognizes HTML from the extension or mime hint, and otherwise from
// a cheap sniff of the head of the stream. It deliberately does NOT claim
// ".xml": a bare XML document is somebody else's format (a feed, an OPF, an
// office part), and claiming it here would shadow those converters.
func (c *HTMLConverter) Accepts(r io.ReadSeeker, info StreamInfo, opts *Options) bool {
	if info.HasExt(".html", ".htm", ".xhtml", ".xht") {
		return true
	}
	if info.HasMimePrefix("text/html", "application/xhtml+xml") {
		return true
	}
	// Sniff. Only a real HTML opener counts; "<" alone is not evidence.
	var head [1024]byte
	n, _ := io.ReadFull(r, head[:])
	if n <= 0 {
		return false
	}
	s := strings.ToLower(string(head[:n]))
	for _, opener := range []string{"<!doctype html", "<html", "<head", "<body"} {
		if strings.Contains(s, opener) {
			return true
		}
	}
	return false
}

// Convert decodes the stream to UTF-8, strips non-content markup, and renders
// GitHub-flavored Markdown.
func (c *HTMLConverter) Convert(r io.ReadSeeker, info StreamInfo, opts *Options) (Result, error) {
	raw, err := io.ReadAll(io.LimitReader(r, htmlMaxBytes+1))
	if err != nil {
		return Result{}, err
	}
	if len(raw) > htmlMaxBytes {
		return Result{}, fmt.Errorf("html: input exceeds %d bytes", htmlMaxBytes)
	}

	declared := ""
	if opts != nil && opts.Charset != "" {
		declared = opts.Charset
	} else if info.Charset != "" {
		declared = info.Charset
	}
	src := DecodeHTMLBytes(raw, declared)

	doc, err := htmlParse(src)
	if err != nil {
		return Result{}, err
	}
	title := htmlTitle(doc)
	htmlStrip(doc)

	md, err := renderHTMLNode(doc, info.URL)
	if err != nil {
		return Result{}, err
	}
	return Result{Markdown: mdutil.Join(md), Title: title}, nil
}

// HTMLToMarkdown is the shared HTML path: every converter whose payload is
// HTML (feed item bodies, epub spine parts, mail parts, …) renders through
// this one function, so all of anymd's HTML comes out identically.
//
// htmlSrc must already be UTF-8 (use DecodeHTMLBytes if you are holding raw
// bytes). baseURL, when non-empty, is used to turn relative href/src values
// into absolute URLs so links in a fetched page stay usable; pass "" to leave
// them relative. Nothing is ever fetched — an <img src> becomes a link, never
// a network request.
//
// The returned markdown has no trailing newline; compose it with mdutil.Join.
func HTMLToMarkdown(htmlSrc string, baseURL string) (string, error) {
	doc, err := htmlParse(htmlSrc)
	if err != nil {
		return "", err
	}
	htmlStrip(doc)
	return renderHTMLNode(doc, baseURL)
}

// HTMLTitle returns the document title of an HTML fragment: the <title>
// element, or the first <h1> when there is no title. It returns "" when
// neither is present or the input does not parse.
func HTMLTitle(htmlSrc string) string {
	doc, err := htmlParse(htmlSrc)
	if err != nil {
		return ""
	}
	return htmlTitle(doc)
}

// DecodeHTMLBytes turns raw page bytes into a UTF-8 string.
//
// Precedence is deliberate, because mojibake is the single most common
// HTML-conversion complaint: an explicitly declared charset (from the
// transport or the caller) wins, then the document's own BOM / <meta charset>
// / <meta http-equiv="content-type"> declaration, then statistical detection
// for bytes that are not valid UTF-8, and finally UTF-8 is assumed. A label we
// cannot look up is skipped rather than treated as fatal.
func DecodeHTMLBytes(raw []byte, declared string) string {
	return strings.TrimPrefix(decodeHTMLBytes(raw, declared), "\ufeff")
}

// decodeHTMLBytes is DecodeHTMLBytes without the leading-BOM trim, which the
// caller applies once: a U+FEFF that survives the transcode is a byte-order
// mark, not content, and left in place it becomes a stray invisible rune at
// the top of every converted document.
func decodeHTMLBytes(raw []byte, declared string) string {
	if s, ok := htmlDecodeLabel(raw, declared); ok {
		return s
	}
	// A BOM is authoritative and beats anything the markup claims.
	if enc, _, certain := charset.DetermineEncoding(raw, ""); certain && enc != nil {
		if s, ok := htmlDecodeEncoding(raw, enc); ok {
			return s
		}
	}
	// The document's own <meta charset> / <meta http-equiv="content-type">.
	// This is scanned here rather than taken from charset.DetermineEncoding
	// because that function reports certain=false for a meta declaration and
	// falls back to windows-1252, which is indistinguishable from a real
	// windows-1252 declaration — and guessing wrong here IS the mojibake bug.
	if s, ok := htmlDecodeLabel(raw, htmlMetaCharset(raw)); ok {
		return s
	}
	if utf8.Valid(raw) {
		return string(raw)
	}
	if det, err := chardet.NewHtmlDetector().DetectBest(raw); err == nil && det != nil {
		if s, ok := htmlDecodeLabel(raw, det.Charset); ok {
			return s
		}
	}
	// Last resort: hand the bytes back. Invalid sequences become U+FFFD at the
	// first place that ranges over them, which beats returning an error for a
	// page we can still mostly read.
	return string(raw)
}

// htmlMetaPrescan is how far into the document we look for a charset
// declaration. The HTML spec's own prescan limit is 1024 bytes; we allow more
// because real pages push meta tags past a pile of conditional comments.
const htmlMetaPrescan = 8192

// htmlMetaCharset finds the encoding a document declares about itself, from
// either <meta charset="..."> or <meta http-equiv="Content-Type"
// content="text/html; charset=...">. It returns "" when nothing is declared.
//
// Only <meta> tags are considered: a "charset=" inside a URL or a script would
// otherwise hijack the whole document's decoding.
func htmlMetaCharset(raw []byte) string {
	window := raw
	if len(window) > htmlMetaPrescan {
		window = window[:htmlMetaPrescan]
	}
	s := strings.ToLower(string(window))
	for i := 0; ; {
		j := strings.Index(s[i:], "<meta")
		if j < 0 {
			return ""
		}
		i += j + len("<meta")
		end := strings.IndexByte(s[i:], '>')
		tag := s[i:]
		if end >= 0 {
			tag = s[i : i+end]
			i += end + 1
		} else {
			i = len(s)
		}
		if label := charsetInAttrs(tag); label != "" {
			return label
		}
		if i >= len(s) {
			return ""
		}
	}
}

// charsetInAttrs pulls the value of a "charset=" occurrence out of one tag's
// attribute text, handling both the bare attribute and the one nested inside a
// content="text/html; charset=..." value.
func charsetInAttrs(tag string) string {
	k := strings.Index(tag, "charset")
	if k < 0 {
		return ""
	}
	rest := tag[k+len("charset"):]
	rest = strings.TrimLeft(rest, " \t\r\n")
	if !strings.HasPrefix(rest, "=") {
		return ""
	}
	rest = strings.TrimLeft(rest[1:], " \t\r\n\"'")
	end := strings.IndexFunc(rest, func(r rune) bool {
		return !(r == '-' || r == '_' || r == '.' || r == ':' || r == '+' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'))
	})
	if end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// htmlDecodeLabel transcodes raw using the named encoding. It reports false
// when the label is empty, unknown, or the transform fails.
func htmlDecodeLabel(raw []byte, label string) (string, bool) {
	if strings.TrimSpace(label) == "" {
		return "", false
	}
	enc, _ := charset.Lookup(strings.TrimSpace(label))
	if enc == nil {
		return "", false
	}
	return htmlDecodeEncoding(raw, enc)
}

func htmlDecodeEncoding(raw []byte, enc encoding.Encoding) (string, bool) {
	out, err := io.ReadAll(transform.NewReader(bytes.NewReader(raw), enc.NewDecoder()))
	if err != nil {
		return "", false
	}
	return string(out), true
}

// htmlParse wraps html.Parse with a recover. The parser is not supposed to
// panic, but it is the component pointed straight at hostile bytes and the
// contract is absolute: malformed input is an error, never a crash.
func htmlParse(src string) (doc *html.Node, err error) {
	defer func() {
		if r := recover(); r != nil {
			doc, err = nil, fmt.Errorf("html: parse panicked: %v", r)
		}
	}()
	return html.Parse(strings.NewReader(src))
}

// htmlTitle prefers <title>, falling back to the first <h1>.
func htmlTitle(doc *html.Node) string {
	if t := htmlFirstElementText(doc, atom.Title); t != "" {
		return t
	}
	return htmlFirstElementText(doc, atom.H1)
}

func htmlFirstElementText(n *html.Node, want atom.Atom) string {
	var found string
	var walk func(*html.Node) bool
	walk = func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.DataAtom == want {
			if t := mdutil.Collapse(htmlNodeText(n)); t != "" {
				found = t
				return true
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if walk(c) {
				return true
			}
		}
		return false
	}
	walk(n)
	return found
}

func htmlNodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// htmlStrip removes everything that is markup plumbing rather than content:
// <script>, <style>, <noscript>, HTML comments, and every child of <head>
// except <title>. Doing this BEFORE conversion is what stops CSS and JS source
// from leaking into the markdown body.
func htmlStrip(root *html.Node) {
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		var next *html.Node
		for c := n.FirstChild; c != nil; c = next {
			next = c.NextSibling
			switch {
			case c.Type == html.CommentNode:
				n.RemoveChild(c)
				continue
			case c.Type == html.ElementNode:
				switch c.DataAtom {
				case atom.Script, atom.Style, atom.Noscript:
					n.RemoveChild(c)
					continue
				}
				if n.Type == html.ElementNode && n.DataAtom == atom.Head && c.DataAtom != atom.Title {
					n.RemoveChild(c)
					continue
				}
			}
			walk(c)
		}
	}
	walk(root)
}

// htmlConv is built once: the converter is documented as safe for concurrent
// use, and constructing the plugin set per call is pure waste.
var htmlConv = sync.OnceValue(func() *converter.Converter {
	return converter.NewConverter(
		converter.WithPlugins(
			base.NewBasePlugin(),
			commonmark.NewCommonmarkPlugin(),
			// GitHub-flavored extras: real pipe tables and ~~strikethrough~~.
			htmlTablePlugin{},
			strikethrough.NewStrikethroughPlugin(),
		),
	)
})

// renderHTMLNode converts a parsed document, resolving relative URLs against
// baseURL when one is given.
func renderHTMLNode(doc *html.Node, baseURL string) (md string, err error) {
	defer func() {
		if r := recover(); r != nil {
			md, err = "", fmt.Errorf("html: convert panicked: %v", r)
		}
	}()
	var opts []converter.ConvertOptionFunc
	if strings.TrimSpace(baseURL) != "" {
		opts = append(opts, converter.WithDomain(strings.TrimSpace(baseURL)))
	}
	out, err := htmlConv().ConvertNode(doc, opts...)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// - - - - - - - - - - - - - - tables - - - - - - - - - - - - - - //
//
// Tables are rendered here rather than by html-to-markdown's `table` plugin.
// That plugin gives up on the shapes real HTML is full of — a table with no
// <th> anywhere, a cell holding a <p> or a <ul>, a cell with a <br> in it —
// and falls back to emitting the cell text as loose paragraphs, which is a
// table silently deleted at exit 0. Measured against docling's ground truth
// (bench/run-quality.sh, ADR 0003) that cost html a tables score of 0.69
// against markitdown's 0.98: eight of the corpus's twenty-two tables came out
// as no table at all.
//
// Rendering it ourselves also satisfies the rule in CONTRACT.md that every
// table in the project goes through mdutil.Table, so a table lifted out of an
// .html page, an .xlsx sheet and a .docx body are byte-identical.

// htmlTableCtxKey marks the render context as being inside a table cell.
// GFM has no nested tables, so a <table> found under this flattens to text —
// which is also what docling does.
type htmlTableCtxKey struct{}

// Bounds on the grid a single <table> may expand to. rowspan/colspan are
// attacker-controlled integers and a pair of them multiply, so a 30-byte
// table can otherwise ask for billions of cells.
const (
	htmlTableMaxCols  = 512
	htmlTableMaxRows  = 4096
	htmlTableMaxCells = 1 << 16
)

// htmlTablePlugin is the converter plugin that installs the table renderer.
type htmlTablePlugin struct{}

func (htmlTablePlugin) Name() string { return "anymd-table" }

func (htmlTablePlugin) Init(conv *converter.Converter) error {
	// Keep the pipe escaped in ordinary prose, as the upstream plugin did.
	conv.Register.EscapedChar('|')

	// The table parts are block-level: that is what makes the whitespace
	// collapse treat a cell boundary as a boundary, so <td>A</td><td>B</td>
	// does not come out as one run of text.
	for _, tag := range []string{"table", "thead", "tbody", "tfoot", "tr", "td", "th", "caption"} {
		conv.Register.TagType(tag, converter.TagTypeBlock, converter.PriorityStandard)
	}
	conv.Register.Renderer(htmlRenderTable, converter.PriorityStandard)
	conv.Register.Renderer(htmlRenderCaptionedImage, converter.PriorityEarly)
	return nil
}

// htmlRenderTable renders one <table> as a GFM pipe table via mdutil.Table.
func htmlRenderTable(ctx converter.Context, w converter.Writer, n *html.Node) converter.RenderStatus {
	if n.Type != html.ElementNode || n.DataAtom != atom.Table {
		return converter.RenderTryNext
	}
	// A layout table is not data. Let the generic fallback render its content
	// as ordinary flow, which is what the upstream plugin did by default.
	switch strings.ToLower(htmlAttr(n, "role")) {
	case "presentation", "none":
		return converter.RenderTryNext
	}
	if ctx.Value(htmlTableCtxKey{}) != nil {
		// Nested table: flatten to its text, in document order.
		_, _ = w.WriteString(htmlTableFlatText(n))
		return converter.RenderSuccess
	}

	grid := htmlTableGrid(n)
	caption := ""
	if c := htmlTableCaption(n); c != nil {
		caption = mdutil.Collapse(htmlNodeText(c))
	}
	if len(grid) == 0 && caption == "" {
		// Nothing to render, but the node is handled: an empty <table> must
		// not fall through to the generic block fallback and emit blank lines.
		return converter.RenderSuccess
	}

	inner := ctx.WithValue(htmlTableCtxKey{}, true)
	rendered := make(map[*html.Node]string, len(grid))
	rows := make([][]string, len(grid))
	for i, row := range grid {
		rows[i] = make([]string, len(row))
		for j, cell := range row {
			if cell == nil {
				continue
			}
			md, ok := rendered[cell]
			if !ok {
				md = htmlCellMarkdown(inner, cell)
				rendered[cell] = md
			}
			rows[i][j] = md
		}
	}

	_, _ = w.WriteString("\n\n")
	if caption != "" {
		_, _ = w.WriteString(caption)
		_, _ = w.WriteString("\n\n")
	}
	// header is nil so mdutil.Table promotes the first row, which is what a
	// GFM table requires and what docling's ground truth does for a table
	// whose first row is <td> rather than <th>.
	_, _ = w.WriteString(mdutil.Table(nil, rows))
	_, _ = w.WriteString("\n\n")
	return converter.RenderSuccess
}

// htmlRenderCaptionedImage drops the alt text of an <img> that sits in a
// <figure> with a <figcaption>. The caption already describes the image, so
// inlining the alt as well emits the same description twice — which is both
// noise and, measured against docling's ground truth, a content-F1 loss.
// Every other image keeps its alt: it is the only text an image contributes.
func htmlRenderCaptionedImage(ctx converter.Context, w converter.Writer, n *html.Node) converter.RenderStatus {
	if n.Type != html.ElementNode || n.DataAtom != atom.Img || !htmlHasFigcaption(n) {
		return converter.RenderTryNext
	}
	src := strings.TrimSpace(htmlAttr(n, "src"))
	if src == "" {
		return converter.RenderSuccess
	}
	_, _ = w.WriteString("![](")
	_, _ = w.WriteString(ctx.AssembleAbsoluteURL(ctx, "img", src))
	_, _ = w.WriteString(")")
	return converter.RenderSuccess
}

// htmlHasFigcaption reports whether the node has a <figure> ancestor that
// carries a <figcaption>.
func htmlHasFigcaption(n *html.Node) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type != html.ElementNode || p.DataAtom != atom.Figure {
			continue
		}
		for c := p.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && c.DataAtom == atom.Figcaption {
				return true
			}
		}
	}
	return false
}

// htmlCellMarkdown renders one cell's children to the inline markdown that
// goes inside a pipe. Block children (<p>, <ul>, <pre>) render normally and
// are flattened to one line by mdutil.EscapeCell, since a newline anywhere
// inside a GFM table terminates it.
func htmlCellMarkdown(ctx converter.Context, cell *html.Node) string {
	htmlDemoteHeadings(cell)

	var buf strings.Builder
	ctx.RenderChildNodes(ctx, &buf, cell)
	s := buf.String()
	// Code blocks carry their newlines as a private marker rune that is only
	// turned back into a newline after rendering; flatten those too, or the
	// table splits apart later.
	s = strings.ReplaceAll(s, string(marker.MarkerCodeBlockNewline), " ")
	// A pipe inside a cell is escaped by mdutil.EscapeCell, so drop the
	// renderer's own pending escape for it — leaving both in place emits the
	// backslash twice.
	s = strings.ReplaceAll(s, string(marker.MarkerEscaping)+"|", "|")
	return strings.ReplaceAll(s, "\r", " ")
}

// htmlDemoteHeadings turns h1..h6 inside a table cell into paragraphs. A
// heading is a document-level structure; "# A" inside a pipe is not a heading
// and would only add stray hashes to the cell.
func htmlDemoteHeadings(cell *html.Node) {
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode {
				switch c.DataAtom {
				case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
					c.DataAtom, c.Data = atom.P, "p"
				}
			}
			walk(c)
		}
	}
	walk(cell)
}

// htmlTableFlatText renders a table as plain text for a context that cannot
// hold one — inside another table's cell, since GFM has no nested tables.
//
// It cannot use htmlNodeText: by the time the renderer runs, the whitespace
// between </td><td> has already been collapsed away, so concatenating the text
// nodes would weld "A1" and "B1" into "A1B1". A separator is emitted at every
// cell, row and line boundary instead.
func htmlTableFlatText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch {
		case n.Type == html.TextNode:
			b.WriteString(n.Data)
			return
		case n.Type == html.ElementNode && !htmlInlineRun[n.DataAtom]:
			// Every element boundary except a pure character-styling one is a
			// word boundary here.
			b.WriteByte(' ')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return mdutil.Collapse(b.String())
}

// htmlInlineRun lists the elements that only style the characters they wrap,
// and so do not separate one word from the next.
var htmlInlineRun = map[atom.Atom]bool{
	atom.B: true, atom.Strong: true, atom.I: true, atom.Em: true,
	atom.U: true, atom.S: true, atom.Strike: true, atom.Del: true,
	atom.Ins: true, atom.Mark: true, atom.Small: true, atom.Big: true,
	atom.Sub: true, atom.Sup: true, atom.Font: true, atom.Span: true,
	atom.Abbr: true, atom.Bdi: true, atom.Bdo: true, atom.Wbr: true,
}

// htmlTableCaption returns the table's own <caption>, or nil.
func htmlTableCaption(table *html.Node) *html.Node {
	for c := table.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.DataAtom == atom.Caption {
			return c
		}
	}
	return nil
}

// htmlTableGrid expands a table into a rectangular grid of cell nodes,
// mirroring a rowspan/colspan cell into every position it covers. Mirroring
// rather than blanking is deliberate: it is what docling's ground truth does,
// and it keeps a spanned header label attached to every column it labels.
//
// The returned rows all have the same length; a position no cell reached is
// nil.
func htmlTableGrid(table *html.Node) [][]*html.Node {
	rows := htmlTableRows(table)
	if len(rows) > htmlTableMaxRows {
		rows = rows[:htmlTableMaxRows]
	}
	if len(rows) == 0 {
		return nil
	}
	grid := make([][]*html.Node, len(rows))
	at := func(r, c int) *html.Node {
		if r < len(grid) && c < len(grid[r]) {
			return grid[r][c]
		}
		return nil
	}
	set := func(r, c int, n *html.Node) {
		for len(grid[r]) <= c {
			grid[r] = append(grid[r], nil)
		}
		grid[r][c] = n
	}

	placed := 0
	for r, tr := range rows {
		col := 0
		for _, cell := range htmlRowCells(tr) {
			for col < htmlTableMaxCols && at(r, col) != nil {
				col++
			}
			if col >= htmlTableMaxCols || placed >= htmlTableMaxCells {
				break
			}
			rs := htmlSpan(cell, "rowspan", len(rows)-r)
			cs := htmlSpan(cell, "colspan", htmlTableMaxCols-col)
			for dr := 0; dr < rs && placed < htmlTableMaxCells; dr++ {
				for dc := 0; dc < cs && placed < htmlTableMaxCells; dc++ {
					set(r+dr, col+dc, cell)
					placed++
				}
			}
			col += cs
		}
	}

	width := 0
	for _, row := range grid {
		if len(row) > width {
			width = len(row)
		}
	}
	if width == 0 {
		return nil
	}
	out := make([][]*html.Node, 0, len(grid))
	for _, row := range grid {
		full := make([]*html.Node, width)
		copy(full, row)
		empty := true
		for _, c := range full {
			if c != nil {
				empty = false
				break
			}
		}
		if empty {
			continue
		}
		out = append(out, full)
	}
	return out
}

// htmlTableRows collects the <tr> elements belonging to this table, in
// document order, without descending into a nested table's rows.
func htmlTableRows(table *html.Node) []*html.Node {
	var rows []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			switch c.DataAtom {
			case atom.Table:
				// belongs to the nested table, not to us
			case atom.Tr:
				rows = append(rows, c)
			default:
				walk(c)
			}
		}
	}
	walk(table)
	return rows
}

// htmlRowCells returns the <td>/<th> children of one row.
func htmlRowCells(tr *html.Node) []*html.Node {
	var cells []*html.Node
	for c := tr.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && (c.DataAtom == atom.Td || c.DataAtom == atom.Th) {
			cells = append(cells, c)
		}
	}
	return cells
}

// htmlSpan reads a rowspan/colspan attribute, clamped to [1, max]. HTML's
// rowspan="0" means "to the end of the section", which clamps to max here;
// anything unparseable is one cell.
func htmlSpan(cell *html.Node, name string, max int) int {
	if max < 1 {
		return 1
	}
	v := strings.TrimSpace(htmlAttr(cell, name))
	if v == "" {
		return 1
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 1
	}
	if n == 0 || n > max {
		return max
	}
	return n
}

// htmlAttr returns an element's attribute value, or "".
func htmlAttr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}
