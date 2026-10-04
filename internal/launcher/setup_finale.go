package launcher

import (
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/datadir"
)

// reloadRoots reads the roots back from the configuration, as the next
// command would -- the roots section may just have changed them -- and
// resolves them, as a launch does before it creates a container.
func (a *App) reloadRoots() error {
	c := a.Cfg
	fresh, err := config.Load(c.Getenv, config.OSFS{}, c.Env)
	if err != nil {
		return err
	}
	c.File, c.Roots, c.RootsFrom = fresh.File, fresh.Roots, fresh.RootsFrom
	return c.ResolveRoots()
}

// setupStart brings the container up, in a whole run, between the
// isolation and git (both decide how it is created): git lists the forwarded agent's keys from it, and sync runs its
// git in it. A running container is left as it is; a stopped one is
// started; an absent one is created only when asked, since that builds
// the image when there is none. Nothing here fails setup: a container
// that does not come up is said, and git and sync do without it.
//
// A default environment still in CABOOSE_HOME itself is not created here:
// creating its container is what moves the data dir into envs/default,
// and setup never moves it.
func (a *App) setupStart(p *prompter) error {
	c := a.Cfg
	p.heading("Container", "Where sessions run: "+c.Container+". Git lists the forwarded agent's keys from it, and sync runs its git in it.")
	if err := a.reloadRoots(); err != nil {
		p.fail("Not started: %s", firstLine(err.Error()))
		return nil
	}
	switch st := a.state(); st {
	case "running":
		p.ok("Running")
		return nil
	case "absent":
		ok, err := p.yesNo("Create and start it now? When there is no image, that builds it first, which takes a few minutes.", true)
		if err != nil {
			return err
		}
		if !ok {
			p.same("Not created: the first launch creates it")
			return nil
		}
	default:
		p.note("Starting it (it is %s)...", st)
	}
	if err := a.ensureRunning(true); err != nil {
		p.fail("Not started: %v", err)
		return nil
	}
	p.ok("Running")
	return nil
}

// setupLogin says, last in a whole run, whether the sandbox has a Claude
// login. The login is Claude Code's own first-run flow, in its TUI, which
// setup cannot drive from the host; with none yet, and the current
// directory under a root the running container mounts, it offers to start
// a session there, which asks for it. Otherwise it says how to.
func (a *App) setupLogin(p *prompter) error {
	c := a.Cfg
	p.heading("Claude login", "The sandbox's own login to Claude, kept in its data dir.")
	in, err := datadir.LoggedIn(c.DataDir)
	switch {
	case err != nil:
		p.warn("Cannot tell whether there is a login: %v", err)
		return nil
	case in:
		p.ok("Logged in")
		return nil
	}
	how := "Run caboose in a project under " + config.DescribeRoots(c.Roots) + ": Claude Code asks you to log in the first time."
	if cwd, err := config.Cwd(); err != nil || a.state() != "running" {
		p.warn("Not logged in yet. %s", how)
		return nil
	} else if _, err := a.containerDir(cwd); err != nil {
		p.warn("Not logged in yet. %s", how)
		return nil
	}
	ok, err := p.yesNo("Not logged in yet. Start a session here, which asks you to log in?", true)
	if err != nil {
		return err
	}
	if !ok {
		p.same("%s", how)
		return nil
	}
	attach := a.attach
	if attach == nil {
		attach = a.Attach
	}
	return attach(nil)
}
