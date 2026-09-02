package crawl

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// As everywhere else in this package: no test here touches the real network.
// Every server is an httptest.Server on loopback, and the one test that names a
// foreign host asserts that the URL is REJECTED before any request is made.

// smOpts returns Options a test can afford, with robots.txt off. Delay <= 0
// means "unset" and would take the 500ms default; MaxDepth 0 is honoured
// literally as "seed only", so a test that wants link-following gets it here.
func smOpts() Options {
	return Options{MaxDepth: 1, Delay: time.Millisecond, IgnoreRobots: true}
}

// smOptsRobots is smOpts with robots.txt honoured, for the tests about it.
func smOptsRobots() Options { return Options{MaxDepth: 1, Delay: time.Millisecond} }

// recorder wraps a route set and remembers every path requested, so a test can
// assert what was NOT fetched — which is the whole point of SitemapOff and of
// the nesting and count caps.
type recorder struct {
	mu    sync.Mutex
	paths []string
}

func (r *recorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

func (r *recorder) count(path string) int {
	n := 0
	for _, p := range r.got() {
		if p == path {
			n++
		}
	}
	return n
}

func recordingSite(t *testing.T, routes map[string]http.HandlerFunc) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	mux := http.NewServeMux()
	for p, h := range routes {
		mux.HandleFunc(p, h)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.paths = append(rec.paths, r.URL.Path)
		rec.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// xmlBody serves a document verbatim as XML.
func xmlBody(doc string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(doc))
	}
}

const smNS = `xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"`

// urlset builds a <urlset> naming locs, absolute against base.
func urlset(base string, locs ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?><urlset %s>`, smNS)
	for _, l := range locs {
		fmt.Fprintf(&b, `<url><loc>%s%s</loc><lastmod>2024-03-01</lastmod></url>`, base, l)
	}
	b.WriteString(`</urlset>`)
	return b.String()
}

// sitemapindex builds a <sitemapindex> naming child sitemaps.
func sitemapindex(base string, locs ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?><sitemapindex %s>`, smNS)
	for _, l := range locs {
		fmt.Fprintf(&b, `<sitemap><loc>%s%s</loc></sitemap>`, base, l)
	}
	b.WriteString(`</sitemapindex>`)
	return b.String()
}

func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func wantPaths(t *testing.T, got, want []string) {
	t.Helper()
	g, w := strings.Join(sorted(got), ","), strings.Join(sorted(want), ",")
	if g != w {
		t.Fatalf("visited %s, want %s", g, w)
	}
}

// --- parsing -----------------------------------------------------------------

func TestParseSitemapURLSet(t *testing.T) {
	doc := `<?xml version="1.0"?><urlset ` + smNS + `>
	  <url><loc>https://x.dev/a</loc><lastmod>2024-03-01</lastmod></url>
	  <url><loc>  https://x.dev/b  </loc></url>
	  <url><priority>0.5</priority></url>
	</urlset>`
	urls, children, err := parseSitemap([]byte(doc))
	if err != nil {
		t.Fatalf("parseSitemap: %v", err)
	}
	if len(children) != 0 {
		t.Fatalf("children = %v, want none", children)
	}
	if len(urls) != 2 {
		t.Fatalf("urls = %+v, want 2", urls)
	}
	if urls[0].Loc != "https://x.dev/a" || urls[1].Loc != "https://x.dev/b" {
		t.Fatalf("locs = %q, %q", urls[0].Loc, urls[1].Loc)
	}
	if urls[0].LastModRaw != "2024-03-01" || urls[0].LastMod.Format("2006-01-02") != "2024-03-01" {
		t.Fatalf("lastmod = %q / %v", urls[0].LastModRaw, urls[0].LastMod)
	}
	if !urls[1].LastMod.IsZero() || urls[1].LastModRaw != "" {
		t.Fatalf("missing lastmod should stay zero, got %v / %q", urls[1].LastMod, urls[1].LastModRaw)
	}
}

func TestParseSitemapLastModFormats(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"2024-03-01", "2024-03-01"},
		{"2024-03-01T10:20:30Z", "2024-03-01"},
		{"2024-03-01T10:20:30+05:30", "2024-03-01"},
		{"2024-03-01T10:20Z", "2024-03-01"},
		{"2024-03", "2024-03-01"},
		{"not a date", ""},
	} {
		got := parseLastMod(tc.raw)
		if tc.want == "" {
			if !got.IsZero() {
				t.Errorf("parseLastMod(%q) = %v, want zero", tc.raw, got)
			}
			continue
		}
		if got.Format("2006-01-02") != tc.want {
			t.Errorf("parseLastMod(%q) = %v, want %s", tc.raw, got, tc.want)
		}
	}
}

// A sitemap with no namespace and one with the WRONG namespace must both parse:
// real sitemaps get this wrong constantly, and refusing them would mean falling
// back to a link crawl on sites that published a perfectly usable list.
func TestParseSitemapNamespaceTolerance(t *testing.T) {
	docs := map[string]string{
		"none":  `<urlset><url><loc>https://x.dev/a</loc></url></urlset>`,
		"wrong": `<urlset xmlns="http://example.com/not-sitemaps"><url><loc>https://x.dev/a</loc></url></urlset>`,
		"prefixed": `<sm:urlset xmlns:sm="http://www.sitemaps.org/schemas/sitemap/0.9">` +
			`<sm:url><sm:loc>https://x.dev/a</sm:loc></sm:url></sm:urlset>`,
	}
	for name, doc := range docs {
		urls, _, err := parseSitemap([]byte(doc))
		if err != nil {
			t.Fatalf("%s: parseSitemap: %v", name, err)
		}
		if len(urls) != 1 || urls[0].Loc != "https://x.dev/a" {
			t.Fatalf("%s: urls = %+v", name, urls)
		}
	}
}

func TestParseSitemapIndex(t *testing.T) {
	doc := sitemapindex("https://x.dev", "/one.xml", "/two.xml")
	urls, children, err := parseSitemap([]byte(doc))
	if err != nil {
		t.Fatalf("parseSitemap: %v", err)
	}
	if len(urls) != 0 {
		t.Fatalf("urls = %+v, want none", urls)
	}
	want := []string{"https://x.dev/one.xml", "https://x.dev/two.xml"}
	wantPaths(t, children, want)
}

func TestParseSitemapRejectsJunk(t *testing.T) {
	for name, doc := range map[string]string{
		"malformed":  `<urlset><url><loc>https://x.dev/a</loc>`,
		"empty":      ``,
		"wrong root": `<html><body>not a sitemap</body></html>`,
		"rss":        `<rss version="2.0"><channel><item><link>https://x.dev/a</link></item></channel></rss>`,
	} {
		if _, _, err := parseSitemap([]byte(doc)); err == nil {
			t.Errorf("%s: parseSitemap succeeded, want error", name)
		}
	}
}

func TestParseSitemapDirectives(t *testing.T) {
	body := []byte("User-agent: *\nDisallow: /x\n" +
		"Sitemap: https://x.dev/a.xml\n" +
		"sitemap:   https://x.dev/b.xml   # trailing comment\n" +
		"# Sitemap: https://x.dev/commented.xml\n" +
		"Sitemap:\n")
	wantPaths(t, parseSitemapDirectives(body), []string{"https://x.dev/a.xml", "https://x.dev/b.xml"})
}

func TestMaybeGunzip(t *testing.T) {
	plain := []byte(`<urlset/>`)
	if got, err := maybeGunzip(plain, maxSitemapBytes); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("plain passthrough: %q, %v", got, err)
	}
	gz := gzipBytes(t, string(plain))
	got, err := maybeGunzip(gz, maxSitemapBytes)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("gunzip: %q, %v", got, err)
	}
	if _, err := maybeGunzip(gz, 4); err == nil {
		t.Fatal("oversized decompression accepted, want error")
	}
}

// --- discovery through Crawl -------------------------------------------------

func TestCrawlSitemapSeedsFrontierAndStillFollowsLinks(t *testing.T) {
	var base string
	srv, _ := recordingSite(t, map[string]http.HandlerFunc{
		"/":       page("/linked"),
		"/only":   page("/deep"), // reachable ONLY via the sitemap
		"/deep":   page(),
		"/linked": page(),
		"/sitemap.xml": func(w http.ResponseWriter, r *http.Request) {
			xmlBody(urlset(base, "/only"))(w, r)
		},
	})
	base = srv.URL

	res, got := collect(t, base+"/", smOpts())

	// /only came from the sitemap; /linked from the seed's links; /deep from
	// /only's links, which proves link discovery still runs from a sitemap URL.
	wantPaths(t, got, []string{"/", "/only", "/linked", "/deep"})
	if res.FromSitemap != 1 || res.FromLinks != 2 {
		t.Fatalf("FromSitemap=%d FromLinks=%d, want 1 and 2", res.FromSitemap, res.FromLinks)
	}
	if len(res.Sitemaps) != 1 || !strings.HasSuffix(res.Sitemaps[0], "/sitemap.xml") {
		t.Fatalf("Sitemaps = %v", res.Sitemaps)
	}
}

func TestCrawlSitemapIndexFansOut(t *testing.T) {
	var base string
	srv, rec := recordingSite(t, map[string]http.HandlerFunc{
		"/":  page(),
		"/a": page(),
		"/b": page(),
		"/sitemap.xml": func(w http.ResponseWriter, r *http.Request) {
			xmlBody(sitemapindex(base, "/s1.xml", "/s2.xml"))(w, r)
		},
		"/s1.xml": func(w http.ResponseWriter, r *http.Request) { xmlBody(urlset(base, "/a"))(w, r) },
		"/s2.xml": func(w http.ResponseWriter, r *http.Request) { xmlBody(urlset(base, "/b"))(w, r) },
	})
	base = srv.URL

	res, got := collect(t, base+"/", smOpts())
	wantPaths(t, got, []string{"/", "/a", "/b"})
	if res.FromSitemap != 2 {
		t.Fatalf("FromSitemap = %d, want 2", res.FromSitemap)
	}
	if len(res.Sitemaps) != 3 {
		t.Fatalf("Sitemaps = %v, want the index and both children", res.Sitemaps)
	}
	if rec.count("/sitemap_index.xml") != 0 {
		t.Fatal("fell through to /sitemap_index.xml even though /sitemap.xml worked")
	}
}

func TestCrawlSitemapGzipped(t *testing.T) {
	// Two shapes, both common: a .xml.gz served as a plain gzip file (our own
	// sniffing handles it) and Content-Encoding: gzip (net/http handles it).
	for _, tc := range []struct {
		name     string
		encoding bool
	}{{"file", false}, {"content-encoding", true}} {
		t.Run(tc.name, func(t *testing.T) {
			var base string
			srv, _ := recordingSite(t, map[string]http.HandlerFunc{
				"/":  page(),
				"/a": page(),
				"/sitemap.xml": func(w http.ResponseWriter, r *http.Request) {
					body := gzipBytes(t, urlset(base, "/a"))
					if tc.encoding {
						w.Header().Set("Content-Type", "application/xml")
						w.Header().Set("Content-Encoding", "gzip")
					} else {
						w.Header().Set("Content-Type", "application/gzip")
					}
					_, _ = w.Write(body)
				},
			})
			base = srv.URL
			res, got := collect(t, base+"/", smOpts())
			wantPaths(t, got, []string{"/", "/a"})
			if res.FromSitemap != 1 {
				t.Fatalf("FromSitemap = %d, want 1", res.FromSitemap)
			}
		})
	}
}

// A gzip bomb must be rejected as an oversized sitemap, not decompressed into
// memory. The crawl carries on by falling back to link discovery.
func TestCrawlSitemapDecompressionBombBounded(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	chunk := make([]byte, 1<<20)
	for written := int64(0); written <= maxSitemapBytes; written += int64(len(chunk)) {
		if _, err := zw.Write(chunk); err != nil {
			t.Fatalf("gzip write: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	bomb := buf.Bytes()
	if len(bomb) > 1<<20 {
		t.Fatalf("bomb is %d bytes compressed; the test would not be cheap", len(bomb))
	}

	srv, _ := recordingSite(t, map[string]http.HandlerFunc{
		"/":  page("/a"),
		"/a": page(),
		"/sitemap.xml": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(bomb)
		},
	})
	res, got := collect(t, srv.URL+"/", smOpts())
	wantPaths(t, got, []string{"/", "/a"}) // link crawling took over
	if len(res.Sitemaps) != 0 {
		t.Fatalf("Sitemaps = %v, want none: the bomb must never parse", res.Sitemaps)
	}
	if res.FromSitemap != 0 {
		t.Fatalf("FromSitemap = %d, want 0", res.FromSitemap)
	}
}

// An index that lists itself must terminate — on the fetched-set, long before
// the depth cap.
func TestCrawlSitemapSelfReferencingIndexTerminates(t *testing.T) {
	var base string
	srv, rec := recordingSite(t, map[string]http.HandlerFunc{
		"/":  page(),
		"/a": page(),
		"/sitemap.xml": func(w http.ResponseWriter, r *http.Request) {
			// Points at itself AND at a real child.
			xmlBody(sitemapindex(base, "/sitemap.xml", "/s1.xml"))(w, r)
		},
		"/s1.xml": func(w http.ResponseWriter, r *http.Request) {
			xmlBody(sitemapindex(base, "/sitemap.xml"))(w, r) // and back again
		},
	})
	base = srv.URL

	done := make(chan struct{})
	var res Result
	var got []string
	go func() {
		defer close(done)
		res, got = collect(t, base+"/", smOpts())
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("self-referencing sitemap index did not terminate")
	}

	wantPaths(t, got, []string{"/"})
	if n := rec.count("/sitemap.xml"); n != 1 {
		t.Fatalf("/sitemap.xml fetched %d times, want exactly 1", n)
	}
	if n := rec.count("/s1.xml"); n != 1 {
		t.Fatalf("/s1.xml fetched %d times, want exactly 1", n)
	}
	if len(res.Sitemaps) != 2 {
		t.Fatalf("Sitemaps = %v, want the index and its one child, each once", res.Sitemaps)
	}
}

func TestCrawlSitemapNestingDepthCapped(t *testing.T) {
	var base string
	routes := map[string]http.HandlerFunc{"/": page(), "/deep": page()}
	// /sitemap.xml -> /s1 -> /s2 -> /s3 -> /s4, with /s4 the only urlset. With
	// the root at depth 0 and a cap of 3, /s3 is fetched but its children are
	// never queued, so /s4 and the URL it lists stay unseen.
	chain := []string{"/sitemap.xml", "/s1.xml", "/s2.xml", "/s3.xml"}
	for i, p := range chain {
		next := "/s4.xml"
		if i+1 < len(chain) {
			next = chain[i+1]
		}
		routes[p] = func(w http.ResponseWriter, r *http.Request) {
			xmlBody(sitemapindex(base, next))(w, r)
		}
	}
	routes["/s4.xml"] = func(w http.ResponseWriter, r *http.Request) { xmlBody(urlset(base, "/deep"))(w, r) }

	srv, rec := recordingSite(t, routes)
	base = srv.URL

	_, got := collect(t, base+"/", smOpts())
	wantPaths(t, got, []string{"/"})
	if rec.count("/s3.xml") != 1 {
		t.Fatalf("/s3.xml fetched %d times, want 1", rec.count("/s3.xml"))
	}
	if rec.count("/s4.xml") != 0 {
		t.Fatal("/s4.xml was fetched: nesting cap not enforced")
	}
}

func TestCrawlSitemapFetchCountCapped(t *testing.T) {
	var base string
	children := make([]string, 0, maxSitemapFetches+20)
	routes := map[string]http.HandlerFunc{"/": page()}
	for i := 0; i < maxSitemapFetches+20; i++ {
		p := fmt.Sprintf("/s%d.xml", i)
		children = append(children, p)
		// Empty urlsets: the cap is what is under test, not the URLs.
		routes[p] = func(w http.ResponseWriter, r *http.Request) { xmlBody(urlset(base))(w, r) }
	}
	routes["/sitemap.xml"] = func(w http.ResponseWriter, r *http.Request) {
		xmlBody(sitemapindex(base, children...))(w, r)
	}

	srv, rec := recordingSite(t, routes)
	base = srv.URL

	res, got := collect(t, base+"/", smOpts())
	wantPaths(t, got, []string{"/"})

	fetched := 0
	for _, p := range rec.got() {
		if strings.HasSuffix(p, ".xml") {
			fetched++
		}
	}
	if fetched != maxSitemapFetches {
		t.Fatalf("fetched %d sitemap documents, want the cap of %d", fetched, maxSitemapFetches)
	}
	if len(res.Sitemaps) != maxSitemapFetches {
		t.Fatalf("Sitemaps = %d, want %d", len(res.Sitemaps), maxSitemapFetches)
	}
	// Budget exhausted means we do NOT then go probing /sitemap_index.xml.
	if rec.count("/sitemap_index.xml") != 0 {
		t.Fatal("probed /sitemap_index.xml after exhausting the fetch budget")
	}
}

// A sitemap we cannot parse is not an error: it means "no sitemap here", and
// link crawling carries on as if the file had been missing.
func TestCrawlSitemapMalformedFallsBackToLinks(t *testing.T) {
	srv, _ := recordingSite(t, map[string]http.HandlerFunc{
		"/":                  page("/a"),
		"/a":                 page(),
		"/sitemap.xml":       xmlBody(`<urlset><url><loc>https://x.dev/a</loc>`),
		"/sitemap_index.xml": xmlBody(`{"not":"xml"}`),
	})
	res, got := collect(t, srv.URL+"/", smOpts())
	wantPaths(t, got, []string{"/", "/a"})
	if len(res.Sitemaps) != 0 {
		t.Fatalf("Sitemaps = %v, want none", res.Sitemaps)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("Errors = %v: a bad sitemap must not be reported as a page error", res.Errors)
	}
	if res.FromLinks != 1 {
		t.Fatalf("FromLinks = %d, want 1", res.FromLinks)
	}
}

func TestCrawlSitemapMissingFallsBackSilently(t *testing.T) {
	srv, rec := recordingSite(t, map[string]http.HandlerFunc{
		"/":  page("/a"),
		"/a": page(),
	})
	res, got := collect(t, srv.URL+"/", smOpts())
	wantPaths(t, got, []string{"/", "/a"})
	if len(res.Errors) != 0 {
		t.Fatalf("Errors = %v, want none", res.Errors)
	}
	if res.Skipped != 0 {
		t.Fatalf("Skipped = %d, want 0", res.Skipped)
	}
	// Both well-known locations were tried, in order, and neither existing is
	// simply not news.
	if rec.count("/sitemap.xml") != 1 || rec.count("/sitemap_index.xml") != 1 {
		t.Fatalf("probe counts: %v", rec.got())
	}
}

func TestCrawlSitemapIndexFallbackLocation(t *testing.T) {
	var base string
	srv, rec := recordingSite(t, map[string]http.HandlerFunc{
		"/":  page(),
		"/a": page(),
		"/sitemap_index.xml": func(w http.ResponseWriter, r *http.Request) {
			xmlBody(sitemapindex(base, "/s1.xml"))(w, r)
		},
		"/s1.xml": func(w http.ResponseWriter, r *http.Request) { xmlBody(urlset(base, "/a"))(w, r) },
	})
	base = srv.URL
	res, got := collect(t, base+"/", smOpts())
	wantPaths(t, got, []string{"/", "/a"})
	if res.FromSitemap != 1 {
		t.Fatalf("FromSitemap = %d, want 1", res.FromSitemap)
	}
	if rec.count("/sitemap.xml") != 1 {
		t.Fatal("/sitemap.xml should have been tried first")
	}
}

// robots.txt is where a site TELLS us where its sitemap is; that directive wins
// over the well-known locations and is never even followed by a probe.
func TestCrawlSitemapFromRobotsDirective(t *testing.T) {
	var base string
	srv, rec := recordingSite(t, map[string]http.HandlerFunc{
		"/":  page(),
		"/a": page(),
		"/robots.txt": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "User-agent: *\nAllow: /\nSitemap: %s/custom-sitemap.xml\n", base)
		},
		"/custom-sitemap.xml": func(w http.ResponseWriter, r *http.Request) {
			xmlBody(urlset(base, "/a"))(w, r)
		},
		"/sitemap.xml": xmlBody(urlset("", "/never")),
	})
	base = srv.URL

	res, got := collect(t, base+"/", smOptsRobots())
	wantPaths(t, got, []string{"/", "/a"})
	if res.FromSitemap != 1 {
		t.Fatalf("FromSitemap = %d, want 1", res.FromSitemap)
	}
	if rec.count("/custom-sitemap.xml") != 1 {
		t.Fatal("the Sitemap: directive was not used")
	}
	if rec.count("/sitemap.xml") != 0 {
		t.Fatal("probed /sitemap.xml even though robots.txt named a sitemap that worked")
	}
	if rec.count("/robots.txt") != 1 {
		t.Fatalf("robots.txt fetched %d times, want 1 (it must be cached)", rec.count("/robots.txt"))
	}
}

// --- a sitemap is a hint, not an authority -----------------------------------

// A sitemap naming another host must not send us there. No request is made to
// the foreign host at all, which is also why this test can name one.
func TestCrawlSitemapForeignHostRejected(t *testing.T) {
	var base string
	srv, _ := recordingSite(t, map[string]http.HandlerFunc{
		"/": page(),
		"/sitemap.xml": func(w http.ResponseWriter, r *http.Request) {
			doc := `<urlset ` + smNS + `>` +
				`<url><loc>https://evil.example.invalid/pwned</loc></url>` +
				`<url><loc>` + base + `/ok</loc></url>` +
				`</urlset>`
			xmlBody(doc)(w, r)
		},
		"/ok": page(),
	})
	base = srv.URL

	res, got := collect(t, base+"/", smOpts()) // SameHost defaults to true
	wantPaths(t, got, []string{"/", "/ok"})
	if res.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1 (the foreign URL)", res.Skipped)
	}
	for _, v := range res.Visited {
		if strings.Contains(v, "example.invalid") {
			t.Fatalf("visited a foreign host from a sitemap: %s", v)
		}
	}
}

// A sitemap cannot list its way past its own robots.txt.
func TestCrawlSitemapRobotsDisallowedURLSkipped(t *testing.T) {
	var base string
	srv, rec := recordingSite(t, map[string]http.HandlerFunc{
		"/": page(),
		"/robots.txt": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "User-agent: *\nDisallow: /private\n")
		},
		"/sitemap.xml": func(w http.ResponseWriter, r *http.Request) {
			xmlBody(urlset(base, "/public", "/private/secret"))(w, r)
		},
		"/public":         page(),
		"/private/secret": page(),
	})
	base = srv.URL

	res, got := collect(t, base+"/", smOptsRobots())
	wantPaths(t, got, []string{"/", "/public"})
	if res.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1 (the disallowed URL)", res.Skipped)
	}
	if rec.count("/private/secret") != 0 {
		t.Fatal("fetched a robots-disallowed URL because a sitemap listed it")
	}
}

func TestCrawlSitemapHonoursIncludeExclude(t *testing.T) {
	var base string
	srv, _ := recordingSite(t, map[string]http.HandlerFunc{
		"/": page(),
		"/sitemap.xml": func(w http.ResponseWriter, r *http.Request) {
			xmlBody(urlset(base, "/docs/a", "/docs/b", "/blog/c"))(w, r)
		},
		"/docs/a": page(),
		"/docs/b": page(),
		"/blog/c": page(),
	})
	base = srv.URL

	opts := smOpts()
	opts.Include = []string{`/docs/`}
	opts.Exclude = []string{`/docs/b$`}
	res, got := collect(t, base+"/", opts)
	wantPaths(t, got, []string{"/", "/docs/a"})
	if res.Skipped != 2 {
		t.Fatalf("Skipped = %d, want 2", res.Skipped)
	}
}

func TestCrawlSitemapRespectsMaxPages(t *testing.T) {
	var base string
	srv, _ := recordingSite(t, map[string]http.HandlerFunc{
		"/":  page(),
		"/a": page(),
		"/b": page(),
		"/c": page(),
		"/sitemap.xml": func(w http.ResponseWriter, r *http.Request) {
			xmlBody(urlset(base, "/a", "/b", "/c"))(w, r)
		},
	})
	base = srv.URL

	opts := smOpts()
	opts.MaxPages = 2
	res, got := collect(t, base+"/", opts)
	if len(got) != 2 {
		t.Fatalf("fetched %v, want 2 pages", got)
	}
	if !res.Truncated {
		t.Fatal("Truncated = false, want true")
	}
}

// The same URL reached both ways is fetched once, and counts as sitemap because
// that is where it entered the frontier.
func TestCrawlSitemapDedupesAgainstLinks(t *testing.T) {
	var base string
	srv, _ := recordingSite(t, map[string]http.HandlerFunc{
		"/":            page("/a"),
		"/a":           page(),
		"/sitemap.xml": func(w http.ResponseWriter, r *http.Request) { xmlBody(urlset(base, "/a"))(w, r) },
	})
	base = srv.URL
	res, got := collect(t, base+"/", smOpts())
	wantPaths(t, got, []string{"/", "/a"})
	if res.FromSitemap != 1 || res.FromLinks != 0 {
		t.Fatalf("FromSitemap=%d FromLinks=%d, want 1 and 0", res.FromSitemap, res.FromLinks)
	}
}

// --- the modes ---------------------------------------------------------------

func TestCrawlSitemapOnlyDoesNotFollowLinks(t *testing.T) {
	var base string
	srv, rec := recordingSite(t, map[string]http.HandlerFunc{
		"/":            page("/linked"),
		"/linked":      page(),
		"/a":           page("/from-a"),
		"/from-a":      page(),
		"/sitemap.xml": func(w http.ResponseWriter, r *http.Request) { xmlBody(urlset(base, "/a"))(w, r) },
	})
	base = srv.URL

	opts := smOpts()
	opts.Sitemap = SitemapOnly
	opts.MaxDepth = 5
	res, got := collect(t, base+"/", opts)
	wantPaths(t, got, []string{"/", "/a"})
	if rec.count("/linked") != 0 || rec.count("/from-a") != 0 {
		t.Fatalf("followed links in SitemapOnly mode: %v", rec.got())
	}
	if res.FromLinks != 0 {
		t.Fatalf("FromLinks = %d, want 0", res.FromLinks)
	}
}

// With no sitemap, SitemapOnly fetches the seed and stops. That is what "only
// the sitemap" means when there is no sitemap — not an error.
func TestCrawlSitemapOnlyWithNoSitemap(t *testing.T) {
	srv, _ := recordingSite(t, map[string]http.HandlerFunc{
		"/":  page("/a"),
		"/a": page(),
	})
	opts := smOpts()
	opts.Sitemap = SitemapOnly
	res, got := collect(t, srv.URL+"/", opts)
	wantPaths(t, got, []string{"/"})
	if len(res.Errors) != 0 {
		t.Fatalf("Errors = %v, want none", res.Errors)
	}
}

func TestCrawlSitemapOffNeverFetchesASitemap(t *testing.T) {
	var base string
	srv, rec := recordingSite(t, map[string]http.HandlerFunc{
		"/":            page("/a"),
		"/a":           page(),
		"/only":        page(),
		"/sitemap.xml": func(w http.ResponseWriter, r *http.Request) { xmlBody(urlset(base, "/only"))(w, r) },
	})
	base = srv.URL

	opts := smOpts()
	opts.Sitemap = SitemapOff
	res, got := collect(t, base+"/", opts)
	wantPaths(t, got, []string{"/", "/a"})
	for _, p := range rec.got() {
		if strings.Contains(p, "sitemap") {
			t.Fatalf("requested %s with Sitemap: SitemapOff", p)
		}
	}
	if len(res.Sitemaps) != 0 || res.FromSitemap != 0 {
		t.Fatalf("Sitemaps=%v FromSitemap=%d, want empty and 0", res.Sitemaps, res.FromSitemap)
	}
}

func TestCrawlRejectsUnknownSitemapMode(t *testing.T) {
	opts := smOpts()
	opts.Sitemap = SitemapMode(42)
	_, err := Crawl(context.Background(), "http://127.0.0.1:1/", opts, func(Page) error { return nil })
	if err == nil {
		t.Fatal("unknown sitemap mode accepted, want an error")
	}
}
