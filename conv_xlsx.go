package anymd

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/muthuishere/anymd/internal/mdutil"
	"github.com/muthuishere/anymd/internal/ooxml"
	"github.com/xuri/excelize/v2"
)

func init() { addBuiltin(&XlsxConverter{}) }

// maxXlsxCells bounds how many cells a workbook may emit. A crafted (or merely
// careless) sheet can claim a used range of millions of cells; materializing
// that as markdown would exhaust memory long before it would help a reader.
const maxXlsxCells = 1_000_000

// maxXlsxSniffBytes bounds the buffering done by the Accepts-time zip probe
// when the stream is not already a ReaderAt.
const maxXlsxSniffBytes = 64 << 20

// maxXlsxMerges bounds the merged-range list a sheet may declare. The list is
// read straight out of the file, so it is attacker-controlled.
const maxXlsxMerges = 1 << 16

// XlsxConverter renders an OOXML workbook as GFM tables, in the workbook's own
// sheet order, each sheet under an "## SheetName" heading.
//
// A sheet is not one table. Spreadsheets are laid out visually, and a single
// sheet routinely holds several unrelated blocks separated by blank rows or
// blank columns — a title block, a revision block, a data grid beside a legend.
// Dumping the whole used range as one table welds them together and destroys
// the row/column alignment of every one of them. So the sheet is split into
// 4-connected regions of non-empty cells and each region becomes its own table
// with its own header row, which is also what docling's ground truth expects.
//
// Values are rendered as displayed rather than as stored: dates come out as
// dates instead of serial numbers, and a formula cell emits its cached result.
// Charts anchored on a sheet contribute their title, type and cached series
// data, and cell comments are appended after the sheet's tables.
type XlsxConverter struct{}

// Name identifies the converter in errors and in `anymd --list`.
func (c *XlsxConverter) Name() string { return "xlsx" }

// Accepts recognizes a workbook from hints, or from the zip magic plus an
// "xl/workbook.xml" entry — the cheapest sniff that distinguishes an xlsx from
// every other PK-prefixed container (docx, pptx, jar, epub).
func (c *XlsxConverter) Accepts(r io.ReadSeeker, info StreamInfo, opts *Options) bool {
	if info.HasExt(".xlsx", ".xlsm", ".xltx", ".xltm") {
		return true
	}
	switch info.NormalizedMime() {
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.ms-excel.sheet.macroenabled.12",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.template":
		return true
	}
	// Decline other office containers outright: they are PK zips too, and their
	// own converters must win.
	if info.HasExt(".docx", ".pptx", ".epub", ".zip", ".jar", ".odt", ".ods") {
		return false
	}
	var head [4]byte
	if n, _ := io.ReadFull(r, head[:]); n != 4 || !bytes.Equal(head[:], []byte{'P', 'K', 3, 4}) {
		return false
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return false
	}
	return xlsxZipHasEntry(r, "xl/workbook.xml")
}

// xlsxZipHasEntry reports whether a zip stream contains the named entry,
// reading only the central directory. It never inflates anything.
func xlsxZipHasEntry(r io.ReadSeeker, name string) (found bool) {
	defer func() {
		// archive/zip is careful, but this runs against hostile bytes and the
		// contract forbids a panic escaping a converter.
		if recover() != nil {
			found = false
		}
	}()
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil || size <= 0 {
		return false
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return false
	}
	ra, ok := r.(io.ReaderAt)
	if !ok {
		b, err := io.ReadAll(io.LimitReader(r, maxXlsxSniffBytes))
		if err != nil {
			return false
		}
		ra, size = bytes.NewReader(b), int64(len(b))
	}
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return false
	}
	for _, f := range zr.File {
		if f.Name == name {
			return true
		}
	}
	return false
}

// Convert renders every visible, non-empty sheet.
func (c *XlsxConverter) Convert(r io.ReadSeeker, info StreamInfo, opts *Options) (res Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("malformed workbook: %v", p)
		}
	}()

	// The archive is opened twice on purpose: excelize models cells, and the
	// parts that carry charts and comments are not reachable through its API.
	// A failure here costs those enrichments, never the document, so the error
	// is deliberately dropped.
	pkg, _ := ooxml.Open(r)
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return Result{}, err
	}

	f, err := excelize.OpenReader(r)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()

	parts := xlsxSheetParts(pkg)

	var blocks []string
	total := 0
	for _, sheet := range f.GetSheetList() {
		part := parts[sheet]
		if part.hidden {
			continue // a hidden sheet is not part of the document a reader sees
		}
		model, err := xlsxSheetModel(f, sheet, &total)
		if err != nil {
			return Result{}, err
		}
		body := xlsxSheetBlocks(model, pkg, part.name)
		if len(body) == 0 {
			continue // an entirely empty sheet emits nothing, not a bare heading
		}
		blocks = append(blocks, mdutil.Heading(2, sheet))
		blocks = append(blocks, body...)
	}
	return Result{Markdown: mdutil.Join(blocks...)}, nil
}

// xlsxSheetPart is where a sheet's XML lives in the package, plus whether the
// workbook hides it.
type xlsxSheetPart struct {
	name   string
	hidden bool
}

// xlsxSheetParts maps sheet name to its part path and visibility by reading
// xl/workbook.xml and its relationships. excelize exposes neither the part
// path (needed to find a sheet's drawings and comments) nor a total ordering
// that survives chartsheets, so the workbook part is read directly.
func xlsxSheetParts(pkg *ooxml.Package) map[string]xlsxSheetPart {
	out := map[string]xlsxSheetPart{}
	if pkg == nil {
		return out
	}
	data := pkg.OptionalPart("xl/workbook.xml")
	if len(data) == 0 {
		return out
	}
	var doc struct {
		Sheets []struct {
			Name  string `xml:"name,attr"`
			State string `xml:"state,attr"`
			ID    string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
		} `xml:"sheets>sheet"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		return out
	}
	targets := pkg.RelTargets("xl/workbook.xml")
	for _, s := range doc.Sheets {
		if s.Name == "" {
			continue
		}
		state := strings.ToLower(strings.TrimSpace(s.State))
		out[s.Name] = xlsxSheetPart{
			name:   ooxml.ResolveTarget("xl/workbook.xml", targets[s.ID]),
			hidden: state == "hidden" || state == "veryhidden",
		}
	}
	return out
}

// xlsxModel is one sheet reduced to what the region splitter needs: the text a
// reader would see in each cell (a merged range repeats its anchor's text
// across the whole span, which is how a spanning cell has to render in a grid
// with no spans), plus which cells a merged range covers, and the span of each
// anchor.
type xlsxModel struct {
	cells  [][]string
	merged [][]bool
	spans  map[[2]int][2]int // anchor {row, col} -> {rowSpan, colSpan}
	rows   int
	cols   int
}

// content reports whether a cell participates in a table region. A cell inside
// a merged range counts even when the range is blank: the merge is the author
// saying those cells belong together.
func (m *xlsxModel) content(r, c int) bool {
	if r < 0 || c < 0 || r >= m.rows || c >= m.cols {
		return false
	}
	return m.cells[r][c] != "" || m.merged[r][c]
}

// shadow reports whether a cell is a non-anchor part of a merged range.
func (m *xlsxModel) shadow(r, c int) bool {
	if r < 0 || c < 0 || r >= m.rows || c >= m.cols || !m.merged[r][c] {
		return false
	}
	_, isAnchor := m.spans[[2]int{r, c}]
	return !isAnchor
}

// span returns the merged span anchored at a cell, or {1, 1}.
func (m *xlsxModel) span(r, c int) (int, int) {
	if s, ok := m.spans[[2]int{r, c}]; ok {
		return s[0], s[1]
	}
	return 1, 1
}

// xlsxSheetModel streams one sheet into a rectangular grid, trimming trailing
// empty rows and columns so a stray formatted cell at ZZ1000 does not become a
// 700-column table, then folds the merged ranges in. total accumulates cells
// across the workbook for the cap.
//
// A sheet excelize cannot iterate — a chartsheet, most often — yields an empty
// model rather than an error: it may still carry a chart worth rendering.
func xlsxSheetModel(f *excelize.File, sheet string, total *int) (*xlsxModel, error) {
	m := &xlsxModel{spans: map[[2]int][2]int{}}

	rows, err := f.Rows(sheet)
	if err != nil {
		return m, nil
	}
	defer rows.Close()

	var grid [][]string
	width, lastRow := 0, 0
	for rows.Next() {
		cols, err := rows.Columns()
		if err != nil {
			return nil, err
		}
		// Trim this row's trailing blanks before it can widen the table.
		end := len(cols)
		for end > 0 && strings.TrimSpace(cols[end-1]) == "" {
			end--
		}
		cols = cols[:end]
		if end > 0 {
			lastRow = len(grid) + 1
			if end > width {
				width = end
			}
		}
		*total += end
		if *total > maxXlsxCells {
			return nil, fmt.Errorf("workbook exceeds the %d-cell limit", maxXlsxCells)
		}
		grid = append(grid, cols)
	}
	if err := rows.Error(); err != nil {
		return nil, err
	}
	grid = grid[:lastRow]

	merges := xlsxMerges(f, sheet)
	// A merged range may reach past the last cell that carries a value; it is
	// still content, so the grid grows to hold it -- but only while that stays
	// within the cell cap, because the range's coordinates come from the file.
	height := len(grid)
	for _, mg := range merges {
		if mg.r1 >= height && int64(mg.r1+1)*int64(max(width, mg.c1+1)) <= maxXlsxCells {
			height = mg.r1 + 1
		}
		if mg.c1 >= width && int64(max(height, mg.r1+1))*int64(mg.c1+1) <= maxXlsxCells {
			width = mg.c1 + 1
		}
	}

	m.rows, m.cols = height, width
	m.cells = make([][]string, height)
	m.merged = make([][]bool, height)
	for i := 0; i < height; i++ {
		m.cells[i] = make([]string, width)
		m.merged[i] = make([]bool, width)
		if i < len(grid) {
			copy(m.cells[i], grid[i])
		}
	}

	for _, mg := range merges {
		if mg.r0 >= height || mg.c0 >= width {
			continue
		}
		r1, c1 := min(mg.r1, height-1), min(mg.c1, width-1)
		text := m.cells[mg.r0][mg.c0]
		m.spans[[2]int{mg.r0, mg.c0}] = [2]int{r1 - mg.r0 + 1, c1 - mg.c0 + 1}
		for r := mg.r0; r <= r1; r++ {
			for c := mg.c0; c <= c1; c++ {
				m.merged[r][c] = true
				m.cells[r][c] = text
			}
		}
	}
	return m, nil
}

// xlsxMergeRange is a merged range in 0-based, half-open-free coordinates.
type xlsxMergeRange struct{ r0, c0, r1, c1 int }

// xlsxMerges reads a sheet's merged ranges, bounded and normalized. Ranges the
// file states backwards or unparseably are dropped rather than trusted.
func xlsxMerges(f *excelize.File, sheet string) []xlsxMergeRange {
	raw, err := f.GetMergeCells(sheet)
	if err != nil {
		return nil
	}
	if len(raw) > maxXlsxMerges {
		raw = raw[:maxXlsxMerges]
	}
	out := make([]xlsxMergeRange, 0, len(raw))
	for _, mc := range raw {
		c0, r0, err := excelize.CellNameToCoordinates(mc.GetStartAxis())
		if err != nil {
			continue
		}
		c1, r1, err := excelize.CellNameToCoordinates(mc.GetEndAxis())
		if err != nil {
			continue
		}
		r0, c0, r1, c1 = r0-1, c0-1, r1-1, c1-1
		if r1 < r0 {
			r0, r1 = r1, r0
		}
		if c1 < c0 {
			c0, c1 = c1, c0
		}
		if r0 < 0 || c0 < 0 {
			continue
		}
		out = append(out, xlsxMergeRange{r0, c0, r1, c1})
	}
	return out
}

// xlsxRegion is the bounding box of one connected block of cells.
type xlsxRegion struct{ r0, c0, r1, c1 int }

// xlsxRegions splits a sheet into its 4-connected regions of non-empty cells,
// in reading order (the region containing the topmost, then leftmost, cell
// comes first).
//
// Diagonal adjacency deliberately does NOT connect: two grids touching only at
// a corner are two tables, which is the case `xlsx_06_edge_cases_.xlsx` names
// "Diagonal". A region's bounding box is filled out to a rectangle, so a hole
// or an L-shaped block still renders as a grid.
func xlsxRegions(m *xlsxModel) []xlsxRegion {
	if m.rows == 0 || m.cols == 0 {
		return nil
	}
	seen := make([][]bool, m.rows)
	for i := range seen {
		seen[i] = make([]bool, m.cols)
	}
	var out []xlsxRegion
	queue := make([][2]int, 0, 64)
	for r := 0; r < m.rows; r++ {
		for c := 0; c < m.cols; c++ {
			if seen[r][c] || !m.content(r, c) {
				continue
			}
			reg := xlsxRegion{r, c, r, c}
			seen[r][c] = true
			queue = append(queue[:0], [2]int{r, c})
			for len(queue) > 0 {
				cur := queue[len(queue)-1]
				queue = queue[:len(queue)-1]
				cr, cc := cur[0], cur[1]
				reg.r0, reg.r1 = min(reg.r0, cr), max(reg.r1, cr)
				reg.c0, reg.c1 = min(reg.c0, cc), max(reg.c1, cc)
				for _, d := range [4][2]int{{0, 1}, {0, -1}, {1, 0}, {-1, 0}} {
					nr, nc := cr+d[0], cc+d[1]
					if nr < 0 || nc < 0 || nr >= m.rows || nc >= m.cols {
						continue
					}
					if seen[nr][nc] || !m.content(nr, nc) {
						continue
					}
					seen[nr][nc] = true
					queue = append(queue, [2]int{nr, nc})
				}
			}
			out = append(out, reg)
		}
	}
	return out
}

// xlsxRegionBlocks renders one region: an optional section label lifted out of
// a merged first row, then the table.
func xlsxRegionBlocks(m *xlsxModel, reg xlsxRegion) []string {
	var blocks []string
	if title, ok := xlsxSectionLabel(m, reg); ok {
		blocks = append(blocks, title)
		reg.r0++
	}
	grid := make([][]string, 0, reg.r1-reg.r0+1)
	for r := reg.r0; r <= reg.r1; r++ {
		grid = append(grid, m.cells[r][reg.c0:reg.c1+1])
	}
	if len(grid) == 0 {
		return blocks
	}
	if t := mdutil.Table(grid[0], grid[1:]); t != "" {
		blocks = append(blocks, t)
	}
	return blocks
}

// xlsxSectionLabel recognizes the "one merged title cell sitting on top of a
// header row" idiom and returns the title, so it renders as a line of prose
// rather than as a one-cell first row that misaligns the whole table.
//
// The shape is deliberately narrow — a single merged cell starting at the
// region's left edge, above a row of at least two unmerged headers — because a
// wider rule would eat legitimate first rows.
func xlsxSectionLabel(m *xlsxModel, reg xlsxRegion) (string, bool) {
	rows, cols := reg.r1-reg.r0+1, reg.c1-reg.c0+1
	if rows < 2 || cols < 2 {
		return "", false
	}
	titleCol, found := -1, 0
	for c := reg.c0; c <= reg.c1; c++ {
		if m.shadow(reg.r0, c) || strings.TrimSpace(m.cells[reg.r0][c]) == "" {
			continue
		}
		found++
		titleCol = c
	}
	if found != 1 || titleCol != reg.c0 {
		return "", false
	}
	rowSpan, colSpan := m.span(reg.r0, titleCol)
	if rowSpan != 1 || colSpan <= 1 || colSpan > cols {
		return "", false
	}
	headers := 0
	for c := reg.c0; c <= reg.c1; c++ {
		if m.shadow(reg.r0+1, c) || strings.TrimSpace(m.cells[reg.r0+1][c]) == "" {
			continue
		}
		if _, cs := m.span(reg.r0+1, c); cs == 1 {
			headers++
		}
	}
	if headers < 2 {
		return "", false
	}
	return mdutil.Collapse(m.cells[reg.r0][titleCol]), true
}

// xlsxModelFromGrid wraps an already-materialized, rectangular grid so a
// converter that is not reading OOXML — conv_xls.go's legacy BIFF path — can
// use the same region splitter. Such a grid carries no merged ranges.
func xlsxModelFromGrid(grid [][]string, width int) *xlsxModel {
	m := &xlsxModel{cells: grid, rows: len(grid), cols: width, spans: map[[2]int][2]int{}}
	m.merged = make([][]bool, len(grid))
	for i := range m.merged {
		m.merged[i] = make([]bool, width)
	}
	return m
}

// xlsxGridBlocks renders every region of a sheet, in reading order, with no
// charts or comments to interleave.
func xlsxGridBlocks(m *xlsxModel) []string {
	var out []string
	for _, reg := range xlsxRegions(m) {
		out = append(out, xlsxRegionBlocks(m, reg)...)
	}
	return out
}

// xlsxSheetBlocks renders one sheet: its tables and its charts interleaved by
// vertical position, then its comments.
//
// The interleaving matters. A chart anchored above the second table on a sheet
// reads as belonging to the first, and emitting every table before every chart
// would reverse that.
func xlsxSheetBlocks(m *xlsxModel, pkg *ooxml.Package, part string) []string {
	type placed struct {
		row    int
		seq    int
		blocks []string
	}
	var items []placed
	for _, reg := range xlsxRegions(m) {
		if b := xlsxRegionBlocks(m, reg); len(b) > 0 {
			items = append(items, placed{reg.r0, len(items), b})
		}
	}
	for _, ch := range xlsxSheetCharts(pkg, part) {
		items = append(items, placed{ch.row, len(items), ch.blocks})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].row < items[j].row })

	var out []string
	for _, it := range items {
		out = append(out, it.blocks...)
	}
	return append(out, xlsxSheetComments(pkg, part)...)
}

// xlsxChart is one chart with the sheet row it is anchored at, so it can be
// ordered against the sheet's tables.
type xlsxChart struct {
	row    int
	blocks []string
}

// xlsxSheetCharts renders every chart anchored on a sheet.
//
// A chart's numbers live in an embedded workbook a reader never sees, but the
// producer also caches the plotted strings and values in the chart part itself
// — the same cache conv_pptx.go reads — so the chart can be recovered as a
// title, a type and a category x series grid without evaluating anything.
func xlsxSheetCharts(pkg *ooxml.Package, part string) []xlsxChart {
	if pkg == nil || part == "" {
		return nil
	}
	var out []xlsxChart
	for _, rel := range pkg.Relationships(part) {
		if rel.External() || !strings.HasSuffix(rel.Type, "/drawing") {
			continue
		}
		drawing := ooxml.ResolveTarget(part, rel.Target)
		if drawing == "" {
			continue
		}
		targets := pkg.RelTargets(drawing)
		for _, anchor := range xlsxDrawingCharts(pkg.OptionalPart(drawing)) {
			chartPart := ooxml.ResolveTarget(drawing, targets[anchor.relID])
			if chartPart == "" {
				continue
			}
			data := pkg.OptionalPart(chartPart)
			if len(data) == 0 {
				continue
			}
			if blocks := xlsxChartBlocks(data); len(blocks) > 0 {
				out = append(out, xlsxChart{row: anchor.row, blocks: blocks})
			}
		}
	}
	return out
}

// xlsxDrawingAnchor is one graphic frame in a drawing part that holds a chart.
type xlsxDrawingAnchor struct {
	row   int
	relID string
}

// maxXlsxDrawingCharts bounds the anchors read from one drawing part.
const maxXlsxDrawingCharts = 4096

// xlsxDrawingCharts pulls (anchor row, chart relationship id) out of a
// spreadsheet drawing part. The row is the anchor's top edge, which is what
// orders a chart against the tables around it.
func xlsxDrawingCharts(data []byte) []xlsxDrawingAnchor {
	if len(data) == 0 {
		return nil
	}
	d := ooxml.NewDecoder(data)
	var (
		out     []xlsxDrawingAnchor
		row     int
		inFrom  bool
		lastRow int
	)
	for len(out) < maxXlsxDrawingCharts {
		tok, err := d.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "twoCellAnchor", "oneCellAnchor", "absoluteAnchor":
				row = 0
			case "from":
				inFrom, lastRow = true, 0
			case "row":
				if inFrom {
					s, err := ooxml.TextOf(d)
					if err != nil {
						return out
					}
					if n, convErr := strconv.Atoi(strings.TrimSpace(s)); convErr == nil && n >= 0 {
						lastRow = n
					}
				}
			case "chart":
				if id := ooxml.Attr(t, "id"); id != "" {
					out = append(out, xlsxDrawingAnchor{row: row, relID: id})
				}
			}
		case xml.EndElement:
			if t.Name.Local == "from" && inFrom {
				inFrom, row = false, lastRow
			}
		}
	}
	return out
}

// xlsxChartTypeNames maps a plot-area element to the words a reader gets. The
// name is worth emitting because a bare grid of numbers does not say whether
// it was drawn as bars, a line or a scatter, and that is often the only thing
// the chart contributed to the page.
var xlsxChartTypeNames = map[string]string{
	"barChart":      "Bar chart",
	"bar3DChart":    "Bar chart",
	"lineChart":     "Line chart",
	"line3DChart":   "Line chart",
	"pieChart":      "Pie chart",
	"pie3DChart":    "Pie chart",
	"doughnutChart": "Pie chart",
	"scatterChart":  "Scatter chart",
}

// xlsxChartBlocks renders one chart part as title, type and data grid.
func xlsxChartBlocks(data []byte) []string {
	title, kind, series, err := xlsxParseChart(data)
	if err != nil {
		return nil
	}
	var blocks []string
	if title != "" {
		blocks = append(blocks, title)
	}
	if kind != "" {
		blocks = append(blocks, kind)
	}
	if len(series) == 0 {
		return blocks
	}

	// Categories come from the first series that names any: every series on a
	// chart shares one category axis, and a scatter series states it as xVal.
	var cats []string
	for _, s := range series {
		if len(s.cats) > 0 {
			cats = s.cats
			break
		}
	}
	header := make([]string, 0, len(series)+1)
	header = append(header, "")
	rowCount := len(cats)
	for _, s := range series {
		header = append(header, s.name)
		if len(s.vals) > rowCount {
			rowCount = len(s.vals)
		}
	}
	if rowCount == 0 {
		return blocks
	}
	rows := make([][]string, 0, rowCount)
	for i := 0; i < rowCount; i++ {
		row := make([]string, 0, len(series)+1)
		if i < len(cats) {
			row = append(row, cats[i])
		} else {
			row = append(row, "")
		}
		for _, s := range series {
			if i < len(s.vals) {
				row = append(row, s.vals[i])
			} else {
				row = append(row, "")
			}
		}
		rows = append(rows, row)
	}
	if t := mdutil.Table(header, rows); t != "" {
		blocks = append(blocks, t)
	}
	return blocks
}

// xlsxParseChart pulls the title, the chart type and every series out of a
// chart part. The first c:title in document order is the chart's; the ones
// after it belong to the axes.
func xlsxParseChart(data []byte) (title, kind string, series []pptxSeries, err error) {
	d := ooxml.NewDecoder(data)
	haveTitle := false
	for {
		tok, tokErr := d.Token()
		if tokErr == io.EOF {
			break
		}
		if tokErr != nil {
			return "", "", nil, tokErr
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch {
		case se.Name.Local == "title":
			if haveTitle {
				continue
			}
			haveTitle = true
			t, tErr := pptxChartTitle(d)
			if tErr != nil {
				return "", "", nil, tErr
			}
			title = t
		case se.Name.Local == "ser":
			s, sErr := xlsxChartSeries(d)
			if sErr != nil {
				return "", "", nil, sErr
			}
			series = append(series, s)
		case kind == "" && strings.HasSuffix(se.Name.Local, "Chart"):
			if name, ok := xlsxChartTypeNames[se.Name.Local]; ok {
				kind = name
			} else {
				kind = "Other chart"
			}
		}
	}
	return title, kind, series, nil
}

// xlsxChartSeries consumes one c:ser. It reads categories from c:cat or, for a
// scatter chart, from c:xVal, and values from c:val or c:yVal — a scatter
// series carries the same data under different tags, and skipping those is how
// a scatter chart silently comes out empty.
func xlsxChartSeries(d *xml.Decoder) (pptxSeries, error) {
	var s pptxSeries
	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return s, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "tx":
				pts, err := pptxCachedPoints(d)
				if err != nil {
					return s, err
				}
				if len(pts) > 0 {
					s.name = pts[0]
				}
			case "cat", "xVal":
				pts, err := pptxCachedPoints(d)
				if err != nil {
					return s, err
				}
				if len(pts) > 0 {
					s.cats = pts
				}
			case "val", "yVal":
				pts, err := pptxCachedPoints(d)
				if err != nil {
					return s, err
				}
				if len(pts) > 0 {
					s.vals = pts
				}
			default:
				depth++
			}
		case xml.EndElement:
			depth--
		}
	}
	return s, nil
}

// maxXlsxComments bounds how many cell notes one sheet may contribute.
const maxXlsxComments = 4096

// xlsxComment is one cell note, positioned so notes come out in reading order.
type xlsxComment struct {
	row, col int
	author   string
	when     string
	text     string
}

// xlsxSheetComments renders a sheet's cell notes, in reading order, after its
// tables.
//
// Notes are a real part of a reviewed spreadsheet — the "why is this number
// like that" the grid cannot hold — and dropping them loses content that has
// no other representation. They are appended rather than inlined because a
// note has no cell of its own to occupy in a markdown table.
func xlsxSheetComments(pkg *ooxml.Package, part string) []string {
	if pkg == nil || part == "" {
		return nil
	}
	var (
		legacy   string
		threaded string
	)
	for _, rel := range pkg.Relationships(part) {
		if rel.External() {
			continue
		}
		switch {
		case strings.HasSuffix(rel.Type, "/comments"):
			legacy = ooxml.ResolveTarget(part, rel.Target)
		case strings.HasSuffix(rel.Type, "/threadedComment"):
			threaded = ooxml.ResolveTarget(part, rel.Target)
		}
	}
	byCell := map[[2]int]xlsxComment{}
	for _, c := range xlsxLegacyComments(pkg.OptionalPart(legacy)) {
		byCell[[2]int{c.row, c.col}] = c
	}
	// A threaded note is the modern form of the same annotation: the legacy
	// part holds a boilerplate-wrapped copy for old Excel, so where both exist
	// the threaded one wins. Its last reply is the current state of the thread.
	for _, c := range xlsxThreadedComments(pkg.OptionalPart(threaded), xlsxPersons(pkg)) {
		byCell[[2]int{c.row, c.col}] = c
	}
	if len(byCell) == 0 {
		return nil
	}
	list := make([]xlsxComment, 0, len(byCell))
	for _, c := range byCell {
		list = append(list, c)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].row != list[j].row {
			return list[i].row < list[j].row
		}
		return list[i].col < list[j].col
	})

	out := make([]string, 0, len(list))
	for _, c := range list {
		var meta []string
		if c.author != "" {
			meta = append(meta, "author: "+c.author)
		}
		if c.when != "" {
			meta = append(meta, "time: "+c.when)
		}
		switch {
		case len(meta) > 0 && c.text != "":
			out = append(out, "["+strings.Join(meta, ", ")+"]: "+c.text)
		case len(meta) > 0:
			out = append(out, "["+strings.Join(meta, ", ")+"]")
		case c.text != "":
			out = append(out, c.text)
		}
	}
	return out
}

// xlsxThreadedBoilerplate is the notice Excel writes into the legacy copy of a
// threaded note so older versions can still display it. It is not content.
const xlsxThreadedBoilerplate = "[Threaded comment]"

// xlsxLegacyComments parses xl/commentsN.xml.
func xlsxLegacyComments(data []byte) []xlsxComment {
	if len(data) == 0 {
		return nil
	}
	var doc struct {
		Authors []string `xml:"authors>author"`
		List    []struct {
			Ref      string `xml:"ref,attr"`
			AuthorID int    `xml:"authorId,attr"`
			Text     struct {
				Runs []string `xml:"r>t"`
				Text string   `xml:"t"`
			} `xml:"text"`
		} `xml:"commentList>comment"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil
	}
	out := make([]xlsxComment, 0, len(doc.List))
	for _, c := range doc.List {
		if len(out) >= maxXlsxComments {
			break
		}
		row, col, ok := xlsxCellRef(c.Ref)
		if !ok {
			continue
		}
		text := strings.TrimSpace(strings.Join(c.Text.Runs, "") + c.Text.Text)
		if text == "" {
			continue
		}
		author := ""
		if c.AuthorID >= 0 && c.AuthorID < len(doc.Authors) {
			author = strings.TrimSpace(doc.Authors[c.AuthorID])
		}
		if strings.HasPrefix(author, "tc={") && strings.Contains(text, xlsxThreadedBoilerplate) {
			// The threaded part, if present, replaces this outright; if it is
			// missing, keep the note but not Excel's compatibility notice.
			if _, after, found := strings.Cut(text, "Comment:\n"); found {
				text = strings.TrimSpace(after)
			}
			author = "Threaded comment"
		}
		out = append(out, xlsxComment{row: row, col: col, author: author, text: text})
	}
	return out
}

// xlsxThreadedComments parses xl/threadedComments/threadedCommentN.xml,
// resolving each personId through the workbook's person list.
func xlsxThreadedComments(data []byte, persons map[string]string) []xlsxComment {
	if len(data) == 0 {
		return nil
	}
	var doc struct {
		Comments []struct {
			Ref      string `xml:"ref,attr"`
			DT       string `xml:"dT,attr"`
			PersonID string `xml:"personId,attr"`
			Text     string `xml:"text"`
		} `xml:"threadedComment"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil
	}
	out := make([]xlsxComment, 0, len(doc.Comments))
	for _, c := range doc.Comments {
		if len(out) >= maxXlsxComments {
			break
		}
		row, col, ok := xlsxCellRef(c.Ref)
		if !ok {
			continue
		}
		author := persons[c.PersonID]
		if author == "" {
			author = "Unknown"
		}
		out = append(out, xlsxComment{
			row:    row,
			col:    col,
			author: author,
			when:   xlsxCommentTime(c.DT),
			text:   strings.TrimSpace(c.Text),
		})
	}
	return out
}

// xlsxPersons maps a threaded-comment personId to a display name.
func xlsxPersons(pkg *ooxml.Package) map[string]string {
	data := pkg.OptionalPart("xl/persons/person.xml")
	if len(data) == 0 {
		return nil
	}
	var doc struct {
		People []struct {
			ID   string `xml:"id,attr"`
			Name string `xml:"displayName,attr"`
		} `xml:"person"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil
	}
	out := make(map[string]string, len(doc.People))
	for _, p := range doc.People {
		if p.ID != "" && p.Name != "" {
			out[p.ID] = p.Name
		}
	}
	return out
}

// xlsxCommentTime normalizes a threaded note's timestamp to millisecond
// precision, which is how the rest of the ecosystem prints it. The fraction is
// truncated, not rounded, and an unparseable value yields "" rather than an
// invented time.
func xlsxCommentTime(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	zone := ""
	if strings.HasSuffix(s, "Z") || strings.HasSuffix(s, "z") {
		s, zone = s[:len(s)-1], "+00:00"
	} else if len(s) >= 6 {
		if tail := s[len(s)-6:]; (tail[0] == '+' || tail[0] == '-') && tail[3] == ':' {
			s, zone = s[:len(s)-6], tail
		}
	}
	base, frac, _ := strings.Cut(s, ".")
	if len(base) != len("2006-01-02T15:04:05") {
		return ""
	}
	for i, r := range base {
		switch i {
		case 4, 7:
			if r != '-' {
				return ""
			}
		case 10:
			if r != 'T' && r != 't' && r != ' ' {
				return ""
			}
		case 13, 16:
			if r != ':' {
				return ""
			}
		default:
			if r < '0' || r > '9' {
				return ""
			}
		}
	}
	for _, r := range frac {
		if r < '0' || r > '9' {
			return ""
		}
	}
	frac = (frac + "000")[:3]
	return base[:10] + "T" + base[11:] + "." + frac + zone
}

// xlsxCellRef converts an A1-style reference to 0-based row and column.
func xlsxCellRef(ref string) (row, col int, ok bool) {
	c, r, err := excelize.CellNameToCoordinates(strings.TrimSpace(ref))
	if err != nil || r < 1 || c < 1 {
		return 0, 0, false
	}
	return r - 1, c - 1, true
}
