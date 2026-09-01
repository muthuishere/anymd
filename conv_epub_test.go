package anymd

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

// epubPart is one entry in a test archive.
type epubPart struct {
	name  string
	body  string
	store bool // stored uncompressed, as the spec requires for "mimetype"
}

// buildEpub writes a real zip in memory, so the fixture is a genuine archive
// rather than a committed binary.
func buildEpub(t *testing.T, parts ...epubPart) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range parts {
		method := zip.Deflate
		if p.store {
			method = zip.Store
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: p.name, Method: method})
		if err != nil {
			t.Fatalf("zip %s: %v", p.name, err)
		}
		if _, err := w.Write([]byte(p.body)); err != nil {
			t.Fatalf("write %s: %v", p.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// nestedEpub is the classic layout: the OPF lives in OEBPS/, so every manifest
// href is OEBPS-relative, and the spine is deliberately out of manifest order.
func nestedEpub(t *testing.T) []byte {
	t.Helper()
	return buildEpub(t,
		epubPart{name: "mimetype", body: "application/epub+zip", store: true},
		epubPart{name: "META-INF/container.xml", body: `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`},
		epubPart{name: "OEBPS/content.opf", body: `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="bid">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>The Small Book</dc:title>
    <dc:creator>A. Writer</dc:creator>
    <dc:language>en</dc:language>
    <dc:date>2024-03-04</dc:date>
  </metadata>
  <manifest>
    <item id="c1" href="text/ch1.xhtml" media-type="application/xhtml+xml"/>
    <item id="c2" href="text/ch2.xhtml" media-type="application/xhtml+xml"/>
    <item id="css" href="styles/main.css" media-type="text/css"/>
    <item id="ncx" href="toc.ncx" media-type="application/x-dtbncx+xml"/>
  </manifest>
  <spine toc="ncx">
    <itemref idref="c2"/>
    <itemref idref="c1"/>
    <itemref idref="ncx"/>
    <itemref idref="missing"/>
  </spine>
</package>`},
		epubPart{name: "OEBPS/text/ch1.xhtml", body: `<?xml version="1.0" encoding="utf-8"?>
<html xmlns="http://www.w3.org/1999/xhtml"><head><title>One</title>
<link rel="stylesheet" href="../styles/main.css"/><script>bad()</script></head>
<body><h1>Chapter One</h1><p>First para with a <a href="ch2.xhtml">link</a>.</p></body></html>`},
		epubPart{name: "OEBPS/text/ch2.xhtml", body: `<?xml version="1.0" encoding="utf-8"?>
<html xmlns="http://www.w3.org/1999/xhtml"><head><title>Two</title></head>
<body><h1>Chapter Two</h1><ul><li>alpha</li><li>beta</li></ul></body></html>`},
		epubPart{name: "OEBPS/styles/main.css", body: "body{color:red}"},
		epubPart{name: "OEBPS/toc.ncx", body: `<?xml version="1.0"?><ncx xmlns="http://www.daisy.org/z3986/2005/ncx/"/>`},
	)
}

func TestEpubConverterAccepts(t *testing.T) {
	c := &EPUBConverter{}
	book := nestedEpub(t)

	if !c.Accepts(bytes.NewReader(book), StreamInfo{Extension: ".epub"}, nil) {
		t.Error("extension hint should be accepted")
	}
	if !c.Accepts(bytes.NewReader(book), StreamInfo{MimeType: "application/epub+zip"}, nil) {
		t.Error("mime hint should be accepted")
	}
	// No hints at all: the self-identifying "mimetype" entry must carry it.
	if !c.Accepts(bytes.NewReader(book), StreamInfo{}, nil) {
		t.Error("PK magic + mimetype entry should be accepted with no hints")
	}
	// A plain zip that is not an epub must be declined.
	plain := buildEpub(t, epubPart{name: "readme.txt", body: "hello"})
	if c.Accepts(bytes.NewReader(plain), StreamInfo{}, nil) {
		t.Error("plain zip must not be accepted as epub")
	}
	if c.Accepts(strings.NewReader("<html></html>"), StreamInfo{}, nil) {
		t.Error("html must not be accepted as epub")
	}
	if c.Accepts(strings.NewReader(""), StreamInfo{}, nil) {
		t.Error("empty must not be accepted as epub")
	}
}

// TestEpubSpineOrderAndRelativeResolution is the whole point of the converter:
// parts come out in SPINE order (not manifest order), and their hrefs resolve
// relative to the OPF's directory (OEBPS/), not the archive root.
func TestEpubSpineOrderAndRelativeResolution(t *testing.T) {
	res, err := New().ConvertBytes(nestedEpub(t), StreamInfo{Extension: ".epub"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "The Small Book" {
		t.Errorf("Title = %q", res.Title)
	}
	want := "# The Small Book\n" +
		"\n" +
		"- **Author:** A. Writer\n" +
		"- **Language:** en\n" +
		"- **Date:** 2024-03-04\n" +
		"\n" +
		"# Chapter Two\n" +
		"\n" +
		"- alpha\n" +
		"- beta\n" +
		"\n" +
		"# Chapter One\n" +
		"\n" +
		"First para with a [link](ch2.xhtml).\n"
	if res.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
	if strings.Contains(res.Markdown, "color:red") || strings.Contains(res.Markdown, "bad()") {
		t.Error("stylesheet/script content leaked into the book")
	}
}

// TestEpubOPFAtArchiveRoot: the other layout, where hrefs are root-relative.
func TestEpubOPFAtArchiveRoot(t *testing.T) {
	book := buildEpub(t,
		epubPart{name: "mimetype", body: "application/epub+zip", store: true},
		epubPart{name: "META-INF/container.xml", body: `<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
<rootfiles><rootfile full-path="book.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`},
		epubPart{name: "book.opf", body: `<package xmlns="http://www.idpf.org/2007/opf">
<metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>Flat</dc:title></metadata>
<manifest><item id="a" href="a.xhtml" media-type="application/xhtml+xml"/></manifest>
<spine><itemref idref="a"/></spine></package>`},
		epubPart{name: "a.xhtml", body: `<html><body><p>Flat body.</p></body></html>`},
	)
	res, err := New().ConvertBytes(book, StreamInfo{Extension: ".epub"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "# Flat\n\nFlat body.\n"; res.Markdown != want {
		t.Errorf("markdown = %q, want %q", res.Markdown, want)
	}
}

// TestEpubPercentEncodedHref: manifest hrefs are URI references.
func TestEpubPercentEncodedHref(t *testing.T) {
	book := buildEpub(t,
		epubPart{name: "mimetype", body: "application/epub+zip", store: true},
		epubPart{name: "META-INF/container.xml", body: `<container><rootfiles><rootfile full-path="OPS/p.opf"/></rootfiles></container>`},
		epubPart{name: "OPS/p.opf", body: `<package><metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>Spaced</dc:title></metadata>
<manifest><item id="a" href="my%20chapter.xhtml" media-type="application/xhtml+xml"/></manifest>
<spine><itemref idref="a"/></spine></package>`},
		epubPart{name: "OPS/my chapter.xhtml", body: `<html><body><p>Body.</p></body></html>`},
	)
	res, err := New().ConvertBytes(book, StreamInfo{Extension: ".epub"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "# Spaced\n\nBody.\n"; res.Markdown != want {
		t.Errorf("markdown = %q, want %q", res.Markdown, want)
	}
}

// TestEpubLatin1Part: a part declaring its own legacy encoding must transcode.
func TestEpubLatin1Part(t *testing.T) {
	book := buildEpub(t,
		epubPart{name: "mimetype", body: "application/epub+zip", store: true},
		epubPart{name: "META-INF/container.xml", body: `<container><rootfiles><rootfile full-path="c.opf"/></rootfiles></container>`},
		epubPart{name: "c.opf", body: `<package><metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>Legacy</dc:title></metadata>
<manifest><item id="a" href="a.xhtml" media-type="application/xhtml+xml"/></manifest>
<spine><itemref idref="a"/></spine></package>`},
		epubPart{name: "a.xhtml", body: "<html><head><meta charset=\"ISO-8859-1\"></head><body><p>Caf\xe9 cr\xe8me</p></body></html>"},
	)
	res, err := New().ConvertBytes(book, StreamInfo{Extension: ".epub"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "# Legacy\n\nCafé crème\n"; res.Markdown != want {
		t.Errorf("markdown = %q, want %q", res.Markdown, want)
	}
}

// TestEpubPathTraversalIsIgnored: an href that climbs out of the archive must
// resolve to nothing, not to a file on disk or another entry.
func TestEpubPathTraversalIsIgnored(t *testing.T) {
	book := buildEpub(t,
		epubPart{name: "mimetype", body: "application/epub+zip", store: true},
		epubPart{name: "META-INF/container.xml", body: `<container><rootfiles><rootfile full-path="OEBPS/c.opf"/></rootfiles></container>`},
		epubPart{name: "secret.xhtml", body: `<html><body><p>SECRET</p></body></html>`},
		epubPart{name: "OEBPS/c.opf", body: `<package><metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>Evil</dc:title></metadata>
<manifest>
 <item id="esc" href="../../../../etc/passwd" media-type="application/xhtml+xml"/>
 <item id="abs" href="/etc/hosts" media-type="application/xhtml+xml"/>
 <item id="ok" href="ok.xhtml" media-type="application/xhtml+xml"/>
</manifest>
<spine><itemref idref="esc"/><itemref idref="abs"/><itemref idref="ok"/></spine></package>`},
		epubPart{name: "OEBPS/ok.xhtml", body: `<html><body><p>Fine.</p></body></html>`},
	)
	res, err := New().ConvertBytes(book, StreamInfo{Extension: ".epub"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "# Evil\n\nFine.\n"; res.Markdown != want {
		t.Errorf("markdown = %q, want %q", res.Markdown, want)
	}
	if strings.Contains(res.Markdown, "SECRET") || strings.Contains(res.Markdown, "root:") {
		t.Fatal("path traversal escaped the archive")
	}
}

// TestEpubMalformedIsErrorNotPanic: every one of these is a plausible thing to
// find on the open internet, and none of them may crash.
func TestEpubMalformedIsErrorNotPanic(t *testing.T) {
	cases := map[string][]byte{
		"truncated zip": []byte("PK\x03\x04mimetypeapplication/epub+zip\x00\x00\x00"),
		"no container": buildEpub(t,
			epubPart{name: "mimetype", body: "application/epub+zip", store: true},
			epubPart{name: "junk.txt", body: "x"}),
		"container is not xml": buildEpub(t,
			epubPart{name: "mimetype", body: "application/epub+zip", store: true},
			epubPart{name: "META-INF/container.xml", body: "<<<<not xml"}),
		"container names a missing opf": buildEpub(t,
			epubPart{name: "mimetype", body: "application/epub+zip", store: true},
			epubPart{name: "META-INF/container.xml", body: `<container><rootfiles><rootfile full-path="nope.opf"/></rootfiles></container>`}),
		"opf is not xml": buildEpub(t,
			epubPart{name: "mimetype", body: "application/epub+zip", store: true},
			epubPart{name: "META-INF/container.xml", body: `<container><rootfiles><rootfile full-path="c.opf"/></rootfiles></container>`},
			epubPart{name: "c.opf", body: "<package><manifest>"}),
		"empty spine": buildEpub(t,
			epubPart{name: "mimetype", body: "application/epub+zip", store: true},
			epubPart{name: "META-INF/container.xml", body: `<container><rootfiles><rootfile full-path="c.opf"/></rootfiles></container>`},
			epubPart{name: "c.opf", body: `<package><metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>Empty</dc:title></metadata><manifest/><spine/></package>`}),
	}
	c := &EPUBConverter{}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic: %v", r)
				}
			}()
			res, err := c.Convert(bytes.NewReader(body), StreamInfo{Extension: ".epub"}, nil)
			if err != nil {
				t.Logf("error (acceptable): %v", err)
				return
			}
			t.Logf("ok: %q", res.Markdown)
		})
	}
}
