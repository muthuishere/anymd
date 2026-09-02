package anymd

// Content-addressed conversion cache.
//
// Converting the same document twice repeats all of the work. Hashing it is
// between 12x and 1411x cheaper than converting it (median ~230x on the
// project's own corpus; a PDF is the extreme), so a cache lookup is close to
// free while a hit saves the entire conversion.
//
// The whole design rests on one idea: the key must cover EVERYTHING that
// affects the output bytes, not just the input bytes. See CacheKey.

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"
)

// Cache stores conversion results under a key derived by CacheKey.
//
// It is deliberately two methods over a string key: an implementation can be a
// map, a directory (DiskCache), Redis, S3 or a CDN without anymd knowing. A
// Cache MUST be safe for concurrent use — the CLI converts with a worker pool.
//
// Put is fire-and-forget: a cache that cannot store an entry must drop it
// silently rather than fail a conversion that already succeeded.
type Cache interface {
	Get(key string) (Result, bool)
	Put(key string, res Result)
}

// ErrorCache is an optional interface a Cache may also implement to remember
// deterministic FAILURES.
//
// It is separate from Cache so that a remote or third-party cache can opt out
// with no ceremony, and so the Cache interface stays the two-method shape a
// caller expects. Only errors CacheableError accepts are ever stored.
type ErrorCache interface {
	GetErr(key string) (error, bool)
	PutErr(key string, err error)
}

// cacheableErrors is the allowlist of failures worth remembering: deterministic
// for these exact bytes, and expensive to rediscover.
//
// ErrNoTextLayer is the motivating case — proving a 200-page scan has no text
// layer costs 50ms+ and the answer can never change for the same bytes.
// ErrUnsupported is deterministic for the same bytes and the same registry, and
// the registry is already in the key.
//
// Everything else is denied by construction because this is an allowlist, not a
// denylist. ErrParseTimeout, ErrMaxDepth and any I/O error are ENVIRONMENTAL:
// a timeout on a loaded machine, a depth budget that the caller may raise, a
// disk that was briefly unreadable. Caching one of those poisons the cache with
// a failure that a later run would not have produced.
var cacheableErrors = []error{ErrNoTextLayer, ErrUnsupported}

// CacheableError reports whether err may be stored in a cache.
//
// It is exported so the policy is visible and testable rather than buried in a
// type switch, and so a caller writing their own Cache can apply exactly the
// same rule.
func CacheableError(err error) bool {
	if err == nil {
		return false
	}
	// Deny first, and deny explicitly: an error that somehow wrapped both an
	// allowlisted sentinel and a timeout must not be cached.
	for _, bad := range []error{ErrParseTimeout, ErrMaxDepth, os.ErrDeadlineExceeded, io.ErrUnexpectedEOF} {
		if errors.Is(err, bad) {
			return false
		}
	}
	for _, ok := range cacheableErrors {
		if errors.Is(err, ok) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// version
// ---------------------------------------------------------------------------

// modulePath is this module's import path, used to find our own version in the
// build info of a program that merely imports us.
const modulePath = "github.com/muthuishere/anymd"

// buildVersion is the fallback stamp. A release build sets it with
//
//	-ldflags "-X github.com/muthuishere/anymd.buildVersion=v1.2.3"
var buildVersion = "dev"

// Version reports the anymd version that the cache key is bound to.
//
// Why the cache key MUST contain it: commit c53cfbf fixed mdutil.Table emitting
// a blank line between rows. That changed the output bytes of every
// table-bearing document in every format. A content-only cache would have gone
// on serving the broken markdown after the upgrade, with no way for a user to
// work out why the fix "did not take". With the version in the key, an upgrade
// invalidates every entry for free, so `cache clean` is never required for
// correctness — only for disk space.
//
// Resolution order:
//
//  1. the anymd module's version from the importing program's build info
//     (bi.Deps — a consumer's bi.Main is THEIR module, not ours);
//  2. bi.Main.Version when anymd itself is the main module and was built from a
//     tagged download;
//  3. the VCS stamps go embeds for a build inside a git checkout
//     (vcs.revision, plus "+dirty" when the tree was modified);
//  4. buildVersion.
//
// The weakness is step 4. `go build` with VCS stamping off (-buildvcs=false, a
// tarball with no .git, `go test` in some setups) yields the constant "dev" for
// every build, so during development the cache can serve output produced by
// code you have since changed. Step 3 narrows that to "same commit, dirty tree"
// — the working-tree edit itself is invisible. The remedies are --no-cache
// while iterating, and `anymd cache clean`; both are documented on the CLI for
// exactly this reason.
func Version() string { return moduleVersion() }

var moduleVersion = sync.OnceValue(resolveVersion)

func resolveVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return buildVersion
	}
	for _, d := range bi.Deps {
		if d != nil && d.Path == modulePath && realVersion(d.Version) {
			return d.Version
		}
	}
	if bi.Main.Path == modulePath && realVersion(bi.Main.Version) {
		return bi.Main.Version
	}
	var rev, dirty string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value
		}
	}
	if rev != "" {
		if dirty == "true" {
			return rev + "+dirty"
		}
		return rev
	}
	return buildVersion
}

func realVersion(v string) bool { return v != "" && v != "(devel)" }

// ---------------------------------------------------------------------------
// key derivation
// ---------------------------------------------------------------------------

// keySchema changes whenever the encoding below changes, so an old entry can
// never be read back under a new interpretation of the same bytes.
const keySchema = "anymd-cache-key/1"

// CacheKey derives the cache key for one conversion.
//
// The key is SHA-256 over a canonical, length-prefixed encoding of everything
// that can change the output:
//
//   - the input bytes;
//   - the anymd version (see Version for why this is not optional);
//   - converter — which converter will handle the stream. The wrapper passes
//     the engine's ordered registry digest, which DETERMINES the answer:
//     dispatch is a pure function of (bytes, hints, options, registry), so two
//     conversions agreeing on all four cannot land on different converters. A
//     caller that already knows the name may pass it instead;
//   - the StreamInfo hints, because they steer dispatch (a .txt hint and a
//     .html hint on the same bytes produce different documents);
//   - the output-affecting Options: the remaining recursion budget,
//     KeepDataURIs, Charset, and WHETHER a Describer or Transcriber is set. An
//     LLM-captioned conversion is a different document from an uncaptioned one,
//     and confusing the two is the most user-visible way this cache could lie.
//
// Every field is written as a tag byte, then its length as a big-endian uint64,
// then its bytes. Length prefixing is what stops ("ab","c") and ("a","bc")
// hashing alike — a concatenation-based key would let a crafted filename move
// content across a field boundary and collide with a different document.
//
// Note what is NOT in the key: LLMTimeout and the Describer's identity. A
// Describer makes conversion non-deterministic in the first place; caching an
// LLM-captioned result caches one sampling of that model's output. That is
// usually what you want (it is why you are caching), but it is a choice, and
// two different Describers share a key. Use separate cache directories if that
// matters.
func CacheKey(content []byte, converter string, info StreamInfo, opts *Options) string {
	return cacheKeyWith(Version(), content, converter, info, opts)
}

// cacheKeyWith is CacheKey with the version supplied. Splitting it out is what
// makes "the same bytes under a different anymd version key differently"
// testable at all: Version() is resolved once per process from build info, so a
// test cannot otherwise vary it.
func cacheKeyWith(version string, content []byte, converter string, info StreamInfo, opts *Options) string {
	h := sha256.New()
	field(h, 'k', []byte(keySchema))
	field(h, 'v', []byte(version))
	field(h, 'c', []byte(converter))

	field(h, 'e', []byte(info.Extension))
	field(h, 'm', []byte(info.MimeType))
	field(h, 's', []byte(info.Charset))
	field(h, 'f', []byte(info.FileName))
	field(h, 'u', []byte(info.URL))

	var o Options
	if opts != nil {
		o = *opts
	}
	// The remaining budget, not MaxDepth: at depth 2 of an 8-deep budget the
	// output is whatever 6 more levels produce, which is exactly what a
	// top-level conversion with MaxDepth 6 produces. Encoding the remainder
	// makes those share an entry, and keeps MaxDepth 0 (meaning 8) and an
	// explicit 8 from being two different keys.
	remaining := -1
	if o.MaxDepth >= 0 {
		remaining = o.maxDepth() - o.depth
	}
	// Clamp before the cast: a negative remaining budget (depth past maxDepth)
	// would wrap to a huge uint64 and give two different states the same key.
	if remaining < 0 {
		remaining = 0
	}
	var num [8]byte
	binary.BigEndian.PutUint64(num[:], uint64(remaining))
	field(h, 'd', num[:])
	field(h, 'o', []byte{boolByte(o.KeepDataURIs), boolByte(o.Describer != nil), boolByte(o.Transcriber != nil)})
	field(h, 'C', []byte(o.Charset))

	field(h, 'b', content)
	return hex.EncodeToString(h.Sum(nil))
}

func field(h io.Writer, tag byte, b []byte) {
	var hdr [9]byte
	hdr[0] = tag
	binary.BigEndian.PutUint64(hdr[1:], uint64(len(b)))
	_, _ = h.Write(hdr[:])
	_, _ = h.Write(b)
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

// registryDigest is the "which converter" component: the engine's converter
// names in dispatch order. Two engines with the same registry share cache
// entries; registering a custom converter, or overriding a built-in, gives a
// disjoint key space, which is the correct outcome — the same bytes convert
// differently on those two engines.
func (e *Engine) registryDigest() string {
	h := sha256.New()
	for _, n := range e.Converters() {
		field(h, 'n', []byte(n))
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// ---------------------------------------------------------------------------
// in-memory LRU
// ---------------------------------------------------------------------------

type memEntry struct {
	key string
	res Result
	err error
}

// MemoryCache is a bounded, concurrency-safe in-memory LRU Cache.
//
// It bounds ENTRIES rather than bytes: a Result is a string, and counting bytes
// would make the common case (a process converting a few hundred documents)
// pay for accounting it does not need. Use DiskCache when the budget must be
// in bytes.
type MemoryCache struct {
	mu      sync.Mutex
	max     int
	ll      *list.List // front = most recently used
	items   map[string]*list.Element
	hits    atomic.Uint64
	misses  atomic.Uint64
	evicted atomic.Uint64
}

// DefaultMemoryEntries is the entry bound NewMemoryCache uses for max <= 0.
const DefaultMemoryEntries = 256

// NewMemoryCache returns an LRU holding at most max entries (DefaultMemoryEntries
// when max <= 0).
func NewMemoryCache(max int) *MemoryCache {
	if max <= 0 {
		max = DefaultMemoryEntries
	}
	return &MemoryCache{max: max, ll: list.New(), items: make(map[string]*list.Element, max)}
}

// Get implements Cache. An entry holding a cached ERROR is not a Get hit: the
// caller asked for a Result, and handing back the zero Result would turn a
// remembered failure into an empty document.
func (c *MemoryCache) Get(key string) (Result, bool) {
	res, err, ok := c.lookup(key)
	if !ok {
		c.misses.Add(1)
		return Result{}, false
	}
	if err != nil {
		return Result{}, false
	}
	c.hits.Add(1)
	return res, true
}

// lookup returns the entry, moving it to the front of the LRU. It does no
// counting: Get and GetErr are two halves of one lookup, and counting here
// would score every error hit as a miss followed by a hit.
func (c *MemoryCache) lookup(key string) (Result, error, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return Result{}, nil, false
	}
	c.ll.MoveToFront(el)
	e := el.Value.(*memEntry)
	return e.res, e.err, true
}

// Put implements Cache.
func (c *MemoryCache) Put(key string, res Result) { c.put(key, res, nil) }

// GetErr implements ErrorCache.
func (c *MemoryCache) GetErr(key string) (error, bool) {
	_, err, ok := c.lookup(key)
	if !ok || err == nil {
		return nil, false
	}
	c.hits.Add(1)
	return err, true
}

// PutErr implements ErrorCache.
func (c *MemoryCache) PutErr(key string, err error) {
	if !CacheableError(err) {
		return
	}
	c.put(key, Result{}, err)
}

func (c *MemoryCache) put(key string, res Result, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value.(*memEntry).res = res
		el.Value.(*memEntry).err = err
		c.ll.MoveToFront(el)
		return
	}
	c.items[key] = c.ll.PushFront(&memEntry{key: key, res: res, err: err})
	for c.ll.Len() > c.max {
		back := c.ll.Back()
		if back == nil {
			return
		}
		c.ll.Remove(back)
		delete(c.items, back.Value.(*memEntry).key)
		c.evicted.Add(1)
	}
}

// Len reports the number of entries currently held.
func (c *MemoryCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Stats reports lookups served and entries evicted.
func (c *MemoryCache) Stats() CacheStats {
	return CacheStats{
		Hits:    c.hits.Load(),
		Misses:  c.misses.Load(),
		Evicted: c.evicted.Load(),
		Entries: int64(c.Len()),
	}
}

// CacheStats is a cache's running counters. Entries and Bytes are a snapshot;
// the counters are cumulative for the life of the value.
type CacheStats struct {
	Hits    uint64
	Misses  uint64
	Evicted uint64
	Entries int64
	Bytes   int64
}

// HitRate returns hits/(hits+misses), or 0 when nothing has been looked up.
func (s CacheStats) HitRate() float64 {
	total := s.Hits + s.Misses
	if total == 0 {
		return 0
	}
	return float64(s.Hits) / float64(total)
}

// ---------------------------------------------------------------------------
// the wrapper
// ---------------------------------------------------------------------------

// CachedEngine wraps an *Engine so that repeating a conversion serves it from a
// Cache instead of redoing it.
//
// It is a wrapper rather than an Options field only because options.go and
// engine.go are owned elsewhere; the seam is deliberately the same one a field
// would use (see cachedConvert), so promoting it later changes no behaviour.
//
// The zero value is not usable; call NewCachedEngine.
type CachedEngine struct {
	engine *Engine
	cache  Cache
	digest string

	hits   atomic.Uint64
	misses atomic.Uint64
}

// NewCachedEngine wraps e so conversions are cached in c. A nil c is legal and
// disables caching, so a caller can write
//
//	eng := anymd.NewCachedEngine(anymd.New(), c)
//
// without branching.
func NewCachedEngine(e *Engine, c Cache) *CachedEngine {
	if e == nil {
		e = New()
	}
	return &CachedEngine{engine: e, cache: c, digest: e.registryDigest()}
}

// Engine returns the wrapped engine.
func (ce *CachedEngine) Engine() *Engine { return ce.engine }

// Cache returns the wrapped cache, which may be nil.
func (ce *CachedEngine) Cache() Cache { return ce.cache }

// Converters returns the wrapped engine's converter names in dispatch order.
func (ce *CachedEngine) Converters() []string { return ce.engine.Converters() }

// Stats reports this wrapper's hit and miss counts.
func (ce *CachedEngine) Stats() CacheStats {
	return CacheStats{Hits: ce.hits.Load(), Misses: ce.misses.Load()}
}

// ConvertBytes converts an in-memory document, via the cache.
func (ce *CachedEngine) ConvertBytes(b []byte, info StreamInfo, opts *Options) (Result, error) {
	if ce.cache == nil {
		return ce.engine.ConvertBytes(b, info, opts)
	}
	local := Options{}
	if opts != nil {
		local = *opts
	}
	return ce.convert(b, info, &local)
}

// ConvertStream converts an arbitrary reader, via the cache.
//
// Caching needs the whole input to hash it, so the stream is read into memory
// first — the engine buffers a non-seekable reader anyway, and hashing costs
// two to three orders of magnitude less than the conversion it saves.
func (ce *CachedEngine) ConvertStream(r io.Reader, info StreamInfo, opts *Options) (Result, error) {
	if ce.cache == nil {
		return ce.engine.ConvertStream(r, info, opts)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return Result{}, err
	}
	return ce.ConvertBytes(b, info, opts)
}

// ConvertFile converts a file from disk, via the cache.
func (ce *CachedEngine) ConvertFile(path string, opts *Options) (Result, error) {
	if ce.cache == nil {
		return ce.engine.ConvertFile(path, opts)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Result{}, err
	}
	return ce.ConvertBytes(b, StreamInfoForFile(path), opts)
}

func (ce *CachedEngine) convert(b []byte, info StreamInfo, opts *Options) (Result, error) {
	key := CacheKey(b, ce.digest, enrich(bytes.NewReader(b), info), opts)

	if res, ok := ce.cache.Get(key); ok {
		ce.hits.Add(1)
		return res, nil
	}
	if ec, ok := ce.cache.(ErrorCache); ok {
		if err, ok := ec.GetErr(key); ok {
			ce.hits.Add(1)
			return Result{}, err
		}
	}
	ce.misses.Add(1)

	res, err := ce.engine.convert(bytes.NewReader(b), info, opts)
	if err != nil {
		if ec, ok := ce.cache.(ErrorCache); ok && CacheableError(err) {
			ec.PutErr(key, err)
		}
		return Result{}, err
	}
	ce.cache.Put(key, res)
	return res, nil
}

// cachedConvert is the seam for promoting this to Options.Cache.
//
// It has exactly the signature Engine.ConvertStream's last line needs, so the
// promotion is: add `Cache Cache` to Options, and change that line from
//
//	return e.convert(rs, info, &o)
//
// to
//
//	return cachedConvert(e, rs, info, &o)
//
// Behaviour is unchanged when Options.Cache is nil, which is the default.
// Because it sits below ConvertStream it also covers Options.Recurse, so a zip
// member that appears in two archives is converted once — the depth component
// of the key is what makes that safe.
func cachedConvert(e *Engine, rs io.ReadSeeker, info StreamInfo, opts *Options) (Result, error) {
	if opts == nil || opts.Cache == nil {
		return e.convert(rs, info, opts)
	}
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return Result{}, err
	}
	b, err := io.ReadAll(rs)
	if err != nil {
		return Result{}, err
	}
	ce := &CachedEngine{engine: e, cache: opts.Cache, digest: e.registryDigest()}
	return ce.convert(b, info, opts)
}
