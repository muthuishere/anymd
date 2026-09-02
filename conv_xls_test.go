package anymd

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"strings"
	"testing"
	"testing/quick"
)

// ---------------------------------------------------------------------------
// Fixture builder.
//
// The contract forbids committing binaries, so the .xls these tests use is
// assembled here: a real BIFF8 workbook stream inside the real Compound File
// container that conv_msg_test.go's buildCFB already writes. Sharing that
// builder is deliberate — the two converters must disagree about the SAME
// bytes, so the disambiguation test is only meaningful if both sides are built
// by one writer.
// ---------------------------------------------------------------------------

// xlsRecord frames one BIFF record: a 16-bit id, a 16-bit length, the body.
func xlsRecord(id uint16, body []byte) []byte {
	out := make([]byte, 4, 4+len(body))
	binary.LittleEndian.PutUint16(out[0:], id)
	binary.LittleEndian.PutUint16(out[2:], uint16(len(body)))
	return append(out, body...)
}

// xlsStr writes a BIFF8 string body: a flags byte (0 = compressed, one byte per
// character) followed by the characters. The character count lives in a
// preceding field whose width differs per record, so callers write it.
func xlsStr(s string) []byte { return append([]byte{0x00}, []byte(s)...) }

// xlsBOF starts a substream: 0x0005 for the workbook globals, 0x0010 for a
// worksheet.
func xlsBOF(docType uint16) []byte {
	body := make([]byte, 16)
	binary.LittleEndian.PutUint16(body[0:], 0x0600) // BIFF8
	binary.LittleEndian.PutUint16(body[2:], docType)
	return xlsRecord(0x0809, body)
}

func xlsEOF() []byte { return xlsRecord(0x000A, nil) }

// xlsRowRec emits a ROW record declaring one row's used span [first, last).
func xlsRowRec(index, first, last uint16) []byte {
	body := make([]byte, 16)
	binary.LittleEndian.PutUint16(body[0:], index)
	binary.LittleEndian.PutUint16(body[2:], first)
	binary.LittleEndian.PutUint16(body[4:], last)
	return xlsRecord(0x0208, body)
}

// xlsLabel emits a LABEL record: a cell holding an inline string.
func xlsLabel(row, col uint16, s string) []byte {
	body := make([]byte, 8)
	binary.LittleEndian.PutUint16(body[0:], row)
	binary.LittleEndian.PutUint16(body[2:], col)
	binary.LittleEndian.PutUint16(body[4:], 0) // xf index
	binary.LittleEndian.PutUint16(body[6:], uint16(len(s)))
	return xlsRecord(0x0204, append(body, xlsStr(s)...))
}

// xlsNumber emits a NUMBER record: a cell holding an IEEE-754 double.
func xlsNumber(row, col uint16, v float64) []byte {
	body := make([]byte, 14)
	binary.LittleEndian.PutUint16(body[0:], row)
	binary.LittleEndian.PutUint16(body[2:], col)
	binary.LittleEndian.PutUint16(body[4:], 0) // xf index
	binary.LittleEndian.PutUint64(body[6:], math.Float64bits(v))
	return xlsRecord(0x0203, body)
}

// xlsBoundSheet emits a BOUNDSHEET record: a sheet name plus the absolute
// offset of its substream inside the workbook stream.
func xlsBoundSheet(pos uint32, name string) []byte {
	body := make([]byte, 7)
	binary.LittleEndian.PutUint32(body[0:], pos)
	body[4], body[5] = 0, 0 // visible worksheet
	body[6] = byte(len(name))
	return xlsRecord(0x0085, append(body, xlsStr(name)...))
}

// xlsSheetSpec describes one sheet: a name and its rows. A string cell becomes
// a LABEL, a float64 becomes a NUMBER, and nil leaves the cell undefined.
type xlsSheetSpec struct {
	Name string
	Rows [][]any
}

// buildXLS assembles a legacy workbook from sheet specs.
func buildXLS(t *testing.T, sheets []xlsSheetSpec) []byte {
	t.Helper()

	// Substreams first: BOUNDSHEET stores an absolute offset, so their sizes
	// must be known before the globals block is written.
	subs := make([][]byte, len(sheets))
	for i, sh := range sheets {
		s := xlsBOF(0x0010)
		for r, row := range sh.Rows {
			last := 0
			for c, v := range row {
				if v != nil {
					last = c + 1
				}
			}
			s = append(s, xlsRowRec(uint16(r), 0, uint16(last))...)
			for c, v := range row {
				switch val := v.(type) {
				case string:
					s = append(s, xlsLabel(uint16(r), uint16(c), val)...)
				case float64:
					s = append(s, xlsNumber(uint16(r), uint16(c), val)...)
				}
			}
		}
		subs[i] = append(s, xlsEOF()...)
	}

	// Every BOUNDSHEET has a fixed length, so the globals size is known before
	// the offsets it contains are computed.
	size := len(xlsBOF(0x0005)) + len(xlsEOF())
	for _, sh := range sheets {
		size += len(xlsBoundSheet(0, sh.Name))
	}
	globals := xlsBOF(0x0005)
	pos := uint32(size)
	for i, sh := range sheets {
		globals = append(globals, xlsBoundSheet(pos, sh.Name)...)
		pos += uint32(len(subs[i]))
	}
	globals = append(globals, xlsEOF()...)

	book := globals
	for _, s := range subs {
		book = append(book, s...)
	}
	if len(book) > cfbStreamAlloc {
		t.Fatalf("fixture workbook is %d bytes, over the %d the CFB writer allocates", len(book), cfbStreamAlloc)
	}
	return buildCFB([]cfbStream{{name: "Workbook", data: book}})
}

// ---------------------------------------------------------------------------
// Accepts — hints
// ---------------------------------------------------------------------------

func TestXLSAcceptsHints(t *testing.T) {
	c := &XLSConverter{}
	empty := bytes.NewReader(nil)
	for _, tc := range []struct {
		name string
		info StreamInfo
		want bool
	}{
		{"xls extension", StreamInfo{Extension: ".xls"}, true},
		{"uppercase extension", StreamInfo{Extension: ".XLS"}, true},
		{"xlt template", StreamInfo{Extension: ".xlt"}, true},
		{"ms-excel mime", StreamInfo{MimeType: "application/vnd.ms-excel"}, true},
		{"ms-excel mime with parameters", StreamInfo{MimeType: "application/vnd.ms-excel; charset=binary"}, true},
		{"msg extension", StreamInfo{Extension: ".msg"}, false},
		{"doc extension", StreamInfo{Extension: ".doc"}, false},
		{"ppt extension", StreamInfo{Extension: ".ppt"}, false},
		{"xlsx extension", StreamInfo{Extension: ".xlsx"}, false},
		{"docx extension", StreamInfo{Extension: ".docx"}, false},
		{"no hint and no bytes", StreamInfo{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.Accepts(empty, tc.info, nil); got != tc.want {
				t.Fatalf("Accepts(%+v) = %v, want %v", tc.info, got, tc.want)
			}
		})
	}
}

// TestXLSAcceptsIsDisjointFromMsg is the test this converter exists to pass.
//
// The CFB magic is byte-for-byte shared with legacy .doc, legacy .ppt and
// Outlook .msg, and the engine treats accept-then-fail as a hard error — so a
// false accept here does not degrade those files, it breaks them permanently.
// Every case below is the SAME container with a different stream name.
func TestXLSAcceptsIsDisjointFromMsg(t *testing.T) {
	xls := &XLSConverter{}
	msg := &MsgConverter{}

	workbook := buildXLS(t, []xlsSheetSpec{{Name: "S", Rows: [][]any{{"a"}}}})
	if !xls.Accepts(bytes.NewReader(workbook), StreamInfo{}, nil) {
		t.Fatal("a bare workbook must be accepted on its Workbook stream alone")
	}
	if msg.Accepts(bytes.NewReader(workbook), StreamInfo{}, nil) {
		t.Fatal("the msg converter must not claim a workbook")
	}

	// A BIFF5/Excel 5 workbook names the stream "Book" instead.
	biff5 := buildCFB([]cfbStream{{name: "Book", data: xlsBOF(0x0005)}})
	if !xls.Accepts(bytes.NewReader(biff5), StreamInfo{}, nil) {
		t.Fatal("a BIFF5 Book stream must be accepted")
	}

	// A MAPI message: right magic, wrong content.
	mail := buildCFB([]cfbStream{
		{name: "__substg1.0_0037001F", data: utf16LE("Subject")},
		{name: "__substg1.0_1000001F", data: utf16LE("Body")},
	})
	if xls.Accepts(bytes.NewReader(mail), StreamInfo{}, nil) {
		t.Fatal("a MAPI message must be declined")
	}
	if !msg.Accepts(bytes.NewReader(mail), StreamInfo{}, nil) {
		t.Fatal("the msg converter must still claim its own file")
	}
	// And with the filename hint, which is the path the CLI actually takes.
	if xls.Accepts(bytes.NewReader(mail), StreamInfo{Extension: ".msg", FileName: "mail.msg"}, nil) {
		t.Fatal("a .msg must be declined by extension too")
	}

	// A legacy .doc: same magic, a WordDocument stream, no workbook.
	doc := buildCFB([]cfbStream{{name: "WordDocument", data: []byte("legacy word")}})
	if xls.Accepts(bytes.NewReader(doc), StreamInfo{}, nil) {
		t.Fatal("a legacy .doc must be declined")
	}

	// A legacy .ppt.
	ppt := buildCFB([]cfbStream{{name: "PowerPoint Document", data: []byte("legacy ppt")}})
	if xls.Accepts(bytes.NewReader(ppt), StreamInfo{}, nil) {
		t.Fatal("a legacy .ppt must be declined")
	}
}

// TestXLSAcceptsDeclinesEverythingElse feeds real containers of the formats
// that sit next to us through Accepts exactly as the engine would.
func TestXLSAcceptsDeclinesEverythingElse(t *testing.T) {
	c := &XLSConverter{}
	for _, tc := range []struct {
		name string
		body []byte
		info StreamInfo
	}{
		{"docx", xlsTestZip(t, "word/document.xml", "<w:document/>"),
			StreamInfo{Extension: ".docx", MimeType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document"}},
		{"docx without hints", xlsTestZip(t, "word/document.xml", "<w:document/>"), StreamInfo{}},
		{"xlsx", xlsTestZip(t, "xl/workbook.xml", "<workbook/>"),
			StreamInfo{Extension: ".xlsx", MimeType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"}},
		{"xlsx without hints", xlsTestZip(t, "xl/workbook.xml", "<workbook/>"), StreamInfo{}},
		{"random bytes", []byte("not a workbook, just some bytes going by"), StreamInfo{}},
		{"empty", nil, StreamInfo{}},
		{"bare magic", cfbMagic, StreamInfo{}},
		{"magic plus noise", append(append([]byte{}, cfbMagic...), bytes.Repeat([]byte{0xA5}, 4096)...), StreamInfo{}},
		{"plain text", []byte("Alpha,Beta\n1,2\n"), StreamInfo{Extension: ".csv", MimeType: "text/csv"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if c.Accepts(bytes.NewReader(tc.body), tc.info, nil) {
				t.Fatalf("%s must be declined", tc.name)
			}
		})
	}
}

// TestXLSAcceptsNeverPanics: Accepts sniffs hostile bytes, so fuzzing it for a
// panic is cheaper than reasoning about every truncation.
func TestXLSAcceptsNeverPanics(t *testing.T) {
	c := &XLSConverter{}
	f := func(b []byte) bool {
		// Prefixing the magic drives the expensive path rather than the
		// two-byte rejection that random bytes would take.
		body := append(append([]byte{}, cfbMagic...), b...)
		c.Accepts(bytes.NewReader(body), StreamInfo{}, nil)
		c.Accepts(bytes.NewReader(b), StreamInfo{}, nil)
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
}

// xlsTestZip builds a one-entry zip, so the OOXML declines are exercised
// against real containers rather than a hand-typed "PK" prefix.
func xlsTestZip(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// Convert
// ---------------------------------------------------------------------------

func TestXLSConvert(t *testing.T) {
	book := buildXLS(t, []xlsSheetSpec{{
		Name: "Sheet1",
		Rows: [][]any{
			{"Alpha", "Beta", "Gamma"},
			{float64(1), 2.5, "three"},
			{"x", nil, "z"},
		},
	}})
	res, err := (&XLSConverter{}).Convert(bytes.NewReader(book), StreamInfo{Extension: ".xls"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "## Sheet1\n\n" +
		"| Alpha | Beta | Gamma |\n" +
		"| --- | --- | --- |\n" +
		"| 1 | 2.5 | three |\n" +
		"| x |  | z |\n"
	if res.Markdown != want {
		t.Fatalf("markdown mismatch\ngot:\n%q\nwant:\n%q", res.Markdown, want)
	}
}

// TestXLSConvertMultiSheet pins sheet order and the "## Name" heading shape
// conv_xlsx.go also emits, so the two Excel paths stay indistinguishable.
func TestXLSConvertMultiSheet(t *testing.T) {
	book := buildXLS(t, []xlsSheetSpec{
		{Name: "First", Rows: [][]any{{"A"}, {"1"}}},
		{Name: "Second", Rows: [][]any{{"B", "C"}, {"2", "3"}}},
	})
	res, err := (&XLSConverter{}).Convert(bytes.NewReader(book), StreamInfo{Extension: ".xls"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "## First\n\n| A |\n| --- |\n| 1 |\n\n" +
		"## Second\n\n| B | C |\n| --- | --- |\n| 2 | 3 |\n"
	if res.Markdown != want {
		t.Fatalf("markdown mismatch\ngot:\n%q\nwant:\n%q", res.Markdown, want)
	}
}

// TestXLSConvertTrimsTrailingBlanks mirrors conv_xlsx.go's trimming: a trailing
// empty column never widens the table and trailing empty rows are dropped,
// while an interior blank survives as padding.
func TestXLSConvertTrimsTrailingBlanks(t *testing.T) {
	book := buildXLS(t, []xlsSheetSpec{{
		Name: "Trim",
		Rows: [][]any{
			{"H1", "H2", nil},
			{"a", nil, nil},
			{nil, nil, nil},
			{nil, nil, nil},
		},
	}})
	res, err := (&XLSConverter{}).Convert(bytes.NewReader(book), StreamInfo{Extension: ".xls"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "## Trim\n\n| H1 | H2 |\n| --- | --- |\n| a |  |\n"
	if res.Markdown != want {
		t.Fatalf("markdown mismatch\ngot:\n%q\nwant:\n%q", res.Markdown, want)
	}
}

// TestXLSConvertSkipsEmptySheets: a wholly empty sheet emits nothing at all,
// not a bare heading.
func TestXLSConvertSkipsEmptySheets(t *testing.T) {
	book := buildXLS(t, []xlsSheetSpec{
		{Name: "Blank", Rows: [][]any{{nil}, {nil}}},
		{Name: "Data", Rows: [][]any{{"only"}}},
	})
	res, err := (&XLSConverter{}).Convert(bytes.NewReader(book), StreamInfo{Extension: ".xls"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "## Data\n\n| only |\n| --- |\n"
	if res.Markdown != want {
		t.Fatalf("markdown mismatch\ngot:\n%q\nwant:\n%q", res.Markdown, want)
	}
}

// TestXLSConvertEscapesPipes: a cell containing a pipe must not break out of
// the table. mdutil owns the rule; this proves the xls path goes through it.
func TestXLSConvertEscapesPipes(t *testing.T) {
	book := buildXLS(t, []xlsSheetSpec{{
		Name: "Pipes",
		Rows: [][]any{{"a|b"}, {"c"}},
	}})
	res, err := (&XLSConverter{}).Convert(bytes.NewReader(book), StreamInfo{Extension: ".xls"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "## Pipes\n\n| a\\|b |\n| --- |\n| c |\n"
	if res.Markdown != want {
		t.Fatalf("markdown mismatch\ngot:\n%q\nwant:\n%q", res.Markdown, want)
	}
}

// TestXLSConvertRejectsGarbage: malformed input is an error, never a panic and
// never invented content.
func TestXLSConvertRejectsGarbage(t *testing.T) {
	c := &XLSConverter{}
	full := buildXLS(t, []xlsSheetSpec{{Name: "S", Rows: [][]any{{"a"}}}})
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"empty", nil},
		{"magic only", cfbMagic},
		{"magic plus noise", append(append([]byte{}, cfbMagic...), bytes.Repeat([]byte{0xA5}, 2048)...)},
		{"truncated container", full[:600]},
		{"header only", full[:512]},
		{"container without a workbook", buildCFB([]cfbStream{{name: "WordDocument", data: []byte("nope")}})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.Convert(bytes.NewReader(tc.body), StreamInfo{Extension: ".xls"}, nil); err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
}

// TestXLSThroughEngine proves dispatch actually lands here — with hints and,
// more importantly, without any.
func TestXLSThroughEngine(t *testing.T) {
	book := buildXLS(t, []xlsSheetSpec{{Name: "E2E", Rows: [][]any{{"h"}, {"v"}}}})
	want := "## E2E\n\n| h |\n| --- |\n| v |\n"

	res, err := ConvertBytes(book, StreamInfo{Extension: ".xls", FileName: "book.xls"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Markdown != want {
		t.Fatalf("markdown mismatch\ngot:\n%q\nwant:\n%q", res.Markdown, want)
	}

	res, err = ConvertBytes(book, StreamInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Markdown != want {
		t.Fatalf("bare stream did not dispatch to the xls converter:\ngot:\n%q\nwant:\n%q", res.Markdown, want)
	}
}

// TestXLSEngineStillConvertsMsg: registering this converter must not steal a
// .msg from MsgConverter, in either dispatch order.
func TestXLSEngineStillConvertsMsg(t *testing.T) {
	mail := buildCFB([]cfbStream{
		{name: "__substg1.0_0037001F", data: utf16LE("Test Subject")},
		{name: "__substg1.0_1000001F", data: utf16LE("Hello there")},
	})
	for _, info := range []StreamInfo{
		{Extension: ".msg", FileName: "mail.msg"},
		{},
	} {
		res, err := ConvertBytes(mail, info)
		if err != nil {
			t.Fatalf("info=%+v: %v", info, err)
		}
		if !strings.Contains(res.Markdown, "Test Subject") || !strings.Contains(res.Markdown, "Hello there") {
			t.Fatalf("info=%+v: msg was hijacked, got %q", info, res.Markdown)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// TestXLSRowCellsNilRow: WorkSheet.Row hands back nil (or panics) for a gap in
// a sparse sheet, and neither may reach the grid builder.
func TestXLSRowCellsNilRow(t *testing.T) {
	if got := xlsRowCells(nil); got != nil {
		t.Fatalf("xlsRowCells(nil) = %v, want nil", got)
	}
}

// TestXLSHasBookStreamShortInput: the probe must answer false, not panic, for
// anything too small to hold a directory.
func TestXLSHasBookStreamShortInput(t *testing.T) {
	for _, n := range []int{0, 1, 8, 64, 511, 512, 513} {
		body := bytes.Repeat([]byte{0xD0}, n)
		if xlsHasBookStream(bytes.NewReader(body)) {
			t.Fatalf("%d bytes of noise must not look like a workbook", n)
		}
	}
}

// TestXLSSplitsRegionsLikeXlsx: the legacy path runs the same region splitter,
// because the invariant this package cares about is that `anymd book.xls` and
// `anymd book.xlsx` are the same bytes for the same data. Splitting on blank
// rows and columns in only one of them would break that outright.
func TestXLSSplitsRegionsLikeXlsx(t *testing.T) {
	book := buildXLS(t, []xlsSheetSpec{{
		Name: "S",
		Rows: [][]any{
			{"Title", nil, nil},
			{nil, nil, nil},
			{"H1", nil, "R1"},
			{"a", nil, "b"},
		},
	}})
	res, err := (&XLSConverter{}).Convert(bytes.NewReader(book), StreamInfo{Extension: ".xls"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "## S\n\n" +
		"| Title |\n| --- |\n\n" +
		"| H1 |\n| --- |\n| a |\n\n" +
		"| R1 |\n| --- |\n| b |\n"
	if res.Markdown != want {
		t.Fatalf("markdown mismatch\ngot:\n%q\nwant:\n%q", res.Markdown, want)
	}
}

// TestXLSMatchesXlsxByteForByte is the invariant itself, exercised on the same
// layout through both converters: a sheet with two blocks separated by a blank
// row, built once as BIFF and once as OOXML.
func TestXLSMatchesXlsxByteForByte(t *testing.T) {
	rows := [][]string{
		{"Header", "Second"},
		{"1", "2"},
		{"", ""},
		{"Note", ""},
	}
	anyRows := make([][]any, len(rows))
	for i, r := range rows {
		anyRows[i] = make([]any, len(r))
		for j, v := range r {
			if v != "" {
				anyRows[i][j] = v
			}
		}
	}

	book := buildXLS(t, []xlsSheetSpec{{Name: "Sheet1", Rows: anyRows}})
	fromXLS, err := (&XLSConverter{}).Convert(bytes.NewReader(book), StreamInfo{Extension: ".xls"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fromXlsx := xlsxConvert(t, xlsxSheet{name: "Sheet1", rows: xlsxRowsXML(rows)})
	if fromXLS.Markdown != fromXlsx {
		t.Fatalf("the two Excel paths disagree\n xls: %q\nxlsx: %q", fromXLS.Markdown, fromXlsx)
	}
}
