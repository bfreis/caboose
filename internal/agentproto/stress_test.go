package agentproto

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// socketPair is two ends of a Unix stream socket whose buffers are small,
// so that a side that stops reading soon stops the other's writes: the
// transport of a link, without a pipe's lockstep.
func socketPair(t *testing.T, buf int) (net.Conn, net.Conn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	var conns [2]net.Conn
	for i, fd := range fds {
		_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, buf)
		_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, buf)
		f := os.NewFile(uintptr(fd), "socketpair")
		c, err := net.FileConn(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = c
	}
	t.Cleanup(func() { conns[0].Close(); conns[1].Close() })
	return conns[0], conns[1]
}

// Many streams move a lot both ways at once over a socket with small
// buffers, the host's hello arriving late (after its streams opened and
// started sending), while control messages go both ways: everything
// arrives intact, no stream stalls, and a control message is never held
// up for long behind the streams' data.
func TestStressBothWaysSmallBuffers(t *testing.T) {
	const (
		streams = 8
		each    = 6 << 20
	)
	hc, ac := socketPair(t, 16<<10)
	host := NewSession(hc, hc, true)
	agent := NewSession(ac, ac, false)
	t.Cleanup(func() { host.Close(); agent.Close() })
	if err := agent.Send(Message{Type: TypeHello, Version: Version}); err != nil {
		t.Fatal(err)
	}

	payload := func(seed int) []byte {
		b := make([]byte, each)
		for i := range b {
			b[i] = byte(i*7 + seed*13 + i>>11)
		}
		return b
	}
	sum := func(b []byte) [32]byte { return sha256.Sum256(b) }

	var wg sync.WaitGroup
	errs := make(chan string, 4*streams)
	// The agent echoes what it gets back with its own bytes, both at once.
	go func() {
		for i := 0; i < streams; i++ {
			st, err := agent.Accept()
			if err != nil {
				return
			}
			seed := int(st.Header()[0])
			wg.Add(1)
			go func() {
				defer wg.Done()
				var w sync.WaitGroup
				w.Add(1)
				go func() {
					defer w.Done()
					if _, err := st.Write(payload(seed + 100)); err != nil {
						errs <- "agent write: " + err.Error()
					}
					st.CloseWrite()
				}()
				got, err := io.ReadAll(st)
				if err != nil {
					errs <- "agent read: " + err.Error()
				} else if sum(got) != sum(payload(seed)) {
					errs <- "agent got other bytes"
				}
				w.Wait()
				st.Close()
			}()
		}
	}()

	// Control messages both ways all along; the longest wait for one.
	stop := make(chan struct{})
	var worst time.Duration
	var cmu sync.Mutex
	ping := func(from *Session) {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			sent := time.Now()
			if err := from.Send(Message{Type: TypePorts, Time: sent.UnixNano()}); err != nil {
				return
			}
		}
	}
	drain := func(s *Session) {
		defer wg.Done()
		for b := range s.Control() {
			m, err := Decode(b)
			if err != nil || m.Type != TypePorts {
				continue
			}
			d := time.Since(time.Unix(0, m.Time))
			cmu.Lock()
			worst = max(worst, d)
			cmu.Unlock()
		}
	}
	wg.Add(2)
	go drain(host)
	go drain(agent)

	var hostWG sync.WaitGroup
	sts := make([]*Stream, streams)
	for i := range sts {
		st, err := host.Open([]byte{byte(i)})
		if err != nil {
			t.Fatal(err)
		}
		sts[i] = st
		hostWG.Add(1)
		go func() {
			defer hostWG.Done()
			var w sync.WaitGroup
			w.Add(1)
			go func() {
				defer w.Done()
				if _, err := st.Write(payload(i)); err != nil {
					errs <- "host write: " + err.Error()
				}
				st.CloseWrite()
			}()
			got, err := io.ReadAll(st)
			if err != nil {
				errs <- "host read: " + err.Error()
			} else if !bytes.Equal(got, payload(i+100)) {
				errs <- "host got other bytes"
			}
			w.Wait()
			st.Close()
		}()
	}
	// The host's hello, late: its streams grow on the agent's side.
	time.Sleep(20 * time.Millisecond)
	if err := host.Send(Message{Type: TypeHello, Version: Version}); err != nil {
		t.Fatal(err)
	}
	wg.Add(2)
	go ping(host)
	go ping(agent)

	done := make(chan struct{})
	go func() { hostWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("stalled: host %v, agent %v", host.Err(), agent.Err())
	}
	close(stop)
	select {
	case e := <-errs:
		t.Fatal(e)
	default:
	}
	cmu.Lock()
	defer cmu.Unlock()
	t.Logf("longest a control message waited: %v", worst)
	if worst > 5*time.Second {
		t.Fatalf("a control message waited %v behind the streams", worst)
	}
	if host.Err() != nil || agent.Err() != nil {
		t.Fatalf("a session ended: host %v, agent %v", host.Err(), agent.Err())
	}
}

// A stream with bytes to send and no credit is logged once it has waited
// long enough, with its counters; one still moving is not.
func TestWatchStallsLogsAStreamWithoutCredit(t *testing.T) {
	host, agent := pair(t)
	lines := make(chan string, 16)
	host.WatchStalls(func(format string, args ...any) { lines <- fmt.Sprintf(format, args...) }, 100*time.Millisecond)
	go func() {
		if st, err := agent.Accept(); err == nil {
			_ = st // never read: the host's credit runs out
		}
	}()
	st, err := host.Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	go st.Write(make([]byte, DefaultWindow+1))
	select {
	case l := <-lines:
		if !strings.Contains(l, "stream 1 has waited") || !strings.Contains(l, fmt.Sprintf("sent %d", DefaultWindow)) {
			t.Fatalf("logged %q", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing logged")
	}
	select {
	case l := <-lines:
		t.Fatalf("logged again: %q", l)
	case <-time.After(300 * time.Millisecond):
	}
}
