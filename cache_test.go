package anymd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

// cacheFakeConv is a converter that always accepts, returns what it is told to,
// and counts how often Convert actually ran. The count is the only honest proof
// that a second conversion was served from cache rather than redone.
type cacheFakeConv struct {
	mu    sync.Mutex
	calls int
	res   Result
	err   error
}

func (c *cacheFakeConv) Name() string { return "CacheFake" }

func (c *cacheFakeConv) Accepts(io.ReadSeeker, StreamInfo, *Options) bool { return true }

func (c *cacheFakeConv) Convert(r io.ReadSeeker, _ StreamInfo, _ *Options) (Result, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	if c.err != nil {
		return Result{}, c.err
	}
	if c.res.Markdown != "" {
		return c.res, nil
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return Result{}, err
	}
	return Result{Markdown: strings.ToUpper(string(b))}, nil
}

func (c *cacheFakeConv) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func fakeEngine(c Converter) *Engine {
	e := &Engine{}
	e.Register(c)
	return e
}

// ---------------------------------------------------------------------------
// key derivation
// ---------------------------------------------------------------------------

func TestCacheKeyIsStable(t *testing.T) {
	a := CacheKey([]byte("hello"), "conv", StreamInfo{Extension: ".txt"}, &Options{})
	b := CacheKey([]byte("hello"), "conv", StreamInfo{Extension: ".txt"}, &Options{})
	if a != b {
		t.Fatalf("key is not deterministic: %s != %s", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("want a 64-char sha256 hex key, got %d chars", len(a))
	}
	// A nil Options must key the same as the zero Options: the zero value is
	// documented as valid, and a caller passing nil means exactly that.
	if n := CacheKey([]byte("hello"), "conv", StreamInfo{Extension: ".txt"}, nil); n != a {
		t.Fatalf("nil Options keyed differently from &Options{}")
	}
}

func TestCacheKeyVaries(t *testing.T) {
	base := func() (string, []byte, string, StreamInfo, *Options) {
		return "v1.0.0", []byte("hello"), "conv", StreamInfo{Extension: ".txt"}, &Options{}
	}
	v, c, cv, info, o := base()
	ref := cacheKeyWith(v, c, cv, info, o)

	cases := []struct {
		name string
		key  string
	}{
		{"different content", cacheKeyWith(v, []byte("hello!"), cv, info, o)},
		{"different version", cacheKeyWith("v1.0.1", c, cv, info, o)},
		{"different converter", cacheKeyWith(v, c, "other", info, o)},
		{"different extension", cacheKeyWith(v, c, cv, StreamInfo{Extension: ".html"}, o)},
		{"different mime", cacheKeyWith(v, c, cv, StreamInfo{Extension: ".txt", MimeType: "text/html"}, o)},
		{"different info charset", cacheKeyWith(v, c, cv, StreamInfo{Extension: ".txt", Charset: "latin1"}, o)},
		{"different filename", cacheKeyWith(v, c, cv, StreamInfo{Extension: ".txt", FileName: "a.txt"}, o)},
		{"different url", cacheKeyWith(v, c, cv, StreamInfo{Extension: ".txt", URL: "http://x"}, o)},
		{"keep data uris", cacheKeyWith(v, c, cv, info, &Options{KeepDataURIs: true})},
		{"different max depth", cacheKeyWith(v, c, cv, info, &Options{MaxDepth: 2})},
		{"recursion disabled", cacheKeyWith(v, c, cv, info, &Options{MaxDepth: -1})},
		{"different opts charset", cacheKeyWith(v, c, cv, info, &Options{Charset: "utf-16"})},
		{"describer set", cacheKeyWith(v, c, cv, info, &Options{Describer: cacheStubDescriber{}})},
		{"transcriber set", cacheKeyWith(v, c, cv, info, &Options{Transcriber: cacheStubTranscriber{}})},
	}
	for _, tc := range cases {
		if tc.key == ref {
			t.Errorf("%s: key did not change", tc.name)
		}
	}

	// MaxDepth 0 means 8, so the two must agree — otherwise every caller who
	// spells the default out explicitly gets a second, redundant cache entry.
	if cacheKeyWith(v, c, cv, info, &Options{MaxDepth: 8}) != ref {
		t.Errorf("MaxDepth 0 and MaxDepth 8 are the same budget but keyed differently")
	}
}

type cacheStubDescriber struct{}

func (cacheStubDescriber) Describe(_ context.Context, _ []byte, _, _ string) (string, error) {
	return "", nil
}

type cacheStubTranscriber struct{}

func (cacheStubTranscriber) Transcribe(_ context.Context, _ []byte, _ string) (string, error) {
	return "", nil
}

// FuzzCacheKey is the most important test in this file. A key collision is the
// one bug this package can have that silently serves one document's content in
// place of another's, so the property under test is exactly that: two different
// (content, version, converter, options) tuples must never share a key, and two
// identical ones must always share one.
func FuzzCacheKey(f *testing.F) {
	f.Add([]byte("a"), []byte("a"), "v1", "v1", "docx", "docx", false, false)
	f.Add([]byte("ab"), []byte("a"), "v1", "bv1", "docx", "docx", false, true)
	f.Add([]byte(""), []byte(""), "", "", "", "", false, false)
	// The classic concatenation collision: ("ab","c") vs ("a","bc"). Length
	// prefixing is what makes these different keys.
	f.Add([]byte("ab"), []byte("a"), "c", "bc", "", "", false, false)

	f.Fuzz(func(t *testing.T, b1, b2 []byte, v1, v2, c1, c2 string, k1, k2 bool) {
		o1 := &Options{KeepDataURIs: k1}
		o2 := &Options{KeepDataURIs: k2}
		k := cacheKeyWith(v1, b1, c1, StreamInfo{}, o1)
		l := cacheKeyWith(v2, b2, c2, StreamInfo{}, o2)

		same := string(b1) == string(b2) && v1 == v2 && c1 == c2 && k1 == k2
		if same && k != l {
			t.Fatalf("identical inputs produced different keys")
		}
		if !same && k == l {
			t.Fatalf("distinct inputs collided: content %q/%q version %q/%q converter %q/%q keep %v/%v",
				b1, b2, v1, v2, c1, c2, k1, k2)
		}
	})
}

// ---------------------------------------------------------------------------
// error policy
// ---------------------------------------------------------------------------

func TestCacheableError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{ErrNoTextLayer, true},
		{fmt.Errorf("anymd: PDFConverter: %w", ErrNoTextLayer), true},
		{ErrUnsupported, true},
		{&UnsupportedError{Ext: ".zzz"}, true},
		{ErrParseTimeout, false},
		{fmt.Errorf("anymd: XLSConverter: %w", ErrParseTimeout), false},
		{ErrMaxDepth, false},
		{errors.New("read /dev/x: input/output error"), false},
		{io.ErrUnexpectedEOF, false},
	}
	for _, tc := range cases {
		if got := CacheableError(tc.err); got != tc.want {
			t.Errorf("CacheableError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// memory cache
// ---------------------------------------------------------------------------

func TestMemoryCacheHitMiss(t *testing.T) {
	c := NewMemoryCache(4)
	if _, ok := c.Get("k"); ok {
		t.Fatal("empty cache returned a hit")
	}
	c.Put("k", Result{Markdown: "# hi", Title: "hi"})
	got, ok := c.Get("k")
	if !ok || got.Markdown != "# hi" || got.Title != "hi" {
		t.Fatalf("Get = %+v, %v", got, ok)
	}
	s := c.Stats()
	if s.Hits != 1 || s.Misses != 1 {
		t.Fatalf("stats = %+v, want 1 hit 1 miss", s)
	}
}

func TestMemoryCacheEvictsLRU(t *testing.T) {
	c := NewMemoryCache(2)
	c.Put("a", Result{Markdown: "a"})
	c.Put("b", Result{Markdown: "b"})
	if _, ok := c.Get("a"); !ok { // a is now the most recent
		t.Fatal("a missing")
	}
	c.Put("cc", Result{Markdown: "c"})
	if c.Len() != 2 {
		t.Fatalf("len = %d, want 2", c.Len())
	}
	if _, ok := c.Get("b"); ok {
		t.Error("b should have been evicted as least recently used")
	}
	if _, ok := c.Get("a"); !ok {
		t.Error("a was used most recently and must survive")
	}
}

func TestMemoryCacheErrors(t *testing.T) {
	c := NewMemoryCache(4)
	c.PutErr("k", fmt.Errorf("anymd: PDFConverter: %w", ErrNoTextLayer))
	if _, ok := c.Get("k"); ok {
		t.Fatal("Get must not turn a remembered error into an empty document")
	}
	err, ok := c.GetErr("k")
	if !ok || !errors.Is(err, ErrNoTextLayer) {
		t.Fatalf("GetErr = %v, %v", err, ok)
	}
	c.PutErr("t", ErrParseTimeout)
	if _, ok := c.GetErr("t"); ok {
		t.Fatal("a timeout is environmental and must never be cached")
	}
}

func TestMemoryCacheConcurrent(t *testing.T) {
	c := NewMemoryCache(64)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := fmt.Sprintf("key-%d", i%8)
			c.Put(k, Result{Markdown: k})
			c.Get(k)
			c.PutErr(k+"-e", ErrNoTextLayer)
			c.GetErr(k + "-e")
		}(i)
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// the wrapper
// ---------------------------------------------------------------------------

func TestCachedEngineServesSecondConversion(t *testing.T) {
	fake := &cacheFakeConv{}
	ce := NewCachedEngine(fakeEngine(fake), NewMemoryCache(8))

	first, err := ce.ConvertBytes([]byte("hello"), StreamInfo{Extension: ".txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ce.ConvertBytes([]byte("hello"), StreamInfo{Extension: ".txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("cached result differs: %+v vs %+v", first, second)
	}
	if fake.count() != 1 {
		t.Fatalf("converter ran %d times, want 1 (the second call must be a hit)", fake.count())
	}
	if s := ce.Stats(); s.Hits != 1 || s.Misses != 1 {
		t.Fatalf("stats = %+v", s)
	}

	// Different bytes must not be served the first document.
	other, err := ce.ConvertBytes([]byte("world"), StreamInfo{Extension: ".txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if other.Markdown != "WORLD" {
		t.Fatalf("different content served %q", other.Markdown)
	}
}

func TestCachedEngineDescriberChangesKey(t *testing.T) {
	fake := &cacheFakeConv{}
	ce := NewCachedEngine(fakeEngine(fake), NewMemoryCache(8))
	in := []byte("hello")
	if _, err := ce.ConvertBytes(in, StreamInfo{}, &Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ce.ConvertBytes(in, StreamInfo{}, &Options{Describer: cacheStubDescriber{}}); err != nil {
		t.Fatal(err)
	}
	if fake.count() != 2 {
		t.Fatalf("converter ran %d times; an LLM-captioned conversion is a different document and must not hit", fake.count())
	}
}

func TestCachedEngineNilCacheIsPassthrough(t *testing.T) {
	fake := &cacheFakeConv{}
	ce := NewCachedEngine(fakeEngine(fake), nil)
	for i := 0; i < 3; i++ {
		if _, err := ce.ConvertBytes([]byte("x"), StreamInfo{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if fake.count() != 3 {
		t.Fatalf("converter ran %d times, want 3 with caching off", fake.count())
	}
}

func TestCachedEngineCachesNoTextLayerButNotTimeout(t *testing.T) {
	fake := &cacheFakeConv{err: ErrNoTextLayer}
	ce := NewCachedEngine(fakeEngine(fake), NewMemoryCache(8))
	for i := 0; i < 2; i++ {
		_, err := ce.ConvertBytes([]byte("scan"), StreamInfo{Extension: ".pdf"}, nil)
		if !errors.Is(err, ErrNoTextLayer) {
			t.Fatalf("err = %v, want ErrNoTextLayer", err)
		}
	}
	if fake.count() != 1 {
		t.Fatalf("converter ran %d times; proving a scan has no text layer must be remembered", fake.count())
	}

	slow := &cacheFakeConv{err: ErrParseTimeout}
	ce2 := NewCachedEngine(fakeEngine(slow), NewMemoryCache(8))
	for i := 0; i < 2; i++ {
		if _, err := ce2.ConvertBytes([]byte("sheet"), StreamInfo{Extension: ".xls"}, nil); !errors.Is(err, ErrParseTimeout) {
			t.Fatalf("err = %v, want ErrParseTimeout", err)
		}
	}
	if slow.count() != 2 {
		t.Fatalf("converter ran %d times; a timeout is environmental and must be retried", slow.count())
	}
}

func TestCachedEngineStreamAndFile(t *testing.T) {
	fake := &cacheFakeConv{}
	ce := NewCachedEngine(fakeEngine(fake), NewMemoryCache(8))
	for i := 0; i < 2; i++ {
		res, err := ce.ConvertStream(strings.NewReader("abc"), StreamInfo{Extension: ".txt"}, nil)
		if err != nil || res.Markdown != "ABC" {
			t.Fatalf("res = %+v err = %v", res, err)
		}
	}
	if fake.count() != 1 {
		t.Fatalf("ConvertStream converted %d times, want 1", fake.count())
	}
}

// TestRegistryDigestDistinguishesEngines proves the "which converter" component
// works: the same bytes on two differently-populated engines must not share an
// entry, because they do not produce the same document.
func TestRegistryDigestDistinguishesEngines(t *testing.T) {
	a := fakeEngine(&cacheFakeConv{})
	b := New()
	if a.registryDigest() == b.registryDigest() {
		t.Fatal("engines with different registries produced the same digest")
	}
	if New().registryDigest() != New().registryDigest() {
		t.Fatal("the same registry produced two digests")
	}
}

func TestVersionIsNonEmpty(t *testing.T) {
	if Version() == "" {
		t.Fatal("Version must never be empty: an empty version component silently weakens every key")
	}
}

// ---------------------------------------------------------------------------
// benchmarks
// ---------------------------------------------------------------------------

func benchDoc() []byte {
	var sb strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&sb, "line %d of a document that costs real work to convert\n", i)
	}
	return []byte(sb.String())
}

func BenchmarkCacheMiss(b *testing.B) {
	doc := benchDoc()
	e := New()
	b.SetBytes(int64(len(doc)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.ConvertBytes(doc, StreamInfo{Extension: ".txt"}, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCacheHit(b *testing.B) {
	doc := benchDoc()
	ce := NewCachedEngine(New(), NewMemoryCache(8))
	if _, err := ce.ConvertBytes(doc, StreamInfo{Extension: ".txt"}, nil); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(doc)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ce.ConvertBytes(doc, StreamInfo{Extension: ".txt"}, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCacheKey(b *testing.B) {
	doc := benchDoc()
	b.SetBytes(int64(len(doc)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = CacheKey(doc, "digest", StreamInfo{Extension: ".txt"}, nil)
	}
}

// TestCachedConvertSeam exercises cachedConvert, the function Engine.ConvertStream
// would call once Options grows a Cache field. Until then Options.cacheOrNil
// reports no cache, so the seam must be an exact passthrough — that is what
// makes the promotion a behaviour-free change.
func TestCachedConvertSeam(t *testing.T) {
	fake := &cacheFakeConv{}
	e := fakeEngine(fake)
	opts := &Options{}
	opts.engine = e
	for i := 0; i < 2; i++ {
		res, err := cachedConvert(e, strings.NewReader("seam"), StreamInfo{Extension: ".txt"}, opts)
		if err != nil || res.Markdown != "SEAM" {
			t.Fatalf("res = %+v err = %v", res, err)
		}
	}
	if fake.count() != 2 {
		t.Fatalf("converter ran %d times; with no Options.Cache the seam must not cache", fake.count())
	}
}
