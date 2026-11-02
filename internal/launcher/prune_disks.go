package launcher

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/bfreis/caboose/internal/vm"
)

// pruneScratch deletes the scratch disks left by VMs that are not
// running: a start makes a VM's anew, empty, and a stop deletes it, so
// one is left only by a VM that ended some other way, and nothing reads
// it again. Each is deleted under its VM's start lock, taken without
// waiting: a VM starting or stopping holds it, and is skipped, as is one
// whose vmm runs (vm.Dir.Runs). Like the rest of plain prune it asks
// nothing: what it deletes is made again at every start.
func (a *App) pruneScratch() {
	disks, _ := filepath.Glob(filepath.Join(a.vmRoot(), "*", "scratch.img"))
	for _, p := range disks {
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		d := vm.Dir(filepath.Dir(p))
		unlock, err := vm.TryLock(d.StartLock())
		if errors.Is(err, vm.ErrLocked) {
			a.Note("left %s: its VM is starting or stopping ('caboose prune' again once it is done)", p)
			continue
		}
		if err != nil {
			a.Note("left %s: %v", p, err)
			continue
		}
		if d.Runs() {
			unlock()
			continue
		}
		size := sizeString(diskAllocated(fi))
		err = os.Remove(p)
		unlock()
		if err != nil {
			a.Note("cannot delete %s: %v", p, err)
			continue
		}
		a.Note("deleted %s, the scratch disk of a VM not running, freeing %s", p, size)
	}
}

// pruneBuilderCache deletes the builder guest's cache disk, dockerd's
// storage in the builder, which only grows: every layer and download any
// build fetched. The next build makes an empty one and fetches everything
// again, so it says what it frees and asks first (CABOOSE_FORCE=1 does
// not ask), and refuses with no terminal. A build holds the builder's
// start lock for as long as its guest runs (builder.Builder.Start), so
// one running is refused; a builder guest an earlier build left running
// is stopped first.
func (a *App) pruneBuilderCache() error {
	d := a.vmDir(builderName)
	cache := filepath.Join(string(d), "cache.img")
	fi, err := os.Lstat(cache)
	if errors.Is(err, os.ErrNotExist) {
		a.Note("no builder cache at %s: nothing to delete (the next build makes an empty one)", cache)
		return nil
	}
	if err != nil {
		return Die("cannot look at %s: %v", cache, err)
	}
	if !fi.Mode().IsRegular() {
		return Die("%s is not a regular file: not deleting it", cache)
	}
	held := func() error {
		return Die("a build is running, and it uses the builder's cache: 'caboose prune --builder-cache' again once it ends")
	}
	unlock, err := vm.TryLock(d.StartLock())
	if errors.Is(err, vm.ErrLocked) {
		return held()
	}
	if err != nil {
		return Die("%v", err)
	}
	// Not held while asking: a build would wait on the question.
	unlock()
	size := sizeString(diskAllocated(fi))
	a.Note("this deletes %s (%s on disk): every layer and download the builder", cache, size)
	a.Note("  kept; the next build makes an empty one, and fetches everything again")
	if a.force() {
		a.Note("CABOOSE_FORCE=1 set, continuing.")
	} else {
		yes, asked, err := a.askYes("delete it?")
		switch {
		case err != nil:
			return err
		case !asked:
			return Die("refusing to delete it non-interactively (set CABOOSE_FORCE=1 to delete it)")
		case !yes:
			return Die("aborted; nothing was changed")
		}
	}
	unlock, err = vm.TryLock(d.StartLock())
	if errors.Is(err, vm.ErrLocked) {
		return held()
	}
	if err != nil {
		return Die("%v", err)
	}
	defer unlock()
	if d.Runs() {
		a.Note("stopping the builder guest an earlier build left running")
		if err := d.Stop(); err != nil {
			return Die("cannot stop the builder guest left running: %v; 'caboose prune --builder-cache' again tries once more", err)
		}
	}
	if err := os.Remove(cache); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Die("cannot delete %s: %v", cache, err)
	}
	a.Note("deleted %s, freeing %s", cache, size)
	return nil
}
