package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSite serves a three-page site: an index and two pages under /docs that
// link to each other and back. httptest only — no test in this package may
// touch the real network.
func fakeSite(t *testing.T) *httptest.Server {
	t.Helper()
	pages := map[string]string{
		"/": `<html><head><title>Home</title></head><body>
			<h1>Home</h1>
			<p><a href="/docs/a.html">A</a> and <a href="docs/b.html">B</a></p>
			<p><a href="https://elsewhere.example/x">offsite</a></p>
		</body></html>`,
		"/docs/a.html": `<html><body><h1>A</h1>
			<p><a href="b.html">to B</a>, <a href="/">home</a>,
			<a href="/docs/never.html">uncrawled</a>,
			<a href="mailto:x@y.dev">mail</a></p>
			<pre><code>see &lt;a href="/docs/b.html"&gt;</code></pre>
		</body></html>`,
		"/docs/b.html": `<html><body><h1>B</h1>
			<p><a href="a.html">back to A</a></p>
		</body></html>`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, body)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func readFile(t *testing.T, parts ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	return string(b)
}

// TestCrawlFlagWithoutCrawl is the --llm-model precedent: a flag that was typed
// and then silently ignored is how an afternoon disappears.
func TestCrawlFlagWithoutCrawl(t *testing.T) {
	cases := [][]string{
		{"--depth", "2", "https://x.dev/"},
		{"--max-pages", "10", "https://x.dev/"},
		{"--crawl-delay", "1s", "https://x.dev/"},
		{"--same-host=false", "https://x.dev/"},
		{"--include", "docs", "https://x.dev/"},
		{"--exclude", "docs", "https://x.dev/"},
		{"--ignore-robots", "https://x.dev/"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, out, errOut := runCLI(t, "", args...)
			if code != exitUsage {
				t.Fatalf("exit = %d, want %d (stderr %q)", code, exitUsage, errOut)
			}
			if !strings.Contains(errOut, "requires --crawl") {
				t.Fatalf("stderr = %q, want it to name --crawl", errOut)
			}
			if out != "" {
				t.Fatalf("stdout = %q, want empty", out)
			}
		})
	}
}

// TestCrawlUsageErrors covers the command lines that cannot mean anything.
func TestCrawlUsageErrors(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no output directory", []string{"--crawl", "https://x.dev/"}, "output directory"},
		{"-o instead of -d", []string{"--crawl", "-o", filepath.Join(dir, "f.md"), "https://x.dev/"}, "use -d DIR"},
		{"no seed", []string{"--crawl", "-d", dir}, "at least one URL"},
		{"a file, not a URL", []string{"--crawl", "-d", dir, "notes.txt"}, "http(s) URLs"},
		{"bad include pattern", []string{"--crawl", "-d", dir, "--include", "([", "https://x.dev/"}, "bad pattern"},
		{"negative depth", []string{"--crawl", "-d", dir, "--depth", "-2", "https://x.dev/"}, "--depth"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := runCLI(t, "", tc.args...)
			if code != exitUsage {
				t.Fatalf("exit = %d, want %d (stderr %q)", code, exitUsage, errOut)
			}
			if !strings.Contains(errOut, tc.want) {
				t.Fatalf("stderr = %q, want it to mention %q", errOut, tc.want)
			}
			if out != "" {
				t.Fatalf("stdout = %q, want empty", out)
			}
		})
	}
}

// TestCrawlThreePageSite is the whole feature end to end: fetch, convert,
// place, rewrite, write.
func TestCrawlThreePageSite(t *testing.T) {
	ts := fakeSite(t)
	out := t.TempDir()

	code, stdout, stderr := runCLI(t, "",
		"--crawl", "--depth", "1", "--ignore-robots", "--crawl-delay", "1ms", "-d", out, ts.URL)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	// A crawl's product is files. Anything on stdout would corrupt a pipe.
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "crawled ") {
		t.Fatalf("no progress on stderr: %q", stderr)
	}
	if !strings.Contains(stderr, "crawl: fetched 3, written 3, skipped") {
		t.Fatalf("summary missing or wrong: %q", stderr)
	}
	// /docs/never.html sits one link past --depth 1, so the run was truncated
	// and must say so — "that is the whole site" and "I stopped early" are
	// different answers.
	if !strings.Contains(stderr, "truncated") {
		t.Fatalf("a depth-capped crawl must report truncation: %q", stderr)
	}

	index := readFile(t, out, "index.md")
	a := readFile(t, out, "docs", "a.html.md")
	_ = readFile(t, out, "docs", "b.html.md")

	// Down a level from the root.
	for _, want := range []string{"(docs/a.html.md)", "(docs/b.html.md)"} {
		if !strings.Contains(index, want) {
			t.Errorf("index.md missing %s:\n%s", want, index)
		}
	}
	// Sideways within docs/, and back up to the root.
	if !strings.Contains(a, "(b.html.md)") {
		t.Errorf("docs/a.html.md should link to its sibling as b.html.md:\n%s", a)
	}
	if !strings.Contains(a, "(../index.md)") {
		t.Errorf("docs/a.html.md should link up as ../index.md:\n%s", a)
	}
	// Not crawled, so still absolute — a partial mirror must not break links.
	if !strings.Contains(a, ts.URL+"/docs/never.html") && !strings.Contains(a, "(/docs/never.html)") {
		t.Errorf("uncrawled link should have been left alone:\n%s", a)
	}
	if !strings.Contains(a, "mailto:x@y.dev") {
		t.Errorf("mailto: should have been left alone:\n%s", a)
	}
	// The URL inside the code sample is content, not navigation.
	if strings.Contains(a, "b.html.md\"&gt;") || strings.Contains(a, `href="b.html.md"`) {
		t.Errorf("a URL inside a code block was rewritten:\n%s", a)
	}
	// Off-host, and --same-host defaults to true, so it was never fetched.
	if _, err := os.Stat(filepath.Join(out, "x.md")); err == nil {
		t.Error("crawl left the seed's host despite the default --same-host")
	}
}

// TestCrawlQuiet: -q silences progress, but never an error.
func TestCrawlQuiet(t *testing.T) {
	ts := fakeSite(t)
	out := t.TempDir()
	code, stdout, stderr := runCLI(t, "",
		"--crawl", "-q", "--depth", "1", "--ignore-robots", "--crawl-delay", "1ms", "-d", out, ts.URL)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if stdout != "" || stderr != "" {
		t.Fatalf("-q should be silent: stdout %q stderr %q", stdout, stderr)
	}
}

// TestCrawlFailingPageDoesNotAbort: one bad page costs one page, not the crawl.
func TestCrawlFailingPageDoesNotAbort(t *testing.T) {
	ts := fakeSite(t)
	out := t.TempDir()

	prev := localPathFn
	localPathFn = func(rawURL, ext string) (string, error) {
		if strings.HasSuffix(rawURL, "/docs/b.html") {
			return "", errors.New("synthetic failure")
		}
		return prev(rawURL, ext)
	}
	t.Cleanup(func() { localPathFn = prev })

	code, stdout, stderr := runCLI(t, "",
		"--crawl", "--depth", "1", "--ignore-robots", "--crawl-delay", "1ms", "-d", out, ts.URL)
	if code != exitFail {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, exitFail, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "synthetic failure") {
		t.Fatalf("the failure was not reported: %q", stderr)
	}
	if !strings.Contains(stderr, "written 2") {
		t.Fatalf("the other pages should still have been written: %q", stderr)
	}
	// The two good pages are there...
	a := readFile(t, out, "docs", "a.html.md")
	readFile(t, out, "index.md")
	// ...and the link to the page that failed stayed absolute, so it still works.
	if strings.Contains(a, "(b.html.md)") {
		t.Errorf("a link to a page that failed must not be localised:\n%s", a)
	}
	if _, err := os.Stat(filepath.Join(out, "docs", "b.html.md")); err == nil {
		t.Error("the failed page was written anyway")
	}
}

// TestUsageListsCrawlFlags guards against the cacheFlagUsage mistake: a usage
// block that exists but was never spliced into --help.
func TestUsageListsCrawlFlags(t *testing.T) {
	code, stdout, stderr := runCLI(t, "", "-h")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if stdout != "" {
		t.Fatalf("usage belongs on stderr, got stdout %q", stdout)
	}
	for _, f := range append([]string{"--crawl"}, crawlFlagNames...) {
		name := f
		if !strings.HasPrefix(name, "--") {
			name = "--" + name
		}
		if !strings.Contains(stderr, name) {
			t.Errorf("--help does not mention %s", name)
		}
	}
}

// TestSafeJoin is the containment check. These paths come from remote servers.
func TestSafeJoin(t *testing.T) {
	root := t.TempDir()
	bad := []string{"", "../evil.md", "../../evil.md", "a/../../evil.md", "/etc/passwd", `\evil.md`}
	for _, rel := range bad {
		if p, err := safeJoin(root, rel); err == nil {
			t.Errorf("safeJoin(%q) = %q, want an error", rel, p)
		}
	}
	good := map[string]string{
		"index.md":      filepath.Join(root, "index.md"),
		"docs/b.md":     filepath.Join(root, "docs", "b.md"),
		"docs/../a.md":  filepath.Join(root, "a.md"),
		"./docs/deep/c": filepath.Join(root, "docs", "deep", "c"),
	}
	for rel, want := range good {
		got, err := safeJoin(root, rel)
		if err != nil {
			t.Errorf("safeJoin(%q): %v", rel, err)
			continue
		}
		if got != want {
			t.Errorf("safeJoin(%q) = %q, want %q", rel, got, want)
		}
	}
}

func TestUniquePath(t *testing.T) {
	taken := map[string]string{"docs/a.md": "https://x.dev/a"}
	if got := uniquePath(taken, "docs/a.md", "https://x.dev/a", ".md"); got != "docs/a.md" {
		t.Errorf("the same URL should keep its path, got %q", got)
	}
	if got := uniquePath(taken, "docs/a.md", "https://x.dev/other", ".md"); got != "docs/a-2.md" {
		t.Errorf("a colliding URL should be suffixed, got %q", got)
	}
}
