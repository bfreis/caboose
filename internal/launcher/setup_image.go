package launcher

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/apkobuild"
	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
)

// setupImage asks what the sandbox is built from, as an image profile:
// caboose's packages (apko), the package groups chosen one by one (apko,
// without the defaults), a Dockerfile the user edits (dockerfile, seeded
// with caboose's), or an image of the user's own (ref). The default is
// always what is configured now. A profile of the kind chosen keeps its
// name; another kind's is written as <kind>.default.
//
// A Dockerfile is never rewritten here: one that is there is kept, and
// only a dir without one is seeded. After a change it offers to build; the
// container moves onto the new image at its next creation ('caboose
// restart'), which is never done here, since it ends sessions.
func (a *App) setupImage(p *prompter) error {
	c := a.Cfg
	cur := c.ImageProfile
	p.heading("Image", "What the sandbox is built from.")
	p.say("Now: %s.", a.imageSummary())
	if cur.Kind == config.ImageKindDockerfile && !isFile(filepath.Join(cur.Dir, "Dockerfile")) {
		p.note("%s has no Dockerfile yet.", a.short(cur.Dir))
	}
	p.blank()
	def := 0
	switch {
	case cur.Kind == config.ImageKindApko && !cur.Defaults:
		def = 1
	case cur.Kind == config.ImageKindDockerfile:
		def = 2
	case cur.Kind == config.ImageKindRef:
		def = 3
	}
	i, err := p.choose("Build the sandbox from", []string{
		"caboose's packages (recommended)",
		"Choose package groups",
		"A Dockerfile you edit (for experts)",
		"An image of your own",
	}, def)
	if err != nil {
		return err
	}
	// The profile of the kind chosen: the one in use when it is of that
	// kind, else the kind's default one.
	profile := func(kind string) string {
		if cur.Kind == kind {
			return cur.String()
		}
		return kind + ".default"
	}
	var e config.Edit
	switch i {
	case 0:
		target, err := a.apkoTarget(profile(config.ImageKindApko))
		if err != nil {
			return err
		}
		name := target.String()
		if cur.String() == name && cur.Defaults {
			p.same("Nothing changed")
			return nil
		}
		e = config.Edit{Set: map[string]any{"image": name}, Unset: []string{name + ".defaults"}}
		// The groups' packages go with the defaults; the profile's own stay,
		// whether it is in use now or not.
		own := ownPackages(target.Packages)
		switch {
		case len(own) == 0:
			e.Unset = append(e.Unset, name+".packages")
		case len(own) < len(target.Packages):
			e.Set[name+".packages"] = own
		}
		if len(own) > 0 {
			p.note("Your own packages in [%s] stay: %s.", name, strings.Join(own, ", "))
		}
	case 1:
		target, err := a.apkoTarget(profile(config.ImageKindApko))
		if err != nil {
			return err
		}
		name := target.String()
		pkgs, err := a.chooseGroups(p, target)
		if err != nil {
			return err
		}
		e = config.Edit{Set: map[string]any{"image": name, name + ".defaults": false, name + ".packages": pkgs}}
	case 2:
		name := profile(config.ImageKindDockerfile)
		e = config.Edit{Set: map[string]any{"image": name}, Tables: []string{name}}
	case 3:
		name := profile(config.ImageKindRef)
		ref, err := a.askRef(p, cur)
		if err != nil {
			return err
		}
		e = config.Edit{Set: map[string]any{"image": name, name + ".image": ref}}
	}
	changed, err := a.writeConfig(e)
	if err != nil {
		return err
	}
	if changed {
		if err := a.rereadConfigFile(); err != nil {
			return err
		}
		if err := c.ReadImage(); err != nil {
			return Die("%v", err)
		}
		p.ok("Wrote image = %q to %s", c.ImageProfile.String(), a.short(filepath.Join(c.EnvDir, config.FileName)))
	}
	seeded := false
	if np := c.ImageProfile; np.Kind == config.ImageKindDockerfile {
		df := filepath.Join(np.Dir, "Dockerfile")
		switch _, err := os.Lstat(df); {
		case errors.Is(err, fs.ErrNotExist):
			data, err := assets.Seed(assets.DefaultSections())
			if err != nil {
				return Die("%v", err)
			}
			if err := a.writeImageDir(p, np.Dir, data); err != nil {
				return err
			}
			seeded = true
		case err != nil:
			return Die("reading %s: %v", df, err)
		default:
			p.note("%s is there already, and stays as it is: it is yours.", a.short(df))
		}
	}
	if !changed && !seeded {
		p.same("Nothing changed")
		return nil
	}
	return a.offerBuild(p)
}

// apkoTarget is the apko profile setup writes, name ("apko.default"), as
// config.toml defines it now, whatever kind is in use.
func (a *App) apkoTarget(name string) (config.ImageProfile, error) {
	_, pname, _ := strings.Cut(name, ".")
	target, err := a.Cfg.File.ApkoProfile(pname)
	if err != nil {
		return target, Die("%v", err)
	}
	return target, nil
}

// chooseGroups asks which of caboose's package groups go into target, an
// apko profile, beyond the required one, and returns the packages to
// write: theirs, and any of target's own packages that are in no group,
// which stay. The groups ticked to start with are those target has all of,
// when it lists its packages without the defaults; else the default
// groups.
func (a *App) chooseGroups(p *prompter, target config.ImageProfile) ([]string, error) {
	p.note("What the sandbox requires is always in: %s.", strings.Join(apkobuild.Required(), ", "))
	p.blank()
	var groups []apkobuild.Group
	for _, g := range apkobuild.Groups() {
		if !g.Required {
			groups = append(groups, g)
		}
	}
	titles, on := make([]string, len(groups)), make([]bool, len(groups))
	for i, g := range groups {
		titles[i] = capFirst(g.Title)
		on[i] = g.Default
		if !target.Defaults {
			on[i] = len(g.Packages) > 0
			for _, pkg := range g.Packages {
				on[i] = on[i] && slices.Contains(target.Packages, pkg)
			}
		}
	}
	on, err := p.checklist("What goes into the image?", titles, on)
	if err != nil {
		return nil, err
	}
	pkgs := []string{}
	for i, g := range groups {
		if on[i] {
			pkgs = append(pkgs, g.Packages...)
		}
	}
	for _, pkg := range ownPackages(target.Packages) {
		if !slices.Contains(pkgs, pkg) {
			pkgs = append(pkgs, pkg)
		}
	}
	return pkgs, nil
}

// ownPackages are those of pkgs in none of caboose's groups.
func ownPackages(pkgs []string) []string {
	inGroup := map[string]bool{}
	for _, g := range apkobuild.Groups() {
		for _, pkg := range g.Packages {
			inGroup[pkg] = true
		}
	}
	own := []string{}
	for _, pkg := range pkgs {
		if !inGroup[pkg] {
			own = append(own, pkg)
		}
	}
	return own
}

// askRef asks for the image to build on, until it is one: not empty, and
// not the environment's own image.
func (a *App) askRef(p *prompter, cur config.ImageProfile) (string, error) {
	p.note("Any image the image check passes ('caboose check-image IMAGE' tells): it is pulled when it is not local.")
	def := ""
	if cur.Kind == config.ImageKindRef {
		def = cur.Ref
	}
	for {
		ref, err := p.ask("The image to build on", def)
		if err != nil {
			return "", err
		}
		switch {
		case ref == "":
			p.fail("Name an image, as docker pull takes it: debian:13.7-slim, say.")
		case config.SameImage(ref, a.Cfg.Image):
			p.fail("'%s' is the environment's own image, which caboose builds on the base: name another.", ref)
		case strings.ContainsAny(ref, " \t\n"):
			p.fail("'%s' is not an image reference.", ref)
		default:
			return ref, nil
		}
	}
}

// writeImageDir writes data as dir/Dockerfile, making dir if need be: a
// dockerfile profile's dir. It is outside the data dir, and
// never mounted into the container: the host's to write as it would any
// file of the user's.
func (a *App) writeImageDir(p *prompter, dir string, data []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Die("%v", err)
	}
	f, err := os.CreateTemp(dir, ".Dockerfile-*")
	if err != nil {
		return Die("%v", err)
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(f.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(dir, "Dockerfile"))
	}
	if err != nil {
		return Die("writing %s: %v", filepath.Join(dir, "Dockerfile"), err)
	}
	p.ok("Wrote %s; it is yours to edit from here on", a.short(filepath.Join(dir, "Dockerfile")))
	return nil
}

// offerBuild offers to build the image the answers just changed. A failed
// build is said, and setup goes on: the rest does not need the image.
func (a *App) offerBuild(p *prompter) error {
	exists := a.state() != "absent"
	p.blank()
	build, err := p.yesNo("Build the image now? It takes a few minutes.", true)
	if err != nil {
		return err
	}
	switch {
	case !build:
		p.same("Not built: the next launch that creates the %s builds it, or %s now", a.noun(), p.code("caboose build"))
	case a.build(nil, a.Stderr) != nil:
		p.fail("The build failed (above). Fix what it says, then run %s.", p.code("caboose build"))
		return nil
	default:
		p.ok("Built image '%s'", a.Cfg.Image)
	}
	if exists {
		p.warn("The %s keeps the image it was created from: %s moves it onto the new one, and ends running sessions.", a.noun(), p.code("caboose restart"))
	}
	return nil
}

// lineDiff is a unified-style difference between two texts, line by line:
// removed lines as "-", added as "+", with two lines of context around
// each change and "..." between changes far apart.
func lineDiff(a, b string) string {
	x, y := strings.Split(strings.TrimSuffix(a, "\n"), "\n"), strings.Split(strings.TrimSuffix(b, "\n"), "\n")
	// lcs[i][j]: the longest common subsequence of x[i:] and y[j:].
	lcs := make([][]int, len(x)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	type line struct {
		op   byte
		text string
	}
	var ops []line
	i, j := 0, 0
	for i < len(x) || j < len(y) {
		switch {
		case i < len(x) && j < len(y) && x[i] == y[j]:
			ops = append(ops, line{' ', x[i]})
			i, j = i+1, j+1
		case i < len(x) && (j == len(y) || lcs[i+1][j] >= lcs[i][j+1]):
			ops = append(ops, line{'-', x[i]})
			i++
		default:
			ops = append(ops, line{'+', y[j]})
			j++
		}
	}
	const ctx = 2
	show := make([]bool, len(ops))
	for k, o := range ops {
		if o.op != ' ' {
			for d := max(0, k-ctx); d <= min(len(ops)-1, k+ctx); d++ {
				show[d] = true
			}
		}
	}
	var out strings.Builder
	gap := false
	for k, o := range ops {
		if !show[k] {
			gap = true
			continue
		}
		if gap && out.Len() > 0 {
			out.WriteString("...\n")
		}
		gap = false
		fmt.Fprintf(&out, "%c %s\n", o.op, o.text)
	}
	return out.String()
}
