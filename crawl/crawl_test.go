package crawl

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

// No test in this package may touch the real network. Every server here is an
// httptest.Server bound to loopback, and the only URLs any test hands to Crawl
// point at one.

// fastOpts returns Options with the politeness delay turned down to something a
// test can afford. Delay <= 0 means "unset" and would take the 500ms default,
// so tests must pass a small positive value rather than zero.
func fastOpts() Options {
	return Options{Delay: time.Millisecond, IgnoreRobots: true}
}

// site spins up a server whose routes are (path -> handler).
func site(t *testing.T, routes map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for p, h := range routes {
		mux.HandleFunc(p, h)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func page(links ...string) http.HandlerFunc {
	var b strings.Builder
	b.WriteString("<html><body>")
	for _, l := range links {
		fmt.Fprintf(&b, `<a href=%q>x</a>`, l)
	}
	b.WriteString("</body></html>")
	body := b.String()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}
}

// collect runs a crawl and returns the paths visited, in order.
func collect(t *testing.T, seed string, opts Options) (Result, []string) {
	t.Helper()
	var got []string
	res, err := Crawl(context.Background(), seed, opts, func(p Page) error {
		got = append(got, pathOf(t, p.URL))
		return nil
	})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	return res, got
}

func pathOf(t *testing.T, raw string) string {
	t.Helper()
	i := strings.Index(raw, "//")
	rest := raw[i+2:]
	j := strings.Index(rest, "/")
	if j < 0 {
		return "/"
	}
	return rest[j:]
}

func TestCrawlDepthLimit(t *testing.T) {
	srv := site(t, map[string]http.HandlerFunc{
		"/a": page("/b"),
		"/b": page("/c"),
		"/c": page(),
	})
	opts := fastOpts()
	opts.MaxDepth = 1
	res, got := collect(t, srv.URL+"/a", opts)

	want := []string{"/a", "/b"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("visited %v, want %v", got, want)
	}
	if res.Fetched != 2 {
		t.Fatalf("Fetched = %d, want 2", res.Fetched)
	}
	if !res.Truncated {
		t.Fatal("Truncated = false; /c was in reach but beyond MaxDepth")
	}
}

func TestCrawlDepthZeroIsSeedOnly(t *testing.T) {
	srv := site(t, map[string]http.HandlerFunc{"/a": page("/b"), "/b": page()})
	opts := fastOpts()
	opts.MaxDepth = 0
	res, got := collect(t, srv.URL+"/a", opts)
	if len(got) != 1 || got[0] != "/a" {
		t.Fatalf("visited %v, want just /a", got)
	}
	if !res.Truncated {
		t.Fatal("Truncated = false, want true")
	}
}

func TestCrawlMaxPagesTruncates(t *testing.T) {
	srv := site(t, map[string]http.HandlerFunc{
		"/a": page("/b", "/c", "/d"),
		"/b": page(), "/c": page(), "/d": page(),
	})
	opts := fastOpts()
	opts.MaxDepth = 3
	opts.MaxPages = 2
	res, got := collect(t, srv.URL+"/a", opts)
	if len(got) != 2 {
		t.Fatalf("visited %v, want 2 pages", got)
	}
	if !res.Truncated {
		t.Fatal("Truncated = false after MaxPages stopped the crawl")
	}
	if res.Fetched != 2 || len(res.Visited) != 2 {
		t.Fatalf("Result = %+v, want Fetched/Visited of 2", res)
	}
}

func TestCrawlSameHostByDefaultAndCrossHostOptIn(t *testing.T) {
	other := site(t, map[string]http.HandlerFunc{"/x": page()})
	home := site(t, map[string]http.HandlerFunc{
		"/a": page("/b", other.URL+"/x"),
		"/b": page(),
	})

	// Default: SameHost is nil, which must mean true — the whole point of the
	// pointer is that forgetting the field cannot open the crawl onto the web.
	opts := fastOpts()
	opts.MaxDepth = 2
	res, got := collect(t, home.URL+"/a", opts)
	if len(got) != 2 {
		t.Fatalf("visited %v, want only the two same-host pages", got)
	}
	if res.Skipped == 0 {
		t.Fatal("Skipped = 0, want the cross-host link counted")
	}

	// Explicit opt-in.
	opts.SameHost = Ptr(false)
	_, got = collect(t, home.URL+"/a", opts)
	if len(got) != 3 {
		t.Fatalf("visited %v, want three pages with SameHost=Ptr(false)", got)
	}
}

func TestCrawlDedupNormalisesBeforeVisiting(t *testing.T) {
	var hits int
	srv := site(t, map[string]http.HandlerFunc{
		"/a": page("/target", "/./target", "/sub/../target", "/target#frag", "/target?x=1"),
		"/target": func(w http.ResponseWriter, r *http.Request) {
			hits++
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html></html>"))
		},
	})
	opts := fastOpts()
	opts.MaxDepth = 2
	res, got := collect(t, srv.URL+"/a", opts)

	// /target, /./target, /sub/../target and /target#frag all normalise to one
	// URL; /target?x=1 is a DIFFERENT document and is fetched separately.
	if hits != 2 {
		t.Fatalf("server saw %d hits on /target, want 2 (one bare, one queried)", hits)
	}
	if len(got) != 3 {
		t.Fatalf("visited %v, want 3", got)
	}
	if res.Fetched != 3 {
		t.Fatalf("Fetched = %d, want 3", res.Fetched)
	}
}

func TestCrawlDedupUsesFinalURLAfterRedirect(t *testing.T) {
	var hits int
	srv := site(t, map[string]http.HandlerFunc{
		"/a": page("/one", "/two", "/target"),
		"/one": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/target", http.StatusFound)
		},
		"/two": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/target", http.StatusMovedPermanently)
		},
		"/target": func(w http.ResponseWriter, r *http.Request) {
			hits++
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>t</html>"))
		},
	})
	opts := fastOpts()
	opts.MaxDepth = 2
	res, got := collect(t, srv.URL+"/a", opts)

	if len(got) != 2 {
		t.Fatalf("visited %v, want /a and /target once", got)
	}
	if got[1] != "/target" {
		t.Fatalf("second visit was %q, want the FINAL redirected URL /target", got[1])
	}
	if hits < 2 {
		t.Fatalf("server saw %d hits on /target; the redirects should still be followed", hits)
	}
	if res.Skipped < 2 {
		t.Fatalf("Skipped = %d, want the two redirect duplicates counted", res.Skipped)
	}
}

func TestCrawlIncludeExcludePrecedence(t *testing.T) {
	srv := site(t, map[string]http.HandlerFunc{
		"/a":         page("/docs/keep", "/docs/drop", "/blog/no"),
		"/docs/keep": page(),
		"/docs/drop": page(),
		"/blog/no":   page(),
	})
	opts := fastOpts()
	opts.MaxDepth = 2
	opts.Include = []string{`/docs/`}
	opts.Exclude = []string{`/docs/drop`} // Exclude wins over Include
	_, got := collect(t, srv.URL+"/a", opts)

	sort.Strings(got)
	want := []string{"/a", "/docs/keep"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("visited %v, want %v", got, want)
	}
}

func TestCrawlBadPatternIsAnError(t *testing.T) {
	opts := fastOpts()
	opts.Include = []string{"("}
	_, err := Crawl(context.Background(), "http://127.0.0.1:1/", opts, func(Page) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "bad include pattern") {
		t.Fatalf("err = %v, want a compile error naming the pattern", err)
	}
}

func TestCrawlRobotsDisallow(t *testing.T) {
	srv := site(t, map[string]http.HandlerFunc{
		"/robots.txt": robotsTxt("User-agent: *\nDisallow: /private\n"),
		"/a":          page("/private/x", "/public/y"),
		"/private/x":  page(),
		"/public/y":   page(),
	})
	opts := fastOpts()
	opts.IgnoreRobots = false
	opts.MaxDepth = 2
	res, got := collect(t, srv.URL+"/a", opts)

	sort.Strings(got)
	if strings.Join(got, ",") != "/a,/public/y" {
		t.Fatalf("visited %v, want /a and /public/y", got)
	}
	if res.Skipped == 0 {
		t.Fatal("Skipped = 0, want the disallowed page counted")
	}

	// IgnoreRobots bypasses it entirely.
	opts.IgnoreRobots = true
	_, got = collect(t, srv.URL+"/a", opts)
	if len(got) != 3 {
		t.Fatalf("visited %v with IgnoreRobots, want all 3", got)
	}
}

func TestCrawlRobotsAllowOverridesByLongestMatch(t *testing.T) {
	srv := site(t, map[string]http.HandlerFunc{
		"/robots.txt":     robotsTxt("User-agent: *\nDisallow: /docs\nAllow: /docs/public\n"),
		"/a":              page("/docs/secret", "/docs/public/ok"),
		"/docs/secret":    page(),
		"/docs/public/ok": page(),
	})
	opts := fastOpts()
	opts.IgnoreRobots = false
	opts.MaxDepth = 2
	_, got := collect(t, srv.URL+"/a", opts)

	sort.Strings(got)
	if strings.Join(got, ",") != "/a,/docs/public/ok" {
		t.Fatalf("visited %v, want the longer Allow to win over the Disallow", got)
	}
}

func TestCrawlMissingRobotsAllows(t *testing.T) {
	srv := site(t, map[string]http.HandlerFunc{
		"/a": page("/b"),
		"/b": page(),
	}) // no /robots.txt route: the mux answers 404
	opts := fastOpts()
	opts.IgnoreRobots = false
	opts.MaxDepth = 2
	_, got := collect(t, srv.URL+"/a", opts)
	if len(got) != 2 {
		t.Fatalf("visited %v, want both pages: a missing robots.txt means allowed", got)
	}
}

func TestCrawlRobotsCrawlDelayIsHonoured(t *testing.T) {
	srv := site(t, map[string]http.HandlerFunc{
		"/robots.txt": robotsTxt("User-agent: *\nCrawl-delay: 0.25\n"),
		"/a":          page("/b"),
		"/b":          page(),
	})
	opts := fastOpts()
	opts.IgnoreRobots = false
	opts.MaxDepth = 1

	start := time.Now()
	_, got := collect(t, srv.URL+"/a", opts)
	elapsed := time.Since(start)

	if len(got) != 2 {
		t.Fatalf("visited %v, want 2", got)
	}
	// robots.txt, then /a, then /b: at least two gaps of 250ms.
	if elapsed < 500*time.Millisecond {
		t.Fatalf("crawl took %v; Crawl-delay of 0.25s was not honoured", elapsed)
	}
}

func TestCrawlContextCancellationStopsMidCrawl(t *testing.T) {
	srv := site(t, map[string]http.HandlerFunc{
		"/a": page("/b", "/c"),
		"/b": page(), "/c": page(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	opts := fastOpts()
	opts.MaxDepth = 2
	var n int
	res, err := Crawl(ctx, srv.URL+"/a", opts, func(Page) error {
		n++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n != 1 {
		t.Fatalf("visited %d pages after cancel, want 1", n)
	}
	if res.Fetched != 1 {
		t.Fatalf("Fetched = %d, want the partial result to still be reported", res.Fetched)
	}
}

func TestCrawlVisitErrorAborts(t *testing.T) {
	srv := site(t, map[string]http.HandlerFunc{"/a": page("/b"), "/b": page()})
	boom := errors.New("boom")
	opts := fastOpts()
	opts.MaxDepth = 2
	_, err := Crawl(context.Background(), srv.URL+"/a", opts, func(Page) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the visit error", err)
	}
}

func TestCrawlNonHTMLIsFetchedButNotParsed(t *testing.T) {
	srv := site(t, map[string]http.HandlerFunc{
		"/a": page("/doc.pdf"),
		"/doc.pdf": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/pdf")
			// Link-shaped bytes inside a non-HTML document must never be
			// followed: a PDF is a document to mirror, not a link graph.
			_, _ = w.Write([]byte(`%PDF-1.4 <a href="/secret">s</a>`))
		},
		"/secret": page(),
	})
	opts := fastOpts()
	opts.MaxDepth = 3
	_, got := collect(t, srv.URL+"/a", opts)

	sort.Strings(got)
	if strings.Join(got, ",") != "/a,/doc.pdf" {
		t.Fatalf("visited %v, want /a and /doc.pdf only", got)
	}
}

func TestCrawlHTTPErrorIsRecordedNotFatal(t *testing.T) {
	srv := site(t, map[string]http.HandlerFunc{
		"/a": page("/gone", "/b"),
		"/gone": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusNotFound)
		},
		"/b": page(),
	})
	opts := fastOpts()
	opts.MaxDepth = 2
	res, got := collect(t, srv.URL+"/a", opts)
	if len(got) != 2 {
		t.Fatalf("visited %v, want the crawl to continue past the 404", got)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("Errors = %v, want the 404 recorded", res.Errors)
	}
}

func TestGetRejectsOversizedBody(t *testing.T) {
	srv := site(t, map[string]http.HandlerFunc{
		"/big": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
		},
	})
	n, err := normalise(fastOpts())
	if err != nil {
		t.Fatal(err)
	}
	c := &crawler{opts: n, client: newClient(n), last: map[string]time.Time{}}
	if _, _, _, err := c.get(context.Background(), srv.URL+"/big", 16); err == nil {
		t.Fatal("oversized body accepted; want an error rather than a truncated document")
	}
	// Under the cap it still works, so the check is not simply refusing everything.
	if _, _, _, err := c.get(context.Background(), srv.URL+"/big", 1<<20); err != nil {
		t.Fatalf("body within the cap: %v", err)
	}
}

func TestCrawlSkipsNonHTTPSchemes(t *testing.T) {
	srv := site(t, map[string]http.HandlerFunc{
		"/a": page("mailto:x@y.z", "javascript:alert(1)", "tel:+1", "data:text/html,x", "#top", "/b"),
		"/b": page(),
	})
	opts := fastOpts()
	opts.MaxDepth = 2
	res, got := collect(t, srv.URL+"/a", opts)
	if len(got) != 2 {
		t.Fatalf("visited %v, want /a and /b only", got)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("Errors = %v, want none", res.Errors)
	}
}

func TestCrawlRejectsBadSeed(t *testing.T) {
	for _, seed := range []string{"ftp://x/y", "not a url", "file:///etc/passwd", ""} {
		if _, err := Crawl(context.Background(), seed, fastOpts(), func(Page) error { return nil }); err == nil {
			t.Fatalf("seed %q accepted, want an error", seed)
		}
	}
}

func TestNormaliseURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://EXAMPLE.com/a", "http://example.com/a"},
		{"http://example.com:80/a", "http://example.com/a"},
		{"https://example.com:443/a", "https://example.com/a"},
		{"http://example.com:8080/a", "http://example.com:8080/a"},
		{"http://example.com", "http://example.com/"},
		{"http://example.com/a#frag", "http://example.com/a"},
		{"http://example.com/x/../a", "http://example.com/a"},
		{"http://example.com/./a", "http://example.com/a"},
		{"http://example.com/a/", "http://example.com/a/"}, // trailing slash is significant
		{"http://example.com/a?b=1", "http://example.com/a?b=1"},
	}
	for _, c := range cases {
		u, err := parseAbsolute(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if got := normaliseURL(u); got != c.want {
			t.Errorf("normaliseURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLocalPath(t *testing.T) {
	cases := []struct{ in, ext, want string }{
		{"https://x.dev/docs/a", "", "docs/a.md"},
		{"https://x.dev/docs/a", "md", "docs/a.md"},
		{"https://x.dev/docs/a", ".txt", "docs/a.txt"},
		{"https://x.dev/docs/", "", "docs/index.md"},
		{"https://x.dev/", "", "index.md"},
		{"https://x.dev", "", "index.md"},
		{"https://x.dev/a.html", "", "a.html.md"}, // keeps its own extension, so /a.html != /a
		{"https://x.dev/x/../a", "", "a.md"},
	}
	for _, c := range cases {
		got, err := LocalPath(c.in, c.ext)
		if err != nil {
			t.Fatalf("LocalPath(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("LocalPath(%q, %q) = %q, want %q", c.in, c.ext, got, c.want)
		}
	}
}

// TestLocalPathNeverEscapes is the security test. LocalPath decides where
// attacker-controlled bytes get written, so nothing it returns may be absolute,
// contain a "..", a separator that is not "/", a backslash, a NUL, a control
// character, or a drive letter.
func TestLocalPathNeverEscapes(t *testing.T) {
	hostile := []string{
		"https://x.dev/../../etc/passwd",
		"https://x.dev/../../../../../../etc/shadow",
		"https://x.dev/%2e%2e/%2e%2e/etc/passwd",
		"https://x.dev/a/..%2f..%2fetc/passwd",
		"https://x.dev//etc/passwd",
		"https://x.dev/C:/Windows/system32",
		`https://x.dev/a\..\..\b`,
		"https://x.dev/" + strings.Repeat("a", 5000),
		"https://x.dev/" + strings.Repeat("a/", 500),
		"../../etc/passwd",
		"/etc/passwd",
		`\\server\share\x`,
		"https://x.dev/~/.ssh/id_rsa",
		"https://x.dev/.git/config",
		"https://x.dev/a\u0001b",
		"https://x.dev/a\u202eb",
		"https://x.dev/%00",
		"https://x.dev/con",
	}
	for _, in := range hostile {
		got, err := LocalPath(in, ".md")
		if err != nil {
			continue // refusing outright is an acceptable answer
		}
		if got == "" {
			t.Errorf("LocalPath(%q) returned an empty path", in)
			continue
		}
		if strings.HasPrefix(got, "/") || strings.HasPrefix(got, `\`) {
			t.Errorf("LocalPath(%q) = %q: absolute", in, got)
		}
		if strings.Contains(got, "..") {
			t.Errorf("LocalPath(%q) = %q: contains ..", in, got)
		}
		if strings.ContainsAny(got, "\\\x00:") {
			t.Errorf("LocalPath(%q) = %q: contains a backslash, NUL or colon", in, got)
		}
		for _, r := range got {
			if r < 0x20 || r == 0x7f || r > 0x7f {
				t.Errorf("LocalPath(%q) = %q: non-printable or non-ASCII rune %q", in, got, r)
				break
			}
		}
		for _, seg := range strings.Split(got, "/") {
			if seg == "" || seg == "." || seg == ".." {
				t.Errorf("LocalPath(%q) = %q: bad segment %q", in, got, seg)
			}
			if len(seg) > maxSegment+16 {
				t.Errorf("LocalPath(%q) = %q: segment of %d bytes", in, got, len(seg))
			}
		}
		if len(got) > maxPathLen {
			t.Errorf("LocalPath(%q) = %q: %d bytes, over the cap", in, got, len(got))
		}
	}
}

// TestLocalPathNoCollisions proves distinct URLs never land on one file — the
// query string, the trailing slash and the sanitised characters all have to be
// disambiguated, not flattened.
func TestLocalPathNoCollisions(t *testing.T) {
	urls := []string{
		"https://x.dev/a",
		"https://x.dev/a/",
		"https://x.dev/a?b=1",
		"https://x.dev/a?b=2",
		"https://x.dev/a?",
		"https://x.dev/a.html",
		"https://x.dev/a.md",
		"https://x.dev/a%20b",
		"https://x.dev/a-b",
		"https://x.dev/a/b",
		"https://x.dev/a/b/",
		"https://x.dev/A",
		"https://x.dev/" + strings.Repeat("z", 300),
		"https://x.dev/" + strings.Repeat("z", 301),
		"https://x.dev/" + strings.Repeat("q/", 200) + "end",
		"https://x.dev/" + strings.Repeat("q/", 200) + "end2",
	}
	seen := map[string]string{}
	for _, u := range urls {
		got, err := LocalPath(u, ".md")
		if err != nil {
			t.Fatalf("LocalPath(%q): %v", u, err)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("collision: %q and %q both map to %q", prev, u, got)
		}
		seen[got] = u
	}
}

// A path that already looks like one of our hash suffixes must not be able to
// impersonate a disambiguated name.
func TestLocalPathHashSuffixCannotBeImpersonated(t *testing.T) {
	queried, err := LocalPath("https://x.dev/a?b=1", ".md")
	if err != nil {
		t.Fatal(err)
	}
	literal, err := LocalPath("https://x.dev/"+strings.TrimSuffix(queried, ".md"), ".md")
	if err != nil {
		t.Fatal(err)
	}
	if literal == queried {
		t.Fatalf("a literal path %q collided with the hashed name for /a?b=1", literal)
	}
}

func TestLocalPathIsDeterministic(t *testing.T) {
	for _, u := range []string{"https://x.dev/docs/a", "https://x.dev/a?b=1", "https://x.dev/"} {
		a, _ := LocalPath(u, ".md")
		b, _ := LocalPath(u, ".md")
		if a != b {
			t.Fatalf("LocalPath(%q) not deterministic: %q vs %q", u, a, b)
		}
	}
}

func TestNormaliseDefaults(t *testing.T) {
	n, err := normalise(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if n.maxDepth != 0 {
		t.Errorf("maxDepth = %d, want 0 honoured literally", n.maxDepth)
	}
	if n.maxPages != defaultMaxPages {
		t.Errorf("maxPages = %d, want %d", n.maxPages, defaultMaxPages)
	}
	if !n.sameHost {
		t.Error("sameHost = false for a zero Options; the default must be true")
	}
	if n.delay != defaultDelay {
		t.Errorf("delay = %v, want %v", n.delay, defaultDelay)
	}
	if !strings.Contains(n.userAgent, "anymd") || !strings.Contains(n.userAgent, "github.com") {
		t.Errorf("userAgent = %q, want it to name anymd and its repository", n.userAgent)
	}
	if !n.robots {
		t.Error("robots = false, want robots.txt honoured by default")
	}
	if n2, _ := normalise(Options{MaxDepth: -1}); n2.maxDepth != defaultMaxDepth {
		t.Errorf("maxDepth = %d for a negative MaxDepth, want the default %d", n2.maxDepth, defaultMaxDepth)
	}
}

func TestCrawlSendsUserAgent(t *testing.T) {
	var seen string
	srv := site(t, map[string]http.HandlerFunc{
		"/a": func(w http.ResponseWriter, r *http.Request) {
			seen = r.Header.Get("User-Agent")
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html></html>"))
		},
	})
	if _, err := Crawl(context.Background(), srv.URL+"/a", fastOpts(), func(Page) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if seen != DefaultUserAgent {
		t.Fatalf("User-Agent = %q, want %q", seen, DefaultUserAgent)
	}
}

func TestCrawlRedirectLoopIsBounded(t *testing.T) {
	var n int
	srv := site(t, map[string]http.HandlerFunc{
		"/loop": func(w http.ResponseWriter, r *http.Request) {
			n++
			http.Redirect(w, r, "/loop?n="+fmt.Sprint(n), http.StatusFound)
		},
	})
	res, err := Crawl(context.Background(), srv.URL+"/loop", fastOpts(), func(Page) error { return nil })
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("Errors = %v, want the redirect chain recorded as one error", res.Errors)
	}
	if n > maxRedirects+1 {
		t.Fatalf("server saw %d requests, want the chain capped at %d", n, maxRedirects+1)
	}
}

func TestCrawlBaseHrefIsHonoured(t *testing.T) {
	srv := site(t, map[string]http.HandlerFunc{
		"/deep/a": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><head><base href="` + srv0(r) + `/root/"></head><body><a href="b">b</a></body></html>`))
		},
		"/root/b": page(),
	})
	opts := fastOpts()
	opts.MaxDepth = 1
	_, got := collect(t, srv.URL+"/deep/a", opts)
	sort.Strings(got)
	if strings.Join(got, ",") != "/deep/a,/root/b" {
		t.Fatalf("visited %v, want <base href> to have been applied", got)
	}
}

func srv0(r *http.Request) string { return "http://" + r.Host }
