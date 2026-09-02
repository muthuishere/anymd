package anymd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// diskDir returns a cache directory deep enough to satisfy CheckCacheDir.
func diskDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "cache")
}

func newTestDisk(t *testing.T, maxBytes int64) *DiskCache {
	t.Helper()
	c, err := NewDiskCache(diskDir(t), maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// aKey is a syntactically valid key (64 hex chars) built from a seed, so tests
// can address entries without converting anything.
func aKey(seed string) string {
	return CacheKey([]byte(seed), "test", StreamInfo{}, nil)
}

func TestDiskCacheRoundTrip(t *testing.T) {
	c := newTestDisk(t, 0)
	k := aKey("a")
	if _, ok := c.Get(k); ok {
		t.Fatal("empty cache returned a hit")
	}
	c.Put(k, Result{Markdown: "# hello\n", Title: "hello"})

	got, ok := c.Get(k)
	if !ok || got.Markdown != "# hello\n" || got.Title != "hello" {
		t.Fatalf("Get = %+v, %v", got, ok)
	}

	// A second DiskCache over the same directory must see it: that is the whole
	// point of a disk cache, and it is also the multi-process case.
	c2, err := NewDiskCache(c.Dir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := c2.Get(k); !ok || got.Markdown != "# hello\n" {
		t.Fatalf("second opener: %+v %v", got, ok)
	}
}

func TestDiskCacheShards(t *testing.T) {
	c := newTestDisk(t, 0)
	k := aKey("a")
	c.Put(k, Result{Markdown: "x"})
	want := filepath.Join(c.Dir(), k[0:2], k[2:4], k+".json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("entry is not sharded at %s: %v", want, err)
	}
}

func TestDiskCacheCorruptIsAMiss(t *testing.T) {
	cases := map[string]string{
		"not json":        "}{ this is not json",
		"truncated":       `{"schema":1,"key":"`,
		"empty":           "",
		"wrong schema":    `{"schema":99,"key":"KEY","markdown":"boom"}`,
		"key mismatch":    `{"schema":1,"key":"0000","markdown":"someone else's document"}`,
		"unknown errcode": `{"schema":1,"key":"KEY","err_code":"from-the-future"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c := newTestDisk(t, 0)
			k := aKey(name)
			c.Put(k, Result{Markdown: "good"})
			p := filepath.Join(c.Dir(), k[0:2], k[2:4], k+".json")
			if err := os.WriteFile(p, []byte(strings.ReplaceAll(body, "KEY", k)), 0o644); err != nil {
				t.Fatal(err)
			}
			res, ok := c.Get(k)
			if ok {
				t.Fatalf("corrupt entry was served as a hit: %+v", res)
			}
			if _, ok := c.GetErr(k); ok {
				t.Fatal("corrupt entry was served as a cached error")
			}
		})
	}
}

func TestDiskCacheKeyVerificationBeatsTheFilename(t *testing.T) {
	c := newTestDisk(t, 0)
	victim, attacker := aKey("victim"), aKey("attacker")
	c.Put(attacker, Result{Markdown: "attacker document"})

	// Move the attacker's entry to the victim's path — the shape a SHA-256
	// collision, or a copied cache directory, would produce. The key inside the
	// payload no longer matches, so it must be a miss, never wrong content.
	src := filepath.Join(c.Dir(), attacker[0:2], attacker[2:4], attacker+".json")
	dst := filepath.Join(c.Dir(), victim[0:2], victim[2:4], victim+".json")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if res, ok := c.Get(victim); ok {
		t.Fatalf("served another document's content: %+v", res)
	}
}

func TestDiskCacheErrorRoundTrip(t *testing.T) {
	c := newTestDisk(t, 0)
	k := aKey("scan.pdf")
	orig := fmt.Errorf("anymd: PDFConverter: %w", ErrNoTextLayer)
	c.PutErr(k, orig)

	if _, ok := c.Get(k); ok {
		t.Fatal("a cached error must not surface as an empty Result")
	}
	err, ok := c.GetErr(k)
	if !ok {
		t.Fatal("cached error not found")
	}
	if !errors.Is(err, ErrNoTextLayer) {
		t.Fatalf("errors.Is broke across the round trip: %v", err)
	}
	if err.Error() != orig.Error() {
		t.Fatalf("message = %q, want %q", err.Error(), orig.Error())
	}

	c.PutErr(aKey("slow.xls"), ErrParseTimeout)
	if _, ok := c.GetErr(aKey("slow.xls")); ok {
		t.Fatal("ErrParseTimeout is environmental and must never reach the disk")
	}
	c.PutErr(aKey("deep.zip"), ErrMaxDepth)
	if _, ok := c.GetErr(aKey("deep.zip")); ok {
		t.Fatal("ErrMaxDepth must never reach the disk")
	}
}

func TestDiskCacheAtomicWriteLeavesNoTemps(t *testing.T) {
	c := newTestDisk(t, 0)
	for i := 0; i < 20; i++ {
		c.Put(aKey(fmt.Sprint(i)), Result{Markdown: strings.Repeat("x", 100)})
	}
	var temps []string
	_ = filepath.WalkDir(c.Dir(), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasPrefix(filepath.Base(p), ".tmp-") {
			temps = append(temps, p)
		}
		return nil
	})
	if len(temps) != 0 {
		t.Fatalf("temp files left behind: %v", temps)
	}
	// Every surviving file must be a complete, parseable entry.
	_ = filepath.WalkDir(c.Dir(), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			return nil
		}
		var e diskEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Errorf("%s is not a whole entry: %v", p, err)
		}
		return nil
	})
}

func TestDiskCacheConcurrentPut(t *testing.T) {
	c := newTestDisk(t, 0)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := aKey(fmt.Sprintf("doc-%d", i%6))
			c.Put(k, Result{Markdown: fmt.Sprintf("doc %d", i%6)})
			if res, ok := c.Get(k); ok && res.Markdown != fmt.Sprintf("doc %d", i%6) {
				t.Errorf("interleaved write produced %q", res.Markdown)
			}
			c.PutErr(aKey(fmt.Sprintf("err-%d", i%6)), ErrNoTextLayer)
			c.GetErr(aKey(fmt.Sprintf("err-%d", i%6)))
		}(i)
	}
	wg.Wait()

	// Every entry that exists must still be readable — a torn write would show
	// up here as a miss on a key we know we wrote.
	for i := 0; i < 6; i++ {
		if _, ok := c.Get(aKey(fmt.Sprintf("doc-%d", i))); !ok {
			t.Errorf("doc-%d lost", i)
		}
	}
}

func TestDiskCacheEvictsUnderBudget(t *testing.T) {
	// A tiny budget so the sweep triggers on the 1 MiB floor.
	c := newTestDisk(t, 4096)
	body := strings.Repeat("y", 2048)
	for i := 0; i < 12; i++ {
		c.Put(aKey(fmt.Sprintf("big-%d", i)), Result{Markdown: body})
	}
	if err := c.Sweep(); err != nil {
		t.Fatal(err)
	}
	s := c.Stats()
	if s.Bytes > c.MaxBytes() {
		t.Fatalf("cache is %d bytes, over its %d budget", s.Bytes, c.MaxBytes())
	}
	if s.Entries == 0 {
		t.Fatal("eviction emptied the cache; it should evict down to 80% of the budget")
	}
	if s.Evicted == 0 {
		t.Fatal("nothing was recorded as evicted")
	}
}

func TestDiskCacheCleanOnlyRemovesItsOwn(t *testing.T) {
	c := newTestDisk(t, 0)
	c.Put(aKey("a"), Result{Markdown: "a"})
	c.Put(aKey("b"), Result{Markdown: "b"})

	stranger := filepath.Join(c.Dir(), "NOTES.md")
	if err := os.WriteFile(stranger, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	n, err := c.Clean()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("removed %d entries, want 2", n)
	}
	if _, err := os.Stat(stranger); err != nil {
		t.Fatalf("Clean deleted a file that was not ours: %v", err)
	}
	if s := c.Stats(); s.Entries != 0 {
		t.Fatalf("entries after clean = %d", s.Entries)
	}
	if _, err := os.Stat(c.Dir()); err != nil {
		t.Fatalf("Clean removed the cache directory itself: %v", err)
	}
}

func TestCheckCacheDir(t *testing.T) {
	if err := CheckCacheDir(string(filepath.Separator)); !errors.Is(err, ErrUnsafeCacheDir) {
		t.Errorf("root accepted: %v", err)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if err := CheckCacheDir(home); !errors.Is(err, ErrUnsafeCacheDir) {
			t.Errorf("home directory accepted: %v", err)
		}
	}
	if err := CheckCacheDir(filepath.Join(t.TempDir(), "anymd")); err != nil {
		t.Errorf("a normal directory was rejected: %v", err)
	}
}

func TestDiskCacheRejectsForeignKeys(t *testing.T) {
	c := newTestDisk(t, 0)
	// Nothing that is not a lowercase-hex key of at least 8 chars may become a
	// path, so a caller cannot smuggle "../../etc/passwd" through as a key.
	for _, k := range []string{"", "abc", "../../etc/passwd", "ABCDEF01", "zzzzzzzz"} {
		c.Put(k, Result{Markdown: "x"})
		if _, ok := c.Get(k); ok {
			t.Errorf("key %q was accepted", k)
		}
	}
	var files int
	_ = filepath.WalkDir(c.Dir(), func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files++
		}
		return nil
	})
	if files != 0 {
		t.Fatalf("%d files written for rejected keys", files)
	}
}

// TestDiskCacheEndToEnd is the real thing: convert a file twice through a
// CachedEngine backed by a directory, and prove the second run does no work.
func TestDiskCacheEndToEnd(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "doc.txt")
	if err := os.WriteFile(src, []byte("hello cache\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fake := &cacheFakeConv{}
	disk, err := NewDiskCache(filepath.Join(dir, "cache"), 0)
	if err != nil {
		t.Fatal(err)
	}

	first := NewCachedEngine(fakeEngine(fake), disk)
	a, err := first.ConvertFile(src, nil)
	if err != nil {
		t.Fatal(err)
	}

	// A brand new engine and a brand new DiskCache over the same directory, as
	// a second process would have.
	disk2, err := NewDiskCache(disk.Dir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	second := NewCachedEngine(fakeEngine(fake), disk2)
	b, err := second.ConvertFile(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("%+v != %+v", a, b)
	}
	if fake.count() != 1 {
		t.Fatalf("converter ran %d times across two processes, want 1", fake.count())
	}
	if s := second.Stats(); s.Hits != 1 {
		t.Fatalf("second run stats = %+v", s)
	}
}

func BenchmarkDiskCacheHit(b *testing.B) {
	dir, err := os.MkdirTemp("", "anymd-bench")
	if err != nil {
		b.Fatal(err)
	}
	defer os.RemoveAll(dir)
	c, err := NewDiskCache(filepath.Join(dir, "cache"), 0)
	if err != nil {
		b.Fatal(err)
	}
	doc := benchDoc()
	ce := NewCachedEngine(New(), c)
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
