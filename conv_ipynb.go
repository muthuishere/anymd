package anymd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/muthuishere/anymd/internal/mdutil"
)

func init() { addBuiltin(&IpynbConverter{}) }

const (
	// maxIpynbBytes bounds how much of a notebook is read. Notebooks with
	// embedded images get large; images are dropped, but the bytes are still
	// parsed, so the ceiling is generous rather than tight.
	maxIpynbBytes = 128 << 20
	// ipynbSniffBytes bounds the Accepts-time validity check.
	ipynbSniffBytes = 1 << 20
)

// IpynbConverter renders a Jupyter notebook: markdown cells verbatim, code
// cells fenced in the kernel's language, and textual outputs fenced beneath
// them. Image and other binary outputs are dropped entirely — a page of base64
// is noise to every consumer of this markdown.
type IpynbConverter struct{}

// Name identifies the converter in errors and in `anymd --list`.
func (c *IpynbConverter) Name() string { return "ipynb" }

// Accepts recognizes a notebook by extension or mime, or by the cheap sniff of
// a JSON opening brace plus an "nbformat" key in the head. It never parses a
// whole file here.
func (c *IpynbConverter) Accepts(r io.ReadSeeker, info StreamInfo, opts *Options) bool {
	hinted := info.HasExt(".ipynb") || info.NormalizedMime() == "application/x-ipynb+json"
	head, complete := ipynbHead(r)
	head = bytes.TrimPrefix(head, []byte{0xEF, 0xBB, 0xBF})
	if !jsonLooksLikeStart(head) || head[firstNonSpace(head)] != '{' {
		return false
	}
	if !bytes.Contains(head, []byte(`"nbformat"`)) {
		// The marker may sit past the sniff window; only a hint saves it then.
		return hinted && !complete
	}
	if complete {
		return json.Valid(head)
	}
	return true
}

// firstNonSpace returns the index of the first non-whitespace byte, or 0 for an
// all-space slice (callers have already established the slice is non-empty and
// starts a JSON value).
func firstNonSpace(b []byte) int {
	for i, ch := range b {
		switch ch {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return i
		}
	}
	return 0
}

// ipynbHead reads at most ipynbSniffBytes and reports whether that was all.
func ipynbHead(r io.ReadSeeker) (head []byte, complete bool) {
	b, err := io.ReadAll(io.LimitReader(r, ipynbSniffBytes+1))
	if err != nil || len(b) == 0 {
		return nil, false
	}
	if len(b) > ipynbSniffBytes {
		return b[:ipynbSniffBytes], false
	}
	return b, true
}

// nbNotebook is the subset of nbformat this converter reads.
type nbNotebook struct {
	NBFormat *int `json:"nbformat"`
	Metadata struct {
		Title      string `json:"title"`
		Kernelspec struct {
			Language string `json:"language"`
		} `json:"kernelspec"`
	} `json:"metadata"`
	Cells []nbCell `json:"cells"`
}

// nbCell is one notebook cell. Source is raw because nbformat allows both a
// string and an array of strings, and both appear in real notebooks.
type nbCell struct {
	CellType string          `json:"cell_type"`
	Source   json.RawMessage `json:"source"`
	Outputs  []nbOutput      `json:"outputs"`
}

// nbOutput is one cell output.
type nbOutput struct {
	OutputType string                     `json:"output_type"`
	Text       json.RawMessage            `json:"text"`
	Data       map[string]json.RawMessage `json:"data"`
}

// Convert renders the notebook.
func (c *IpynbConverter) Convert(r io.ReadSeeker, info StreamInfo, opts *Options) (Result, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxIpynbBytes+1))
	if err != nil {
		return Result{}, err
	}
	if len(raw) > maxIpynbBytes {
		return Result{}, fmt.Errorf("input exceeds the %d-byte limit", maxIpynbBytes)
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})

	var nb nbNotebook
	if err := json.Unmarshal(raw, &nb); err != nil {
		return Result{}, fmt.Errorf("malformed notebook: %w", err)
	}
	if nb.NBFormat == nil {
		return Result{}, fmt.Errorf("not a notebook: no nbformat key")
	}

	lang := strings.TrimSpace(nb.Metadata.Kernelspec.Language)
	if lang == "" {
		lang = "python"
	}

	var blocks []string
	for _, cell := range nb.Cells {
		src := nbSource(cell.Source)
		switch cell.CellType {
		case "markdown", "raw":
			blocks = append(blocks, strings.TrimRight(src, "\n"))
		case "code":
			if strings.TrimSpace(src) != "" {
				blocks = append(blocks, mdutil.CodeBlock(lang, src))
			}
			blocks = append(blocks, nbOutputBlocks(cell.Outputs)...)
		}
	}
	return Result{Markdown: mdutil.Join(blocks...), Title: strings.TrimSpace(nb.Metadata.Title)}, nil
}

// nbOutputBlocks renders the textual outputs of a cell. display_data (charts,
// images) and any binary mime type are skipped; stream text and the text/plain
// form of an execute_result are kept.
func nbOutputBlocks(outs []nbOutput) []string {
	var blocks []string
	for _, out := range outs {
		var text string
		switch out.OutputType {
		case "stream":
			text = nbSource(out.Text)
		case "execute_result":
			if v, ok := out.Data["text/plain"]; ok {
				text = nbSource(v)
			}
		default:
			continue // display_data, error, and anything unknown
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		blocks = append(blocks, mdutil.CodeBlock("", text))
	}
	return blocks
}

// nbSource decodes an nbformat multiline value, which is legally either a
// string or an array of string fragments to be concatenated.
func nbSource(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []string
	if err := json.Unmarshal(raw, &parts); err == nil {
		return strings.Join(parts, "")
	}
	return ""
}
