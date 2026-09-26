package launcher

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"github.com/bfreis/caboose/internal/assets"
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

// SandboxInstructions returns the sandbox CLAUDE.md to install: the
// checkout's tracked copy when there is a checkout, read fresh on every
// launch so an edit reaches the next session with no rebuild, and the copy
// embedded at build time otherwise.
func SandboxInstructions(checkout string) ([]byte, error) {
	if checkout != "" {
		if b, err := os.ReadFile(filepath.Join(checkout, assets.SandboxInstructionsPath)); err == nil {
			return b, nil
		}
	}
	return assets.SandboxInstructions()
}
