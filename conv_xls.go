package anymd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	xlslib "github.com/extrame/xls"
	"github.com/richardlehane/mscfb"

	"github.com/muthuishere/anymd/internal/mdutil"
)

func init() { addBuiltin(&XLSConverter{}) }

// maxXLSCells bounds how many cells a legacy workbook may emit. It matches
// maxXlsxCells deliberately: the two Excel paths must behave identically, so a
// sheet that is too big to render as markdown is too big in both formats.
const maxXLSCells = 1_000_000

const (
	// maxXLSBytes caps the whole container. BIFF is parsed by a library that
	// indexes sector chains from in-file offsets, so the input is bounded
	// before it is handed over rather than trusting any length field inside.
	maxXLSBytes = 128 << 20
	// maxXLSSniffBytes bounds the buffering Accepts does when the stream is
	// not already an io.ReaderAt.
	maxXLSSniffBytes = 64 << 20
	// maxXLSEntries bounds the CFB directory walk done by the Accepts probe.
	maxXLSEntries = 1 << 16
	// maxXLSSheets and maxXLSRows bound the walk over a workbook whose own
	// header fields are attacker controlled.
	maxXLSSheets = 4096
	maxXLSRows   = 1 << 20
	// maxXLSColumns is BIFF's hard column limit; a row claiming more than 256
	// used columns is malformed, whatever its ROW record says.
	maxXLSColumns = 256
)

// xlsBookStreams are the two names BIFF gives its workbook stream: "Workbook"
// in BIFF8 (Excel 97+) and "Book" in BIFF5/7 (Excel 5/95). Finding one of them
// as a top-level CFB stream is what positively identifies a spreadsheet inside
// a container whose magic is shared with .doc, .ppt and .msg.
var xlsBookStreams = []string{"Workbook", "Book"}

// XLSConverter renders a legacy binary Excel workbook (BIFF inside an OLE2 /
// Compound File container, the pre-2007 ".xls") as one GFM table per sheet, in
// the workbook's own sheet order, each under an "## SheetName" heading.
//
// Its output shape is deliberately identical to XlsxConverter's — same
// heading level, same trailing-blank trimming, same cell cap, and the same
// split of a sheet into one table per 4-connected region — so a reader cannot
// tell which of the two Excel formats a document came from. BIFF's merged-cell
// records are not exposed by the parser, so a legacy sheet's merges do not
// join regions the way an .xlsx's do; everything else is shared code.
type XLSConverter struct{}

// Name identifies the converter in errors and in `anymd --list`.
func (c *XLSConverter) Name() string { return "xls" }

// Accepts recognizes a legacy workbook.
//
// The CFB magic alone is NOT enough: it is byte-for-byte the magic of legacy
// .doc and .ppt and of an Outlook .msg, and because the engine treats an
// accept-then-fail as a hard error, a false accept would permanently break
// those files rather than letting their own converter run. So on a bare stream
// we decline unless we can POSITIVELY confirm a top-level "Workbook"/"Book"
// stream in the CFB directory, and we decline outright when the MAPI
// `__substg1.0_` marker that MsgConverter keys on is present — which makes the
// two converters provably disjoint. When in doubt we decline: a false decline
// only means the extension hint (or the fallback) decides.
func (c *XLSConverter) Accepts(r io.ReadSeeker, info StreamInfo, opts *Options) bool {
	// Never claim a sibling legacy format, however the stream is sniffed.
	if info.HasExt(".msg", ".doc", ".dot", ".ppt", ".pot", ".pps") {
		return false
	}
	if info.HasExt(".xls", ".xlt", ".xlm", ".xlw") {
		return true
	}
	switch info.NormalizedMime() {
	case "application/vnd.ms-excel", "application/msexcel", "application/x-msexcel",
		"application/x-excel", "application/excel":
		return true
	}
	var head [8]byte
	if n, _ := io.ReadFull(r, head[:]); n != len(head) || !bytes.Equal(head[:], cfbMagic) {
		return false
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return false
	}
	return xlsHasBookStream(r)
}

// xlsHasBookStream reports whether a CFB stream carries a workbook, by reading
// only its directory — never a cell. It answers false for anything it cannot
// confirm, including a MAPI message, which it rejects before parsing.
func xlsHasBookStream(r io.ReadSeeker) (found bool) {
	defer func() {
		// mscfb walks sector chains built from in-file indices; the contract
		// forbids a panic escaping a converter, and Accepts is no exception.
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
		b, err := io.ReadAll(io.LimitReader(r, maxXLSSniffBytes))
		if err != nil {
			return false
		}
		if bytes.Contains(b, msgPropPrefixUTF16) {
			return false
		}
		ra = bytes.NewReader(b)
	} else if xlsLooksLikeMsg(ra, size) {
		return false
	}

	doc, err := mscfb.New(ra)
	if err != nil {
		return false
	}
	seen := 0
	for entry, err := doc.Next(); err == nil; entry, err = doc.Next() {
		if seen++; seen > maxXLSEntries {
			return false
		}
		if len(entry.Path) != 0 {
			continue // nested storage: a workbook stream lives at the root
		}
		if strings.HasPrefix(entry.Name, msgPropPrefix) {
			return false // a MAPI message wearing the same magic
		}
		for _, want := range xlsBookStreams {
			if entry.Name == want && entry.Size > 0 {
				found = true
			}
		}
	}
	return found
}

// xlsLooksLikeMsg does the same bounded `__substg1.0_` scan MsgConverter's
// Accepts does, without copying the stream, so the two converters agree on
// what a .msg is.
func xlsLooksLikeMsg(ra io.ReaderAt, size int64) bool {
	n64 := size
	if n64 > maxMsgSniff {
		n64 = maxMsgSniff
	}
	buf := make([]byte, n64)
	n, err := ra.ReadAt(buf, 0)
	if n <= 0 && err != nil {
		return false
	}
	return bytes.Contains(buf[:n], msgPropPrefixUTF16)
}

// Convert renders every non-empty sheet.
func (c *XLSConverter) Convert(r io.ReadSeeker, info StreamInfo, opts *Options) (res Result, err error) {
	defer func() {
		// github.com/extrame/xls is not hardened: it binary.Reads records whose
		// sizes come straight out of the file. A malformed workbook must be an
		// error, never a panic.
		if p := recover(); p != nil {
			res = Result{}
			err = fmt.Errorf("malformed workbook: %v", p)
		}
	}()

	raw, err := io.ReadAll(io.LimitReader(r, maxXLSBytes+1))
	if err != nil {
		return Result{}, err
	}
	if len(raw) > maxXLSBytes {
		return Result{}, fmt.Errorf("workbook too large: over %d bytes", maxXLSBytes)
	}

	wb, err := openWorkbookGuarded(raw)
	if err != nil {
		return Result{}, err
	}
	// OpenReader reports "this is not a workbook" as (nil, nil): no Workbook or
	// Book stream was found. Accepts should have caught that, but a nil deref
	// here would be a panic, so it is checked rather than assumed.
	if wb == nil {
		return Result{}, fmt.Errorf("no workbook stream found (not a legacy .xls?)")
	}

	sheets := wb.NumSheets()
	if sheets > maxXLSSheets {
		sheets = maxXLSSheets
	}
	var blocks []string
	total := 0
	for i := 0; i < sheets; i++ {
		sh := wb.GetSheet(i)
		if sh == nil {
			continue
		}
		grid, width, err := xlsSheetGrid(sh, &total)
		if err != nil {
			return Result{}, err
		}
		if len(grid) == 0 || width == 0 {
			continue // an entirely empty sheet emits nothing, not a bare heading
		}
		// Split on blank rows and columns exactly as the xlsx path does: the
		// two Excel formats hold the same visual layouts, and a legacy sheet
		// with a title block above a data grid must not come out as one welded
		// table when its .xlsx twin comes out as two.
		body := xlsxGridBlocks(xlsxModelFromGrid(grid, width))
		if len(body) == 0 {
			continue
		}
		blocks = append(blocks, mdutil.Heading(2, sh.Name))
		blocks = append(blocks, body...)
	}
	return Result{Markdown: mdutil.Join(blocks...)}, nil
}

// xlsSheetGrid reads one sheet into a rectangular grid, trimming trailing empty
// columns and dropping trailing empty rows, so a stray formatted cell far off
// to the right does not become a several-hundred-column table. total
// accumulates cells across the workbook for the cap.
//
// It mirrors sheetGrid in conv_xlsx.go step for step; the two Excel paths must
// produce byte-identical markdown for the same data.
func xlsSheetGrid(sh *xlslib.WorkSheet, total *int) ([][]string, int, error) {
	rows := int(sh.MaxRow) + 1
	if rows > maxXLSRows {
		rows = maxXLSRows
	}
	grid := make([][]string, 0, rows)
	width, lastRow := 0, 0
	for i := 0; i < rows; i++ {
		cols := xlsRowCells(xlsRow(sh, i))
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
		if *total > maxXLSCells {
			return nil, 0, fmt.Errorf("workbook exceeds the %d-cell limit", maxXLSCells)
		}
		grid = append(grid, cols)
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

// xlsRow fetches one row, or nil.
//
// WorkSheet.Row dereferences its rows map unconditionally, so it panics on any
// index inside MaxRow that carries no ROW record — which is every gap in a
// sparse sheet. Recovering per row keeps one hole from failing the whole
// workbook; the Convert-level recover would abort the document.
func xlsRow(sh *xlslib.WorkSheet, i int) (row *xlslib.Row) {
	defer func() {
		if recover() != nil {
			row = nil
		}
	}()
	return sh.Row(i)
}

// xlsRowCells reads one row as display strings, left-padded from column 0 so a
// row that starts at column D still lines up under the header.
//
// Row.Col renders through the cell's number format, so a date comes out as a
// date and a currency as its formatted text rather than as the underlying
// serial number.
func xlsRowCells(row *xlslib.Row) []string {
	if row == nil {
		return nil
	}
	// LastCol is BIFF's "last defined column + 1"; FirstCol can be nonzero on a
	// sparse row. Both are read from the file, so the span is clamped.
	last := row.LastCol()
	if last <= 0 {
		return nil
	}
	if last > maxXLSColumns {
		last = maxXLSColumns
	}
	cells := make([]string, last)
	first := row.FirstCol()
	if first < 0 {
		first = 0
	}
	for c := first; c < last; c++ {
		cells[c] = row.Col(c)
	}
	return cells
}

// ErrParseTimeout means the underlying parser did not finish within
// xlsParseBudget and was abandoned. It is distinct from a malformed-file error:
// the input may be perfectly valid and merely pathological.
var ErrParseTimeout = errors.New("anymd: parser exceeded its time budget")

// xlsParseBudget bounds a single OpenReader call. Chosen to be far above any
// legitimate workbook (the largest in our corpus parses in milliseconds) and far
// below a caller's patience.
const xlsParseBudget = 10 * time.Second

// openWorkbookGuarded runs the BIFF parser under a wall-clock budget.
//
// This exists because of a real, fuzzer-found defect, and the shape is
// deliberate. github.com/extrame/xls can enter a non-terminating loop on a
// crafted workbook (see testdata/fuzz/FuzzXls/29ed7f0c0d49e0d5). A loop is not
// a panic, so the recover() above cannot catch it: without this guard a single
// uploaded file wedges the calling goroutine forever, which for a server is a
// denial of service.
//
// Go cannot kill a goroutine, so the abandoned parse keeps running until it
// finishes or the process exits. That leak is the price of not blocking the
// caller, and it is bounded: the input is already capped at maxXLSBytes, and
// the parser holds no locks the rest of the package needs. Returning promptly
// with an error is strictly better than never returning at all.
func openWorkbookGuarded(raw []byte) (wb *xlslib.WorkBook, err error) {
	type result struct {
		wb  *xlslib.WorkBook
		err error
	}
	done := make(chan result, 1) // buffered: the abandoned goroutine must not block forever

	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- result{nil, fmt.Errorf("malformed workbook: %v", p)}
			}
		}()
		w, e := xlslib.OpenReader(bytes.NewReader(raw), "utf-8")
		done <- result{w, e}
	}()

	timer := time.NewTimer(xlsParseBudget)
	defer timer.Stop()

	select {
	case r := <-done:
		return r.wb, r.err
	case <-timer.C:
		return nil, ErrParseTimeout
	}
}
