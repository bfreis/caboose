package caboose

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/bfreis/caboose/internal/appbundle"
	"github.com/bfreis/caboose/internal/selfupdate"
	"github.com/bfreis/caboose/internal/selfupdate/releasetest"
)

// macTools are stand-ins for what install.sh runs on a Mac, first on its
// PATH: uname says Darwin on arm64; ditto unpacks only the zip the test
// published (FAKE_APP_ZIP), as the tree in FAKE_APP_TREE; plutil reads a
// string key; codesign fails with FAKE_CODESIGN_FAIL set; open records
// what it was asked in FAKE_OPEN_LOG. None of the real ones ever runs,
// on a Mac either.
var macTools = map[string]string{
	"uname": `case "$1" in -s) echo Darwin ;; -m) echo arm64 ;; *) echo Darwin ;; esac`,
	"ditto": `[ "$1 $2" = "-x -k" ] && [ $# -eq 4 ] || exit 2
cmp -s "$3" "$FAKE_APP_ZIP" || { echo "ditto: not the published zip" >&2; exit 1; }
mkdir -p "$4" && cp -R "$FAKE_APP_TREE/." "$4/"`,
	"plutil": `[ "$1 $3 $4 $5" = "-extract raw -o -" ] || exit 2
v=$(sed -n "/<key>$2<\/key>/{n;s/.*<string>\(.*\)<\/string>.*/\1/p;}" "$6" 2>/dev/null)
[ -n "$v" ] || exit 1
echo "$v"`,
	"codesign": `[ "$1 $2 $3" = "--verify --deep --strict" ] || exit 2
[ -z "$FAKE_CODESIGN_FAIL" ]`,
	"open": `echo "$@" >> "$FAKE_OPEN_LOG"`,
}

// macInstall is a Mac to run install.sh on, and a release host with
// v1.1.0 for darwin/arm64, the app's zip in it.
type macInstall struct {
	t     *testing.T
	home  string
	srv   *releasetest.Server
	env   []string
	opens string
}

func newMacInstall(t *testing.T) *macInstall {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	bin := fakeDocker(t)
	for name, body := range macTools {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m := &macInstall{t: t, home: t.TempDir(), srv: releasetest.New(t)}
	m.opens = filepath.Join(t.TempDir(), "opens")
	m.srv.Publish(t, "v1.1.0", true, "darwin/arm64")
	zip := releasetest.AppZip(t, appbundle.ReleaseID, "v1.1.0", appbundle.ReleaseID+" TEAM123456")
	m.srv.Add("v1.1.0", selfupdate.AppAssetName("v1.1.0", "arm64"), zip)
	zipFile := filepath.Join(t.TempDir(), "app.zip")
	if err := os.WriteFile(zipFile, zip, 0o644); err != nil {
		t.Fatal(err)
	}
	tree := t.TempDir()
	if err := releasetest.Unzip(context.Background(), zipFile, tree); err != nil {
		t.Fatal(err)
	}
	m.env = []string{"HOME=" + m.home, "PATH=" + bin + ":" + os.Getenv("PATH"), "CABOOSE_RELEASES_URL=" + m.srv.Base,
		"CABOOSE_NO_SETUP=1", "FAKE_APP_ZIP=" + zipFile, "FAKE_APP_TREE=" + tree, "FAKE_OPEN_LOG=" + m.opens}
	return m
}

func (m *macInstall) run(env ...string) (string, error) {
	m.t.Helper()
	cmd := exec.Command("sh", "install.sh")
	cmd.Env = append(append([]string{}, m.env...), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (m *macInstall) app() string { return appbundle.InstallPath(m.home) }

// appVersion is the CabooseVersion of the bundle at app, or "".
func appVersion(app string) string {
	b, err := appbundle.ReadPlist(filepath.Join(app, "Contents", "Info.plist"))
	if err != nil {
		return ""
	}
	v, _ := appbundle.ParseString(b, "CabooseVersion")
	return v
}

func (m *macInstall) opened() string {
	b, _ := os.ReadFile(m.opens)
	return string(b)
}

// Only the bundle is left in ~/Applications: no stage, no old one.
func (m *macInstall) onlyTheApp() {
	m.t.Helper()
	es, _ := os.ReadDir(filepath.Dir(m.app()))
	for _, e := range es {
		if e.Name() != appbundle.Name {
			m.t.Errorf("left in ~/Applications: %s", e.Name())
		}
	}
}

func TestInstallScriptApp(t *testing.T) {
	m := newMacInstall(t)
	out, err := m.run()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if v := appVersion(m.app()); v != "v1.1.0" {
		t.Errorf("the app is %q\n%s", v, out)
	}
	for _, want := range []string{
		"caboose: installed the menu-bar app in " + m.app() + "\n",
		"caboose: opening the menu-bar app\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if got := m.opened(); got != m.app()+"\n" {
		t.Errorf("opened %q", got)
	}
	m.onlyTheApp()
	// What install.sh puts there, an update takes as its own.
	if v, err := (selfupdate.App{Path: m.app()}).Version(); v != "v1.1.0" || err != nil {
		t.Errorf("the updater reads %q, %v", v, err)
	}
}

// An older one of caboose's is replaced.
func TestInstallScriptAppReplaces(t *testing.T) {
	m := newMacInstall(t)
	old := filepath.Join(m.app(), "Contents")
	if err := os.MkdirAll(filepath.Join(old, "MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "Info.plist"), appbundle.Plist(appbundle.Info{ID: appbundle.ReleaseID, Version: "v1.0.0"}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "MacOS", "stale"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := m.run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if v := appVersion(m.app()); v != "v1.1.0" {
		t.Errorf("the app is %q", v)
	}
	if _, err := os.Stat(filepath.Join(old, "MacOS", "stale")); !os.IsNotExist(err) {
		t.Errorf("the old bundle's file is still there: %v", err)
	}
	m.onlyTheApp()
}

// Whatever keeps the app from being installed, caboose is installed all
// the same, it says why, and opens nothing.
func TestInstallScriptAppNot(t *testing.T) {
	for name, c := range map[string]struct {
		setup func(m *macInstall)
		env   []string
		want  string
		kept  string // what stays at the app's place
	}{
		"another app there": {setup: func(m *macInstall) {
			c := filepath.Join(m.app(), "Contents")
			if err := os.MkdirAll(c, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(c, "Info.plist"), appbundle.Plist(appbundle.Info{ID: "com.example.other", Version: "v9.0.0"}), 0o644); err != nil {
				t.Fatal(err)
			}
		}, want: "is not caboose's menu-bar app: it is left as it is", kept: "v9.0.0"},
		"a checkout's app there": {setup: func(m *macInstall) {
			c := filepath.Join(m.app(), "Contents")
			if err := os.MkdirAll(c, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(c, "Info.plist"), appbundle.Plist(appbundle.Info{ID: appbundle.CheckoutID, Version: "v0.0.1"}), 0o644); err != nil {
				t.Fatal(err)
			}
		}, want: "is not caboose's menu-bar app", kept: "v0.0.1"},
		"a symlink there": {setup: func(m *macInstall) {
			if err := os.MkdirAll(filepath.Dir(m.app()), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), m.app()); err != nil {
				t.Fatal(err)
			}
		}, want: "is not caboose's menu-bar app"},
		"not signed": {env: []string{"FAKE_CODESIGN_FAIL=1"}, want: "is not validly signed"},
		"checksum": {setup: func(m *macInstall) {
			m.srv.Replace("v1.1.0", selfupdate.AppAssetName("v1.1.0", "arm64"), []byte("evil"))
		}, want: "does not match its checksum"},
		"not in the checksums": {setup: func(m *macInstall) {
			m.srv.Publish(t, "v1.1.0", true, "darwin/arm64")
		}, want: "checksums.txt of v1.1.0 lists no caboose-app_1.1.0_darwin_arm64.zip"},
	} {
		t.Run(name, func(t *testing.T) {
			m := newMacInstall(t)
			if c.setup != nil {
				c.setup(m)
			}
			out, err := m.run(c.env...)
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if !strings.Contains(out, "caboose: the menu-bar app was not installed: ") || !strings.Contains(out, c.want) {
				t.Errorf("no %q in:\n%s", c.want, out)
			}
			if !strings.Contains(out, "caboose: installed caboose v1.1.0 in ") {
				t.Errorf("caboose not installed:\n%s", out)
			}
			if got := appVersion(m.app()); got != c.kept {
				t.Errorf("the app's place holds %q, want %q", got, c.kept)
			}
			if got := m.opened(); got != "" {
				t.Errorf("opened %q", got)
			}
			if es, _ := os.ReadDir(filepath.Dir(m.app())); len(es) > 1 {
				t.Errorf("left in ~/Applications: %v", es)
			}
		})
	}
}

// CABOOSE_NO_APP: no app, not even downloaded; and never off a Mac.
func TestInstallScriptNoApp(t *testing.T) {
	m := newMacInstall(t)
	out, err := m.run("CABOOSE_NO_APP=1")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out, "menu-bar app") || m.opened() != "" {
		t.Errorf("said or opened something of the app:\n%s", out)
	}
	if _, err := os.Lstat(filepath.Dir(m.app())); !os.IsNotExist(err) {
		t.Errorf("made ~/Applications: %v", err)
	}
	for _, h := range m.srv.Hits() {
		if strings.Contains(h, "caboose-app_") {
			t.Errorf("downloaded %s", h)
		}
	}
}

// On a Mac, with its own ditto, plutil and codesign (open alone stubbed):
// install.sh installs an ad hoc release's app, and replaces it.
func TestInstallScriptAppMacTools(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	zip := releasetest.MacAppZip(t, "v1.1.0")
	bin := fakeDocker(t)
	opens := filepath.Join(t.TempDir(), "opens")
	if err := os.WriteFile(filepath.Join(bin, "open"), []byte("#!/bin/sh\necho \"$@\" >> \"$FAKE_OPEN_LOG\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := releasetest.New(t)
	s.Publish(t, "v1.1.0", true, runtime.GOOS+"/"+runtime.GOARCH)
	s.Add("v1.1.0", selfupdate.AppAssetName("v1.1.0", runtime.GOARCH), zip)
	home := t.TempDir()
	for range 2 {
		cmd := exec.Command("sh", "install.sh")
		cmd.Env = []string{"HOME=" + home, "PATH=" + bin + ":" + os.Getenv("PATH"), "CABOOSE_RELEASES_URL=" + s.Base,
			"CABOOSE_NO_SETUP=1", "FAKE_OPEN_LOG=" + opens}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "caboose: installed the menu-bar app in ") {
			t.Fatalf("%v\n%s", err, out)
		}
	}
	app := appbundle.InstallPath(home)
	if v := appVersion(app); v != "v1.1.0" {
		t.Errorf("the app is %q", v)
	}
	if out, err := exec.Command("/usr/bin/codesign", "--verify", "--deep", "--strict", app).CombinedOutput(); err != nil {
		t.Errorf("does not verify: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(opens); string(b) != app+"\n"+app+"\n" {
		t.Errorf("opened %q", b)
	}
}
