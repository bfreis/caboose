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

// managedPath is where a file of sandbox/skills goes, relative to
// datadir.ManagedDir.
func managedPath(rel string) string { return ".claude/skills/" + rel }

// modPath is where a file of sandbox/mods goes, relative to
// datadir.ManagedDir.
func modPath(rel string) string { return "mods/" + rel }

// EmbeddedSandboxFiles is the CLAUDE.md, skills and mods built into this
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
	if err := readEmbedded(skills, managedPath, files); err != nil {
		return nil, err
	}
	mods, err := assets.SandboxMods()
	if err != nil {
		return nil, err
	}
	if err := readEmbedded(mods, modPath, files); err != nil {
		return nil, err
	}
	return files, nil
}

// readEmbedded adds every file of fsys to files, at place(its path).
func readEmbedded(fsys fs.FS, place func(string) string, files map[string][]byte) error {
	return fs.WalkDir(fsys, ".", func(p string, e fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return err
		}
		if e.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		files[place(p)] = b
		return err
	})
}

// checkoutLimits are what is left of the limits on what is read from a
// checkout, shared by its skills and mods.
type checkoutLimits struct {
	bytes, files, entries int
}

// SandboxFiles is the CLAUDE.md, skills and mods to install, by path
// relative to datadir.ManagedDir: the checkout's when there is one, else
// the embedded ones. The checkout's CLAUDE.md that cannot be used for any
// reason but being absent, or its skills or mods tree (when sandbox/skills
// or sandbox/mods exists there) that has a link, a file that is not plain,
// or more than the limits allow, is an error: the caller's cue to install
// the embedded set, all of it, since the two never mix.
func SandboxFiles(checkout string) (map[string][]byte, error) {
	files, err := EmbeddedSandboxFiles()
	if err != nil || checkout == "" {
		return files, err
	}
	d := nofollow.Dir(checkout)
	lim := checkoutLimits{bytes: maxSkillBytes, files: maxSkillFiles, entries: maxSkillEntries}
	b, _, err := d.ReadFileMax(assets.SandboxInstructionsPath, maxSkillBytes)
	switch {
	case err == nil:
		files["CLAUDE.md"] = b
		lim.bytes -= len(b)
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	// What go:embed leaves out of sandbox/skills (a name that starts with
	// . or _) the checkout's set leaves out too, so it is the embedded one.
	// A mod is embedded with all:, a plugin folder's manifest being in
	// .claude-plugin, so none is left out there.
	for _, t := range []struct {
		root, prefix string
		place        func(string) string
		hidden       bool
	}{
		{assets.SandboxSkillsPath, ".claude/skills/", managedPath, false},
		{assets.SandboxModsPath, "mods/", modPath, true},
	} {
		if _, err := d.Lstat(t.root); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		for k := range files {
			if strings.HasPrefix(k, t.prefix) {
				delete(files, k)
			}
		}
		if err := readCheckoutTree(d, t.root, t.place, t.hidden, &lim, files); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// readCheckoutTree reads the files under root of the checkout d into files
// at place(path under root), within lim.
func readCheckoutTree(d nofollow.Dir, root string, place func(string) string, hidden bool, lim *checkoutLimits, files map[string][]byte) error {
	return d.Walk(root, func(rel string, e fs.DirEntry) error {
		if lim.entries--; lim.entries < 0 {
			return fmt.Errorf("%s has more than %d entries", root, maxSkillEntries)
		}
		if strings.Count(rel, "/")-strings.Count(root, "/") > maxSkillDepth {
			return fmt.Errorf("%s: deeper than %d levels", rel, maxSkillDepth)
		}
		if n := e.Name(); !hidden && (strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_")) {
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
		if lim.files--; lim.files < 0 {
			return fmt.Errorf("%s has more than %d files", root, maxSkillFiles)
		}
		b, _, err := d.ReadFileMax(rel, int64(lim.bytes))
		if err != nil {
			return err
		}
		lim.bytes -= len(b)
		files[place(strings.TrimPrefix(rel, root+"/"))] = b
		return nil
	})
}
