package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/appbundle"
)

func TestAppArgs(t *testing.T) {
	plist := appbundle.Plist(appbundle.Info{ID: appbundle.CheckoutID, Version: "v1.0.0"})
	read := func(p string) ([]byte, error) {
		if p == "/A/Caboose.app/Contents/Info.plist" {
			return plist, nil
		}
		return nil, fs.ErrNotExist
	}
	exe := "/A/Caboose.app/Contents/MacOS/caboose"
	for _, c := range []struct {
		exe        string
		argv, want []string
	}{
		{exe, nil, []string{"app"}},
		{exe, []string{"-psn_0_42"}, []string{"app"}},
		{exe, []string{"app", "--selftest"}, []string{"app", "--selftest"}},
		{exe, []string{"status"}, []string{"status"}},
		{"/usr/local/bin/caboose", nil, nil},
		{"/B/Caboose.app/Contents/MacOS/caboose", nil, nil},
		{"/usr/local/bin/caboose", []string{"-psn_0_42"}, []string{"-psn_0_42"}},
	} {
		if got := appArgs(c.exe, c.argv, read); !slices.Equal(got, c.want) {
			t.Errorf("appArgs(%q, %q) = %q, want %q", c.exe, c.argv, got, c.want)
		}
	}
}

// caboose app is the Mac's; anywhere else it says what to use instead.
// Like daemon, it needs no environment: a config.toml that cannot be read
// is not what it reports.
func TestAppElsewhere(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("the app runs here")
	}
	home := sandboxEnv(t)
	dir := filepath.Join(home, ".caboose", "envs", "default")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("nope = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{{"app"}, {"app", "--selftest"}} {
		code, out, errs := runIt(argv...)
		if code != 1 || out != "" || errs != "caboose: the menu-bar app is macOS only; `caboose ui` opens the web UI\n" {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", argv, code, out, errs)
		}
	}
}

// __app-bundle assembles the bundle around this binary, with the
// checkout's identifier, and needs no environment either.
func TestAppBundleCommand(t *testing.T) {
	home := sandboxEnv(t)
	dir := filepath.Join(home, ".caboose", "envs", "default")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("nope = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Its one place: CABOOSE_HOME/app-dev, never where it is run from.
	app := filepath.Join(home, ".caboose", "app-dev", "Caboose.app")
	if code, out, errs := runIt("__app-bundle"); code != 0 || out != app+"\n" || errs != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	b, err := appbundle.ReadPlist(filepath.Join(app, "Contents", "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if id, err := appbundle.ParseIdentifier(b); err != nil || id != appbundle.CheckoutID {
		t.Errorf("identifier %q, %v", id, err)
	}
	fi, err := os.Stat(filepath.Join(app, "Contents", "MacOS", "caboose"))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("executable %v, %v", fi, err)
	}
	if code, _, errs := runIt("__app-bundle", filepath.Join(t.TempDir(), "Caboose.app")); code != 1 || !strings.Contains(errs, "usage: caboose __app-bundle [--release VERSION --exe PATH DIR]\n") {
		t.Errorf("a DIR: exit %d, %q", code, errs)
	}
}

// app and __app-bundle are left out of the short help.
func TestAppCommandsHidden(t *testing.T) {
	for _, name := range []string{"app", "__app-bundle"} {
		c, ok := lookup(name)
		if !ok || !c.Hidden {
			t.Errorf("%s: %+v", name, c)
		}
		if strings.Contains(ShortUsage(), "\n  "+name+" ") {
			t.Errorf("--help lists %s", name)
		}
	}
}
