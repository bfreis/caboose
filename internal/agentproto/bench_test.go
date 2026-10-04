package agentproto

import (
	"io"
	"net"
	"testing"
	"time"
)

// latent delays each write to c by d, as a link with a one-way latency of
// d does, in order and without holding up the writer.
type latent struct {
	net.Conn
	q chan latentWrite
}

type latentWrite struct {
	at time.Time
	p  []byte
}

func withLatency(c net.Conn, d time.Duration) *latent {
	l := &latent{Conn: c, q: make(chan latentWrite, 1<<16)}
	go func() {
		for w := range l.q {
			time.Sleep(time.Until(w.at))
			if _, err := c.Write(w.p); err != nil {
				return
			}
		}
	}()
	return l
}

func (l *latent) Write(p []byte) (int, error) {
	l.q <- latentWrite{time.Now().Add(latency), append([]byte(nil), p...)}
	return len(p), nil
}

var latency time.Duration

func tcpPair(b *testing.B) (net.Conn, net.Conn) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	return c, <-accepted
}

// benchStream sends 64 MiB on one stream, host to agent, over loopback TCP
// with a one-way latency of d, after the hellos when hello is set (which
// announce the windows) or without them, as with an older peer.
func benchStream(b *testing.B, d time.Duration, hello bool) {
	const total = 64 << 20
	b.SetBytes(total)
	latency = d
	for i := 0; i < b.N; i++ {
		x, y := tcpPair(b)
		var wx, wy io.WriteCloser = x, y
		if d > 0 {
			wx, wy = withLatency(x, d), withLatency(y, d)
		}
		host := NewSession(x, wx, true)
		agent := NewSession(y, wy, false)
		if hello {
			_ = host.Send(Message{Type: TypeHello, Version: Version})
			_ = agent.Send(Message{Type: TypeHello, Version: Version})
			<-host.Control()
			<-agent.Control()
		}
		go func() {
			st, err := agent.Accept()
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, st)
			st.Close()
		}()
		st, err := host.Open(nil)
		if err != nil {
			b.Fatal(err)
		}
		buf := make([]byte, 32<<10)
		for sent := 0; sent < total; sent += len(buf) {
			if _, err := st.Write(buf); err != nil {
				b.Fatal(err)
			}
		}
		_ = st.CloseWrite()
		_, _ = io.Copy(io.Discard, st)
		host.Close()
		agent.Close()
	}
}

func BenchmarkStream(b *testing.B)          { benchStream(b, 0, true) }
func BenchmarkStreamOlderPeer(b *testing.B) { benchStream(b, 0, false) }
func BenchmarkStream10ms(b *testing.B)      { benchStream(b, 10*time.Millisecond, true) }
func BenchmarkStream10msOlder(b *testing.B) { benchStream(b, 10*time.Millisecond, false) }
