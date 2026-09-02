package main

// The conversion cache, on the command line.
//
// Two rules shape everything here:
//
//   - It is OFF by default. A library and a CLI must not start writing to a
//     user's disk because they were run. And for a one-shot conversion the
//     cache is pure cost: you pay the hash and store the entry for a document
//     you will never convert again.
//   - Nothing it does may be irreversible by accident. `cache clean` deletes
//     files, so it is guarded twice — CheckCacheDir rejects a directory too
//     close to the root, and DiskCache.Clean only ever removes its own entry
//     files from inside its own directory.

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/muthuishere/anymd"
)

// cacheOptions is the parsed cache portion of the command line. It is a
// separate struct so it can be embedded in main's config with one line.
type cacheOptions struct {
	enable  bool
	disable bool
	dir     string
}

// cacheFlagNames are the flags that only mean something once caching is on.
var cacheFlagNames = []string{"cache-dir"}

// registerCacheFlags adds --cache, --no-cache and --cache-dir to fs.
func registerCacheFlags(fs *flag.FlagSet, co *cacheOptions) {
	fs.BoolVar(&co.enable, "cache", false, "")
	fs.BoolVar(&co.disable, "no-cache", false, "")
	fs.StringVar(&co.dir, "cache-dir", "", "")
}

// cacheFlagUsage is the block to splice into the main usage text.
const cacheFlagUsage = `cache flags (off by default — anymd writes nothing to your disk unasked):
      --cache        reuse a previous conversion of identical bytes
      --no-cache     force a fresh conversion (wins over --cache)
      --cache-dir D  cache location (default: os.UserCacheDir()/anymd)

  The key covers the input bytes, the anymd version, the converter registry,
  the type hints and the output-affecting options — so upgrading anymd
  invalidates every entry by itself and you never need "cache clean" for
  correctness. The exception is a build with no VCS stamp (-buildvcs=false, a
  source tarball): its version is a constant, so while developing anymd itself
  use --no-cache or "anymd cache clean".
`

// resolveCache turns the flags into a Cache, or nil when caching is off.
//
// setFlags is main's record of which flags actually appeared, so --cache-dir
// with no --cache is an error rather than a silently ignored argument — the
// same rule the LLM flags follow, for the same reason.
func resolveCache(co *cacheOptions, setFlags map[string]bool, stderr io.Writer) (anymd.Cache, int) {
	on := co.enable && !co.disable
	if co.enable && co.disable {
		// Not a usage error: a wrapper script that always passes --cache plus a
		// user who typed --no-cache is a sensible thing to happen, and the safe
		// reading of "do and do not cache" is do not.
		fmt.Fprintln(stderr, "anymd: --no-cache overrides --cache")
	}
	if !on {
		for _, n := range cacheFlagNames {
			if setFlags[n] && !co.disable {
				fmt.Fprintf(stderr, "anymd: --%s requires --cache\n", n)
				return nil, exitUsage
			}
		}
		return nil, exitOK
	}

	dir, err := cacheDir(co.dir)
	if err != nil {
		fmt.Fprintf(stderr, "anymd: %v\n", err)
		return nil, exitFail
	}
	if err := anymd.CheckCacheDir(dir); err != nil {
		fmt.Fprintf(stderr, "anymd: %v\n", err)
		return nil, exitUsage
	}
	c, err := anymd.NewDiskCache(dir, 0)
	if err != nil {
		fmt.Fprintf(stderr, "anymd: %v\n", err)
		return nil, exitFail
	}
	return c, exitOK
}

// cacheDir resolves the effective cache directory.
func cacheDir(override string) (string, error) {
	if strings.TrimSpace(override) != "" {
		return filepath.Abs(override)
	}
	return anymd.DefaultCacheDir()
}

// ---------------------------------------------------------------------------
// `anymd cache`
// ---------------------------------------------------------------------------

const cacheUsage = `anymd cache — inspect and clear the conversion cache

usage: anymd cache <command> [--cache-dir DIR]

  path    print the cache directory
  stats   print entry count and total size
  clean   delete every cached entry

  Caching is off unless you pass --cache to a conversion. The cache is
  content-addressed and keyed on the anymd version, so an upgrade invalidates
  it on its own; clean is for reclaiming disk space, not for correctness.
`

// runCacheCommand implements `anymd cache …`, mirroring `anymd config …`:
// the flag is accepted on either side of the verb.
func runCacheCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("anymd cache", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var dir string
	fs.StringVar(&dir, "cache-dir", "", "")
	fs.Usage = func() { fmt.Fprint(stderr, cacheUsage) }

	var verb string
	rest := args
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		verb, rest = rest[0], rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if verb == "" {
		if fs.NArg() > 0 {
			verb = fs.Arg(0)
		} else {
			fmt.Fprint(stderr, cacheUsage)
			return exitUsage
		}
	}

	resolved, err := cacheDir(dir)
	if err != nil {
		fmt.Fprintf(stderr, "anymd: %v\n", err)
		return exitFail
	}

	switch verb {
	case "path":
		// Printing a path must never create it: `anymd cache path` is what a
		// script calls to decide whether a cache exists at all.
		fmt.Fprintln(stdout, resolved)
		return exitOK
	case "stats":
		return cacheStats(resolved, stdout)
	case "clean":
		return cacheClean(resolved, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "anymd: unknown cache command %q\n\n", verb)
		fmt.Fprint(stderr, cacheUsage)
		return exitUsage
	}
}

func cacheStats(dir string, stdout io.Writer) int {
	fmt.Fprintf(stdout, "cache dir:   %s\n", dir)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		fmt.Fprintf(stdout, "entries:     0 (directory does not exist)\n")
		fmt.Fprintf(stdout, "size:        0 B\n")
		return exitOK
	}
	// Read-only: NewDiskCache would create the directory, and we already know
	// it is there.
	c, err := anymd.NewDiskCache(dir, 0)
	if err != nil {
		fmt.Fprintf(stdout, "entries:     unknown (%v)\n", err)
		return exitOK
	}
	s := c.Stats()
	fmt.Fprintf(stdout, "entries:     %d\n", s.Entries)
	fmt.Fprintf(stdout, "size:        %s\n", humanBytes(s.Bytes))
	fmt.Fprintf(stdout, "budget:      %s\n", humanBytes(c.MaxBytes()))
	fmt.Fprintf(stdout, "version key: %s\n", anymd.Version())
	// Hit rate is per-process: counting it across runs would mean a write on
	// every lookup, which is the one thing a read path must not add.
	fmt.Fprintf(stdout, "hit rate:    n/a (per-process; this run made no lookups)\n")
	return exitOK
}

func cacheClean(dir string, stdout, stderr io.Writer) int {
	if err := anymd.CheckCacheDir(dir); err != nil {
		fmt.Fprintf(stderr, "anymd: %v\n", err)
		return exitUsage
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		fmt.Fprintf(stdout, "nothing to clean: %s does not exist\n", dir)
		return exitOK
	}
	c, err := anymd.NewDiskCache(dir, 0)
	if err != nil {
		fmt.Fprintf(stderr, "anymd: %v\n", err)
		return exitFail
	}
	before := c.Stats()
	n, err := c.Clean()
	if err != nil {
		fmt.Fprintf(stderr, "anymd: %v\n", err)
		return exitFail
	}
	fmt.Fprintf(stdout, "removed %d entries (%s) from %s\n", n, humanBytes(before.Bytes), dir)
	return exitOK
}

// humanBytes formats a byte count for a human, in powers of 1024.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
