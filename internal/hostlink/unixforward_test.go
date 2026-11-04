package hostlink

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agent"
	"github.com/bfreis/caboose/internal/agentproto"
)

// shortDir is a directory whose path leaves room for a socket's: t.TempDir's
// can be over the limit.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cbu")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// unixLink is a Host and a real agent, in process, with one forward configured
// per name.
type unixLink struct {
	t       *testing.T
	base    string
	trust   string // "" is fallible
	sess    *agentproto.Session
	done    chan error
	mu      sync.Mutex
	reports [][]agentproto.SocketInfo
	logs    bytes.Buffer
	socket  string
}

// Write is the link's log.
func (l *unixLink) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.logs.Write(b)
}

func (l *unixLink) logged() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.logs.String()
}

func (l *unixLink) lastReport() []agentproto.SocketInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.reports) == 0 {
		return nil
	}
	return l.reports[len(l.reports)-1]
}

func (l *unixLink) hostPath(name string) string { return filepath.Join(l.base, "h", name+".sock") }
func (l *unixLink) sandboxPath(name string) string {
	return filepath.Join(l.base, "s", name+".sock")
}

// newUnixLink sets the directories up; a test may leave things at the
// sockets' places before start.
func newUnixLink(t *testing.T) *unixLink {
	t.Helper()
	l := &unixLink{t: t, base: shortDir(t), done: make(chan error, 1)}
	if err := os.Mkdir(filepath.Join(l.base, "s"), 0o700); err != nil {
		t.Fatal(err)
	}
	return l
}

func (l *unixLink) cfg(names ...string) Config {
	cfg := Config{Log: log.New(l, "", 0), Trust: cmp.Or(l.trust, "fallible"), HostListens: func(int) bool { return false }}
	for _, n := range names {
		cfg.UnixForwards = append(cfg.UnixForwards, UnixForward{Name: n, Sandbox: l.sandboxPath(n), Host: l.hostPath(n)})
	}
	cfg.UnixForwarded = func(s []agentproto.SocketInfo) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.reports = append(l.reports, s)
	}
	return cfg
}

// start runs a real agent and a Host, and waits for the report of the sockets.
func (l *unixLink) start(names ...string) {
	l.t.Helper()
	if runtime.GOOS != "linux" {
		l.t.Skip("the agent reads /proc")
	}
	hr, aw := io.Pipe()
	ar, hw := io.Pipe()
	l.socket = filepath.Join(l.base, "a")
	go agent.RunLink(ar, aw, agent.Config{Socket: l.socket, ProcRoot: "/proc", Interval: time.Hour})
	l.run(hr, hw, names)
}

func (l *unixLink) run(r io.Reader, w io.WriteCloser, names []string) {
	l.t.Helper()
	l.sess = agentproto.NewSession(r, w, true)
	l.t.Cleanup(func() { l.sess.Close() })
	go func() { l.done <- Run(l.sess, l.cfg(names...)) }()
	waitUntil(l.t, "the report of the sockets", func() bool { return l.lastReport() != nil })
}

func waitUntil(t *testing.T, what string, f func() bool) {
	t.Helper()
	for range 500 {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %s", what)
}

// serveUnix listens at path and hands each connection to f.
func serveUnix(t *testing.T, path string, f func(*net.UnixConn)) {
	t.Helper()
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.AcceptUnix()
			if err != nil {
				return
			}
			go f(c)
		}
	}()
}

func dialHost(t *testing.T, path string) *net.UnixConn {
	t.Helper()
	c, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	return c.(*net.UnixConn)
}

// Bytes go both ways; each side's half close reaches the other, which can
// still answer after it.
func TestUnixForwardHalfClose(t *testing.T) {
	l := newUnixLink(t)
	serveUnix(t, l.sandboxPath("app"), func(c *net.UnixConn) {
		defer c.Close()
		// All that came, once the client closed its side; then the answer.
		b, err := io.ReadAll(c)
		if err != nil {
			return
		}
		_, _ = c.Write(append([]byte("got "), b...))
	})
	l.start("app")
	c := dialHost(t, l.hostPath("app"))
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "got hello" {
		t.Fatalf("read %q, %v", got, err)
	}
}

// A server's half close reaches the client, which can still send.
func TestUnixForwardServerHalfClose(t *testing.T) {
	l := newUnixLink(t)
	got := make(chan string, 1)
	serveUnix(t, l.sandboxPath("app"), func(c *net.UnixConn) {
		defer c.Close()
		_, _ = c.Write([]byte("banner"))
		_ = c.CloseWrite()
		b, _ := io.ReadAll(c)
		got <- string(b)
	})
	l.start("app")
	c := dialHost(t, l.hostPath("app"))
	b, err := io.ReadAll(c)
	if err != nil || string(b) != "banner" {
		t.Fatalf("read %q, %v", b, err)
	}
	if _, err := c.Write([]byte("after")); err != nil {
		t.Fatal(err)
	}
	c.CloseWrite()
	select {
	case s := <-got:
		if s != "after" {
			t.Errorf("the server read %q", s)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server read nothing")
	}
}

// More than any window or frame, both ways at once.
func TestUnixForwardLargeTransfer(t *testing.T) {
	l := newUnixLink(t)
	serveUnix(t, l.sandboxPath("app"), func(c *net.UnixConn) {
		defer c.Close()
		_, _ = io.Copy(c, c)
		_ = c.CloseWrite()
	})
	l.start("app")
	c := dialHost(t, l.hostPath("app"))
	data := make([]byte, 16<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = c.Write(data)
		_ = c.CloseWrite()
	}()
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("read %d bytes, sent %d; equal: false", len(got), len(data))
	}
}

// The same under hostile as under fallible: only the host's own user reaches
// the socket, and config.toml alone decides it.
func TestUnixForwardUnderHostile(t *testing.T) {
	l := newUnixLink(t)
	l.trust = "hostile"
	serveUnix(t, l.sandboxPath("app"), func(c *net.UnixConn) {
		defer c.Close()
		_, _ = c.Write([]byte("ok"))
	})
	l.start("app")
	got, err := io.ReadAll(dialHost(t, l.hostPath("app")))
	if err != nil || string(got) != "ok" {
		t.Fatalf("read %q, %v", got, err)
	}
}

// With nothing listening in the sandbox, the connection closes at once.
func TestUnixForwardNothingListening(t *testing.T) {
	l := newUnixLink(t)
	l.start("app")
	c := dialHost(t, l.hostPath("app"))
	start := time.Now()
	_ = c.SetReadDeadline(start.Add(5 * time.Second))
	n, err := c.Read(make([]byte, 1))
	var ne net.Error
	if n != 0 || err == nil || errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("read %d, %v: the connection did not close", n, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("it took %v to close", d)
	}
}

// The socket is 0600 and its directory 0700, whatever the directory was and
// whatever the umask gave.
func TestUnixForwardModes(t *testing.T) {
	l := newUnixLink(t)
	dir := filepath.Dir(l.hostPath("app"))
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	l.start("app")
	fi, err := os.Lstat(dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the directory: %v, %v", fi.Mode(), err)
	}
	fi, err = os.Lstat(l.hostPath("app"))
	if err != nil || fi.Mode().Perm() != 0o600 || fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("the socket: %v, %v", fi.Mode(), err)
	}
	if r := l.lastReport(); len(r) != 1 || r[0].Reason != "" || r[0].HostPath != l.hostPath("app") {
		t.Errorf("the report: %+v", r)
	}
}

func TestListenUnixForwardFreshDirIsPrivate(t *testing.T) {
	path := filepath.Join(shortDir(t), "new", "a.sock")
	l, err := listenUnixForward(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.ln.Close()
	fi, err := os.Lstat(filepath.Dir(path))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the directory: %v, %v", fi.Mode(), err)
	}
}

// What a dead link left is replaced.
func TestUnixForwardStaleSocketReplaced(t *testing.T) {
	path := filepath.Join(shortDir(t), "a.sock")
	old, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	old.SetUnlinkOnClose(false)
	old.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("the leftover is gone already: %v", err)
	}
	l, err := listenUnixForward(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.ln.Close()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("the new socket: %v", err)
	}
	c.Close()
}

// Anything but a socket at the path stays, untouched, and is refused, saying
// what to do; another forward goes on.
func TestUnixForwardRefusesWhatIsNoSocket(t *testing.T) {
	for name, place := range map[string]func(t *testing.T, path, base string){
		"regular file": func(t *testing.T, path, _ string) {
			if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"symlink to a file": func(t *testing.T, path, base string) {
			target := filepath.Join(base, "target")
			if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		},
		"symlink to a socket": func(t *testing.T, path, base string) {
			target := filepath.Join(base, "target")
			ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: target, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ln.Close() })
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		},
		"dangling symlink": func(t *testing.T, path, base string) {
			if err := os.Symlink(filepath.Join(base, "nowhere"), path); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(t *testing.T, path, _ string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			l := newUnixLink(t)
			if err := os.MkdirAll(filepath.Dir(l.hostPath("bad")), 0o700); err != nil {
				t.Fatal(err)
			}
			place(t, l.hostPath("bad"), l.base)
			before, err := os.Lstat(l.hostPath("bad"))
			if err != nil {
				t.Fatal(err)
			}
			serveUnix(t, l.sandboxPath("good"), func(c *net.UnixConn) { c.Close() })
			l.start("bad", "good")
			r := l.lastReport()
			if len(r) != 2 || !strings.Contains(r[0].Reason, "not a socket") || !strings.Contains(r[0].Reason, "caboose link --restart") || r[1].Reason != "" {
				t.Fatalf("the report: %+v", r)
			}
			after, err := os.Lstat(l.hostPath("bad"))
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatalf("what was there changed: %v, %v", after, err)
			}
			if _, err := os.Lstat(l.hostPath("good")); err != nil {
				t.Errorf("the other forward: %v", err)
			}
			// And the link's end leaves it too.
			l.sess.Close()
			<-l.done
			if after, err := os.Lstat(l.hostPath("bad")); err != nil || !os.SameFile(before, after) {
				t.Errorf("the link's end took it: %v", err)
			}
			if b, err := os.ReadFile(filepath.Join(l.base, "target")); err == nil && string(b) != "keep" {
				t.Errorf("the link's target was changed: %q", b)
			}
		})
	}
}

// A symlink at the directory's place is refused, and nothing is made through
// it.
func TestUnixForwardRefusesSymlinkedDir(t *testing.T) {
	base := shortDir(t)
	target := filepath.Join(base, "real")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "d")
	if err := os.Symlink(target, dir); err != nil {
		t.Fatal(err)
	}
	_, err := listenUnixForward(filepath.Join(dir, "a.sock"))
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("listen: %v", err)
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Errorf("made through the link: %v", entries)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o755 {
		t.Errorf("the link's target's mode changed: %v", fi.Mode())
	}
}

// A path over what a Mac's socket takes is refused on every host, and says
// what to do.
func TestUnixForwardPathTooLong(t *testing.T) {
	base := shortDir(t)
	dir := filepath.Join(base, strings.Repeat("d", MaxSocketPath-len(base)-len("/x.sock")-1))
	fits := filepath.Join(dir, "x.sock")
	if len(fits) != MaxSocketPath {
		t.Fatalf("the test's own path is %d bytes", len(fits))
	}
	l, err := listenUnixForward(fits)
	if err != nil {
		t.Fatalf("a path of %d bytes: %v", len(fits), err)
	}
	l.ln.Close()
	over := filepath.Join(dir, "xy.sock")
	_, err = listenUnixForward(over)
	if err == nil || !strings.Contains(err.Error(), "shorter name") || !strings.Contains(err.Error(), "CABOOSE_HOME") {
		t.Fatalf("a path of %d bytes: %v", len(over), err)
	}
	if _, err := os.Lstat(over); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("something is at the path: %v", err)
	}
}

// An agent whose hello does not say it serves them gets no listener, and the
// report says to restart. It never gets a stream either.
func TestUnixForwardOlderAgent(t *testing.T) {
	l := newUnixLink(t)
	if err := os.MkdirAll(filepath.Dir(l.hostPath("app")), 0o700); err != nil {
		t.Fatal(err)
	}
	// A socket a dead link left must not stay to refuse clients.
	old, err := net.ListenUnix("unix", &net.UnixAddr{Name: l.hostPath("app"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	old.SetUnlinkOnClose(false)
	old.Close()

	hr, aw := io.Pipe()
	ar, hw := io.Pipe()
	older := agentproto.NewSession(ar, aw, false)
	defer older.Close()
	// In its own goroutine: the pipe holds nothing until the Host reads.
	go older.Send(agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version})
	var sockets []string
	var smu sync.Mutex
	go func() {
		for b := range older.Control() {
			if m, err := agentproto.Decode(b); err == nil && m.Type == agentproto.TypeSockets {
				smu.Lock()
				sockets = append(sockets, string(b))
				smu.Unlock()
			}
		}
	}()
	l.run(hr, hw, []string{"app"})
	r := l.lastReport()
	if len(r) != 1 || r[0].Reason != ReasonSocketOldAgent || !strings.Contains(r[0].Reason, "caboose restart") {
		t.Fatalf("the report: %+v", r)
	}
	if _, err := os.Lstat(l.hostPath("app")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("something is at the socket's path: %v", err)
	}
	smu.Lock()
	defer smu.Unlock()
	if len(sockets) != 0 {
		t.Errorf("sent an older agent %v", sockets)
	}
}

// What is on the wire names the configured path, in the stream's header, and
// the hello that asks for it is the agent's only say.
func TestUnixForwardHeaderIsTheHosts(t *testing.T) {
	l := newUnixLink(t)
	hr, aw := io.Pipe()
	ar, hw := io.Pipe()
	peer := agentproto.NewSession(ar, aw, false)
	defer peer.Close()
	go peer.Send(agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version, SocketForwards: true})
	go func() {
		for range peer.Control() {
		}
	}()
	l.run(hr, hw, []string{"app"})
	c := dialHost(t, l.hostPath("app"))
	defer c.Close()
	st, err := peer.Accept()
	if err != nil {
		t.Fatal(err)
	}
	var h agentproto.StreamHeader
	if err := json.Unmarshal(st.Header(), &h); err != nil || h.Socket != l.sandboxPath("app") || h.Port != 0 {
		t.Fatalf("header %s: %v", st.Header(), err)
	}
}

// The agent dials only what can name a socket; a bad header closes the stream.
func TestAgentRefusesBadSocketHeader(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the agent reads /proc")
	}
	base := shortDir(t)
	hr, aw := io.Pipe()
	ar, hw := io.Pipe()
	go agent.RunLink(ar, aw, agent.Config{Socket: base + "/a", ProcRoot: "/proc", Interval: time.Hour})
	sess := agentproto.NewSession(hr, hw, true)
	defer sess.Close()
	go func() {
		for range sess.Control() {
		}
	}()
	// A relative path would be dialled from the agent's own directory.
	if err := os.Chdir(base); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir("/") })
	accepted := make(chan struct{}, 1)
	serveUnix(t, base+"/rel", func(c *net.UnixConn) { accepted <- struct{}{}; c.Close() })
	for _, bad := range []string{"rel", "/" + strings.Repeat("a", agentproto.MaxSocketPath)} {
		hdr, _ := json.Marshal(agentproto.StreamHeader{Socket: bad})
		st, err := sess.Open(hdr)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := st.Read(make([]byte, 1))
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%q: the stream gave data", bad)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%q: the stream stayed open", bad)
		}
	}
	select {
	case <-accepted:
		t.Error("the agent dialled a relative path")
	default:
	}
}

// At most maxUnixConns at once, across the forwards; one over is closed at
// once and logged no more than once in a while; one that ends makes room.
func TestUnixForwardConnectionBound(t *testing.T) {
	l := newUnixLink(t)
	for _, n := range []string{"a", "b"} {
		serveUnix(t, l.sandboxPath(n), func(c *net.UnixConn) {
			defer c.Close()
			_, _ = io.Copy(c, c)
		})
	}
	l.start("a", "b")
	ping := func(c *net.UnixConn) error {
		if _, err := c.Write([]byte("x")); err != nil {
			return err
		}
		_, err := io.ReadFull(c, make([]byte, 1))
		return err
	}
	var held []*net.UnixConn
	for i := range maxUnixConns {
		c := dialHost(t, l.hostPath([]string{"a", "b"}[i%2]))
		if err := ping(c); err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		held = append(held, c)
	}
	for range 5 {
		over := dialHost(t, l.hostPath("a"))
		_ = over.SetDeadline(time.Now().Add(5 * time.Second))
		if err := ping(over); err == nil {
			t.Fatal("a connection over the bound was carried")
		} else {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Fatalf("a connection over the bound stayed open: %v", err)
			}
		}
	}
	if n := strings.Count(l.logged(), "a connection refused"); n != 1 {
		t.Errorf("logged the refusal %d times:\n%s", n, l.logged())
	}
	held[0].Close()
	waitUntil(t, "room for another connection", func() bool {
		c := dialHost(t, l.hostPath("b"))
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		return ping(c) == nil
	})
}

// The link's end takes the socket with it and ends what it carried.
func TestUnixForwardLinkStopRemovesSocket(t *testing.T) {
	l := newUnixLink(t)
	serveUnix(t, l.sandboxPath("app"), func(c *net.UnixConn) {
		defer c.Close()
		_, _ = io.Copy(c, c)
	})
	l.start("app")
	c := dialHost(t, l.hostPath("app"))
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(c, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	l.sess.Close()
	select {
	case <-l.done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	if _, err := os.Lstat(l.hostPath("app")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the socket stayed: %v", err)
	}
	if fi, err := os.Lstat(filepath.Dir(l.hostPath("app"))); err != nil || !fi.IsDir() {
		t.Errorf("its directory: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := c.Read(make([]byte, 1)); n != 0 || err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("the connection goes on: %d, %v", n, err)
	}
	if r := l.lastReport(); r != nil {
		t.Errorf("the last report is %+v, not none", r)
	}
}

// `caboose-agent ports` shows them to a session, with or without TCP ports.
func TestUnixForwardShownToSession(t *testing.T) {
	l := newUnixLink(t)
	if err := os.MkdirAll(filepath.Dir(l.hostPath("bad")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.hostPath("bad"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	l.start("app", "bad")
	var out string
	waitUntil(t, "the sockets in ports' output", func() bool {
		var b strings.Builder
		if agent.Ports(l.socket, &b) != nil {
			return false
		}
		out = b.String()
		return strings.Contains(out, "socket app")
	})
	for _, want := range []string{"socket app (" + l.sandboxPath("app") + ") forwarded from the host's " + l.hostPath("app"),
		"socket bad (" + l.sandboxPath("bad") + ") not forwarded: "} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
}

// With none configured, nothing is made or reported.
func TestUnixForwardNoneConfigured(t *testing.T) {
	l := newUnixLink(t)
	if runtime.GOOS != "linux" {
		t.Skip("the agent reads /proc")
	}
	hr, aw := io.Pipe()
	ar, hw := io.Pipe()
	go agent.RunLink(ar, aw, agent.Config{Socket: l.base + "/a", ProcRoot: "/proc", Interval: time.Hour})
	sess := agentproto.NewSession(hr, hw, true)
	done := make(chan error, 1)
	cfg := l.cfg()
	go func() { done <- Run(sess, cfg) }()
	waitUntil(t, "the agent's socket", func() bool {
		_, err := os.Stat(l.base + "/a")
		return err == nil
	})
	time.Sleep(50 * time.Millisecond)
	sess.Close()
	<-done
	if _, err := os.Lstat(filepath.Dir(l.hostPath("x"))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the directory was made: %v", err)
	}
	if len(l.reports) != 0 {
		t.Errorf("reports: %v", l.reports)
	}
}

// A socket something answers on is another link's: a second forward starting
// leaves it, says so, and takes nothing with it as it stops.
func TestUnixForwardLiveSocketLeftAlone(t *testing.T) {
	l := newUnixLink(t)
	if err := os.MkdirAll(filepath.Dir(l.hostPath("app")), 0o700); err != nil {
		t.Fatal(err)
	}
	live, err := net.ListenUnix("unix", &net.UnixAddr{Name: l.hostPath("app"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	before, err := os.Lstat(l.hostPath("app"))
	if err != nil {
		t.Fatal(err)
	}
	l.start("app")
	r := l.lastReport()
	if len(r) != 1 || !strings.Contains(r[0].Reason, "already answers") || !strings.Contains(r[0].Reason, "CABOOSE_DATA_DIR") {
		t.Fatalf("the report: %+v", r)
	}
	l.sess.Close()
	<-l.done
	after, err := os.Lstat(l.hostPath("app"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("the live socket: %v", err)
	}
	c, err := net.Dial("unix", l.hostPath("app"))
	if err != nil {
		t.Fatalf("it no longer answers: %v", err)
	}
	c.Close()
}

// An older agent's link leaves a live socket too, while it clears a dead one.
func TestClearStaleSocketLeavesLive(t *testing.T) {
	path := filepath.Join(shortDir(t), "a.sock")
	live, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if err := clearStaleSocket(path); err == nil || !strings.Contains(err.Error(), "already answers") {
		t.Errorf("a live socket: %v", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("a live socket was removed: %v", err)
	}
	live.SetUnlinkOnClose(false)
	live.Close()
	if err := clearStaleSocket(path); err != nil {
		t.Errorf("a dead socket: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a dead socket stayed: %v", err)
	}
}

// A link that stops takes only the socket it made: one that has since taken
// the path stays.
func TestUnixForwardStopLeavesSuccessor(t *testing.T) {
	path := filepath.Join(shortDir(t), "a.sock")
	first, err := listenUnixForward(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.ln.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// Kept open so that the first's inode cannot be used for the successor.
	hold, err := net.ListenUnix("unix", &net.UnixAddr{Name: path + "2", Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	succ, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer succ.Close()
	if err := removeOwnSocket(path, first.own); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("the successor was removed: %v", err)
	}
	own, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := removeOwnSocket(path, own); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("its own socket stayed: %v", err)
	}
}

// A connection whose stream cannot be opened gives its place back: with the
// session's streams all taken, more than the bound of them fail, and once there
// is room one is carried.
func TestUnixForwardOpenFailureReleases(t *testing.T) {
	l := newUnixLink(t)
	hr, aw := io.Pipe()
	ar, hw := io.Pipe()
	peer := agentproto.NewSession(ar, aw, false)
	defer peer.Close()
	go peer.Send(agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version, SocketForwards: true})
	go func() {
		for range peer.Control() {
		}
	}()
	carried := make(chan struct{}, 1)
	go func() {
		for {
			st, err := peer.Accept()
			if err != nil {
				return
			}
			var h agentproto.StreamHeader
			if json.Unmarshal(st.Header(), &h) == nil && h.Socket != "" {
				carried <- struct{}{}
			}
		}
	}()
	l.run(hr, hw, []string{"app"})
	var held []*agentproto.Stream
	for range agentproto.MaxStreams {
		st, err := l.sess.Open([]byte(`{"name":"filler"}`))
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, st)
	}
	for i := range maxUnixConns + 1 {
		c := dialHost(t, l.hostPath("app"))
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Read(make([]byte, 1)); errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("connection %d stayed open with no stream for it", i)
		}
		c.Close()
	}
	held[0].Close()
	waitUntil(t, "a connection carried once there is room", func() bool {
		c := dialHost(t, l.hostPath("app"))
		defer c.Close()
		select {
		case <-carried:
			return true
		case <-time.After(50 * time.Millisecond):
			return false
		}
	})
}

func TestSocketIsDead(t *testing.T) {
	for err, want := range map[error]bool{
		syscall.ECONNREFUSED: true, syscall.ENOENT: true,
		syscall.EAGAIN: false, syscall.EACCES: false, os.ErrDeadlineExceeded: false,
	} {
		wrapped := &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", err)}
		if got := socketIsDead(wrapped); got != want {
			t.Errorf("%v: %v", err, got)
		}
	}
}
