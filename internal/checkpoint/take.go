package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/saferm"
	"github.com/bfreis/caboose/internal/sandboxcfg"
	"github.com/bfreis/caboose/internal/version"
	"golang.org/x/sys/unix"
)

// The free-space floors on the store's volume.
const (
	softFloorMin     = 10 << 30 // below max(10 GiB, 5%): skipped with SkipBelowSoft, else taken and warned about
	softFloorPercent = 5
	hardFloorMin     = 1 << 30 // below max(1 GiB, 1%): not taken
	hardFloorPercent = 1
)

// Floors are the soft and hard free-space floors of a volume of total bytes.
func Floors(total uint64) (soft, hard uint64) {
	return max(softFloorMin, total/100*softFloorPercent), max(hardFloorMin, total/100*hardFloorPercent)
}

// The shrink thresholds: a checkpoint holding half or less of the
// previous one's entries (bytes), when that one held more than this many.
const (
	shrinkEntries = 1000
	shrinkBytes   = 100 << 20
)

// Notices Take gives TakeOptions.Notify.
const (
	NoticeLowSpace = "low-space" // below the soft floor; the checkpoint was still taken
	NoticeShrunk   = "shrunk"    // the checkpoint holds far less than the one before, now pinned
)

// ErrLowSpace is a checkpoint not taken because the store's volume is
// below the soft floor, with TakeOptions.SkipBelowSoft. Nothing is pruned
// for it.
var ErrLowSpace = errors.New("checkpoint: below the soft free-space floor")

// Notice is something the user should hear about a take. Rate limiting
// is the caller's.
type Notice struct {
	Kind string
	Path string // the configured path
	ID   ID     // the checkpoint taken
	Prev ID     // NoticeShrunk: the earlier checkpoint to restore from
	// Pinned is set with NoticeShrunk when Prev was pinned (Store.Pins).
	Pinned bool
	Text   string
}

// StatfsFunc reports a volume's available and total bytes.
type StatfsFunc func(path string) (avail, total uint64, err error)

// statfs is the real StatfsFunc.
func statfs(p string) (avail, total uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(p, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	return uint64(st.Bavail) * bs, uint64(st.Blocks) * bs, nil
}

// TakeOptions are Take's.
type TakeOptions struct {
	NoPrune bool // leave the key unpruned (a before-restore checkpoint)
	Pinned  []ID // never pruned
	// SkipBelowSoft refuses (ErrLowSpace) below the soft floor, as the
	// schedule does; without it the checkpoint is taken there, and
	// LowSpace says so. Below the hard floor none is taken either way.
	SkipBelowSoft bool

	Now    func() time.Time // the clock; time.Now when nil
	Statfs StatfsFunc       // free space on the store's volume; unix.Statfs when nil
	Notify func(Notice)     // told of a low disk and of a sharp shrink; may be nil
	// Denied are the home-relative login files dropped from a home source's
	// copy; sandboxcfg.Denied when nil.
	Denied []string
	// Copy carries Copy's test switches (NoWhole, NoReflink, MaxEntries,
	// Hooks); Take sets the rest.
	Copy Options
}

// TakeResult is what Take did.
type TakeResult struct {
	ID       ID
	Meta     Meta
	LowSpace bool  // below the soft floor
	Pruned   []ID  // dropped by the prune after it
	PruneErr error // a prune that failed; the checkpoint itself was taken
	// Pinned is the previous checkpoint, pinned because this one shrank
	// sharply from it (nil when it did not, or the pin failed: PinErr).
	Pinned   *ID
	PinErr   error
	Unpinned []string // pins this checkpoint released by holding as much again
}

// Take takes a checkpoint of src for reason: it takes the store's lock
// (waiting as Store.Lock does) and calls TakeLocked.
func Take(ctx context.Context, s *Store, src Source, reason string, o TakeOptions) (TakeResult, error) {
	l, err := s.Lock(ctx, 0)
	if err != nil {
		return TakeResult{}, err
	}
	defer l.Unlock()
	return TakeLocked(ctx, l, s, src, reason, o)
}

// TakeLocked takes a checkpoint of src for reason with the store's lock,
// l, already held by the caller (restore holds it for its whole run). It
// refuses while the store is exposed, and when no exposed-store check is
// set at all; refuses below the hard free-space floor; copies src into a
// .tmp- directory of the store, drops the login files from a home source's
// copy, writes meta.json and publishes the checkpoint; compares it with the
// previous one (a sharp shrink is recorded and notified, and the previous
// one pinned in the store, Pins; a pin of the key that this one holds as
// much as again is released); and prunes the key unless o.NoPrune. Whatever fails after the .tmp- directory was made
// removes it.
func TakeLocked(ctx context.Context, l *Lock, s *Store, src Source, reason string, o TakeOptions) (TakeResult, error) {
	if err := l.check(s); err != nil {
		return TakeResult{}, err
	}
	if s.Exposed == nil {
		return TakeResult{}, errors.New("checkpoint: no exposed-store check is set: refusing to take a checkpoint")
	}
	if err := s.exposed(); err != nil {
		return TakeResult{}, err
	}
	if src.Path == "" || (src.Kind != SourceRoot && src.Kind != SourceHome) {
		return TakeResult{}, fmt.Errorf("checkpoint: bad source %q (%q)", src.Path, src.Kind)
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	statfsFn := o.Statfs
	if statfsFn == nil {
		statfsFn = statfs
	}
	denied := o.Denied
	if denied == nil {
		denied = sandboxcfg.Denied
	}
	var res TakeResult
	started := now()
	key := Key(src.Path)

	// Free space, before anything is written.
	low, err := checkSpace(statfsFn, s.dir)
	if err != nil {
		s.Logf("checkpoint of %s not taken: %v", src.Path, err)
		return res, err
	}
	res.LowSpace = low
	if low && o.SkipBelowSoft {
		avail, total, _ := statfsFn(s.dir)
		soft, _ := Floors(total)
		err := fmt.Errorf("%w: %d MiB free, under the %d MiB the floor asks", ErrLowSpace, avail>>20, soft>>20)
		s.Logf("checkpoint of %s not taken: %v", src.Path, err)
		return res, err
	}

	prev := latestReadable(s, key)

	stamp, err := s.NextStamp(l, key, started)
	if err != nil {
		return res, err
	}
	tmp, err := s.NewTemp(l, key, stamp)
	if err != nil {
		return res, err
	}
	fail := func(err error) (TakeResult, error) {
		if rerr := saferm.RemoveTree(s.dir, tmp); rerr != nil {
			s.Logf("removing %s: %v", filepath.Base(tmp), rerr)
			err = errors.Join(err, rerr)
		}
		s.Logf("checkpoint of %s failed: %v", src.Path, err)
		return TakeResult{LowSpace: low}, err
	}

	co := o.Copy
	co.Root = s.dir
	co.Now = now
	co.Prev, co.PrevStarted = "", time.Time{}
	co.Space = func() error {
		_, err := checkSpace(statfsFn, s.dir)
		return err
	}
	if prev != nil && prev.Meta.Kind == kindOf(src) {
		co.Prev = filepath.Join(s.checkpointDir(prev.ID), TreeName)
		co.PrevStarted = prev.Meta.Started
	}
	cr, err := Copy(ctx, src, filepath.Join(tmp, TreeName), co)
	if err != nil {
		return fail(err)
	}
	dropped, err := dropLogin(tmp, src, denied)
	if err != nil {
		return fail(err)
	}

	m := Meta{
		Kind:            kindOf(src),
		Source:          src.Host(),
		Method:          cr.Method,
		Reason:          reason,
		Started:         started.UTC(),
		Finished:        now().UTC(),
		Entries:         cr.Entries,
		Bytes:           cr.Bytes,
		Linked:          cr.Linked,
		Skipped:         cr.Skipped,
		SkippedExamples: cr.SkippedExamples,
		Dropped:         dropped,
		Caboose:         version.Get().Version,
	}
	if prev != nil && shrank(prev.Meta, m.Entries, m.Bytes) {
		m.Shrunk = &Shrunk{ID: prev.ID.String(), Entries: prev.Meta.Entries, Bytes: prev.Meta.Bytes}
	}
	id, err := s.Publish(l, tmp, src.Path, m)
	if err != nil {
		return fail(err)
	}
	m.Version, m.ID, m.Path = MetaVersion, id.String(), src.Path
	res.ID, res.Meta = id, m

	if low && o.Notify != nil {
		o.Notify(Notice{Kind: NoticeLowSpace, Path: src.Path, ID: id,
			Text: fmt.Sprintf("the disk holding the checkpoints is low on space; the checkpoint of %s was still taken", src.Path)})
	}
	if gone, err := s.recovered(l, key, m); err != nil {
		s.Logf("%s: pins: %v", id, err)
	} else {
		for _, g := range gone {
			s.Logf("unpinned %s: %s holds as much again", g, id)
		}
		res.Unpinned = gone
	}
	if m.Shrunk != nil {
		s.Logf("%s holds half or less of what %s held (%d entries, %d bytes, then %d, %d)", id, prev.ID, m.Entries, m.Bytes, prev.Meta.Entries, prev.Meta.Bytes)
		pin := Pin{ID: prev.ID.String(), Entries: prev.Meta.Entries, Bytes: prev.Meta.Bytes, By: id.String(), Since: now().UTC()}
		if err := s.addPin(l, key, pin); err != nil {
			s.Logf("pinning %s: %v", prev.ID, err)
			res.PinErr = err
		} else {
			s.Logf("pinned %s: %s shrank from it", prev.ID, id)
			p := prev.ID
			res.Pinned = &p
		}
		if o.Notify != nil {
			text := fmt.Sprintf("the checkpoint of %s holds half or less of what %s held; %s is kept until released: if files were lost, restore from it", src.Path, prev.ID, prev.ID)
			if res.Pinned == nil {
				text = fmt.Sprintf("the checkpoint of %s holds half or less of what %s held; if files were lost, restore from %s", src.Path, prev.ID, prev.ID)
			}
			o.Notify(Notice{Kind: NoticeShrunk, Path: src.Path, ID: id, Prev: prev.ID, Pinned: res.Pinned != nil, Text: text})
		}
	}
	switch {
	case o.NoPrune:
	case res.PinErr != nil:
		// The checkpoint to restore from is not pinned: nothing of the key
		// is dropped this time.
		res.PruneErr = fmt.Errorf("checkpoint: not pruned, since %s could not be pinned: %w", prev.ID, res.PinErr)
	default:
		res.Pruned, res.PruneErr = s.Prune(l, key, DefaultRules(now()), o.Pinned)
	}
	s.Logf("took %s method=%s duration=%s entries=%d bytes=%d linked=%d skipped=%d", id, m.Method,
		m.Finished.Sub(m.Started).Round(time.Millisecond), m.Entries, m.Bytes, m.Linked, m.Skipped)
	return res, nil
}

func kindOf(src Source) string {
	if src.File {
		return KindFile
	}
	return KindDir
}

// latestReadable is key's newest complete checkpoint with a usable
// meta.json, or nil.
func latestReadable(s *Store, key string) *Entry {
	es, err := s.List(key)
	if err != nil {
		return nil
	}
	for i := range es {
		if es[i].Err == nil && es[i].Meta != nil {
			return &es[i]
		}
	}
	return nil
}

// checkSpace reports whether the volume of dir is below the soft floor,
// and ErrDiskFull below the hard floor.
func checkSpace(fn StatfsFunc, dir string) (low bool, err error) {
	avail, total, err := fn(dir)
	if err != nil {
		return false, fmt.Errorf("checkpoint: free space: %w", err)
	}
	soft, hard := Floors(total)
	if avail < hard {
		return true, fmt.Errorf("%w (%d MiB free)", ErrDiskFull, avail>>20)
	}
	return avail < soft, nil
}

// shrank says whether entries or bytes fell sharply from prev's: to half
// or less, of more than 1,000 entries or 100 MiB.
func shrank(prev *Meta, entries, bytes int64) bool {
	return prev.Entries > shrinkEntries && entries*2 <= prev.Entries ||
		prev.Bytes > shrinkBytes && bytes*2 <= prev.Bytes
}

// dropLogin removes from a home source's copy in tmp every login file
// (home-relative, in denied) inside it, and returns them as ~/REL. The
// copy keeps the source's modes, so a read-only parent is given owner rwx
// through its descriptor first and its mode set back after. A login file
// that cannot be dropped is an error: a checkpoint is never kept with it.
func dropLogin(tmp string, src Source, denied []string) ([]string, error) {
	if src.Kind != SourceHome {
		return nil, nil
	}
	homeRel, ok := strings.CutPrefix(src.Rel, "home/")
	if !ok {
		return nil, fmt.Errorf("checkpoint: home source %q is not under home/", src.Rel)
	}
	tree := filepath.Join(tmp, TreeName)
	var dropped []string
	for _, d := range denied {
		var inner string
		switch {
		case d == homeRel:
		case strings.HasPrefix(d, homeRel+"/") && !src.File:
			inner = d[len(homeRel)+1:]
		default:
			continue
		}
		if inner == "" {
			if _, err := os.Lstat(tree); err != nil {
				continue
			}
			if err := saferm.Remove(tmp, tree); err != nil {
				return nil, fmt.Errorf("checkpoint: dropping ~/%s: %w", d, err)
			}
			dropped = append(dropped, "~/"+d)
			continue
		}
		did, err := dropIn(tmp, tree, inner)
		if err != nil {
			return nil, fmt.Errorf("checkpoint: dropping ~/%s: %w", d, err)
		}
		if did {
			dropped = append(dropped, "~/"+d)
		}
	}
	return dropped, nil
}

// dropIn removes inner (slash-separated) from the copy at tmp/tree, if
// it is there. The way to it is opened without following: where the copy
// holds a symlink or a file on the way, the login file is not in the copy.
// The copy keeps the source's modes, so each directory on the way that
// denies its owner rwx (0555, 0400, 0000) is given it, by name in its
// parent just looked at (the copy is in the store, which no sandbox
// reaches, so nothing changes it meanwhile), and set back after, innermost
// first.
func dropIn(tmp, tree, inner string) (bool, error) {
	top, err := os.OpenFile(tmp, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, err
	}
	open := []*os.File{top}
	type fix struct {
		parent *os.File
		name   string
		mode   uint32
	}
	var fixes []fix
	defer func() {
		for _, f := range open {
			f.Close()
		}
	}()
	restore := func() error {
		var errs []error
		for i := len(fixes) - 1; i >= 0; i-- {
			f := fixes[i]
			err := unix.Fchmodat(int(f.parent.Fd()), f.name, f.mode, 0)
			runtime.KeepAlive(f.parent)
			if err != nil {
				errs = append(errs, err)
			}
		}
		fixes = nil
		return errors.Join(errs...)
	}
	comps := []string{TreeName}
	if dir := path.Dir(inner); dir != "." {
		comps = append(comps, strings.Split(dir, "/")...)
	}
	parent := top
	for _, c := range comps {
		var st unix.Stat_t
		if err := fstatat(parent, c, &st); err != nil {
			if errors.Is(err, unix.ENOENT) {
				return false, restore()
			}
			return false, errors.Join(err, restore())
		}
		if uint32(st.Mode)&unix.S_IFMT != unix.S_IFDIR {
			return false, restore() // a symlink or a file: nothing below it
		}
		if mode := uint32(st.Mode) & 0o7777; mode&0o700 != 0o700 {
			err := unix.Fchmodat(int(parent.Fd()), c, mode|0o700, 0)
			runtime.KeepAlive(parent)
			if err != nil {
				return false, errors.Join(err, restore())
			}
			fixes = append(fixes, fix{parent, c, mode})
		}
		next, err := openat(parent, c, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return false, errors.Join(err, restore())
		}
		open = append(open, next)
		var nst unix.Stat_t
		if err := fstat(next, &nst); err != nil || !sameFile(&nst, &st) {
			return false, errors.Join(errors.New("the copy changed while dropping"), restore())
		}
		parent = next
	}
	base := path.Base(inner)
	var st unix.Stat_t
	if err := fstatat(parent, base, &st); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, restore()
		}
		return false, errors.Join(err, restore())
	}
	var rerr error
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		// A sandbox can make the login's name a directory: it goes whole,
		// so one planted directory cannot fail every checkpoint.
		rerr = saferm.RemoveTreeAt(parent, base)
	} else {
		rerr = saferm.Remove(tmp, filepath.Join(tree, filepath.FromSlash(inner)))
	}
	if rerr == nil {
		if err := fstatat(parent, base, &st); !errors.Is(err, unix.ENOENT) {
			rerr = errors.New("still there after its removal")
		}
	}
	if err := restore(); err != nil && rerr == nil {
		rerr = err
	}
	if rerr != nil {
		return false, rerr
	}
	return true, nil
}

// ErrExposed is returned when a sandbox can reach the checkpoint store.
var ErrExposed = errors.New("a sandbox can reach it")

// Reach is a host directory a sandbox can reach: a root (configured or
// mounted) or a mount source, and what it is, for the message.
type Reach struct {
	Path string
	What string // "root /work/dev of environment default"
}

// CheckReach refuses (ErrExposed) when any of reach is the store, holds
// it, or is inside it. That is decided by identity (device and inode),
// never by comparing path strings, since one directory can have several
// names (symlinks, bind mounts, firmlinks, a case-insensitive volume). A
// path that does not exist reaches nothing; one that cannot be looked at
// is an error. The caller sets Store.Exposed to call it with every
// environment's roots and mount sources.
func (s *Store) CheckReach(reach []Reach) error {
	type ident struct {
		dev uint64
		ino uint64
	}
	idOf := func(p string) (ident, error) {
		var st unix.Stat_t
		if err := unix.Stat(p, &st); err != nil {
			return ident{}, err
		}
		return ident{uint64(st.Dev), st.Ino}, nil
	}
	store, err := idOf(s.dir)
	if err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	up := map[ident]bool{store: true} // the store and each of its ancestors
	for p := filepath.Dir(s.dir); ; p = filepath.Dir(p) {
		id, err := idOf(p)
		if err != nil {
			return fmt.Errorf("checkpoint: %w", err)
		}
		up[id] = true
		if filepath.Dir(p) == p {
			break
		}
	}
	for _, r := range reach {
		if r.Path == "" {
			continue
		}
		id, err := idOf(r.Path)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return fmt.Errorf("checkpoint: %s: %w", r.Path, err)
		}
		exposed := up[id]
		if !exposed {
			// Inside the store: the store is among its ancestors.
			phys, err := filepath.EvalSymlinks(r.Path)
			if err != nil {
				return fmt.Errorf("checkpoint: %s: %w", r.Path, err)
			}
			for p := phys; ; p = filepath.Dir(p) {
				if pid, err := idOf(p); err == nil && pid == store {
					exposed = true
					break
				}
				if filepath.Dir(p) == p {
					break
				}
			}
		}
		if exposed {
			return fmt.Errorf("the checkpoint store %s is inside %s (%s): %w", s.dir, r.What, r.Path, ErrExposed)
		}
	}
	return nil
}
