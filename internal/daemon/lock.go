package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// flock is unix.Flock on f, again when a signal interrupts it.
func flock(f *os.File, how int) error {
	for {
		err := unix.Flock(int(f.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// tryLock takes the exclusive flock on path (created, 0600) without waiting.
// Held by another, it fails with an error matching syscall.EWOULDBLOCK.
func tryLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := flock(f, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, &fs.PathError{Op: "flock", Path: path, Err: err}
	}
	return f, nil
}

// unlock releases a lock tryLock or lockWait took.
func unlock(f *os.File) {
	_ = flock(f, unix.LOCK_UN)
	f.Close()
}

// lockWait takes the exclusive flock on path, waiting at most d for another
// holder to let go.
func lockWait(ctx context.Context, path string, d time.Duration) (*os.File, error) {
	deadline := time.Now().Add(d)
	for {
		f, err := tryLock(path)
		if err == nil || !errors.Is(err, syscall.EWOULDBLOCK) {
			return f, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s stayed locked for %v: %w", path, d, syscall.EWOULDBLOCK)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// held reports whether something holds the flock on path. It creates
// nothing: a lock file that does not exist is held by no one.
func held(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := flock(f, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true, nil
		}
		return false, err
	}
	_ = flock(f, unix.LOCK_UN)
	return false, nil
}
