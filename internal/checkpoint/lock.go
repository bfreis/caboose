package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// ErrLocked is returned when the store's lock is held elsewhere: a
// checkpoint is being taken, pruned or restored.
var ErrLocked = errors.New("checkpoint: a checkpoint is being taken; try again")

// DefaultLockWait is how long Store.Lock waits when given no wait.
const DefaultLockWait = 30 * time.Second

// lockPoll is how often Store.Lock tries again.
const lockPoll = 50 * time.Millisecond

// Lock is the store's lock, held: an exclusive flock on the store's lock
// file, taken for every take, prune and restore. A flock belongs to the
// open file, so two Locks in one process exclude each other too.
type Lock struct {
	mu  sync.Mutex
	dir string // the store's physical path
	f   *os.File
}

// TryLock takes the store's lock if it is free, or returns ErrLocked.
func (s *Store) TryLock() (*Lock, error) {
	f, err := os.OpenFile(filepath.Join(s.dir, lockName), os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		runtime.KeepAlive(f)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("checkpoint: lock: %w", err)
	}
	return &Lock{dir: s.dir, f: f}, nil
}

// Lock takes the store's lock, waiting up to wait for it (DefaultLockWait
// when wait is 0 or less) or until ctx ends; past that it returns an error
// that is ErrLocked.
func (s *Store) Lock(ctx context.Context, wait time.Duration) (*Lock, error) {
	if wait <= 0 {
		wait = DefaultLockWait
	}
	deadline := time.Now().Add(wait)
	for {
		l, err := s.TryLock()
		if !errors.Is(err, ErrLocked) {
			return l, err
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("waited %s for the checkpoint store %s: %w", wait, s.dir, ErrLocked)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w (%v)", ErrLocked, ctx.Err())
		case <-time.After(lockPoll):
		}
	}
}

// Unlock releases the lock; a second call does nothing.
func (l *Lock) Unlock() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close() // closing the only descriptor releases the flock
	l.f = nil
	return err
}

// Held says whether l is held and is s's lock.
func (l *Lock) Held(s *Store) bool { return l.check(s) == nil }

// check refuses a lock that is nil, released, or another store's.
func (l *Lock) check(s *Store) error {
	if l == nil {
		return errors.New("checkpoint: the store's lock is not held")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return errors.New("checkpoint: the store's lock was released")
	}
	if s == nil || l.dir != s.dir {
		return errors.New("checkpoint: a lock of another store")
	}
	return nil
}
