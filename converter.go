package anymd

import "io"

// Result is what a converter produces.
type Result struct {
	// Markdown is the converted body.
	Markdown string
	// Title is an optional document title (docx core properties, <title>, …).
	Title string
}

// String returns the markdown body, so a Result can be printed directly.
func (r Result) String() string { return r.Markdown }

// Converter turns one family of formats into Markdown.
//
// The contract is markitdown's two-method shape:
//
//	Accepts — cheap, hint-and-sniff only. It may read from r freely to sniff
//	          magic bytes and need not rewind: the engine seeks r back to 0
//	          before every Accepts and before Convert.
//	Convert — the real work. r is rewound to 0 before the call.
//
// Accepts must not be expensive: it runs against every registered converter.
type Converter interface {
	Accepts(r io.ReadSeeker, info StreamInfo, opts *Options) bool
	Convert(r io.ReadSeeker, info StreamInfo, opts *Options) (Result, error)
}

// Named is an optional interface: a converter that reports its own name gets
// that name in error messages and in `anymd --list`. Converters that do not
// implement it fall back to their Go type name.
type Named interface {
	Name() string
}

// Priority orders converters within the registry. Lower runs first.
//
// The engine tries converters in ascending priority, and the first whose
// Accepts returns true wins. Specific formats claim a low number; catch-alls
// that would swallow anything claim a high one.
const (
	// PrioritySpecific is for converters keyed to an unambiguous magic number
	// or a unique extension (docx, pdf, xlsx, …). Default for new converters.
	PrioritySpecific = 0
	// PriorityGeneric is for converters that recognize a broad family and
	// would otherwise shadow a specific one (html, zip-as-container, …).
	PriorityGeneric = 10
	// PriorityFallback is for the last-resort text converter, which accepts
	// anything that decodes as text. Exactly one converter should sit here.
	PriorityFallback = 100
)

// Prioritized is an optional interface; a converter that does not implement it
// is registered at PrioritySpecific.
type Prioritized interface {
	Priority() int
}
