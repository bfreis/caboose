package agentproto

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pair is a host session and an agent session joined by pipes.
func pair(t *testing.T) (host, agent *Session) {
	t.Helper()
	hr, aw := io.Pipe()
	ar, hw := io.Pipe()
	host = NewSession(hr, hw, true)
	agent = NewSession(ar, aw, false)
	t.Cleanup(func() { host.Close(); agent.Close() })
	return host, agent
}

func recvControl(t *testing.T, s *Session) Message {
	t.Helper()
	select {
	case b, ok := <-s.Control():
		if !ok {
			t.Fatalf("control closed: %v", s.Err())
		}
		m, err := Decode(b)
		if err != nil {
			t.Fatal(err)
		}
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no control message")
	}
	return Message{}
}

func TestControlBothWays(t *testing.T) {
	host, agent := pair(t)
	if err := host.Send(Message{Type: TypeHello, Version: Version}); err != nil {
		t.Fatal(err)
	}
	if m := recvControl(t, agent); m.Type != TypeHello || m.Version != Version {
		t.Fatalf("agent got %+v", m)
	}
	if err := agent.Send(Message{Type: TypePorts, Ports: []int{3000, 8080}}); err != nil {
		t.Fatal(err)
	}
	if m := recvControl(t, host); m.Type != TypePorts || len(m.Ports) != 2 || m.Ports[1] != 8080 {
		t.Fatalf("host got %+v", m)
	}
}

// A stream carries more than a window each way at once, intact, and each
// side's close reads as EOF on the other.
func TestStreamBothWaysPastTheWindow(t *testing.T) {
	host, agent := pair(t)
	up := make([]byte, 3*DefaultWindow+123)
	down := make([]byte, 2*DefaultWindow+7)
	rand.Read(up)
	rand.Read(down)

	var wg sync.WaitGroup
	var agentGot []byte
	var agentErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		st, err := agent.Accept()
		if err != nil {
			agentErr = err
			return
		}
		if string(st.Header()) != "hdr" {
			agentErr = errors.New("header " + string(st.Header()))
		}
		done := make(chan struct{})
		go func() {
			agentGot, _ = io.ReadAll(st)
			close(done)
		}()
		if _, err := st.Write(down); err != nil {
			agentErr = err
		}
		st.CloseWrite()
		<-done
		st.Close()
	}()

	st, err := host.Open([]byte("hdr"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(st)
		done <- b
	}()
	if _, err := st.Write(up); err != nil {
		t.Fatal(err)
	}
	st.CloseWrite()
	hostGot := <-done
	st.Close()
	wg.Wait()
	if agentErr != nil {
		t.Fatal(agentErr)
	}
	if !bytes.Equal(agentGot, up) {
		t.Errorf("agent got %d bytes, want %d intact", len(agentGot), len(up))
	}
	if !bytes.Equal(hostGot, down) {
		t.Errorf("host got %d bytes, want %d intact", len(hostGot), len(down))
	}
}

// Closing a stream the peer is still sending on resets it there.
func TestCloseResetsThePeer(t *testing.T) {
	host, agent := pair(t)
	st, err := host.Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	ast, err := agent.Accept()
	if err != nil {
		t.Fatal(err)
	}
	ast.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := st.Write(make([]byte, 1024))
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("writes go on after the peer closed")
		}
	}
	if _, err := st.Read(make([]byte, 1)); err == nil {
		t.Error("read after a reset succeeded")
	}
}

func TestOnlyTheHostOpens(t *testing.T) {
	_, agent := pair(t)
	if _, err := agent.Open(nil); err == nil {
		t.Fatal("the agent opened a stream")
	}
}

// rawPeer is a host session whose peer is the test, writing frames by hand.
func rawPeer(t *testing.T) (host *Session, peer io.Writer) {
	t.Helper()
	hr, pw := io.Pipe()
	_, hw := io.Pipe()
	host = NewSession(hr, hw, true)
	t.Cleanup(func() { host.Close() })
	return host, pw
}

func frame(typ byte, id uint32, payload []byte) []byte {
	b := make([]byte, headerLen+len(payload))
	b[0] = typ
	binary.BigEndian.PutUint32(b[1:5], id)
	binary.BigEndian.PutUint32(b[5:9], uint32(len(payload)))
	copy(b[headerLen:], payload)
	return b
}

func waitDone(t *testing.T, s *Session) {
	t.Helper()
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the session survived a broken peer")
	}
}

// The container is not trusted: a peer breaking the protocol is cut off.
func TestBrokenPeersEndTheSession(t *testing.T) {
	big := make([]byte, headerLen)
	big[0] = frameControl
	binary.BigEndian.PutUint32(big[5:9], MaxPayload+1)
	cases := map[string][]byte{
		"oversized frame":        big,
		"opens a stream":         frame(frameOpen, 2, nil),
		"control off stream 0":   frame(frameControl, 3, []byte(`{}`)),
		"data on stream 0":       frame(frameData, 0, []byte("x")),
		"unknown frame type":     frame(99, 1, nil),
		"truncated frame header": {frameControl, 0, 0},
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			host, peer := rawPeer(t)
			go func() {
				peer.Write(b)
				if c, ok := peer.(io.Closer); ok && name == "truncated frame header" {
					c.Close()
				}
			}()
			waitDone(t, host)
		})
	}
}

func TestWindowOverrunEndsTheSession(t *testing.T) {
	hr, pw := io.Pipe()
	ar, hw := io.Pipe()
	host := NewSession(hr, hw, true)
	t.Cleanup(func() { host.Close() })
	// Read the host's open frame, then answer with more than a window.
	go func() {
		var h [headerLen]byte
		io.ReadFull(ar, h[:])
		chunk := make([]byte, MaxPayload)
		for i := 0; i <= HostWindow/MaxPayload; i++ {
			if _, err := pw.Write(frame(frameData, 1, chunk)); err != nil {
				return
			}
		}
	}()
	if _, err := host.Open(nil); err != nil {
		t.Fatal(err)
	}
	waitDone(t, host)
}

// The host holds at most MaxStreams open. The peer here never answers, so
// no reset frees one behind the test's back.
func TestStreamLimit(t *testing.T) {
	hr, _ := io.Pipe()
	pr, hw := io.Pipe()
	go io.Copy(io.Discard, pr)
	host := NewSession(hr, hw, true)
	t.Cleanup(func() { host.Close() })
	for i := 0; i < MaxStreams; i++ {
		if _, err := host.Open(nil); err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
	}
	if _, err := host.Open(nil); err == nil {
		t.Fatal("opened past MaxStreams")
	}
}

func TestSessionCloseEndsStreams(t *testing.T) {
	host, agent := pair(t)
	st, err := host.Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Accept(); err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() {
		_, err := st.Read(make([]byte, 1))
		errc <- err
	}()
	agent.Close()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("read succeeded after the session ended")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("read still blocked after the session ended")
	}
}

// sendable is how much st writes before it waits for the peer: the peer
// here never reads.
func sendable(t *testing.T, st *Stream) int {
	t.Helper()
	var sent atomic.Int64
	go func() {
		chunk := make([]byte, MaxPayload/4)
		for {
			n, err := st.Write(chunk)
			sent.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	var last int64 = -1
	for {
		time.Sleep(50 * time.Millisecond)
		n := sent.Load()
		if n == last {
			return int(n)
		}
		last = n
	}
}

// Each side's hello announces its window, which the other's streams then
// send ahead: past DefaultWindow, and no further.
func TestHelloAnnouncesTheWindow(t *testing.T) {
	host, agent := pair(t)
	if err := host.Send(Message{Type: TypeHello, Version: Version}); err != nil {
		t.Fatal(err)
	}
	if m := recvControl(t, agent); m.Window != HostWindow {
		t.Fatalf("the host announced %d", m.Window)
	}
	// A stream opened before the agent's hello arrives grows with it.
	st, err := host.Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Send(Message{Type: TypeHello, Version: Version}); err != nil {
		t.Fatal(err)
	}
	if m := recvControl(t, host); m.Window != AgentWindow {
		t.Fatalf("the agent announced %d", m.Window)
	}
	ast, err := agent.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if n := sendable(t, st); n != AgentWindow {
		t.Fatalf("the host sent %d ahead, want %d", n, AgentWindow)
	}
	if n := sendable(t, ast); n != HostWindow {
		t.Fatalf("the agent sent %d ahead, want %d", n, HostWindow)
	}
}

// A peer whose hello announces no window is sent no more than
// DefaultWindow ahead, and gets its grants in time for it to go on.
func TestPeerWithoutWindowGetsTheDefaultWindow(t *testing.T) {
	hr, pw := io.Pipe()
	pr, hw := io.Pipe()
	host := NewSession(hr, hw, true)
	t.Cleanup(func() { host.Close() })
	go io.Copy(io.Discard, pr)
	if _, err := pw.Write(frame(frameControl, 0, []byte(`{"type":"hello","version":1}`))); err != nil {
		t.Fatal(err)
	}
	recvControl(t, host)
	st, err := host.Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := sendable(t, st); n != DefaultWindow {
		t.Fatalf("sent %d ahead, want %d", n, DefaultWindow)
	}
	// The peer sends a whole DefaultWindow, and the host grants
	// before it has read all of it.
	chunk := make([]byte, MaxPayload)
	go func() {
		for i := 0; i < DefaultWindow/MaxPayload; i++ {
			if _, err := pw.Write(frame(frameData, st.id, chunk)); err != nil {
				return
			}
		}
	}()
	if _, err := io.ReadFull(st, make([]byte, grantAt)); err != nil {
		t.Fatal(err)
	}
	if grantAt > DefaultWindow/2 {
		t.Fatalf("grants at %d: a peer stalls", grantAt)
	}
}

// A grant past the window this side is sending with ends the session.
func TestGrantPastTheWindowEndsTheSession(t *testing.T) {
	hr, peer := io.Pipe()
	pr, hw := io.Pipe()
	go io.Copy(io.Discard, pr)
	host := NewSession(hr, hw, true)
	t.Cleanup(func() { host.Close() })
	if _, err := host.Open(nil); err != nil {
		t.Fatal(err)
	}
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], 1)
	if _, err := peer.Write(frame(frameWindow, 1, b[:])); err != nil {
		t.Fatal(err)
	}
	waitDone(t, host)
}

// Streams opened faster than they are accepted all wait for Accept: none
// is reset for want of room, up to MaxStreams of them.
func TestStreamsWaitForAccept(t *testing.T) {
	host, agent := pair(t)
	for i := 0; i < MaxStreams; i++ {
		st, err := host.Open([]byte{byte(i)})
		if err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
		if _, err := st.Write([]byte{byte(i)}); err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
	}
	for i := 0; i < MaxStreams; i++ {
		got := make(chan *Stream, 1)
		go func() {
			if st, err := agent.Accept(); err == nil {
				got <- st
			}
		}()
		var st *Stream
		select {
		case st = <-got:
		case <-time.After(5 * time.Second):
			t.Fatalf("stream %d was never accepted", i)
		}
		b := make([]byte, 1)
		if _, err := io.ReadFull(st, b); err != nil || b[0] != st.Header()[0] {
			t.Fatalf("stream %d: read %v, %v", st.Header()[0], b, err)
		}
	}
}

// A stream the agent resets unaccepted is reported to OnRefused, with its
// header.
func TestOnRefused(t *testing.T) {
	ar, pw := io.Pipe()
	pr, aw := io.Pipe()
	go io.Copy(io.Discard, pr)
	agent := NewSession(ar, aw, false)
	t.Cleanup(func() { agent.Close() })
	refused := make(chan string, 1)
	agent.OnRefused(func(h []byte) { refused <- string(h) })
	go func() {
		for i := 0; i < MaxStreams; i++ {
			pw.Write(frame(frameOpen, uint32(2*i+1), []byte("held")))
		}
		pw.Write(frame(frameOpen, 2*MaxStreams+1, []byte("over")))
	}()
	select {
	case h := <-refused:
		if h != "over" {
			t.Fatalf("refused %q", h)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream over MaxStreams was not reported")
	}
}

// A side with Limits takes no more streams at once than they say, resetting
// the next unaccepted, and announces their window.
func TestLimits(t *testing.T) {
	hr, aw := io.Pipe()
	ar, hw := io.Pipe()
	opener := NewSession(hr, hw, true)
	server := NewSessionLimited(ar, aw, false, Limits{Window: DefaultWindow, Streams: 2})
	t.Cleanup(func() { opener.Close(); server.Close() })
	refused := make(chan string, 4)
	server.OnRefused(func(h []byte) { refused <- string(h) })
	if err := server.Send(Message{Type: TypeHello, Version: Version}); err != nil {
		t.Fatal(err)
	}
	if m := recvControl(t, opener); m.Window != DefaultWindow {
		t.Fatalf("the limited side announced %d", m.Window)
	}
	for _, h := range []string{"a", "b", "c"} {
		if _, err := opener.Open([]byte(h)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case h := <-refused:
		if h != "c" {
			t.Fatalf("refused %q, want the third", h)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stream over the limit was not refused")
	}
	for _, want := range []string{"a", "b"} {
		st, err := server.Accept()
		if err != nil || string(st.Header()) != want {
			t.Fatalf("accepted %v %v, want %q", st, err, want)
		}
	}
}
