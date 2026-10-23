package conformance

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const (
	dN8    = "0a1b2c3d"
	dNonce = "0a1b2c3d4e5f60718293a4b5c6d7e8f9"
)

func TestHomePath(t *testing.T) {
	for _, rel := range []string{"data/home/.config/caboose/conf-0a1b2c3d-d-home", "home/envs/cf-00aa11bb/data/home/.config/caboose/conf-0a1b2c3d-d-home"} {
		p, err := HomePath(Canary{Rel: rel})
		if err != nil || p.String() != "/home/agent/.config/caboose/conf-0a1b2c3d-d-home" {
			t.Fatalf("%s: %v %v", rel, p, err)
		}
	}
	for _, rel := range []string{
		"data/home/x", "data/home/.config/caboose/conf-0a1b2c3d-d-home/sub", "home/conf-0a1b2c3d-d", "data/home/.config/caboose/conf-0a1b2c3d-",
		"data/home/.config/caboose/conf-0A1B2C3D-x", "roots/conf-0a1b2c3d-dev/part/conf-0a1b2c3d-x", "data/home/.config/caboose/../conf-0a1b2c3d-x",
		// The home's top level is the image's, not the data dir's: no mount puts it there.
		"data/home/conf-0a1b2c3d-d-home", "home/envs/default/data/home/.config/caboose/conf-0a1b2c3d-d-home",
		"x/data/home/.config/caboose/conf-0a1b2c3d-d-home",
	} {
		if _, err := HomePath(Canary{Rel: rel}); err == nil {
			t.Errorf("%s: accepted", rel)
		}
	}
	if _, err := ReadHome(HPath{"/etc/passwd"}); err == nil {
		t.Error("ReadHome took a path HomePath did not make")
	}
	if _, err := AppendHome(HPath{}); err == nil {
		t.Error("AppendHome took an empty path")
	}
}

func TestNewSockPath(t *testing.T) {
	for _, s := range []string{"/run/a.sock", "/tmp/tmux-1000/default", "/home/agent/.x/s@1:2+3"} {
		if _, err := NewSockPath(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	for _, s := range []string{
		"", "run/a.sock", "/run/../a", "/run/./a", "/run//a", "/run/a/", "/run/a b", "/run/a'b", "/run/$a",
		"/run/`a`", "/run/a;b", "/run/*", "/run/~", "/" + strings.Repeat("a", 103), "/run/a\nb",
	} {
		if _, err := NewSockPath(s); err == nil {
			t.Errorf("%q: accepted", s)
		}
	}
}

func TestScanBuilders(t *testing.T) {
	if _, err := ScanNames("0A1B2C3D"); err == nil {
		t.Error("ScanNames took a bad nonce")
	}
	for _, x := range [][2]string{{dN8, "ffffffff" + dNonce[8:]}, {dN8, dNonce + "'"}, {"bad", dNonce}, {dN8, "0a1b2c3d x"}} {
		if _, err := ScanContent(x[0], x[1]); err == nil {
			t.Errorf("ScanContent(%q, %q) accepted", x[0], x[1])
		}
	}
	n, err := ScanNames(dN8)
	if err != nil {
		t.Fatal(err)
	}
	cnt, err := ScanContent(dN8, dNonce)
	if err != nil {
		t.Fatal(err)
	}
	ls, err := ListSockets(dN8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ListSockets("x"); err == nil {
		t.Error("ListSockets took a bad nonce")
	}
	if !strings.Contains(ls.Body(), ScanComplete(dN8)) {
		t.Error("the socket listing does not print that it went through")
	}
	for _, cmd := range []Cmd{n, cnt, ls, ProcNetUnix()} {
		if cmd.Destructive() || !strings.HasSuffix(cmd.Body(), "1>&2") {
			t.Errorf("%q", cmd.Body())
		}
		shN(t, cmd.Body())
	}
	if !strings.Contains(n.Body(), ScanComplete(dN8)) || !strings.Contains(cnt.Body(), ScanComplete(dN8)) {
		t.Error("a scan does not print that it went through")
	}
}

// shN checks a body is valid sh, with no shell here running it.
func shN(t *testing.T, body string) {
	t.Helper()
	standIn := strings.ReplaceAll(body, "caboose-agent", "conf-agent-stand-in")
	if out, err := exec.Command("sh", "-n", "-c", "LC_ALL=C; { "+standIn+"; } >/dev/null").CombinedOutput(); err != nil {
		t.Errorf("not valid sh: %q: %v %s", body, err, out)
	}
}

func TestProbeBuilders(t *testing.T) {
	link, _ := NewSockPath("/run/l.sock")
	d, _ := NewSockPath("/run/d.sock")
	if _, err := ProbeDaemon(dN8, dNonce, link, SockPath{}); err == nil {
		t.Error("ProbeDaemon took no socket")
	}
	if _, err := ProbeDaemon(dN8, dNonce, SockPath{}, d); err == nil {
		t.Error("ProbeDaemon took no link")
	}
	if _, err := ProbeDaemon("ffffffff", dNonce, link, d); err == nil {
		t.Error("ProbeDaemon took a nonce of another run")
	}
	for _, only := range [][]string{nil, {"daemon-hello"}, {"answer", "nosuch"}, {"answer,grant"}} {
		if _, err := ProbeLink(dN8, dNonce, link, only); err == nil {
			t.Errorf("ProbeLink(%q) accepted", only)
		}
	}
	pd, err := ProbeDaemon(dN8, dNonce, link, d)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := ProbeLink(dN8, dNonce, link, []string{"answer", "grant"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []Cmd{pd, pl} {
		if !strings.HasPrefix(c.Body(), "/work/conf-0a1b2c3d-dev/part/.conformance-probe --nonce "+dNonce+" --link '/run/l.sock' ") {
			t.Errorf("%q", c.Body())
		}
		shN(t, strings.Replace(c.Body(), "/work/conf-0a1b2c3d-dev/part/.conformance-probe", "conf-probe-stand-in", 1))
	}
	if rel, err := ProbeRel(dN8); err != nil || rel != "roots/conf-0a1b2c3d-dev/part/.conformance-probe" {
		t.Errorf("%q %v", rel, err)
	}
}

func TestAgentDaemonBuilders(t *testing.T) {
	for _, u := range []string{"http://127.0.0.1:7171/", "http://[::1]:7171/#token=x", "http://0x7f000001:7171/", "http://example.invalid/"} {
		c, err := AgentOpen(u)
		if err != nil {
			t.Errorf("%s: %v", u, err)
			continue
		}
		shN(t, c.Body())
	}
	for _, u := range []string{"file:///etc/passwd", "http://a'b/", "http://a b/", "http://$x/", "http://a/`x`", "javascript:x", "http://a/?q=1", "http://a/" + strings.Repeat("a", 40)} {
		if _, err := AgentOpen(u); err == nil {
			t.Errorf("%q: accepted", u)
		}
	}
	if _, err := AgentRequestAlias("conf-ask", "it's"); err == nil {
		t.Error("a quote in a request's text")
	}
	if _, err := AgentRequests("--approve", "1;x"); err == nil {
		t.Error("a semicolon in requests' arguments")
	}
	notify, _ := AgentNotify("x")
	f, err := Flood(40, notify)
	if err != nil {
		t.Fatal(err)
	}
	shN(t, f.Body())
	req, _ := AgentRequestAlias("conf-ask", "x")
	if _, err := Flood(10, req); err != nil {
		t.Errorf("a request flood: %v", err)
	}
	rm, _ := Remove(SPath{"/work/conf-0a1b2c3d-dev/part/conf-0a1b2c3d-x"})
	for name, c := range map[string]struct {
		n   int
		cmd Cmd
	}{
		"one":         {1, notify},
		"too many":    {65, notify},
		"not agent":   {10, Cmd{body: "touch /x"}},
		"destructive": {10, rm},
		"chained":     {10, Cmd{body: "caboose-agent notify 'x'; touch /x"}},
		"empty":       {10, Cmd{}},
	} {
		if _, err := Flood(c.n, c.cmd); err == nil {
			t.Errorf("Flood %s: accepted", name)
		}
	}
	for _, pid := range []int{0, -1, 1 << 23} {
		if _, err := KillZero(pid); err == nil {
			t.Errorf("KillZero(%d) accepted", pid)
		}
	}
	if _, err := KillTerm(1); err == nil {
		t.Error("KillTerm(1) accepted")
	}
	for _, x := range []struct {
		host string
		port int
		how  string
	}{{"evil.example", 7171, UINoToken}, {"127.0.0.1", 0, UINoToken}, {"127.0.0.1", 7171, "with-token"}, {"127.0.0.1'", 7171, UINoToken}} {
		if _, err := CurlUI(x.host, x.port, x.how); err == nil {
			t.Errorf("CurlUI(%q, %d, %q) accepted", x.host, x.port, x.how)
		}
	}
	for _, host := range []string{"127.0.0.1", "host.docker.internal", "fd00::1", "::ffff:192.168.1.2"} {
		for _, how := range []string{UINoToken, UIWrongToken, UIForeign} {
			c, err := CurlUI(host, 7171, how)
			if err != nil {
				t.Errorf("%s %s: %v", host, how, err)
				continue
			}
			if strings.Contains(c.Body(), "Caboose-Token") != (how == UIWrongToken) {
				t.Errorf("%s %s: %q", host, how, c.Body())
			}
			shN(t, c.Body())
		}
	}
	c, _ := CurlUI("::ffff:192.168.1.2", 7171, UINoToken)
	if !strings.Contains(c.Body(), "'http://192.168.1.2:7171/api/requests'") {
		t.Errorf("a mapped address is not unmapped: %q", c.Body())
	}
	c, _ = CurlUI("fd00::1", 7171, UINoToken)
	if !strings.Contains(c.Body(), "'http://[fd00::1]:7171/api/requests'") {
		t.Errorf("an IPv6 address is not bracketed: %q", c.Body())
	}
}

// SameSocket, run by a real shell: a socket and a path through a link to
// its directory are the same file, another socket is not, and a path that
// is no socket (gone since it was found) prints SocketGone alone.
func TestSameSocketInShell(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "real")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a.sock", "b.sock"} {
		l, err := net.Listen("unix", filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
	}
	if err := os.Symlink("real", dir+"-link"); err != nil {
		t.Fatal(err)
	}
	sp := func(p string) SockPath {
		s, err := NewSockPath(p)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	bound := []SockPath{sp(dir + "/b.sock"), sp(dir + "/a.sock")}
	for _, x := range []struct{ s, want string }{
		{dir + "-link/a.sock", dir + "/a.sock"},
		{dir + "/b.sock", dir + "/b.sock"},
		{dir + "/gone.sock", SocketGone},
	} {
		cmd, err := SameSocket(sp(x.s), bound)
		if err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("sh", "-c", cmd.Body()).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v %s", x.s, err, out)
		}
		if got := strings.TrimSpace(string(out)); got != x.want {
			t.Errorf("%s: %q, want %q", x.s, got, x.want)
		}
	}
}

// SocketHolders, run by a real shell, finds this process holding the
// socket it listens on, by the inode /proc/net/unix gives it; ProcExe
// names this process's executable.
func TestSocketHoldersInShell(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("no /proc")
	}
	l, err := net.Listen("unix", filepath.Join(t.TempDir(), "h.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	f, err := l.(*net.UnixListener).File()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		t.Fatal(err)
	}
	cmd, err := SocketHolders("0a1b2c3d")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-c", cmd.Body()).CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	ls := strings.Split(strings.TrimSpace(string(out)), "\n")
	want := fmt.Sprintf("socket:[%d]", st.Ino)
	prefix := fmt.Sprintf("/proc/%d/fd/", os.Getpid())
	found := false
	for i := 0; i+1 < len(ls); i++ {
		if ls[i] == want && strings.HasPrefix(ls[i+1], prefix) {
			found = true
		}
	}
	if last := ls[len(ls)-1]; !found || (last != ScanComplete("0a1b2c3d")+" 0" && last != ScanComplete("0a1b2c3d")+" 1") {
		t.Errorf("%s held by %d not found, or no last line: %q", want, os.Getpid(), ls)
	}
	cmd, err = ProcExe(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	out, err = exec.Command("sh", "-c", cmd.Body()).CombinedOutput()
	if got := strings.TrimSpace(string(out)); err != nil || got != exe {
		t.Errorf("%q %v, want %q", got, err, exe)
	}
	for _, bad := range []int{0, -1, 1 << 23} {
		if _, err := ProcExe(bad); err == nil {
			t.Errorf("pid %d taken", bad)
		}
	}
}

// DaemonRefusals counts the daemon's own lines for a connection it took
// and closed without a frame, and nothing else.
func TestDaemonRefusals(t *testing.T) {
	log := "2026/10/20 10:00:00 caboose daemon v1 started (pid 1, protocol 3, idle exit after 1m0s)\n" +
		"2026/10/20 10:00:01 refused a connection: uid 0 is not this daemon's user 501\n" +
		"2026/10/20 10:00:02 refused a connection: 64 are open already\n" +
		"2026/10/20 10:00:03 a client's hello: read: i/o timeout\n" +
		"2026/10/20 10:00:04 refused a connection whose hello names the kind \"web\"\n" +
		"2026/10/20 10:00:05 replace: x asks for v2; no host command runs: exiting\n"
	if n := DaemonRefusals([]byte(log)); n != 4 {
		t.Errorf("%d, want 4", n)
	}
	if n := DaemonRefusals(nil); n != 0 {
		t.Errorf("%d for nothing", n)
	}
}

// The witness's lines are the daemon's own: each is in a log call of its
// server, so a reworded log line fails here, not on a Mac run.
func TestRefusalLinesAreTheDaemons(t *testing.T) {
	src, err := os.ReadFile("../daemon/server.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range refusalLines {
		if !strings.Contains(string(src), `s.log.Printf("`+l) {
			t.Errorf("the daemon logs no %q", l)
		}
	}
}

// A log moved aside between two marks is told, whether the new one is
// shorter or has grown past the old size: its first line is cappedlog's.
func TestDaemonLogRotation(t *testing.T) {
	start := "2026/10/20 10:00:00 caboose daemon v1 started\n"
	refusal := "2026/10/20 10:00:01 a client's hello: EOF\n"
	before := MarkDaemonLog(nil, []byte(start+refusal))
	if before.Refusals != 1 {
		t.Fatalf("%+v", before)
	}
	grown := MarkDaemonLog(nil, []byte(start+refusal+refusal))
	if grown.RotatedSince(before) || grown.Refusals != 2 {
		t.Errorf("an appended log taken as rotated: %+v", grown)
	}
	header := "2026/10/20 10:00:02 caboose daemon: daemon.log reached 10 MiB: the lines before are in daemon.log.1\n"
	for name, m := range map[string]DaemonLogMark{
		"shorter": MarkDaemonLog([]byte(start+refusal+refusal), []byte(header)),
		"longer":  MarkDaemonLog([]byte(start+refusal+refusal), []byte(header+strings.Repeat(refusal, 3))),
	} {
		if !m.RotatedSince(before) {
			t.Errorf("%s: a rotated log not told: %+v", name, m)
		}
	}
	if !MarkDaemonLog(nil, nil).RotatedSince(before) {
		t.Error("a log gone not told")
	}
	if MarkDaemonLog(nil, []byte(start)).RotatedSince(MarkDaemonLog(nil, nil)) {
		t.Error("a log that was empty and has a first line taken as rotated")
	}
}
