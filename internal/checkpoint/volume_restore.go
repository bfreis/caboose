package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// BeforeRestoreSuffix names the disk a volume's restore set aside, beside
// the restored one, until the user deletes it.
const BeforeRestoreSuffix = ".before-restore"

// RestoringName is the name, in the volumes' directory, of the clone of a
// checkpoint that a restore of volume name makes before putting it in
// place; RestoringPrefix and RestoringInfix what every such name starts
// and continues with (".NAME.img.restoring-<nanoseconds>"). It only ever
// holds a checkpoint's copy, never the disk as it was, so one an
// interrupted restore left can be deleted: the checkpoint keeps it.
func RestoringName(name string, now time.Time) string {
	return fmt.Sprintf("%s%s.img%s%d", RestoringPrefix, name, RestoringInfix, now.UnixNano())
}

const (
	RestoringPrefix = "."
	RestoringInfix  = ".restoring-"
)

// Test seams: the rename that puts the checkpoint's clone over the disk,
// and the link that keeps the disk under its second name.
var (
	renameOver = renameat
	linkDisk   = linkat
)

// VolumeRestoreOptions are RestoreVolume's.
type VolumeRestoreOptions struct {
	// Dir is the directory of the volumes' disks (the data dir's
	// vm/volumes), the host's own; Name the volume whose disk, Name.img,
	// is replaced.
	Dir, Name string
	// Stopped runs during while the VM is stopped and kept from starting,
	// or refuses (the VM runs). Required.
	Stopped func(during func() error) error
	// LockWait is how long to wait for the store's lock.
	LockWait time.Duration
}

// VolumeRestored is what RestoreVolume did.
type VolumeRestored struct {
	ID    ID
	Path  string // the configured path the checkpoint is of
	Disk  string // the restored disk
	Bytes int64
	// Before is where the disk as it was is kept (Disk+BeforeRestoreSuffix),
	// "" when there was none.
	Before string
}

// RestoreVolume puts the volume checkpoint id back as the volume's disk,
// with the VM stopped: the checkpoint's clone is cloned again beside the
// disk (RestoringName), the disk is given a second name,
// NAME.img.before-restore (a hard link, refusing to replace anything),
// and the new clone renamed over NAME.img, which replaces it in one step.
// NAME.img is never missing: interrupted between the link and the rename,
// a restore leaves the disk with both names (the same file, which the
// next restore finds and unlinks) and a clone of the checkpoint, which it
// deletes too. It refuses while a NAME.img.before-restore of its own is
// there (an earlier restore's, not yet deleted), for a checkpoint of
// another volume, and whatever o.Stopped refuses. On failure the disk is
// as it was.
func RestoreVolume(ctx context.Context, s *Store, id string, o VolumeRestoreOptions) (VolumeRestored, error) {
	var res VolumeRestored
	if o.Stopped == nil || o.Dir == "" || !filepath.IsAbs(o.Dir) || o.Name == "" {
		return res, errors.New("checkpoint: a volume's restore needs its disk's directory, its name, and the VM held stopped")
	}
	pid, err := ParseID(id)
	if err != nil {
		return res, err
	}
	l, err := s.Lock(ctx, o.LockWait)
	if err != nil {
		return res, err
	}
	defer l.Unlock()
	if s.Exposed == nil {
		return res, errors.New("checkpoint: no exposed-store check is set: refusing to restore")
	}
	if err := s.exposed(); err != nil {
		return res, err
	}
	e, err := s.Resolve(pid)
	if err != nil {
		return res, err
	}
	if e.Err != nil || e.Meta == nil {
		return res, fmt.Errorf("checkpoint: %s cannot be read: %v", e.ID, e.Err)
	}
	res.ID, res.Path = e.ID, e.Meta.Path
	if e.Meta.Kind != KindVolume || e.Meta.Volume == nil {
		return res, fmt.Errorf("checkpoint: %s is not of a volume's disk", e.ID)
	}
	if e.Meta.Volume.Name != o.Name {
		return res, fmt.Errorf("checkpoint: %s is of volume %s's disk, not %s's", e.ID, e.Meta.Volume.Name, o.Name)
	}
	disk := o.Name + ".img"
	res.Disk = filepath.Join(o.Dir, disk)
	before := disk + BeforeRestoreSuffix
	tree, err := os.OpenFile(filepath.Join(s.checkpointDir(e.ID), TreeName), os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return res, fmt.Errorf("checkpoint: %s: %w", e.ID, err)
	}
	defer tree.Close()
	var tst unix.Stat_t
	if err := fstat(tree, &tst); err != nil || tst.Mode&unix.S_IFMT != unix.S_IFREG {
		return res, fmt.Errorf("checkpoint: %s holds no disk", e.ID)
	}
	dir, err := openDirPath(o.Dir)
	if err != nil {
		return res, fmt.Errorf("checkpoint: %w", err)
	}
	defer dir.Close()

	err = o.Stopped(func() error {
		var st, bst unix.Stat_t
		had := true
		switch err := fstatat(dir, disk, &st); {
		case errors.Is(err, unix.ENOENT):
			had = false
		case err != nil:
			return err
		case st.Mode&unix.S_IFMT != unix.S_IFREG:
			return fmt.Errorf("%s is not a regular file", res.Disk)
		}
		switch err := fstatat(dir, before, &bst); {
		case err == nil && had && bst.Dev == st.Dev && bst.Ino == st.Ino:
			// An interrupted restore's second name of the disk: nothing of
			// its own.
			if err := unlinkat(dir, before); err != nil {
				return fmt.Errorf("%s is a second name of %s, left by an interrupted restore, and cannot be removed: %w", filepath.Join(o.Dir, before), res.Disk, err)
			}
			s.Logf("removed %s, a second name of %s an interrupted restore left", before, disk)
		case err == nil:
			return fmt.Errorf("%s is there, set aside by an earlier restore: delete it (or move it away) once you no longer need it, then restore again", filepath.Join(o.Dir, before))
		case !errors.Is(err, unix.ENOENT):
			return err
		case !had:
		}
		if err := removeRestoring(dir, o.Name, s.Logf); err != nil {
			return err
		}
		tmp := RestoringName(o.Name, time.Now())
		if err := cloneFile(tree, dir, tmp); err != nil {
			_ = unlinkat(dir, tmp)
			if diskFull(err) {
				return fmt.Errorf("%w (%v)", ErrDiskFull, err)
			}
			return fmt.Errorf("%w (%v)", ErrNoClone, err)
		}
		if had {
			if err := linkDisk(dir, disk, dir, before); err != nil {
				_ = unlinkat(dir, tmp)
				return fmt.Errorf("setting %s aside: %w", res.Disk, err)
			}
			res.Before = filepath.Join(o.Dir, before)
			err = renameOver(dir, tmp, dir, disk)
		} else {
			err = renameNoReplace(dir, tmp, dir, disk)
		}
		if err != nil {
			err = fmt.Errorf("putting the checkpoint in place: %w", err)
			if had {
				// The disk is still NAME.img: drop its second name.
				if uerr := unlinkat(dir, before); uerr != nil {
					err = fmt.Errorf("%w; %s is the disk as it was, under a second name, which could not be removed (%v): delete it", err, res.Before, uerr)
				}
				res.Before = ""
			}
			_ = unlinkat(dir, tmp)
			return err
		}
		if err := dir.Sync(); err != nil {
			s.Logf("syncing %s after the restore: %v", o.Dir, err)
		}
		res.Bytes = tst.Size
		return nil
	})
	if err != nil {
		s.Logf("restore of %s to volume %s not done: %v", e.ID, o.Name, err)
		return res, err
	}
	s.Logf("restored %s to volume %s (%s), the disk as it was at %s", e.ID, o.Name, res.Disk, res.Before)
	return res, nil
}

// removeRestoring deletes the clones of checkpoints earlier restores of
// volume name left in d (RestoringName): copies of checkpoints, never a
// disk as it was.
func removeRestoring(d *os.File, name string, logf func(string, ...any)) error {
	names, err := d.Readdirnames(-1)
	if _, serr := d.Seek(0, 0); err == nil {
		err = serr
	}
	if err != nil {
		return err
	}
	for _, n := range names {
		if !IsRestoringOf(n, name) {
			continue
		}
		if err := unlinkat(d, n); err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("removing %s, an interrupted restore's copy of a checkpoint: %w", n, err)
		}
		logf("removed %s, an interrupted restore's copy of a checkpoint", n)
	}
	return nil
}

// IsRestoringOf says whether file is a name RestoringName gives for
// volume name.
func IsRestoringOf(file, name string) bool {
	rest, ok := strings.CutPrefix(file, RestoringPrefix+name+".img"+RestoringInfix)
	if !ok || rest == "" {
		return false
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// renameat renames from in fromDir to to in toDir, replacing to.
func renameat(fromDir *os.File, from string, toDir *os.File, to string) error {
	err := unix.Renameat(int(fromDir.Fd()), from, int(toDir.Fd()), to)
	runtime.KeepAlive(fromDir)
	runtime.KeepAlive(toDir)
	return err
}

// unlinkat removes the file name in d.
func unlinkat(d *os.File, name string) error {
	err := unix.Unlinkat(int(d.Fd()), name, 0)
	runtime.KeepAlive(d)
	return err
}
