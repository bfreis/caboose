package checkpoint

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bfreis/caboose/internal/cappedlog"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/saferm"
	"golang.org/x/sys/unix"
)

const (
	metaName = "meta.json"
	pathName = "path.json"
	logName  = "checkpoints.log"
	lockName = "lock"
	// TreeName is the copy inside a checkpoint: a directory, or a file for
	// a file source.
	TreeName = "tree"

	tmpPrefix   = ".tmp-"
	trashPrefix = ".trash-"

	logCap = 1 << 20
	// LeftoverAge is how old a .tmp- or .trash- directory must be before
	// CleanLeftovers removes it: a younger one is left, in case it belongs
	// to a take or a removal that is still running.
	LeftoverAge = time.Hour
)

// StoreDir is the store of the environment whose directory is envDir.
func StoreDir(envDir string) string { return envDir + "/checkpoints" }

// Store is an environment's checkpoint store, opened and checked.
type Store struct {
	dir string // physical, checked inside CABOOSE_HOME

	// Exposed says whether a sandbox can reach the store (an error when it
	// can; CheckReach decides it from the roots and mount sources). Prune
	// and CleanLeftovers, and whatever takes or restores, call it first and
	// refuse on error. Prune and CleanLeftovers skip the check when it is
	// nil; TakeLocked refuses to take a checkpoint at all.
	Exposed func() error

	logMu sync.Mutex
	log   *cappedlog.Log
}

// Open opens the store of the environment whose directory is envDir,
// creating it (0700) if missing; envDir must exist. It refuses unless
// cabooseHome is absolute, envDir is absolute and clean and is
// config.EnvDirFor(cabooseHome, name) for a valid environment name, and the
// store's physical path is strictly inside the physical cabooseHome. That
// physical path (Dir) is the only root a removal in the store is made with.
func Open(cabooseHome, envDir string) (*Store, error) {
	if cabooseHome == "" || !filepath.IsAbs(cabooseHome) {
		return nil, fmt.Errorf("checkpoint: CABOOSE_HOME %q is not an absolute path", cabooseHome)
	}
	if envDir == "" || !filepath.IsAbs(envDir) || filepath.Clean(envDir) != envDir {
		return nil, fmt.Errorf("checkpoint: environment directory %q is not an absolute clean path", envDir)
	}
	name := filepath.Base(envDir)
	if !config.ValidEnv(name) || config.EnvDirFor(cabooseHome, name) != envDir {
		return nil, fmt.Errorf("checkpoint: %q is not an environment directory of %q", envDir, cabooseHome)
	}
	home, err := filepath.EvalSymlinks(cabooseHome)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	env, err := filepath.EvalSymlinks(envDir)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	dir := StoreDir(env)
	rel, err := filepath.Rel(home, dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, fmt.Errorf("checkpoint: the store %s is not inside %s", dir, home)
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("checkpoint: the store %s is not a directory", dir)
	}
	return &Store{dir: dir}, nil
}

// Dir is the store's physical path.
func (s *Store) Dir() string { return s.dir }

// Close closes the store's log, if it was opened.
func (s *Store) Close() error {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.log == nil {
		return nil
	}
	err := s.log.Close()
	s.log = nil
	return err
}

// Logf appends a line to the store's log, checkpoints.log, capped at 1 MiB.
// A log that cannot be opened is not an error of what is being logged.
func (s *Store) Logf(format string, args ...any) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.log == nil {
		l, err := cappedlog.Open(filepath.Join(s.dir, logName), logCap, "caboose checkpoints")
		if err != nil {
			return
		}
		s.log = l
	}
	line := time.Now().Format("2006/01/02 15:04:05 ") + fmt.Sprintf(format, args...)
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	_, _ = s.log.Write([]byte(line))
}

// exposed runs the exposed-store check, if one is set.
func (s *Store) exposed() error {
	if s.Exposed == nil {
		return nil
	}
	if err := s.Exposed(); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	return nil
}

// KeyInfo is a key of the store and the path it is for.
type KeyInfo struct {
	Key  string
	Path string
}

// Keys lists the store's keys, each with the path its path.json names.
// Entries that are not a key's directory, and keys whose path.json is
// missing, unreadable or names a path of another key, are left out.
func (s *Store) Keys() ([]KeyInfo, error) {
	des, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	var out []KeyInfo
	for _, de := range des {
		k := de.Name()
		if !ValidKey(k) || !de.IsDir() {
			continue
		}
		p, err := s.keyPath(k)
		if err != nil || Key(p) != k {
			continue
		}
		out = append(out, KeyInfo{Key: k, Path: p})
	}
	return out, nil
}

// keyPath reads key k's path.json, refusing anything but a regular file.
func (s *Store) keyPath(k string) (string, error) {
	f, err := openRegular(filepath.Join(s.dir, k, pathName))
	if err != nil {
		return "", err
	}
	defer f.Close()
	return decodePath(f)
}

// openRegular opens p for reading without following a symlink, and only if
// it is a regular file.
func openRegular(p string) (*os.File, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("checkpoint: %s is not a regular file", p)
	}
	return f, nil
}

// Entry is one checkpoint as listed. Err is set when its meta.json cannot
// be used (unreadable, of a newer caboose, or naming another checkpoint):
// such a checkpoint is shown but never pruned.
type Entry struct {
	ID    ID
	Stamp time.Time
	Meta  *Meta
	Err   error
}

// checkpointDir is the directory of the checkpoint id in s.
func (s *Store) checkpointDir(id ID) string {
	return filepath.Join(s.dir, id.Key, FormatStamp(id.Stamp))
}

// List lists key's checkpoints, newest first. Only a directory named by a
// valid stamp and holding a meta.json counts: one without meta.json is not
// a checkpoint. A key with no directory has none.
func (s *Store) List(key string) ([]Entry, error) {
	if !ValidKey(key) {
		return nil, fmt.Errorf("checkpoint: bad key %q", key)
	}
	kd := filepath.Join(s.dir, key)
	fi, err := os.Lstat(kd)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("checkpoint: %s is not a directory", kd)
	}
	des, err := os.ReadDir(kd)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	var out []Entry
	for _, de := range des {
		t, err := ParseStamp(de.Name())
		if err != nil || !de.IsDir() {
			continue
		}
		id := ID{Key: key, Stamp: t}
		mp := filepath.Join(kd, de.Name(), metaName)
		if _, err := os.Lstat(mp); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		e := Entry{ID: id, Stamp: t}
		e.Meta, e.Err = readMeta(mp, id)
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Stamp.After(out[j].Stamp) })
	return out, nil
}

func readMeta(p string, id ID) (*Meta, error) {
	f, err := openRegular(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m, err := DecodeMeta(f)
	if err != nil {
		return nil, err
	}
	if m.ID != id.String() {
		return nil, fmt.Errorf("checkpoint: %s names %q", p, m.ID)
	}
	return m, nil
}

// Resolve turns KEY@latest into the newest complete, readable checkpoint
// of the key; a concrete ID is returned as it is, if it is listed.
func (s *Store) Resolve(id ID) (Entry, error) {
	es, err := s.List(id.Key)
	if err != nil {
		return Entry{}, err
	}
	for _, e := range es {
		if id.Latest && e.Err == nil || !id.Latest && e.Stamp.Equal(id.Stamp) {
			return e, nil
		}
	}
	return Entry{}, fmt.Errorf("checkpoint: no checkpoint %s", id)
}

// NextStamp is the stamp a new checkpoint of key taken at now gets: the
// first second at or after now that key holds no directory for. Two
// checkpoints in the same second never share a stamp: the second one is
// stamped a second later (and may be a little ahead of the clock), never
// over the first. Under the lock, so nothing else takes it meanwhile;
// NewTemp then carries the stamp to Publish.
func (s *Store) NextStamp(l *Lock, key string, now time.Time) (time.Time, error) {
	if err := l.check(s); err != nil {
		return time.Time{}, err
	}
	if !ValidKey(key) {
		return time.Time{}, fmt.Errorf("checkpoint: bad key %q", key)
	}
	t := now.UTC().Truncate(time.Second)
	for {
		_, err := os.Lstat(filepath.Join(s.dir, key, FormatStamp(t)))
		if errors.Is(err, fs.ErrNotExist) {
			return t, nil
		}
		if err != nil {
			return time.Time{}, fmt.Errorf("checkpoint: %w", err)
		}
		t = t.Add(time.Second)
	}
}

// NewTemp makes the directory (0700) a checkpoint of key stamped stamp is
// built in, .tmp-<key>-<stamp>-<rand> in the store, which nothing lists.
// The caller fills it (the copy goes in TreeName) and gives it to Publish,
// or removes it with saferm.RemoveTree(s.Dir(), dir) on failure.
func (s *Store) NewTemp(l *Lock, key string, stamp time.Time) (string, error) {
	if err := l.check(s); err != nil {
		return "", err
	}
	if !ValidKey(key) {
		return "", fmt.Errorf("checkpoint: bad key %q", key)
	}
	d, err := os.MkdirTemp(s.dir, tmpPrefix+key+"-"+FormatStamp(stamp)+"-")
	if err != nil {
		return "", fmt.Errorf("checkpoint: %w", err)
	}
	return d, nil
}

// tempStamp reads back the stamp NewTemp put in tmp's name, for key.
func (s *Store) tempStamp(tmp, key string) (time.Time, error) {
	bad := fmt.Errorf("checkpoint: %q is not a directory NewTemp made for %s", tmp, key)
	if filepath.Dir(tmp) != s.dir {
		return time.Time{}, bad
	}
	rest, ok := strings.CutPrefix(filepath.Base(tmp), tmpPrefix+key+"-")
	if !ok || len(rest) < len(StampLayout)+2 || rest[len(StampLayout)] != '-' {
		return time.Time{}, bad
	}
	t, err := ParseStamp(rest[:len(StampLayout)])
	if err != nil {
		return time.Time{}, bad
	}
	fi, err := os.Lstat(tmp)
	if err != nil || !fi.IsDir() {
		return time.Time{}, bad
	}
	return t, nil
}

// Publish makes the directory tmp, from NewTemp, the checkpoint of path it
// was made for: it writes tmp/meta.json from m (exclusive create; Version,
// ID and Path are filled in), syncs it and tmp, makes the key's directory
// and path.json on first use (a key whose path.json names another path is
// refused), refuses when the checkpoint is already there, and renames tmp
// into place, so the checkpoint appears whole. On error tmp is left for the
// caller to remove.
func (s *Store) Publish(l *Lock, tmp, path string, m Meta) (ID, error) {
	if err := l.check(s); err != nil {
		return ID{}, err
	}
	if path == "" || strings.IndexByte(path, 0) >= 0 {
		return ID{}, fmt.Errorf("checkpoint: bad path %q", path)
	}
	key := Key(path)
	stamp, err := s.tempStamp(tmp, key)
	if err != nil {
		return ID{}, err
	}
	id := ID{Key: key, Stamp: stamp}
	m.Version, m.ID, m.Path = MetaVersion, id.String(), path
	b, err := json.MarshalIndent(&m, "", "  ")
	if err != nil {
		return ID{}, fmt.Errorf("checkpoint: %w", err)
	}
	if err := writeExclusive(filepath.Join(tmp, metaName), append(b, '\n')); err != nil {
		return ID{}, err
	}
	if err := syncDir(tmp); err != nil {
		return ID{}, err
	}
	if err := s.ensureKey(key, path); err != nil {
		return ID{}, err
	}
	dst := s.checkpointDir(id)
	if _, err := os.Lstat(dst); !errors.Is(err, fs.ErrNotExist) {
		if err == nil {
			return ID{}, fmt.Errorf("checkpoint: %s already exists", id)
		}
		return ID{}, fmt.Errorf("checkpoint: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return ID{}, fmt.Errorf("checkpoint: %w", err)
	}
	// The checkpoint is in place: a sync that fails now is logged, not
	// reported as a checkpoint that failed.
	for _, d := range []string{filepath.Join(s.dir, key), s.dir} {
		if err := syncDir(d); err != nil {
			s.Logf("%s published, but syncing %s: %v", id, d, err)
		}
	}
	return id, nil
}

// ensureKey makes key's directory (0700) and its path.json for path, or
// checks the path.json there names path.
func (s *Store) ensureKey(key, path string) error {
	kd := filepath.Join(s.dir, key)
	if err := os.Mkdir(kd, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("checkpoint: %w", err)
	}
	fi, err := os.Lstat(kd)
	if err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("checkpoint: %s is not a directory", kd)
	}
	pj := filepath.Join(kd, pathName)
	if _, err := os.Lstat(pj); err == nil {
		got, err := s.keyPath(key)
		if err != nil {
			s.Logf("key %s: path.json: %v", key, err)
			return fmt.Errorf("checkpoint: key %s: %w", key, err)
		}
		if got != path {
			s.Logf("key %s: path.json names %q, not %q: refused", key, got, path)
			return fmt.Errorf("checkpoint: key %s is for %q, not %q", key, got, path)
		}
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("checkpoint: %w", err)
	}
	b, err := json.Marshal(pathDoc{Version: MetaVersion, Path: path})
	if err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	// Written whole under another name and linked into place, so path.json
	// is never seen half written, and never replaced.
	tmp := pj + ".tmp-" + randHex()
	if err := writeExclusive(tmp, append(b, '\n')); err != nil {
		return err
	}
	lerr := os.Link(tmp, pj)
	if err := saferm.Remove(kd, tmp); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	if lerr != nil {
		return fmt.Errorf("checkpoint: %w", lerr)
	}
	return syncDir(kd)
}

// writeExclusive creates p (0600, failing if anything is there), writes b
// and syncs it.
func writeExclusive(p string, b []byte) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return writeFileErr(err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return writeFileErr(err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return writeFileErr(err)
	}
	if err := f.Close(); err != nil {
		return writeFileErr(err)
	}
	return nil
}

// writeFileErr is a failed write of a store file: ErrDiskFull for want of
// space.
func writeFileErr(err error) error {
	if errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT) {
		return fmt.Errorf("%w: %w", ErrDiskFull, err)
	}
	return fmt.Errorf("checkpoint: %w", err)
}

func syncDir(p string) error {
	f, err := os.OpenFile(p, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	return nil
}

func randHex() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail
	}
	return hex.EncodeToString(b[:])
}

// CleanLeftovers removes the store's .tmp- and .trash- directories older
// than LeftoverAge (by mtime, against now): what a take or a removal that
// did not finish left; and, as old, the temporary files a write of
// pins.json or of a key's path.json that did not finish left (regular
// files only, through saferm). It needs the store's lock, held, and refuses while
// the store is exposed. Younger ones are kept, and anything of those names
// that is not a directory is left alone and logged. It returns the names
// removed.
func (s *Store) CleanLeftovers(l *Lock, now time.Time) ([]string, error) {
	if err := l.check(s); err != nil {
		return nil, err
	}
	if err := s.exposed(); err != nil {
		return nil, err
	}
	des, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	var removed []string
	var errs []error
	for _, de := range des {
		n := de.Name()
		if !strings.HasPrefix(n, tmpPrefix) && !strings.HasPrefix(n, trashPrefix) {
			continue
		}
		p := filepath.Join(s.dir, n)
		fi, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if !fi.IsDir() {
			s.Logf("leftover %s is not a directory: left alone", n)
			continue
		}
		if now.Sub(fi.ModTime()) < LeftoverAge {
			continue
		}
		if err := saferm.RemoveTree(s.dir, p); err != nil {
			s.Logf("removing leftover %s: %v", n, err)
			errs = append(errs, err)
			continue
		}
		s.Logf("removed leftover %s", n)
		removed = append(removed, n)
	}
	files, err := s.cleanTempFiles(now)
	removed = append(removed, files...)
	if err != nil {
		errs = append(errs, err)
	}
	return removed, errors.Join(errs...)
}

// cleanTempFiles removes pins.json.tmp-* in the store and path.json.tmp-*
// in each key's directory, older than LeftoverAge, and returns them as
// paths relative to the store.
func (s *Store) cleanTempFiles(now time.Time) ([]string, error) {
	var removed []string
	var errs []error
	sweep := func(dir, prefix string) {
		des, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, de := range des {
			n := de.Name()
			if !strings.HasPrefix(n, prefix) {
				continue
			}
			p := filepath.Join(dir, n)
			fi, err := os.Lstat(p)
			if err != nil || !fi.Mode().IsRegular() || now.Sub(fi.ModTime()) < LeftoverAge {
				continue
			}
			rel, _ := filepath.Rel(s.dir, p)
			if err := saferm.Remove(s.dir, p); err != nil {
				s.Logf("removing leftover %s: %v", rel, err)
				errs = append(errs, err)
				continue
			}
			s.Logf("removed leftover %s", rel)
			removed = append(removed, rel)
		}
	}
	sweep(s.dir, pinsName+".tmp-")
	des, err := os.ReadDir(s.dir)
	if err != nil {
		return removed, fmt.Errorf("checkpoint: %w", err)
	}
	for _, de := range des {
		if ValidKey(de.Name()) && de.IsDir() {
			sweep(filepath.Join(s.dir, de.Name()), pathName+".tmp-")
		}
	}
	return removed, errors.Join(errs...)
}
