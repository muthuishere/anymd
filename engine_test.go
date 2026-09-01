package anymd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// test-only converters. Named with a tzz prefix because several agents write
// sibling *_test.go files into this same package.
// ---------------------------------------------------------------------------

// tzzConv is a scriptable converter: everything about its behaviour is a field.
type tzzConv struct {
	name    string
	prio    int
	accept  bool
	out     string
	err     error
	onConv  func(r io.ReadSeeker, info StreamInfo, opts *Options)
	accepts *int // incremented on every Accepts call, when non-nil
}

func (c *tzzConv) Name() string  { return c.name }
func (c *tzzConv) Priority() int { return c.prio }

func (c *tzzConv) Accepts(r io.ReadSeeker, info StreamInfo, opts *Options) bool {
	if c.accepts != nil {
		*c.accepts++
	}
	return c.accept
}

func (c *tzzConv) Convert(r io.ReadSeeker, info StreamInfo, opts *Options) (Result, error) {
	if c.onConv != nil {
		c.onConv(r, info, opts)
	}
	if c.err != nil {
		return Result{}, c.err
	}
	return Result{Markdown: c.out}, nil
}

// tzzReaderOnly hides the Seek method of whatever it wraps, so ConvertStream
// has to buffer.
type tzzReaderOnly struct{ r io.Reader }

func (t tzzReaderOnly) Read(p []byte) (int, error) { return t.r.Read(p) }

// tzzNester recurses forever, so MaxDepth is the only thing that stops it. It
// reports the deepest level actually reached.
type tzzNester struct{}

func (c *tzzNester) Name() string                                     { return "nester" }
func (c *tzzNester) Priority() int                                    { return PrioritySpecific }
func (c *tzzNester) Accepts(io.ReadSeeker, StreamInfo, *Options) bool { return true }

func (c *tzzNester) Convert(r io.ReadSeeker, info StreamInfo, opts *Options) (Result, error) {
	depth := opts.Depth()
	res, err := opts.Recurse(bytes.NewReader([]byte("x")), StreamInfo{MimeType: "text/plain"})
	if errors.Is(err, ErrMaxDepth) {
		return Result{Markdown: fmt.Sprintf("depth=%d", depth)}, nil
	}
	if err != nil {
		return Result{}, err
	}
	return res, nil
}

// ---------------------------------------------------------------------------

// TestEngineDispatchOrder pins the two ordering rules Register documents:
// ascending priority, and registration order as the tie-break.
func TestEngineDispatchOrder(t *testing.T) {
	tests := []struct {
		name     string
		register []Converter
		want     string
	}{
		{
			name: "lower priority wins over a built-in registered first",
			register: []Converter{
				&PlainTextConverter{}, // PriorityFallback = 100
				&tzzConv{name: "generic", prio: PriorityGeneric, accept: true, out: "generic\n"},
			},
			want: "generic\n",
		},
		{
			name: "lower priority wins even when registered last",
			register: []Converter{
				&tzzConv{name: "generic", prio: PriorityGeneric, accept: true, out: "generic\n"},
				&tzzConv{name: "specific", prio: PrioritySpecific, accept: true, out: "specific\n"},
			},
			want: "specific\n",
		},
		{
			name: "ties break by registration order",
			register: []Converter{
				&tzzConv{name: "first", prio: PrioritySpecific, accept: true, out: "first\n"},
				&tzzConv{name: "second", prio: PrioritySpecific, accept: true, out: "second\n"},
			},
			want: "first\n",
		},
		{
			name: "a converter that declines is skipped for the next one",
			register: []Converter{
				&tzzConv{name: "declines", prio: PrioritySpecific, accept: false, out: "nope\n"},
				&tzzConv{name: "accepts", prio: PrioritySpecific, accept: true, out: "yes\n"},
			},
			want: "yes\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &Engine{}
			for _, c := range tt.register {
				e.Register(c)
			}
			res, err := e.ConvertBytes([]byte("hello"), StreamInfo{MimeType: "text/plain"}, nil)
			if err != nil {
				t.Fatalf("ConvertBytes: %v", err)
			}
			if res.Markdown != tt.want {
				t.Errorf("Markdown = %q, want %q", res.Markdown, tt.want)
			}
		})
	}
}

// TestEngineConvertersOrder checks the reported dispatch order is sorted by
// priority, using the real built-in registry.
func TestEngineConvertersOrder(t *testing.T) {
	names := New().Converters()
	pos := func(want string) int {
		for i, n := range names {
			if n == want {
				return i
			}
		}
		return -1
	}
	zip, plain := pos("zip"), pos("plaintext")
	if zip < 0 {
		t.Fatalf("zip not registered; Converters() = %v", names)
	}
	if plain < 0 {
		t.Fatalf("plaintext not registered; Converters() = %v", names)
	}
	if zip >= plain {
		t.Errorf("zip (PriorityGeneric) must dispatch before plaintext (PriorityFallback); got %v", names)
	}
}

// TestEngineFallbackNeverShadows: the fallback must never win against a
// converter that actually claims the stream, whatever the registration order.
func TestEngineFallbackNeverShadows(t *testing.T) {
	for _, plainFirst := range []bool{true, false} {
		e := &Engine{}
		specific := &tzzConv{name: "specific", prio: PrioritySpecific, accept: true, out: "SPECIFIC\n"}
		if plainFirst {
			e.Register(&PlainTextConverter{})
			e.Register(specific)
		} else {
			e.Register(specific)
			e.Register(&PlainTextConverter{})
		}
		// Plain UTF-8 text: exactly the input plaintext would happily claim.
		res, err := e.ConvertBytes([]byte("just text\n"), StreamInfo{Extension: ".txt"}, nil)
		if err != nil {
			t.Fatalf("plainFirst=%v: %v", plainFirst, err)
		}
		if res.Markdown != "SPECIFIC\n" {
			t.Errorf("plainFirst=%v: Markdown = %q, want %q", plainFirst, res.Markdown, "SPECIFIC\n")
		}
	}
}

// TestEngineErrUnsupported: nothing accepts, so the caller gets ErrUnsupported
// with the hints echoed back for debugging.
func TestEngineErrUnsupported(t *testing.T) {
	e := &Engine{}
	e.Register(&tzzConv{name: "never", prio: PrioritySpecific, accept: false})
	_, err := e.ConvertBytes([]byte("x"), StreamInfo{MimeType: "application/x-weird", Extension: "ODD"}, nil)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	want := `anymd: no converter accepted this stream (ext=".odd" mime="application/x-weird")`
	if err.Error() != want {
		t.Errorf("err = %q, want %q", err.Error(), want)
	}
}

// TestEngineAcceptThenFailDoesNotFallThrough pins the deliberate design
// decision in engine.go: once a converter accepts, its failure is the answer.
// Falling through here would let plaintext emit binary garbage as "markdown"
// every time a real parser hit a malformed file.
func TestEngineAcceptThenFailDoesNotFallThrough(t *testing.T) {
	e := &Engine{}
	boom := errors.New("boom")
	e.Register(&tzzConv{name: "explodes", prio: PrioritySpecific, accept: true, err: boom})
	e.Register(&PlainTextConverter{})

	res, err := e.ConvertBytes([]byte("perfectly good text\n"), StreamInfo{Extension: ".txt"}, nil)
	if err == nil {
		t.Fatalf("want an error, got Result{Markdown: %q}", res.Markdown)
	}
	if res.Markdown != "" {
		t.Errorf("Markdown = %q, want empty on error", res.Markdown)
	}
	if !errors.Is(err, boom) {
		t.Errorf("err does not wrap the converter's error: %v", err)
	}
	if want := "anymd: explodes: boom"; err.Error() != want {
		t.Errorf("err = %q, want %q", err.Error(), want)
	}
	if errors.Is(err, ErrUnsupported) {
		t.Error("an accepted-then-failed conversion must not report ErrUnsupported")
	}
}

// TestEngineBuffersNonSeekableReader: a bare io.Reader must be buffered so
// dispatch can rewind, and both Accepts and Convert must see the full stream
// from byte 0.
func TestEngineBuffersNonSeekableReader(t *testing.T) {
	const body = "line one\nline two\n"
	var seenInAccepts, seenInConvert string

	sniffer := &tzzConv{name: "sniffer", prio: PrioritySpecific, accept: true}
	sniffer.onConv = func(r io.ReadSeeker, info StreamInfo, opts *Options) {
		b, _ := io.ReadAll(r)
		seenInConvert = string(b)
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			t.Errorf("Convert got a reader that cannot seek: %v", err)
		}
	}

	// A converter that consumes the stream during Accepts, to prove the engine
	// rewinds before Convert.
	greedy := &tzzGreedy{seen: &seenInAccepts}

	e := &Engine{}
	e.Register(greedy)
	e.Register(sniffer)

	res, err := e.ConvertStream(tzzReaderOnly{strings.NewReader(body)}, StreamInfo{MimeType: "text/plain"}, nil)
	if err != nil {
		t.Fatalf("ConvertStream: %v", err)
	}
	if res.Markdown != "" {
		t.Errorf("Markdown = %q, want %q", res.Markdown, "")
	}
	if seenInAccepts != body {
		t.Errorf("Accepts saw %q, want %q", seenInAccepts, body)
	}
	if seenInConvert != body {
		t.Errorf("Convert saw %q, want %q (engine did not rewind after Accepts)", seenInConvert, body)
	}
}

// tzzGreedy drains the reader in Accepts and then declines.
type tzzGreedy struct{ seen *string }

func (c *tzzGreedy) Name() string  { return "greedy" }
func (c *tzzGreedy) Priority() int { return PrioritySpecific }
func (c *tzzGreedy) Accepts(r io.ReadSeeker, _ StreamInfo, _ *Options) bool {
	b, _ := io.ReadAll(r)
	*c.seen = string(b)
	return false
}
func (c *tzzGreedy) Convert(io.ReadSeeker, StreamInfo, *Options) (Result, error) {
	return Result{}, errors.New("unreachable")
}

// TestEngineSniffsMissingMime: an absent mime hint is filled from the first
// 512 bytes, and a present one is left alone.
func TestEngineSniffsMissingMime(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		in    StreamInfo
		want  string
		wantN string // NormalizedMime
	}{
		{
			name:  "html sniffed",
			body:  "<!DOCTYPE html>\n<html><body>hi</body></html>",
			in:    StreamInfo{},
			want:  "text/html; charset=utf-8",
			wantN: "text/html",
		},
		{
			name:  "plain text sniffed",
			body:  "just some words",
			in:    StreamInfo{},
			want:  "text/plain; charset=utf-8",
			wantN: "text/plain",
		},
		{
			name:  "binary sniffed",
			body:  "\x00\x01\x02\x03nonsense",
			in:    StreamInfo{},
			want:  "application/octet-stream",
			wantN: "application/octet-stream",
		},
		{
			name:  "existing hint is not overwritten",
			body:  "<!DOCTYPE html><html></html>",
			in:    StreamInfo{MimeType: "application/x-mine; q=1"},
			want:  "application/x-mine; q=1",
			wantN: "application/x-mine",
		},
		{
			name:  "empty stream keeps an empty hint",
			body:  "",
			in:    StreamInfo{},
			want:  "",
			wantN: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got StreamInfo
			c := &tzzConv{name: "spy", prio: PrioritySpecific, accept: true}
			c.onConv = func(_ io.ReadSeeker, info StreamInfo, _ *Options) { got = info }
			e := &Engine{}
			e.Register(c)
			if _, err := e.ConvertBytes([]byte(tt.body), tt.in, nil); err != nil {
				t.Fatalf("ConvertBytes: %v", err)
			}
			if got.MimeType != tt.want {
				t.Errorf("MimeType = %q, want %q", got.MimeType, tt.want)
			}
			if got.NormalizedMime() != tt.wantN {
				t.Errorf("NormalizedMime() = %q, want %q", got.NormalizedMime(), tt.wantN)
			}
		})
	}
}

// TestOptionsRecurseOutsideConversion: Recurse is only meaningful mid-dispatch,
// because that is where the engine handle lives.
func TestOptionsRecurseOutsideConversion(t *testing.T) {
	for _, o := range []*Options{nil, {}, {MaxDepth: 4}} {
		_, err := o.Recurse(bytes.NewReader([]byte("x")), StreamInfo{})
		if err == nil {
			t.Fatalf("Recurse on %#v returned no error", o)
		}
		if want := "anymd: Recurse called outside a conversion"; err.Error() != want {
			t.Errorf("err = %q, want %q", err.Error(), want)
		}
		if errors.Is(err, ErrMaxDepth) {
			t.Error("calling Recurse outside a conversion is not a depth problem")
		}
	}
}

// TestEngineMaxDepth: the recursion bound is enforced by Options.Recurse, and a
// negative MaxDepth turns recursion off entirely.
func TestEngineMaxDepth(t *testing.T) {
	tests := []struct {
		name     string
		maxDepth int
		want     string
	}{
		{name: "default is 8", maxDepth: 0, want: "depth=8"},
		{name: "explicit 1", maxDepth: 1, want: "depth=1"},
		{name: "explicit 3", maxDepth: 3, want: "depth=3"},
		{name: "negative disables recursion", maxDepth: -1, want: "depth=0"},
		{name: "large negative disables recursion", maxDepth: -100, want: "depth=0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &Engine{}
			e.Register(&tzzNester{})
			res, err := e.ConvertBytes([]byte("x"), StreamInfo{MimeType: "text/plain"}, &Options{MaxDepth: tt.maxDepth})
			if err != nil {
				t.Fatalf("ConvertBytes: %v", err)
			}
			if res.Markdown != tt.want {
				t.Errorf("Markdown = %q, want %q", res.Markdown, tt.want)
			}
		})
	}
}

// TestEngineDoesNotMutateCallerOptions: ConvertStream copies the Options, so a
// caller can reuse one across conversions without picking up engine state.
func TestEngineDoesNotMutateCallerOptions(t *testing.T) {
	opts := &Options{MaxDepth: 2}
	e := &Engine{}
	e.Register(&tzzNester{})
	if _, err := e.ConvertBytes([]byte("x"), StreamInfo{MimeType: "text/plain"}, opts); err != nil {
		t.Fatalf("ConvertBytes: %v", err)
	}
	if opts.Depth() != 0 {
		t.Errorf("caller Options.Depth() = %d after conversion, want 0", opts.Depth())
	}
	if _, err := opts.Recurse(bytes.NewReader([]byte("x")), StreamInfo{}); err == nil {
		t.Error("caller Options kept a live engine handle after the conversion returned")
	}
}
