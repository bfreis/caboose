package launcher

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/apkobuild"
	"github.com/bfreis/caboose/internal/assets"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/docker"
	"github.com/bfreis/caboose/internal/imagecheck"
	"github.com/bfreis/caboose/internal/vm/builder"
)

// apkoEnv is an App with an apko image profile, a fake docker whose engine
// is arch and whose load keeps what it read, and apkobuild stood in for:
// resolving writes a lock of the spec's packages at version, building
// writes the lock's hash as the tarball.
type apkoEnv struct {
	a        *App
	errb     *bytes.Buffer
	loaded   string // what docker load read
	resolved int    // how many times the packages were resolved
	version  string // the version the next resolve locks
	buildErr error
}

func newApkoEnv(t *testing.T, arch string) *apkoEnv {
	t.Helper()
	tmp := t.TempDir()
	e := &apkoEnv{errb: &bytes.Buffer{}, loaded: filepath.Join(tmp, "loaded"), version: "1"}
	script := "#!/bin/sh\ncase \"$1\" in\n" +
		"  info) echo " + arch + "; exit 0 ;;\n" +
		"  load) cat > " + e.loaded + "; echo 'Loaded image: caboose-base:x'; exit 0 ;;\n" +
		"esac\nexit 1\n"
	bin := filepath.Join(tmp, "docker")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e.a = &App{
		Cfg: &config.Config{Env: "x", Image: "caboose:x", EnvDir: filepath.Join(tmp, "env"), CabooseHome: filepath.Join(tmp, "home"),
			ImageProfile: config.DefaultImageProfile(), Home: tmp, Getenv: func(string) string { return "" }},
		Docker: &docker.CLI{Path: bin},
		Stdout: &bytes.Buffer{}, Stderr: e.errb,
	}
	if err := os.MkdirAll(e.a.Cfg.EnvDir, 0o755); err != nil {
		t.Fatal(err)
	}
	e.a.apko = &apkoTools{
		resolve: func(_ context.Context, spec apkobuild.Spec, o apkobuild.Options) (*apkobuild.Lock, error) {
			e.resolved++
			if o.CacheDir != filepath.Join(tmp, "home", "cache", "apk") {
				t.Errorf("cache dir %q", o.CacheDir)
			}
			list, err := spec.List()
			if err != nil {
				return nil, err
			}
			return writeLock(t, filepath.Join(tmp, "resolved.json"), spec, list, e.version, o.Arch), nil
		},
		build: func(_ context.Context, l *apkobuild.Lock, o apkobuild.Options, tag string, w io.Writer) (string, error) {
			if tag != "caboose-base:x" || o.Arch != l.Arch() {
				t.Errorf("built %s for %s from a lock for %s", tag, o.Arch, l.Arch())
			}
			if e.buildErr != nil {
				return "", e.buildErr
			}
			_, err := io.WriteString(w, "tarball of "+l.Hash())
			return "sha256:x", err
		},
	}
	return e
}

// An apko build resolves the packages into the lock when there is none,
// and then builds that lock, as it is, until something calls for
// resolving again: other packages, another architecture, or --pull.
func TestBuildApkoResolvesWhenItMust(t *testing.T) {
	e := newApkoEnv(t, "x86_64")
	c := e.a.Cfg
	hash, err := e.a.buildApko("caboose-base:x", false)
	if err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	lock, err := apkobuild.ReadLock(c.LockPath())
	if err != nil {
		t.Fatal(err)
	}
	if hash != lock.Hash() || e.resolved != 1 || lock.Arch() != "x86_64" {
		t.Errorf("hash %s, lock %s, resolved %d, arch %s", hash, lock.Hash(), e.resolved, lock.Arch())
	}
	if b, _ := os.ReadFile(e.loaded); string(b) != "tarball of "+hash {
		t.Errorf("docker load read %q", b)
	}
	n := len(lock.Packages())
	for _, want := range []string{
		"caboose: resolving the packages of apko.default (it has no lock yet)\n",
		"caboose: resolved " + strconv.Itoa(n) + " packages for apko.default\n",
		"caboose: building the base image 'caboose-base:x' from " + strconv.Itoa(n) + " packages (apko.default)\n",
		"Loaded image: caboose-base:x",
		"caboose: lock written to " + e.a.short(c.LockPath()) + "\n",
	} {
		if !strings.Contains(e.errb.String(), want) {
			t.Errorf("no %q in:\n%s", want, e.errb)
		}
	}

	// Built again as it is: the same hash, nothing resolved.
	e.version = "2"
	if again, err := e.a.buildApko("caboose-base:x", false); err != nil || again != hash || e.resolved != 1 {
		t.Errorf("again: %s %v, resolved %d", again, err, e.resolved)
	}

	// --pull resolves again, to newer packages.
	e.errb.Reset()
	pulled, err := e.a.buildApko("caboose-base:x", true)
	if err != nil || pulled == hash || e.resolved != 2 || !strings.Contains(e.errb.String(), "(as --pull asks)") {
		t.Errorf("pull: %s %v, resolved %d\n%s", pulled, err, e.resolved, e.errb)
	}

	// Other packages.
	e.errb.Reset()
	c.ImageProfile.Packages = []string{"postgresql-17-client"}
	if _, err := e.a.buildApko("caboose-base:x", false); err != nil || e.resolved != 3 || !strings.Contains(e.errb.String(), "(its packages changed)") {
		t.Errorf("packages: %v, resolved %d\n%s", err, e.resolved, e.errb)
	}
	if lock, _ := apkobuild.ReadLock(c.LockPath()); !strings.Contains(strings.Join(lock.Input(), " "), "postgresql-17-client") {
		t.Errorf("lock input %v", lock.Input())
	}

	// Another engine.
	e2 := newApkoEnv(t, "aarch64")
	e2.a.Cfg.EnvDir = c.EnvDir
	e2.a.Cfg.ImageProfile = c.ImageProfile
	if _, err := e2.a.buildApko("caboose-base:x", false); err != nil || e2.resolved != 1 ||
		!strings.Contains(e2.errb.String(), "(its lock is for x86_64, and the engine is aarch64)") {
		t.Errorf("arch: %v, resolved %d\n%s", err, e2.resolved, e2.errb)
	}
}

// A lock of the profile's spec whose packages caboose's groups no longer
// make is resolved again, said as the launcher's change; a spec changed in
// a way that asks for the same packages keeps the lock, recording the new
// spec, so the image is current again after the build.
func TestBuildApkoLockSpec(t *testing.T) {
	e := newApkoEnv(t, "x86_64")
	c := e.a.Cfg
	list, err := c.ImageProfile.Spec().List()
	if err != nil {
		t.Fatal(err)
	}
	writeLock(t, c.LockPath(), c.ImageProfile.Spec(), append(list, "retired-package"), "1", "x86_64")
	if _, err := e.a.buildApko("caboose-base:x", false); err != nil || e.resolved != 1 ||
		!strings.Contains(e.errb.String(), "(caboose's packages changed with this launcher)") {
		t.Errorf("groups: %v, resolved %d\n%s", err, e.resolved, e.errb)
	}

	e.errb.Reset()
	c.ImageProfile.Packages = []string{apkobuild.DefaultPackages()[0]}
	hash, err := e.a.buildApko("caboose-base:x", false)
	if err != nil || e.resolved != 1 || strings.Contains(e.errb.String(), "resolving") {
		t.Errorf("same packages: %v, resolved %d\n%s", err, e.resolved, e.errb)
	}
	lock, err := apkobuild.ReadLock(c.LockPath())
	if err != nil || !lock.Spec().Equal(c.ImageProfile.Spec()) || lock.Hash() != hash {
		t.Errorf("lock %+v, %v", lock, err)
	}
	if st, ok := e.a.classifyLock(hash); !ok {
		t.Errorf("after the build: %+v", st)
	}
}

// A lock that cannot be read is resolved again; a build that fails says
// so, and leaves no half-loaded image unsaid.
func TestBuildApkoFailures(t *testing.T) {
	e := newApkoEnv(t, "amd64")
	if err := os.WriteFile(e.a.Cfg.LockPath(), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.buildErr = errors.New("no such package")
	_, err := e.a.buildApko("caboose-base:x", false)
	if err == nil || !strings.Contains(err.Error(), "building the base image 'caboose-base:x': no such package") {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(e.errb.String(), "its lock cannot be used") || e.resolved != 1 {
		t.Errorf("resolved %d:\n%s", e.resolved, e.errb)
	}
	// The lock records the image it built, so a failed build leaves the
	// last one as it was.
	if b, _ := os.ReadFile(e.a.Cfg.LockPath()); string(b) != "{" {
		t.Errorf("after a failed build the lock is %q", b)
	}
	// amd64, as some engines say it, is x86_64 to apk.
	e.buildErr = nil
	if _, err := e.a.buildApko("caboose-base:x", false); err != nil {
		t.Fatal(err)
	}
	if l, _ := apkobuild.ReadLock(e.a.Cfg.LockPath()); l == nil || l.Arch() != "x86_64" {
		t.Errorf("lock %v", l)
	}

	e = newApkoEnv(t, "riscv64")
	if _, err := e.a.buildApko("caboose-base:x", false); err == nil || !strings.Contains(err.Error(), "an apko image is built for x86_64 and aarch64 only") {
		t.Errorf("riscv64: %v", err)
	}

	// No architecture from docker: a dead end it says how out of.
	e = newApkoEnv(t, "x86_64")
	e.a.Docker.Path = "/nonexistent/docker"
	if _, err := e.a.buildApko("caboose-base:x", false); err == nil || !strings.Contains(err.Error(), "cannot ask the docker engine its architecture") {
		t.Errorf("no docker: %v", err)
	}
}

// A docker load that fails at once stops the build: it does not go on
// fetching packages nobody will load, and the error is docker's.
func TestBuildApkoLoadFailureCancelsTheBuild(t *testing.T) {
	e := newApkoEnv(t, "x86_64")
	script := "#!/bin/sh\ncase \"$1\" in\n  info) echo x86_64; exit 0 ;;\n  load) echo 'no space left on device' >&2; exit 3 ;;\nesac\nexit 1\n"
	if err := os.WriteFile(e.a.Docker.Path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cancelled := make(chan struct{})
	e.a.apko.build = func(ctx context.Context, _ *apkobuild.Lock, _ apkobuild.Options, _ string, _ io.Writer) (string, error) {
		<-ctx.Done()
		close(cancelled)
		return "", ctx.Err()
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.a.buildApko("caboose-base:x", false)
		done <- err
	}()
	select {
	case err := <-done:
		var ee *ExitError
		if !errors.As(err, &ee) || ee.Code != 3 {
			t.Errorf("err = %#v, want docker's exit status 3", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the build was not stopped when docker load failed")
	}
	select {
	case <-cancelled:
	default:
		t.Error("the build's context was not cancelled")
	}
}

// fakeBuildGuest plays the builder guest for caboose build under vm: it
// keeps the request, reads the base's tarball to its end as docker load
// would, and answers with a result, or fails with err.
type fakeBuildGuest struct {
	req      builder.Request
	tarball  string
	err      error
	stopped  bool
	template bool
}

func (g *fakeBuildGuest) Build(req builder.Request) (builder.Result, error) {
	g.req = req
	if req.BaseTarball != nil {
		b, err := io.ReadAll(req.BaseTarball)
		if err != nil {
			return builder.Result{}, err
		}
		g.tarball = string(b)
	}
	if g.err != nil {
		return builder.Result{}, g.err
	}
	return builder.Result{Base: req.BaseTag, BaseID: "sha256:base", ImageID: "sha256:0123456789abcdef0123",
		Report: &imagecheck.Report{Platform: "linux-arm64-glibc"}}, nil
}

func (g *fakeBuildGuest) Template() error { g.template = true; return nil }
func (g *fakeBuildGuest) Stop() error     { g.stopped = true; return nil }

// vmApkoEnv is apkoEnv under vm, on an arm64 Mac, with g as its builder.
func vmApkoEnv(t *testing.T, g *fakeBuildGuest) *apkoEnv {
	t.Helper()
	e := newApkoEnv(t, "x86_64")
	e.a.Docker = nil // under vm there may be no docker engine at all
	e.a.Cfg.Isolation = config.KindVM
	e.a.Cfg.DataDir = filepath.Join(t.TempDir(), "data")
	e.a.vmFiles = func() (vmFiles, error) { return vmFiles{Arch: "arm64"}, nil }
	e.a.buildGuest = func(out, template string) (vmBuildGuest, error) {
		for _, d := range []string{out, template} {
			if d != "" {
				if err := os.WriteFile(d, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		return g, nil
	}
	return e
}

// Under vm an apko base is built on the host, for the Mac's architecture,
// and its tarball goes into the builder's load as the request's base; the
// layer is labelled with the lock's hash, as with docker, and the lock is
// written once the image is recorded.
func TestBuildApkoUnderVM(t *testing.T) {
	g := &fakeBuildGuest{}
	e := vmApkoEnv(t, g)
	if err := e.a.buildVM(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	lock, err := apkobuild.ReadLock(e.a.Cfg.LockPath())
	if err != nil {
		t.Fatal(err)
	}
	if lock.Arch() != "aarch64" {
		t.Errorf("lock for %s, want the Mac's aarch64", lock.Arch())
	}
	if g.tarball != "tarball of "+lock.Hash() {
		t.Errorf("the builder's load read %q", g.tarball)
	}
	if g.req.BaseTag != "caboose-base:x" || g.req.BaseRef != "" || g.req.BaseContext != "" || g.req.Image != "caboose:x" {
		t.Errorf("request %+v", g.req)
	}
	rec, ok, err := e.a.vmImages().get("caboose:x")
	if err != nil || !ok {
		t.Fatalf("no image recorded: %v", err)
	}
	want := layerLabels("caboose-base:x", "sha256:base", config.ImageKindApko, lock.Hash(), "linux-arm64-glibc")
	if !reflect.DeepEqual(rec.Labels, want) {
		t.Errorf("labels %v, want %v", rec.Labels, want)
	}
	if id := e.a.vmImages().ImageID("caboose-base:x"); id != "sha256:base" {
		t.Errorf("base recorded as %q", id)
	}
	if !g.template || !g.stopped {
		t.Errorf("template %v, stopped %v", g.template, g.stopped)
	}
	if st, ok := e.a.classifyLock(rec.Labels[assets.LabelBaseHash]); !ok {
		t.Errorf("after the build: %+v", st)
	}
	if !strings.Contains(e.errb.String(), "(it has no lock yet)") {
		t.Errorf("stderr:\n%s", e.errb)
	}

	// A lock for another architecture is resolved again, said as the
	// builder's.
	writeLock(t, e.a.Cfg.LockPath(), e.a.Cfg.ImageProfile.Spec(), lock.Input(), "1", "x86_64")
	e.errb.Reset()
	if err := e.a.buildVM(nil); err != nil || !strings.Contains(e.errb.String(), "(its lock is for x86_64, and the builder guest is aarch64)") {
		t.Errorf("arch: %v\n%s", err, e.errb)
	}
}

// A build that fails in the guest -- its load, or any step after it --
// leaves the lock as it was, and no image recorded.
func TestBuildApkoUnderVMFails(t *testing.T) {
	g := &fakeBuildGuest{err: errors.New("loading the base image 'caboose-base:x': docker load: exit status 1")}
	e := vmApkoEnv(t, g)
	err := e.a.buildVM(nil)
	if err == nil || !strings.Contains(err.Error(), "docker load: exit status 1") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(e.a.Cfg.LockPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed build wrote the lock: %v", err)
	}
	if _, ok, _ := e.a.vmImages().get("caboose:x"); ok {
		t.Error("a failed build recorded an image")
	}

	// A build of the tarball that fails says so, whatever the guest's load
	// made of the stream cut short.
	g.err = nil
	e.buildErr = errors.New("no such package")
	if err := e.a.buildVM(nil); err == nil || !strings.Contains(err.Error(), "building the base image 'caboose-base:x': no such package") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(e.a.Cfg.LockPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed build wrote the lock: %v", err)
	}
}

// Under caboose apply (deferLock) a build writes no lock: it leaves that
// to apply, which writes it once config.toml is written.
func TestBuildApkoDefersTheLock(t *testing.T) {
	e := newApkoEnv(t, "x86_64")
	e.a.deferLock = true
	hash, err := e.a.buildApko("caboose-base:x", false)
	if err != nil {
		t.Fatalf("%v\n%s", err, e.errb)
	}
	if _, err := os.Stat(e.a.Cfg.LockPath()); !os.IsNotExist(err) {
		t.Fatalf("the lock was written: %v", err)
	}
	if e.a.pendingLock == nil {
		t.Fatal("nothing left to write the lock with")
	}
	if err := e.a.pendingLock(); err != nil {
		t.Fatal(err)
	}
	if l, err := apkobuild.ReadLock(e.a.Cfg.LockPath()); err != nil || l.Hash() != hash {
		t.Errorf("lock %v %v, want hash %s", l, err, hash)
	}
}

// A load that closes the tarball once it is done with it (the builder
// guest's docker load does, as soon as it ends) stops the build then, not
// when the load's caller returns, and the load's error is what is said.
func TestBuildApkoClosedSourceStopsTheBuild(t *testing.T) {
	e := newApkoEnv(t, "x86_64")
	cancelled := make(chan struct{})
	e.a.apko.build = func(ctx context.Context, _ *apkobuild.Lock, _ apkobuild.Options, _ string, _ io.Writer) (string, error) {
		<-ctx.Done()
		close(cancelled)
		return "", ctx.Err()
	}
	stepFailed := errors.New("docker load in the guest: exit status 1")
	_, err := e.a.apkoBase("caboose-base:x", false, "x86_64", "the builder guest", func(r io.Reader, _ string) error {
		c, ok := r.(io.Closer)
		if !ok {
			t.Fatal("the tarball cannot be closed")
		}
		_ = c.Close()
		select {
		case <-cancelled:
		case <-time.After(10 * time.Second):
			t.Error("closing the tarball did not stop the build")
		}
		return stepFailed
	})
	if !errors.Is(err, stepFailed) {
		t.Errorf("err = %v, want the load's", err)
	}
}
