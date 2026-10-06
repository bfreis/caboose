package launcher

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/config"
)

// setupRoot is one root as setup edits it: Path is the host path as
// written in config.toml (~ and all), Container the long form's path in
// the sandbox, "" for /work/<Name>.
type setupRoot struct{ Name, Path, Container string }

// container is where the container mounts r.
func (r setupRoot) container() string {
	if r.Container != "" {
		return r.Container
	}
	return config.WorkDir + "/" + r.Name
}

// fileRoots are the roots config.toml sets, else the default: the file is
// what setup edits.
func (a *App) fileRoots() []setupRoot {
	if f := a.Cfg.File; f != nil && len(f.Roots) > 0 {
		var roots []setupRoot
		for _, name := range slices.Sorted(maps.Keys(f.Roots)) {
			roots = append(roots, setupRoot{name, f.Roots[name].Host, f.Roots[name].Path})
		}
		return roots
	}
	return []setupRoot{{Name: config.DefaultRootName, Path: "~/" + config.DefaultRootName}}
}

// rootsEdit is the edit that writes roots as config.toml's [roots].
func rootsEdit(roots []setupRoot) config.Edit {
	e := config.Edit{SetRoots: true, Roots: map[string]config.FileRoot{}}
	for _, r := range roots {
		e.Roots[r.Name] = config.FileRoot{Host: r.Path, Path: r.Container}
	}
	return e
}

// setupRoots asks for the roots: the directories whose projects the
// container sees, each at /work/<name> or the path its long form names. The
// current ones are edited in a loop -- change one, add one, remove one --
// until they are taken as they are, which needs every one to exist and
// none to hold another.
//
// A change that moves projects to another container path is said, and
// confirmed: Claude Code keeps each project's state under its path in the
// container, so after it the state for the old path is not found. Nothing
// is moved or deleted.
func (a *App) setupRoots(p *prompter) error {
	c := a.Cfg
	cur := a.fileRoots()
	p.heading("Roots", "The directories that hold your projects. The sandbox mounts each at /work/NAME.")
	roots := slices.Clone(cur)
	for first := true; ; first = false {
		if !first {
			p.blank()
		}
		rows := make([][2]string, len(roots))
		for i, r := range roots {
			rows[i] = [2]string{r.container(), "← " + r.Path}
		}
		p.table(rows)
		problems := a.rootProblems(roots)
		for _, pr := range problems {
			p.fail("%s", pr)
		}
		p.blank()
		done := false
		type action struct {
			label string
			do    func() error
		}
		var acts []action
		if len(problems) == 0 {
			acts = append(acts, action{"Use these", func() error { done = true; return nil }})
		}
		for i := range roots {
			acts = append(acts, action{"Change " + rootLabel(roots, i), func() error { return a.changeRoot(p, roots, i) }})
		}
		acts = append(acts, action{"Add another root", func() error {
			var err error
			roots, err = a.addRoot(p, roots)
			return err
		}})
		if len(roots) > 1 {
			for i := range roots {
				acts = append(acts, action{"Remove " + rootLabel(roots, i), func() error {
					roots = slices.Delete(slices.Clone(roots), i, i+1)
					return nil
				}})
			}
		}
		labels := make([]string, len(acts))
		for i, act := range acts {
			labels[i] = act.label
		}
		question := "Use these roots?"
		if len(problems) > 0 {
			question = "These roots cannot be used as they are. Fix them:"
		}
		i, err := p.choose(question, labels, 0)
		if err != nil {
			return err
		}
		if err := acts[i].do(); err != nil {
			return err
		}
		if done {
			break
		}
	}

	if slices.Equal(roots, cur) {
		p.same("Nothing changed")
		return nil
	}
	if moves := a.rootMoves(cur, roots); len(moves) > 0 {
		for _, m := range moves {
			p.warn("%s", capFirst(m))
		}
		p.note("Claude Code keeps each project's state (memories, history, settings) under its path in the sandbox, " +
			"so it will not find what it has under the old one. Nothing is moved or deleted.")
		ok, err := p.yesNo("Change the roots anyway?", false)
		if err != nil {
			return err
		}
		if !ok {
			p.same("Nothing changed")
			return nil
		}
	}

	if _, err := a.writeConfig(rootsEdit(roots)); err != nil {
		return err
	}
	p.ok("Wrote them to %s", a.short(filepath.Join(c.EnvDir, config.FileName)))
	if a.state() != "absent" {
		p.warn("The %s keeps the roots it was created with: %s remounts them, and ends running sessions.", a.noun(), p.code("caboose restart"))
	}
	return nil
}

// rootLabel names roots[i] in a menu.
func rootLabel(roots []setupRoot, i int) string {
	return roots[i].Name + " (" + roots[i].Path + ")"
}

// hostPath is a root's path as the host sees it: ~ expanded, and physical
// when it exists.
func (a *App) hostPath(p string) string {
	p = config.ExpandTilde(p, a.Cfg.Home)
	if phys, err := config.Physical(p); err == nil {
		return phys
	}
	return filepath.Clean(p)
}

// rootProblems is what keeps roots from being used: a path that is not an
// existing directory, roots that hold one another, or container paths
// config.FileRoots refuses.
func (a *App) rootProblems(roots []setupRoot) []string {
	var out []string
	for _, r := range roots {
		if why := a.badRootPath(r.Path); why != "" {
			out = append(out, why)
		}
	}
	if _, err := config.FileRoots(rootsEdit(roots).Roots, a.Cfg.Home); err != nil && len(out) == 0 {
		out = append(out, err.Error())
	}
	for i, r := range roots {
		for _, o := range roots[i+1:] {
			if hr, ho := a.hostPath(r.Path), a.hostPath(o.Path); config.Within(hr, ho) || config.Within(ho, hr) {
				out = append(out, fmt.Sprintf("%s and %s overlap: a project under both would have two paths in the sandbox", r.Path, o.Path))
			}
		}
	}
	return out
}

// badRootPath says why p cannot be a root, or "".
func (a *App) badRootPath(p string) string {
	full := config.ExpandTilde(p, a.Cfg.Home)
	if !filepath.IsAbs(full) {
		return p + " is not an absolute path (or one starting with ~/)"
	}
	fi, err := os.Stat(full)
	switch {
	case os.IsNotExist(err):
		return p + " does not exist"
	case err != nil:
		return fmt.Sprintf("%s: %v", p, err)
	case !fi.IsDir():
		return p + " is not a directory"
	}
	return ""
}

// askRootPath asks for a root's directory until it is one; "" (an empty
// answer with no default) is none.
func (a *App) askRootPath(p *prompter, def string) (string, error) {
	for {
		path, err := p.ask("Directory (an absolute path, or one starting with ~/)", def)
		if err != nil || path == "" {
			return "", err
		}
		if why := a.badRootPath(path); why != "" {
			p.fail("%s", why)
			if path == def {
				def = ""
			}
			continue
		}
		return path, nil
	}
}

// askRootName asks for a root's name, the directory under /work, until it
// is a valid one no other root in roots (but skip) has, nor mounts at.
func (a *App) askRootName(p *prompter, path string, roots []setupRoot, skip int) (string, error) {
	def := rootNameFor(path)
	for {
		name, err := p.ask("Name for "+path+", mounted at /work/NAME", def)
		if err != nil {
			return "", err
		}
		taken := false
		for i, r := range roots {
			taken = taken || i != skip && (r.Name == name || r.container() == config.WorkDir+"/"+name)
		}
		switch {
		case !config.ValidRootName(name):
			p.fail("'%s' is not a root name: lowercase letters, digits, - and _, starting with a letter or digit, at most 32", name)
		case taken:
			p.fail("Another root is named '%s'", name)
		default:
			return name, nil
		}
		if name == def {
			def = ""
		}
	}
}

// rootNameFor suggests a root's name from its directory's.
func rootNameFor(path string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(filepath.Base(filepath.Clean(path))) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.TrimLeft(b.String(), "-_")
	if len(name) > 32 {
		name = name[:32]
	}
	if !config.ValidRootName(name) {
		return ""
	}
	return name
}

// changeRoot asks for roots[i]'s new directory, in place.
func (a *App) changeRoot(p *prompter, roots []setupRoot, i int) error {
	path, err := a.askRootPath(p, roots[i].Path)
	if err != nil || path == "" {
		return err
	}
	roots[i].Path = path
	return nil
}

// addRoot asks for another root, at /work/NAME. A sole root at /work
// itself moves to its own /work/NAME, since /work is for a sole root;
// rootMoves says so before anything is written.
func (a *App) addRoot(p *prompter, roots []setupRoot) ([]setupRoot, error) {
	path, err := a.askRootPath(p, "")
	if err != nil || path == "" {
		return roots, err
	}
	roots = slices.Clone(roots)
	if len(roots) == 1 && roots[0].Container == config.WorkDir {
		roots[0].Container = ""
	}
	roots = append(roots, setupRoot{Path: path})
	name, err := a.askRootName(p, path, roots, len(roots)-1)
	if err != nil {
		return nil, err
	}
	roots[len(roots)-1].Name = name
	return roots, nil
}

// rootMoves says what a change from cur to next does to projects that are
// visible now: moved to another container path, or no longer mounted.
func (a *App) rootMoves(cur, next []setupRoot) []string {
	var out []string
	for _, o := range cur {
		host := a.hostPath(o.Path)
		if _, err := os.Stat(host); err != nil {
			continue // not a root anyone could have used
		}
		var mounted []config.Root
		for _, n := range next {
			mounted = append(mounted, config.Root{Name: n.Name, Host: a.hostPath(n.Path), Container: n.container()})
		}
		where, _ := config.ContainerPath(mounted, host)
		switch {
		case where == "":
			out = append(out, fmt.Sprintf("projects under %s are no longer mounted (they were at %s)", o.Path, o.container()))
		case where != o.container():
			out = append(out, fmt.Sprintf("projects under %s move from %s/... to %s/...", o.Path, o.container(), where))
		}
	}
	return out
}
