package menubar

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// testCode is the one-time code the fake daemon hands out.
var testCode = strings.Repeat("T", daemonproto.CodeLen)

// webResult is web's answer, with a code only when asked for one.
func webResult(listening, code bool) daemonproto.WebResult {
	w := daemonproto.WebResult{WebState: daemonproto.WebState{Listening: listening, Addr: "127.0.0.1:7171"}, URL: "http://127.0.0.1:7171/"}
	if listening {
		if code {
			w.CodeURL = w.URL + "#code=" + testCode
		}
	} else {
		w.Error = "port 7171 is taken"
	}
	return w
}

// status is the status the fake daemon answers; set it with setStatus.
func (r *rig) setStatus(st daemonproto.StatusResult) {
	if st.Envs == nil {
		st.Envs = []daemonproto.EnvStatus{}
	}
	if st.Attention == nil {
		st.Attention = []daemonproto.AttentionItem{}
	}
	r.d.handle(daemonproto.MethodStatus, func(json.RawMessage) (any, error) { return st, nil })
}

func (r *rig) setWeb(listening bool) {
	r.d.handle(daemonproto.MethodWeb, func(b json.RawMessage) (any, error) {
		var p daemonproto.WebParams
		if err := json.Unmarshal(b, &p); err != nil || p.Rotate {
			return nil, errors.New("web: bad params")
		}
		return webResult(listening, p.Code), nil
	})
}

const marker = "SANDBOXMARK"

func askOf() *daemonproto.Ask {
	return &daemonproto.Ask{Context: marker + " context", Options: []string{marker + " a", "b"},
		Recommendation: marker + " recommends", Meanwhile: marker + " meanwhile"}
}

// requestsScript serves requests and request for env from views and
// their offers.
type requestsScript struct {
	mu     sync.Mutex
	views  map[string][]daemonproto.RequestView
	offers map[uint64][]daemonproto.Offer
	gone   map[uint64]bool
}

func (s *requestsScript) install(d *fakeDaemon) {
	d.handle(daemonproto.MethodRequests, func(p json.RawMessage) (any, error) {
		var rp daemonproto.RequestsParams
		_ = json.Unmarshal(p, &rp)
		s.mu.Lock()
		defer s.mu.Unlock()
		vs := s.views[rp.Env]
		if vs == nil {
			vs = []daemonproto.RequestView{}
		}
		if rp.Limit > 0 && len(vs) > rp.Limit {
			vs = vs[:rp.Limit]
		}
		return daemonproto.RequestsResult{Requests: vs}, nil
	})
	d.handle(daemonproto.MethodRequest, func(p json.RawMessage) (any, error) {
		var rp daemonproto.RequestParams
		_ = json.Unmarshal(p, &rp)
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, v := range s.views[rp.Env] {
			if v.ID == rp.ID {
				if s.gone[v.ID] {
					v.State = "allowed"
					v.DecidedBy = "cli"
					return daemonproto.RequestResult{Request: v}, nil
				}
				return daemonproto.RequestResult{Request: v, Offers: s.offers[v.ID]}, nil
			}
		}
		return nil, daemonproto.NewError(daemonproto.KindNotFound, "no request #%d", rp.ID)
	})
}

func view(id uint64, kind, key, effect string, created time.Time) daemonproto.RequestView {
	return daemonproto.RequestView{ID: id, Kind: kind, Key: key, Effect: effect, State: "pending", Created: created,
		Expires: created.Add(time.Hour), Session: marker + " session", Ask: askOf()}
}

func allOffers() []daemonproto.Offer {
	return []daemonproto.Offer{
		{Scope: daemonproto.ScopeOnce, Effect: "allow it this one time"},
		{Scope: daemonproto.ScopeHours, Effect: "allow it for {hours} hours", HoursMin: 4, HoursMax: 24},
		{Scope: daemonproto.ScopeAlways, Effect: "add it to config.toml", Diff: []string{"+x"}, Hash: strings.Repeat("a", 64)},
	}
}

// scripted is the golden menu's status: three environments (one
// running under vm, one stopped, one another daemon's), ten attention
// items, 25 requests pending, proposals.
func scripted(r *rig) *requestsScript {
	var att []daemonproto.AttentionItem
	att = append(att,
		daemonproto.AttentionItem{Env: "dev", Kind: daemonproto.AttentionRequests, Text: "25 pending requests", Route: "requests", Since: t0},
		daemonproto.AttentionItem{Env: "dev", Kind: daemonproto.AttentionProposals, Text: "2 pending proposals", Route: "proposals", Since: t0},
		daemonproto.AttentionItem{Env: "ops", Kind: daemonproto.AttentionProposals, Text: "1 pending proposal", Route: "proposals", Since: t0},
		daemonproto.AttentionItem{Env: "ops", Kind: daemonproto.AttentionLinkFailed, Text: "the link failed: exec failed", Route: "env/ops", Since: t0},
	)
	for i := range 6 {
		att = append(att, daemonproto.AttentionItem{Env: "dev", Kind: daemonproto.AttentionCheckpoint, Text: fmt.Sprintf("checkpoint note %d", i), Since: t0})
	}
	r.setStatus(daemonproto.StatusResult{
		Daemon: daemonproto.DaemonInfo{PID: 42, Version: "v1.0.0", Protocol: 1, Started: t0, Dir: "/Users/someone/.caboose/daemon",
			Web: &daemonproto.WebState{Listening: true, Addr: "127.0.0.1:7171"}, App: &daemonproto.AppInfo{Attached: true}},
		Envs: []daemonproto.EnvStatus{
			{Name: "dev", Sandbox: "running", Link: "running", Pending: 25},
			{Name: "ops", Sandbox: "stopped", Link: "failed", LastError: "exec: caboose-agent: not found"},
			{Name: "side", Sandbox: "running", Link: "running", Owner: daemonproto.OwnerDev},
		},
		Attention: att,
	})
	r.d.handle(daemonproto.MethodEnvDetails, func(p json.RawMessage) (any, error) {
		return daemonproto.EnvDetailsResult{Env: "dev", Isolation: "vm", Started: t0.Add(-3 * time.Hour),
			Sessions:  []string{"main", "side work"},
			Resources: &daemonproto.ResourcesDetail{MemoryBytes: 3328599654, MemoryLimitBytes: 8 << 30, Vnodes: 41200, VnodesMax: 263168},
			Ports:     []daemonproto.PortDetail{{Port: 3000, URL: "http://localhost:3000/"}, {Port: 5173, URL: "http://localhost:5173/"}},
			Keep:      []daemonproto.KeepDetail{}}, nil
	})
	s := &requestsScript{views: map[string][]daemonproto.RequestView{}, offers: map[uint64][]daemonproto.Offer{}, gone: map[uint64]bool{}}
	for i := 1; i <= 25; i++ {
		id := uint64(i)
		v := view(id, "egress", fmt.Sprintf("egress:host%d.example.com", i), "connect to host"+fmt.Sprint(i), t0.Add(time.Duration(i)*time.Minute))
		s.views["dev"] = append([]daemonproto.RequestView{v}, s.views["dev"]...)
		s.offers[id] = []daemonproto.Offer{{Scope: daemonproto.ScopeOnce, Effect: "allow once"}}
	}
	// The newest has every offer, and is a URL with a long path.
	s.views["dev"][0] = view(25, "open_url", "open_url:https://example.com",
		"open https://example.com/docs/"+strings.Repeat("x", 200)+"/end in the browser", t0.Add(25*time.Minute))
	s.offers[25] = allOffers()
	s.install(r.d)
	r.setWeb(true)
	return s
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s differs (go test -run %s -update):\n--- got\n%s--- want\n%s", name, t.Name(), got, want)
	}
}

func TestMenuGolden(t *testing.T) {
	r := newRig(t, nil)
	scripted(r)
	r.start()
	r.advance(PollEvery) // the details of the first poll arrive with the status's
	golden(t, "menu.golden", Dump(r.ui.lastMenu()))
	if ic := r.ui.lastIcon(); ic.Title != " 10" || ic.Image != ImageRunning {
		t.Errorf("icon %+v", ic)
	}
}

func TestMenuStopped(t *testing.T) {
	r := newRig(t, func(o *Options) { o.Dev = true })
	r.setStatus(daemonproto.StatusResult{Daemon: daemonproto.DaemonInfo{Dev: true},
		Envs: []daemonproto.EnvStatus{{Name: "dev", Sandbox: "stopped", Link: "stopped",
			Job: &daemonproto.JobInfo{Kind: "restart", State: "failed", Since: t0, Finished: t0, Error: "the build failed"}}}})
	r.start()
	golden(t, "menu-stopped.golden", Dump(r.ui.lastMenu()))
}

// An apply job is said as one, without the proposal's name, which is the
// sandbox's; while it runs, stop and restart are not offered.
func TestMenuApplyJob(t *testing.T) {
	for _, c := range []struct{ state, err, want string }{
		{"running", "", "applying a proposal…"},
		{"done", "", "applied a proposal: its image is used from the next restart"},
		{"failed", "the build failed", "applying a proposal failed: the build failed"},
	} {
		r := newRig(t, nil)
		j := &daemonproto.JobInfo{Kind: daemonproto.JobApply, State: c.state, Since: t0, Proposal: "evil-name.toml", Error: c.err}
		if c.state != "running" {
			j.Finished = t0
		}
		r.setStatus(daemonproto.StatusResult{Envs: []daemonproto.EnvStatus{{Name: "dev", Sandbox: "running", Link: "running", Job: j}}})
		r.start()
		m := r.ui.lastMenu()
		if findTitle(m, c.want) == nil {
			t.Errorf("%s: no %q in\n%s", c.state, c.want, Dump(m))
		}
		if strings.Contains(Dump(m), "evil-name") {
			t.Errorf("%s: the proposal's name in the menu:\n%s", c.state, Dump(m))
		}
		if running := findTitle(m, "Restart…") == nil; running != (c.state == "running") {
			t.Errorf("%s: restart offered %v", c.state, !running)
		}
	}
}

func TestMenuDisconnected(t *testing.T) {
	r := newRig(t, nil)
	r.d.down = true
	r.d.startErr = errors.New("the daemon did not answer within 10s of being started")
	r.start()
	r.advance(DisconnectedAfter)
	golden(t, "menu-disconnected.golden", Dump(r.ui.lastMenu()))
}

func TestReconnect(t *testing.T) {
	r := newRig(t, nil)
	r.d.down = true
	r.d.startErr = errors.New("cannot start")
	r.start()
	if ic := r.ui.lastIcon(); ic.Title == " !" {
		t.Error("disconnected at once")
	}
	r.advance(9 * time.Second)
	if ic := r.ui.lastIcon(); ic.Title == " !" {
		t.Error("disconnected before 10 s")
	}
	r.advance(time.Second)
	if ic := r.ui.lastIcon(); ic.Title != " !" || ic.Description != "caboose: the daemon is not answering" {
		t.Errorf("not disconnected at 10 s: %+v", ic)
	}
	r.advance(100 * time.Second)
	// At once, then 5, 10, 20, 30, 30 s apart.
	if got, want := secs(r.d.ensures), "0,5,15,35,65,95"; got != want {
		t.Errorf("Ensure at %s, want %s", got, want)
	}
	r.d.mu.Lock()
	r.d.startErr = nil
	r.d.mu.Unlock()
	r.advance(30 * time.Second)
	if len(r.d.callsOf(daemonproto.MethodAppAttach)) != 1 {
		t.Fatal("not attached after the daemon came back")
	}
	if ic := r.ui.lastIcon(); ic.Title == " !" {
		t.Errorf("still disconnected: %+v", ic)
	}
	// Lost: the first attempt is 5 s later, through Ensure; the backoff
	// starts again.
	r.d.kill()
	r.d.mu.Lock()
	r.d.startErr = errors.New("cannot start")
	r.d.mu.Unlock()
	ensures := len(r.d.ensures)
	r.advance(PollEvery) // the next poll finds it gone
	st := r.d.callsOf(daemonproto.MethodStatus)
	lossAt := st[len(st)-1].at
	r.advance(40 * time.Second)
	var after []time.Duration
	for _, e := range r.d.ensures[ensures:] {
		after = append(after, e-lossAt)
	}
	if got, want := secs(after), "5,15,35"; got != want {
		t.Errorf("Ensure after the loss at %s, want %s", got, want)
	}
}

func TestAppOlder(t *testing.T) {
	r := newRig(t, nil)
	r.d.dialErr = daemonproto.NewError(daemonproto.KindProtocolMismatch, "the running daemon speaks protocol 2")
	r.start()
	if ic := r.ui.lastIcon(); ic.Title != " !" {
		t.Errorf("a refusal is not shown at once: %+v", ic)
	}
	if findTitle(r.ui.lastMenu(), TextAppOlder) == nil {
		t.Errorf("no update line:\n%s", Dump(r.ui.lastMenu()))
	}
	r.advance(59 * time.Second)
	if len(r.d.connects) != 1 {
		t.Errorf("retried within 60 s: %s", secs(r.d.connects))
	}
	r.advance(time.Second)
	if got := secs(r.d.connects); got != "0,60" {
		t.Errorf("connects at %s", got)
	}
	if len(r.d.ensures) != 0 {
		t.Error("an older app started a daemon")
	}
}

func TestAppNewer(t *testing.T) {
	r := newRig(t, nil)
	r.d.frame.Protocol = 0
	r.d.compat = daemonproto.CompatClientNewer
	r.d.handle(daemonproto.MethodReplace, func(json.RawMessage) (any, error) {
		return daemonproto.ReplaceResult{}, nil
	})
	r.start()
	r.advance(2 * time.Minute)
	reps := r.d.callsOf(daemonproto.MethodReplace)
	if len(reps) != 1 {
		t.Fatalf("%d replaces", len(reps))
	}
	var p daemonproto.ReplaceParams
	if err := json.Unmarshal([]byte(reps[0].params), &p); err != nil || p.Force || p.Version != "v1.0.0" {
		t.Errorf("replace %s", reps[0].params)
	}
	if strings.Contains(reps[0].params, `"force":true`) {
		t.Error("forced")
	}
	if findTitle(r.ui.lastMenu(), TextWaitUpdate) == nil {
		t.Errorf("no waiting line:\n%s", Dump(r.ui.lastMenu()))
	}
	if len(r.d.callsOf(daemonproto.MethodAppAttach)) != 0 {
		t.Error("attached to an older daemon")
	}
}

func TestNoSpawnAfterNewer(t *testing.T) {
	r := newRig(t, nil)
	r.d.frame.Version = "v2.0.0" // same protocol, a newer release
	r.start()
	if len(r.d.callsOf(daemonproto.MethodAppAttach)) != 1 {
		t.Fatal("did not work with a newer daemon of the same protocol")
	}
	r.d.kill()
	r.advance(PollEvery)
	r.advance(time.Minute)
	if len(r.d.ensures) != 0 {
		t.Errorf("started a daemon after seeing a newer one: %s", secs(r.d.ensures))
	}
	if n := len(r.d.connects); n < 20 {
		t.Errorf("only %d connects: it should watch every 2 s", n)
	}
	if findTitle(r.ui.lastMenu(), TextWaitNewer) == nil {
		t.Errorf("no waiting line:\n%s", Dump(r.ui.lastMenu()))
	}
}

func TestNoSpawnWhenReplacedNewer(t *testing.T) {
	r := newRig(t, nil)
	r.d.down = true
	r.d.replaced = &daemon.Replaced{Version: "v1.5.0", Protocol: 1, At: t0}
	r.start()
	r.advance(10 * time.Second)
	if len(r.d.ensures) != 0 {
		t.Errorf("started a daemon though replaced.json names a newer one")
	}
	if got := secs(r.d.connects); got != "0,2,4,6,8,10" {
		t.Errorf("connects at %s", got)
	}
	r.d.mu.Lock()
	r.d.replaced = &daemon.Replaced{Version: "v1.0.0", Protocol: 1, At: t0}
	r.d.mu.Unlock()
	r.advance(2 * time.Second)
	if len(r.d.ensures) != 1 {
		t.Errorf("did not start one once replaced.json named its own: %d", len(r.d.ensures))
	}
}

func TestBusy(t *testing.T) {
	r := newRig(t, nil)
	r.d.handle(daemonproto.MethodAppAttach, func(json.RawMessage) (any, error) {
		return nil, daemonproto.NewError(daemonproto.KindBusy, "an app is attached")
	})
	r.start()
	r.advance(QuitWait) // the connection is closed, or given up on
	// The alert is shown, and the app waits for the user to close it:
	// quitting at once would take the alert down with the app.
	s := r.ui.snapshot()
	if len(s.alerts) != 1 || s.alerts[0] != TextBusy || s.alertIDs[0] != BusyAlertID || s.quitted {
		t.Fatalf("alerts %v %v, quit %v", s.alerts, s.alertIDs, s.quitted)
	}
	// Nothing more is drawn or asked while it waits.
	icons, calls, connects := len(s.icons), len(r.d.callsOf(daemonproto.MethodStatus)), r.connectCount()
	r.advance(PollEvery * 3)
	if s := r.ui.snapshot(); len(s.icons) != icons || s.quitted {
		t.Errorf("drew %d icons while quitting, quit %v", len(s.icons)-icons, s.quitted)
	}
	if n, m := len(r.d.callsOf(daemonproto.MethodStatus)), r.connectCount(); n != calls || m != connects {
		t.Errorf("%d status calls and %d connects while quitting", n-calls, m-connects)
	}
	// Another alert's closing is not this one's.
	r.do(func(e Events) { e.AlertClosed("other") })
	if r.ui.snapshot().quitted {
		t.Fatal("quit on another alert's closing")
	}
	r.do(func(e Events) { e.AlertClosed(BusyAlertID) })
	if !r.ui.snapshot().quitted {
		t.Error("did not quit once the alert was closed")
	}
}

func TestBusyAlertWait(t *testing.T) {
	r := newRig(t, nil)
	r.d.handle(daemonproto.MethodAppAttach, func(json.RawMessage) (any, error) {
		return nil, daemonproto.NewError(daemonproto.KindBusy, "an app is attached")
	})
	r.start()
	r.advance(AlertWait - time.Second)
	if r.ui.snapshot().quitted {
		t.Fatal("quit before the alert was closed or AlertWait passed")
	}
	r.advance(time.Second)
	if !r.ui.snapshot().quitted {
		t.Error("an alert left open did not end the app after AlertWait")
	}
}

func TestOlderDaemonLacksAttach(t *testing.T) {
	r := newRig(t, nil)
	r.d.handle(daemonproto.MethodAppAttach, nil)
	delete(r.d.handlers, daemonproto.MethodAppAttach)
	r.start()
	r.advance(PollEvery)
	if findTitle(r.ui.lastMenu(), "this daemon is older than the app: its alerts stay the system's") == nil {
		t.Errorf("no line:\n%s", Dump(r.ui.lastMenu()))
	}
	if len(r.d.callsOf(daemonproto.MethodDialogs)) != 0 || len(r.d.callsOf(daemonproto.MethodNotifications)) != 0 {
		t.Error("polled what the daemon cannot serve")
	}
	if len(r.ui.snapshot().alerts) != 0 {
		t.Error("an alert for an older daemon")
	}
}

// selectAnswer selects the item of id and returns the confirmation the
// UI was asked to show, if any.
func (r *rig) selectItem(id string) {
	r.t.Helper()
	if find(r.ui.lastMenu(), id) == nil {
		r.t.Fatalf("no item %s in\n%s", id, Dump(r.ui.lastMenu()))
	}
	r.do(func(e Events) { e.Selected(id) })
}

func TestAnswerOffers(t *testing.T) {
	r := newRig(t, nil)
	s := scripted(r)
	r.start()
	m := r.ui.lastMenu()
	req := findTitle(m, RequestTitle("dev", s.views["dev"][0]))
	if req == nil {
		t.Fatalf("no request item:\n%s", Dump(m))
	}
	var got []string
	for _, k := range req.Children {
		if !k.Separator {
			got = append(got, k.Title)
		}
	}
	if want := "Allow once|Allow for 4 hours|Allow for 8 hours|Allow for 24 hours|Allow always…|Deny|Details… (web page)"; strings.Join(got, "|") != want {
		t.Errorf("answers %q, want %q", strings.Join(got, "|"), want)
	}
	// Only "once" offered.
	other := findTitle(m, RequestTitle("dev", s.views["dev"][1]))
	got = nil
	for _, k := range other.Children {
		if !k.Separator {
			got = append(got, k.Title)
		}
	}
	if want := "Allow once|Deny|Details… (web page)"; strings.Join(got, "|") != want {
		t.Errorf("answers %q, want %q", strings.Join(got, "|"), want)
	}
	// With the page not served, "always" cannot be chosen.
	r.d.handle(daemonproto.MethodStatus, func(json.RawMessage) (any, error) {
		return daemonproto.StatusResult{Daemon: daemonproto.DaemonInfo{Web: &daemonproto.WebState{Listening: false}},
			Envs: []daemonproto.EnvStatus{{Name: "dev", Sandbox: "running", Link: "running", Pending: 25}}, Attention: []daemonproto.AttentionItem{}}, nil
	})
	r.advance(PollEvery)
	if it := find(r.ui.lastMenu(), "req:dev:25:always"); it == nil || it.Enabled || it.Title != "Allow always… (web UI not running)" {
		t.Errorf("always while the page is down: %+v", it)
	}
}

func TestAnswerConfirmed(t *testing.T) {
	r := newRig(t, nil)
	scripted(r)
	var answers []string
	r.d.handle(daemonproto.MethodAnswer, func(p json.RawMessage) (any, error) {
		answers = append(answers, string(p))
		var ap daemonproto.AnswerParams
		_ = json.Unmarshal(p, &ap)
		return daemonproto.AnswerResult{Request: daemonproto.RequestView{ID: ap.ID, Kind: "open_url", State: "allowed", Created: t0}, Recorded: true}, nil
	})
	r.start()
	before := len(r.d.callsOf(daemonproto.MethodRequest))
	r.selectItem("req:dev:25:hours:8")
	if n := len(r.d.callsOf(daemonproto.MethodRequest)); n != before+1 {
		t.Errorf("the request was not fetched again before the confirmation (%d -> %d)", before, n)
	}
	s := r.ui.snapshot()
	if len(s.confirm) != 1 {
		t.Fatalf("confirmations %+v", s.confirm)
	}
	cf := s.confirm[0]
	if cf.Message != "Allow #25 for 8 hours?" || cf.Action != "Allow for 8 hours" || cf.AnswerEffect != "allow it for 8 hours" ||
		!strings.HasPrefix(cf.Effect, "open https://example.com/docs/") {
		t.Errorf("confirmation %+v", cf)
	}
	if !strings.HasPrefix(cf.SandboxSays, SandboxLabel+"\n") || !strings.Contains(cf.SandboxSays, marker+" context") ||
		strings.Contains(cf.Effect, marker) || strings.Contains(cf.AnswerEffect, marker) || utf8.RuneCountInString(cf.SandboxSays) > MaxSandboxSays {
		t.Errorf("the sandbox's words: %q / effect %q", cf.SandboxSays, cf.Effect)
	}
	if len(answers) != 0 {
		t.Fatal("answered before the confirmation")
	}
	r.do(func(e Events) { e.Confirmed(cf.ID, false) })
	r.do(func(e Events) { e.Confirmed(cf.ID, true) }) // a stale id: nothing
	if len(answers) != 0 {
		t.Fatal("answered on Cancel")
	}
	r.selectItem("req:dev:25:hours:8")
	cf = r.ui.snapshot().confirm[1]
	r.do(func(e Events) { e.Confirmed("c-other", true) })
	if len(answers) != 0 {
		t.Fatal("answered on another alert's confirmation")
	}
	r.do(func(e Events) { e.Confirmed(cf.ID, true) })
	if len(answers) != 1 || answers[0] != `{"env":"dev","id":25,"verdict":"allow","scope":"hours","hours":8}` {
		t.Errorf("answers %v", answers)
	}
	r.selectItem("req:dev:25:deny")
	cf = r.ui.snapshot().confirm[2]
	if cf.Message != "Deny #25?" || cf.Action != "Deny" {
		t.Errorf("deny confirmation %+v", cf)
	}
	r.do(func(e Events) { e.Confirmed(cf.ID, true) })
	if len(answers) != 2 || answers[1] != `{"env":"dev","id":25,"verdict":"deny"}` {
		t.Errorf("answers %v", answers)
	}
	r.selectItem("req:dev:25:once")
	cf = r.ui.snapshot().confirm[3]
	r.do(func(e Events) { e.Confirmed(cf.ID, true) })
	if len(answers) != 3 || answers[2] != `{"env":"dev","id":25,"verdict":"allow","scope":"once"}` {
		t.Errorf("answers %v", answers)
	}
	for _, a := range answers {
		if strings.Contains(a, "decided_by") || strings.Contains(a, "confirm_diff") {
			t.Errorf("an answer says who decided: %s", a)
		}
	}
}

func TestAnswerNoLongerOffered(t *testing.T) {
	r := newRig(t, nil)
	s := scripted(r)
	var answered int
	r.d.handle(daemonproto.MethodAnswer, func(json.RawMessage) (any, error) { answered++; return nil, errors.New("no") })
	r.start()
	s.mu.Lock()
	s.offers[25] = []daemonproto.Offer{{Scope: daemonproto.ScopeHours, Effect: "for {hours}", HoursMin: 1, HoursMax: 4}}
	s.mu.Unlock()
	r.selectItem("req:dev:25:hours:8") // outside the new bounds
	r.selectItem("req:dev:25:once")    // no longer offered
	if n := len(r.ui.snapshot().confirm); n != 0 {
		t.Errorf("%d confirmations for answers no longer offered", n)
	}
	if findTitle(r.ui.lastMenu(), "#25: that answer is no longer offered") == nil {
		t.Errorf("no line:\n%s", Dump(r.ui.lastMenu()))
	}
	// Answered elsewhere meanwhile: no confirmation either.
	s.mu.Lock()
	s.gone[25] = true
	s.mu.Unlock()
	r.selectItem("req:dev:25:deny")
	if n := len(r.ui.snapshot().confirm); n != 0 || answered != 0 {
		t.Errorf("%d confirmations, %d answers for a decided request", n, answered)
	}
	// The line goes after a minute.
	r.advance(InfoFor)
	if findTitle(r.ui.lastMenu(), "#25: that answer is no longer offered") != nil {
		t.Error("the line stayed")
	}
}

func TestAlreadyAnswered(t *testing.T) {
	r := newRig(t, nil)
	s := scripted(r)
	r.d.handle(daemonproto.MethodAnswer, func(json.RawMessage) (any, error) {
		s.mu.Lock()
		s.gone[25] = true
		s.mu.Unlock()
		return nil, daemonproto.NewError(daemonproto.KindAlreadyAnswered, "request #25 was decided already")
	})
	r.start()
	r.selectItem("req:dev:25:once")
	cf := r.ui.snapshot().confirm[0]
	r.do(func(e Events) { e.Confirmed(cf.ID, true) })
	if findTitle(r.ui.lastMenu(), "#25 was already answered by cli") == nil {
		t.Errorf("no line:\n%s", Dump(r.ui.lastMenu()))
	}
	if n := len(r.d.callsOf(daemonproto.MethodAnswer)); n != 1 {
		t.Errorf("%d answers: retried", n)
	}
	if len(r.ui.snapshot().notes) != 0 {
		t.Error("a notification for it")
	}
}

func TestAlwaysOpensThePage(t *testing.T) {
	r := newRig(t, nil)
	scripted(r)
	r.start()
	r.selectItem("req:dev:25:always")
	s := r.ui.snapshot()
	if len(s.opened) != 1 || s.opened[0] != "http://127.0.0.1:7171/#code="+testCode+"&to=requests/dev" {
		t.Errorf("opened %v", s.opened)
	}
	if len(r.d.callsOf(daemonproto.MethodAnswer)) != 0 || len(s.confirm) != 0 {
		t.Error("always answered from the app")
	}
}

func TestReflowHeld(t *testing.T) {
	r := newRig(t, nil)
	r.setStatus(daemonproto.StatusResult{Envs: []daemonproto.EnvStatus{{Name: "dev", Sandbox: "stopped", Link: "stopped"}}})
	r.start()
	r.do(func(e Events) { e.MenuOpened() })
	menus := len(r.ui.snapshot().menus)
	r.setStatus(daemonproto.StatusResult{Envs: []daemonproto.EnvStatus{{Name: "alpha", Sandbox: "running", Link: "running"}, {Name: "dev", Sandbox: "stopped", Link: "stopped"}}})
	r.advance(10 * PollEvery)
	if n := len(r.ui.snapshot().menus); n != menus {
		t.Fatalf("the menu changed while open (%d -> %d)", menus, n)
	}
	// The icon may follow meanwhile.
	if ic := r.ui.lastIcon(); ic.Description != "caboose: 1 running" {
		t.Errorf("icon %+v", ic)
	}
	r.do(func(e Events) { e.MenuClosed() })
	if n := len(r.ui.snapshot().menus); n != menus+1 || findTitle(r.ui.lastMenu(), "alpha: running") == nil {
		t.Errorf("the held menu was not applied at close:\n%s", Dump(r.ui.lastMenu()))
	}
}

func TestHostileText(t *testing.T) {
	r := newRig(t, nil)
	r.d.noValidate = true
	long := strings.Repeat("y", 500)
	views := []daemonproto.RequestView{
		view(1, "alias", "alias:deploy", "run alias deploy\nand more "+long, t0),
		view(2, "open_url", "open_url:https://example.com", "open https://example.com/\u202eevil/"+long+" in the browser", t0),
		view(3, "egress", "egress:"+long, "connect", t0),
		view(4, "weird\u202ekind", "x", "y\nz", t0),
	}
	s := &requestsScript{views: map[string][]daemonproto.RequestView{"dev": views}, offers: map[uint64][]daemonproto.Offer{}}
	for _, v := range views {
		s.offers[v.ID] = allOffers()
	}
	s.install(r.d)
	r.setWeb(true)
	r.setStatus(daemonproto.StatusResult{
		Envs: []daemonproto.EnvStatus{{Name: "dev", Sandbox: "running", Link: "failed", Pending: 4, LastError: marker + "\nlink error \u202e"},
			{Name: "ops", Sandbox: "running", Link: "running", Job: &daemonproto.JobInfo{Kind: "stop", State: "failed", Since: t0, Error: marker + " job"}}},
		Attention: []daemonproto.AttentionItem{{Env: "dev", Kind: daemonproto.AttentionLinkFailed, Text: "the link failed: " + marker, Since: t0}},
	})
	seq := uint64(0)
	r.d.handle(daemonproto.MethodNotifications, func(json.RawMessage) (any, error) {
		seq++
		return daemonproto.NotificationsResult{Items: []daemonproto.Notification{
			{Seq: seq, Env: "dev", Origin: daemonproto.OriginSandbox, Title: daemonproto.SandboxTitle("dev"), Text: "Approve request?: " + marker, At: t0},
		}, Next: seq}, nil
	})
	r.d.handle(daemonproto.MethodDialogs, func(json.RawMessage) (any, error) { return daemonproto.DialogsResult{}, nil })
	r.start()
	r.do(func(e Events) { e.NotificationAuth(true) })
	r.advance(3 * PollEvery)
	snap := r.ui.snapshot()
	if len(snap.notes) == 0 {
		t.Fatal("no notification")
	}
	// The sandbox's own words (its ask and session) never reach a menu
	// title; the link's error and a job's only info lines.
	for _, m := range snap.menus {
		var walk func([]Item, bool)
		walk = func(items []Item, sub bool) {
			for _, it := range items {
				if strings.Contains(it.Title, marker) && (!it.Info || sub) {
					t.Errorf("sandbox text in a %v item: %q", it, it.Title)
				}
				if utf8.RuneCountInString(it.Title) > MaxTitle || strings.ContainsAny(it.Title, "\n\r\u202e") {
					t.Errorf("not one clean line of at most %d: %q", MaxTitle, it.Title)
				}
				walk(it.Children, true)
			}
		}
		walk(m.Items, false)
	}
	for _, ic := range snap.icons {
		if strings.Contains(ic.Title+ic.Description, marker) {
			t.Errorf("sandbox text in the icon: %+v", ic)
		}
	}
	for _, n := range snap.notes {
		if strings.Contains(n.Title, marker) || (n.ID != SummaryID && n.Title != "From the dev sandbox") {
			t.Errorf("notification title %q", n.Title)
		}
	}
	m := r.ui.lastMenu()
	for _, v := range views {
		if it := findTitle(m, RequestTitle("dev", v)); it == nil {
			t.Errorf("no item for #%d", v.ID)
		}
	}
	if it := findTitle(m, RequestTitle("dev", views[1])); it != nil && !strings.HasPrefix(it.Title, "#2 dev open: https://example.com/") {
		t.Errorf("the host did not survive: %q", it.Title)
	}
	if it := findTitle(m, RequestTitle("dev", views[0])); it != nil && it.Title != "#1 dev alias: deploy" {
		t.Errorf("an alias title from the effect: %q", it.Title)
	}
	// In a confirmation, the sandbox's words come after the label only.
	r.selectItem("req:dev:2:once")
	cf := r.ui.snapshot().confirm[0]
	i := strings.Index(cf.SandboxSays, marker)
	if j := strings.Index(cf.SandboxSays, SandboxLabel); j != 0 || i < len(SandboxLabel) || strings.Contains(cf.Message+cf.Action+cf.AnswerEffect+cf.Effect, marker) {
		t.Errorf("confirmation %+v", cf)
	}
	if strings.ContainsRune(cf.Effect, '\u202e') {
		t.Errorf("a bidi override in an alert: %q", cf.Effect)
	}
}

func TestURLDialog(t *testing.T) {
	r := newRig(t, nil)
	views := []daemonproto.RequestView{
		view(7, "open_url", "open_url:https://example.com", "open https://example.com/page in the browser", t0),
		view(8, "open_url", "open_url:https://example.org", "open https://example.org/x in the browser", t0),
	}
	s := &requestsScript{views: map[string][]daemonproto.RequestView{"dev": views}, offers: map[uint64][]daemonproto.Offer{}}
	s.install(r.d)
	var mu sync.Mutex
	next := &daemonproto.DialogRef{Env: "dev", ID: 7}
	setNext := func(ref *daemonproto.DialogRef) { mu.Lock(); next = ref; mu.Unlock() }
	r.d.handle(daemonproto.MethodDialogs, func(json.RawMessage) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		return daemonproto.DialogsResult{Dialog: next}, nil
	})
	var answers []string
	r.d.handle(daemonproto.MethodAnswer, func(p json.RawMessage) (any, error) {
		answers = append(answers, string(p))
		return daemonproto.AnswerResult{Request: daemonproto.RequestView{ID: 7, Kind: "open_url", State: "allowed", Created: t0}, Recorded: true}, nil
	})
	r.start()
	if p := r.d.callsOf(daemonproto.MethodAppAttach)[0].params; p != `{"capabilities":["dialogs"],"version":"v1.0.0"}` {
		t.Errorf("attached with %s", p)
	}
	s1 := r.ui.snapshot()
	if len(s1.asks) != 1 || s1.asks[0].URL != "https://example.com/page" || s1.asks[0].Env != "dev" || !strings.Contains(s1.asks[0].SandboxSays, marker) {
		t.Fatalf("asks %+v", s1.asks)
	}
	r.advance(3 * PollEvery)
	if n := len(r.ui.snapshot().asks); n != 1 {
		t.Errorf("asked %d times for one request", n)
	}
	r.do(func(e Events) { e.URLAnswered(s1.asks[0].ID, true) })
	if len(answers) != 1 || answers[0] != `{"env":"dev","id":7,"verdict":"allow","scope":"once"}` {
		t.Errorf("answers %v", answers)
	}
	// The daemon still hands it out until the answer lands: not asked
	// again.
	r.advance(PollEvery)
	if n := len(r.ui.snapshot().asks); n != 1 {
		t.Errorf("asked again: %d", n)
	}
	setNext(&daemonproto.DialogRef{Env: "dev", ID: 8})
	r.advance(PollEvery)
	s2 := r.ui.snapshot()
	if len(s2.asks) != 2 || s2.asks[1].URL != "https://example.org/x" {
		t.Fatalf("asks %+v", s2.asks)
	}
	// Denied: no scope.
	r.do(func(e Events) { e.URLAnswered(s2.asks[1].ID, false) })
	if len(answers) != 2 || answers[1] != `{"env":"dev","id":8,"verdict":"deny"}` {
		t.Errorf("answers %v", answers)
	}
	// Another goes away while shown: dismissed.
	setNext(&daemonproto.DialogRef{Env: "dev", ID: 7})
	r.advance(PollEvery)
	s3 := r.ui.snapshot()
	if len(s3.asks) != 3 {
		t.Fatalf("asks %+v", s3.asks)
	}
	setNext(nil)
	r.advance(PollEvery)
	if d := r.ui.snapshot().dismiss; len(d) != 1 || d[0] != s3.asks[2].ID {
		t.Errorf("dismissed %v", d)
	}
	r.do(func(e Events) { e.URLAnswered(s3.asks[2].ID, true) })
	if len(answers) != 2 {
		t.Error("answered a dismissed dialog")
	}
	// Losing the daemon dismisses the open one.
	setNext(&daemonproto.DialogRef{Env: "dev", ID: 8})
	r.advance(PollEvery)
	s4 := r.ui.snapshot()
	r.d.kill()
	r.advance(PollEvery)
	if d := r.ui.snapshot().dismiss; len(d) != 2 || d[1] != s4.asks[len(s4.asks)-1].ID {
		t.Errorf("dismissed %v at the loss", d)
	}
}

func TestNotifications(t *testing.T) {
	r := newRig(t, nil)
	r.setWeb(true)
	var mu sync.Mutex
	var queue []daemonproto.Notification
	seq := uint64(0)
	push := func(n daemonproto.Notification) {
		mu.Lock()
		seq++
		n.Seq, n.At = seq, t0
		queue = append(queue, n)
		mu.Unlock()
	}
	r.d.handle(daemonproto.MethodNotifications, func(p json.RawMessage) (any, error) {
		var np daemonproto.NotificationsParams
		_ = json.Unmarshal(p, &np)
		mu.Lock()
		defer mu.Unlock()
		res := daemonproto.NotificationsResult{Next: seq, Items: []daemonproto.Notification{}}
		for _, n := range queue {
			if n.Seq > np.After {
				res.Items = append(res.Items, n)
			}
		}
		return res, nil
	})
	r.start()
	if len(r.d.callsOf(daemonproto.MethodNotifications)) != 0 {
		t.Error("polled notifications without the capability")
	}
	r.do(func(e Events) { e.NotificationAuth(true) })
	at := r.d.callsOf(daemonproto.MethodAppAttach)
	if len(at) != 2 || at[1].params != `{"capabilities":["dialogs","notifications"],"version":"v1.0.0"}` {
		t.Fatalf("re-attach %v", at)
	}
	push(daemonproto.Notification{Env: "dev", Origin: daemonproto.OriginHost, Title: "caboose: dev", Text: "dev restarted", Route: "env/dev"})
	push(daemonproto.Notification{Env: "dev", Origin: daemonproto.OriginSandbox, Title: daemonproto.SandboxTitle("dev"), Text: "Approve request?: click"})
	push(daemonproto.Notification{Origin: daemonproto.OriginHost, Title: "caboose", Text: "restored a hook"})
	push(daemonproto.Notification{Env: "dev", Origin: daemonproto.OriginHost, Title: "caboose: dev checkpoints", Text: "low space"})
	push(daemonproto.Notification{Env: "dev", Origin: daemonproto.OriginHost, Title: "caboose: dev", Text: "fifth"})
	r.advance(PollEvery)
	notes := r.ui.snapshot().notes
	want := []Notification{
		{ID: "n1", Title: "caboose: dev", Body: "dev restarted"},
		{ID: "n2", Title: "From the dev sandbox", Body: "Approve request?: click"},
		{ID: "n3", Title: "caboose", Body: "restored a hook"},
		{ID: SummaryID, Title: "caboose", Body: "1 thing needs you"},
		{ID: SummaryID, Title: "caboose", Body: "2 things need you"},
	}
	if fmt.Sprint(notes) != fmt.Sprint(want) {
		t.Errorf("notes\n%v\nwant\n%v", notes, want)
	}
	// Clicks open their routes; a sandbox's its environment's page.
	r.do(func(e Events) { e.NotificationClicked("n1") })
	r.do(func(e Events) { e.NotificationClicked("n2") })
	r.do(func(e Events) { e.NotificationClicked(SummaryID) })
	r.do(func(e Events) { e.NotificationClicked("n999") })
	base := "http://127.0.0.1:7171/#code=" + testCode
	if got := fmt.Sprint(r.ui.snapshot().opened); got != fmt.Sprint([]string{base + "&to=env/dev", base + "&to=env/dev", base}) {
		t.Errorf("opened %s", got)
	}
	// After the window, one on its own again.
	r.advance(ThrottleWindow)
	push(daemonproto.Notification{Env: "dev", Origin: daemonproto.OriginHost, Title: "caboose: dev", Text: "later"})
	r.advance(PollEvery)
	if n := r.ui.snapshot().notes; n[len(n)-1].ID != "n6" {
		t.Errorf("after the window: %v", n[len(n)-1])
	}
	// With the page down, a click opens nothing.
	r.setWeb(false)
	r.do(func(e Events) { e.NotificationClicked("n6") })
	if n := len(r.ui.snapshot().opened); n != 3 {
		t.Errorf("opened with the page down")
	}
	if findTitle(r.ui.lastMenu(), "web UI not running: port 7171 is taken") == nil {
		t.Errorf("no line:\n%s", Dump(r.ui.lastMenu()))
	}
}

func TestSandboxNotificationTitle(t *testing.T) {
	for _, n := range []daemonproto.Notification{
		{Seq: 1, Env: "dev", Origin: daemonproto.OriginSandbox, Title: "caboose: dev", Text: "x", Route: "requests"},
		{Seq: 1, Env: "dev", Origin: daemonproto.OriginSandbox, Title: "Approve request?", Text: "x"},
	} {
		title, _, route, ok := present(n)
		if !ok || title != "From the dev sandbox" || route != "env/dev" {
			t.Errorf("%+v: %q %q %v", n, title, route, ok)
		}
	}
	if title, _, _, _ := present(daemonproto.Notification{Seq: 1, Env: "dev", Origin: daemonproto.OriginHost, Title: "Approve?"}); title != "caboose" {
		t.Errorf("a host title outside the table: %q", title)
	}
	if _, _, _, ok := present(daemonproto.Notification{Seq: 1, Origin: "other", Title: "x"}); ok {
		t.Error("an unknown origin shown")
	}
}

func TestWebNeverLogsCode(t *testing.T) {
	r := newRig(t, nil)
	scripted(r)
	r.ui.openErr = errors.New("cannot open http://127.0.0.1:7171/#code=" + testCode)
	r.start()
	r.selectItem("web")
	r.selectItem("proposals")
	r.selectItem("env:dev")
	r.selectItem("requests-more")
	r.selectItem("attention-more")
	r.selectItem("req:dev:25:details")
	s := r.ui.snapshot()
	if len(s.opened) != 6 {
		t.Fatalf("opened %d", len(s.opened))
	}
	wants := []string{"", "&to=proposals", "&to=env/dev", "&to=requests", "", "&to=requests/dev"}
	for i, w := range wants {
		if s.opened[i] != "http://127.0.0.1:7171/#code="+testCode+w {
			t.Errorf("opened %d: %s", i, s.opened[i])
		}
	}
	if strings.Contains(r.logText(), testCode) {
		t.Error("the code is in the log")
	}
	for _, m := range s.menus {
		for _, ti := range titles(m) {
			if strings.Contains(ti, testCode) {
				t.Errorf("the code in a title: %q", ti)
			}
		}
	}
	// A route outside the page's is never opened.
	r.core.post(func() { r.core.openRoute("env/../../x") })
	r.core.waitIdle()
	if len(r.ui.snapshot().opened) != 6 {
		t.Error("opened a bad route")
	}
}

func TestPortsAndLog(t *testing.T) {
	r := newRig(t, nil)
	scripted(r)
	r.start()
	r.advance(PollEvery)
	r.selectItem("port:dev:5173")
	r.selectItem("log")
	s := r.ui.snapshot()
	if fmt.Sprint(s.opened) != "[http://localhost:5173/ file:///Users/someone/.caboose/daemon/daemon.log]" {
		t.Errorf("opened %v", s.opened)
	}
}

func TestStopRestart(t *testing.T) {
	r := newRig(t, nil)
	scripted(r)
	var jobs []string
	for _, m := range []string{daemonproto.MethodSandboxStop, daemonproto.MethodSandboxRestart} {
		r.d.handle(m, func(p json.RawMessage) (any, error) {
			jobs = append(jobs, m+" "+string(p))
			if len(jobs) > 1 {
				return nil, daemonproto.NewError(daemonproto.KindBusy, "a job runs")
			}
			return daemonproto.JobResult{Job: daemonproto.JobInfo{Kind: "stop", State: "running", Since: t0}}, nil
		})
	}
	r.start()
	r.advance(PollEvery)
	r.selectItem("stop:dev")
	cf := r.ui.snapshot().confirm[0]
	if cf.Message != "Stop dev?" || cf.Action != "Stop dev" ||
		cf.Effect != "This ends every session in dev, as `caboose -e dev stop` does." ||
		cf.SandboxSays != SandboxLabel+"\nSessions:\n  main\n  side work" {
		t.Errorf("confirmation %+v", cf)
	}
	r.do(func(e Events) { e.Confirmed(cf.ID, false) })
	if len(jobs) != 0 {
		t.Fatal("stopped on Cancel")
	}
	r.selectItem("stop:dev")
	cf = r.ui.snapshot().confirm[1]
	r.do(func(e Events) { e.Confirmed(cf.ID, true) })
	if len(jobs) != 1 || jobs[0] != `sandbox_stop {"env":"dev"}` {
		t.Errorf("jobs %v", jobs)
	}
	r.selectItem("restart:dev")
	cf = r.ui.snapshot().confirm[2]
	if !strings.Contains(cf.Effect, "rebuilds the image first when it is out of date") || cf.Action != "Restart dev" {
		t.Errorf("restart confirmation %+v", cf)
	}
	r.do(func(e Events) { e.Confirmed(cf.ID, true) })
	if findTitle(r.ui.lastMenu(), "a job already runs for dev") == nil {
		t.Errorf("no busy line:\n%s", Dump(r.ui.lastMenu()))
	}
}

func TestLoginItem(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	if it := loginOf(r.ui.lastMenu()); it == nil || it.Checked {
		t.Fatalf("login item %+v", it)
	}
	r.selectLogin()
	if it := loginOf(r.ui.lastMenu()); it == nil || !it.Checked {
		t.Errorf("login item after on %+v", it)
	}
	r.selectLogin()
	r.ui.mu.Lock()
	r.ui.onState = LoginNeedsApproval
	r.ui.mu.Unlock()
	r.selectLogin() // sets on, which needs approval
	if it := loginOf(r.ui.lastMenu()); it == nil || it.Title != "Open at login (needs approval in System Settings)" {
		t.Errorf("login item needing approval %+v", it)
	}
	r.selectLogin()
	if got := fmt.Sprint(r.ui.setLog); got != "[true false true]" {
		t.Errorf("set %s", got)
	}
	if !strings.Contains(strings.Join(r.ui.snapshot().calls, ","), "OpenLoginSettings") {
		t.Error("needs approval did not open the settings")
	}
}

// TestLoginItemRefused: a LoginCheck that refuses keeps the login item
// off, and needing approval unapproved, and the menu says why; turning it
// off is never asked.
func TestLoginItemRefused(t *testing.T) {
	var (
		mu     sync.Mutex
		refuse error
		asked  int
	)
	setRefuse := func(err error) { mu.Lock(); refuse = err; mu.Unlock() }
	r := newRig(t, func(o *Options) {
		o.LoginCheck = func() error { mu.Lock(); defer mu.Unlock(); asked++; return refuse }
	}).start()
	setRefuse(errors.New("a sandbox reaches it"))
	r.selectLogin()
	if it := loginOf(r.ui.lastMenu()); it == nil || it.Checked {
		t.Errorf("login item after a refused on %+v", it)
	}
	if findTitle(r.ui.lastMenu(), "Open at login refused: a sandbox reaches it") == nil {
		t.Errorf("no refusal line:\n%s", Dump(r.ui.lastMenu()))
	}
	r.ui.mu.Lock()
	if got := fmt.Sprint(r.ui.setLog); got != "[]" {
		t.Errorf("set %s after a refusal", got)
	}
	r.ui.mu.Unlock()
	setRefuse(nil)
	r.selectLogin()
	r.selectLogin() // off: not asked
	mu.Lock()
	if asked != 2 {
		t.Errorf("asked %d times, want 2", asked)
	}
	mu.Unlock()
	r.ui.mu.Lock()
	r.ui.onState = LoginNeedsApproval
	r.ui.mu.Unlock()
	r.selectLogin() // on, which needs approval
	setRefuse(errors.New("nope"))
	r.selectLogin() // approving is turning it on
	if strings.Contains(strings.Join(r.ui.snapshot().calls, ","), "OpenLoginSettings") {
		t.Error("a refused approval opened the settings")
	}
	if got := fmt.Sprint(r.ui.setLog); got != "[true false true]" {
		t.Errorf("set %s", got)
	}
}

func TestQuitClosesFirst(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.selectItem("quit")
	select {
	case err := <-r.done:
		r.done <- err // for the cleanup
	case <-time.After(10 * time.Second):
		t.Fatal("did not quit")
	}
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	if !r.d.conns[0].closed {
		t.Error("quit without closing the connection")
	}
}

// Quit chosen while a URL dialog is shown (the menu opened over it) ends
// the UI and closes the connection, and answers nothing: the glue closes
// the dialog without an answer, and the daemon denies the request in
// flight once the attachment ends. A late answer from the glue is
// dropped too.
func TestQuitWithURLDialogShown(t *testing.T) {
	r := newRig(t, nil)
	views := []daemonproto.RequestView{
		view(7, "open_url", "open_url:https://example.com", "open https://example.com/page in the browser", t0),
	}
	s := &requestsScript{views: map[string][]daemonproto.RequestView{"dev": views}, offers: map[uint64][]daemonproto.Offer{}}
	s.install(r.d)
	r.d.handle(daemonproto.MethodDialogs, func(json.RawMessage) (any, error) {
		return daemonproto.DialogsResult{Dialog: &daemonproto.DialogRef{Env: "dev", ID: 7}}, nil
	})
	var mu sync.Mutex
	var answers []string
	r.d.handle(daemonproto.MethodAnswer, func(p json.RawMessage) (any, error) {
		mu.Lock()
		answers = append(answers, string(p))
		mu.Unlock()
		return daemonproto.AnswerResult{}, nil
	})
	r.start()
	asks := r.ui.snapshot().asks
	if len(asks) != 1 {
		t.Fatalf("asks %+v", asks)
	}
	r.do(func(e Events) { e.MenuOpened() })
	r.do(func(e Events) { e.MenuClosed() })
	r.selectItem("quit")
	select {
	case err := <-r.done:
		r.done <- err // for the cleanup
	case <-time.After(10 * time.Second):
		t.Fatal("did not quit with a URL dialog shown")
	}
	r.core.URLAnswered(asks[0].ID, true)
	r.core.waitIdle()
	mu.Lock()
	defer mu.Unlock()
	if len(answers) != 0 {
		t.Errorf("answered at quit: %v", answers)
	}
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	if !r.d.conns[0].closed {
		t.Error("quit without closing the connection")
	}
}

// TestEventsNeverBlock holds every daemon call and dial forever: each
// Events method still returns at once.
func TestEventsNeverBlock(t *testing.T) {
	r := newRig(t, nil)
	scripted(r)
	r.start()
	hang := make(chan struct{})
	r.d.mu.Lock()
	r.d.hang = hang
	r.d.mu.Unlock()
	defer close(hang)
	// The next poll's calls now hang: fire its timers without waiting
	// for the core to settle, which it never does.
	limit := r.clock.Now().Add(PollEvery)
	for t := r.clock.next(limit); t != nil; t = r.clock.next(limit) {
		t.f()
	}
	ids := []string{"web", "req:dev:25:once", "stop:dev", "proposals", "quit"}
	done := make(chan string, 64)
	go func() {
		ev := Events(r.core)
		ev.MenuOpened()
		done <- "MenuOpened"
		for _, id := range ids {
			ev.Selected(id)
			done <- "Selected " + id
		}
		ev.Confirmed("c1", true)
		done <- "Confirmed"
		ev.URLAnswered("u1", true)
		done <- "URLAnswered"
		ev.NotificationClicked("n1")
		done <- "NotificationClicked"
		ev.NotificationAuth(true)
		done <- "NotificationAuth"
		ev.MenuClosed()
		done <- "MenuClosed"
		close(done)
	}()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-done:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("an Events method blocked on a hanging daemon")
		}
	}
}

// A protocol refusal is a newer caboose's daemon: the app never starts
// one after it, even once nothing answers.
func TestNoSpawnAfterRefusal(t *testing.T) {
	r := newRig(t, nil)
	r.d.dialErr = daemonproto.NewError(daemonproto.KindProtocolMismatch, "the running daemon speaks protocol 2")
	r.start()
	r.d.mu.Lock()
	r.d.dialErr, r.d.down = nil, true
	r.d.mu.Unlock()
	r.advance(3 * time.Minute)
	if len(r.d.connects) < 3 || len(r.d.ensures) != 0 {
		t.Errorf("connects at %s, ensures at %s", secs(r.d.connects), secs(r.d.ensures))
	}
}

func TestNoSpawnAfterOlderCompat(t *testing.T) {
	r := newRig(t, nil)
	r.d.compat = daemonproto.CompatClientOlder
	r.start()
	if findTitle(r.ui.lastMenu(), TextAppOlder) == nil {
		t.Errorf("no update line:\n%s", Dump(r.ui.lastMenu()))
	}
	r.d.mu.Lock()
	r.d.down = true
	r.d.mu.Unlock()
	r.advance(3 * time.Minute)
	if len(r.d.connects) < 3 || len(r.d.ensures) != 0 {
		t.Errorf("connects at %s, ensures at %s", secs(r.d.connects), secs(r.d.ensures))
	}
}

// Before a request's offers arrive, its submenu offers only Deny.
func TestMenuBeforeOffers(t *testing.T) {
	r := newRig(t, nil)
	s := &requestsScript{views: map[string][]daemonproto.RequestView{"dev": {view(3, "alias", "alias:deploy", "run alias deploy", t0)}}}
	s.install(r.d)
	r.d.handle(daemonproto.MethodRequest, func(json.RawMessage) (any, error) {
		return nil, daemonproto.NewError(daemonproto.KindBrokerUnavailable, "not now")
	})
	r.setStatus(daemonproto.StatusResult{Envs: []daemonproto.EnvStatus{{Name: "dev", Sandbox: "running", Link: "running", Pending: 1}}})
	r.start()
	it := findTitle(r.ui.lastMenu(), "#3 dev alias: deploy")
	if it == nil {
		t.Fatalf("no request item:\n%s", Dump(r.ui.lastMenu()))
	}
	var got []string
	for _, k := range it.Children {
		if !k.Separator {
			got = append(got, k.Title)
		}
	}
	if want := "Deny|Details… (web page)"; strings.Join(got, "|") != want {
		t.Errorf("answers before offers %q, want %q", strings.Join(got, "|"), want)
	}
}

func TestItemTitleClipped(t *testing.T) {
	b := &builder{acts: map[string]action{}}
	it := b.item("x", strings.Repeat("w", 300), action{kind: actRoute})
	if utf8.RuneCountInString(it.Title) > MaxTitle {
		t.Errorf("an enabled item of %d characters", utf8.RuneCountInString(it.Title))
	}
}
