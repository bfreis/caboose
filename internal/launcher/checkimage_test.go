package launcher

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/imagecheck"
)

// fakeImageGuest plays the builder guest for caboose check-image: a store
// of images, each with the report its probe gives.
type fakeImageGuest struct {
	store   map[string]*imagecheck.Report
	remote  map[string]*imagecheck.Report // what a pull can fetch
	calls   []string
	stopped bool
}

func (g *fakeImageGuest) Has(image string) (bool, error) {
	g.calls = append(g.calls, "has "+image)
	_, ok := g.store[image]
	return ok, nil
}

func (g *fakeImageGuest) Pull(image string, progress io.Writer) error {
	g.calls = append(g.calls, "pull "+image)
	rep, ok := g.remote[image]
	if !ok {
		return errors.New("pull access denied for " + image)
	}
	io.WriteString(progress, "Pull complete\n")
	g.store[image] = rep
	return nil
}

func (g *fakeImageGuest) Check(image string, probe []byte, uid, gid int) (string, *imagecheck.Report, error) {
	g.calls = append(g.calls, "check "+image)
	if len(probe) == 0 {
		return "", nil, errors.New("no probe")
	}
	rep := *g.store[image]
	rep.Image = image
	return "sha256:" + image, &rep, nil
}

func (g *fakeImageGuest) Stop() error { g.stopped = true; return nil }

func vmCheckApp(g *fakeImageGuest, bootErr error) (*App, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	a := &App{Cfg: &config.Config{Env: "default", Image: "caboose:default", Isolation: isolationVM}, Stdout: &stdout, Stderr: &stderr}
	a.checkGuest = func() (imageGuest, error) {
		if bootErr != nil {
			return nil, bootErr
		}
		return g, nil
	}
	return a, &stdout, &stderr
}

func passingReport(t *testing.T) *imagecheck.Report {
	t.Helper()
	b, err := os.ReadFile("../vm/builder/testdata/probe-ok.txt")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := imagecheck.Parse(string(b))
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func exitCode(err error) int {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	if err != nil {
		return -1
	}
	return 0
}

// Under vm the default base is checked where caboose build made it, in the
// builder guest's store, and reported as under docker.
func TestCheckImageVMDefaultBase(t *testing.T) {
	g := &fakeImageGuest{store: map[string]*imagecheck.Report{"caboose-base:default": passingReport(t)}}
	a, stdout, _ := vmCheckApp(g, nil)
	if err := a.CheckImage(nil); err != nil {
		t.Fatalf("CheckImage: %v", err)
	}
	if want := "has caboose-base:default check caboose-base:default"; strings.Join(g.calls, " ") != want {
		t.Errorf("calls %q, want %q", g.calls, want)
	}
	if !g.stopped {
		t.Error("the builder guest was left running")
	}
	if !strings.Contains(stdout.String(), "Usable: every requirement is met.") {
		t.Errorf("stdout:\n%s", stdout)
	}
}

// A default base no build has made yet is not pulled: its name is only
// caboose's own tag.
func TestCheckImageVMNotBuilt(t *testing.T) {
	g := &fakeImageGuest{store: map[string]*imagecheck.Report{}, remote: map[string]*imagecheck.Report{"caboose-base:default": passingReport(t)}}
	a, _, stderr := vmCheckApp(g, nil)
	err := a.CheckImage(nil)
	if exitCode(err) != checkFailed || !strings.Contains(err.Error(), "not built yet") {
		t.Fatalf("err = %v", err)
	}
	if strings.Join(g.calls, " ") != "has caboose-base:default" {
		t.Errorf("calls %q", g.calls)
	}
	if !strings.Contains(stderr.String(), "caboose build") {
		t.Errorf("stderr says nothing of what to do:\n%s", stderr)
	}
}

// A named image the guest lacks is pulled there, then checked: a refused
// one exits 1, as under docker.
func TestCheckImageVMNamedRefused(t *testing.T) {
	bare := imagecheck.NoShellReport("", 501, 20)
	g := &fakeImageGuest{store: map[string]*imagecheck.Report{}, remote: map[string]*imagecheck.Report{"example/bare:1": bare}}
	a, stdout, stderr := vmCheckApp(g, nil)
	err := a.CheckImage([]string{"example/bare:1"})
	if exitCode(err) != checkUnmet {
		t.Fatalf("err = %v", err)
	}
	if want := "has example/bare:1 pull example/bare:1 check example/bare:1"; strings.Join(g.calls, " ") != want {
		t.Errorf("calls %q, want %q", g.calls, want)
	}
	if !strings.Contains(stdout.String(), "Not usable") || !strings.Contains(stderr.String(), "Pull complete") {
		t.Errorf("stdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
}

// An image the guest cannot pull -- one only a docker engine on this Mac
// has, say -- could not be checked, and the user is told what to do.
func TestCheckImageVMPullFails(t *testing.T) {
	g := &fakeImageGuest{store: map[string]*imagecheck.Report{}}
	a, _, stderr := vmCheckApp(g, nil)
	err := a.CheckImage([]string{"local-only:dev"})
	if exitCode(err) != checkFailed || !strings.Contains(err.Error(), "cannot pull image 'local-only:dev' in the builder guest") {
		t.Fatalf("err = %v", err)
	}
	for _, s := range []string{"registry", "caboose -e ENV check-image local-only:dev"} {
		if !strings.Contains(stderr.String(), s) {
			t.Errorf("stderr lacks %q:\n%s", s, stderr)
		}
	}
	if !g.stopped {
		t.Error("the builder guest was left running")
	}
}

// A builder that does not boot is a could-not-check.
func TestCheckImageVMBootFails(t *testing.T) {
	a, _, _ := vmCheckApp(nil, Die("no caboose-vmm next to caboose"))
	if err := a.CheckImage(nil); exitCode(err) != checkFailed {
		t.Fatalf("err = %v", err)
	}
}

// Under vm an image without Docker's engine is usable all the same: the
// checklist says docker won't work inside the sandbox, and what to add.
func TestCheckImageVMNoDockerd(t *testing.T) {
	rep := passingReport(t)
	rep.Engine = map[string]string{"dockerd": "", "containerd": "", "containerd-shim-runc-v2": "", "runc": "", "iptables": "", "docker": ""}
	g := &fakeImageGuest{store: map[string]*imagecheck.Report{"caboose-base:default": rep}}
	a, stdout, stderr := vmCheckApp(g, nil)
	if err := a.CheckImage(nil); err != nil {
		t.Fatalf("CheckImage: %v", err)
	}
	for _, s := range []string{"docker inside", "not in this image: `docker` won't work inside the sandbox", "Usable: every requirement is met."} {
		if !strings.Contains(stdout.String(), s) {
			t.Errorf("stdout lacks %q:\n%s", s, stdout)
		}
	}
	if !strings.Contains(stderr.String(), "dockerd section") {
		t.Errorf("stderr says nothing of what to add:\n%s", stderr)
	}

	rep.Engine = map[string]string{"dockerd": "/d", "containerd": "/c", "containerd-shim-runc-v2": "/s", "runc": "/r", "iptables": "/i", "docker": "/k"}
	rep.EngineVersion = "28.3.0"
	a, stdout, stderr = vmCheckApp(g, nil)
	if err := a.CheckImage(nil); err != nil {
		t.Fatalf("CheckImage: %v", err)
	}
	if !strings.Contains(stdout.String(), "available (dockerd 28.3.0)") || strings.Contains(stderr.String(), "dockerd section") {
		t.Errorf("stdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
}
