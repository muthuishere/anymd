package anymd

import (
	"errors"
	"io"
	"time"
)

// ErrMaxDepth is returned when a container converter (zip, epub, mail with
// attachments) would recurse past Options.MaxDepth.
var ErrMaxDepth = errors.New("anymd: max recursion depth exceeded")

// Options tunes a conversion. The zero value is valid and gives markitdown's
// defaults.
type Options struct {
	// MaxDepth bounds container recursion (a zip inside a zip …). 0 means the
	// default of 8. Negative disables recursion entirely.
	MaxDepth int

	// KeepDataURIs keeps base64 image payloads inline as data: URIs instead of
	// dropping them to an empty ![](). Matches markitdown's keep_data_uris.
	KeepDataURIs bool

	// Charset overrides the detected encoding for text-ish formats.
	Charset string

	// Cache, when non-nil, serves and stores conversions content-addressed.
	// Nil (the default) disables caching entirely: a library must not write to
	// a caller's disk unasked, and a one-shot conversion pays the hash for
	// nothing. See CacheKey for what the key covers — notably the anymd
	// version, so an upgrade invalidates rather than serving stale output.
	Cache Cache

	// Describer, when non-nil, is used to caption images and to read pages that
	// have no text layer. Nil (the default) means anymd makes NO network calls
	// during conversion — see the Describer docs.
	Describer Describer

	// Transcriber, when non-nil, is used to convert audio to text. Nil (the
	// default) means audio formats are unsupported.
	Transcriber Transcriber

	// LLMTimeout bounds a single Describer or Transcriber call. Zero means
	// 60s. A slow model must not be able to stall a whole document.
	LLMTimeout time.Duration

	// engine and depth are engine-managed; converters read them through
	// Recurse and never set them.
	engine *Engine
	depth  int
}

func (o *Options) maxDepth() int {
	if o == nil || o.MaxDepth == 0 {
		return 8
	}
	return o.MaxDepth
}

// Recurse converts a nested stream with the same engine and options, tracking
// depth. Container converters (zip, epub, msg) MUST use this rather than
// building their own Engine, so MaxDepth is actually enforced.
func (o *Options) Recurse(r io.ReadSeeker, info StreamInfo) (Result, error) {
	if o == nil || o.engine == nil {
		return Result{}, errors.New("anymd: Recurse called outside a conversion")
	}
	if o.MaxDepth < 0 || o.depth+1 > o.maxDepth() {
		return Result{}, ErrMaxDepth
	}
	child := *o
	child.depth = o.depth + 1
	return o.engine.convert(r, info, &child)
}

// Depth reports how many containers deep the current conversion is (0 at the
// top level).
func (o *Options) Depth() int {
	if o == nil {
		return 0
	}
	return o.depth
}

// llmTimeout returns the per-call budget for a Describer or Transcriber.
func (o *Options) llmTimeout() time.Duration {
	if o == nil || o.LLMTimeout <= 0 {
		return 60 * time.Second
	}
	return o.LLMTimeout
}
