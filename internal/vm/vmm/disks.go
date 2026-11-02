package vmm

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/bfreis/caboose/internal/vm"
	"github.com/bfreis/caboose/internal/vm/vz"
)

// Disks is a Runner that serves block devices of its own (vm.Machine's
// BlockDevices, by index), whose size can change while the VM runs.
type Disks interface {
	// DiskSize is device i's capacity in bytes.
	DiskSize(i int) (int64, error)
	// ResizeDisk makes device i size bytes and tells the guest; what its
	// error says of the disk is vz.ResizeApplied's and vz.FileError's.
	ResizeDisk(i int, size int64) error
}

// disks answers the requests on vm.VMMPort for m's block devices, on r,
// one resize at a time: a second one while one runs is refused, not
// queued, since the launcher that asked is waiting with a bound of its
// own.
type disks struct {
	m        vm.Machine
	r        Runner
	resizing chan struct{} // holds a token while a resize runs
	logf     func(format string, args ...any)
}

func newDisks(m vm.Machine, r Runner, logf func(format string, args ...any)) *disks {
	return &disks{m: m, r: r, resizing: make(chan struct{}, 1), logf: logf}
}

// serve answers the one request on c.
func (d *disks) serve(c net.Conn) {
	defer c.Close()
	_ = vm.ServeDisks(c, c.SetDeadline, 5*time.Second, d.do)
}

// do answers req.
func (d *disks) do(req vm.DiskRequest) vm.DiskAnswer {
	dr, ok := d.r.(Disks)
	if !ok {
		return vm.DiskAnswer{Error: "this VM's runner serves no disks of its own"}
	}
	i := -1
	for j, b := range d.m.BlockDevices {
		if b.Serial == req.Serial {
			i = j
		}
	}
	if i < 0 {
		return vm.DiskAnswer{Error: fmt.Sprintf("this VM has no disk of caboose-vmm's with serial %q", req.Serial)}
	}
	size := func() int64 { n, _ := dr.DiskSize(i); return n }
	switch req.Op {
	case vm.DiskSize:
		n, err := dr.DiskSize(i)
		if err != nil {
			return vm.DiskAnswer{Error: err.Error()}
		}
		return vm.DiskAnswer{Size: n}
	case vm.DiskResize:
		select {
		case d.resizing <- struct{}{}:
			defer func() { <-d.resizing }()
		default:
			return vm.DiskAnswer{Size: size(), Error: "another resize of this VM's disks is running"}
		}
		t0 := time.Now()
		err := dr.ResizeDisk(i, req.Size)
		a := resizeAnswer(err)
		a.Size = size()
		d.logf("disk %q: resized to %d bytes in %v (asked %d): %v", req.Serial, a.Size, time.Since(t0), req.Size, err)
		return a
	}
	return vm.DiskAnswer{Error: fmt.Sprintf("no disk request %q", req.Op)}
}

// resizeAnswer is what err, ResizeDisk's, says of the disk.
func resizeAnswer(err error) vm.DiskAnswer {
	a := vm.DiskAnswer{Applied: vz.ResizeApplied(err), Told: err == nil}
	var fe *vz.FileError
	if errors.As(err, &fe) {
		a.FileSize, a.FileError = fe.Size, fe.Err.Error()
		a.Told = a.Applied && !errors.Is(err, vz.ErrNotTold)
	}
	if err != nil {
		a.Error = err.Error()
	}
	return a
}
