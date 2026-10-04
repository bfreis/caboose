package caboose

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/bfreis/caboose/internal/selfupdate"
	"github.com/bfreis/caboose/internal/selfupdate/releasetest"
)

// runInstall runs install.sh with plain sh (dash, where it is /bin/sh), a
// HOME of its own and the fake release host, in a session of its own so it
// has no terminal to run setup on, whoever runs the tests.
func runInstall(t *testing.T, home, base string, env ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "install.sh")
	cmd.Env = append([]string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "CABOOSE_RELEASES_URL=" + base}, env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func publish(t *testing.T, s *releasetest.Server, tags ...string) {
	t.Helper()
	for i, tag := range tags {
		s.Publish(t, tag, i == len(tags)-1, runtime.GOOS+"/"+runtime.GOARCH)
	}
}

func TestInstallScript(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	s := releasetest.New(t)
	publish(t, s, "v1.0.0", "v1.1.0")
	home := t.TempDir()
	l := selfupdate.DefaultLayout(home)

	out, err := runInstall(t, home, s.Base)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{
		"caboose: downloading caboose v1.1.0 for " + runtime.GOOS + "/" + runtime.GOARCH + "\n",
		"caboose: installed caboose v1.1.0 in " + filepath.Join(l.Versions, "v1.1.0") + "\n",
		"is not on your PATH",
		`export PATH="$HOME/.local/bin:$PATH"`,
		"no terminal to run caboose setup on; run it yourself: caboose setup",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	// The layout the updater keeps: it must read this install as its own.
	if l.Current() != "v1.1.0" {
		t.Errorf("Current = %q", l.Current())
	}
	if tag, ok := l.Managed(l.Link); tag != "v1.1.0" || !ok {
		t.Errorf("Managed = %q, %v", tag, ok)
	}
	if got, err := exec.Command(l.Link, "version").Output(); err != nil || string(got) != "caboose v1.1.0 version\n" {
		t.Errorf("the installed caboose: %q, %v", got, err)
	}

	// Pinned, then again the latest: two versions, the one before kept...
	if out, err := runInstall(t, home, s.Base, "CABOOSE_VERSION=1.0.0", "CABOOSE_NO_SETUP=1"); err != nil ||
		l.Current() != "v1.0.0" || !strings.Contains(out, "caboose: next: caboose setup\n") {
		t.Fatalf("pinned: current %q, %v\n%s", l.Current(), err, out)
	}
	s.Publish(t, "v1.2.0", true, runtime.GOOS+"/"+runtime.GOARCH)
	if out, err := runInstall(t, home, s.Base, "CABOOSE_NO_SETUP=1"); err != nil || l.Current() != "v1.2.0" {
		t.Fatalf("latest: current %q, %v\n%s", l.Current(), err, out)
	}
	// ...and no more.
	es, _ := os.ReadDir(l.Versions)
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	if got := strings.Join(names, " "); got != "v1.0.0 v1.2.0" {
		t.Errorf("versions: %s", got)
	}
}

// A download that does not match its checksum installs nothing.
func TestInstallScriptChecksum(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	s := releasetest.New(t)
	publish(t, s, "v1.0.0")
	s.Replace("v1.0.0", selfupdate.AssetName("v1.0.0", runtime.GOOS, runtime.GOARCH),
		releasetest.Archive(t, map[string][]byte{"caboose": []byte("evil")}))
	home := t.TempDir()
	out, err := runInstall(t, home, s.Base, "CABOOSE_NO_SETUP=1")
	if err == nil || !strings.Contains(out, "does not match its checksum") {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Lstat(selfupdate.DefaultLayout(home).Link); err == nil {
		t.Error("installed")
	}
}

// No release to be had: said, and nothing installed.
func TestInstallScriptNoRelease(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	s := releasetest.New(t)
	home := t.TempDir()
	out, err := runInstall(t, home, s.Base)
	if err == nil || !strings.Contains(out, "cannot reach "+s.Base+"/latest") {
		t.Fatalf("%v\n%s", err, out)
	}
	s.Publish(t, "v1.0.0", true, "plan9/mips")
	out, err = runInstall(t, home, s.Base)
	if err == nil || !strings.Contains(out, "cannot download") {
		t.Fatalf("%v\n%s", err, out)
	}
}

// A latest release that is not caboose's, as the vm kernel's source
// release once was on a repository with no other: refused, and nothing
// installed.
func TestInstallScriptLatestNotCaboose(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	s := releasetest.New(t)
	home := t.TempDir()
	for _, tag := range []string{"kernel-6.18.54", "vmtest", "v1.2", "v1.2.3+x"} {
		s.Publish(t, tag, true)
		out, err := runInstall(t, home, s.Base, "CABOOSE_NO_SETUP=1")
		if err == nil || !strings.Contains(out, "/tag/"+tag+"', which is no caboose version") {
			t.Errorf("latest %s: %v\n%s", tag, err, out)
		}
	}
	if _, err := os.Lstat(selfupdate.DefaultLayout(home).Link); err == nil {
		t.Error("installed")
	}
}

// caboose-vmm, when the archive has one, goes next to caboose: on a Mac,
// which a stub uname makes this machine look like.
func TestInstallScriptVMM(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	bin := t.TempDir()
	uname := "#!/bin/sh\ncase \"$1\" in -s) echo Darwin ;; -m) echo arm64 ;; *) echo Darwin ;; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "uname"), []byte(uname), 0o755); err != nil {
		t.Fatal(err)
	}
	s := releasetest.New(t)
	s.VMM = true
	s.Publish(t, "v1.0.0", true, "darwin/arm64")
	home := t.TempDir()
	l := selfupdate.DefaultLayout(home)
	cmd := exec.Command("sh", "install.sh")
	cmd.Env = []string{"HOME=" + home, "PATH=" + bin + ":" + os.Getenv("PATH"), "CABOOSE_RELEASES_URL=" + s.Base, "CABOOSE_NO_SETUP=1"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	p := filepath.Join(l.Versions, "v1.0.0", selfupdate.VMM)
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("caboose-vmm: %v, %v", fi, err)
	}
	if got, err := os.ReadFile(p); err != nil || string(got) != string(releasetest.VMMBinary("v1.0.0")) {
		t.Fatalf("caboose-vmm holds %q, %v", got, err)
	}
}
