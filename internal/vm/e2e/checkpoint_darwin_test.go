package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/vm"
)

// vmHolder is a volume's checkpoint.Holder on the e2e VM, as the
// launcher's is on the sandbox's.
type vmHolder struct {
	v    *backend.VM
	name string
}

func (h vmHolder) Hold(ctx context.Context, during func() error) (checkpoint.Held, error) {
	held, err := h.v.HoldVolume(ctx, h.name, during)
	return checkpoint.Held{Frozen: held.Frozen, FrozenFor: held.FrozenFor}, err
}

// TestVolumeCheckpoint boots a VM with two volumes, one on caboose-vmm's
// device (unless CABOOSE_VM_E2E_LIVE=0) and one on the framework's own
// disk, and a writer on each; takes a checkpoint of each through the
// checkpoint store, as the daemon does (freeze, clone, thaw); has the
// guest check each clone, through a share, with e2fsck -fn and read its
// log with debugfs, which must end between what the writer had written
// just before and just after; does the same for a clone taken without a
// freeze, for comparison; then stops the VM, restores the checkpoints in
// place and boots again: the volumes hold the logs as the checkpoints
// had them. The work directory is in DIR (APFS, for the clones).
func TestVolumeCheckpoint(t *testing.T) {
	dir := os.Getenv("CABOOSE_VM_E2E")
	if dir == "" {
		t.Skip("CABOOSE_VM_E2E names no directory of VM files")
	}
	work, err := os.MkdirTemp(dir, "cvc")
	if err != nil {
		t.Fatal(err)
	}
	share := filepath.Join(work, "share")
	home := filepath.Join(work, "home")
	env := config.EnvDirFor(home, "default")
	for _, d := range []string{share, env} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h := host{dir: dir, work: work, volSize: 4 << 30}
	vmdir := vm.Dir(filepath.Join(work, "vm"))
	v := backend.NewVM("e2e-ckpt", vmdir, h)
	v.Self, _ = os.Executable()
	v.Ready = ready
	defer func() {
		for _, f := range []string{vmdir.Log(), vmdir.Console()} {
			if b, err := os.ReadFile(f); err == nil {
				_ = os.WriteFile(filepath.Join(dir, "ckpt-"+filepath.Base(f)), b, 0o644)
			}
		}
		_ = v.Remove()
		os.RemoveAll(work)
	}()
	start := time.Now()
	out := func(script string) string {
		t.Helper()
		s, err := backend.Output(v, "sh", "-c", script+" 2>&1")
		if err != nil {
			t.Errorf("%s: %v\n%s", script, err, s)
		}
		return strings.TrimSpace(s)
	}
	vols := map[string]string{"live": "/vol/live", "fw": "/vol/fw"}
	spec := backend.Spec{
		Image: "e2e", Hostname: "caboose-e2e", User: "0:0",
		Mounts: []backend.Mount{{Source: share, Target: "/ck"}},
		Volumes: []backend.Volume{
			{Name: "docker", Target: "/var/lib/docker"},
			{Name: "fw", Target: "/vol/fw"},
			{Name: "live", Target: "/vol/live", Live: liveVolumes()},
		},
		Labels: []string{"caboose.isolation=vm"},
	}
	if err := v.Create(spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := v.WaitReady(90 * time.Second); err != nil {
		t.Fatalf("ready: %v", err)
	}
	step(t, start, "ready; disks: %s; debugfs %s", out(`for d in /sys/block/vd*; do echo -n "${d##*/}=$(cat $d/serial 2>/dev/null) "; done`), out(`command -v debugfs || echo none`))

	// The writers: a log of numbered lines, one write each, and four 4 MiB
	// files rewritten with fsync, so that a freeze has data to flush.
	for _, target := range vols {
		out(`cd ` + target + ` && (i=0; while [ $i -lt 3000000 ]; do echo $i; i=$((i+1)); done > log) > /dev/null 2>&1 < /dev/null &
cd ` + target + ` && (n=0; while [ $n -lt 100000 ]; do dd if=/dev/urandom of=blob.$((n%4)) bs=1M count=4 conv=fsync 2>/dev/null; n=$((n+1)); done) > /dev/null 2>&1 < /dev/null &
true`)
	}
	time.Sleep(3 * time.Second)
	last := func(target string) int {
		t.Helper()
		n, err := strconv.Atoi(out(`tail -n 1 ` + target + `/log | tr -d '\n'`))
		if err != nil {
			t.Errorf("%s's log: %v", target, err)
		}
		return n
	}

	s, err := checkpoint.Open(home, env)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Exposed = func() error { return nil }
	ids := map[string]checkpoint.ID{}
	upTo := map[string]int{}
	recovered := 0
	for _, name := range []string{"live", "fw"} {
		target := vols[name]
		before := last(target)
		l, err := s.Lock(context.Background(), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		t0 := time.Now()
		r, err := checkpoint.TakeLocked(context.Background(), l, s, checkpoint.Source{
			Path: target, Kind: checkpoint.SourceVolume, Top: filepath.Join(work, "volumes"), Rel: name + ".img", File: true,
			Volume: name, Hold: vmHolder{v, name},
		}, checkpoint.ReasonManual, checkpoint.TakeOptions{NoPrune: true})
		took := time.Since(t0)
		l.Unlock()
		after := last(target)
		if err != nil {
			t.Fatalf("checkpoint of %s: %v", name, err)
		}
		ids[name] = r.ID
		step(t, start, "%s: checkpoint %s in %v: %s, %d bytes, frozen=%v for %d ms; the log was at %d before, %d after",
			name, r.ID, took.Round(time.Millisecond), r.Meta.Method, r.Meta.Bytes, r.Meta.Volume.Frozen, r.Meta.Volume.FrozenMS, before, after)
		if !r.Meta.Volume.Frozen {
			t.Errorf("%s: not frozen", name)
		}
		// The clone, for the guest to check, and one taken unfrozen.
		tree := filepath.Join(s.Dir(), r.ID.Key, checkpoint.FormatStamp(r.ID.Stamp), checkpoint.TreeName)
		if err := unix.Clonefile(tree, filepath.Join(share, name+".img"), 0); err != nil {
			t.Fatal(err)
		}
		if err := unix.Clonefile(filepath.Join(work, "volumes", name+".img"), filepath.Join(share, name+"-unfrozen.img"), 0); err != nil {
			t.Fatal(err)
		}
		fsck := out(`e2fsck -fn /ck/` + name + `.img; echo "exit $?"`)
		step(t, start, "%s: e2fsck -fn of the checkpoint: %s", name, strings.ReplaceAll(fsck, "\n", " / "))
		if !strings.HasSuffix(fsck, "exit 0") {
			t.Errorf("%s: e2fsck of the checkpoint: %s", name, fsck)
		}
		log := out(`debugfs -R 'cat /log' /ck/` + name + `.img 2>/dev/null | tail -n 1; debugfs -R 'cat /log' /ck/` + name + `.img 2>/dev/null | wc -l`)
		f := strings.Fields(log)
		if len(f) != 2 {
			t.Errorf("%s: the checkpoint's log: %q", name, log)
			continue
		}
		n, _ := strconv.Atoi(f[0])
		lines, _ := strconv.Atoi(f[1])
		upTo[name] = n
		step(t, start, "%s: the checkpoint's log ends at %d, %d lines", name, n, lines)
		if n < before || n > after || lines != n+1 {
			t.Errorf("%s: the checkpoint's log ends at %d with %d lines; the writer was at %d before, %d after", name, n, lines, before, after)
		}
		un := out(`e2fsck -fn /ck/` + name + `-unfrozen.img; echo "exit $?"`)
		step(t, start, "%s: e2fsck -fn of a clone taken unfrozen, for comparison: %s", name, strings.ReplaceAll(un, "\n", " / "))
		if strings.Contains(un, "journal recovery") {
			recovered++
		}
	}
	// The comparison has to show what a freeze spares: else the writers
	// left nothing in the journal, and the frozen clone's clean check
	// proves nothing.
	if recovered == 0 {
		t.Error("no clone taken unfrozen needed journal recovery: the writers did not exercise the freeze")
	}
	out(`pkill -f 'i=0; whil[e]' ; true`)
	step(t, start, "dmesg: %s", out(`dmesg | grep -i -E 'freez|thaw|EXT4-fs (warning|error)' | tail -8; true`))

	// Restore with the VM running: refused.
	for name := range ids {
		if _, err := checkpoint.RestoreVolume(context.Background(), s, ids[name].String(), checkpoint.VolumeRestoreOptions{
			Dir: filepath.Join(work, "volumes"), Name: name, Stopped: v.HoldStopped}); err == nil || !strings.Contains(err.Error(), "is running") {
			t.Errorf("%s: a restore while running: %v", name, err)
		}
	}
	if err := v.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// What a restore of live interrupted between its two steps leaves:
	// its disk under a second name, and a clone of the checkpoint beside
	// it. The restore below has to go through it, on APFS.
	vols0 := filepath.Join(work, "volumes")
	if err := os.Link(filepath.Join(vols0, "live.img"), filepath.Join(vols0, "live.img"+checkpoint.BeforeRestoreSuffix)); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(s.Dir(), ids["live"].Key, checkpoint.FormatStamp(ids["live"].Stamp), checkpoint.TreeName)
	if err := unix.Clonefile(tree, filepath.Join(vols0, checkpoint.RestoringName("live", time.Unix(1, 0))), 0); err != nil {
		t.Fatal(err)
	}
	for name, id := range ids {
		r, err := checkpoint.RestoreVolume(context.Background(), s, id.String(), checkpoint.VolumeRestoreOptions{
			Dir: filepath.Join(work, "volumes"), Name: name, Stopped: v.HoldStopped})
		if err != nil {
			t.Fatalf("restore %s: %v", name, err)
		}
		step(t, start, "%s: restored %s, the disk as it was at %s", name, id, filepath.Base(r.Before))
		var d, b unix.Stat_t
		if err := unix.Stat(r.Disk, &d); err != nil {
			t.Fatal(err)
		}
		if err := unix.Stat(r.Before, &b); err != nil || b.Ino == d.Ino || b.Nlink != 1 {
			t.Errorf("%s: before-restore %v, inode %d (the disk's %d), %d links", name, err, b.Ino, d.Ino, b.Nlink)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(vols0, ".*.restoring-*")); len(left) > 0 {
		t.Errorf("left %v", left)
	}
	if err := v.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := v.WaitReady(90 * time.Second); err != nil {
		t.Fatalf("ready again: %v", err)
	}
	for name, target := range vols {
		got := last(target)
		step(t, start, "%s: after the restore, the log ends at %d (the checkpoint's: %d)", name, got, upTo[name])
		if got != upTo[name] {
			t.Errorf("%s: restored log at %d, want %d", name, got, upTo[name])
		}
	}
	if err := v.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// TestVolumeFreezeSafetyNets boots a VM with a volume and a writer on it,
// freezes the volume through the agent as a checkpoint does, and checks
// the agent's two safety nets in the real guest: a connection that froze
// it and ends has it thawed at once; one that stays open but sends no
// thaw has it thawed after agentproto.MaxFrozen, and its late thaw hears
// it expired.
func TestVolumeFreezeSafetyNets(t *testing.T) {
	dir := os.Getenv("CABOOSE_VM_E2E")
	if dir == "" {
		t.Skip("CABOOSE_VM_E2E names no directory of VM files")
	}
	work, err := os.MkdirTemp(dir, "cvf")
	if err != nil {
		t.Fatal(err)
	}
	h := host{dir: dir, work: work, volSize: 4 << 30}
	vmdir := vm.Dir(filepath.Join(work, "vm"))
	v := backend.NewVM("e2e-freeze", vmdir, h)
	v.Self, _ = os.Executable()
	v.Ready = ready
	defer func() {
		for _, f := range []string{vmdir.Log(), vmdir.Console()} {
			if b, err := os.ReadFile(f); err == nil {
				_ = os.WriteFile(filepath.Join(dir, "freeze-"+filepath.Base(f)), b, 0o644)
			}
		}
		_ = v.Remove()
		os.RemoveAll(work)
	}()
	start := time.Now()
	out := func(script string) string {
		t.Helper()
		s, err := backend.Output(v, "sh", "-c", script+" 2>&1")
		if err != nil {
			t.Errorf("%s: %v\n%s", script, err, s)
		}
		return strings.TrimSpace(s)
	}
	if err := v.Create(backend.Spec{
		Image: "e2e", Hostname: "caboose-e2e", User: "0:0",
		Volumes: []backend.Volume{{Name: "docker", Target: "/var/lib/docker"}, {Name: "live", Target: "/vol/live", Live: liveVolumes()}},
		Labels:  []string{"caboose.isolation=vm"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := v.WaitReady(90 * time.Second); err != nil {
		t.Fatalf("ready: %v", err)
	}
	out(`cd /vol/live && (i=0; while [ $i -lt 100000000 ]; do echo $i; i=$((i+1)); sleep 0.01; done > log) > /dev/null 2>&1 < /dev/null & true`)
	time.Sleep(time.Second)
	size := func() string { return out(`wc -l < /vol/live/log`) }
	moving := func() bool {
		a := size()
		time.Sleep(500 * time.Millisecond)
		return size() != a
	}
	if !moving() {
		t.Fatal("the writer does not write")
	}
	freeze := func(token string) *vm.Control {
		t.Helper()
		c, err := v.Dir.Control(10 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		info, err := c.Volume(agentproto.VolumeOp{Op: agentproto.VolumeFreeze, Name: "live", Target: "/vol/live", Token: token}, time.Minute)
		if err != nil || info.Error != "" {
			t.Fatalf("freeze: %+v %v", info, err)
		}
		return c
	}

	c := freeze("hangup")
	if moving() {
		t.Error("written while frozen")
	}
	c.Close()
	resumed := time.Now()
	for !moving() {
		if time.Since(resumed) > 10*time.Second {
			t.Fatal("still frozen 10 s after the connection that froze it ended")
		}
	}
	step(t, start, "thawed once the connection ended: writing again within %v", time.Since(resumed).Round(100*time.Millisecond))

	c = freeze("expire")
	defer c.Close()
	frozenAt := time.Now()
	for !moving() {
		if time.Since(frozenAt) > agentproto.MaxFrozen+10*time.Second {
			t.Fatalf("still frozen %v after the freeze", time.Since(frozenAt))
		}
	}
	after := time.Since(frozenAt)
	step(t, start, "with no thaw, writing again %v after the freeze (MaxFrozen %v)", after.Round(100*time.Millisecond), agentproto.MaxFrozen)
	if after < agentproto.MaxFrozen-time.Second {
		t.Errorf("thawed after %v, before MaxFrozen", after)
	}
	info, err := c.Volume(agentproto.VolumeOp{Op: agentproto.VolumeThaw, Name: "live", Target: "/vol/live", Token: "expire"}, time.Minute)
	step(t, start, "the late thaw: %+v %v", info, err)
	if err != nil || !info.Expired || !strings.Contains(info.Note, "thawed it itself") {
		t.Errorf("the late thaw: %+v %v", info, err)
	}
	step(t, start, "console: %s", strings.ReplaceAll(readTail(vmdir.Console(), 3), "\n", " / "))
}

func readTail(p string, n int) string {
	b, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
