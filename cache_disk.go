package anymd

// DiskCache: a content-addressed cache in a directory, shared safely between
// processes.
//
// Every rule here exists because a cache that is WRONG is worse than no cache:
// a truncated file must not be served as a document, a hash collision must not
// hand one document's markdown to another, and a crash mid-write must leave the
// cache exactly as it was.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// diskSchema is the on-disk entry format version. A reader that finds any other
// value treats the entry as a miss, so an old cache directory is silently
// ignored rather than misread. Bump it whenever diskEntry changes meaning.
const diskSchema = 1

// DefaultCacheBytes is the disk budget NewDiskCache uses for maxBytes <= 0.
const DefaultCacheBytes = 256 << 20 // 256 MiB

// entrySuffix is the only filename shape the cache ever creates or deletes.
// Every removal is filtered on it, so a directory that also holds something
// else (a user pointing --cache-dir somewhere careless) cannot lose that file.
const entrySuffix = ".json"

// diskEntry is one cached conversion.
//
// Key is stored INSIDE the entry and verified on read. The filename already
// encodes the key, but a filename is not evidence: a partially written file
// finished by an unrelated crash, a directory copied between machines, or a
// (astronomically unlikely) SHA-256 collision would all present as a valid
// path. Verifying the key inside the payload makes any of those a miss.
type diskEntry struct {
	Schema   int       `json:"schema"`
	Key      string    `json:"key"`
	Version  string    `json:"anymd_version"`
	Markdown string    `json:"markdown,omitempty"`
	Title    string    `json:"title,omitempty"`
	ErrCode  string    `json:"err_code,omitempty"`
	ErrMsg   string    `json:"err_msg,omitempty"`
	Created  time.Time `json:"created"`
}

// errCodes maps a stable on-disk code to the sentinel it must still match with
// errors.Is after a round trip. Only errors CacheableError accepts appear here;
// an entry naming an unknown code is a miss.
var errCodes = map[string]error{
	"no-text-layer": ErrNoTextLayer,
	"unsupported":   ErrUnsupported,
}

func errCodeFor(err error) string {
	for code, sentinel := range errCodes {
		if errors.Is(err, sentinel) {
			return code
		}
	}
	return ""
}

// cachedError replays a remembered failure: the original message, still
// matching its sentinel under errors.Is.
type cachedError struct {
	msg      string
	sentinel error
}

func (e *cachedError) Error() string { return e.msg }
func (e *cachedError) Unwrap() error { return e.sentinel }

// DiskCache is a Cache backed by a directory, safe for concurrent use by
// multiple goroutines AND by multiple processes.
//
// Entries are sharded two levels deep by key prefix (ab/cd/<key>.json), which
// keeps any one directory to a few hundred files at 100k entries instead of
// 100k in one directory — a shape that makes ext4 and APFS lookups slow and
// `ls` unusable.
type DiskCache struct {
	dir      string
	maxBytes int64

	sweeping atomic.Bool
	pending  atomic.Int64 // bytes written since the last sweep

	hits    atomic.Uint64
	misses  atomic.Uint64
	evicted atomic.Uint64

	// mu serializes sweeps within this process. Across processes the atomic
	// rename and the tolerance for a vanished file are what make it safe.
	mu sync.Mutex
}

// DefaultCacheDir returns os.UserCacheDir()/anymd.
//
// Deliberately NOT ~/.config/anymd, where the LLM config lives: a cache is
// regenerable data, and putting it in the config directory means a user who
// backs up or syncs their dotfiles carries hundreds of megabytes of derived
// markdown with them. The XDG split exists for exactly this distinction.
func DefaultCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "anymd"), nil
}

// NewDiskCache opens (creating if needed) a cache directory with a byte budget.
// An empty dir means DefaultCacheDir; maxBytes <= 0 means DefaultCacheBytes.
func NewDiskCache(dir string, maxBytes int64) (*DiskCache, error) {
	if dir == "" {
		d, err := DefaultCacheDir()
		if err != nil {
			return nil, err
		}
		dir = d
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		maxBytes = DefaultCacheBytes
	}
	return &DiskCache{dir: abs, maxBytes: maxBytes}, nil
}

// Dir returns the absolute cache directory.
func (c *DiskCache) Dir() string { return c.dir }

// MaxBytes returns the disk budget.
func (c *DiskCache) MaxBytes() int64 { return c.maxBytes }

// path returns the sharded file path for a key. A key shorter than four
// characters cannot be sharded and is not one of ours, so it gets no path.
func (c *DiskCache) path(key string) (string, bool) {
	if len(key) < 8 || !hexOnly(key) {
		return "", false
	}
	return filepath.Join(c.dir, key[0:2], key[2:4], key+entrySuffix), true
}

func hexOnly(s string) bool {
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

// Get implements Cache. Anything unexpected — a missing file, a truncated
// file, invalid JSON, a schema we do not know, a key that does not match — is
// a MISS. A cache must never be a source of errors or of wrong content; the
// worst it may do is fail to help.
func (c *DiskCache) Get(key string) (Result, bool) {
	e, ok := c.load(key)
	if !ok {
		c.misses.Add(1)
		return Result{}, false
	}
	if e.ErrCode != "" {
		// A remembered failure is not a Result. GetErr will pick it up.
		return Result{}, false
	}
	c.hits.Add(1)
	return Result{Markdown: e.Markdown, Title: e.Title}, true
}

// GetErr implements ErrorCache.
func (c *DiskCache) GetErr(key string) (error, bool) {
	e, ok := c.load(key)
	if !ok || e.ErrCode == "" {
		return nil, false
	}
	sentinel, known := errCodes[e.ErrCode]
	if !known {
		// Written by a newer anymd that remembers more failures than we do.
		return nil, false
	}
	c.hits.Add(1)
	msg := e.ErrMsg
	if msg == "" {
		msg = sentinel.Error()
	}
	return &cachedError{msg: msg, sentinel: sentinel}, true
}

func (c *DiskCache) load(key string) (diskEntry, bool) {
	p, ok := c.path(key)
	if !ok {
		return diskEntry{}, false
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return diskEntry{}, false
	}
	var e diskEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		// Corrupt beyond repair, and it will never become valid. Dropping it
		// reclaims the space; failing to drop it is not an error either.
		c.discard(p)
		return diskEntry{}, false
	}
	if e.Schema != diskSchema || e.Key != key {
		c.discard(p)
		return diskEntry{}, false
	}
	// Touch for the LRU. atime is unreliable (relatime, noatime), so mtime is
	// the access clock; a failure here only costs eviction accuracy.
	now := time.Now()
	_ = os.Chtimes(p, now, now)
	return e, true
}

// discard removes an unusable entry, best effort and never fatal.
func (c *DiskCache) discard(p string) {
	if filepath.Ext(p) == entrySuffix && c.contains(p) {
		_ = os.Remove(p)
	}
}

// Put implements Cache.
func (c *DiskCache) Put(key string, res Result) {
	c.store(key, diskEntry{Markdown: res.Markdown, Title: res.Title})
}

// PutErr implements ErrorCache. Errors outside the allowlist are dropped.
func (c *DiskCache) PutErr(key string, err error) {
	if !CacheableError(err) {
		return
	}
	code := errCodeFor(err)
	if code == "" {
		return
	}
	c.store(key, diskEntry{ErrCode: code, ErrMsg: err.Error()})
}

func (c *DiskCache) store(key string, e diskEntry) {
	p, ok := c.path(key)
	if !ok {
		return
	}
	e.Schema = diskSchema
	e.Key = key
	e.Version = Version()
	e.Created = time.Now().UTC()
	raw, err := json.Marshal(e)
	if err != nil {
		return
	}
	if err := writeAtomic(p, raw); err != nil {
		return
	}
	c.pending.Add(int64(len(raw)))
	c.maybeSweep()
}

// writeAtomic writes via a temp file in the SAME directory plus a rename.
//
// Rename within a directory is atomic on every filesystem we target, so a
// reader — in this process or another — sees either the old entry or the whole
// new one, never a half-written file that would later be served as truth. A
// plain os.WriteFile interrupted by a crash or a full disk leaves exactly that.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	// Sync before rename: the rename can otherwise reach disk before the data,
	// so a power loss leaves a correctly named, empty entry.
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// maybeSweep enforces the budget without walking the tree on every Put. A walk
// is O(entries); doing one per Put would cost more than the conversions saved.
func (c *DiskCache) maybeSweep() {
	trigger := c.maxBytes / 8
	if trigger < 1<<20 {
		trigger = 1 << 20
	}
	if c.pending.Load() < trigger {
		return
	}
	if !c.sweeping.CompareAndSwap(false, true) {
		return // another goroutine is already sweeping
	}
	defer c.sweeping.Store(false)
	c.pending.Store(0)
	_ = c.Sweep()
}

// Sweep enforces the byte budget, deleting least-recently-used entries until
// the cache is at 80% of it. Going to exactly the budget would make the next
// Put sweep again; leaving headroom amortizes the walk.
//
// Concurrency: another process may be reading, writing or deleting the same
// files. Every removal tolerates a file that is already gone, and a reader that
// loses the race gets a miss.
func (c *DiskCache) Sweep() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	type item struct {
		path string
		size int64
		mod  time.Time
	}
	var items []item
	var total int64
	err := filepath.WalkDir(c.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable subtree is not a reason to stop
		}
		if d.IsDir() || filepath.Ext(p) != entrySuffix {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		items = append(items, item{p, info.Size(), info.ModTime()})
		total += info.Size()
		return nil
	})
	if err != nil {
		return err
	}
	if total <= c.maxBytes {
		return nil
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.Before(items[j].mod) })
	target := c.maxBytes * 4 / 5
	for _, it := range items {
		if total <= target {
			break
		}
		if err := os.Remove(it.path); err != nil && !os.IsNotExist(err) {
			continue
		}
		total -= it.size
		c.evicted.Add(1)
	}
	return nil
}

// Stats walks the cache and reports entry count and total size alongside this
// value's cumulative counters.
//
// Hits and misses are per-process and not persisted: a hit rate across
// invocations would need a counter file written on every lookup, which is a
// write on the read path — exactly what a cache should not add.
func (c *DiskCache) Stats() CacheStats {
	s := CacheStats{Hits: c.hits.Load(), Misses: c.misses.Load(), Evicted: c.evicted.Load()}
	_ = filepath.WalkDir(c.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != entrySuffix {
			return nil
		}
		if info, err := d.Info(); err == nil {
			s.Entries++
			s.Bytes += info.Size()
		}
		return nil
	})
	return s
}

// Clean removes every entry and every empty shard directory, and returns how
// many entries it removed.
//
// It deletes only files ending in entrySuffix, and only files that pass
// contains — so a cache directory that someone also keeps notes in loses the
// cache and nothing else, and a caller who resolved the directory wrongly
// cannot turn Clean into `rm -rf`.
func (c *DiskCache) Clean() (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	removed := 0
	var dirs []string
	err := filepath.WalkDir(c.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != c.dir {
				dirs = append(dirs, p)
			}
			return nil
		}
		if filepath.Ext(p) != entrySuffix || !c.contains(p) {
			return nil
		}
		if err := os.Remove(p); err == nil {
			removed++
		}
		return nil
	})
	if err != nil {
		return removed, err
	}
	// Deepest first, and only if empty: os.Remove on a directory refuses to
	// delete a non-empty one, which is precisely the guard we want.
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, d := range dirs {
		_ = os.Remove(d)
	}
	return removed, nil
}

// contains reports whether p is inside the cache directory, after resolving
// "..". It is the containment check that makes a mistyped --cache-dir survivable.
func (c *DiskCache) contains(p string) bool {
	abs, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(c.dir, abs)
	if err != nil {
		return false
	}
	return rel != ".." && !hasDotDotPrefix(rel) && !filepath.IsAbs(rel)
}

func hasDotDotPrefix(rel string) bool {
	return len(rel) >= 3 && rel[0] == '.' && rel[1] == '.' && os.IsPathSeparator(rel[2])
}

// ErrUnsafeCacheDir reports a cache directory that must not be operated on.
var ErrUnsafeCacheDir = errors.New("anymd: refusing to operate on this cache directory")

// CheckCacheDir rejects a directory that `cache clean` must never be pointed
// at: the filesystem root, a home directory, or anything else with no path
// segments below the root.
//
// The failure this prevents is a typo — `--cache-dir /` — turning a cleanup
// into data loss. Clean is already limited to its own file suffix, so this is
// the second of two independent guards, not the only one.
func CheckCacheDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	abs = filepath.Clean(abs)
	if abs == string(filepath.Separator) || abs == filepath.VolumeName(abs)+string(filepath.Separator) {
		return fmt.Errorf("%w: %s is the filesystem root", ErrUnsafeCacheDir, abs)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if filepath.Clean(home) == abs {
			return fmt.Errorf("%w: %s is your home directory", ErrUnsafeCacheDir, abs)
		}
	}
	// Two segments minimum ("/tmp/x", "C:\\tmp\\x"): a single top-level
	// directory is far more likely to be a typo than a cache location.
	rest := strings.TrimPrefix(abs, filepath.VolumeName(abs))
	segs := 0
	for _, part := range strings.Split(rest, string(filepath.Separator)) {
		if part != "" {
			segs++
		}
	}
	if segs < 2 {
		return fmt.Errorf("%w: %s is too close to the filesystem root", ErrUnsafeCacheDir, abs)
	}
	return nil
}
