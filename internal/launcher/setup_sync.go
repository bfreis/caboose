package launcher

import (
	"path/filepath"

	"github.com/bfreis/caboose/internal/config"
)

// setupSync asks for the sync remote and whether a launch syncs by itself
// (auto_sync in [session]). auto_sync is config.toml's; the remote lives in the sync
// repo, whose git runs in the container, so a new one is set by running
// what `caboose sync --remote URL` runs: the container brought up (never
// built), no session running, a first sync. That failing is said, with
// what to run once it can work, and setup goes on. A remote cannot be
// removed here, as with caboose sync: turning auto_sync off stops the
// automatic part.
func (a *App) setupSync(p *prompter) error {
	c := a.Cfg
	cur := a.newSyncer(nil).RemoteHint()
	p.heading("Sync", "Keeps memories, settings, skills, agents, commands and MCP servers in step across machines, "+
		"through a git remote you own. An empty private repo will do.")
	question := "Sync remote, a git URL (empty for none)"
	if cur != "" {
		question = "Sync remote, a git URL"
	}
	url, err := p.ask(question, cur)
	if err != nil {
		return err
	}
	curAuto := c.AutoSync
	auto := curAuto
	if url != "" || curAuto {
		if auto, err = p.yesNo("Sync by itself, before a launch that finds nothing running? (auto_sync)", curAuto); err != nil {
			return err
		}
	}
	if auto != curAuto {
		if _, err := a.writeConfig(config.Edit{Set: map[string]any{"session.auto_sync": auto}}); err != nil {
			return err
		}
		p.ok("Wrote auto_sync = %v in [session] to %s", auto, a.short(filepath.Join(c.EnvDir, config.FileName)))
	}
	switch {
	case url == cur:
		if auto == curAuto {
			p.same("Nothing changed")
		}
		return nil
	case url == "":
		return nil
	}
	p.note("Setting the remote and syncing, as 'caboose sync --remote %s' does...", url)
	if err := a.Sync([]string{"--remote", url}); err != nil {
		p.fail("Did not sync: %v", err)
		if a.newSyncer(nil).RemoteHint() == url {
			p.warn("The remote is set; %s finishes the first sync.", p.code("caboose sync"))
		} else {
			p.warn("The remote is not set; once that is fixed, %s sets it.", p.code("caboose sync --remote "+url))
		}
		return nil
	}
	p.ok("Synced with %s", url)
	return nil
}
