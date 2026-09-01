package anymd_test

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/muthuishere/anymd"
)

// Example is the headline: hand anymd a document and get GitHub-flavored
// Markdown back. The hints in StreamInfo are optional — with none at all the
// engine sniffs the first 512 bytes — but passing the extension you already
// know saves it the guess.
func Example() {
	doc := strings.NewReader(`
		<html><head><title>Quarterly Notes</title></head><body>
		<h1>Quarterly Notes</h1>
		<p>Revenue is <b>up</b>.</p>
		<ul><li>EMEA</li><li>APAC</li></ul>
		</body></html>`)

	res, err := anymd.Convert(doc, anymd.StreamInfo{Extension: ".html"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("title:", res.Title)
	fmt.Println(res.Markdown)
	// Output:
	// title: Quarterly Notes
	// # Quarterly Notes
	//
	// Revenue is **up**.
	//
	// - EMEA
	// - APAC
}

// ExampleConvertBytes converts a document already in memory. Delimited text
// becomes one GFM pipe table with the first row promoted to the header.
func ExampleConvertBytes() {
	csv := []byte("region,seats,renewed\nEMEA,120,yes\nAPAC,64,no\n")

	res, err := anymd.ConvertBytes(csv, anymd.StreamInfo{Extension: ".csv"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(res.Markdown)
	// Output:
	// | region | seats | renewed |
	// | --- | --- | --- |
	// | EMEA | 120 | yes |
	// | APAC | 64 | no |
}

// ExampleConvertFile converts a file from disk. The path seeds the extension,
// filename and MIME hints, so no StreamInfo is needed.
//
// The temp path is deliberately not printed: it changes on every run, and an
// example's output has to be byte-stable to be worth compiling.
func ExampleConvertFile() {
	dir, err := os.MkdirTemp("", "anymd-example")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "release.md")
	if err := os.WriteFile(path, []byte("# v1.2.0\n\n- faster zip walk\n"), 0o600); err != nil {
		log.Fatal(err)
	}

	res, err := anymd.ConvertFile(path)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(res.Markdown)
	// Output:
	// # v1.2.0
	//
	// - faster zip walk
}

// vcardConverter is a converter for a made-up format, written the way a
// consumer would write one: two required methods, plus the optional Named and
// Prioritized.
//
// Accepts is the hot path — it runs against every stream the engine sees — so
// it looks at hints and a magic prefix only, and never parses the document.
type vcardConverter struct{}

// Name gives the converter a stable identity in errors and in Engine.Converters.
// Without it the engine falls back to the Go type name.
func (vcardConverter) Name() string { return "vcard" }

// Priority puts this ahead of the plaintext fallback. A .vcf file decodes as
// UTF-8 text, so at PriorityFallback or later the catch-all would claim it
// first and emit the raw file. PrioritySpecific (0) is the right home for a
// converter keyed to a unique extension and magic string.
func (vcardConverter) Priority() int { return anymd.PrioritySpecific }

func (vcardConverter) Accepts(r io.ReadSeeker, info anymd.StreamInfo, opts *anymd.Options) bool {
	if info.HasExt(".vcf") {
		return true
	}
	var head [11]byte
	n, _ := io.ReadFull(r, head[:])
	return string(head[:n]) == "BEGIN:VCARD"
}

func (vcardConverter) Convert(r io.ReadSeeker, info anymd.StreamInfo, opts *anymd.Options) (anymd.Result, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return anymd.Result{}, err
	}
	var name string
	var rows []string
	for _, line := range strings.Split(string(b), "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || key == "BEGIN" || key == "END" {
			continue
		}
		if key == "FN" {
			name = val
			continue
		}
		rows = append(rows, "- **"+key+"**: "+val)
	}
	if name == "" {
		// A converter that accepts and then fails is a hard error: the engine
		// does not fall through to a catch-all that would emit the raw file.
		return anymd.Result{}, errors.New("vcard has no FN (formatted name) property")
	}
	md := "# " + name + "\n"
	if len(rows) > 0 {
		md += "\n" + strings.Join(rows, "\n") + "\n"
	}
	return anymd.Result{Markdown: md, Title: name}, nil
}

// ExampleEngine_Register adds a consumer-defined converter to a private
// registry. New gives you every built-in; Register layers yours on top.
func ExampleEngine_Register() {
	e := anymd.New()
	e.Register(vcardConverter{})

	card := []byte("BEGIN:VCARD\nVERSION:3.0\nFN:Ada Lovelace\nEMAIL:ada@example.com\nEND:VCARD\n")

	res, err := e.ConvertBytes(card, anymd.StreamInfo{Extension: ".vcf"}, nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(res.Markdown)

	// An accepting converter that fails is a hard error, never a silent
	// fall-through to the plaintext fallback.
	_, err = e.ConvertBytes([]byte("BEGIN:VCARD\nEND:VCARD\n"), anymd.StreamInfo{Extension: ".vcf"}, nil)
	fmt.Println("err:", err)

	// Output:
	// # Ada Lovelace
	//
	// - **VERSION**: 3.0
	// - **EMAIL**: ada@example.com
	// err: anymd: vcard: vcard has no FN (formatted name) property
}

// ExampleEngine_Converters lists the registry in dispatch order: ascending
// priority, ties broken by registration order.
//
// Only the invariants are printed, not the whole list — new formats land in
// this project regularly, and an example asserting the full order would break
// on every one of them. What does not change is that the text catch-all sits
// alone at PriorityFallback, and so is always last.
func ExampleEngine_Converters() {
	names := anymd.New().Converters()

	fmt.Println("last:", names[len(names)-1])
	fmt.Println("has csv:", slices.Contains(names, "csv"))
	fmt.Println("has pdf:", slices.Contains(names, "pdf"))
	// Output:
	// last: plaintext
	// has csv: true
	// has pdf: true
}

// ExampleOptions shows MaxDepth bounding container recursion. Containers
// recurse through Options.Recurse, which carries the depth counter, so the
// limit is enforced centrally rather than trusted to each converter.
//
// A member that is too deep is reported inline and its siblings still convert:
// losing a whole archive because one member nested too far would be the wrong
// trade.
func ExampleOptions() {
	inner := makeZip(map[string]string{"note.txt": "hello from the inside"})
	outer := makeZip(map[string]string{"inner.zip": string(inner)})

	e := anymd.New()

	// MaxDepth 1: the outer archive's members convert, but the inner archive's
	// members are one level too far.
	shallow, err := e.ConvertBytes(outer, anymd.StreamInfo{Extension: ".zip"}, &anymd.Options{MaxDepth: 1})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(shallow.Markdown)

	fmt.Println("---")

	// MaxDepth 2 reaches all the way down.
	deep, err := e.ConvertBytes(outer, anymd.StreamInfo{Extension: ".zip"}, &anymd.Options{MaxDepth: 2})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(deep.Markdown)

	// Output:
	// ## inner.zip
	//
	// ## note.txt
	//
	// *[could not convert: anymd: max recursion depth exceeded]*
	// ---
	// ## inner.zip
	//
	// ## note.txt
	//
	// hello from the inside
}

// ExampleUnsupportedError shows the two ways to read a "nothing claimed this"
// failure: the sentinel, for a quick branch, and the typed error, for the
// detail worth logging.
func ExampleUnsupportedError() {
	// Binary bytes with a NUL, so even the text fallback declines rather than
	// emitting garbage as "markdown".
	blob := []byte{0x00, 0x01, 0x02, 0x03, 0xff, 0xfe}

	_, err := anymd.ConvertBytes(blob, anymd.StreamInfo{Extension: ".widget"})

	fmt.Println("unsupported:", errors.Is(err, anymd.ErrUnsupported))

	var ue *anymd.UnsupportedError
	if errors.As(err, &ue) {
		fmt.Println("ext:", ue.Ext)
		fmt.Println("mime:", ue.Mime)
		// Declined names every converter that looked and passed. It is kept out
		// of Error() so the registry never leaks into rendered markdown.
		fmt.Println("declined some:", len(ue.Declined) > 0)
	}
	// Output:
	// unsupported: true
	// ext: .widget
	// mime: application/octet-stream
	// declined some: true
}

// ExampleErrNoTextLayer shows the scanned-PDF contract. The fixture is a
// structurally valid PDF whose only page draws a filled rectangle and no text —
// the shape of a pure scan, without needing a committed binary or a real image.
//
// The point is that this is an error, not an empty success. "" is exactly what
// a genuinely blank document would produce, so collapsing the two would leave
// the caller unable to tell "nothing to extract" from "the text is locked
// inside pixels" — and only the second is worth routing to an OCR step, which
// anymd deliberately does not ship.
func ExampleErrNoTextLayer() {
	_, err := anymd.ConvertBytes(imageOnlyPDF(), anymd.StreamInfo{Extension: ".pdf"})

	fmt.Println("no text layer:", errors.Is(err, anymd.ErrNoTextLayer))
	fmt.Println("empty output:", err == nil)
	// Output:
	// no text layer: true
	// empty output: false
}

// makeZip builds a zip archive in memory. Fixtures are built in Go on purpose:
// the repo carries no binary test data, so these examples pass on a bare clone.
// Names are written in sorted order so the emitted markdown is deterministic.
func makeZip(members map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(members))
	for name := range members {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		w, err := zw.Create(name)
		if err != nil {
			log.Fatal(err)
		}
		if _, err := io.WriteString(w, members[name]); err != nil {
			log.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		log.Fatal(err)
	}
	return buf.Bytes()
}

// imageOnlyPDF writes a minimal one-page PDF with a content stream that paints
// a rectangle and never enters a text object — a stand-in for a scan.
func imageOnlyPDF() []byte {
	const content = "0 0 0 rg\n100 100 400 500 re\nf\n"
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [ 3 0 R ] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [ 0 0 612 792 ] /Resources << >> /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content),
	}

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs)+1)
	for i, o := range objs {
		offsets[i+1] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(objs)+1)
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i <= len(objs); i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objs)+1, xref)
	return buf.Bytes()
}
