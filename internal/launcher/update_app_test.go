package launcher

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/appbundle"
	"github.com/bfreis/caboose/internal/selfupdate"
	"github.com/bfreis/caboose/internal/selfupdate/releasetest"
)

const appSigner = appbundle.ReleaseID + " TEAM123456"

// withApp gives e's release host Caboose.app for v1.1.0, and the machine
// install.sh's of v1.0.0 (when installed), with the tests' ditto and
// codesign.
func withApp(t *testing.T, e *updateEnv, installed bool) selfupdate.App {
	t.Helper()
	pinAppSupported(t, true)
	e.srv.Add("v1.1.0", selfupdate.AppAssetName("v1.1.0", runtime.GOARCH), releasetest.AppZip(t, appbundle.ReleaseID, "v1.1.0", appSigner))
	app := selfupdate.App{Path: appbundle.InstallPath(e.a.Cfg.Home), Unzip: releasetest.Unzip, Codesign: releasetest.Codesign}
	e.a.installedApp = func() selfupdate.App { return app }
	if installed {
		zip := filepath.Join(t.TempDir(), "app.zip")
		if err := os.WriteFile(zip, releasetest.AppZip(t, appbundle.ReleaseID, "v1.0.0", appSigner), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := releasetest.Unzip(context.Background(), zip, filepath.Dir(app.Path)); err != nil {
			t.Fatal(err)
		}
	}
	return app
}

func appVersion(t *testing.T, app selfupdate.App) string {
	t.Helper()
	v, err := app.Version()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// An update brings install.sh's Caboose.app along, in the background and
// in the foreground.
func TestUpdateReplacesTheApp(t *testing.T) {
	e := newUpdateEnv(t)
	app := withApp(t, e, true)
	if err := e.a.Update([]string{"--background"}); err != nil {
		t.Fatal(err)
	}
	if st := e.state(); st.To != "v1.1.0" || st.AppError != "" {
		t.Errorf("state %+v", st)
	}
	if v := appVersion(t, app); v != "v1.1.0" {
		t.Errorf("the app is %s", v)
	}

	e = newUpdateEnv(t)
	app = withApp(t, e, true)
	if err := e.a.Update(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	if v := appVersion(t, app); v != "v1.1.0" {
		t.Errorf("the app is %s", v)
	}
	if !strings.Contains(e.out.String(), "Updated Caboose.app to v1.1.0, in "+app.Path) {
		t.Errorf("said:\n%s", e.out)
	}
}

// An app left behind (its download failed last time) is brought up to the
// latest release by the next check, though the command is current.
func TestUpdateCatchesTheAppUp(t *testing.T) {
	e := newUpdateEnv(t)
	app := withApp(t, e, true)
	e.version("v1.1.0")
	if err := e.a.Update(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	if v := appVersion(t, app); v != "v1.1.0" {
		t.Errorf("the app is %s", v)
	}
	out := e.out.String()
	if !strings.Contains(out, "caboose v1.1.0 is the latest release.") || !strings.Contains(out, "Updated Caboose.app to v1.1.0") {
		t.Errorf("said:\n%s", out)
	}
}

// No Caboose.app of install.sh's, no app update, and nothing said of one.
func TestUpdateWithoutTheApp(t *testing.T) {
	e := newUpdateEnv(t)
	app := withApp(t, e, false)
	if err := e.a.Update(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	if _, err := os.Stat(app.Path); !os.IsNotExist(err) {
		t.Errorf("made %s: %v", app.Path, err)
	}
	if strings.Contains(e.out.String(), "Caboose.app") {
		t.Errorf("said:\n%s", e.out)
	}
	// And never off a Mac, whatever is there.
	e = newUpdateEnv(t)
	app = withApp(t, e, true)
	pinAppSupported(t, false)
	if err := e.a.Update([]string{"--background"}); err != nil {
		t.Fatal(err)
	}
	if v := appVersion(t, app); v != "v1.0.0" {
		t.Errorf("the app is %s", v)
	}
}

// An app that cannot be updated leaves the command updated, says why, and
// records it.
func TestUpdateAppFails(t *testing.T) {
	e := newUpdateEnv(t)
	app := withApp(t, e, true)
	e.srv.Replace("v1.1.0", selfupdate.AppAssetName("v1.1.0", runtime.GOARCH), []byte("not it"))
	err := e.a.Update(nil)
	if err == nil || !strings.Contains(err.Error(), "updating Caboose.app to v1.1.0") {
		t.Errorf("Update = %v", err)
	}
	if e.current() != "v1.1.0" {
		t.Errorf("the command is %s", e.current())
	}
	if v := appVersion(t, app); v != "v1.0.0" {
		t.Errorf("the app is %s", v)
	}
	if st := e.state(); !strings.Contains(st.AppError, "checksum mismatch") {
		t.Errorf("state %+v", st)
	}
	if !strings.Contains(e.out.String(), "Caboose.app stays as it was") {
		t.Errorf("said:\n%s", e.out)
	}

	e = newUpdateEnv(t)
	withApp(t, e, true)
	e.srv.Replace("v1.1.0", selfupdate.AppAssetName("v1.1.0", runtime.GOARCH), []byte("not it"))
	if err := e.a.Update([]string{"--background"}); err != nil {
		t.Fatal(err)
	}
	if st := e.state(); st.To != "v1.1.0" || st.Error != "" || !strings.Contains(st.AppError, "checksum mismatch") {
		t.Errorf("state %+v", st)
	}
}
