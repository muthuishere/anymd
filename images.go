package anymd

// Inlining images into the Markdown.
//
// A document's pictures are part of its content. Dropping them to an empty
// `![]()` keeps the position and loses the thing itself, which is fine for a
// decorative rule and useless for the chart the paragraph beneath it is
// discussing. So every converter that can reach an image's bytes inlines them
// as a data: URI, and the Markdown is self-contained: no sidecar directory, no
// relative paths to keep in step, nothing to lose when the file is moved.
//
// The cost is size, and it is not small: base64 is 4 bytes for every 3, so a
// 3 MB photograph becomes 4 MB of Markdown. anymd's usual destination is a
// context window, where that is a real budget rather than a rounding error.
// Hence the caps below. They are deliberately caps and not a silent best
// effort: an image too large to inline still emits its placeholder, so the
// reader can see that something was there.

import (
	"encoding/base64"
	"net/http"
	"strings"
)

// Defaults for the image budget, used when Options leaves them at zero.
const (
	// defaultMaxImageBytes is the largest single image, measured on the raw
	// bytes before encoding. A page-sized scan at a sensible resolution fits;
	// a print-resolution photograph does not.
	defaultMaxImageBytes = 2 << 20

	// defaultMaxImageTotalBytes is the whole document's budget, again on raw
	// bytes. It bounds a picture book, which would otherwise produce a
	// hundred megabytes of Markdown from a file that opened fine.
	defaultMaxImageTotalBytes = 16 << 20

	// minInlineImageBytes skips images too small to be worth carrying: a
	// spacer, a rule, a bullet glyph drawn as a picture.
	minInlineImageBytes = 128
)

// imageBudget meters the image bytes one conversion may inline.
//
// It is per-conversion rather than global because the cap is about the size of
// the output document, and it is passed explicitly rather than hung off
// Options because Options is shared between conversions and must stay
// read-only: a budget mutates as it is spent.
type imageBudget struct {
	drop      bool
	perImage  int
	remaining int
	seen      map[string]string // sha-keyed, so a repeated logo is encoded once
}

// newImageBudget reads the caller's limits, filling in the defaults.
func newImageBudget(opts *Options) *imageBudget {
	b := &imageBudget{
		perImage:  defaultMaxImageBytes,
		remaining: defaultMaxImageTotalBytes,
		seen:      map[string]string{},
	}
	if opts == nil {
		return b
	}
	b.drop = opts.DropImages
	if opts.MaxImageBytes > 0 {
		b.perImage = opts.MaxImageBytes
	}
	if opts.MaxImageTotalBytes > 0 {
		b.remaining = opts.MaxImageTotalBytes
	}
	return b
}

// dataURI encodes one image as a data: URI, or returns "" when it cannot be
// inlined — because the caller switched inlining off, because the bytes are
// not an image, or because the budget is spent. A "" result is not an error:
// the caller falls back to the placeholder it would have emitted before.
func (b *imageBudget) dataURI(data []byte, mime string) string {
	if b == nil || b.drop || len(data) < minInlineImageBytes {
		return ""
	}
	if mime == "" || !strings.HasPrefix(mime, "image/") {
		mime = http.DetectContentType(data)
	}
	if !strings.HasPrefix(mime, "image/") {
		return ""
	}
	if len(data) > b.perImage {
		return ""
	}
	// A document that repeats one image — a letterhead on every page, a logo
	// in every slide footer — should pay for it once, both in bytes and in
	// the budget.
	key := imageKey(data)
	if uri, ok := b.seen[key]; ok {
		return uri
	}
	if len(data) > b.remaining {
		return ""
	}
	b.remaining -= len(data)
	uri := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
	b.seen[key] = uri
	return uri
}

// imageKey identifies an image's bytes for the dedup cache. The length plus
// both ends is enough to tell a document's own images apart without hashing
// every megabyte, and a collision costs a reused URI for identical-looking
// data rather than anything unsafe.
func imageKey(data []byte) string {
	const edge = 64
	head, tail := data, data
	if len(data) > edge {
		head, tail = data[:edge], data[len(data)-edge:]
	}
	var sb strings.Builder
	sb.Grow(2*edge + 8)
	sb.WriteString(string(rune(len(data))))
	sb.Write(head)
	sb.Write(tail)
	return sb.String()
}

// markdownImage renders an image: the data URI when there is one, otherwise the
// empty destination that keeps the picture's place in the text.
func markdownImage(alt, uri string) string {
	alt = strings.NewReplacer("[", `\[`, "]", `\]`, "\n", " ", "\r", " ").Replace(alt)
	return "![" + alt + "](" + uri + ")"
}
