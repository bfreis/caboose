package launcher

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/bfreis/caboose/internal/appbundle"
	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/macui"
	"github.com/bfreis/caboose/internal/menubar"
	"github.com/bfreis/caboose/internal/proposal"
	"github.com/bfreis/caboose/internal/selfupdate"
)

// appSupported is whether this system runs the menu-bar app, and has
// status and doctor say anything of it: macOS only. A variable for the
// tests.
var appSupported = runtime.GOOS == "darwin"

// appMacOnly is what caboose app says on any other system.
const appMacOnly = "the menu-bar app is macOS only; `caboose ui` opens the web UI"

// RunApp is `caboose app [--selftest]`: the menu-bar app, a client of the
// daemon of kind app, on the main thread until it quits (cmd/caboose locks
// the main goroutine to it). --selftest checks every class and selector
// the app uses against the running system and exits. A -psn_ argument,
// which older Launch Services add, is ignored.
func (a *App) RunApp(args []string) error {
	if !appSupported {
		return Die("%s", appMacOnly)
	}
	selftest := false
	for _, x := range args {
		switch {
		case x == "--selftest":
			selftest = true
		case strings.HasPrefix(x, "-psn_"):
		default:
			return Die("usage: caboose app [--selftest]")
		}
	}
	if a.Cfg.CabooseHome == "" {
		return Die("neither CABOOSE_HOME nor HOME is set: the daemon has no directory")
	}
	exe, err := a.executable()
	if err != nil {
		return Die("%v", err)
	}
	if err := a.checkAppPlace(context.Background(), exe); err != nil {
		return Die("caboose app refuses to run from %s: %v (make app puts Caboose.app in %s, where no sandbox reaches)", exe, err, filepath.Dir(appbundle.CheckoutPath(a.Cfg.CabooseHome)))
	}
	if selftest {
		if err := macui.SelfTest(a.Stdout); err != nil {
			return Die("%v", err)
		}
		return nil
	}
	core := menubar.New(a.appOptions(exe, macui.New()))
	if err := core.Run(); err != nil {
		return Die("%v", err)
	}
	return nil
}

// appOptions are the core's options for the app whose executable is exe,
// on ui.
func (a *App) appOptions(exe string, ui menubar.UI) menubar.Options {
	lg := log.New(a.Stderr, "caboose app: ", log.LstdFlags)
	return menubar.Options{
		Daemon:   a.appDaemon(),
		UI:       ui,
		Version:  currentVersion(),
		Protocol: daemonProtocol,
		Dev:      a.installed().kind == installDev,
		Dialogs:  true,
		Log:      func(s string) { lg.Print(s) },
		// Asked again before the login item is turned on: what the
		// sandboxes reach may have grown since the app started.
		LoginCheck: func() error { return a.checkAppPlace(context.Background(), exe) },
	}
}

// appDaemon is how the app reaches the daemon: as client kind app, never
// replacing a running daemon (the core asks an older one to make way
// itself, without force), and starting one from the executable
// appSpawnExe chooses.
func (a *App) appDaemon() appDaemon {
	cfg := a.daemonConfig()
	cfg.Client = daemonproto.ClientApp
	cfg.NoReplace = true
	cfg.Spawn = a.spawnAppDaemon
	return appDaemon{cfg: cfg}
}

// appDaemon is menubar.Daemon on daemon's client.
type appDaemon struct{ cfg daemon.Config }

var _ menubar.Daemon = appDaemon{}

func (d appDaemon) Connect(ctx context.Context) (menubar.Conn, error) {
	c, err := daemon.Connect(ctx, d.cfg)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (d appDaemon) Ensure(ctx context.Context) (menubar.Conn, error) {
	c, err := daemon.Ensure(ctx, d.cfg)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (d appDaemon) Replaced() (daemon.Replaced, bool) {
	r, err := daemon.ReadReplaced(d.cfg.Dir)
	return r, err == nil
}

// spawnAppDaemon starts the daemon, detached and with MinimalEnv, from the
// executable appSpawnExe chooses.
func (a *App) spawnAppDaemon() error {
	exe, err := a.appSpawnExe()
	if err != nil {
		return err
	}
	return a.startDaemon(exe)
}

// appSpawnExe is the executable the app starts the daemon from: for an
// app built from a checkout, its own; for a release's, the installed
// CLI's current binary when install.sh's layout has one not older than
// the app, else its own. So an app never starts an older daemon than the
// CLI would.
func (a *App) appSpawnExe() (string, error) {
	own, err := a.executable()
	if err != nil {
		return "", err
	}
	f := menubar.SpawnFacts{Checkout: a.installed().kind == installDev, Own: own, OwnVersion: currentVersion()}
	if a.Cfg.Home != "" && a.Cfg.CabooseHome != "" {
		l := a.layout()
		if tag := l.Current(); tag != "" {
			p := filepath.Join(l.Versions, tag, selfupdate.Binary)
			if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
				f.Installed, f.InstalledVersion = p, tag
			}
		}
	}
	return menubar.SpawnExe(f), nil
}

// AppBundle is `caboose __app-bundle` (make app): it assembles Caboose.app
// around this executable at its one fixed place,
// CABOOSE_HOME/app-dev/Caboose.app (appbundle.CheckoutPath), always with
// the checkout's identifier, whatever this build's version stamp says,
// and prints the bundle's path. The app judges that place itself at every
// launch (checkAppPlace).
func (a *App) AppBundle(args []string) error {
	if len(args) != 0 {
		return Die("usage: caboose __app-bundle")
	}
	if a.Cfg.CabooseHome == "" || !filepath.IsAbs(a.Cfg.CabooseHome) {
		return Die("CABOOSE_HOME (or HOME) is not set to an absolute path: the bundle has no place")
	}
	exe, err := a.executable()
	if err != nil {
		return Die("%v", err)
	}
	dest := appbundle.CheckoutPath(a.Cfg.CabooseHome)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return Die("%v", err)
	}
	if err := appbundle.Assemble(dest, exe, appBundleInfo()); err != nil {
		return Die("%v", err)
	}
	fmt.Fprintln(a.Stdout, dest)
	return nil
}

// appBundleInfo is what __app-bundle writes in the Info.plist: the
// checkout's identifier, always, until releases build a bundle of their
// own.
func appBundleInfo() appbundle.Info {
	return appbundle.Info{ID: appbundle.CheckoutID, Version: currentVersion()}
}

// appLine is status's "app       :" value, from the daemon's own account.
func appLine(v linkView) string {
	switch {
	case v.Daemon == nil:
		return "unknown (the daemon is not running or does not answer)"
	case v.Daemon.Daemon.App == nil:
		return "unknown (this daemon does not say; run: caboose daemon restart)"
	case !v.Daemon.Daemon.App.Attached:
		return "not running"
	}
	return "attached" + appDetail(v.Daemon.Daemon.App)
}

// appDetail is " (pid N, VERSION)", for what the daemon knows.
func appDetail(ai *daemonproto.AppInfo) string {
	var parts []string
	if ai.PID > 0 {
		parts = append(parts, fmt.Sprintf("pid %d", ai.PID))
	}
	if v := proposal.Printable(daemonproto.Text(ai.Version, 64)); v != "" {
		parts = append(parts, v)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// doctorApp is doctor's note of the menu-bar app; never a problem, since
// the app is optional.
func doctorApp(c *checkup, v linkView) {
	switch {
	case v.Daemon == nil, v.Daemon.Daemon.App == nil:
		return // the daemon's row says why
	case v.Daemon.Daemon.App.Attached:
		c.note("menu-bar app", "attached%s", appDetail(v.Daemon.Daemon.App))
	default:
		c.note("menu-bar app", "not running (notifications and the URL question use osascript)")
	}
}
