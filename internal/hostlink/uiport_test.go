package hostlink

import (
	"bytes"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/policy"
)

// The web UI's ports are never forwarded, even with a forward_ports set
// that holds them (config.toml refuses one; this is a set read another
// way), and the refusal is logged once a link, however often the sandbox
// lists them. No port is bound for real: Listen is the test's.
func TestUIPortsNeverForwarded(t *testing.T) {
	hr, aw := io.Pipe()
	ar, hw := io.Pipe()
	hostSess := agentproto.NewSession(hr, hw, true)
	agentSess := agentproto.NewSession(ar, aw, false)
	t.Cleanup(func() { hostSess.Close(); agentSess.Close() })
	all, err := ParsePorts("1-65535")
	if err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	var listened []int
	h := &Host{sess: hostSess, forwards: map[int]*forward{}, refused: map[int]string{}, cfg: Config{
		Ports:       all,
		Log:         log.New(&logged, "", 0),
		HostListens: func(int) bool { return false },
		Listen: func(p int) (net.Listener, error) {
			listened = append(listened, p)
			return nil, io.ErrUnexpectedEOF // in use: nothing is bound
		},
	}}
	for range 3 {
		done := make(chan struct{})
		go func() { h.reconcile([]int{7171, 7172, 3000}); close(done) }()
		var msg agentproto.Message
		select {
		case b := <-agentSess.Control():
			if msg, err = agentproto.Decode(b); err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no forwards message")
		}
		<-done
		for _, f := range msg.Forwards {
			switch f.Port {
			case 7171, 7172:
				if f.HostPort != 0 || !strings.Contains(f.Reason, "web UI") {
					t.Errorf("port %d: %+v", f.Port, f)
				}
			case 3000:
				if f.Reason != "the port is in use on the host" {
					t.Errorf("port 3000: %+v", f)
				}
			}
		}
	}
	for _, p := range listened {
		if p == 7171 || p == 7172 {
			t.Errorf("listened for %d", p)
		}
	}
	if len(listened) != 3 {
		t.Errorf("listened %v, want 3000 each time", listened)
	}
	if n := strings.Count(logged.String(), "never forwarded"); n != 1 {
		t.Errorf("logged %d times:\n%s", n, logged.String())
	}
}

// A URL to the web UI's ports is refused whatever names the host, and
// before anything decides it.
func TestCheckURLRefusesUIPorts(t *testing.T) {
	for _, bad := range []string{
		"http://127.0.0.1:7171/", "http://localhost:7171", "http://127.1:7171", "http://2130706433:7171",
		"http://[::1]:7171", "http://example.com:7171", "https://x:7172/", "http://0x7f.1:7171/",
		"http://[::ffff:127.0.0.1]:7172/", "http://localhost.:7171/#token=x", "http://foo.localhost:7172/api",
		"http://127.0.0.1:07171/", "HTTP://127.0.0.1:7171/", "http://127.0.0.1:0000007172/",
		"http://127.0.0.1:99999/",
	} {
		_, err := CheckURL(bad)
		if err == nil {
			t.Errorf("CheckURL(%q) accepted", bad)
		} else if !strings.Contains(bad, "99999") && !strings.Contains(err.Error(), "web UI") {
			t.Errorf("CheckURL(%q): %v", bad, err)
		}
	}
	for _, ok := range []string{"http://127.0.0.1:7170/", "http://localhost:8080", "https://example.com:7173/x", "http://example.com/7171", "https://example.com/?p=7171"} {
		if _, err := CheckURL(ok); err != nil {
			t.Errorf("CheckURL(%q): %v", ok, err)
		}
	}
}

// Neither an open nor a request to open the web UI's address reaches the
// Decider, even one that would allow it; nothing opens.
func TestUIPortURLNeverDecided(t *testing.T) {
	fd := newFakeDecider()
	fd.set(policy.KindOpenURL, broker.Outcome{Verdict: broker.Allowed})
	fa := &fakeActions{}
	a := startExecHost(t, Config{Ports: PortSet{}, OpenURL: OpenAllow, Actions: fa, Decide: fd})
	for _, u := range []string{"http://127.0.0.1:7171/#token=x", "http://localhost:7172/"} {
		if r := a.request(agentproto.Message{Op: agentproto.OpOpen, URL: u}); r.OK || !strings.Contains(r.Error, "web UI") {
			t.Errorf("open %s: %+v", u, r)
		}
		r := a.request(agentproto.Message{Op: agentproto.OpBrokerRequest, Ask: &agentproto.Ask{Kind: "open_url", URL: u}})
		if r.OK || !strings.Contains(r.Error, "web UI") {
			t.Errorf("request %s: %+v", u, r)
		}
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if len(fd.decided) != 0 || len(fd.filed) != 0 {
		t.Errorf("decided %+v, filed %+v", fd.decided, fd.filed)
	}
	if len(fa.opened) != 0 {
		t.Errorf("opened %v", fa.opened)
	}
}

// Forwarded is told the ports forwarded, not those refused, each time the
// set may have changed. The listener is on 127.0.0.1, port 0.
func TestForwardedCallback(t *testing.T) {
	hr, aw := io.Pipe()
	ar, hw := io.Pipe()
	hostSess := agentproto.NewSession(hr, hw, true)
	agentSess := agentproto.NewSession(ar, aw, false)
	t.Cleanup(func() { hostSess.Close(); agentSess.Close() })
	allowed, err := ParsePorts("3000-3001")
	if err != nil {
		t.Fatal(err)
	}
	var got [][]int
	h := &Host{sess: hostSess, forwards: map[int]*forward{}, refused: map[int]string{}, cfg: Config{
		Ports:       allowed,
		Log:         log.New(io.Discard, "", 0),
		Listen:      func(int) (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") },
		HostListens: func(int) bool { return false },
		Forwarded:   func(p []int) { got = append(got, p) },
	}}
	t.Cleanup(h.closeAll)
	go func() {
		for range agentSess.Control() {
		}
	}()
	h.reconcile([]int{3001, 3000, 4000})
	h.reconcile([]int{3001})
	if len(got) != 2 || len(got[0]) != 2 || got[0][0] != 3000 || got[0][1] != 3001 || len(got[1]) != 1 || got[1][0] != 3001 {
		t.Errorf("Forwarded was told %v", got)
	}
}
