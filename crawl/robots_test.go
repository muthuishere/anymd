package crawl

import (
	"net/http"
	"testing"
	"time"
)

// robotsTxt serves a robots.txt body. It lives here because the robots tests in
// crawl_test.go need it too.
func robotsTxt(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(body))
	}
}

func TestParseRobotsGroupSelection(t *testing.T) {
	body := []byte(`
# a comment
User-agent: *
Disallow: /everyone

User-agent: anymd-crawler
Disallow: /just-us
Crawl-delay: 2
`)
	// Our agent's own group wins over "*", and only its rules apply.
	r := parseRobots(body, "anymd-crawler/1.0 (+https://example.invalid)")
	if r == nil {
		t.Fatal("parseRobots returned nil")
	}
	if r.allows("/just-us", "") {
		t.Error("/just-us allowed; our own group disallows it")
	}
	if !r.allows("/everyone", "") {
		t.Error("/everyone disallowed; that rule belongs to the * group, not ours")
	}
	if r.crawlDelay != 2*time.Second {
		t.Errorf("crawlDelay = %v, want 2s", r.crawlDelay)
	}

	// An agent with no group of its own falls back to "*".
	r = parseRobots(body, "someone-else/1.0")
	if r.allows("/everyone", "") {
		t.Error("/everyone allowed for an unnamed agent; the * group applies")
	}
	if !r.allows("/just-us", "") {
		t.Error("/just-us disallowed for an unnamed agent; that group is not theirs")
	}
}

func TestParseRobotsConsecutiveAgentsShareOneGroup(t *testing.T) {
	body := []byte("User-agent: alpha\nUser-agent: beta\nDisallow: /x\n")
	for _, ua := range []string{"alpha", "beta"} {
		r := parseRobots(body, ua)
		if r == nil || r.allows("/x", "") {
			t.Errorf("%s: /x allowed, want the shared group to apply", ua)
		}
	}
	if r := parseRobots(body, "gamma"); r != nil {
		t.Error("gamma got rules; there is no * group and none named it")
	}
}

func TestRobotsLongestMatchWinsAndAllowBreaksTies(t *testing.T) {
	r := parseRobots([]byte("User-agent: *\nDisallow: /docs\nAllow: /docs/public\n"), "x")
	if r.allows("/docs/secret", "") {
		t.Error("/docs/secret allowed, want the Disallow to apply")
	}
	if !r.allows("/docs/public/page", "") {
		t.Error("/docs/public/page disallowed, want the longer Allow to win")
	}

	// Equal-length rules: Allow wins, which is the conventional tie-break.
	tie := parseRobots([]byte("User-agent: *\nDisallow: /x\nAllow: /x\n"), "x")
	if !tie.allows("/x", "") {
		t.Error("/x disallowed on an exact tie, want Allow to win")
	}
}

func TestRobotsEmptyDisallowAllowsEverything(t *testing.T) {
	r := parseRobots([]byte("User-agent: *\nDisallow:\n"), "x")
	if r == nil {
		t.Fatal("nil rules")
	}
	if !r.allows("/anything", "") {
		t.Error("an empty Disallow must mean allow everything")
	}
}

func TestRobotsDisallowAll(t *testing.T) {
	r := parseRobots([]byte("User-agent: *\nDisallow: /\n"), "x")
	for _, p := range []string{"/", "/a", "/a/b/c"} {
		if r.allows(p, "") {
			t.Errorf("%s allowed under Disallow: /", p)
		}
	}
}

func TestRobotsWildcardAndAnchor(t *testing.T) {
	r := parseRobots([]byte("User-agent: *\nDisallow: /*.pdf$\nDisallow: /a/*/private\n"), "x")
	cases := []struct {
		path  string
		allow bool
	}{
		{"/x.pdf", false},
		{"/deep/y.pdf", false},
		{"/x.pdf.html", true}, // "$" anchors the end
		{"/a/one/private", false},
		{"/a/one/two/private", false},
		{"/a/private", true}, // "*" needs something between the slashes
		{"/b/one/private", true},
	}
	for _, c := range cases {
		if got := r.allows(c.path, ""); got != c.allow {
			t.Errorf("allows(%q) = %v, want %v", c.path, got, c.allow)
		}
	}
}

func TestRobotsMatchesQueryString(t *testing.T) {
	r := parseRobots([]byte("User-agent: *\nDisallow: /s?q=\n"), "x")
	if r.allows("/s", "q=secret") {
		t.Error("/s?q=secret allowed; the pattern includes the query")
	}
	if !r.allows("/s", "page=2") {
		t.Error("/s?page=2 disallowed; the pattern does not match it")
	}
}

func TestRobotsUnparseableMeansAllowed(t *testing.T) {
	for _, body := range []string{
		"",
		"total nonsense with no colons at all",
		"<html><body>404 not found</body></html>",
		"Disallow: /x\n", // a rule with no User-agent belongs to nobody
	} {
		r := parseRobots([]byte(body), "x")
		if r != nil && !r.allows("/anything", "") {
			t.Errorf("body %q disallowed /anything; unparseable robots.txt means allowed", body)
		}
	}
	// And the nil receiver — the value cached for a missing robots.txt — allows.
	var nilRules *robotsFile
	if !nilRules.allows("/x", "") {
		t.Error("nil rules disallowed; a missing robots.txt means allowed")
	}
}

func TestRobotsIsCaseInsensitiveOnKeysAndAgents(t *testing.T) {
	r := parseRobots([]byte("USER-AGENT: ANYMD-CRAWLER\nDISALLOW: /x\n"), "anymd-crawler/1.0")
	if r == nil || r.allows("/x", "") {
		t.Error("uppercase keys or agent names were not matched")
	}
}

func TestAgentToken(t *testing.T) {
	cases := map[string]string{
		"anymd-crawler (+https://github.com/muthuishere/anymd)": "anymd-crawler",
		"anymd-crawler/1.2": "anymd-crawler",
		"  Googlebot  ":     "googlebot",
		"":                  "",
		"a/b c":             "a",
	}
	if got := agentToken(DefaultUserAgent); got != "anymd-crawler" {
		t.Errorf("agentToken(DefaultUserAgent) = %q, want anymd-crawler", got)
	}
	for in, want := range cases {
		if got := agentToken(in); got != want {
			t.Errorf("agentToken(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRobotsCrawlDelayFractional(t *testing.T) {
	r := parseRobots([]byte("User-agent: *\nCrawl-delay: 0.5\n"), "x")
	if r == nil || r.crawlDelay != 500*time.Millisecond {
		t.Fatalf("crawlDelay = %v, want 500ms", r.crawlDelay)
	}
	// A junk value must not become a huge or negative delay.
	if r := parseRobots([]byte("User-agent: *\nCrawl-delay: soon\n"), "x"); r.crawlDelay != 0 {
		t.Errorf("crawlDelay = %v for a junk value, want 0", r.crawlDelay)
	}
	if r := parseRobots([]byte("User-agent: *\nCrawl-delay: -5\n"), "x"); r.crawlDelay != 0 {
		t.Errorf("crawlDelay = %v for a negative value, want 0", r.crawlDelay)
	}
}
