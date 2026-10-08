package apkobuild

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// A package cache has an advisory lock, CacheLockFile in it: whatever uses
// the cache (Resolve, Build, Check) holds it shared for as long as it does,
// and PruneCache takes it exclusive, without waiting, so that it never
// removes a package a build has fetched, or found cached, and not yet
// recorded in a lock. The one-hour window PruneCache also keeps only sees
// what was fetched lately, not what a build found already there.

// CacheLockFile is the lock file's name in a package cache.
const CacheLockFile = ".lock"

// ErrCacheInUse is PruneCache's when something is using the cache: it then
// removes nothing.
var ErrCacheInUse = errors.New("a build is using the package cache")

// LockCache takes dir's lock, shared, waiting for a prune that holds it to
// finish, and returns what releases it. dir is made when missing. "" is
// no cache, with nothing to lock.
func LockCache(dir string) (unlock func(), err error) {
	if dir == "" {
		return func() {}, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("the package cache: %w", err)
	}
	f, err := openCacheLock(dir)
	if err != nil {
		return nil, err
	}
	if err := flock(f, unix.LOCK_SH); err != nil {
		f.Close()
		return nil, fmt.Errorf("locking the package cache: %w", err)
	}
	return func() { f.Close() }, nil
}

// tryLockCacheExclusive takes dir's lock exclusive, if nothing holds it:
// ok is false when something does.
func tryLockCacheExclusive(dir string) (unlock func(), ok bool, err error) {
	f, err := openCacheLock(dir)
	if err != nil {
		return nil, false, err
	}
	switch err := flock(f, unix.LOCK_EX|unix.LOCK_NB); {
	case errors.Is(err, unix.EWOULDBLOCK):
		f.Close()
		return nil, false, nil
	case err != nil:
		f.Close()
		return nil, false, fmt.Errorf("locking the package cache: %w", err)
	}
	return func() { f.Close() }, true, nil
}

// openCacheLock opens dir's lock file, made 0644 when missing, never
// through a symlink, and refuses anything there but a plain file.
func openCacheLock(dir string) (*os.File, error) {
	p := filepath.Join(dir, CacheLockFile)
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("the package cache's lock: %w", err)
	}
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = errors.New("not a plain file")
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("the package cache's lock %s: %w", p, err)
	}
	return f, nil
}

// flock is unix.Flock on f, again when a signal interrupts it.
func flock(f *os.File, how int) error {
	for {
		err := unix.Flock(int(f.Fd()), how)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
