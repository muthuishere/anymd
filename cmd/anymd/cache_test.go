package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muthuishere/anymd"
)

// cacheRun drives the cache subcommand the way main does, capturing both
// streams.
func cacheRun(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := runCacheCommand(args, &out, &errb)
	return code, out.String(), errb.String()
}

func cacheTempDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "anymd-cache")
}

func TestCacheCommandPath(t *testing.T) {
	dir := cacheTempDir(t)
	code, out, errs := cacheRun("path", "--cache-dir", dir)
	if code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errs)
	}
	if strings.TrimSpace(out) != dir {
		t.Fatalf("out = %q, want %q", out, dir)
	}
	// `cache path` is what a script calls to find out whether a cache exists.
	// Printing the path must not bring one into being.
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("cache path created the directory")
	}

	// The flag is accepted on either side of the verb, like `anymd config`.
	if code, out2, _ := cacheRun("--cache-dir", dir, "path"); code != exitOK || strings.TrimSpace(out2) != dir {
		t.Fatalf("flag-before-verb form: code %d out %q", code, out2)
	}
}

func TestCacheCommandPathDefaultsToUserCacheDir(t *testing.T) {
	code, out, _ := cacheRun("path")
	if code != exitOK {
		t.Fatalf("code = %d", code)
	}
	want, err := anymd.DefaultCacheDir()
	if err != nil {
		t.Skip("no user cache dir on this machine")
	}
	if strings.TrimSpace(out) != want {
		t.Fatalf("out = %q, want %q", strings.TrimSpace(out), want)
	}
	// A cache is regenerable data and must not land in the config directory.
	if strings.Contains(want, filepath.Join(".config", "anymd")) {
		t.Errorf("the cache is in the config directory: %s", want)
	}
}

func TestCacheCommandStats(t *testing.T) {
	dir := cacheTempDir(t)
	code, out, _ := cacheRun("stats", "--cache-dir", dir)
	if code != exitOK || !strings.Contains(out, "entries:     0") {
		t.Fatalf("stats on a missing directory: code %d out %q", code, out)
	}

	c, err := anymd.NewDiskCache(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	c.Put(anymd.CacheKey([]byte("a"), "x", anymd.StreamInfo{}, nil), anymd.Result{Markdown: "hello"})
	c.Put(anymd.CacheKey([]byte("b"), "x", anymd.StreamInfo{}, nil), anymd.Result{Markdown: "world"})

	code, out, _ = cacheRun("stats", "--cache-dir", dir)
	if code != exitOK {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out, "entries:     2") {
		t.Fatalf("out = %q, want 2 entries", out)
	}
	for _, want := range []string{"size:", "budget:", "version key:"} {
		if !strings.Contains(out, want) {
			t.Errorf("stats is missing %q:\n%s", want, out)
		}
	}
}

func TestCacheCommandClean(t *testing.T) {
	dir := cacheTempDir(t)
	c, err := anymd.NewDiskCache(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	c.Put(anymd.CacheKey([]byte("a"), "x", anymd.StreamInfo{}, nil), anymd.Result{Markdown: "hello"})

	code, out, errs := cacheRun("clean", "--cache-dir", dir)
	if code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errs)
	}
	if !strings.Contains(out, "removed 1 entries") {
		t.Fatalf("out = %q", out)
	}
	if s := c.Stats(); s.Entries != 0 {
		t.Fatalf("%d entries survived clean", s.Entries)
	}
}

// TestCacheCleanRefusesDangerousDirs is the guard that makes a typo survivable:
// `--cache-dir /` must never turn a cleanup into data loss.
func TestCacheCleanRefusesDangerousDirs(t *testing.T) {
	bad := []string{string(filepath.Separator)}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		bad = append(bad, home)
	}
	for _, dir := range bad {
		code, _, errs := cacheRun("clean", "--cache-dir", dir)
		if code != exitUsage {
			t.Errorf("clean %s: code = %d, want %d", dir, code, exitUsage)
		}
		if !strings.Contains(errs, "refusing") {
			t.Errorf("clean %s: stderr = %q", dir, errs)
		}
	}
}

func TestCacheCommandUnknownVerb(t *testing.T) {
	code, _, errs := cacheRun("nuke")
	if code != exitUsage {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(errs, "unknown cache command") {
		t.Fatalf("stderr = %q", errs)
	}
	if code, _, _ := cacheRun(); code != exitUsage {
		t.Fatalf("no verb: code = %d", code)
	}
}

// parseCacheFlags mirrors what main's flag set does, so the resolution tests
// exercise the real parse path rather than hand-built structs.
func parseCacheFlags(t *testing.T, args ...string) (*cacheOptions, map[string]bool) {
	t.Helper()
	co := &cacheOptions{}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(new(bytes.Buffer))
	registerCacheFlags(fs, co)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return co, set
}

func TestResolveCacheDefaultsOff(t *testing.T) {
	co, set := parseCacheFlags(t)
	var errb bytes.Buffer
	c, code := resolveCache(co, set, &errb)
	if code != exitOK {
		t.Fatalf("code = %d", code)
	}
	if c != nil {
		t.Fatal("caching must be off unless asked for: a CLI does not write to a user's disk unbidden")
	}
}

func TestResolveCacheNoCacheWins(t *testing.T) {
	dir := cacheTempDir(t)
	co, set := parseCacheFlags(t, "--cache", "--no-cache", "--cache-dir", dir)
	var errb bytes.Buffer
	c, code := resolveCache(co, set, &errb)
	if code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
	if c != nil {
		t.Fatal("--no-cache must win over --cache")
	}
	if !strings.Contains(errb.String(), "overrides") {
		t.Fatalf("the override must be reported, not silent: %q", errb.String())
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("a disabled cache created its directory")
	}
}

func TestResolveCacheDirRequiresCache(t *testing.T) {
	co, set := parseCacheFlags(t, "--cache-dir", cacheTempDir(t))
	var errb bytes.Buffer
	_, code := resolveCache(co, set, &errb)
	if code != exitUsage {
		t.Fatalf("code = %d, want a usage error", code)
	}
	if !strings.Contains(errb.String(), "requires --cache") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

func TestResolveCacheRejectsRoot(t *testing.T) {
	co, set := parseCacheFlags(t, "--cache", "--cache-dir", string(filepath.Separator))
	var errb bytes.Buffer
	_, code := resolveCache(co, set, &errb)
	if code != exitUsage {
		t.Fatalf("code = %d, want a usage error for --cache-dir /", code)
	}
}

// TestCacheEndToEndSecondConversionHits converts a real file twice through the
// cache the CLI flags produce. The second conversion must be served from disk.
func TestCacheEndToEndSecondConversionHits(t *testing.T) {
	dir := cacheTempDir(t)
	src := filepath.Join(t.TempDir(), "doc.txt")
	if err := os.WriteFile(src, []byte("# heading\n\nbody text\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	co, set := parseCacheFlags(t, "--cache", "--cache-dir", dir)
	var errb bytes.Buffer
	c, code := resolveCache(co, set, &errb)
	if code != exitOK || c == nil {
		t.Fatalf("resolveCache: code %d stderr %s", code, errb.String())
	}

	first := anymd.NewCachedEngine(anymd.New(), c)
	a, err := first.ConvertFile(src, nil)
	if err != nil {
		t.Fatal(err)
	}

	// A second process: new engine, new cache handle, same directory.
	c2, code := resolveCache(co, set, &errb)
	if code != exitOK {
		t.Fatal(errb.String())
	}
	second := anymd.NewCachedEngine(anymd.New(), c2)
	b, err := second.ConvertFile(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Markdown != b.Markdown || a.Title != b.Title {
		t.Fatalf("cached conversion differs:\n%q\n%q", a.Markdown, b.Markdown)
	}
	if s := second.Stats(); s.Hits != 1 || s.Misses != 0 {
		t.Fatalf("second run stats = %+v, want a pure hit", s)
	}

	// And `cache stats` sees what the conversion left behind.
	if _, out, _ := cacheRun("stats", "--cache-dir", dir); !strings.Contains(out, "entries:     1") {
		t.Fatalf("stats after a cached conversion: %q", out)
	}
}

// TestCacheFlagUsageDocumentsTheFlags keeps the help text and the flags in step:
// a flag nobody can discover may as well not exist.
func TestCacheFlagUsageDocumentsTheFlags(t *testing.T) {
	for _, want := range []string{"--cache", "--no-cache", "--cache-dir"} {
		if !strings.Contains(cacheFlagUsage, want) {
			t.Errorf("the usage block does not mention %s", want)
		}
	}
	if !strings.Contains(cacheFlagUsage, "off by default") {
		t.Error("the usage block must say caching is off by default")
	}
}
