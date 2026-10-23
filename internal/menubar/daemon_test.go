package menubar

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/saferm"
)

// realDaemon reaches a real daemon's socket; it never starts one.
type realDaemon struct{ cfg daemon.Config }

func (d realDaemon) Connect(ctx context.Context) (Conn, error) {
	c, err := daemon.Connect(ctx, d.cfg)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (d realDaemon) Ensure(ctx context.Context) (Conn, error) { return d.Connect(ctx) }

func (d realDaemon) Replaced() (daemon.Replaced, bool) {
	r, err := daemon.ReadReplaced(d.cfg.Dir)
	return r, err == nil
}

// dialogQueue is the daemon's URL dialog queue, scripted: it hands out
// queued requests to the attachment holding it, and denies the one in
// flight when that attachment ends.
type dialogQueue struct {
	mu       sync.Mutex
	holder   uint64
	queued   []daemonproto.DialogRef
	inFlight *daemonproto.DialogRef
	denied   []daemonproto.DialogRef
}

func (q *dialogQueue) SetApp(n uint64, on bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	switch {
	case on:
		q.holder = n
	case q.holder == n:
		q.holder = 0
		if q.inFlight != nil {
			q.denied = append(q.denied, *q.inFlight)
			q.inFlight = nil
		}
	}
}

func (q *dialogQueue) Next(n uint64) (*daemonproto.DialogRef, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if n != q.holder {
		return nil, daemonproto.NewError(daemonproto.KindBadRequest, "not the holder")
	}
	if q.inFlight == nil && len(q.queued) > 0 {
		r := q.queued[0]
		q.queued = q.queued[1:]
		q.inFlight = &r
	}
	if q.inFlight == nil {
		return nil, nil
	}
	r := *q.inFlight
	return &r, nil
}

func (q *dialogQueue) push(r daemonproto.DialogRef) {
	q.mu.Lock()
	q.queued = append(q.queued, r)
	q.mu.Unlock()
}

// brokers answers request and answer for open_url requests, recording
// who answered what.
type brokers struct {
	q       *dialogQueue
	mu      sync.Mutex
	answers []string
}

func (b *brokers) Call(_ context.Context, who, method, env string, params json.RawMessage) (any, error) {
	switch method {
	case daemonproto.MethodRequest:
		var p daemonproto.RequestParams
		_ = json.Unmarshal(params, &p)
		return &daemonproto.RequestResult{Request: daemonproto.RequestView{ID: p.ID, Kind: "open_url", Key: "open_url:https://example.com",
			Effect: "open https://example.com/x in the browser", State: "pending", Created: t0}}, nil
	case daemonproto.MethodAnswer:
		b.mu.Lock()
		b.answers = append(b.answers, who+" "+string(params))
		b.mu.Unlock()
		var p daemonproto.AnswerParams
		_ = json.Unmarshal(params, &p)
		b.q.mu.Lock()
		if b.q.inFlight != nil && b.q.inFlight.ID == p.ID {
			b.q.inFlight = nil
		}
		b.q.mu.Unlock()
		return &daemonproto.AnswerResult{Request: daemonproto.RequestView{ID: p.ID, Kind: "open_url", State: "allowed", Created: t0}, Recorded: true}, nil
	}
	return nil, daemonproto.NewError(daemonproto.KindUnknownMethod, "%s", method)
}

// waitUntil polls cond, for what a real daemon does in its own time.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAgainstDaemon runs the core against a real daemon, in this
// process, on a socket in a short temp home: one app attached at a time,
// notifications routed to the app, URL dialogs answered as the app, and
// the dialog in flight denied when the app goes away.
func TestAgainstDaemon(t *testing.T) {
	home, err := os.MkdirTemp("", "cbd")
	if err != nil {
		t.Fatal(err)
	}
	dir := daemon.Dir(home, false)
	var fbMu sync.Mutex
	var fallback []string
	notifier := daemon.NewNotifier(func(title, text string) error {
		fbMu.Lock()
		fallback = append(fallback, title+"|"+text)
		fbMu.Unlock()
		return nil
	})
	q := &dialogQueue{}
	br := &brokers{q: q}
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() {
		ran <- daemon.Run(ctx, daemon.Options{Dir: dir, Version: "v1.0.0", Idle: time.Hour, Notifier: notifier,
			Dialogs: q, Brokers: br, Log: io.Discard})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-ran:
			if err != nil {
				t.Errorf("the daemon: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the daemon did not stop")
		}
		if err := saferm.RemoveRoot(home); err != nil {
			t.Errorf("removing %s: %v", home, err)
		}
	})
	cfg := daemon.Config{Dir: dir, Version: "v1.0.0", Client: daemonproto.ClientApp}
	waitUntil(t, "the daemon's socket", func() bool {
		c, err := daemon.Connect(t.Context(), cfg)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	cli, err := daemon.Connect(t.Context(), daemon.Config{Dir: dir, Version: "v1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	attached := func() bool {
		st, err := cli.Status(t.Context())
		return err == nil && st.Daemon.App != nil && st.Daemon.App.Attached
	}

	// Another app holds the attachment: this one says so and quits.
	first, err := daemon.Connect(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.AppAttach(t.Context(), daemonproto.AppAttachParams{}); err != nil {
		t.Fatal(err)
	}
	r := newRig(t, func(o *Options) { o.Daemon = realDaemon{cfg} }).start()
	if s := r.ui.snapshot(); len(s.alerts) != 1 || s.alerts[0] != TextBusy || s.quitted {
		t.Fatalf("a second app: alerts %v, quit %v", s.alerts, s.quitted)
	}
	r.do(func(e Events) { e.AlertClosed(BusyAlertID) })
	waitUntil(t, "the second app to quit once its alert is closed", func() bool { return r.ui.snapshot().quitted })
	first.Close()
	waitUntil(t, "the first app's attachment to end", func() bool { return !attached() })

	r = newRig(t, func(o *Options) { o.Daemon = realDaemon{cfg} }).start()
	r.do(func(e Events) { e.NotificationAuth(true) })
	if !attached() {
		t.Fatal("not attached")
	}
	st, _ := cli.Status(t.Context())
	if got := strings.Join(st.Daemon.App.Capabilities, ","); got != "dialogs,notifications" {
		t.Errorf("capabilities %s", got)
	}

	// A host notification goes to the app, not the system's.
	if err := notifier.For("dev", daemon.SourceDaemon).Notify("ignored", "dev restarted"); err != nil {
		t.Fatal(err)
	}
	r.advance(PollEvery)
	if n := r.ui.snapshot().notes; len(n) != 1 || n[0].Title != "caboose: dev" || n[0].Body != "dev restarted" {
		t.Errorf("notes %+v", n)
	}
	fbMu.Lock()
	if len(fallback) != 0 {
		t.Errorf("the system's showed %v", fallback)
	}
	fbMu.Unlock()

	// A URL dialog, asked and answered as the app.
	q.push(daemonproto.DialogRef{Env: "dev", ID: 7})
	r.advance(PollEvery)
	asks := r.ui.snapshot().asks
	if len(asks) != 1 || asks[0].URL != "https://example.com/x" {
		t.Fatalf("asks %+v", asks)
	}
	r.do(func(e Events) { e.URLAnswered(asks[0].ID, true) })
	br.mu.Lock()
	if len(br.answers) != 1 || br.answers[0] != `app {"env":"dev","id":7,"verdict":"allow","scope":"once"}` {
		t.Errorf("answers %v", br.answers)
	}
	br.mu.Unlock()

	// Another in flight when the app quits: the daemon's queue denies it.
	q.push(daemonproto.DialogRef{Env: "dev", ID: 8})
	r.advance(PollEvery)
	if n := len(r.ui.snapshot().asks); n != 2 {
		t.Fatalf("%d asks", n)
	}
	r.selectItem("quit")
	waitUntil(t, "the attachment to end", func() bool { return !attached() })
	waitUntil(t, "the app to quit with its URL dialog shown", func() bool { return r.ui.snapshot().quitted })
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.denied) != 1 || q.denied[0].ID != 8 {
		t.Errorf("denied %v", q.denied)
	}
}

func TestRealDaemonAdapterReplaced(t *testing.T) {
	dir := t.TempDir()
	if _, ok := (realDaemon{daemon.Config{Dir: dir}}).Replaced(); ok {
		t.Error("a replaced.json where there is none")
	}
	if _, err := daemon.ReadReplaced(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadReplaced: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, daemon.ReplacedFile), []byte(`{"version":"v2.0.0","protocol":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r, ok := (realDaemon{daemon.Config{Dir: dir}}).Replaced()
	if !ok || MayStart(false, r, ok, 1, "v1.0.0") {
		t.Errorf("replaced %+v %v", r, ok)
	}
}
