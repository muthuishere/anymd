// Package anymd converts documents of almost any kind into GitHub-flavored
// Markdown, in pure Go. Point it at a .docx, .pdf, .xlsx, .pptx, .epub, .msg,
// .ipynb, .html, .csv, .json, an RSS feed, an image, or a zip full of those,
// and it hands back a Markdown string suitable for an LLM context window, a
// docs pipeline, or a diff.
//
// It is a Go-native answer to Microsoft's markitdown: the same converter
// registry shape, the same "hints plus sniffing" dispatch, and the same
// Markdown-out goal — with no cgo, no Python, and no native library to ship.
//
// # Scope, stated honestly
//
// anymd extracts what is already text in the document. It does not invent text
// that is not there:
//
//	no OCR              — a scanned PDF or a photo of a page yields no prose
//	no transcription    — audio and video are not supported at all
//	no LLM captioning   — an image's pixels are never described
//	no network          — a converter never fetches a remote asset or link
//
// Each of those would require a model, a service, an API key, or a native
// dependency, and would break the one property this library sells. If you want
// them, wrap anymd in your own pipeline, where you control the cost and the
// data boundary.
//
// # The pure-Go promise
//
// This is the reason to pick anymd over a wrapper around a Python tool or a
// native library. There is no cgo anywhere in the dependency graph, no poppler,
// no libmagic, no LibreOffice subprocess, no interpreter. CGO_ENABLED=0 builds
// work, and the module cross-compiles anywhere Go does:
//
//	GOOS=windows GOARCH=arm64 CGO_ENABLED=0 go build ./...
//
// A scratch container or a CI runner needs nothing installed but the binary.
//
// # Quick start
//
// The three-line case, using the package-level helpers backed by [Default]:
//
//	res, err := anymd.ConvertFile("report.docx")
//	if err != nil {
//		log.Fatal(err)
//	}
//	fmt.Println(res.Markdown) // and res.Title, when the format carries one
//
// For bytes you already hold, use [ConvertBytes]; for an arbitrary reader, use
// [Convert]. Each takes a [StreamInfo] of hints — extension, MIME type,
// filename, charset, origin URL — every field optional. With no hints at all,
// dispatch falls back to sniffing the first 512 bytes.
//
// # The converter registry
//
// An [Engine] is a list of [Converter] values, each of which implements two
// methods:
//
//	Accepts — cheap. Hints and magic bytes only, never a full parse: it runs
//	          against every registered converter on every conversion.
//	Convert — the real work. The stream is rewound to 0 before both calls.
//
// Dispatch is deliberately simple. Converters are tried in ascending
// [Prioritized] order — [PrioritySpecific] (0) for an unambiguous magic number
// or extension, [PriorityGeneric] (10) for a family that would otherwise shadow
// a specific format, [PriorityFallback] (100) for the single text catch-all —
// with ties broken by registration order. The first converter whose Accepts
// returns true wins.
//
// A converter that accepts and then fails is a hard error. The engine does NOT
// fall through to the next candidate. That is a design choice, not an
// oversight: falling through means a corrupt .docx quietly comes back as the
// plaintext converter's rendering of compressed XML, which looks like output,
// diffs like output, and is garbage. An error the caller can see beats
// plausible nonsense they cannot.
//
// [Engine.Converters] returns the live registry in dispatch order, which is
// always the authoritative answer for the build you have.
//
// # Options
//
// [Options] is optional everywhere; a nil *Options and the zero value both give
// the defaults. The fields that matter:
//
//	MaxDepth     bounds container recursion (a zip inside a zip inside …).
//	             0 means the default of 8; negative disables recursion.
//	KeepDataURIs keeps base64 image payloads inline instead of dropping them.
//	Charset      overrides the detected encoding for text-ish formats.
//
// Container converters (zip, epub, msg) must recurse through
// [Options.Recurse], never by constructing a fresh Engine. Recurse carries the
// same engine and options down and increments the depth counter, which is what
// makes MaxDepth real rather than advisory. Past the limit it returns
// [ErrMaxDepth]; a container reports that inline against the offending member
// and keeps walking its siblings.
//
// # Errors
//
// When nothing claims a stream, conversion fails with an [UnsupportedError],
// which unwraps to the [ErrUnsupported] sentinel:
//
//	if errors.Is(err, anymd.ErrUnsupported) {
//		var ue *anymd.UnsupportedError
//		errors.As(err, &ue)
//		log.Printf("no converter for ext=%q mime=%q (declined: %v)",
//			ue.Ext, ue.Mime, ue.Declined)
//	}
//
// Format-specific outcomes get their own sentinels:
//
//	ErrNoTextLayer  a PDF parsed cleanly but has no text layer at all
//	ErrEncryptedPDF a PDF is encrypted and would not open with an empty password
//	ErrMaxDepth     a container hit Options.MaxDepth
//
// [ErrNoTextLayer] is the one worth explaining. A pure scan — every page a
// single raster image — could be reported as a successful conversion producing
// "". It is not, because "" is exactly what a genuinely blank document
// produces, and the caller would have no way to tell them apart. The
// distinction is operationally load-bearing: "there is nothing to extract" ends
// the job, while "the text is locked inside images" is a signal to route the
// file to an OCR step that anymd deliberately does not ship. Errors that
// collapse those two cases push the ambiguity onto every caller. Same reasoning
// for [ErrEncryptedPDF]: an encrypted file must never be mistaken for an empty
// one.
//
// # Extending it
//
// A converter is two methods, so a consumer's own format is a small type and
// one Register call:
//
//	type TodoConverter struct{}
//
//	func (TodoConverter) Name() string  { return "todo" }
//	func (TodoConverter) Priority() int { return anymd.PrioritySpecific }
//
//	func (TodoConverter) Accepts(r io.ReadSeeker, info anymd.StreamInfo, o *anymd.Options) bool {
//		return info.HasExt(".todo")
//	}
//
//	func (TodoConverter) Convert(r io.ReadSeeker, info anymd.StreamInfo, o *anymd.Options) (anymd.Result, error) {
//		b, err := io.ReadAll(r)
//		// … render b as Markdown …
//		return anymd.Result{Markdown: string(b)}, err
//	}
//
//	e := anymd.New()             // every built-in
//	e.Register(TodoConverter{})  // yours, ahead of the fallback
//	res, err := e.ConvertFile("chores.todo", nil)
//
// [Named] and [Prioritized] are optional: a converter that implements neither
// is registered at PrioritySpecific under its Go type name. To override a
// built-in, register at a lower priority than it. Register mutates the Engine,
// so do all registration before the first conversion and before sharing the
// Engine across goroutines; once built, an Engine is safe for concurrent use.
//
// # Supported formats
//
//	Plain text / Markdown  .txt .text .md .markdown .log   verbatim (fallback)
//	CSV / TSV              .csv .tsv .tab                  delimiter sniffing → GFM table
//	Excel                  .xlsx .xlsm .xltx .xltm         one heading + table per sheet
//	Excel (legacy BIFF)    .xls .xlt .xlm .xlw             same output as .xlsx
//	Word                   .docx                           headings, lists, tables, links, title
//	PDF                    .pdf                            text layer, page by page (no OCR)
//	HTML                   .html .htm .xhtml               headings, lists, tables, links, code
//	Feeds                  .rss .atom .xml .rdf            feed and entry titles, dates, summaries
//	JSON                   .json                           pretty-printed, fenced
//	Notebooks              .ipynb                          markdown cells, code cells, text outputs
//	EPUB                   .epub                           spine order, chapter by chapter
//	PowerPoint             .pptx                           slide text, tables, charts, notes
//	Images                 .jpg .jpeg .png .gif .webp .tiff .bmp   dimensions and EXIF only
//	Outlook mail           .msg                            subject, header table, body
//	ZIP                    .zip                            each member converted, bounded by MaxDepth
//
// Anything with no matching converter that still decodes as UTF-8 text falls
// through to the plaintext converter. Genuinely binary input that nothing
// claims is an error, never silent garbage.
//
// # Security posture
//
// Every converter is a parser aimed at bytes someone else chose, so:
//
//   - Never panics. Malformed input is an error. Lengths, indices, and offsets
//     read out of a document are treated as attacker-controlled, and a
//     dependency that reports corruption by panicking is wrapped in a recover.
//   - Allocations are bounded. Per-format input caps, cell and glyph caps, and
//     zip-bomb limits (per-entry, archive-wide, and entry-count) mean a small
//     hostile file cannot expand into an unbounded one. A declared
//     uncompressed size is never trusted to size a buffer.
//   - Recursion is bounded centrally by Options.MaxDepth via Options.Recurse.
//   - No network, no subprocess, no shell. A document cannot make anymd phone
//     home. The single network call in the project is the CLI's explicit URL
//     argument, which is a fetch you asked for, resolved before any converter
//     sees a byte.
//   - No cgo, so there is no memory-unsafe parser in the dependency graph.
//
// Archive member names that try to escape the root — absolute paths, drive
// letters, ".." components — are refused rather than repeated into output.
//
// Finding a way to panic a converter is a bug worth reporting.
package anymd
