package launcher

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/hostlink"
	"github.com/bfreis/caboose/internal/proposal"
)

// A proposed alias is a host command the sandbox would then run whenever
// a session asks (or, with policy "ask", whenever a person allows it): it
// has a refusal list of its own, judged on this machine's facts, before
// anything is shown as a question. Its command must be a program here,
// outside everything a sandbox reaches and everything that holds
// caboose's state or the home's credentials, reached through no symlink
// in those places, and no shell or other program that runs what it is
// told; its directory must be outside caboose's state and the home's
// hidden directories. A user's own [alias.NAME] in config.toml is never
// held to this list: it is theirs. Every alias, theirs or proposed, is
// decided again at every run (internal/policy).

// aliasLaunchers are the programs a proposed alias may not run, by base
// name in lower case: shells, and programs that run another program, or
// code, that their arguments name. An alias that ran one would run
// whatever its fixed arguments say, which is a shell command by another
// name.
var aliasLaunchers = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true, "ksh": true, "mksh": true,
	"csh": true, "tcsh": true, "ash": true, "busybox": true, "toybox": true, "pwsh": true,
	"env": true, "sudo": true, "doas": true, "su": true, "osascript": true, "open": true,
	"xargs": true, "nohup": true, "timeout": true, "nice": true, "exec": true, "script": true,
	"expect": true, "python": true, "python3": true, "perl": true, "ruby": true, "node": true,
	"deno": true, "bun": true, "php": true, "lua": true, "tclsh": true, "wish": true,
	"caboose": true, "caboose-agent": true, "ssh": true, "launchctl": true, "setsid": true,
	"stdbuf": true, "chroot": true, "awk": true, "gawk": true, "mawk": true, "nawk": true,
	"find": true, "make": true, "gmake": true, "screen": true, "tmux": true,
	"nodejs": true, "pypy": true, "pypy3": true, "luajit": true, "rscript": true, "julia": true,
	"java": true, "jshell": true, "arch": true, "time": true, "flock": true, "unshare": true,
	"nsenter": true, "systemd-run": true, "sandbox-exec": true, "gdb": true, "lldb": true,
	"vi": true, "vim": true, "nvim": true, "emacs": true, "watch": true, "parallel": true,
}

// versionedLauncher is an interpreter's versioned name: python3.12,
// perl5.36, ruby3.2, php8.3, lua5.4, node22.
var versionedLauncher = regexp.MustCompile(`^(python|pypy|perl|ruby|php|lua|luajit|node|tclsh|wish|java)[0-9][0-9.]*$`)

// isLauncher reports whether a program of base name base is one an alias
// proposal may not run.
func isLauncher(base string) bool {
	b := strings.ToLower(base)
	return aliasLaunchers[b] || versionedLauncher.MatchString(b)
}

// under reports whether p is dir or below it, ignoring case: on a disk
// that ignores it (a Mac's, by default), /Users/U/Dev is ~/dev.
func under(p, dir string) bool {
	if p == "" || dir == "" {
		return false
	}
	return config.Within(strings.ToLower(filepath.Clean(p)), strings.ToLower(filepath.Clean(dir)))
}

// aliasPlace is a place a proposed alias's command or directory may not
// be in, for a refusal.
type aliasPlace struct{ what, dir string }

// aliasForbidden are the places no proposed alias's command or directory
// may be: caboose's own state, and the home's hidden directories (each
// found under the home, physical and as written).
func (a *App) aliasForbidden() []aliasPlace {
	c := a.Cfg
	var out []aliasPlace
	for _, own := range []aliasPlace{
		{"caboose's home", c.CabooseHome},
		{"this environment's directory", c.EnvDir},
		{"its data dir", c.DataDir},
	} {
		if own.dir == "" {
			continue
		}
		out = append(out, own, aliasPlace{own.what, physicalOr(own.dir)})
	}
	return out
}

// hiddenInHome reports whether p is in a hidden directory of the home,
// as written or physical: where tools keep their settings and
// credentials.
func (a *App) hiddenInHome(p string) bool {
	for _, home := range []string{a.Cfg.Home, physicalOr(a.Cfg.Home)} {
		if home == "" || home == "/" || !under(p, home) || strings.EqualFold(filepath.Clean(p), filepath.Clean(home)) {
			continue
		}
		rel := strings.TrimPrefix(strings.ToLower(filepath.Clean(p)), strings.ToLower(filepath.Clean(home))+"/")
		if first, _, _ := strings.Cut(rel, "/"); strings.HasPrefix(first, ".") {
			return true
		}
	}
	return false
}

// rootPlaces are this environment's configured and mounted roots,
// read-only or not, and every environment's writable mounts (writable,
// also among places): no proposed alias's command may be in one. An error
// is that what the sandboxes can write cannot be told.
func (a *App) rootPlaces() (places, writable []aliasPlace, err error) {
	var out []aliasPlace
	for _, r := range a.Cfg.Roots {
		out = append(out, aliasPlace{"the root at " + r.Container, r.Host}, aliasPlace{"the root at " + r.Container, physicalOr(r.Host)})
	}
	for _, r := range a.fileRoots() {
		out = append(out, aliasPlace{"the root " + r.Name, a.hostPath(r.Path)})
	}
	for _, m := range a.mountedRoots() {
		out = append(out, aliasPlace{"the root the " + a.noun() + " mounts at " + m.Container, physicalOr(m.Host)})
	}
	// Every other environment's roots too: a root of any of them is a
	// sandbox's to read, and maybe to write.
	names, err := a.envNames()
	if err != nil {
		return nil, nil, err
	}
	for _, n := range names {
		if n == a.Cfg.Env {
			continue
		}
		if _, err := os.Stat(config.EnvDirFor(a.Cfg.CabooseHome, n)); err != nil {
			continue
		}
		c, err := config.Load(a.Cfg.Getenv, config.OSFS{}, n)
		if err != nil {
			return nil, nil, fmt.Errorf("environment %s: %v", n, err)
		}
		for _, r := range c.Roots {
			out = append(out, aliasPlace{"a root of environment " + n, physicalOr(r.Host)})
		}
	}
	w, err := a.sandboxWritable()
	if err != nil {
		return nil, nil, err
	}
	for _, p := range w {
		writable = append(writable, aliasPlace{"what a sandbox can write", p})
	}
	// And what the next sandbox will write: the writable parts
	// config.toml lists, mounted or not yet.
	for _, r := range a.Cfg.Roots {
		for _, p := range r.WritableHost() {
			writable = append(writable, aliasPlace{"a writable part of the root at " + r.Container, physicalOr(p)})
		}
	}
	return append(out, writable...), writable, nil
}

// checkProposedAlias checks a proposed alias on the host: refusals are why
// it cannot be added at all, warnings what to know before saying yes.
// command is its command's physical path.
func (a *App) checkProposedAlias(al proposal.Alias) (command string, refusals, warnings []string) {
	refuse := func(format string, args ...any) { refusals = append(refusals, fmt.Sprintf(format, args...)) }
	shown := proposal.Printable(al.Command[0])
	cmd, via, err := hostlink.ResolvePath(al.Command[0], false)
	if err != nil {
		return "", []string{fmt.Sprintf("Its command %s does not exist on this machine, or cannot be looked at: %s.", shown, proposal.Printable(err.Error()))}, nil
	}
	if fi, err := os.Stat(cmd); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		refuse("Its command %s (%s) is not an executable file.", shown, proposal.Printable(cmd))
	}
	spellings := append([]string{al.Command[0], cmd}, via...)
	if interp := hostlink.Interpreter(cmd); interp != "" {
		// A script: its interpreter is what runs, and is judged as the
		// command is.
		if ipath, ivia, err := hostlink.ResolvePath(interp, false); err == nil {
			spellings = append(append(spellings, interp, ipath), ivia...)
		} else {
			refuse("Its command %s is a script for %s, which does not exist on this machine.", shown, proposal.Printable(interp))
		}
	}
	for _, s := range spellings {
		if isLauncher(filepath.Base(s)) {
			refuse("Its command %s is %s, a shell or a program that runs what its arguments say: an alias may not run one, since it would run whatever those arguments are.",
				shown, proposal.Printable(filepath.Base(s)))
			break
		}
	}
	places, writable, err := a.rootPlaces()
	if err != nil {
		refuse("What the sandboxes can write cannot be told now (%s), so no alias can be judged: try again once caboose status shows them.", proposal.Printable(err.Error()))
	}
	places = append(places, a.aliasForbidden()...)
	refused := map[string]bool{}
	for _, s := range spellings {
		for _, pl := range places {
			if under(s, pl.dir) && !refused[pl.what] {
				refused[pl.what] = true
				refuse("Its command %s is within %s (%s), which the sandbox, or caboose's own state, is: the sandbox could change what it runs.",
					shown, pl.what, proposal.Printable(pl.dir))
			}
		}
		if a.hiddenInHome(s) && !refused["hidden"] {
			refused["hidden"] = true
			refuse("Its command %s is in a hidden directory of your home, where tools keep their settings and credentials.", shown)
		}
	}

	if al.Cwd != "" {
		full := config.ExpandTilde(al.Cwd, a.Cfg.Home)
		dir, dvia, err := hostlink.ResolvePath(full, false)
		if err != nil {
			refuse("Its cwd %s does not exist on this machine, or cannot be looked at: %s.", proposal.Printable(al.Cwd), proposal.Printable(err.Error()))
		} else {
			if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
				refuse("Its cwd %s is not a directory.", proposal.Printable(al.Cwd))
			}
			ds := append([]string{full, dir}, dvia...)
			done := map[string]bool{}
			for _, s := range ds {
				for _, pl := range a.aliasForbidden() {
					if under(s, pl.dir) && !done[pl.what] {
						done[pl.what] = true
						refuse("Its cwd %s is within %s (%s).", proposal.Printable(al.Cwd), pl.what, proposal.Printable(pl.dir))
					}
				}
				if a.hiddenInHome(s) && !done["hidden"] {
					done["hidden"] = true
					refuse("Its cwd %s is in a hidden directory of your home, where tools keep their settings and credentials.", proposal.Printable(al.Cwd))
				}
				for _, pl := range writable {
					if !under(s, pl.dir) || done["writable"] {
						continue
					}
					done["writable"] = true
					if a.Cfg.Trust != config.TrustFallible {
						refuse(`Its cwd %s is within %s (%s): trust = "hostile" refuses an alias that runs where the sandbox can write.`, proposal.Printable(al.Cwd), pl.what, proposal.Printable(pl.dir))
					} else {
						warnings = append(warnings, fmt.Sprintf("Its cwd %s is within %s (%s): what it runs reads files the sandbox can change.", proposal.Printable(al.Cwd), pl.what, proposal.Printable(pl.dir)))
					}
				}
			}
		}
	}
	for _, e := range al.Command[1:] {
		if strings.HasPrefix(e, "/") {
			warnings = append(warnings, fmt.Sprintf("It names the path %s on this machine: check what is there.", proposal.Printable(e)))
		}
	}
	if al.Policy != config.AliasAsk {
		warnings = append(warnings, `It runs whenever a session asks, without asking you: policy = "ask" would ask first (caboose requests).`)
	}
	if f := a.Cfg.File; f != nil && f.Aliases[al.Name].Name != "" {
		warnings = append(warnings, fmt.Sprintf("It replaces the alias %s config.toml has now: the difference below says how.", al.Name))
	}
	return cmd, refusals, warnings
}

// aliasEdit is the config.toml edit that writes al: its keys set, and
// those the alias of its name has now that al does not, commented out.
func (a *App) aliasEdit(al proposal.Alias) config.Edit {
	e := config.Edit{Set: al.Edit()}
	prefix := "alias." + al.Name + "."
	if f := a.Cfg.File; f != nil {
		for _, k := range slices.Sorted(maps.Keys(f.Vals)) {
			if _, set := e.Set[k]; strings.HasPrefix(k, prefix) && !set {
				e.Unset = append(e.Unset, k)
			}
		}
	}
	return e
}

// configDiff is what writing e changes in config.toml, as a line diff,
// or why it cannot be written safely.
func (a *App) configDiff(e config.Edit) (string, error) {
	path, data, _, err := readConfigToEdit(a.Cfg.EnvDir)
	if err != nil {
		return "", err
	}
	edited := config.EditFile(data, e)
	if err := config.CheckEdit(path, data, edited, e); err != nil {
		return "", err
	}
	return lineDiff(string(data), string(edited)), nil
}

// aliasPlan is a proposed alias as it would be applied.
type aliasPlan struct {
	alias   proposal.Alias
	command string
	edit    config.Edit
	diff    string
}

// planAlias is planProposal's for an alias.
func (a *App) planAlias(al proposal.Alias) (*aliasPlan, []string, []string) {
	cmd, refusals, warnings := a.checkProposedAlias(al)
	if len(refusals) > 0 {
		return nil, refusals, nil
	}
	e := a.aliasEdit(al)
	d, err := a.configDiff(e)
	if err != nil {
		return nil, []string{fmt.Sprintf("It cannot be written into config.toml safely: %s.", proposal.Printable(err.Error()))}, nil
	}
	return &aliasPlan{alias: al, command: cmd, edit: e, diff: d}, nil, warnings
}

// showAlias shows what applying pl changes.
func (a *App) showAlias(p *prompter, pl *aliasPlan) {
	al := pl.alias
	p.say("A host command the sandbox may run ([alias.%s]), with caboose-agent run %s:", al.Name, al.Name)
	var words []string
	for _, e := range al.Command {
		words = append(words, proposal.Printable(e))
	}
	rows := [][2]string{{"command", strings.Join(words, " ")}}
	if pl.command != al.Command[0] {
		rows = append(rows, [2]string{"", "which is " + proposal.Printable(pl.command) + " now"})
	}
	dir := "a fresh empty directory"
	if al.Cwd != "" {
		dir = proposal.Printable(al.Cwd)
	}
	rows = append(rows, [2]string{"in", dir})
	for _, k := range slices.Sorted(maps.Keys(al.Params)) {
		rows = append(rows, [2]string{"{" + k + "}", strings.Join(al.Params[k], ", ")})
	}
	p.table(rows)
	p.blank()
	p.diff(a.short(filepath.Join(a.Cfg.EnvDir, config.FileName)), "with the alias", pl.diff)
	p.blank()
}

// applyAlias asks for and writes pl: the alias's name typed out, then
// config.toml written, provided its change is still the one shown. ok is
// false when it was not written.
func (a *App) applyAlias(p *prompter, pl *aliasPlan) (ok bool, err error) {
	name := pl.alias.Name
	typed, err := p.ask(fmt.Sprintf("To let the sandbox run %s on this machine, type the alias's name, %s", proposal.Printable(pl.alias.Command[0]), name), "")
	if err != nil {
		return false, err
	}
	if typed != name {
		p.fail("That is not %s: nothing was applied, and the proposal is left pending", name)
		return false, nil
	}
	d, err := a.configDiff(pl.edit)
	if err != nil || d != pl.diff {
		p.fail("config.toml changed since it was shown: nothing was applied, and the proposal is left pending (apply shows it again)")
		return false, nil
	}
	if _, err := a.writeConfig(pl.edit); err != nil {
		return false, err
	}
	if err := a.rereadConfigFile(); err != nil {
		return false, err
	}
	p.ok("Wrote [alias.%s] to %s: the link reads it within seconds, with no restart", name, a.short(filepath.Join(a.Cfg.EnvDir, config.FileName)))
	return true, nil
}
