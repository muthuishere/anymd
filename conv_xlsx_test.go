package anymd

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

// xlsxSheet is one sheet of a hand-built fixture workbook: a name plus the raw
// <row> elements of its sheetData.
type xlsxSheet struct {
	name string
	rows string
}

// buildXlsx writes a minimal but real .xlsx in memory. The parts are written by
// hand rather than by excelize because excelize's writer normalizes away the
// two things these tests must exercise: a formula cell carrying a cached value,
// and a stray styled cell far outside the real used range.
func buildXlsx(t *testing.T, sheets ...xlsxSheet) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}

	var types, wbSheets, rels strings.Builder
	types.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
		`<Default Extension="xml" ContentType="application/xml"/>` +
		`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
		`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>`)
	rels.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rIdStyles" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>`)
	for i, s := range sheets {
		n := i + 1
		types.WriteString(fmt.Sprintf(`<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, n))
		wbSheets.WriteString(fmt.Sprintf(`<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, s.name, n, n))
		rels.WriteString(fmt.Sprintf(`<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, n, n))
		add(fmt.Sprintf("xl/worksheets/sheet%d.xml", n),
			`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`+
				`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`+
				`<sheetData>`+s.rows+`</sheetData></worksheet>`)
	}
	types.WriteString(`</Types>`)
	rels.WriteString(`</Relationships>`)

	add("[Content_Types].xml", types.String())
	add("_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`+
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`+
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>`+
		`</Relationships>`)
	add("xl/workbook.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`+
		`<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" `+
		`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">`+
		`<sheets>`+wbSheets.String()+`</sheets></workbook>`)
	add("xl/_rels/workbook.xml.rels", rels.String())
	// cellXfs index 1 carries numFmtId 14 (a short date), so B2 renders as a
	// date rather than as the serial number 45356.
	add("xl/styles.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`+
		`<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`+
		`<fonts count="1"><font/></fonts><fills count="1"><fill/></fills>`+
		`<borders count="1"><border/></borders>`+
		`<cellStyleXfs count="1"><xf/></cellStyleXfs>`+
		`<cellXfs count="2"><xf/><xf numFmtId="14" applyNumberFormat="1"/></cellXfs>`+
		`</styleSheet>`)

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// multiSheetFixture has a date, a formula with a cached value, an entirely
// empty sheet, and a stray styled cell at ZZ1000.
func multiSheetFixture(t *testing.T) []byte {
	t.Helper()
	return buildXlsx(t,
		xlsxSheet{"Data", `<row r="1">` +
			`<c r="A1" t="inlineStr"><is><t>Name</t></is></c>` +
			`<c r="B1" t="inlineStr"><is><t>When</t></is></c>` +
			`<c r="C1" t="inlineStr"><is><t>Total</t></is></c>` +
			`</row>` +
			`<row r="2">` +
			`<c r="A2" t="inlineStr"><is><t>alpha</t></is></c>` +
			`<c r="B2" s="1"><v>45356</v></c>` +
			`<c r="C2"><f>D2+E2</f><v>5</v></c>` +
			`</row>` +
			`<row r="1000"><c r="ZZ1000" s="1"/></row>`},
		xlsxSheet{"Empty", ``},
		xlsxSheet{"Notes", `<row r="1"><c r="A1" t="inlineStr"><is><t>note</t></is></c></row>` +
			`<row r="2"><c r="A2" t="inlineStr"><is><t>pipes | here</t></is></c></row>`},
	)
}

func TestXlsxConvertMultiSheet(t *testing.T) {
	data := multiSheetFixture(t)
	res, err := New().ConvertBytes(data, StreamInfo{Extension: ".xlsx", FileName: "book.xlsx"}, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	want := "## Data\n" +
		"\n" +
		"| Name | When | Total |\n" +
		"| --- | --- | --- |\n" +
		"| alpha | 03-05-24 | 5 |\n" +
		"\n" +
		"## Notes\n" +
		"\n" +
		"| note |\n" +
		"| --- |\n" +
		"| pipes \\| here |\n"
	if res.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
}

func TestXlsxAcceptsAndRejects(t *testing.T) {
	c := &XlsxConverter{}
	data := multiSheetFixture(t)

	// Bare stream, no hints at all: the zip magic plus xl/workbook.xml must
	// carry the decision.
	if !c.Accepts(bytes.NewReader(data), StreamInfo{}, nil) {
		t.Error("Accepts(bare xlsx) = false, want true")
	}
	if !c.Accepts(bytes.NewReader(nil), StreamInfo{Extension: ".xlsx"}, nil) {
		t.Error("Accepts(ext hint) = false, want true")
	}
	// A PK zip that is not a workbook must be declined, or it would shadow the
	// docx/pptx converters.
	var other bytes.Buffer
	zw := zip.NewWriter(&other)
	w, _ := zw.Create("word/document.xml")
	w.Write([]byte("<x/>"))
	zw.Close()
	if c.Accepts(bytes.NewReader(other.Bytes()), StreamInfo{}, nil) {
		t.Error("Accepts(non-workbook zip) = true, want false")
	}
	if c.Accepts(strings.NewReader("hello, world"), StreamInfo{}, nil) {
		t.Error("Accepts(plain text) = true, want false")
	}
}

// TestXlsxEmptyWorkbook checks that a workbook whose every sheet is empty
// produces no markdown at all rather than a run of bare headings.
func TestXlsxEmptyWorkbook(t *testing.T) {
	data := buildXlsx(t, xlsxSheet{"One", ``}, xlsxSheet{"Two", ``})
	res, err := New().ConvertBytes(data, StreamInfo{Extension: ".xlsx"}, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if res.Markdown != "" {
		t.Errorf("markdown = %q, want empty", res.Markdown)
	}
}

// TestXlsxCellCap proves the guard fires instead of building a giant table.
func TestXlsxCellCap(t *testing.T) {
	// Start the running total one cell below the cap, so the very first sheet
	// crosses it: the guard must surface as an error, not as a giant table.
	total := maxXlsxCells - 1
	f := excelize.NewFile()
	defer f.Close()
	for r := 1; r <= 3; r++ {
		for c := 1; c <= 2; c++ {
			name, _ := excelize.CoordinatesToCellName(c, r)
			f.SetCellValue("Sheet1", name, "v")
		}
	}
	if _, _, err := sheetGrid(f, "Sheet1", &total); err == nil {
		t.Error("sheetGrid past the cell cap returned nil error, want an error")
	}
}

func TestXlsxMalformedIsAnError(t *testing.T) {
	c := &XlsxConverter{}
	// Zip magic with nothing behind it: must error, never panic.
	bad := append([]byte{'P', 'K', 3, 4}, bytes.Repeat([]byte{0}, 64)...)
	if _, err := c.Convert(bytes.NewReader(bad), StreamInfo{Extension: ".xlsx"}, nil); err == nil {
		t.Error("Convert(garbage) = nil error, want an error")
	}
}
