package anymd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// stubTranscriber is a Transcriber that returns a canned reply (or a canned
// error) and records what it was handed. No network, no key, no fixtures.
type stubTranscriber struct {
	text    string
	err     error
	gotMime string
	gotLen  int
	calls   int
}

func (s *stubTranscriber) Transcribe(ctx context.Context, audio []byte, mime string) (string, error) {
	s.calls++
	s.gotMime = mime
	s.gotLen = len(audio)
	if s.err != nil {
		return "", s.err
	}
	return s.text, nil
}

// Synthetic streams with real magic bytes — the sniffing paths must work on a
// stream with no filename and no mime at all.
var (
	mp3ID3  = append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), bytes.Repeat([]byte{0x11}, 32)...)
	mp3Sync = append([]byte{0xFF, 0xFB, 0x90, 0x44}, bytes.Repeat([]byte{0x22}, 32)...)
	wavRIFF = append([]byte("RIFF\x24\x00\x00\x00WAVEfmt "), bytes.Repeat([]byte{0x33}, 32)...)
	m4aFtyp = append([]byte("\x00\x00\x00\x20ftypM4A \x00\x00\x00\x00"), bytes.Repeat([]byte{0x44}, 32)...)
	oggPage = append([]byte("OggS\x00\x02\x00\x00\x00\x00\x00\x00"), bytes.Repeat([]byte{0x55}, 32)...)
	flacHdr = append([]byte("fLaC\x00\x00\x00\x22"), bytes.Repeat([]byte{0x66}, 32)...)
)

func audioOpts(tr Transcriber) *Options { return &Options{Transcriber: tr} }

// TestAudioAcceptsDeclinesWithoutTranscriber is the load-bearing case: with no
// Transcriber the converter must DECLINE, so the engine's error stays the
// honest ErrUnsupported instead of "the audio converter failed".
func TestAudioAcceptsDeclinesWithoutTranscriber(t *testing.T) {
	c := &AudioConverter{}
	for _, tc := range []struct {
		name string
		body []byte
		info StreamInfo
	}{
		{"mp3 by extension", mp3ID3, StreamInfo{Extension: ".mp3", FileName: "a.mp3"}},
		{"wav by mime", wavRIFF, StreamInfo{MimeType: "audio/wav"}},
		{"bare magic bytes", oggPage, StreamInfo{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if c.Accepts(bytes.NewReader(tc.body), tc.info, &Options{}) {
				t.Error("Accepts = true with a nil Transcriber, want false")
			}
			if c.Accepts(bytes.NewReader(tc.body), tc.info, nil) {
				t.Error("Accepts = true with nil Options, want false")
			}
		})
	}
}

// TestAudioEngineErrorWithoutTranscriber: end to end, the engine must report
// ErrUnsupported rather than an accept-then-fail from this converter.
func TestAudioEngineErrorWithoutTranscriber(t *testing.T) {
	_, err := New().ConvertBytes(mp3ID3, StreamInfo{Extension: ".mp3", FileName: "talk.mp3"}, nil)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

// TestAudioAcceptsWithTranscriber covers extension, mime and magic-byte
// routing once transcription is available.
func TestAudioAcceptsWithTranscriber(t *testing.T) {
	c := &AudioConverter{}
	opts := audioOpts(&stubTranscriber{text: "x"})
	cases := []struct {
		name string
		body []byte
		info StreamInfo
		want bool
	}{
		{"mp3 magic id3", mp3ID3, StreamInfo{}, true},
		{"mp3 magic frame sync", mp3Sync, StreamInfo{}, true},
		{"wav magic", wavRIFF, StreamInfo{}, true},
		{"m4a ftyp magic", m4aFtyp, StreamInfo{}, true},
		{"ogg magic", oggPage, StreamInfo{}, true},
		{"flac magic", flacHdr, StreamInfo{}, true},
		{"extension only", []byte("no magic here at all"), StreamInfo{Extension: ".m4a"}, true},
		{"webm extension", []byte("no magic here at all"), StreamInfo{Extension: ".webm"}, true},
		{"mpga extension", []byte("no magic here at all"), StreamInfo{Extension: ".mpga"}, true},
		{"mime only", []byte("no magic here at all"), StreamInfo{MimeType: "audio/ogg; codecs=opus"}, true},
		{"plain text declines", []byte("hello, this is prose"), StreamInfo{Extension: ".txt"}, false},
		{"png declines", []byte("\x89PNG\r\n\x1a\nrest"), StreamInfo{Extension: ".png"}, false},
		{"jpeg declines", []byte("\xFF\xD8\xFFmore"), StreamInfo{Extension: ".jpg"}, false},
		{"zip declines", []byte("PK\x03\x04rest of it"), StreamInfo{Extension: ".zip"}, false},
		{"empty declines", nil, StreamInfo{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.Accepts(bytes.NewReader(tc.body), tc.info, opts); got != tc.want {
				t.Errorf("Accepts = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAudioConvertMarkdown asserts the EXACT document, not "contains".
func TestAudioConvertMarkdown(t *testing.T) {
	tr := &stubTranscriber{text: "  Hello, this is the recording.  "}
	res, err := New().ConvertBytes(mp3ID3,
		StreamInfo{Extension: ".mp3", FileName: "talk.mp3", MimeType: "audio/mpeg"},
		audioOpts(tr))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	want := "# Transcript\n\n- Source: talk.mp3\n\nHello, this is the recording.\n"
	if res.Markdown != want {
		t.Errorf("markdown =\n%q\nwant\n%q", res.Markdown, want)
	}
	if res.Title != "Transcript" {
		t.Errorf("Title = %q", res.Title)
	}
	if tr.calls != 1 {
		t.Errorf("Transcribe called %d times, want 1", tr.calls)
	}
	if tr.gotMime != "audio/mpeg" {
		t.Errorf("mime handed to the Transcriber = %q", tr.gotMime)
	}
	if tr.gotLen != len(mp3ID3) {
		t.Errorf("audio length = %d, want %d", tr.gotLen, len(mp3ID3))
	}
}

// TestAudioConvertNoFileName: with no filename there is no Source line, and
// the document is still exactly right.
func TestAudioConvertNoFileName(t *testing.T) {
	res, err := (&AudioConverter{}).Convert(bytes.NewReader(oggPage), StreamInfo{},
		audioOpts(&stubTranscriber{text: "Spoken words."}))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	want := "# Transcript\n\nSpoken words.\n"
	if res.Markdown != want {
		t.Errorf("markdown = %q, want %q", res.Markdown, want)
	}
}

// TestAudioMimeInference: the Transcriber must be told a real media type even
// when the caller supplied none, or the endpoint rejects the upload.
func TestAudioMimeInference(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		info StreamInfo
		want string
	}{
		{"mp3 magic", mp3ID3, StreamInfo{}, "audio/mpeg"},
		{"wav magic", wavRIFF, StreamInfo{}, "audio/wav"},
		{"m4a magic", m4aFtyp, StreamInfo{}, "audio/mp4"},
		{"ogg magic", oggPage, StreamInfo{}, "audio/ogg"},
		{"flac magic", flacHdr, StreamInfo{}, "audio/flac"},
		{"extension when no magic", []byte("xxxxxxxxxxxx"), StreamInfo{Extension: ".webm"}, "audio/webm"},
		{"caller mime wins", mp3ID3, StreamInfo{MimeType: "audio/x-custom; rate=8000"}, "audio/x-custom"},
		{"video container mime kept", m4aFtyp, StreamInfo{MimeType: "video/mp4"}, "video/mp4"},
		{"non-audio mime ignored", wavRIFF, StreamInfo{MimeType: "application/octet-stream"}, "audio/wav"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := &stubTranscriber{text: "ok"}
			if _, err := (&AudioConverter{}).Convert(bytes.NewReader(tc.body), tc.info, audioOpts(tr)); err != nil {
				t.Fatalf("Convert: %v", err)
			}
			if tr.gotMime != tc.want {
				t.Errorf("mime = %q, want %q", tr.gotMime, tc.want)
			}
		})
	}
}

// TestAudioTranscriberErrorSurfaces: the transcript IS the document, so a
// failure must be an error — never an empty success.
func TestAudioTranscriberErrorSurfaces(t *testing.T) {
	boom := errors.New("provider exploded")
	_, err := New().ConvertBytes(wavRIFF, StreamInfo{Extension: ".wav", FileName: "a.wav"},
		audioOpts(&stubTranscriber{err: boom}))
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap the Transcriber error", err)
	}
	if !strings.Contains(err.Error(), "audio") {
		t.Errorf("err = %v, want the converter named", err)
	}
}

// TestAudioEmptyTranscriptIsAnError: a Transcriber that returns whitespace has
// failed; emitting a heading with no body would be a silent empty success.
func TestAudioEmptyTranscriptIsAnError(t *testing.T) {
	_, err := (&AudioConverter{}).Convert(bytes.NewReader(mp3Sync),
		StreamInfo{Extension: ".mp3"}, audioOpts(&stubTranscriber{text: "   \n  "}))
	if err == nil {
		t.Fatal("want an error for an empty transcript")
	}
}

// TestAudioConvertWithoutTranscriber: called directly (bypassing the engine),
// Convert must error rather than panic on a nil Transcriber.
func TestAudioConvertWithoutTranscriber(t *testing.T) {
	if _, err := (&AudioConverter{}).Convert(bytes.NewReader(mp3ID3), StreamInfo{}, &Options{}); err == nil {
		t.Error("want an error with no Transcriber")
	}
	if _, err := (&AudioConverter{}).Convert(bytes.NewReader(mp3ID3), StreamInfo{}, nil); err == nil {
		t.Error("want an error with nil Options")
	}
}

// TestAudioEmptyStream: zero bytes is malformed input, not a transcript.
func TestAudioEmptyStream(t *testing.T) {
	tr := &stubTranscriber{text: "ok"}
	if _, err := (&AudioConverter{}).Convert(bytes.NewReader(nil), StreamInfo{Extension: ".mp3"}, audioOpts(tr)); err == nil {
		t.Error("want an error for an empty stream")
	}
	if tr.calls != 0 {
		t.Error("an empty stream must not reach the Transcriber")
	}
}

// TestAudioConverterRegistered: it must be in the default registry, or none of
// the above ever runs in a real conversion.
func TestAudioConverterRegistered(t *testing.T) {
	for _, n := range New().Converters() {
		if n == "audio" {
			return
		}
	}
	t.Fatal("audio converter is not registered")
}
