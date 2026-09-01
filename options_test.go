package anymd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
)

// TestOptionsMaxDepthDefault pins the documented default: 0 means 8, and any
// explicit value (including a negative one) is taken literally.
func TestOptionsMaxDepthDefault(t *testing.T) {
	tests := []struct {
		name string
		opts *Options
		want int
	}{
		{"nil Options defaults to 8", nil, 8},
		{"zero value defaults to 8", &Options{}, 8},
		{"explicit 0 defaults to 8", &Options{MaxDepth: 0}, 8},
		{"explicit 1", &Options{MaxDepth: 1}, 1},
		{"explicit 8", &Options{MaxDepth: 8}, 8},
		{"explicit 32", &Options{MaxDepth: 32}, 32},
		{"negative is taken literally", &Options{MaxDepth: -1}, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.opts.maxDepth(); got != tt.want {
				t.Errorf("maxDepth() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestOptionsDepthDefault: Depth is 0 at the top level, including on nil.
func TestOptionsDepthDefault(t *testing.T) {
	var nilOpts *Options
	if got := nilOpts.Depth(); got != 0 {
		t.Errorf("(*Options)(nil).Depth() = %d, want 0", got)
	}
	if got := (&Options{}).Depth(); got != 0 {
		t.Errorf("(&Options{}).Depth() = %d, want 0", got)
	}
	if got := (&Options{MaxDepth: 5}).Depth(); got != 0 {
		t.Errorf("Depth() = %d before any conversion, want 0", got)
	}
}

// tzzDepthRecorder recurses a fixed number of times and records the Depth()
// seen at each level.
type tzzDepthRecorder struct {
	want int
	seen *[]int
}

func (c *tzzDepthRecorder) Name() string                                     { return "depthrec" }
func (c *tzzDepthRecorder) Priority() int                                    { return PrioritySpecific }
func (c *tzzDepthRecorder) Accepts(io.ReadSeeker, StreamInfo, *Options) bool { return true }

func (c *tzzDepthRecorder) Convert(r io.ReadSeeker, info StreamInfo, opts *Options) (Result, error) {
	d := opts.Depth()
	*c.seen = append(*c.seen, d)
	if d >= c.want {
		return Result{Markdown: fmt.Sprintf("bottom=%d", d)}, nil
	}
	return opts.Recurse(bytes.NewReader([]byte("x")), StreamInfo{MimeType: "text/plain"})
}

// TestOptionsDepthIncrementsThroughRecurse: each nested Recurse is exactly one
// level deeper, and the caller's own Options is never advanced.
func TestOptionsDepthIncrementsThroughRecurse(t *testing.T) {
	for _, levels := range []int{0, 1, 3, 7} {
		t.Run(fmt.Sprintf("levels=%d", levels), func(t *testing.T) {
			var seen []int
			e := &Engine{}
			e.Register(&tzzDepthRecorder{want: levels, seen: &seen})

			res, err := e.ConvertBytes([]byte("x"), StreamInfo{MimeType: "text/plain"}, nil)
			if err != nil {
				t.Fatalf("ConvertBytes: %v", err)
			}
			if want := fmt.Sprintf("bottom=%d", levels); res.Markdown != want {
				t.Errorf("Markdown = %q, want %q", res.Markdown, want)
			}
			want := make([]int, levels+1)
			for i := range want {
				want[i] = i
			}
			if len(seen) != len(want) {
				t.Fatalf("depths seen = %v, want %v", seen, want)
			}
			for i := range want {
				if seen[i] != want[i] {
					t.Fatalf("depths seen = %v, want %v", seen, want)
				}
			}
		})
	}
}

// TestOptionsRecurseDepthBound: Recurse refuses the step that would cross
// MaxDepth, and refuses every step when MaxDepth is negative.
func TestOptionsRecurseDepthBound(t *testing.T) {
	tests := []struct {
		name     string
		maxDepth int
		want     int // deepest Depth() reached before ErrMaxDepth
	}{
		{"default", 0, 8},
		{"one level", 1, 1},
		{"five levels", 5, 5},
		{"negative allows nothing", -1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deepest := -1
			e := &Engine{}
			e.Register(&tzzNester{})
			// tzzNester reports the depth at which Recurse first refused.
			res, err := e.ConvertBytes([]byte("x"), StreamInfo{MimeType: "text/plain"}, &Options{MaxDepth: tt.maxDepth})
			if err != nil {
				t.Fatalf("ConvertBytes: %v", err)
			}
			if _, scanErr := fmt.Sscanf(res.Markdown, "depth=%d", &deepest); scanErr != nil {
				t.Fatalf("unexpected markdown %q: %v", res.Markdown, scanErr)
			}
			if deepest != tt.want {
				t.Errorf("deepest depth = %d, want %d", deepest, tt.want)
			}
		})
	}
}

// tzzTopOnly is a container that claims the stream only at the top level, so
// the stream it recurses into is dispatched to somebody else.
type tzzTopOnly struct{ onTop func(*Options) }

func (c *tzzTopOnly) Name() string  { return "toponly" }
func (c *tzzTopOnly) Priority() int { return PrioritySpecific }
func (c *tzzTopOnly) Accepts(_ io.ReadSeeker, _ StreamInfo, opts *Options) bool {
	return opts.Depth() == 0
}

func (c *tzzTopOnly) Convert(_ io.ReadSeeker, _ StreamInfo, opts *Options) (Result, error) {
	c.onTop(opts)
	return Result{Markdown: "top\n"}, nil
}

// TestOptionsRecursePropagatesSettings: a child conversion inherits the
// caller's knobs, so KeepDataURIs set once holds all the way down.
func TestOptionsRecursePropagatesSettings(t *testing.T) {
	var childOpts *Options
	child := &tzzConv{name: "child", prio: PriorityGeneric, accept: true, out: "child\n"}
	child.onConv = func(_ io.ReadSeeker, _ StreamInfo, o *Options) { childOpts = o }

	parent := &tzzTopOnly{onTop: func(o *Options) {
		if _, err := o.Recurse(bytes.NewReader([]byte("x")), StreamInfo{MimeType: "text/plain"}); err != nil {
			t.Errorf("Recurse: %v", err)
		}
	}}

	e := &Engine{}
	e.Register(parent)
	e.Register(child)

	opts := &Options{MaxDepth: 4, KeepDataURIs: true, Charset: "latin1"}
	if _, err := e.ConvertBytes([]byte("x"), StreamInfo{MimeType: "text/plain"}, opts); err != nil {
		t.Fatalf("ConvertBytes: %v", err)
	}
	if childOpts == nil {
		t.Fatal("child converter never ran")
	}
	if childOpts.Depth() != 1 {
		t.Errorf("child Depth() = %d, want 1", childOpts.Depth())
	}
	if !childOpts.KeepDataURIs {
		t.Error("KeepDataURIs did not propagate through Recurse")
	}
	if childOpts.Charset != "latin1" {
		t.Errorf("child Charset = %q, want %q", childOpts.Charset, "latin1")
	}
	if childOpts.MaxDepth != 4 {
		t.Errorf("child MaxDepth = %d, want 4", childOpts.MaxDepth)
	}
}

// TestErrMaxDepthIsMatchable: container converters branch on this sentinel, so
// it must survive errors.Is through whatever wrapping happens.
func TestErrMaxDepthIsMatchable(t *testing.T) {
	o := &Options{MaxDepth: -1, engine: &Engine{}}
	_, err := o.Recurse(bytes.NewReader([]byte("x")), StreamInfo{})
	if !errors.Is(err, ErrMaxDepth) {
		t.Fatalf("err = %v, want ErrMaxDepth", err)
	}
	if want := "anymd: max recursion depth exceeded"; err.Error() != want {
		t.Errorf("err = %q, want %q", err.Error(), want)
	}
}
