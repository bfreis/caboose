package launcher

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/nofollow"
)

// ModulePath identifies a caboose checkout by its go.mod.
const ModulePath = "github.com/bfreis/caboose"

// IsCheckout reports whether dir is a caboose checkout: its go.mod declares
// this module and it has the tracked sandbox/CLAUDE.md.
func IsCheckout(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, assets.SandboxInstructionsPath))
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	f, err := os.Open(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "module" {
			return strings.Trim(fields[1], `"`) == ModulePath
		}
	}
	return false
}

// FindCheckout locates the caboose checkout this binary belongs to, or
// returns "".
//
// buildTime is a path stamped in with -ldflags "-X main.sourceDir=...", and
// wins while it still holds a checkout. Otherwise the executable is resolved
// through symlinks (e.g. ~/.local/bin/caboose -> the build output) and its
// directory and that directory's parent are tried, which covers both a
// binary built at the repo root and one built into bin/.
//
// "" is the normal case for most users, not a degraded one: a release or
// `go install` binary has no checkout anywhere near it, and uses the files
// embedded at build time instead.
func FindCheckout(buildTime string) string {
	var candidates []string
	if buildTime != "" {
		candidates = append(candidates, buildTime)
	}
	if exe, err := os.Executable(); err == nil {
		if exe, err = filepath.EvalSymlinks(exe); err == nil {
			d := filepath.Dir(exe)
			candidates = append(candidates, d, filepath.Dir(d))
		}
	}
	for _, c := range candidates {
		if !IsCheckout(c) {
			continue
		}
		if p, err := filepath.EvalSymlinks(c); err == nil {
			if abs, err := filepath.Abs(p); err == nil {
				return abs
			}
		}
	}
	return ""
}

// Limits on what is read from a checkout, which the sandbox can write: the
// bytes of the CLAUDE.md and the skills in all, the files kept, the entries
// looked at (every directory and skipped file too) and how deep, counting
// the file itself as a level.
const (
	maxSkillBytes   = 1 << 20
	maxSkillFiles   = 64
	maxSkillEntries = 256
	maxSkillDepth   = 4
)

// SandboxInstructions returns the sandbox CLAUDE.md to install: the
// checkout's tracked copy when there is a checkout, read fresh on every
// launch so an edit reaches the next session with no rebuild, and the copy
// embedded at build time otherwise. The checkout is under a root the
// sandbox can write, so nothing in it is read through a link, or past
// maxSkillBytes.
func SandboxInstructions(checkout string) ([]byte, error) {
	if checkout != "" {
		if b, _, err := nofollow.Dir(checkout).ReadFileMax(assets.SandboxInstructionsPath, maxSkillBytes); err == nil {
			return b, nil
		}
	}
	return assets.SandboxInstructions()
}

// managedPath is where a file of sandbox/skills goes, relative to
// datadir.ManagedDir.
func managedPath(rel string) string { return ".claude/skills/" + rel }

// EmbeddedSandboxFiles is the CLAUDE.md and skills built into this
// launcher, by path relative to datadir.ManagedDir.
func EmbeddedSandboxFiles() (map[string][]byte, error) {
	md, err := assets.SandboxInstructions()
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{"CLAUDE.md": md}
	skills, err := assets.SandboxSkills()
	if err != nil {
		return nil, err
	}
	err = fs.WalkDir(skills, ".", func(p string, e fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return err
		}
		if e.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(skills, p)
		files[managedPath(p)] = b
		return err
	})
	return files, err
}

// SandboxFiles is the CLAUDE.md and skills to install, by path relative to
// datadir.ManagedDir: the checkout's when there is one, else the embedded
// ones. The checkout's CLAUDE.md that cannot be used for any reason but
// being absent, or its skills tree (when sandbox/skills exists there) that
// has a link, a file that is not plain, or more than the limits allow, is
// an error: the caller's cue to install the embedded set, all of it, since
// the two never mix.
func SandboxFiles(checkout string) (map[string][]byte, error) {
	files, err := EmbeddedSandboxFiles()
	if err != nil || checkout == "" {
		return files, err
	}
	d := nofollow.Dir(checkout)
	total := 0
	b, _, err := d.ReadFileMax(assets.SandboxInstructionsPath, maxSkillBytes)
	switch {
	case err == nil:
		files["CLAUDE.md"] = b
		total = len(b)
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	if _, err := d.Lstat(assets.SandboxSkillsPath); errors.Is(err, fs.ErrNotExist) {
		return files, nil
	} else if err != nil {
		return nil, err
	}
	for k := range files {
		if k != "CLAUDE.md" {
			delete(files, k)
		}
	}
	entries := 0
	err = d.Walk(assets.SandboxSkillsPath, func(rel string, e fs.DirEntry) error {
		if entries++; entries > maxSkillEntries {
			return fmt.Errorf("%s has more than %d entries", assets.SandboxSkillsPath, maxSkillEntries)
		}
		if strings.Count(rel, "/")-strings.Count(assets.SandboxSkillsPath, "/") > maxSkillDepth {
			return fmt.Errorf("%s: deeper than %d levels", rel, maxSkillDepth)
		}
		// What go:embed leaves out, so the checkout's set is the embedded one.
		if n := e.Name(); strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_") {
			if e.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if e.IsDir() {
			return nil
		}
		if !e.Type().IsRegular() {
			return fmt.Errorf("%s: %w", rel, nofollow.ErrNotPlain)
		}
		if len(files)-1 >= maxSkillFiles {
			return fmt.Errorf("%s has more than %d files", assets.SandboxSkillsPath, maxSkillFiles)
		}
		b, _, err := d.ReadFileMax(rel, int64(maxSkillBytes-total))
		if err != nil {
			return err
		}
		total += len(b)
		files[managedPath(strings.TrimPrefix(rel, assets.SandboxSkillsPath+"/"))] = b
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}
