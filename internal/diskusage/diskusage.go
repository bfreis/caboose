// Package diskusage measures the space files take up on this machine, three
// ways: their apparent size (what ls shows), their allocated blocks (what
// du shows: a sparse file's holes take none), and, where the filesystem
// reports it, their unshared size: the blocks no other file shares, which
// deleting the file gives back. On APFS a clone (cp -c, clonefile) shares
// every block with its source until either is written, so du counts each
// clone in full while deleting one frees only what it does not share.
//
// A walk never follows a symlink and never opens a file: every directory
// is opened relative to its parent's descriptor with O_NOFOLLOW, and every
// entry is looked at with fstatat(AT_SYMLINK_NOFOLLOW), so a directory
// someone else can write (a sandbox's kept home) can steer the walk nowhere
// outside itself. Only the top path is followed as given.
package diskusage

import (
	"errors"
	"time"
)

// Usage is what a walk found under one or more paths. A file with more
// than one hard link counts once, wherever the walker met it first, and
// its unshared size only once the walker has met every one of its links:
// deleting some of a file's links frees nothing.
type Usage struct {
	// Files is the regular files counted.
	Files int64 `json:"files"`
	// Apparent is the regular files' sizes.
	Apparent int64 `json:"apparent"`
	// Allocated is the blocks of everything counted (files, directories,
	// symlinks), as st_blocks*512.
	Allocated int64 `json:"allocated"`
	// Unshared is the sum of the unshared sizes measured, over
	// UnsharedFiles of the Files.
	Unshared      int64 `json:"unshared"`
	UnsharedFiles int64 `json:"unshared_files"`
	// Unreadable counts the entries that could not be looked at (a
	// directory that cannot be opened or listed, an entry that cannot be
	// stat'ed); what they hold is not counted.
	Unreadable int64 `json:"unreadable,omitempty"`
}

// Add adds o to u.
func (u *Usage) Add(o Usage) {
	u.Files += o.Files
	u.Apparent += o.Apparent
	u.Allocated += o.Allocated
	u.Unshared += o.Unshared
	u.UnsharedFiles += o.UnsharedFiles
	u.Unreadable += o.Unreadable
}

// UnsharedComplete reports whether the unshared size was measured for every
// file counted.
func (u Usage) UnsharedComplete() bool { return u.UnsharedFiles == u.Files }

// ErrUnsupported is a UnsharedSizer's answer for a file whose filesystem
// does not report an unshared size.
var ErrUnsupported = errors.New("the filesystem does not report an unshared size")

// UnsharedSizer measures a regular file's unshared size: name, in the
// directory open as dirfd, never followed if a symlink.
type UnsharedSizer interface {
	UnsharedSize(dirfd int, name string) (int64, error)
}

// maxDepth bounds how deep a walk goes: each level holds a descriptor.
const maxDepth = 128

// readBatch is how many names a directory is read at a time, so that a
// directory of millions of entries never sits in memory whole.
var readBatch = 1024

// maxTracked bounds the files with more than one link a walker keeps
// track of at a time: each takes memory until every one of its links has
// been met.
var maxTracked = 1 << 20

// Walker walks paths, counting each file once across every walk it makes.
type Walker struct {
	// Unshared measures unshared sizes; nil measures none.
	Unshared UnsharedSizer
	// Budget bounds the time spent measuring unshared sizes, counted from
	// the first walk: past it, files are still counted, but their
	// unshared size is not measured. 0 is no bound.
	Budget time.Duration
	// Now is the clock (time.Now when nil), for the budget and Elapsed.
	Now func() time.Time

	started    time.Time
	stopped    time.Time
	overdue    bool
	unsupp     int64
	unmeasured int64
	untracked  int64
	// linked is the files with more than one link met so far, while some
	// of their links have not been.
	linked   map[fileID]*linkedFile
	inflight bool
}

type fileID struct{ dev, ino uint64 }

// linkedFile is a file with more than one link, met at least once.
type linkedFile struct {
	met, nlink uint64
	// unshared is its unshared size, when measured, added to u once every
	// link has been met.
	unshared int64
	measured bool
	u        *Usage
}

func (w *Walker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Walker) start() {
	if !w.inflight {
		w.inflight = true
		w.started = w.now()
		w.linked = map[fileID]*linkedFile{}
	}
}

// OverBudget reports whether the budget ran out, leaving files unmeasured.
func (w *Walker) OverBudget() bool { return w.overdue }

// Elapsed is how long the walks took: from the first one's start to the
// last one's end.
func (w *Walker) Elapsed() time.Duration {
	if !w.inflight {
		return 0
	}
	return w.stopped.Sub(w.started)
}

// Unsupported counts the files whose filesystem reported no unshared size.
func (w *Walker) Unsupported() int64 { return w.unsupp }

// Unmeasured counts the files whose unshared size could not be read, for
// a reason other than the filesystem having none: counted, unmeasured.
func (w *Walker) Unmeasured() int64 { return w.unmeasured }

// LinkedOutside counts the files with a hard link the walks never met:
// counted, but with no unshared size, since deleting what was walked
// would leave them on disk.
func (w *Walker) LinkedOutside() int64 { return int64(len(w.linked)) }

// Untracked counts the files with more than one link met while maxTracked
// others were tracked: each is counted wherever it is met, maybe more
// than once, with no unshared size.
func (w *Walker) Untracked() int64 { return w.untracked }

// measure reports whether a file's unshared size is to be measured now.
func (w *Walker) measure() bool {
	if w.Unshared == nil || w.overdue {
		return false
	}
	if w.Budget > 0 && w.now().Sub(w.started) >= w.Budget {
		w.overdue = true
		return false
	}
	return true
}

// first reports whether the file (dev, ino), with nlink links, is met for
// the first time, and so counted into u. A file of one link is; one of
// more is tracked until all its links have been met, its unshared size
// (unshared, when measured) added to the Usage it was counted in only
// then.
func (w *Walker) first(u *Usage, dev, ino, nlink uint64) (bool, *linkedFile) {
	if nlink <= 1 {
		return true, nil
	}
	id := fileID{dev, ino}
	if f := w.linked[id]; f != nil {
		f.met++
		if f.met >= f.nlink {
			if f.measured {
				f.u.Unshared += f.unshared
				f.u.UnsharedFiles++
			}
			delete(w.linked, id)
		}
		return false, nil
	}
	if len(w.linked) >= maxTracked {
		w.untracked++
		return true, nil
	}
	f := &linkedFile{met: 1, nlink: nlink, u: u}
	w.linked[id] = f
	return true, f
}
