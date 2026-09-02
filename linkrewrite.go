package anymd

// Rewriting a mirrored site's links.
//
// The whole file is a text transformation. It is deliberately conservative:
// anything it does not fully understand it copies through byte for byte, so the
// worst outcome is a link that stays absolute — never a corrupted document and
// never a panic. That asymmetry is the design. A crawl feeds this function
// attacker-influenced Markdown, and "left it alone" is always a safe answer
// while "half-rewrote it" is not.

import (
	"net/url"
	"path"
	"regexp"
	"strings"
)

// RewriteLinks rewrites Markdown links that point at crawled URLs so they refer
// to the local files those pages were written to.
//
// It is a pure function over text: no network, no filesystem. That is what
// makes a crawl's output self-contained — a mirrored site whose links still
// point at the internet is not a mirror.
//
// from is the URL the document was fetched from, used to resolve relative
// targets. mapping is url -> path-relative-to-the-output-root. A link with no
// entry in mapping is left exactly as it was, absolute, because a link to a
// page we did not fetch must still work.
func RewriteLinks(md, from string, mapping map[string]string) (out string) {
	// Malformed Markdown is input, not an error. A slice bound this parser got
	// wrong must degrade to "unrewritten document", not take the crawl down.
	defer func() {
		if r := recover(); r != nil {
			out = md
		}
	}()

	if md == "" || len(mapping) == 0 {
		return md
	}
	r := newRewriter(from, mapping)
	return r.document(md)
}

// rewriter carries the per-document state: the base URL relative targets
// resolve against, the url -> local path mapping, and where the current
// document itself was written, which is what every emitted path is relative to.
type rewriter struct {
	base     *url.URL
	mapping  map[string]string
	fromPath string
}

func newRewriter(from string, mapping map[string]string) *rewriter {
	r := &rewriter{mapping: mapping}
	if u, err := url.Parse(from); err == nil && u.Scheme != "" {
		r.base = u
		if p, ok := r.lookup(u); ok {
			r.fromPath = p
		}
	}
	return r
}

// ---------------------------------------------------------------------------
// Block level: find the parts of the document that are prose.
// ---------------------------------------------------------------------------

// refDefRe matches a link reference definition: `[id]: target "optional title"`.
// The target is captured separately so only it is ever replaced.
var refDefRe = regexp.MustCompile(`^( {0,3}\[(?:[^\[\]\\]|\\.)*\]:[ \t]*)(<[^<>]*>|[^ \t<]\S*)([ \t].*)?$`)

// document walks the text line by line, skipping fenced and indented code
// blocks. A URL in a code sample is content, not navigation: rewriting it would
// silently corrupt the very example a reader is about to copy.
func (r *rewriter) document(md string) string {
	lines := strings.Split(md, "\n")

	var (
		fenceChar   byte // 0 when not inside a fenced block
		fenceLen    int
		inIndented  bool
		prevBlank   = true
		outputLines = make([]string, 0, len(lines))
	)

	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		blank := strings.TrimSpace(line) == ""

		if fenceChar != 0 {
			// Inside a fence: only a matching closing fence ends it.
			if isFenceMarker(trimmed, fenceChar, fenceLen) && indent < 4 {
				fenceChar, fenceLen = 0, 0
			}
			outputLines = append(outputLines, line)
			prevBlank = blank
			continue
		}

		if indent < 4 {
			if c, n, ok := openingFence(trimmed); ok {
				fenceChar, fenceLen = c, n
				inIndented = false
				outputLines = append(outputLines, line)
				prevBlank = blank
				continue
			}
		}

		// An indented code block starts on a 4-space line that does not
		// interrupt a paragraph, and blank lines inside it do not end it.
		switch {
		case blank:
			// leave inIndented as it is
		case indent >= 4 && (prevBlank || inIndented):
			inIndented = true
		default:
			inIndented = false
		}
		if inIndented && !blank {
			outputLines = append(outputLines, line)
			prevBlank = blank
			continue
		}

		outputLines = append(outputLines, r.line(line))
		prevBlank = blank
	}

	return strings.Join(outputLines, "\n")
}

func openingFence(trimmed string) (byte, int, bool) {
	if len(trimmed) < 3 {
		return 0, 0, false
	}
	c := trimmed[0]
	if c != '`' && c != '~' {
		return 0, 0, false
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == c {
		n++
	}
	if n < 3 {
		return 0, 0, false
	}
	// A ``` info string may not itself contain a backtick.
	if c == '`' && strings.ContainsRune(trimmed[n:], '`') {
		return 0, 0, false
	}
	return c, n, true
}

func isFenceMarker(trimmed string, c byte, min int) bool {
	n := 0
	for n < len(trimmed) && trimmed[n] == c {
		n++
	}
	return n >= min && strings.TrimSpace(trimmed[n:]) == ""
}

// line rewrites one prose line: either a reference definition, or inline spans.
func (r *rewriter) line(s string) string {
	if m := refDefRe.FindStringSubmatch(s); m != nil {
		head, target, tail := m[1], m[2], m[3]
		angle := strings.HasPrefix(target, "<") && strings.HasSuffix(target, ">")
		dest := target
		if angle {
			dest = target[1 : len(target)-1]
		}
		if next, ok := r.rewriteTarget(dest); ok {
			return head + renderDest(next, angle) + tail
		}
		return s
	}
	return r.inline(s)
}

// ---------------------------------------------------------------------------
// Inline level.
// ---------------------------------------------------------------------------

// inline scans one line for links and images, stepping over code spans and
// backslash escapes so neither is mistaken for markup.
func (r *rewriter) inline(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	for i := 0; i < len(s); {
		switch s[i] {
		case '\\':
			b.WriteByte(s[i])
			if i+1 < len(s) {
				b.WriteByte(s[i+1])
				i += 2
			} else {
				i++
			}
		case '`':
			end := skipCodeSpan(s, i)
			b.WriteString(s[i:end])
			i = end
		case '[':
			if repl, n, ok := r.link(s, i); ok {
				b.WriteString(repl)
				i += n
				continue
			}
			b.WriteByte(s[i])
			i++
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}

// skipCodeSpan returns the index just past the inline code span starting at i.
// An unterminated run of backticks is not a code span, so only the run itself
// is skipped.
func skipCodeSpan(s string, i int) int {
	j := i
	for j < len(s) && s[j] == '`' {
		j++
	}
	run := j - i
	for k := j; k < len(s); {
		if s[k] != '`' {
			k++
			continue
		}
		e := k
		for e < len(s) && s[e] == '`' {
			e++
		}
		if e-k == run {
			return e
		}
		k = e
	}
	return j
}

// link tries to read an inline link or image starting at s[i] == '['. It
// returns the replacement text and how many bytes of s it consumed.
//
// The link text is rewritten recursively, which is what makes the common
// `[![alt](img.png)](page.html)` shape work.
func (r *rewriter) link(s string, i int) (string, int, bool) {
	cb := matchBracket(s, i)
	if cb < 0 || cb+1 >= len(s) || s[cb+1] != '(' {
		return "", 0, false
	}
	end := matchParen(s, cb+1)
	if end < 0 {
		return "", 0, false
	}

	inner := s[cb+2 : end]
	pre, dest, post, angle, ok := splitDestination(inner)
	if !ok {
		return "", 0, false
	}

	text := r.inline(s[i+1 : cb])
	next, changed := r.rewriteTarget(dest)
	if !changed {
		// Not a page we fetched. The destination goes back byte for byte,
		// brackets, title and all: an absolute link that still works beats a
		// prettied-up one that does not.
		return "[" + text + "](" + inner + ")", end + 1 - i, true
	}
	return "[" + text + "](" + pre + renderDest(next, angle) + post + ")", end + 1 - i, true
}

// matchBracket finds the ']' closing the '[' at i, allowing nesting, escapes
// and code spans inside the link text.
func matchBracket(s string, i int) int {
	depth := 0
	for j := i; j < len(s); {
		switch s[j] {
		case '\\':
			j += 2
			continue
		case '`':
			j = skipCodeSpan(s, j)
			continue
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return j
			}
		}
		j++
	}
	return -1
}

// matchParen finds the ')' closing the '(' at i, allowing balanced parentheses
// and quoted titles (a title may legitimately contain a bare parenthesis).
func matchParen(s string, i int) int {
	depth := 0
	for j := i; j < len(s); {
		switch s[j] {
		case '\\':
			j += 2
			continue
		case '"', '\'':
			if q := skipQuoted(s, j); q > j {
				j = q
				continue
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return j
			}
		}
		j++
	}
	return -1
}

// skipQuoted returns the index past a quoted string starting at i, or i if it
// is unterminated (in which case the quote is just a character).
func skipQuoted(s string, i int) int {
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		if s[j] == '\\' {
			j++
			continue
		}
		if s[j] == q {
			return j + 1
		}
	}
	return i
}

// splitDestination separates a link's destination from the whitespace around it
// and any title that follows, so only the destination is ever touched.
func splitDestination(inner string) (pre, dest, post string, angle, ok bool) {
	n := 0
	for n < len(inner) && (inner[n] == ' ' || inner[n] == '\t') {
		n++
	}
	pre, rest := inner[:n], inner[n:]
	if rest == "" {
		return pre, "", "", false, true
	}
	if rest[0] == '<' {
		if e := strings.IndexByte(rest, '>'); e > 0 {
			return pre, rest[1:e], rest[e+1:], true, true
		}
		return "", "", "", false, false
	}
	depth := 0
	j := 0
	for ; j < len(rest); j++ {
		c := rest[j]
		if c == '\\' {
			j++
			continue
		}
		if c == '(' {
			depth++
			continue
		}
		if c == ')' {
			depth--
			continue
		}
		if (c == ' ' || c == '\t') && depth == 0 {
			break
		}
	}
	return pre, rest[:j], rest[j:], false, true
}

// renderDest emits a destination, keeping angle brackets where they were and
// adding them where the path would otherwise break the link syntax.
func renderDest(dest string, angle bool) string {
	if angle || strings.ContainsAny(dest, "()") {
		return "<" + dest + ">"
	}
	return dest
}

// ---------------------------------------------------------------------------
// Target resolution.
// ---------------------------------------------------------------------------

// skipSchemes are targets that are not documents. Rewriting one would break it.
var skipSchemes = []string{"mailto:", "tel:", "javascript:", "data:", "sms:", "ftp:", "file:"}

// rewriteTarget maps one link target onto a path relative to this document.
// The second result is false whenever the target must be left exactly as it
// was — an unmapped page, a fragment, a non-document scheme, anything unparsed.
func (r *rewriter) rewriteTarget(target string) (string, bool) {
	t := strings.TrimSpace(target)
	if t == "" || strings.HasPrefix(t, "#") {
		return "", false
	}
	lower := strings.ToLower(t)
	for _, s := range skipSchemes {
		if strings.HasPrefix(lower, s) {
			return "", false
		}
	}

	u, err := url.Parse(t)
	if err != nil {
		return "", false
	}
	if r.base != nil {
		u = r.base.ResolveReference(u)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}

	local, ok := r.lookup(u)
	if !ok {
		return "", false
	}

	rel := relativeTo(r.fromPath, local)
	if u.Fragment != "" {
		frag := u.EscapedFragment()
		if frag == "" {
			frag = u.Fragment
		}
		rel += "#" + frag
	}
	return rel, true
}

// lookup finds a URL in the mapping, tolerating the differences that never
// change which page is meant: a fragment, a trailing slash, a capitalised host.
// A mirror that leaves a link absolute because the key said "/docs/" and the
// page said "/docs" is broken in exactly the way this function exists to avoid.
func (r *rewriter) lookup(u *url.URL) (string, bool) {
	c := *u
	c.Fragment, c.RawFragment = "", ""

	try := func(x url.URL) (string, bool) {
		v, ok := r.mapping[x.String()]
		return v, ok
	}
	if v, ok := try(c); ok {
		return v, true
	}

	alt := c
	switch {
	case alt.Path == "":
		alt.Path = "/"
	case strings.HasSuffix(alt.Path, "/") && len(alt.Path) > 1:
		alt.Path = strings.TrimSuffix(alt.Path, "/")
	default:
		alt.Path += "/"
	}
	if v, ok := try(alt); ok {
		return v, true
	}

	lc := c
	lc.Scheme = strings.ToLower(lc.Scheme)
	lc.Host = strings.ToLower(lc.Host)
	if lc != c {
		if v, ok := try(lc); ok {
			return v, true
		}
	}

	// Last resort: the mapping may key on the URL including its fragment.
	if v, ok := r.mapping[u.String()]; ok {
		return v, true
	}
	return "", false
}

// relativeTo expresses toPath relative to the directory holding fromPath, both
// given relative to the output root.
//
//	docs/a.md -> docs/b.md   =>  b.md
//	docs/a.md -> index.md    =>  ../index.md
//	index.md  -> docs/b.md   =>  docs/b.md
//
// An empty fromPath means "the document sits at the output root", which is the
// only sane reading when the caller did not tell us where it was written.
func relativeTo(fromPath, toPath string) string {
	to := splitPath(toPath)
	if len(to) == 0 {
		return ""
	}
	fromDir := splitPath(path.Dir("/" + strings.TrimPrefix(cleanSlash(fromPath), "/")))

	i := 0
	for i < len(fromDir) && i < len(to)-1 && fromDir[i] == to[i] {
		i++
	}
	parts := make([]string, 0, len(fromDir)-i+len(to)-i)
	for j := i; j < len(fromDir); j++ {
		parts = append(parts, "..")
	}
	parts = append(parts, to[i:]...)
	return escapePath(strings.Join(parts, "/"))
}

func cleanSlash(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if p == "" {
		return ""
	}
	return path.Clean("/" + strings.TrimPrefix(p, "/"))
}

func splitPath(p string) []string {
	var out []string
	for _, s := range strings.Split(strings.ReplaceAll(p, "\\", "/"), "/") {
		if s == "" || s == "." {
			continue
		}
		out = append(out, s)
	}
	return out
}

// escapePath percent-encodes each segment so a path with a space in it stays a
// single link target. ".." must survive untouched or the arithmetic above is
// undone by the escaping.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if s == ".." || s == "." {
			continue
		}
		segs[i] = (&url.URL{Path: s}).EscapedPath()
	}
	return strings.Join(segs, "/")
}
