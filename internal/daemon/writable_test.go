package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// fakeWritable answers writable with what result holds at each call.
type fakeWritable struct {
	mu     sync.Mutex
	calls  int
	result *daemonproto.WritableResult
	err    error
}

func (f *fakeWritable) Writable(context.Context) (*daemonproto.WritableResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	r := *f.result
	return &r, nil
}

func (f *fakeWritable) set(r *daemonproto.WritableResult, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.result, f.err = r, err
}

// writable answers any client that may read, from the service at every
// request; a daemon given none does not know the method.
func TestWritableMethod(t *testing.T) {
	none := newHarness(t)
	runInProcess(t, Options{Dir: none.dir, Version: "v1.0.0", Idle: time.Minute, Log: testLog{t}})
	if _, err := dialAs(t, none.dir, daemonproto.ClientCLI, 0).Writable(t.Context()); daemonproto.Kind(err) != daemonproto.KindUnknownMethod {
		t.Fatalf("without Writable: %v", err)
	}

	f := &fakeWritable{result: &daemonproto.WritableResult{Writable: []daemonproto.WritablePath{{Path: "/w", Envs: []string{"alpha"}}}}}
	dir, s := startServer(t, Options{Version: "v1.0.0", Idle: time.Minute, Log: testLog{t}, Writable: f})
	for _, kind := range []string{daemonproto.ClientCLI, daemonproto.ClientApp} {
		r, err := dialAs(t, dir, kind, 0).Writable(t.Context())
		if err != nil || len(r.Writable) != 1 || r.Writable[0].Path != "/w" {
			t.Fatalf("%s: %+v, %v", kind, r, err)
		}
	}
	// The host paths are the command line's restore's, not the web
	// page's.
	f.mu.Lock()
	before := f.calls
	f.mu.Unlock()
	if _, e := s.callWeb(t.Context(), daemonproto.MethodWritable, nil); webKind(e) != daemonproto.KindNotAllowed {
		t.Fatalf("web: %v", e)
	}
	f.mu.Lock()
	if f.calls != before {
		t.Fatal("the web page reached Writable")
	}
	f.mu.Unlock()
	c := dialAs(t, dir, daemonproto.ClientCLI, 0)
	// Each answer is the service's of the time, never an earlier one.
	f.set(&daemonproto.WritableResult{Writable: []daemonproto.WritablePath{{Path: "/w2", Envs: []string{"beta"}}}}, nil)
	if r, err := c.Writable(t.Context()); err != nil || len(r.Writable) != 1 || r.Writable[0].Path != "/w2" {
		t.Fatalf("a stale answer: %+v, %v", r, err)
	}
	f.set(nil, errors.New("the engine does not answer"))
	if _, err := c.Writable(t.Context()); daemonproto.Kind(err) != daemonproto.KindInternal || !strings.Contains(err.Error(), "the engine does not answer") {
		t.Fatalf("an error of the service: %v", err)
	}
	// An answer the daemon cannot vouch for is never sent.
	f.set(&daemonproto.WritableResult{Writable: []daemonproto.WritablePath{{Path: "/w/\x1b[2J", Envs: []string{"alpha"}}}}, nil)
	if _, err := c.Writable(t.Context()); daemonproto.Kind(err) != daemonproto.KindInternal {
		t.Fatalf("an invalid answer: %v", err)
	}
	if err := c.Ping(t.Context()); err != nil {
		t.Fatalf("the connection did not survive: %v", err)
	}
}

// A client of a newer protocol is told to replace the daemon, and does
// not reach the service.
func TestWritableRefusals(t *testing.T) {
	f := &fakeWritable{result: &daemonproto.WritableResult{}}
	h := newHarness(t)
	runInProcess(t, Options{Dir: h.dir, Version: "v1.0.0", Idle: time.Minute, Log: testLog{t}, Writable: f})
	if _, err := dialAs(t, h.dir, daemonproto.ClientCLI, daemonproto.ProtocolVersion+1).Writable(t.Context()); daemonproto.Kind(err) != daemonproto.KindProtocolMismatch {
		t.Errorf("a newer client: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls != 0 {
		t.Errorf("%d calls served", f.calls)
	}
}
