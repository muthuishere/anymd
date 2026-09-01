package anymd

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// tzzMember is one entry to write into a test archive.
type tzzMember struct {
	name  string
	body  string
	store bool // Store instead of Deflate (an epub's mimetype must be stored)
	dir   bool
}

// tzzMakeZip builds a real zip in memory, per the contract's "fixtures in Go"
// rule. Entry order in the file is the order given here.
func tzzMakeZip(t *testing.T, members ...tzzMember) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, m := range members {
		name := m.name
		if m.dir && !strings.HasSuffix(name, "/") {
			name += "/"
		}
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		if m.store {
			h.Method = zip.Store
		}
		if m.dir {
			h.SetMode(0o755 | 0x80000000) // ModeDir
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatalf("CreateHeader(%q): %v", name, err)
		}
		if _, err := w.Write([]byte(m.body)); err != nil {
			t.Fatalf("Write(%q): %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip Close: %v", err)
	}
	return buf.Bytes()
}

// tzzZipEngine is a minimal engine: the zip container plus the text fallback.
// Built explicitly rather than with New() so a sibling converter landing in the
// registry mid-review cannot change these expectations.
func tzzZipEngine() *Engine {
	e := &Engine{}
	e.Register(&ZipConverter{})
	e.Register(&PlainTextConverter{})
	return e
}

func tzzConvertZip(t *testing.T, data []byte, opts *Options) string {
	t.Helper()
	res, err := tzzZipEngine().ConvertBytes(data, StreamInfoForFile("bundle.zip"), opts)
	if err != nil {
		t.Fatalf("ConvertBytes: %v", err)
	}
	return res.Markdown
}

// ---------------------------------------------------------------------------
// Accepts
// ---------------------------------------------------------------------------

// TestZipAcceptsDoesNotShadowKnownFormats is the whole reason this converter
// sits at PriorityGeneric. docx, pptx, xlsx and epub are all zip files; if the
// container claimed them, `anymd report.docx` would emit a directory listing of
// XML parts instead of the document.
func TestZipAcceptsDoesNotShadowKnownFormats(t *testing.T) {
	tests := []struct {
		name    string
		members []tzzMember
		info    StreamInfo
		want    bool
	}{
		{
			name:    "a plain archive is claimed",
			members: []tzzMember{{name: "a.txt", body: "hi"}},
			want:    true,
		},
		{
			name:    "an empty archive is claimed",
			members: nil,
			want:    true,
		},
		{
			name: "docx is declined",
			members: []tzzMember{
				{name: "[Content_Types].xml", body: "<Types/>"},
				{name: "word/document.xml", body: "<w:document/>"},
			},
			want: false,
		},
		{
			name: "pptx is declined",
			members: []tzzMember{
				{name: "[Content_Types].xml", body: "<Types/>"},
				{name: "ppt/presentation.xml", body: "<p:presentation/>"},
			},
			want: false,
		},
		{
			name: "xlsx is declined",
			members: []tzzMember{
				{name: "[Content_Types].xml", body: "<Types/>"},
				{name: "xl/workbook.xml", body: "<workbook/>"},
			},
			want: false,
		},
		{
			name: "epub is declined on its mimetype entry",
			members: []tzzMember{
				{name: "mimetype", body: "application/epub+zip", store: true},
				{name: "OEBPS/content.opf", body: "<package/>"},
			},
			want: false,
		},
		{
			name: "a mimetype entry that is not epub does not decline",
			members: []tzzMember{
				{name: "mimetype", body: "application/vnd.oasis.opendocument.text", store: true},
				{name: "content.xml", body: "<office/>"},
			},
			want: true,
		},
		{
			name:    "a marker name deeper in the tree is not a marker",
			members: []tzzMember{{name: "backup/word/document.xml", body: "<w:document/>"}},
			want:    true,
		},
		{
			name:    "a marker-like name is not a marker",
			members: []tzzMember{{name: "word/document.xml.bak", body: "x"}},
			want:    true,
		},
	}

	c := &ZipConverter{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := tzzMakeZip(t, tt.members...)
			if got := c.Accepts(bytes.NewReader(data), tt.info, nil); got != tt.want {
				t.Errorf("Accepts() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestZipAcceptsNonZip(t *testing.T) {
	tests := []struct {
		name string
		body string
		info StreamInfo
		want bool
	}{
		{name: "plain text", body: "just words\n", want: false},
		{name: "empty input", body: "", want: false},
		{name: "too short to hold a signature", body: "PK", want: false},
		{name: "PK but the wrong signature", body: "PK\x07\x08rest of it", want: false},
		{name: "pdf", body: "%PDF-1.7\n...", want: false},
		{
			name: "truncated zip with a .zip hint is claimed, for a real error",
			body: "PK\x03\x04truncated right here",
			info: StreamInfo{Extension: ".zip"},
			want: true,
		},
		{
			name: "truncated zip with a zip mime hint is claimed",
			body: "PK\x03\x04truncated right here",
			info: StreamInfo{MimeType: "application/zip"},
			want: true,
		},
		{
			name: "truncated zip with no hint is left alone",
			body: "PK\x03\x04truncated right here",
			want: false,
		},
	}

	c := &ZipConverter{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.Accepts(strings.NewReader(tt.body), tt.info, nil); got != tt.want {
				t.Errorf("Accepts() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestZipConverterRegistration(t *testing.T) {
	c := &ZipConverter{}
	if got := c.Name(); got != "zip" {
		t.Errorf("Name() = %q, want %q", got, "zip")
	}
	if got := c.Priority(); got != PriorityGeneric {
		t.Errorf("Priority() = %d, want PriorityGeneric (%d)", got, PriorityGeneric)
	}
	// Registered from init(), so a fresh engine dispatches to it.
	found := false
	for _, n := range New().Converters() {
		if n == "zip" {
			found = true
		}
	}
	if !found {
		t.Error("zip is not in the built-in registry")
	}
}

// ---------------------------------------------------------------------------
// Convert
// ---------------------------------------------------------------------------

func TestZipConvert(t *testing.T) {
	tests := []struct {
		name    string
		members []tzzMember
		want    string
	}{
		{
			name:    "empty archive",
			members: nil,
			want:    "",
		},
		{
			name:    "one member",
			members: []tzzMember{{name: "a.txt", body: "hello\n"}},
			want:    "## a.txt\n\nhello\n",
		},
		{
			name: "members keep the archive's own order, not sorted order",
			members: []tzzMember{
				{name: "z.txt", body: "last\n"},
				{name: "a.txt", body: "first\n"},
				{name: "m.txt", body: "middle\n"},
			},
			want: "## z.txt\n\nlast\n\n## a.txt\n\nfirst\n\n## m.txt\n\nmiddle\n",
		},
		{
			name: "nested paths are used verbatim as headings",
			members: []tzzMember{
				{name: "docs/intro.md", body: "# Intro\n"},
				{name: "docs/deep/notes.txt", body: "notes\n"},
			},
			want: "## docs/intro.md\n\n# Intro\n\n## docs/deep/notes.txt\n\nnotes\n",
		},
		{
			name: "directory entries are skipped",
			members: []tzzMember{
				{name: "docs", dir: true},
				{name: "docs/a.txt", body: "hi\n"},
			},
			want: "## docs/a.txt\n\nhi\n",
		},
		{
			name: "zero-byte members are skipped",
			members: []tzzMember{
				{name: "empty.txt", body: ""},
				{name: "a.txt", body: "hi\n"},
				{name: "also-empty.log", body: ""},
			},
			want: "## a.txt\n\nhi\n",
		},
		{
			name:    "an archive of nothing but empties renders nothing",
			members: []tzzMember{{name: "a.txt", body: ""}, {name: "b/", dir: true}},
			want:    "",
		},
		{
			name: "CRLF bodies are normalised by the inner converter",
			members: []tzzMember{
				{name: "a.txt", body: "one\r\ntwo\r\n"},
			},
			want: "## a.txt\n\none\ntwo\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tzzConvertZip(t, tzzMakeZip(t, tt.members...), nil)
			if got != tt.want {
				t.Errorf("Markdown =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

// TestZipConvertTitle: the archive's own file name is the document title.
func TestZipConvertTitle(t *testing.T) {
	data := tzzMakeZip(t, tzzMember{name: "a.txt", body: "hi\n"})
	res, err := tzzZipEngine().ConvertBytes(data, StreamInfoForFile("/tmp/reports/bundle.zip"), nil)
	if err != nil {
		t.Fatalf("ConvertBytes: %v", err)
	}
	if res.Title != "bundle.zip" {
		t.Errorf("Title = %q, want %q", res.Title, "bundle.zip")
	}
}

// TestZipOneBadMemberDoesNotLoseTheRest is the design point of the converter: a
// zip is a bag of unrelated things, so an unconvertible member is reported
// inline and the walk continues.
func TestZipOneBadMemberDoesNotLoseTheRest(t *testing.T) {
	const unsupported = `anymd: no converter accepted this stream (ext=".zzz" mime="application/octet-stream")`

	tests := []struct {
		name    string
		members []tzzMember
		want    string
	}{
		{
			name: "a bad member in the middle",
			members: []tzzMember{
				{name: "good1.txt", body: "one\n"},
				{name: "bad.zzz", body: "\x00\x01\x02\x03binary junk"},
				{name: "good2.txt", body: "two\n"},
			},
			want: "## good1.txt\n\none\n\n## bad.zzz\n\n*[could not convert: " + unsupported + "]*\n\n" +
				"## good2.txt\n\ntwo\n",
		},
		{
			name: "a bad member first",
			members: []tzzMember{
				{name: "bad.zzz", body: "\x00\x01\x02\x03binary junk"},
				{name: "good.txt", body: "ok\n"},
			},
			want: "## bad.zzz\n\n*[could not convert: " + unsupported + "]*\n\n## good.txt\n\nok\n",
		},
		{
			name:    "every member bad still produces a document",
			members: []tzzMember{{name: "bad.zzz", body: "\x00\x01\x02"}},
			want:    "## bad.zzz\n\n*[could not convert: " + unsupported + "]*\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tzzConvertZip(t, tzzMakeZip(t, tt.members...), nil)
			if got != tt.want {
				t.Errorf("Markdown =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

// TestZipUnsafeEntryNames: a member whose name escapes the archive root is
// never handed on. We do not extract to disk, but a name that lies about where
// it lives is a name we refuse to act on.
func TestZipUnsafeEntryNames(t *testing.T) {
	tests := []struct {
		name  string
		entry string
	}{
		{"parent traversal", "../escape.txt"},
		{"nested traversal", "docs/../../escape.txt"},
		{"traversal in the middle", "a/../../b.txt"},
		{"absolute posix path", "/etc/passwd"},
		{"windows separators traversal", `..\escape.txt`},
		{"windows drive letter", `C:\Windows\win.ini`},
		{"windows drive with forward slashes", "C:/Windows/win.ini"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := tzzMakeZip(t,
				tzzMember{name: tt.entry, body: "payload\n"},
				tzzMember{name: "good.txt", body: "ok\n"},
			)
			got := tzzConvertZip(t, data, nil)
			if strings.Contains(got, "payload") {
				t.Errorf("the unsafe member's body was emitted:\n%q", got)
			}
			if !strings.Contains(got, "*[could not convert: unsafe entry path]*") {
				t.Errorf("no note for the unsafe member:\n%q", got)
			}
			if !strings.Contains(got, "## good.txt\n\nok") {
				t.Errorf("the safe member was lost:\n%q", got)
			}
		})
	}
}

func TestZipUnsafeEntryNameUnit(t *testing.T) {
	safe := []string{"a.txt", "docs/a.txt", "a..b.txt", "..a/b.txt", "a/..b", "./a.txt", "a/./b.txt"}
	for _, n := range safe {
		if unsafeEntryName(n) {
			t.Errorf("unsafeEntryName(%q) = true, want false", n)
		}
	}
	unsafe := []string{"", "..", "../a", "a/../../b", "/a", `\a`, "C:/a", `c:\a`, "a/.."}
	for _, n := range unsafe {
		if !unsafeEntryName(n) {
			t.Errorf("unsafeEntryName(%q) = false, want true", n)
		}
	}
}

// ---------------------------------------------------------------------------
// Recursion
// ---------------------------------------------------------------------------

// TestZipNestedArchive: a zip inside a zip goes through opts.Recurse, so the
// inner members show up under the inner archive's heading.
func TestZipNestedArchive(t *testing.T) {
	inner := tzzMakeZip(t, tzzMember{name: "a.txt", body: "deep\n"})
	outer := tzzMakeZip(t,
		tzzMember{name: "inner.zip", body: string(inner)},
		tzzMember{name: "top.txt", body: "shallow\n"},
	)
	want := "## inner.zip\n\n## a.txt\n\ndeep\n\n## top.txt\n\nshallow\n"
	if got := tzzConvertZip(t, outer, nil); got != want {
		t.Errorf("Markdown =\n%q\nwant\n%q", got, want)
	}
}

// TestZipMaxDepth: when Recurse refuses to go deeper, that is a fact about the
// one member, reported as a note — not a reason to fail the archive.
func TestZipMaxDepth(t *testing.T) {
	inner := tzzMakeZip(t, tzzMember{name: "a.txt", body: "deep\n"})
	outer := tzzMakeZip(t,
		tzzMember{name: "inner.zip", body: string(inner)},
		tzzMember{name: "top.txt", body: "shallow\n"},
	)

	tests := []struct {
		name string
		opts *Options
		want string
	}{
		{
			name: "depth 2 reaches the inner members",
			opts: &Options{MaxDepth: 2},
			want: "## inner.zip\n\n## a.txt\n\ndeep\n\n## top.txt\n\nshallow\n",
		},
		{
			name: "depth 1 stops inside the inner archive",
			opts: &Options{MaxDepth: 1},
			want: "## inner.zip\n\n## a.txt\n\n*[could not convert: anymd: max recursion depth exceeded]*\n\n" +
				"## top.txt\n\nshallow\n",
		},
		{
			name: "negative depth stops at the outer archive's own members",
			opts: &Options{MaxDepth: -1},
			want: "## inner.zip\n\n*[could not convert: anymd: max recursion depth exceeded]*\n\n" +
				"## top.txt\n\n*[could not convert: anymd: max recursion depth exceeded]*\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tzzConvertZip(t, outer, tt.opts); got != tt.want {
				t.Errorf("Markdown =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Zip-bomb defense
// ---------------------------------------------------------------------------

// tzzSetLimits swaps the bomb caps for the duration of one test.
func tzzSetLimits(t *testing.T, entries int, entryBytes, totalBytes int64) {
	t.Helper()
	oe, ob, ot := maxZipEntries, maxEntryBytes, maxTotalBytes
	maxZipEntries, maxEntryBytes, maxTotalBytes = entries, entryBytes, totalBytes
	t.Cleanup(func() { maxZipEntries, maxEntryBytes, maxTotalBytes = oe, ob, ot })
}

func TestZipDefaultLimits(t *testing.T) {
	if maxZipEntries != 10000 {
		t.Errorf("maxZipEntries = %d, want 10000", maxZipEntries)
	}
	if maxEntryBytes != 64<<20 {
		t.Errorf("maxEntryBytes = %d, want 64 MiB", maxEntryBytes)
	}
	if maxTotalBytes != 512<<20 {
		t.Errorf("maxTotalBytes = %d, want 512 MiB", maxTotalBytes)
	}
	// A per-entry cap that is not smaller than the archive cap would make the
	// archive cap unreachable and the per-entry cap the only real bound.
	if maxEntryBytes >= maxTotalBytes {
		t.Errorf("maxEntryBytes (%d) must be below maxTotalBytes (%d)", maxEntryBytes, maxTotalBytes)
	}
}

func TestZipEntryCountCap(t *testing.T) {
	tzzSetLimits(t, 2, maxEntryBytes, maxTotalBytes)
	members := []tzzMember{
		{name: "a.txt", body: "a\n"},
		{name: "b.txt", body: "b\n"},
		{name: "c.txt", body: "c\n"},
	}
	_, err := tzzZipEngine().ConvertBytes(tzzMakeZip(t, members...), StreamInfoForFile("bomb.zip"), nil)
	if err == nil {
		t.Fatal("an archive over the entry cap converted without error")
	}
	if want := "anymd: zip: zip has 3 entries, over the 2 limit"; err.Error() != want {
		t.Errorf("err = %q, want %q", err.Error(), want)
	}

	// Exactly at the cap is fine.
	if got := tzzConvertZip(t, tzzMakeZip(t, members[:2]...), nil); got != "## a.txt\n\na\n\n## b.txt\n\nb\n" {
		t.Errorf("an archive exactly at the entry cap was rejected: %q", got)
	}
}

// TestZipPerEntryCap: one oversized member is skipped with a note; its
// siblings still convert.
func TestZipPerEntryCap(t *testing.T) {
	tzzSetLimits(t, maxZipEntries, 8, 1<<20)
	data := tzzMakeZip(t,
		tzzMember{name: "big.txt", body: strings.Repeat("x", 64)},
		tzzMember{name: "small.txt", body: "ok\n"},
	)
	want := "## big.txt\n\n*[could not convert: entry exceeds the 8 byte limit]*\n\n## small.txt\n\nok\n"
	if got := tzzConvertZip(t, data, nil); got != want {
		t.Errorf("Markdown =\n%q\nwant\n%q", got, want)
	}
}

// TestZipCumulativeCap is the cap that actually stops a bomb: every member is
// comfortably under the per-entry limit, and together they are not.
func TestZipCumulativeCap(t *testing.T) {
	tzzSetLimits(t, maxZipEntries, 1<<20, 20)
	members := make([]tzzMember, 0, 8)
	for i := 0; i < 8; i++ {
		members = append(members, tzzMember{
			name: fmt.Sprintf("part%d.txt", i),
			body: strings.Repeat("y", 10), // each is 10 bytes, far under 1 MiB
		})
	}
	_, err := tzzZipEngine().ConvertBytes(tzzMakeZip(t, members...), StreamInfoForFile("bomb.zip"), nil)
	if err == nil {
		t.Fatal("a cumulative-overflow archive converted without error")
	}
	if want := "anymd: zip: decompressed size exceeds the 20 byte limit"; err.Error() != want {
		t.Errorf("err = %q, want %q", err.Error(), want)
	}
}

// TestZipCumulativeCapNotTrippedEarly: an archive that fits must not be
// rejected, including one that lands exactly on the budget.
func TestZipCumulativeCapNotTrippedEarly(t *testing.T) {
	tzzSetLimits(t, maxZipEntries, 1<<20, 6)
	data := tzzMakeZip(t,
		tzzMember{name: "a.txt", body: "aaa"},
		tzzMember{name: "b.txt", body: "bbb"},
	)
	want := "## a.txt\n\naaa\n\n## b.txt\n\nbbb\n"
	if got := tzzConvertZip(t, data, nil); got != want {
		t.Errorf("Markdown =\n%q\nwant\n%q", got, want)
	}
}

// TestZipLiesAboutUncompressedSize: the converter must never trust the size
// field in the header. We cannot forge one through archive/zip, so this pins
// the property instead — a member is bounded by what it actually decompresses
// to, and a huge declared size on a tiny body is harmless.
func TestZipHighlyCompressibleMemberIsBounded(t *testing.T) {
	tzzSetLimits(t, maxZipEntries, 1024, 1<<20)
	// 1 MiB of zeroes deflates to a couple of hundred bytes: the classic
	// compression-ratio attack.
	data := tzzMakeZip(t,
		tzzMember{name: "bomb.txt", body: strings.Repeat("\x00", 1<<20)},
		tzzMember{name: "ok.txt", body: "fine\n"},
	)
	if len(data) > 4096 {
		t.Fatalf("fixture did not compress; got %d bytes", len(data))
	}
	want := "## bomb.txt\n\n*[could not convert: entry exceeds the 1024 byte limit]*\n\n## ok.txt\n\nfine\n"
	if got := tzzConvertZip(t, data, nil); got != want {
		t.Errorf("Markdown =\n%q\nwant\n%q", got, want)
	}
}

// TestZipCorruptArchiveIsAnError, not a panic and not silent success.
func TestZipCorruptArchiveIsAnError(t *testing.T) {
	_, err := tzzZipEngine().ConvertBytes([]byte("PK\x03\x04 not really a zip"), StreamInfo{Extension: ".zip"}, nil)
	if err == nil {
		t.Fatal("a corrupt archive converted without error")
	}
	if !strings.HasPrefix(err.Error(), "anymd: zip: read zip: ") {
		t.Errorf("err = %q, want an anymd: zip: read zip: … error", err.Error())
	}
}

// TestZipTruncatedMemberIsNoted: a member whose deflate stream is damaged is a
// note, not a lost archive.
func TestZipTruncatedMemberIsNoted(t *testing.T) {
	good := tzzMakeZip(t,
		tzzMember{name: "broken.txt", body: strings.Repeat("abcdefgh", 64)},
		tzzMember{name: "ok.txt", body: "fine\n"},
	)
	// Corrupt the first member's compressed payload in place. The central
	// directory still parses, so the failure surfaces per-member at read time.
	corrupt := append([]byte(nil), good...)
	for i := 40; i < 70 && i < len(corrupt); i++ {
		corrupt[i] ^= 0xff
	}
	got := tzzConvertZip(t, corrupt, nil)
	if !strings.Contains(got, "## broken.txt\n\n*[could not convert: ") {
		t.Errorf("no note for the damaged member:\n%q", got)
	}
	if !strings.Contains(got, "## ok.txt\n\nfine") {
		t.Errorf("the healthy member was lost:\n%q", got)
	}
}

// TestZipNoteIsSingleLine: an error string with newlines must not break out of
// the italic note and corrupt the surrounding markdown.
func TestZipNoteIsSingleLine(t *testing.T) {
	got := note("a.txt", "line one\nline two\n\nline three")
	want := "## a.txt\n\n*[could not convert: line one line two line three]*\n"
	if got != want {
		t.Errorf("note() = %q, want %q", got, want)
	}
}

// tzzSeekOnly hides ReaderAt, leaving only Read and Seek — the shape the zip
// reader cannot use directly.
type tzzSeekOnly struct{ r *bytes.Reader }

func (s tzzSeekOnly) Read(p []byte) (int, error) { return s.r.Read(p) }
func (s tzzSeekOnly) Seek(off int64, whence int) (int64, error) {
	return s.r.Seek(off, whence)
}

// TestZipWithoutReaderAt: archive/zip needs random access, so a ReadSeeker that
// is not a ReaderAt has to be buffered. The engine hands us a *bytes.Reader in
// practice, but the contract is io.ReadSeeker and a container converter one
// level up may hand us something narrower.
func TestZipWithoutReaderAt(t *testing.T) {
	data := tzzMakeZip(t,
		tzzMember{name: "a.txt", body: "one\n"},
		tzzMember{name: "b.txt", body: "two\n"},
	)
	c := &ZipConverter{}
	r := tzzSeekOnly{bytes.NewReader(data)}
	if _, ok := interface{}(r).(interface {
		ReadAt([]byte, int64) (int, error)
	}); ok {
		t.Fatal("fixture still exposes ReadAt; the test proves nothing")
	}
	if !c.Accepts(r, StreamInfo{}, nil) {
		t.Fatal("Accepts declined a plain archive read through a seek-only reader")
	}
	res, err := c.Convert(r, StreamInfo{FileName: "bundle.zip"}, &Options{engine: tzzZipEngine()})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	want := "## a.txt\n\none\n\n## b.txt\n\ntwo\n"
	if res.Markdown != want {
		t.Errorf("Markdown = %q, want %q", res.Markdown, want)
	}
}
