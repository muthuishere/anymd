package anymd

import (
	"strings"
	"testing"
)

// site is the mapping a three-page crawl of https://x.dev would produce.
var site = map[string]string{
	"https://x.dev/":            "index.md",
	"https://x.dev/docs/a":      "docs/a.md",
	"https://x.dev/docs/b":      "docs/b.md",
	"https://x.dev/docs/deep/c": "docs/deep/c.md",
	"https://x.dev/logo.png":    "logo.png",
}

func TestRewriteLinksInline(t *testing.T) {
	cases := []struct {
		name string
		md   string
		from string
		want string
	}{
		{
			"sibling in the same directory",
			"see [b](https://x.dev/docs/b)",
			"https://x.dev/docs/a",
			"see [b](b.md)",
		},
		{
			"up to the root",
			"[home](https://x.dev/)",
			"https://x.dev/docs/a",
			"[home](../index.md)",
		},
		{
			"down from the root",
			"[a](https://x.dev/docs/a)",
			"https://x.dev/",
			"[a](docs/a.md)",
		},
		{
			"across directory levels",
			"[a](https://x.dev/docs/a) and [home](https://x.dev/)",
			"https://x.dev/docs/deep/c",
			"[a](../a.md) and [home](../../index.md)",
		},
		{
			"relative target resolved against from",
			"[b](b) and [up](../)",
			"https://x.dev/docs/a",
			"[b](b.md) and [up](../index.md)",
		},
		{
			"root-relative target",
			"[b](/docs/b)",
			"https://x.dev/docs/a",
			"[b](b.md)",
		},
		{
			"image",
			"![logo](https://x.dev/logo.png)",
			"https://x.dev/docs/a",
			"![logo](../logo.png)",
		},
		{
			"image nested in a link",
			"[![logo](/logo.png)](/docs/b)",
			"https://x.dev/docs/a",
			"[![logo](../logo.png)](b.md)",
		},
		{
			"title preserved",
			`[b](https://x.dev/docs/b "The B Page")`,
			"https://x.dev/docs/a",
			`[b](b.md "The B Page")`,
		},
		{
			"single-quoted title with a parenthesis in it",
			"[b](/docs/b 'B (draft)')",
			"https://x.dev/docs/a",
			"[b](b.md 'B (draft)')",
		},
		{
			"angle brackets preserved",
			"[b](<https://x.dev/docs/b>)",
			"https://x.dev/docs/a",
			"[b](<b.md>)",
		},
		{
			"fragment carried onto the local path",
			"[b](https://x.dev/docs/b#intro)",
			"https://x.dev/docs/a",
			"[b](b.md#intro)",
		},
		{
			"trailing slash still matches the mapping",
			"[b](https://x.dev/docs/b/)",
			"https://x.dev/docs/a",
			"[b](b.md)",
		},
		{
			"query string is part of the identity, so an unmapped one is left alone",
			"[b](https://x.dev/docs/b?v=2)",
			"https://x.dev/docs/a",
			"[b](https://x.dev/docs/b?v=2)",
		},
		{
			"reference definition",
			"text [b]\n\n[b]: https://x.dev/docs/b\n",
			"https://x.dev/docs/a",
			"text [b]\n\n[b]: b.md\n",
		},
		{
			"reference definition with a title and angle brackets",
			`[b]: <https://x.dev/docs/b> "B"`,
			"https://x.dev/docs/a",
			`[b]: <b.md> "B"`,
		},
		{
			"self link",
			"[me](https://x.dev/docs/a)",
			"https://x.dev/docs/a",
			"[me](a.md)",
		},
		{
			"document written at the root when from is unmapped",
			"[b](https://x.dev/docs/b)",
			"https://x.dev/not-crawled",
			"[b](docs/b.md)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RewriteLinks(tc.md, tc.from, site); got != tc.want {
				t.Errorf("\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// TestRewriteLinksLeavesUnmappedAlone is the rule that makes a partial mirror
// usable: a page we never fetched must still be reachable over the internet.
func TestRewriteLinksLeavesUnmappedAlone(t *testing.T) {
	unchanged := []string{
		"[other](https://other.dev/page)",
		"[missed](https://x.dev/docs/never-crawled)",
		`[t](https://x.dev/nope "Title")`,
		"[t](<https://x.dev/nope>)",
		"[mail](mailto:a@b.dev)",
		"[call](tel:+15551234)",
		"[js](javascript:void(0))",
		"[d](data:text/plain;base64,QQ==)",
		"[frag](#section)",
		"[empty]()",
		"[ref][elsewhere]",
		"[nope]: https://x.dev/uncrawled",
		"plain https://x.dev/docs/b autolink-ish text",
	}
	for _, md := range unchanged {
		if got := RewriteLinks(md, "https://x.dev/docs/a", site); got != md {
			t.Errorf("RewriteLinks(%q) = %q, want it untouched", md, got)
		}
	}
}

// TestRewriteLinksSkipsCode is the subtle one. A URL inside a code sample is
// content: rewriting it corrupts the example a reader is about to copy.
func TestRewriteLinksSkipsCode(t *testing.T) {
	cases := []struct {
		name string
		md   string
		want string // "" means unchanged
	}{
		{"fenced", "```\n[b](https://x.dev/docs/b)\n```\n", ""},
		{"fenced with an info string", "```md\n[b](https://x.dev/docs/b)\n```\n", ""},
		{"tilde fence", "~~~\n[b](https://x.dev/docs/b)\n~~~\n", ""},
		{"fence not closed by a shorter run", "````\n[b](/docs/b)\n```\n[b](/docs/b)\n````\n", ""},
		{"indented block", "para\n\n    [b](https://x.dev/docs/b)\n\ntail\n", ""},
		{"inline code span", "use `[b](https://x.dev/docs/b)` here", ""},
		{"double backtick span", "use ``[b](/docs/b)`` here", ""},
		{
			"prose around a fence is still rewritten",
			"[b](/docs/b)\n\n```\n[b](/docs/b)\n```\n\n[b](/docs/b)\n",
			"[b](b.md)\n\n```\n[b](/docs/b)\n```\n\n[b](b.md)\n",
		},
		{
			"code span next to a real link",
			"`/docs/b` then [b](/docs/b)",
			"`/docs/b` then [b](b.md)",
		},
		{
			"a four-space continuation line is not a code block",
			"para\n    [b](/docs/b)\n",
			"para\n    [b](b.md)\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.want
			if want == "" {
				want = tc.md
			}
			if got := RewriteLinks(tc.md, "https://x.dev/docs/a", site); got != want {
				t.Errorf("\n got: %q\nwant: %q", got, want)
			}
		})
	}
}

func TestRewriteLinksEscaping(t *testing.T) {
	m := map[string]string{
		"https://x.dev/a%20b":    "docs/a b.md",
		"https://x.dev/p%281%29": "p(1).md",
	}
	got := RewriteLinks("[s](https://x.dev/a%20b) [p](https://x.dev/p%281%29)", "https://x.dev/", m)
	// Every segment is percent-encoded, so a space or a parenthesis in a file
	// name can never terminate the link early.
	want := "[s](docs/a%20b.md) [p](p%281%29.md)"
	if got != want {
		t.Fatalf("\n got: %q\nwant: %q", got, want)
	}
}

// TestRewriteLinksMalformed asserts the "never panic" half of the contract.
// Everything here is nonsense; none of it may take down a crawl.
func TestRewriteLinksMalformed(t *testing.T) {
	inputs := []string{
		"",
		"[",
		"[]",
		"[](",
		"[]()",
		"[a](b",
		"[a](<b",
		"[a](<b>",
		"![",
		"![](",
		"[[[[[[[[",
		"]]]]]]]]",
		"`unclosed [b](https://x.dev/docs/b)",
		"```\nunclosed fence [b](/docs/b)\n",
		"[a](https://x.dev/docs/b \"unterminated",
		"[\\](\\)",
		"[a](%zz)",
		"[a](https://[::1]bad)",
		strings.Repeat("[a](", 200),
		"\x00[a](/docs/b)\x00",
	}
	for _, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("RewriteLinks(%q) panicked: %v", in, r)
				}
			}()
			RewriteLinks(in, "https://x.dev/docs/a", site)
		}()
	}
}

func TestRewriteLinksNoMapping(t *testing.T) {
	md := "[b](https://x.dev/docs/b)"
	if got := RewriteLinks(md, "https://x.dev/docs/a", nil); got != md {
		t.Fatalf("empty mapping changed the document: %q", got)
	}
}

func TestRelativeTo(t *testing.T) {
	cases := []struct{ from, to, want string }{
		{"docs/a.md", "docs/b.md", "b.md"},
		{"docs/a.md", "index.md", "../index.md"},
		{"index.md", "docs/b.md", "docs/b.md"},
		{"docs/deep/c.md", "docs/a.md", "../a.md"},
		{"docs/deep/c.md", "index.md", "../../index.md"},
		{"", "docs/b.md", "docs/b.md"},
		{"a/b/c/d.md", "a/b/x/y.md", "../x/y.md"},
		{"docs/a.md", "docs/a.md", "a.md"},
		{"docs/docs.md", "docs/docs.md", "docs.md"},
	}
	for _, tc := range cases {
		if got := relativeTo(tc.from, tc.to); got != tc.want {
			t.Errorf("relativeTo(%q, %q) = %q, want %q", tc.from, tc.to, got, tc.want)
		}
	}
}
