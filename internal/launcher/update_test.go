package launcher

import (
	"bytes"
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/selfupdate"
	"github.com/bfreis/caboose/internal/selfupdate/releasetest"
)

// updateEnv is a machine with caboose v1.0.0 installed by install.sh's
// layout, running from it, and a release host with v1.0.0 and v1.1.0.
type updateEnv struct {
	t       *testing.T
	a       *App
	srv     *releasetest.Server
	out     *bytes.Buffer
	err     *bytes.Buffer
	env     map[string]string
	spawned [][]string
	now     time.Time
}

func newUpdateEnv(t *testing.T) *updateEnv {
	t.Helper()
	home := t.TempDir()
	srv := releasetest.New(t)
	plat := goosArch()
	srv.Publish(t, "v1.0.0", false, plat)
	srv.Publish(t, "v1.1.0", true, plat)
	e := &updateEnv{t: t, srv: srv, out: &bytes.Buffer{}, err: &bytes.Buffer{},
		env: map[string]string{"CABOOSE_RELEASES_URL": srv.Base}, now: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)}
	l := selfupdate.DefaultLayout(home)
	if err := l.Install(context.Background(), selfupdate.Source{Base: srv.Base}, "v1.0.0", runtime.GOOS, runtime.GOARCH, ""); err != nil {
		t.Fatal(err)
	}
	e.a = &App{
		Cfg: &config.Config{Home: home, CabooseHome: filepath.Join(home, ".caboose"),
			Getenv: func(k string) string { return e.env[k] }},
		Stdout: e.out, Stderr: e.err,
		Executable: func() (string, error) { return l.Link, nil },
		Spawn: func(exe string, args ...string) error {
			e.spawned = append(e.spawned, append([]string{exe}, args...))
			return nil
		},
		Now: func() time.Time { return e.now },
	}
	e.version("v1.0.0")
	return e
}

func (e *updateEnv) version(v string) {
	old := currentVersion
	currentVersion = func() string { return v }
	e.t.Cleanup(func() { currentVersion = old })
}

func (e *updateEnv) state() selfupdate.State { return selfupdate.ReadState(e.a.Cfg.CabooseHome) }

func (e *updateEnv) current() string { return e.a.layout().Current() }

func TestAutoUpdateStartsOnceADay(t *testing.T) {
	e := newUpdateEnv(t)
	e.a.AutoUpdate()
	e.a.AutoUpdate()
	if len(e.spawned) != 1 || strings.Join(e.spawned[0][1:], " ") != "update --background" {
		t.Fatalf("spawned %v", e.spawned)
	}
	if got, _ := filepath.EvalSymlinks(e.spawned[0][0]); !strings.HasSuffix(got, filepath.Join("v1.0.0", "caboose")) {
		t.Errorf("spawned %s", got)
	}
	e.now = e.now.Add(23 * time.Hour)
	e.a.AutoUpdate()
	e.now = e.now.Add(2 * time.Hour)
	e.a.AutoUpdate()
	if len(e.spawned) != 2 {
		t.Errorf("spawned %d times over 25h, want 2", len(e.spawned))
	}
	if e.err.Len() != 0 {
		t.Errorf("said:\n%s", e.err)
	}
}

// Never for an install that does not update itself, or with the variable.
func TestAutoUpdateLeavesAlone(t *testing.T) {
	for name, setup := range map[string]func(e *updateEnv){
		"off":         func(e *updateEnv) { e.env["CABOOSE_NO_AUTO_UPDATE"] = "1" },
		"dev build":   func(e *updateEnv) { e.version("dev") },
		"a checkout":  func(e *updateEnv) { e.a.Checkout = "/src/caboose" },
		"not managed": func(e *updateEnv) { e.a.Executable = func() (string, error) { return "/usr/local/bin/caboose", nil } },
	} {
		t.Run(name, func(t *testing.T) {
			e := newUpdateEnv(t)
			setup(e)
			e.a.AutoUpdate()
			if len(e.spawned) != 0 {
				t.Errorf("spawned %v", e.spawned)
			}
		})
	}
	e := newUpdateEnv(t)
	e.env["CABOOSE_NO_AUTO_UPDATE"] = "0"
	if e.a.AutoUpdate(); len(e.spawned) != 1 {
		t.Error("CABOOSE_NO_AUTO_UPDATE=0 turned updates off")
	}
}

// The background update installs the newer release, and the first run of it
// says so, once.
func TestUpdateInBackground(t *testing.T) {
	e := newUpdateEnv(t)
	if err := e.a.Update([]string{"--background"}); err != nil {
		t.Fatal(err)
	}
	if e.current() != "v1.1.0" {
		t.Errorf("current %q", e.current())
	}
	st := e.state()
	if st.From != "v1.0.0" || st.To != "v1.1.0" || st.Announced || st.Latest != "v1.1.0" || st.Error != "" || !st.Checked.Equal(e.now) {
		t.Errorf("state %+v", st)
	}
	if e.out.Len()+e.err.Len() != 0 {
		t.Errorf("said: %q %q", e.out, e.err)
	}

	// Still the old one running: nothing to announce yet.
	e.a.AutoUpdate()
	if e.err.Len() != 0 {
		t.Errorf("said:\n%s", e.err)
	}
	e.version("v1.1.0")
	e.a.AutoUpdate()
	e.a.AutoUpdate()
	if got := e.err.String(); got != "caboose: updated to v1.1.0 (from v1.0.0)\n" {
		t.Errorf("said %q", got)
	}

	// Up to date now: a check changes nothing.
	e.a.Executable = func() (string, error) { return filepath.Join(e.a.layout().Versions, "v1.1.0", "caboose"), nil }
	if err := e.a.Update([]string{"--background"}); err != nil {
		t.Fatal(err)
	}
	if st := e.state(); st.To != "v1.1.0" || !st.Announced || st.Error != "" {
		t.Errorf("state %+v", st)
	}
}

// A failure is recorded, for doctor, and changes nothing.
func TestUpdateInBackgroundFails(t *testing.T) {
	e := newUpdateEnv(t)
	e.env["CABOOSE_RELEASES_URL"] = e.srv.Base + "/nothing-here"
	if err := e.a.Update([]string{"--background"}); err != nil {
		t.Fatal(err)
	}
	if st := e.state(); st.Error == "" || st.To != "" || e.current() != "v1.0.0" {
		t.Errorf("state %+v, current %s", st, e.current())
	}
	if l, text := e.a.updateSummary(); l != levelNote || !strings.Contains(text, "the last update check (just now) failed") {
		t.Errorf("summary %v %q", l, text)
	}
}

func TestUpdateNow(t *testing.T) {
	e := newUpdateEnv(t)
	if err := e.a.Update(nil); err != nil {
		t.Fatal(err)
	}
	if e.current() != "v1.1.0" || !strings.Contains(e.out.String(), "✓ Updated caboose: v1.0.0 → v1.1.0") {
		t.Errorf("current %s, said:\n%s", e.current(), e.out)
	}
	if st := e.state(); !st.Announced {
		t.Errorf("announced again later: %+v", st)
	}

	e.out.Reset()
	e.version("v1.1.0")
	if err := e.a.Update(nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "caboose v1.1.0 is the latest release.") {
		t.Errorf("said:\n%s", e.out)
	}
}

func TestUpdateNowRefuses(t *testing.T) {
	e := newUpdateEnv(t)
	e.version("v1.0.0-3-gabcdef-dirty")
	if err := e.a.Update(nil); err == nil || !strings.Contains(err.Error(), "is a development build") {
		t.Errorf("dev: %v", err)
	}
	e.version("v1.0.0")
	e.a.Executable = func() (string, error) { return "/opt/caboose/caboose", nil }
	if err := e.a.Update(nil); err == nil || !strings.Contains(err.Error(), "was not installed by install.sh") ||
		!strings.Contains(err.Error(), installScriptURL) {
		t.Errorf("not managed: %v", err)
	}
	e = newUpdateEnv(t)
	unlock, err := selfupdate.Lock(e.a.Cfg.CabooseHome)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := e.a.Update(nil); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("locked: %v", err)
	}
	if err := e.a.Update([]string{"--now"}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("bad flag: %v", err)
	}
}

func TestUpdateSummary(t *testing.T) {
	e := newUpdateEnv(t)
	check := func(want level, text string) {
		t.Helper()
		if l, got := e.a.updateSummary(); l != want || !strings.Contains(got, text) {
			t.Errorf("got %v %q, want %v %q", l, got, want, text)
		}
	}
	check(levelOK, "v1.0.0, updates itself (not checked yet)")
	if err := selfupdate.WriteState(e.a.Cfg.CabooseHome, selfupdate.State{Checked: e.now.Add(-3 * time.Hour), Latest: "v1.0.0"}); err != nil {
		t.Fatal(err)
	}
	check(levelOK, "v1.0.0, updates itself (last checked 3h ago)")
	if err := selfupdate.WriteState(e.a.Cfg.CabooseHome, selfupdate.State{Checked: e.now, Latest: "v1.1.0", Error: "no space"}); err != nil {
		t.Fatal(err)
	}
	check(levelNote, "v1.1.0 is out")
	e.env["CABOOSE_NO_AUTO_UPDATE"] = "1"
	check(levelNote, "automatic updates are off")
	e.version("dev")
	check(levelOK, "a development build")
	e.version("v1.0.0")
	e.a.Executable = func() (string, error) { return "/opt/caboose/caboose", nil }
	check(levelNote, "not installed by install.sh (/opt/caboose/caboose)")
}

// install.sh's layout and DefaultLayout must agree; the script is tested
// against it in the root package.
func TestLayoutUnderHome(t *testing.T) {
	l := selfupdate.DefaultLayout("/h")
	if l.Link != "/h/.local/bin/caboose" || l.Versions != "/h/.local/share/caboose/versions" {
		t.Errorf("%+v", l)
	}
}

func goosArch() string { return runtime.GOOS + "/" + runtime.GOARCH }
