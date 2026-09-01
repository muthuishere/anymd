package anymd

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/muthuishere/anymd/internal/mdutil"
)

func init() { addBuiltin(&CSVConverter{}) }

// maxCSVCells bounds the table a delimited file may expand into, so a hostile
// 10 KiB of commas cannot turn into gigabytes of markdown.
const maxCSVCells = 1_000_000

// maxCSVBytes bounds how much of a delimited stream is read at all.
const maxCSVBytes = 256 << 20

// csvDelims are the candidates the sniffer scores when no extension says which
// separator a file uses.
var csvDelims = []rune{',', '\t', ';', '|'}

// CSVConverter renders delimiter-separated text as a single GFM table with the
// first row as the header.
//
// Real-world exports are ragged, so parsing never enforces a field count: a row
// with too few fields is padded and the header is widened to the widest row
// rather than truncating data away.
type CSVConverter struct{}

// Name identifies the converter in errors and in `anymd --list`.
func (c *CSVConverter) Name() string { return "csv" }

// Accepts keys off the extension and mime hints only. Sniffing text for commas
// would steal prose from the plain-text fallback.
func (c *CSVConverter) Accepts(r io.ReadSeeker, info StreamInfo, opts *Options) bool {
	if info.HasExt(".csv", ".tsv", ".tab") {
		return true
	}
	switch info.NormalizedMime() {
	case "text/csv", "application/csv", "text/tab-separated-values", "text/tsv":
		return true
	}
	return false
}

// Convert parses the stream and renders the table.
func (c *CSVConverter) Convert(r io.ReadSeeker, info StreamInfo, opts *Options) (Result, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxCSVBytes+1))
	if err != nil {
		return Result{}, err
	}
	if len(raw) > maxCSVBytes {
		return Result{}, fmt.Errorf("input exceeds the %d-byte limit", maxCSVBytes)
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM

	cr := csv.NewReader(bytes.NewReader(raw))
	cr.Comma = csvDelimiter(info, raw)
	cr.FieldsPerRecord = -1 // ragged rows are normal in the wild; never fail on them
	cr.LazyQuotes = true    // a stray quote must not abort a whole export
	cr.ReuseRecord = false

	var rows [][]string
	width, cells := 0, 0
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Result{}, err
		}
		cells += len(rec)
		if cells > maxCSVCells {
			return Result{}, fmt.Errorf("input exceeds the %d-cell limit", maxCSVCells)
		}
		if len(rec) > width {
			width = len(rec)
		}
		rows = append(rows, rec)
	}
	if len(rows) == 0 || width == 0 {
		return Result{}, nil
	}
	header := make([]string, width)
	copy(header, rows[0])
	return Result{Markdown: mdutil.Join(mdutil.Table(header, rows[1:]))}, nil
}

// csvDelimiter picks the separator: the extension decides when it can, and
// otherwise the candidate that splits the first few lines most consistently
// wins. Consistency beats raw count — prose full of commas has an erratic field
// count, a real CSV does not.
func csvDelimiter(info StreamInfo, raw []byte) rune {
	if info.HasExt(".tsv", ".tab") || info.NormalizedMime() == "text/tab-separated-values" {
		return '\t'
	}
	if info.HasExt(".csv") {
		return ','
	}
	best, bestScore := ',', -1
	for _, d := range csvDelims {
		if s := csvScore(raw, d); s > bestScore {
			best, bestScore = d, s
		}
	}
	return best
}

// csvScore counts the fields d yields on each of the first 5 non-empty lines and
// returns (fields-1) when every line agrees, or -1 when they disagree or the
// delimiter never appears. Only the first 64 KiB is examined.
func csvScore(raw []byte, d rune) int {
	head := raw
	if len(head) > 64<<10 {
		head = head[:64<<10]
	}
	lines := strings.Split(strings.ReplaceAll(string(head), "\r\n", "\n"), "\n")
	if len(head) < len(raw) && len(lines) > 1 {
		lines = lines[:len(lines)-1] // drop the line the 64 KiB cut truncated
	}
	seen, want := 0, -1
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		n := strings.Count(line, string(d)) + 1
		if want == -1 {
			want = n
		} else if n != want {
			return -1
		}
		if seen++; seen == 5 {
			break
		}
	}
	if want <= 1 {
		return -1
	}
	return want - 1
}
