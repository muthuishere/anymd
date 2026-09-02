package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/muthuishere/anymd"
)

// runCLI drives run() with captured streams, restoring the stdin indirection.
func runCLI(t *testing.T, in string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	prev := stdin
	stdin = strings.NewReader(in)
	defer func() { stdin = prev }()

	var so, se bytes.Buffer
	code = run(args, &so, &se)
	return code, so.String(), se.String()
}

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStdinToStdout(t *testing.T) {
	code, out, errOut := runCLI(t, "hello\nworld\n")
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	if out != "hello\nworld\n" {
		t.Fatalf("stdout = %q", out)
	}
	if errOut != "" {
		t.Fatalf("stderr should be empty, got %q", errOut)
	}
}

func TestStdinWithTypeHint(t *testing.T) {
	code, out, _ := runCLI(t, "a,b\n1,2\n", "-t", "txt")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if out != "a,b\n1,2\n" {
		t.Fatalf("stdout = %q", out)
	}
}

func TestSingleFileToStdout(t *testing.T) {
	dir := t.TempDir()
	src := write(t, dir, "note.txt", "one\ntwo\n")

	code, out, errOut := runCLI(t, "", src)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	if out != "one\ntwo\n" {
		t.Fatalf("stdout = %q", out)
	}
	// The whole point of the stderr split: stdout must stay pipeable.
	if errOut != "" {
		t.Fatalf("progress leaked to stderr expectation broken: %q", errOut)
	}
	if strings.Contains(out, "converted") {
		t.Fatalf("progress noise leaked into stdout: %q", out)
	}
}

func TestOutputToFile(t *testing.T) {
	dir := t.TempDir()
	src := write(t, dir, "note.txt", "body text\n")
	dst := filepath.Join(dir, "out.md")

	code, out, errOut := runCLI(t, "", "-o", dst, src)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	if out != "" {
		t.Fatalf("stdout should be empty when -o is used, got %q", out)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "body text\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestBatchToOutdir(t *testing.T) {
	dir := t.TempDir()
	a := write(t, dir, "a.txt", "alpha\n")
	b := write(t, dir, "b.csv", "x\n")
	outdir := filepath.Join(dir, "nested", "out")

	code, out, errOut := runCLI(t, "", "-d", outdir, a, b)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty in batch mode", out)
	}
	if !strings.Contains(errOut, "converted 2, failed 0") {
		t.Fatalf("missing summary line: %q", errOut)
	}
	got, err := os.ReadFile(filepath.Join(outdir, "a.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "alpha\n" {
		t.Fatalf("a.md = %q", got)
	}
	if _, err := os.Stat(filepath.Join(outdir, "b.md")); err != nil {
		t.Fatalf("b.md missing: %v", err)
	}
}

func TestBatchQuietSuppressesProgress(t *testing.T) {
	dir := t.TempDir()
	a := write(t, dir, "a.txt", "alpha\n")
	b := write(t, dir, "b.txt", "beta\n")
	outdir := filepath.Join(dir, "out")

	code, _, errOut := runCLI(t, "", "-q", "-d", outdir, a, b)
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if errOut != "" {
		t.Fatalf("-q should silence stderr, got %q", errOut)
	}
}

func TestBatchOutputIsInInputOrder(t *testing.T) {
	dir := t.TempDir()
	var args []string
	var want strings.Builder
	for _, name := range []string{"z", "m", "a", "q", "b", "c", "d", "e"} {
		args = append(args, write(t, dir, name+".txt", name+"\n"))
		want.WriteString(name + "\n")
	}
	code, out, _ := runCLI(t, "", append([]string{"-q"}, args...)...)
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if out != want.String() {
		t.Fatalf("stdout = %q, want %q", out, want.String())
	}
}

func TestFailingInputStillConvertsTheRest(t *testing.T) {
	dir := t.TempDir()
	good := write(t, dir, "good.txt", "fine\n")
	missing := filepath.Join(dir, "nope.txt")
	outdir := filepath.Join(dir, "out")

	// A missing path fails at expandInputs, so use an unreadable-but-present
	// input instead: a binary blob with no converter.
	bad := write(t, dir, "bad.bin", "\x00\x01\x02\xff\xfe")

	code, _, errOut := runCLI(t, "", "-d", outdir, good, bad)
	if code != exitFail {
		t.Fatalf("exit = %d, want %d (stderr=%q)", code, exitFail, errOut)
	}
	if !strings.Contains(errOut, "converted 1, failed 1") {
		t.Fatalf("summary = %q", errOut)
	}
	if _, err := os.Stat(filepath.Join(outdir, "good.md")); err != nil {
		t.Fatalf("the good input should still have converted: %v", err)
	}

	// And a genuinely missing file is an input error too.
	if code, _, _ := runCLI(t, "", missing); code != exitFail {
		t.Fatalf("missing file exit = %d, want %d", code, exitFail)
	}
}

func TestUsageErrors(t *testing.T) {
	dir := t.TempDir()
	a := write(t, dir, "a.txt", "a\n")
	b := write(t, dir, "b.txt", "b\n")

	if code, _, _ := runCLI(t, "", "--no-such-flag"); code != exitUsage {
		t.Fatalf("unknown flag exit = %d, want %d", code, exitUsage)
	}
	if code, _, _ := runCLI(t, "", "-o", filepath.Join(dir, "one.md"), a, b); code != exitUsage {
		t.Fatalf("-o FILE with two inputs exit = %d, want %d", code, exitUsage)
	}
	if code, _, _ := runCLI(t, "", "-o", filepath.Join(dir, "x.md"), "-d", dir, a); code != exitUsage {
		t.Fatalf("-o with -d exit = %d, want %d", code, exitUsage)
	}
}

func TestODirectoryBatchesIntoIt(t *testing.T) {
	dir := t.TempDir()
	a := write(t, dir, "a.txt", "alpha\n")
	b := write(t, dir, "b.txt", "beta\n")
	outdir := filepath.Join(dir, "out")
	if err := os.Mkdir(outdir, 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runCLI(t, "", "-q", "-o", outdir, a, b)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	for _, n := range []string{"a.md", "b.md"} {
		if _, err := os.Stat(filepath.Join(outdir, n)); err != nil {
			t.Fatalf("%s missing: %v", n, err)
		}
	}
}

func TestDirectoryWithoutRecursiveIsAnError(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "sub/one.txt", "one\n")
	code, _, errOut := runCLI(t, "", filepath.Join(dir, "sub"))
	if code != exitFail {
		t.Fatalf("exit = %d, want %d", code, exitFail)
	}
	if !strings.Contains(errOut, "use -r") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestRecursiveWalksDirectories(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "sub/one.txt", "one\n")
	write(t, dir, "sub/deep/two.txt", "two\n")
	outdir := filepath.Join(dir, "out")

	code, _, errOut := runCLI(t, "", "-r", "-d", outdir, filepath.Join(dir, "sub"))
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	for _, n := range []string{"one.md", "two.md"} {
		if _, err := os.Stat(filepath.Join(outdir, n)); err != nil {
			t.Fatalf("%s missing: %v", n, err)
		}
	}
}

func TestExtFlag(t *testing.T) {
	dir := t.TempDir()
	a := write(t, dir, "a.txt", "alpha\n")
	outdir := filepath.Join(dir, "out")
	if code, _, _ := runCLI(t, "", "-q", "--ext", "markdown", "-d", outdir, a); code != exitOK {
		t.Fatal("expected success")
	}
	if _, err := os.Stat(filepath.Join(outdir, "a.markdown")); err != nil {
		t.Fatalf("a.markdown missing: %v", err)
	}
}

func TestList(t *testing.T) {
	code, out, errOut := runCLI(t, "", "--list")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if errOut != "" {
		t.Fatalf("stderr = %q", errOut)
	}
	if !strings.Contains(out, "plaintext") {
		t.Fatalf("--list output missing plaintext: %q", out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if lines[len(lines)-1] != "plaintext" {
		t.Fatalf("plaintext should sort last (PriorityFallback), got %q", out)
	}
}

func TestVersion(t *testing.T) {
	code, out, _ := runCLI(t, "", "--version")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"anymd ", "commit:", "go:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("--version output %q missing %q", out, want)
		}
	}
}

func TestHelpExitsZero(t *testing.T) {
	if code, _, _ := runCLI(t, "", "-h"); code != exitOK {
		t.Fatalf("-h should exit 0")
	}
}

func TestRenderBodyTitle(t *testing.T) {
	cases := []struct {
		name  string
		title string
		body  string
		want  string
	}{
		{"prepends", "My Doc", "text\n", "# My Doc\n\ntext\n"},
		{"already h1", "My Doc", "# Other\n\ntext\n", "# Other\n\ntext\n"},
		{"no title", "", "text\n", "text\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderBody(anymd.Result{Title: tc.title, Markdown: tc.body}, true)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOutputBase(t *testing.T) {
	cases := map[string]string{
		"a/b/report.docx":                 "report",
		"report.txt":                      "report",
		"https://example.com/docs/x.html": "x",
		"https://example.com":             "example.com",
	}
	for in, want := range cases {
		if got := outputBase(in); got != want {
			t.Fatalf("outputBase(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- LLM flags and the config subcommand -----------------------------------
//
// Nothing here touches the network or a real key. The point of these tests is
// the two things that are easy to get quietly wrong: a flag that is accepted
// and then ignored, and a secret that ends up on stdout.

// isolateConfig points ConfigPath() at a temp dir and clears every key the
// resolver consults, so a developer's real shell cannot make a test pass.
func isolateConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	for _, k := range append(append([]string{}, keyEnvVars...), transcribeEnvVars...) {
		t.Setenv(k, "")
	}
	return filepath.Join(dir, "anymd", "anymdconfig.json")
}

// fakeConfig writes a config file that references an environment variable,
// never a literal key.
func fakeConfig(t *testing.T, dir string) string {
	t.Helper()
	return write(t, dir, "anymdconfig.json", `{
  "model": "from-file/model",
  "base_url": "https://from-file.invalid/v1",
  "api_key": "${ANYMD_TEST_KEY}",
  "timeout_ms": 12000
}`)
}

func TestLLMFlagWithoutLLMIsUsageError(t *testing.T) {
	isolateConfig(t)
	dir := t.TempDir()
	src := write(t, dir, "a.txt", "hello\n")

	for _, args := range [][]string{
		{"--llm-model", "gpt-4o", src},
		{"--llm-base-url", "http://localhost:11434/v1", src},
		{"--llm-timeout", "10s", src},
		{"--llm-transcribe", src},
		{"--llm-transcribe-model", "whisper-1", src},
		{"--llm-config", "/nope.json", src},
	} {
		code, out, errOut := runCLI(t, "", args...)
		if code != exitUsage {
			t.Fatalf("%v: exit = %d, want %d (stderr %q)", args, code, exitUsage, errOut)
		}
		if !strings.Contains(errOut, "requires --llm") {
			t.Fatalf("%v: stderr = %q, want it to name the missing --llm", args, errOut)
		}
		if out != "" {
			t.Fatalf("%v: stdout = %q, want empty", args, out)
		}
	}
}

func TestLLMWithoutAPIKeyNamesTheEnvVar(t *testing.T) {
	isolateConfig(t)
	dir := t.TempDir()
	src := write(t, dir, "a.txt", "hello\n")

	code, out, errOut := runCLI(t, "", "--llm", src)
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, exitUsage, errOut)
	}
	if !strings.Contains(errOut, "OPENROUTER_API_KEY") {
		t.Fatalf("stderr = %q, want it to name the environment variable", errOut)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty", out)
	}
}

// TestLLMPrecedence checks explicit flag > config file, without making a call:
// applyLLM is driven directly so the resolved values can be inspected.
func TestLLMPrecedence(t *testing.T) {
	isolateConfig(t)
	t.Setenv("ANYMD_TEST_KEY", "not-a-real-key")
	path := fakeConfig(t, t.TempDir())

	// File alone.
	cfg := &config{llm: true, llmConfig: path, quiet: true, setFlags: map[string]bool{}}
	opts := &anymd.Options{}
	var se bytes.Buffer
	if code := applyLLM(cfg, opts, &se); code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, se.String())
	}
	if opts.Describer == nil {
		t.Fatal("Describer not wired")
	}
	if opts.LLMTimeout != 12*time.Second {
		t.Fatalf("LLMTimeout = %v, want the file's 12s", opts.LLMTimeout)
	}

	// Flag beats file.
	cfg = &config{
		llm: true, llmConfig: path, quiet: true,
		llmModel: "flag/model", llmTimeout: 3 * time.Second,
		setFlags: map[string]bool{"llm-model": true, "llm-timeout": true},
	}
	opts = &anymd.Options{}
	se.Reset()
	if code := applyLLM(cfg, opts, &se); code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, se.String())
	}
	if opts.LLMTimeout != 3*time.Second {
		t.Fatalf("LLMTimeout = %v, want the flag's 3s", opts.LLMTimeout)
	}
}

// TestLLMChatterStaysOffStdout is the pipeline guarantee: `anymd --llm x | grep`
// must not see a word about models.
func TestLLMChatterStaysOffStdout(t *testing.T) {
	isolateConfig(t)
	t.Setenv("OPENROUTER_API_KEY", "not-a-real-key")
	dir := t.TempDir()
	src := write(t, dir, "a.txt", "hello\n")

	code, out, errOut := runCLI(t, "", "--llm", src)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	if out != "hello\n" {
		t.Fatalf("stdout = %q, want only the markdown", out)
	}
	if !strings.Contains(errOut, "llm enabled") {
		t.Fatalf("stderr = %q, want the progress note on stderr", errOut)
	}

	// -q suppresses it entirely.
	code, out, errOut = runCLI(t, "", "--llm", "-q", src)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	if out != "hello\n" {
		t.Fatalf("stdout = %q", out)
	}
	if errOut != "" {
		t.Fatalf("stderr = %q, want -q to silence the llm note", errOut)
	}
}

func TestConfigPath(t *testing.T) {
	want := isolateConfig(t)

	code, out, errOut := runCLI(t, "", "config", "path")
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	if strings.TrimSpace(out) != want {
		t.Fatalf("stdout = %q, want %q", strings.TrimSpace(out), want)
	}

	// An override is honored verbatim.
	code, out, _ = runCLI(t, "", "config", "path", "--llm-config", "/somewhere/else.json")
	if code != exitOK || strings.TrimSpace(out) != "/somewhere/else.json" {
		t.Fatalf("exit = %d, stdout = %q", code, out)
	}
}

// TestConfigShowNeverPrintsASecret is the test that matters. A real key is put
// in the environment and in a literal field; neither may appear anywhere in the
// output, in any form.
func TestConfigShowNeverPrintsASecret(t *testing.T) {
	isolateConfig(t)
	const secret = "sk-do-not-print-me-1234567890"
	t.Setenv("ANYMD_TEST_KEY", secret)
	t.Setenv("OPENROUTER_API_KEY", secret)

	dir := t.TempDir()

	// 1. the ${VAR} form — the shape we recommend.
	path := fakeConfig(t, dir)
	code, out, errOut := runCLI(t, "", "config", "show", "--llm-config", path)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	assertNoSecret(t, secret, out+errOut)
	if !strings.Contains(out, "<set from ${ANYMD_TEST_KEY}>") {
		t.Fatalf("stdout = %q, want the ${VAR} reference named", out)
	}
	if !strings.Contains(out, "from-file/model") {
		t.Fatalf("stdout = %q, want the non-secret fields shown", out)
	}

	// 2. a key written literally into the file, and a token in a header, and
	//    credentials in a proxy URL — the careless cases.
	literal := write(t, dir, "literal.json", `{
  "model": "m",
  "api_key": "`+secret+`",
  "proxy": "http://user:`+secret+`@127.0.0.1:8080",
  "headers": { "Authorization": "Bearer `+secret+`" }
}`)
	code, out, errOut = runCLI(t, "", "config", "show", "--llm-config", literal)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	assertNoSecret(t, secret, out+errOut)
	if !strings.Contains(out, "set literally in the file") {
		t.Fatalf("stdout = %q, want the literal key called out", out)
	}

	// 3. no file at all.
	code, out, errOut = runCLI(t, "", "config", "show", "--llm-config", filepath.Join(dir, "missing.json"))
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	assertNoSecret(t, secret, out+errOut)
}

// assertNoSecret fails if the value appears in full or as any prefix long
// enough to be a useful leak. A "masked" key is still a leak.
func assertNoSecret(t *testing.T, secret, output string) {
	t.Helper()
	if strings.Contains(output, secret) {
		t.Fatal("the API key value appears in the output")
	}
	for n := 8; n <= len(secret); n++ {
		if strings.Contains(output, secret[:n]) {
			t.Fatalf("a %d-character prefix of the API key appears in the output", n)
		}
	}
}

func TestConfigInit(t *testing.T) {
	isolateConfig(t)
	path := filepath.Join(t.TempDir(), "nested", "anymdconfig.json")

	code, out, errOut := runCLI(t, "", "config", "init", "--llm-config", path)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	if !strings.Contains(out, path) {
		t.Fatalf("stdout = %q, want it to name the file written", out)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no POSIX permission bits — it uses ACLs, and Go reports 0666
	// for any file it creates there. The 0600 we pass to OpenFile is still
	// correct and still honored on every platform that has modes; only the
	// assertion is Unix-specific.
	if runtime.GOOS != "windows" {
		if got := st.Mode().Perm(); got != 0o600 {
			t.Fatalf("mode = %o, want 600", got)
		}
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "${OPENROUTER_API_KEY}") {
		t.Fatalf("starter config should reference an env var, got %q", body)
	}

	// It must refuse to overwrite, and must not have touched the file.
	code, _, errOut = runCLI(t, "", "config", "init", "--llm-config", path)
	if code != exitFail {
		t.Fatalf("exit = %d, want %d on an existing file", code, exitFail)
	}
	if !strings.Contains(errOut, "refusing to overwrite") {
		t.Fatalf("stderr = %q", errOut)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(body) {
		t.Fatal("the existing config was modified")
	}
}

func TestConfigUnknownVerb(t *testing.T) {
	isolateConfig(t)
	for _, args := range [][]string{{"config"}, {"config", "wat"}} {
		code, out, _ := runCLI(t, "", args...)
		if code != exitUsage {
			t.Fatalf("%v: exit = %d, want %d", args, code, exitUsage)
		}
		if out != "" {
			t.Fatalf("%v: stdout = %q, want empty", args, out)
		}
	}
}

func TestConfigStarterFileIsLoadable(t *testing.T) {
	isolateConfig(t)
	t.Setenv("OPENROUTER_API_KEY", "not-a-real-key")
	path := filepath.Join(t.TempDir(), "anymdconfig.json")
	if code, _, errOut := runCLI(t, "", "config", "init", "--llm-config", path); code != exitOK {
		t.Fatalf("init failed: %q", errOut)
	}

	// The file `config init` writes must be one `--llm` accepts, comments and
	// all — a starter config that does not load is worse than none.
	dir := t.TempDir()
	src := write(t, dir, "a.txt", "hello\n")
	code, out, errOut := runCLI(t, "", "--llm", "-q", "--llm-config", path, src)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	if out != "hello\n" {
		t.Fatalf("stdout = %q", out)
	}
}

func TestLLMTranscribeWiring(t *testing.T) {
	isolateConfig(t)
	t.Setenv("ANYMD_TEST_KEY", "not-a-real-key")
	path := fakeConfig(t, t.TempDir())

	// --llm alone must NOT wire a Transcriber: audio costs money per file and
	// is a separate endpoint, so it gets its own opt-in.
	cfg := &config{llm: true, llmConfig: path, quiet: true, setFlags: map[string]bool{}}
	opts := &anymd.Options{}
	var se bytes.Buffer
	if code := applyLLM(cfg, opts, &se); code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, se.String())
	}
	if opts.Transcriber != nil {
		t.Fatal("--llm alone wired a Transcriber")
	}

	// With the flag, it is wired.
	cfg = &config{
		llm: true, llmTranscribe: true, llmConfig: path, quiet: true,
		setFlags: map[string]bool{"llm-transcribe": true},
	}
	opts = &anymd.Options{}
	se.Reset()
	if code := applyLLM(cfg, opts, &se); code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, se.String())
	}
	if opts.Transcriber == nil {
		t.Fatal("--llm-transcribe did not wire a Transcriber")
	}

	// --llm-transcribe-model without --llm-transcribe is a usage error, not a
	// silently ignored flag.
	cfg = &config{
		llm: true, llmConfig: path, quiet: true, llmTransModel: "whisper-1",
		setFlags: map[string]bool{"llm-transcribe-model": true},
	}
	se.Reset()
	if code := applyLLM(cfg, &anymd.Options{}, &se); code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(se.String(), "requires --llm-transcribe") {
		t.Fatalf("stderr = %q", se.String())
	}
}

// TestFlagsMayFollowFilenames pins markitdown compatibility. Go's flag package
// stops at the first positional, so `anymd report.docx -o report.md` — the form
// markitdown's own --help documents — used to fail with
// "stat -o: no such file or directory".
func TestFlagsMayFollowFilenames(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "report.csv")
	if err := os.WriteFile(src, []byte("a,b\n1,2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "report.md")

	var out, errOut bytes.Buffer
	if code := run([]string{src, "-o", dst}, &out, &errOut); code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, errOut.String())
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("output not written: %v", err)
	}
	if !strings.Contains(string(got), "| a | b |") {
		t.Errorf("unexpected output: %q", got)
	}
}

// A "--" terminator must still reach a file whose name looks like a flag.
func TestDoubleDashStopsPermutation(t *testing.T) {
	dir := t.TempDir()
	odd := filepath.Join(dir, "-o")
	if err := os.WriteFile(odd, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"--", odd}, &out, &errOut); code != exitOK {
		t.Fatalf("exit = %d, stderr = %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "hello") {
		t.Errorf("stdout = %q", out.String())
	}
}

// permuteArgs must not swallow the argument after a boolean flag.
func TestPermuteArgsKeepsBooleanOperands(t *testing.T) {
	got := permuteArgs([]string{"a.csv", "-q", "b.csv"})
	want := []string{"-q", "a.csv", "b.csv"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
