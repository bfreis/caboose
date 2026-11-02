package builder

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/vm"
)

// syncBuffer is a bytes.Buffer two goroutines may use.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Start takes the builder's start lock, waiting, and saying so, while
// another build holds it, and lets go of it when the start fails; a
// guest's Stop lets go of it too.
func TestStartHoldsTheLock(t *testing.T) {
	dir := vm.Dir(t.TempDir())
	var stderr syncBuffer
	vmmFails := errors.New("no vmm here")
	b := &Builder{Dir: dir, Stderr: &stderr, StartVMM: func(vm.Dir) error { return vmmFails }}
	held, err := vm.TryLock(dir.StartLock())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := b.Start("", "")
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(stderr.String(), "another build is using the builder; waiting for it to end") {
		if time.Now().After(deadline) {
			t.Fatalf("no word of waiting: %q", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("started while the lock was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	held()
	if err := <-done; !errors.Is(err, vmmFails) {
		t.Fatalf("start: %v", err)
	}
	again, err := vm.TryLock(dir.StartLock())
	if err != nil {
		t.Fatalf("a failed start kept the lock: %v", err)
	}
	again()

	// A guest holds it until Stop.
	unlock, err := dir.HoldStart()
	if err != nil {
		t.Fatal(err)
	}
	g := &Guest{b: b, unlock: unlock}
	if _, err := vm.TryLock(dir.StartLock()); !errors.Is(err, vm.ErrLocked) {
		t.Fatalf("a running guest's lock is free: %v", err)
	}
	if err := g.Stop(); err != nil {
		t.Fatal(err)
	}
	again, err = vm.TryLock(dir.StartLock())
	if err != nil {
		t.Fatalf("Stop kept the lock: %v", err)
	}
	again()
}
