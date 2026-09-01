package anymd

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/muthuishere/anymd/internal/mdutil"
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

// XlsxConverter renders an OOXML workbook as one GFM table per sheet, in the
// workbook's own sheet order, each under an "## SheetName" heading.
//
// Values are rendered as displayed rather than as stored: dates come out as
// dates instead of serial numbers, and a formula cell emits its cached result.
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

// Convert renders every non-empty sheet.
func (c *XlsxConverter) Convert(r io.ReadSeeker, info StreamInfo, opts *Options) (res Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("malformed workbook: %v", p)
		}
	}()

	f, err := excelize.OpenReader(r)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()

	var blocks []string
	total := 0
	for _, sheet := range f.GetSheetList() {
		grid, n, err := sheetGrid(f, sheet, &total)
		if err != nil {
			return Result{}, err
		}
		if len(grid) == 0 || n == 0 {
			continue // an entirely empty sheet emits nothing, not a bare heading
		}
		blocks = append(blocks,
			mdutil.Heading(2, sheet),
			mdutil.Table(grid[0], grid[1:]),
		)
	}
	return Result{Markdown: mdutil.Join(blocks...)}, nil
}

// sheetGrid streams one sheet into a rectangular grid, trimming trailing empty
// rows and columns so a stray formatted cell at ZZ1000 does not become a
// 700-column table. total accumulates cells across the workbook for the cap.
func sheetGrid(f *excelize.File, sheet string, total *int) ([][]string, int, error) {
	rows, err := f.Rows(sheet)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var grid [][]string
	width, lastRow := 0, 0
	for rows.Next() {
		cols, err := rows.Columns()
		if err != nil {
			return nil, 0, err
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
			return nil, 0, fmt.Errorf("workbook exceeds the %d-cell limit", maxXlsxCells)
		}
		grid = append(grid, cols)
	}
	if err := rows.Error(); err != nil {
		return nil, 0, err
	}
	grid = grid[:lastRow]
	for i, row := range grid {
		if len(row) < width {
			padded := make([]string, width)
			copy(padded, row)
			grid[i] = padded
		}
	}
	return grid, width, nil
}
