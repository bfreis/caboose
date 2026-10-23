package daemon

import (
	"bytes"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
)

type lockedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) count(s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), s)
}

// Every connection that reaches the daemon with no hello first, a
// request in its place or nothing at all, is logged as "a client's
// hello".
func TestConnectionWithoutHelloIsLogged(t *testing.T) {
	log := &lockedLog{}
	dir, _ := startServer(t, Options{Version: "v1.0.0", Idle: time.Minute, Log: log})
	req, err := daemonproto.Request(1, "status", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, send := range []func(net.Conn){
		func(c net.Conn) { _ = daemonproto.Write(c, req) },
		func(net.Conn) {},
	} {
		c, err := net.Dial("unix", Socket(dir))
		if err != nil {
			t.Fatal(err)
		}
		send(c)
		c.Close()
		deadline := time.Now().Add(5 * time.Second)
		for log.count("a client's hello") < i+1 {
			if time.Now().After(deadline) {
				t.Fatalf("connection %d not logged as a client's hello", i)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}
