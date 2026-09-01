package anymd

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/muthuishere/anymd/internal/mdutil"
)

func init() { addBuiltin(&ZipConverter{}) }

// Zip-bomb bounds. They are vars rather than consts only so tests can lower
// them; nothing outside this package can reach them.
//
// The cumulative cap is the one that actually stops a bomb: a per-entry cap
// alone still lets 10,000 entries of 64 MiB each expand to 640 GiB.
var (
	maxZipEntries = 10000
	maxEntryBytes = int64(64) << 20  // 64 MiB per member
	maxTotalBytes = int64(512) << 20 // 512 MiB decompressed across the archive
)

// Marker entries that identify a zip as something more specific than "a zip".
// A zip containing any of these belongs to a sibling converter, so ZipConverter
// declines it rather than shadowing them — docx, pptx, xlsx and epub are all
// zip files, and this converter sits at PriorityGeneric, ahead of nothing but
// the fallback.
var zipFormatMarkers = []string{
	"word/document.xml",    // docx
	"ppt/presentation.xml", // pptx
	"xl/workbook.xml",      // xlsx
}

// epubMimetype is the exact body of an epub's uncompressed "mimetype" entry.
const epubMimetype = "application/epub+zip"

// ZipConverter renders a zip archive as one Markdown document: an H2 per
// member, followed by that member converted through the same engine.
//
// It is deliberately generic. A zip is a bag of unrelated things, so a member
// that fails to convert is reported inline and the walk continues — losing 99
// good files because the 100th was corrupt would be the wrong trade.
type ZipConverter struct{}

// Name identifies the converter in errors and in `anymd --list`.
func (c *ZipConverter) Name() string { return "zip" }

// Priority is PriorityGeneric: "it is a zip" is a broad claim, and the
// specific zip-based formats must get first refusal.
func (c *ZipConverter) Priority() int { return PriorityGeneric }

// Accepts reports whether this is a zip that no more specific converter owns.
//
// It reads the central directory, which is a bounded tail read rather than a
// walk of the members, so the "Accepts is cheap" rule still holds: no entry is
// decompressed except an epub's ~20-byte "mimetype".
func (c *ZipConverter) Accepts(r io.ReadSeeker, info StreamInfo, opts *Options) bool {
	var magic [4]byte
	if n, _ := io.ReadFull(r, magic[:]); n < 4 || !isZipMagic(magic[:]) {
		return false
	}
	zr, err := zipReaderFor(r)
	if err != nil {
		// Truncated or corrupt. Claim it only when the hints say "zip", so the
		// user gets a real parse error instead of a bare ErrUnsupported; a
		// stray PK-prefixed blob is left to whoever else wants it.
		return info.HasExt(".zip") || info.HasMimePrefix("application/zip", "application/x-zip-compressed")
	}
	return !isKnownZipFormat(zr)
}

// Convert walks the archive in its stored order, emitting a heading and the
// converted body for each member.
func (c *ZipConverter) Convert(r io.ReadSeeker, info StreamInfo, opts *Options) (Result, error) {
	zr, err := zipReaderFor(r)
	if err != nil {
		return Result{}, fmt.Errorf("read zip: %w", err)
	}
	if len(zr.File) > maxZipEntries {
		return Result{}, fmt.Errorf("zip has %d entries, over the %d limit", len(zr.File), maxZipEntries)
	}

	budget := maxTotalBytes
	var blocks []string
	for _, f := range zr.File {
		name := f.Name
		if f.FileInfo().IsDir() || strings.HasSuffix(name, "/") {
			continue
		}
		if unsafeEntryName(name) {
			blocks = append(blocks, note(name, "unsafe entry path"))
			continue
		}

		body, n, err := readEntry(f, budget)
		if err != nil {
			if err == errArchiveTooLarge {
				return Result{}, fmt.Errorf("decompressed size exceeds the %d byte limit", maxTotalBytes)
			}
			blocks = append(blocks, note(name, err.Error()))
			continue
		}
		budget -= n
		if len(body) == 0 {
			continue // zero-byte member: nothing to say about it
		}

		sub, err := opts.Recurse(bytes.NewReader(body), StreamInfoForFile(name))
		if err != nil {
			// Includes ErrMaxDepth: too deep is a fact about this member, not a
			// reason to abandon its siblings.
			blocks = append(blocks, note(name, err.Error()))
			continue
		}
		blocks = append(blocks, mdutil.Heading(2, name), sub.Markdown)
	}
	return Result{Markdown: mdutil.Join(blocks...), Title: info.FileName}, nil
}

// errArchiveTooLarge signals the cumulative decompression budget is spent,
// which unlike a per-member problem must abort the whole archive.
var errArchiveTooLarge = fmt.Errorf("archive decompression budget exhausted")

// readEntry decompresses one member under both the per-entry cap and whatever
// remains of the archive-wide budget.
func readEntry(f *zip.File, budget int64) ([]byte, int64, error) {
	limit := maxEntryBytes
	capIsBudget := false
	if budget < limit {
		limit, capIsBudget = budget, true
	}
	rc, err := f.Open()
	if err != nil {
		return nil, 0, err
	}
	defer rc.Close()

	// Read one byte past the limit so overflow is detectable. Never size the
	// buffer from f.UncompressedSize64: that field is attacker-controlled.
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, 0, err
	}
	if n > limit {
		if capIsBudget {
			return nil, 0, errArchiveTooLarge
		}
		return nil, 0, fmt.Errorf("entry exceeds the %d byte limit", maxEntryBytes)
	}
	return buf.Bytes(), n, nil
}

// note renders a member we could not render, as a heading plus an italic
// reason, so the gap is visible in the output instead of silent.
func note(name, reason string) string {
	return mdutil.Join(
		mdutil.Heading(2, name),
		"*[could not convert: "+mdutil.Collapse(reason)+"]*",
	)
}

// isZipMagic matches the local-file-header and empty-archive signatures.
func isZipMagic(b []byte) bool {
	return len(b) >= 4 && b[0] == 'P' && b[1] == 'K' &&
		((b[2] == 0x03 && b[3] == 0x04) || (b[2] == 0x05 && b[3] == 0x06))
}

// zipReaderFor opens r as a zip, buffering only when r cannot do random access.
func zipReaderFor(r io.ReadSeeker) (*zip.Reader, error) {
	if ra, ok := r.(io.ReaderAt); ok {
		size, err := r.Seek(0, io.SeekEnd)
		if err != nil {
			return nil, err
		}
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return zip.NewReader(ra, size)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(r, maxTotalBytes))
	if err != nil {
		return nil, err
	}
	return zip.NewReader(bytes.NewReader(b), int64(len(b)))
}

// isKnownZipFormat reports whether the archive is really a docx/pptx/xlsx/epub.
func isKnownZipFormat(zr *zip.Reader) bool {
	for _, f := range zr.File {
		name := strings.TrimPrefix(f.Name, "./")
		for _, m := range zipFormatMarkers {
			if name == m {
				return true
			}
		}
		if name == "mimetype" {
			var head [64]byte
			rc, err := f.Open()
			if err != nil {
				continue
			}
			n, _ := io.ReadFull(rc, head[:])
			rc.Close()
			if strings.TrimSpace(string(head[:n])) == epubMimetype {
				return true
			}
		}
	}
	return false
}

// unsafeEntryName rejects names that escape the archive root: absolute paths,
// Windows drive letters, and any ".." component. We never write these to disk,
// but a name that lies about its location is a name we should not repeat.
func unsafeEntryName(name string) bool {
	if name == "" {
		return true
	}
	n := strings.ReplaceAll(name, `\`, "/")
	if strings.HasPrefix(n, "/") || filepath.IsAbs(name) {
		return true
	}
	if len(n) >= 2 && n[1] == ':' {
		return true // C:/... on any host
	}
	for _, part := range strings.Split(n, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}
