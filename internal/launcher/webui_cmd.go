package launcher

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/hostlink"
	"github.com/bfreis/caboose/internal/proposal"
	"github.com/bfreis/caboose/internal/uiport"
)

// webBudget bounds caboose ui's wait for the daemon, which is started when
// nothing answers.
const webBudget = 40 * time.Second

// openURL opens u in the user's browser: App.Opener, or this machine's.
func (a *App) openURL(u string) error {
	if a.Opener != nil {
		return a.Opener(u)
	}
	return hostlink.System{}.OpenURL(u)
}

// holderCommand is what finds the process that holds a TCP port here.
func holderCommand(port int) string {
	if runtime.GOOS == "darwin" {
		return fmt.Sprintf("lsof -nP -iTCP:%d -sTCP:LISTEN", port)
	}
	return "ss -ltnp"
}

// addrPort is the port of a host:port address, or 0.
func addrPort(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil || p == "" {
		return 0
	}
	n := 0
	for _, c := range p {
		if c < '0' || c > '9' || n > 65535 {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// webPort is the port of a web state's address, the release's when it
// has none.
func webPort(w daemonproto.WebState) int {
	if p := addrPort(w.Addr); p != 0 {
		return p
	}
	return uiport.Release
}

// notListening explains a page the daemon does not serve.
func notListening(w daemonproto.WebState) string {
	why := w.Error
	if why == "" {
		why = "the daemon does not serve it"
	}
	return fmt.Sprintf("the web UI is not served on %s: %s\n"+
		"  Another process may hold the port (a release uses %d and a checkout's build %d). Find it with: %s\n"+
		"  Stop it, then run: %s",
		w.Addr, proposal.Printable(why), uiport.Release, uiport.Dev, holderCommand(webPort(w)), runDaemonHint)
}

// uiPage reads caboose ui's page argument: requests, proposals, or an
// environment's name.
func uiPage(arg string) (string, error) {
	switch arg {
	case "":
		return "", nil
	case "requests", "proposals":
		return arg, nil
	}
	if !config.ValidEnv(arg) {
		return "", Die("usage: caboose ui [requests|proposals|ENV] [--print] [--new-token]\n  %q is not a page or an environment name", proposal.Printable(arg))
	}
	return "env/" + arg, nil
}

// UI is `caboose ui`: it opens the web page with a one-time code, which
// the page trades for its token, through the daemon, which is started if
// need be. It never opens an address the daemon does not serve: whatever
// else holds the port would be handed the code.
func (a *App) UI(args []string) error {
	const usage = "usage: caboose ui [requests|proposals|ENV] [--print] [--new-token]"
	var print, rotate bool
	var pageArg string
	for _, arg := range args {
		switch {
		case arg == "--print":
			print = true
		case arg == "--new-token":
			rotate = true
		case strings.HasPrefix(arg, "-"):
			return Die("%s (got %q)", usage, proposal.Printable(arg))
		case pageArg != "":
			return Die("%s", usage)
		default:
			pageArg = arg
		}
	}
	page, err := uiPage(pageArg)
	if err != nil {
		return err
	}
	if page == "" && a.Cfg.Env != config.DefaultEnv {
		page = "env/" + a.Cfg.Env
	}
	ctx, cancel := context.WithTimeout(context.Background(), webBudget)
	defer cancel()
	c, err := a.dialDaemon(ctx, true)
	if err != nil {
		return Die("the caboose daemon could not be reached: %s\n  run: %s", daemonErrText(err), runDaemonHint)
	}
	defer c.Close()
	r, err := c.Web(ctx, daemonproto.WebParams{Rotate: rotate, Code: true})
	if err != nil {
		var de *daemonproto.Error
		if errors.As(err, &de) && de.Kind == daemonproto.KindUnknownMethod {
			return Die("the running caboose daemon is older than this caboose and has no web UI;\n  run: %s", runDaemonHint)
		}
		return Die("the caboose daemon would not give the web UI's address: %s\n  run: %s", daemonErrText(err), runDaemonHint)
	}
	// Warned either way: when the page is not served, a sandbox on the
	// host's network may be what holds its port.
	a.warnHostNetwork()
	if !r.Listening || r.Code() == "" {
		return Die("%s", notListening(r.WebState))
	}
	u := r.CodeURL
	if page != "" {
		u += "&to=" + page
	}
	if rotate {
		a.Note("the page's token was replaced: a page open with the old one is refused from now on")
	}
	if print {
		fmt.Fprintln(a.Stdout, u)
		a.Note("this address opens the page once, within %d s: the code after #code= is spent when the page opens, or after %d s. Run caboose ui again for another.", int(daemonproto.CodeTTL/time.Second), int(daemonproto.CodeTTL/time.Second))
		return nil
	}
	if err := a.openURL(u); err != nil {
		return Die("could not open a browser: %s\n  run: %s, which prints the address instead", firstLine(err.Error()), envCommand(a.Cfg.Env, "ui --print"))
	}
	fmt.Fprintf(a.Stdout, "opened %s in your browser\n", r.URL)
	return nil
}

// warnHostNetwork says when any environment's sandbox is on the host's
// network, where it can take the page's port whenever the daemon does not
// hold it. Best effort: an environment it cannot read is skipped.
func (a *App) warnHostNetwork() {
	var bad []string
	if len(hostNetworkArgs(a.Cfg.RunArgs)) > 0 {
		bad = append(bad, a.Cfg.Env)
	}
	if a.Cfg.CabooseHome != "" {
		if names, err := a.envNames(); err == nil {
			for _, n := range names {
				if n == a.Cfg.Env {
					continue
				}
				cfg, err := config.Load(a.Cfg.Getenv, config.OSFS{}, n)
				if err == nil && len(hostNetworkArgs(cfg.RunArgs)) > 0 {
					bad = append(bad, n)
				}
			}
		}
	}
	if len(bad) > 0 {
		a.Note("warning: environment %s runs on the host's network (--network host in run_args): a sandbox there can take port %d or %d while the daemon does not hold it and read the token from the page; remove it (caboose doctor says where)",
			strings.Join(bad, ", "), uiport.Release, uiport.Dev)
	}
}

// webLine is status's "web       :" value, from the daemon's own account.
func webLine(v linkView) string {
	switch {
	case v.Daemon == nil:
		return "unknown (the daemon is not running or does not answer)"
	case v.Daemon.Daemon.Web == nil:
		return "unknown (this daemon does not say; run: caboose daemon restart)"
	}
	w := v.Daemon.Daemon.Web
	if w.Listening {
		return fmt.Sprintf("http://%s/ (listening)", w.Addr)
	}
	why := proposal.Printable(w.Error)
	switch {
	case why == "":
		return fmt.Sprintf("not listening: %s", w.Addr)
	case strings.Contains(why, "address already in use"):
		return fmt.Sprintf("not listening: %s is in use", w.Addr)
	}
	return fmt.Sprintf("not listening: %s: %s", w.Addr, why)
}

// doctorWeb checks the web page: its listener and its token file, and says
// what is refused on its ports. v is the daemon's account, already asked.
func (a *App) doctorWeb(c *checkup, v linkView) {
	switch {
	case v.Daemon == nil:
		c.unchecked("web", "the daemon does not answer, so the listener is unknown")
	case v.Daemon.Daemon.Web == nil:
		c.note("web", "this daemon does not report a web UI (it may be older than this caboose); run: %s", runDaemonHint)
	case v.Daemon.Daemon.Web.Listening:
		c.ok("web", "http://%s/ (listening)", v.Daemon.Daemon.Web.Addr)
	default:
		w := v.Daemon.Daemon.Web
		c.problem("web", fmt.Sprintf("find what holds the port with: %s; stop it, then %s", holderCommand(webPort(*w)), runDaemonHint),
			"not listening on %s: %s", w.Addr, or(proposal.Printable(w.Error), "no reason given"))
	}
	if a.Cfg.CabooseHome != "" {
		a.doctorWebToken(c)
	}
	c.ok("web ports", "%d and %d are the web UI's: forward_ports, open_urls, the outbound proxy and run_args refuse them on purpose", uiport.Release, uiport.Dev)
}

// doctorWebToken checks web.token without following a link or reading it.
func (a *App) doctorWebToken(c *checkup) {
	p := filepath.Join(a.daemonDir(), daemon.TokenFile)
	fi, err := os.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		c.note("web token", "none yet (%s): the daemon makes it when it serves the page", p)
		return
	case err != nil:
		c.problem("web token", "check "+p, "cannot be read: %v", err)
		return
	case !fi.Mode().IsRegular():
		c.problem("web token", "remove "+p+", then "+runDaemonHint, "%s is not a plain file (%s)", p, fi.Mode().Type())
		return
	case fi.Mode().Perm() != 0o600:
		c.problem("web token", "chmod 600 "+p+", or caboose ui --new-token", "%s has mode %04o, not 0600: another user may read it", p, fi.Mode().Perm())
		return
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		c.problem("web token", "caboose ui --new-token, after removing "+p, "%s belongs to user %d, not you", p, st.Uid)
		return
	}
	c.ok("web token", "%s (mode 0600, yours)", p)
}
