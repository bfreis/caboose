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

// ReadFile reads the plain file at rel, and returns its mode with it.
func (d Dir) ReadFile(rel string) ([]byte, fs.FileMode, error) {
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
	data, err := io.ReadAll(f)
	return data, fi.Mode(), err
}

// ReadDir lists the directory at rel, sorted by name.
func (d Dir) ReadDir(rel string) ([]fs.DirEntry, error) {
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
	es, err := f.ReadDir(-1)
	slices.SortFunc(es, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return es, err
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

// Walk calls fn for every entry under the directory at rel, depth first and
// in name order, with the entry's path relative to d. Directories are
// entered only when plain; fn returning fs.SkipDir for one skips it. A
// missing rel is walked as empty.
func (d Dir) Walk(rel string, fn func(rel string, e fs.DirEntry) error) error {
	es, err := d.ReadDir(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range es {
		p := path.Join(rel, e.Name())
		err := fn(p, e)
		if e.IsDir() {
			if errors.Is(err, fs.SkipDir) {
				continue
			}
			if err == nil {
				err = d.Walk(p, fn)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}
