package anymd

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/strikethrough"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
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
	for _, marker := range []string{"<!doctype html", "<html", "<head", "<body"} {
		if strings.Contains(s, marker) {
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
			table.NewTablePlugin(),
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
