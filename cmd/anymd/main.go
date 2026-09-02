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
	"encoding/json"
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
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/muthuishere/anymd"
	"github.com/muthuishere/anymd/llm"
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
	cache     cacheOptions
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

	// LLM options. Everything here is off unless --llm is given: without it
	// anymd makes no network calls during conversion at all, which is the
	// guarantee that lets you point it at untrusted input.
	llm           bool
	llmConfig     string
	llmModel      string
	llmBaseURL    string
	llmTimeout    time.Duration
	llmTranscribe bool
	llmTransModel string

	// setFlags records which flags actually appeared on the command line, so
	// precedence (explicit flag > config file > environment > default) can be
	// applied and so an LLM flag without --llm is a usage error rather than a
	// silently ignored one.
	setFlags map[string]bool

	args []string
}

func newFlagSet(cfg *config, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("anymd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	registerCacheFlags(fs, &cfg.cache)

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

	boolVar(&cfg.llm, "llm")
	strVar(&cfg.llmConfig, "", "llm-config")
	strVar(&cfg.llmModel, "", "llm-model")
	strVar(&cfg.llmBaseURL, "", "llm-base-url")
	fs.DurationVar(&cfg.llmTimeout, "llm-timeout", 0, "")
	boolVar(&cfg.llmTranscribe, "llm-transcribe")
	strVar(&cfg.llmTransModel, "", "llm-transcribe-model")

	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	return fs
}

const usage = `anymd — convert any document to Markdown (pure Go, no cgo)

usage: anymd [flags] [file|url ...]
       anymd config <path|show|init> [--llm-config FILE]
       anymd cache  <path|stats|clean> [--cache-dir DIR]

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

llm flags (all off by default — WITHOUT --llm anymd makes no network calls
during conversion at all, which is what makes it safe on untrusted input):
      --llm          enable LLM image captioning (one model call per image)
      --llm-config F config file; default ~/.config/anymd/anymdconfig.json
      --llm-model M  override the model from the config file
      --llm-base-url U  override the endpoint (a local Ollama or vLLM works)
      --llm-timeout D   bound one model call, e.g. 30s (default 60s)
      --llm-transcribe  also transcribe audio (one call per file, costs money)
      --llm-transcribe-model M  speech model (default whisper-1); this is a
                     different endpoint from --llm-model, hence its own flag

  Precedence: explicit flag > config file > environment > default.
  The API key is read from the environment (OPENROUTER_API_KEY, OPENAI_API_KEY
  or ANTHROPIC_API_KEY), or from the config file via ${VAR} interpolation.
  anymd never prints a key, not even masked.

` + "\n" + cacheFlagUsage + `
config subcommand:
  anymd config path   print the config file path
  anymd config show   print the resolved config with every secret redacted
  anymd config init   write a starter config (mode 0600), never overwriting

cache subcommand:
  anymd cache path    print the cache directory
  anymd cache stats   entry count and size on disk
  anymd cache clean   remove cached entries (refuses paths outside the cache dir)

exit codes: 0 all converted · 1 one or more failed · 2 usage error
`

// run is the whole program; main is a wrapper so tests can drive it directly.
func run(args []string, stdout, stderr io.Writer) int {
	// "config" is a subcommand, not an input file. It is checked before flag
	// parsing because Go's flag package stops at the first positional argument
	// anyway, so `anymd config show` would never reach the parser as flags.
	if len(args) > 0 && args[0] == "config" {
		return runConfig(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "cache" {
		return runCacheCommand(args[1:], stdout, stderr)
	}

	cfg := &config{}
	fs := newFlagSet(cfg, stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	cfg.args = fs.Args()
	cfg.setFlags = make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { cfg.setFlags[f.Name] = true })

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

	if code := applyLLM(cfg, opts, stderr); code != exitOK {
		return code
	}

	var code int
	if opts.Cache, code = resolveCache(&cfg.cache, cfg.setFlags, stderr); code != exitOK {
		return code
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
		// A short write to stdout is real (a closed pipe, a full disk) and must
		// not be reported as success — `anymd big.pdf | head` should not exit 0
		// while having lost the document.
		if _, err := io.WriteString(stdout, body); err != nil {
			fmt.Fprintf(stderr, "anymd: writing to stdout: %v\n", err)
			return exitFail
		}
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

// ---------------------------------------------------------------------------
// LLM wiring
//
// Everything below is inert unless --llm is given. That is the point: the
// default build converts documents without a single outbound packet, and
// turning that off has to be an explicit, visible act.
// ---------------------------------------------------------------------------

// llmFlagNames are the flags that only mean something with --llm.
var llmFlagNames = []string{"llm-config", "llm-model", "llm-base-url", "llm-timeout", "llm-transcribe", "llm-transcribe-model"}

// keyEnvVars are the environment variables a key is read from, in the order
// toolnexus consults them. Named here so an error message can name them; the
// VALUES are never read into a message, printed, or logged.
var keyEnvVars = []string{"OPENROUTER_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY"}

// applyLLM resolves the LLM configuration and attaches a Describer (and, once
// it exists, a Transcriber) to opts.
//
// Precedence is explicit flag > config file > environment > default. The config
// file's ${VAR} interpolation is what pulls the environment in, so the file is
// the layer that decides WHICH environment variable matters; a flag overrides
// whatever it resolved to.
func applyLLM(cfg *config, opts *anymd.Options, stderr io.Writer) int {
	if !cfg.llm {
		// Silently ignoring a flag the user typed is how an afternoon gets
		// lost — especially --llm-model, whose absence looks like the model
		// simply performing badly.
		for _, n := range llmFlagNames {
			if cfg.setFlags[n] {
				fmt.Fprintf(stderr, "anymd: --%s requires --llm\n", n)
				return exitUsage
			}
		}
		return exitOK
	}

	lc, err := llm.Load(cfg.llmConfig)
	if err != nil {
		// llm.Load's errors name the field and the environment VARIABLE, never
		// a value, so this is safe to print verbatim.
		fmt.Fprintf(stderr, "anymd: %v\n", err)
		return exitUsage
	}

	// Flags override the file.
	if cfg.setFlags["llm-model"] {
		lc.Model = cfg.llmModel
	}
	if cfg.setFlags["llm-base-url"] {
		lc.BaseURL = cfg.llmBaseURL
	}
	if cfg.setFlags["llm-timeout"] {
		if cfg.llmTimeout <= 0 {
			fmt.Fprintln(stderr, "anymd: --llm-timeout must be positive, e.g. 30s")
			return exitUsage
		}
		lc.TimeoutMs = int(cfg.llmTimeout / time.Millisecond)
		opts.LLMTimeout = cfg.llmTimeout
	} else if lc.TimeoutMs > 0 {
		opts.LLMTimeout = time.Duration(lc.TimeoutMs) * time.Millisecond
	}

	if !hasAPIKey(lc.APIKey) {
		fmt.Fprintf(stderr, "anymd: --llm needs an API key: set %s in the environment "+
			"(or %s / %s), or add \"api_key\": \"${%s}\" to %s\n",
			keyEnvVars[0], keyEnvVars[1], keyEnvVars[2], keyEnvVars[0], configPathOr(cfg.llmConfig))
		return exitUsage
	}

	opts.Describer = llm.New(lc)

	if cfg.llmTranscribe {
		// Transcription is a separate opt-in because it is a separate endpoint
		// (/audio/transcriptions, not chat completions), a separate model, and
		// a separate per-file charge. Bundling it into --llm would bill people
		// for audio they only meant to skip.
		if !hasTranscribeKey(lc.APIKey) {
			fmt.Fprintf(stderr, "anymd: --llm-transcribe needs an API key for the speech endpoint: "+
				"set %s in the environment (or %s / %s)\n",
				transcribeEnvVars[0], transcribeEnvVars[1], transcribeEnvVars[2])
			return exitUsage
		}
		opts.Transcriber = llm.NewTranscriber(lc, cfg.llmTransModel)
	} else if cfg.setFlags["llm-transcribe-model"] {
		fmt.Fprintln(stderr, "anymd: --llm-transcribe-model requires --llm-transcribe")
		return exitUsage
	}

	if !cfg.quiet {
		fmt.Fprintf(stderr, "anymd: llm enabled (model %s) — one model call per image\n", modelName(lc.Model))
		if cfg.llmTranscribe {
			fmt.Fprintf(stderr, "anymd: audio transcription enabled (model %s) — one model call per file\n",
				orDefault(cfg.llmTransModel, llm.DefaultTranscribeModel+" (default)"))
		}
	}
	return exitOK
}

// transcribeEnvVars are the variables llm.NewTranscriber consults, in its
// order. They differ from the captioning set: the speech endpoint is
// OpenAI-shaped, so an Anthropic key is no use to it.
var transcribeEnvVars = []string{"OPENAI_API_KEY", "OPENROUTER_API_KEY", "LLM_API_KEY"}

// hasTranscribeKey reports whether the speech endpoint will authenticate,
// without ever holding the value.
func hasTranscribeKey(fromConfig string) bool {
	if fromConfig != "" {
		return true
	}
	for _, v := range transcribeEnvVars {
		if os.Getenv(v) != "" {
			return true
		}
	}
	return false
}

// hasAPIKey reports whether a key will resolve, WITHOUT ever holding onto,
// returning, or printing the value.
func hasAPIKey(fromConfig string) bool {
	if fromConfig != "" {
		return true
	}
	for _, v := range keyEnvVars {
		if os.Getenv(v) != "" {
			return true
		}
	}
	return false
}

func modelName(m string) string {
	if m == "" {
		return llm.DefaultModel + " (default)"
	}
	return m
}

// configPathOr resolves the effective config path for a message.
func configPathOr(override string) string {
	if override != "" {
		return override
	}
	p, err := llm.ConfigPath()
	if err != nil {
		return "the config file"
	}
	return p
}

// ---------------------------------------------------------------------------
// `anymd config`
// ---------------------------------------------------------------------------

const configUsage = `anymd config — inspect and create the LLM config file

usage: anymd config <command> [--llm-config FILE]

  path   print the config file path
  show   print the resolved config with every secret redacted
  init   write a starter config to the path (mode 0600); never overwrites

  --llm-config FILE   use FILE instead of ~/.config/anymd/anymdconfig.json
`

// starterConfig is what `config init` writes. JSON has no comments, so the
// guidance rides in "_comment" keys, which the loader ignores.
const starterConfig = `{
  "_comment": "anymd LLM config. Every string field supports ${VAR} interpolation from the environment. NEVER put a literal API key in this file — reference an environment variable instead.",

  "model": "openai/gpt-4o-mini",
  "base_url": "https://openrouter.ai/api/v1",
  "api_key": "${OPENROUTER_API_KEY}",

  "_comment_optional": "Everything below is optional; delete what you do not need.",
  "retries": 2,
  "timeout_ms": 60000,
  "anthropic": false
}
`

// runConfig implements the `config` subcommand.
func runConfig(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("anymd config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var path string
	fs.StringVar(&path, "llm-config", "", "")
	fs.Usage = func() { fmt.Fprint(stderr, configUsage) }

	// Accept the flag on either side of the verb: `config --llm-config F show`
	// and `config show --llm-config F` both read naturally.
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
			fmt.Fprint(stderr, configUsage)
			return exitUsage
		}
	}

	resolved := path
	if resolved == "" {
		p, err := llm.ConfigPath()
		if err != nil {
			fmt.Fprintf(stderr, "anymd: %v\n", err)
			return exitFail
		}
		resolved = p
	}

	switch verb {
	case "path":
		fmt.Fprintln(stdout, resolved)
		return exitOK
	case "show":
		return configShow(resolved, stdout, stderr)
	case "init":
		return configInit(resolved, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "anymd: unknown config command %q\n\n", verb)
		fmt.Fprint(stderr, configUsage)
		return exitUsage
	}
}

// configShow prints the config file with every secret redacted.
//
// It deliberately parses the RAW file rather than using llm.Load: Load
// interpolates ${VAR}, and a resolved config holds the actual key. Reading the
// raw form means the key's value is never loaded into a variable that could be
// printed by a later careless edit — what we show is the ${VAR} reference the
// author wrote, plus whether it currently resolves.
func configShow(path string, stdout, stderr io.Writer) int {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		fmt.Fprintf(stdout, "config file: %s (does not exist)\n", path)
		fmt.Fprintf(stdout, "model:       %s\n", llm.DefaultModel+" (default)")
		fmt.Fprintf(stdout, "base_url:    <provider default>\n")
		fmt.Fprintf(stdout, "api_key:     %s\n", redactAPIKey(""))
		fmt.Fprintf(stdout, "\nRun `anymd config init` to create it.\n")
		return exitOK
	}
	if err != nil {
		fmt.Fprintf(stderr, "anymd: %v\n", err)
		return exitFail
	}

	var fc llm.FileConfig
	if err := json.Unmarshal(raw, &fc); err != nil {
		fmt.Fprintf(stderr, "anymd: parsing %s: %v\n", path, err)
		return exitFail
	}

	fmt.Fprintf(stdout, "config file: %s\n", path)
	fmt.Fprintf(stdout, "model:       %s\n", orDefault(fc.Model, llm.DefaultModel+" (default)"))
	fmt.Fprintf(stdout, "base_url:    %s\n", orDefault(fc.BaseURL, "<provider default>"))
	fmt.Fprintf(stdout, "api_key:     %s\n", redactAPIKey(fc.APIKey))
	fmt.Fprintf(stdout, "anthropic:   %t\n", fc.Anthropic)
	fmt.Fprintf(stdout, "retries:     %d\n", fc.Retries)
	fmt.Fprintf(stdout, "timeout_ms:  %s\n", orDefault(itoaOrEmpty(fc.TimeoutMs), "60000 (default)"))
	fmt.Fprintf(stdout, "prompt:      %s\n", orDefault(fc.Prompt, "<built-in>"))
	fmt.Fprintf(stdout, "proxy:       %s\n", redactURL(fc.Proxy))
	fmt.Fprintf(stdout, "insecure_skip_verify: %t\n", fc.InsecureSkipVerify)
	if len(fc.Headers) > 0 {
		names := make([]string, 0, len(fc.Headers))
		for k := range fc.Headers {
			names = append(names, k)
		}
		sort.Strings(names)
		fmt.Fprintln(stdout, "headers:")
		for _, k := range names {
			// A header value is as likely to be a token as api_key is, so it
			// gets exactly the same treatment.
			fmt.Fprintf(stdout, "  %s: %s\n", k, redactAPIKey(fc.Headers[k]))
		}
	}
	return exitOK
}

// envRefOnly matches a field whose entire value is a single ${VAR} reference.
var envRefOnly = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

// redactAPIKey describes a secret field without ever emitting its value.
//
// There are exactly three shapes, and none of them prints the secret — not in
// full, not truncated, not masked. A masked key still leaks its length and its
// prefix, and it teaches people that showing part of a key is fine.
func redactAPIKey(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		for _, name := range keyEnvVars {
			if os.Getenv(name) != "" {
				return fmt.Sprintf("<unset in config; will use $%s from the environment>", name)
			}
		}
		return fmt.Sprintf("<unset — set $%s in the environment>", keyEnvVars[0])
	}
	if m := envRefOnly.FindStringSubmatch(v); m != nil {
		if os.Getenv(m[1]) != "" {
			return fmt.Sprintf("<set from ${%s}>", m[1])
		}
		return fmt.Sprintf("<references ${%s}, which is NOT set>", m[1])
	}
	return "<set literally in the file — move it to ${VAR} and keep it out of the file>"
}

// redactURL strips any embedded credentials from a URL before printing it.
func redactURL(v string) string {
	if v == "" {
		return "<none>"
	}
	if envRefOnly.MatchString(v) {
		return redactAPIKey(v)
	}
	u, err := url.Parse(v)
	if err != nil {
		return "<unparseable>"
	}
	if u.User != nil {
		u.User = url.User("<redacted>")
	}
	return u.String()
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func itoaOrEmpty(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// configInit writes a starter config, refusing to touch an existing one.
//
// O_EXCL rather than a Stat-then-Write: the check and the write are one atomic
// operation, so there is no window in which a config someone else just created
// gets clobbered. Mode 0600 because the file is where a key reference — and,
// against advice, sometimes a key — lives.
func configInit(path string, stdout, stderr io.Writer) int {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			fmt.Fprintf(stderr, "anymd: %v\n", err)
			return exitFail
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			fmt.Fprintf(stderr, "anymd: %s already exists; refusing to overwrite it\n", path)
			return exitFail
		}
		fmt.Fprintf(stderr, "anymd: %v\n", err)
		return exitFail
	}
	if _, err := f.WriteString(starterConfig); err != nil {
		f.Close()
		fmt.Fprintf(stderr, "anymd: %v\n", err)
		return exitFail
	}
	if err := f.Close(); err != nil {
		fmt.Fprintf(stderr, "anymd: %v\n", err)
		return exitFail
	}
	fmt.Fprintf(stdout, "wrote %s (mode 0600)\n", path)
	fmt.Fprintf(stdout, "Set $%s in your environment, then: anymd --llm doc.pdf\n", keyEnvVars[0])
	return exitOK
}
