package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/bfreis/caboose/internal/proposal"
	"golang.org/x/sys/unix"
)

// What restore did to an entry of a restored tree to make it safe to open
// on the host.
const (
	// ActionMode: setuid, setgid or sticky cleared.
	ActionMode = "mode"
)

// Sanitized is one change made to a restored tree.
type Sanitized struct {
	Path   string // slash-separated, relative to the restored tree ("." for its top)
	Action string // ActionMode
	Reason string
}

type devIno struct{ dev, ino uint64 }

func idOf(st *unix.Stat_t) devIno { return devIno{uint64(st.Dev), st.Ino} }

// modeFix is a directory given owner rwx so it could be walked and
// changed, and the permission bits it gets back at the end.
type modeFix struct {
	rel  string
	id   devIno
	mode uint32
}

// sanitizer makes a restored tree safe to open on the host. The tree was
// written by a sandbox, so no setuid, setgid or sticky bit is left in it.
// Nothing else of it is changed: its contents, a repository's git
// configuration and hooks among them, are the sandbox's, and the host's
// own tools act on them as they would on any file.
//
// Everything is done on file descriptors held from the restore's own
// temporary directory down, never by a path, which a sandbox able to write
// beside the target could redirect; nothing is ever followed. The walk has
// no depth bound, only max entries.
type sanitizer struct {
	ctx    context.Context
	parent *os.File // the directory holding the tree
	name   string   // the tree's name in it
	top    *os.File // the tree, a directory
	max    int64
	seen   int64

	fixes []modeFix
	out   []Sanitized
}

// testHookSanitizeEntry, when set (by tests only), runs before each entry
// the walk looks at.
var testHookSanitizeEntry func(rel string)

// sanitizeTree makes the restored tree at name in the open directory
// parent safe to open on the host (see sanitizer), and returns what it
// changed and the identity of the tree it made safe. file says the tree is
// a single file.
func sanitizeTree(ctx context.Context, parent *os.File, name string, file bool, maxEntries int) ([]Sanitized, devIno, error) {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	z := &sanitizer{ctx: ctx, parent: parent, name: name, max: int64(maxEntries)}
	var st unix.Stat_t
	if err := fstatat(parent, name, &st); err != nil {
		return nil, devIno{}, fmt.Errorf("checkpoint: %w", err)
	}
	typ := uint32(st.Mode) & unix.S_IFMT
	if file {
		if typ != unix.S_IFREG {
			return nil, devIno{}, errors.New("checkpoint: the restored file is not a regular file")
		}
		if err := z.clearBits(parent, name, ".", &st); err != nil {
			return nil, devIno{}, err
		}
		return z.out, idOf(&st), nil
	}
	if typ != unix.S_IFDIR {
		return nil, devIno{}, errors.New("checkpoint: the restored tree is not a directory")
	}
	top, err := z.enter(parent, name, ".", &st)
	if err != nil {
		return nil, devIno{}, err
	}
	defer top.Close()
	z.top = top
	if err := z.walk(top, "."); err != nil {
		return nil, devIno{}, err
	}
	if err := z.putModesBack(); err != nil {
		return nil, devIno{}, err
	}
	return z.out, idOf(&st), nil
}

func (z *sanitizer) note(rel, action, reason string) {
	z.out = append(z.out, Sanitized{Path: proposal.Printable(rel), Action: action, Reason: reason})
}

// changed is the error for an entry that is no longer what was looked at:
// nothing but restore writes in its own temporary directory, so this is
// not skipped but stops the restore.
func changed(rel string) error {
	return fmt.Errorf("checkpoint: %s changed while the restored copy was made safe", proposal.Printable(rel))
}

func relErr(rel string, err error) error {
	return fmt.Errorf("checkpoint: %s: %w", proposal.Printable(rel), err)
}

// setMode sets the mode of name in d, which st (just looked at, without
// following) says is a directory or a regular file, never following:
// through a descriptor opened O_NOFOLLOW|O_NONBLOCK and checked to be that
// entry, or, when its owner cannot open it for reading, chmodUnopenable.
// Never by path: chmod(2) follows a symlink, and a macOS symlink has a
// mode of its own.
func setMode(d *os.File, name, rel string, st *unix.Stat_t, mode uint32) error {
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if uint32(st.Mode)&unix.S_IFMT == unix.S_IFDIR {
		flags |= unix.O_DIRECTORY
	}
	f, err := openat(d, name, flags, 0)
	if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) {
		ok, err := chmodUnopenable(d, name, st, mode)
		if err != nil {
			return relErr(rel, err)
		}
		if !ok {
			return changed(rel)
		}
		return nil
	}
	if err != nil {
		return relErr(rel, err)
	}
	defer f.Close()
	var now unix.Stat_t
	if err := fstat(f, &now); err != nil {
		return relErr(rel, err)
	}
	if !sameFile(st, &now) {
		return changed(rel)
	}
	if err := fchmod(f, mode); err != nil {
		return relErr(rel, err)
	}
	return nil
}

// clearBits clears setuid, setgid and sticky from the regular file name
// in d.
func (z *sanitizer) clearBits(d *os.File, name, rel string, st *unix.Stat_t) error {
	mode := uint32(st.Mode) & 0o7777
	if mode&0o7000 == 0 {
		return nil
	}
	if err := setMode(d, name, rel, st, mode&0o777); err != nil {
		return err
	}
	z.note(rel, ActionMode, fmt.Sprintf("mode %04o made %04o", mode, mode&0o777))
	return nil
}

// openDirAt opens the directory name in d, never following, checked to
// be the directory st (when given) describes.
func openDirAt(d *os.File, name string, st *unix.Stat_t) (*os.File, error) {
	sub, err := openat(d, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if st != nil {
		var now unix.Stat_t
		if err := fstat(sub, &now); err != nil || !sameFile(st, &now) {
			sub.Close()
			return nil, unix.ESTALE
		}
	}
	return sub, nil
}

// enter opens the directory name in d (st, just looked at) for the walk:
// setuid, setgid and sticky cleared, and owner rwx given for the walk
// (recorded, and put back at the end).
func (z *sanitizer) enter(d *os.File, name, rel string, st *unix.Stat_t) (*os.File, error) {
	mode := uint32(st.Mode) & 0o7777
	want := mode & 0o777
	if want&0o700 != 0o700 {
		z.fixes = append(z.fixes, modeFix{rel: rel, id: idOf(st), mode: want})
		want |= 0o700
	}
	if want != mode {
		if err := setMode(d, name, rel, st, want); err != nil {
			return nil, err
		}
		if mode&0o7000 != 0 {
			z.note(rel, ActionMode, fmt.Sprintf("mode %04o made %04o", mode, mode&0o777))
		}
	}
	sub, err := openDirAt(d, name, st)
	if errors.Is(err, unix.ESTALE) {
		return nil, changed(rel)
	}
	if err != nil {
		return nil, relErr(rel, err)
	}
	return sub, nil
}

// walk goes through the directory d at rel: it clears the mode bits and
// collects the directories whose modes go back at the end.
func (z *sanitizer) walk(d *os.File, rel string) error {
	for {
		names, rerr := d.Readdirnames(1024)
		for _, name := range names {
			if name == "." || name == ".." {
				continue
			}
			if err := z.entry(d, rel, name); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return fmt.Errorf("checkpoint: reading %s: %w", proposal.Printable(rel), rerr)
		}
	}
}

func (z *sanitizer) entry(d *os.File, rel, name string) error {
	if err := z.ctx.Err(); err != nil {
		return err
	}
	z.seen++
	if z.seen > z.max {
		return fmt.Errorf("%w (over %d entries)", ErrTooLarge, z.max)
	}
	r := path.Join(rel, name)
	if testHookSanitizeEntry != nil {
		testHookSanitizeEntry(r)
	}
	var st unix.Stat_t
	if err := fstatat(d, name, &st); err != nil {
		return relErr(r, err)
	}
	switch uint32(st.Mode) & unix.S_IFMT {
	case unix.S_IFDIR:
		sub, err := z.enter(d, name, r, &st)
		if err != nil {
			return err
		}
		defer sub.Close()
		return z.walk(sub, r)
	case unix.S_IFREG:
		return z.clearBits(d, name, r, &st)
	}
	return nil
}

// openDirRel opens the directory at rel (slash-separated, "." for the
// tree) below the tree, one component at a time, never following.
func (z *sanitizer) openDirRel(rel string) (*os.File, error) {
	cur, err := openDirAt(z.top, ".", nil)
	if err != nil {
		return nil, err
	}
	if rel == "." || rel == "" {
		return cur, nil
	}
	for _, c := range strings.Split(rel, "/") {
		next, err := openDirAt(cur, c, nil)
		cur.Close()
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return cur, nil
}

// putModesBack gives the directories the walk opened up their own
// permission bits back, deepest first. One no longer there
// is left as it is.
func (z *sanitizer) putModesBack() error {
	sort.SliceStable(z.fixes, func(i, j int) bool {
		return strings.Count(z.fixes[i].rel, "/") > strings.Count(z.fixes[j].rel, "/")
	})
	for _, f := range z.fixes {
		if err := z.putModeBack(f); err != nil {
			return err
		}
	}
	return nil
}

func (z *sanitizer) putModeBack(f modeFix) error {
	d, name := z.parent, z.name
	if f.rel != "." {
		dd, err := z.openDirRel(path.Dir(f.rel))
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
			return changed(f.rel)
		}
		if err != nil {
			return relErr(f.rel, err)
		}
		defer dd.Close()
		d, name = dd, path.Base(f.rel)
	}
	var st unix.Stat_t
	if err := fstatat(d, name, &st); err != nil || idOf(&st) != f.id || uint32(st.Mode)&unix.S_IFMT != unix.S_IFDIR {
		return changed(f.rel)
	}
	return setMode(d, name, f.rel, &st, f.mode)
}
