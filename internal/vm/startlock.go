package vm

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
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

// answerWaits are how long Answers waits on each dial: a vmm that has
// not been scheduled for a while (a Mac just woken) can take more than
// the first to accept.
var answerWaits = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// dialVMM dials vm.sock (the tests stand in for it).
var dialVMM = func(path string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", path, timeout)
}

// Answers reports whether a vmm answers on d's vm.sock, trying again,
// with a longer wait, while a dial fails for another reason than there
// being no socket or nothing listening on it: those are a no at once.
func (d Dir) Answers() bool {
	for _, wait := range answerWaits {
		c, err := dialVMM(d.Socket(), wait)
		if err == nil {
			c.Close()
			return true
		}
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return false
		}
	}
	return false
}

// Runs reports whether a vmm runs d's VM: one holds its Lock, which vmm
// takes for as long as it runs, or answers on vm.sock (Answers). Seeing
// whether the lock is held means taking it for a moment, which would make
// a vmm starting at that moment fail: call Runs only with the StartLock
// held, which every start holds while it starts vmm.
func (d Dir) Runs() bool {
	unlock, err := TryLock(d.Lock())
	if err != nil {
		// Held, or not to be looked at: running, as far as anyone can
		// tell.
		return true
	}
	unlock()
	return d.Answers()
}
