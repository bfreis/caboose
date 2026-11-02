package agent

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/bfreis/caboose/internal/agentproto"
)

// fsCalls records a freezer's syncfs calls, FIFREEZEs and FITHAWs, and
// can fail or slow them.
type fsCalls struct {
	mu        sync.Mutex
	calls     []string
	frozen    map[string]bool
	syncErr   error
	freezeErr error
	thawErr   error
	// slow, when set, holds the freeze until it is closed.
	slow chan struct{}
}

func newFSCalls() *fsCalls { return &fsCalls{frozen: map[string]bool{}} }

func (f *fsCalls) freezer(max time.Duration) *freezer {
	return newFreezer(max, func(t string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, "sync "+t)
		return f.syncErr
	}, func(t string) error {
		if f.slow != nil {
			<-f.slow
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, "freeze "+t)
		if f.freezeErr != nil {
			return f.freezeErr
		}
		f.frozen[t] = true
		return nil
	}, func(t string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, "thaw "+t)
		if f.thawErr != nil {
			return f.thawErr
		}
		if !f.frozen[t] {
			return unix.EINVAL
		}
		delete(f.frozen, t)
		return nil
	})
}

func (f *fsCalls) state() ([]string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...), len(f.frozen)
}

// waitThawed waits for nothing to be frozen.
func (f *fsCalls) waitThawed(t *testing.T, within time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(within); ; time.Sleep(5 * time.Millisecond) {
		if _, n := f.state(); n == 0 {
			return
		}
		if time.Now().After(deadline) {
			calls, _ := f.state()
			t.Fatalf("still frozen after %v: %q", within, calls)
		}
	}
}

// A freeze, then its thaw: each once, the thaw not expired.
func TestFreezeThaw(t *testing.T) {
	f := newFSCalls()
	z := f.freezer(time.Hour)
	if err := z.Freeze("/v", "t1"); err != nil {
		t.Fatal(err)
	}
	// One freeze of a target at a time.
	if err := z.Freeze("/v", "t2"); err == nil || !strings.Contains(err.Error(), "frozen already") {
		t.Errorf("a second freeze: %v", err)
	}
	// A thaw with another token leaves it frozen.
	if exp, _, err := z.Thaw("/v", "t2"); !exp || err != nil {
		t.Errorf("another token's thaw: %v %v", exp, err)
	}
	if _, n := f.state(); n != 1 {
		t.Fatal("another token's thaw thawed it")
	}
	exp, why, err := z.Thaw("/v", "t1")
	if exp || why != "" || err != nil {
		t.Errorf("thaw: %v %q %v", exp, why, err)
	}
	calls, n := f.state()
	if n != 0 || strings.Join(calls, ",") != "sync /v,freeze /v,thaw /v" {
		t.Errorf("calls %q, frozen %d", calls, n)
	}
	// Thawed already: a second thaw is expired, and calls nothing.
	if exp, _, _ := z.Thaw("/v", "t1"); !exp {
		t.Error("a second thaw")
	}
	if calls, _ := f.state(); len(calls) != 3 {
		t.Errorf("calls %q", calls)
	}
	if err := z.Freeze("/v", ""); err == nil {
		t.Error("an empty token")
	}
	if err := z.Freeze("/v", strings.Repeat("x", agentproto.MaxVolumeToken+1)); err == nil {
		t.Error("a long token")
	}
}

// With no thaw from the host, the agent thaws it itself after max, and
// the host's late thaw hears it expired, and why.
func TestFreezeExpires(t *testing.T) {
	f := newFSCalls()
	z := f.freezer(50 * time.Millisecond)
	if err := z.Freeze("/v", "t1"); err != nil {
		t.Fatal(err)
	}
	f.waitThawed(t, 5*time.Second)
	exp, why, err := z.Thaw("/v", "t1")
	if !exp || err != nil || !strings.Contains(why, "thawed it itself after 50ms") {
		t.Errorf("late thaw: %v %q %v", exp, why, err)
	}
	// The target can be frozen again.
	if err := z.Freeze("/v", "t2"); err != nil {
		t.Errorf("freeze again: %v", err)
	}
	if exp, _, _ := z.Thaw("/v", "t2"); exp {
		t.Error("the second freeze expired")
	}
}

// Something in the guest thawed it (FITHAW says EINVAL): expired.
func TestThawFoundThawed(t *testing.T) {
	f := newFSCalls()
	z := f.freezer(time.Hour)
	if err := z.Freeze("/v", "t1"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	delete(f.frozen, "/v")
	f.mu.Unlock()
	exp, why, err := z.Thaw("/v", "t1")
	if !exp || err != nil || !strings.Contains(why, "something in the guest thawed it") {
		t.Errorf("%v %q %v", exp, why, err)
	}
	// Another FITHAW error is the thaw's error.
	f2 := newFSCalls()
	z = f2.freezer(time.Hour)
	if err := z.Freeze("/v", "t1"); err != nil {
		t.Fatal(err)
	}
	f2.thawErr = unix.EIO
	if _, _, err := z.Thaw("/v", "t1"); !errors.Is(err, unix.EIO) {
		t.Errorf("FITHAW EIO: %v", err)
	}
}

// A thaw while the freeze still runs (the host gave up waiting for it):
// the freeze is undone as soon as it returns, and fails.
func TestThawDuringFreeze(t *testing.T) {
	f := newFSCalls()
	f.slow = make(chan struct{})
	z := f.freezer(time.Hour)
	done := make(chan error, 1)
	go func() { done <- z.Freeze("/v", "t1") }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		z.mu.Lock()
		pending := z.frozen["/v"] != nil
		z.mu.Unlock()
		if pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the freeze never started")
		}
	}
	exp, why, err := z.Thaw("/v", "t1")
	if !exp || err != nil || !strings.Contains(why, "had not ended") {
		t.Errorf("thaw: %v %q %v", exp, why, err)
	}
	close(f.slow)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "gave up") {
		t.Errorf("freeze: %v", err)
	}
	calls, n := f.state()
	if n != 0 || strings.Join(calls, ",") != "sync /v,freeze /v,thaw /v" {
		t.Errorf("calls %q, frozen %d", calls, n)
	}
}

// A failed freeze leaves nothing recorded; EBUSY is something else's
// freeze.
func TestFreezeFails(t *testing.T) {
	f := newFSCalls()
	f.freezeErr = unix.EBUSY
	z := f.freezer(time.Hour)
	if err := z.Freeze("/v", "t1"); err == nil || !strings.Contains(err.Error(), "by something in the guest") {
		t.Errorf("EBUSY: %v", err)
	}
	f.freezeErr = nil
	if err := z.Freeze("/v", "t1"); err != nil {
		t.Errorf("after a failed freeze: %v", err)
	}
}

// The filesystem is synced before it is frozen, so that FIFREEZE, which
// holds every writer, has little left to flush; a sync that fails still
// lets the freeze go on, which then flushes everything itself.
func TestFreezeSyncsFirst(t *testing.T) {
	f := newFSCalls()
	z := f.freezer(time.Hour)
	if err := z.Freeze("/v", "t1"); err != nil {
		t.Fatal(err)
	}
	if calls, _ := f.state(); strings.Join(calls, ",") != "sync /v,freeze /v" {
		t.Errorf("calls %q", calls)
	}
	if _, _, err := z.Thaw("/v", "t1"); err != nil {
		t.Fatal(err)
	}
	f = newFSCalls()
	f.syncErr = unix.EIO
	z = f.freezer(time.Hour)
	var logged []string
	z.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	if err := z.Freeze("/v", "t1"); err != nil {
		t.Fatalf("a freeze after a failed sync: %v", err)
	}
	if _, n := f.state(); n != 1 || len(logged) != 1 || !strings.Contains(logged[0], "syncfs /v") {
		t.Errorf("frozen %d, logged %q", n, logged)
	}
}

// ThawAll thaws every frozen filesystem, as a shutdown does.
func TestThawAll(t *testing.T) {
	f := newFSCalls()
	z := f.freezer(time.Hour)
	for i, v := range []string{"/a", "/b"} {
		if err := z.Freeze(v, fmt.Sprint("t", i)); err != nil {
			t.Fatal(err)
		}
	}
	z.ThawAll()
	if _, n := f.state(); n != 0 {
		t.Error("left frozen")
	}
	if exp, why, _ := z.Thaw("/a", "t0"); !exp || !strings.Contains(why, "shutting down") {
		t.Errorf("%v %q", exp, why)
	}
}

// A shutdown's first step thaws what the machine's freezer holds frozen.
func TestShutdownThaws(t *testing.T) {
	f := newFSCalls()
	m := &machine{logf: t.Logf, done: make(chan struct{}), freezer: f.freezer(time.Hour)}
	if err := m.freezer.Freeze("/v", "t1"); err != nil {
		t.Fatal(err)
	}
	m.stopping()
	if _, n := f.state(); n != 0 {
		t.Error("a shutdown left a volume frozen")
	}
	select {
	case <-m.done:
	default:
		t.Error("done not closed")
	}
}

// The freezer remembers how the last maxEnded freezes ended, no more.
func TestFreezerRemembersFew(t *testing.T) {
	z := newFreezer(time.Hour, nil, nil, nil)
	for i := range maxEnded + 5 {
		z.remember(fmt.Sprint(i), "x")
	}
	if len(z.ended) != maxEnded || len(z.order) != maxEnded {
		t.Errorf("%d, %d", len(z.ended), len(z.order))
	}
	if _, ok := z.ended["0"]; ok {
		t.Error("kept the oldest")
	}
}

// The steps: a freeze finds the volume on caboose-vmm's disk by name and
// target, or on the framework's by target; it must be mounted, with its
// own filesystem there. A thaw needs nothing but the freezer.
func TestVolumeFreezeStep(t *testing.T) {
	g := newVolGuest(t)
	f := newFSCalls()
	z := f.freezer(time.Hour)
	same := ""
	ops := g.ops()
	ops.freezer = z
	ops.sameFS = func(dev, target string) error {
		same = dev + " " + target
		return nil
	}
	disks := []agentproto.GuestDisk{{Device: "/dev/vdc", Target: "/var/lib/docker"}, {Serial: "cache", Target: g.target}}
	op := agentproto.VolumeOp{Op: agentproto.VolumeFreeze, Name: "cache", Target: g.target, Token: "t1"}
	if info := ops.step(disks, op); info.Error != "" {
		t.Fatalf("%+v", info)
	}
	if same != "/dev/vdd "+g.target {
		t.Errorf("checked %q", same)
	}
	op.Op = agentproto.VolumeThaw
	if info := ops.step(disks, op); info.Error != "" || info.Expired {
		t.Fatalf("thaw: %+v", info)
	}
	// The wrong target for the name: refused.
	bad := agentproto.VolumeOp{Op: agentproto.VolumeFreeze, Name: "cache", Target: "/elsewhere", Token: "t2"}
	if info := ops.step(disks, bad); !strings.Contains(info.Error, `no volume "cache" at "/elsewhere"`) {
		t.Errorf("wrong target: %+v", info)
	}
	// A volume on the framework's disk, found by its target.
	fw := []agentproto.GuestDisk{{Device: "/dev/vdd", Target: g.target}}
	op = agentproto.VolumeOp{Op: agentproto.VolumeFreeze, Name: "cache", Target: g.target, Token: "t3"}
	if info := ops.step(fw, op); info.Error != "" {
		t.Fatalf("framework disk: %+v", info)
	}
	// A thaw needs neither the device nor the mount table.
	ops.mounts = func() ([]string, error) { return nil, errors.New("no mount table") }
	op.Op = agentproto.VolumeThaw
	if info := ops.step(fw, op); info.Error != "" || info.Expired {
		t.Fatalf("thaw, framework disk: %+v", info)
	}
	ops = g.ops()
	ops.freezer = z
	ops.sameFS = func(dev, target string) error { return errors.New("not the volume's") }
	op.Op, op.Token = agentproto.VolumeFreeze, "t4"
	if info := ops.step(fw, op); !strings.Contains(info.Error, "not the volume's") {
		t.Errorf("another filesystem: %+v", info)
	}
	g.mounted = []string{"/"}
	ops.sameFS = func(string, string) error { return nil }
	if info := ops.step(fw, op); !strings.Contains(info.Error, "is not mounted") {
		t.Errorf("unmounted: %+v", info)
	}
	if _, n := f.state(); n != 0 {
		t.Error("left frozen")
	}
	// A resize's step still finds only caboose-vmm's disks.
	if info := ops.step(fw, agentproto.VolumeOp{Op: agentproto.VolumeUsage, Name: "cache", Target: g.target}); !strings.Contains(info.Error, "on caboose-vmm's disks") {
		t.Errorf("usage on the framework's disk: %+v", info)
	}
}

// The real ioctls' checks: sameFS refuses a target whose filesystem is
// not the device's.
func TestSameFS(t *testing.T) {
	if err := sameFS("/dev/null", t.TempDir()); err == nil {
		t.Error("a character device")
	}
	if err := sameFS("/no/such/dev", t.TempDir()); err == nil {
		t.Error("a missing device")
	}
	blk := unix.Stat_t{Mode: unix.S_IFBLK | 0o600, Rdev: 0xfd01}
	chr := unix.Stat_t{Mode: unix.S_IFCHR | 0o600, Rdev: 0xfd01}
	on := unix.Stat_t{Dev: 0xfd01}
	off := unix.Stat_t{Dev: 0xfd02}
	if !onDevice(&blk, &on) || onDevice(&blk, &off) || onDevice(&chr, &on) {
		t.Error("onDevice")
	}
	if err := fifreeze("/no/such/dir"); err == nil {
		t.Error("froze a missing directory")
	}
}

// freezeGuest is a guest whose volume steps go to a freezer.
type freezeGuest struct {
	fakeGuest
	z *freezer
}

func (g *freezeGuest) Volume(op agentproto.VolumeOp) agentproto.VolumeInfo {
	info := agentproto.VolumeInfo{Op: op.Op, Target: op.Target}
	switch op.Op {
	case agentproto.VolumeFreeze:
		if err := g.z.Freeze(op.Target, op.Token); err != nil {
			info.Error = err.Error()
		}
	case agentproto.VolumeThaw:
		var err error
		info.Expired, info.Note, err = g.z.Thaw(op.Target, op.Token)
		if err != nil {
			info.Error = err.Error()
		}
	}
	return info
}

// A connection that froze a volume and ended (the host went away) has it
// thawed at once; a freeze and thaw run even while a resize's step holds
// the guest's turn.
func TestControlThawsOnHangUp(t *testing.T) {
	f := newFSCalls()
	g := &freezeGuest{z: f.freezer(time.Hour)}
	c := &ControlServer{Guest: g, Clock: &fakeClock{}}
	c.volume.Lock() // a resize's step running
	defer c.volume.Unlock()
	host, done := controlConn(t, c)
	hello(t, host)
	op := agentproto.VolumeOp{Op: agentproto.VolumeFreeze, Name: "cache", Target: "/v", Token: "t1"}
	send(t, host, agentproto.Message{Type: agentproto.TypeVolume, VolumeOp: &op})
	if m := recv(t, host); m.VolumeInfo == nil || m.VolumeInfo.Error != "" {
		t.Fatalf("freeze: %+v", m)
	}
	if _, n := f.state(); n != 1 {
		t.Fatal("not frozen")
	}
	host.Close()
	<-done
	f.waitThawed(t, 5*time.Second)

	// One that thawed before ending thaws nothing more.
	f = newFSCalls()
	g.z = f.freezer(time.Hour)
	host, done = controlConn(t, c)
	hello(t, host)
	send(t, host, agentproto.Message{Type: agentproto.TypeVolume, VolumeOp: &op})
	recv(t, host)
	thaw := op
	thaw.Op = agentproto.VolumeThaw
	send(t, host, agentproto.Message{Type: agentproto.TypeVolume, VolumeOp: &thaw})
	if m := recv(t, host); m.VolumeInfo == nil || m.VolumeInfo.Expired || m.VolumeInfo.Error != "" {
		t.Fatalf("thaw: %+v", m)
	}
	host.Close()
	<-done
	if calls, _ := f.state(); len(calls) != 3 {
		t.Errorf("calls %q", calls)
	}
}
