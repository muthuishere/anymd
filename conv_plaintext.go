package anymd

import (
	"io"
	"strings"
	"unicode/utf8"
)

func init() { addBuiltin(&PlainTextConverter{}) }

// PlainTextConverter is the last-resort converter: anything that decodes as
// text passes through verbatim. It sits at PriorityFallback so it can never
// shadow a real format.
type PlainTextConverter struct{}

func (c *PlainTextConverter) Name() string  { return "plaintext" }
func (c *PlainTextConverter) Priority() int { return PriorityFallback }

func (c *PlainTextConverter) Accepts(r io.ReadSeeker, info StreamInfo, opts *Options) bool {
	if info.HasMimePrefix("text/") {
		return true
	}
	if info.HasExt(".txt", ".text", ".md", ".markdown", ".log") {
		return true
	}
	// No usable hint: accept only if the head actually decodes as UTF-8 text
	// with no NUL bytes, so we never emit binary garbage as "markdown".
	var head [4096]byte
	n, _ := io.ReadFull(r, head[:])
	if n == 0 {
		return true // empty input is trivially text
	}
	b := head[:n]
	if strings.IndexByte(string(b), 0) >= 0 {
		return false
	}
	// A truncated final rune is fine; anything else invalid is not text.
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size <= 1 {
			return len(b) < utf8.UTFMax // tolerate a rune split by the 4KiB cut
		}
		b = b[size:]
	}
	return true
}

func (c *PlainTextConverter) Convert(r io.ReadSeeker, info StreamInfo, opts *Options) (Result, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return Result{}, err
	}
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	return Result{Markdown: strings.TrimRight(text, "\n") + "\n"}, nil
}
