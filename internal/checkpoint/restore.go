package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/proposal"
	"github.com/bfreis/caboose/internal/saferm"
	"golang.org/x/sys/unix"
)

// restoreTempPrefix starts the name of a restore's temporary directory,
// made beside the target.
const restoreTempPrefix = ".caboose-restore-"

// restoreOut is the copy's name inside that directory.
const restoreOut = "out"

var (
	// ErrTargetExists is returned when anything is at the target already:
	// a restore writes only into a new directory.
	ErrTargetExists = errors.New("checkpoint: the target exists; restore writes only into a new directory")
	// ErrTargetRefused is returned for a target restore may not write.
	ErrTargetRefused = errors.New("checkpoint: restore may not write there")
	// ErrNoReplace is returned when the target's filesystem cannot rename
	// without replacing what is there; the finished copy is left in place.
	ErrNoReplace = errors.New("checkpoint: this filesystem cannot rename without replacing")
)

// errNoReplace is what a system without a no-replace rename gives.
var errNoReplace = errors.New("no rename without replacing on this system")

// Test seams: the no-replace rename, a hook run just before it, and one
// run between the copy and making it safe.
var (
	renameNoReplace      = renameNoReplaceOS
	restoreTake          = TakeLocked
	testHookBeforeRename func(target string)
	testHookAfterCopy    func(out string)
)

// EnvPath is a host path and the environment whose sandbox reaches it.
type EnvPath struct {
	Env  string
	Path string
}

// PathSet is what sandboxes reach on the host: what they can write (every
// environment's writable set), and the parts of roots they can read
// without writing.
type PathSet struct {
	Writable []EnvPath
	ReadOnly []EnvPath
	// Fallback says the set is the conservative one, computed without
	// the daemon (every root of every environment counted as writable, and
	// every data dir's home).
	Fallback bool
}

// SandboxPaths gives restore the PathSet its target is judged against.
type SandboxPaths interface {
	SandboxPaths(ctx context.Context) (PathSet, error)
}

// RestoreOptions are Restore's.
type RestoreOptions struct {
	// CabooseHome is CABOOSE_HOME: a target inside it is refused.
	CabooseHome string
	// Paths gives the paths sandboxes reach. Required.
	Paths SandboxPaths
	// Live returns the live source of the checkpoint's configured path, to
	// take a before-restore checkpoint of: nil when the path is no longer
	// configured or does not resolve (the error says why). Nil Live: none
	// is taken.
	Live func(path string) (*Source, error)

	LockWait    time.Duration    // how long to wait for the store's lock; Store.Lock's default when 0
	Now         func() time.Time // the clock; time.Now when nil
	Statfs      StatfsFunc       // free space on the target's volume; unix.Statfs when nil
	StoreStatfs StatfsFunc       // free space on the store's volume, for the before-restore take
	Notify      func(Notice)     // passed to the before-restore take
	// Copy carries Copy's test switches (NoWhole, NoReflink, MaxEntries,
	// Hooks) for the copy out; the before-restore take gets the first
	// three. Restore sets the rest.
	Copy Options
}

// Restored is what Restore did.
type Restored struct {
	Target string // the new directory (or file), physical
	ID     ID     // the checkpoint restored, concrete
	Path   string // the configured path it is a checkpoint of
	// BeforeRestore is the checkpoint of the live path taken first, or nil;
	// NoBeforeRestore then says why none was taken.
	BeforeRestore   *ID
	NoBeforeRestore string
	Method          Method
	Entries         int64
	// Skipped counts what the copy left out (special files, directories
	// it could not read, another filesystem), with up to 20 examples.
	Skipped         int64
	SkippedExamples []string
	// Sanitized is every change made to make the copy safe to open on the
	// host.
	Sanitized []Sanitized
	// Warnings are for the user: the conservative set used, a
	// before-restore checkpoint that failed.
	Warnings []string
	Readers  []string // environments whose sandboxes can read it, read-only
	Fallback bool     // the conservative PathSet was used
	// HomePath says the checkpoint is of a path of the sandbox's home,
	// which may hold secrets of its own.
	HomePath bool
	// Left is where the finished copy was left when it could not be
	// renamed into place; "" otherwise.
	Left string
}

// Restore copies the checkpoint id (KEY@STAMP, or KEY@latest) into to, a
// directory (or file) that must not exist, and makes the copy safe to
// open on the host. It holds the store's lock for its whole run:
//
//   - the ID is resolved to a concrete checkpoint first, with a complete,
//     readable meta.json; that one is restored, and pinned while a
//     checkpoint of the live path is taken (o.Live), before anything is
//     copied, so whatever is done with the restored copy can be undone;
//   - the target must not exist (an empty directory or a dangling symlink
//     there is refused too) and its parent must be a directory; one inside
//     CABOOSE_HOME or any environment's writable set (configured or
//     mounted) is refused before anything is written, and readers are
//     reported;
//   - the store must not be exposed, and the target's volume must stay
//     above the hard free-space floor before, during and after the copy;
//   - the copy goes into a temporary directory beside the target, never
//     hard-linked out of the store, is made safe (sanitizeTree), and is
//     renamed into place by a rename that never replaces anything; where
//     the filesystem has none, or the target appeared meanwhile, the
//     finished copy is left where it is and the error says where.
//
// On any failure while copying or making safe, the temporary directory
// is removed, by the exact path made; nothing else is created, changed or
// removed outside it and the new target.
func Restore(ctx context.Context, s *Store, id, to string, o RestoreOptions) (Restored, error) {
	var res Restored
	if s == nil {
		return res, errors.New("checkpoint: no store")
	}
	if o.Paths == nil {
		return res, errors.New("checkpoint: restore needs the paths sandboxes reach")
	}
	if o.CabooseHome == "" || !filepath.IsAbs(o.CabooseHome) {
		return res, fmt.Errorf("checkpoint: CABOOSE_HOME %q is not an absolute path", o.CabooseHome)
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	statfsFn := o.Statfs
	if statfsFn == nil {
		statfsFn = statfs
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

	// The checkpoint, resolved once: KEY@latest is fixed here, before the
	// before-restore checkpoint would become the latest.
	e, err := s.Resolve(pid)
	if err != nil {
		return res, err
	}
	if e.Err != nil || e.Meta == nil {
		return res, fmt.Errorf("checkpoint: %s cannot be read: %v", e.ID, e.Err)
	}
	res.ID, res.Path = e.ID, e.Meta.Path
	res.HomePath = strings.HasPrefix(e.Meta.Path, "~")

	target, parent, err := restoreTarget(to)
	if err != nil {
		return res, err
	}
	res.Target = target

	set, err := o.Paths.SandboxPaths(ctx)
	if err != nil {
		return res, fmt.Errorf("checkpoint: which paths sandboxes reach: %w", err)
	}
	if res.Fallback = set.Fallback; res.Fallback {
		res.Warnings = append(res.Warnings, fallbackWarning)
		s.Logf("restore of %s to %s: %s", res.ID, target, fallbackWarning)
	}
	res.Readers, err = restoreTargetPolicy(target, o.CabooseHome, set)
	if err != nil {
		s.Logf("restore of %s to %s refused: %v", res.ID, target, err)
		return res, err
	}

	if _, err := checkSpace(statfsFn, parent); err != nil {
		s.Logf("restore of %s to %s not done: %v", res.ID, target, err)
		return res, err
	}

	// A checkpoint of the live path first, with the one restored pinned.
	switch src, lerr := liveSource(o.Live, e.Meta.Path); {
	case src != nil:
		tr, err := restoreTake(ctx, l, s, *src, ReasonBeforeRestore, TakeOptions{
			NoPrune: true, Pinned: []ID{res.ID}, Now: o.Now, Statfs: o.StoreStatfs, Notify: o.Notify,
			Copy: Options{NoWhole: o.Copy.NoWhole, NoReflink: o.Copy.NoReflink, MaxEntries: o.Copy.MaxEntries},
		})
		if err != nil {
			res.NoBeforeRestore = fmt.Sprintf("the checkpoint of %s before restoring failed: %v", e.Meta.Path, err)
			res.Warnings = append(res.Warnings, res.NoBeforeRestore)
		} else {
			res.BeforeRestore = &tr.ID
		}
	default:
		res.NoBeforeRestore = fmt.Sprintf("%s does not resolve now: %v", e.Meta.Path, lerr)
	}

	// The parent, and the temporary directory made in it, are held open
	// from here on: everything below is done relative to them, so a
	// sandbox that can write the parent cannot redirect any of it.
	parentF, err := openHeldDir(parent, nil, "")
	if err != nil {
		return res, err
	}
	defer parentF.Close()
	tmp, err := os.MkdirTemp(parent, restoreTempPrefix)
	if err != nil {
		return res, fmt.Errorf("checkpoint: %w", err)
	}
	out := filepath.Join(tmp, restoreOut)
	fail := func(err error) (Restored, error) {
		if rerr := saferm.RemoveTree(parent, tmp); rerr != nil {
			err = errors.Join(err, fmt.Errorf("removing %s: %w", tmp, rerr))
		}
		s.Logf("restore of %s to %s failed: %v", res.ID, target, err)
		return res, err
	}
	tmpF, err := openHeldDir(tmp, parentF, filepath.Base(tmp))
	if err != nil {
		return fail(err)
	}
	defer tmpF.Close()
	space := func() error {
		_, err := checkSpace(statfsFn, parent)
		return err
	}
	co := o.Copy
	co.Root, co.Now, co.Space = tmp, now, space
	co.Prev, co.PrevStarted = "", time.Time{} // never hard-linked out of the store
	src := Source{
		Path: e.Meta.Path,
		Kind: SourceRoot,
		Top:  s.dir,
		Rel:  e.ID.Key + "/" + FormatStamp(e.ID.Stamp) + "/" + TreeName,
		File: e.Meta.Kind == KindFile,
	}
	cr, err := copyAt(ctx, src, tmpF, out, co)
	if err != nil {
		return fail(err)
	}
	if err := space(); err != nil {
		return fail(err)
	}
	res.Method, res.Entries = cr.Method, cr.Entries
	res.Skipped, res.SkippedExamples = cr.Skipped, cr.SkippedExamples
	if testHookAfterCopy != nil {
		testHookAfterCopy(out)
	}
	sz, made, err := sanitizeTree(ctx, tmpF, restoreOut, src.File, o.Copy.MaxEntries)
	if err != nil {
		return fail(err)
	}
	res.Sanitized = sz
	// What is renamed into place is the tree just made safe, nothing put
	// in its place meanwhile.
	var ost unix.Stat_t
	if err := fstatat(tmpF, restoreOut, &ost); err != nil || idOf(&ost) != made {
		return fail(fmt.Errorf("checkpoint: the restored copy in %s was replaced while it was made safe", tmp))
	}

	if testHookBeforeRename != nil {
		testHookBeforeRename(target)
	}
	if err := renameNoReplace(tmpF, restoreOut, parentF, filepath.Base(target)); err != nil {
		res.Left = out
		switch {
		case errors.Is(err, unix.EEXIST) || errors.Is(err, unix.ENOTEMPTY):
			err = fmt.Errorf("%w: %s appeared while restoring; nothing was overwritten; the copy is in %s", ErrTargetExists, target, out)
		case errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) ||
			errors.Is(err, unix.ENOSYS) || errors.Is(err, errNoReplace):
			err = fmt.Errorf("%w (%v); the restored copy is in %s: move it yourself", ErrNoReplace, err, out)
		default:
			err = fmt.Errorf("checkpoint: renaming the restored copy into place: %w; it is in %s", err, out)
		}
		s.Logf("restore of %s to %s: %v", res.ID, target, err)
		return res, err
	}
	if err := saferm.Remove(parent, tmp); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("the empty %s could not be removed: %v", tmp, err))
	}
	before := "none"
	if res.BeforeRestore != nil {
		before = res.BeforeRestore.String()
	}
	s.Logf("restored %s to %s method=%s entries=%d sanitized=%d before-restore=%s", res.ID, target, res.Method, res.Entries, len(res.Sanitized), before)
	return res, nil
}

// openHeldDir opens the directory p to hold for a restore, never
// following its last element, and checks it is the directory an Lstat of
// p sees. With in, it is opened as name relative to in.
func openHeldDir(p string, in *os.File, name string) (*os.File, error) {
	var want unix.Stat_t
	if err := unix.Lstat(p, &want); err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	var f *os.File
	var err error
	if in != nil {
		f, err = openat(in, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	} else {
		f, err = os.OpenFile(p, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	var got unix.Stat_t
	if err := fstat(f, &got); err != nil || !sameFile(&want, &got) {
		f.Close()
		return nil, fmt.Errorf("checkpoint: %s changed while it was opened", p)
	}
	return f, nil
}

// liveSource asks live for the source of path; no live is no source.
func liveSource(live func(string) (*Source, error), path string) (*Source, error) {
	if live == nil {
		return nil, errors.New("not asked")
	}
	src, err := live(path)
	if src == nil && err == nil {
		err = errors.New("not configured")
	}
	return src, err
}

// restoreTarget checks to (after ~ expansion and made absolute) is free
// and its parent a directory, and returns the target on its parent's
// physical path, and that parent.
func restoreTarget(to string) (target, parent string, err error) {
	if to == "" {
		return "", "", errors.New("checkpoint: no target given")
	}
	if to == "~" || strings.HasPrefix(to, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", fmt.Errorf("checkpoint: %w", err)
		}
		to = filepath.Join(home, strings.TrimPrefix(to, "~"))
	}
	abs, err := filepath.Abs(to)
	if err != nil {
		return "", "", fmt.Errorf("checkpoint: %w", err)
	}
	if err := free(abs); err != nil {
		return "", "", err
	}
	base := filepath.Base(abs)
	if base == "." || base == ".." || base == string(filepath.Separator) {
		return "", "", fmt.Errorf("checkpoint: %s names no new directory", abs)
	}
	parent, err = filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", "", fmt.Errorf("checkpoint: the target's parent: %w", err)
	}
	fi, err := os.Lstat(parent)
	if err != nil {
		return "", "", fmt.Errorf("checkpoint: the target's parent: %w", err)
	}
	if !fi.IsDir() {
		return "", "", fmt.Errorf("checkpoint: the target's parent %s is not a directory", parent)
	}
	target = filepath.Join(parent, base)
	if err := free(target); err != nil {
		return "", "", err
	}
	return target, parent, nil
}

// free says p holds nothing, not even a dangling symlink.
func free(p string) error {
	_, err := os.Lstat(p)
	if err == nil {
		return fmt.Errorf("%w: %s", ErrTargetExists, proposal.Printable(p))
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("checkpoint: %w", err)
	}
	return nil
}

// fallbackWarning says the conservative PathSet was used.
const fallbackWarning = "caboose could not ask its daemon which paths sandboxes can write, so it counted every root of every environment, and every data dir's home, as writable"

// restoreTargetPolicy judges the target (its parent physical): inside
// CABOOSE_HOME (the data dirs, the store, the daemon's directories), or
// inside any environment's writable set, where a sandbox could change the
// copy while it is written and made safe, and a tree from one environment
// would be handed to another's sandbox. Either is refused. Inside a
// read-only part of a root is fine, and the environments whose sandboxes
// can read it are returned.
func restoreTargetPolicy(target, cabooseHome string, set PathSet) ([]string, error) {
	in := newInside(target)
	shown := proposal.Printable(target)
	if in.under(cabooseHome) {
		return nil, fmt.Errorf("%w: %s is inside CABOOSE_HOME (%s)", ErrTargetRefused, shown, cabooseHome)
	}
	writers := map[string]bool{}
	for _, p := range set.Writable {
		if in.under(p.Path) {
			writers[p.Env] = true
		}
	}
	readers := map[string]bool{}
	for _, p := range set.ReadOnly {
		if in.under(p.Path) {
			readers[p.Env] = true
		}
	}
	if len(writers) > 0 {
		return nil, fmt.Errorf("%w: the sandboxes of %s can write %s", ErrTargetRefused, envList(sortedKeys(writers)), shown)
	}
	return sortedKeys(readers), nil
}

func envList(envs []string) string {
	if len(envs) == 1 {
		return "environment " + envs[0]
	}
	return "environments " + strings.Join(envs, ", ")
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// inside answers whether a target that does not exist yet is at or below
// a path: by the paths' physical strings, and by identity (device and
// inode) of each of the target's existing ancestors, since one directory
// can have several names (a symlink, a bind mount, a firmlink, a
// case-insensitive volume).
type inside struct {
	target string
	ids    map[devIno]bool
}

func newInside(target string) *inside {
	in := &inside{target: target, ids: map[devIno]bool{}}
	for p := filepath.Dir(target); ; p = filepath.Dir(p) {
		var st unix.Stat_t
		if unix.Stat(p, &st) == nil {
			in.ids[idOf(&st)] = true
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return in
}

func (in *inside) under(p string) bool {
	if p == "" || !filepath.IsAbs(p) {
		return false
	}
	phys := filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		phys = r
	}
	if within(phys, in.target) || within(filepath.Clean(p), in.target) {
		return true
	}
	var st unix.Stat_t
	return unix.Stat(p, &st) == nil && in.ids[idOf(&st)]
}

// within says whether p is dir or below it, by their strings.
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
