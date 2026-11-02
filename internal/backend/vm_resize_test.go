package backend

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/vm"
)

// resizeRig is a running VM, as far as a resize sees it: its record,
// machine.json, a socket that answers, caboose-vmm's disk requests and
// the guest's agent stood in for, every call recorded in order.
type resizeRig struct {
	t     *testing.T
	v     *VM
	path  string // the volume's disk
	calls []string
	says  []string

	capacity int64
	// resize answers a DiskResize; nil applies it and tells the guest.
	resize    func(size int64) (vm.DiskAnswer, error)
	steps     map[string]func(op agentproto.VolumeOp) (agentproto.VolumeInfo, error)
	cloneErr  error
	removeErr error
	// ctx is the resize's; cancel, when set, is called as the step named
	// by cancelAt begins (an interrupt during it).
	ctx      context.Context
	cancel   context.CancelFunc
	cancelAt string
}

func newResizeRig(t *testing.T, live bool) *resizeRig {
	t.Helper()
	dir, err := os.MkdirTemp("", "vr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	r := &resizeRig{t: t, path: filepath.Join(dir, "cache.img"), capacity: 64 << 30, steps: map[string]func(agentproto.VolumeOp) (agentproto.VolumeInfo, error){}, ctx: context.Background()}
	d := vm.Dir(filepath.Join(dir, "vm"))
	m := vm.Machine{Disks: []vm.Disk{{Path: "root"}, {Path: "scratch"}}}
	if live {
		m.BlockDevices = []vm.BlockDevice{{Serial: "cache"}}
	} else {
		m.Disks = append(m.Disks, vm.Disk{})
	}
	if err := d.WriteState(vm.State{Machine: m, Volumes: []string{"cache"}}); err != nil {
		t.Fatal(err)
	}
	if live {
		m.BlockDevices[0].Path = r.path
	} else {
		m.Disks[2].Path = r.path
	}
	if err := d.WriteMachine(m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.path, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.v = NewVM("vr", d, nil)
	r.v.resize = &resizeSeams{
		disks: r.disks,
		agent: func() (volumeAgent, error) { r.calls = append(r.calls, "dial"); return rigAgent{r}, nil },
		clone: func(src, dst string) error {
			r.calls = append(r.calls, "clone "+filepath.Base(src)+" "+filepath.Base(dst))
			r.interrupt("clone")
			if r.cloneErr == nil {
				return os.WriteFile(dst, nil, 0o600)
			}
			return r.cloneErr
		},
		remove: func(p string) error {
			r.calls = append(r.calls, "remove "+filepath.Base(p))
			if r.removeErr == nil {
				return os.Remove(p)
			}
			return r.removeErr
		},
	}
	// The VM's agent: mounted, a third used, every step done.
	r.steps[agentproto.VolumeUsage] = func(agentproto.VolumeOp) (agentproto.VolumeInfo, error) {
		return agentproto.VolumeInfo{Target: "/home/agent/.cache/x", Mounted: true, FSBytes: 63 << 30, UsedBytes: 20 << 30}, nil
	}
	return r
}

// serve makes the VM's socket answer, so that it counts as running.
func (r *resizeRig) serve() {
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

func (r *resizeRig) disks(req vm.DiskRequest, _ time.Duration) (vm.DiskAnswer, error) {
	if req.Serial != "cache" {
		return vm.DiskAnswer{Error: "no such disk"}, nil
	}
	switch req.Op {
	case vm.DiskSize:
		r.calls = append(r.calls, "size")
		return vm.DiskAnswer{Size: r.capacity}, nil
	case vm.DiskResize:
		r.calls = append(r.calls, fmt.Sprintf("resize %dG", req.Size>>30))
		r.interrupt("resize")
		if r.resize != nil {
			return r.resize(req.Size)
		}
		r.capacity = req.Size
		return vm.DiskAnswer{Size: req.Size, Applied: true, Told: true}, nil
	}
	return vm.DiskAnswer{}, errors.New("bad op")
}

type rigAgent struct{ r *resizeRig }

func (a rigAgent) Volume(op agentproto.VolumeOp, _ time.Duration) (agentproto.VolumeInfo, error) {
	call := "agent " + op.Op
	if op.Size != 0 {
		call += fmt.Sprintf(" %dG", op.Size>>30)
	}
	a.r.calls = append(a.r.calls, call)
	a.r.interrupt(op.Op)
	if op.Name != "cache" {
		a.r.t.Errorf("a step for %q", op.Name)
	}
	if f := a.r.steps[op.Op]; f != nil {
		info, err := f(op)
		info.Op = op.Op
		return info, err
	}
	return agentproto.VolumeInfo{Op: op.Op, Mounted: true, FSBytes: op.Size, UsedBytes: 20 << 30}, nil
}

func (a rigAgent) Close() error { return nil }

func (r *resizeRig) run(size int64) (VolumeResize, error) {
	r.t.Helper()
	return r.v.ResizeVolume(r.ctx, "cache", size, func(format string, args ...any) { r.says = append(r.says, fmt.Sprintf(format, args...)) })
}

// interruptAt has the resize interrupted as step begins: an agent step's
// name, "clone" or "resize".
func (r *resizeRig) interruptAt(step string) {
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.t.Cleanup(r.cancel)
	r.cancelAt = step
}

func (r *resizeRig) interrupt(step string) {
	if r.cancel != nil && step == r.cancelAt {
		r.cancel()
		// The resize sees it while the step runs.
		time.Sleep(20 * time.Millisecond)
	}
}

func (r *resizeRig) wantCalls(want ...string) {
	r.t.Helper()
	if !slices.Equal(r.calls, want) {
		r.t.Errorf("calls:\n  %s\nwant:\n  %s", strings.Join(r.calls, "\n  "), strings.Join(want, "\n  "))
	}
}

func wantErr(t *testing.T, err error, parts ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want one saying %q", parts)
	}
	t.Log(err)
	for _, p := range parts {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("error %q does not say %q", err, p)
		}
	}
}

func failStep(msg string) func(agentproto.VolumeOp) (agentproto.VolumeInfo, error) {
	return func(agentproto.VolumeOp) (agentproto.VolumeInfo, error) {
		return agentproto.VolumeInfo{Error: msg}, nil
	}
}

// A grow: caboose-vmm makes the disk longer, then the guest grows its
// filesystem, mounted.
func TestResizeGrow(t *testing.T) {
	r := newResizeRig(t, true)
	r.serve()
	res, err := r.run(96 << 30)
	if err != nil || !res.Applied || res.From != 64<<30 {
		t.Fatalf("%+v, %v", res, err)
	}
	r.wantCalls("size", "dial", "resize 96G", "agent grow 96G")
}

// A grow caboose-vmm refused changes nothing, and is not recorded; a
// file it could not put back is said.
func TestResizeGrowRefused(t *testing.T) {
	r := newResizeRig(t, true)
	r.serve()
	r.resize = func(int64) (vm.DiskAnswer, error) {
		return vm.DiskAnswer{Size: 64 << 30, FileSize: 96 << 30, Error: "the guest refused"}, nil
	}
	res, err := r.run(96 << 30)
	if res.Applied {
		t.Error("applied")
	}
	wantErr(t, err, "keeps its size, 64G", "the guest refused", "its file stays at 96G")
	r.wantCalls("size", "dial", "resize 96G")
}

// A grown disk whose filesystem did not grow is applied, and says how
// the filesystem catches up.
func TestResizeGrowFSFails(t *testing.T) {
	r := newResizeRig(t, true)
	r.serve()
	r.steps[agentproto.VolumeGrow] = failStep("EXT4_IOC_RESIZE_FS: no space")
	res, err := r.run(96 << 30)
	if !res.Applied {
		t.Error("not applied")
	}
	wantErr(t, err, "is 96G now", "did not grow", "run this again", "next start")
}

// caboose-vmm's answer lost: the disk's size says whether the resize
// took, and an unconfirmed one is said.
func TestResizeGrowAnswerLost(t *testing.T) {
	r := newResizeRig(t, true)
	r.serve()
	r.resize = func(size int64) (vm.DiskAnswer, error) {
		r.capacity = size
		return vm.DiskAnswer{}, errors.New("caboose-vmm did not answer in 1m0s")
	}
	res, err := r.run(96 << 30)
	if err != nil || !res.Applied {
		t.Fatalf("%+v, %v", res, err)
	}
	r.wantCalls("size", "dial", "resize 96G", "size", "agent grow 96G")
	if !slices.ContainsFunc(r.says, func(s string) bool { return strings.Contains(s, "did not confirm") }) {
		t.Errorf("says %q", r.says)
	}
}

// The same size again grows the filesystem only, catching a failed
// grow's second half up.
func TestResizeSameSize(t *testing.T) {
	r := newResizeRig(t, true)
	r.serve()
	res, err := r.run(64 << 30)
	if err != nil || !res.Applied {
		t.Fatalf("%+v, %v", res, err)
	}
	r.wantCalls("size", "dial", "agent grow 64G")
}

// A resize needs the VM running, the volume on caboose-vmm's disk, and
// no other resize of the VM.
func TestResizeRefusals(t *testing.T) {
	r := newResizeRig(t, true)
	_, err := r.run(96 << 30)
	wantErr(t, err, "is not running")

	r = newResizeRig(t, false)
	r.serve()
	_, err = r.run(96 << 30)
	wantErr(t, err, "not on one of caboose-vmm's disks", "macOS 27")
	r.wantCalls()

	r = newResizeRig(t, true)
	r.serve()
	unlock, err := vm.TryLock(r.v.Dir.ResizeLock())
	if err != nil {
		t.Fatal(err)
	}
	// flock is per open file: a second open in this process is refused
	// as another process's would be.
	_, err = r.run(96 << 30)
	unlock()
	wantErr(t, err, "another resize")
	r.wantCalls()
}

// A checkpoint holding the VM (its start lock and its checkpoint lock):
// the resize says it waits for it, then runs once it ends.
func TestResizeWaitsForCheckpoint(t *testing.T) {
	r := newResizeRig(t, true)
	r.serve()
	unstart, err := vm.TryLock(r.v.Dir.StartLock())
	if err != nil {
		t.Fatal(err)
	}
	uncp, err := vm.TryLock(r.v.Dir.CheckpointLock())
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { uncp(); unstart() }) }
	// Never a hang, whatever the resize says.
	timer := time.AfterFunc(5*time.Second, release)
	defer timer.Stop()
	var says []string
	res, err := r.v.ResizeVolume(r.ctx, "cache", 96<<30, func(format string, args ...any) {
		s := fmt.Sprintf(format, args...)
		says = append(says, s)
		if strings.Contains(s, "a checkpoint of one of the VM's volumes is being taken") {
			release()
		}
	})
	if err != nil || !res.Applied {
		t.Fatalf("%+v, %v", res, err)
	}
	if len(says) == 0 || !strings.Contains(says[0], "a checkpoint of one of the VM's volumes is being taken; waiting for it to end") {
		t.Errorf("says %q", says)
	}
}

// A shrink: the volume unmounted, its disk cloned, its filesystem
// checked and shrunk, the disk lowered, the volume mounted again, the
// clone deleted.
func TestResizeShrink(t *testing.T) {
	r := newResizeRig(t, true)
	r.serve()
	res, err := r.run(32 << 30)
	if err != nil || !res.Applied {
		t.Fatalf("%+v, %v", res, err)
	}
	r.wantCalls("size", "dial", "agent usage", "agent unmount", "clone cache.img cache.img.before-shrink",
		"agent shrinkfs 32G", "resize 32G", "agent mount 32G", "remove cache.img.before-shrink")
}

// Below what the volume holds, plus the margin, a shrink is refused
// before anything changes, saying the smallest size it takes.
func TestResizeShrinkBelowUsed(t *testing.T) {
	r := newResizeRig(t, true)
	r.serve()
	// 20G used: a margin of 2G.
	_, err := r.run(21 << 30)
	wantErr(t, err, "holds 20G", "leaves 2G", "down to 22G")
	r.wantCalls("size", "dial", "agent usage")
	// A small volume's margin is 1G.
	if got := ShrinkMargin(3 << 30); got != 1<<30 {
		t.Errorf("margin of 3G: %d", got)
	}
	r = newResizeRig(t, true)
	r.serve()
	if _, err := r.run(22 << 30); err != nil {
		t.Errorf("22G: %v", err)
	}
}

// A volume something holds is not unmounted: the holders are named, the
// guest's words made printable, and nothing changes.
func TestResizeShrinkHeld(t *testing.T) {
	r := newResizeRig(t, true)
	r.serve()
	r.steps[agentproto.VolumeUnmount] = func(agentproto.VolumeOp) (agentproto.VolumeInfo, error) {
		return agentproto.VolumeInfo{Error: "/home/agent/.cache/x is in use", Holders: []string{"412 (go): /home/agent/.cache/x/a", "77 (sh\x1b[2J): /home/agent/.cache/x"}, HoldersMore: 3}, nil
	}
	res, err := r.run(32 << 30)
	if res.Applied {
		t.Error("applied")
	}
	wantErr(t, err, "is in use", "nothing changed", "Stop what holds it", "412 (go): /home/agent/.cache/x/a", "and 3 more")
	if strings.Contains(err.Error(), "\x1b") {
		t.Errorf("an escape reached the error: %q", err)
	}
	r.wantCalls("size", "dial", "agent usage", "agent unmount")
}

// What the guest says of holders is bounded on the host too: at most
// MaxVolumeHolders of them, each cut to 200 bytes where a character
// starts.
func TestResizeHoldersBounded(t *testing.T) {
	var hs []string
	for i := range agentproto.MaxVolumeHolders + 2 {
		hs = append(hs, fmt.Sprintf("%d (sh): /x", 100+i))
	}
	got := holders(agentproto.VolumeInfo{Holders: hs})
	if n := strings.Count(got, "\n") + 1; n != agentproto.MaxVolumeHolders || strings.Contains(got, "110 (sh)") {
		t.Errorf("%d lines:\n%s", n, got)
	}
	long := "1 (sh): /" + strings.Repeat("é", 200)
	got = holders(agentproto.VolumeInfo{Holders: []string{long}})
	if !utf8.ValidString(got) || len(got) > 2+200+3 || !strings.HasSuffix(got, "...") {
		t.Errorf("%d bytes, valid %v: %q", len(got), utf8.ValidString(got), got)
	}
	if g := guestText(strings.Repeat("€", agentproto.MaxVolumeText)); !utf8.ValidString(g) || len(g) > agentproto.MaxVolumeText+3 {
		t.Errorf("guestText: %d bytes, valid %v", len(g), utf8.ValidString(g))
	}
}

// An unmount that fails with no holder named changes nothing either.
func TestResizeShrinkUnmountFails(t *testing.T) {
	r := newResizeRig(t, true)
	r.serve()
	r.steps[agentproto.VolumeUnmount] = func(agentproto.VolumeOp) (agentproto.VolumeInfo, error) {
		return agentproto.VolumeInfo{Error: "unmounting /x: it is busy", Mounted: true}, nil
	}
	_, err := r.run(32 << 30)
	wantErr(t, err, "it is busy", "nothing changed")
	r.wantCalls("size", "dial", "agent usage", "agent unmount")

	// An answer that has it unmounted (still in use elsewhere, and not
	// mounted again) says so, and what mounts it.
	r = newResizeRig(t, true)
	r.serve()
	r.steps[agentproto.VolumeUnmount] = failStep("/x is unmounted here, but its filesystem is still in use; mounting it again here failed")
	_, err = r.run(32 << 30)
	wantErr(t, err, "still in use", "nothing was shrunk", "not mounted", "caboose restart")
	if strings.Contains(err.Error(), "nothing changed") {
		t.Errorf("says nothing changed: %v", err)
	}
	r.wantCalls("size", "dial", "agent usage", "agent unmount")
}

// An unmount the agent does not answer in time is not taken as nothing
// changed: the agent is asked how the volume is, and one it unmounted is
// mounted again.
func TestResizeShrinkUnmountLost(t *testing.T) {
	lost := func(agentproto.VolumeOp) (agentproto.VolumeInfo, error) {
		return agentproto.VolumeInfo{}, vm.ErrNoVolumeAnswer
	}
	usage := func(mounted bool, fail string) func(agentproto.VolumeOp) (agentproto.VolumeInfo, error) {
		n := 0
		return func(agentproto.VolumeOp) (agentproto.VolumeInfo, error) {
			n++
			if n > 1 && fail != "" {
				return agentproto.VolumeInfo{Error: fail}, nil
			}
			return agentproto.VolumeInfo{Target: "/x", Mounted: n == 1 || mounted, UsedBytes: 20 << 30}, nil
		}
	}
	for _, tc := range []struct {
		name  string
		usage func(agentproto.VolumeOp) (agentproto.VolumeInfo, error)
		say   []string
		calls []string
	}{
		{"still mounted", usage(true, ""), []string{"did not answer", "still mounted", "nothing changed"}, nil},
		{"unmounted", usage(false, ""), []string{"did not answer", "was unmounted", "mounted again"}, []string{"agent mount 64G"}},
		{"still busy", usage(true, "another step of a volume's resize is running in the guest"), []string{"may still be unmounting", "another step", "caboose restart"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newResizeRig(t, true)
			r.serve()
			r.steps[agentproto.VolumeUnmount] = lost
			r.steps[agentproto.VolumeUsage] = tc.usage
			_, err := r.run(32 << 30)
			wantErr(t, err, tc.say...)
			r.wantCalls(append([]string{"size", "dial", "agent usage", "agent unmount", "agent usage"}, tc.calls...)...)
		})
	}
}

// An image without e2fsck or resize2fs is refused before anything
// changes, saying what to install.
func TestResizeShrinkNoTools(t *testing.T) {
	r := newResizeRig(t, true)
	r.serve()
	r.steps[agentproto.VolumeUsage] = func(agentproto.VolumeOp) (agentproto.VolumeInfo, error) {
		return agentproto.VolumeInfo{Target: "/x", Mounted: true, UsedBytes: 1 << 30, Missing: "resize2fs"}, nil
	}
	_, err := r.run(32 << 30)
	wantErr(t, err, "has no resize2fs", "e2fsprogs", "nothing changed")
	r.wantCalls("size", "dial", "agent usage")
}

// A failed backup mounts the volume again, at the disk's size, and
// shrinks nothing.
func TestResizeShrinkCloneFails(t *testing.T) {
	r := newResizeRig(t, true)
	r.serve()
	r.cloneErr = errors.New("no space left")
	res, err := r.run(32 << 30)
	if res.Applied {
		t.Error("applied")
	}
	wantErr(t, err, "backing volume cache's disk up", "no space left", "was not shrunk", "mounted again")
	r.wantCalls("size", "dial", "agent usage", "agent unmount", "clone cache.img cache.img.before-shrink", "agent mount 64G")
}

// A filesystem step that fails mounts the volume again at the disk's
// size and keeps the backup, saying how to restore it.
func TestResizeShrinkFSFails(t *testing.T) {
	for _, why := range []string{"e2fsck -f -p /dev/vdd exited 4", "resize2fs /dev/vdd 33554432K exited 1"} {
		r := newResizeRig(t, true)
		r.serve()
		r.steps[agentproto.VolumeShrinkFS] = failStep(why)
		res, err := r.run(32 << 30)
		if res.Applied {
			t.Error("applied")
		}
		wantErr(t, err, why, "keeps its size, 64G", "mounted again", "delete the backup", "caboose stop", "cache.img.before-shrink over")
		r.wantCalls("size", "dial", "agent usage", "agent unmount", "clone cache.img cache.img.before-shrink", "agent shrinkfs 32G", "agent mount 64G")
		if _, err := os.Stat(r.path + BackupSuffix); err != nil {
			t.Errorf("the backup: %v", err)
		}
	}
}

// A filesystem step the agent does not answer in time may still run: no
// mount is tried, and the message says to wait, then restart.
func TestResizeShrinkFSTimeout(t *testing.T) {
	r := newResizeRig(t, true)
	r.serve()
	r.steps[agentproto.VolumeShrinkFS] = func(agentproto.VolumeOp) (agentproto.VolumeInfo, error) {
		return agentproto.VolumeInfo{}, vm.ErrNoVolumeAnswer
	}
	_, err := r.run(32 << 30)
	wantErr(t, err, "may still run", "not mounted again", "backup stays", "console log", "shrinkfs: done", "caboose restart", "before-shrink over")
	r.wantCalls("size", "dial", "agent usage", "agent unmount", "clone cache.img cache.img.before-shrink", "agent shrinkfs 32G")
}

// A filesystem step that may have left the filesystem half shrunk (a
// tool's end by a signal, a failed resize2fs) is not mounted again: the
// backup stays, and the message says how to restore it.
func TestResizeShrinkFSDirty(t *testing.T) {
	r := newResizeRig(t, true)
	r.serve()
	r.steps[agentproto.VolumeShrinkFS] = func(agentproto.VolumeOp) (agentproto.VolumeInfo, error) {
		return agentproto.VolumeInfo{Error: "resize2fs /dev/vdd 33554432K exited 1", Dirty: true}, nil
	}
	_, err := r.run(32 << 30)
	wantErr(t, err, "exited 1", "half shrunk", "not mounted again", "caboose stop", "before-shrink over")
	r.wantCalls("size", "dial", "agent usage", "agent unmount", "clone cache.img cache.img.before-shrink", "agent shrinkfs 32G")
	if _, err := os.Stat(r.path + BackupSuffix); err != nil {
		t.Errorf("the backup: %v", err)
	}
}

// An interrupt before the unmount ends the resize there, nothing
// changed; after, the step under way ends, the rest is not done, and the
// volume is mounted again, at the disk's size.
func TestResizeShrinkInterrupted(t *testing.T) {
	pre := []string{"size", "dial", "agent usage"}
	for _, tc := range []struct {
		at      string
		calls   []string
		applied bool
		err     []string
	}{
		{"usage", nil, false, []string{"interrupted; nothing changed"}},
		{"unmount", []string{"agent unmount", "agent mount 64G"}, false, []string{"interrupted before anything was shrunk", "mounted again"}},
		{"clone", []string{"agent unmount", "clone cache.img cache.img.before-shrink", "agent mount 64G", "remove cache.img.before-shrink"}, false,
			[]string{"interrupted before anything was shrunk", "mounted again"}},
		{"shrinkfs", []string{"agent unmount", "clone cache.img cache.img.before-shrink", "agent shrinkfs 32G", "agent mount 64G", "remove cache.img.before-shrink"}, false,
			[]string{"interrupted once its filesystem was shrunk", "keeps 64G", "mounted again"}},
		{"resize", []string{"agent unmount", "clone cache.img cache.img.before-shrink", "agent shrinkfs 32G", "resize 32G", "agent mount 32G", "remove cache.img.before-shrink"}, true, nil},
	} {
		t.Run(tc.at, func(t *testing.T) {
			r := newResizeRig(t, true)
			r.serve()
			r.interruptAt(tc.at)
			res, err := r.run(32 << 30)
			if res.Applied != tc.applied {
				t.Errorf("applied %v", res.Applied)
			}
			if tc.err == nil && err != nil {
				t.Errorf("%v", err)
			} else if tc.err != nil {
				wantErr(t, err, tc.err...)
			}
			r.wantCalls(append(append([]string{}, pre...), tc.calls...)...)
			says := strings.Join(r.says, "\n")
			if tc.at != "usage" && strings.Count(says, "interrupted:") != 1 {
				t.Errorf("says %q", says)
			}
		})
	}
	// What leaving during the filesystem's step would leave is said as the
	// interrupt lands.
	r := newResizeRig(t, true)
	r.serve()
	r.interruptAt("shrinkfs")
	_, _ = r.run(32 << 30)
	if says := strings.Join(r.says, "\n"); !strings.Contains(says, "interrupting again leaves at once") || !strings.Contains(says, "shrinkfs: done") {
		t.Errorf("says %q", says)
	}
	// Interrupted before it started: nothing at all.
	r = newResizeRig(t, true)
	r.serve()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.ctx = ctx
	_, err := r.run(32 << 30)
	wantErr(t, err, "interrupted; nothing changed")
	r.wantCalls("size", "dial")
}

// A disk caboose-vmm did not lower keeps its size: the volume is mounted
// again, its filesystem grown back to fill it, and the backup goes, as
// the filesystem's steps went through.
func TestResizeShrinkCapacityFails(t *testing.T) {
	r := newResizeRig(t, true)
	r.serve()
	r.resize = func(int64) (vm.DiskAnswer, error) {
		return vm.DiskAnswer{Size: 64 << 30, Error: "the requests in flight did not finish in 10s"}, nil
	}
	res, err := r.run(32 << 30)
	if res.Applied {
		t.Error("applied")
	}
	wantErr(t, err, "did not lower", "did not finish", "keeps 64G", "grown back", "mounted again")
	r.wantCalls("size", "dial", "agent usage", "agent unmount", "clone cache.img cache.img.before-shrink",
		"agent shrinkfs 32G", "resize 32G", "agent mount 64G", "remove cache.img.before-shrink")

	// Not mounted again, the backup stays.
	r = newResizeRig(t, true)
	r.serve()
	r.resize = func(int64) (vm.DiskAnswer, error) {
		return vm.DiskAnswer{Size: 64 << 30, Error: "busy"}, nil
	}
	r.steps[agentproto.VolumeMount] = failStep("mounting /dev/vdd: invalid argument")
	_, err = r.run(32 << 30)
	wantErr(t, err, "did not lower", "mounting it again failed", "backup stays")
	r.wantCalls("size", "dial", "agent usage", "agent unmount", "clone cache.img cache.img.before-shrink",
		"agent shrinkfs 32G", "resize 32G", "agent mount 64G")
	if _, err := os.Stat(r.path + BackupSuffix); err != nil {
		t.Errorf("the backup: %v", err)
	}
}

// A lowered disk whose volume does not mount again is applied, keeps the
// backup, and says to restart.
func TestResizeShrinkRemountFails(t *testing.T) {
	r := newResizeRig(t, true)
	r.serve()
	r.steps[agentproto.VolumeMount] = failStep("mounting /dev/vdd at /x: invalid argument")
	res, err := r.run(32 << 30)
	if !res.Applied {
		t.Error("not applied")
	}
	wantErr(t, err, "are 32G now", "mounting it again failed", "caboose restart", "backup stays")
	r.wantCalls("size", "dial", "agent usage", "agent unmount", "clone cache.img cache.img.before-shrink",
		"agent shrinkfs 32G", "resize 32G", "agent mount 32G")
}

// An earlier shrink's backup is never overwritten; an unmounted volume's
// use cannot be told, and neither shrinks.
func TestResizeShrinkPreconditions(t *testing.T) {
	r := newResizeRig(t, true)
	r.serve()
	if err := os.WriteFile(r.path+BackupSuffix, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := r.run(32 << 30)
	wantErr(t, err, "earlier shrink's backup", "delete it")
	r.wantCalls("size", "dial", "agent usage")

	r = newResizeRig(t, true)
	r.serve()
	r.steps[agentproto.VolumeUsage] = func(agentproto.VolumeOp) (agentproto.VolumeInfo, error) {
		return agentproto.VolumeInfo{Target: "/x"}, nil
	}
	_, err = r.run(32 << 30)
	wantErr(t, err, "not mounted", "caboose restart")
	r.wantCalls("size", "dial", "agent usage")
}

// A shrink's disk notes: an unconfirmed size and a file left longer.
func TestResizeShrinkFileLeft(t *testing.T) {
	r := newResizeRig(t, true)
	r.serve()
	r.resize = func(size int64) (vm.DiskAnswer, error) {
		return vm.DiskAnswer{Size: size, Applied: true, FileSize: 64 << 30, FileError: "truncate: busy", Error: "x"}, nil
	}
	res, err := r.run(32 << 30)
	if err != nil || !res.Applied {
		t.Fatalf("%+v, %v", res, err)
	}
	says := strings.Join(r.says, "\n")
	for _, w := range []string{"did not confirm", "file stays at 64G, not 32G", "truncate: busy"} {
		if !strings.Contains(says, w) {
			t.Errorf("says %q, not %q", says, w)
		}
	}
}

// VolumeState tells the launcher how the VM has a volume.
func TestVolumeState(t *testing.T) {
	r := newResizeRig(t, true)
	vs, err := r.v.VolumeState("cache")
	if err != nil || vs != (VolumeState{Has: true, Live: true}) {
		t.Errorf("stopped: %+v, %v", vs, err)
	}
	r.serve()
	if vs, _ := r.v.VolumeState("cache"); vs != (VolumeState{Running: true, Has: true, Live: true}) {
		t.Errorf("running: %+v", vs)
	}
	if vs, _ := r.v.VolumeState("other"); vs != (VolumeState{Running: true}) {
		t.Errorf("another: %+v", vs)
	}
	if vs, err := NewVM("x", vm.Dir(t.TempDir()+"/none"), nil).VolumeState("cache"); err != nil || vs != (VolumeState{}) {
		t.Errorf("no VM: %+v, %v", vs, err)
	}
}

// A stop that finds the start lock held by a volume's resize says it is
// waiting for it, then stops once the resize lets go; one held by
// something else waits without that word.
func TestStopWaitsForResize(t *testing.T) {
	for _, resizing := range []bool{true, false} {
		r := newResizeRig(t, true)
		var notes []string
		noted := make(chan struct{}, 1)
		r.v.Note = func(format string, args ...any) {
			notes = append(notes, fmt.Sprintf(format, args...))
			noted <- struct{}{}
		}
		unstart, err := vm.HoldLock(r.v.Dir.StartLock())
		if err != nil {
			t.Fatal(err)
		}
		if resizing {
			unresize, err := vm.TryLock(r.v.Dir.ResizeLock())
			if err != nil {
				t.Fatal(err)
			}
			defer unresize()
		}
		done := make(chan error, 1)
		go func() { done <- r.v.Stop() }()
		if resizing {
			select {
			case <-noted:
			case <-time.After(5 * time.Second):
				t.Fatal("no word of the resize")
			}
		} else {
			time.Sleep(100 * time.Millisecond)
		}
		select {
		case err := <-done:
			t.Fatalf("stopped while the lock was held: %v", err)
		default:
		}
		unstart()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if resizing != (len(notes) == 1) || (resizing && !strings.Contains(notes[0], "a resize of one of the VM's volumes is running; waiting")) {
			t.Errorf("resizing %v: notes %q", resizing, notes)
		}
	}
}
