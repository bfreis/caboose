package launcher

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/vm"
)

// scratchVM makes a VM directory under the data dir's vm/ with a scratch
// disk, and returns the disk's path.
func scratchVM(t *testing.T, a *App, name string) string {
	t.Helper()
	p := filepath.Join(a.vmRoot(), name, "scratch.img")
	writeSized(t, p, 64<<20)
	return p
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// prune deletes the scratch disk of a VM not running, asking nothing; it
// leaves one whose VM is starting or stopping (its start lock held), and
// says so, and those of a VM whose vmm runs, saying nothing.
func TestPruneScratch(t *testing.T) {
	e, _ := adminEnv(t, lsConfig, nil)
	a := e.a
	stale := scratchVM(t, a, "stale")
	busy := scratchVM(t, a, "busy")
	locked := scratchVM(t, a, "locked")
	served := scratchVM(t, a, "served")
	unlock, err := vm.TryLock(vm.Dir(filepath.Dir(busy)).StartLock())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	unvmm, err := vm.TryLock(vm.Dir(filepath.Dir(locked)).Lock())
	if err != nil {
		t.Fatal(err)
	}
	defer unvmm()
	sock := vm.Dir(filepath.Dir(served)).Socket()
	if len(sock) >= maxSocketPath {
		t.Skipf("socket path too long here: %s", sock)
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	a.pruneScratch()
	if exists(stale) || !exists(busy) || !exists(locked) || !exists(served) {
		t.Errorf("stale %v busy %v locked %v served %v", exists(stale), exists(busy), exists(locked), exists(served))
	}
	errs := e.errb.String()
	for _, want := range []string{
		"caboose: deleted " + stale + ", the scratch disk of a VM not running, freeing ",
		"caboose: left " + busy + ": its VM is starting or stopping ('caboose prune' again once it is done)\n",
	} {
		if !strings.Contains(errs, want) {
			t.Errorf("lacks %q:\n%s", want, errs)
		}
	}
	if strings.Contains(errs, locked) || strings.Contains(errs, served) {
		t.Errorf("names a running VM's disk:\n%s", errs)
	}
	// The start lock is let go of: a start could take it now.
	again, err := vm.TryLock(vm.Dir(filepath.Dir(stale)).StartLock())
	if err != nil {
		t.Errorf("start lock still held: %v", err)
	} else {
		again()
	}
}

// Plain prune deletes the leftover scratch disks before it starts the
// sandbox.
func TestPruneDeletesScratch(t *testing.T) {
	b := newBoxApp(t, isolationContainer, runningBox(isolationContainer))
	b.mountLocal("local/linux-arm64")
	mkdirs(t, b.data, "local/linux-arm64/share/claude", "local/linux-arm64/bin")
	b.Cfg.CabooseHome, b.Cfg.Home = filepath.Join(b.tmp, "caboose-home"), b.tmp
	stale := scratchVM(t, b.App, "old-vm")
	if err := b.Prune(nil); err != nil {
		t.Fatalf("%v\n%s", err, b.errb)
	}
	if _, errs := b.said(); exists(stale) || !strings.Contains(errs, "deleted "+stale) {
		t.Errorf("exists %v:\n%s", exists(stale), errs)
	}
}

// prune --builder-cache says what it frees and asks; with no terminal it
// refuses, as it does while a build holds the builder; CABOOSE_FORCE=1
// deletes without asking, stopping nothing when no builder runs. With no
// cache there is nothing to do, and a cache that is no regular file is
// refused.
func TestPruneBuilderCache(t *testing.T) {
	e, _ := adminEnv(t, lsConfig, nil)
	a := e.a
	d := a.vmDir(builderName)
	cache := filepath.Join(string(d), "cache.img")
	writeSized(t, cache, 1<<30)
	if err := a.Prune([]string{"--builder-cache", "--docker"}); err == nil || !strings.Contains(err.Error(), "usage: caboose prune [--docker | --builder-cache]") {
		t.Errorf("two flags: %v", err)
	}

	a.Terminal = func() (io.ReadCloser, error) { return nil, errors.New("no terminal") }
	if err := a.Prune([]string{"--builder-cache"}); err == nil || !strings.Contains(err.Error(), "refusing to delete it non-interactively (set CABOOSE_FORCE=1 to delete it)") || !exists(cache) {
		t.Errorf("no terminal: %v, exists %v", err, exists(cache))
	}
	a.Terminal = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("n\n")), nil }
	if err := a.Prune([]string{"--builder-cache"}); err == nil || !strings.Contains(err.Error(), "aborted; nothing was changed") || !exists(cache) {
		t.Errorf("no: %v, exists %v", err, exists(cache))
	}

	// A build running: refused before asking anything.
	running := "a build is running, and it uses the builder's cache: 'caboose prune --builder-cache' again once it ends"
	unlock, err := vm.TryLock(d.StartLock())
	if err != nil {
		t.Fatal(err)
	}
	asked := false
	a.Terminal = func() (io.ReadCloser, error) { asked = true; return io.NopCloser(strings.NewReader("y\n")), nil }
	if err := a.Prune([]string{"--builder-cache"}); err == nil || !strings.Contains(err.Error(), running) || asked || !exists(cache) {
		t.Errorf("a build running: %v, asked %v, exists %v", err, asked, exists(cache))
	}
	unlock()
	// A build that starts while the question waits: refused after it.
	a.Terminal = func() (io.ReadCloser, error) {
		unlock, err = vm.TryLock(d.StartLock())
		if err != nil {
			t.Fatal(err)
		}
		return io.NopCloser(strings.NewReader("y\n")), nil
	}
	if err := a.Prune([]string{"--builder-cache"}); err == nil || !strings.Contains(err.Error(), running) || !exists(cache) {
		t.Errorf("a build started meanwhile: %v, exists %v", err, exists(cache))
	}
	unlock()
	a.Cfg.Getenv = func(k string) string { return map[string]string{"CABOOSE_FORCE": "1"}[k] }

	e.errb.Reset()
	if err := a.Prune([]string{"--builder-cache"}); err != nil || exists(cache) {
		t.Fatalf("forced: %v, exists %v", err, exists(cache))
	}
	errs := e.errb.String()
	for _, want := range []string{
		"caboose: this deletes " + cache + " (",
		"caboose: CABOOSE_FORCE=1 set, continuing.\n",
		"caboose: deleted " + cache + ", freeing ",
	} {
		if !strings.Contains(errs, want) {
			t.Errorf("lacks %q:\n%s", want, errs)
		}
	}
	if strings.Contains(errs, "stopping the builder") {
		t.Errorf("stopped a builder that was not running:\n%s", errs)
	}

	e.errb.Reset()
	if err := a.Prune([]string{"--builder-cache"}); err != nil || !strings.Contains(e.errb.String(), "no builder cache at "+cache+": nothing to delete") {
		t.Errorf("none: %v\n%s", err, e.errb)
	}
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := a.Prune([]string{"--builder-cache"}); err == nil || !strings.Contains(err.Error(), "is not a regular file") || !exists(cache) {
		t.Errorf("a directory: %v", err)
	}
}
