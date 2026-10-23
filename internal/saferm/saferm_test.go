package saferm

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func mk(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestRemoveAllInside(t *testing.T) {
	root := t.TempDir()
	mk(t, filepath.Join(root, "a", "b", "f"))
	if err := RemoveAll(root, filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(root, "a")) || !exists(root) {
		t.Fatal("expected a gone and root kept")
	}
	// Missing path is fine.
	if err := RemoveAll(root, filepath.Join(root, "nope", "x")); err != nil {
		t.Fatal(err)
	}
}

// layout gives a test a base directory of its own (a t.TempDir) holding
// the root and an "outside" directory, and makes it the working directory,
// so that every path a test hands a removal, empty and relative ones too,
// stays under the base even if the guard were broken.
func layout(t *testing.T) (base, root, outside string) {
	t.Helper()
	base = t.TempDir()
	t.Chdir(base)
	root, outside = filepath.Join(base, "root"), filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return base, root, outside
}

func TestRefusals(t *testing.T) {
	base, root, outside := layout(t)
	mk(t, filepath.Join(outside, "keep"))
	mk(t, filepath.Join(root, "in"))
	cases := map[string]string{
		"root itself":    root,
		"root with dot":  filepath.Join(root, "."),
		"parent":         base,
		"outside":        outside,
		"dotdot escape":  filepath.Join(root, "..", "outside"),
		"file outside":   filepath.Join(outside, "keep"),
		"relative":       "outside",
		"prefix sibling": root + "-sibling",
	}
	for name, p := range cases {
		err := RemoveAll(root, p)
		if err == nil {
			t.Errorf("%s: expected refusal", name)
		}
		if err != nil && !errors.Is(err, ErrOutside) {
			t.Errorf("%s: unexpected error %v", name, err)
		}
	}
	if !exists(outside) || !exists(filepath.Join(outside, "keep")) || !exists(filepath.Join(root, "in")) {
		t.Fatal("something was removed")
	}
	for _, bad := range []string{"", " "} {
		if err := RemoveAll(root, bad); err == nil && bad == "" {
			t.Error("empty path accepted")
		}
	}
	if err := RemoveAll("", filepath.Join(root, "in")); err == nil {
		t.Error("empty root accepted")
	}
	if !exists(filepath.Join(root, "in")) || !exists(filepath.Join(outside, "keep")) {
		t.Fatal("removed with an empty path or root")
	}
}

func TestSymlinkEscapes(t *testing.T) {
	base, root, outside := layout(t)
	mk(t, filepath.Join(outside, "d", "keep"))
	// A directory link inside root pointing out: paths through it are outside.
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(root, filepath.Join(root, "link", "d")); !errors.Is(err, ErrOutside) {
		t.Fatalf("want ErrOutside, got %v", err)
	}
	if !exists(filepath.Join(outside, "d", "keep")) {
		t.Fatal("removed through a symlink")
	}
	// The link itself is removed as a link, its target untouched.
	if err := RemoveAll(root, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(root, "link")) || !exists(filepath.Join(outside, "d", "keep")) {
		t.Fatal("link not removed alone")
	}
	// A root given as a symlink is resolved.
	real := t.TempDir()
	mk(t, filepath.Join(real, "f"))
	ln := filepath.Join(t.TempDir(), "rootlink")
	if err := os.Symlink(real, ln); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(ln, filepath.Join(ln, "f")); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(real, "f")) {
		t.Fatal("f should be gone")
	}
	// A path given through a symlink to outside the root.
	if err := os.Symlink(base, filepath.Join(root, "up")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(root, filepath.Join(root, "up", "outside")); !errors.Is(err, ErrOutside) {
		t.Fatalf("want ErrOutside, got %v", err)
	}
	if !exists(outside) {
		t.Fatal("outside removed")
	}
}

func TestRemove(t *testing.T) {
	_, root, outside := layout(t)
	mk(t, filepath.Join(root, "f"))
	mk(t, filepath.Join(outside, "g"))
	if err := Remove(root, filepath.Join(outside, "g")); !errors.Is(err, ErrOutside) {
		t.Fatalf("want ErrOutside, got %v", err)
	}
	if err := Remove(root, root); !errors.Is(err, ErrOutside) {
		t.Fatalf("want ErrOutside, got %v", err)
	}
	if err := Remove(root, filepath.Join(root, "f")); err != nil || exists(filepath.Join(root, "f")) {
		t.Fatal("f not removed", err)
	}
}

func TestRemoveRoot(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	mk(t, filepath.Join(base, "x"))
	if err := RemoveRoot(""); err == nil {
		t.Error("empty root accepted")
	}
	// The home directory (a stand-in here) is refused.
	home := filepath.Join(base, "home")
	mk(t, filepath.Join(home, "keep"))
	t.Setenv("HOME", home)
	if err := RemoveRoot(home); err == nil || !exists(filepath.Join(home, "keep")) {
		t.Error("home accepted")
	}
	if !exists(filepath.Join(base, "x")) {
		t.Fatal("removed with an empty root")
	}
	f := filepath.Join(t.TempDir(), "file")
	mk(t, f)
	if err := RemoveRoot(f); err == nil || !exists(f) {
		t.Error("a file accepted as root")
	}
	d := filepath.Join(t.TempDir(), "d")
	mk(t, filepath.Join(d, "x"))
	if err := RemoveRoot(d); err != nil || exists(d) {
		t.Fatal("root not removed", err)
	}
}
