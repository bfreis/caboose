package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/bfreis/caboose/internal/agentproto"
)

// volGuest is a guest with one volume, "cache", on /dev/vdd, as the
// steps see it; everything they do is recorded.
type volGuest struct {
	t       *testing.T
	disk    *diskCalls
	target  string
	mounted []string // the mount table's targets
	size    int64    // the disk, as the guest sees it
	// sizes, when set, is what the disk's size reads as, one call after
	// another (the last one sticks): the guest hearing of a resize late.
	sizes     []int64
	used      int64
	holders   []string
	unmountEr error
	runs      []string
	codes     map[string]int // a tool's exit, by name: -1 a signal's end
	absent    map[string]bool
	trims     []string
	trimErr   error
	trimSlow  time.Duration
	exclErr   error
	mountErr  error
	mountsErr error // the mount table, once a mount failed
}

func newVolGuest(t *testing.T) *volGuest {
	g := &volGuest{t: t, target: filepath.Join(t.TempDir(), "cache"), size: 64 << 30, used: 20 << 30, codes: map[string]int{}}
	g.disk = &diskCalls{sysBlock: sysDisks(t, map[string]string{"vdc": "", "vdd": "cache"}), dev: "/dev"}
	g.mounted = []string{"/", g.target}
	return g
}

func (g *volGuest) ops() volumeOps {
	d := g.disk.ops()
	mount := d.mount
	d.mount = func(src, dst, fs string, flags uintptr, data string) error {
		if g.mountErr != nil {
			g.mountsErr = errors.New("no mount table")
			return g.mountErr
		}
		g.mounted = append(g.mounted, dst)
		return mount(src, dst, fs, flags, data)
	}
	return volumeOps{
		disks: d,
		devSize: func(dev string) (int64, error) {
			if dev != "/dev/vdd" {
				g.t.Errorf("the size of %s", dev)
			}
			if len(g.sizes) > 0 {
				g.size = g.sizes[0]
				if len(g.sizes) > 1 {
					g.sizes = g.sizes[1:]
				}
			}
			return g.size, nil
		},
		statfs:  func(string) (int64, int64, error) { return g.size - 1<<30, g.used, nil },
		mounts:  func() ([]string, error) { return g.mounted, g.mountsErr },
		holders: func(string) ([]string, error) { return g.holders, nil },
		unmount: func(target string) error {
			if g.unmountEr != nil {
				return g.unmountEr
			}
			for i, m := range g.mounted {
				if m == target {
					g.mounted = append(g.mounted[:i], g.mounted[i+1:]...)
					return nil
				}
			}
			return unix.EINVAL
		},
		trim: func(target string) error {
			g.trims = append(g.trims, target)
			time.Sleep(g.trimSlow)
			return g.trimErr
		},
		exclusive: func(dev string) error {
			if dev != "/dev/vdd" {
				g.t.Errorf("exclusive %s", dev)
			}
			return g.exclErr
		},
		look: func(name string) (string, error) {
			if g.absent[name] {
				return "", errors.New("none")
			}
			return "/usr/sbin/" + name, nil
		},
		run: func(name string, args ...string) toolRun {
			g.runs = append(g.runs, name+" "+strings.Join(args, " "))
			for _, m := range g.mounted {
				if m == g.target {
					g.t.Errorf("%s ran with the volume mounted", name)
				}
			}
			if g.absent[name] {
				return toolRun{Code: -1, Err: errors.New("the image has no " + name)}
			}
			code := g.codes[name]
			var err error
			switch {
			case code < 0:
				err = errors.New("signal: killed")
			case code != 0:
				err = fmt.Errorf("exit status %d", code)
			}
			return toolRun{Out: name + " says hi\nline two", Started: true, Code: code, Err: err}
		},
		sizeWait: 50 * time.Millisecond,
	}
}

func (g *volGuest) step(op string, size int64) agentproto.VolumeInfo {
	return g.ops().step([]agentproto.GuestDisk{{Device: "/dev/vdc", Target: "/var/lib/docker"}, {Serial: "cache", Target: g.target}},
		agentproto.VolumeOp{Op: op, Name: "cache", Size: size})
}

// usage says what the volume is, changing nothing; a name the boot has
// no disk of caboose-vmm's for is refused.
func TestVolumeUsage(t *testing.T) {
	g := newVolGuest(t)
	info := g.step(agentproto.VolumeUsage, 0)
	want := agentproto.VolumeInfo{Op: "usage", Target: g.target, Mounted: true, DiskBytes: 64 << 30, FSBytes: 63 << 30, UsedBytes: 20 << 30}
	if !reflect.DeepEqual(info, want) {
		t.Errorf("%+v, want %+v", info, want)
	}
	// What a shrink would run and the image lacks is said.
	for _, tc := range []struct {
		absent map[string]bool
		want   string
	}{
		{map[string]bool{"resize2fs": true}, "resize2fs"},
		{map[string]bool{"e2fsck": true, "resize2fs": true}, "e2fsck, resize2fs"},
	} {
		g := newVolGuest(t)
		g.absent = tc.absent
		if info := g.step(agentproto.VolumeUsage, 0); info.Missing != tc.want || info.Error != "" {
			t.Errorf("%v: %+v", tc.absent, info)
		}
	}
	info = g.ops().step([]agentproto.GuestDisk{{Device: "/dev/vdc", Target: "/x"}}, agentproto.VolumeOp{Op: "usage", Name: "cache"})
	if !strings.Contains(info.Error, `no volume "cache"`) {
		t.Errorf("a volume the boot lacks: %+v", info)
	}
	if info := g.step("defrag", 0); !strings.Contains(info.Error, `no volume step "defrag"`) {
		t.Errorf("an unknown step: %+v", info)
	}
}

// A grow waits for the guest to see the disk's new size, then grows the
// mounted filesystem; a size that never shows is an error, and nothing is
// grown.
func TestVolumeGrow(t *testing.T) {
	g := newVolGuest(t)
	g.sizes = []int64{64 << 30, 64 << 30, 96 << 30}
	info := g.step(agentproto.VolumeGrow, 96<<30)
	if info.Error != "" || info.DiskBytes != 96<<30 {
		t.Fatalf("%+v", info)
	}
	if want := []string{"/dev/vdd " + g.target}; !reflect.DeepEqual(g.disk.grows, want) {
		t.Errorf("grows %q", g.disk.grows)
	}
	g = newVolGuest(t)
	info = g.step(agentproto.VolumeGrow, 96<<30)
	if !strings.Contains(info.Error, "sees the disk at 68719476736 bytes, not 103079215104") || len(g.disk.grows) != 0 {
		t.Errorf("a size never seen: %+v, grows %q", info, g.disk.grows)
	}
	g = newVolGuest(t)
	g.mounted = []string{"/"}
	if info := g.step(agentproto.VolumeGrow, 64<<30); !strings.Contains(info.Error, "is not mounted") {
		t.Errorf("unmounted: %+v", info)
	}
}

// An unmount is refused while anything holds the volume, naming at most
// MaxVolumeHolders of them, mounts inside it first; with none it
// unmounts.
func TestVolumeUnmount(t *testing.T) {
	g := newVolGuest(t)
	g.mounted = append(g.mounted, g.target+"/sub", g.target+"-other")
	for i := range 12 {
		g.holders = append(g.holders, strconv.Itoa(100+i)+" (go): "+g.target+"/f")
	}
	info := g.step(agentproto.VolumeUnmount, 0)
	if !strings.Contains(info.Error, "is in use") || len(info.Holders) != agentproto.MaxVolumeHolders || info.HoldersMore != 3 {
		t.Fatalf("%+v", info)
	}
	if info.Holders[0] != "mount at "+g.target+"/sub" || info.Holders[1] != "100 (go): "+g.target+"/f" {
		t.Errorf("holders %q", info.Holders)
	}
	if !info.Mounted {
		t.Error("unmounted although held")
	}

	if len(g.trims) != 0 {
		t.Errorf("trimmed although held: %q", g.trims)
	}

	// Trimmed first, the discards queued at the unmount being lost.
	g = newVolGuest(t)
	info = g.step(agentproto.VolumeUnmount, 0)
	if info.Error != "" || info.Mounted || info.Note != "" {
		t.Errorf("%+v", info)
	}
	if want := []string{g.target}; !reflect.DeepEqual(g.trims, want) {
		t.Errorf("trims %q", g.trims)
	}
	// Unmounted already: nothing to do.
	if info := g.step(agentproto.VolumeUnmount, 0); info.Error != "" {
		t.Errorf("again: %+v", info)
	}

	g = newVolGuest(t)
	g.unmountEr = unix.EBUSY
	if info := g.step(agentproto.VolumeUnmount, 0); !strings.Contains(info.Error, "it is busy") || !strings.Contains(info.Error, "mount namespace") {
		t.Errorf("busy: %+v", info)
	}
}

// A trim that fails, or takes too long, is said, and the unmount goes on.
func TestVolumeUnmountTrim(t *testing.T) {
	g := newVolGuest(t)
	g.trimErr = unix.EOPNOTSUPP
	info := g.step(agentproto.VolumeUnmount, 0)
	if info.Error != "" || info.Mounted || !strings.Contains(info.Note, "not trimmed") || !strings.Contains(info.Note, "not supported") {
		t.Errorf("%+v", info)
	}
	if err := trimAll([]string{"/a"}, func(string) error { time.Sleep(time.Second); return nil }, 20*time.Millisecond); err == nil || !strings.Contains(err.Error(), "did not end in") {
		t.Errorf("a slow trim: %v", err)
	}
}

// A filesystem still in use once unmounted here -- a container's mount of
// it, in a mount namespace of its own -- is mounted again, and the
// unmount refused, naming the likely cause.
func TestVolumeUnmountElsewhere(t *testing.T) {
	g := newVolGuest(t)
	g.exclErr = unix.EBUSY
	info := g.step(agentproto.VolumeUnmount, 0)
	if !strings.Contains(info.Error, "still in use") || !strings.Contains(info.Error, "another mount namespace") || !strings.Contains(info.Error, "mounted again") || !info.Mounted {
		t.Errorf("%+v", info)
	}
	if want := []string{"/dev/vdd " + g.target + " ext4 discard"}; !reflect.DeepEqual(g.disk.mounts, want) {
		t.Errorf("mounts %q", g.disk.mounts)
	}
	// And when it does not mount again, that is said, and the answer has
	// it unmounted, even with no mount table to tell.
	g = newVolGuest(t)
	g.exclErr, g.mountErr = unix.EBUSY, unix.EINVAL
	info = g.step(agentproto.VolumeUnmount, 0)
	if !strings.Contains(info.Error, "mounting it again here failed") || info.Mounted {
		t.Errorf("%+v", info)
	}
}

// Before powering off, every kept disk is trimmed; a failure is logged,
// and the power-off goes on.
func TestTrimDisks(t *testing.T) {
	var trims, logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	trim := func(t string) error {
		trims = append(trims, t)
		if t == "/vol" {
			return unix.EIO
		}
		return nil
	}
	trimDisks([]agentproto.GuestDisk{{Device: "/dev/vdc", Target: "/var/lib/docker"}, {Serial: "v", Target: "/vol"}}, trim, time.Second, logf)
	if !reflect.DeepEqual(trims, []string{"/var/lib/docker", "/vol"}) || len(logs) != 1 || !strings.Contains(logs[0], "/vol: input/output error") {
		t.Errorf("trims %q, logs %q", trims, logs)
	}
	trims, logs = nil, nil
	trimDisks(nil, trim, time.Second, logf)
	if len(trims)+len(logs) != 0 {
		t.Errorf("no disks: trims %q, logs %q", trims, logs)
	}
}

// The filesystem is checked with e2fsck -f -p, then shrunk with
// resize2fs, never while mounted. An e2fsck that found what it would not
// fix unattended stops the shrink, resize2fs unrun; one that fixed
// something goes on.
func TestVolumeShrinkFS(t *testing.T) {
	g := newVolGuest(t)
	if info := g.step(agentproto.VolumeShrinkFS, 32<<30); !strings.Contains(info.Error, "still mounted") || len(g.runs) != 0 {
		t.Fatalf("mounted: %+v, runs %q", info, g.runs)
	}
	g.mounted = []string{"/"}
	info := g.step(agentproto.VolumeShrinkFS, 32<<30)
	if info.Error != "" {
		t.Fatalf("%+v", info)
	}
	if want := []string{"e2fsck -f -p -E discard /dev/vdd", "resize2fs /dev/vdd 33554432K"}; !reflect.DeepEqual(g.runs, want) {
		t.Errorf("runs %q", g.runs)
	}
	if info.Dirty {
		t.Error("dirty")
	}

	// A tool that ended halfway, or a resize2fs that failed, may have left
	// the filesystem half changed: Dirty. One that did not run, or an
	// e2fsck that stopped at what it would not fix, changed nothing more.
	for _, tc := range []struct {
		codes  map[string]int
		absent map[string]bool
		runs   int
		err    string
		dirty  bool
	}{
		{map[string]int{"e2fsck": 1}, nil, 2, "", false},
		{map[string]int{"e2fsck": 2}, nil, 2, "", false},
		{map[string]int{"e2fsck": 4}, nil, 1, "exited 4, and nothing was shrunk", false},
		{map[string]int{"e2fsck": 8}, nil, 1, "exited 8", false},
		{map[string]int{"e2fsck": -1}, nil, 1, "did not exit (signal: killed), and may have left the filesystem half repaired", true},
		{map[string]int{"resize2fs": 1}, nil, 2, "resize2fs /dev/vdd 33554432K exited 1, and may have left", true},
		{map[string]int{"resize2fs": -1}, nil, 2, "resize2fs /dev/vdd 33554432K did not exit", true},
		{nil, map[string]bool{"e2fsck": true}, 1, "e2fsck: the image has no e2fsck", false},
		{nil, map[string]bool{"resize2fs": true}, 2, "resize2fs: the image has no resize2fs", false},
	} {
		g := newVolGuest(t)
		g.mounted, g.codes, g.absent = []string{"/"}, tc.codes, tc.absent
		info := g.step(agentproto.VolumeShrinkFS, 32<<30)
		if len(g.runs) != tc.runs || (tc.err == "") != (info.Error == "") || !strings.Contains(info.Error, tc.err) || info.Dirty != tc.dirty {
			t.Errorf("%v %v: %+v, runs %q", tc.codes, tc.absent, info, g.runs)
		}
	}
	// A size off the disk, or under 1G, is refused.
	for _, size := range []int64{96 << 30, 512 << 20} {
		g := newVolGuest(t)
		g.mounted = []string{"/"}
		if info := g.step(agentproto.VolumeShrinkFS, size); info.Error == "" || len(g.runs) != 0 {
			t.Errorf("%d: %+v", size, info)
		}
	}
}

// A mount mounts with discard and grows the filesystem once the guest
// sees the disk at the size given; one still at another size is mounted
// ungrown, and said, so a filesystem never outgrows a disk being lowered.
func TestVolumeMount(t *testing.T) {
	g := newVolGuest(t)
	g.mounted = []string{"/"}
	g.sizes = []int64{64 << 30, 32 << 30}
	info := g.step(agentproto.VolumeMount, 32<<30)
	if info.Error != "" || !info.Mounted || info.Note != "" {
		t.Fatalf("%+v", info)
	}
	if want := []string{"/dev/vdd " + g.target + " ext4 discard"}; !reflect.DeepEqual(g.disk.mounts, want) {
		t.Errorf("mounts %q", g.disk.mounts)
	}
	if len(g.disk.grows) != 1 {
		t.Errorf("grows %q", g.disk.grows)
	}

	g = newVolGuest(t)
	g.mounted = []string{"/"}
	info = g.step(agentproto.VolumeMount, 32<<30)
	if info.Error != "" || !info.Mounted || !strings.Contains(info.Note, "left at its size") || len(g.disk.grows) != 0 {
		t.Errorf("a size not seen: %+v, grows %q", info, g.disk.grows)
	}
	// Mounted already: nothing to do.
	if info := g.step(agentproto.VolumeMount, 64<<30); info.Error != "" || len(g.disk.mounts) != 1 {
		t.Errorf("again: %+v, mounts %q", info, g.disk.mounts)
	}
}

// What the guest answers is bounded: a tool's long output is cut to its
// end.
func TestVolumeBound(t *testing.T) {
	long := strings.Repeat("x", 3*agentproto.MaxVolumeText) + "END"
	if got := bound(long); len(got) != agentproto.MaxVolumeText || !strings.HasSuffix(got, "END") {
		t.Errorf("bound: %d bytes, %q...", len(got), got[:10])
	}
	// Cut where a character starts: 3-byte characters, the cut landing
	// inside one.
	wide := strings.Repeat("é€", agentproto.MaxVolumeText)
	if got := bound(wide); !utf8.ValidString(got) || len(got) > agentproto.MaxVolumeText || !strings.HasSuffix(got, "é€") {
		t.Errorf("bound of wide characters: %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
}

// The holders are found by the device of the files a process has open,
// its directories and its mappings, whatever their paths.
func TestVolumeHolders(t *testing.T) {
	vol := t.TempDir()
	proc := t.TempDir()
	file := filepath.Join(vol, "held")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The device of the volume's files: an overlay, as a sandbox's /tmp
	// may be, gives its directories another one, which ext4 never does.
	var st unix.Stat_t
	if err := unix.Stat(file, &st); err != nil {
		t.Fatal(err)
	}
	var other unix.Stat_t
	if err := unix.Stat("/dev/null", &other); err != nil || other.Dev == st.Dev {
		t.Skip("/dev/null is on the temp dir's filesystem")
	}
	mk := func(pid, comm string, links map[string]string, maps string) {
		dir := filepath.Join(proc, pid)
		if err := os.MkdirAll(filepath.Join(dir, "fd"), 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "maps"), []byte(maps), 0o644)
		for l, to := range links {
			if err := os.Symlink(to, filepath.Join(dir, l)); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("10", "writer", map[string]string{"cwd": "/dev", "fd/3": "/dev/null", "fd/4": file}, "")
	mk("11", "shell", map[string]string{"cwd": file}, "")
	mk("12", "mapper", map[string]string{"cwd": "/dev"},
		fmt.Sprintf("7f00-7f01 r--p 00000000 %02x:%02x 1234 %s/lib.so\n", unix.Major(st.Dev), unix.Minor(st.Dev), vol))
	mk("13", "idle\x1b", map[string]string{"cwd": "/dev", "fd/0": "/dev/null"}, "7f00-7f01 r--p 00000000 00:00 0 [heap]\n")
	// A mapping of no file (inode 0) on the device's numbers is none.
	mk("14", "anon", map[string]string{"cwd": "/dev"},
		fmt.Sprintf("7f00-7f01 rw-p 00000000 %02x:%02x 0 [anon]\n", unix.Major(st.Dev), unix.Minor(st.Dev)))
	mk("self", "x", nil, "")
	got, err := volumeHolders(proc, st.Dev)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10 (writer): " + file, "11 (shell): " + file, "12 (mapper): " + vol + "/lib.so"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("holders %q, want %q", got, want)
	}
}

// The mount table's targets, its octal escapes undone.
func TestMountTargets(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mounts")
	if err := os.WriteFile(p, []byte("/dev/vda / ext4 ro 0 0\n/dev/vdd /home/agent/my\\040cache ext4 rw,discard 0 0\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := mountTargets(p)
	if err != nil || !reflect.DeepEqual(got, []string{"/", "/home/agent/my cache"}) {
		t.Errorf("%q, %v", got, err)
	}
	if got := unescapeMount(`a\134b\04`); got != `a\b\04` {
		t.Errorf("unescape %q", got)
	}
}

// The guest's own steps are the real ones.
func TestGuestVolumes(t *testing.T) {
	if guestVolumes.sizeWait <= 0 {
		t.Errorf("wait %v", guestVolumes.sizeWait)
	}
	if r := runTool("caboose-no-such-tool"); r.Started || r.Code != -1 || r.Err == nil || !strings.Contains(r.Err.Error(), "e2fsprogs") {
		t.Errorf("a missing tool: %+v", r)
	}
	// Found on PATH, else in volumeTools; run to its end, its exit said;
	// a signal's end is no exit.
	dir := t.TempDir()
	for name, body := range map[string]string{"caboose-t-ok": "echo out; echo err >&2; exit 3", "caboose-t-sig": "kill -9 $$"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	if p, err := lookTool("caboose-t-ok"); err != nil || p != filepath.Join(dir, "caboose-t-ok") {
		t.Errorf("on PATH: %q, %v", p, err)
	}
	if p, err := lookTool("sh"); err != nil || !strings.HasSuffix(p, "/sh") {
		t.Errorf("in volumeTools: %q, %v", p, err)
	}
	if r := runTool("caboose-t-ok"); !r.Started || r.Code != 3 || r.Out != "out\nerr\n" {
		t.Errorf("ran: %+v", r)
	}
	if r := runTool("caboose-t-sig"); !r.Started || r.Code != -1 || r.Err == nil {
		t.Errorf("a signal's end: %+v", r)
	}
	if _, err := devSize(filepath.Join(t.TempDir(), "none")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing device: %v", err)
	}
}
