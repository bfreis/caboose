package conformance

import (
	"os"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/config"
)

func TestMakeCanary(t *testing.T) {
	r := newRig(t, config.KindContainer)
	g := r.g
	part := "roots/" + g.RootNames()[1] + "/" + WritablePart
	f, err := g.MakeCanary(part, "wr-file", File)
	if err != nil {
		t.Fatal(err)
	}
	if f.Rel != part+"/conf-"+g.Nonce8()+"-wr-file" || len(f.Paths) != 1 {
		t.Fatalf("file canary %+v", f)
	}
	if b := mustRead(t, r.path(f.Rel)); !strings.HasPrefix(b, "caboose-conformance "+g.Nonce()) {
		t.Fatalf("content %q", b)
	}
	d, err := g.MakeCanary(part, "wr-dir", Dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(r.path(d.Rel)); err != nil || !fi.IsDir() {
		t.Fatalf("dir canary: %v", err)
	}
	tr, err := g.MakeCanary("roots/"+g.RootNames()[0], "ro-tree", Tree)
	if err != nil {
		t.Fatal(err)
	}
	dirs, files := 0, 0
	for _, p := range tr.Paths {
		fi, err := os.Lstat(r.path(p))
		if err != nil {
			t.Fatal(err)
		}
		if fi.IsDir() {
			dirs++
		} else {
			files++
		}
		if !g.IsCanary(p) {
			t.Fatalf("%s not recorded", p)
		}
		if _, ok := g.CanaryIdentity(p); !ok {
			t.Fatalf("%s has no identity", p)
		}
	}
	if dirs != 3 || files != 6 {
		t.Fatalf("tree has %d dirs, %d files", dirs, files)
	}
	if _, err := g.MakeCanary(part, "wr-file", File); err == nil {
		t.Fatal("a role made twice")
	}
	for _, role := range []string{"", "Up", "a/b", "../x", strings.Repeat("a", 30)} {
		if _, err := g.MakeCanary(part, role, File); err == nil {
			t.Fatalf("role %q taken", role)
		}
	}
	for _, parent := range []string{"", "/tmp", "../x", "roots/../..", "nowhere"} {
		if _, err := g.MakeCanary(parent, "p", File); err == nil {
			t.Fatalf("parent %q taken", parent)
		}
	}
	// A parent that is a symlink is never followed.
	must(t, os.Symlink(r.real, r.path("canary/link")))
	if _, err := g.MakeCanary("canary/link", "outside", File); err == nil {
		t.Fatal("a symlinked parent was followed")
	}
	if es, _ := os.ReadDir(r.real); len(es) != 0 {
		t.Fatalf("wrote through a symlink: %v", es)
	}
	if _, err := (&Guard{}).MakeCanary(part, "x", File); err == nil {
		t.Fatal("a zero guard made a canary")
	}
}

func TestMkdirNoFollow(t *testing.T) {
	base := t.TempDir()
	out := t.TempDir()
	must(t, os.Symlink(out, base+"/l"))
	if err := mkdirNoFollow(base, "l/x"); err == nil {
		t.Fatal("went through a symlink")
	}
	if es, _ := os.ReadDir(out); len(es) != 0 {
		t.Fatalf("made %v outside", es)
	}
	for _, bad := range []string{"", "a//b", "../a", "a/./b"} {
		if err := mkdirNoFollow(base, bad); err == nil {
			t.Fatalf("%q taken", bad)
		}
	}
	if err := mkdirNoFollow(base, "a/b/c"); err != nil {
		t.Fatal(err)
	}
	if err := mkdirNoFollow(base, "a/b/c"); err != nil {
		t.Fatalf("an existing directory: %v", err)
	}
}
