// Package nofollow reads and writes files under a directory that something
// else can write to as well -- the container, for the parts of the data dir
// it mounts -- without following a symlink anywhere on the way.
//
// os.Root alone is not enough for that: it keeps a path inside the root, but
// follows a symlink that stays inside it, and the data dir holds secrets of
// its own (the Claude login in .claude/.credentials.json, gh's token). A
// memory file the container turned into a symlink to one of those would be
// read -- and synced -- as a memory file.
//
// So every path is walked one directory at a time. Each component is
// Lstat'ed, opened, and checked to be the object the Lstat saw
// (os.SameFile): one swapped for a symlink in between is refused, not
// followed. A file with more than one hard link is refused too, since a
// link is a way to the same secrets that no symlink check sees. Writes go
// to a new file, created exclusively, and are renamed into place, so a
// symlink at the target is replaced, never written through.
//
// The top directory itself is trusted: it is this process's own (the data
// dir, which the container does not mount), and its path is followed as
// given.
package nofollow

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"
)

// ErrNotPlain is returned for a path that is, or runs through, anything
// other than plain directories and a plain file: a symlink, a hard-linked
// file, a device, or an object swapped for another while it was opened.
var ErrNotPlain = errors.New("not a plain file or directory")

// Dir is a directory whose contents may be written by someone else.
type Dir string

// testHookOpened, when set, runs between the Lstat of a path component and
// its opening -- the window a swap would have to hit.
var testHookOpened func(name string)

// testHookRename, when set, runs just before Rename's rename, once the
// directory is open.
var testHookRename func(oldRel string)

func (d Dir) notPlain(rel string) error {
	return &fs.PathError{Op: "open", Path: path.Join(string(d), rel), Err: ErrNotPlain}
}

// split checks rel, a slash-separated path relative to d, and returns its
// elements. "" and "." are d itself.
func split(rel string) ([]string, error) {
	if rel == "" || rel == "." {
		return nil, nil
	}
	if strings.HasPrefix(rel, "/") || path.Clean(rel) != rel {
		return nil, fmt.Errorf("%q: not a clean relative path", rel)
	}
	els := strings.Split(rel, "/")
	for _, el := range els {
		if el == ".." {
			return nil, fmt.Errorf("%q: not a clean relative path", rel)
		}
	}
	return els, nil
}

// open opens the directory at rel, one plain directory at a time. With
// create, missing directories are made (0o777, less the umask).
func (d Dir) open(rel string, create bool) (*os.Root, error) {
	els, err := split(rel)
	if err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(string(d))
	if err != nil {
		return nil, err
	}
	for i, el := range els {
		fi, err := r.Lstat(el)
		if create && errors.Is(err, fs.ErrNotExist) {
			if err = r.Mkdir(el, 0o777); err == nil || errors.Is(err, fs.ErrExist) {
				fi, err = r.Lstat(el)
			}
		}
		if err != nil {
			r.Close()
			return nil, err
		}
		if !fi.IsDir() {
			r.Close()
			return nil, d.notPlain(strings.Join(els[:i+1], "/"))
		}
		if testHookOpened != nil {
			testHookOpened(strings.Join(els[:i+1], "/"))
		}
		sub, err := r.OpenRoot(el)
		r.Close()
		if err != nil {
			return nil, err
		}
		if st, err := sub.Stat("."); err != nil || !os.SameFile(fi, st) {
			sub.Close()
			return nil, d.notPlain(strings.Join(els[:i+1], "/"))
		}
		r = sub
	}
	return r, nil
}

// parent opens the directory holding rel and returns it with rel's last
// element.
func (d Dir) parent(rel string, create bool) (*os.Root, string, error) {
	els, err := split(rel)
	if err != nil {
		return nil, "", err
	}
	if len(els) == 0 {
		return nil, "", fmt.Errorf("%q: names no file", rel)
	}
	r, err := d.open(strings.Join(els[:len(els)-1], "/"), create)
	if err != nil {
		return nil, "", err
	}
	return r, els[len(els)-1], nil
}

// plainFile reports whether fi is a regular file with one link.
func plainFile(fi fs.FileInfo) bool {
	if !fi.Mode().IsRegular() {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return !ok || uint64(st.Nlink) <= 1
}

// openFile opens the plain file name in r with flag, checked as open checks
// a directory. O_NONBLOCK keeps a FIFO swapped in from blocking the open.
func (d Dir) openFile(r *os.Root, rel, name string, flag int) (*os.File, fs.FileInfo, error) {
	fi, err := r.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !plainFile(fi) {
		return nil, nil, d.notPlain(rel)
	}
	if testHookOpened != nil {
		testHookOpened(rel)
	}
	f, err := r.OpenFile(name, flag|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	if st, err := f.Stat(); err != nil || !os.SameFile(fi, st) || !plainFile(st) {
		f.Close()
		return nil, nil, d.notPlain(rel)
	}
	return f, fi, nil
}

// Lstat is rel's own FileInfo, reached through plain directories.
func (d Dir) Lstat(rel string) (fs.FileInfo, error) {
	els, err := split(rel)
	if err != nil {
		return nil, err
	}
	if len(els) == 0 {
		return os.Lstat(string(d))
	}
	r, name, err := d.parent(rel, false)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return r.Lstat(name)
}

// ErrTooLarge is returned by ReadFileMax for a file over its limit.
var ErrTooLarge = errors.New("file too large")

// ReadFileMax reads the plain file at rel, of at most max bytes, and
// returns its mode with it. One whose size is over max is refused before a
// byte is read, and what grows past it while being read is cut off there,
// so a sparse or growing file costs max+1 bytes at most. The error is
// ErrTooLarge. There is no unbounded read: whoever writes the directory
// decides how large its files are.
func (d Dir) ReadFileMax(rel string, max int64) ([]byte, fs.FileMode, error) {
	r, name, err := d.parent(rel, false)
	if err != nil {
		return nil, 0, err
	}
	defer r.Close()
	f, fi, err := d.openFile(r, rel, name, os.O_RDONLY)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	tooLarge := &fs.PathError{Op: "read", Path: path.Join(string(d), rel), Err: ErrTooLarge}
	if fi.Size() > max {
		return nil, 0, tooLarge
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err == nil && int64(len(data)) > max {
		return nil, 0, tooLarge
	}
	return data, fi.Mode(), err
}

// MaxDepth and MaxEntries bound what Walk walks, and MaxEntries what
// ReadDir lists: whoever writes the directory decides how deep and wide its
// tree is, and each level is opened from the top again.
const (
	MaxDepth   = 64
	MaxEntries = 200000
)

// maxEntries is MaxEntries, lowered by tests.
var maxEntries = MaxEntries

// ErrTooMany is returned for a directory, or a tree, over MaxEntries or
// MaxDepth.
var ErrTooMany = errors.New("too many entries, or too deep, to read")

// ReadDir lists the directory at rel, sorted by name: at most MaxEntries,
// else ErrTooMany.
func (d Dir) ReadDir(rel string) ([]fs.DirEntry, error) {
	return d.readDir(rel, maxEntries)
}

// readDir is ReadDir for at most max entries.
func (d Dir) readDir(rel string, max int) ([]fs.DirEntry, error) {
	r, err := d.open(rel, false)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	f, err := r.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var es []fs.DirEntry
	for {
		batch, err := f.ReadDir(1024)
		es = append(es, batch...)
		if len(es) > max {
			return nil, &fs.PathError{Op: "readdir", Path: path.Join(string(d), rel), Err: ErrTooMany}
		}
		if errors.Is(err, io.EOF) || err == nil && len(batch) == 0 {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	slices.SortFunc(es, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return es, nil
}

// WriteFile replaces the file at rel with data, making the directories on
// the way; perm is applied with the umask, as os.WriteFile applies it to a
// new file. The new file is written beside it and renamed over it: whatever
// was at rel (a symlink, say) is replaced, never written through. A
// directory at rel is an error.
func (d Dir) WriteFile(rel string, data []byte, perm fs.FileMode) error {
	r, name, err := d.parent(rel, true)
	if err != nil {
		return err
	}
	defer r.Close()
	if fi, err := r.Lstat(name); err == nil && fi.IsDir() {
		return &fs.PathError{Op: "write", Path: path.Join(string(d), rel), Err: syscall.EISDIR}
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	tmp := ".caboose-" + hex.EncodeToString(b[:])
	f, err := r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = r.Rename(tmp, name)
	}
	if err != nil {
		_ = r.Remove(tmp)
	}
	return err
}

// WriteInPlace overwrites the plain file at rel, keeping it the same file:
// for a single-file bind mount, which a rename over would fail on.
func (d Dir) WriteInPlace(rel string, data []byte) error {
	r, name, err := d.parent(rel, false)
	if err != nil {
		return err
	}
	defer r.Close()
	f, _, err := d.openFile(r, rel, name, os.O_WRONLY)
	if err != nil {
		return err
	}
	// Truncated only once it is known to be the file meant.
	err = f.Truncate(0)
	if err == nil {
		_, err = f.Write(data)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Remove removes the file, symlink or empty directory at rel, itself: the
// last element is never followed.
func (d Dir) Remove(rel string) error {
	r, name, err := d.parent(rel, false)
	if err != nil {
		return err
	}
	defer r.Close()
	return r.Remove(name)
}

// ErrRenameAcross is returned by Rename for two paths in different
// directories.
var ErrRenameAcross = errors.New("rename across directories")

// Rename renames the entry at oldRel to newRel, both in one directory, which
// is reached through plain directories. The entry itself is moved, whatever
// it is (a symlink stays a symlink, pointing where it did; a FIFO or a
// directory is moved, never opened): neither last element is followed. It
// is how something the other side planted is moved aside without touching
// what it names.
//
// An existing entry at newRel is refused with fs.ErrExist rather than
// replaced: moving aside never loses a file. That check and the rename are
// two steps, so an entry created at newRel in between is replaced -- the
// directory entry, never what it names, since rename(2) does not follow it.
// Putting a file into place over another is WriteFile's job.
func (d Dir) Rename(oldRel, newRel string) error {
	oldEls, err := split(oldRel)
	if err != nil {
		return err
	}
	newEls, err := split(newRel)
	if err != nil {
		return err
	}
	if len(oldEls) == 0 || len(newEls) == 0 {
		return fmt.Errorf("rename %q to %q: names no file", oldRel, newRel)
	}
	if !slices.Equal(oldEls[:len(oldEls)-1], newEls[:len(newEls)-1]) {
		return &os.LinkError{Op: "rename", Old: path.Join(string(d), oldRel), New: path.Join(string(d), newRel), Err: ErrRenameAcross}
	}
	r, oldName, err := d.parent(oldRel, false)
	if err != nil {
		return err
	}
	defer r.Close()
	newName := newEls[len(newEls)-1]
	if _, err := r.Lstat(oldName); err != nil {
		return err
	}
	if oldName == newName {
		return nil
	}
	if _, err := r.Lstat(newName); err == nil {
		return &os.LinkError{Op: "rename", Old: path.Join(string(d), oldRel), New: path.Join(string(d), newRel), Err: fs.ErrExist}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if testHookRename != nil {
		testHookRename(oldRel)
	}
	return r.Rename(oldName, newName)
}

// Walk calls fn for every entry under the directory at rel, depth first and
// in name order, with the entry's path relative to d. Directories are
// entered only when plain; fn returning fs.SkipDir for one skips it. A
// missing rel is walked as empty. A tree deeper than MaxDepth below rel, or
// of more than MaxEntries entries, is ErrTooMany once Walk reaches past
// the bound, after fn has seen what came before.
func (d Dir) Walk(rel string, fn func(rel string, e fs.DirEntry) error) error {
	left := maxEntries
	return d.walk(rel, fn, 0, &left)
}

func (d Dir) walk(rel string, fn func(rel string, e fs.DirEntry) error, depth int, left *int) error {
	if depth > MaxDepth {
		return &fs.PathError{Op: "walk", Path: path.Join(string(d), rel), Err: ErrTooMany}
	}
	es, err := d.readDir(rel, *left)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	*left -= len(es)
	for _, e := range es {
		p := path.Join(rel, e.Name())
		err := fn(p, e)
		if e.IsDir() {
			if errors.Is(err, fs.SkipDir) {
				continue
			}
			if err == nil {
				err = d.walk(p, fn, depth+1, left)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}
