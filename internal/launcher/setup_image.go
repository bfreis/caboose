package launcher

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
)

// setupImage asks what the sandbox is built on: the Dockerfile embedded in
// caboose (which changes with caboose), the environment's own
// image/Dockerfile (written from a preset, then the user's), or an image
// of the user's own ([image] base). The default is always what is there now.
//
// A Dockerfile written here is never rewritten unasked: replacing one
// shows how it differs from the fresh preset and asks first, and the other
// files in image/ are left alone. After a change it offers to build; the
// container moves onto the new image at its next creation ('caboose
// restart'), which is never done here, since it ends sessions.
func (a *App) setupImage(p *prompter) error {
	c := a.Cfg
	p.heading("Image", "What the sandbox is built on.")
	if err := c.CheckImages(); errors.Is(err, config.ErrTwoBases) {
		return Die("%v", err)
	}
	dir := filepath.Join(c.EnvDir, config.ImageDirName)
	var unsetBase bool
	switch {
	case c.BaseImage != "":
		p.say("Now: your own image, '%s' (base in [image]).", c.BaseImage)
		p.blank()
		keep, err := p.yesNo(fmt.Sprintf("Keep building on '%s'?", c.BaseImage), true)
		if err != nil {
			return err
		}
		if keep {
			p.same("Nothing changed")
			return nil
		}
		unsetBase = true
		fallthrough
	case c.ImageDir == "":
		var data []byte
		var err error
		if !unsetBase {
			p.say("Now: the Dockerfile built into caboose, which is Ubuntu with")
			p.bullets(sectionTitles(assets.DefaultSections()))
			p.blank()
			p.note("It changes as caboose does. A Dockerfile of the environment's own is yours to edit, and never changes by itself.")
			p.blank()
		}
		choices := []string{
			"Keep building from the Dockerfile built into caboose",
			"Write caboose's default Dockerfile into " + a.short(dir) + "/, to edit",
			"Choose what goes in, and write that into " + a.short(dir) + "/",
		}
		if unsetBase {
			choices[0] = "Build from the Dockerfile built into caboose"
		}
		i, err := p.choose("Build the sandbox from", choices, 0)
		if err != nil {
			return err
		}
		switch i {
		case 0:
			if !unsetBase {
				p.same("Nothing changed")
				return nil
			}
		case 1:
			data, err = assets.Preset(assets.DefaultSections())
		case 2:
			data, err = a.choosePreset(p, assets.DefaultSections())
		}
		if err != nil {
			return err
		}
		if unsetBase {
			if _, err := a.writeConfig(config.Edit{Unset: []string{"image.base"}}); err != nil {
				return err
			}
			p.ok("Removed base from [image] in %s", a.short(filepath.Join(c.EnvDir, config.FileName)))
			c.BaseImage = ""
		}
		if data != nil {
			if err := a.writeImageDir(p, dir, data); err != nil {
				return err
			}
		}
	default:
		changed, err := a.replaceDockerfile(p, dir)
		if err != nil || !changed {
			return err
		}
	}
	return a.offerBuild(p)
}

// replaceDockerfile shows the environment's image/Dockerfile and offers a
// fresh preset in its place: shown as a difference, and asked for again.
func (a *App) replaceDockerfile(p *prompter, dir string) (bool, error) {
	path := filepath.Join(dir, "Dockerfile")
	cur, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, Die("reading %s: %v", path, err)
	}
	defaults := assets.DefaultSections()
	switch h, ok := assets.ReadHeader(cur); {
	case cur == nil:
		p.say("Now: %s/, which has no Dockerfile.", a.short(dir))
	case ok:
		p.say("Now: %s, written from caboose's preset %s, with", a.short(path), h.ID)
		p.bullets(sectionTitles(h.Sections))
		if h.ID != assets.PresetID() {
			p.blank()
			p.warn("caboose's preset has changed since: it is %s now.", assets.PresetID())
		}
		defaults = h.Sections
	default:
		p.say("Now: %s, your own (it names no preset).", a.short(path))
	}
	p.blank()
	i, err := p.choose("The environment's Dockerfile", []string{
		"Keep it as it is",
		"Replace it with a fresh preset (you see the difference first)",
	}, 0)
	if err != nil {
		return false, err
	}
	if i == 0 {
		p.same("Nothing changed")
		return false, nil
	}
	data, err := a.choosePreset(p, defaults)
	if err != nil {
		return false, err
	}
	if string(data) == string(cur) {
		p.same("The fresh preset is what %s has already; nothing changed", a.short(path))
		return false, nil
	}
	p.blank()
	p.diff(a.short(path), "the fresh preset", lineDiff(string(cur), string(data)))
	p.blank()
	ok, err := p.yesNo("Replace "+a.short(path)+" with it?", false)
	if err != nil || !ok {
		if err == nil {
			p.same("Nothing changed")
		}
		return false, err
	}
	return true, a.writeImageDir(p, dir, data)
}

// choosePreset asks, section by section, what goes into the image, with
// defaults (section names) checked, and returns the Dockerfile.
func (a *App) choosePreset(p *prompter, defaults []string) ([]byte, error) {
	p.note("The base is always in: Ubuntu, and what caboose needs (git, curl, tmux, ripgrep, ...).")
	p.blank()
	all := assets.Sections()
	titles, on := make([]string, len(all)), make([]bool, len(all))
	for i, s := range all {
		titles[i], on[i] = capFirst(s.Title), slices.Contains(defaults, s.Name)
	}
	on, err := p.checklist("What else goes into the image?", titles, on)
	if err != nil {
		return nil, err
	}
	var names []string
	for i, s := range all {
		if on[i] {
			names = append(names, s.Name)
		}
	}
	return assets.Preset(names)
}

// writeImageDir writes data as dir/Dockerfile, making dir if need be, and
// points this run's configuration at it. dir is outside the data dir, and
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
	a.Cfg.ImageDir = dir
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

// sectionTitles lists the titles of the named sections, for a list.
func sectionTitles(names []string) []string {
	var titles []string
	for _, s := range assets.Sections() {
		if slices.Contains(names, s.Name) {
			titles = append(titles, capFirst(s.Title))
		}
	}
	if len(titles) == 0 {
		return []string{"nothing optional"}
	}
	return titles
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
