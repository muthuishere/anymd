package main

// Crawling a site into a directory of Markdown, on the command line.
//
// Three rules shape this file:
//
//   - It is OFF by default, and it is the only mode in which anymd follows a
//     link on its own. Fetching one URL you typed is a different act from
//     walking someone's site, so it takes a different, explicit flag.
//   - It writes many files, so it demands -d. A mirror on stdout is not a
//     mirror, and there would be no way to name the files it produced.
//   - Nothing it writes may land outside -d. crawl.LocalPath sanitises the
//     path, and safeJoin checks the result again before any file is created:
//     these are paths derived from remote input, and one layer of defence
//     against "../../.ssh/authorized_keys" is not enough.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/muthuishere/anymd"
	"github.com/muthuishere/anymd/crawl"
)

// crawlFn and localPathFn indirect the crawl package so the CLI layer can be
// tested without a crawler — the same trick main.go already plays with stdin.
var (
	crawlFn      = crawl.Crawl
	localPathFn  = crawl.LocalPath
	crawlTimeout = 30 * time.Minute
)

// crawlOptions is the parsed crawl portion of the command line.
type crawlOptions struct {
	enable       bool
	depth        int
	maxPages     int
	delay        time.Duration
	sameHost     bool
	ignoreRobots bool
	sitemap      string
	include      stringList
	exclude      stringList
}

// crawlFlagNames are the flags that only mean something with --crawl. Silently
// ignoring one of these is how somebody spends an afternoon wondering why
// --depth 3 fetched a single page.
var crawlFlagNames = []string{"depth", "max-pages", "crawl-delay", "same-host", "include", "exclude", "ignore-robots", "sitemap"}

// stringList collects a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// registerCrawlFlags adds the crawl flags to fs.
func registerCrawlFlags(fs *flag.FlagSet, co *crawlOptions) {
	fs.BoolVar(&co.enable, "crawl", false, "")
	// -1 is "unset": crawl.Options treats a negative MaxDepth as "use the
	// default" precisely because 0 already means something ("the seed only").
	fs.IntVar(&co.depth, "depth", -1, "")
	fs.IntVar(&co.maxPages, "max-pages", 0, "")
	fs.DurationVar(&co.delay, "crawl-delay", 0, "")
	fs.BoolVar(&co.sameHost, "same-host", true, "")
	fs.BoolVar(&co.ignoreRobots, "ignore-robots", false, "")
	fs.Var(&co.include, "include", "")
	fs.Var(&co.exclude, "exclude", "")
	fs.StringVar(&co.sitemap, "sitemap", "auto", "")
}

// parseSitemapMode maps the --sitemap value onto crawl.SitemapMode.
//
// The default is "auto" rather than a bare empty string so that `--sitemap ""`
// is an error a user can understand instead of silently meaning the default.
func parseSitemapMode(v string) (crawl.SitemapMode, error) {
	switch v {
	case "auto":
		return crawl.SitemapAuto, nil
	case "only":
		return crawl.SitemapOnly, nil
	case "off":
		return crawl.SitemapOff, nil
	default:
		return 0, fmt.Errorf("--sitemap %q: want auto, only or off", v)
	}
}

// crawlFlagUsage is the block spliced into the main usage text. It is spliced
// there and not merely declared here — a flag nobody can discover from --help
// may as well not exist.
const crawlFlagUsage = `crawl flags (off by default — without --crawl a URL argument is fetched once
and nothing is followed):
      --crawl        follow links from each URL argument; requires -d DIR
      --depth N      how many links deep to follow (default 1; --depth 0 is
                     the seed page only)
      --max-pages N  cap the total number of pages (default 200)
      --crawl-delay D  wait between requests to one host, e.g. 1s (default
                     500ms). A fast crawler is a denial of service, so there
                     is deliberately no way to ask for none.
      --same-host=false  allow the crawl to leave the seed's host (default:
                     stay on it — escaping onto the open web is a choice)
      --include RE   only crawl URLs matching this regexp (repeatable)
      --exclude RE   never crawl URLs matching this regexp (repeatable,
                     and it wins over --include)
      --sitemap M    auto (default) | only | off — auto seeds the crawl from a
                     sitemap when one exists AND still follows links; only
                     skips link following; off never looks for one
      --ignore-robots  do not read robots.txt (say why to yourself first)

  Each page is converted, its output path derived from its URL, and its links
  to other crawled pages rewritten to relative local paths — so the directory
  browses offline. A link to a page that was NOT crawled stays absolute, so it
  still works.
`

// applyCrawl validates the crawl portion of the command line.
//
// Every failure here is exit 2, not exit 1: none of them is "the work failed",
// all of them are "that command line does not mean anything".
func applyCrawl(cfg *config, stderr io.Writer) int {
	if !cfg.crawl.enable {
		for _, n := range crawlFlagNames {
			if cfg.setFlags[n] {
				fmt.Fprintf(stderr, "anymd: --%s requires --crawl\n", n)
				return exitUsage
			}
		}
		return exitOK
	}

	if cfg.out != "" {
		fmt.Fprintln(stderr, "anymd: --crawl writes many files; use -d DIR, not -o")
		return exitUsage
	}
	if cfg.outdir == "" {
		fmt.Fprintln(stderr, "anymd: --crawl writes one file per page, so it needs an output directory: -d DIR")
		return exitUsage
	}
	if len(cfg.args) == 0 {
		fmt.Fprintln(stderr, "anymd: --crawl needs at least one URL to start from")
		return exitUsage
	}
	for _, a := range cfg.args {
		if !isURL(a) {
			fmt.Fprintf(stderr, "anymd: --crawl takes http(s) URLs, not %s\n", a)
			return exitUsage
		}
	}
	if cfg.setFlags["depth"] && cfg.crawl.depth < 0 {
		fmt.Fprintln(stderr, "anymd: --depth cannot be negative (0 means the seed only)")
		return exitUsage
	}
	if cfg.crawl.maxPages < 0 {
		fmt.Fprintln(stderr, "anymd: --max-pages cannot be negative")
		return exitUsage
	}
	if cfg.crawl.delay < 0 {
		fmt.Fprintln(stderr, "anymd: --crawl-delay cannot be negative")
		return exitUsage
	}
	// Compile the patterns here so a typo is a usage error before a single
	// request goes out, rather than an error halfway through a site.
	for _, set := range [][]string{cfg.crawl.include, cfg.crawl.exclude} {
		for _, re := range set {
			if _, err := regexp.Compile(re); err != nil {
				fmt.Fprintf(stderr, "anymd: bad pattern %q: %v\n", re, err)
				return exitUsage
			}
		}
	}
	return exitOK
}

// crawledPage is one page held between the two passes.
//
// The write has to wait for the crawl to finish: page 1 may link to page 40,
// and until page 40 has an output path there is nothing to rewrite that link
// to. So pass one converts and records, pass two rewrites and writes.
type crawledPage struct {
	url  string
	rel  string // slash-separated, relative to the output root
	body string
	err  error
}

// runCrawl crawls every URL argument and mirrors it under cfg.outdir.
//
// Nothing here writes to stdout. A crawl's output is files; its narration is
// progress, and progress belongs on stderr where it cannot corrupt a pipe.
func runCrawl(engine *anymd.Engine, cfg *config, opts *anymd.Options, stderr io.Writer) int {
	root, err := filepath.Abs(cfg.outdir)
	if err != nil {
		fmt.Fprintf(stderr, "anymd: %v\n", err)
		return exitFail
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		fmt.Fprintf(stderr, "anymd: %v\n", err)
		return exitFail
	}

	sitemapMode, err := parseSitemapMode(cfg.crawl.sitemap)
	if err != nil {
		fmt.Fprintf(stderr, "anymd: %v\n", err)
		return exitUsage
	}

	copts := crawl.Options{
		Sitemap:      sitemapMode,
		MaxDepth:     cfg.crawl.depth,
		MaxPages:     cfg.crawl.maxPages,
		Delay:        cfg.crawl.delay,
		UserAgent:    userAgent,
		IgnoreRobots: cfg.crawl.ignoreRobots,
		Include:      cfg.crawl.include,
		Exclude:      cfg.crawl.exclude,
		Insecure:     cfg.insecure,
	}
	if cfg.setFlags["same-host"] {
		// crawl.Options.SameHost is a *bool so that "unset" is representable
		// and defaults to the safe answer. Only an explicit --same-host on the
		// command line gets to overrule that.
		copts.SameHost = crawl.Ptr(cfg.crawl.sameHost)
	}

	var (
		pages     []*crawledPage
		mapping   = make(map[string]string) // url -> output path, for RewriteLinks
		taken     = make(map[string]string) // output path -> url, collision guard
		seen      = make(map[string]bool)
		fetched   int
		skipped   int
		failed    int
		truncated bool
	)

	visit := func(p crawl.Page) error {
		if seen[p.URL] {
			return nil
		}
		seen[p.URL] = true
		fetched++
		if !cfg.quiet {
			fmt.Fprintf(stderr, "crawled %s %s\n", progressCount(fetched, cfg.crawl.maxPages), p.URL)
		}

		cp := &crawledPage{url: p.URL}
		pages = append(pages, cp)

		rel, err := localPathFn(p.URL, cfg.ext)
		if err != nil {
			cp.err = fmt.Errorf("output path: %w", err)
		} else {
			rel = uniquePath(taken, filepath.ToSlash(rel), p.URL, cfg.ext)
			res, err := engine.ConvertBytes(p.Body, pageStreamInfo(p, cfg), opts)
			if err != nil {
				cp.err = err
			} else {
				cp.rel, cp.body = rel, renderBody(res, cfg.title)
				taken[rel] = p.URL
				mapping[p.URL] = rel
			}
		}

		if cp.err != nil {
			// One bad page must not cost us the other thirty-nine. It is
			// reported, left out of the mapping (so links to it stay absolute
			// and still work), and the crawl goes on.
			failed++
			fmt.Fprintf(stderr, "anymd: %s: %v\n", p.URL, cp.err)
			if cfg.failFast {
				return cp.err
			}
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), crawlTimeout)
	defer cancel()

	for _, seed := range cfg.args {
		res, err := crawlFn(ctx, seed, copts, visit)
		skipped += res.Skipped
		truncated = truncated || res.Truncated
		for u, e := range res.Errors {
			failed++
			fmt.Fprintf(stderr, "anymd: %s: %v\n", u, e)
		}
		if err != nil {
			failed++
			fmt.Fprintf(stderr, "anymd: %s: %v\n", seed, err)
			if cfg.failFast {
				break
			}
		}
	}

	written := 0
	for _, cp := range pages {
		if cp.err != nil {
			continue
		}
		dst, err := safeJoin(root, cp.rel)
		if err != nil {
			failed++
			fmt.Fprintf(stderr, "anymd: %s: %v\n", cp.url, err)
			continue
		}
		if err := writeFile(dst, anymd.RewriteLinks(cp.body, cp.url, mapping)); err != nil {
			failed++
			fmt.Fprintf(stderr, "anymd: %s: %v\n", cp.url, err)
			continue
		}
		written++
	}

	if !cfg.quiet {
		line := fmt.Sprintf("crawl: fetched %d, written %d, skipped %d, failed %d", fetched, written, skipped, failed)
		if truncated {
			line += " (truncated: a --depth or --max-pages cap stopped the crawl early)"
		}
		fmt.Fprintln(stderr, line)
	}
	if failed > 0 {
		return exitFail
	}
	return exitOK
}

// progressCount renders "12/40" when there is a cap to count towards, and just
// "12" when there is not. Inventing a denominator would be a lie: a crawler
// does not know how big a site is until it has finished walking it.
func progressCount(n, max int) string {
	if max > 0 {
		return fmt.Sprintf("%d/%d", n, max)
	}
	return fmt.Sprintf("%d", n)
}

// uniquePath keeps two distinct URLs from landing on one file. LocalPath
// promises not to collide; this is the belt to that pair of braces, because a
// collision here silently loses a page.
func uniquePath(taken map[string]string, rel, u, ext string) string {
	if owner, ok := taken[rel]; !ok || owner == u {
		return rel
	}
	base := strings.TrimSuffix(rel, ext)
	for n := 2; ; n++ {
		cand := fmt.Sprintf("%s-%d%s", base, n, ext)
		if owner, ok := taken[cand]; !ok || owner == u {
			return cand
		}
	}
}

// safeJoin resolves rel under root and refuses anything that escapes it.
//
// This is deliberately redundant with crawl.LocalPath's own sanitising. The
// path comes from a remote server's URLs; two independent checks is the
// correct amount of paranoia for code that creates files from them.
func safeJoin(root, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("empty output path")
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) {
		return "", fmt.Errorf("refusing an absolute output path %q", rel)
	}
	if vol := filepath.VolumeName(filepath.FromSlash(rel)); vol != "" {
		return "", fmt.Errorf("refusing an absolute output path %q", rel)
	}
	full := filepath.Clean(filepath.Join(root, filepath.FromSlash(rel)))
	prefix := filepath.Clean(root) + string(os.PathSeparator)
	if !strings.HasPrefix(full, prefix) {
		return "", fmt.Errorf("refusing to write %q outside %s", rel, root)
	}
	return full, nil
}

// pageStreamInfo gives the engine everything it knows about a fetched page.
// The URL's own extension beats the Content-Type, which is routinely wrong.
func pageStreamInfo(p crawl.Page, cfg *config) anymd.StreamInfo {
	info := anymd.StreamInfo{URL: p.URL}
	if u, err := url.Parse(p.URL); err == nil {
		if b := path.Base(u.Path); b != "" && b != "." && b != "/" {
			info.FileName = b
		}
		if e := strings.ToLower(path.Ext(u.Path)); e != "" {
			info.Extension = e
		}
	}
	if p.ContentType != "" {
		if mt, params, err := mime.ParseMediaType(p.ContentType); err == nil {
			info.MimeType = mt
			info.Charset = params["charset"]
			if info.Extension == "" {
				if exts, err := mime.ExtensionsByType(mt); err == nil && len(exts) > 0 {
					sort.Strings(exts)
					info.Extension = exts[0]
				}
			}
		} else {
			info.MimeType = p.ContentType
		}
	}
	if info.Extension == "" && info.MimeType == "" {
		// A crawler follows hyperlinks, so HTML is the overwhelmingly likely
		// answer when the server told us nothing.
		info.Extension = ".html"
	}
	if cfg.charset != "" {
		info.Charset = cfg.charset
	}
	applyTypeHint(&info, cfg.typeHint)
	return info
}
