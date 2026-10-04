package launcher

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/proposal"
)

// checkProposedRoot checks a proposed root on the host. host is its
// physical path, what would be mounted and written; refusals say why it
// cannot be a root at all, warnings what the user should know before
// saying yes.
//
// Every check is on the physical path: a proposed path through a symlink
// is judged, and mounted, as where it leads now, and a symlink the sandbox
// could change later (one inside a root) does not decide it.
func (a *App) checkProposedRoot(r proposal.Root) (host string, refusals, warnings []string) {
	c := a.Cfg
	shown := proposal.Printable(r.Path)
	full := config.ExpandTilde(r.Path, c.Home)
	if !filepath.IsAbs(full) {
		return "", []string{fmt.Sprintf("The root %s is not an absolute path, or one starting with ~/.", shown)}, nil
	}
	host, err := config.Physical(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", []string{fmt.Sprintf("The root %s does not exist on this machine.", shown)}, nil
		}
		return "", []string{fmt.Sprintf("The root %s cannot be looked at: %v", shown, err)}, nil
	}
	if fi, err := os.Stat(host); err != nil || !fi.IsDir() {
		return "", []string{fmt.Sprintf("The root %s is not a directory.", shown)}, nil
	}
	refuse := func(format string, args ...any) { refusals = append(refusals, fmt.Sprintf(format, args...)) }

	home := physicalOr(c.Home)
	switch {
	case host == "/":
		refuse("The root %s is the whole disk.", shown)
	case home != "" && config.Within(home, host):
		refuse("The root %s (%s) is your home directory, or holds it: it would hand the sandbox your keys, tokens and everything else in it.", shown, host)
	case home != "" && config.Within(host, home):
		first, _, _ := strings.Cut(strings.TrimPrefix(host, home+"/"), "/")
		if strings.HasPrefix(first, ".") || first == "Library" {
			refuse("The root %s (%s) is in ~/%s, where tools keep their settings and credentials.", shown, host, first)
		}
	}
	for _, own := range []struct{ what, dir string }{
		{"caboose's home", c.CabooseHome},
		{"this environment", c.EnvDir},
		{"the data dir", c.DataDir},
	} {
		p := physicalOr(own.dir)
		if p != "" && (config.Within(host, p) || config.Within(p, host)) {
			refuse("The root %s (%s) overlaps %s, %s: the sandbox could change its own configuration, and read the Claude login.", shown, host, own.what, p)
			break
		}
	}
	for _, o := range a.fileRoots() {
		if o.Name == r.Name {
			refuse("There is a root named %s already, at %s.", r.Name, o.Path)
		}
		if oh := a.hostPath(o.Path); config.Within(host, oh) || config.Within(oh, host) {
			refuse("The root %s (%s) overlaps the root %s: a project under both would have two paths in the sandbox.", shown, host, o.Path)
		}
	}
	for _, m := range a.mountedRoots() {
		if mh := physicalOr(m.Host); mh != "" && (config.Within(host, mh) || config.Within(mh, host)) && !a.configuredRoot(mh) {
			refuse("The root %s (%s) overlaps %s, which the %s mounts at %s.", shown, host, m.Host, a.noun(), m.Container)
		}
	}
	if len(refusals) > 0 {
		return host, refusals, nil
	}

	found, more := sensitiveIn(host)
	if len(found) > 0 {
		list := strings.Join(found, ", ")
		if more {
			list += ", and more"
		}
		warnings = append(warnings, fmt.Sprintf("%s holds what looks like credentials or keys: %s. The sandbox could read them all.", host, list))
	}
	return host, nil, warnings
}

// configuredRoot reports whether host is one of config.toml's roots, which
// the overlap check against them has already said.
func (a *App) configuredRoot(host string) bool {
	for _, o := range a.fileRoots() {
		if a.hostPath(o.Path) == host {
			return true
		}
	}
	return false
}

// physicalOr is p's physical path, else p cleaned; "" for "".
func physicalOr(p string) string {
	if p == "" {
		return ""
	}
	if phys, err := config.Physical(p); err == nil {
		return phys
	}
	return filepath.Clean(p)
}

// sensitive names what a root should not hand the sandbox unremarked:
// directories and files that hold credentials or keys.
var sensitive = map[string]bool{
	".ssh": true, ".aws": true, ".gnupg": true, ".kube": true, ".docker": true, ".azure": true,
	".config": true, ".netrc": true, ".npmrc": true, ".pypirc": true, ".git-credentials": true,
	".vault-token": true, "Keychains": true, "credentials.json": true,
	"id_rsa": true, "id_ecdsa": true, "id_ed25519": true, "id_dsa": true,
}

// sensitiveSuffixes are the file endings sensitive by themselves.
var sensitiveSuffixes = []string{".pem", ".key", ".p12", ".pfx", ".kdbx", ".keychain-db"}

// sensitiveIn looks under dir, a few levels down, for what sensitive
// names, and a .env file; more is set when it found more than it lists, or
// stopped before looking everywhere. It never follows a symlink, and skips
// what only holds code (.git, node_modules).
func sensitiveIn(dir string) (found []string, more bool) {
	const maxDepth, maxEntries, maxFound = 4, 20000, 6
	seen := 0
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if seen++; seen > maxEntries {
			more = true
			return filepath.SkipAll
		}
		name := d.Name()
		hit := sensitive[name] || name == ".env" || strings.HasPrefix(name, ".env.")
		for _, s := range sensitiveSuffixes {
			hit = hit || !d.IsDir() && strings.HasSuffix(name, s)
		}
		if hit {
			if len(found) == maxFound {
				more = true
				return filepath.SkipAll
			}
			found = append(found, proposal.Printable(rel))
			if d.IsDir() {
				return filepath.SkipDir
			}
		}
		if d.IsDir() && (name == ".git" || name == "node_modules" || strings.Count(rel, string(filepath.Separator)) >= maxDepth-1) {
			return filepath.SkipDir
		}
		return nil
	})
	return found, more
}
