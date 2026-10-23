package launcher

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/datadir"
	"github.com/bfreis/caboose/internal/docker"
	"github.com/bfreis/caboose/internal/sandboxcfg"
	"golang.org/x/sys/unix"
)

// UnresolvedPath is a [checkpoints] path with nothing to copy, and why: it
// is skipped, never an error that stops the others.
type UnresolvedPath struct {
	Path   string
	Reason string
}

// checkpointSources resolves [checkpoints] paths to what a checkpoint
// copies, afresh at every call: the roots, the running sandbox's mounts
// and the sandbox config change under it, and the sandbox writes some of
// them.
//
// A container path is copied when it is, or is inside, a part of a root
// the sandbox can write by either account: the configuration's (a root's
// writable list), or the running sandbox's (its mounts and the writable
// parts its label records; a sandbox without that label wrote every root
// it mounts). So a part made writable is copied before the restart that
// mounts it so, and a part made read-only is still copied while the
// sandbox can write it. Its host directory is the root's, physical.
//
// A home path ~/REL is copied when REL is, or is inside, a keep entry of
// the sandbox config (read through nofollow, Required entries included)
// or one the sandbox has mounted. The sandbox config only decides which
// paths qualify: the host directory is always the data dir's home/REL,
// computed here, so nothing the sandbox writes can point a checkpoint
// outside the data dir.
//
// Whatever the configuration, a source that is, holds or is inside any
// environment's checkpoint store or a daemon directory is refused, and so
// is one that is missing or reached through a symlink (the copy refuses
// that too, as it opens each component).
func (a *App) checkpointSources() ([]checkpoint.Source, []UnresolvedPath) {
	if len(a.Cfg.Checkpoints) == 0 {
		return nil, nil
	}
	var srcs []checkpoint.Source
	var bad []UnresolvedPath
	unresolved := func(p, format string, args ...any) {
		bad = append(bad, UnresolvedPath{Path: p, Reason: fmt.Sprintf(format, args...)})
	}
	guard, gerr := a.checkpointGuard()
	roots := a.checkpointRoots()
	keeps, kerr := a.checkpointKeeps()
	for _, p := range a.Cfg.Checkpoints {
		if gerr != nil {
			unresolved(p, "%v", gerr)
			continue
		}
		var s checkpoint.Source
		if rel, ok := strings.CutPrefix(p, "~/"); ok {
			if kerr != nil {
				unresolved(p, "%v", kerr)
				continue
			}
			var reason string
			if s, reason = a.homeSource(p, rel, keeps); reason != "" {
				unresolved(p, "%s", reason)
				continue
			}
		} else {
			var ok bool
			if s, ok = rootSource(p, roots); !ok {
				unresolved(p, "%s is not a writable part of a root; checkpoints copy only what the sandbox can write", p)
				continue
			}
		}
		if reason := checkSourcePath(&s); reason != "" {
			unresolved(p, "%s %s", p, reason)
			continue
		}
		if reason := guard.refuses(s.Host()); reason != "" {
			unresolved(p, "%s (%s) %s; a checkpoint never copies it", p, s.Host(), reason)
			continue
		}
		srcs = append(srcs, s)
	}
	return srcs, bad
}

// checkpointRoot is a root a checkpoint path can be in: its container
// path, its writable parts, and its host path, physical.
type checkpointRoot struct {
	container string
	writable  []string
	host      string
}

// checkpointRoots are the roots the running sandbox has mounted, each with
// the writable parts its label says (all of it, without the label), then
// the configured ones: the union of what the sandbox can write now and
// what it will after a restart. A root whose host directory cannot be
// resolved is left out.
func (a *App) checkpointRoots() []checkpointRoot {
	var out []checkpointRoot
	add := func(roots []config.Root) {
		for _, r := range roots {
			host, err := config.Physical(r.Host)
			if err != nil {
				continue
			}
			out = append(out, checkpointRoot{container: r.Container, writable: r.Writable, host: host})
		}
	}
	add(a.mountedRoots())
	add(a.Cfg.Roots)
	return out
}

// rootSource is container path p's source: under the first of roots that
// holds p in a writable part.
func rootSource(p string, roots []checkpointRoot) (checkpoint.Source, bool) {
	for _, r := range roots {
		if r.container == "" || !config.Within(p, r.container) {
			continue
		}
		rel := ""
		if p != r.container {
			rel = strings.TrimPrefix(p, strings.TrimSuffix(r.container, "/")+"/")
		}
		if !inWritable(rel, r.writable) {
			continue
		}
		return checkpoint.Source{Path: p, Kind: checkpoint.SourceRoot, Top: r.host, Rel: rel}, true
	}
	return checkpoint.Source{}, false
}

// inWritable reports whether rel, relative to a root ("" for the root
// itself), is or is inside one of its writable parts.
func inWritable(rel string, parts []string) bool {
	for _, w := range parts {
		if w == "." || rel == w || strings.HasPrefix(rel, w+"/") {
			return true
		}
	}
	return false
}

// checkpointKeep is a kept path of the home, relative to it.
type checkpointKeep struct {
	rel  string
	file bool // a file entry
	// known says whether file is known: false for a path only the
	// sandbox's mounts name, which say nothing of what it is.
	known bool
}

// checkpointKeeps are the sandbox config's keep entries (Required ones
// included), then the home paths the sandbox has mounted from the data
// dir. The sandbox config is read anew, through nofollow, every time.
func (a *App) checkpointKeeps() ([]checkpointKeep, error) {
	sb, err := datadir.LoadSandboxConfig(a.Cfg.DataDir, a.rootPaths())
	if err != nil {
		return nil, fmt.Errorf("the sandbox config cannot be read: %v", err)
	}
	var out []checkpointKeep
	for _, k := range sb.Keep {
		out = append(out, checkpointKeep{rel: k.Rel, file: k.File, known: true})
	}
	if rels, ok := a.mountedKeeps(); ok {
		for _, r := range rels {
			out = append(out, checkpointKeep{rel: r})
		}
	}
	return out, nil
}

// homeSource is home path p (~/rel)'s source, or why it has none.
func (a *App) homeSource(p, rel string, keeps []checkpointKeep) (checkpoint.Source, string) {
	if slices.Contains(sandboxcfg.Denied, rel) {
		return checkpoint.Source{}, p + " is the sandbox's login, which a checkpoint never copies"
	}
	for _, k := range keeps {
		if rel != k.rel && !strings.HasPrefix(rel, k.rel+"/") {
			continue
		}
		top, err := config.Physical(a.Cfg.DataDir)
		if err != nil {
			return checkpoint.Source{}, fmt.Sprintf("the data dir %s: %v", a.Cfg.DataDir, err)
		}
		s := checkpoint.Source{Path: p, Kind: checkpoint.SourceHome, Top: top, Rel: path.Join(datadir.HomeDir, rel)}
		if rel == k.rel {
			if k.known {
				s.File = k.file
			} else if fi, err := os.Lstat(s.Host()); err == nil && fi.Mode().IsRegular() {
				s.File = true
			}
		}
		return s, ""
	}
	return checkpoint.Source{}, p + " is not kept by the sandbox config"
}

// checkSourcePath looks at s's path one component at a time from its top,
// following nothing, and says what is wrong with it: "" when it is there
// and of its kind (a directory, or a regular file for a file source),
// with no symlink on the way. The copy opens it the same way, so this
// only says early, and in doctor, what a checkpoint would refuse.
func checkSourcePath(s *checkpoint.Source) string {
	fi, err := os.Lstat(s.Top)
	if err != nil {
		return fmt.Sprintf("cannot be reached: %v", err)
	}
	cur := s.Top
	if s.Rel != "" {
		for _, comp := range strings.Split(s.Rel, "/") {
			cur = filepath.Join(cur, comp)
			if fi, err = os.Lstat(cur); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return fmt.Sprintf("does not exist (no %s)", cur)
				}
				return fmt.Sprintf("cannot be reached: %v", err)
			}
			if fi.Mode()&fs.ModeSymlink != 0 {
				return fmt.Sprintf("runs through a symbolic link, %s; checkpoints never follow one", cur)
			}
			if cur != s.Host() && !fi.IsDir() {
				return fmt.Sprintf("runs through %s, which is not a directory", cur)
			}
		}
	}
	switch {
	case s.File && !fi.Mode().IsRegular():
		return "is not a regular file"
	case !s.File && !fi.IsDir():
		return "is not a directory"
	}
	return ""
}

// fileID is a file's identity: what decides whether two names are one
// directory, whatever symlinks, bind mounts, firmlinks or a
// case-insensitive volume make of their spelling.
type fileID struct{ dev, ino uint64 }

func statID(p string) (fileID, error) {
	var st unix.Stat_t
	if err := unix.Stat(p, &st); err != nil {
		return fileID{}, err
	}
	return fileID{uint64(st.Dev), st.Ino}, nil
}

// checkpointGuard holds what no checkpoint may copy: every environment's
// checkpoint store and the daemon directories, by identity. holds are the
// directories that are, or hold, one of them (each one's nearest existing
// directory and all its ancestors: a store not made yet is where its
// environment's directory is); inside are the ones that exist.
type checkpointGuard struct {
	holds  map[fileID]string
	inside map[fileID]string
}

// checkpointGuard is the guard for every environment under CABOOSE_HOME.
// An error is one it could not look at, and makes every path unresolved:
// failing closed.
func (a *App) checkpointGuard() (*checkpointGuard, error) {
	g := &checkpointGuard{holds: map[fileID]string{}, inside: map[fileID]string{}}
	home := a.Cfg.CabooseHome
	if home == "" {
		return nil, errors.New("CABOOSE_HOME is not set, so there is no checkpoint store")
	}
	names, err := a.envNames()
	if err != nil {
		return nil, fmt.Errorf("listing the environments: %v", err)
	}
	type dir struct{ path, what string }
	var protect []dir
	for _, n := range names {
		x := checkpoint.StoreDir(config.EnvDirFor(home, n))
		protect = append(protect, dir{x, "the checkpoint store " + x})
	}
	for _, x := range daemon.Dirs(home) {
		protect = append(protect, dir{x, "the daemon directory " + x})
	}
	for _, d := range protect {
		x, what := d.path, d.what
		id, err := statID(x)
		switch {
		case err == nil:
			g.inside[id] = what
		case !errors.Is(err, unix.ENOENT):
			return nil, fmt.Errorf("%s: %v", x, err)
		}
		// x, or the nearest directory of its that exists, and everything
		// above it.
		p := x
		for {
			phys, err := filepath.EvalSymlinks(p)
			if err == nil {
				p = phys
				break
			}
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("%s: %v", p, err)
			}
			if filepath.Dir(p) == p {
				break
			}
			p = filepath.Dir(p)
		}
		for ; ; p = filepath.Dir(p) {
			id, err := statID(p)
			if err != nil {
				return nil, fmt.Errorf("%s: %v", p, err)
			}
			if _, ok := g.holds[id]; !ok {
				g.holds[id] = what
			}
			if filepath.Dir(p) == p {
				break
			}
		}
	}
	return g, nil
}

// refuses says why host path p, physical and with no symlink in it (as
// checkSourcePath leaves a source's), may not be copied: "" when it may.
func (g *checkpointGuard) refuses(p string) string {
	id, err := statID(p)
	if err != nil {
		return fmt.Sprintf("cannot be looked at: %v", err)
	}
	if what, ok := g.holds[id]; ok {
		return "is or holds " + what
	}
	for q := filepath.Dir(p); ; q = filepath.Dir(q) {
		id, err := statID(q)
		if err != nil {
			return fmt.Sprintf("cannot be looked at: %v", err)
		}
		if what, ok := g.inside[id]; ok {
			return "is inside " + what
		}
		if filepath.Dir(q) == q {
			return ""
		}
	}
}

// envApp makes the App of another environment under the same
// CABOOSE_HOME, as the daemon does: its config.toml, read without this
// command's CABOOSE_DATA_DIR or CABOOSE_ENV, its sandbox on this App's
// docker.
func (a *App) envApp(env string) (*App, error) {
	getenv := a.Cfg.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	cfg, err := config.Load(DaemonGetenv(getenv), config.OSFS{}, env)
	if err != nil {
		return nil, err
	}
	d := a.Docker
	if d == nil {
		d = &docker.CLI{Path: "docker", Stderr: io.Discard}
	}
	return &App{Cfg: cfg, Docker: d, Stdout: io.Discard, Stderr: io.Discard, Checkout: a.Checkout}, nil
}

// checkpointReach is every host directory a sandbox of any environment
// under CABOOSE_HOME reaches, or will at its next start: each
// environment's configured roots, its data dir's home/ (the keep entries
// are mounted from inside it), and every mount of its sandbox when one
// exists. An environment with no directory there is not asked
// (envsWithDir). apps makes another environment's App (nil: envApp). It fails,
// rather than leave an environment out, when an environment's config
// cannot be read or its sandbox exists but cannot say what it mounts: the
// store refuses every take, prune and restore until then.
func (a *App) checkpointReach(apps func(env string) (*App, error)) ([]checkpoint.Reach, error) {
	if apps == nil {
		apps = a.envApp
	}
	names, err := a.envsWithDir()
	if err != nil {
		return nil, fmt.Errorf("listing the environments: %v", err)
	}
	var reach []checkpoint.Reach
	for _, n := range names {
		b := a
		if n != a.Cfg.Env {
			if b, err = apps(n); err != nil {
				return nil, fmt.Errorf("environment %s: %v", n, err)
			}
		}
		for _, r := range b.Cfg.Roots {
			reach = append(reach, checkpoint.Reach{Path: r.Host, What: fmt.Sprintf("root %s of environment %s", r.Container, n)})
		}
		if b.Cfg.DataDir != "" {
			reach = append(reach, checkpoint.Reach{Path: filepath.Join(b.Cfg.DataDir, datadir.HomeDir), What: "the kept home of environment " + n})
		}
		mounts, err := sandboxMounts(b.box())
		if err != nil {
			return nil, fmt.Errorf("the sandbox of environment %s cannot say what it mounts: %v", n, err)
		}
		for _, m := range mounts {
			reach = append(reach, checkpoint.Reach{Path: m.Source, What: fmt.Sprintf("mount %s of environment %s's sandbox", m.Target, n)})
			if dir := vmShareDir(b, m); dir != "" {
				reach = append(reach, checkpoint.Reach{Path: dir, What: fmt.Sprintf("the directory shared for mount %s of environment %s's VM", m.Target, n)})
			}
		}
	}
	return reach, nil
}

// vmShareDir is the directory a VM is given for its mount m when that is
// more than m's source, or "": a virtio-fs share is a directory, so a
// file is shared with its whole directory, which the guest's root can
// mount whole. Only under vm; a source that is not there, or not a file,
// is shared as it is.
func vmShareDir(b *App, m backend.Mount) string {
	if !b.isVM() || !filepath.IsAbs(m.Source) {
		return ""
	}
	if fi, err := os.Stat(m.Source); err != nil || fi.IsDir() {
		return ""
	}
	return filepath.Dir(filepath.Clean(m.Source))
}

// sandboxMounts are what box mounts: none when it does not exist, and an
// error when it may exist but cannot say. A failed Mounts alone does not
// tell the two apart: docker's inspect fails alike for a missing container
// and an engine that did not answer it, and State calls both "absent".
// So for a container, the engine is asked again: one that does not answer
// at all runs no container (nothing of its can reach the store); one that
// answers and lists no container by the name has none; any other answer
// is an error, and the check fails closed.
func sandboxMounts(box backend.Backend) ([]backend.Mount, error) {
	mounts, err := box.Mounts()
	if err == nil {
		return mounts, nil
	}
	d, ok := box.(*backend.Docker)
	if !ok {
		if box.State() == "absent" {
			return nil, nil
		}
		return nil, err
	}
	if d.CLI.Quiet("version") != nil {
		return nil, nil
	}
	out, lerr := d.CLI.Output("container", "ls", "--all", "--quiet", "--filter", "name=^/"+regexp.QuoteMeta(d.Name)+"$")
	switch {
	case lerr != nil:
		return nil, fmt.Errorf("%v; listing containers: %v", err, lerr)
	case strings.TrimSpace(out) == "":
		return nil, nil
	}
	return nil, err
}

// checkpointExposed is the store's Exposed check: s refuses every take,
// prune and restore while any sandbox of any environment can reach it
// (Store.CheckReach, by identity), with what reaches it computed afresh
// each time.
func (a *App) checkpointExposed(s *checkpoint.Store, apps func(env string) (*App, error)) func() error {
	return func() error {
		reach, err := a.checkpointReach(apps)
		if err != nil {
			return fmt.Errorf("cannot tell whether a sandbox can reach the checkpoint store %s: %v", s.Dir(), err)
		}
		return s.CheckReach(reach)
	}
}

// openCheckpointStore opens this environment's checkpoint store with its
// exposed-store check set.
func (a *App) openCheckpointStore(apps func(env string) (*App, error)) (*checkpoint.Store, error) {
	s, err := checkpoint.Open(a.Cfg.CabooseHome, a.Cfg.EnvDir)
	if err != nil {
		return nil, err
	}
	s.Exposed = a.checkpointExposed(s, apps)
	return s, nil
}
