package launcher

import (
	"github.com/bfreis/caboose/internal/config"
)

// Host exec is the link's (hostlink/hostexec.go): with host_exec on, a
// session's `caboose-agent host CMD` runs CMD on this machine, as the
// user, in the link's environment. Nothing of it is fixed at creation, so
// it needs no label and no restart: the link offers it from config.toml,
// rereading the file when it changes.

// hostExecOrigin is what set host_exec, for a message.
func hostExecOrigin(c *config.Config) string { return c.Origin("link.host_exec") }

// doctorHostExec is doctor's row on host exec: a note when it is on,
// since it opens the wall; nothing when it is off, the default.
func (a *App) doctorHostExec(c *checkup) {
	if a.Cfg.HostExec {
		c.note("host exec", "on: sessions in this environment can run commands on this machine as you (%s)", hostExecOrigin(a.Cfg))
	}
}

// hostExecSummary is status's line on host exec, "" when it is off.
func (a *App) hostExecSummary() string {
	if a.Cfg.HostExec {
		return "on: sessions can run commands on this machine as you (" + hostExecOrigin(a.Cfg) + ")"
	}
	return ""
}
