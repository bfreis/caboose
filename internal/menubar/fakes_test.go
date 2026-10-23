package menubar

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
)

// t0 is the fake clock's start: a fixed instant in UTC, so nothing
// depends on the machine's zone.
var t0 = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// fakeClock fires its timers only when the test advances it.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	f       func()
	stopped bool
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		was := !t.stopped
		t.stopped = true
		return was
	}
}

// next takes the earliest timer due by limit, moving the clock to it.
func (c *fakeClock) next(limit time.Time) *fakeTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	var best *fakeTimer
	for _, t := range c.timers {
		if !t.stopped && !t.at.After(limit) && (best == nil || t.at.Before(best.at)) {
			best = t
		}
	}
	if best == nil {
		return nil
	}
	best.stopped = true
	if best.at.After(c.now) {
		c.now = best.at
	}
	return best
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.After(c.now) {
		c.now = t
	}
}

// fakeUI records every call.
type fakeUI struct {
	mu       sync.Mutex
	ev       Events
	quit     chan struct{}
	quitted  bool
	calls    []string
	menus    []Menu
	icons    []IconState
	confirm  []Confirmation
	asks     []URLDialog
	notes    []Notification
	opened   []string
	dismiss  []string
	alerts   []string
	alertIDs []string
	openErr  error
	login    LoginState
	onState  LoginState // what SetLoginItem(true) leaves
	setLog   []bool
}

func newFakeUI() *fakeUI {
	return &fakeUI{quit: make(chan struct{}), login: LoginOff, onState: LoginOn}
}

func (u *fakeUI) rec(s string) {
	u.mu.Lock()
	u.calls = append(u.calls, s)
	u.mu.Unlock()
}

func (u *fakeUI) Run(ev Events, ready func()) error {
	ready() // queues the core's start before the test can see ev
	u.mu.Lock()
	u.ev = ev
	u.mu.Unlock()
	<-u.quit
	return nil
}

func (u *fakeUI) SetIcon(s IconState) {
	u.mu.Lock()
	u.icons = append(u.icons, s)
	u.mu.Unlock()
	u.rec("SetIcon")
}

func (u *fakeUI) SetMenu(m Menu) {
	u.mu.Lock()
	u.menus = append(u.menus, m)
	u.mu.Unlock()
	u.rec("SetMenu")
}

func (u *fakeUI) Confirm(c Confirmation) {
	u.mu.Lock()
	u.confirm = append(u.confirm, c)
	u.mu.Unlock()
	u.rec("Confirm " + c.ID)
}

func (u *fakeUI) AskURL(d URLDialog) {
	u.mu.Lock()
	u.asks = append(u.asks, d)
	u.mu.Unlock()
	u.rec("AskURL " + d.ID)
}

func (u *fakeUI) Dismiss(id string) {
	u.mu.Lock()
	u.dismiss = append(u.dismiss, id)
	u.mu.Unlock()
	u.rec("Dismiss " + id)
}

func (u *fakeUI) Notify(n Notification) {
	u.mu.Lock()
	u.notes = append(u.notes, n)
	u.mu.Unlock()
	u.rec("Notify " + n.ID)
}

func (u *fakeUI) Withdraw(ids []string) { u.rec("Withdraw " + strings.Join(ids, ",")) }

func (u *fakeUI) Open(url string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.opened = append(u.opened, url)
	u.calls = append(u.calls, "Open")
	return u.openErr
}

func (u *fakeUI) LoginItem() (LoginState, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.login, nil
}

func (u *fakeUI) SetLoginItem(on bool) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.setLog = append(u.setLog, on)
	if on {
		u.login = u.onState
	} else {
		u.login = LoginOff
	}
	return nil
}

func (u *fakeUI) OpenLoginSettings() error { u.rec("OpenLoginSettings"); return nil }

func (u *fakeUI) Alert(id, text string) {
	u.mu.Lock()
	u.alerts = append(u.alerts, text)
	u.alertIDs = append(u.alertIDs, id)
	u.mu.Unlock()
	u.rec("Alert")
}

func (u *fakeUI) Quit() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.quitted {
		u.quitted = true
		close(u.quit)
	}
}

func (u *fakeUI) lastMenu() Menu {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.menus) == 0 {
		return Menu{}
	}
	return u.menus[len(u.menus)-1]
}

func (u *fakeUI) lastIcon() IconState {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.icons) == 0 {
		return IconState{}
	}
	return u.icons[len(u.icons)-1]
}

func (u *fakeUI) snapshot() fakeUI {
	u.mu.Lock()
	defer u.mu.Unlock()
	return fakeUI{calls: slices.Clone(u.calls), menus: slices.Clone(u.menus), icons: slices.Clone(u.icons),
		confirm: slices.Clone(u.confirm), asks: slices.Clone(u.asks), notes: slices.Clone(u.notes),
		opened: slices.Clone(u.opened), dismiss: slices.Clone(u.dismiss), alerts: slices.Clone(u.alerts),
		alertIDs: slices.Clone(u.alertIDs), quitted: u.quitted}
}

// call is one call the fake daemon got.
type call struct {
	at     time.Duration // since t0
	method string
	params string // JSON
}

type handler func(params json.RawMessage) (any, error)

// fakeDaemon scripts the daemon: whether it answers, its hello, and a
// handler per method; it records every call.
type fakeDaemon struct {
	mu       sync.Mutex
	clock    *fakeClock
	down     bool // nothing answers
	startErr error
	dialErr  error // Connect fails with it while up
	frame    daemonproto.Frame
	compat   daemonproto.Compat
	handlers map[string]handler
	calls    []call
	connects []time.Duration
	ensures  []time.Duration
	replaced *daemon.Replaced
	conns    []*fakeConn
	hang     chan struct{} // set: every call waits on it
	// noValidate skips the client's checks of results, to send what a
	// daemon never does.
	noValidate bool
}

func newFakeDaemon(clock *fakeClock) *fakeDaemon {
	d := &fakeDaemon{clock: clock, frame: daemonproto.Frame{Type: daemonproto.TypeHello, Protocol: daemonproto.ProtocolVersion, Version: "v1.0.0", Client: daemonproto.ClientDaemon},
		compat: daemonproto.CompatSame, handlers: map[string]handler{}}
	d.handle(daemonproto.MethodAppAttach, func(p json.RawMessage) (any, error) {
		var ap daemonproto.AppAttachParams
		_ = json.Unmarshal(p, &ap)
		return daemonproto.AppAttachResult{Attached: true, Capabilities: ap.Capabilities}, nil
	})
	d.handle(daemonproto.MethodStatus, func(json.RawMessage) (any, error) {
		return daemonproto.StatusResult{Envs: []daemonproto.EnvStatus{}, Attention: []daemonproto.AttentionItem{}}, nil
	})
	d.handle(daemonproto.MethodNotifications, func(json.RawMessage) (any, error) { return daemonproto.NotificationsResult{}, nil })
	d.handle(daemonproto.MethodDialogs, func(json.RawMessage) (any, error) { return daemonproto.DialogsResult{}, nil })
	return d
}

func (d *fakeDaemon) since() time.Duration { return d.clock.Now().Sub(t0) }

func (d *fakeDaemon) handle(method string, h handler) {
	d.mu.Lock()
	d.handlers[method] = h
	d.mu.Unlock()
}

func (d *fakeDaemon) Connect(ctx context.Context) (Conn, error) {
	d.mu.Lock()
	d.connects = append(d.connects, d.since())
	down, dialErr, hang := d.down, d.dialErr, d.hang
	d.mu.Unlock()
	if hang != nil {
		select {
		case <-hang:
		case <-ctx.Done():
		}
	}
	if down {
		return nil, daemonproto.NewError(daemonproto.KindUnavailable, "nothing answers")
	}
	if dialErr != nil {
		return nil, dialErr
	}
	return d.newConn(), nil
}

func (d *fakeDaemon) Ensure(ctx context.Context) (Conn, error) {
	d.mu.Lock()
	d.ensures = append(d.ensures, d.since())
	err := d.startErr
	if err == nil {
		d.down = false
	}
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return d.newConn(), nil
}

func (d *fakeDaemon) Replaced() (daemon.Replaced, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.replaced == nil {
		return daemon.Replaced{}, false
	}
	return *d.replaced, true
}

func (d *fakeDaemon) newConn() *fakeConn {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := &fakeConn{d: d, frame: d.frame, compat: d.compat}
	d.conns = append(d.conns, c)
	return c
}

// kill makes the daemon go away: its connections fail, nothing answers.
func (d *fakeDaemon) kill() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.down = true
	for _, c := range d.conns {
		c.dead = true
	}
}

func (r *rig) connectCount() int {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	return len(r.d.connects) + len(r.d.ensures)
}

func (d *fakeDaemon) callsOf(method string) []call {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []call
	for _, c := range d.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

type fakeConn struct {
	d      *fakeDaemon
	frame  daemonproto.Frame
	compat daemonproto.Compat
	dead   bool
	closed bool
}

func (c *fakeConn) Daemon() daemonproto.Frame  { return c.frame }
func (c *fakeConn) Compat() daemonproto.Compat { return c.compat }

func (c *fakeConn) Close() error {
	c.d.mu.Lock()
	c.closed = true
	c.d.mu.Unlock()
	return nil
}

// Call answers as the real client does: the result decoded from JSON and
// Validated.
func (c *fakeConn) Call(ctx context.Context, method string, params, result any) error {
	d := c.d
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		raw = b
	}
	d.mu.Lock()
	d.calls = append(d.calls, call{at: d.since(), method: method, params: string(raw)})
	dead, closed, h, hang, noValidate := c.dead, c.closed, d.handlers[method], d.hang, d.noValidate
	d.mu.Unlock()
	if hang != nil {
		select {
		case <-hang:
		case <-ctx.Done():
		}
		return daemonproto.NewError(daemonproto.KindTimeout, "hung")
	}
	if dead || closed {
		return daemonproto.NewError(daemonproto.KindUnavailable, "the connection to the daemon is closed")
	}
	if h == nil {
		return daemonproto.NewError(daemonproto.KindUnknownMethod, "%s", method)
	}
	res, err := h(raw)
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, result); err != nil {
		return err
	}
	if v, ok := result.(interface{ Validate() error }); ok && !noValidate {
		if err := v.Validate(); err != nil {
			return daemonproto.NewError(daemonproto.KindBadFrame, "the result of %s: %v", method, err)
		}
	}
	return nil
}

// rig is a core with fakes all round, running.
type rig struct {
	t     *testing.T
	clock *fakeClock
	ui    *fakeUI
	d     *fakeDaemon
	core  *Core
	logMu sync.Mutex
	logs  []string
	done  chan error
}

func newRig(t *testing.T, mod func(*Options)) *rig {
	t.Helper()
	clock := &fakeClock{now: t0}
	r := &rig{t: t, clock: clock, ui: newFakeUI(), d: newFakeDaemon(clock), done: make(chan error, 1)}
	return r.with(mod)
}

func (r *rig) with(mod func(*Options)) *rig {
	opts := Options{Daemon: r.d, UI: r.ui, Clock: r.clock, Version: "v1.0.0", Dialogs: true,
		Log: func(s string) { r.logMu.Lock(); r.logs = append(r.logs, s); r.logMu.Unlock() }}
	if mod != nil {
		mod(&opts)
	}
	r.core = New(opts)
	return r
}

// start runs the core and waits for it to settle.
func (r *rig) start() *rig {
	go func() { r.done <- r.core.Run() }()
	r.t.Cleanup(func() {
		r.ui.Quit()
		select {
		case <-r.done:
		case <-time.After(10 * time.Second):
			r.t.Error("the core did not stop")
		}
	})
	for {
		r.ui.mu.Lock()
		ev := r.ui.ev
		r.ui.mu.Unlock()
		if ev != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	r.core.waitIdle()
	return r
}

// advance moves the clock by d, firing each timer due in turn and letting
// the core settle after each.
func (r *rig) advance(d time.Duration) {
	limit := r.clock.Now().Add(d)
	for {
		t := r.clock.next(limit)
		if t == nil {
			break
		}
		t.f()
		r.core.waitIdle()
	}
	r.clock.set(limit)
	r.core.waitIdle()
}

// do posts an event and lets the core settle.
func (r *rig) do(f func(Events)) {
	f(r.core)
	r.core.waitIdle()
}

func (r *rig) logText() string {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	return strings.Join(r.logs, "\n")
}

// titles is every title the menu holds.
func titles(m Menu) []string {
	var out []string
	var walk func([]Item)
	walk = func(items []Item) {
		for _, it := range items {
			out = append(out, it.Title)
			walk(it.Children)
		}
	}
	walk(m.Items)
	return out
}

// find is the item of id in m, or nil.
func find(m Menu, id string) *Item {
	var walk func([]Item) *Item
	walk = func(items []Item) *Item {
		for i := range items {
			if items[i].ID == id {
				return &items[i]
			}
			if f := walk(items[i].Children); f != nil {
				return f
			}
		}
		return nil
	}
	return walk(m.Items)
}

func findTitle(m Menu, title string) *Item {
	var walk func([]Item) *Item
	walk = func(items []Item) *Item {
		for i := range items {
			if items[i].Title == title {
				return &items[i]
			}
			if f := walk(items[i].Children); f != nil {
				return f
			}
		}
		return nil
	}
	return walk(m.Items)
}

func secs(ds []time.Duration) string {
	var s []string
	for _, d := range ds {
		s = append(s, fmt.Sprint(d.Seconds()))
	}
	return strings.Join(s, ",")
}

// Dump is the menu as text, one item a line, indented by depth: what the
// tests compare against golden files.
func Dump(m Menu) string {
	var b strings.Builder
	var walk func(items []Item, depth int)
	walk = func(items []Item, depth int) {
		for _, it := range items {
			b.WriteString(strings.Repeat("  ", depth))
			if it.Separator {
				b.WriteString("----\n")
				continue
			}
			b.WriteString(it.Title)
			var flags []string
			if it.Info {
				flags = append(flags, "info")
			} else if !it.Enabled {
				flags = append(flags, "disabled")
			}
			if it.Checked {
				flags = append(flags, "checked")
			}
			if it.ID != "" {
				flags = append(flags, "id="+it.ID)
			}
			if len(flags) > 0 {
				b.WriteString("  [" + strings.Join(flags, " ") + "]")
			}
			b.WriteString("\n")
			walk(it.Children, depth+1)
		}
	}
	walk(m.Items, 0)
	return b.String()
}

// waitIdle waits until no event is queued and no worker runs.
func (c *Core) waitIdle() {
	c.mu.Lock()
	for c.work > 0 {
		c.idle.Wait()
	}
	c.mu.Unlock()
}
