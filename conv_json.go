package anymd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/muthuishere/anymd/internal/mdutil"
)

func init() { addBuiltin(&JSONConverter{}) }

const (
	// maxJSONBytes bounds how much of a stream is read for conversion.
	maxJSONBytes = 128 << 20
	// jsonSniffBytes bounds the Accepts-time validity check. Past this the head
	// is inconclusive (a valid document is simply truncated), so Accepts falls
	// back to the extension/mime hint rather than parsing the whole file.
	jsonSniffBytes = 1 << 20
	// maxJSONTableCells bounds the promoted array-of-objects table.
	maxJSONTableCells = 1_000_000
)

// JSONConverter renders JSON as a fenced, re-indented code block — except for
// the common "exported records" shape, a top-level array of flat objects, which
// becomes a GFM table because a table is far easier for a reader (human or
// model) to scan than 400 lines of braces.
type JSONConverter struct{}

// Name identifies the converter in errors and in `anymd --list`.
func (c *JSONConverter) Name() string { return "json" }

// Priority sits one step behind PrioritySpecific so the notebook converter,
// whose files are also JSON, always gets first refusal.
func (c *JSONConverter) Priority() int { return PrioritySpecific + 1 }

// Accepts requires the bytes to actually be JSON, because the .json extension
// is frequently wrong and a mis-claim is a hard error for the engine.
func (c *JSONConverter) Accepts(r io.ReadSeeker, info StreamInfo, opts *Options) bool {
	if info.HasExt(".ipynb") {
		return false
	}
	hinted := info.HasExt(".json") ||
		info.NormalizedMime() == "application/json" ||
		info.NormalizedMime() == "text/json"
	head, complete := jsonHead(r)
	if !jsonLooksLikeStart(head) {
		return false
	}
	if complete {
		return json.Valid(head)
	}
	// Inconclusive head: trust the hint or nothing.
	return hinted
}

// jsonHead reads at most jsonSniffBytes and reports whether that was the whole
// stream.
func jsonHead(r io.ReadSeeker) (head []byte, complete bool) {
	b, err := io.ReadAll(io.LimitReader(r, jsonSniffBytes+1))
	if err != nil {
		return nil, false
	}
	if len(b) > jsonSniffBytes {
		return b[:jsonSniffBytes], false
	}
	return b, true
}

// jsonLooksLikeStart is the cheap prefix test: the first non-space byte must
// open an object or an array.
func jsonLooksLikeStart(b []byte) bool {
	for _, ch := range b {
		switch ch {
		case ' ', '\t', '\r', '\n':
			continue
		case '{', '[':
			return true
		default:
			return false
		}
	}
	return false
}

// Convert renders the document.
func (c *JSONConverter) Convert(r io.ReadSeeker, info StreamInfo, opts *Options) (Result, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxJSONBytes+1))
	if err != nil {
		return Result{}, err
	}
	if len(raw) > maxJSONBytes {
		return Result{}, fmt.Errorf("input exceeds the %d-byte limit", maxJSONBytes)
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	if !json.Valid(raw) {
		return Result{}, fmt.Errorf("not valid JSON")
	}
	if md, ok := jsonRecordsTable(raw); ok {
		return Result{Markdown: mdutil.Join(md)}, nil
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return Result{}, err
	}
	return Result{Markdown: mdutil.Join(mdutil.CodeBlock("json", out.String()))}, nil
}

// jsonRecordsTable renders a top-level array of flat objects as a table, with
// the union of keys in first-seen order as the header. It walks tokens rather
// than unmarshalling into a map so key order survives, and reports false for
// anything that is not that exact shape.
func jsonRecordsTable(raw []byte) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	t, err := dec.Token()
	if err != nil || t != json.Delim('[') {
		return "", false
	}
	var keys []string
	seen := map[string]bool{}
	var records []map[string]string
	cells := 0
	for dec.More() {
		t, err := dec.Token()
		if err != nil || t != json.Delim('{') {
			return "", false
		}
		rec := map[string]string{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return "", false
			}
			k, ok := kt.(string)
			if !ok {
				return "", false
			}
			var v any
			if err := dec.Decode(&v); err != nil {
				return "", false
			}
			s, ok := jsonScalar(v)
			if !ok {
				return "", false // a nested object or array: keep the code block
			}
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
			rec[k] = s
			if cells++; cells > maxJSONTableCells {
				return "", false
			}
		}
		if _, err := dec.Token(); err != nil { // closing '}'
			return "", false
		}
		records = append(records, rec)
	}
	if _, err := dec.Token(); err != nil { // closing ']'
		return "", false
	}
	if len(keys) == 0 || len(records) == 0 {
		return "", false
	}
	rows := make([][]string, 0, len(records))
	for _, rec := range records {
		row := make([]string, len(keys))
		for i, k := range keys {
			row[i] = rec[k]
		}
		rows = append(rows, row)
	}
	return mdutil.Table(keys, rows), true
}

// jsonScalar formats a decoded value if it is a scalar, and reports false for
// objects and arrays.
func jsonScalar(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", true
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	case json.Number:
		return x.String(), true
	default:
		return "", false
	}
}
