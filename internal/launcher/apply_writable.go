package launcher

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/proposal"
)

// A proposed writable part is a path in the sandbox that a session would
// write: a directory of a root config.toml has, which the sandbox then
// mounts writable at its next restart. It has a refusal list of its own,
// judged on this machine's facts, before anything is shown as a question.
// The path is mapped to the host through the roots the sandbox mounts
// (config.toml's when there is none), never through anything the sandbox
// says of the host, and must be a real directory under a configured root,
// reached through no symbolic link; it must not be or hold the home, be in
// a hidden directory of it, or be in, be or hold caboose's own state (by
// path and by identity); and it must not be writable already.

// writablePlan is a proposed writable part as it would be applied.
type writablePlan struct {
	// container is the path in the sandbox, as proposed; host its
	// physical path on this machine.
	container, host string
	// root is the config.toml root that holds it, and part the path
	// relative to it ("." for all of it).
	root setupRoot
	part string
	// dropped are the root's writable parts the new one holds, which it
	// replaces.
	dropped []string
	edit    config.Edit
	diff    string
}

// checkProposedWritable checks a proposed writable part on the host:
// refusals are why it cannot be made writable at all, warnings what to
// know before saying yes. The plan is nil when there are refusals.
func (a *App) checkProposedWritable(w proposal.Writable) (pl *writablePlan, refusals, warnings []string) {
	c := a.Cfg
	shown := proposal.Printable(w.Path)
	refuse := func(format string, args ...any) { refusals = append(refusals, fmt.Sprintf(format, args...)) }

	// The sandbox's path, mapped to this machine's through what the
	// sandbox mounts: the session names what it sees.
	var via []config.Root
	if mounted := a.mountedRoots(); len(mounted) > 0 {
		for _, m := range mounted {
			via = append(via, config.Root{Host: physicalOr(m.Host), Container: m.Container})
		}
	} else {
		for _, r := range a.fileRoots() {
			via = append(via, config.Root{Name: r.Name, Host: a.hostPath(r.Path), Container: r.container()})
		}
	}
	host, ok := config.HostPath(via, w.Path)
	if !ok {
		return nil, []string{fmt.Sprintf("The path %s is not inside a root the %s mounts.", shown, a.noun())}, nil
	}
	// And the root config.toml has there: what the edit changes. Roots
	// that overlap on the host are refused at every launch; were they
	// not, the deepest is the one whose part it is.
	var root *setupRoot
	var rootHost string
	for _, r := range a.fileRoots() {
		if rh := a.hostPath(r.Path); config.Within(host, rh) && len(rh) > len(rootHost) {
			r := r
			root, rootHost = &r, rh
		}
	}
	if root == nil {
		return nil, []string{fmt.Sprintf("The path %s (%s) is not inside a root this environment has in config.toml.", shown, proposal.Printable(host))}, nil
	}
	part := "."
	if host != rootHost {
		part = strings.TrimPrefix(host, rootHost+"/")
	}
	if err := config.CheckWritable([]string{part}); err != nil {
		return nil, []string{fmt.Sprintf("The path %s cannot be a writable part of the root %s: %s.", shown, root.Name, proposal.Printable(err.Error()))}, nil
	}
	if fi, err := os.Lstat(rootHost); err != nil || !fi.IsDir() {
		return nil, []string{fmt.Sprintf("The root %s, %s, is not a directory on this machine.", root.Name, proposal.Printable(rootHost))}, nil
	}
	if at, problem := config.WritablePartProblem(rootHost, part); problem != "" {
		return nil, []string{fmt.Sprintf("The path %s cannot be made writable: %s %s. A writable part must be a real directory under the root, with no symbolic link on the way.",
			shown, proposal.Printable(at), problem)}, nil
	}
	if phys, err := config.Physical(host); err != nil || phys != host {
		return nil, []string{fmt.Sprintf("The path %s (%s) is not where it leads on this machine: a writable part must be reached through no symbolic link.", shown, proposal.Printable(host))}, nil
	}

	hostShown := proposal.Printable(host)
	home := physicalOr(c.Home)
	switch {
	case c.Home != "" && config.HoldsDir(host, c.Home):
		refuse("The path %s (%s) is your home directory, or holds it: the sandbox could change your keys, tokens and everything else in it.", shown, hostShown)
	case home != "" && config.Within(host, home) && host != home:
		first, _, _ := strings.Cut(strings.TrimPrefix(host, home+"/"), "/")
		if strings.HasPrefix(first, ".") || strings.EqualFold(first, "Library") {
			refuse("The path %s (%s) is in ~/%s, where tools keep their settings and credentials.", shown, hostShown, proposal.Printable(first))
		} else if a.hiddenInHome(host) {
			refuse("The path %s (%s) is in a hidden directory of your home, where tools keep their settings and credentials.", shown, hostShown)
		}
	}
	// The home reached under another name (a bind mount, a firmlink, a
	// directory in it mounted from the home itself), which no comparison
	// of paths sees.
	if name, ok := a.homeEntryOf(host); ok && len(refusals) == 0 {
		refuse("The path %s (%s) is in ~/%s, where tools keep their settings and credentials.", shown, hostShown, proposal.Printable(name))
	}
	// caboose's own state, by path and by identity: a second name for a
	// directory above it, or for the path itself, is the same directory.
	for _, own := range []struct{ what, dir string }{
		{"caboose's home", c.CabooseHome},
		{"this environment's directory", c.EnvDir},
		{"the data dir", c.DataDir},
	} {
		if own.dir == "" {
			continue
		}
		if config.HoldsDir(host, own.dir) || config.HoldsDir(own.dir, host) {
			refuse("The path %s (%s) is in, is or holds %s, %s: the sandbox could change its own configuration, and read the Claude login.",
				shown, hostShown, own.what, proposal.Printable(physicalOr(own.dir)))
			break
		}
	}
	switch {
	case slices.Contains(root.Writable, "."):
		refuse("The path %s is writable already: the whole root %s is.", shown, root.Name)
	default:
		for _, e := range root.Writable {
			if config.Within(part, e) {
				refuse("The path %s is writable already: the root %s's writable part %s holds it.", shown, root.Name, proposal.Printable(e))
				break
			}
		}
	}
	if len(refusals) > 0 {
		return nil, refusals, nil
	}

	pl = &writablePlan{container: w.Path, host: host, root: *root, part: part}
	next := []string{"."}
	if part != "." {
		next = nil
		for _, e := range root.Writable {
			if config.Within(e, part) {
				pl.dropped = append(pl.dropped, e)
				continue
			}
			next = append(next, e)
		}
		next = append(next, part)
	} else {
		pl.dropped = slices.Clone(root.Writable)
	}
	roots := a.fileRoots()
	for i := range roots {
		if roots[i].Name == root.Name {
			roots[i].Writable = next
		}
	}
	edit := rootsEdit(roots)
	if _, err := config.FileRoots(edit.Roots, c.Home); err != nil {
		return nil, []string{fmt.Sprintf("The roots cannot take it: %s.", proposal.Printable(err.Error()))}, nil
	}
	d, err := a.configDiff(edit)
	if err != nil {
		return nil, []string{fmt.Sprintf("It cannot be written into config.toml safely: %s.", proposal.Printable(err.Error()))}, nil
	}
	pl.edit, pl.diff = edit, d

	if len(pl.dropped) > 0 {
		warnings = append(warnings, fmt.Sprintf("It holds what the root %s lets the sandbox write now (%s), which it replaces in the list.",
			root.Name, proposal.Printable(strings.Join(pl.dropped, ", "))))
	}
	if found, more := sensitiveIn(host); len(found) > 0 {
		list := strings.Join(found, ", ")
		if more {
			list += ", and more"
		}
		warnings = append(warnings, fmt.Sprintf("%s holds what looks like credentials or keys: %s. The sandbox could change or delete them.", hostShown, list))
	}
	return pl, nil, warnings
}

// homeEntryOf finds, by identity, a hidden directory of the home or its
// Library that p is in: the name of the first ancestor of p (p itself
// included) whose parent is the home, by device and inode, and whose name
// starts with "." or is Library in any case.
func (a *App) homeEntryOf(p string) (string, bool) {
	if a.Cfg.Home == "" {
		return "", false
	}
	homeFI, err := os.Stat(a.Cfg.Home)
	if err != nil {
		return "", false
	}
	for cur := filepath.Clean(p); ; cur = filepath.Dir(cur) {
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", false
		}
		if fi, err := os.Stat(parent); err == nil && os.SameFile(fi, homeFI) {
			name := filepath.Base(cur)
			if strings.HasPrefix(name, ".") || strings.EqualFold(name, "Library") {
				return name, true
			}
			return "", false
		}
	}
}

// writableSummary is what applying pl lets the sandbox do, in plain
// words; the host path in it is what the view's hash covers.
func (a *App) writableSummary(pl *writablePlan) string {
	return fmt.Sprintf("make %s writable, from the sandbox's next restart: a sandbox can then change or delete anything under %s (writable part %s of the root %s)",
		proposal.Printable(pl.container), proposal.Printable(a.short(pl.host)), proposal.Printable(pl.part), pl.root.Name)
}

// showWritable shows what applying pl changes.
func (a *App) showWritable(p *prompter, pl *writablePlan) {
	p.say("Let the sandbox write a part of the root %s ([roots], writable):", pl.root.Name)
	p.table([][2]string{{proposal.Printable(pl.container), "← " + proposal.Printable(pl.host)}})
	p.blank()
	p.diff(a.short(filepath.Join(a.Cfg.EnvDir, config.FileName)), "with the change", pl.diff)
	p.blank()
	p.warn("A sandbox can then change or delete anything under %s. It takes effect at the %s's next restart.", proposal.Printable(pl.host), a.noun())
}

// applyWritable asks for and writes pl: the root's name typed out, then
// config.toml written, provided its change is still the one shown. ok is
// false when it was not written.
func (a *App) applyWritable(p *prompter, pl *writablePlan) (ok bool, err error) {
	name := pl.root.Name
	typed, err := p.ask(fmt.Sprintf("To let the sandbox write %s, type the root's name, %s", proposal.Printable(pl.host), name), "")
	if err != nil {
		return false, err
	}
	if typed != name {
		p.fail("That is not %s: nothing was applied, and the proposal is left pending", name)
		return false, nil
	}
	if a.beforeProposalWrite != nil {
		a.beforeProposalWrite()
	}
	// Planned again from what is there now: a directory swapped for a
	// link, or config.toml edited, since it was shown is not written.
	now, refusals, _ := a.checkProposedWritable(proposal.Writable{Path: pl.container})
	switch {
	case len(refusals) > 0:
		for _, r := range refusals {
			p.fail("%s", r)
		}
		p.fail("It changed since it was shown: nothing was applied, and the proposal is left pending (apply shows it again)")
		return false, nil
	case now.host != pl.host:
		p.fail("%s is %s now, not %s: nothing was applied, and the proposal is left pending (apply shows it again)",
			proposal.Printable(pl.container), proposal.Printable(now.host), proposal.Printable(pl.host))
		return false, nil
	case now.diff != pl.diff:
		p.fail("config.toml changed since it was shown: nothing was applied, and the proposal is left pending (apply shows it again)")
		return false, nil
	}
	if _, err := a.writeConfig(pl.edit); err != nil {
		return false, err
	}
	if err := a.rereadConfigFile(); err != nil {
		return false, err
	}
	p.ok("Wrote the roots to %s: %s is writable from the %s's next restart", a.short(filepath.Join(a.Cfg.EnvDir, config.FileName)), proposal.Printable(pl.container), a.noun())
	return true, nil
}
