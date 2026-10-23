package launcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/appbundle"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
)

func pinAppSupported(t *testing.T, on bool) {
	t.Helper()
	old := appSupported
	appSupported = on
	t.Cleanup(func() { appSupported = old })
}

func pinVersion(t *testing.T, v string) {
	t.Helper()
	old := currentVersion
	currentVersion = func() string { return v }
	t.Cleanup(func() { currentVersion = old })
}

// appApp is an App whose executable is exe, under a home in a temp dir.
func appApp(t *testing.T, exe string) *App {
	t.Helper()
	// Physical, as the app's place check wants; short, for the daemon's
	// socket under it.
	home := shortTempDir(t)
	return &App{
		Cfg:        &config.Config{Home: home, CabooseHome: filepath.Join(home, ".caboose"), Getenv: func(string) string { return "" }},
		Executable: func() (string, error) { return exe, nil },
		Spawn:      func(exe string, args ...string) error { t.Errorf("spawned %s %v", exe, args); return nil },
	}
}

// bundle makes a bundle with identifier id under dir, returning its
// executable.
func bundle(t *testing.T, dir, id string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "src-caboose")
	if err := os.WriteFile(src, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(dir, appbundle.Name)
	if id == appbundle.CheckoutID {
		if err := appbundle.Assemble(app, src, appbundle.Info{ID: id, Version: "v1.2.3"}); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := os.MkdirAll(filepath.Join(app, "Contents", "MacOS"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), appbundle.Plist(appbundle.Info{ID: id, Version: "v1.2.3"}), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(app, "Contents", "MacOS", "caboose"), []byte("bin"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(app, "Contents", "MacOS", "caboose")
}

// A checkout's Caboose.app is a checkout's build whatever its version
// stamp: a release-looking one included, with no checkout beside it.
func TestInstalledHonoursTheCheckoutBundle(t *testing.T) {
	pinVersion(t, "v1.2.3")
	d := t.TempDir()
	a := appApp(t, bundle(t, d, appbundle.CheckoutID))
	if k := a.installed().kind; k != installDev {
		t.Errorf("checkout bundle: kind %v", k)
	}
	if !strings.HasSuffix(a.daemonDir(), "daemon-dev") {
		t.Errorf("daemon dir %s", a.daemonDir())
	}
	other := appApp(t, bundle(t, filepath.Join(d, "o"), "com.example.other"))
	if k := other.installed().kind; k == installDev {
		t.Error("another identifier's bundle counted as a checkout's")
	}
	plain := appApp(t, filepath.Join(d, "caboose"))
	if k := plain.installed().kind; k == installDev {
		t.Error("a release outside any bundle counted as a checkout's")
	}
}

// installLayout makes install.sh's layout under a's home with tag current.
func installLayout(t *testing.T, a *App, tag string) string {
	t.Helper()
	l := a.layout()
	bin := filepath.Join(l.Versions, tag, "caboose")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(l.Link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bin, l.Link); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestAppSpawnExe(t *testing.T) {
	own := "/Applications/Caboose.app/Contents/MacOS/caboose"
	for _, tc := range []struct {
		name, version, installed string
		checkout, wantInstalled  bool
	}{
		{"release, CLI newer", "v1.2.0", "v1.3.0", false, true},
		{"release, CLI same", "v1.2.0", "v1.2.0", false, true},
		{"release, CLI older", "v1.2.0", "v1.1.9", false, false},
		{"release, no CLI", "v1.2.0", "", false, false},
		{"checkout, CLI newer", "v1.2.0-3-gabcdef0", "v9.0.0", false, false},
		{"checkout next to the source", "v1.2.0", "v1.3.0", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pinVersion(t, tc.version)
			a := appApp(t, own)
			if tc.checkout {
				a.Checkout = t.TempDir()
			}
			want := own
			if tc.installed != "" {
				bin := installLayout(t, a, tc.installed)
				if tc.wantInstalled {
					want = bin
				}
			}
			got, err := a.appSpawnExe()
			if err != nil || got != want {
				t.Errorf("appSpawnExe = %q, %v; want %q", got, err, want)
			}
		})
	}
	// A checkout's bundle spawns itself, whatever its stamp and the CLI.
	pinVersion(t, "v1.2.0")
	d := t.TempDir()
	exe := bundle(t, d, appbundle.CheckoutID)
	a := appApp(t, exe)
	installLayout(t, a, "v1.3.0")
	if got, _ := a.appSpawnExe(); got != exe {
		t.Errorf("checkout bundle spawned %q", got)
	}
	// The layout's binary gone: the app's own.
	b := appApp(t, own)
	bin := installLayout(t, b, "v1.3.0")
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.appSpawnExe(); got != own {
		t.Errorf("missing CLI binary: spawned %q", got)
	}
}

// The app's daemon config: kind app, never replacing, spawning through
// appSpawnExe as the CLI does (detached, MinimalEnv).
func TestAppDaemonConfig(t *testing.T) {
	pinVersion(t, "v1.2.0")
	own := "/Applications/Caboose.app/Contents/MacOS/caboose"
	a := appApp(t, own)
	bin := installLayout(t, a, "v1.3.0")
	var spawned []string
	a.Spawn = func(exe string, args ...string) error {
		spawned = append(spawned, exe+" "+strings.Join(args, " "))
		return nil
	}
	d := a.appDaemon()
	if d.cfg.Client != daemonproto.ClientApp || !d.cfg.NoReplace || d.cfg.Spawn == nil || d.cfg.Dir != a.daemonDir() {
		t.Fatalf("config %+v", d.cfg)
	}
	if err := d.cfg.Spawn(); err != nil || len(spawned) != 1 || spawned[0] != bin+" daemon" {
		t.Errorf("spawned %q, %v", spawned, err)
	}
	if _, ok := d.Replaced(); ok {
		t.Error("replaced.json read from nothing")
	}
	if err := os.MkdirAll(d.cfg.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.cfg.Dir, daemon.ReplacedFile), []byte(`{"version":"v2.0.0","protocol":7,"at":"2026-10-17T09:30:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r, ok := d.Replaced(); !ok || r.Version != "v2.0.0" || r.Protocol != 7 {
		t.Errorf("replaced %+v, %v", r, ok)
	}
	// Nothing answering: Connect says so, and is a nil Conn, not a typed
	// nil.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if c, err := d.Connect(ctx); c != nil || daemonproto.Kind(err) != daemonproto.KindUnavailable {
		t.Errorf("connect %v, %v", c, err)
	}
}

func TestAppElsewhereSaysSo(t *testing.T) {
	pinAppSupported(t, false)
	a := appApp(t, "/x/caboose")
	err := a.RunApp(nil)
	if ee, ok := err.(*ExitError); !ok || ee.Code != 1 || ee.Msg != appMacOnly {
		t.Errorf("%v", err)
	}
}

func TestAppRefusesArguments(t *testing.T) {
	pinAppSupported(t, true)
	a := appApp(t, "/x/caboose")
	if err := a.RunApp([]string{"--nope"}); err == nil || !strings.Contains(err.Error(), "usage: caboose app") {
		t.Errorf("%v", err)
	}
}

// __app-bundle writes the checkout's identifier whatever the build says
// of itself: a release's stamp, from a managed install.
func TestAppBundleAlwaysTheCheckoutIdentifier(t *testing.T) {
	for _, v := range []string{"v1.2.3", "v1.2.3-4-gabcdef0", "dev"} {
		pinVersion(t, v)
		d := t.TempDir()
		src := filepath.Join(d, "caboose")
		if err := os.WriteFile(src, []byte("bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		a := appApp(t, src)
		if got := appBundleInfo(); got.ID != appbundle.CheckoutID || got.Version != v {
			t.Errorf("%s: info %+v", v, got)
		}
		var out strings.Builder
		a.Stdout = &out
		if err := a.AppBundle(nil); err != nil {
			t.Fatal(err)
		}
		app := filepath.Join(a.Cfg.CabooseHome, "app-dev", appbundle.Name)
		if out.String() != app+"\n" {
			t.Errorf("%s: printed %q, want %q", v, out.String(), app)
		}
		b, err := appbundle.ReadPlist(filepath.Join(app, "Contents", "Info.plist"))
		if err != nil {
			t.Fatal(err)
		}
		if id, err := appbundle.ParseIdentifier(b); err != nil || id != appbundle.CheckoutID {
			t.Errorf("%s: identifier %q, %v", v, id, err)
		}
	}
}

func appState(ai *daemonproto.AppInfo) linkView {
	return linkView{Daemon: &daemonproto.StatusResult{Daemon: daemonproto.DaemonInfo{App: ai}}}
}

func TestAppLine(t *testing.T) {
	for _, tc := range []struct {
		v    linkView
		want string
	}{
		{appState(&daemonproto.AppInfo{Attached: true, PID: 4242, Version: "v1.2.3"}), "attached (pid 4242, v1.2.3)"},
		{appState(&daemonproto.AppInfo{Attached: true, PID: 7}), "attached (pid 7)"},
		{appState(&daemonproto.AppInfo{Attached: true, Version: "v1.2.3"}), "attached (v1.2.3)"},
		{appState(&daemonproto.AppInfo{Attached: true}), "attached"},
		{appState(&daemonproto.AppInfo{Attached: true, PID: 1, Version: "v1\x1b[31m\u202e"}), "attached (pid 1, v1?[31m?)"},
		{appState(&daemonproto.AppInfo{}), "not running"},
		{appState(nil), "unknown (this daemon does not say; run: caboose daemon restart)"},
		{linkView{}, "unknown (the daemon is not running or does not answer)"},
	} {
		if got := appLine(tc.v); got != tc.want {
			t.Errorf("appLine = %q, want %q", got, tc.want)
		}
	}
}

// status prints the app's line after web's, on macOS only, whether or not
// the sandbox runs.
func TestStatusAppLine(t *testing.T) {
	for _, on := range []bool{true, false} {
		for _, st := range []string{"running", "absent"} {
			b := newBoxApp(t, isolationContainer, runningBox(isolationContainer))
			pinAppSupported(t, on)
			b.box.Status = st
			b.daemon.Stat = &daemonproto.StatusResult{Daemon: daemonproto.DaemonInfo{PID: 9, Version: "v9",
				App: &daemonproto.AppInfo{Attached: true, PID: 31, Version: "v9"}}}
			if err := b.Status(); err != nil {
				t.Fatal(err)
			}
			out, _ := b.said()
			has := strings.Contains(out, "\napp       : attached (pid 31, v9)\n")
			if has != on {
				t.Errorf("on %v, %s: app line %v\n%s", on, st, has, out)
			}
			if on {
				web, app := strings.Index(out, "\nweb       : "), strings.Index(out, "\napp       : ")
				if web < 0 || app < web || strings.Count(out[web+1:app], "\n") != 0 {
					t.Errorf("app line not right after web's:\n%s", out)
				}
			}
		}
	}
}

func TestDoctorApp(t *testing.T) {
	for _, tc := range []struct {
		v    linkView
		want string
	}{
		{appState(&daemonproto.AppInfo{Attached: true, PID: 31, Version: "v9"}), "attached (pid 31, v9)"},
		{appState(&daemonproto.AppInfo{}), "not running (notifications and the URL question use osascript)"},
		{appState(nil), ""},
		{linkView{}, ""},
	} {
		c := &checkup{}
		doctorApp(c, tc.v)
		r := webRows(c, "menu-bar app")
		switch {
		case tc.want == "" && len(c.rows) != 0:
			t.Errorf("rows %+v", c.rows)
		case tc.want != "" && (len(r) != 1 || r[0].level != levelNote || r[0].text != tc.want):
			t.Errorf("rows %+v, want %q", c.rows, tc.want)
		}
	}
}

func TestDoctorAppRowOnlyWhereSupported(t *testing.T) {
	for _, on := range []bool{true, false} {
		pinAppSupported(t, on)
		fd := &fakeDaemon{Stat: &daemonproto.StatusResult{Daemon: daemonproto.DaemonInfo{PID: 7, Version: currentVersion(),
			App: &daemonproto.AppInfo{Attached: true, PID: 31, Version: "v9"}}}}
		a, _, _ := newCLIApp(t, fd)
		var n int
		for _, r := range doctorRows(a, false) {
			if r.label == "menu-bar app" {
				n++
			}
		}
		if want := map[bool]int{true: 1, false: 0}[on]; n != want {
			t.Errorf("supported %v: %d menu-bar app rows, want %d", on, n, want)
		}
	}
}
