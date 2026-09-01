// Package ooxml holds the zip-and-relationships plumbing shared by the Office
// Open XML converters (docx, pptx, and anything else that is a zip of XML
// parts).
//
// Everything here treats the archive as hostile input: part names are
// sanitized before they are indexed, every part read is capped so a zip bomb
// cannot exhaust memory, and a missing or duplicated part is an ordinary error
// rather than a panic.
package ooxml

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
)

// MaxPartSize caps the decompressed size of any single part. OOXML documents
// with a legitimately larger body part do not exist in practice, and the cap is
// what stops a 1 KiB zip from decompressing into all of memory.
const MaxPartSize = 64 << 20

// maxArchiveBuffer caps how much we will buffer in memory when the caller's
// stream is not already an io.ReaderAt (archive/zip needs random access).
const maxArchiveBuffer = 512 << 20

// ErrPartNotFound is returned by Part for a part that is absent from the
// archive (or whose name failed sanitization).
var ErrPartNotFound = errors.New("ooxml: part not found")

// Package is a read-only view of an OOXML archive.
type Package struct {
	zr    *zip.Reader
	files map[string]*zip.File
	names []string
}

// Open reads the zip central directory from r. It does not decompress
// anything, so it is cheap enough to call from a converter's Accepts.
func Open(r io.ReadSeeker) (*Package, error) {
	if r == nil {
		return nil, errors.New("ooxml: nil reader")
	}
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if size <= 0 {
		return nil, errors.New("ooxml: empty stream")
	}

	ra, ok := r.(io.ReaderAt)
	if !ok {
		// Fall back to buffering; bounded, because the stream is untrusted.
		if size > maxArchiveBuffer {
			return nil, fmt.Errorf("ooxml: archive too large to buffer (%d bytes)", size)
		}
		buf, err := io.ReadAll(io.LimitReader(r, maxArchiveBuffer))
		if err != nil {
			return nil, err
		}
		br := bytes.NewReader(buf)
		ra, size = br, int64(len(buf))
	}

	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return nil, err
	}
	p := &Package{zr: zr, files: make(map[string]*zip.File, len(zr.File))}
	for _, f := range zr.File {
		name, ok := cleanName(f.Name)
		if !ok {
			continue // absolute, traversing, or otherwise unusable name
		}
		if _, dup := p.files[name]; dup {
			continue // first entry wins; a duplicate is a known zip-confusion trick
		}
		p.files[name] = f
		p.names = append(p.names, name)
	}
	sort.Strings(p.names)
	return p, nil
}

// cleanName normalizes a zip entry name and rejects anything that could escape
// the archive root. We never write these to disk, but a traversing name must
// still not alias a part we look up by a well-known path.
func cleanName(name string) (string, bool) {
	if name == "" || strings.ContainsRune(name, 0) {
		return "", false
	}
	n := strings.ReplaceAll(name, "\\", "/")
	if strings.HasPrefix(n, "/") {
		return "", false
	}
	if strings.HasSuffix(n, "/") {
		return "", false // directory entry
	}
	c := path.Clean(n)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", false
	}
	return c, true
}

// Names returns every usable part name, sorted.
func (p *Package) Names() []string {
	out := make([]string, len(p.names))
	copy(out, p.names)
	return out
}

// Has reports whether the archive contains the named part.
func (p *Package) Has(name string) bool {
	c, ok := cleanName(name)
	if !ok {
		return false
	}
	_, found := p.files[c]
	return found
}

// Part returns a part's bytes, refusing to decompress more than MaxPartSize.
func (p *Package) Part(name string) ([]byte, error) {
	c, ok := cleanName(name)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrPartNotFound, name)
	}
	f, found := p.files[c]
	if !found {
		return nil, fmt.Errorf("%w: %q", ErrPartNotFound, name)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	// Read one byte past the cap so an oversized part is detected rather than
	// silently truncated into malformed XML.
	b, err := io.ReadAll(io.LimitReader(rc, MaxPartSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > MaxPartSize {
		return nil, fmt.Errorf("ooxml: part %q exceeds %d bytes", c, int64(MaxPartSize))
	}
	return b, nil
}

// OptionalPart returns a part's bytes, or nil when it is missing or unreadable.
// Use it for parts whose absence must degrade rather than fail (numbering.xml,
// core.xml, notes slides).
func (p *Package) OptionalPart(name string) []byte {
	b, err := p.Part(name)
	if err != nil {
		return nil
	}
	return b
}

// Rel is one entry of a _rels part.
type Rel struct {
	ID         string
	Type       string
	Target     string
	TargetMode string
}

// External reports whether the relationship points outside the package (a
// hyperlink, typically).
func (r Rel) External() bool { return strings.EqualFold(r.TargetMode, "External") }

// RelsPathFor maps a part name to the path of its relationships part, e.g.
// "word/document.xml" -> "word/_rels/document.xml.rels".
func RelsPathFor(part string) string {
	dir, base := path.Split(part)
	return dir + "_rels/" + base + ".rels"
}

// Relationships parses the _rels part belonging to partName. A missing or
// malformed rels part yields an empty slice, never an error: relationships are
// an enrichment, and losing them should cost a hyperlink, not the document.
func (p *Package) Relationships(partName string) []Rel {
	data := p.OptionalPart(RelsPathFor(partName))
	if len(data) == 0 {
		return nil
	}
	var doc struct {
		Rels []struct {
			ID         string `xml:"Id,attr"`
			Type       string `xml:"Type,attr"`
			Target     string `xml:"Target,attr"`
			TargetMode string `xml:"TargetMode,attr"`
		} `xml:"Relationship"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil
	}
	out := make([]Rel, 0, len(doc.Rels))
	for _, r := range doc.Rels {
		out = append(out, Rel{ID: r.ID, Type: r.Type, Target: r.Target, TargetMode: r.TargetMode})
	}
	return out
}

// RelTargets returns the relationships of partName as an id -> target map.
func (p *Package) RelTargets(partName string) map[string]string {
	rels := p.Relationships(partName)
	if len(rels) == 0 {
		return nil
	}
	m := make(map[string]string, len(rels))
	for _, r := range rels {
		if r.ID == "" {
			continue
		}
		if _, dup := m[r.ID]; dup {
			continue // first wins
		}
		m[r.ID] = r.Target
	}
	return m
}

// ResolveTarget turns a relationship target that is relative to sourcePart's
// directory into a package-absolute part name. External targets (URLs) and
// targets that would escape the package come back unchanged/empty so a caller
// cannot be tricked into reading outside the archive.
func ResolveTarget(sourcePart, target string) string {
	if target == "" {
		return ""
	}
	if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
		return ""
	}
	t := strings.ReplaceAll(target, "\\", "/")
	if strings.HasPrefix(t, "/") {
		t = strings.TrimPrefix(t, "/")
	} else {
		t = path.Join(path.Dir(sourcePart), t)
	}
	c, ok := cleanName(t)
	if !ok {
		return ""
	}
	return c
}

// IsZip reports whether the stream starts with the local-file-header magic
// "PK\x03\x04". It rewinds r to 0 before returning.
func IsZip(r io.ReadSeeker) bool {
	if r == nil {
		return false
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return false
	}
	var magic [4]byte
	n, _ := io.ReadFull(r, magic[:])
	_, _ = r.Seek(0, io.SeekStart)
	return n == 4 && magic == [4]byte{'P', 'K', 0x03, 0x04}
}

// HasAnyPart is the cheap sniff a converter's Accepts should use: check the
// zip magic, read only the central directory, and test for a marker part. It
// never parses XML and it rewinds r before returning.
func HasAnyPart(r io.ReadSeeker, names ...string) bool {
	if !IsZip(r) {
		return false
	}
	p, err := Open(r)
	_, _ = r.Seek(0, io.SeekStart)
	if err != nil {
		return false
	}
	for _, n := range names {
		if p.Has(n) {
			return true
		}
	}
	return false
}

// SkipElement consumes tokens up to and including the end tag matching the
// StartElement the caller has just read. It is iterative on purpose: the input
// nesting depth is attacker-controlled, so recursion here would be a stack
// overflow waiting to happen.
func SkipElement(d *xml.Decoder) error {
	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		switch tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		}
	}
	return nil
}

// TextOf consumes the element the caller has just started and returns its
// direct character data, ignoring any child elements.
func TextOf(d *xml.Decoder) (string, error) {
	var sb strings.Builder
	depth := 1
	for depth > 0 {
		tok, err := d.Token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.CharData:
			if depth == 1 {
				sb.Write(t)
			}
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		}
	}
	return sb.String(), nil
}

// Attr returns the value of the first attribute with the given local name,
// ignoring its namespace prefix. OOXML prefixes vary between producers, so
// matching on the local name is the only stable option.
func Attr(se xml.StartElement, local string) string {
	for _, a := range se.Attr {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

// OnOff reads an OOXML boolean toggle attribute (w:val / val). An absent value
// means "on"; "0", "false" and "off" mean off.
func OnOff(se xml.StartElement) bool {
	v := Attr(se, "val")
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "1", "true", "on":
		return true
	default:
		return false
	}
}

// NewDecoder returns an xml.Decoder configured the way every OOXML part needs:
// tolerant of the odd non-conforming producer, and never reaching the network
// for an entity or a DTD.
func NewDecoder(data []byte) *xml.Decoder {
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = false
	d.Entity = xml.HTMLEntity
	return d
}
