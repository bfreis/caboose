package diskusage

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// On APFS a fresh clone shares every block with its source, so neither
// has much of its own; writing part of the clone gives it that part.
// $TMPDIR must be on APFS, as a Mac's is.
func TestAPFSPrivateSize(t *testing.T) {
	p := DefaultUnshared()
	if p == nil {
		t.Fatal("no UnsharedSizer on macOS")
	}
	dir := t.TempDir()
	src, clone := filepath.Join(dir, "src"), filepath.Join(dir, "clone")
	data := make([]byte, 8<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	priv := func(name string) int64 {
		t.Helper()
		fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close(fd)
		n, err := p.UnsharedSize(fd, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return n
	}
	if n := priv("src"); n < 8<<20 {
		t.Errorf("before cloning, src's private size is %d, want its 8 MiB", n)
	}
	if err := unix.Clonefile(src, clone, 0); err != nil {
		t.Fatal(err)
	}
	if a, b := priv("src"), priv("clone"); a > 64<<10 || b > 64<<10 {
		t.Errorf("fresh clone: private src %d, clone %d, want ~0", a, b)
	}
	f, err := os.OpenFile(clone, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(make([]byte, 1<<20), 2<<20); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	b := priv("clone")
	if b < 1<<20 || b > 2<<20 {
		t.Errorf("clone after writing 1 MiB: private %d", b)
	}
	t.Logf("clone's private size after a 1 MiB write: %d", b)

	w := Walker{Unshared: p}
	u := walk(&w, dir, nil)
	if !u.UnsharedComplete() || u.Allocated < 16<<20 || u.Unshared < 1<<20 || u.Unshared > 3<<20 {
		t.Errorf("walk: %+v", u)
	}
	t.Logf("walk: %+v", u)

	// A symlink is never followed: its private size is its own, if any.
	if err := os.Symlink(src, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if n, err := p.UnsharedSize(fd, "link"); err == nil && n >= 1<<20 {
		t.Errorf("link: %d, the target's", n)
	}
}

// A failed call's errno is the call's own, as purego's trampoline read it
// at once: ENOENT for a name that is not there, ENOTDIR for a directory
// descriptor that is a file's. Neither is ErrUnsupported.
func TestAPFSPrivateSizeErrno(t *testing.T) {
	p := DefaultUnshared()
	if p == nil {
		t.Fatal("no UnsharedSizer on macOS")
	}
	dir := t.TempDir()
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if _, err := p.UnsharedSize(fd, "missing"); !errors.Is(err, syscall.ENOENT) || errors.Is(err, ErrUnsupported) {
		t.Errorf("missing: %v, want ENOENT", err)
	}
	f := filepath.Join(dir, "file")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ffd, err := unix.Open(f, unix.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(ffd)
	if _, err := p.UnsharedSize(ffd, "x"); !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("file as dirfd: %v, want ENOTDIR", err)
	}
	// What APFS says of a file of two links: logged, as the walker gives
	// a linked file's unshared size only once it met every link.
	if err := os.WriteFile(filepath.Join(dir, "big"), make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(dir, "big"), filepath.Join(dir, "big-link")); err != nil {
		t.Fatal(err)
	}
	n, err := p.UnsharedSize(fd, "big")
	t.Logf("private size of a 1 MiB file of two links: %d, %v", n, err)
}
