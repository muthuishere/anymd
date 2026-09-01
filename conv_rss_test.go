package anymd

import (
	"strings"
	"testing"
)

const rssFixture = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel>
<title>Deemwar Notes</title>
<link>https://example.com/</link>
<description>Field notes on shipping software.</description>
<item>
  <title>First &amp; foremost</title>
  <link>https://example.com/posts/first.html</link>
  <pubDate>Mon, 02 Jan 2006 15:04:05 +0000</pubDate>
  <author>jane@example.com (Jane Doe)</author>
  <description>&lt;p&gt;Hello &lt;b&gt;world&lt;/b&gt; — see &lt;a href="/docs/a.html"&gt;docs&lt;/a&gt;.&lt;/p&gt;</description>
</item>
<item>
  <title>Second</title>
  <link>https://example.com/posts/second.html</link>
  <description>&lt;ul&gt;&lt;li&gt;one&lt;/li&gt;&lt;li&gt;two&lt;/li&gt;&lt;/ul&gt;</description>
</item>
</channel></rss>`

const atomFixture = `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Atom Journal</title>
  <subtitle>Short entries.</subtitle>
  <link href="https://atom.example.org/"/>
  <updated>2020-05-06T07:08:09Z</updated>
  <entry>
    <title>Alpha</title>
    <link href="https://atom.example.org/alpha"/>
    <published>2020-05-06T07:08:09Z</published>
    <author><name>Ada</name></author>
    <content type="html">&lt;p&gt;Alpha &lt;code&gt;body&lt;/code&gt;.&lt;/p&gt;</content>
  </entry>
  <entry>
    <title>Beta</title>
    <link href="https://atom.example.org/beta"/>
    <published>2021-06-07T08:09:10Z</published>
    <content type="html">&lt;p&gt;Beta body.&lt;/p&gt;</content>
  </entry>
</feed>`

func TestRSSConverterAccepts(t *testing.T) {
	c := &RSSConverter{}
	cases := []struct {
		name string
		src  string
		info StreamInfo
		want bool
	}{
		{"rss ext + rss root", rssFixture, StreamInfo{Extension: ".rss"}, true},
		{"xml ext + rss root", rssFixture, StreamInfo{Extension: ".xml"}, true},
		{"atom ext + feed root", atomFixture, StreamInfo{Extension: ".atom"}, true},
		{"rss mime", rssFixture, StreamInfo{MimeType: "application/rss+xml"}, true},
		{"atom mime", atomFixture, StreamInfo{MimeType: "application/atom+xml; charset=utf-8"}, true},
		{"rdf root", `<?xml version="1.0"?><rdf:RDF xmlns:rdf="x"><channel/></rdf:RDF>`, StreamInfo{Extension: ".xml"}, true},

		// The rule that matters: a plain .xml that is not a feed must be left
		// for whoever really owns it, not claimed and then hard-failed.
		{"plain xml rejected", `<?xml version="1.0"?><config><db host="x"/></config>`, StreamInfo{Extension: ".xml"}, false},
		{"xml mime, not a feed", `<?xml version="1.0"?><note><to>Tove</to></note>`, StreamInfo{MimeType: "text/xml"}, false},
		{"svg rejected", `<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`, StreamInfo{Extension: ".xml"}, false},
		{"feed body but no hint", rssFixture, StreamInfo{}, false},
		{"html not claimed", `<html><body><p>hi</p></body></html>`, StreamInfo{Extension: ".html"}, false},
		{"empty", "", StreamInfo{Extension: ".xml"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.Accepts(strings.NewReader(tc.src), tc.info, nil); got != tc.want {
				t.Errorf("Accepts = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPlainXMLIsNotRoutedToRSS is the end-to-end half of the rule above: a
// non-feed .xml must not come out of the engine as a feed rendering.
func TestPlainXMLIsNotRoutedToRSS(t *testing.T) {
	src := []byte(`<?xml version="1.0"?><config><db host="x"/></config>`)
	res, err := New().ConvertBytes(src, StreamInfo{Extension: ".xml"}, nil)
	if err != nil {
		// No converter claimed it, or a sibling converter failed. Either way
		// the rss converter did not invent a feed out of it.
		return
	}
	if strings.HasPrefix(res.Markdown, "# ") && strings.Contains(res.Markdown, "## ") {
		t.Fatalf("plain xml rendered as a feed: %q", res.Markdown)
	}
}

func TestRSS20Feed(t *testing.T) {
	res, err := New().ConvertBytes([]byte(rssFixture), StreamInfo{Extension: ".rss"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "Deemwar Notes" {
		t.Errorf("Title = %q", res.Title)
	}
	want := "# Deemwar Notes\n" +
		"\n" +
		"Field notes on shipping software.\n" +
		"\n" +
		"## First & foremost\n" +
		"\n" +
		"*2006-01-02T15:04:05Z — by Jane Doe*\n" +
		"\n" +
		"[First & foremost](https://example.com/posts/first.html)\n" +
		"\n" +
		"Hello **world** — see [docs](https://example.com/docs/a.html).\n" +
		"\n" +
		"## Second\n" +
		"\n" +
		"[Second](https://example.com/posts/second.html)\n" +
		"\n" +
		"- one\n" +
		"- two\n"
	if res.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
}

func TestAtomFeed(t *testing.T) {
	res, err := New().ConvertBytes([]byte(atomFixture), StreamInfo{Extension: ".atom"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "Atom Journal" {
		t.Errorf("Title = %q", res.Title)
	}
	want := "# Atom Journal\n" +
		"\n" +
		"Short entries.\n" +
		"\n" +
		"## Alpha\n" +
		"\n" +
		"*2020-05-06T07:08:09Z — by Ada*\n" +
		"\n" +
		"[Alpha](https://atom.example.org/alpha)\n" +
		"\n" +
		"Alpha `body`.\n" +
		"\n" +
		"## Beta\n" +
		"\n" +
		"*2021-06-07T08:09:10Z*\n" +
		"\n" +
		"[Beta](https://atom.example.org/beta)\n" +
		"\n" +
		"Beta body.\n"
	if res.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
}

// TestRSSPreservesItemOrder: gofeed can sort items by date; we must not. The
// fixture is deliberately newest-last so a sort would be visible.
func TestRSSPreservesItemOrder(t *testing.T) {
	src := `<rss version="2.0"><channel><title>T</title>
<item><title>Third</title><pubDate>Wed, 03 Jan 2007 00:00:00 +0000</pubDate></item>
<item><title>First</title><pubDate>Mon, 01 Jan 2001 00:00:00 +0000</pubDate></item>
<item><title>Second</title><pubDate>Tue, 02 Jan 2004 00:00:00 +0000</pubDate></item>
</channel></rss>`
	res, err := New().ConvertBytes([]byte(src), StreamInfo{Extension: ".rss"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "# T\n" +
		"\n## Third\n\n*2007-01-03T00:00:00Z*\n" +
		"\n## First\n\n*2001-01-01T00:00:00Z*\n" +
		"\n## Second\n\n*2004-01-02T00:00:00Z*\n"
	if res.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
}

// TestRSSContentEncodedWins: content:encoded is the full body, description is
// the teaser; the full body must win.
func TestRSSContentEncodedWins(t *testing.T) {
	src := `<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/"><channel>
<title>T</title>
<item><title>Post</title>
<description>teaser only</description>
<content:encoded><![CDATA[<p>The <em>full</em> body.</p>]]></content:encoded>
</item></channel></rss>`
	res, err := New().ConvertBytes([]byte(src), StreamInfo{Extension: ".rss"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "# T\n\n## Post\n\nThe *full* body.\n"
	if res.Markdown != want {
		t.Errorf("markdown = %q, want %q", res.Markdown, want)
	}
}

// TestRSSNoNetworkForImages: an <img> in a feed body renders as a link, and is
// never fetched.
func TestRSSNoNetworkForImages(t *testing.T) {
	src := `<rss version="2.0"><channel><title>T</title><link>http://127.0.0.1:1/</link>
<item><title>P</title><description>&lt;img src="pic.png" alt="Pic"&gt;</description></item>
</channel></rss>`
	res, err := New().ConvertBytes([]byte(src), StreamInfo{Extension: ".rss"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "# T\n\n## P\n\n![Pic](http://127.0.0.1:1/pic.png)\n"
	if res.Markdown != want {
		t.Errorf("markdown = %q, want %q", res.Markdown, want)
	}
}

// TestRSSMalformedIsErrorNotPanic.
func TestRSSMalformedIsErrorNotPanic(t *testing.T) {
	inputs := []string{
		`<rss version="2.0"><channel><title>unterminated`,
		`<feed xmlns="http://www.w3.org/2005/Atom"><entry><title>x`,
		"<rss>" + strings.Repeat("<a>", 2000),
		`<rss version="2.0"><channel><item><description>&lt;table&gt;&lt;tr&gt;</description></item></channel></rss>`,
	}
	c := &RSSConverter{}
	for _, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on %q: %v", in, r)
				}
			}()
			if _, err := c.Convert(strings.NewReader(in), StreamInfo{Extension: ".rss"}, nil); err != nil {
				t.Logf("error (acceptable): %v", err)
			}
		}()
	}
}
