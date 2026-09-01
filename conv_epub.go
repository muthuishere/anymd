package anymd

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"

	"github.com/muthuishere/anymd/internal/mdutil"
)

func init() { addBuiltin(&EPUBConverter{}) }

const (
	// epubZipMediaType is the exact payload of the uncompressed "mimetype" entry
	// that every conforming epub stores first in the archive.
	epubZipMediaType = "application/epub+zip"
	// epubPartMax caps a single spine part. A book chapter is kilobytes; a
	// 64 MiB "chapter" is an attack, and io.LimitReader is what stops it from
	// becoming a decompression bomb.
	epubPartMax = 64 << 20
	// epubXMLMax caps the small control files (container.xml, the OPF).
	epubXMLMax = 16 << 20
	// epubZipMax caps the whole archive we will buffer.
	epubZipMax = 512 << 20
)

// EPUBConverter renders an EPUB as Markdown by walking the spine in reading
// order and converting each XHTML part through the shared HTML path.
//
// It is implemented on archive/zip plus encoding/xml — no epub library — so
// the module keeps its "pure Go, nothing to ship" promise.
type EPUBConverter struct{}

// Name implements Named.
func (c *EPUBConverter) Name() string { return "epub" }

// Accepts recognizes an epub from its extension, its mime type, or the
// self-identifying "mimetype" entry the spec requires at the front of the
// archive. That last check is what lets a bare, unnamed stream be recognized
// without unzipping anything.
func (c *EPUBConverter) Accepts(r io.ReadSeeker, info StreamInfo, opts *Options) bool {
	if info.HasExt(".epub") {
		return true
	}
	if info.NormalizedMime() == epubZipMediaType {
		return true
	}
	// The spec: first entry, stored (not deflated), named "mimetype", holding
	// exactly "application/epub+zip". So the string sits at a fixed offset in
	// the local file header region — a 128-byte read is enough.
	var head [128]byte
	n, _ := io.ReadFull(r, head[:])
	if n < 4 {
		return false
	}
	b := head[:n]
	if string(b[:4]) != "PK\x03\x04" {
		return false
	}
	return strings.Contains(string(b), "mimetype"+epubZipMediaType)
}

// Convert reads container.xml, locates the OPF package, and emits the title,
// a short metadata block, and every spine part in order.
func (c *EPUBConverter) Convert(r io.ReadSeeker, info StreamInfo, opts *Options) (res Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			res, err = Result{}, fmt.Errorf("epub: panicked: %v", p)
		}
	}()

	size, err := epubSeekSize(r)
	if err != nil {
		return Result{}, err
	}
	if size > epubZipMax {
		return Result{}, fmt.Errorf("epub: archive exceeds %d bytes", epubZipMax)
	}
	zr, err := zip.NewReader(epubNewReaderAt(r), size)
	if err != nil {
		return Result{}, fmt.Errorf("epub: %w", err)
	}

	files := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		// Normalize separators and reject anything that escapes the archive
		// root. A zip entry name is attacker-controlled; "../../etc/passwd"
		// must never become a lookup we honor.
		name := epubCleanName(f.Name)
		if name == "" {
			continue
		}
		if _, dup := files[name]; !dup {
			files[name] = f
		}
	}

	opfPath, err := epubOPFPath(files)
	if err != nil {
		return Result{}, err
	}
	pkg, err := epubPackage(files, opfPath)
	if err != nil {
		return Result{}, err
	}

	title := mdutil.Collapse(epubFirst(pkg.Metadata.Title))
	blocks := []string{mdutil.Heading(1, title)}
	if meta := epubMetaBlock(pkg); meta != "" {
		blocks = append(blocks, meta)
	}

	// Part hrefs are relative to the OPF's own directory, not the archive
	// root. An OPF at "OEBPS/content.opf" with href "text/ch1.xhtml" means the
	// entry "OEBPS/text/ch1.xhtml". Getting this wrong is THE classic epub bug.
	opfDir := path.Dir(opfPath)
	if opfDir == "." {
		opfDir = ""
	}

	byID := make(map[string]epubManifestItem, len(pkg.Manifest.Items))
	for _, it := range pkg.Manifest.Items {
		byID[it.ID] = it
	}

	for _, ref := range pkg.Spine.ItemRefs {
		it, ok := byID[ref.IDRef]
		if !ok || !epubIsDocument(it) {
			continue
		}
		name := epubResolveHref(opfDir, it.Href)
		f, ok := files[name]
		if !ok {
			continue
		}
		src, err := epubReadEntry(f, epubPartMax)
		if err != nil {
			return Result{}, fmt.Errorf("epub: %s: %w", name, err)
		}
		// Parts declare their own encoding; go through the same decoder the
		// HTML converter uses so a Latin-1 chapter is not mojibake.
		md, err := HTMLToMarkdown(DecodeHTMLBytes(src, ""), info.URL)
		if err != nil {
			return Result{}, fmt.Errorf("epub: %s: %w", name, err)
		}
		blocks = append(blocks, md)
	}

	return Result{Markdown: mdutil.Join(blocks...), Title: title}, nil
}

// epubMetaBlock renders the creator / language / date lines the OPF carried.
func epubMetaBlock(pkg *epubOPF) string {
	var lines []string
	if v := mdutil.Collapse(strings.Join(pkg.Metadata.Creator, ", ")); v != "" {
		lines = append(lines, "- **Author:** "+v)
	}
	if v := mdutil.Collapse(epubFirst(pkg.Metadata.Language)); v != "" {
		lines = append(lines, "- **Language:** "+v)
	}
	if v := mdutil.Collapse(epubFirst(pkg.Metadata.Date)); v != "" {
		lines = append(lines, "- **Date:** "+v)
	}
	return strings.Join(lines, "\n")
}

// epubIsDocument reports whether a manifest item is a content document worth
// converting. The spine also references things like the NCX, which is not.
func epubIsDocument(it epubManifestItem) bool {
	switch strings.ToLower(strings.TrimSpace(it.MediaType)) {
	case "application/xhtml+xml", "text/html":
		return true
	case "":
		ext := strings.ToLower(path.Ext(it.Href))
		return ext == ".xhtml" || ext == ".html" || ext == ".htm"
	}
	return false
}

// --- container.xml / OPF ---------------------------------------------------

type epubContainer struct {
	Rootfiles []struct {
		FullPath  string `xml:"full-path,attr"`
		MediaType string `xml:"media-type,attr"`
	} `xml:"rootfiles>rootfile"`
}

type epubManifestItem struct {
	ID        string `xml:"id,attr"`
	Href      string `xml:"href,attr"`
	MediaType string `xml:"media-type,attr"`
}

// epubOPF is the package document. Field tags use bare local names so they
// match regardless of which namespace prefix the publisher chose (dc:title,
// DC:title, …).
type epubOPF struct {
	Metadata struct {
		Title    []string `xml:"title"`
		Creator  []string `xml:"creator"`
		Language []string `xml:"language"`
		Date     []string `xml:"date"`
	} `xml:"metadata"`
	Manifest struct {
		Items []epubManifestItem `xml:"item"`
	} `xml:"manifest"`
	Spine struct {
		ItemRefs []struct {
			IDRef string `xml:"idref,attr"`
		} `xml:"itemref"`
	} `xml:"spine"`
}

func epubOPFPath(files map[string]*zip.File) (string, error) {
	f, ok := files["META-INF/container.xml"]
	if !ok {
		return "", fmt.Errorf("epub: META-INF/container.xml missing")
	}
	b, err := epubReadEntry(f, epubXMLMax)
	if err != nil {
		return "", fmt.Errorf("epub: container.xml: %w", err)
	}
	var c epubContainer
	if err := xml.Unmarshal(b, &c); err != nil {
		return "", fmt.Errorf("epub: container.xml: %w", err)
	}
	for _, rf := range c.Rootfiles {
		if p := epubCleanName(rf.FullPath); p != "" {
			return p, nil
		}
	}
	return "", fmt.Errorf("epub: container.xml declares no rootfile")
}

func epubPackage(files map[string]*zip.File, opfPath string) (*epubOPF, error) {
	f, ok := files[opfPath]
	if !ok {
		return nil, fmt.Errorf("epub: package document %q missing", opfPath)
	}
	b, err := epubReadEntry(f, epubXMLMax)
	if err != nil {
		return nil, fmt.Errorf("epub: %s: %w", opfPath, err)
	}
	var pkg epubOPF
	if err := xml.Unmarshal(b, &pkg); err != nil {
		return nil, fmt.Errorf("epub: %s: %w", opfPath, err)
	}
	return &pkg, nil
}

// --- small helpers ---------------------------------------------------------

// epubCleanName normalizes a zip entry or href to a root-relative slash path,
// returning "" for anything that tries to escape the archive.
func epubCleanName(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	name = strings.TrimPrefix(name, "./")
	if name == "" || strings.HasPrefix(name, "/") {
		return ""
	}
	name = path.Clean(name)
	if name == "." || name == ".." || strings.HasPrefix(name, "../") {
		return ""
	}
	return name
}

// epubResolveHref joins an OPF-relative href onto the OPF's directory. The
// href is a URI reference, so percent-escapes are decoded and any fragment or
// query is dropped before it becomes a zip entry name.
func epubResolveHref(dir, href string) string {
	href = strings.TrimSpace(href)
	if i := strings.IndexAny(href, "#?"); i >= 0 {
		href = href[:i]
	}
	if decoded, err := url.PathUnescape(href); err == nil {
		href = decoded
	}
	if href == "" {
		return ""
	}
	if dir != "" {
		href = dir + "/" + href
	}
	return epubCleanName(href)
}

// epubReadEntry decompresses one entry, hard-capped at max bytes so a zip bomb
// cannot exhaust memory.
func epubReadEntry(f *zip.File, max int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("entry exceeds %d bytes", max)
	}
	return b, nil
}

func epubFirst(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[0]
}

// epubSeekSize measures a ReadSeeker without consuming it.
func epubSeekSize(r io.ReadSeeker) (int64, error) {
	cur, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	end, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, err := r.Seek(cur, io.SeekStart); err != nil {
		return 0, err
	}
	return end, nil
}

// epubSeekReaderAt adapts an io.ReadSeeker to io.ReaderAt, which archive/zip
// requires. It is not safe for concurrent use, which matches how the engine
// hands a stream to exactly one converter at a time.
type epubSeekReaderAt struct{ rs io.ReadSeeker }

func epubNewReaderAt(rs io.ReadSeeker) io.ReaderAt {
	if ra, ok := rs.(io.ReaderAt); ok {
		return ra
	}
	return &epubSeekReaderAt{rs: rs}
}

func (s *epubSeekReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if _, err := s.rs.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	return io.ReadFull(s.rs, p)
}
