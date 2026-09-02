package anymd

import (
	"context"
	"net/http"
	"strings"
)

// describeImage asks the caller's Describer to caption the image, and returns
// "" when there is no Describer or the call fails.
//
// A Describer failure is deliberately NOT an error for the document: a model
// outage or a rate limit should cost you the caption, not the conversion. The
// rest of the output — placeholder, dimensions, EXIF — is still correct.
func describeImage(b []byte, info StreamInfo, opts *Options) string {
	if opts == nil || opts.Describer == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.llmTimeout())
	defer cancel()

	mime := info.NormalizedMime()
	if mime == "" || !strings.HasPrefix(mime, "image/") {
		mime = http.DetectContentType(b)
	}

	caption, err := opts.Describer.Describe(ctx, b, mime, "")
	if err != nil || strings.TrimSpace(caption) == "" {
		return ""
	}
	return strings.TrimSpace(caption)
}

// describeImageWithHint is describeImage with the document's own alt text or
// caption passed through, so the model is told what the author already said.
// Converters that have such a hint (docx/pptx alt text, a PDF figure caption)
// should prefer it.
func describeImageWithHint(b []byte, mime, hint string, opts *Options) string {
	if opts == nil || opts.Describer == nil || len(b) == 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.llmTimeout())
	defer cancel()

	if mime == "" || !strings.HasPrefix(mime, "image/") {
		mime = http.DetectContentType(b)
	}
	if !strings.HasPrefix(mime, "image/") {
		return "" // not an image; never pay for a model call on arbitrary bytes
	}

	caption, err := opts.Describer.Describe(ctx, b, mime, hint)
	if err != nil || strings.TrimSpace(caption) == "" {
		return ""
	}
	return strings.TrimSpace(caption)
}

// HasDescriber reports whether captioning is available, so a converter can skip
// the work of extracting image bytes when nothing will read them.
func (o *Options) HasDescriber() bool { return o != nil && o.Describer != nil }
