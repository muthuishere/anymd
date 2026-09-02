package crawl

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

// Bounds on sitemap discovery. Like the crawl's own caps these are not
// configurable: a sitemap is a document written by the site, so every one of
// them is a memory or liveness guarantee rather than a preference.
const (
	// maxSitemapDepth caps how far a <sitemapindex> may nest. The root document
	// is depth 0, so 3 allows an index of an index of an index of a urlset —
	// deeper than any real site, and a hard stop for one that points at itself.
	maxSitemapDepth = 3
	// maxSitemapFetches caps how many sitemap documents one discovery may fetch,
	// across every index it follows. A fan-out of indexes cannot turn into a
	// crawl of its own.
	maxSitemapFetches = 50
	// maxSitemapBytes caps a sitemap document BOTH on the wire and after
	// decompression, so a gzip bomb is an error for that sitemap rather than
	// 4 GiB of zeroes in RAM.
	maxSitemapBytes = 32 << 20 // 32 MiB
	// maxSitemapURLs caps the URLs one discovery will collect. The sitemaps
	// protocol itself allows 50,000 per document; this is the total.
	maxSitemapURLs = 50000
)

// SitemapMode selects how Crawl uses a site's sitemap.
//
// Unlike Options.SameHost this is a plain value and not a pointer, because the
// enum is ordered so that the SAFE, WANTED default is the zero value:
// SitemapAuto. SameHost needed a *bool only because its desired default (true)
// is not a bool's zero value, so "unset" and "off" were indistinguishable. Here
// "unset" and "the default" are the same constant, and nothing is lost.
type SitemapMode int

const (
	// SitemapAuto uses a sitemap when one is found AND still follows links.
	// The sitemap seeds the frontier at depth 0; link discovery runs from every
	// page as usual. This is the default and needs no flag to benefit from: it
	// finds pages the seed does not link to, and finds them in one request
	// instead of a walk.
	SitemapAuto SitemapMode = iota
	// SitemapOnly fetches the seed plus the URLs a sitemap lists, and follows
	// no links at all. Useful on a large site whose sitemap is authoritative,
	// where link-following would only add cost. If no sitemap is found this
	// fetches the seed and nothing else — that is not an error, it is what
	// "only the sitemap" means when there is no sitemap.
	SitemapOnly
	// SitemapOff never fetches a sitemap and discovers pages by following
	// links, exactly as anymd did before sitemaps existed.
	SitemapOff
)

// SitemapEntry is one <url> a sitemap lists.
type SitemapEntry struct {
	// Loc is the raw <loc>, absolute as the sitemaps protocol requires. It has
	// been through no gate yet: a sitemap is a hint, not an authority, so the
	// caller still applies SameHost, Include/Exclude and robots.txt to it.
	Loc string
	// LastMod is <lastmod> parsed as a W3C datetime, zero if the sitemap
	// omitted it or wrote something unparseable. It costs nothing to keep and a
	// caller may want it to skip unchanged pages.
	LastMod time.Time
	// LastModRaw is <lastmod> exactly as written, kept because a value we could
	// not parse is still information.
	LastModRaw string
}

// sitemapEntry is the decode target for one <url> or <sitemap> element. The
// field tags carry no namespace on purpose: encoding/xml then matches on the
// LOCAL name in any namespace, which is what real sitemaps need — the schema is
// http://www.sitemaps.org/schemas/sitemap/0.9 and a large minority of live
// sitemaps declare it wrong, declare a different one, or declare none at all.
type sitemapEntryXML struct {
	Loc     string `xml:"loc"`
	LastMod string `xml:"lastmod"`
}

// parseSitemap decodes one sitemap document, returning the <url> entries of a
// <urlset> and the child locations of a <sitemapindex>. Exactly one of the two
// is populated for a well-formed document.
//
// Anything that is not one of those two root elements is an error, and so is
// malformed XML. A caller treats that the way it treats a missing sitemap: fall
// back to link crawling. We do not try to salvage a broken document — guessing
// at what a half-parsed sitemap meant is how a crawler ends up fetching URLs
// nobody published.
func parseSitemap(data []byte) (urls []SitemapEntry, children []string, err error) {
	d := xml.NewDecoder(bytes.NewReader(data))
	// Real sitemaps show up in Latin-1 and other encodings; without this the
	// decoder refuses them outright. Bytes are passed through as-is, which is
	// right for <loc>, whose contents are URL-escaped ASCII by the protocol.
	d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }

	var root string
	for {
		tok, terr := d.Token()
		if terr == io.EOF {
			break
		}
		if terr != nil {
			return nil, nil, fmt.Errorf("crawl: sitemap: %w", terr)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if root == "" {
			root = se.Name.Local
			switch root {
			case "urlset", "sitemapindex":
			default:
				return nil, nil, fmt.Errorf("crawl: sitemap: unexpected root element %q", root)
			}
			continue
		}
		want := "url"
		if root == "sitemapindex" {
			want = "sitemap"
		}
		if se.Name.Local != want {
			if err := d.Skip(); err != nil {
				return nil, nil, fmt.Errorf("crawl: sitemap: %w", err)
			}
			continue
		}
		var e sitemapEntryXML
		if err := d.DecodeElement(&e, &se); err != nil {
			return nil, nil, fmt.Errorf("crawl: sitemap: %w", err)
		}
		loc := strings.TrimSpace(e.Loc)
		if loc == "" {
			continue
		}
		if root == "sitemapindex" {
			if len(children) < maxSitemapURLs {
				children = append(children, loc)
			}
			continue
		}
		if len(urls) >= maxSitemapURLs {
			continue
		}
		raw := strings.TrimSpace(e.LastMod)
		urls = append(urls, SitemapEntry{Loc: loc, LastMod: parseLastMod(raw), LastModRaw: raw})
	}
	if root == "" {
		return nil, nil, errors.New("crawl: sitemap: no root element")
	}
	return urls, children, nil
}

// lastModFormats are the W3C datetime profile the sitemaps protocol specifies,
// widest first. A value in none of them is kept raw and left zero.
var lastModFormats = []string{
	time.RFC3339,
	"2006-01-02T15:04Z07:00",
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02",
	"2006-01",
	"2006",
}

func parseLastMod(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, f := range lastModFormats {
		if t, err := time.Parse(f, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// gunzipMagic is the two-byte header every gzip stream starts with.
var gunzipMagic = []byte{0x1f, 0x8b}

// maybeGunzip decompresses data if it is a gzip stream, bounded.
//
// It sniffs the magic bytes rather than trusting a header. net/http already
// decompresses a Content-Encoding: gzip response transparently (it adds the
// Accept-Encoding itself, since we do not), so the case left to handle is the
// far more common one: a .xml.gz served as a plain file with a content type
// like application/gzip. Sniffing covers both without caring which happened.
//
// The decompressed size is capped independently of the transfer size, which is
// the whole point: a few KiB on the wire can be gigabytes expanded, and a
// sitemap is a document written by the site being crawled.
func maybeGunzip(data []byte, limit int64) ([]byte, error) {
	if !bytes.HasPrefix(data, gunzipMagic) {
		return data, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("crawl: sitemap: gzip: %w", err)
	}
	defer zr.Close()
	// One byte past the cap, so an oversized stream is detected rather than
	// silently truncated into a half-document.
	out, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil {
		return nil, fmt.Errorf("crawl: sitemap: gzip: %w", err)
	}
	if int64(len(out)) > limit {
		return nil, fmt.Errorf("crawl: sitemap: decompressed size exceeds %d bytes", limit)
	}
	return out, nil
}

// parseSitemapDirectives pulls the "Sitemap:" lines out of a robots.txt body.
//
// The directive is global — it belongs to no User-agent group — so it is read
// straight off the file rather than through the group selection in robots.go.
func parseSitemapDirectives(body []byte) []string {
	var out []string
	for _, raw := range strings.Split(string(body), "\n") {
		line := raw
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.ToLower(strings.TrimSpace(key)) != "sitemap" {
			continue
		}
		if v := strings.TrimSpace(value); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// discoverSitemap finds the URLs a seed's sitemaps list.
//
// Discovery order, first source that yields any URL wins:
//
//  1. every "Sitemap:" directive in the origin's robots.txt — we fetch that
//     file anyway, and it is the only place a site can TELL us where its
//     sitemap is. (Skipped when Options.IgnoreRobots is set: we then never
//     fetch robots.txt at all, so there is nothing to read it from.)
//  2. /sitemap.xml at the origin.
//  3. /sitemap_index.xml at the origin.
//
// A sitemap that is missing, refused, oversized, gzip-bombed or malformed is
// not an error — it means this source yielded nothing and the next one, or
// plain link crawling, takes over. Nothing here can fail a crawl.
//
// Sitemap DOCUMENTS are only ever fetched from the seed's own host, including
// the children of an index. That is what the protocol requires, and it means a
// sitemap cannot aim our requests at a third party. The URLs a sitemap LISTS
// are a different matter: they are returned unfiltered and the caller puts them
// through SameHost, Include/Exclude and robots.txt like any other discovery.
func (c *crawler) discoverSitemap(ctx context.Context, seed *url.URL) []SitemapEntry {
	o := origin(seed)
	var sources [][]string
	if c.opts.robots {
		if body := c.robotsBody[o]; len(body) > 0 {
			if locs := parseSitemapDirectives(body); len(locs) > 0 {
				sources = append(sources, locs)
			}
		}
	}
	sources = append(sources, []string{o + "/sitemap.xml"}, []string{o + "/sitemap_index.xml"})

	// fetched and the fetch budget are shared across sources, so falling back
	// can never re-fetch a document or exceed the cap in total.
	fetched := map[string]bool{}
	budget := maxSitemapFetches
	for _, src := range sources {
		entries := c.walkSitemaps(ctx, seed, src, fetched, &budget)
		if len(entries) > 0 {
			return entries
		}
		if ctx.Err() != nil || budget <= 0 {
			return nil
		}
	}
	return nil
}

// walkSitemaps fetches roots breadth-first, following <sitemapindex> children
// down to maxSitemapDepth and stopping when the shared fetch budget runs out.
func (c *crawler) walkSitemaps(ctx context.Context, seed *url.URL, roots []string, fetched map[string]bool, budget *int) []SitemapEntry {
	type queued struct {
		url   string
		depth int
	}
	var queue []queued
	for _, r := range roots {
		queue = append(queue, queued{url: r})
	}

	var out []SitemapEntry
	for i := 0; i < len(queue); i++ {
		if ctx.Err() != nil || *budget <= 0 || len(out) >= maxSitemapURLs {
			break
		}
		q := queue[i]

		u, err := parseAbsolute(q.url)
		if err != nil {
			continue
		}
		// A sitemap document off the seed's host is not ours to fetch, however
		// it got named. This is also what stops an index from walking us onto
		// somebody else's server one hop at a time.
		if !sameHost(u, seed) {
			continue
		}
		norm := normaliseURL(u)
		// A self-referencing index — or two indexes pointing at each other —
		// terminates here rather than at the depth cap.
		if fetched[norm] {
			continue
		}
		fetched[norm] = true
		*budget--

		data, err := c.fetchSitemap(ctx, u)
		if err != nil {
			continue
		}
		urls, children, err := parseSitemap(data)
		if err != nil {
			continue
		}
		c.sitemapDocs = append(c.sitemapDocs, norm)
		for _, e := range urls {
			if len(out) >= maxSitemapURLs {
				break
			}
			out = append(out, e)
		}
		if q.depth >= maxSitemapDepth {
			continue
		}
		for _, ch := range children {
			// Resolve against the document, so a relative <loc> — which the
			// protocol forbids but sitemaps contain anyway — cannot become an
			// unrelated absolute URL.
			cu, err := u.Parse(strings.TrimSpace(ch))
			if err != nil {
				continue
			}
			queue = append(queue, queued{url: cu.String(), depth: q.depth + 1})
		}
	}
	return out
}

// fetchSitemap retrieves and, if needed, decompresses one sitemap document,
// honouring the politeness delay like any other request.
func (c *crawler) fetchSitemap(ctx context.Context, u *url.URL) ([]byte, error) {
	if err := c.wait(ctx, u.Host, c.opts.delay); err != nil {
		return nil, err
	}
	body, _, _, err := c.get(ctx, u.String(), maxSitemapBytes)
	if err != nil {
		return nil, err
	}
	c.bytes += int64(len(body))
	return maybeGunzip(body, maxSitemapBytes)
}
