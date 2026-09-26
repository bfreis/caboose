package nofollow

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// layout makes, under a temp dir, a "top" the tests work in and a secret
// both inside it (top/secret, as .credentials.json is inside the data dir)
// and outside it (outside/secret).
func layout(t *testing.T) (top, outside string) {
	t.Helper()
	base := t.TempDir()
	top, outside = filepath.Join(base, "top"), filepath.Join(base, "outside")
	for _, dir := range []string{filepath.Join(top, "a", "b"), outside} {
		if err := os.MkdirAll(dir, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	for path, data := range map[string]string{
		filepath.Join(top, "a", "b", "f"): "plain",
		filepath.Join(top, "secret"):      "inside secret",
		filepath.Join(outside, "secret"):  "outside secret",
	} {
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return top, outside
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func notPlain(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, ErrNotPlain) {
		t.Errorf("%s: err = %v, want ErrNotPlain", what, err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReadFile(t *testing.T) {
	top, _ := layout(t)
	data, mode, err := Dir(top).ReadFile("a/b/f")
	if err != nil || string(data) != "plain" || !mode.IsRegular() {
		t.Fatalf("ReadFile = %q, %v, %v", data, mode, err)
	}
	if _, _, err := Dir(top).ReadFile("a/b/missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: err = %v, want ErrNotExist", err)
	}
	for _, rel := range []string{"../x", "/a", "a/../secret", "a//b", ""} {
		if _, _, err := Dir(top).ReadFile(rel); err == nil {
			t.Errorf("ReadFile(%q) succeeded", rel)
		}
	}
}

// Every way to reach a secret: a symlinked file, a symlinked directory on
// the way (both staying inside top, which os.Root alone would follow), and
// a hard link.
func TestReadRefusesLinks(t *testing.T) {
	top, outside := layout(t)
	symlink(t, "../../secret", filepath.Join(top, "a", "b", "in"))
	symlink(t, filepath.Join(outside, "secret"), filepath.Join(top, "a", "b", "out"))
	symlink(t, "b", filepath.Join(top, "a", "lb"))
	if err := os.Link(filepath.Join(top, "secret"), filepath.Join(top, "a", "b", "hard")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"a/b/in", "a/b/out", "a/lb/f", "a/b/hard"} {
		data, _, err := Dir(top).ReadFile(rel)
		notPlain(t, rel, err)
		if data != nil {
			t.Errorf("%s: read %q", rel, data)
		}
	}
}

// A component swapped for a symlink between its Lstat and its opening is
// caught by the identity check, for a directory and for the file itself.
func TestSwapIsCaught(t *testing.T) {
	for _, swap := range []string{"a", "a/b/f"} {
		t.Run(swap, func(t *testing.T) {
			top, _ := layout(t)
			testHookOpened = func(name string) {
				if name != swap {
					return
				}
				p := filepath.Join(top, filepath.FromSlash(name))
				if err := os.Rename(p, p+".moved"); err != nil {
					t.Fatal(err)
				}
				// Staying inside the directory, where os.Root follows it:
				// only the identity check can catch it.
				var target string
				if strings.Contains(name, "/") {
					if err := os.WriteFile(filepath.Join(top, "a", "b", "g"), []byte("sibling"), 0o644); err != nil {
						t.Fatal(err)
					}
					target = "g"
				} else {
					if err := os.MkdirAll(filepath.Join(top, "decoy", "b"), 0o777); err != nil {
						t.Fatal(err)
					}
					// A plain file: only the check on "a" itself stands
					// between the reader and it.
					if err := os.WriteFile(filepath.Join(top, "decoy", "b", "f"), []byte("decoy"), 0o644); err != nil {
						t.Fatal(err)
					}
					target = "decoy"
				}
				symlink(t, target, p)
			}
			defer func() { testHookOpened = nil }()
			data, _, err := Dir(top).ReadFile("a/b/f")
			notPlain(t, "swapped "+swap, err)
			if data != nil {
				t.Errorf("read %q", data)
			}
		})
	}
}

func TestWriteFile(t *testing.T) {
	top, _ := layout(t)
	d := Dir(top)
	if err := d.WriteFile("a/new/dir/f", []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(top, "a", "new", "dir", "f")
	if got := read(t, p); got != "new" {
		t.Errorf("wrote %q", got)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o755 {
		t.Errorf("mode %v, want 0755", fi.Mode().Perm())
	}
	if err := d.WriteFile("a/b/f", []byte("over"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(top, "a", "b", "f")); got != "over" {
		t.Errorf("overwrote with %q", got)
	}
	es, _ := os.ReadDir(filepath.Join(top, "a", "b"))
	if len(es) != 1 {
		t.Errorf("left behind: %v", es)
	}
	if err := d.WriteFile("a/b", nil, 0o644); err == nil {
		t.Error("wrote over a directory")
	}
}

// A symlink at the target is replaced by the file; a symlinked directory
// on the way is refused. Neither secret changes.
func TestWriteNeverFollows(t *testing.T) {
	top, outside := layout(t)
	d := Dir(top)
	symlink(t, filepath.Join(outside, "secret"), filepath.Join(top, "a", "b", "out"))
	symlink(t, "../../secret", filepath.Join(top, "a", "b", "in"))
	symlink(t, outside, filepath.Join(top, "a", "lo"))
	for _, rel := range []string{"a/b/out", "a/b/in"} {
		if err := d.WriteFile(rel, []byte("written"), 0o644); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Lstat(filepath.Join(top, filepath.FromSlash(rel)))
		if err != nil || !fi.Mode().IsRegular() {
			t.Errorf("%s: not replaced by a file: %v, %v", rel, fi, err)
		}
	}
	notPlain(t, "through a symlinked dir", d.WriteFile("a/lo/secret", []byte("written"), 0o644))
	notPlain(t, "into a symlinked dir", d.WriteFile("a/lo/new", []byte("written"), 0o644))
	if got := read(t, filepath.Join(outside, "secret")); got != "outside secret" {
		t.Errorf("outside secret is now %q", got)
	}
	if got := read(t, filepath.Join(top, "secret")); got != "inside secret" {
		t.Errorf("inside secret is now %q", got)
	}
	if _, err := os.Lstat(filepath.Join(outside, "new")); err == nil {
		t.Error("a file was created outside")
	}
}

func TestWriteInPlace(t *testing.T) {
	top, outside := layout(t)
	d := Dir(top)
	p := filepath.Join(top, "a", "b", "f")
	before, _ := os.Stat(p)
	if err := d.WriteInPlace("a/b/f", []byte("in place")); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(p)
	if !os.SameFile(before, after) || read(t, p) != "in place" {
		t.Errorf("not written in place: same file %v, content %q", os.SameFile(before, after), read(t, p))
	}
	symlink(t, filepath.Join(outside, "secret"), filepath.Join(top, "a", "b", "out"))
	notPlain(t, "symlink", d.WriteInPlace("a/b/out", []byte("x")))
	if got := read(t, filepath.Join(outside, "secret")); got != "outside secret" {
		t.Errorf("outside secret is now %q", got)
	}
	// Swapped after the check, for a sibling os.Root would follow the
	// symlink to: refused before anything is truncated.
	g := filepath.Join(top, "a", "b", "g")
	if err := os.WriteFile(g, []byte("sibling"), 0o644); err != nil {
		t.Fatal(err)
	}
	testHookOpened = func(name string) {
		if name == "a/b/f" {
			_ = os.Rename(p, p+".moved")
			symlink(t, "g", p)
		}
	}
	defer func() { testHookOpened = nil }()
	notPlain(t, "swapped", d.WriteInPlace("a/b/f", []byte("x")))
	if got := read(t, g); got != "sibling" {
		t.Errorf("the sibling is now %q", got)
	}
}

func TestRemove(t *testing.T) {
	top, outside := layout(t)
	d := Dir(top)
	symlink(t, filepath.Join(outside, "secret"), filepath.Join(top, "a", "b", "out"))
	symlink(t, outside, filepath.Join(top, "a", "lo"))
	if err := d.Remove("a/b/out"); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove("a/lo"); err != nil { // the link, not what it names
		t.Fatal(err)
	}
	notPlain(t, "through a symlinked dir", func() error {
		symlink(t, outside, filepath.Join(top, "a", "lo"))
		return d.Remove("a/lo/secret")
	}())
	if read(t, filepath.Join(outside, "secret")) != "outside secret" {
		t.Error("the outside secret was touched")
	}
	if err := d.Remove("a/b"); !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
		t.Errorf("removing a non-empty dir: %v", err)
	}
}

func TestWalk(t *testing.T) {
	top, outside := layout(t)
	d := Dir(top)
	symlink(t, outside, filepath.Join(top, "a", "lo"))
	if err := os.MkdirAll(filepath.Join(top, "a", "skip", "deep"), 0o777); err != nil {
		t.Fatal(err)
	}
	var seen []string
	err := d.Walk("a", func(rel string, e fs.DirEntry) error {
		seen = append(seen, rel+":"+e.Type().String())
		if rel == "a/skip" {
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "a/b:d---------,a/b/f:----------,a/lo:L---------,a/skip:d---------"
	if got := strings.Join(seen, ","); got != want {
		t.Errorf("walked\n  %s\nwant\n  %s", got, want)
	}
	if err := d.Walk("missing", func(string, fs.DirEntry) error { t.Error("called"); return nil }); err != nil {
		t.Errorf("walking a missing dir: %v", err)
	}
}
