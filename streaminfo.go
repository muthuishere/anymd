package anymd

import (
	"mime"
	"path/filepath"
	"strings"
)

// StreamInfo carries everything known about a byte stream before conversion.
// Every field is a HINT and may be empty: converters must tolerate a bare
// stream and decide from content when the hints are absent.
//
// Mirrors markitdown's StreamInfo (_stream_info.py) so the mental model ports
// across languages.
type StreamInfo struct {
	// MimeType, e.g. "application/pdf". Parameters are stripped by NormalizedMime.
	MimeType string
	// Extension including the leading dot, lowercased, e.g. ".pdf".
	Extension string
	// Charset, e.g. "utf-8". Empty means unknown.
	Charset string
	// FileName is the base name, when the stream came from a file.
	FileName string
	// URL is the origin URL, when the stream came from the network. Some
	// converters (feeds, wiki exports) key off it.
	URL string
}

// NormalizedMime returns MimeType lowercased with any ";" parameters removed.
func (s StreamInfo) NormalizedMime() string {
	m := s.MimeType
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = m[:i]
	}
	return strings.ToLower(strings.TrimSpace(m))
}

// Ext returns Extension lowercased with a guaranteed leading dot ("" stays "").
func (s StreamInfo) Ext() string {
	e := strings.ToLower(strings.TrimSpace(s.Extension))
	if e == "" {
		return ""
	}
	if !strings.HasPrefix(e, ".") {
		e = "." + e
	}
	return e
}

// HasExt reports whether the extension hint matches any of exts (each with a
// leading dot).
func (s StreamInfo) HasExt(exts ...string) bool {
	e := s.Ext()
	if e == "" {
		return false
	}
	for _, want := range exts {
		if e == strings.ToLower(want) {
			return true
		}
	}
	return false
}

// HasMimePrefix reports whether the normalized mime type starts with any prefix.
func (s StreamInfo) HasMimePrefix(prefixes ...string) bool {
	m := s.NormalizedMime()
	if m == "" {
		return false
	}
	for _, p := range prefixes {
		if strings.HasPrefix(m, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// CopyAndUpdate returns a copy with every non-empty field of other applied.
func (s StreamInfo) CopyAndUpdate(other StreamInfo) StreamInfo {
	out := s
	if other.MimeType != "" {
		out.MimeType = other.MimeType
	}
	if other.Extension != "" {
		out.Extension = other.Extension
	}
	if other.Charset != "" {
		out.Charset = other.Charset
	}
	if other.FileName != "" {
		out.FileName = other.FileName
	}
	if other.URL != "" {
		out.URL = other.URL
	}
	return out
}

// StreamInfoForFile builds the hints derivable from a path alone.
func StreamInfoForFile(path string) StreamInfo {
	ext := strings.ToLower(filepath.Ext(path))
	return StreamInfo{
		Extension: ext,
		FileName:  filepath.Base(path),
		MimeType:  mime.TypeByExtension(ext),
	}
}
