package launcher

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/config"
)

// rootMounts are the mounts that give the sandbox roots: each root
// read-only, unless its writable list is ["."], and each writable part of
// it mounted read-write over it, at the part's own path under the root's
// container path, so every file keeps the container path it has with the
// whole root mounted (Claude Code's project keys and caboose sync depend
// on it). Parents come first: a backend that mounts in the order given
// (the vm guest does) would otherwise cover a part with its root.
//
// It takes the roots and nothing else: no grant or answer of the
// sandbox's reaches what is mounted writable, which only config.toml's
// writable lists decide.
func rootMounts(roots []config.Root) []backend.Mount {
	var ms []backend.Mount
	for _, r := range roots {
		ms = append(ms, backend.Mount{Source: r.Host, Target: r.Container, ReadOnly: !r.WholeWritable()})
		if r.WholeWritable() {
			continue
		}
		hosts, ctrs := r.WritableHost(), r.WritableContainer()
		for i := range hosts {
			ms = append(ms, backend.Mount{Source: hosts[i], Target: ctrs[i]})
		}
	}
	slices.SortStableFunc(ms, func(a, b backend.Mount) int { return depth(a.Target) - depth(b.Target) })
	return ms
}

// depth is how many components container path p has.
func depth(p string) int { return strings.Count(strings.TrimSuffix(p, "/"), "/") }

// writableLabel is LabelWritable's value for roots: each root's container
// path and its writable parts as written, sorted, [] for none. Every root
// is in it, so a root it does not name is one it cannot speak for.
func writableLabel(roots []config.Root) string {
	m := map[string][]string{}
	for _, r := range roots {
		parts := slices.Clone(r.Writable)
		if parts == nil {
			parts = []string{}
		}
		slices.Sort(parts)
		m[r.Container] = parts
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// parseWritableLabel is writableLabel read back; ok is false when v is
// not one, which is no answer at all.
func parseWritableLabel(v string) (map[string][]string, bool) {
	var m map[string][]string
	if json.Unmarshal([]byte(v), &m) != nil || m == nil {
		return nil, false
	}
	for ctr, parts := range m {
		if config.CheckWritable(parts) != nil || config.CheckContainerPath(ctr) != nil {
			return nil, false
		}
	}
	return m, true
}

// mountedWritable is the writable list of the mounted root at container
// path ctr, from the sandbox's labels: what it records, or ["."] when it
// records nothing it can be read by. That fails closed: a root whose
// writable parts cannot be told counts as wholly writable.
func mountedWritable(labels map[string]string, ctr string) []string {
	v, ok := labels[assets.LabelWritable]
	if !ok {
		return []string{"."}
	}
	m, ok := parseWritableLabel(v)
	if !ok {
		return []string{"."}
	}
	parts, ok := m[ctr]
	if !ok {
		return []string{"."}
	}
	if len(parts) == 0 {
		return nil
	}
	return parts
}

// describeAccess is what the sandbox may write of r, for a message:
// "read-only", "writable", or "read-only but a, b".
func describeAccess(r config.Root) string {
	switch {
	case r.WholeWritable():
		return "writable"
	case len(r.Writable) == 0:
		return "read-only"
	}
	return "read-only but " + strings.Join(r.Writable, ", ")
}

// accessList is roots as "host at container (access), ...".
func accessList(roots []config.Root) string {
	var parts []string
	for _, r := range roots {
		parts = append(parts, fmt.Sprintf("%s at %s (%s)", r.Host, r.Container, describeAccess(r)))
	}
	return strings.Join(parts, ", ")
}

// writableDrift says how what the sandbox may write of its roots differs
// from what the configuration says, or "" when it does not, or when the
// roots themselves differ (warnIfRootsDrifted says that) or there is no
// sandbox: what the running sandbox has of each root that changed, and
// what config.toml now makes of it, as "the running VM has /work/dev
// read-only; config.toml now makes /work/dev/p writable". Changing it
// takes a restart, which every caller says: the mounts are fixed when the
// sandbox is created.
func (a *App) writableDrift() string {
	mounted := a.mountedRoots()
	if len(mounted) == 0 || !config.SameRoots(mounted, a.Cfg.Roots) {
		return ""
	}
	if d := a.missingLabel(assets.LabelWritable, "writable parts"); d != "" {
		return d
	}
	if config.SameWritable(mounted, a.Cfg.Roots) {
		return ""
	}
	var have, change []string
	for _, m := range mounted {
		for _, c := range a.Cfg.Roots {
			if c.Container != m.Container || config.SameWritable([]config.Root{m}, []config.Root{c}) {
				continue
			}
			have = append(have, rootAccess(m))
			change = append(change, writableChange(m, c))
		}
	}
	if len(have) == 0 {
		return ""
	}
	return fmt.Sprintf("the running %s has %s; config.toml now makes %s", a.noun(), strings.Join(have, ", "), strings.Join(change, ", and "))
}

// rootAccess is what the sandbox may write of r, by container path:
// "/work writable", "/work read-only", "/work read-only except /work/a".
func rootAccess(r config.Root) string {
	switch {
	case r.WholeWritable():
		return r.Container + " writable"
	case len(r.Writable) == 0:
		return r.Container + " read-only"
	}
	return r.Container + " read-only except " + strings.Join(r.WritableContainer(), ", ")
}

// writableChange is what want makes of a root the sandbox has as have,
// for "config.toml now makes ...": the whole root's access when either
// side is the whole root, else the parts it adds and the parts it drops.
func writableChange(have, want config.Root) string {
	switch {
	case want.WholeWritable():
		return "all of " + want.Container + " writable"
	case len(want.Writable) == 0:
		return "all of " + want.Container + " read-only"
	case have.WholeWritable():
		return rootAccess(want)
	}
	hw, ww := have.WritableContainer(), want.WritableContainer()
	var added, dropped []string
	for _, w := range ww {
		if !slices.Contains(hw, w) {
			added = append(added, w)
		}
	}
	for _, h := range hw {
		if !slices.Contains(ww, h) {
			dropped = append(dropped, h)
		}
	}
	var out []string
	if len(added) > 0 {
		out = append(out, strings.Join(added, ", ")+" writable")
	}
	if len(dropped) > 0 {
		out = append(out, strings.Join(dropped, ", ")+" read-only")
	}
	return strings.Join(out, " and ")
}

// warnIfWritableDrifted is warnIfRootsDrifted for the writable parts.
func (a *App) warnIfWritableDrifted() {
	if d := a.writableDrift(); d != "" {
		a.Note("%s.", d)
		a.Note("'caboose restart' applies it, remounting the roots (this kills running sessions).")
	}
}

// noteReadOnlyRoots says, in one line, when the sandbox can write no part
// of any root: nothing in it can change a project, which is safe, and
// surprising the first time.
func (a *App) noteReadOnlyRoots() {
	if a.inSetup {
		return
	}
	for _, r := range a.visibleRoots() {
		if len(r.Writable) > 0 {
			return
		}
	}
	a.Note("every root is read-only to the %s, so nothing in it can change your projects: list the parts it may write in writable ('%s' asks)",
		a.noun(), SetupCommand(a.Cfg.Env, "roots"))
}

// otherWritable is every host path a sandbox of another environment
// under CABOOSE_HOME can write, or will at its next start, physical: what
// its configured roots would mount writable (config.toml's, sandbox or
// not, as configuredPaths counts them for a restore), and every mount of
// its sandbox when it has one now (writableOf). This environment's own
// are the overlap of its roots, which ResolveRoots refuses. An
// environment whose engine cannot be asked counts by its configured
// parts alone (its sandbox is not running then); one whose config.toml
// does not load is in unknown, as "ENV: why".
func (a *App) otherWritable() (paths, unknown []string) {
	names, err := a.envsWithDir()
	if err != nil {
		return nil, []string{fmt.Sprintf("the environments: %v", err)}
	}
	add := func(p string) {
		if p != "" && !slices.Contains(paths, p) {
			paths = append(paths, p)
		}
	}
	for _, n := range names {
		if n == a.Cfg.Env {
			continue
		}
		app, err := a.envApp(n)
		if err != nil {
			unknown = append(unknown, fmt.Sprintf("%s: its config.toml does not load (%v); fix it, or remove the environment", n, err))
			continue
		}
		for _, m := range rootMounts(app.Cfg.Roots) {
			if !m.ReadOnly {
				add(physicalOr(m.Source))
			}
		}
		if b, ok, err := a.sandboxBox(n); err == nil && ok {
			// A sandbox there whose mounts cannot be read may still
			// mount more than its config.toml now says: refuse.
			live, err := writableOf(b)
			if err != nil {
				unknown = append(unknown, fmt.Sprintf("%s: its sandbox's mounts cannot be read (%v)", n, err))
				continue
			}
			for _, p := range live {
				add(p)
			}
		}
	}
	return paths, unknown
}

// strictlyHolds reports whether w holds p, by identity, and is not p
// itself: a sandbox can swap what lies inside its writable mount, never
// the mount point itself, so a root another environment mounts writable
// whole, at the same directory, is shared, not exposed.
func strictlyHolds(w, p string) bool {
	if !config.HoldsDir(w, p) || filepath.Clean(w) == filepath.Clean(p) {
		return false
	}
	wi, werr := os.Stat(w)
	pi, perr := os.Stat(p)
	return werr != nil || perr != nil || !os.SameFile(wi, pi)
}

// rootInWritable says why root r, resolved, cannot be mounted when it
// lies inside one of writable (strictlyHolds), or a symlink met on the
// way to it lies in one, by identity: the sandbox that writes there can
// swap it for a symlink to anywhere (~/.ssh, say) before the mount is
// made. A root that is one of writable is shared, and fine. "" when none
// of that is so.
func rootInWritable(r config.Root, writable []string) string {
	for _, w := range writable {
		if w != "" && strictlyHolds(w, r.Host) {
			return fmt.Sprintf("is in %s, which another environment's sandbox can write, and so could put a symbolic link to anywhere in its place; name a directory no sandbox writes", w)
		}
	}
	return config.LinkInPaths(r, writable)
}

// checkRootReach refuses, just before a sandbox is created, a root that
// lies, or is reached through a symlink that lies, where another
// environment's sandbox can write (otherWritable). An environment whose
// config.toml does not load refuses the launch, failing closed, naming it.
func (a *App) checkRootReach() error {
	if len(a.Cfg.Roots) == 0 {
		return nil
	}
	ws, unknown := a.otherWritable()
	if len(unknown) > 0 {
		return Die("what another environment's sandbox can write, where the roots might lie, cannot be told: %s (its directory is under %s)", strings.Join(unknown, "; "), filepath.Join(a.Cfg.CabooseHome, "envs"))
	}
	for _, r := range a.Cfg.Roots {
		if why := rootInWritable(r, ws); why != "" {
			return Die("root %s, %s, %s (%s).", r.Name, r.Host, why, a.Cfg.RootsOrigin())
		}
	}
	return nil
}
