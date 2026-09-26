package launcher

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/proposal"
)

// `caboose apply` is the host's half of a proposal (internal/proposal): a
// session in the sandbox writes what it would change -- a Dockerfile
// section, a root -- and apply shows each one and makes
// the change when the user says so. The sandbox never writes config.toml
// or image/ itself: both decide what it is, and what of the host it
// reaches, so a person at the host says yes to every change, having seen
// all of it.
//
// A proposal is untrusted through and through. What it cannot ask for is
// refused before anything is shown as a question: it is not a matter of
// saying no to it. A root has the most to refuse, since it hands the
// sandbox a host directory: the home or anything holding it, anything in a
// hidden directory of the home (where tools keep their credentials),
// anything holding caboose's own state, anything overlapping a root. It is
// checked, and written, as its physical path, so a symlink the sandbox can
// change later does not decide what is mounted; and it is confirmed by
// typing its name, which no habit of pressing Enter gets past.

// ApplyCommand is the apply command for env, as SetupCommand is setup's.
func ApplyCommand(env string) string { return envCommand(env, "apply") }

// envCommand is caboose's command cmd for env: the -e form for any
// environment but the default one.
func envCommand(env, cmd string) string {
	if env != config.DefaultEnv {
		return "caboose -e " + env + " " + cmd
	}
	return "caboose " + cmd
}

// Apply is `caboose apply`: every pending proposal, one at a time, each
// applied, left pending or deleted as the user says; then one build, and
// the restart that moves the container onto the changes, offered.
func (a *App) Apply() error {
	c := a.Cfg
	if c.EnvDir == "" {
		return Die("this environment has no dir of its own (CABOOSE_DATA_DIR alone names its data), so no config.toml or image/ to apply a proposal to")
	}
	entries, err := proposal.List(c.DataDir)
	if err != nil {
		return Die("reading %s: %v", filepath.Join(c.DataDir, proposal.Dir), err)
	}
	if len(entries) == 0 {
		a.Note("no pending proposals. A session proposes a change by writing one into %s (see ~/.claude/CLAUDE.md in the sandbox).", proposal.ContainerDir)
		return nil
	}
	term, err := a.openTerminal()
	if err != nil {
		return Die("caboose apply asks before it changes anything, and there is no terminal to ask on (%v);\n"+
			"       run it in one. Nothing was changed.", err)
	}
	defer term.Close()
	p := a.newSetupPrompter(term)
	err = a.applyRun(p, entries)
	a.exportProposals()
	if errors.Is(err, errNoAnswer) {
		fmt.Fprintln(a.Stderr)
		return Die("apply stopped: the proposals applied before this one are written; the rest are still pending")
	}
	return err
}

// applied is what a run has changed.
type applied struct {
	dockerfile, config bool
}

func (a *App) applyRun(p *prompter, entries []proposal.Entry) error {
	c := a.Cfg
	p.steps = len(entries)
	p.banner("caboose apply", "environment "+c.Env)
	p.note("%d %s from sessions in the sandbox. Each is shown whole, and nothing changes until you say so. ^C stops; what was applied before it stays.",
		len(entries), plural(len(entries), "proposal", "proposals"))
	var done applied
	for _, e := range entries {
		if err := a.applyOne(p, e, &done); err != nil {
			return err
		}
	}
	p.blank()
	if !done.dockerfile && !done.config {
		p.same("Nothing was applied")
		return nil
	}
	return a.applyFinish(p, done)
}

// plan is one proposal as it would be applied.
type plan struct {
	// dockerfile is the new image/Dockerfile, and from what it was made.
	dockerfile, oldDockerfile []byte
	source                    string
	// root is the root to add, at host, its physical path.
	root *proposal.Root
	host string
}

func (pl plan) empty() bool { return pl.dockerfile == nil && pl.root == nil }

// applyOne shows one proposal and does what the user says with it.
func (a *App) applyOne(p *prompter, e proposal.Entry, done *applied) error {
	name := proposal.Printable(strings.TrimSuffix(e.File, proposal.Ext))
	if e.Err != nil {
		p.heading(name, "")
		p.fail("This is no proposal caboose can apply: %s", proposal.Printable(e.Err.Error()))
		return a.keepOrDelete(p, e)
	}
	pr := e.Proposal
	p.heading(pr.Title, pr.Reason)
	p.note("%s, proposed %s", proposal.Printable(e.File), ago(a.now(), e.Modified))
	p.blank()

	pl, refusals, warnings := a.planProposal(pr)
	if len(refusals) > 0 {
		for _, r := range refusals {
			p.fail("%s", r)
		}
		p.blank()
		p.note("Nothing in it can be applied as it is.")
		return a.keepOrDelete(p, e)
	}
	a.showPlan(p, pl)
	for _, w := range warnings {
		p.warn("%s", w)
	}
	if pl.empty() {
		p.same("There is nothing left in it to apply")
		return a.keepOrDelete(p, e)
	}
	p.blank()
	def := 0
	if pl.root != nil {
		def = 1 // a root is not added on Enter alone
	}
	i, err := p.choose("Apply this proposal?", []string{"Apply it", "Leave it pending", "Delete it"}, def)
	if err != nil {
		return err
	}
	switch i {
	case 1:
		p.same("Left pending")
		return nil
	case 2:
		return a.deleteProposal(p, e)
	}

	edit := config.Edit{}
	if pl.root != nil {
		ok, err := a.confirmRoot(p, pl, &edit)
		if err != nil || !ok {
			return err
		}
	}
	if pl.dockerfile != nil {
		if err := a.writeImageDir(p, filepath.Join(a.Cfg.EnvDir, config.ImageDirName), pl.dockerfile); err != nil {
			return err
		}
		done.dockerfile = true
	}
	if edit.SetRoots {
		if _, err := a.writeConfig(edit); err != nil {
			return err
		}
		if err := a.rereadConfigFile(); err != nil {
			return err
		}
		p.ok("Wrote the roots to %s", a.short(filepath.Join(a.Cfg.EnvDir, config.FileName)))
		done.config = true
	}
	if err := proposal.Remove(a.Cfg.DataDir, e.File); err != nil {
		p.warn("Applied, but it could not be removed (%v): delete %s by hand, or apply will offer it again", err, a.short(filepath.Join(a.Cfg.DataDir, proposal.Dir, e.File)))
		return nil
	}
	p.ok("Applied %s", name)
	return nil
}

// planProposal works out what applying pr would change, or why it cannot:
// refusals are what nobody can say yes to, warnings what to know first.
func (a *App) planProposal(pr *proposal.Proposal) (pl plan, refusals, warnings []string) {
	c := a.Cfg
	if s := pr.Section; s != nil {
		source, old, err := a.dockerfileBase()
		switch {
		case err != nil:
			refusals = append(refusals, fmt.Sprintf("Cannot read the Dockerfile: %v", err))
		case source == proposal.SourceBaseImage:
			refusals = append(refusals, fmt.Sprintf("This environment builds on base_image %s, an image rather than a Dockerfile, so a section cannot go into it.", c.BaseImage))
		case proposal.Hash(old) != pr.DockerfileSHA256:
			refusals = append(refusals, "The Dockerfile has changed since this was proposed. Ask the session to propose it again: it reads the current one in "+
				proposal.ContainerDir+"/"+proposal.CurrentDir+".")
		default:
			next, err := assets.SetSection(old, s.Name, s.Title, s.Body)
			if err != nil {
				refusals = append(refusals, fmt.Sprintf("Its section cannot go into the Dockerfile: %v", err))
				break
			}
			pl.dockerfile, pl.oldDockerfile, pl.source = next, old, source
			if source == proposal.SourcePreset {
				warnings = append(warnings, fmt.Sprintf("This environment builds from the Dockerfile built into caboose, which changes as caboose does. "+
					"Applying this writes caboose's preset, with this section, as %s: from then on the environment builds from its own Dockerfile, "+
					"and a newer caboose's changes to its Dockerfile no longer reach it ('caboose setup image' shows how the two differ).",
					a.short(filepath.Join(c.EnvDir, config.ImageDirName, "Dockerfile"))))
			}
		}
	}

	if r := pr.Root; r != nil {
		host, why, warn := a.checkProposedRoot(*r)
		refusals = append(refusals, why...)
		warnings = append(warnings, warn...)
		pl.root, pl.host = r, host
	}
	return pl, refusals, warnings
}

// showPlan shows what applying pl changes.
func (a *App) showPlan(p *prompter, pl plan) {
	c := a.Cfg
	if pl.dockerfile != nil {
		from := a.short(filepath.Join(c.EnvDir, config.ImageDirName, "Dockerfile"))
		if pl.source == proposal.SourcePreset {
			from = "caboose's preset"
		}
		p.say("The image: a section in the Dockerfile.")
		p.diff(from, "with the proposal", lineDiff(string(pl.oldDockerfile), string(pl.dockerfile)))
		p.blank()
	}
	if pl.root != nil {
		p.say("Mount a host directory into the sandbox ([roots]):")
		p.table([][2]string{{config.WorkDir + "/" + pl.root.Name, "← " + pl.host}})
		if a.short(pl.host) != proposal.Printable(pl.root.Path) && pl.host != proposal.Printable(pl.root.Path) {
			p.note("    proposed as %s", proposal.Printable(pl.root.Path))
		}
		p.blank()
		p.warn("Everything under %s becomes readable and writable from the sandbox.", pl.host)
	}
}

// confirmRoot asks for what adding pl's root takes: a name for the single
// root there is now, when there is one (it moves to /work/NAME), and the
// root's own name typed out. It fills edit's roots when the user goes
// ahead; ok is false when not.
func (a *App) confirmRoot(p *prompter, pl plan, edit *config.Edit) (ok bool, err error) {
	c := a.Cfg
	if v := c.Getenv("CABOOSE_REPO_ROOT"); v != "" {
		p.warn("CABOOSE_REPO_ROOT is set (%s), and wins over config.toml's roots while it is.", v)
	}
	cur := a.fileRoots()
	roots := append([]setupRoot(nil), cur...)
	if len(roots) == 1 && roots[0].Name == "" {
		p.note("The root there is now, %s, is mounted at %s. With two, each is mounted at %s/NAME, so it needs a name too.",
			roots[0].Path, config.WorkDir, config.WorkDir)
		name, err := a.askRootName(p, roots[0].Path, append(roots, setupRoot{Name: pl.root.Name}), 0)
		if err != nil {
			return false, err
		}
		roots[0].Name = name
	}
	roots = append(roots, setupRoot{Name: pl.root.Name, Path: a.rootSpelling(pl.host)})
	if moves := a.rootMoves(cur, roots); len(moves) > 0 {
		for _, m := range moves {
			p.warn("%s", capFirst(m))
		}
		p.note("Claude Code keeps each project's state (memories, history, settings) under its path in the container, " +
			"so it will not find what it has under the old one. Nothing is moved or deleted.")
		if ok, err := p.yesNo("Change the roots anyway?", false); err != nil || !ok {
			if err == nil {
				p.same("Left pending")
			}
			return false, err
		}
	}
	typed, err := p.ask(fmt.Sprintf("To mount %s, type the root's name, %s", pl.host, pl.root.Name), "")
	if err != nil {
		return false, err
	}
	if typed != pl.root.Name {
		p.fail("That is not %s: nothing was applied, and the proposal is left pending", pl.root.Name)
		return false, nil
	}
	edit.SetRoots, edit.Unset = true, []string{"repo_root"}
	edit.Roots = map[string]string{}
	for _, r := range roots {
		edit.Roots[r.Name] = r.Path
	}
	return true, nil
}

// rootSpelling is how a root at physical path host is written into
// config.toml: as it is, or under ~ when it is under the home's physical
// path. Either way only symlinks the host's own (a home under /var, say,
// which is /private/var on a Mac) stand between it and the directory
// checked: never one the sandbox could change.
func (a *App) rootSpelling(host string) string {
	home := physicalOr(a.Cfg.Home)
	if home != "" && home != "/" && host != home && config.Within(host, home) {
		return "~" + strings.TrimPrefix(host, home)
	}
	return host
}

// keepOrDelete asks what to do with a proposal that is not applied.
func (a *App) keepOrDelete(p *prompter, e proposal.Entry) error {
	i, err := p.choose("What now?", []string{"Delete it", "Leave it pending"}, 0)
	if err != nil {
		return err
	}
	if i == 1 {
		p.same("Left pending")
		return nil
	}
	return a.deleteProposal(p, e)
}

func (a *App) deleteProposal(p *prompter, e proposal.Entry) error {
	if err := proposal.Remove(a.Cfg.DataDir, e.File); err != nil {
		p.fail("Could not delete it: %v", err)
		return nil
	}
	p.ok("Deleted %s", proposal.Printable(e.File))
	return nil
}

// applyFinish builds the image when a Dockerfile changed, and offers the
// restart that moves the container onto what was applied.
func (a *App) applyFinish(p *prompter, done applied) error {
	restart := envCommand(a.Cfg.Env, "restart")
	if done.dockerfile {
		build, err := p.yesNo("Build the image now? It takes a few minutes.", true)
		if err != nil {
			return err
		}
		switch {
		case !build:
			p.same("Not built: %s builds it, and so does the restart that moves the container onto it", p.code(envCommand(a.Cfg.Env, "build")))
		case a.build(nil, a.Stderr) != nil:
			p.fail("The build failed (above). Fix what it says, then run %s and %s.", p.code(envCommand(a.Cfg.Env, "build")), p.code(restart))
			return nil
		default:
			p.ok("Built image '%s'", a.Cfg.Image)
		}
	}
	if a.state() == "absent" {
		p.note("There is no container yet: the next launch creates it with all of this.")
		return nil
	}
	sessions := a.liveSessions()
	q := "Restart the container now, to use what was applied?"
	if n := len(sessions); n > 0 {
		q = fmt.Sprintf("Restart the container now, to use what was applied? This ends %d running %s:", n, plural(n, "session", "sessions"))
	}
	p.blank()
	if len(sessions) > 0 {
		p.listQuestion(q)
		p.bullets(sessions)
		q = "Restart now?"
	}
	ok, err := p.yesNo(q, false)
	if err != nil {
		return err
	}
	if !ok {
		p.same("Not restarted: %s when you are ready (it ends running sessions)", p.code(restart))
		return nil
	}
	if err := a.reloadConfig(); err != nil {
		p.fail("Cannot read the configuration back (%v): run %s yourself", err, p.code(restart))
		return nil
	}
	a.lossConfirmed = true
	if err := a.Restart(); err != nil {
		return err
	}
	p.ok("Restarted: %s starts a session again, and /resume in it picks up a conversation", p.code("caboose"))
	return nil
}

// liveSessions are the tmux sessions running in the container.
func (a *App) liveSessions() []string {
	if a.state() != "running" {
		return nil
	}
	out, _ := a.Docker.Output("exec", a.Cfg.Container, "tmux", "list-sessions", "-F", "#{session_name}")
	var sessions []string
	for _, s := range strings.Split(out, "\n") {
		if s = strings.TrimSpace(s); s != "" {
			sessions = append(sessions, s)
		}
	}
	return sessions
}

// rereadConfigFile reads config.toml back into the configuration after a
// proposal wrote it, so that the next proposal is planned against it.
func (a *App) rereadConfigFile() error {
	path := filepath.Join(a.Cfg.EnvDir, config.FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return Die("reading %s: %v", path, err)
	}
	f, err := config.ParseFile(path, data)
	if err != nil {
		return Die("%v", err)
	}
	a.Cfg.File = f
	return nil
}

// reloadConfig resolves the whole configuration again, roots included, for
// a restart that has to create the container from what was applied.
func (a *App) reloadConfig() error {
	cfg, err := config.Load(a.Cfg.Getenv, config.OSFS{}, a.Cfg.Env)
	if err != nil {
		return err
	}
	if err := cfg.ResolveRoots(); err != nil {
		return err
	}
	a.Cfg = cfg
	return nil
}

// dockerfileBase is the Dockerfile a proposed section goes into, and where
// it comes from: the environment's image/Dockerfile, or, when it has none,
// caboose's default preset, which the first section proposed becomes. On
// CABOOSE_BASE_IMAGE there is none.
func (a *App) dockerfileBase() (source string, data []byte, err error) {
	c := a.Cfg
	if c.BaseImage != "" {
		return proposal.SourceBaseImage, nil, nil
	}
	data, err = os.ReadFile(filepath.Join(c.EnvDir, config.ImageDirName, "Dockerfile"))
	if err == nil {
		return proposal.SourceImageDir, data, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", nil, err
	}
	data, err = assets.Preset(assets.DefaultSections())
	return proposal.SourcePreset, data, err
}

// exportProposals writes what a proposal is made against where sessions
// can read it (proposal.WriteCurrent). Best-effort: a session without it
// can still propose a root.
func (a *App) exportProposals() {
	c := a.Cfg
	if c.EnvDir == "" {
		return
	}
	source, df, err := a.dockerfileBase()
	if err != nil {
		a.Note("not telling sessions about the Dockerfile: %v", err)
		return
	}
	s := proposal.State{Source: source, Dockerfile: df, BaseImage: c.BaseImage}
	if roots := a.fileRoots(); len(roots) == 1 && roots[0].Name == "" {
		s.RepoRoot = roots[0].Path
	} else {
		s.Roots = map[string]string{}
		for _, r := range roots {
			s.Roots[r.Name] = r.Path
		}
	}
	if err := proposal.WriteCurrent(c.DataDir, s); err != nil {
		a.Note("not telling sessions what a proposal is made against: %v", err)
	}
}

// notePendingProposals says, in one line, when sessions have proposed
// changes that are waiting for caboose apply.
func (a *App) notePendingProposals() {
	entries, err := proposal.List(a.Cfg.DataDir)
	if err != nil || len(entries) == 0 {
		return
	}
	names := proposal.Names(entries)
	for i, n := range names {
		names[i] = proposal.Printable(n)
	}
	a.Note("%d pending %s (%s): '%s' reviews %s", len(entries), plural(len(entries), "proposal", "proposals"),
		strings.Join(names, ", "), ApplyCommand(a.Cfg.Env), plural(len(entries), "it", "them"))
}
