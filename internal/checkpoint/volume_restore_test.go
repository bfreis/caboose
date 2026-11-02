package checkpoint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// restoreRig is a store with a checkpoint of volume cache whose disk held
// "then", the disk now holding "now", and a Stopped that records itself.
type restoreRig struct {
	s       *Store
	dir     string // the volumes' disks
	id      ID
	stopped int
	refuse  error
}

func newRestoreRig(t *testing.T) *restoreRig {
	t.Helper()
	useCopyClone(t)
	r := &restoreRig{s: takeStore(t)}
	src := volumeSrc(t, "then", &fakeHold{frozen: true})
	r.dir = src.Top
	tr, err := takeVolume(t, r.s, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.id = tr.ID
	if err := os.WriteFile(r.disk(), []byte("now"), 0o600); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *restoreRig) disk() string { return filepath.Join(r.dir, "cache.img") }

func (r *restoreRig) restore(name string) (VolumeRestored, error) {
	return RestoreVolume(context.Background(), r.s, r.id.String(), VolumeRestoreOptions{
		Dir: r.dir, Name: name, LockWait: time.Second,
		Stopped: func(during func() error) error {
			r.stopped++
			if r.refuse != nil {
				return r.refuse
			}
			return during()
		},
	})
}

func readOr(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

// dirNames lists dir.
func dirNames(t *testing.T, dir string) string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range des {
		out = append(out, d.Name())
	}
	return strings.Join(out, ",")
}

// The checkpoint goes back as the disk, the disk as it was beside it, all
// with the VM held stopped.
func TestRestoreVolume(t *testing.T) {
	r := newRestoreRig(t)
	res, err := r.restore("cache")
	if err != nil {
		t.Fatal(err)
	}
	if r.stopped != 1 || readOr(t, r.disk()) != "then" || readOr(t, r.disk()+BeforeRestoreSuffix) != "now" {
		t.Errorf("stopped %d, disk %q, before %q", r.stopped, readOr(t, r.disk()), readOr(t, r.disk()+BeforeRestoreSuffix))
	}
	if res.Before != r.disk()+BeforeRestoreSuffix || res.Disk != r.disk() || res.Bytes != 4 || res.Path != "~/cache" {
		t.Errorf("%+v", res)
	}
	if got := dirNames(t, r.dir); got != "cache.img,cache.img.before-restore" {
		t.Errorf("left %s", got)
	}
	// Again, the earlier restore's disk not deleted: refused, nothing
	// changed.
	if err := os.WriteFile(r.disk(), []byte("later"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.restore("cache"); err == nil || !strings.Contains(err.Error(), "set aside by an earlier restore") {
		t.Errorf("again: %v", err)
	}
	if readOr(t, r.disk()) != "later" || readOr(t, r.disk()+BeforeRestoreSuffix) != "now" {
		t.Error("changed by a refused restore")
	}
}

// No disk at all (deleted): the checkpoint becomes it, nothing set aside.
func TestRestoreVolumeNoDisk(t *testing.T) {
	r := newRestoreRig(t)
	if err := os.Remove(r.disk()); err != nil {
		t.Fatal(err)
	}
	res, err := r.restore("cache")
	if err != nil || res.Before != "" || readOr(t, r.disk()) != "then" || dirNames(t, r.dir) != "cache.img" {
		t.Errorf("%+v %v, %s", res, err, dirNames(t, r.dir))
	}
}

// Refused while the VM runs (Stopped refuses), for another volume's name,
// and for a checkpoint of a path: the disk as it was.
func TestRestoreVolumeRefused(t *testing.T) {
	r := newRestoreRig(t)
	r.refuse = errors.New("the VM is running")
	if _, err := r.restore("cache"); err == nil || !strings.Contains(err.Error(), "running") {
		t.Errorf("running: %v", err)
	}
	r.refuse = nil
	if _, err := r.restore("other"); err == nil || !strings.Contains(err.Error(), "not other's") {
		t.Errorf("another volume: %v", err)
	}
	if r.stopped != 1 || readOr(t, r.disk()) != "now" || dirNames(t, r.dir) != "cache.img" {
		t.Errorf("stopped %d, disk %q, %s", r.stopped, readOr(t, r.disk()), dirNames(t, r.dir))
	}
	// A meta.json that names a volume but says another kind.
	mp := filepath.Join(r.s.checkpointDir(r.id), metaName)
	b, err := os.ReadFile(mp)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mp, []byte(regexp.MustCompile(`"kind":\s*"volume"`).ReplaceAllString(string(b), `"kind": "file"`)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.restore("cache"); err == nil || !strings.Contains(err.Error(), "not of a volume's disk") {
		t.Errorf("kind file: %v", err)
	}
	// A checkpoint of a directory.
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := r.s.Lock(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := TakeLocked(context.Background(), l, r.s, Source{Path: "/work/x", Kind: SourceRoot, Top: tree}, ReasonManual, TakeOptions{Statfs: plenty, Copy: walkerOnly})
	l.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	r.id = tr.ID
	if _, err := r.restore("cache"); err == nil || !strings.Contains(err.Error(), "not of a volume's disk") {
		t.Errorf("a path's: %v", err)
	}
	if _, err := RestoreVolume(context.Background(), r.s, r.id.String(), VolumeRestoreOptions{Dir: r.dir, Name: "cache"}); err == nil {
		t.Error("no Stopped")
	}
}

// A clone that cannot be made, or a rename into place that fails: the
// disk as it was, back in its place, nothing left beside it.
func TestRestoreVolumeFails(t *testing.T) {
	r := newRestoreRig(t)
	cloneFile = func(_, dir *os.File, name string) error {
		_ = copyClone(nil, dir, name) // a half-made clone, then the failure
		return unix.EXDEV
	}
	if _, err := r.restore("cache"); !errors.Is(err, ErrNoClone) {
		t.Errorf("no clone: %v", err)
	}
	if readOr(t, r.disk()) != "now" || dirNames(t, r.dir) != "cache.img" {
		t.Errorf("after a failed clone: %q, %s", readOr(t, r.disk()), dirNames(t, r.dir))
	}
	cloneFile = copyClone
	old := renameOver
	t.Cleanup(func() { renameOver = old })
	renameOver = func(*os.File, string, *os.File, string) error { return unix.EIO }
	if _, err := r.restore("cache"); err == nil || !strings.Contains(err.Error(), "putting the checkpoint in place") {
		t.Errorf("rename: %v", err)
	}
	if readOr(t, r.disk()) != "now" || dirNames(t, r.dir) != "cache.img" {
		t.Errorf("after a failed rename: %q, %s", readOr(t, r.disk()), dirNames(t, r.dir))
	}
	renameOver = old
	oldLink := linkDisk
	t.Cleanup(func() { linkDisk = oldLink })
	linkDisk = func(*os.File, string, *os.File, string) error { return unix.EIO }
	if _, err := r.restore("cache"); err == nil || !strings.Contains(err.Error(), "setting") {
		t.Errorf("link: %v", err)
	}
	if readOr(t, r.disk()) != "now" || dirNames(t, r.dir) != "cache.img" {
		t.Errorf("after a failed link: %q, %s", readOr(t, r.disk()), dirNames(t, r.dir))
	}
}

// The disk is never missing: when the checkpoint's clone goes over it,
// the disk is still NAME.img, and NAME.img.before-restore is the same
// file.
func TestRestoreVolumeNeverMissing(t *testing.T) {
	r := newRestoreRig(t)
	old := renameOver
	t.Cleanup(func() { renameOver = old })
	checked := false
	renameOver = func(fd *os.File, from string, td *os.File, to string) error {
		var d, b unix.Stat_t
		if err := unix.Stat(r.disk(), &d); err != nil {
			t.Errorf("the disk, as the clone goes over it: %v", err)
		}
		if err := unix.Stat(r.disk()+BeforeRestoreSuffix, &b); err != nil || b.Ino != d.Ino {
			t.Errorf("before-restore is not the disk: %v, %d against %d", err, b.Ino, d.Ino)
		}
		checked = true
		return old(fd, from, td, to)
	}
	if _, err := r.restore("cache"); err != nil || !checked {
		t.Fatalf("%v, checked %v", err, checked)
	}
	if readOr(t, r.disk()) != "then" || readOr(t, r.disk()+BeforeRestoreSuffix) != "now" {
		t.Errorf("disk %q, before %q", readOr(t, r.disk()), readOr(t, r.disk()+BeforeRestoreSuffix))
	}
}

// What a restore interrupted between its two steps leaves, the disk under
// both names and the checkpoint's clone beside it, does not stop the next
// restore: the second name and the clone go, and it is done.
func TestRestoreVolumeAfterInterrupted(t *testing.T) {
	r := newRestoreRig(t)
	if err := os.Link(r.disk(), r.disk()+BeforeRestoreSuffix); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(r.dir, RestoringName("cache", time.Unix(1, 2)))
	if err := os.WriteFile(stale, []byte("then"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(r.dir, RestoringName("cache2", time.Unix(1, 2)))
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.restore("cache"); err != nil {
		t.Fatal(err)
	}
	if readOr(t, r.disk()) != "then" || readOr(t, r.disk()+BeforeRestoreSuffix) != "now" {
		t.Errorf("disk %q, before %q", readOr(t, r.disk()), readOr(t, r.disk()+BeforeRestoreSuffix))
	}
	if got := dirNames(t, r.dir); got != ".cache2.img.restoring-1000000002,cache.img,cache.img.before-restore" {
		t.Errorf("left %s", got)
	}
}

func TestIsRestoringOf(t *testing.T) {
	for _, c := range []struct {
		file, name string
		want       bool
	}{
		{RestoringName("cache", time.Unix(0, 5)), "cache", true},
		{".cache.img.restoring-123", "cache", true},
		{".cache.img.restoring-", "cache", false},
		{".cache.img.restoring-12x", "cache", false},
		{".cache.img.restoring-1", "cach", false},
		{"cache.img.restoring-1", "cache", false},
		{".cache2.img.restoring-1", "cache", false},
	} {
		if got := IsRestoringOf(c.file, c.name); got != c.want {
			t.Errorf("IsRestoringOf(%q, %q) = %v", c.file, c.name, got)
		}
	}
}
