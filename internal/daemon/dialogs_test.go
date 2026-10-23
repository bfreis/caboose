package daemon

import (
	"fmt"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// fakeDialogs records what the daemon tells the queue, and hands the
// holder one request.
type fakeDialogs struct {
	mu     sync.Mutex
	calls  []string
	holder uint64
}

func (f *fakeDialogs) SetApp(n uint64, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("%d:%v", n, on))
	switch {
	case on:
		f.holder = n
	case f.holder == n:
		f.holder = 0
	}
}

func (f *fakeDialogs) Next(n uint64) (*daemonproto.DialogRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("next %d", n))
	if n != f.holder {
		return nil, daemonproto.NewError(daemonproto.KindBadRequest, "not the holder")
	}
	return &daemonproto.DialogRef{Env: "alpha", ID: 7}, nil
}

func (f *fakeDialogs) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// dialogs is the attached app's alone, and only once it attached with
// dialogs: the command line and the page are not allowed, another app
// connection is refused, and an attachment without dialogs too. The
// queue hears of each attachment by its number, and of its end.
func TestAppDialogs(t *testing.T) {
	fd := &fakeDialogs{}
	dir, s := startServer(t, Options{Version: "v1.0.0", Idle: time.Minute, Log: testLog{t}, Dialogs: fd,
		peer: func(net.Conn) (peerCred, error) { return peerCred{UID: ownUID(), PID: 4242}, nil }})
	cli := dialAs(t, dir, daemonproto.ClientCLI, 0)
	if _, err := cli.Dialogs(t.Context()); daemonproto.Kind(err) != daemonproto.KindNotAllowed {
		t.Errorf("dialogs from the cli: %v", err)
	}
	if _, e := s.callWeb(t.Context(), daemonproto.MethodDialogs, paramsFor(daemonproto.MethodDialogs)); webKind(e) != daemonproto.KindNotAllowed {
		t.Errorf("dialogs from the web: %v", e)
	}
	a1 := dialAs(t, dir, daemonproto.ClientApp, 0)
	if _, err := a1.Dialogs(t.Context()); daemonproto.Kind(err) != daemonproto.KindBadRequest {
		t.Errorf("dialogs before attaching: %v", err)
	}
	if _, err := attach(t, a1, `{"capabilities":["notifications"]}`); err != nil {
		t.Fatal(err)
	}
	if _, err := a1.Dialogs(t.Context()); daemonproto.Kind(err) != daemonproto.KindBadRequest {
		t.Errorf("dialogs without the capability: %v", err)
	}
	if _, err := attach(t, a1, `{"capabilities":["dialogs"]}`); err != nil {
		t.Fatal(err)
	}
	if d, err := a1.Dialogs(t.Context()); err != nil || d == nil || *d != (daemonproto.DialogRef{Env: "alpha", ID: 7}) {
		t.Errorf("dialogs: %+v %v", d, err)
	}
	a2 := dialAs(t, dir, daemonproto.ClientApp, 0)
	if _, err := a2.Dialogs(t.Context()); daemonproto.Kind(err) != daemonproto.KindBadRequest {
		t.Errorf("dialogs from an app not attached: %v", err)
	}
	a1.Close()
	waitFor(t, 5*time.Second, "the queue hears the app went away", func() bool {
		n := 0
		for _, c := range fd.log() {
			if c == "1:false" {
				n++
			}
		}
		return n == 2
	})
	if _, err := attach(t, a2, `{"capabilities":["dialogs"]}`); err != nil {
		t.Fatal(err)
	}
	if _, err := a2.Dialogs(t.Context()); err != nil {
		t.Errorf("the next app: %v", err)
	}
	want := []string{"1:false", "1:true", "next 1", "1:false", "2:true", "next 2"}
	if got := fd.log(); !slices.Equal(got, want) {
		t.Errorf("the queue heard %v, want %v", got, want)
	}
}
