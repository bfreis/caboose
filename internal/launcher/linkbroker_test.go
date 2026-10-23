package launcher

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/policy"
)

// linkActions are this machine's actions, faked: what was opened, and
// what a dialog answers.
type linkActions struct {
	mu      sync.Mutex
	opened  []string
	asked   []string
	answer  bool
	failAsk bool
}

func (a *linkActions) OpenURL(u string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.opened = append(a.opened, u)
	return nil
}

func (a *linkActions) Notify(string, string) error { return nil }

func (a *linkActions) Confirm(prompt string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, prompt)
	if a.failAsk {
		return false, errors.New("no dialog here")
	}
	return a.answer, nil
}

func (a *linkActions) state() (opened, asked []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.opened...), append([]string(nil), a.asked...)
}

// brokerEnv is an environment in a temporary CABOOSE_HOME whose
// config.toml says text, and an App on it.
func brokerEnv(t *testing.T, text string) (*App, func(string)) {
	t.Helper()
	home := t.TempDir()
	env := map[string]string{"HOME": home, "CABOOSE_HOME": filepath.Join(home, ".caboose")}
	getenv := func(k string) string { return env[k] }
	write := func(s string) {
		dir := config.EnvDirFor(env["CABOOSE_HOME"], config.DefaultEnv)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(text)
	cfg, err := config.Load(getenv, config.OSFS{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return &App{Cfg: cfg}, write
}

// brokerRunner is a link runner for app holding its link.lock, with its
// broker, and a link it serves to the returned agent end, dialled.
func brokerRunner(t *testing.T, app *App, actions *linkActions) *linkRunner {
	t.Helper()
	lock, unlock, err := lockLinkFile(app.Cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	r, err := app.newLinkRunner(linkRun{out: io.Discard, lock: lock, actions: actions})
	if err != nil {
		t.Fatal(err)
	}
	if r.broker == nil || r.cfg.Decide == nil {
		t.Fatal("the link opened no broker")
	}
	t.Cleanup(r.closeBroker)
	return r
}

// A link that holds link.lock opens the environment's broker and decides
// through it; one handed a lock it does not hold opens none, and does not
// start: only the process holding link.lock serves the broker.
func TestLinkOpensBroker(t *testing.T) {
	app, _ := brokerEnv(t, "[link]\nopen_urls = \"ask\"\n")
	r := brokerRunner(t, app, &linkActions{})
	o, err := r.cfg.Decide.Decide(policy.Request{Kind: policy.KindOpenURL, URL: "https://example.com/", Origin: "https://example.com"})
	if err != nil || o.Verdict != broker.Pending || o.Request == 0 {
		t.Fatalf("decided %+v, %v", o, err)
	}
	if _, err := os.Stat(filepath.Join(app.Cfg.EnvDir, "requests.json")); err != nil {
		t.Fatalf("no requests.json in the environment's dir: %v", err)
	}

	other, err := os.OpenFile(filepath.Join(app.Cfg.DataDir, linkLockFile), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := app.newLinkRunner(linkRun{out: io.Discard, lock: other, actions: &linkActions{}}); err == nil || !strings.Contains(err.Error(), "link.lock") {
		t.Fatalf("a link without the lock: %v", err)
	}
	// No lock at all (as tests and the builder link run): no broker.
	if r, err := app.newLinkRunner(linkRun{out: io.Discard, actions: &linkActions{}}); err != nil || r.broker != nil || r.cfg.Decide != nil {
		t.Fatalf("no lock: %v, %+v", err, r)
	}
}

// A link with no broker still knows the environment's configured trust,
// so what decides for it fails closed under hostile, and is fallible only
// when config.toml says (or defaults to) fallible.
func TestLinkWithoutBrokerCarriesTrust(t *testing.T) {
	for text, want := range map[string]string{"": "fallible", "trust = \"hostile\"\n": "hostile"} {
		app, _ := brokerEnv(t, text)
		r, err := app.newLinkRunner(linkRun{out: io.Discard, actions: &linkActions{}})
		if err != nil {
			t.Fatal(err)
		}
		if r.cfg.Trust != want {
			t.Errorf("config %q: the link's trust is %q, want %q", text, r.cfg.Trust, want)
		}
	}
}

// The policy follows config.toml as the link rereads it: trust, which
// the link's settings carry too, and open_urls_allow, which only the
// broker reads.
func TestLinkReloadsPolicy(t *testing.T) {
	app, write := brokerEnv(t, "[link]\nopen_urls = \"ask\"\nhost_exec = true\n")
	r := brokerRunner(t, app, &linkActions{})
	d := r.cfg.Decide
	if o, _ := d.Decide(policy.Request{Kind: policy.KindHostExec}); o.Verdict != broker.Allowed {
		t.Fatalf("host exec, fallible: %+v", o)
	}
	if r.cfg.HostExec == nil {
		t.Fatal("host exec not offered")
	}
	write("trust = \"hostile\"\n[link]\nopen_urls = \"ask\"\nhost_exec = true\n")
	r.reload()
	if o, _ := d.Decide(policy.Request{Kind: policy.KindHostExec}); o.Verdict != broker.Denied || !strings.Contains(o.Reason, "hostile") {
		t.Fatalf("host exec, hostile: %+v", o)
	}
	if r.cfg.HostExec != nil {
		t.Fatal("host exec still offered under hostile")
	}
	write("[link]\nopen_urls = \"ask\"\nopen_urls_allow = [\"https://docs.example.com\"]\n")
	r.reload()
	if o, _ := d.Decide(policy.Request{Kind: policy.KindOpenURL, URL: "https://docs.example.com/x", Origin: "https://docs.example.com"}); o.Verdict != broker.Allowed {
		t.Fatalf("open_urls_allow: %+v", o)
	}
	// A file that does not read keeps the policy in use.
	write("[link\n")
	r.reload()
	if o, _ := d.Decide(policy.Request{Kind: policy.KindOpenURL, URL: "https://docs.example.com/x", Origin: "https://docs.example.com"}); o.Verdict != broker.Allowed {
		t.Fatalf("after a broken edit: %+v", o)
	}
}

// End to end over a dialled link: the agent's open returns at once,
// pending; the dialog asks, one at a time, naming the environment and the
// URL the host checked; its yes opens the URL, by the broker; its no
// denies the request; a dialog that cannot be shown leaves it pending.
func TestLinkURLDialog(t *testing.T) {
	app, _ := brokerEnv(t, "[link]\nopen_urls = \"ask\"\n")
	actions := &linkActions{answer: true}
	r := brokerRunner(t, app, actions)
	host, agentEnd := net.Pipe()
	box := linkerBox{t: t, dial: func() (io.ReadWriteCloser, error) { return host, nil }}
	app.Backend = box
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.serveDialed(ctx, box)
	}()
	defer func() { cancel(); <-done }()

	s := agentproto.NewSession(agentEnd, agentEnd, false)
	defer s.Close()
	hello, _ := agentproto.Decode(<-s.Control())
	if !hello.Broker {
		t.Fatalf("the hello offers no requests: %+v", hello)
	}
	_ = s.Send(agentproto.Message{Type: agentproto.TypeHello, Version: agentproto.Version})
	var id uint64
	open := func(u string) agentproto.Message {
		id++
		if err := s.Send(agentproto.Message{Type: agentproto.TypeRequest, ID: id, Op: agentproto.OpOpen, URL: u}); err != nil {
			t.Fatal(err)
		}
		for {
			select {
			case b := <-s.Control():
				m, _ := agentproto.Decode(b)
				if m.Type == agentproto.TypeResponse && m.ID == id {
					return m
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no answer to open")
			}
		}
	}
	start := time.Now()
	m := open("https://example.com/yes")
	if m.OK || m.Reason != agentproto.ReasonPending || m.RequestID == 0 || time.Since(start) > 2*time.Second {
		t.Fatalf("open: %+v after %v", m, time.Since(start))
	}
	waitUntil(t, func() bool { o, _ := actions.state(); return len(o) == 1 })
	opened, asked := actions.state()
	if opened[0] != "https://example.com/yes" || len(asked) != 1 || !strings.Contains(asked[0], "environment "+app.Cfg.Env) || !strings.Contains(asked[0], "https://example.com/yes") {
		t.Fatalf("opened %v, asked %q", opened, asked)
	}
	if q, _ := r.broker.Request(m.RequestID); q.State != broker.StateAllowed || q.DecidedBy != broker.ByDialog {
		t.Fatalf("request %+v", q)
	}

	actions.mu.Lock()
	actions.answer = false
	actions.mu.Unlock()
	m = open("https://example.com/no")
	waitUntil(t, func() bool { q, _ := r.broker.Request(m.RequestID); return q.State == broker.StateDenied })
	if o, _ := actions.state(); len(o) != 1 {
		t.Fatalf("a refused URL opened: %v", o)
	}

	actions.mu.Lock()
	actions.failAsk = true
	actions.mu.Unlock()
	m = open("https://example.com/later")
	waitUntil(t, func() bool { _, a := actions.state(); return len(a) == 3 })
	time.Sleep(50 * time.Millisecond)
	if q, _ := r.broker.Request(m.RequestID); q.State != broker.StatePending {
		t.Fatalf("a dialog that failed decided %+v", q)
	}
}

// A request answered before its dialog's turn shows no dialog, and a
// dialog answering one already answered is dropped.
func TestURLDialogAfterAnswer(t *testing.T) {
	app, _ := brokerEnv(t, "[link]\nopen_urls = \"ask\"\n")
	actions := &linkActions{answer: true}
	r := brokerRunner(t, app, actions)
	// Hold the queue (as an attached app would, which never polls) until
	// the CLI has answered the second.
	r.dialog.queue.SetApp(99, true)
	o, _ := r.cfg.Decide.Decide(policy.Request{Kind: policy.KindOpenURL, URL: "https://example.com/", Origin: "https://example.com"})
	o2, _ := r.cfg.Decide.Decide(policy.Request{Kind: policy.KindOpenURL, URL: "https://other.example/", Origin: "https://other.example"})
	if _, err := r.broker.Answer(o2.Request, broker.Reply{Verdict: broker.ReplyDeny, Scope: broker.ScopeOnce, By: broker.ByCLI}); err != nil {
		t.Fatal(err)
	}
	r.dialog.queue.SetApp(99, false)
	waitUntil(t, func() bool { q, _ := r.broker.Request(o.Request); return q.State != broker.StatePending })
	time.Sleep(50 * time.Millisecond)
	_, asked := actions.state()
	for _, a := range asked {
		if strings.Contains(a, "other.example") {
			t.Fatalf("asked about a request already answered: %q", a)
		}
	}
	// The dialog's own answer, late, is dropped.
	var logged bytes.Buffer
	d := &urlDialog{actions: actions, answers: r.broker, logf: func(f string, a ...any) { logged.WriteString(f) }}
	d.show(app.Cfg.Env, o2.Request, "https://other.example/")
	if q, _ := r.broker.Request(o2.Request); q.State != broker.StateDenied || q.DecidedBy != broker.ByCLI {
		t.Fatalf("the CLI's answer was overturned: %+v", q)
	}
}

// The agent's count of sessions with the mod reaches the runner.
func TestLinkKeepsMods(t *testing.T) {
	app, _ := brokerEnv(t, "")
	r := brokerRunner(t, app, &linkActions{})
	if _, ok := r.lastMods(); ok {
		t.Fatal("a count before any came")
	}
	r.cfg.Mods(agentproto.ModsInfo{Sessions: 3, WithMod: 2})
	if m, ok := r.lastMods(); !ok || m.Sessions != 3 || m.WithMod != 2 {
		t.Fatalf("got %+v %v", m, ok)
	}
}

// waitUntil waits for cond.
func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("timed out")
}

// fakeAnswers records the answers a dialog gives.
type fakeAnswers struct {
	mu       sync.Mutex
	answered int
}

func (f *fakeAnswers) Request(uint64) (broker.RequestInfo, error) {
	return broker.RequestInfo{State: broker.StatePending}, nil
}

func (f *fakeAnswers) MarkInFlight(uint64, string) error { return nil }

func (f *fakeAnswers) DenyInFlight(uint64, string) error { return nil }

func (f *fakeAnswers) Answer(uint64, broker.Reply) (broker.AnswerResult, error) {
	f.mu.Lock()
	f.answered++
	f.mu.Unlock()
	return broker.AnswerResult{}, nil
}

// heldActions holds a dialog open until released.
type heldActions struct {
	linkActions
	shown, release chan struct{}
}

func (h *heldActions) Confirm(string) (bool, error) {
	close(h.shown)
	<-h.release
	return true, nil
}

// A dialog still showing when the link stops answers nothing: the link
// lets go of link.lock next, and the environment may be another's.
func TestURLDialogClosed(t *testing.T) {
	fa := &fakeAnswers{}
	h := &heldActions{shown: make(chan struct{}), release: make(chan struct{})}
	d := &urlDialog{actions: h, answers: fa, logf: func(string, ...any) {}}
	done := make(chan struct{})
	go func() { defer close(done); d.show("default", 1, "https://example.com/") }()
	<-h.shown
	d.close()
	close(h.release)
	<-done
	if fa.answered != 0 {
		t.Fatal("a dialog answered after the link stopped")
	}
	// And the link's closing closes its dialogs.
	app, _ := brokerEnv(t, "")
	r := brokerRunner(t, app, &linkActions{})
	r.closeBroker()
	if r.dialog.serving(func() {}) {
		t.Fatal("closing the broker left its dialog serving")
	}
}
