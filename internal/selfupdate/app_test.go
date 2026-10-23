package selfupdate_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/appbundle"
	"github.com/bfreis/caboose/internal/selfupdate"
	"github.com/bfreis/caboose/internal/selfupdate/releasetest"
)

const team = "TEAM123456"

// installedApp writes a release's bundle of tag at ~/Applications in a
// new home, signed by signer, and returns its App.
func installedApp(t *testing.T, tag, signer string) selfupdate.App {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Applications")
	if err := releasetest.Unzip(context.Background(), writeZip(t, releasetest.AppZip(t, appbundle.ReleaseID, tag, signer)), dir); err != nil {
		t.Fatal(err)
	}
	return selfupdate.App{Path: filepath.Join(dir, appbundle.Name), Unzip: releasetest.Unzip, Codesign: releasetest.Codesign}
}

func writeZip(t *testing.T, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "app.zip")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func publishApp(t *testing.T, s *releasetest.Server, tag, id, signer string) {
	t.Helper()
	s.Publish(t, tag, true, "darwin/arm64")
	s.Add(tag, selfupdate.AppAssetName(tag, "arm64"), releasetest.AppZip(t, id, tag, signer))
}

func appInstall(a selfupdate.App, s *releasetest.Server, tag string) (bool, error) {
	return a.Install(context.Background(), selfupdate.Source{Base: s.Base}, tag, "arm64")
}

// leftovers are the names in the bundle's parent but the bundle's.
func leftovers(t *testing.T, a selfupdate.App) []string {
	t.Helper()
	es, err := os.ReadDir(filepath.Dir(a.Path))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		if e.Name() != appbundle.Name {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestAppAssetName(t *testing.T) {
	if got := selfupdate.AppAssetName("v1.2.3", "arm64"); got != "caboose-app_1.2.3_darwin_arm64.zip" {
		t.Errorf("AppAssetName = %q", got)
	}
}

func TestAppInstallReplacesAnOlderOne(t *testing.T) {
	s := releasetest.New(t)
	publishApp(t, s, "v1.1.0", appbundle.ReleaseID, appbundle.ReleaseID+" "+team)
	a := installedApp(t, "v1.0.0", appbundle.ReleaseID+" "+team)
	done, err := appInstall(a, s, "v1.1.0")
	if err != nil || !done {
		t.Fatalf("Install = %v, %v", done, err)
	}
	if v, err := a.Version(); v != "v1.1.0" || err != nil {
		t.Errorf("Version = %q, %v", v, err)
	}
	if got := read(t, filepath.Join(a.Path, "Contents", "MacOS", "caboose")); got != string(releasetest.Binary("v1.1.0")) {
		t.Errorf("the executable: %q", got)
	}
	if l := leftovers(t, a); l != nil {
		t.Errorf("left beside it: %q", l)
	}
	// Current now: nothing to do, nothing downloaded.
	before := len(s.Hits())
	if done, err := appInstall(a, s, "v1.1.0"); done || err != nil {
		t.Errorf("again: %v, %v", done, err)
	}
	if len(s.Hits()) != before {
		t.Errorf("downloaded again: %q", s.Hits()[before:])
	}
}

// Ad hoc stays ad hoc: a fork's releases replace each other.
func TestAppInstallAdHoc(t *testing.T) {
	s := releasetest.New(t)
	publishApp(t, s, "v1.1.0", appbundle.ReleaseID, appbundle.ReleaseID+" -")
	a := installedApp(t, "v1.0.0", appbundle.ReleaseID+" -")
	if done, err := appInstall(a, s, "v1.1.0"); err != nil || !done {
		t.Fatalf("Install = %v, %v", done, err)
	}
}

func TestAppInstallNone(t *testing.T) {
	s := releasetest.New(t)
	publishApp(t, s, "v1.1.0", appbundle.ReleaseID, appbundle.ReleaseID+" "+team)
	dir := t.TempDir()
	a := selfupdate.App{Path: filepath.Join(dir, appbundle.Name), Unzip: releasetest.Unzip, Codesign: releasetest.Codesign}
	if done, err := appInstall(a, s, "v1.1.0"); done || !errors.Is(err, selfupdate.ErrNoApp) {
		t.Errorf("Install = %v, %v", done, err)
	}
	if es, _ := os.ReadDir(dir); len(es) != 0 {
		t.Errorf("made %v", es)
	}
	// A checkout's bundle there is not a release's: left be.
	co := installedApp(t, "v1.0.0", appbundle.ReleaseID+" "+team)
	plist := filepath.Join(co.Path, "Contents", "Info.plist")
	if err := os.WriteFile(plist, appbundle.Plist(appbundle.Info{ID: appbundle.CheckoutID, Version: "v1.0.0"}), 0o644); err != nil {
		t.Fatal(err)
	}
	if done, err := appInstall(co, s, "v1.1.0"); done || !errors.Is(err, selfupdate.ErrNoApp) {
		t.Errorf("a checkout's bundle: %v, %v", done, err)
	}
}

// Whatever is wrong with the new bundle, the installed one stays as it
// was, and nothing is left beside it.
func TestAppInstallRefuses(t *testing.T) {
	signed := appbundle.ReleaseID + " " + team
	for name, c := range map[string]struct {
		publish func(t *testing.T, s *releasetest.Server)
		want    string
	}{
		"another team": {func(t *testing.T, s *releasetest.Server) {
			publishApp(t, s, "v1.1.0", appbundle.ReleaseID, appbundle.ReleaseID+" OTHERTEAM1")
		}, `signed by team "OTHERTEAM1", not "TEAM123456"`},
		"ad hoc": {func(t *testing.T, s *releasetest.Server) {
			publishApp(t, s, "v1.1.0", appbundle.ReleaseID, appbundle.ReleaseID+" -")
		}, `signed by team "", not "TEAM123456"`},
		"another identifier signed": {func(t *testing.T, s *releasetest.Server) {
			publishApp(t, s, "v1.1.0", appbundle.ReleaseID, "dev.example.other "+team)
		}, `signed as "dev.example.other"`},
		"a checkout's bundle": {func(t *testing.T, s *releasetest.Server) {
			publishApp(t, s, "v1.1.0", appbundle.CheckoutID, signed)
		}, "is not a release's Caboose.app"},
		"another version": {func(t *testing.T, s *releasetest.Server) {
			s.Publish(t, "v1.1.0", true, "darwin/arm64")
			s.Add("v1.1.0", selfupdate.AppAssetName("v1.1.0", "arm64"), releasetest.AppZip(t, appbundle.ReleaseID, "v1.0.5", signed))
		}, "holds caboose v1.0.5, not v1.1.0"},
		"checksum": {func(t *testing.T, s *releasetest.Server) {
			publishApp(t, s, "v1.1.0", appbundle.ReleaseID, signed)
			s.Replace("v1.1.0", selfupdate.AppAssetName("v1.1.0", "arm64"), releasetest.AppZip(t, appbundle.ReleaseID, "v1.1.0", signed+"x"))
		}, "checksum mismatch"},
		"no asset": {func(t *testing.T, s *releasetest.Server) {
			s.Publish(t, "v1.1.0", true, "darwin/arm64")
		}, "lists no caboose-app_1.1.0_darwin_arm64.zip"},
		"no bundle": {func(t *testing.T, s *releasetest.Server) {
			s.Publish(t, "v1.1.0", true, "darwin/arm64")
			s.Add("v1.1.0", selfupdate.AppAssetName("v1.1.0", "arm64"), releasetest.Archive(t, map[string][]byte{"x": nil}))
		}, "zip: not a valid zip file"},
	} {
		t.Run(name, func(t *testing.T) {
			s := releasetest.New(t)
			c.publish(t, s)
			a := installedApp(t, "v1.0.0", signed)
			done, err := appInstall(a, s, "v1.1.0")
			if done || err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Install = %v, %v; want %q", done, err, c.want)
			}
			if v, err := a.Version(); v != "v1.0.0" || err != nil {
				t.Errorf("the installed one: %q, %v", v, err)
			}
			if l := leftovers(t, a); l != nil {
				t.Errorf("left beside it: %q", l)
			}
		})
	}
}

// A signature that does not verify.
func TestAppInstallBrokenSignature(t *testing.T) {
	s := releasetest.New(t)
	publishApp(t, s, "v1.1.0", appbundle.ReleaseID, appbundle.ReleaseID+" "+team)
	a := installedApp(t, "v1.0.0", appbundle.ReleaseID+" "+team)
	a.Unzip = func(ctx context.Context, zip, dir string) error {
		if err := releasetest.Unzip(ctx, zip, dir); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, appbundle.Name, "Contents", "BROKEN"), nil, 0o644)
	}
	if done, err := appInstall(a, s, "v1.1.0"); done || err == nil || !strings.Contains(err.Error(), "signature is not valid") {
		t.Errorf("Install = %v, %v", done, err)
	}
	if v, _ := a.Version(); v != "v1.0.0" {
		t.Errorf("the installed one: %q", v)
	}
}

// On a Mac, with its own ditto and codesign: an ad hoc release replaces
// an ad hoc one, as a fork's do, and what codesign -dv prints is read.
func TestAppInstallMacTools(t *testing.T) {
	old := releasetest.MacAppZip(t, "v1.0.0")
	s := releasetest.New(t)
	s.Publish(t, "v1.1.0", true, "darwin/arm64")
	s.Add("v1.1.0", selfupdate.AppAssetName("v1.1.0", "arm64"), releasetest.MacAppZip(t, "v1.1.0"))
	dir := filepath.Join(t.TempDir(), "Applications")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	a := selfupdate.App{Path: filepath.Join(dir, appbundle.Name)}
	if out, err := exec.Command("/usr/bin/ditto", "-x", "-k", writeZip(t, old), dir).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	done, err := a.Install(context.Background(), selfupdate.Source{Base: s.Base}, "v1.1.0", "arm64")
	if err != nil || !done {
		t.Fatalf("Install = %v, %v", done, err)
	}
	if v, err := a.Version(); v != "v1.1.0" || err != nil {
		t.Errorf("Version = %q, %v", v, err)
	}
	if out, err := exec.Command("/usr/bin/codesign", "--verify", "--deep", "--strict", a.Path).CombinedOutput(); err != nil {
		t.Errorf("the installed bundle does not verify: %v\n%s", err, out)
	}
	if l := leftovers(t, a); l != nil {
		t.Errorf("left beside it: %q", l)
	}
}
