package vmm

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/hvsock"
	"github.com/bfreis/caboose/internal/vm"
	"github.com/bfreis/caboose/internal/vm/vz"
)

// diskRunner is a VM with block devices of its own, whose resizes answer
// as err says, and whose guest has no ports.
type diskRunner struct {
	mu      sync.Mutex
	sizes   []int64
	err     error
	resizes []string
	hold    chan struct{} // a resize waits on it, when set
}

func (r *diskRunner) Start(vm.Machine) error { return nil }
func (r *diskRunner) Connect(uint32) (io.ReadWriteCloser, error) {
	return nil, errors.New("connection reset by peer")
}
func (r *diskRunner) Stop() error           { return nil }
func (r *diskRunner) Done() <-chan struct{} { return nil }
func (r *diskRunner) Err() error            { return nil }

func (r *diskRunner) DiskSize(i int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sizes[i], nil
}

func (r *diskRunner) ResizeDisk(i int, size int64) error {
	if r.hold != nil {
		<-r.hold
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resizes = append(r.resizes, fmt.Sprintf("%d %d", i, size))
	if vz.ResizeApplied(r.err) {
		r.sizes[i] = size
	}
	return r.err
}

// plainRunner is a VM with no block devices of its own.
type plainRunner struct{}

func (plainRunner) Start(vm.Machine) error { return nil }
func (plainRunner) Connect(uint32) (io.ReadWriteCloser, error) {
	return nil, errors.New("connection reset by peer")
}
func (plainRunner) Stop() error           { return nil }
func (plainRunner) Done() <-chan struct{} { return nil }
func (plainRunner) Err() error            { return nil }

var twoDisks = vm.Machine{BlockDevices: []vm.BlockDevice{{Serial: "a"}, {Serial: "cache"}}}

// A resize names its disk by serial, and its answer says what
// ResizeDisk's error says of the disk.
func TestDisksResize(t *testing.T) {
	notTold := fmt.Errorf("%w (5s)", vz.ErrNotTold)
	for _, tc := range []struct {
		name string
		err  error
		want vm.DiskAnswer
	}{
		{"done", nil, vm.DiskAnswer{Size: 8 << 30, Applied: true, Told: true}},
		{"not told", notTold, vm.DiskAnswer{Size: 8 << 30, Applied: true, Error: notTold.Error()}},
		{"refused", errors.New("the device stopped"), vm.DiskAnswer{Size: 4 << 30, Error: "the device stopped"}},
		{"shrink, file not cut", &vz.FileError{Capacity: 8 << 30, Size: 16 << 30, Applied: true, Err: errors.New("EBUSY")},
			vm.DiskAnswer{Size: 8 << 30, Applied: true, Told: true, FileSize: 16 << 30, FileError: "EBUSY"}},
		{"not told, file not cut", errors.Join(notTold, &vz.FileError{Capacity: 8 << 30, Size: 16 << 30, Applied: true, Err: errors.New("EBUSY")}),
			vm.DiskAnswer{Size: 8 << 30, Applied: true, FileSize: 16 << 30, FileError: "EBUSY"}},
		{"grow refused, file left", fmt.Errorf("refused; %w", &vz.FileError{Capacity: 4 << 30, Size: 8 << 30, Err: errors.New("EIO")}),
			vm.DiskAnswer{Size: 4 << 30, FileSize: 8 << 30, FileError: "EIO"}},
	} {
		r := &diskRunner{sizes: []int64{1 << 30, 4 << 30}, err: tc.err}
		d := newDisks(twoDisks, r, t.Logf)
		got := d.do(vm.DiskRequest{Op: vm.DiskResize, Serial: "cache", Size: 8 << 30})
		if tc.want.Error == "" && tc.err != nil {
			tc.want.Error = tc.err.Error()
		}
		if got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
		if len(r.resizes) != 1 || r.resizes[0] != fmt.Sprintf("1 %d", 8<<30) {
			t.Errorf("%s: resizes %q", tc.name, r.resizes)
		}
	}
}

// A size is asked by serial too; a serial the VM has no disk with, a
// runner with no disks of its own and an unknown request are refused.
func TestDisksRefusals(t *testing.T) {
	r := &diskRunner{sizes: []int64{1 << 30, 4 << 30}}
	d := newDisks(twoDisks, r, t.Logf)
	if a := d.do(vm.DiskRequest{Op: vm.DiskSize, Serial: "a"}); a != (vm.DiskAnswer{Size: 1 << 30}) {
		t.Errorf("size: %+v", a)
	}
	for _, tc := range []struct {
		d    *disks
		req  vm.DiskRequest
		want string
	}{
		{d, vm.DiskRequest{Op: vm.DiskResize, Serial: "other", Size: 8 << 30}, `no disk of caboose-vmm's with serial "other"`},
		{d, vm.DiskRequest{Op: "format", Serial: "a"}, `no disk request "format"`},
		{newDisks(twoDisks, plainRunner{}, t.Logf), vm.DiskRequest{Op: vm.DiskSize, Serial: "a"}, "serves no disks"},
	} {
		if a := tc.d.do(tc.req); !strings.Contains(a.Error, tc.want) {
			t.Errorf("%+v: %+v", tc.req, a)
		}
	}
	if len(r.resizes) != 0 {
		t.Errorf("resized %q", r.resizes)
	}
}

// A second resize while one runs is refused, not queued.
func TestDisksOneResize(t *testing.T) {
	r := &diskRunner{sizes: []int64{1 << 30, 4 << 30}, hold: make(chan struct{})}
	d := newDisks(twoDisks, r, t.Logf)
	done := make(chan vm.DiskAnswer)
	go func() { done <- d.do(vm.DiskRequest{Op: vm.DiskResize, Serial: "cache", Size: 8 << 30}) }()
	for len(d.resizing) == 0 {
		time.Sleep(time.Millisecond)
	}
	if a := d.do(vm.DiskRequest{Op: vm.DiskResize, Serial: "a", Size: 2 << 30}); !strings.Contains(a.Error, "another resize") {
		t.Errorf("a second resize: %+v", a)
	}
	close(r.hold)
	if a := <-done; !a.Applied {
		t.Errorf("the first: %+v", a)
	}
	if a := d.do(vm.DiskRequest{Op: vm.DiskResize, Serial: "a", Size: 2 << 30}); !a.Applied {
		t.Errorf("after it: %+v", a)
	}
}

// vm.sock's VMMPort is vmm's own: AskDisks reaches the runner's disks,
// and a vmm that serves no such port (an older one: the guest's, where
// nothing listens) is ErrOldVMM.
func TestDisksOverSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "vd")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := dir + "/vm.sock"
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	r := &diskRunner{sizes: []int64{1 << 30, 4 << 30}}
	go serve(ln, r, newDisks(twoDisks, r, t.Logf))
	a, err := vm.AskDisks(sock, vm.DiskRequest{Op: vm.DiskResize, Serial: "cache", Size: 6 << 30}, 5*time.Second)
	if err != nil || !a.Applied || a.Size != 6<<30 {
		t.Fatalf("%+v, %v", a, err)
	}

	old := dir + "/old.sock"
	ln2, err := net.Listen("unix", old)
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	go func() {
		for {
			c, err := ln2.Accept()
			if err != nil {
				return
			}
			// What an older vmm does: the guest's port, nothing there.
			_, _ = hvsock.Accept(c, time.Second)
			c.Close()
		}
	}()
	if _, err := vm.AskDisks(old, vm.DiskRequest{Op: vm.DiskSize, Serial: "cache"}, 5*time.Second); !errors.Is(err, vm.ErrOldVMM) {
		t.Errorf("an older vmm: %v", err)
	}
}
