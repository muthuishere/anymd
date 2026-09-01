package anymd

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Fuzzing anymd is not about output quality — it is about the one invariant in
// CONTRACT.md rule 5 and the README's security posture: **a converter never
// panics**. Every converter here is a parser aimed at bytes an attacker chose,
// so an `error` on garbage is correct and a panic is a P0. Each target below
// therefore asserts exactly one thing: the call returned. Results and errors
// are deliberately ignored.
//
// Seeds come from two places:
//
//  1. hand-written minimal / truncated / empty cases, committed inline so the
//     corpus is useful in a bare CI checkout with nothing external mounted; and
//  2. Microsoft markitdown's own test corpus, read from disk at test time.
//
// No binary is ever copied into this repo (CONTRACT.md rule 6). If the corpus
// is not on this machine the external seeds are skipped silently, so CI never
// depends on a path outside the module.

// fzCorpusEnv names the environment variable that points at a checkout of
// markitdown's packages/markitdown/tests/test_files directory.
const fzCorpusEnv = "ANYMD_CORPUS"

// fzCorpusDefault is the author's local checkout. It is a convenience only:
// when it does not exist, every corpus seed is skipped.
const fzCorpusDefault = "/Users/muthuishere/muthu/deemwarworkspace/ceo-workspace/deemwar-one-os/anywiki/ref/markitdown-main/packages/markitdown/tests/test_files"

// fzMaxInput bounds the work per execution. Fuzzing a 100 MB document tells us
// nothing a 2 MB one does not, and starves the mutator of iterations.
const fzMaxInput = 2 << 20 // 2 MiB

// fzMaxSeed bounds a seed read off disk. Seeds are executed on every run of the
// target, so a huge one buys coverage at the price of throughput.
const fzMaxSeed = 1 << 20 // 1 MiB

func fzCorpusDir() string {
	if d := os.Getenv(fzCorpusEnv); d != "" {
		return d
	}
	return fzCorpusDefault
}

// fzSeedFiles adds each named file from the external corpus, silently skipping
// any that is missing, unreadable, or larger than fzMaxSeed.
func fzSeedFiles(f *testing.F, names ...string) {
	f.Helper()
	dir := fzCorpusDir()
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil || len(b) == 0 || len(b) > fzMaxSeed {
			continue
		}
		f.Add(b)
	}
}

// fzSeeds adds hand-written seeds: the empty document, a truncated prefix of
// each sample, and the sample itself. Truncation is where length-prefixed
// binary formats historically panic.
func fzSeeds(f *testing.F, samples ...[]byte) {
	f.Helper()
	f.Add([]byte(nil))
	f.Add([]byte{})
	for _, s := range samples {
		f.Add(s)
		if len(s) > 4 {
			f.Add(s[:len(s)/2])
			f.Add(s[:4])
		}
	}
}

// fzFirst wraps a converter so it is asked before every built-in, whatever its
// declared priority. Without it a mutated .docx that stopped looking like a zip
// would be claimed by the plaintext fallback and the target under test would
// never see it.
type fzFirst struct{ Converter }

func (fzFirst) Priority() int { return -1000 }
func (fzFirst) Name() string  { return "fuzztarget" }

// fzEngine returns an engine with c in front of the full built-in registry.
// The built-ins stay registered because container converters (zip, epub, msg)
// recurse through opts.Recurse, which dispatches against this same engine.
func fzEngine(c Converter) *Engine {
	e := New()
	e.Register(fzFirst{c})
	return e
}

var (
	fzEngineOnce  sync.Once
	fzEngineCache map[string]*Engine
	fzDispatchEng *Engine
)

func fzEngineFor(name string, c Converter) *Engine {
	fzEngineOnce.Do(func() {
		fzEngineCache = map[string]*Engine{}
		fzDispatchEng = New()
	})
	if e, ok := fzEngineCache[name]; ok {
		return e
	}
	e := fzEngine(c)
	fzEngineCache[name] = e
	return e
}

// fzRun is the whole assertion: convert, and return. Any panic fails the test
// and Go records the input under testdata/fuzz — which is exactly the artifact
// we want committed for a regression.
func fzRun(t *testing.T, name string, c Converter, ext string, data []byte) {
	t.Helper()
	if len(data) > fzMaxInput {
		t.Skip("input over the fuzz size bound")
	}
	e := fzEngineFor(name, c)
	info := StreamInfo{Extension: ext, FileName: "fuzz" + ext}
	// MaxDepth is kept small: container recursion is bounded behaviour we
	// already test directly, and deep nesting only burns fuzz iterations.
	_, _ = e.ConvertBytes(data, info, &Options{MaxDepth: 3})
}

// --- minimal hand-written fixtures ------------------------------------------
//
// Each is the smallest thing that still steers the mutator at the real parser:
// a zip end-of-central-directory record, a PDF header, an OLE2 signature.

var (
	fzMinZip = []byte("PK\x03\x04\x14\x00\x00\x00\x00\x00" +
		"\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00" +
		"PK\x05\x06\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00")
	fzMinPDF  = []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\ntrailer\n<< /Root 1 0 R >>\n%%EOF\n")
	fzMinOLE2 = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	fzMinPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR" +
		"\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89" +
		"\x00\x00\x00\x00IEND\xaeB`\x82")
	fzMinJPEG = []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00\xff\xd9")
)

// --- targets ----------------------------------------------------------------

// FuzzDocx pushes arbitrary bytes at the Word converter. docx is a zip of XML,
// so the interesting failures live in the seam between the two.
func FuzzDocx(f *testing.F) {
	fzSeeds(f, fzMinZip)
	f.Add([]byte("PK\x03\x04broken"))
	fzSeedFiles(f, "test.docx", "test_with_comment.docx", "equations.docx", "rlink.docx")
	f.Fuzz(func(t *testing.T, data []byte) {
		fzRun(t, "docx", &DocxConverter{}, ".docx", data)
	})
}

// FuzzPptx pushes arbitrary bytes at the PowerPoint converter, which walks
// slide relationships and follows them across parts.
func FuzzPptx(f *testing.F) {
	fzSeeds(f, fzMinZip)
	fzSeedFiles(f, "test_svg_no_fallback.pptx", "test.pptx")
	f.Fuzz(func(t *testing.T, data []byte) {
		fzRun(t, "pptx", &PptxConverter{}, ".pptx", data)
	})
}

// FuzzXlsx pushes arbitrary bytes at the modern Excel converter.
func FuzzXlsx(f *testing.F) {
	fzSeeds(f, fzMinZip)
	fzSeedFiles(f, "test.xlsx")
	f.Fuzz(func(t *testing.T, data []byte) {
		fzRun(t, "xlsx", &XlsxConverter{}, ".xlsx", data)
	})
}

// FuzzXls pushes arbitrary bytes at the legacy BIFF reader. This is the target
// most likely to find something: BIFF is a stream of length-prefixed records
// inside an OLE2 container, i.e. attacker-controlled offsets all the way down.
func FuzzXls(f *testing.F) {
	fzSeeds(f, fzMinOLE2)
	fzSeedFiles(f, "test.xls")
	f.Fuzz(func(t *testing.T, data []byte) {
		fzRun(t, "xls", &XLSConverter{}, ".xls", data)
	})
}

// FuzzPdf pushes arbitrary bytes at the PDF text extractor: a cross-reference
// table of byte offsets, each of which the document itself supplies.
func FuzzPdf(f *testing.F) {
	fzSeeds(f, fzMinPDF)
	f.Add([]byte("%PDF-1.7\ntrailer\n"))
	fzSeedFiles(f, "masterformat_partial_numbering.pdf", "movie-theater-booking-2024.pdf",
		"RECEIPT-2024-TXN-98765_retail_purchase.pdf", "SPARSE-2024-INV-1234_borderless_table.pdf",
		"test.pdf", "REPAIR-2022-INV-001_multipage.pdf", "MEDRPT-2024-PAT-3847_medical_report_scan.pdf")
	f.Fuzz(func(t *testing.T, data []byte) {
		fzRun(t, "pdf", &PDFConverter{}, ".pdf", data)
	})
}

// FuzzZip pushes arbitrary bytes at the container converter, which recurses
// into every member and so reaches every other converter behind it.
func FuzzZip(f *testing.F) {
	fzSeeds(f, fzMinZip)
	fzSeedFiles(f, "test_files.zip")
	f.Fuzz(func(t *testing.T, data []byte) {
		fzRun(t, "zip", &ZipConverter{}, ".zip", data)
	})
}

// FuzzEpub pushes arbitrary bytes at the EPUB converter: a zip whose OPF
// manifest names the parts to visit, in an order the document chooses.
func FuzzEpub(f *testing.F) {
	fzSeeds(f, fzMinZip)
	fzSeedFiles(f, "test.epub")
	f.Fuzz(func(t *testing.T, data []byte) {
		fzRun(t, "epub", &EPUBConverter{}, ".epub", data)
	})
}

// FuzzMsg pushes arbitrary bytes at the Outlook converter, which walks an OLE2
// compound file's directory tree.
func FuzzMsg(f *testing.F) {
	fzSeeds(f, fzMinOLE2)
	fzSeedFiles(f, "test_outlook_msg.msg")
	f.Fuzz(func(t *testing.T, data []byte) {
		fzRun(t, "msg", &MsgConverter{}, ".msg", data)
	})
}

// FuzzHTML pushes arbitrary bytes at the HTML converter. Malformed markup is
// the normal case here, so the bar is that pathological nesting still returns.
func FuzzHTML(f *testing.F) {
	fzSeeds(f,
		[]byte("<html><body><h1>x</h1><table><tr><td>a</td></tr></table></body></html>"),
		[]byte("<table><tr><td><table><tr><td>deep"),
		[]byte("<!doctype html><title>t</title><ul><li>a<li>b"),
	)
	fzSeedFiles(f, "test_blog.html", "test_serp.html", "test_wikipedia.html")
	f.Fuzz(func(t *testing.T, data []byte) {
		fzRun(t, "html", &HTMLConverter{}, ".html", data)
	})
}

// FuzzRSS pushes arbitrary bytes at the feed converter, i.e. at an XML parser
// with entity expansion and namespace handling behind it.
func FuzzRSS(f *testing.F) {
	fzSeeds(f,
		[]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>t</title><item><title>i</title></item></channel></rss>`),
		[]byte(`<feed xmlns="http://www.w3.org/2005/Atom"><title>t</title><entry><title>e</title></entry></feed>`),
	)
	fzSeedFiles(f, "test_rss.xml")
	f.Fuzz(func(t *testing.T, data []byte) {
		fzRun(t, "rss", &RSSConverter{}, ".xml", data)
	})
}

// FuzzImage pushes arbitrary bytes at the image/EXIF reader. EXIF is a nest of
// IFD pointers that can loop, point past the file, or claim absurd counts.
func FuzzImage(f *testing.F) {
	fzSeeds(f, fzMinPNG, fzMinJPEG)
	fzSeedFiles(f, "test.jpg", "test_llm.jpg")
	f.Fuzz(func(t *testing.T, data []byte) {
		fzRun(t, "image", &ImageConverter{}, ".jpg", data)
	})
}

// FuzzJSON pushes arbitrary bytes at the JSON converter.
func FuzzJSON(f *testing.F) {
	fzSeeds(f,
		[]byte(`{"a":[1,2,{"b":null}],"c":"é"}`),
		[]byte(`[[[[[[[[[[1]]]]]]]]]]`),
		[]byte(`{"a":`),
	)
	fzSeedFiles(f, "test.json")
	f.Fuzz(func(t *testing.T, data []byte) {
		fzRun(t, "json", &JSONConverter{}, ".json", data)
	})
}

// FuzzCSV pushes arbitrary bytes at the delimiter sniffer and table emitter —
// ragged rows and unterminated quotes are the interesting shapes.
func FuzzCSV(f *testing.F) {
	fzSeeds(f,
		[]byte("a,b,c\n1,2,3\n"),
		[]byte("a\tb\n1\t2\n"),
		[]byte("a,\"b\n1,2"),
		[]byte(",,,\n,,\n,"),
	)
	fzSeedFiles(f, "test_mskanji.csv")
	f.Fuzz(func(t *testing.T, data []byte) {
		fzRun(t, "csv", &CSVConverter{}, ".csv", data)
	})
}

// FuzzIpynb pushes arbitrary bytes at the notebook converter: JSON whose shape
// (cells, cell_type, source as string or list) the document controls.
func FuzzIpynb(f *testing.F) {
	fzSeeds(f,
		[]byte(`{"cells":[{"cell_type":"markdown","source":["# h\n"]}],"metadata":{},"nbformat":4}`),
		[]byte(`{"cells":[{"cell_type":"code","source":"x=1","outputs":[{"text":["out"]}]}]}`),
		[]byte(`{"cells":null}`),
	)
	fzSeedFiles(f, "test_notebook.ipynb")
	f.Fuzz(func(t *testing.T, data []byte) {
		fzRun(t, "ipynb", &IpynbConverter{}, ".ipynb", data)
	})
}

// FuzzEngineDispatch fuzzes the layer the per-format targets bypass: arbitrary
// bytes with an arbitrary extension hint, through the real registry. This is
// the CLI's actual code path (`anymd -t <ext>`), where the hint and the bytes
// routinely disagree — a .pdf hint on zip bytes, an empty hint on anything.
func FuzzEngineDispatch(f *testing.F) {
	seeds := []struct {
		ext  string
		data []byte
	}{
		{".txt", []byte("hello world\n")},
		{"", []byte("hello world\n")},
		{".pdf", fzMinZip},   // hint and bytes deliberately disagree
		{".docx", fzMinPDF},  //
		{".xls", fzMinPNG},   //
		{".zip", fzMinOLE2},  //
		{".html", fzMinJPEG}, //
		{".json", []byte("not json at all")},
		{"..", []byte{0}},
		{strings.Repeat(".x", 64), []byte("a")},
		{".csv", nil},
	}
	for _, s := range seeds {
		f.Add(s.ext, s.data)
	}
	for _, n := range []string{"random.bin", "test.json", "test_notebook.ipynb", "test_mskanji.csv", "test.epub"} {
		b, err := os.ReadFile(filepath.Join(fzCorpusDir(), n))
		if err != nil || len(b) == 0 || len(b) > fzMaxSeed {
			continue
		}
		f.Add(filepath.Ext(n), b)
	}

	fzEngineOnce.Do(func() {
		fzEngineCache = map[string]*Engine{}
		fzDispatchEng = New()
	})

	f.Fuzz(func(t *testing.T, ext string, data []byte) {
		if len(data) > fzMaxInput || len(ext) > 256 {
			t.Skip("input over the fuzz size bound")
		}
		info := StreamInfo{Extension: ext, FileName: "fuzz" + ext}
		_, _ = fzDispatchEng.ConvertBytes(data, info, &Options{MaxDepth: 3})
	})
}
