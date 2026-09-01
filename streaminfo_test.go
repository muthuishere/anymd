package anymd

import "testing"

func TestStreamInfoNormalizedMime(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain", "application/pdf", "application/pdf"},
		{"uppercased", "APPLICATION/PDF", "application/pdf"},
		{"charset parameter stripped", "text/html; charset=UTF-8", "text/html"},
		{"multiple parameters stripped", "text/plain; charset=utf-8; boundary=x", "text/plain"},
		{"parameter with no space", "text/csv;charset=utf-8", "text/csv"},
		{"surrounding space trimmed", "  text/plain  ", "text/plain"},
		{"space before the semicolon trimmed", "text/plain ; charset=utf-8", "text/plain"},
		{"leading semicolon", "; charset=utf-8", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (StreamInfo{MimeType: tt.in}).NormalizedMime(); got != tt.want {
				t.Errorf("NormalizedMime(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestStreamInfoExt(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty stays empty", "", ""},
		{"whitespace only stays empty", "   ", ""},
		{"already dotted", ".pdf", ".pdf"},
		{"leading dot added", "pdf", ".pdf"},
		{"lowercased", ".PDF", ".pdf"},
		{"lowercased and dotted", "PDF", ".pdf"},
		{"space trimmed", "  .Docx ", ".docx"},
		{"double extension kept whole", ".tar.gz", ".tar.gz"},
		{"a bare dot", ".", "."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (StreamInfo{Extension: tt.in}).Ext(); got != tt.want {
				t.Errorf("Ext(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestStreamInfoHasExt covers the empty-hint case explicitly: HasExt on an
// absent extension must be false. If it were true (or if a caller passed no
// exts and got true), every converter would claim every file and dispatch would
// collapse to "first registered wins".
func TestStreamInfoHasExt(t *testing.T) {
	tests := []struct {
		name string
		info StreamInfo
		exts []string
		want bool
	}{
		{"empty hint is never a match", StreamInfo{}, []string{".zip", ".txt"}, false},
		{"empty hint against an empty ext", StreamInfo{}, []string{""}, false},
		{"whitespace hint is never a match", StreamInfo{Extension: "  "}, []string{".zip"}, false},
		{"no candidates is never a match", StreamInfo{Extension: ".zip"}, nil, false},
		{"exact match", StreamInfo{Extension: ".zip"}, []string{".zip"}, true},
		{"match on the second candidate", StreamInfo{Extension: ".txt"}, []string{".zip", ".txt"}, true},
		{"hint case is ignored", StreamInfo{Extension: ".ZIP"}, []string{".zip"}, true},
		{"candidate case is ignored", StreamInfo{Extension: ".zip"}, []string{".ZIP"}, true},
		{"undotted hint still matches", StreamInfo{Extension: "zip"}, []string{".zip"}, true},
		{"no match", StreamInfo{Extension: ".zip"}, []string{".docx"}, false},
		{"substring is not a match", StreamInfo{Extension: ".zipx"}, []string{".zip"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.info.HasExt(tt.exts...); got != tt.want {
				t.Errorf("HasExt(%q, %v) = %v, want %v", tt.info.Extension, tt.exts, got, tt.want)
			}
		})
	}
}

// TestStreamInfoHasMimePrefix mirrors HasExt: an absent mime hint must not
// match, not even against the empty prefix, which every string starts with.
func TestStreamInfoHasMimePrefix(t *testing.T) {
	tests := []struct {
		name     string
		info     StreamInfo
		prefixes []string
		want     bool
	}{
		{"empty hint is never a match", StreamInfo{}, []string{"text/"}, false},
		{"empty hint against an empty prefix", StreamInfo{}, []string{""}, false},
		{"no candidates is never a match", StreamInfo{MimeType: "text/plain"}, nil, false},
		{"family prefix", StreamInfo{MimeType: "text/plain"}, []string{"text/"}, true},
		{"parameters ignored", StreamInfo{MimeType: "text/html; charset=utf-8"}, []string{"text/html"}, true},
		{"hint case ignored", StreamInfo{MimeType: "TEXT/PLAIN"}, []string{"text/"}, true},
		{"prefix case ignored", StreamInfo{MimeType: "text/plain"}, []string{"TEXT/"}, true},
		{"match on the second candidate", StreamInfo{MimeType: "application/zip"}, []string{"text/", "application/zip"}, true},
		{"no match", StreamInfo{MimeType: "application/zip"}, []string{"text/"}, false},
		{"prefix only, not substring", StreamInfo{MimeType: "application/zip"}, []string{"zip"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.info.HasMimePrefix(tt.prefixes...); got != tt.want {
				t.Errorf("HasMimePrefix(%q, %v) = %v, want %v", tt.info.MimeType, tt.prefixes, got, tt.want)
			}
		})
	}
}

// TestStreamInfoCopyAndUpdate: only non-empty fields of the argument win, so a
// container can hand a child what it knows without erasing what it does not.
func TestStreamInfoCopyAndUpdate(t *testing.T) {
	base := StreamInfo{
		MimeType:  "application/zip",
		Extension: ".zip",
		Charset:   "utf-8",
		FileName:  "bundle.zip",
		URL:       "https://example.test/bundle.zip",
	}

	tests := []struct {
		name  string
		other StreamInfo
		want  StreamInfo
	}{
		{"empty other changes nothing", StreamInfo{}, base},
		{
			name:  "one field overwritten",
			other: StreamInfo{MimeType: "text/plain"},
			want:  StreamInfo{MimeType: "text/plain", Extension: ".zip", Charset: "utf-8", FileName: "bundle.zip", URL: base.URL},
		},
		{
			name:  "every field overwritten",
			other: StreamInfo{MimeType: "text/csv", Extension: ".csv", Charset: "latin1", FileName: "a.csv", URL: "https://example.test/a.csv"},
			want:  StreamInfo{MimeType: "text/csv", Extension: ".csv", Charset: "latin1", FileName: "a.csv", URL: "https://example.test/a.csv"},
		},
		{
			name:  "empty fields of other do not clear the base",
			other: StreamInfo{Extension: ".csv", URL: ""},
			want:  StreamInfo{MimeType: "application/zip", Extension: ".csv", Charset: "utf-8", FileName: "bundle.zip", URL: base.URL},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := base.CopyAndUpdate(tt.other)
			if got != tt.want {
				t.Errorf("CopyAndUpdate() = %+v, want %+v", got, tt.want)
			}
			// The receiver is a value; updating must not touch the original.
			if base.MimeType != "application/zip" || base.Extension != ".zip" {
				t.Errorf("CopyAndUpdate mutated its receiver: %+v", base)
			}
		})
	}
}

func TestStreamInfoForFile(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		wantExt  string
		wantName string
		wantMime string // NormalizedMime, "" means "must be empty"
	}{
		{name: "plain name", path: "report.pdf", wantExt: ".pdf", wantName: "report.pdf", wantMime: "application/pdf"},
		{name: "nested path", path: "/tmp/a/b/report.PDF", wantExt: ".pdf", wantName: "report.PDF", wantMime: "application/pdf"},
		{name: "html", path: "index.html", wantExt: ".html", wantName: "index.html", wantMime: "text/html"},
		{name: "no extension", path: "/usr/local/bin/README", wantExt: "", wantName: "README", wantMime: ""},
		{name: "no extension, bare name", path: "Makefile", wantExt: "", wantName: "Makefile", wantMime: ""},
		{name: "unknown extension", path: "blob.zzz", wantExt: ".zzz", wantName: "blob.zzz", wantMime: ""},
		{name: "trailing dot", path: "weird.", wantExt: ".", wantName: "weird.", wantMime: ""},
		// A dotfile has no extension in the everyday sense, but filepath.Ext
		// treats the whole name as one. Pinned so the surprise is documented.
		{name: "dotfile", path: ".gitignore", wantExt: ".gitignore", wantName: ".gitignore", wantMime: ""},
		{name: "dotfile with a real extension", path: ".env.local", wantExt: ".local", wantName: ".env.local", wantMime: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StreamInfoForFile(tt.path)
			if got.Extension != tt.wantExt {
				t.Errorf("Extension = %q, want %q", got.Extension, tt.wantExt)
			}
			if got.Ext() != tt.wantExt {
				t.Errorf("Ext() = %q, want %q", got.Ext(), tt.wantExt)
			}
			if got.FileName != tt.wantName {
				t.Errorf("FileName = %q, want %q", got.FileName, tt.wantName)
			}
			if got.NormalizedMime() != tt.wantMime {
				t.Errorf("NormalizedMime() = %q, want %q", got.NormalizedMime(), tt.wantMime)
			}
			if got.Charset != "" || got.URL != "" {
				t.Errorf("a path cannot know Charset/URL, got %+v", got)
			}
		})
	}
}
