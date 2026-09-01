// Package anymd converts any document to Markdown, in pure Go.
//
// It is a Go-native answer to Microsoft's markitdown: the same converter
// registry shape, the same "hints plus sniffing" dispatch, and GitHub-flavored
// Markdown out — with no cgo, no Python, and no native library to ship. A
// binary built from this module runs anywhere Go cross-compiles to.
//
//	md, err := anymd.ConvertFile("report.docx")
//	fmt.Println(md.Markdown)
package anymd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"sort"
	"sync"
)

// ErrUnsupported is returned when no registered converter accepted the stream.
var ErrUnsupported = errors.New("anymd: no converter accepted this stream")

type entry struct {
	conv     Converter
	priority int
	name     string
	seq      int
}

// Engine holds a converter registry. The zero Engine is empty; use New for one
// with every built-in converter registered.
type Engine struct {
	entries []entry
	seq     int
}

// New returns an Engine with all built-in converters registered.
func New() *Engine {
	e := &Engine{}
	for _, c := range builtins() {
		e.Register(c)
	}
	return e
}

// Register adds a converter. Its Priority (if it implements Prioritized)
// decides ordering; ties break by registration order, so a later Register at
// the same priority runs after an earlier one.
//
// To override a built-in, register at a lower priority than it.
func (e *Engine) Register(c Converter) {
	e.seq++
	p := PrioritySpecific
	if pc, ok := c.(Prioritized); ok {
		p = pc.Priority()
	}
	e.entries = append(e.entries, entry{conv: c, priority: p, name: converterName(c), seq: e.seq})
	sort.SliceStable(e.entries, func(i, j int) bool {
		if e.entries[i].priority != e.entries[j].priority {
			return e.entries[i].priority < e.entries[j].priority
		}
		return e.entries[i].seq < e.entries[j].seq
	})
}

// Converters returns the registered converter names in dispatch order.
func (e *Engine) Converters() []string {
	out := make([]string, len(e.entries))
	for i, en := range e.entries {
		out[i] = en.name
	}
	return out
}

func converterName(c Converter) string {
	if n, ok := c.(Named); ok {
		return n.Name()
	}
	t := reflect.TypeOf(c)
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t.Name()
}

// ConvertStream converts an arbitrary reader. If r is not an io.ReadSeeker it
// is buffered into memory first, because dispatch needs to rewind.
func (e *Engine) ConvertStream(r io.Reader, info StreamInfo, opts *Options) (Result, error) {
	rs, ok := r.(io.ReadSeeker)
	if !ok {
		buf, err := io.ReadAll(r)
		if err != nil {
			return Result{}, err
		}
		rs = bytes.NewReader(buf)
	}
	if opts == nil {
		opts = &Options{}
	}
	o := *opts
	return e.convert(rs, info, &o)
}

// ConvertBytes converts an in-memory document.
func (e *Engine) ConvertBytes(b []byte, info StreamInfo, opts *Options) (Result, error) {
	return e.ConvertStream(bytes.NewReader(b), info, opts)
}

// ConvertFile converts a file from disk, seeding the hints from its path.
func (e *Engine) ConvertFile(path string, opts *Options) (Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	return e.ConvertStream(f, StreamInfoForFile(path), opts)
}

// convert is the dispatch core: rewind, ask each converter in priority order,
// first Accepts wins.
func (e *Engine) convert(r io.ReadSeeker, info StreamInfo, opts *Options) (Result, error) {
	if opts == nil {
		opts = &Options{}
	}
	opts.engine = e
	info = enrich(r, info)

	var tried []string
	for _, en := range e.entries {
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			return Result{}, err
		}
		if !en.conv.Accepts(r, info, opts) {
			continue
		}
		tried = append(tried, en.name)
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			return Result{}, err
		}
		res, err := en.conv.Convert(r, info, opts)
		if err == nil {
			return res, nil
		}
		// A converter that accepted but failed is a real error, not a reason to
		// fall through to a catch-all that would silently emit garbage.
		return Result{}, fmt.Errorf("anymd: %s: %w", en.name, err)
	}
	return Result{}, fmt.Errorf("%w (ext=%q mime=%q)", ErrUnsupported, info.Ext(), info.NormalizedMime())
}

// enrich fills a missing mime hint by sniffing the first 512 bytes, so a bare
// stream with no filename still dispatches.
func enrich(r io.ReadSeeker, info StreamInfo) StreamInfo {
	if info.MimeType != "" {
		return info
	}
	var head [512]byte
	n, _ := io.ReadFull(r, head[:])
	_, _ = r.Seek(0, io.SeekStart)
	if n == 0 {
		return info
	}
	info.MimeType = http.DetectContentType(head[:n])
	return info
}

// Default returns the shared package-level Engine, built on first use.
//
// It is a function, not a variable, on purpose: Go initializes package-level
// variables BEFORE it runs init(), and every converter registers itself from an
// init(). A `var Default = New()` therefore captures an empty registry and makes
// every package-level helper return ErrUnsupported — which is exactly the bug
// this shape prevents. Building lazily guarantees registration has finished.
func Default() *Engine { return defaultEngine() }

var defaultEngine = sync.OnceValue(New)

// Convert converts a reader with the default engine.
func Convert(r io.Reader, info StreamInfo) (Result, error) {
	return Default().ConvertStream(r, info, nil)
}

// ConvertBytes converts an in-memory document with the default engine.
func ConvertBytes(b []byte, info StreamInfo) (Result, error) {
	return Default().ConvertBytes(b, info, nil)
}

// ConvertFile converts a file from disk with the default engine.
func ConvertFile(path string) (Result, error) {
	return Default().ConvertFile(path, nil)
}
