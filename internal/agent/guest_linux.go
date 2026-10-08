package agent

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/bfreis/caboose/internal/agentproto"
)

// A vm guest's agent, `caboose-agent guest`: the init's child, as root. It
// serves the control, exec and link ports, and is the machine the control
// port boots (machine). The init restarts it if it dies, and it then takes
// up the boot it already did from bootFile.
const (
	stateDir  = "/run/caboose"
	bootFile  = stateDir + "/boot.json"
	sharesDir = stateDir + "/shares"
)

// RunGuest serves the guest's ports until one fails; log is the console.
func RunGuest(log io.Writer) error {
	logf := func(format string, args ...any) { fmt.Fprintf(log, "caboose-agent: "+format+"\n", args...) }
	m := &machine{logf: logf, console: log, done: make(chan struct{})}
	go m.keepLease()
	if err := m.resume(); err != nil {
		logf("the boot before this agent's restart is lost: %v", err)
	}
	ctl := &ControlServer{Guest: m, Clock: SystemClock{}, Logf: logf}
	lnControl, err := listenVsock(agentproto.PortControl)
	if err != nil {
		return err
	}
	lnExec, err := listenVsock(agentproto.PortExec)
	if err != nil {
		return err
	}
	lnLink, err := listenVsock(agentproto.PortLink)
	if err != nil {
		return err
	}
	errs := make(chan error, 3)
	go func() {
		errs <- lnExec.serve(func(c io.ReadWriteCloser) { _ = m.execServer().ServeConn(c) })
	}()
	go func() { errs <- lnLink.serve(m.serveLink) }()
	go func() {
		errs <- lnControl.serve(func(c io.ReadWriteCloser) {
			if err := ctl.ServeConn(c); err != nil {
				logf("control: %v", err)
			}
		})
	}()
	return <-errs
}

// machine is the guest as the control port boots it.
type machine struct {
	logf    func(format string, args ...any)
	console io.Writer

	mu   sync.Mutex
	spec *agentproto.BootSpec
	// started is the boot whose entrypoint runs, ready or not yet: the
	// link is served from then on, so that the outbound proxy is up for
	// the entrypoint's own downloads (a first install of Claude Code).
	started *agentproto.BootSpec
	// linked is closed once linkable has a boot to serve (linkUp): a link
	// connection that comes first waits for it (waitLinkable).
	linked chan struct{}
	entry  *exec.Cmd     // the entrypoint, when this agent started it
	exited chan struct{} // closed when it exits

	done     chan struct{} // closed by Shutdown: ends keepLease
	doneOnce sync.Once
}

func (m *machine) booted() *agentproto.BootSpec {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.spec
}

// linkable is the boot the link is served for: once its entrypoint has
// started, before it is ready.
func (m *machine) linkable() *agentproto.BootSpec {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.spec != nil {
		return m.spec
	}
	return m.started
}

// linkWait bounds how long a link connection made before the entrypoint
// started waits for it: the launcher dials the link port as soon as the
// agent has the boot (ensureRunning), and a boot mounts and checks its
// disks first. A boot that fails never starts it.
var linkWait = 60 * time.Second

// linkedLocked is linked, made on first use; m.mu is held.
func (m *machine) linkedLocked() chan struct{} {
	if m.linked == nil {
		m.linked = make(chan struct{})
	}
	return m.linked
}

// linkUp says the link has a boot to serve now: a boot's entrypoint
// started, or an earlier run's boot was taken up. Only the first call
// closes linked.
func (m *machine) linkUp() {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch := m.linkedLocked()
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// waitLinkable is linkable, waiting up to limit for it to have a boot;
// nil once the limit passes with none, or once the machine shuts down.
func (m *machine) waitLinkable(limit time.Duration) *agentproto.BootSpec {
	m.mu.Lock()
	linked := m.linkedLocked()
	m.mu.Unlock()
	if spec := m.linkable(); spec != nil {
		return spec
	}
	t := time.NewTimer(limit)
	defer t.Stop()
	select {
	case <-linked:
	case <-m.done: // nil in tests that never shut down: never ready
		return nil
	case <-t.C:
	}
	return m.linkable()
}

// resume takes up a boot an earlier run of this agent did.
func (m *machine) resume() error {
	b, err := os.ReadFile(bootFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var spec agentproto.BootSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		return err
	}
	if _, err := parseUser(spec.User); err != nil {
		return err
	}
	m.mu.Lock()
	m.spec = &spec
	m.mu.Unlock()
	m.linkUp()
	m.logf("taking up the boot of %s", spec.Hostname)
	return nil
}

// Boot is Guest's: the sandbox as docker run makes a container, then the
// entrypoint, as the sandbox's user, until it says it is ready.
func (m *machine) Boot(spec agentproto.BootSpec) error {
	if m.booted() != nil {
		return nil
	}
	asked := sinceBoot()
	if spec.User == "" || len(spec.Cmd) == 0 || !filepath.IsAbs(spec.Cmd[0]) || !filepath.IsAbs(spec.Ready) {
		return errors.New("the boot spec needs a user, an entrypoint by its path and a ready file")
	}
	if _, err := parseUser(spec.User); err != nil {
		return err
	}
	tags, binds, err := planMounts(spec.Mounts)
	if err != nil {
		return err
	}
	if err := checkDisks(spec.Disks); err != nil {
		return err
	}
	if spec.Hostname != "" {
		if err := unix.Sethostname([]byte(spec.Hostname)); err != nil {
			return fmt.Errorf("hostname: %w", err)
		}
	}
	if err := replaceFile("/etc/hosts", hostsFile(spec.Hostname)); err != nil {
		return err
	}
	resolved := make(chan struct{}) // closed once the warm-up is over
	warming := false
	if pnp, err := os.ReadFile("/proc/net/pnp"); err == nil {
		if rc := resolvConf(string(pnp), muslRoot("/")); rc != "" {
			if err := replaceFile("/etc/resolv.conf", rc); err != nil {
				return err
			}
			// Not waited for: the boot goes on at once (see warmResolver),
			// and only a WaitResolver boot waits for it once ready.
			if server := firstNameserver(rc); server != "" {
				warming = true
				go func() {
					defer close(resolved)
					m.warmResolver(net.JoinHostPort(server, "53"), warmUp)
				}()
			}
		}
	} else {
		m.logf("no DHCP answer to take a resolver from (%v)", err)
	}
	if !warming {
		close(resolved)
	}
	if err := mountShares(tags, binds); err != nil {
		return err
	}
	if err := mountDisks(spec.Disks); err != nil {
		return err
	}
	if spec.Egress != "" {
		spec.Env = noProxyHost(spec.Env, spec.Hostname)
		// Not a failed boot: ssh then goes by the VM's own NAT.
		env, note, err := sshEgress("/", spec.Env, guestAgent, spec.Hostname, sshVersion(spec.Env))
		switch {
		case err != nil:
			m.logf("ssh does not go through the outbound proxy: %v", err)
		case note != "":
			spec.Env = env
			m.logf("%s", note)
		}
	}
	if err := m.start(spec); err != nil {
		return err
	}
	m.mu.Lock()
	m.started = &spec
	m.mu.Unlock()
	m.linkUp()
	m.logf("the entrypoint started %v after the kernel (the boot was asked for at %v)", sinceBoot(), asked)
	if err := m.waitReady(spec.Ready); err != nil {
		return err
	}
	m.logf("the entrypoint was ready %v after the kernel", sinceBoot())
	if spec.WaitResolver {
		m.awaitResolver(resolved, warmUp+time.Second)
	}
	b, _ := json.Marshal(spec)
	if err := os.WriteFile(bootFile, b, 0o600); err != nil {
		return err
	}
	m.mu.Lock()
	m.spec = &spec
	m.mu.Unlock()
	return nil
}

// start starts the entrypoint in a process group of its own, so a stop
// reaches everything it started, as tini's TINI_KILL_PROCESS_GROUP does in
// a container. Its output is the console: the host's docker logs. Its
// user has its groups, as docker run gives them (userCred).
func (m *machine) start(spec agentproto.BootSpec) error {
	cred, err := userCred("/", spec.User)
	if err != nil {
		return err
	}
	cmd := &exec.Cmd{Path: spec.Cmd[0], Args: spec.Cmd, Env: spec.Env, Stdout: m.console, Stderr: m.console}
	cmd.Dir = getenv(spec.Env, "HOME")
	if cmd.Dir == "" {
		cmd.Dir = "/"
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred, Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", spec.Cmd[0], err)
	}
	exited := make(chan struct{})
	go func() {
		err := cmd.Wait()
		m.logf("the entrypoint exited: %v", err)
		close(exited)
	}()
	m.mu.Lock()
	m.entry, m.exited = cmd, exited
	m.mu.Unlock()
	return nil
}

// waitReady waits for the entrypoint's ready file, for as long as the
// entrypoint runs: the launcher decides how long is too long.
func (m *machine) waitReady(ready string) error {
	m.mu.Lock()
	exited := m.exited
	m.mu.Unlock()
	for {
		if _, err := os.Stat(ready); err == nil {
			return nil
		}
		select {
		case <-exited:
			return errors.New("the entrypoint exited before it was ready; what it said is on the console")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// execServer is the exec port's server for the sandbox as booted, or one
// that refuses until it is.
func (m *machine) execServer() *ExecServer {
	spec := m.booted()
	if spec == nil {
		return &ExecServer{Refuse: "the sandbox has not booted yet"}
	}
	dir := getenv(spec.Env, "HOME")
	if dir == "" {
		dir = "/"
	}
	return &ExecServer{Env: spec.Env, Dir: dir, User: spec.User}
}

// serveLink runs `caboose-agent link` on c as the sandbox's user, as the
// host's docker exec does in a container: its socket, and the files the
// relay touches, are the user's. From the entrypoint's start, not its
// ready file: the launcher links a VM as it boots (ensureRunning), so a
// connection that comes before even that waits for it (waitLinkable)
// rather than end at once, which the host would only retry after a pause.
// One that waits in vain is closed, as before; never under m.mu.
func (m *machine) serveLink(c io.ReadWriteCloser) {
	defer c.Close()
	f, ok := c.(*os.File)
	if !ok {
		return
	}
	spec := m.waitLinkable(linkWait)
	if spec == nil {
		m.logf("link: no boot to serve the host's connection (the entrypoint did not start within %v, or the machine is shutting down); closing it", linkWait)
		return
	}
	cred, err := userCred("/", spec.User)
	if err != nil {
		m.logf("link: %v", err)
		return
	}
	self, err := os.Executable()
	if err != nil {
		m.logf("link: %v", err)
		return
	}
	cmd := &exec.Cmd{Path: self, Args: []string{self, "link"}, Env: spec.Env, Stdin: f, Stdout: f, Stderr: m.console}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	if err := cmd.Run(); err != nil {
		m.logf("link: %v", err)
	}
}

// Shutdown is Guest's: the entrypoint's group gets a TERM and ten seconds,
// as docker stop gives a container, then every process a TERM and a KILL;
// then the disks are synced and the machine powers off.
func (m *machine) Shutdown() {
	m.doneOnce.Do(func() { close(m.done) })
	m.mu.Lock()
	entry, exited := m.entry, m.exited
	m.mu.Unlock()
	if entry != nil {
		_ = unix.Kill(-entry.Process.Pid, unix.SIGTERM)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
		}
	}
	// kill(-1) spares the init and this agent.
	_ = unix.Kill(-1, unix.SIGTERM)
	time.Sleep(time.Second)
	_ = unix.Kill(-1, unix.SIGKILL)
	unix.Sync()
	if err := unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
		m.logf("power off: %v", err)
	}
}

// keepLease keeps the DHCP lease the kernel's client took at boot (see
// dhcp.go), until Shutdown. A guest with no network, or none from DHCP,
// has none to keep. It holds UDP port 68, which the kernel's client let
// go before the init ran.
func (m *machine) keepLease() {
	defer func() {
		if r := recover(); r != nil {
			m.logf("DHCP: the renewals stopped: %v", r)
		}
	}()
	pnp, err := os.ReadFile("/proc/net/pnp")
	if err != nil {
		return
	}
	server := pnpServer(string(pnp))
	if server == nil {
		return
	}
	addr, hw, err := leasedAddr(server)
	if err != nil {
		m.logf("DHCP: no lease to keep: %v", err)
		return
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 68})
	if err != nil {
		m.logf("DHCP: no lease renewals: %v", err)
		return
	}
	defer conn.Close()
	c := &dhcpClient{conn: conn, server: &net.UDPAddr{IP: server, Port: 67}, addr: addr, hw: hw, wait: 4 * time.Second, tries: 3}
	keepLease(c, m.logf, m.done, dhcpTick)
}

// leasedAddr is the IPv4 address, and the MAC, of the interface on
// server's subnet: the one ip=dhcp configured.
func leasedAddr(server net.IP) (net.IP, net.HardwareAddr, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, nil, err
	}
	for _, ifc := range ifs {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && n.Contains(server) {
				return n.IP.To4(), ifc.HardwareAddr, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("no interface on the subnet of %v", server)
}

// bind is one mount of a share's directory at a target, read-only or not.
type bind struct {
	source, target string
	readOnly       bool
}

// planMounts checks the boot spec's mounts, and returns the shares to
// mount and the binds to make of them, in order.
func planMounts(ms []agentproto.GuestMount) (tags []string, binds []bind, err error) {
	seen := map[string]bool{}
	for _, gm := range ms {
		if gm.Tag == "" || strings.ContainsAny(gm.Tag, "/\x00") || gm.Tag == "." || gm.Tag == ".." {
			return nil, nil, fmt.Errorf("mount: bad share tag %q", gm.Tag)
		}
		if p := gm.Path; p != "" && (filepath.IsAbs(p) || filepath.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../")) {
			return nil, nil, fmt.Errorf("mount: path %q in share %s is not a clean relative path", p, gm.Tag)
		}
		if !filepath.IsAbs(gm.Target) || filepath.Clean(gm.Target) != gm.Target || gm.Target == "/" {
			return nil, nil, fmt.Errorf("mount: bad target %q", gm.Target)
		}
		if !seen[gm.Tag] {
			seen[gm.Tag] = true
			tags = append(tags, gm.Tag)
		}
		binds = append(binds, bind{source: filepath.Join(sharesDir, gm.Tag, gm.Path), target: gm.Target, readOnly: gm.ReadOnly})
	}
	return tags, binds, nil
}

// mountShares mounts each virtio-fs share once, then binds each of its
// directories (or files) at their targets, making a missing target as
// docker does, as root.
func mountShares(tags []string, binds []bind) error {
	for _, tag := range tags {
		dir := filepath.Join(sharesDir, tag)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := unix.Mount(tag, dir, "virtiofs", 0, ""); err != nil {
			return fmt.Errorf("mounting share %s: %w", tag, err)
		}
	}
	for _, b := range binds {
		st, err := os.Stat(b.source)
		if err != nil {
			return fmt.Errorf("mount: %w", err)
		}
		if st.IsDir() {
			err = os.MkdirAll(b.target, 0o755)
		} else if _, serr := os.Lstat(b.target); errors.Is(serr, os.ErrNotExist) {
			if err = os.MkdirAll(filepath.Dir(b.target), 0o755); err == nil {
				err = os.WriteFile(b.target, nil, 0o644)
			}
		}
		if err != nil {
			return fmt.Errorf("mount target %s: %w", b.target, err)
		}
		if err := unix.Mount(b.source, b.target, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("mounting %s at %s: %w", b.source, b.target, err)
		}
		// A bind takes no flags of its own until it is remounted.
		if b.readOnly {
			if err := unix.Mount("", b.target, "", unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY, ""); err != nil {
				return fmt.Errorf("making %s read-only: %w", b.target, err)
			}
		}
	}
	return nil
}

// vdName is a disk the boot spec may name: one after the root and the
// scratch disk.
var vdName = regexp.MustCompile(`^/dev/vd[c-z]$`)

// checkDisks checks the boot spec's disks before anything is mounted.
func checkDisks(ds []agentproto.GuestDisk) error {
	for _, d := range ds {
		if !vdName.MatchString(d.Device) {
			return fmt.Errorf("disk: bad device %q", d.Device)
		}
		if !filepath.IsAbs(d.Target) || filepath.Clean(d.Target) != d.Target || d.Target == "/" {
			return fmt.Errorf("disk: bad target %q", d.Target)
		}
	}
	return nil
}

// mountDisks mounts each ext4 disk at its target, making the target.
func mountDisks(ds []agentproto.GuestDisk) error {
	for _, d := range ds {
		if err := waitForFile(d.Device, 5*time.Second); err != nil {
			return err
		}
		if err := os.MkdirAll(d.Target, 0o755); err != nil {
			return err
		}
		if err := unix.Mount(d.Device, d.Target, "ext4", 0, ""); err != nil {
			return fmt.Errorf("mounting %s at %s: %w", d.Device, d.Target, err)
		}
	}
	return nil
}

// resolvConf is resolv.conf from what the kernel's DHCP client (ip=dhcp)
// wrote to /proc/net/pnp, or "" when it names no server. Its options make
// a lost query cost a second rather than the default five: vmnet's
// resolver loses the first query after a boot, and a lookup the sandbox
// makes before warmResolver's is answered is one of those. Every docker
// container in the guest copies this file. glibc and
// musl read them differently: glibc's timeout is per try, of attempts
// tries (at most 5), and musl's is the whole lookup's, retried every
// timeout/attempts. So each gets its own, both retrying after 1 s and
// giving up after 5 s (glibc's default gives up after 10, musl's after 5).
// A program of the other kind reads the other's options: on a musl root,
// a glibc container, or any Go program (Go reads them as glibc does),
// waits 5 s before it retries a lost query, and on a glibc root a musl
// container gives up after 1 s. No one line suits both, since the same
// timeout is a try's to one and the whole lookup's to the other; so the
// builder, whose first steps are such lookups (the probe in a user's
// image), waits for the warm-up (WaitResolver), after which vmnet loses
// none.
func resolvConf(pnp string, musl bool) string {
	var out []string
	servers := 0
	for _, line := range strings.Split(pnp, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		switch f[0] {
		case "nameserver":
			if f[1] != "0.0.0.0" {
				out = append(out, line)
				servers++
			}
		case "domain", "search":
			out = append(out, "search "+f[1])
		}
	}
	if servers == 0 {
		return ""
	}
	if musl {
		out = append(out, "options timeout:5 attempts:5")
	} else {
		out = append(out, "options timeout:1 attempts:5")
	}
	return strings.Join(out, "\n") + "\n"
}

// muslRoot says whether the system at root is a musl one, by its dynamic
// loader, which every dynamic musl system has.
func muslRoot(root string) bool {
	m, _ := filepath.Glob(filepath.Join(root, "lib", "ld-musl-*.so.1"))
	return len(m) > 0
}

// warmUp is the longest warmResolver asks for: vmnet's resolver took 1.3
// to 1.8 s to answer after a boot on the Mac (slice 13).
const warmUp = 4 * time.Second

// warmResolver asks the resolver at addr until it answers, for at most
// budget, and says on the console how long that took, or that it never
// answered. The first query after a boot is the one vmnet loses (the
// gateway's ARP, its resolver waking), and this sends it as early as the
// guest can. It runs beside the boot, never before the entrypoint: waiting
// for it cost every boot up to that 1.8 s, whereas a lookup the sandbox
// makes before the resolver wakes is only retried a second later
// (resolvConf's options, for the root's libc), and still answered well
// within its 5 s. A boot whose spec says WaitResolver waits for it once
// the entrypoint is ready, which it mostly is by then. It is
// the agent's, not the init's, holds nothing the boot or Shutdown waits
// for, starts no process, and its one socket closes when it returns.
func (m *machine) warmResolver(addr string, budget time.Duration) {
	took, err := askResolver(addr, budget)
	if err != nil {
		m.logf("the resolver at %s did not answer in %v: %v", addr, took.Round(time.Millisecond), err)
		return
	}
	m.logf("the resolver at %s answered in %v", addr, took.Round(time.Millisecond))
}

// awaitResolver waits for the warm-up to be over, for at most limit (it
// ends by its own budget, warmUp, well before), and says on the console
// when the boot waited for it at all.
func (m *machine) awaitResolver(resolved <-chan struct{}, limit time.Duration) {
	select {
	case <-resolved:
		return
	default:
	}
	start := time.Now()
	select {
	case <-resolved:
		m.logf("the boot waited %v for the resolver's warm-up", time.Since(start).Round(time.Millisecond))
	case <-time.After(limit):
		m.logf("the boot stopped waiting for the resolver's warm-up after %v", limit)
	}
}

// firstNameserver is resolv.conf's first nameserver, or "".
func firstNameserver(rc string) string {
	for _, line := range strings.Split(rc, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "nameserver" {
			return f[1]
		}
	}
	return ""
}

// askResolver sends addr a query for the root's name servers every
// quarter second, a new ID each time, until an answer to any of them
// comes back or budget runs out. What the answer says is no matter.
func askResolver(addr string, budget time.Duration) (time.Duration, error) {
	start := time.Now()
	deadline := start.Add(budget)
	c, err := net.DialTimeout("udp", addr, budget)
	if err != nil {
		return time.Since(start), err
	}
	defer c.Close()
	buf := make([]byte, 512)
	lastErr := errors.New("no answer")
	for id := uint16(1); time.Now().Before(deadline); id++ {
		next := time.Now().Add(250 * time.Millisecond)
		if next.After(deadline) {
			next = deadline
		}
		if _, err := c.Write(rootQuery(id)); err != nil {
			lastErr = err
		}
		_ = c.SetReadDeadline(next)
		for {
			n, err := c.Read(buf)
			if err != nil {
				if !errors.Is(err, os.ErrDeadlineExceeded) {
					lastErr = err // refused, say: wait out the quarter all the same
					time.Sleep(time.Until(next))
				}
				break
			}
			got := binary.BigEndian.Uint16(buf)
			if n >= 12 && buf[2]&0x80 != 0 && got >= 1 && got <= id {
				return time.Since(start), nil
			}
		}
	}
	return time.Since(start), lastErr
}

// rootQuery is a DNS query, recursion desired, for the root's NS records.
func rootQuery(id uint16) []byte {
	q := make([]byte, 17)
	binary.BigEndian.PutUint16(q[0:], id)
	binary.BigEndian.PutUint16(q[2:], 0x0100) // RD
	binary.BigEndian.PutUint16(q[4:], 1)      // QDCOUNT
	// q[12] is the root's empty name
	binary.BigEndian.PutUint16(q[13:], 2) // NS
	binary.BigEndian.PutUint16(q[15:], 1) // IN
	return q
}

func hostsFile(hostname string) string {
	s := "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n"
	if hostname != "" {
		s += "127.0.1.1\t" + hostname + "\n"
	}
	return s
}

// replaceFile writes a new file at path, over whatever was there: an
// image's /etc/resolv.conf may be a link into a /run the guest does not
// have.
func replaceFile(path, content string) error {
	_ = os.Remove(path)
	return os.WriteFile(path, []byte(content), 0o644)
}
