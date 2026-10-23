package conformance

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
)

func TestStubs(t *testing.T) {
	r := newRig(t, config.KindContainer)
	if err := r.g.CheckStubs(); err == nil {
		t.Fatal("stubs that were never written are in place")
	}
	must(t, r.g.WriteStubs())
	if err := r.g.WriteStubs(); err == nil {
		t.Fatal("the stubs were written twice")
	}
	must(t, r.g.CheckStubs())
	if n, err := r.g.StubCalls(); err != nil || n != 0 {
		t.Fatalf("StubCalls = %d, %v before any call", n, err)
	}
	// Each stub only logs; the dialogs decline. Run with a PATH of /usr/bin
	// and /bin alone, as a stub is run, from the test's temp B.
	for _, n := range StubNames {
		cmd := exec.Command(r.path(StubDir + "/" + n))
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		err := cmd.Run()
		dialog := n == "zenity" || n == "kdialog"
		var ee *exec.ExitError
		if dialog && !errors.As(err, &ee) || !dialog && err != nil {
			t.Errorf("stub %s: %v", n, err)
		}
	}
	if n, err := r.g.StubCalls(); err != nil || n != len(StubNames) {
		t.Fatalf("StubCalls = %d, %v after %d calls", n, err, len(StubNames))
	}
	// The openers record what they were asked to open, and only they.
	for _, n := range []string{"open", "xdg-open", "osascript"} {
		cmd := exec.Command(r.path(StubDir+"/"+n), "http://example.invalid/"+n)
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		must(t, cmd.Run())
	}
	if got, err := r.g.StubOpened(); err != nil || strings.Join(got, ",") != ",,http://example.invalid/open,http://example.invalid/xdg-open" {
		t.Fatalf("StubOpened = %q, %v", got, err)
	}
	if n, err := r.g.StubCalls(); err != nil || n != len(StubNames)+3 {
		t.Fatalf("StubCalls = %d, %v", n, err)
	}
	// The suite's alias scripts may sit beside them; a link may not.
	r.write(StubDir+"/conf-marker", "#!/bin/sh\n")
	must(t, r.g.CheckStubs())
	must(t, os.Symlink("/bin/true", r.path(StubDir+"/x")))
	if err := r.g.CheckStubs(); err == nil {
		t.Fatal("a symlink among the stubs was taken")
	}
	must(t, os.Remove(r.path(StubDir+"/x")))
	r.write(StubDir+"/open", "#!/bin/sh\nexec /usr/bin/open \"$@\"\n")
	if err := r.g.CheckStubs(); err == nil {
		t.Fatal("a changed stub was taken")
	}
}

func TestReadCanary(t *testing.T) {
	r := newRig(t, config.KindContainer)
	h := &GuardHost{G: r.g}
	c, err := h.MakeCanary("canary", "alias-out", Dir)
	must(t, err)
	r.write(c.Rel+"/ran", "ran\n")
	if b, err := h.ReadCanary(c.Rel + "/ran"); err != nil || string(b) != "ran\n" {
		t.Fatalf("ReadCanary = %q, %v", b, err)
	}
	if _, err := h.ReadCanary(c.Rel + "/none"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing file: %v", err)
	}
	for _, rel := range []string{"home/.conformance", r.g.DataMarker(), c.Rel + "/../x", "/etc/passwd", ""} {
		if _, err := h.ReadCanary(rel); err == nil {
			t.Errorf("%q was read", rel)
		}
	}
	// A symlink below the canary, or replacing it, is never followed.
	other := t.TempDir() + "/secret"
	must(t, os.WriteFile(other, []byte("secret"), 0o600))
	must(t, os.Symlink(other, r.path(c.Rel+"/env")))
	if b, err := h.ReadCanary(c.Rel + "/env"); err == nil {
		t.Fatalf("a symlink was followed: %q", b)
	}
	must(t, os.Rename(r.path(c.Rel), r.path("canary/moved")))
	must(t, os.Symlink(r.path("canary/moved"), r.path(c.Rel)))
	if b, err := h.ReadCanary(c.Rel + "/ran"); err == nil {
		t.Fatalf("a symlinked canary was followed: %q", b)
	}
}

func TestPopulate(t *testing.T) {
	r := newRig(t, config.KindContainer)
	h := &GuardHost{G: r.g}
	d, err := h.MakeCanary("canary", "rst-dir", Dir)
	must(t, err)
	f, err := h.MakeCanary("canary", "rst-file", File)
	must(t, err)
	must(t, h.Populate(d, 3))
	es, err := os.ReadDir(r.path(d.Rel))
	must(t, err)
	if len(es) != 3 {
		t.Fatalf("%d files", len(es))
	}
	b, err := os.ReadFile(r.path(d.Rel + "/" + path.Base(d.Rel) + "-p2"))
	if err != nil || !strings.HasPrefix(string(b), "caboose-conformance "+r.g.Nonce()) {
		t.Fatalf("%q %v", b, err)
	}
	if err := h.Populate(d, 1); err == nil {
		t.Fatal("a file was written over")
	}
	if err := h.Populate(f, 1); err == nil {
		t.Fatal("a file canary was populated")
	}
	if err := h.Populate(Canary{Rel: "roots", Kind: Dir}, 1); err == nil {
		t.Fatal("a non-canary was populated")
	}
	if err := h.Populate(d, 0); err == nil {
		t.Fatal("no files is a number")
	}
}

func TestStoreTree(t *testing.T) {
	r := newRig(t, config.KindContainer)
	top := r.g.StoreRel() + "/k/20260101T000000Z/tree"
	r.write(top+"/a/b", "x")
	r.write(top+"/c", "y")
	tree, err := r.g.StoreTree(top)
	must(t, err)
	if len(tree) != 3 || tree["a/b"].Hash == "" || !tree["a"].Mode.IsDir() {
		t.Fatalf("tree %v", tree)
	}
	must(t, os.Symlink("/etc", r.path(top+"/l")))
	tree, err = r.g.StoreTree(top)
	must(t, err)
	if tree["l"].Mode&fs.ModeSymlink == 0 {
		t.Fatal("a symlink in the copy was followed")
	}
	for _, bad := range []string{"roots", r.g.StoreRel(), r.g.StoreRel() + "/../x", "home/envs/other/checkpoints/k"} {
		if _, err := r.g.StoreTree(bad); err == nil {
			t.Errorf("%s was read as a checkpoint", bad)
		}
	}
}
