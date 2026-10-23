package launcher

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"

	"github.com/bfreis/caboose/internal/checkpoint"
	"golang.org/x/sys/unix"
)

// ckFS is what setup and doctor ask of this machine to tell the method a
// checkpoint of a path would get: the OS, the device and filesystem of a
// path, and a volume's free space. A struct of functions so that a test
// gives it any machine it likes.
type ckFS struct {
	goos string
	// info is a path's device and filesystem type ("apfs", "btrfs", ...,
	// "" when not told apart).
	info func(path string) (dev uint64, fstype string, err error)
	// statfs is a volume's available and total bytes.
	statfs checkpoint.StatfsFunc
}

// hostCkFS is this machine's. A variable for the tests.
var hostCkFS = ckFS{goos: runtime.GOOS, info: pathFS, statfs: realStatfs}

func realStatfs(p string) (avail, total uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(p, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	return uint64(st.Bavail) * bs, uint64(st.Blocks) * bs, nil
}

// pathFS is the device and filesystem type of p.
func pathFS(p string) (uint64, string, error) {
	var st unix.Stat_t
	if err := unix.Stat(p, &st); err != nil {
		return 0, "", err
	}
	t, err := fsTypeName(p)
	if err != nil {
		return 0, "", err
	}
	return uint64(st.Dev), t, nil
}

// nearestExisting is p, or the nearest ancestor of it that exists.
func nearestExisting(p string) string {
	for {
		if _, err := os.Lstat(p); err == nil || !errors.Is(err, fs.ErrNotExist) {
			return p
		}
		if filepath.Dir(p) == p {
			return p
		}
		p = filepath.Dir(p)
	}
}

// ckMethod is how checkpoints of a path would be copied here.
type ckMethod struct {
	// Name is checkpoint.MethodClonefile, MethodReflink or MethodCopy.
	Name string
	// Slow says the copies are full at first, unchanged files linked after.
	Slow bool
	// Why says why, for the person reading.
	Why string
}

// reflinkTypes are the Linux filesystems whose files the walker clones.
var reflinkTypes = []string{"btrfs", "xfs", "zfs", "bcachefs"}

// method tells how a checkpoint of the host directory src would be copied
// into the store at store (neither need exist: the nearest directory that
// does is looked at). On macOS a whole-tree clone needs one APFS volume;
// elsewhere the walker clones each file on a filesystem that can, else
// copies it, linking the files that did not change from the checkpoint
// before.
func (f ckFS) method(src, store string) ckMethod {
	sd, st, serr := f.info(nearestExisting(src))
	dd, dt, derr := f.info(nearestExisting(store))
	if serr != nil || derr != nil {
		err := serr
		if err == nil {
			err = derr
		}
		return ckMethod{Name: checkpoint.MethodCopy, Slow: true, Why: fmt.Sprintf("the filesystem could not be looked at (%v): assuming full copies, unchanged files linked after", err)}
	}
	same := sd == dd
	if f.goos == "darwin" {
		if same && st == "apfs" && dt == "apfs" {
			return ckMethod{Name: checkpoint.MethodClonefile, Why: "one APFS volume as the store: a whole-tree clone"}
		}
		return ckMethod{Name: checkpoint.MethodCopy, Slow: true, Why: "not on the store's APFS volume: copies at first, unchanged files linked after; slower"}
	}
	if same && slices.Contains(reflinkTypes, st) {
		return ckMethod{Name: checkpoint.MethodReflink, Why: "per-file clones on " + st}
	}
	return ckMethod{Name: checkpoint.MethodCopy, Slow: true, Why: "reflink if the filesystem supports it, else copy: copies at first, unchanged files linked after; slower"}
}
