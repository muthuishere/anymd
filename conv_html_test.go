package anymd

import (
	"strings"
	"testing"
)

// convertHTML runs a document through a fresh engine, which is what a consumer
// gets from anymd.New().
func convertHTML(t *testing.T, src []byte, info StreamInfo) Result {
	t.Helper()
	res, err := New().ConvertBytes(src, info, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	return res
}

func TestHTMLConverterAccepts(t *testing.T) {
	c := &HTMLConverter{}
	cases := []struct {
		name string
		src  string
		info StreamInfo
		want bool
	}{
		{"by extension", "not really html", StreamInfo{Extension: ".html"}, true},
		{"by htm extension", "x", StreamInfo{Extension: ".HTM"}, true},
		{"by mime", "x", StreamInfo{MimeType: "text/html; charset=utf-8"}, true},
		{"by xhtml mime", "x", StreamInfo{MimeType: "application/xhtml+xml"}, true},
		{"by sniff doctype", "<!DOCTYPE html><p>hi", StreamInfo{}, true},
		{"by sniff html tag", "\n\n  <html><body>hi</body></html>", StreamInfo{}, true},
		{"plain text declined", "just some prose, with < and > in it", StreamInfo{}, false},
		{"bare xml declined", `<?xml version="1.0"?><rss version="2.0"></rss>`, StreamInfo{Extension: ".xml"}, false},
		{"empty declined", "", StreamInfo{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.Accepts(strings.NewReader(tc.src), tc.info, nil); got != tc.want {
				t.Errorf("Accepts = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHTMLConverterPriorityIsGeneric(t *testing.T) {
	// docx/xlsx/epub are zip-of-XML; HTML must never get first refusal.
	if got := (&HTMLConverter{}).Priority(); got != PriorityGeneric {
		t.Fatalf("Priority = %d, want PriorityGeneric (%d)", got, PriorityGeneric)
	}
}

// TestHTMLTableListsLinksAndEntities is the workhorse case: a GFM pipe table, a
// nested list, a link, an HTML entity, and the script/style/comment stripping.
func TestHTMLTableListsLinksAndEntities(t *testing.T) {
	src := `<!doctype html>
<html><head>
<title>Quarterly &amp; Co</title>
<style>b{color:red}</style>
<script>alert("boom")</script>
<meta name="description" content="ignored">
</head>
<body>
<!-- a comment that must not survive -->
<h1>Report</h1>
<p>Visit <a href="https://example.com/docs/spec.html">the spec</a> &amp; enjoy &mdash; it&rsquo;s good.</p>
<ul><li>alpha<ul><li>alpha one</li><li>alpha two</li></ul></li><li>beta</li></ul>
<table>
<thead><tr><th>Name</th><th>Qty</th></tr></thead>
<tbody><tr><td>Widget</td><td>3</td></tr><tr><td>Gadget|X</td><td>12</td></tr></tbody>
</table>
<noscript>enable javascript</noscript>
</body></html>`

	res := convertHTML(t, []byte(src), StreamInfo{Extension: ".html"})

	if res.Title != "Quarterly & Co" {
		t.Errorf("Title = %q, want %q", res.Title, "Quarterly & Co")
	}
	want := "# Report\n" +
		"\n" +
		"Visit [the spec](https://example.com/docs/spec.html) & enjoy — it’s good.\n" +
		"\n" +
		"- alpha\n" +
		"  \n" +
		"  - alpha one\n" +
		"  - alpha two\n" +
		"- beta\n" +
		"\n" +
		"| Name      | Qty |\n" +
		"|-----------|-----|\n" +
		"| Widget    | 3   |\n" +
		"| Gadget\\|X | 12  |\n"
	if res.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
	for _, forbidden := range []string{"alert", "color:red", "a comment that must not survive", "enable javascript"} {
		if strings.Contains(res.Markdown, forbidden) {
			t.Errorf("stripped content leaked into output: %q", forbidden)
		}
	}
}

// TestHTMLTitleFallsBackToH1 covers a page with no <title> element.
func TestHTMLTitleFallsBackToH1(t *testing.T) {
	res := convertHTML(t, []byte(`<html><body><h1>Only Heading</h1><p>Body.</p></body></html>`),
		StreamInfo{Extension: ".html"})
	if res.Title != "Only Heading" {
		t.Errorf("Title = %q, want %q", res.Title, "Only Heading")
	}
	if want := "# Only Heading\n\nBody.\n"; res.Markdown != want {
		t.Errorf("markdown = %q, want %q", res.Markdown, want)
	}
}

// TestHTMLLatin1MetaCharset proves the transcoding path: the bytes are
// ISO-8859-1 and only the <meta charset> says so. Getting this wrong is the
// mojibake complaint, so it is asserted byte-exactly.
func TestHTMLLatin1MetaCharset(t *testing.T) {
	// 0xE9 = é, 0xE8 = è, 0xE7 = ç in ISO-8859-1 (invalid UTF-8 on their own).
	raw := []byte("<html><head><meta charset=\"ISO-8859-1\"><title>Caf\xe9</title></head>" +
		"<body><p>Cr\xe8me br\xfbl\xe9e, fran\xe7ais.</p></body></html>")

	res := convertHTML(t, raw, StreamInfo{Extension: ".html"})
	if res.Title != "Café" {
		t.Errorf("Title = %q, want %q", res.Title, "Café")
	}
	if want := "Crème brûlée, français.\n"; res.Markdown != want {
		t.Errorf("markdown = %q, want %q", res.Markdown, want)
	}
}

// TestHTMLCharsetFromHTTPEquiv covers the older declaration form.
func TestHTMLCharsetFromHTTPEquiv(t *testing.T) {
	raw := []byte("<html><head><meta http-equiv=\"Content-Type\" content=\"text/html; charset=windows-1252\">" +
		"</head><body><p>Smart \x93quotes\x94 and an em\x97dash.</p></body></html>")
	res := convertHTML(t, raw, StreamInfo{Extension: ".html"})
	if want := "Smart “quotes” and an em—dash.\n"; res.Markdown != want {
		t.Errorf("markdown = %q, want %q", res.Markdown, want)
	}
}

// TestHTMLStreamInfoCharsetWins: a transport-declared charset beats the
// document's own (wrong) declaration, matching browser precedence.
func TestHTMLStreamInfoCharsetWins(t *testing.T) {
	raw := []byte("<html><head><meta charset=\"utf-8\"></head><body><p>caf\xe9</p></body></html>")
	res := convertHTML(t, raw, StreamInfo{Extension: ".html", Charset: "iso-8859-1"})
	if want := "café\n"; res.Markdown != want {
		t.Errorf("markdown = %q, want %q", res.Markdown, want)
	}
}

// TestHTMLRelativeURLResolution: links and images from a fetched page must stay
// usable, so relative references resolve against StreamInfo.URL.
func TestHTMLRelativeURLResolution(t *testing.T) {
	src := `<html><body>
<p><a href="spec.html">sibling</a> <a href="/root.html">root</a> <a href="../up.html">up</a></p>
<p><img src="img/logo.png" alt="Logo"></p>
<p><a href="https://other.example/x">absolute</a></p>
</body></html>`
	res := convertHTML(t, []byte(src), StreamInfo{
		Extension: ".html",
		URL:       "https://example.com/base/dir/page.html",
	})
	want := "[sibling](https://example.com/base/dir/spec.html) [root](https://example.com/root.html) [up](https://example.com/base/up.html)\n" +
		"\n" +
		"![Logo](https://example.com/base/dir/img/logo.png)\n" +
		"\n" +
		"[absolute](https://other.example/x)\n"
	if res.Markdown != want {
		t.Errorf("markdown = %q, want %q", res.Markdown, want)
	}
}

// TestHTMLNoBaseURLKeepsRelative: with no URL hint we must not invent a host.
func TestHTMLNoBaseURLKeepsRelative(t *testing.T) {
	res := convertHTML(t, []byte(`<html><body><p><a href="spec.html">s</a></p></body></html>`),
		StreamInfo{Extension: ".html"})
	if want := "[s](spec.html)\n"; res.Markdown != want {
		t.Errorf("markdown = %q, want %q", res.Markdown, want)
	}
}

// TestHTMLToMarkdownHelper pins the exported helper other converters depend on.
func TestHTMLToMarkdownHelper(t *testing.T) {
	md, err := HTMLToMarkdown(`<p>Hi <b>there</b> <a href="/a">a</a>.</p><script>x</script>`, "https://h.example/p/")
	if err != nil {
		t.Fatal(err)
	}
	if want := "Hi **there** [a](https://h.example/a)."; md != want {
		t.Errorf("HTMLToMarkdown = %q, want %q", md, want)
	}

	md, err = HTMLToMarkdown("<p>plain</p>", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := "plain"; md != want {
		t.Errorf("HTMLToMarkdown = %q, want %q", md, want)
	}
}

func TestHTMLTitleHelper(t *testing.T) {
	if got := HTMLTitle("<html><head><title> Spaced   Out </title></head></html>"); got != "Spaced Out" {
		t.Errorf("HTMLTitle = %q", got)
	}
	if got := HTMLTitle("<html><body><p>no title</p></body></html>"); got != "" {
		t.Errorf("HTMLTitle = %q, want empty", got)
	}
}

// TestHTMLMalformedNeverPanics: the parser is pointed at hostile bytes, so the
// only acceptable outcomes are markdown or an error.
func TestHTMLMalformedNeverPanics(t *testing.T) {
	inputs := []string{
		"<html><body><p>unclosed",
		"<table><tr><td>a</table></tr></td>",
		"<div>" + strings.Repeat("<div>", 500) + "deep",
		"<a href=\"htt p://%%%\">bad url</a>",
		"<html><body>\x00\xff\xfe binary-ish</body></html>",
		"<!--",
		"<html><head><title>",
	}
	for _, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on %q: %v", in, r)
				}
			}()
			if _, err := New().ConvertBytes([]byte(in), StreamInfo{Extension: ".html"}, nil); err != nil {
				t.Logf("error (acceptable) on %q: %v", in, err)
			}
		}()
	}
}

func TestDecodeHTMLBytesFallbacks(t *testing.T) {
	// Valid UTF-8 with no declaration passes through untouched.
	if got := DecodeHTMLBytes([]byte("héllo — ok"), ""); got != "héllo — ok" {
		t.Errorf("utf8 passthrough = %q", got)
	}
	// A BOM is authoritative.
	if got := DecodeHTMLBytes([]byte("\xef\xbb\xbfbom"), ""); got != "bom" {
		t.Errorf("bom = %q", got)
	}
	// An unknown label must not be fatal; we fall through to detection.
	if got := DecodeHTMLBytes([]byte("plain"), "not-a-real-charset"); got != "plain" {
		t.Errorf("unknown label = %q", got)
	}
}
