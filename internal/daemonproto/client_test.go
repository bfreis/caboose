package daemonproto

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/saferm"
)

// fakeServer listens on a temp socket and runs handle for each connection.
// The socket lives in a short directory the test makes itself (a Unix socket
// path is limited to ~104 bytes), removed by that exact path at cleanup.
type fakeServer struct {
	path string
	ln   net.Listener
	mu   sync.Mutex
	open []net.Conn
	wg   sync.WaitGroup
}

func newFake(t *testing.T, handle func(*Conn)) *fakeServer {
	t.Helper()
	dir, err := os.MkdirTemp("", "cbd")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{path: filepath.Join(dir, "d.sock")}
	s.ln, err = net.Listen("unix", s.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.ln.Close()
		s.mu.Lock()
		for _, c := range s.open {
			c.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
		if err := saferm.RemoveRoot(dir); err != nil {
			t.Error(err)
		}
	})
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			nc, err := s.ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.open = append(s.open, nc)
			s.mu.Unlock()
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer nc.Close()
				handle(NewConn(nc))
			}()
		}
	}()
	return s
}

// serve is a daemon that completes the handshake and answers requests with
// reply until the client goes.
func serve(reply func(c *Conn, req *Frame) bool) func(*Conn) {
	return func(c *Conn) {
		if _, _, err := ServerHandshake(c, "v9", time.Second); err != nil {
			return
		}
		for {
			req, err := c.Read(0)
			if err != nil {
				return
			}
			if !reply(c, req) {
				return
			}
		}
	}
}

func echo(c *Conn, req *Frame) bool {
	var f *Frame
	switch req.Method {
	case MethodStatus:
		f, _ = Result(req.ID, &StatusResult{Daemon: DaemonInfo{PID: 4, Version: "v9", Protocol: 1}, Envs: []EnvStatus{{Name: "foo", Sandbox: SandboxRunning, Link: LinkRunning}}})
	case MethodEnsureLink, MethodRestartLink, MethodStopLink:
		var p EnvParams
		if e := DecodeParams(req, &p); e != nil {
			f = Failure(req.ID, e)
		} else if p.Env == "gone" {
			f = Failure(req.ID, NewError(KindUnknownEnv, "no environment %q", p.Env))
		} else {
			f, _ = Result(req.ID, &LinkResult{Action: ActionStarted, Env: EnvStatus{Name: p.Env}})
		}
	case MethodReplace:
		var p ReplaceParams
		_ = DecodeParams(req, &p)
		f, _ = Result(req.ID, &ReplaceResult{Exiting: p.Force})
	case MethodPing, MethodStop:
		f, _ = Result(req.ID, nil)
	default:
		f = Failure(req.ID, NewError(KindUnknownMethod, "no method %q", req.Method))
	}
	return c.Write(f, time.Second) == nil
}

var opts = Options{Client: ClientCLI, Version: "v1", DialTimeout: time.Second, HelloTimeout: 300 * time.Millisecond, RequestTimeout: 300 * time.Millisecond}

func dial(t *testing.T, s *fakeServer) *Client {
	t.Helper()
	c, err := Dial(context.Background(), s.path, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestClientCalls(t *testing.T) {
	s := newFake(t, serve(echo))
	c := dial(t, s)
	ctx := context.Background()
	if d := c.Daemon(); d.Version != "v9" || d.Protocol != ProtocolVersion || c.Compat() != CompatSame {
		t.Errorf("daemon hello %+v", d)
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := c.Status(ctx)
	if err != nil || st.Daemon.PID != 4 || len(st.Envs) != 1 || st.Envs[0].Name != "foo" {
		t.Fatalf("%v %+v", err, st)
	}
	for _, call := range []func(context.Context, string) (*LinkResult, error){c.EnsureLink, c.RestartLink, c.StopLink} {
		r, err := call(ctx, "foo")
		if err != nil || r.Env.Name != "foo" {
			t.Fatalf("%v %+v", err, r)
		}
	}
	if _, err := c.EnsureLink(ctx, "gone"); !errors.Is(err, &Error{Kind: KindUnknownEnv}) {
		t.Errorf("unknown env: %v", err)
	}
	if _, err := c.EnsureLink(ctx, "../x"); Kind(err) != KindBadRequest {
		t.Errorf("bad env must be refused before sending: %v", err)
	}
	if err := c.Call(ctx, "nope", nil, nil); Kind(err) != KindUnknownMethod {
		t.Errorf("unknown method: %v", err)
	}
	if r, err := c.Replace(ctx, "v2", true); err != nil || !r.Exiting {
		t.Errorf("replace: %v %+v", err, r)
	}
	if err := c.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	// A refused request left the connection usable.
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestClientSkipsEvents(t *testing.T) {
	s := newFake(t, serve(func(c *Conn, req *Frame) bool {
		_ = c.Write(&Frame{Type: TypeEvent, Event: "tick"}, time.Second)
		f, _ := Result(req.ID, nil)
		return c.Write(f, time.Second) == nil
	}))
	if err := dial(t, s).Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDialUnavailable(t *testing.T) {
	dir, err := os.MkdirTemp("", "cbd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = saferm.RemoveRoot(dir) })
	_, err = Dial(context.Background(), filepath.Join(dir, "none.sock"), opts)
	if Kind(err) != KindUnavailable {
		t.Errorf("no socket: %v", err)
	}
	// A stale socket file nothing listens on.
	ln, _ := net.Listen("unix", filepath.Join(dir, "stale.sock"))
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	_, err = Dial(context.Background(), filepath.Join(dir, "stale.sock"), opts)
	if Kind(err) != KindUnavailable {
		t.Errorf("stale socket: %v", err)
	}
	_, err = Dial(context.Background(), "/"+strings.Repeat("a", MaxSocketPath), opts)
	if Kind(err) != KindBadRequest || !strings.Contains(err.Error(), "CABOOSE_HOME") {
		t.Errorf("long path: %v", err)
	}
}

func TestHelloTimeout(t *testing.T) {
	done := make(chan struct{})
	s := newFake(t, func(c *Conn) { <-done }) // accepts, never speaks
	t.Cleanup(func() { close(done) })
	start := time.Now()
	_, err := Dial(context.Background(), s.path, opts)
	if Kind(err) != KindTimeout || !strings.Contains(err.Error(), "caboose daemon restart") {
		t.Errorf("silent daemon: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

func TestRequestTimeoutBreaksConnection(t *testing.T) {
	done := make(chan struct{})
	s := newFake(t, serve(func(c *Conn, req *Frame) bool { <-done; return false }))
	t.Cleanup(func() { close(done) })
	c := dial(t, s)
	start := time.Now()
	err := c.Ping(context.Background())
	if Kind(err) != KindTimeout {
		t.Fatalf("hung request: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
	// Late answers must never be read as the next call's.
	if err := c.Ping(context.Background()); Kind(err) != KindUnavailable {
		t.Errorf("after a timeout: %v", err)
	}
}

func TestContextEndsCall(t *testing.T) {
	done := make(chan struct{})
	s := newFake(t, serve(func(c *Conn, req *Frame) bool { <-done; return false }))
	t.Cleanup(func() { close(done) })
	o := opts
	o.RequestTimeout = time.Minute
	c, err := Dial(context.Background(), s.path, o)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.Ping(ctx); Kind(err) != KindTimeout {
		t.Errorf("deadline: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

func TestPeerSendsGarbage(t *testing.T) {
	for name, bad := range map[string]string{
		"not json":      "garbage\n",
		"oversize":      strings.Repeat("x", MaxLine+10) + "\n",
		"wrong id":      `{"type":"response","id":99,"result":{}}` + "\n",
		"request frame": `{"type":"request","id":1,"method":"ping"}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			s := newFake(t, func(c *Conn) {
				if _, _, err := ServerHandshake(c, "v9", time.Second); err != nil {
					return
				}
				if _, err := c.Read(time.Second); err != nil {
					return
				}
				_, _ = c.Raw().Write([]byte(bad))
			})
			c := dial(t, s)
			if err := c.Ping(context.Background()); Kind(err) != KindBadFrame {
				t.Errorf("got %v", err)
			}
			if err := c.Ping(context.Background()); Kind(err) != KindUnavailable {
				t.Errorf("after garbage: %v", err)
			}
		})
	}
}

func TestPeerClosesMidCall(t *testing.T) {
	s := newFake(t, serve(func(c *Conn, req *Frame) bool { return false }))
	if err := dial(t, s).Ping(context.Background()); Kind(err) != KindUnavailable {
		t.Errorf("got %v", err)
	}
}

func TestNotADaemon(t *testing.T) {
	s := newFake(t, func(c *Conn) {
		_, _ = c.Read(time.Second)
		_ = c.Write(Hello(ClientCLI, "v"), time.Second) // a client's hello, not a daemon's
	})
	if _, err := Dial(context.Background(), s.path, opts); Kind(err) != KindBadFrame {
		t.Errorf("got %v", err)
	}
}

// rawHandshake plays a client of the given protocol against ServerHandshake.
func rawHandshake(t *testing.T, serverProto, protocol int) (compat Compat, got []*Frame, srvErr error) {
	t.Helper()
	return rawHandshakeAs(t, ClientCLI, serverProto, protocol)
}

// rawHandshakeAs is rawHandshake with a hello naming kind.
func rawHandshakeAs(t *testing.T, kind string, serverProto, protocol int) (compat Compat, got []*Frame, srvErr error) {
	t.Helper()
	srv, cli := net.Pipe()
	defer cli.Close()
	res := make(chan error, 1)
	var cmp Compat
	go func() {
		defer srv.Close()
		_, c, err := serverHandshake(NewConn(srv), "v9", serverProto, time.Second)
		cmp = c
		res <- err
	}()
	h := Hello(kind, "v1")
	h.Protocol = protocol
	go func() { _ = Write(cli, h) }()
	r := NewReader(cli)
	_ = cli.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		f, err := r.Read()
		if err != nil {
			break
		}
		got = append(got, f)
	}
	return cmp, got, <-res
}

func TestVersionSkew(t *testing.T) {
	cmp, got, err := rawHandshake(t, 1, 1)
	if err != nil || cmp != CompatSame || len(got) != 1 || got[0].Type != TypeHello || got[0].Client != ClientDaemon {
		t.Errorf("same: %v %v %v", cmp, err, got)
	}

	// A newer client is let through.
	cmp, got, err = rawHandshake(t, 1, 2)
	if err != nil || cmp != CompatClientNewer || len(got) != 1 || got[0].Protocol != 1 {
		t.Errorf("newer: %v %v %v", cmp, err, got)
	}

	// An older one gets the daemon's hello, then a protocol_mismatch, and the server errors.
	cmp, got, err = rawHandshake(t, 2, 1)
	if cmp != CompatClientOlder || Kind(err) != KindProtocolMismatch {
		t.Errorf("older: %v %v", cmp, err)
	}
	if len(got) != 2 || got[0].Type != TypeHello || got[0].Protocol != 2 || got[1].Error == nil || got[1].Error.Kind != KindProtocolMismatch || got[1].ID != 0 {
		t.Errorf("older client saw %+v", got)
	}
}

func TestDialAgainstOtherProtocols(t *testing.T) {
	handler := func(proto int) func(*Conn) {
		return func(c *Conn) {
			if _, _, err := serverHandshake(c, "vX", proto, time.Second); err != nil {
				return
			}
			for {
				req, err := c.Read(0)
				if err != nil {
					return
				}
				f, _ := Result(req.ID, &ReplaceResult{Exiting: true})
				_ = c.Write(f, time.Second)
			}
		}
	}
	// A client newer than the daemon: dial works, and replace is understood.
	s := newFake(t, handler(1))
	o := opts
	o.Protocol = 2
	c, err := Dial(context.Background(), s.path, o)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Compat() != CompatClientNewer {
		t.Fatalf("compat %v", c.Compat())
	}
	if r, err := c.Replace(context.Background(), "v2", true); err != nil || !r.Exiting {
		t.Fatalf("%v %+v", err, r)
	}
	// A client older than the daemon refuses and says what to run.
	s2 := newFake(t, handler(2))
	_, err = Dial(context.Background(), s2.path, opts)
	if Kind(err) != KindProtocolMismatch || !strings.Contains(err.Error(), "caboose daemon stop") {
		t.Errorf("older client: %v", err)
	}
}

func TestServerRefusesBadFirstFrame(t *testing.T) {
	for name, first := range map[string]string{
		"a request first": `{"type":"request","id":1,"method":"ping"}` + "\n",
		"garbage":         "nonsense\n",
		"daemon hello":    `{"type":"hello","protocol":1,"version":"v","client":"daemon"}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			srv, cli := net.Pipe()
			defer cli.Close()
			done := make(chan error, 1)
			go func() {
				defer srv.Close()
				_, _, err := ServerHandshake(NewConn(srv), "v9", time.Second)
				done <- err
			}()
			go func() { _, _ = cli.Write([]byte(first)) }()
			f, _ := NewReader(cli).Read()
			if err := <-done; err == nil {
				t.Error("accepted")
			}
			if f != nil && (f.Error == nil || f.Error.Kind != KindBadRequest) {
				t.Errorf("answer %+v", f)
			}
		})
	}
}

func TestServerHandshakeTimeout(t *testing.T) {
	srv, cli := net.Pipe()
	defer cli.Close()
	defer srv.Close()
	start := time.Now()
	_, _, err := ServerHandshake(NewConn(srv), "v9", 100*time.Millisecond)
	if err == nil || time.Since(start) > 2*time.Second {
		t.Errorf("%v after %v", err, time.Since(start))
	}
}

func TestEventFloodCannotStretchACall(t *testing.T) {
	s := newFake(t, serve(func(c *Conn, req *Frame) bool {
		for c.Write(&Frame{Type: TypeEvent, Event: "tick"}, time.Second) == nil {
		}
		return false
	}))
	c := dial(t, s)
	start := time.Now()
	err := c.Ping(context.Background())
	if k := Kind(err); k != KindBadFrame && k != KindTimeout {
		t.Errorf("got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

func TestSlowDripCannotStretchACall(t *testing.T) {
	// One byte at a time, forever, never a newline.
	s := newFake(t, serve(func(c *Conn, req *Frame) bool {
		for {
			if _, err := c.Raw().Write([]byte("x")); err != nil {
				return false
			}
			time.Sleep(20 * time.Millisecond)
		}
	}))
	c := dial(t, s)
	start := time.Now()
	if err := c.Ping(context.Background()); Kind(err) != KindTimeout {
		t.Errorf("got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

func TestFailureClosesTheSocket(t *testing.T) {
	closed := make(chan struct{})
	s := newFake(t, serve(func(c *Conn, req *Frame) bool {
		_, _ = c.Raw().Write([]byte("garbage\n"))
		_, err := c.Read(2 * time.Second) // the client's close shows as EOF
		if err != nil {
			close(closed)
		}
		return false
	}))
	c := dial(t, s)
	if err := c.Ping(context.Background()); Kind(err) != KindBadFrame {
		t.Fatalf("got %v", err)
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Error("the client left the socket open after a failure")
	}
}

func TestStatusStringsChecked(t *testing.T) {
	s := newFake(t, serve(func(c *Conn, req *Frame) bool {
		f, _ := Result(req.ID, &StatusResult{Envs: []EnvStatus{{Name: "foo", LastError: "boom \u202e evil"}}})
		return c.Write(f, time.Second) == nil
	}))
	c := dial(t, s)
	if _, err := c.Status(context.Background()); Kind(err) != KindBadFrame {
		t.Errorf("got %v", err)
	}
}

// No socket client is the daemon: a hello naming daemon gets a
// bad_request and no daemon hello, and the server errors before any
// request.
func TestServerHandshakeRefusesDaemon(t *testing.T) {
	for _, kind := range []string{ClientDaemon} {
		_, got, err := rawHandshakeAs(t, kind, 1, 1)
		if err == nil || len(got) != 1 || got[0].Type != TypeResponse || got[0].Error == nil || got[0].Error.Kind != KindBadRequest {
			t.Errorf("%s: err %v, frames %+v", kind, err, got)
		}
	}
	for _, kind := range []string{ClientCLI} {
		if _, got, err := rawHandshakeAs(t, kind, 1, 1); err != nil || len(got) != 1 || got[0].Type != TypeHello {
			t.Errorf("%s: err %v, frames %+v", kind, err, got)
		}
	}
}
