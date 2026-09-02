package main

// Every test here installs into t.TempDir() via --dir. Nothing in this file may
// write to the real ~/.claude or ~/.agents: a test suite that installs a skill
// into the developer's agent configuration is a test suite that has escaped.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	skillsdata "github.com/muthuishere/anymd/skills"
)

// skillsRun drives the subcommand the way main does, capturing both streams.
func skillsRun(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := runSkills(args, &out, &errb)
	return code, out.String(), errb.String()
}

// skillRoot returns a temporary skills ROOT (the directory an agent scans), so
// the skill lands in <root>/anymd exactly as it would in ~/.claude/skills.
func skillRoot(t *testing.T) (root, dir string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "skills")
	return root, filepath.Join(root, skillDirName)
}

func embeddedSkillMD(t *testing.T) []byte {
	t.Helper()
	b, err := skillsdata.FS.ReadFile(skillsdata.Root + "/SKILL.md")
	if err != nil {
		t.Fatalf("reading embedded SKILL.md: %v", err)
	}
	return b
}

// ---------------------------------------------------------------------------
// the embedded copy is the file on disk
// ---------------------------------------------------------------------------

// TestEmbeddedSkillMatchesRepo is the guard that keeps `go install`ed binaries
// honest: the bytes compiled into the binary are the bytes of the file people
// review in the repository. The embed shim makes drift impossible by
// construction, and this test makes that a checked property rather than a
// property of how someone happened to wire it up.
func TestEmbeddedSkillMatchesRepo(t *testing.T) {
	onDisk, err := os.ReadFile(filepath.Join("..", "..", "skills", "anymd", "SKILL.md"))
	if err != nil {
		t.Fatalf("reading skills/anymd/SKILL.md: %v", err)
	}
	if !bytes.Equal(onDisk, embeddedSkillMD(t)) {
		t.Fatal("embedded SKILL.md differs from skills/anymd/SKILL.md")
	}

	files, err := embeddedSkill()
	if err != nil {
		t.Fatalf("embeddedSkill: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no embedded skill files")
	}
	var found bool
	for _, f := range files {
		if f.rel == "SKILL.md" {
			found = true
		}
		if strings.Contains(f.rel, "..") {
			t.Fatalf("embedded path escapes: %q", f.rel)
		}
	}
	if !found {
		t.Fatal("embedded skill has no SKILL.md")
	}
}

// ---------------------------------------------------------------------------
// install
// ---------------------------------------------------------------------------

func TestSkillsInstallCreatesFile(t *testing.T) {
	root, dir := skillRoot(t)

	code, out, errs := skillsRun("install", "--dir", root)
	if code != exitOK {
		t.Fatalf("code = %d, stderr = %s", code, errs)
	}
	if out != "" {
		t.Fatalf("install wrote to stdout: %q", out)
	}
	if !strings.Contains(errs, "installed") {
		t.Fatalf("no progress on stderr: %q", errs)
	}

	dst := filepath.Join(dir, "SKILL.md")
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading installed skill: %v", err)
	}
	if !bytes.Equal(got, embeddedSkillMD(t)) {
		t.Fatal("installed content differs from the embedded skill")
	}

	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != skillFileMode {
		t.Fatalf("file mode = %v, want %v", fi.Mode().Perm(), skillFileMode)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != skillDirMode {
		t.Fatalf("dir mode = %v, want %v", di.Mode().Perm(), skillDirMode)
	}
}

func TestSkillsInstallTwiceIsAlreadyCurrent(t *testing.T) {
	root, _ := skillRoot(t)
	if code, _, errs := skillsRun("install", "--dir", root); code != exitOK {
		t.Fatalf("first install: code %d, stderr %s", code, errs)
	}
	code, _, errs := skillsRun("install", "--dir", root)
	if code != exitOK {
		t.Fatalf("second install: code = %d, want %d (stderr %s)", code, exitOK, errs)
	}
	if !strings.Contains(errs, "already current") {
		t.Fatalf("stderr = %q, want it to say already current", errs)
	}
}

func TestSkillsInstallRefusesToClobberAnEditedCopy(t *testing.T) {
	root, dir := skillRoot(t)
	if code, _, errs := skillsRun("install", "--dir", root); code != exitOK {
		t.Fatalf("install: code %d, stderr %s", code, errs)
	}
	dst := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(dst, []byte("# my own notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, errs := skillsRun("install", "--dir", root)
	if code != exitFail {
		t.Fatalf("code = %d, want %d", code, exitFail)
	}
	if !strings.Contains(errs, "--force") {
		t.Fatalf("stderr = %q, want it to name --force", errs)
	}
	if b, _ := os.ReadFile(dst); string(b) != "# my own notes\n" {
		t.Fatal("the edited copy was overwritten anyway")
	}

	code, _, errs = skillsRun("install", "--dir", root, "--force")
	if code != exitOK {
		t.Fatalf("--force: code = %d, stderr %s", code, errs)
	}
	if b, _ := os.ReadFile(dst); !bytes.Equal(b, embeddedSkillMD(t)) {
		t.Fatal("--force did not restore the built-in skill")
	}
}

// ---------------------------------------------------------------------------
// list / path
// ---------------------------------------------------------------------------

func TestSkillsListReportsAllThreeStates(t *testing.T) {
	root, dir := skillRoot(t)

	code, out, _ := skillsRun("list", "--dir", root)
	if code != exitOK {
		t.Fatalf("code = %d", code)
	}
	if !strings.HasPrefix(out, stateNotInstalled+"\t") {
		t.Fatalf("out = %q, want %q first", out, stateNotInstalled)
	}
	if !strings.Contains(out, dir) {
		t.Fatalf("out = %q, want it to name %s", out, dir)
	}

	if code, _, errs := skillsRun("install", "--dir", root); code != exitOK {
		t.Fatalf("install: %d %s", code, errs)
	}
	if _, out, _ = skillsRun("list", "--dir", root); !strings.HasPrefix(out, stateCurrent+"\t") {
		t.Fatalf("after install out = %q, want %q", out, stateCurrent)
	}

	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, out, _ = skillsRun("list", "--dir", root); !strings.HasPrefix(out, stateDiffers+"\t") {
		t.Fatalf("after edit out = %q, want %q", out, stateDiffers)
	}
}

func TestSkillsPathIsScriptFriendly(t *testing.T) {
	root, dir := skillRoot(t)
	code, out, errs := skillsRun("path", "--dir", root)
	if code != exitOK {
		t.Fatalf("code = %d, stderr %s", code, errs)
	}
	if strings.TrimSpace(out) != dir {
		t.Fatalf("out = %q, want %q", out, dir)
	}
	// Printing a path must not bring it into being: `skills path` is what a
	// script calls to decide whether anything is installed.
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("skills path created the directory")
	}
	// The flag is accepted on either side of the verb, like `anymd config`.
	if code, out2, _ := skillsRun("--dir", root, "path"); code != exitOK || strings.TrimSpace(out2) != dir {
		t.Fatalf("flag-before-verb: code %d out %q", code, out2)
	}
}

func TestSkillsPathDefaultsToBothWellKnownDirs(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory on this machine")
	}
	// Read-only: `path` never creates anything, so this cannot touch the real
	// agent configuration.
	code, out, _ := skillsRun("path")
	if code != exitOK {
		t.Fatalf("code = %d", code)
	}
	lines := strings.Fields(strings.TrimSpace(out))
	want := []string{
		filepath.Join(home, ".claude", "skills", skillDirName),
		filepath.Join(home, ".agents", "skills", skillDirName),
	}
	if len(lines) != 2 || lines[0] != want[0] || lines[1] != want[1] {
		t.Fatalf("out = %q, want %v", out, want)
	}

	if _, out, _ := skillsRun("path", "--target", "claude"); strings.TrimSpace(out) != want[0] {
		t.Fatalf("--target claude out = %q", out)
	}
	if _, out, _ := skillsRun("path", "--target", "agents"); strings.TrimSpace(out) != want[1] {
		t.Fatalf("--target agents out = %q", out)
	}
	if code, _, _ := skillsRun("path", "--target", "nope"); code != exitUsage {
		t.Fatalf("bad --target: code = %d, want %d", code, exitUsage)
	}
}

// TestSkillsDirNamedAnymdIsNotNested guards the natural typo: pasting the path
// `skills path` printed back into `--dir` must not bury the skill one level
// deeper, where no agent looks.
func TestSkillsDirNamedAnymdIsNotNested(t *testing.T) {
	_, dir := skillRoot(t)
	_, out, _ := skillsRun("path", "--dir", dir)
	if strings.TrimSpace(out) != dir {
		t.Fatalf("out = %q, want %q (not nested)", strings.TrimSpace(out), dir)
	}
}

// ---------------------------------------------------------------------------
// uninstall
// ---------------------------------------------------------------------------

func TestSkillsUninstallRemovesWhatItInstalled(t *testing.T) {
	root, dir := skillRoot(t)
	if code, _, errs := skillsRun("install", "--dir", root); code != exitOK {
		t.Fatalf("install: %d %s", code, errs)
	}
	code, out, errs := skillsRun("uninstall", "--dir", root)
	if code != exitOK {
		t.Fatalf("code = %d, stderr %s", code, errs)
	}
	if out != "" {
		t.Fatalf("uninstall wrote to stdout: %q", out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("%s still exists after uninstall", dir)
	}
	// The skills ROOT is not ours to remove.
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("uninstall removed the skills root %s", root)
	}

	// Uninstalling nothing is not a failure.
	if code, _, _ := skillsRun("uninstall", "--dir", root); code != exitOK {
		t.Fatalf("second uninstall: code = %d, want %d", code, exitOK)
	}
}

// TestSkillsUninstallKeepsForeignFiles: a directory holding something anymd did
// not write is left standing, with the file intact.
func TestSkillsUninstallKeepsForeignFiles(t *testing.T) {
	root, dir := skillRoot(t)
	if code, _, errs := skillsRun("install", "--dir", root); code != exitOK {
		t.Fatalf("install: %d %s", code, errs)
	}
	foreign := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(foreign, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, errs := skillsRun("uninstall", "--dir", root)
	if code != exitOK {
		t.Fatalf("code = %d, stderr %s", code, errs)
	}
	if !strings.Contains(errs, "did not install") {
		t.Fatalf("stderr = %q, want it to explain why the directory stayed", errs)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("uninstall removed a file anymd did not write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); !os.IsNotExist(err) {
		t.Fatal("SKILL.md survived uninstall")
	}
}

func TestSkillsUninstallRefusesAnEditedCopyWithoutForce(t *testing.T) {
	root, dir := skillRoot(t)
	if code, _, errs := skillsRun("install", "--dir", root); code != exitOK {
		t.Fatalf("install: %d %s", code, errs)
	}
	dst := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(dst, []byte("my own notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := skillsRun("uninstall", "--dir", root); code != exitFail ||
		!strings.Contains(errs, "--force") {
		t.Fatalf("code = %d stderr = %q", code, errs)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatal("the edited copy was removed anyway")
	}
	if code, _, errs := skillsRun("uninstall", "--dir", root, "--force"); code != exitOK {
		t.Fatalf("--force: code = %d stderr %s", code, errs)
	}
}

// TestSkillsRefusesUnsafeDir is the one that matters: a bad --dir must not be
// a route to deleting a home directory or something next to the filesystem
// root. The refusal happens before anything is created, so install is checked
// too — a guard that only covers the delete path is half a guard.
func TestSkillsRefusesUnsafeDir(t *testing.T) {
	sep := string(filepath.Separator)
	for _, dir := range []string{
		sep,                         // resolves to /anymd — one segment from the root
		filepath.Join(sep, "anymd"), // already named anymd, so used verbatim
	} {
		if code, _, errs := skillsRun("uninstall", "--dir", dir); code != exitUsage {
			t.Fatalf("uninstall --dir %q: code = %d, want %d (stderr %s)", dir, code, exitUsage, errs)
		}
		if code, _, errs := skillsRun("install", "--dir", dir); code != exitUsage {
			t.Fatalf("install --dir %q: code = %d, want %d (stderr %s)", dir, code, exitUsage, errs)
		}
		if _, err := os.Stat(filepath.Join(sep, "anymd")); !os.IsNotExist(err) {
			t.Fatal("a refused install created something anyway")
		}
	}

	// The home directory itself is refused even if a target somehow resolves
	// to it, which is the check that keeps `--dir ~` from ever being fatal.
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if err := checkSkillDir(home); err == nil {
			t.Fatal("checkSkillDir accepted the home directory")
		}
	}
}

// TestSkillsWithinDir covers the containment check directly: it is the last
// line of defence in the delete loop, so it must not be only incidentally
// exercised.
func TestSkillsWithinDir(t *testing.T) {
	base := filepath.Join(string(filepath.Separator), "tmp", "skills", "anymd")
	for _, tc := range []struct {
		p    string
		want bool
	}{
		{filepath.Join(base, "SKILL.md"), true},
		{filepath.Join(base, "sub", "a.md"), true},
		{base, true},
		{filepath.Join(base, "..", "other.md"), false},
		{filepath.Join(string(filepath.Separator), "etc", "passwd"), false},
	} {
		if got := withinDir(base, tc.p); got != tc.want {
			t.Fatalf("withinDir(%q, %q) = %v, want %v", base, tc.p, got, tc.want)
		}
	}
	if _, err := destFor(base, "../escape.md"); err == nil {
		t.Fatal("destFor allowed an escaping relative path")
	}
}

// ---------------------------------------------------------------------------
// usage
// ---------------------------------------------------------------------------

func TestSkillsUnknownVerbIsUsageError(t *testing.T) {
	root, _ := skillRoot(t)
	code, out, errs := skillsRun("frobnicate", "--dir", root)
	if code != exitUsage {
		t.Fatalf("code = %d, want %d", code, exitUsage)
	}
	if out != "" {
		t.Fatalf("usage error wrote to stdout: %q", out)
	}
	if !strings.Contains(errs, "unknown skills command") {
		t.Fatalf("stderr = %q", errs)
	}

	// No verb at all prints the usage and fails the same way.
	if code, _, _ := skillsRun(); code != exitUsage {
		t.Fatalf("no verb: code = %d, want %d", code, exitUsage)
	}
	// A flag that means nothing for the verb is an error, not a silent no-op.
	if code, _, _ := skillsRun("list", "--dir", root, "--force"); code != exitUsage {
		t.Fatalf("--force on list: code = %d, want %d", code, exitUsage)
	}
	if code, _, _ := skillsRun("path", "--dir", root, "--target", "claude"); code != exitUsage {
		t.Fatalf("--dir with --target: code = %d, want %d", code, exitUsage)
	}
}

// TestSkillsIsDiscoverableFromHelp is the regression test for the bug this repo
// has already shipped once: cacheFlagUsage was written and never spliced into
// the usage text, so the flags worked and nobody could find them.
func TestSkillsIsDiscoverableFromHelp(t *testing.T) {
	for _, want := range []string{
		"anymd skills",
		"skills install",
		"skills uninstall",
		"--target",
	} {
		if !strings.Contains(usage, want) {
			t.Fatalf("--help usage text does not mention %q", want)
		}
	}
}

// TestSkillsDispatchesFromRun makes sure the subcommand is reachable through
// the real entry point and not only through runSkills.
func TestSkillsDispatchesFromRun(t *testing.T) {
	root, dir := skillRoot(t)
	var out, errb bytes.Buffer
	if code := run([]string{"skills", "path", "--dir", root}, &out, &errb); code != exitOK {
		t.Fatalf("code = %d, stderr %s", code, errb.String())
	}
	if strings.TrimSpace(out.String()) != dir {
		t.Fatalf("out = %q, want %q", out.String(), dir)
	}
}
