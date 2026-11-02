package checkpoint

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// On APFS, with the real clone, link and rename: the checkpoint goes over
// the disk, which is never missing, and the disk as it was is kept under
// its second name, a file of its own once the clone has its first.
func TestRestoreVolumeAPFS(t *testing.T) {
	r := newRestoreRig(t)
	cloneFile = cloneInto
	old := renameOver
	t.Cleanup(func() { renameOver = old })
	var linked unix.Stat_t
	renameOver = func(fd *os.File, from string, td *os.File, to string) error {
		var d unix.Stat_t
		if err := unix.Stat(r.disk(), &d); err != nil {
			t.Errorf("the disk, as the clone goes over it: %v", err)
		}
		if err := unix.Stat(r.disk()+BeforeRestoreSuffix, &linked); err != nil || linked.Ino != d.Ino || linked.Nlink != 2 {
			t.Errorf("before-restore: %v, inode %d (the disk's %d), links %d", err, linked.Ino, d.Ino, linked.Nlink)
		}
		return old(fd, from, td, to)
	}
	res, err := r.restore("cache")
	if err != nil {
		t.Fatal(err)
	}
	var d, b unix.Stat_t
	if err := unix.Stat(r.disk(), &d); err != nil {
		t.Fatal(err)
	}
	if err := unix.Stat(res.Before, &b); err != nil {
		t.Fatal(err)
	}
	if readOr(t, r.disk()) != "then" || readOr(t, res.Before) != "now" || d.Ino == b.Ino || b.Nlink != 1 || d.Nlink != 1 {
		t.Errorf("disk %q (inode %d, %d links), before %q (inode %d, %d links)", readOr(t, r.disk()), d.Ino, d.Nlink, readOr(t, res.Before), b.Ino, b.Nlink)
	}
	if got := dirNames(t, r.dir); got != "cache.img,cache.img.before-restore" {
		t.Errorf("left %s", got)
	}
	t.Logf("restored on APFS: disk inode %d, before-restore inode %d", d.Ino, b.Ino)
}
