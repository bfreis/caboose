package checkpoint

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// gitConfigWithCommand is a git config that runs a command; a restore
// leaves it as it is.
const gitConfigWithCommand = "[core]\n\trepositoryformatversion = 0\n\tfsmonitor = /tmp/evil.sh\n\tbare = false\n"

// underTemp fails the test unless p is below the system's temp directory:
// every tree these tests change or remove is.
func underTemp(t *testing.T, p string) {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rp, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	if !within(tmp, rp) || rp == tmp {
		t.Fatalf("%s is not below %s", rp, tmp)
	}
}

// sanTree is a fresh dir holding an empty tree "out" to sanitize.
func sanTree(t *testing.T) (dir, out string) {
	t.Helper()
	dir = tempTree(t)
	out = filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, out
}

// sanitizeT runs sanitizeTree with a timeout: nothing it does may block
// (a fifo behind a symlink is never opened).
func sanitizeT(t *testing.T, dir string, file bool) []Sanitized {
	t.Helper()
	underTemp(t, dir)
	type res struct {
		s   []Sanitized
		err error
	}
	ch := make(chan res, 1)
	go func() {
		s, err := sanitizeDir(dir, file, 0)
		ch <- res{s, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.s
	case <-time.After(30 * time.Second):
		t.Fatal("sanitizeTree did not return")
		return nil
	}
}

// sanitizeDir runs sanitizeTree on dir/out.
func sanitizeDir(dir string, file bool, max int) ([]Sanitized, error) {
	d, err := openDirPath(dir)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	s, _, err := sanitizeTree(context.Background(), d, "out", file, max)
	return s, err
}

func readT(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func findSan(ss []Sanitized, rel string) *Sanitized {
	for i := range ss {
		if ss[i].Path == rel {
			return &ss[i]
		}
	}
	return nil
}

func modeOf(t *testing.T, p string) uint32 {
	t.Helper()
	st := lstat(t, p)
	return uint32(st.Mode) & 0o7777
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func TestSanitizeModes(t *testing.T) {
	skipRoot(t)
	dir, out := sanTree(t)
	outside := tempTree(t)
	suid := filepath.Join(outside, "suid")
	mustWrite(t, suid, "x", 0o755)
	if err := unix.Chmod(suid, 0o4755); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, suid, filepath.Join(out, "link"))
	mustWrite(t, filepath.Join(out, "f-suid"), "x", 0o755)
	mustWrite(t, filepath.Join(out, "f-sgid"), "x", 0o755)
	mustWrite(t, filepath.Join(out, "locked", "f"), "x", 0o755)
	if err := os.MkdirAll(filepath.Join(out, "d-sgid"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(out, "d-sticky"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(out, "ro", ".git", "config"), gitConfigWithCommand, 0o644)
	chmod := func(p string, m uint32) {
		t.Helper()
		if err := unix.Chmod(p, m); err != nil {
			t.Fatal(err)
		}
	}
	chmod(filepath.Join(out, "f-suid"), 0o4755)
	// setgid on a file takes being in its group, which some systems'
	// temp directories do not give: checked only where it took.
	_ = unix.Chmod(filepath.Join(out, "f-sgid"), 0o2755)
	sgid := modeOf(t, filepath.Join(out, "f-sgid"))&0o2000 != 0
	chmod(filepath.Join(out, "locked", "f"), 0o4000) // no owner read: changed without opening it to read
	chmod(filepath.Join(out, "locked"), 0)
	chmod(filepath.Join(out, "d-sgid"), 0o2755)
	chmod(filepath.Join(out, "d-sticky"), 0o1777)
	chmod(filepath.Join(out, "ro", ".git"), 0o555)
	chmod(filepath.Join(out, "ro"), 0o500)

	ss := sanitizeT(t, dir, false)

	want := map[string]uint32{"f-suid": 0o755, "d-sgid": 0o755, "d-sticky": 0o777, "locked": 0, "ro/.git": 0o555, "ro": 0o500}
	if got := modeOf(t, filepath.Join(out, "f-sgid")); got != 0o755 {
		t.Errorf("f-sgid mode %o", got)
	}
	if s := findSan(ss, "f-sgid"); sgid && (s == nil || s.Action != ActionMode) {
		t.Errorf("f-sgid reported %+v", s)
	}
	for rel, m := range want {
		if got := modeOf(t, filepath.Join(out, rel)); got != m {
			t.Errorf("%s mode %o, want %o", rel, got, m)
		}
	}
	chmod(filepath.Join(out, "locked"), 0o700)
	if got := modeOf(t, filepath.Join(out, "locked", "f")); got != 0 {
		t.Errorf("locked/f mode %o", got)
	}
	chmod(filepath.Join(out, "ro"), 0o700)
	if got := readT(t, filepath.Join(out, "ro", ".git", "config")); got != gitConfigWithCommand {
		t.Errorf("config in a read-only .git changed: %q", got)
	}
	if got := modeOf(t, suid); got != 0o4755 {
		t.Errorf("the setuid file a symlink names changed: %o", got)
	}
	for _, rel := range []string{"f-suid", "d-sgid", "d-sticky", "locked/f"} {
		if s := findSan(ss, rel); s == nil || s.Action != ActionMode {
			t.Errorf("%s reported %+v", rel, s)
		}
	}
	if findSan(ss, "link") != nil {
		t.Error("the symlink reported")
	}
}

func TestSanitizeFile(t *testing.T) {
	dir := tempTree(t)
	f := filepath.Join(dir, "out")
	mustWrite(t, f, "x", 0o755)
	if err := unix.Chmod(f, 0o4755); err != nil {
		t.Fatal(err)
	}
	ss := sanitizeT(t, dir, true)
	if m := modeOf(t, f); m != 0o755 || len(ss) != 1 || ss[0].Path != "." {
		t.Errorf("mode %o, %+v", m, ss)
	}
}

func TestSanitizeNoDepthBound(t *testing.T) {
	dir, out := sanTree(t)
	deep := filepath.Join(out+strings.Repeat("/d", maxDepth+50), "f")
	mustWrite(t, deep, "x", 0o644)
	if err := unix.Chmod(deep, 0o4755); err != nil {
		t.Fatal(err)
	}
	sanitizeT(t, dir, false)
	if got := modeOf(t, deep); got != 0o755 {
		t.Errorf("deep setuid file mode %o", got)
	}
}

func TestSanitizeMaxEntries(t *testing.T) {
	dir, out := sanTree(t)
	for i := range 10 {
		mustWrite(t, filepath.Join(out, fmt.Sprint(i)), "x", 0o644)
	}
	if _, err := sanitizeDir(dir, false, 5); err == nil {
		t.Fatal("no error past max entries")
	}
}
