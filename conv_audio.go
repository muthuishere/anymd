package anymd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/muthuishere/anymd/internal/mdutil"
)

func init() { addBuiltin(&AudioConverter{}) }

// maxAudioBytes bounds how much of an audio stream is buffered before it is
// handed to a Transcriber. A speech service's own limit is far lower (25 MiB is
// the common one), so this is only a guard against a hostile stream exhausting
// memory before we ever get to that check.
const maxAudioBytes = 128 << 20

// AudioConverter turns speech into Markdown by handing the bytes to
// Options.Transcriber.
//
// This is the one converter in the tree that cannot work offline: there is no
// pure-Go speech recogniser to ship, and inventing one is out of scope. So the
// converter exists but is *inert* until the caller supplies a Transcriber —
// which is why Accepts returns false when Options.Transcriber is nil.
//
// That nil check is load-bearing, not defensive. The engine treats a converter
// that accepts and then fails as a hard error and does NOT fall through to the
// fallback, so accepting an .mp3 with no Transcriber would replace the honest
// ErrUnsupported ("nothing handles this stream") with a misleading "audio
// converter failed". Declining keeps the error truthful and keeps the promise
// that a default conversion makes no network calls.
type AudioConverter struct{}

// Name identifies the converter in errors and in `anymd --list`.
func (c *AudioConverter) Name() string { return "audio" }

// Accepts recognizes audio by magic bytes, mime, then extension — but only
// when a Transcriber is available to actually read it. See the type doc.
func (c *AudioConverter) Accepts(r io.ReadSeeker, info StreamInfo, opts *Options) bool {
	if opts == nil || opts.Transcriber == nil {
		return false
	}
	var head [16]byte
	n, _ := io.ReadFull(r, head[:])
	if audioMagic(head[:n]) != "" {
		return true
	}
	if info.HasMimePrefix("audio/") {
		return true
	}
	return info.HasExt(audioExts...)
}

// audioExts is the extension set, matching what the common speech endpoints
// accept. .mp4 and .webm are containers that usually carry video too; a
// transcription service reads the audio track and ignores the rest.
var audioExts = []string{
	".mp3", ".m4a", ".wav", ".flac", ".ogg", ".oga", ".opus",
	".webm", ".mp4", ".mpga", ".mpeg", ".mpg",
}

// audioMagic returns a short format name for the magic bytes at the head of an
// audio stream, or "" if none matched. This is what lets an extensionless,
// mime-less stream — a zip member, a mail attachment — still route here.
func audioMagic(b []byte) string {
	switch {
	// ID3v2-tagged mp3, then a bare MPEG audio frame sync (0xFF Ex/Fx).
	case len(b) >= 3 && string(b[:3]) == "ID3":
		return "mp3"
	case len(b) >= 2 && b[0] == 0xFF && (b[1]&0xE0) == 0xE0:
		return "mp3"
	// RIFF....WAVE
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WAVE":
		return "wav"
	// ISO base media: the ftyp box sits at offset 4 for m4a/mp4.
	case len(b) >= 12 && string(b[4:8]) == "ftyp":
		return "m4a"
	case len(b) >= 4 && string(b[:4]) == "OggS":
		return "ogg"
	case len(b) >= 4 && string(b[:4]) == "fLaC":
		return "flac"
	}
	return ""
}

// Convert transcribes the audio and renders it as prose.
//
// Unlike image captioning — where a Describer failure degrades to "no caption"
// and the document still carries its text — a Transcriber failure here is a
// real error: the transcript IS the entire content of the document, so
// returning an empty success would be exactly the silent-empty-success this
// project refuses.
func (c *AudioConverter) Convert(r io.ReadSeeker, info StreamInfo, opts *Options) (Result, error) {
	if opts == nil || opts.Transcriber == nil {
		// Unreachable through the engine (Accepts already declined), but a
		// caller may invoke a converter directly.
		return Result{}, fmt.Errorf("no Transcriber configured")
	}

	b, err := io.ReadAll(io.LimitReader(r, maxAudioBytes+1))
	if err != nil {
		return Result{}, err
	}
	if len(b) > maxAudioBytes {
		return Result{}, fmt.Errorf("audio too large: over %d bytes", maxAudioBytes)
	}
	if len(b) == 0 {
		return Result{}, fmt.Errorf("empty audio stream")
	}

	ctx, cancel := context.WithTimeout(context.Background(), opts.llmTimeout())
	defer cancel()

	text, err := opts.Transcriber.Transcribe(ctx, b, audioMime(b, info))
	if err != nil {
		return Result{}, fmt.Errorf("transcription failed: %w", err)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Result{}, fmt.Errorf("transcription returned no text")
	}

	blocks := []string{mdutil.Heading(1, "Transcript")}
	if name := strings.TrimSpace(info.FileName); name != "" {
		blocks = append(blocks, "- Source: "+mdutil.Collapse(name))
	}
	blocks = append(blocks, text)

	return Result{Markdown: mdutil.Join(blocks...), Title: "Transcript"}, nil
}

// audioMime settles on the media type to declare to the Transcriber: the
// caller's hint when it is already an audio/video type, otherwise one derived
// from the magic bytes or the extension. It never returns "", because a
// transcription endpoint that has to guess is a transcription endpoint that
// rejects the upload.
func audioMime(b []byte, info StreamInfo) string {
	if m := info.NormalizedMime(); strings.HasPrefix(m, "audio/") || strings.HasPrefix(m, "video/") {
		return m
	}
	switch audioMagic(b) {
	case "mp3":
		return "audio/mpeg"
	case "wav":
		return "audio/wav"
	case "m4a":
		return "audio/mp4"
	case "ogg":
		return "audio/ogg"
	case "flac":
		return "audio/flac"
	}
	switch info.Ext() {
	case ".mp3", ".mpga", ".mpeg", ".mpg":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".m4a", ".mp4":
		return "audio/mp4"
	case ".flac":
		return "audio/flac"
	case ".ogg", ".oga", ".opus":
		return "audio/ogg"
	case ".webm":
		return "audio/webm"
	}
	return "audio/mpeg"
}
