package agent

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/bfreis/caboose/internal/agentproto"
)

func TestPlanMounts(t *testing.T) {
	tags, binds, err := planMounts([]agentproto.GuestMount{
		{Tag: "work", Target: "/work"},
		{Tag: "data", Path: "home/.claude", Target: "/home/agent/.claude"},
		{Tag: "data", Path: "home/.claude.json", Target: "/home/agent/.claude.json"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(tags, ",") != "work,data" {
		t.Errorf("tags %q", tags)
	}
	want := []bind{
		{sharesDir + "/work", "/work"},
		{sharesDir + "/data/home/.claude", "/home/agent/.claude"},
		{sharesDir + "/data/home/.claude.json", "/home/agent/.claude.json"},
	}
	if len(binds) != len(want) {
		t.Fatalf("binds %q", binds)
	}
	for i := range want {
		if binds[i] != want[i] {
			t.Errorf("bind %d: %q, want %q", i, binds[i], want[i])
		}
	}
	for _, bad := range []agentproto.GuestMount{
		{Tag: "", Target: "/x"},
		{Tag: "a/b", Target: "/x"},
		{Tag: "..", Target: "/x"},
		{Tag: "d", Path: "../etc", Target: "/x"},
		{Tag: "d", Path: "/etc", Target: "/x"},
		{Tag: "d", Path: "a/../../b", Target: "/x"},
		{Tag: "d", Target: "relative"},
		{Tag: "d", Target: "/a/../b"},
		{Tag: "d", Target: "/"},
	} {
		if _, _, err := planMounts([]agentproto.GuestMount{bad}); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

// What the kernel's ip=dhcp leaves in /proc/net/pnp, and each libc's
// options: a retry after 1 s, giving up after 5.
func TestResolvConf(t *testing.T) {
	pnp := "#PROTO: DHCP\ndomain local\nnameserver 192.168.64.1\nnameserver 0.0.0.0\nbootserver 192.168.64.1\n"
	if got := resolvConf(pnp, false); got != "search local\nnameserver 192.168.64.1\noptions timeout:1 attempts:5\n" {
		t.Errorf("glibc: %q", got)
	}
	if got := resolvConf(pnp, true); got != "search local\nnameserver 192.168.64.1\noptions timeout:5 attempts:5\n" {
		t.Errorf("musl: %q", got)
	}
	if got := resolvConf("#PROTO: DHCP\nbootserver 0.0.0.0\n", false); got != "" {
		t.Errorf("no server: %q", got)
	}
}

func TestMuslRoot(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "lib"), 0o755)
	os.WriteFile(filepath.Join(root, "lib", "ld-linux-aarch64.so.1"), nil, 0o755)
	if muslRoot(root) {
		t.Error("glibc root taken for musl")
	}
	os.WriteFile(filepath.Join(root, "lib", "ld-musl-aarch64.so.1"), nil, 0o755)
	if !muslRoot(root) {
		t.Error("musl root not seen")
	}
}

// fakeResolver answers every query on a UDP port but the first drop ones,
// with the query's ID and the answer bit set.
func fakeResolver(t *testing.T, drop int) string {
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := c.ReadFrom(buf)
			if err != nil {
				return
			}
			if drop > 0 {
				drop--
				continue
			}
			if n >= 12 {
				buf[2] |= 0x80
				c.WriteTo(buf[:n], from)
			}
		}
	}()
	return c.LocalAddr().String()
}

// The warm-up resends until an answer comes, and gives up at its budget.
func TestAskResolver(t *testing.T) {
	if took, err := askResolver(fakeResolver(t, 0), time.Second); err != nil || took > 200*time.Millisecond {
		t.Errorf("answering: %v, %v", took, err)
	}
	if took, err := askResolver(fakeResolver(t, 2), 2*time.Second); err != nil || took < 400*time.Millisecond || took > 900*time.Millisecond {
		t.Errorf("two dropped: %v, %v", took, err)
	}
	if took, err := askResolver(fakeResolver(t, 1000), 600*time.Millisecond); err == nil || took < 550*time.Millisecond || took > 900*time.Millisecond {
		t.Errorf("silent: %v, %v", took, err)
	}
	if rq := rootQuery(7); len(rq) != 17 || rq[1] != 7 || rq[2] != 1 || rq[14] != 2 || rq[16] != 1 {
		t.Errorf("query % x", rq)
	}
}

// A WaitResolver boot waits for the warm-up, silently when it is over
// already, and never past its limit.
func TestAwaitResolver(t *testing.T) {
	var console syncWriter
	m := &machine{logf: func(f string, a ...any) { fmt.Fprintf(&console, f+"\n", a...) }}
	done := make(chan struct{})
	close(done)
	m.awaitResolver(done, time.Second)
	if out := console.String(); out != "" {
		t.Errorf("an over warm-up said %q", out)
	}
	later := make(chan struct{})
	time.AfterFunc(200*time.Millisecond, func() { close(later) })
	start := time.Now()
	m.awaitResolver(later, 5*time.Second)
	if took := time.Since(start); took < 150*time.Millisecond || took > 2*time.Second {
		t.Errorf("waited %v for a warm-up over in 200ms", took)
	}
	start = time.Now()
	m.awaitResolver(make(chan struct{}), 200*time.Millisecond)
	if took := time.Since(start); took < 150*time.Millisecond || took > 2*time.Second {
		t.Errorf("waited %v past a 200ms limit", took)
	}
	if out := console.String(); !strings.Contains(out, "waited") || !strings.Contains(out, "stopped waiting") {
		t.Errorf("console %q", out)
	}
}

// The warm-up says on the console how long the resolver took, or that it
// never answered; it asks resolv.conf's first nameserver.
func TestWarmResolver(t *testing.T) {
	var console syncWriter
	m := &machine{logf: func(f string, a ...any) { fmt.Fprintf(&console, f+"\n", a...) }}
	m.warmResolver(fakeResolver(t, 1), time.Second)
	m.warmResolver(fakeResolver(t, 1000), 300*time.Millisecond)
	if out := console.String(); !strings.Contains(out, " answered in ") || !strings.Contains(out, " did not answer in ") {
		t.Errorf("console %q", out)
	}
	if got := firstNameserver("search local\nnameserver 10.0.0.1\nnameserver 10.0.0.2\n"); got != "10.0.0.1" {
		t.Errorf("first nameserver %q", got)
	}
	if got := firstNameserver("search local\n"); got != "" {
		t.Errorf("no nameserver %q", got)
	}
}

// The entrypoint runs with the spec's environment, in its HOME, and the
// boot waits for its ready file -- or fails once it is gone without one.
func TestEntrypointReady(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	script := filepath.Join(dir, "entrypoint")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$FOO $(pwd) $1\" > out; sleep 0.2; touch ready; sleep 30\n"), 0o755)
	var console syncWriter
	m := &machine{logf: t.Logf, console: &console}
	spec := agentproto.BootSpec{Cmd: []string{script, "--cc-supervise"}, Env: []string{"FOO=bar", "HOME=" + dir, "PATH=" + os.Getenv("PATH")}, Ready: ready}
	if err := m.start(spec); err != nil {
		t.Fatal(err)
	}
	// m is reused below: the cleanup kills this one's process group (its
	// sleep holds the console open), and waits for its log line, which must
	// not come after the test.
	first := m
	t.Cleanup(func() { syscall.Kill(-first.entry.Process.Pid, syscall.SIGKILL); <-first.exited })
	start := time.Now()
	if err := m.waitReady(ready); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Error("ready before the entrypoint made the file")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "out")); string(b) != "bar "+dir+" --cc-supervise\n" {
		t.Errorf("entrypoint saw %q", b)
	}

	os.WriteFile(script, []byte("#!/bin/sh\necho failing; exit 1\n"), 0o755)
	m = &machine{logf: t.Logf, console: &console}
	if err := m.start(spec); err != nil {
		t.Fatal(err)
	}
	if err := m.waitReady(filepath.Join(dir, "never")); err == nil || !strings.Contains(err.Error(), "exited") {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(console.String(), "failing") {
		t.Errorf("console %q", console.String())
	}
}

// Before the boot the exec port says so rather than run as nobody knows
// whom; a spec without what the boot needs is refused before it touches
// anything.
func TestGuestBeforeBoot(t *testing.T) {
	m := &machine{logf: t.Logf}
	r := runExec(t, m.execServer(), agentproto.ExecRequest{Argv: []string{"true"}}, "", nil)
	if r.code != agentproto.ExitCannotRun || !strings.Contains(r.errOut, "not booted") {
		t.Errorf("%+v", r)
	}
	for _, spec := range []agentproto.BootSpec{
		{Cmd: []string{"/e"}, Ready: "/r"},
		{User: "1000:1000", Ready: "/r"},
		{User: "1000:1000", Cmd: []string{"e"}, Ready: "/r"},
		{User: "1000:1000", Cmd: []string{"/e"}},
		{User: "agent", Cmd: []string{"/e"}, Ready: "/r"},
		{User: "1000:1000", Cmd: []string{"/e"}, Ready: "/r", Mounts: []agentproto.GuestMount{{Tag: "x", Target: "rel"}}},
	} {
		if err := m.Boot(spec); err == nil {
			t.Errorf("booted %+v", spec)
		}
	}
}

// The boot's times on the console are the monotonic clock's, which moves on.
func TestSinceBoot(t *testing.T) {
	a := sinceBoot()
	time.Sleep(20 * time.Millisecond)
	if b := sinceBoot(); a <= 0 || b < a+10*time.Millisecond {
		t.Errorf("%v then %v", a, b)
	}
}

func TestScratchOnTmpfs(t *testing.T) {
	for cmdline, want := range map[string]bool{
		"console=hvc0 caboose.scratch=tmpfs ip=dhcp\n": true,
		"console=hvc0 ip=dhcp\n":                       false,
		"caboose.scratch=tmpfsx":                       false,
	} {
		if got := scratchOnTmpfs(cmdline); got != want {
			t.Errorf("%q: %v", cmdline, got)
		}
	}
}

// securityfs is mounted on the sysfs, as locked down as it, and a kernel
// without it still boots; everything else is required.
func TestLateMountTable(t *testing.T) {
	sysfs, sec := -1, -1
	for i, m := range lateMountTable {
		switch m.fs {
		case "sysfs":
			sysfs = i
		case "securityfs":
			sec = i
			if m.dst != "/sys/kernel/security" || !m.optional || m.flags != unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC {
				t.Errorf("securityfs: %+v", m)
			}
		default:
			if m.optional {
				t.Errorf("%s optional", m.fs)
			}
		}
	}
	if sysfs < 0 || sec <= sysfs {
		t.Errorf("sysfs at %d, securityfs at %d", sysfs, sec)
	}
}

// A boot spec's disks are after the root and scratch disk, at clean
// absolute targets.
func TestCheckDisks(t *testing.T) {
	ok := []agentproto.GuestDisk{{Device: "/dev/vdc", Target: "/var/lib/docker"}, {Device: "/dev/vdz", Target: "/data"}}
	if err := checkDisks(ok); err != nil {
		t.Fatal(err)
	}
	for _, d := range []agentproto.GuestDisk{
		{Device: "/dev/vda", Target: "/x"}, {Device: "/dev/vdb", Target: "/x"}, {Device: "/dev/sda", Target: "/x"},
		{Device: "/dev/vdc", Target: "x"}, {Device: "/dev/vdc", Target: "/"}, {Device: "/dev/vdc", Target: "/a/../b"},
	} {
		if checkDisks([]agentproto.GuestDisk{d}) == nil {
			t.Errorf("%+v accepted", d)
		}
	}
}

// A link connection that comes before the entrypoint starts waits for it,
// and is served with its boot once it has; a boot taken up from an earlier
// run serves at once.
func TestLinkWaitsForTheBoot(t *testing.T) {
	m := &machine{logf: t.Logf, done: make(chan struct{})}
	spec := &agentproto.BootSpec{User: "1000:1000", Hostname: "box"}
	got := make(chan *agentproto.BootSpec, 1)
	start := time.Now()
	go func() { got <- m.waitLinkable(5 * time.Second) }()
	time.Sleep(150 * time.Millisecond)
	m.mu.Lock()
	m.started = spec
	m.mu.Unlock()
	m.linkUp()
	select {
	case s := <-got:
		if s != spec {
			t.Errorf("served %+v", s)
		}
		if took := time.Since(start); took < 100*time.Millisecond || took > 2*time.Second {
			t.Errorf("served after %v, the boot started after 150ms", took)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still waiting once the entrypoint started")
	}
	m.linkUp() // a second one changes nothing
	if s := m.waitLinkable(time.Millisecond); s != spec {
		t.Errorf("once started: %+v", s)
	}
}

// One that waits in vain is closed once the bound passes, or at once when
// the machine shuts down meanwhile.
func TestLinkWaitEnds(t *testing.T) {
	old := linkWait
	t.Cleanup(func() { linkWait = old })
	closed := func(t *testing.T, m *machine, within time.Duration) {
		t.Helper()
		fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err != nil {
			t.Fatal(err)
		}
		ours, theirs := os.NewFile(uintptr(fds[0]), "host"), os.NewFile(uintptr(fds[1]), "link")
		defer ours.Close()
		start := time.Now()
		go m.serveLink(theirs)
		ours.SetReadDeadline(time.Now().Add(10 * time.Second))
		if n, err := ours.Read(make([]byte, 1)); n != 0 || err == nil || os.IsTimeout(err) {
			t.Fatalf("read %d, %v: not closed", n, err)
		}
		if took := time.Since(start); took > within {
			t.Errorf("closed after %v", took)
		}
	}
	t.Run("bound", func(t *testing.T) {
		linkWait = 200 * time.Millisecond
		var console syncWriter
		m := &machine{logf: func(f string, a ...any) { fmt.Fprintf(&console, f+"\n", a...) }, done: make(chan struct{})}
		start := time.Now()
		closed(t, m, 3*time.Second)
		if took := time.Since(start); took < 150*time.Millisecond {
			t.Errorf("closed after %v, before the bound", took)
		}
		if !strings.Contains(console.String(), "no boot to serve") {
			t.Errorf("console %q", console.String())
		}
	})
	t.Run("shutdown", func(t *testing.T) {
		linkWait = time.Minute
		m := &machine{logf: t.Logf, done: make(chan struct{})}
		time.AfterFunc(100*time.Millisecond, func() { m.doneOnce.Do(func() { close(m.done) }) })
		closed(t, m, 5*time.Second)
	})
}
