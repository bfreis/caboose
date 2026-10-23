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

func TestReadFileMax(t *testing.T) {
	top, _ := layout(t)
	for max, ok := range map[int64]bool{4: false, 5: true, 6: true} {
		data, _, err := Dir(top).ReadFileMax("a/b/f", max)
		if ok != (err == nil) || (ok && string(data) != "plain") || (!ok && !errors.Is(err, ErrTooLarge)) {
			t.Errorf("max %d: %q, %v", max, data, err)
		}
	}
	// A sparse file far over the limit is refused by its size, unread.
	big := filepath.Join(top, "big")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 40); err != nil {
		t.Skip("no sparse files here:", err)
	}
	f.Close()
	if _, _, err := Dir(top).ReadFileMax("big", 1<<20); !errors.Is(err, ErrTooLarge) {
		t.Errorf("sparse: %v", err)
	}
	// Links are refused as ReadFile refuses them.
	symlink(t, filepath.Join(top, "secret"), filepath.Join(top, "ln"))
	if _, _, err := Dir(top).ReadFileMax("ln", 100); !errors.Is(err, ErrNotPlain) {
		t.Errorf("link: %v", err)
	}
	if _, _, err := Dir(top).ReadFileMax("a/b/missing", 100); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: %v", err)
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

func TestRename(t *testing.T) {
	top, outside := layout(t)
	d := Dir(top)
	if err := d.Rename("a/b/f", "a/b/g"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(top, "a", "b", "g")); got != "plain" {
		t.Errorf("renamed file holds %q", got)
	}
	// A symlink is moved as a link: what it names stays where it is.
	symlink(t, filepath.Join(outside, "secret"), filepath.Join(top, "a", "b", "out"))
	if err := d.Rename("a/b/out", "a/b/out.aside"); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(top, "a", "b", "out.aside")); err != nil || target != filepath.Join(outside, "secret") {
		t.Errorf("moved link: %q, %v", target, err)
	}
	if read(t, filepath.Join(outside, "secret")) != "outside secret" {
		t.Error("the outside secret was touched")
	}
	if _, err := os.Lstat(filepath.Join(outside, "secret.aside")); err == nil {
		t.Error("the link's target was renamed")
	}
	// A symlinked directory is moved as a link too, never entered.
	symlink(t, outside, filepath.Join(top, "a", "lo"))
	if err := d.Rename("a/lo", "a/lo.aside"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(top, "a", "lo.aside")); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("moved dir link: %v, %v", fi, err)
	}
	// A FIFO is moved without being opened (an open would block).
	if err := syscall.Mkfifo(filepath.Join(top, "a", "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.Rename("a/fifo", "a/fifo.aside"); err != nil {
		t.Fatal(err)
	}
	// An existing target is refused, and both stay.
	if err := d.Rename("a/b/g", "a/b/out.aside"); !errors.Is(err, fs.ErrExist) {
		t.Errorf("over an existing entry: %v", err)
	}
	if got := read(t, filepath.Join(top, "a", "b", "g")); got != "plain" {
		t.Errorf("source now %q", got)
	}
	if err := d.Rename("a/b/missing", "a/b/x"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing source: %v", err)
	}
	// Different directories are refused.
	for _, pair := range [][2]string{{"a/b/g", "a/g"}, {"a/b/g", "g"}, {"secret", "a/secret"}} {
		if err := d.Rename(pair[0], pair[1]); !errors.Is(err, ErrRenameAcross) {
			t.Errorf("rename %s to %s: %v, want ErrRenameAcross", pair[0], pair[1], err)
		}
	}
	for _, pair := range [][2]string{{"", "x"}, {"x", "."}, {"../x", "../y"}, {"/abs", "/abs2"}, {"a/../x", "a/../y"}} {
		if err := d.Rename(pair[0], pair[1]); err == nil {
			t.Errorf("rename %q to %q: no error", pair[0], pair[1])
		}
	}
}

// A symlink in a parent component is refused, and nothing it names moves.
func TestRenameRefusesLinkedParent(t *testing.T) {
	top, outside := layout(t)
	symlink(t, outside, filepath.Join(top, "a", "lo"))
	notPlain(t, "through a symlinked dir", Dir(top).Rename("a/lo/secret", "a/lo/moved"))
	if read(t, filepath.Join(outside, "secret")) != "outside secret" {
		t.Error("the outside secret was moved")
	}
	if _, err := os.Lstat(filepath.Join(outside, "moved")); err == nil {
		t.Error("something was renamed outside")
	}
}

// A directory component swapped for a symlink during the walk is refused,
// and nothing it names is renamed: one leaving the top (os.Root refuses it)
// and one staying inside, which only the identity check catches.
func TestRenameSwapIsCaught(t *testing.T) {
	for _, inside := range []bool{false, true} {
		t.Run(map[bool]string{false: "outside", true: "inside"}[inside], func(t *testing.T) {
			top, outside := layout(t)
			target, link := outside, outside
			if inside {
				// Beside b, where os.Root follows a relative link.
				target, link = filepath.Join(top, "a", "decoy"), "decoy"
				if err := os.Mkdir(target, 0o777); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(target, "f"), []byte("decoy f"), 0o644); err != nil {
				t.Fatal(err)
			}
			testHookOpened = func(name string) {
				if name != "a/b" {
					return
				}
				p := filepath.Join(top, "a", "b")
				if err := os.Rename(p, p+".moved"); err != nil {
					t.Fatal(err)
				}
				symlink(t, link, p)
			}
			defer func() { testHookOpened = nil }()
			err := Dir(top).Rename("a/b/f", "a/b/g")
			if inside {
				notPlain(t, "swapped a/b", err)
			} else if err == nil {
				t.Error("swapped a/b: no error")
			}
			if read(t, filepath.Join(target, "f")) != "decoy f" {
				t.Error("the decoy was moved")
			}
			if _, err := os.Lstat(filepath.Join(target, "g")); err == nil {
				t.Error("something was renamed through the link")
			}
			if read(t, filepath.Join(top, "a", "b.moved", "f")) != "plain" {
				t.Error("the original file moved")
			}
		})
	}
}

// The directory swapped for a symlink after it was opened, just before the
// rename: the rename happens in the directory that was opened, never
// through the link.
func TestRenameLateSwap(t *testing.T) {
	top, outside := layout(t)
	if err := os.WriteFile(filepath.Join(outside, "f"), []byte("outside f"), 0o644); err != nil {
		t.Fatal(err)
	}
	testHookRename = func(string) {
		p := filepath.Join(top, "a", "b")
		if err := os.Rename(p, p+".moved"); err != nil {
			t.Fatal(err)
		}
		symlink(t, outside, p)
	}
	defer func() { testHookRename = nil }()
	if err := Dir(top).Rename("a/b/f", "a/b/g"); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(outside, "f")) != "outside f" {
		t.Error("the outside file was moved")
	}
	if _, err := os.Lstat(filepath.Join(outside, "g")); err == nil {
		t.Error("something was renamed outside")
	}
	if read(t, filepath.Join(top, "a", "b.moved", "g")) != "plain" {
		t.Error("the file was not renamed in the directory opened")
	}
}
