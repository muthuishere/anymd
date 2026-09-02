package anymd

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

// xlsxSheet is one sheet of a hand-built fixture workbook: a name plus the raw
// <row> elements of its sheetData, optionally the raw <mergeCell> elements and
// the workbook-level visibility state.
type xlsxSheet struct {
	name   string
	rows   string
	merges string
	state  string
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
		state := ""
		if s.state != "" {
			state = fmt.Sprintf(` state="%s"`, s.state)
		}
		wbSheets.WriteString(fmt.Sprintf(`<sheet name="%s" sheetId="%d"%s r:id="rId%d"/>`, s.name, n, state, n))
		rels.WriteString(fmt.Sprintf(`<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, n, n))
		merges := ""
		if s.merges != "" {
			merges = `<mergeCells>` + s.merges + `</mergeCells>`
		}
		add(fmt.Sprintf("xl/worksheets/sheet%d.xml", n),
			`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`+
				`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`+
				`<sheetData>`+s.rows+`</sheetData>`+merges+`</worksheet>`)
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
		xlsxSheet{name: "Data", rows: `<row r="1">` +
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
		xlsxSheet{name: "Empty"},
		xlsxSheet{name: "Notes", rows: `<row r="1"><c r="A1" t="inlineStr"><is><t>note</t></is></c></row>` +
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
	data := buildXlsx(t, xlsxSheet{name: "One"}, xlsxSheet{name: "Two"})
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
	if _, err := xlsxSheetModel(f, "Sheet1", &total); err == nil {
		t.Error("xlsxSheetModel past the cell cap returned nil error, want an error")
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

// xlsxRowsXML turns a rectangular literal into sheetData rows. An empty string
// means an empty cell, which is exactly what the region splitter reads as a
// gap, so the fixtures below can be written as pictures of the sheet.
func xlsxRowsXML(rows [][]string) string {
	var sb strings.Builder
	for r, row := range rows {
		sb.WriteString(fmt.Sprintf(`<row r="%d">`, r+1))
		for c, v := range row {
			if v == "" {
				continue
			}
			name, err := excelize.CoordinatesToCellName(c+1, r+1)
			if err != nil {
				continue
			}
			sb.WriteString(fmt.Sprintf(`<c r="%s" t="inlineStr"><is><t>%s</t></is></c>`, name, v))
		}
		sb.WriteString(`</row>`)
	}
	return sb.String()
}

func xlsxConvert(t *testing.T, sheets ...xlsxSheet) string {
	t.Helper()
	res, err := New().ConvertBytes(buildXlsx(t, sheets...), StreamInfo{Extension: ".xlsx"}, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	return res.Markdown
}

// TestXlsxSplitsOnBlankRows is the defect `xlsx_07_gap_tolerance_.xlsx` names:
// a sheet holding a title block above a data grid is two tables, not one. Dumped
// as a single table the header row is the title and every data column is
// misaligned, which is how that file scored 0.11 on the table metric.
func TestXlsxSplitsOnBlankRows(t *testing.T) {
	got := xlsxConvert(t, xlsxSheet{name: "S", rows: xlsxRowsXML([][]string{
		{"Title"},
		{""},
		{"H1", "H2"},
		{"1", "2"},
	})})
	want := "## S\n\n" +
		"| Title |\n| --- |\n\n" +
		"| H1 | H2 |\n| --- | --- |\n| 1 | 2 |\n"
	if got != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", got, want)
	}
}

// TestXlsxSplitsOnBlankColumns: two grids side by side are two tables, and they
// come out in reading order.
func TestXlsxSplitsOnBlankColumns(t *testing.T) {
	got := xlsxConvert(t, xlsxSheet{name: "S", rows: xlsxRowsXML([][]string{
		{"L1", "", "R1"},
		{"L2", "", "R2"},
	})})
	want := "## S\n\n" +
		"| L1 |\n| --- |\n| L2 |\n\n" +
		"| R1 |\n| --- |\n| R2 |\n"
	if got != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", got, want)
	}
}

// TestXlsxDiagonalBlocksDoNotConnect pins the connectivity rule: cells touching
// only at a corner belong to different tables. Eight-way adjacency would weld
// a staircase of unrelated blocks into one grid.
func TestXlsxDiagonalBlocksDoNotConnect(t *testing.T) {
	got := xlsxConvert(t, xlsxSheet{name: "S", rows: xlsxRowsXML([][]string{
		{"a", "b", "", ""},
		{"", "", "c", "d"},
	})})
	want := "## S\n\n" +
		"| a | b |\n| --- | --- |\n\n" +
		"| c | d |\n| --- | --- |\n"
	if got != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", got, want)
	}
}

// TestXlsxInteriorGapStaysInsideOneTable records the tolerance chosen: ONE
// blank row or column ends a table, because Excel authors separate blocks by a
// single row far more often than they leave a hole inside one. A gap that is
// genuinely interior — surrounded by cells of the same block — is filled out by
// the region's bounding box and survives as an empty cell.
func TestXlsxInteriorGapStaysInsideOneTable(t *testing.T) {
	got := xlsxConvert(t, xlsxSheet{name: "S", rows: xlsxRowsXML([][]string{
		{"H1", "H2"},
		{"a", ""},
		{"c", "b"},
	})})
	want := "## S\n\n| H1 | H2 |\n| --- | --- |\n| a |  |\n| c | b |\n"
	if got != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", got, want)
	}
}

// TestXlsxMergedCellRepeatsAcrossItsSpan: markdown has no colspan, so the only
// faithful rendering of a merged cell is its text in every column it covers.
func TestXlsxMergedCellRepeatsAcrossItsSpan(t *testing.T) {
	got := xlsxConvert(t, xlsxSheet{
		name:   "S",
		rows:   xlsxRowsXML([][]string{{"M", "x"}, {"", "y"}}),
		merges: `<mergeCell ref="A1:A2"/>`,
	})
	want := "## S\n\n| M | x |\n| --- | --- |\n| M | y |\n"
	if got != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", got, want)
	}
}

// TestXlsxMergedBlankRangeIsStillContent: a merge with no value is the author
// saying those cells belong to the block, so it keeps the region together and
// contributes its rows.
func TestXlsxMergedBlankRangeIsStillContent(t *testing.T) {
	got := xlsxConvert(t, xlsxSheet{
		name:   "S",
		rows:   xlsxRowsXML([][]string{{"H"}}),
		merges: `<mergeCell ref="A2:B2"/>`,
	})
	want := "## S\n\n| H |  |\n| --- | --- |\n|  |  |\n"
	if got != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", got, want)
	}
}

// TestXlsxSectionLabelBecomesText: a single merged cell spanning the width of
// the block above a header row is a section title, not a one-cell first row.
// Leaving it in the table shifts every header one row down.
func TestXlsxSectionLabelBecomesText(t *testing.T) {
	got := xlsxConvert(t, xlsxSheet{
		name: "S",
		rows: xlsxRowsXML([][]string{
			{"Reading List", "", ""},
			{"#", "Title", "Author"},
			{"1", "Dune", "Herbert"},
		}),
		merges: `<mergeCell ref="A1:C1"/>`,
	})
	want := "## S\n\nReading List\n\n" +
		"| # | Title | Author |\n| --- | --- | --- |\n| 1 | Dune | Herbert |\n"
	if got != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", got, want)
	}
}

// TestXlsxUnmergedFirstRowIsNotASectionLabel guards the narrowness of the rule:
// a one-cell first row that is not merged across the block is data.
func TestXlsxUnmergedFirstRowIsNotASectionLabel(t *testing.T) {
	got := xlsxConvert(t, xlsxSheet{name: "S", rows: xlsxRowsXML([][]string{
		{"Label", ""},
		{"H1", "H2"},
		{"1", "2"},
	})})
	want := "## S\n\n| Label |  |\n| --- | --- |\n| H1 | H2 |\n| 1 | 2 |\n"
	if got != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", got, want)
	}
}

// TestXlsxHiddenSheetIsSkipped: a sheet the workbook hides is not part of the
// document a reader sees, and emitting it invents content.
func TestXlsxHiddenSheetIsSkipped(t *testing.T) {
	got := xlsxConvert(t,
		xlsxSheet{name: "Visible", rows: xlsxRowsXML([][]string{{"a"}})},
		xlsxSheet{name: "Gone", rows: xlsxRowsXML([][]string{{"b"}}), state: "hidden"},
	)
	want := "## Visible\n\n| a |\n| --- |\n"
	if got != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", got, want)
	}
}

// TestXlsxChartBlocks covers the scatter shape specifically: a scatter series
// states its categories and values as c:xVal / c:yVal, and a parser that only
// knows c:cat / c:val renders it as a title with no data at all.
func TestXlsxChartBlocks(t *testing.T) {
	const head = `<c:chartSpace xmlns:c="http://schemas.openxmlformats.org/drawingml/2006/chart" ` +
		`xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><c:chart>` +
		`<c:title><c:tx><c:rich><a:p><a:r><a:t>Sales</a:t></a:r></a:p></c:rich></c:tx></c:title><c:plotArea>`
	const tail = `</c:plotArea></c:chart></c:chartSpace>`
	pts := func(vals ...string) string {
		var sb strings.Builder
		for i, v := range vals {
			sb.WriteString(fmt.Sprintf(`<c:pt idx="%d"><c:v>%s</c:v></c:pt>`, i, v))
		}
		return sb.String()
	}

	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "bar chart from cat and val",
			body: `<c:barChart><c:ser>` +
				`<c:tx><c:strRef><c:strCache>` + pts("S1") + `</c:strCache></c:strRef></c:tx>` +
				`<c:cat><c:strRef><c:strCache>` + pts("2019", "2020") + `</c:strCache></c:strRef></c:cat>` +
				`<c:val><c:numRef><c:numCache>` + pts("1", "2") + `</c:numCache></c:numRef></c:val>` +
				`</c:ser></c:barChart>`,
			want: []string{"Sales", "Bar chart",
				"|  | S1 |\n| --- | --- |\n| 2019 | 1 |\n| 2020 | 2 |"},
		},
		{
			name: "scatter chart from xVal and yVal",
			body: `<c:scatterChart><c:ser>` +
				`<c:tx><c:strRef><c:strCache>` + pts("S1") + `</c:strCache></c:strRef></c:tx>` +
				`<c:xVal><c:numRef><c:numCache>` + pts("1", "2") + `</c:numCache></c:numRef></c:xVal>` +
				`<c:yVal><c:numRef><c:numCache>` + pts("10", "20") + `</c:numCache></c:numRef></c:yVal>` +
				`</c:ser></c:scatterChart>`,
			want: []string{"Sales", "Scatter chart",
				"|  | S1 |\n| --- | --- |\n| 1 | 10 |\n| 2 | 20 |"},
		},
		{
			name: "unknown plot type still names itself",
			body: `<c:areaChart><c:ser>` +
				`<c:val><c:numRef><c:numCache>` + pts("7") + `</c:numCache></c:numRef></c:val>` +
				`</c:ser></c:areaChart>`,
			want: []string{"Sales", "Other chart", "|  |  |\n| --- | --- |\n|  | 7 |"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := xlsxChartBlocks([]byte(head + tt.body + tail))
			if strings.Join(got, "\n---\n") != strings.Join(tt.want, "\n---\n") {
				t.Errorf("blocks =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

// TestXlsxDrawingCharts pins the anchor row, which is what orders a chart
// against the tables on the same sheet.
func TestXlsxDrawingCharts(t *testing.T) {
	const drawing = `<xdr:wsDr xmlns:xdr="http://schemas.openxmlformats.org/drawingml/2006/spreadsheetDrawing" ` +
		`xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" ` +
		`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
		`<xdr:twoCellAnchor><xdr:from><xdr:col>4</xdr:col><xdr:row>21</xdr:row></xdr:from>` +
		`<xdr:to><xdr:col>9</xdr:col><xdr:row>35</xdr:row></xdr:to>` +
		`<xdr:graphicFrame><a:graphic><a:graphicData>` +
		`<c:chart xmlns:c="http://schemas.openxmlformats.org/drawingml/2006/chart" r:id="rId1"/>` +
		`</a:graphicData></a:graphic></xdr:graphicFrame></xdr:twoCellAnchor>` +
		`<xdr:oneCellAnchor><xdr:from><xdr:col>0</xdr:col><xdr:row>4</xdr:row></xdr:from>` +
		`<xdr:graphicFrame><a:graphic><a:graphicData>` +
		`<c:chart xmlns:c="http://schemas.openxmlformats.org/drawingml/2006/chart" r:id="rId2"/>` +
		`</a:graphicData></a:graphic></xdr:graphicFrame></xdr:oneCellAnchor></xdr:wsDr>`

	got := xlsxDrawingCharts([]byte(drawing))
	want := []xlsxDrawingAnchor{{row: 21, relID: "rId1"}, {row: 4, relID: "rId2"}}
	if len(got) != len(want) {
		t.Fatalf("anchors = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("anchor %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestXlsxLegacyComments: an author plus a note, and Excel's compatibility
// boilerplate for a threaded note stripped back to the note itself.
func TestXlsxLegacyComments(t *testing.T) {
	const data = `<comments xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
		`<authors><author>John Reviewer</author><author>tc={ABC}</author></authors>` +
		`<commentList>` +
		`<comment ref="A1" authorId="0"><text><r><t>Why Python?</t></r></text></comment>` +
		`<comment ref="C3" authorId="1"><text><t>[Threaded comment]` + "\n\nblurb\n\nComment:\n    Minimum ducks" +
		`</t></text></comment>` +
		`</commentList></comments>`

	got := xlsxLegacyComments([]byte(data))
	want := []xlsxComment{
		{row: 0, col: 0, author: "John Reviewer", text: "Why Python?"},
		{row: 2, col: 2, author: "Threaded comment", text: "Minimum ducks"},
	}
	if len(got) != len(want) {
		t.Fatalf("comments = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("comment %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestXlsxThreadedCommentsLastReplyWins: a thread is stored as a parent plus
// replies against one cell, and the last one is the current state of it.
func TestXlsxThreadedCommentsLastReplyWins(t *testing.T) {
	const persons = `<personList xmlns="http://schemas.microsoft.com/office/spreadsheetml/2018/threadedcomments">` +
		`<person displayName="Jane Smith (JS)" id="{P1}"/><person displayName="Marcus Sterling (MS)" id="{P2}"/>` +
		`</personList>`
	const threaded = `<ThreadedComments xmlns="http://schemas.microsoft.com/office/spreadsheetml/2018/threadedcomments">` +
		`<threadedComment ref="F7" dT="2026-06-18T17:12:37.41" personId="{P1}"><text>Minimum ducks</text></threadedComment>` +
		`<threadedComment ref="F7" dT="2026-06-18T17:15:52.31" personId="{P2}"><text>So low</text></threadedComment>` +
		`</ThreadedComments>`

	var people map[string]string
	var doc struct {
		People []struct {
			ID   string `xml:"id,attr"`
			Name string `xml:"displayName,attr"`
		} `xml:"person"`
	}
	if err := xml.Unmarshal([]byte(persons), &doc); err != nil {
		t.Fatal(err)
	}
	people = map[string]string{}
	for _, p := range doc.People {
		people[p.ID] = p.Name
	}

	got := xlsxThreadedComments([]byte(threaded), people)
	if len(got) != 2 {
		t.Fatalf("comments = %+v, want 2", got)
	}
	last := got[len(got)-1]
	want := xlsxComment{row: 6, col: 5, author: "Marcus Sterling (MS)",
		when: "2026-06-18T17:15:52.310", text: "So low"}
	if last != want {
		t.Errorf("last comment = %+v, want %+v", last, want)
	}
}

func TestXlsxCommentTime(t *testing.T) {
	tests := []struct{ in, want string }{
		{"2026-06-18T17:15:52.31", "2026-06-18T17:15:52.310"},
		{"2026-06-18T17:15:52", "2026-06-18T17:15:52.000"},
		{"2026-06-18T17:15:52.987654", "2026-06-18T17:15:52.987"},
		{"2026-06-18T17:15:52Z", "2026-06-18T17:15:52.000+00:00"},
		{"2026-06-18T17:15:52.5+05:30", "2026-06-18T17:15:52.500+05:30"},
		{"", ""},
		{"not a time", ""},
		{"2026-06-18", ""},
		{"2026-06-18T17:15:52.abc", ""},
	}
	for _, tt := range tests {
		if got := xlsxCommentTime(tt.in); got != tt.want {
			t.Errorf("xlsxCommentTime(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
