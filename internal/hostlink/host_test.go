package hostlink

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agent"
	"github.com/bfreis/caboose/internal/agentproto"
)

func TestParsePorts(t *testing.T) {
	set, err := ParsePorts("3000-3999, 5173 8000-8999")
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[int]bool{2999: false, 3000: true, 3999: true, 4000: false, 5173: true, 5174: false, 8080: true} {
		if set.Has(p) != want {
			t.Errorf("Has(%d) = %v", p, !want)
		}
	}
	if set, err := ParsePorts("none"); err != nil || set.Has(3000) {
		t.Errorf("none: %v %v", set, err)
	}
	for _, bad := range []string{"", "0", "70000", "3000-", "x", "9000-8000", "-5"} {
		if _, err := ParsePorts(bad); err == nil {
			t.Errorf("ParsePorts(%q) accepted", bad)
		}
	}
}

func TestCheckURL(t *testing.T) {
	for _, ok := range []string{"https://example.com/", "http://localhost:3000/x?y=1"} {
		if _, err := CheckURL(ok); err != nil {
			t.Errorf("CheckURL(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{
		"file:///etc/passwd", "javascript:alert(1)", "ssh://host", "https://", "x",
		"https://user:pw@example.com/", "https://example.com/\x1b[31m", "https://example.com/‮",
		"https://example.com/" + strings.Repeat("a", maxURL),
	} {
		if _, err := CheckURL(bad); err == nil {
			t.Errorf("CheckURL(%q) accepted", bad)
		}
	}
}

type fakeActions struct {
	mu      sync.Mutex
	opened  []string
	notes   []string
	answer  bool
	confirm int
}

func (f *fakeActions) OpenURL(u string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, u)
	return nil
}

func (f *fakeActions) Notify(title, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notes = append(f.notes, title+": "+text)
	return nil
}

func (f *fakeActions) Confirm(string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.confirm++
	return f.answer, nil
}

// linked runs a real agent link and a Host against each other in-process.
// The agent reads the real /proc, so this needs Linux.
func linked(t *testing.T, cfg Config) (socket string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the agent reads /proc")
	}
	hr, aw := io.Pipe()
	ar, hw := io.Pipe()
	dir, err := os.MkdirTemp("", "cbh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket = dir + "/s"
	go agent.RunLink(ar, aw, agent.Config{Socket: socket, ProcRoot: "/proc", Interval: 20 * time.Millisecond,
		WorkDir: filepath.Clean(os.TempDir())})
	sess := agentproto.NewSession(hr, hw, true)
	t.Cleanup(func() { sess.Close() })
	if cfg.Log == nil {
		cfg.Log = log.New(io.Discard, "", 0)
	}
	go Run(sess, cfg)
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(socket); err == nil {
			return socket
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no agent socket")
	return ""
}

// A port the sandbox listens on, in forward_ports, is reachable through the
// host's listener; the sandbox's server is bound to loopback only.
func TestForwarding(t *testing.T) {
	srv, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	port := srv.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := srv.Accept()
			if err != nil {
				return
			}
			go func() {
				b, _ := io.ReadAll(c)
				c.Write(bytes.ToUpper(b))
				c.Close()
			}()
		}
	}()
	// The test's server and the host's listener share this machine, so the
	// forward listens elsewhere: what is under test is the path, not the
	// port number.
	hostLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listened := make(chan int, 4)
	set, _ := ParsePorts(strconv.Itoa(port))
	linked(t, Config{Ports: set, Actions: &fakeActions{}, Listen: func(p int) (net.Listener, error) {
		listened <- p
		if p != port {
			return nil, io.ErrUnexpectedEOF
		}
		return hostLn, nil
	}})
	select {
	case p := <-listened:
		if p != port {
			t.Fatalf("forwarded %d, want %d", p, port)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the port was never forwarded")
	}
	c, err := net.Dial("tcp", hostLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("through the link"))
	c.(*net.TCPConn).CloseWrite()
	got, _ := io.ReadAll(c)
	if string(got) != "THROUGH THE LINK" {
		t.Fatalf("got %q", got)
	}
}

// Ports outside forward_ports are reported, not forwarded.
func TestOutsideForwardPorts(t *testing.T) {
	srv, _ := net.Listen("tcp", "127.0.0.1:0")
	defer srv.Close()
	port := srv.Addr().(*net.TCPAddr).Port
	socket := linked(t, Config{Ports: PortSet{}, Actions: &fakeActions{}, Listen: func(int) (net.Listener, error) {
		t.Error("listened for a port outside forward_ports")
		return nil, io.EOF
	}})
	var out bytes.Buffer
	want := strconv.Itoa(port) + " "
	for i := 0; i < 250; i++ {
		out.Reset()
		agent.Ports(socket, &out)
		if strings.Contains(out.String(), "not in forward_ports") && strings.Contains(out.String(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("port %d not reported as refused:\n%s", port, out.String())
}

func TestOpen(t *testing.T) {
	cases := []struct {
		policy  string
		answer  bool
		url     string
		opened  bool
		asked   bool
		errWant string
	}{
		{OpenAsk, true, "https://example.com/", true, true, ""},
		{OpenAsk, false, "https://example.com/", false, true, "declined"},
		{OpenAllow, false, "https://example.com/", true, false, ""},
		{OpenOff, true, "https://example.com/", false, false, "open_urls"},
		{OpenAllow, true, "file:///etc/passwd", false, false, "only http and https"},
	}
	for _, c := range cases {
		t.Run(c.policy+" "+c.url, func(t *testing.T) {
			fa := &fakeActions{answer: c.answer}
			socket := linked(t, Config{Ports: PortSet{}, OpenURL: c.policy, Actions: fa})
			err := agent.Open(socket, c.url)
			if c.errWant == "" && err != nil || c.errWant != "" && (err == nil || !strings.Contains(err.Error(), c.errWant)) {
				t.Fatalf("err %v, want %q", err, c.errWant)
			}
			if (len(fa.opened) == 1) != c.opened || (fa.confirm == 1) != c.asked {
				t.Fatalf("opened %v, asked %d times", fa.opened, fa.confirm)
			}
		})
	}
}

func TestNotifyIsMadePrintable(t *testing.T) {
	fa := &fakeActions{}
	socket := linked(t, Config{Ports: PortSet{}, Actions: fa})
	if err := agent.Notify(socket, "t\x1b]0;x\x07", "done‮ "+strings.Repeat("y", 2*maxText)); err != nil {
		t.Fatal(err)
	}
	n := fa.notes[0]
	if strings.ContainsAny(n, "\x1b\x07‮") {
		t.Errorf("unprintable characters reached the notification: %q", n)
	}
	if len(n) > maxTitle+maxText+20 {
		t.Errorf("notification of %d bytes was not cut short", len(n))
	}
}

func TestRequestsAreRateLimited(t *testing.T) {
	socket := linked(t, Config{Ports: PortSet{}, Actions: &fakeActions{}})
	var refused bool
	for i := 0; i < requestBurst+3; i++ {
		if err := agent.Notify(socket, "", "x"); err != nil && strings.Contains(err.Error(), "too many") {
			refused = true
		}
	}
	if !refused {
		t.Fatal("a burst past the limit was never refused")
	}
}

// Under vm, the host's hello names where the agent is to serve this
// machine's SSH agent: a client there reaches it, both ways, through a
// stream the host opens; with none named, nothing listens.
func TestSSHAgent(t *testing.T) {
	dir, err := os.MkdirTemp("", "cbs")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	hostSock, guestSock := dir+"/host", dir+"/guest"
	ln, err := net.Listen("unix", hostSock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	linked(t, Config{SSHAgent: guestSock, SSHAuthSock: hostSock})
	var c net.Conn
	for i := 0; i < 200; i++ {
		if c, err = net.Dial("unix", guestSock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("no SSH agent socket in the sandbox: %v", err)
	}
	defer c.Close()
	// Two at once, as git's does.
	c2, err := net.Dial("unix", guestSock)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	for _, conn := range []net.Conn{c, c2} {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := conn.Write([]byte("\x00\x00\x00\x01\x0b")); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, 5)
		if _, err := io.ReadFull(conn, got); err != nil || string(got) != "\x00\x00\x00\x01\x0b" {
			t.Fatalf("through the agent: %q, %v", got, err)
		}
	}
	if fi, err := os.Stat(guestSock); err != nil || fi.Mode().Perm() != 0o666 {
		t.Errorf("the sandbox's socket: %v, %v", fi, err)
	}

	other := dir + "/none"
	linked(t, Config{SSHAgent: other})
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(other); err == nil {
		t.Error("an SSH agent socket with no host agent to reach")
	}
}

// A session that ends before the agent's hello says so, for the launcher
// to retry it soon: an agent with nothing to serve yet closes at once.
func TestRunEndsBeforeHello(t *testing.T) {
	hr, aw := io.Pipe()
	ar, hw := io.Pipe()
	sess := agentproto.NewSession(hr, hw, true)
	defer sess.Close()
	go func() { _, _ = io.Copy(io.Discard, ar) }()
	done := make(chan error, 1)
	go func() { done <- Run(sess, Config{Log: log.New(io.Discard, "", 0)}) }()
	aw.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNoHello) {
			t.Errorf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not end")
	}
}
