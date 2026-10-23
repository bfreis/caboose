//go:build unix

package saferm

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// maxTreeDepth bounds RemoveTree's walk. The walk holds one open directory
// per level, as os.RemoveAll does, so a tree deeper than this is refused
// with an error naming the limit rather than run the process out of file
// descriptors; nothing below the limit is left half-changed by it, and the
// tree is then not removed.
var maxTreeDepth = 4096 // a variable for tests only

// testHookAfterStat, when set (by tests only), runs after an entry was
// reported a directory and before it is opened, so a test can swap it.
var testHookAfterStat func(dir *os.File, name string)

// testHookAfterOpen, when set (by tests only), runs after a directory was
// opened and checked, and before its mode is changed through the descriptor.
var testHookAfterOpen func(dir *os.File, name string)

// testHookRemoveAfterStat, when set (by tests only), runs in RemoveTreeAt's
// removal walk after an entry was reported a directory and before it is
// opened, so a test can swap it.
var testHookRemoveAfterStat func(dir *os.File, name string)

// RemoveTree removes path and everything below it, if path is inside root,
// as RemoveAll does, with the same refusals; a path that does not exist is
// not an error. Unlike RemoveAll it first gives every directory of the tree
// the owner permissions removal needs (and, on macOS, clears the user
// immutable and append-only flags), so a tree with read-only directories
// (0555, 0000) is removed too.
//
// That fixing walk works on file descriptors and never follows a symlink:
// each entry is looked at with fstatat(AT_SYMLINK_NOFOLLOW), only a
// directory or a regular file is opened (O_NOFOLLOW), and only after its
// device, inode and type match what was looked at is it changed, through
// that descriptor. Symlinks, fifos, sockets and devices are never opened or
// changed; an entry swapped for something else mid-walk is left as it is.
func RemoveTree(root, path string) error {
	p, err := resolve(root, path)
	if err != nil || p == "" {
		return err
	}
	if err := fixTree(p); err != nil {
		return err
	}
	return os.RemoveAll(p)
}

// fixTree runs the fixing walk over p, whose parent is already resolved.
func fixTree(p string) error {
	parent, err := openParent(filepath.Dir(p))
	if err != nil {
		return fmt.Errorf("saferm: %w", err)
	}
	defer parent.Close()
	return fixEntry(parent, filepath.Base(p), p, 0)
}

// fixEntry fixes the entry name of the directory dir; shown is its path, for
// errors only.
func fixEntry(dir *os.File, name, shown string, depth int) error {
	var st unix.Stat_t
	err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW)
	runtime.KeepAlive(dir)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("saferm: %s: %w", shown, err)
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		return fixFile(dir, name, shown, &st)
	case unix.S_IFDIR:
		return fixDir(dir, name, shown, &st, depth)
	}
	return nil
}

func fixDir(dir *os.File, name, shown string, st *unix.Stat_t, depth int) error {
	if depth >= maxTreeDepth {
		return fmt.Errorf("saferm: %s: tree deeper than %d levels, not removed", shown, maxTreeDepth)
	}
	if testHookAfterStat != nil {
		testHookAfterStat(dir, name)
	}
	if st.Mode&unix.S_IRUSR == 0 {
		// Not openable for reading: give owner rwx without opening it so.
		ok, err := chmodUnreadable(dir, name, shown, st, uint32(st.Mode&0o7777)|0o700)
		if err != nil || !ok {
			return err
		}
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	runtime.KeepAlive(dir)
	if err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return nil // gone or swapped for something else: left alone
		}
		return fmt.Errorf("saferm: %s: %w", shown, err)
	}
	f := os.NewFile(uintptr(fd), shown)
	defer f.Close()
	var now unix.Stat_t
	err = unix.Fstat(int(f.Fd()), &now)
	runtime.KeepAlive(f)
	if err != nil {
		return fmt.Errorf("saferm: %s: %w", shown, err)
	}
	if !sameObject(st, &now) {
		return nil
	}
	if testHookAfterOpen != nil {
		testHookAfterOpen(dir, name)
	}
	if err := clearFlags(f, shown, &now); err != nil {
		return err
	}
	if now.Mode&0o700 != 0o700 {
		err = unix.Fchmod(int(f.Fd()), uint32(now.Mode&0o7777)|0o700)
		runtime.KeepAlive(f)
		if err != nil {
			return fmt.Errorf("saferm: %s: %w", shown, err)
		}
	}
	for {
		names, err := f.Readdirnames(1024)
		for _, n := range names {
			if err := fixEntry(f, n, shown+"/"+n, depth+1); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("saferm: %s: %w", shown, err)
		}
		if len(names) == 0 {
			return nil
		}
	}
}

// sameObject says whether two stats describe the same object of the same type.
func sameObject(a, b *unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Mode&unix.S_IFMT == b.Mode&unix.S_IFMT
}

// RemoveTreeAt removes the entry name of the open directory dir and, when
// it is a directory, everything below it, as RemoveTree does (owner
// permissions and, on macOS, flags fixed first), but entirely relative to
// dir: the root is the directory dir holds, whatever its path now names, so
// a path above it being swapped for a symlink cannot move the removal. name
// must be a single component, not "." or ".."; anything else is refused
// with ErrOutside. An entry that is not a directory (a symlink among them)
// is unlinked, never followed; a name that does not exist is not an error.
func RemoveTreeAt(dir *os.File, name string) error {
	if dir == nil || name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') || strings.ContainsRune(name, 0) {
		return fmt.Errorf("%w: %q is not a name in the directory", ErrOutside, name)
	}
	shown := filepath.Join(dir.Name(), name)
	if err := fixEntry(dir, name, shown, 0); err != nil {
		return err
	}
	return removeEntryAt(dir, name, shown, 0)
}

// removeEntryAt unlinks name in dir, emptying it first when it is a
// directory; every call is relative to a descriptor this walk holds.
func removeEntryAt(dir *os.File, name, shown string, depth int) error {
	var st unix.Stat_t
	err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW)
	runtime.KeepAlive(dir)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("saferm: %s: %w", shown, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		err = unix.Unlinkat(int(dir.Fd()), name, 0)
		runtime.KeepAlive(dir)
		if err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("saferm: %s: %w", shown, err)
		}
		return nil
	}
	if depth >= maxTreeDepth {
		return fmt.Errorf("saferm: %s: tree deeper than %d levels, not removed", shown, maxTreeDepth)
	}
	if testHookRemoveAfterStat != nil {
		testHookRemoveAfterStat(dir, name)
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	runtime.KeepAlive(dir)
	if err != nil {
		return fmt.Errorf("saferm: %s: %w", shown, err)
	}
	f := os.NewFile(uintptr(fd), shown)
	defer f.Close()
	var now unix.Stat_t
	err = unix.Fstat(int(f.Fd()), &now)
	runtime.KeepAlive(f)
	if err != nil {
		return fmt.Errorf("saferm: %s: %w", shown, err)
	}
	if !sameObject(&st, &now) {
		return fmt.Errorf("saferm: %s: changed while it was removed", shown)
	}
	// Entries removed while a directory is read may make the listing skip
	// others (APFS), so it is read again until the directory goes.
	for pass := 0; ; pass++ {
		if pass > 0 {
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return fmt.Errorf("saferm: %s: %w", shown, err)
			}
		}
		for {
			names, rerr := f.Readdirnames(1024)
			for _, n := range names {
				if err := removeEntryAt(f, n, shown+"/"+n, depth+1); err != nil {
					return err
				}
			}
			if rerr == io.EOF || (rerr == nil && len(names) == 0) {
				break
			}
			if rerr != nil {
				return fmt.Errorf("saferm: %s: %w", shown, rerr)
			}
		}
		err = unix.Unlinkat(int(dir.Fd()), name, unix.AT_REMOVEDIR)
		runtime.KeepAlive(dir)
		if err == nil || errors.Is(err, unix.ENOENT) {
			return nil
		}
		if (!errors.Is(err, unix.ENOTEMPTY) && !errors.Is(err, unix.EEXIST)) || pass >= 3 {
			return fmt.Errorf("saferm: %s: %w", shown, err)
		}
	}
}
