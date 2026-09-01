package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
