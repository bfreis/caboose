package backend

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/vm"
)

// holdRig is a VM with one volume, "cache", as a hold sees it: its record,
// a socket that answers when it runs, and the guest's agent stood in for,
// every call recorded in order.
type holdRig struct {
	t      *testing.T
	v      *VM
	mu     sync.Mutex
	calls  []string
	ops    []agentproto.VolumeOp
	answer map[string]func(agentproto.VolumeOp) (agentproto.VolumeInfo, error)
}

func newHoldRig(t *testing.T, volumes ...string) *holdRig {
	t.Helper()
	dir, err := os.MkdirTemp("", "vh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	r := &holdRig{t: t, answer: map[string]func(agentproto.VolumeOp) (agentproto.VolumeInfo, error){}}
	d := vm.Dir(filepath.Join(dir, "vm"))
	st := vm.State{Machine: vm.Machine{Disks: []vm.Disk{{Path: "root"}, {Path: "scratch"}}}, Volumes: volumes}
	for _, n := range volumes {
		st.Boot.Disks = append(st.Boot.Disks, agentproto.GuestDisk{Serial: n, Target: "/home/agent/" + n})
	}
	if err := d.WriteState(st); err != nil {
		t.Fatal(err)
	}
	r.v = NewVM("vh", d, nil)
	r.v.resize = &resizeSeams{agent: func() (volumeAgent, error) { r.call("dial"); return holdAgent{r}, nil }}
	return r
}

func (r *holdRig) call(c string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
}

func (r *holdRig) got() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.calls, ",")
}

// serve makes the VM's socket answer, so that it counts as running.
func (r *holdRig) serve() {
	ln, err := net.Listen("unix", r.v.Dir.Socket())
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
}

type holdAgent struct{ r *holdRig }

func (a holdAgent) Volume(op agentproto.VolumeOp, _ time.Duration) (agentproto.VolumeInfo, error) {
	a.r.call(op.Op)
	a.r.mu.Lock()
	a.r.ops = append(a.r.ops, op)
	f := a.r.answer[op.Op]
	a.r.mu.Unlock()
	if f != nil {
		return f(op)
	}
	return agentproto.VolumeInfo{Op: op.Op, Target: op.Target}, nil
}

func (a holdAgent) Close() error { a.r.call("close"); return nil }

// during is a clone that records itself, and checks the VM can neither
// start nor stop meanwhile.
func (r *holdRig) during(err error) func() error {
	return func() error {
		r.call("clone")
		if unlock, lerr := vm.TryLock(r.v.Dir.StartLock()); lerr == nil {
			unlock()
			r.t.Error("the start lock was free during the clone")
		}
		return err
	}
}

// Running with the volume: freeze, clone, thaw, with one token and the
// volume's target, then the connection closed.
func TestHoldVolumeFreezes(t *testing.T) {
	r := newHoldRig(t, "other", "cache")
	r.serve()
	held, err := r.v.HoldVolume(context.Background(), "cache", r.during(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := r.got(); got != "dial,freeze,clone,thaw,close" {
		t.Errorf("calls %s", got)
	}
	if !held.Frozen || held.FrozenFor <= 0 {
		t.Errorf("held %+v", held)
	}
	f, th := r.ops[0], r.ops[1]
	if f.Name != "cache" || f.Target != "/home/agent/cache" || f.Token == "" || th.Token != f.Token || th.Target != f.Target {
		t.Errorf("ops %+v", r.ops)
	}
}

// Whatever the clone does, a frozen filesystem is thawed.
func TestHoldVolumeThawsAfterFailedClone(t *testing.T) {
	r := newHoldRig(t, "cache")
	r.serve()
	_, err := r.v.HoldVolume(context.Background(), "cache", r.during(errors.New("no space")))
	if err == nil || !strings.Contains(err.Error(), "no space") {
		t.Errorf("err %v", err)
	}
	if got := r.got(); got != "dial,freeze,clone,thaw,close" {
		t.Errorf("calls %s", got)
	}
}

// A clone past its bound: thawed at the bound, before the clone ends,
// and an error, though the clone is waited for.
func TestHoldVolumeSlowClone(t *testing.T) {
	r := newHoldRig(t, "cache")
	r.serve()
	old := holdWait
	holdWait = 30 * time.Millisecond
	t.Cleanup(func() { holdWait = old })
	release := make(chan struct{})
	r.answer[agentproto.VolumeThaw] = func(op agentproto.VolumeOp) (agentproto.VolumeInfo, error) {
		close(release)
		return agentproto.VolumeInfo{Op: op.Op}, nil
	}
	held, err := r.v.HoldVolume(context.Background(), "cache", func() error {
		r.call("clone")
		select {
		case <-release:
		case <-time.After(2 * time.Second):
			r.call("no thaw while cloning")
		}
		r.call("clone done")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "took longer than 30ms") {
		t.Errorf("err %v", err)
	}
	if got := r.got(); got != "dial,freeze,clone,thaw,clone done,close" {
		t.Errorf("calls %s", got)
	}
	if !held.Frozen {
		t.Errorf("held %+v", held)
	}
}

// A freeze that fails, or is not answered, sends no thaw on the
// connection (a late answer would be read as the thaw's): it is closed,
// which has the agent thaw what it froze. The clone does not run.
func TestHoldVolumeFreezeFails(t *testing.T) {
	for name, f := range map[string]func(agentproto.VolumeOp) (agentproto.VolumeInfo, error){
		"refused": func(op agentproto.VolumeOp) (agentproto.VolumeInfo, error) {
			return agentproto.VolumeInfo{Op: op.Op, Error: "is not mounted"}, nil
		},
		"no answer": func(agentproto.VolumeOp) (agentproto.VolumeInfo, error) {
			return agentproto.VolumeInfo{}, vm.ErrNoVolumeAnswer
		},
		"broken": func(agentproto.VolumeOp) (agentproto.VolumeInfo, error) {
			return agentproto.VolumeInfo{}, errors.New("connection reset")
		},
	} {
		r := newHoldRig(t, "cache")
		r.serve()
		r.answer[agentproto.VolumeFreeze] = f
		_, err := r.v.HoldVolume(context.Background(), "cache", r.during(nil))
		if err == nil || !strings.Contains(err.Error(), "freezing volume cache") {
			t.Errorf("%s: err %v", name, err)
		}
		if got := r.got(); got != "dial,freeze,close" {
			t.Errorf("%s: calls %s", name, got)
		}
	}
}

// A thaw that finds the freeze already ended, fails or is not answered:
// an error, so the clone is not used; the connection is closed.
func TestHoldVolumeThawFails(t *testing.T) {
	for name, tc := range map[string]struct {
		f    func(agentproto.VolumeOp) (agentproto.VolumeInfo, error)
		want string
	}{
		"expired": {func(op agentproto.VolumeOp) (agentproto.VolumeInfo, error) {
			return agentproto.VolumeInfo{Op: op.Op, Expired: true, Note: "the agent thawed it itself after 20s"}, nil
		}, "may not be consistent"},
		"no answer": {func(agentproto.VolumeOp) (agentproto.VolumeInfo, error) {
			return agentproto.VolumeInfo{}, vm.ErrNoVolumeAnswer
		}, "once this connection ends"},
		"failed": {func(op agentproto.VolumeOp) (agentproto.VolumeInfo, error) {
			return agentproto.VolumeInfo{Op: op.Op, Error: "FITHAW: EIO"}, nil
		}, "FITHAW: EIO"},
	} {
		r := newHoldRig(t, "cache")
		r.serve()
		r.answer[agentproto.VolumeThaw] = tc.f
		_, err := r.v.HoldVolume(context.Background(), "cache", r.during(nil))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v", name, err)
		}
		if got := r.got(); got != "dial,freeze,clone,thaw,close" {
			t.Errorf("%s: calls %s", name, got)
		}
	}
}

// Stopped, or running without the volume: the clone runs with the VM kept
// from starting, and no agent is asked.
func TestHoldVolumeUnfrozen(t *testing.T) {
	r := newHoldRig(t, "cache")
	held, err := r.v.HoldVolume(context.Background(), "cache", r.during(nil))
	if err != nil || held.Frozen || r.got() != "clone" {
		t.Errorf("stopped: %+v %v %s", held, err, r.got())
	}
	r = newHoldRig(t, "other")
	r.serve()
	held, err = r.v.HoldVolume(context.Background(), "cache", r.during(nil))
	if err != nil || held.Frozen || r.got() != "clone" {
		t.Errorf("without the volume: %+v %v %s", held, err, r.got())
	}
	// No VM at all: nothing uses the disk.
	v := NewVM("none", vm.Dir(filepath.Join(t.TempDir(), "none")), nil)
	ran := false
	if _, err := v.HoldVolume(context.Background(), "cache", func() error { ran = true; return nil }); err != nil || !ran {
		t.Errorf("no VM: %v %v", err, ran)
	}
}

// A resize running, which holds the resize lock and the start lock:
// refused, the clone never run.
func TestHoldVolumeDuringResize(t *testing.T) {
	r := newHoldRig(t, "cache")
	r.serve()
	unlock, err := vm.TryLock(r.v.Dir.ResizeLock())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	unstart, err := vm.TryLock(r.v.Dir.StartLock())
	if err != nil {
		t.Fatal(err)
	}
	defer unstart()
	_, err = r.v.HoldVolume(context.Background(), "cache", r.during(nil))
	if err == nil || !strings.Contains(err.Error(), "resize") || r.got() != "" {
		t.Errorf("err %v, calls %s", err, r.got())
	}
}

// The resize lock alone held, as a start or stop's probe of it holds it
// for an instant (Resizing), is no resize: the checkpoint goes on.
func TestHoldVolumeResizeLockAlone(t *testing.T) {
	r := newHoldRig(t, "cache")
	r.serve()
	unlock, err := vm.TryLock(r.v.Dir.ResizeLock())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := r.v.HoldVolume(context.Background(), "cache", r.during(nil)); err != nil || r.got() != "dial,freeze,clone,thaw,close" {
		t.Errorf("err %v, calls %s", err, r.got())
	}
}

// A start or stop holding the start lock is waited for, saying so, and
// the wait ends with ctx, or at holdLockWait.
func TestHoldVolumeWaitsForStart(t *testing.T) {
	r := newHoldRig(t, "cache")
	r.serve()
	var mu sync.Mutex
	var notes []string
	r.v.Note = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		notes = append(notes, fmt.Sprintf(format, args...))
	}
	unstart, err := vm.TryLock(r.v.Dir.StartLock())
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(150 * time.Millisecond); unstart() }()
	t0 := time.Now()
	if _, err := r.v.HoldVolume(context.Background(), "cache", r.during(nil)); err != nil || r.got() != "dial,freeze,clone,thaw,close" {
		t.Errorf("err %v, calls %s", err, r.got())
	}
	mu.Lock()
	if time.Since(t0) < 100*time.Millisecond || len(notes) != 1 || !strings.Contains(notes[0], "waiting for the VM's start or stop") {
		t.Errorf("after %v, notes %q", time.Since(t0), notes)
	}
	mu.Unlock()

	r = newHoldRig(t, "cache")
	r.serve()
	unstart, err = vm.TryLock(r.v.Dir.StartLock())
	if err != nil {
		t.Fatal(err)
	}
	defer unstart()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if _, err := r.v.HoldVolume(ctx, "cache", r.during(nil)); !errors.Is(err, context.Canceled) || r.got() != "" {
		t.Errorf("cancelled: err %v, calls %s", err, r.got())
	}
	defer func(w time.Duration) { holdLockWait = w }(holdLockWait)
	holdLockWait = 50 * time.Millisecond
	if _, err := r.v.HoldVolume(context.Background(), "cache", r.during(nil)); !errors.Is(err, vm.ErrLocked) || !strings.Contains(err.Error(), "start or stop") || r.got() != "" {
		t.Errorf("bounded: err %v, calls %s", err, r.got())
	}
}

// A context ended before the freeze: nothing frozen, no clone.
func TestHoldVolumeCancelled(t *testing.T) {
	r := newHoldRig(t, "cache")
	r.serve()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.v.HoldVolume(ctx, "cache", r.during(nil)); !errors.Is(err, context.Canceled) || r.got() != "" {
		t.Errorf("err %v, calls %s", err, r.got())
	}
}

// A checkpoint holds the checkpoint lock throughout, and a stop waiting
// for the start lock meanwhile says a checkpoint is what it waits for,
// not a resize.
func TestHoldVolumeSaysCheckpoint(t *testing.T) {
	r := newHoldRig(t, "cache")
	r.serve()
	var mu sync.Mutex
	var notes []string
	r.v.Note = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		notes = append(notes, fmt.Sprintf(format, args...))
	}
	done := make(chan struct{})
	_, err := r.v.HoldVolume(context.Background(), "cache", func() error {
		if !r.v.Dir.Checkpointing() {
			t.Error("the checkpoint lock was free during the clone")
		}
		go func() {
			defer close(done)
			unlock, err := r.v.holdStart()
			if err == nil {
				unlock()
			}
		}()
		time.Sleep(100 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(notes) != 1 || !strings.Contains(notes[0], "a checkpoint of one of the VM's volumes") {
		t.Errorf("notes %q", notes)
	}
	if r.v.Dir.Checkpointing() {
		t.Error("the checkpoint lock held after the hold")
	}
}

// orderHost is a VMHost that records when a start asks for a volume's
// disk, and never starts vmm.
type orderHost struct {
	stubHost
	asked chan string
}

func (h orderHost) Volume(name string) (VMDisk, error) {
	h.asked <- name
	return VMDisk{Path: "/d/" + name}, nil
}

// With no VM yet, a restore's or a checkpoint's hold takes the volumes'
// lock, and a first start (create, then start) waits for it before it
// asks for a disk: it never opens the disk being replaced or cloned.
func TestHoldsOrderFirstStart(t *testing.T) {
	for _, hold := range []string{"stopped", "volume"} {
		t.Run(hold, func(t *testing.T) {
			dir := t.TempDir()
			h := orderHost{stubHost{VMImage{ID: "i", Disk: "/d", User: "0:0", Cmd: []string{"/e"}}}, make(chan string, 4)}
			v := NewVM("box", vm.Dir(filepath.Join(dir, "vm", "box")), h)
			v.Stderr = nil
			v.VolumesLock = filepath.Join(dir, "vm", "volumes", "volumes.lock")
			release := make(chan struct{})
			created := make(chan error, 1)
			during := func() error {
				go func() {
					created <- v.Create(Spec{Image: "i", Volumes: []Volume{{Name: "cache", Target: "/c"}}})
				}()
				select {
				case n := <-h.asked:
					t.Errorf("the start asked for %s's disk during the hold", n)
				case <-time.After(200 * time.Millisecond):
				}
				close(release)
				return nil
			}
			var err error
			if hold == "stopped" {
				err = v.HoldStopped(during)
			} else {
				_, err = v.HoldVolume(context.Background(), "cache", during)
			}
			if err != nil {
				t.Fatal(err)
			}
			<-release
			select {
			case n := <-h.asked:
				if n != "cache" {
					t.Errorf("asked for %s", n)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the start never asked for the disk")
			}
			<-created
		})
	}
}

// HoldStopped refuses while the VM runs, and keeps it from starting
// while during runs.
func TestHoldStopped(t *testing.T) {
	r := newHoldRig(t, "cache")
	if err := r.v.HoldStopped(r.during(nil)); err != nil || r.got() != "clone" {
		t.Errorf("stopped: %v %s", err, r.got())
	}
	r = newHoldRig(t, "cache")
	r.serve()
	if err := r.v.HoldStopped(r.during(nil)); err == nil || !strings.Contains(err.Error(), "is running") || r.got() != "" {
		t.Errorf("running: %v %s", err, r.got())
	}
	v := NewVM("none", vm.Dir(filepath.Join(t.TempDir(), "none")), nil)
	ran := false
	if err := v.HoldStopped(func() error { ran = true; return nil }); err != nil || !ran {
		t.Errorf("no VM: %v %v", err, ran)
	}
}
