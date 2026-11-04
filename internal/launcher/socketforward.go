package launcher

import (
	"fmt"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/hostlink"
)

// forwardSocketsText is c's socket forwards as linkSettings compares them.
func forwardSocketsText(c *config.Config) string {
	lines := make([]string, 0, len(c.ForwardSockets))
	for _, s := range c.ForwardSockets {
		lines = append(lines, s.Name+"="+s.Path)
	}
	return strings.Join(lines, "\n")
}

// unixForwards is c's socket forwards as a link serves them.
func unixForwards(c *config.Config) []hostlink.UnixForward {
	var out []hostlink.UnixForward
	for _, s := range c.ForwardSockets {
		out = append(out, hostlink.UnixForward{Name: s.Name, Sandbox: s.Path, Host: config.SocketHostPath(c.EnvDir, s.Name)})
	}
	return out
}

// socketView is one configured socket forward as status and doctor tell it.
type socketView struct {
	name, path, host string
	// serving says the link listens at host. Why says why not, when the
	// running link said so, or the path alone settles it.
	serving bool
	why     string
}

// socketViews reads what came of each configured socket forward: link.json,
// written by the link that runs now, when up says one does.
func (a *App) socketViews(up bool) []socketView {
	var said []agentproto.SocketInfo
	current := false
	if up {
		if st, ok := readLinkState(a.Cfg.DataDir); ok {
			said = st.Sockets
			current = st.ForwardSockets == forwardSocketsText(a.Cfg)
		}
	}
	var views []socketView
	for _, f := range unixForwards(a.Cfg) {
		v := socketView{name: f.Name, path: f.Sandbox, host: f.Host}
		if err := hostlink.CheckSocketPath(f.Host); err != nil {
			v.why = err.Error()
		} else if i := slices.IndexFunc(said, func(s agentproto.SocketInfo) bool { return s.Name == f.Name && s.Path == f.Sandbox }); current && i >= 0 {
			v.why, v.serving = said[i].Reason, said[i].Reason == ""
		}
		views = append(views, v)
	}
	return views
}

// socketLines are status's lines for the socket forwards.
func (a *App) socketLines(up bool) []string {
	var lines []string
	for _, v := range a.socketViews(up) {
		l := fmt.Sprintf("%s: %s reaches %s in the %s", v.name, v.host, v.path, a.noun())
		switch {
		case v.why != "":
			l += "; not listening: " + v.why
		case !up:
			l += " (while the link runs)"
		case !v.serving:
			l += " (not listening yet: the link has not served it)"
		}
		lines = append(lines, l)
	}
	return lines
}

// doctorSockets is doctor's row for each socket forward.
func (a *App) doctorSockets(c *checkup, up bool) {
	for _, v := range a.socketViews(up) {
		label := "socket " + v.name
		switch {
		case v.why == hostlink.ReasonSocketOldAgent:
			c.problem(label, "caboose restart"+endsSessions, "%s", v.why)
		case v.why != "":
			c.problem(label, "as it says, then caboose link --restart", "%s", v.why)
		case v.serving:
			c.ok(label, "%s reaches %s in the %s", v.host, v.path, a.noun())
		case up:
			c.note(label, "%s: the running link has not served it yet (it rereads config.toml within seconds)", v.host)
		default:
			c.ok(label, "%s reaches %s in the %s while the link runs", v.host, v.path, a.noun())
		}
	}
}
