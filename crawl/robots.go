package crawl

import (
	"strconv"
	"strings"
	"time"
)

// robotsFile is the subset of robots.txt that matters to a bounded document
// crawler: the rule group that applies to us, and the Crawl-delay it asks for.
//
// It is deliberately minimal. robots.txt has no ratified grammar, only a de
// facto one, and a parser that guesses at the exotic corners of it is a parser
// that will one day guess "allowed" when the site meant "no". Everything this
// file does not understand is ignored, and anything it cannot parse at all
// means "crawling is allowed" — that is the standard's behaviour and the
// behaviour every major crawler implements.
type robotsFile struct {
	rules      []robotsRule
	crawlDelay time.Duration
}

// robotsRule is one Allow or Disallow line.
type robotsRule struct {
	pattern string
	allow   bool
}

// group is one User-agent block while parsing.
type group struct {
	agents []string
	rules  []robotsRule
	delay  time.Duration
}

// parseRobots parses robots.txt and returns the rules that apply to userAgent.
//
// Group selection follows the convention: the group naming our agent token
// exactly (case-insensitively) wins; otherwise the "*" group applies; otherwise
// nothing is disallowed. Our agent token is the User-Agent header up to the
// first "/" or space, which is how a site operator would write it.
func parseRobots(data []byte, userAgent string) *robotsFile {
	token := agentToken(userAgent)

	var groups []*group
	var cur *group
	// newGroup is true while we are still collecting consecutive User-agent
	// lines, so "User-agent: a\nUser-agent: b\nDisallow: /x" is ONE group.
	startingGroup := false

	for _, raw := range strings.Split(string(data), "\n") {
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
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)

		switch key {
		case "user-agent":
			if cur == nil || !startingGroup {
				cur = &group{}
				groups = append(groups, cur)
				startingGroup = true
			}
			cur.agents = append(cur.agents, strings.ToLower(value))
		case "disallow", "allow":
			if cur == nil {
				continue // a rule before any User-agent line belongs to nobody
			}
			startingGroup = false
			if value == "" {
				// An empty Disallow means "allow everything"; an empty Allow
				// means nothing. Either way there is no rule to record.
				continue
			}
			cur.rules = append(cur.rules, robotsRule{pattern: value, allow: key == "allow"})
		case "crawl-delay":
			if cur == nil {
				continue
			}
			startingGroup = false
			if secs, err := strconv.ParseFloat(value, 64); err == nil && secs > 0 {
				cur.delay = time.Duration(secs * float64(time.Second))
			}
		}
	}

	var star, exact *group
	for _, g := range groups {
		for _, a := range g.agents {
			if a == "*" && star == nil {
				star = g
			}
			if token != "" && a == token && exact == nil {
				exact = g
			}
		}
	}
	chosen := exact
	if chosen == nil {
		chosen = star
	}
	if chosen == nil {
		return nil
	}
	return &robotsFile{rules: chosen.rules, crawlDelay: chosen.delay}
}

// agentToken reduces a User-Agent header to the name a site operator would
// write in robots.txt: "anymd-crawler/1.2 (+url)" -> "anymd-crawler".
func agentToken(ua string) string {
	ua = strings.TrimSpace(strings.ToLower(ua))
	if i := strings.IndexAny(ua, "/ \t"); i >= 0 {
		ua = ua[:i]
	}
	return ua
}

// allows reports whether the rules permit fetching a path (plus its query,
// because robots.txt patterns may include one).
//
// Longest matching pattern wins; on an exact tie Allow wins, which is what
// makes a narrow Allow able to carve a hole in a broad Disallow.
func (r *robotsFile) allows(escapedPath, rawQuery string) bool {
	if r == nil || len(r.rules) == 0 {
		return true
	}
	target := escapedPath
	if target == "" {
		target = "/"
	}
	if rawQuery != "" {
		target += "?" + rawQuery
	}

	best := -1
	allow := true
	for _, rule := range r.rules {
		if !robotsMatch(rule.pattern, target) {
			continue
		}
		n := len(strings.TrimSuffix(rule.pattern, "$"))
		if n > best || (n == best && rule.allow) {
			best = n
			allow = rule.allow
		}
	}
	return allow
}

// robotsMatch implements robots.txt path matching: a plain prefix match, with
// "*" standing for any run of characters and a trailing "$" anchoring the end.
func robotsMatch(pattern, target string) bool {
	anchored := strings.HasSuffix(pattern, "$")
	if anchored {
		pattern = strings.TrimSuffix(pattern, "$")
	}
	parts := strings.Split(pattern, "*")

	pos := 0
	for i, part := range parts {
		if part == "" {
			if i == len(parts)-1 && anchored {
				return true // pattern ends in "*$": anything left over is fine
			}
			continue
		}
		if i == 0 {
			if !strings.HasPrefix(target[pos:], part) {
				return false
			}
			pos += len(part)
			continue
		}
		if i == len(parts)-1 && anchored {
			if !strings.HasSuffix(target[pos:], part) {
				return false
			}
			pos = len(target)
			continue
		}
		j := strings.Index(target[pos:], part)
		if j < 0 {
			return false
		}
		pos += j + len(part)
	}
	if anchored && len(parts) == 1 {
		return pos == len(target)
	}
	return true
}
