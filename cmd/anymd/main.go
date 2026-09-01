// Command anymd converts any document to Markdown.
//
//	anymd report.docx > report.md
//	cat page.html | anymd -t html
//	anymd -r docs/ -d out/
//
// Markdown goes to stdout; progress, warnings and errors go to stderr, so the
// tool composes in a pipeline. Exit codes: 0 all converted, 1 one or more
// failed, 2 usage error.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/muthuishere/anymd"
)

// Build stamps. The Makefile sets these with -ldflags -X main.version=...
var (
	version = "dev"
	commit  = ""
)

// Exit codes. Distinct codes let CI scripts branch on "conversion failed"
// versus "you called me wrong".
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

// fetchTimeout bounds the only network access in the whole project. Converters
// never touch the network; the CLI fetches a URL argument and hands the bytes
// down as an ordinary stream.
const fetchTimeout = 30 * time.Second

// userAgent identifies us politely to the hosts we fetch from.
var userAgent = "anymd/" + version + " (+https://github.com/muthuishere/anymd)"

// stdin is indirected so tests can feed the stdin path without a real pipe.
var stdin io.Reader = os.Stdin

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// config is the parsed command line.
type config struct {
	out       string
	outdir    string
	ext       string
	typeHint  string
	charset   string
	maxDepth  int
	keepURIs  bool
	recursive bool
	quiet     bool
	failFast  bool
	title     bool
	insecure  bool
	list      bool
	showVer   bool
	args      []string
}

func newFlagSet(cfg *config, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("anymd", flag.ContinueOnError)
	fs.SetOutput(stderr)

	strVar := func(p *string, def string, names ...string) {
		for _, n := range names {
			fs.StringVar(p, n, def, "")
		}
	}
	boolVar := func(p *bool, names ...string) {
		for _, n := range names {
			fs.BoolVar(p, n, false, "")
		}
	}

	strVar(&cfg.out, "", "o", "output")
	strVar(&cfg.outdir, "", "d", "outdir")
	strVar(&cfg.ext, ".md", "ext")
	strVar(&cfg.typeHint, "", "t", "type")
	strVar(&cfg.charset, "", "charset")
	fs.IntVar(&cfg.maxDepth, "max-depth", 0, "")
	boolVar(&cfg.keepURIs, "keep-data-uris")
	boolVar(&cfg.recursive, "r", "recursive")
	boolVar(&cfg.quiet, "q", "quiet")
	boolVar(&cfg.failFast, "fail-fast")
	boolVar(&cfg.title, "title")
	boolVar(&cfg.insecure, "insecure")
	boolVar(&cfg.list, "list")
	boolVar(&cfg.showVer, "version")

	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	return fs
}

const usage = `anymd — convert any document to Markdown (pure Go, no cgo)

usage: anymd [flags] [file|url ...]

  With no arguments (or "-") anymd reads stdin and writes Markdown to stdout.
  With a single input and no -o/-d it writes to stdout. Progress, warnings and
  errors always go to stderr.

flags:
  -o FILE|DIR        write to FILE; if DIR exists as a directory, batch into it
  -d, --outdir DIR   batch output directory (created if missing)
      --ext .md      output extension in batch mode (default ".md")
  -t, --type EXT     force an extension hint, e.g. -t docx (useful with stdin)
  -r, --recursive    walk directories given as inputs
      --charset NAME override the detected text encoding
      --max-depth N  bound container recursion (zip in a zip); default 8
      --keep-data-uris  keep base64 images inline as data: URIs
      --title        prepend "# Title" when the converter found one
  -q, --quiet        suppress per-file progress lines on stderr
      --fail-fast    stop at the first error (default: continue, report at end)
      --insecure     skip TLS verification when fetching a URL (self-signed
                     internal hosts only)
      --list         print the registered converters in dispatch order
      --version      print version, commit and Go version

exit codes: 0 all converted · 1 one or more failed · 2 usage error
`

// run is the whole program; main is a wrapper so tests can drive it directly.
func run(args []string, stdout, stderr io.Writer) int {
	cfg := &config{}
	fs := newFlagSet(cfg, stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	cfg.args = fs.Args()

	if cfg.showVer {
		fmt.Fprintf(stdout, "anymd %s\ncommit: %s\ngo: %s\n", version, resolveCommit(), runtime.Version())
		return exitOK
	}

	engine := anymd.New()

	if cfg.list {
		for _, n := range engine.Converters() {
			fmt.Fprintln(stdout, n)
		}
		return exitOK
	}

	if cfg.ext == "" {
		cfg.ext = ".md"
	}
	if !strings.HasPrefix(cfg.ext, ".") {
		cfg.ext = "." + cfg.ext
	}

	opts := &anymd.Options{
		MaxDepth:     cfg.maxDepth,
		KeepDataURIs: cfg.keepURIs,
		Charset:      cfg.charset,
	}

	// stdin mode: no inputs at all, or the single conventional "-".
	if len(cfg.args) == 0 || (len(cfg.args) == 1 && cfg.args[0] == "-") {
		if cfg.out != "" && cfg.outdir != "" {
			fmt.Fprintln(stderr, "anymd: -o and -d are mutually exclusive")
			return exitUsage
		}
		return runStdin(engine, cfg, opts, stdout, stderr)
	}

	inputs, err := expandInputs(cfg.args, cfg.recursive)
	if err != nil {
		fmt.Fprintf(stderr, "anymd: %v\n", err)
		return exitFail
	}
	if len(inputs) == 0 {
		fmt.Fprintln(stderr, "anymd: no input files")
		return exitFail
	}

	dests, code := planDestinations(inputs, cfg, stderr)
	if code != exitOK {
		return code
	}

	return convertAll(engine, inputs, dests, cfg, opts, stdout, stderr)
}

func resolveCommit() string {
	if commit != "" {
		return commit
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return "unknown"
}

// runStdin converts standard input. There is no filename, so we pass whatever
// hints we have (-t/--charset) and let the engine sniff the rest.
func runStdin(engine *anymd.Engine, cfg *config, opts *anymd.Options, stdout, stderr io.Writer) int {
	buf, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "anymd: stdin: %v\n", err)
		return exitFail
	}
	info := anymd.StreamInfo{Charset: cfg.charset}
	applyTypeHint(&info, cfg.typeHint)

	res, err := engine.ConvertBytes(buf, info, opts)
	if err != nil {
		fmt.Fprintf(stderr, "anymd: stdin: %v\n", err)
		return exitFail
	}
	body := renderBody(res, cfg.title)

	dst := cfg.out
	if dst == "" && cfg.outdir != "" {
		if err := os.MkdirAll(cfg.outdir, 0o755); err != nil {
			fmt.Fprintf(stderr, "anymd: %v\n", err)
			return exitFail
		}
		dst = filepath.Join(cfg.outdir, "stdin"+cfg.ext)
	}
	if dst == "" {
		io.WriteString(stdout, body)
		return exitOK
	}
	if err := writeFile(dst, body); err != nil {
		fmt.Fprintf(stderr, "anymd: %v\n", err)
		return exitFail
	}
	if !cfg.quiet {
		fmt.Fprintf(stderr, "converted stdin -> %s\n", dst)
	}
	return exitOK
}

// applyTypeHint turns a -t value ("docx", ".docx") into extension + mime hints.
func applyTypeHint(info *anymd.StreamInfo, hint string) {
	hint = strings.TrimSpace(strings.ToLower(hint))
	if hint == "" {
		return
	}
	if !strings.HasPrefix(hint, ".") {
		hint = "." + hint
	}
	info.Extension = hint
	if m := mime.TypeByExtension(hint); m != "" {
		info.MimeType = m
	}
}

// isURL reports whether an argument should be fetched rather than opened.
func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// expandInputs resolves directory arguments (walking them under -r) into a
// flat, deterministic list of inputs. URLs pass through untouched.
func expandInputs(args []string, recursive bool) ([]string, error) {
	var out []string
	for _, a := range args {
		if isURL(a) {
			out = append(out, a)
			continue
		}
		st, err := os.Stat(a)
		if err != nil {
			return nil, err
		}
		if !st.IsDir() {
			out = append(out, a)
			continue
		}
		if !recursive {
			return nil, fmt.Errorf("%s is a directory (use -r to walk it)", a)
		}
		var found []string
		err = filepath.WalkDir(a, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			found = append(found, p)
			return nil
		})
		if err != nil {
			return nil, err
		}
		sort.Strings(found)
		out = append(out, found...)
	}
	return out, nil
}

// planDestinations decides, per input, where its markdown goes. An empty
// destination means stdout.
func planDestinations(inputs []string, cfg *config, stderr io.Writer) ([]string, int) {
	if cfg.out != "" && cfg.outdir != "" {
		fmt.Fprintln(stderr, "anymd: -o and -d are mutually exclusive")
		return nil, exitUsage
	}

	dir := cfg.outdir
	if dir == "" && cfg.out != "" {
		// -o naming an existing directory means "batch into it".
		if st, err := os.Stat(cfg.out); err == nil && st.IsDir() {
			dir = cfg.out
		} else {
			if len(inputs) > 1 {
				fmt.Fprintln(stderr, "anymd: -o FILE with multiple inputs; use -d DIR (or point -o at an existing directory)")
				return nil, exitUsage
			}
			return []string{cfg.out}, exitOK
		}
	}

	if dir == "" {
		// No output target: everything to stdout, in input order.
		return make([]string, len(inputs)), exitOK
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(stderr, "anymd: %v\n", err)
		return nil, exitFail
	}
	dests := make([]string, len(inputs))
	used := make(map[string]int, len(inputs))
	for i, in := range inputs {
		base := outputBase(in)
		name := base + cfg.ext
		// Distinct inputs can share a basename (a/report.txt, b/report.txt).
		// Suffix collisions rather than silently clobbering.
		if n, seen := used[name]; seen {
			used[name] = n + 1
			name = fmt.Sprintf("%s-%d%s", base, n, cfg.ext)
		} else {
			used[name] = 1
		}
		dests[i] = filepath.Join(dir, name)
	}
	return dests, exitOK
}

// outputBase derives the extension-less output name for an input path or URL.
func outputBase(in string) string {
	if isURL(in) {
		if u, err := url.Parse(in); err == nil {
			b := path.Base(u.Path)
			if b != "" && b != "." && b != "/" {
				return strings.TrimSuffix(b, path.Ext(b))
			}
			if u.Host != "" {
				return sanitize(u.Host)
			}
		}
		return "download"
	}
	b := filepath.Base(in)
	return strings.TrimSuffix(b, filepath.Ext(b))
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, s)
}

// outcome is one input's result, carried back from a worker.
type outcome struct {
	body    string
	err     error
	skipped bool
}

// convertAll converts every input with a worker pool, then emits the results in
// INPUT ORDER. Determinism beats raw speed: stdout must be diffable.
func convertAll(engine *anymd.Engine, inputs, dests []string, cfg *config, opts *anymd.Options, stdout, stderr io.Writer) int {
	results := make([]outcome, len(inputs))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	workers := runtime.NumCPU()
	if workers > len(inputs) {
		workers = len(inputs)
	}
	if workers < 1 {
		workers = 1
	}

	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				select {
				case <-ctx.Done():
					results[i] = outcome{skipped: true}
					continue
				default:
				}
				body, err := convertOne(engine, inputs[i], cfg, opts)
				results[i] = outcome{body: body, err: err}
				if err != nil && cfg.failFast {
					cancel()
				}
			}
		}()
	}
	for i := range inputs {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	converted, failed := 0, 0
	for i, in := range inputs {
		res := results[i]
		if res.skipped {
			continue
		}
		if res.err != nil {
			failed++
			fmt.Fprintf(stderr, "anymd: %s: %v\n", in, res.err)
			if cfg.failFast {
				break
			}
			continue
		}
		if dests[i] == "" {
			if _, err := io.WriteString(stdout, res.body); err != nil {
				failed++
				fmt.Fprintf(stderr, "anymd: %s: %v\n", in, err)
				continue
			}
		} else if err := writeFile(dests[i], res.body); err != nil {
			failed++
			fmt.Fprintf(stderr, "anymd: %s: %v\n", in, err)
			continue
		}
		converted++
		if !cfg.quiet && dests[i] != "" {
			fmt.Fprintf(stderr, "converted %s -> %s\n", in, dests[i])
		}
	}

	if len(inputs) > 1 && !cfg.quiet {
		fmt.Fprintf(stderr, "converted %d, failed %d\n", converted, failed)
	}
	if failed > 0 {
		return exitFail
	}
	return exitOK
}

// convertOne converts a single input (path or URL) to a markdown body.
func convertOne(engine *anymd.Engine, in string, cfg *config, opts *anymd.Options) (string, error) {
	var (
		res anymd.Result
		err error
	)
	if isURL(in) {
		var body []byte
		var info anymd.StreamInfo
		body, info, err = fetch(in, cfg.insecure)
		if err != nil {
			return "", err
		}
		if cfg.charset != "" {
			info.Charset = cfg.charset
		}
		applyTypeHint(&info, cfg.typeHint)
		res, err = engine.ConvertBytes(body, info, opts)
	} else {
		var f *os.File
		f, err = os.Open(in)
		if err != nil {
			return "", err
		}
		defer f.Close()
		info := anymd.StreamInfoForFile(in)
		if cfg.charset != "" {
			info.Charset = cfg.charset
		}
		applyTypeHint(&info, cfg.typeHint)
		res, err = engine.ConvertStream(f, info, opts)
	}
	if err != nil {
		return "", err
	}
	return renderBody(res, cfg.title), nil
}

// renderBody applies --title: prepend "# Title" when the converter found one
// and the body does not already open with an h1.
func renderBody(res anymd.Result, withTitle bool) string {
	body := res.Markdown
	if !withTitle || strings.TrimSpace(res.Title) == "" {
		return body
	}
	if strings.HasPrefix(strings.TrimLeft(body, " \t\n"), "# ") {
		return body
	}
	head := "# " + strings.TrimSpace(res.Title) + "\n"
	if strings.TrimSpace(body) == "" {
		return head
	}
	return head + "\n" + strings.TrimLeft(body, "\n")
}

func writeFile(path, body string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

// fetch retrieves a URL. This function is the ONLY network access in the whole
// project — converters are offline by construction, so what they see is always
// a plain byte stream.
func fetch(rawurl string, insecure bool) ([]byte, anymd.StreamInfo, error) {
	client := &http.Client{Timeout: fetchTimeout}
	if insecure {
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // opt-in via --insecure
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, anymd.StreamInfo{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")

	resp, err := client.Do(req)
	if err != nil {
		return nil, anymd.StreamInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, anymd.StreamInfo{}, fmt.Errorf("http %s", resp.Status)
	}

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, resp.Body); err != nil {
		return nil, anymd.StreamInfo{}, err
	}

	final := resp.Request.URL.String()
	info := anymd.StreamInfo{URL: final, FileName: path.Base(resp.Request.URL.Path)}
	ct := resp.Header.Get("Content-Type")
	if ct != "" {
		if mt, params, err := mime.ParseMediaType(ct); err == nil {
			info.MimeType = mt
			info.Charset = params["charset"]
			if exts, err := mime.ExtensionsByType(mt); err == nil && len(exts) > 0 {
				sort.Strings(exts)
				info.Extension = exts[0]
			}
		} else {
			info.MimeType = ct
		}
	}
	// The final URL's own extension is the stronger hint when it has one.
	if e := strings.ToLower(path.Ext(resp.Request.URL.Path)); e != "" {
		info.Extension = e
		if info.MimeType == "" {
			info.MimeType = mime.TypeByExtension(e)
		}
	}
	return buf.Bytes(), info, nil
}
