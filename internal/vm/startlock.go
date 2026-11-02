package vm

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// HoldStart waits for, then takes, the exclusive lock on d's StartLock,
// and returns what lets go of it. A start holds it from making the scratch
// disk until vmm has it open, and a stop from asking the guest to go until
// the disk is deleted, so that one launcher's stop never deletes the disk
// another's start has just made. With no directory, the error is
// os.ErrNotExist's: there is no VM to order.
func (d Dir) HoldStart() (func(), error) { return HoldLock(d.StartLock()) }

// Resizing says whether a resize of the VM's volumes holds its
// ResizeLock now.
func (d Dir) Resizing() bool {
	unlock, err := TryLock(d.ResizeLock())
	if err == nil {
		unlock()
	}
	return errors.Is(err, ErrLocked)
}

// ErrLocked is TryLock's: another process holds the lock.
var ErrLocked = errors.New("the lock is held")

// TryLock takes the exclusive flock on path, made when it is missing, or
// returns ErrLocked at once when another process holds it.
func TryLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err != syscall.EINTR {
			break
		}
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		f.Close()
		return nil, ErrLocked
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return func() { f.Close() }, nil
}

// HoldLock waits for, then takes, an exclusive flock on path, made when it
// is missing (its directory is not), and returns what lets go of it. The
// lock is the open file's: a process that ends lets go of it, and a
// process started while it is held, as vmm is, does not inherit it (Go
// opens every file close-on-exec).
func HoldLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return func() { f.Close() }, nil
}
