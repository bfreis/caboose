package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
)

// A trimmed /proc/net/tcp: 0x0BB8 = 3000 listening on 127.0.0.1, 0x1F90 =
// 8080 on 0.0.0.0, and an established connection that must not count.
const procTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000   501        0 1 1 0 100 0 0 10 0
   1: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000   501        0 2 1 0 100 0 0 10 0
   2: 0100007F:C350 0100007F:0BB8 01 00000000:00000000 00:00000000 00000000   501        0 3 1 0 20 4 30 10 -1
   3: garbage
`

// tcp6: 0x1F90 = 8080 again on ::, and 0x14E9 = 5353 on ::1.
const procTCP6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:1F90 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000   501        0 4 1 0 100 0 0 10 0
   1: 00000000000000000000000001000000:14E9 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000   501        0 5 1 0 100 0 0 10 0
`

func procFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "net"), 0o755)
	os.WriteFile(filepath.Join(root, "net", "tcp"), []byte(procTCP), 0o644)
	os.WriteFile(filepath.Join(root, "net", "tcp6"), []byte(procTCP6), 0o644)
	return root
}

func TestListeningPorts(t *testing.T) {
	got := listeningPorts(procFixture(t))
	if want := []int{3000, 5353, 8080}; !slices.Equal(got, want) {
		t.Fatalf("ports %v, want %v", got, want)
	}
	if got := listeningPorts(t.TempDir()); len(got) != 0 {
		t.Fatalf("no tables should mean no ports, got %v", got)
	}
}

// fakeHost runs RunLink against a host session in this process.
type fakeHost struct {
	sess   *agentproto.Session
	socket string
	done   chan error
}

func startLink(t *testing.T) *fakeHost {
	t.Helper()
	hr, aw := io.Pipe()
	ar, hw := io.Pipe()
	// A short path: a Unix socket's must fit in about 100 bytes.
	dir, err := os.MkdirTemp("", "cba")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	h := &fakeHost{
		sess:   agentproto.NewSession(hr, hw, true),
		socket: filepath.Join(dir, "s"),
		done:   make(chan error, 1),
	}
	go func() {
		h.done <- RunLink(ar, aw, Config{Socket: h.socket, ProcRoot: procFixture(t), Interval: 20 * time.Millisecond})
	}()
	t.Cleanup(func() { h.sess.Close() })
	if m := h.next(t); m.Type != agentproto.TypeHello || m.Version != agentproto.Version {
		t.Fatalf("first message %+v, want a hello", m)
	}
	h.sess.Send(agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version})
	return h
}

func (h *fakeHost) next(t *testing.T) agentproto.Message {
	t.Helper()
	select {
	case b := <-h.sess.Control():
		m, err := agentproto.Decode(b)
		if err != nil {
			t.Fatal(err)
		}
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no message from the link")
	}
	return agentproto.Message{}
}

func (h *fakeHost) nextOf(t *testing.T, typ string) agentproto.Message {
	t.Helper()
	for {
		if m := h.next(t); m.Type == typ {
			return m
		}
	}
}

func waitSocket(t *testing.T, path string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the link never made its socket")
}

func TestLinkReportsPorts(t *testing.T) {
	h := startLink(t)
	m := h.nextOf(t, agentproto.TypePorts)
	if want := []int{3000, 5353, 8080}; !slices.Equal(m.Ports, want) {
		t.Fatalf("ports %v, want %v", m.Ports, want)
	}
}

// A command in the container reaches the host as a request, and the host's
// answer comes back to it.
func TestRequestsRoundTrip(t *testing.T) {
	h := startLink(t)
	waitSocket(t, h.socket)
	go func() {
		for b := range h.sess.Control() {
			m, _ := agentproto.Decode(b)
			if m.Type != agentproto.TypeRequest {
				continue
			}
			r := agentproto.Message{Type: agentproto.TypeResponse, ID: m.ID, OK: m.URL == "https://example.com/"}
			if !r.OK {
				r.Error = "refused " + m.URL
			}
			h.sess.Send(r)
		}
	}()
	if err := Open(h.socket, "https://example.com/"); err != nil {
		t.Fatalf("open: %v", err)
	}
	err := Open(h.socket, "file:///etc/passwd")
	if err == nil || !strings.Contains(err.Error(), "refused file:///etc/passwd") {
		t.Fatalf("open of a refused URL: %v", err)
	}
}

func TestStatusIsAnsweredHere(t *testing.T) {
	h := startLink(t)
	h.nextOf(t, agentproto.TypePorts)
	h.sess.Send(agentproto.Message{Type: agentproto.TypeForwards, Forwards: []agentproto.Forward{
		{Port: 3000, HostPort: 3000}, {Port: 8080, Reason: "not in forward_ports"},
	}})
	waitSocket(t, h.socket)
	var out bytes.Buffer
	deadline := time.Now().Add(5 * time.Second)
	for {
		out.Reset()
		if err := Ports(h.socket, &out); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "localhost:3000") || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, want := range []string{"3000   forwarded to the host's localhost:3000", "8080   not forwarded: not in forward_ports", "5353   not forwarded yet"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("ports output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestNoLink(t *testing.T) {
	if err := Open(filepath.Join(t.TempDir(), "none"), "https://example.com/"); err != ErrNoLink {
		t.Fatalf("got %v, want ErrNoLink", err)
	}
}

// A stream the host opens reaches a server on the container's loopback,
// bound to 127.0.0.1 only.
func TestStreamsReachLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		b, _ := io.ReadAll(c)
		c.Write(bytes.ToUpper(b))
		c.Close()
	}()
	h := startLink(t)
	hdr, _ := json.Marshal(agentproto.StreamHeader{Port: ln.Addr().(*net.TCPAddr).Port})
	st, err := h.sess.Open(hdr)
	if err != nil {
		t.Fatal(err)
	}
	st.Write([]byte("hello"))
	st.CloseWrite()
	got, _ := io.ReadAll(st)
	if string(got) != "HELLO" {
		t.Fatalf("got %q through the stream", got)
	}
}

// A stream for a port nothing listens on is reset, not left hanging.
func TestStreamToAClosedPortIsReset(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	h := startLink(t)
	hdr, _ := json.Marshal(agentproto.StreamHeader{Port: port})
	st, err := h.sess.Open(hdr)
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
		if err == nil || err == io.EOF {
			t.Fatalf("read gave %v, want a reset", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stream hangs")
	}
}

func TestLinkEndsWithTheHost(t *testing.T) {
	h := startLink(t)
	h.sess.Close()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the link outlived its host")
	}
}
