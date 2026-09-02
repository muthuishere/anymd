package main

// Installing the agent skill, on the command line.
//
// A CLI that AI agents are meant to reach for has to be discoverable BY those
// agents, and agents discover skills by looking in two well-known directories.
// Shipping SKILL.md inside the repository and hoping somebody finds it and
// copies it by hand is not a distribution path.
//
// Three rules shape this file:
//
//   - The skill is embedded in the binary, not read from disk relative to the
//     executable. The people who most need `anymd skills install` are exactly
//     the people who ran `go install` and have no checkout to read from.
//   - Nothing is overwritten silently. A skill file is a text file a user may
//     have edited; a byte-for-byte match is "already current", anything else is
//     a refusal that names --force.
//   - Nothing is deleted outside the resolved target directory, and a directory
//     holding files anymd did not write is left alone. `--dir ~` must not be a
//     way to lose your home directory.

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	skillsdata "github.com/muthuishere/anymd/skills"
)

// skillDirName is the directory the skill installs as, inside a skills root.
const skillDirName = skillsdata.Root

// skillDirMode and skillFileMode are the modes new directories and files get.
// A skill is world-readable documentation, not a secret — unlike the LLM
// config, which is 0600 because a key can end up in it.
const (
	skillDirMode  os.FileMode = 0o755
	skillFileMode os.FileMode = 0o644
)

// errUnsafeSkillDir marks a destination too dangerous to create or delete in.
var errUnsafeSkillDir = errors.New("unsafe skill directory")

const skillsUsage = `anymd skills — install the anymd agent skill where AI agents look for it

usage: anymd skills <command> [--target claude|agents|all] [--dir PATH] [--force]

  install    write the built-in skill into the target directories
  list       show each target, and whether it holds the current skill
  path       print the target directories, one per line
  uninstall  remove the skill files anymd installed

  --target claude   ~/.claude/skills   (Claude Code)
  --target agents   ~/.agents/skills   (the shared convention)
  --target all      both (the default)
  --dir PATH        install into PATH instead; PATH is the skills ROOT, so the
                    skill lands in PATH/` + skillDirName + `. Use it for a project-local
                    skill directory: anymd skills install --dir .claude/skills
  --force           overwrite (or remove) a file that differs from the built-in
                    skill. Without it a modified file is reported and left.

  The skill is compiled into the binary, so this works from a "go install"
  with no repository checked out. stdout carries only the machine-readable
  output of "path" and "list"; progress and diagnostics go to stderr.
`

// runSkills implements `anymd skills …`, mirroring `anymd config` and
// `anymd cache`: the flags are accepted on either side of the verb.
func runSkills(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("anymd skills", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		target string
		dir    string
		force  bool
	)
	fs.StringVar(&target, "target", "all", "")
	fs.StringVar(&dir, "dir", "", "")
	fs.BoolVar(&force, "force", false, "")
	fs.Usage = func() { fmt.Fprint(stderr, skillsUsage) }

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
			fmt.Fprint(stderr, skillsUsage)
			return exitUsage
		}
	}

	set := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	// --dir names one explicit destination; --target chooses among the
	// well-known ones. Asking for both is a command line that does not mean
	// anything, and guessing which one wins is how the skill lands somewhere
	// nobody looks.
	if set["dir"] && set["target"] {
		fmt.Fprintln(stderr, "anymd: --dir and --target are mutually exclusive")
		return exitUsage
	}
	// A flag that does nothing is worse than a flag that errors: --force on
	// `list` reads as "list and fix", which it is not.
	if set["force"] && verb != "install" && verb != "uninstall" {
		fmt.Fprintf(stderr, "anymd: --force means nothing for `skills %s`\n", verb)
		return exitUsage
	}

	targets, err := skillTargets(target, dir)
	if err != nil {
		fmt.Fprintf(stderr, "anymd: %v\n", err)
		return exitUsage
	}

	switch verb {
	case "install":
		return skillsInstall(targets, force, stderr)
	case "list":
		return skillsList(targets, stdout, stderr)
	case "path":
		for _, t := range targets {
			fmt.Fprintln(stdout, t.dir)
		}
		return exitOK
	case "uninstall":
		return skillsUninstall(targets, force, stderr)
	default:
		fmt.Fprintf(stderr, "anymd: unknown skills command %q\n\n", verb)
		fmt.Fprint(stderr, skillsUsage)
		return exitUsage
	}
}

// ---------------------------------------------------------------------------
// targets
// ---------------------------------------------------------------------------

// skillTarget is one resolved destination. root is the skills directory an
// agent scans; dir is root/anymd, the only place this command ever writes.
type skillTarget struct {
	name string
	dir  string
}

// skillTargets resolves --target / --dir into the destinations to act on.
func skillTargets(target, dir string) ([]skillTarget, error) {
	if strings.TrimSpace(dir) != "" {
		abs, err := filepath.Abs(expandHome(dir))
		if err != nil {
			return nil, err
		}
		abs = filepath.Clean(abs)
		// `--dir ~/.claude/skills/anymd` is the natural typo for a path people
		// have just read out of `skills path`. Treating it as a skills root
		// would bury the skill in .../anymd/anymd, where no agent looks.
		if filepath.Base(abs) == skillDirName {
			return []skillTarget{{name: "dir", dir: abs}}, nil
		}
		return []skillTarget{{name: "dir", dir: filepath.Join(abs, skillDirName)}}, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot find your home directory (%v); pass --dir PATH", err)
	}
	claude := skillTarget{name: "claude", dir: filepath.Join(home, ".claude", "skills", skillDirName)}
	agents := skillTarget{name: "agents", dir: filepath.Join(home, ".agents", "skills", skillDirName)}

	switch target {
	case "", "all":
		return []skillTarget{claude, agents}, nil
	case "claude":
		return []skillTarget{claude}, nil
	case "agents":
		return []skillTarget{agents}, nil
	default:
		return nil, fmt.Errorf("unknown --target %q (want claude, agents or all)", target)
	}
}

// expandHome resolves a leading "~" so `--dir ~/x` works even when the shell
// did not expand it (quoted, or passed through a config file).
func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~"+string(filepath.Separator)) && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p[1:], "/"))
}

// checkSkillDir refuses destinations where a create-and-delete command has no
// business operating. It is the same guard anymd.CheckCacheDir applies, for the
// same reason: `--dir ~` must not be a way to lose your home directory.
func checkSkillDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	abs = filepath.Clean(abs)
	if abs == string(filepath.Separator) || abs == filepath.VolumeName(abs)+string(filepath.Separator) {
		return fmt.Errorf("%w: %s is the filesystem root", errUnsafeSkillDir, abs)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && filepath.Clean(home) == abs {
		return fmt.Errorf("%w: %s is your home directory", errUnsafeSkillDir, abs)
	}
	rest := strings.TrimPrefix(abs, filepath.VolumeName(abs))
	segs := 0
	for _, part := range strings.Split(rest, string(filepath.Separator)) {
		if part != "" {
			segs++
		}
	}
	if segs < 2 {
		return fmt.Errorf("%w: %s is too close to the filesystem root", errUnsafeSkillDir, abs)
	}
	return nil
}

// ---------------------------------------------------------------------------
// the embedded skill
// ---------------------------------------------------------------------------

// skillFile is one embedded file: a path relative to the installed skill
// directory, and its bytes.
type skillFile struct {
	rel  string
	data []byte
}

// embeddedSkill reads the skill out of the binary, in a stable order.
func embeddedSkill() ([]skillFile, error) {
	var out []skillFile
	err := iofs.WalkDir(skillsdata.FS, skillsdata.Root, func(p string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(skillsdata.Root, path.Clean(p))
		if err != nil {
			return err
		}
		b, err := skillsdata.FS.ReadFile(p)
		if err != nil {
			return err
		}
		out = append(out, skillFile{rel: filepath.ToSlash(rel), data: b})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	if len(out) == 0 {
		// Unreachable with a correct build, but a binary that would install an
		// empty directory is worth failing loudly over.
		return nil, errors.New("no skill files are embedded in this build")
	}
	return out, nil
}

// destFor joins a target directory and an embedded relative path, refusing any
// result that escapes the target. Embedded paths cannot contain "..", so this
// can only fire on a bug — which is exactly when a delete loop must stop.
func destFor(dir, rel string) (string, error) {
	dst := filepath.Join(dir, filepath.FromSlash(rel))
	if !withinDir(dir, dst) {
		return "", fmt.Errorf("refusing to touch %s: it is outside %s", dst, dir)
	}
	return dst, nil
}

// withinDir reports whether p is dir itself or lies under it.
func withinDir(dir, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}

// ---------------------------------------------------------------------------
// install
// ---------------------------------------------------------------------------

func skillsInstall(targets []skillTarget, force bool, stderr io.Writer) int {
	files, err := embeddedSkill()
	if err != nil {
		fmt.Fprintf(stderr, "anymd: %v\n", err)
		return exitFail
	}

	code := exitOK
	for _, t := range targets {
		if err := checkSkillDir(t.dir); err != nil {
			fmt.Fprintf(stderr, "anymd: %v\n", err)
			code = exitUsage
			continue
		}
		if err := os.MkdirAll(t.dir, skillDirMode); err != nil {
			fmt.Fprintf(stderr, "anymd: %v\n", err)
			code = worse(code, exitFail)
			continue
		}
		for _, f := range files {
			dst, err := destFor(t.dir, f.rel)
			if err != nil {
				fmt.Fprintf(stderr, "anymd: %v\n", err)
				code = worse(code, exitFail)
				continue
			}
			switch existing, err := os.ReadFile(dst); {
			case err == nil && bytes.Equal(existing, f.data):
				fmt.Fprintf(stderr, "anymd: %s is already current\n", dst)
				continue
			case err == nil && !force:
				// Someone may have edited their copy. Overwriting it because
				// they typed "install" a second time is the kind of data loss
				// a tool never gets forgiven for.
				fmt.Fprintf(stderr, "anymd: %s differs from the built-in skill; "+
					"pass --force to overwrite it\n", dst)
				code = worse(code, exitFail)
				continue
			case err != nil && !os.IsNotExist(err):
				fmt.Fprintf(stderr, "anymd: %v\n", err)
				code = worse(code, exitFail)
				continue
			}
			if parent := filepath.Dir(dst); parent != t.dir {
				if err := os.MkdirAll(parent, skillDirMode); err != nil {
					fmt.Fprintf(stderr, "anymd: %v\n", err)
					code = worse(code, exitFail)
					continue
				}
			}
			if err := os.WriteFile(dst, f.data, skillFileMode); err != nil {
				fmt.Fprintf(stderr, "anymd: %v\n", err)
				code = worse(code, exitFail)
				continue
			}
			// WriteFile honours the mode only when it creates the file; an
			// overwrite keeps whatever mode was there.
			if force {
				_ = os.Chmod(dst, skillFileMode)
			}
			fmt.Fprintf(stderr, "anymd: installed %s\n", dst)
		}
	}
	if code == exitOK {
		fmt.Fprintln(stderr, "anymd: restart your agent if it caches its skill list")
	}
	return code
}

// worse keeps the most severe exit code seen. A usage error outranks a failure
// because it says the command line, not the disk, is what needs fixing.
func worse(a, b int) int {
	if a == exitUsage || b == exitUsage {
		return exitUsage
	}
	if a == exitFail || b == exitFail {
		return exitFail
	}
	return exitOK
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

// skillState is what `list` reports per target.
const (
	stateCurrent      = "current"
	stateDiffers      = "differs"
	stateNotInstalled = "not installed"
)

// skillsList answers "why isn't my agent seeing it?" — which is almost always
// "it is not there" or "it is there but it is an old copy".
func skillsList(targets []skillTarget, stdout, stderr io.Writer) int {
	files, err := embeddedSkill()
	if err != nil {
		fmt.Fprintf(stderr, "anymd: %v\n", err)
		return exitFail
	}
	for _, t := range targets {
		// Tab-separated, state first: `anymd skills list | cut -f2` is the
		// path, and the state stays readable at a glance.
		fmt.Fprintf(stdout, "%s\t%s\n", skillState(t, files), t.dir)
	}
	return exitOK
}

func skillState(t skillTarget, files []skillFile) string {
	if st, err := os.Stat(t.dir); err != nil || !st.IsDir() {
		return stateNotInstalled
	}
	present := 0
	for _, f := range files {
		dst, err := destFor(t.dir, f.rel)
		if err != nil {
			return stateDiffers
		}
		existing, err := os.ReadFile(dst)
		if err != nil {
			continue
		}
		present++
		if !bytes.Equal(existing, f.data) {
			return stateDiffers
		}
	}
	switch present {
	case 0:
		return stateNotInstalled
	case len(files):
		return stateCurrent
	default:
		// The directory exists and holds some of the skill: an interrupted
		// install, or an older release with a different file set.
		return stateDiffers
	}
}

// ---------------------------------------------------------------------------
// uninstall
// ---------------------------------------------------------------------------

// skillsUninstall removes only the files this build would have installed, only
// from inside the resolved target directory, and removes the directory itself
// only once nothing else is left in it.
func skillsUninstall(targets []skillTarget, force bool, stderr io.Writer) int {
	files, err := embeddedSkill()
	if err != nil {
		fmt.Fprintf(stderr, "anymd: %v\n", err)
		return exitFail
	}

	code := exitOK
	for _, t := range targets {
		if err := checkSkillDir(t.dir); err != nil {
			fmt.Fprintf(stderr, "anymd: %v\n", err)
			code = exitUsage
			continue
		}
		if st, err := os.Stat(t.dir); err != nil || !st.IsDir() {
			fmt.Fprintf(stderr, "anymd: nothing to remove: %s does not exist\n", t.dir)
			continue
		}
		for _, f := range files {
			dst, err := destFor(t.dir, f.rel)
			if err != nil {
				fmt.Fprintf(stderr, "anymd: %v\n", err)
				code = worse(code, exitFail)
				continue
			}
			existing, err := os.ReadFile(dst)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				fmt.Fprintf(stderr, "anymd: %v\n", err)
				code = worse(code, exitFail)
				continue
			}
			if !bytes.Equal(existing, f.data) && !force {
				fmt.Fprintf(stderr, "anymd: %s differs from the built-in skill; "+
					"pass --force to remove it anyway\n", dst)
				code = worse(code, exitFail)
				continue
			}
			if err := os.Remove(dst); err != nil {
				fmt.Fprintf(stderr, "anymd: %v\n", err)
				code = worse(code, exitFail)
				continue
			}
			fmt.Fprintf(stderr, "anymd: removed %s\n", dst)
		}

		// The directory goes only when it is empty. Anything else in there is
		// something anymd did not write, and removing it is not this command's
		// business.
		leftovers, err := os.ReadDir(t.dir)
		if err != nil {
			fmt.Fprintf(stderr, "anymd: %v\n", err)
			code = worse(code, exitFail)
			continue
		}
		if len(leftovers) == 0 {
			if err := os.Remove(t.dir); err != nil {
				fmt.Fprintf(stderr, "anymd: %v\n", err)
				code = worse(code, exitFail)
				continue
			}
			fmt.Fprintf(stderr, "anymd: removed %s\n", t.dir)
			continue
		}
		names := make([]string, 0, len(leftovers))
		for _, e := range leftovers {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		fmt.Fprintf(stderr, "anymd: kept %s: it still holds %d file(s) anymd did not install (%s)\n",
			t.dir, len(names), strings.Join(names, ", "))
	}
	return code
}
