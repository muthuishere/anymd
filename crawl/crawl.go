// Package crawl fetches a site and hands each page to a callback.
//
// It is deliberately OUTSIDE the converter path. anymd's core guarantee is that
// a converter never touches the network, which is what makes it safe to point
// at untrusted input. Crawling is network by definition, so it lives here: a
// separate package a caller opts into, never something a conversion can trigger.
package crawl

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// Defaults applied to any unset Option. They are deliberately small: an
// unbounded crawler pointed at a large site is indistinguishable from an attack
// on it.
const (
	defaultMaxDepth = 1
	defaultMaxPages = 200
	defaultDelay    = 500 * time.Millisecond

	// DefaultUserAgent names anymd and its repository, so an operator seeing it
	// in a log can find out what just walked their site.
	DefaultUserAgent = "anymd-crawler (+https://github.com/muthuishere/anymd)"
)

// Hard bounds that are not configurable. A crawl talks to servers it does not
// control, so every one of these is a memory or liveness guarantee, not a
// preference.
const (
	// maxBodyBytes caps a single response. Anything larger is an error for that
	// URL, not a reason to hold 4 GiB in RAM.
	maxBodyBytes = 32 << 20 // 32 MiB
	// maxTotalBytes caps the whole crawl. Hitting it truncates.
	maxTotalBytes = 512 << 20 // 512 MiB
	// maxRobotsBytes caps robots.txt, which is a text file and has no business
	// being large.
	maxRobotsBytes = 512 << 10 // 512 KiB
	// maxRedirects caps one redirect chain, so a server cannot loop us forever.
	maxRedirects = 5
	// requestTimeout bounds a single request end to end.
	requestTimeout = 30 * time.Second

	// maxSegment and maxPathLen bound what LocalPath can produce, so a long URL
	// cannot yield a filename the OS refuses to create.
	maxSegment = 100
	maxPathLen = 180
)

// Options bounds a crawl. The zero value is NOT safe to use — Crawl applies
// defaults for anything unset, because an unbounded crawler pointed at a large
// site is indistinguishable from an attack on it.
//
// Crawl takes Options by value and normalises a copy, so a caller can reuse one
// Options across crawls and Crawl never mutates it.
type Options struct {
	// MaxDepth is how many links deep to follow from the seed. 0 means the
	// seed only. Defaults to 1.
	//
	// Because 0 is a meaningful value ("seed only") and also the zero value,
	// "unset" cannot be distinguished from "0" here either. A negative MaxDepth
	// is how you say "use the default"; 0 is honoured literally.
	MaxDepth int
	// MaxPages caps the total fetched. Defaults to 200. A value <= 0 means
	// unset and takes the default.
	MaxPages int
	// SameHost restricts the crawl to the seed's host. Defaults to true;
	// setting it false is how a crawl escapes onto the open web, so it must be
	// an explicit choice.
	//
	// It is a *bool and not a bool precisely because the default is true and
	// the zero value of a bool is false: with a plain bool, a caller who never
	// thought about the field would silently get the dangerous behaviour.
	// nil means unset, so the safe default applies. To leave the seed's host a
	// caller must say so explicitly:
	//
	//	opts.SameHost = crawl.Ptr(false)   // yes, really crawl the open web
	//
	// (Inverting the field to CrossHost bool would also make the zero value
	// safe, but it would silently change the meaning of code already written
	// against the documented SameHost field. A pointer keeps the field's name
	// and sense and makes "unset" representable, which is the actual problem.)
	SameHost *bool
	// Delay between requests to one host. Defaults to 500ms. Politeness is not
	// optional: a fast crawler is a denial of service. A value <= 0 means unset
	// and takes the default; there is deliberately no way to ask for no delay.
	Delay time.Duration
	// UserAgent identifies the crawler. Defaults to a string naming anymd and
	// its repository, so an operator seeing it in a log can find out what it is.
	UserAgent string
	// IgnoreRobots disables robots.txt.
	//
	// Defaults to false, and leaving it false is the right answer. Setting it
	// true means fetching pages whose owner has published a machine-readable
	// request that you not fetch them. That is not a technicality: it can get
	// your address blocked, it breaks the one convention that keeps automated
	// clients tolerated on the open web, and in some jurisdictions and under
	// some terms of service it is the difference between reading a site and
	// breaching a contract. Use it on infrastructure you own, or with the
	// operator's agreement. Not because the crawl was inconvenient.
	IgnoreRobots bool
	// Include and Exclude are regular expressions matched against the full URL.
	// Exclude wins over Include. They are compiled once, up front; a bad
	// pattern is returned as an error rather than panicking mid-crawl.
	//
	// They apply to discovered links only, never to the seed: a crawl that
	// silently fetched nothing because the seed failed its own Include filter
	// would be a confusing way to report a typo.
	Include, Exclude []string
	// Insecure skips TLS verification.
	Insecure bool
}

// Ptr returns a pointer to v. It exists so an Options literal can set a
// pointer-valued field inline: crawl.Options{SameHost: crawl.Ptr(false)}.
func Ptr[T any](v T) *T { return &v }

// Page is one fetched document.
type Page struct {
	URL         string
	Body        []byte
	ContentType string
	Depth       int
}

// Result reports what a crawl did.
type Result struct {
	Fetched   int
	Skipped   int
	Errors    map[string]error
	Visited   []string
	Truncated bool // MaxPages or MaxDepth stopped it early
}

// normalised is Options with every default filled in and every pattern
// compiled, so the crawl loop never has to reason about "unset" again.
type normalised struct {
	maxDepth  int
	maxPages  int
	sameHost  bool
	delay     time.Duration
	userAgent string
	robots    bool // true = honour robots.txt
	include   []*regexp.Regexp
	exclude   []*regexp.Regexp
	insecure  bool
}

func normalise(o Options) (normalised, error) {
	n := normalised{
		maxDepth:  o.MaxDepth,
		maxPages:  o.MaxPages,
		sameHost:  true,
		delay:     o.Delay,
		userAgent: strings.TrimSpace(o.UserAgent),
		robots:    !o.IgnoreRobots,
		insecure:  o.Insecure,
	}
	if o.MaxDepth < 0 {
		n.maxDepth = defaultMaxDepth
	}
	if o.MaxPages <= 0 {
		n.maxPages = defaultMaxPages
	}
	if o.SameHost != nil {
		n.sameHost = *o.SameHost
	}
	if o.Delay <= 0 {
		n.delay = defaultDelay
	}
	if n.userAgent == "" {
		n.userAgent = DefaultUserAgent
	}
	var err error
	if n.include, err = compileAll(o.Include, "include"); err != nil {
		return normalised{}, err
	}
	if n.exclude, err = compileAll(o.Exclude, "exclude"); err != nil {
		return normalised{}, err
	}
	return n, nil
}

func compileAll(pats []string, what string) ([]*regexp.Regexp, error) {
	if len(pats) == 0 {
		return nil, nil
	}
	out := make([]*regexp.Regexp, 0, len(pats))
	for _, p := range pats {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("crawl: bad %s pattern %q: %w", what, p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

// allowedByFilters reports whether a discovered URL survives Include/Exclude.
// Exclude wins.
func (n normalised) allowedByFilters(u string) bool {
	for _, re := range n.exclude {
		if re.MatchString(u) {
			return false
		}
	}
	if len(n.include) == 0 {
		return true
	}
	for _, re := range n.include {
		if re.MatchString(u) {
			return true
		}
	}
	return false
}

// item is one queued URL and the depth it was found at.
type item struct {
	url   string
	depth int
}

// Crawl walks from seed, calling visit for each page it fetches.
//
// visit is called serially, so it needs no locking. Returning an error from
// visit aborts the crawl.
//
// The walk is breadth-first BY DEPTH: every URL at depth d is fetched before
// any at depth d+1, so MaxDepth means what a reader expects and MaxPages
// truncates the deepest pages rather than an arbitrary slice of the site.
//
// A URL is fetched at most once. URLs are normalised before deduping — see
// normaliseURL for exactly what that means — and, after a redirect, it is the
// FINAL URL that is recorded, so two paths that redirect to one page are fetched
// once.
func Crawl(ctx context.Context, seed string, opts Options, visit func(Page) error) (Result, error) {
	res := Result{Errors: map[string]error{}}

	n, err := normalise(opts)
	if err != nil {
		return res, err
	}
	if visit == nil {
		return res, errors.New("crawl: visit callback is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	seedURL, err := parseAbsolute(seed)
	if err != nil {
		return res, fmt.Errorf("crawl: seed %q: %w", seed, err)
	}
	seedNorm := normaliseURL(seedURL)

	c := &crawler{
		opts:   n,
		client: newClient(n),
		robots: map[string]*robotsFile{},
		last:   map[string]time.Time{},
		seen:   map[string]bool{},
	}

	queue := []item{{url: seedNorm, depth: 0}}
	c.seen[seedNorm] = true

	for len(queue) > 0 {
		var next []item
		for _, it := range queue {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			if res.Fetched >= n.maxPages {
				res.Truncated = true
				return res, nil
			}
			if c.bytes >= maxTotalBytes {
				res.Truncated = true
				return res, nil
			}

			u, err := parseAbsolute(it.url)
			if err != nil {
				res.Errors[it.url] = err
				continue
			}

			// robots.txt applies to the seed as much as to anything else: if a
			// site says do not fetch this, "but you typed it" is not an answer.
			ok, err := c.robotsAllows(ctx, u)
			if err != nil {
				res.Errors[it.url] = err
				continue
			}
			if !ok {
				res.Skipped++
				continue
			}

			page, finalURL, err := c.fetch(ctx, u)
			if err != nil {
				if ctx.Err() != nil {
					return res, ctx.Err()
				}
				res.Errors[it.url] = err
				continue
			}
			// Dedup on the FINAL url: a redirect target already fetched under
			// another name must not be handed to visit twice.
			if finalURL != it.url {
				if c.seen[finalURL] {
					res.Skipped++
					continue
				}
				c.seen[finalURL] = true
			}

			page.Depth = it.depth
			res.Fetched++
			res.Visited = append(res.Visited, finalURL)
			if err := visit(page); err != nil {
				return res, err
			}

			// Links come out of HTML only. A PDF or a docx is worth mirroring,
			// but it is not a link graph and we do not pretend it is.
			if !isHTML(page.ContentType) {
				continue
			}
			base, err := parseAbsolute(finalURL)
			if err != nil {
				continue
			}
			for _, link := range extractLinks(base, page.Body) {
				lu, err := parseAbsolute(link)
				if err != nil {
					continue
				}
				norm := normaliseURL(lu)
				if c.seen[norm] {
					continue
				}
				if n.sameHost && !sameHost(lu, seedURL) {
					c.seen[norm] = true
					res.Skipped++
					continue
				}
				if !n.allowedByFilters(norm) {
					c.seen[norm] = true
					res.Skipped++
					continue
				}
				if it.depth+1 > n.maxDepth {
					// Eligible, but beyond the depth budget. That is exactly
					// what Truncated is for: the caller must be able to tell
					// "that is the whole site" from "I stopped early".
					res.Truncated = true
					continue
				}
				c.seen[norm] = true
				next = append(next, item{url: norm, depth: it.depth + 1})
			}
		}
		queue = next
	}
	return res, nil
}

// crawler holds the mutable state of one crawl.
type crawler struct {
	opts   normalised
	client *http.Client
	robots map[string]*robotsFile // origin -> rules (nil value = allow all)
	last   map[string]time.Time   // host -> time of last request
	seen   map[string]bool
	bytes  int64
}

func newClient(n normalised) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if n.insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in via Options.Insecure
	}
	return &http.Client{
		Timeout:   requestTimeout,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return nil
		},
	}
}

// wait honours the politeness delay for a host, and the context while it does.
func (c *crawler) wait(ctx context.Context, host string, delay time.Duration) error {
	if last, ok := c.last[host]; ok {
		if d := delay - time.Since(last); d > 0 {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
			}
		}
	}
	c.last[host] = time.Now()
	return nil
}

// fetch retrieves one URL, returning the page and the FINAL url after redirects.
func (c *crawler) fetch(ctx context.Context, u *url.URL) (Page, string, error) {
	delay := c.opts.delay
	if r := c.robots[origin(u)]; r != nil && r.crawlDelay > delay {
		// Crawl-delay is a floor the site asked for, so it wins when it is
		// slower than ours. It never speeds us up.
		delay = r.crawlDelay
	}
	if err := c.wait(ctx, u.Host, delay); err != nil {
		return Page{}, "", err
	}

	body, ct, final, err := c.get(ctx, u.String(), maxBodyBytes)
	if err != nil {
		return Page{}, "", err
	}
	c.bytes += int64(len(body))

	fu, err := parseAbsolute(final)
	if err != nil {
		return Page{}, "", err
	}
	return Page{URL: final, Body: body, ContentType: ct}, normaliseURL(fu), nil
}

// get performs one bounded GET. It is the only place in the package that speaks
// HTTP, so every cap lives here.
func (c *crawler) get(ctx context.Context, rawurl string, limit int64) (body []byte, contentType, final string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("User-Agent", c.opts.userAgent)
	req.Header.Set("Accept", "*/*")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", "", fmt.Errorf("http %s", resp.Status)
	}

	// Read one byte past the cap so an oversized body is detected rather than
	// silently truncated into a half-document.
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", "", err
	}
	if int64(len(b)) > limit {
		return nil, "", "", fmt.Errorf("response body exceeds %d bytes", limit)
	}
	return b, resp.Header.Get("Content-Type"), resp.Request.URL.String(), nil
}

// robotsAllows reports whether robots.txt permits fetching u, fetching and
// caching the file for u's origin on first use.
func (c *crawler) robotsAllows(ctx context.Context, u *url.URL) (bool, error) {
	if !c.opts.robots {
		return true, nil
	}
	o := origin(u)
	r, cached := c.robots[o]
	if !cached {
		if err := c.wait(ctx, u.Host, c.opts.delay); err != nil {
			return false, err
		}
		body, _, _, err := c.get(ctx, o+"/robots.txt", maxRobotsBytes)
		if err != nil {
			// Missing, refused, or unreachable robots.txt means crawling is
			// allowed. That is the standard's behaviour, not a failure.
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			r = nil
		} else {
			r = parseRobots(body, c.opts.userAgent)
		}
		c.robots[o] = r
	}
	if r == nil {
		return true, nil
	}
	return r.allows(u.EscapedPath(), u.RawQuery), nil
}

// parseAbsolute parses an absolute http(s) URL and rejects everything else.
func parseAbsolute(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return nil, fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("missing host")
	}
	return u, nil
}

// normaliseURL produces the canonical string a URL is deduped by.
//
// The rules, chosen deliberately:
//   - scheme and host are lowercased; a default port (:80 on http, :443 on
//     https) is dropped, because those name the same server.
//   - the fragment is dropped: #install is a position in a document, not a
//     different document, and fetching a page twice for two anchors is a bug.
//   - the path is resolved (./ and ../ collapse) and an empty path becomes "/".
//   - a TRAILING SLASH IS SIGNIFICANT. /a and /a/ are two URLs to a server —
//     they routinely serve different content and resolve relative links
//     differently — so collapsing them would fetch the wrong page and, worse,
//     make LocalPath collide two documents onto one file.
//   - a QUERY IS SIGNIFICANT, and its parameters are NOT reordered. ?page=2 is
//     a different document; and re-sorting parameters would mean sending a URL
//     the site never published.
func normaliseURL(u *url.URL) string {
	c := *u
	c.Scheme = strings.ToLower(c.Scheme)
	c.Fragment = ""
	c.RawFragment = ""
	c.User = nil

	c.Host = canonicalHost(&c)

	p := c.EscapedPath()
	trailing := strings.HasSuffix(p, "/")
	cleaned := path.Clean("/" + p)
	if cleaned != "/" && trailing {
		cleaned += "/"
	}
	c.RawPath = ""
	up, err := url.PathUnescape(cleaned)
	if err != nil {
		up = cleaned
	}
	c.Path = up
	if esc := c.EscapedPath(); esc != cleaned {
		c.RawPath = cleaned
	}
	return c.String()
}

func origin(u *url.URL) string {
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

// sameHost compares authorities, not just hostnames: 127.0.0.1:8080 and
// 127.0.0.1:9090 are different servers, and treating them as one host is how a
// "same host" crawl quietly walks onto a neighbour.
func sameHost(a, b *url.URL) bool {
	return canonicalHost(a) == canonicalHost(b)
}

// canonicalHost lowercases an authority and drops the scheme's default port.
func canonicalHost(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	scheme := strings.ToLower(u.Scheme)
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if port == "" {
		return host
	}
	return net.JoinHostPort(host, port)
}

// isHTML reports whether a Content-Type is something we should look for links
// in. Anything else is fetched and handed over, but never parsed.
func isHTML(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		mt = strings.TrimSpace(strings.ToLower(strings.SplitN(ct, ";", 2)[0]))
	}
	switch mt {
	case "text/html", "application/xhtml+xml":
		return true
	}
	return false
}

// extractLinks pulls <a href> targets out of an HTML document and resolves them
// against base (honouring <base href> if the document sets one).
func extractLinks(base *url.URL, body []byte) []string {
	var out []string
	seen := map[string]bool{}
	z := html.NewTokenizer(strings.NewReader(string(body)))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return out
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			if !hasAttr {
				continue
			}
			tag := string(name)
			if tag != "a" && tag != "base" {
				continue
			}
			var href string
			for {
				k, v, more := z.TagAttr()
				if string(k) == "href" {
					href = string(v)
				}
				if !more {
					break
				}
			}
			href = strings.TrimSpace(href)
			if href == "" {
				continue
			}
			if tag == "base" {
				if b, err := base.Parse(href); err == nil && b.Host != "" {
					base = b
				}
				continue
			}
			if skipHref(href) {
				continue
			}
			abs, err := base.Parse(href)
			if err != nil {
				continue
			}
			switch strings.ToLower(abs.Scheme) {
			case "http", "https":
			default:
				continue
			}
			abs.Fragment = ""
			abs.RawFragment = ""
			s := abs.String()
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
}

// skipHref rejects hrefs that are not documents to fetch.
func skipHref(href string) bool {
	if strings.HasPrefix(href, "#") {
		return true
	}
	lower := strings.ToLower(href)
	for _, p := range []string{"mailto:", "javascript:", "tel:", "data:"} {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// hexSuffix matches a name that already looks like one of our disambiguating
// hash suffixes. Such a name is itself hashed, so a literal URL path can never
// impersonate the disambiguator and collide with a hashed one.
var hexSuffix = regexp.MustCompile(`-[0-9a-f]{8}$`)

// safeSegment keeps a conservative, portable filename character set. Everything
// else — separators, backslashes, colons (so a drive letter cannot survive),
// control characters, NUL — is replaced.
func safeSegment(s string) (string, bool) {
	changed := false
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
			changed = true
		}
	}
	out := b.String()
	// "." and ".." must never survive: they are the whole escape attack. A
	// sanitised segment can also GROW a ".." (a\..\b becomes a-..-b), so the
	// check is on the result, not the input.
	if strings.Trim(out, ".") == "" {
		return "", true
	}
	for strings.Contains(out, "..") {
		out = strings.ReplaceAll(out, "..", "-")
		changed = true
	}
	if len(out) > maxSegment {
		out = out[:maxSegment]
		changed = true
	}
	return out, changed
}

// LocalPath maps a URL to a relative file path for a mirrored copy, e.g.
// https://x.dev/docs/a?b=1 -> docs/a-1f3c9a20.md. It must be deterministic, must
// never escape the output directory, and must not collide two distinct URLs onto
// one file.
//
// The rules:
//
//   - The returned path is always relative, always slash-separated, and always
//     inside the output root. Every segment is rewritten to [A-Za-z0-9._-];
//     "." and ".." segments are dropped after the path is resolved, so "..",
//     absolute paths, backslashes, drive letters, NUL and control characters
//     cannot escape. This function decides where attacker-controlled bytes get
//     written, so it treats the URL as hostile by default.
//   - A path ending in "/" — and the bare origin — becomes index<ext> under that
//     directory. So /a and /a/ are "a.md" and "a/index.md": distinct files, as
//     they are distinct URLs.
//   - ext is appended, never substituted: /a.html becomes "a.html.md", not
//     "a.md", because stripping the URL's own extension would collide /a.html
//     with /a.
//   - Anything the raw URL cannot express in a filename — a query string, a
//     character that had to be rewritten, a segment or path that had to be
//     truncated — gets an 8-hex-character suffix of the SHA-256 of the
//     normalised URL. A name that already ends in "-" plus 8 hex characters is
//     hashed too, so a literal path can never impersonate a suffix. A segment
//     that is not already lowercase is hashed as well, because the default
//     filesystems on macOS and Windows are case-insensitive and /README and
//     /readme are two URLs. Two distinct URLs therefore cannot land on one file
//     short of a SHA-256 collision.
//   - Percent-encoding is NOT a difference: /a%20b and "/a b" are the same URL
//     and deliberately map to the same file.
//
// ext is the extension to give the file; "" means ".md". It is sanitised the
// same way a path segment is.
func LocalPath(rawURL, ext string) (string, error) {
	ext = strings.TrimSpace(ext)
	if ext == "" {
		ext = ".md"
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	cleanExt, _ := safeSegment(ext)
	if cleanExt == "" || cleanExt == "." {
		return "", errors.New("crawl: unusable extension")
	}
	ext = cleanExt

	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", fmt.Errorf("crawl: LocalPath %q: %w", rawURL, err)
	}

	key := rawURL
	if u.IsAbs() && u.Host != "" {
		key = normaliseURL(u)
	}

	raw := u.EscapedPath()
	if raw == "" && u.Opaque != "" {
		raw = u.Opaque
	}
	trailing := raw == "" || strings.HasSuffix(raw, "/")

	needHash := u.RawQuery != "" || u.ForceQuery

	segs := strings.Split(path.Clean("/"+raw), "/")
	out := make([]string, 0, len(segs)+1)
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			// path.Clean has already resolved what it can; a leading ".." that
			// survives is an escape attempt and is dropped outright.
			if s == ".." {
				needHash = true
			}
			continue
		}
		dec, err := url.PathUnescape(s)
		if err != nil {
			dec = s
			needHash = true
		}
		if dec != s {
			needHash = true
		}
		clean, changed := safeSegment(dec)
		if changed {
			needHash = true
		}
		if clean == "" {
			continue
		}
		if strings.ToLower(clean) != clean {
			// macOS and Windows filesystems are case-insensitive by default, so
			// /README and /readme would otherwise be one file. Disambiguate.
			needHash = true
		}
		out = append(out, clean)
	}

	if trailing || len(out) == 0 {
		out = append(out, "index")
	}

	last := out[len(out)-1]
	if hexSuffix.MatchString(last) {
		needHash = true
	}
	if needHash {
		last += "-" + hash8(key)
	}
	last += ext
	out[len(out)-1] = last

	p := strings.Join(out, "/")
	if len(p) > maxPathLen {
		// Too long to write. Collapse to a single hashed name rather than
		// truncating into something that might collide.
		p = trimTo(out[len(out)-1], maxSegment)
		p = strings.TrimSuffix(p, ext)
		p = trimTo(p, maxSegment-len(ext)-9) + "-" + hash8(key) + ext
	}
	return p, nil
}

func trimTo(s string, n int) string {
	if n < 1 {
		n = 1
	}
	if len(s) > n {
		return s[:n]
	}
	return s
}

func hash8(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}
