package anymd

import "context"

// Describer turns an image into text. It is how anymd gets image captioning and
// OCR without shipping a model or picking a vendor.
//
// This is the equivalent of markitdown's `llm_client=` parameter, but as an
// interface rather than a concrete SDK object: anything that can look at bytes
// and return a description satisfies it, including a local model, a hosted API,
// or a stub in your tests.
//
// Options.Describer is nil by default. That is deliberate and load-bearing:
// with no Describer, anymd makes no network calls of any kind during
// conversion, which is the guarantee that lets you point it at untrusted input.
// Supplying one is opt-in, per-conversion, and visible in the caller's code.
//
// Implementations must respect ctx, must not panic, and should return an error
// rather than a partial description when the request fails — a converter
// treats a Describer error as "no caption available" and continues, so a
// transient outage degrades output instead of failing the document.
type Describer interface {
	// Describe returns a short prose description of the image. mime is the
	// image's media type (e.g. "image/png"); hint carries any context the
	// document already provided, such as existing alt text or a caption, and
	// may be empty.
	Describe(ctx context.Context, img []byte, mime, hint string) (string, error)
}

// Transcriber turns audio into text.
//
// Same contract and same default as Describer: nil means no transcription and
// no network. Supplying one closes the last format gap against markitdown,
// which transcribes audio by calling a remote speech service.
type Transcriber interface {
	// Transcribe returns the spoken content of the audio. mime is the media
	// type (e.g. "audio/mpeg").
	Transcribe(ctx context.Context, audio []byte, mime string) (string, error)
}

// describerFunc adapts a plain function to Describer, for callers who do not
// want to declare a type.
type describerFunc func(context.Context, []byte, string, string) (string, error)

func (f describerFunc) Describe(ctx context.Context, img []byte, mime, hint string) (string, error) {
	return f(ctx, img, mime, hint)
}

// DescriberFunc adapts f to the Describer interface.
func DescriberFunc(f func(ctx context.Context, img []byte, mime, hint string) (string, error)) Describer {
	return describerFunc(f)
}
