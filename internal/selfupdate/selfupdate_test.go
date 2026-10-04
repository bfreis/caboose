package selfupdate_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/selfupdate"
	"github.com/bfreis/caboose/internal/selfupdate/releasetest"
)

const plat = "linux/arm64"

func install(t *testing.T, l selfupdate.Layout, s *releasetest.Server, tag, running string) error {
	t.Helper()
	return l.Install(context.Background(), selfupdate.Source{Base: s.Base}, tag, "linux", "arm64", running)
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func versions(t *testing.T, l selfupdate.Layout) []string {
	t.Helper()
	es, _ := os.ReadDir(l.Versions)
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func TestLatest(t *testing.T) {
	s := releasetest.New(t)
	src := selfupdate.Source{Base: s.Base}
	if _, err := src.Latest(context.Background()); err == nil {
		t.Error("no release, and no error")
	}
	s.Publish(t, "v1.2.0", true, plat)
	s.Publish(t, "v1.3.0-rc.1", false, plat)
	if tag, err := src.Latest(context.Background()); tag != "v1.2.0" || err != nil {
		t.Errorf("Latest = %q, %v", tag, err)
	}
}

// A latest release that is not caboose's, as the vm kernel's source
// release once was on a repository with no other, is never an update.
func TestLatestNotCaboose(t *testing.T) {
	s := releasetest.New(t)
	src := selfupdate.Source{Base: s.Base}
	for _, tag := range []string{"kernel-6.18.54", "vmtest", "v1.2"} {
		s.Publish(t, tag, true)
		if got, err := src.Latest(context.Background()); err == nil || !strings.Contains(err.Error(), "no caboose version") {
			t.Errorf("latest %s: Latest = %q, %v", tag, got, err)
		}
	}
}

func TestInstall(t *testing.T) {
	s := releasetest.New(t)
	for _, tag := range []string{"v1.0.0", "v1.1.0", "v1.2.0"} {
		s.Publish(t, tag, true, plat, "darwin/arm64")
	}
	l := selfupdate.DefaultLayout(t.TempDir())

	// A fresh install: the version's dir, and the link to it.
	if err := install(t, l, s, "v1.0.0", "/usr/bin/true"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, l.Link); got != string(releasetest.Binary("v1.0.0")) {
		t.Errorf("the link runs %q", got)
	}
	if fi, _ := os.Stat(filepath.Join(l.Versions, "v1.0.0", "caboose")); fi == nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("the binary: %v", fi)
	}
	if l.Current() != "v1.0.0" {
		t.Errorf("Current = %q", l.Current())
	}

	// Two more, the second run from the first: two versions stay, the new
	// one and the one before.
	if err := install(t, l, s, "v1.1.0", filepath.Join(l.Versions, "v1.0.0", "caboose")); err != nil {
		t.Fatal(err)
	}
	if err := install(t, l, s, "v1.2.0", filepath.Join(l.Versions, "v1.1.0", "caboose")); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(versions(t, l), " "); got != "v1.1.0 v1.2.0" {
		t.Errorf("versions: %s", got)
	}
	if l.Current() != "v1.2.0" {
		t.Errorf("Current = %q", l.Current())
	}
}

// The running version is never removed, even when it is neither the new one
// nor the one the link pointed at.
func TestInstallKeepsTheRunningOne(t *testing.T) {
	s := releasetest.New(t)
	for _, tag := range []string{"v1.0.0", "v1.1.0", "v1.2.0"} {
		s.Publish(t, tag, true, plat)
	}
	l := selfupdate.DefaultLayout(t.TempDir())
	for _, tag := range []string{"v1.0.0", "v1.1.0"} {
		if err := install(t, l, s, tag, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := install(t, l, s, "v1.2.0", filepath.Join(l.Versions, "v1.0.0", "caboose")); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(versions(t, l), " "); got != "v1.0.0 v1.1.0 v1.2.0" {
		t.Errorf("versions: %s", got)
	}
}

// A download that does not match its checksum changes nothing.
func TestInstallChecksumMismatch(t *testing.T) {
	s := releasetest.New(t)
	s.Publish(t, "v1.0.0", true, plat)
	s.Publish(t, "v1.1.0", true, plat)
	l := selfupdate.DefaultLayout(t.TempDir())
	if err := install(t, l, s, "v1.0.0", ""); err != nil {
		t.Fatal(err)
	}
	s.Replace("v1.1.0", selfupdate.AssetName("v1.1.0", "linux", "arm64"),
		releasetest.Archive(t, map[string][]byte{"caboose": []byte("evil")}))
	err := install(t, l, s, "v1.1.0", "")
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	if l.Current() != "v1.0.0" || strings.Join(versions(t, l), " ") != "v1.0.0" {
		t.Errorf("changed: current %q, versions %v", l.Current(), versions(t, l))
	}
}

func TestInstallNoAssetForThePlatform(t *testing.T) {
	s := releasetest.New(t)
	s.Publish(t, "v1.0.0", true, "darwin/amd64")
	l := selfupdate.DefaultLayout(t.TempDir())
	if err := install(t, l, s, "v1.0.0", ""); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Lstat(l.Link); err == nil {
		t.Error("linked")
	}
}

func TestManaged(t *testing.T) {
	s := releasetest.New(t)
	s.Publish(t, "v1.0.0", true, plat)
	l := selfupdate.DefaultLayout(t.TempDir())
	if err := install(t, l, s, "v1.0.0", ""); err != nil {
		t.Fatal(err)
	}
	for exe, want := range map[string]string{
		l.Link: "v1.0.0", // through the symlink
		filepath.Join(l.Versions, "v1.0.0", "caboose"): "v1.0.0",
		"/usr/bin/true":                     "",
		filepath.Join(l.Versions, "v1.0.0"): "",
	} {
		if tag, ok := l.Managed(exe); tag != want || ok != (want != "") {
			t.Errorf("Managed(%s) = %q, %v", exe, tag, ok)
		}
	}
}

func TestState(t *testing.T) {
	home := t.TempDir()
	if st := selfupdate.ReadState(home); st != (selfupdate.State{}) {
		t.Errorf("no file: %+v", st)
	}
	want := selfupdate.State{Checked: time.Unix(1700000000, 0).UTC(), Latest: "v1.2.0", From: "v1.1.0", To: "v1.2.0"}
	if err := selfupdate.WriteState(home, want); err != nil {
		t.Fatal(err)
	}
	if got := selfupdate.ReadState(home); got != want {
		t.Errorf("got %+v", got)
	}
}

func TestLock(t *testing.T) {
	home := t.TempDir()
	unlock, err := selfupdate.Lock(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := selfupdate.Lock(home); err != selfupdate.ErrLocked {
		t.Errorf("second lock: %v", err)
	}
	unlock()
	if u, err := selfupdate.Lock(home); err != nil {
		t.Errorf("after unlock: %v", err)
	} else {
		u()
	}
}

// A darwin archive's caboose-vmm is installed next to caboose; a linux one
// has none, and none is made up.
func TestInstallVMM(t *testing.T) {
	s := releasetest.New(t)
	s.VMM = true
	s.Publish(t, "v1.0.0", true, "darwin/arm64", plat)
	for _, c := range []struct {
		goos, goarch string
		vmm          bool
	}{{"darwin", "arm64", true}, {"linux", "arm64", false}} {
		l := selfupdate.DefaultLayout(t.TempDir())
		if err := l.Install(context.Background(), selfupdate.Source{Base: s.Base}, "v1.0.0", c.goos, c.goarch, "/usr/bin/true"); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(l.Versions, "v1.0.0", selfupdate.VMM)
		fi, err := os.Stat(p)
		switch {
		case c.vmm && (err != nil || fi.Mode().Perm() != 0o755 || read(t, p) != string(releasetest.VMMBinary("v1.0.0"))):
			t.Errorf("%s: caboose-vmm %v, %v", c.goos, fi, err)
		case !c.vmm && err == nil:
			t.Errorf("%s: a caboose-vmm from nowhere", c.goos)
		}
	}
}

// A download is whole and checked against checksums.txt, or an error.
func TestDownload(t *testing.T) {
	s := releasetest.New(t)
	s.Publish(t, "v1.0.0", true, plat)
	s.Add("v1.0.0", "big.tar.gz", []byte("the vm's files"))
	src := selfupdate.Source{Base: s.Base}
	var b strings.Builder
	if err := src.Download(context.Background(), "v1.0.0", "big.tar.gz", &b, 1<<20); err != nil || b.String() != "the vm's files" {
		t.Fatalf("%q, %v", b.String(), err)
	}
	s.Replace("v1.0.0", "big.tar.gz", []byte("tampered"))
	if err := src.Download(context.Background(), "v1.0.0", "big.tar.gz", io.Discard, 1<<20); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("a tampered download: %v", err)
	}
	if err := src.Download(context.Background(), "v1.0.0", "missing.tar.gz", io.Discard, 1<<20); err == nil {
		t.Fatal("a file checksums.txt does not list")
	}
}
