package menubar

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
)

// Timings of the core.
const (
	// PollEvery is how often status, notifications and dialogs are asked
	// for while connected.
	PollEvery = 2 * time.Second
	// DetailsEvery is how often a running environment's details are
	// asked for; DetailsFresh how old they may be when the menu opens.
	DetailsEvery = 30 * time.Second
	DetailsFresh = 10 * time.Second
	// RequestsEvery is how often an environment with requests pending is
	// listed again even though its count has not changed.
	RequestsEvery = 30 * time.Second
	// FirstRetry is the wait after losing the daemon before the first
	// attempt to reach it, so that whatever replaced it starts its
	// successor first; each failure doubles the wait, to MaxRetry.
	FirstRetry = 5 * time.Second
	MaxRetry   = 30 * time.Second
	// OlderRetry is the wait between attempts while the daemon refuses
	// this app's protocol as older than its own.
	OlderRetry = 60 * time.Second
	// WatchEvery is how often the app looks for a daemon it may not
	// start (a newer caboose's).
	WatchEvery = 2 * time.Second
	// DisconnectedAfter is how long without a daemon before the icon
	// says so.
	DisconnectedAfter = 10 * time.Second
	// InfoFor is how long a transient line stays in the menu.
	InfoFor = time.Minute
	// QuitWait bounds the wait for the connection to close at Quit.
	QuitWait = 3 * time.Second
	// AlertWait bounds how long an app that quits after an alert waits
	// for the user to close it: a second app left unattended still ends.
	AlertWait = 2 * time.Minute
	// ReplacedEvery is how often the app asks whether an update replaced
	// its bundle.
	ReplacedEvery = 5 * time.Second
)

// Fixed texts.
const (
	TextBusy         = "Caboose is already running in the menu bar"
	TextNotAnswering = "caboose's daemon is not answering"
	TextAppOlder     = "caboose is newer than this menu-bar app: update the app, or quit it"
	TextWaitNewer    = "waiting for caboose's daemon: a newer caboose runs it"
	TextWaitUpdate   = "waiting for caboose's daemon to update"
	TextConnecting   = "connecting to caboose's daemon…"
)

// BusyAlertID is the alert's ID for TextBusy.
const BusyAlertID = "busy"

// hoursPlaceholder stands for the hours in an hours offer's effect.
const hoursPlaceholder = "{hours}"

// Clock is the core's time: Now, and AfterFunc, which runs f once d has
// passed and returns a stop function.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) func() bool {
	return time.AfterFunc(d, f).Stop
}

// Options configure New.
type Options struct {
	Daemon Daemon
	UI     UI
	Clock  Clock // nil is the system's
	// Version is the app's caboose version; Protocol its protocol (0 is
	// daemonproto.ProtocolVersion).
	Version  string
	Protocol int
	// Dev is set for an app built from a checkout: its title says so
	// before the daemon does.
	Dev bool
	// Dialogs is set when the UI can ask URL questions: the app then
	// attaches with the dialogs capability.
	Dialogs bool
	// Log receives one line per event worth keeping; nil discards. It
	// never receives the web page's token.
	Log func(string)
	// LoginCheck, when set, is asked before the login item is turned on
	// (or approved): an error refuses it, and the menu says why. Nil
	// asks nothing.
	LoginCheck func() error
	// Replaced, when set, says whether the app's bundle on disk is no
	// longer the one it runs from, as an update leaves it; Relaunch then
	// starts the new one, once this process has ended. Asked every
	// ReplacedEvery, but never while the user has a question in front of
	// them: once Replaced says so, the core relaunches and quits. A
	// Relaunch that fails is logged, and the app goes on as it is.
	Replaced func() bool
	Relaunch func() error
}

type connState int

const (
	stConnecting connState = iota // never connected yet
	stConnected
	stLost       // lost, or never reached
	stRefused    // the daemon refuses this app's protocol as older
	stWaitNewer  // a newer caboose's daemon went away: watching only
	stWaitUpdate // asked an older daemon to make way
)

type reqKey struct {
	env string
	id  uint64
}

type detailsEntry struct {
	res *daemonproto.EnvDetailsResult
	at  time.Time
}

type infoLine struct {
	text  string
	until time.Time
}

// confirmation is the alert the core waits on.
type confirmation struct {
	id  string
	act action
	// params is the answer to send once confirmed.
	params daemonproto.AnswerParams
}

// dialog is the URL question the UI shows.
type dialog struct {
	id  string
	ref daemonproto.DialogRef
}

// Core is the app's core. It implements Events.
type Core struct {
	opts  Options
	clock Clock
	proto int

	// The queue: every event, glue's and workers' alike, is a func run by
	// the loop goroutine, which alone touches the state below.
	mu      sync.Mutex
	queue   []func()
	wake    chan struct{}
	work    int // queued events and running workers
	idle    *sync.Cond
	ctx     context.Context
	cancel  context.CancelFunc
	stopped bool

	// State, the loop's alone.
	conn        Conn
	gen         uint64
	state       connState
	lostAt      time.Time
	lastErr     string
	attempts    int
	seenNewer   bool
	replaceSent map[string]bool

	attached  bool
	caps      []string
	notifAuth bool
	noAttach  bool
	noReqs    bool

	status    *daemonproto.StatusResult
	details   map[string]*detailsEntry
	views     map[string][]daemonproto.RequestView
	fetched   map[string]int       // the pending count each env's views were listed at
	fetchedAt map[string]time.Time // and when
	offers    map[reqKey]*daemonproto.RequestResult
	inflight  map[string]bool

	menuOpen  bool
	menuDirty bool
	sentMenu  *Menu
	actions   map[string]action
	// past are the tables of the menus sent before the last, newest
	// last: the glue may still show one of them (it holds a menu sent as
	// it opened), and AppKit posts the menu's closing before the
	// selection made in it, by when the close has sent a newer menu.
	past      []map[string]action
	sentIcon  *IconState
	info      []infoLine
	login     LoginState
	loginErr  string
	nextID    int
	confirm   *confirmation
	dialog    *dialog
	answering *daemonproto.DialogRef

	notifAfter uint64
	throttle   Throttle
	routes     map[string]string
	routeOrder []string
	quitting   bool
	// quitAlert is the alert a quit waits for the user to close ("" for
	// none); stopReady is set once the quit waits for nothing else.
	quitAlert string
	stopReady bool
	uiQuit    bool
}

var _ Events = (*Core)(nil)

// New returns the core, which runs once Run is called.
func New(o Options) *Core {
	c := &Core{opts: o, clock: o.Clock, proto: o.Protocol, wake: make(chan struct{}, 1),
		replaceSent: map[string]bool{}, routes: map[string]string{}}
	if c.clock == nil {
		c.clock = realClock{}
	}
	if c.proto == 0 {
		c.proto = daemonproto.ProtocolVersion
	}
	c.idle = sync.NewCond(&c.mu)
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.reset()
	return c
}

func (c *Core) reset() {
	c.attached, c.caps, c.noAttach, c.noReqs = false, nil, false, false
	c.status = nil
	c.details = map[string]*detailsEntry{}
	c.views = map[string][]daemonproto.RequestView{}
	c.fetched = map[string]int{}
	c.fetchedAt = map[string]time.Time{}
	c.offers = map[reqKey]*daemonproto.RequestResult{}
	c.inflight = map[string]bool{}
}

// Run runs the app: it hands the calling (main) thread to the UI until
// the user quits, and returns the UI's error.
func (c *Core) Run() error {
	go c.loop()
	err := c.opts.UI.Run(c, func() { c.post(c.start) })
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
	c.cancel()
	return err
}

func (c *Core) logf(format string, a ...any) {
	if c.opts.Log != nil {
		c.opts.Log(Clean(fmt.Sprintf(format, a...)))
	}
}

// post queues f for the loop. It never blocks.
func (c *Core) post(f func()) {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.queue = append(c.queue, f)
	c.work++
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Core) loop() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.wake:
		}
		for {
			c.mu.Lock()
			if len(c.queue) == 0 {
				c.mu.Unlock()
				break
			}
			f := c.queue[0]
			c.queue = c.queue[1:]
			c.mu.Unlock()
			f()
			c.done()
		}
	}
}

func (c *Core) done() {
	c.mu.Lock()
	c.work--
	if c.work == 0 {
		c.idle.Broadcast()
	}
	c.mu.Unlock()
}

// spawn runs f in a worker; then posts its result, which the loop runs.
func (c *Core) spawn(f func() func()) {
	c.mu.Lock()
	c.work++
	c.mu.Unlock()
	go func() {
		defer c.done()
		if r := f(); r != nil {
			c.post(r)
		}
	}()
}

// after runs f on the loop once d has passed.
func (c *Core) after(d time.Duration, f func()) {
	c.clock.AfterFunc(d, func() { c.post(f) })
}

// call runs one daemon call of a kind in a worker, at most one of the
// kind in flight; done runs on the loop with its error, unless the
// connection changed meanwhile. A call that finds the connection gone
// loses it.
func (c *Core) call(kind, method string, params, result any, done func(error)) bool {
	if c.conn == nil || c.inflight[kind] {
		return false
	}
	c.inflight[kind] = true
	conn, gen := c.conn, c.gen
	c.spawn(func() func() {
		err := conn.Call(c.ctx, method, params, result)
		return func() {
			if gen != c.gen {
				return
			}
			delete(c.inflight, kind)
			if lost(err) {
				c.lose(err)
				return
			}
			done(err)
		}
	})
	return true
}

func lost(err error) bool {
	switch daemonproto.Kind(err) {
	case daemonproto.KindUnavailable, daemonproto.KindTimeout, daemonproto.KindBadFrame:
		return err != nil
	}
	return false
}

// Events: each only queues.

func (c *Core) Selected(itemID string)           { c.post(func() { c.selected(itemID) }) }
func (c *Core) MenuOpened()                      { c.post(c.menuOpened) }
func (c *Core) MenuClosed()                      { c.post(c.menuClosed) }
func (c *Core) Confirmed(id string, ok bool)     { c.post(func() { c.confirmed(id, ok) }) }
func (c *Core) URLAnswered(id string, open bool) { c.post(func() { c.urlAnswered(id, open) }) }
func (c *Core) NotificationClicked(id string)    { c.post(func() { c.notificationClicked(id) }) }
func (c *Core) NotificationAuth(granted bool)    { c.post(func() { c.notificationAuth(granted) }) }
func (c *Core) AlertClosed(id string)            { c.post(func() { c.alertClosed(id) }) }

func (c *Core) start() {
	c.lostAt = c.clock.Now()
	c.after(DisconnectedAfter, c.render)
	if c.opts.Replaced != nil && c.opts.Relaunch != nil {
		c.after(ReplacedEvery, c.checkReplaced)
	}
	c.readLogin()
	c.render()
	c.attempt()
}

// checkReplaced relaunches into a bundle an update put in place of this
// one, once no question waits on the user (an answer half given is not
// lost to it).
func (c *Core) checkReplaced() {
	if c.quitting {
		return
	}
	if c.confirm != nil || c.dialog != nil || c.answering != nil || !c.opts.Replaced() {
		c.after(ReplacedEvery, c.checkReplaced)
		return
	}
	if err := c.opts.Relaunch(); err != nil {
		c.logf("the app was updated, but cannot relaunch into the new one: %v", err)
		return
	}
	c.logf("the app was updated: relaunching into the new one")
	c.quit()
}

// Connecting.

func (c *Core) attempt() {
	if c.conn != nil || c.inflight["connect"] || c.quitting {
		return
	}
	c.inflight["connect"] = true
	seen, gen := c.seenNewer, c.gen
	d := c.opts.Daemon
	c.spawn(func() func() {
		conn, err := d.Connect(c.ctx)
		may := true
		if daemonproto.Kind(err) == daemonproto.KindUnavailable && err != nil {
			r, ok := d.Replaced()
			may = MayStart(seen, r, ok, c.proto, c.opts.Version)
			if may {
				conn, err = d.Ensure(c.ctx)
			}
		}
		return func() {
			delete(c.inflight, "connect")
			if gen != c.gen || c.quitting {
				if conn != nil {
					c.closeConn(conn)
				}
				return
			}
			c.connected(conn, err, may)
		}
	})
}

func (c *Core) connected(conn Conn, err error, may bool) {
	if err == nil && conn == nil {
		err = daemonproto.NewError(daemonproto.KindUnavailable, "no connection")
	}
	if err != nil {
		c.lastErr = Clean(err.Error())
		switch {
		case daemonproto.Kind(err) == daemonproto.KindProtocolMismatch:
			c.seenNewer = true
			c.state = stRefused
			c.after(OlderRetry, c.attempt)
		case !may:
			c.state = stWaitNewer
			c.after(WatchEvery, c.attempt)
		default:
			c.state = stLost
			c.after(c.backoff(), c.attempt)
		}
		c.logf("reaching the daemon: %s", c.lastErr)
		c.render()
		return
	}
	f := conn.Daemon()
	if newer(f.Protocol, f.Version, c.proto, c.opts.Version) {
		c.seenNewer = true
	}
	switch conn.Compat() {
	case daemonproto.CompatClientNewer:
		key := fmt.Sprintf("%d/%s", f.Protocol, f.Version)
		c.state = stWaitUpdate
		if !c.replaceSent[key] {
			c.replaceSent[key] = true
			c.logf("the daemon (caboose %s, protocol %d) is older than the app: asking it to make way", f.Version, f.Protocol)
			ver := c.opts.Version
			c.spawn(func() func() {
				var r daemonproto.ReplaceResult
				_ = conn.Call(c.ctx, daemonproto.MethodReplace, &daemonproto.ReplaceParams{Version: ver, Force: false}, &r)
				_ = conn.Close()
				return nil
			})
			c.attempts = 1
			c.after(FirstRetry, c.attempt)
		} else {
			c.closeConn(conn)
			c.after(c.backoff(), c.attempt)
		}
		c.render()
		return
	case daemonproto.CompatClientOlder:
		c.closeConn(conn)
		c.seenNewer = true
		c.state = stRefused
		c.after(OlderRetry, c.attempt)
		c.render()
		return
	}
	c.gen++
	c.conn = conn
	c.state = stConnected
	c.attempts = 0
	c.lastErr = ""
	c.reset()
	c.logf("connected to the daemon (caboose %s)", f.Version)
	c.attach()
	c.tick(c.gen)
	c.render()
}

// backoff is the wait before the next attempt: FirstRetry, doubled for
// each failure since, to MaxRetry.
func (c *Core) backoff() time.Duration {
	d := FirstRetry
	for i := 0; i < c.attempts && d < MaxRetry; i++ {
		d *= 2
	}
	c.attempts++
	return min(d, MaxRetry)
}

func (c *Core) closeConn(conn Conn) {
	c.spawn(func() func() { _ = conn.Close(); return nil })
}

// lose ends the connection: what it showed goes, and the first attempt to
// reach a daemon again comes FirstRetry later.
func (c *Core) lose(err error) {
	if c.conn == nil {
		return
	}
	c.lastErr = Clean(err.Error())
	c.logf("lost the daemon: %s", c.lastErr)
	c.closeConn(c.conn)
	c.conn = nil
	c.gen++
	c.reset()
	if c.dialog != nil {
		c.opts.UI.Dismiss(c.dialog.id)
		c.dialog = nil
	}
	c.answering = nil
	if c.confirm != nil {
		c.opts.UI.Dismiss(c.confirm.id)
		c.confirm = nil
	}
	if c.state == stConnected {
		c.state = stLost
	}
	c.lostAt = c.clock.Now()
	c.attempts = 1
	c.after(FirstRetry, c.attempt)
	c.after(DisconnectedAfter, c.render)
	c.render()
}

// Attaching.

func (c *Core) wantCaps() []string {
	var caps []string
	if c.opts.Dialogs {
		caps = append(caps, daemonproto.CapDialogs)
	}
	if c.notifAuth {
		caps = append(caps, daemonproto.CapNotifications)
	}
	return caps
}

func (c *Core) attach() {
	var r daemonproto.AppAttachResult
	c.call("attach", daemonproto.MethodAppAttach, &daemonproto.AppAttachParams{Capabilities: c.wantCaps(), Version: c.opts.Version}, &r, func(err error) {
		switch {
		case err != nil && daemonproto.Kind(err) == daemonproto.KindBusy:
			c.logf("another app is attached")
			c.quitAfterAlert(BusyAlertID, TextBusy)
			return
		case err != nil && (daemonproto.Kind(err) == daemonproto.KindUnknownMethod || daemonproto.Kind(err) == daemonproto.KindNotAllowed):
			c.noAttach = true
			c.render()
			return
		}
		if err != nil {
			c.logf("attaching: %v", err)
			return
		}
		c.attached = r.Attached
		c.caps = r.Capabilities
		if c.has(daemonproto.CapNotifications) {
			c.pollNotifications()
		}
		if c.has(daemonproto.CapDialogs) {
			c.pollDialogs()
		}
		c.render()
	})
}

func (c *Core) has(cp string) bool {
	if !c.attached {
		return false
	}
	for _, x := range c.caps {
		if x == cp {
			return true
		}
	}
	return false
}

func (c *Core) notificationAuth(granted bool) {
	if granted == c.notifAuth {
		return
	}
	c.notifAuth = granted
	if c.conn != nil && !c.noAttach {
		c.attach()
	}
}

// Polling.

func (c *Core) tick(gen uint64) {
	if gen != c.gen || c.conn == nil {
		return
	}
	c.pollStatus()
	if c.has(daemonproto.CapNotifications) {
		c.pollNotifications()
	}
	if c.has(daemonproto.CapDialogs) {
		c.pollDialogs()
	}
	c.pollDetails(DetailsEvery)
	c.after(PollEvery, func() { c.tick(gen) })
}

func (c *Core) pollStatus() {
	var st daemonproto.StatusResult
	c.call("status", daemonproto.MethodStatus, nil, &st, func(err error) {
		if err != nil {
			c.logf("status: %v", err)
			return
		}
		c.status = &st
		c.statusChanged()
		c.render()
	})
}

func (c *Core) statusChanged() {
	now := c.clock.Now()
	known := map[string]bool{}
	var changed []string
	for _, e := range c.status.Envs {
		known[e.Name] = true
		if e.Owner != "" || e.Pending == 0 {
			c.dropViews(e.Name)
			continue
		}
		if n, ok := c.fetched[e.Name]; !ok || n != e.Pending || now.Sub(c.fetchedAt[e.Name]) >= RequestsEvery {
			changed = append(changed, e.Name)
		}
	}
	for env := range c.views {
		if !known[env] {
			c.dropViews(env)
		}
	}
	for env := range c.details {
		if !known[env] {
			delete(c.details, env)
		}
	}
	if len(changed) > 0 && !c.noReqs {
		c.fetchRequests(changed)
	}
}

func (c *Core) dropViews(env string) {
	delete(c.views, env)
	delete(c.fetched, env)
	delete(c.fetchedAt, env)
	for k := range c.offers {
		if k.env == env {
			delete(c.offers, k)
		}
	}
	c.dismissGone()
}

// dismissGone closes a confirmation whose request is no longer pending.
func (c *Core) dismissGone() {
	if c.confirm == nil || c.confirm.act.kind != actAnswer {
		return
	}
	for _, v := range c.views[c.confirm.act.env] {
		if v.ID == c.confirm.act.id {
			return
		}
	}
	c.opts.UI.Dismiss(c.confirm.id)
	c.confirm = nil
}

type envRequests struct {
	env     string
	pending int
	views   []daemonproto.RequestView
	offers  map[uint64]*daemonproto.RequestResult
	err     error
}

// fetchRequests lists each env's pending requests, and fetches the
// offers of those not cached, in one worker.
func (c *Core) fetchRequests(envs []string) {
	if c.conn == nil || c.inflight["requests"] {
		return
	}
	c.inflight["requests"] = true
	conn, gen := c.conn, c.gen
	pending := map[string]int{}
	have := map[reqKey]bool{}
	for _, e := range c.status.Envs {
		pending[e.Name] = e.Pending
	}
	for k := range c.offers {
		have[k] = true
	}
	c.spawn(func() func() {
		var out []envRequests
		var fatal error
		for _, env := range envs {
			er := envRequests{env: env, pending: pending[env], offers: map[uint64]*daemonproto.RequestResult{}}
			var list daemonproto.RequestsResult
			er.err = conn.Call(c.ctx, daemonproto.MethodRequests, &daemonproto.RequestsParams{Env: env, Limit: MaxRequestItems}, &list)
			if er.err == nil {
				er.views = list.Requests
				for _, v := range list.Requests {
					if have[reqKey{env, v.ID}] {
						continue
					}
					var r daemonproto.RequestResult
					if err := conn.Call(c.ctx, daemonproto.MethodRequest, &daemonproto.RequestParams{Env: env, ID: v.ID}, &r); err == nil {
						er.offers[v.ID] = &r
					} else if lost(err) {
						fatal = err
						break
					}
				}
			} else if lost(er.err) {
				fatal = er.err
			}
			out = append(out, er)
			if fatal != nil {
				break
			}
		}
		return func() {
			if gen != c.gen {
				return
			}
			delete(c.inflight, "requests")
			if fatal != nil {
				c.lose(fatal)
				return
			}
			now := c.clock.Now()
			for _, er := range out {
				switch {
				case daemonproto.Kind(er.err) == daemonproto.KindUnknownMethod && er.err != nil:
					c.noReqs = true
					continue
				case er.err != nil:
					c.logf("requests of %s: %v", er.env, er.err)
					continue
				}
				var views []daemonproto.RequestView
				ids := map[uint64]bool{}
				for _, v := range er.views {
					if v.State == "" || v.State == "pending" {
						views = append(views, v)
						ids[v.ID] = true
					}
				}
				c.views[er.env] = views
				c.fetched[er.env] = er.pending
				c.fetchedAt[er.env] = now
				for id, r := range er.offers {
					c.offers[reqKey{er.env, id}] = r
				}
				for k := range c.offers {
					if k.env == er.env && !ids[k.id] {
						delete(c.offers, k)
					}
				}
			}
			c.dismissGone()
			c.render()
		}
	})
}

func (c *Core) pollDetails(age time.Duration) {
	if c.status == nil {
		return
	}
	now := c.clock.Now()
	for _, e := range c.status.Envs {
		if e.Sandbox != daemonproto.SandboxRunning || e.Owner != "" {
			continue
		}
		if d := c.details[e.Name]; d != nil && now.Sub(d.at) < age {
			continue
		}
		env := e.Name
		var r daemonproto.EnvDetailsResult
		c.call("details:"+env, daemonproto.MethodEnvDetails, &daemonproto.EnvParams{Env: env}, &r, func(err error) {
			switch {
			case daemonproto.Kind(err) == daemonproto.KindUnknownEnv && err != nil:
				delete(c.details, env)
			case err != nil:
				c.logf("details of %s: %v", env, err)
				return
			default:
				c.details[env] = &detailsEntry{res: &r, at: c.clock.Now()}
			}
			c.render()
		})
	}
}

// Notifications.

func (c *Core) pollNotifications() {
	var r daemonproto.NotificationsResult
	c.call("notifications", daemonproto.MethodNotifications, &daemonproto.NotificationsParams{After: c.notifAfter}, &r, func(err error) {
		if err != nil {
			c.logf("notifications: %v", err)
			return
		}
		for _, n := range r.Items {
			c.deliver(n)
		}
		if r.Next > c.notifAfter {
			c.notifAfter = r.Next
		}
	})
}

// present is how the app shows a daemon notification: a sandbox's always
// under the sandbox's title and opening its environment's page; a host's
// under its own title only when it is one of the host's fixed ones.
func present(n daemonproto.Notification) (title, body, route string, ok bool) {
	body = Clean(n.Text)
	switch n.Origin {
	case daemonproto.OriginSandbox:
		if !daemonproto.ValidEnv(n.Env) {
			return "", "", "", false
		}
		return daemonproto.SandboxTitle(n.Env), body, daemonproto.RouteEnv(n.Env), true
	case daemonproto.OriginHost:
		title = "caboose"
		for _, t := range daemonproto.HostTitles(n.Env) {
			if n.Title == t {
				title = t
			}
		}
		route = n.Route
		if !validRoute(route) {
			route = ""
		}
		return title, body, route, true
	}
	return "", "", "", false
}

func (c *Core) deliver(n daemonproto.Notification) {
	title, body, route, ok := present(n)
	if !ok {
		return
	}
	own, count := c.throttle.Admit(c.clock.Now())
	if own {
		id := "n" + strconv.FormatUint(n.Seq, 10)
		c.remember(id, route)
		c.opts.UI.Notify(Notification{ID: id, Title: title, Body: body})
		return
	}
	c.remember(SummaryID, "")
	c.opts.UI.Notify(Notification{ID: SummaryID, Title: "caboose", Body: plural(count, "thing needs", "things need") + " you"})
}

const maxRoutes = 64

func (c *Core) remember(id, route string) {
	if _, ok := c.routes[id]; !ok {
		c.routeOrder = append(c.routeOrder, id)
	}
	c.routes[id] = route
	for len(c.routeOrder) > maxRoutes {
		delete(c.routes, c.routeOrder[0])
		c.routeOrder = c.routeOrder[1:]
	}
}

func (c *Core) notificationClicked(id string) {
	route, ok := c.routes[id]
	if !ok {
		return
	}
	c.openRoute(route)
}

// URL dialogs.

func (c *Core) pollDialogs() {
	var r daemonproto.DialogsResult
	c.call("dialogs", daemonproto.MethodDialogs, &daemonproto.DialogsParams{}, &r, func(err error) {
		if err != nil {
			c.logf("dialogs: %v", err)
			return
		}
		c.dialogsResult(r.Dialog)
	})
}

func (c *Core) dialogsResult(ref *daemonproto.DialogRef) {
	if c.dialog != nil && (ref == nil || *ref != c.dialog.ref) {
		c.opts.UI.Dismiss(c.dialog.id)
		c.dialog = nil
	}
	if c.answering != nil && (ref == nil || *ref != *c.answering) {
		c.answering = nil
	}
	if ref == nil || c.dialog != nil || c.answering != nil {
		return
	}
	want := *ref
	var r daemonproto.RequestResult
	c.call("dialog", daemonproto.MethodRequest, &daemonproto.RequestParams{Env: want.Env, ID: want.ID}, &r, func(err error) {
		if err != nil {
			c.logf("the URL request %s #%d: %v", want.Env, want.ID, err)
			return
		}
		if c.dialog != nil || c.answering != nil || r.Request.ID != want.ID || r.Request.Kind != "open_url" {
			return
		}
		c.nextID++
		d := &dialog{id: "u" + strconv.Itoa(c.nextID), ref: want}
		c.dialog = d
		c.opts.UI.AskURL(URLDialog{ID: d.id, Env: want.Env, URL: dialogURL(r.Request.Effect),
			SandboxSays: sandboxSays(r.Request.Ask, r.Request.Session)})
	})
}

// dialogURL is the URL an open_url request's host effect names, or the
// effect itself when it does not read as one.
func dialogURL(effect string) string {
	u, ok := strings.CutPrefix(effect, "open ")
	if ok {
		u, ok = strings.CutSuffix(u, " in the browser")
	}
	if !ok {
		return Clean(effect)
	}
	return Clean(u)
}

func (c *Core) urlAnswered(id string, open bool) {
	if c.dialog == nil || c.dialog.id != id {
		return
	}
	ref := c.dialog.ref
	c.dialog = nil
	c.answering = &ref
	p := daemonproto.AnswerParams{Env: ref.Env, ID: ref.ID, Verdict: daemonproto.VerdictDeny}
	if open {
		p.Verdict, p.Scope = daemonproto.VerdictAllow, daemonproto.ScopeOnce
	}
	c.sendAnswer("url", p, false)
}

// Answers.

func (c *Core) sendAnswer(kind string, p daemonproto.AnswerParams, report bool) {
	if c.conn == nil {
		c.addInfo(fmt.Sprintf("#%d was not answered: caboose's daemon is not answering", p.ID))
		return
	}
	conn, gen := c.conn, c.gen
	c.spawn(func() func() {
		var r daemonproto.AnswerResult
		err := conn.Call(c.ctx, daemonproto.MethodAnswer, &p, &r)
		by := ""
		if daemonproto.Kind(err) == daemonproto.KindAlreadyAnswered && err != nil && report {
			var cur daemonproto.RequestResult
			if conn.Call(c.ctx, daemonproto.MethodRequest, &daemonproto.RequestParams{Env: p.Env, ID: p.ID}, &cur) == nil {
				by = cur.Request.DecidedBy
			}
		}
		return func() {
			if gen != c.gen {
				return
			}
			if lost(err) {
				c.lose(err)
				return
			}
			switch {
			case daemonproto.Kind(err) == daemonproto.KindAlreadyAnswered && err != nil:
				if report {
					line := fmt.Sprintf("#%d was already answered", p.ID)
					if by != "" {
						line += " by " + by
					}
					c.addInfo(line)
				}
			case err != nil:
				c.addInfo(fmt.Sprintf("#%d: %s", p.ID, err.Error()))
			case r.OpenError != "":
				c.addInfo("opening the URL failed: " + r.OpenError)
			}
			delete(c.fetched, p.Env) // list it again at the next status
			c.render()
		}
	})
}

func (c *Core) askAnswer(a action) {
	if c.confirm != nil {
		c.opts.UI.Dismiss(c.confirm.id)
		c.confirm = nil
	}
	if c.conn == nil {
		c.addInfo(fmt.Sprintf("#%d: caboose's daemon is not answering", a.id))
		return
	}
	var r daemonproto.RequestResult
	c.call("confirm", daemonproto.MethodRequest, &daemonproto.RequestParams{Env: a.env, ID: a.id}, &r, func(err error) {
		if daemonproto.Kind(err) == daemonproto.KindNotFound && err != nil {
			c.addInfo(fmt.Sprintf("#%d is no longer pending", a.id))
			return
		}
		if err != nil {
			c.addInfo(fmt.Sprintf("#%d: %s", a.id, err.Error()))
			return
		}
		if line, done := decidedLine(a.id, r.Request); done {
			c.addInfo(line)
			return
		}
		cf, ok := confirmAnswer(a, &r)
		if !ok {
			c.addInfo(fmt.Sprintf("#%d: that answer is no longer offered", a.id))
			return
		}
		c.nextID++
		cf.ID = "c" + strconv.Itoa(c.nextID)
		p := daemonproto.AnswerParams{Env: a.env, ID: a.id, Verdict: daemonproto.VerdictDeny}
		if a.scope != "" {
			p.Verdict, p.Scope = daemonproto.VerdictAllow, a.scope
			if a.scope == daemonproto.ScopeHours {
				p.Hours = a.hours
			}
		}
		c.confirm = &confirmation{id: cf.ID, act: a, params: p}
		c.opts.UI.Confirm(cf)
	})
}

// decidedLine is what to say of request id when v says it is decided
// already; done is false while it is pending.
func decidedLine(id uint64, v daemonproto.RequestView) (line string, done bool) {
	if v.ID != id || v.State == "" || v.State == "pending" {
		return "", false
	}
	if v.State == "expired" || v.DecidedBy == "expiry" {
		return fmt.Sprintf("#%d expired", id), true
	}
	line = fmt.Sprintf("#%d was already answered", id)
	if by := Clean(v.DecidedBy); by != "" {
		line += " by " + by
	}
	return line, true
}

// confirmAnswer is the alert for answer a to request r, as it is now; ok
// is false when the request is no longer pending or the answer no longer
// offered.
func confirmAnswer(a action, r *daemonproto.RequestResult) (Confirmation, bool) {
	if r.Request.ID != a.id || (r.Request.State != "" && r.Request.State != "pending") {
		return Confirmation{}, false
	}
	cf := Confirmation{Effect: Clean(r.Request.Effect), SandboxSays: sandboxSays(r.Request.Ask, r.Request.Session)}
	switch a.scope {
	case "":
		cf.Message = fmt.Sprintf("Deny #%d?", a.id)
		cf.AnswerEffect = "The sandbox is refused, this time."
		cf.Action = "Deny"
		return cf, true
	case daemonproto.ScopeOnce:
		o := r.Offered(daemonproto.ScopeOnce)
		if o == nil {
			return cf, false
		}
		cf.Message = fmt.Sprintf("Allow #%d once?", a.id)
		cf.AnswerEffect = Clean(o.Effect)
		cf.Action = "Allow once"
		return cf, true
	case daemonproto.ScopeHours:
		o := r.Offered(daemonproto.ScopeHours)
		if o == nil || a.hours < o.HoursMin || a.hours > o.HoursMax || a.hours < 1 || a.hours > daemonproto.MaxHours {
			return cf, false
		}
		h := plural(a.hours, "hour", "hours")
		cf.Message = fmt.Sprintf("Allow #%d for %s?", a.id, h)
		cf.AnswerEffect = Clean(strings.ReplaceAll(o.Effect, hoursPlaceholder, strconv.Itoa(a.hours)))
		cf.Action = "Allow for " + h
		return cf, true
	}
	return cf, false
}

func (c *Core) confirmed(id string, ok bool) {
	cf := c.confirm
	if cf == nil || cf.id != id {
		return
	}
	c.confirm = nil
	if !ok {
		return
	}
	switch cf.act.kind {
	case actAnswer:
		c.sendAnswer("answer", cf.params, true)
	case actStop, actRestart:
		c.runJob(cf.act)
	}
}

// Stop and restart.

func (c *Core) askJob(a action) {
	if c.confirm != nil {
		c.opts.UI.Dismiss(c.confirm.id)
		c.confirm = nil
	}
	verb, cmd := "Stop", "stop"
	effect := fmt.Sprintf("This ends every session in %s, as `caboose -e %s stop` does.", a.env, a.env)
	if a.kind == actRestart {
		verb, cmd = "Restart", "restart"
		effect = fmt.Sprintf("This ends every session in %s, and rebuilds the image first when it is out of date, as `caboose -e %s %s` does.", a.env, a.env, cmd)
	}
	// The sessions' names are the sandbox's, made inside it: shown under
	// its label, never in the host's sentence.
	var sessions []string
	if d := c.details[a.env]; d != nil {
		for _, s := range d.res.Sessions {
			if s = strings.TrimSpace(Clean(s)); s != "" {
				sessions = append(sessions, "  "+Clip(s, MaxTitle))
			}
		}
	}
	says := ""
	if len(sessions) == 0 {
		effect += "\nNo session is known."
	} else {
		says = clipLines(SandboxLabel+"\nSessions:\n"+strings.Join(sessions, "\n"), MaxSandboxSays)
	}
	c.nextID++
	cf := Confirmation{ID: "c" + strconv.Itoa(c.nextID), Message: verb + " " + a.env + "?",
		Effect: effect, SandboxSays: says, Action: verb + " " + a.env}
	c.confirm = &confirmation{id: cf.ID, act: a}
	c.opts.UI.Confirm(cf)
}

// jobGone says why a stop or restart the user saw is no longer offered.
func (c *Core) jobGone(a action) string {
	verb := "stop"
	if a.kind == actRestart {
		verb = "restart"
	}
	if c.status != nil && c.state == stConnected {
		for _, e := range c.status.Envs {
			if e.Name != a.env {
				continue
			}
			switch {
			case e.Owner != "":
				return a.env + ": not sent, another daemon serves it now"
			case e.Job != nil && e.Job.State == daemonproto.JobRunning:
				return "a job already runs for " + a.env
			case e.Sandbox != daemonproto.SandboxRunning:
				return a.env + ": not sent, the sandbox is no longer running"
			}
			return a.env + ": " + verb + " no longer offered"
		}
		return a.env + ": not sent, the environment is gone"
	}
	return a.env + ": not sent, caboose's daemon is not answering"
}

func (c *Core) runJob(a action) {
	method := daemonproto.MethodSandboxStop
	if a.kind == actRestart {
		method = daemonproto.MethodSandboxRestart
	}
	var r daemonproto.JobResult
	if !c.call("job:"+a.env, method, &daemonproto.EnvParams{Env: a.env}, &r, func(err error) {
		switch {
		case daemonproto.Kind(err) == daemonproto.KindBusy && err != nil:
			c.addInfo("a job already runs for " + a.env)
		case err != nil:
			c.addInfo(a.env + ": " + err.Error())
		}
		c.render()
	}) {
		c.addInfo(a.env + ": not sent, caboose's daemon is not answering")
	}
}

// Opening things.

// openRoute opens the web page at route: through the daemon's web
// answer, with a fresh one-time code each time, and only while the daemon
// serves the page.
func (c *Core) openRoute(route string) {
	if !validRoute(route) {
		c.logf("refused to open a route outside the page's")
		return
	}
	var r daemonproto.WebResult
	sent := c.call("web", daemonproto.MethodWeb, &daemonproto.WebParams{Code: true}, &r, func(err error) {
		if err != nil {
			c.addInfo("web UI: " + err.Error())
			return
		}
		u, ok := pageURL(&r, route, c.opts.Dev)
		if !ok {
			why := r.Error
			if why == "" {
				why = "the daemon does not serve it"
			}
			c.addInfo("web UI not running: " + why)
			return
		}
		if err := c.opts.UI.Open(u); err != nil {
			c.addInfo("opening the web UI failed")
			c.logf("opening the web UI failed")
		}
	})
	if !sent && c.conn == nil {
		c.addInfo("web UI: caboose's daemon is not answering")
	}
}

func (c *Core) openLog() {
	if c.status == nil {
		c.addInfo("the log: caboose's daemon is not answering")
		return
	}
	if c.status.Daemon.Dir == "" || !filepath.IsAbs(c.status.Daemon.Dir) {
		c.addInfo("the log: the daemon names no directory")
		return
	}
	u := url.URL{Scheme: "file", Path: filepath.Join(c.status.Daemon.Dir, daemon.LogFile)}
	if err := c.opts.UI.Open(u.String()); err != nil {
		c.addInfo("opening the log failed: " + err.Error())
	}
}

func (c *Core) readLogin() {
	st, err := c.opts.UI.LoginItem()
	c.login, c.loginErr = st, ""
	if err != nil {
		c.loginErr = "Open at login: " + err.Error()
	}
}

// toggleLogin turns the login item from shown, the state its item
// showed, to the other: nothing when it is no longer in that state.
func (c *Core) toggleLogin(shown LoginState) {
	if shown != c.login {
		c.addInfo("Open at login changed meanwhile: nothing done")
		return
	}
	var err error
	switch c.login {
	case LoginOn:
		err = c.opts.UI.SetLoginItem(false)
	case LoginOff, LoginNeedsApproval:
		if c.opts.LoginCheck != nil {
			if err = c.opts.LoginCheck(); err != nil {
				c.loginErr = "Open at login refused: " + err.Error()
				c.logf("%s", c.loginErr)
				c.render()
				return
			}
		}
		if c.login == LoginOff {
			err = c.opts.UI.SetLoginItem(true)
		} else {
			err = c.opts.UI.OpenLoginSettings()
		}
	default:
		return
	}
	c.readLogin()
	if err != nil {
		c.loginErr = "Open at login: " + err.Error()
	}
	c.render()
}

func (c *Core) quit() {
	if c.quitting {
		return
	}
	c.quitting = true
	conn := c.conn
	c.conn = nil
	c.gen++
	if conn == nil {
		c.readyToStop()
		return
	}
	c.spawn(func() func() {
		_ = conn.Close()
		return c.readyToStop
	})
	c.after(QuitWait, c.readyToStop)
}

// quitAfterAlert shows the alert and quits once the user closes it, or
// AlertWait after, whichever comes first; the connection closes at once,
// and nothing more is drawn or asked meanwhile.
func (c *Core) quitAfterAlert(id, text string) {
	if c.quitting {
		return
	}
	c.quitAlert = id
	c.opts.UI.Alert(id, text)
	c.after(AlertWait, func() { c.alertClosed(id) })
	c.quit()
}

// alertClosed ends the wait for the alert a quit waits for; any other
// alert's closing changes nothing.
func (c *Core) alertClosed(id string) {
	if id == "" || id != c.quitAlert {
		return
	}
	c.quitAlert = ""
	c.stopUI()
}

// readyToStop is the quit's connection closed, or given up on.
func (c *Core) readyToStop() {
	c.stopReady = true
	c.stopUI()
}

// stopUI ends the UI once the quit waits for nothing.
func (c *Core) stopUI() {
	if !c.stopReady || c.quitAlert != "" || c.uiQuit {
		return
	}
	c.uiQuit = true
	c.opts.UI.Quit()
}

// The menu.

// keptMenus is how many menus before the last one a selection is still
// resolved against.
const keptMenus = 3

// lookup is what item id stands for in the menu the user saw: the last
// one sent, or one before it. An ID stands for the same action in every
// menu (see action), so the newest table that has it says what the user
// clicked; current is whether the last menu sent still has it.
func (c *Core) lookup(id string) (a action, current, ok bool) {
	if a, ok := c.actions[id]; ok {
		return a, true, true
	}
	for i := len(c.past) - 1; i >= 0; i-- {
		if a, ok := c.past[i][id]; ok {
			return a, false, true
		}
	}
	return action{}, false, false
}

func (c *Core) selected(id string) {
	a, current, ok := c.lookup(id)
	if !ok || c.quitting {
		return
	}
	if !current {
		// The item went from the menu after the user saw it. An answer
		// still goes to the daemon, which says what became of the
		// request; what the status alone decided is said here.
		switch a.kind {
		case actStop, actRestart:
			c.addInfo(c.jobGone(a))
			return
		case actPort:
			c.addInfo(fmt.Sprintf("%s: localhost:%d is no longer listening", a.env, a.port))
			return
		}
	}
	switch a.kind {
	case actAnswer:
		c.askAnswer(a)
	case actAlways:
		c.openRoute(daemonproto.RouteEnvRequests(a.env))
	case actRoute:
		c.openRoute(a.route)
	case actPort:
		if a.port > 0 && a.port <= 65535 {
			if err := c.opts.UI.Open(fmt.Sprintf("http://localhost:%d/", a.port)); err != nil {
				c.addInfo(fmt.Sprintf("opening localhost:%d failed", a.port))
			}
		}
	case actStop, actRestart:
		c.askJob(a)
	case actLog:
		c.openLog()
	case actLogin:
		c.toggleLogin(a.login)
	case actQuit:
		c.quit()
	}
}

func (c *Core) menuOpened() {
	c.menuOpen = true
	c.pollDetails(DetailsFresh)
}

func (c *Core) menuClosed() {
	c.menuOpen = false
	if c.menuDirty {
		c.render()
	}
}

func (c *Core) addInfo(s string) {
	c.info = append(c.info, infoLine{text: Clip(s, MaxTitle), until: c.clock.Now().Add(InfoFor)})
	if len(c.info) > 4 {
		c.info = c.info[len(c.info)-4:]
	}
	c.after(InfoFor, c.render)
	c.render()
}

func (c *Core) connLine() string {
	switch c.state {
	case stConnected:
		return ""
	case stRefused:
		return TextAppOlder
	case stWaitNewer:
		return TextWaitNewer
	case stWaitUpdate:
		return TextWaitUpdate
	}
	if c.clock.Now().Sub(c.lostAt) < DisconnectedAfter && c.lastErr == "" {
		return TextConnecting
	}
	if c.lastErr != "" {
		return TextNotAnswering + ": " + c.lastErr
	}
	return TextNotAnswering
}

func (c *Core) model() model {
	now := c.clock.Now()
	kept := c.info[:0]
	var lines []string
	for _, l := range c.info {
		if now.Before(l.until) {
			kept = append(kept, l)
			lines = append(lines, l.text)
		}
	}
	c.info = kept
	m := model{now: now, dev: c.opts.Dev, conn: c.connLine(), status: c.status, info: lines,
		noRequests: c.noReqs, noAttach: c.noAttach, login: c.login, loginErr: c.loginErr,
		details: map[string]*daemonproto.EnvDetailsResult{}}
	if c.conn != nil && c.status == nil {
		m.conn = TextConnecting
	}
	for env, d := range c.details {
		m.details[env] = d.res
	}
	for env, vs := range c.views {
		for _, v := range vs {
			m.requests = append(m.requests, reqEntry{env: env, view: v, offers: c.offers[reqKey{env, v.ID}]})
		}
	}
	return m
}

// render brings the icon and the menu up to date; the menu not while it
// is open.
func (c *Core) render() {
	if c.quitting {
		return
	}
	m := c.model()
	in := IconInput{}
	switch c.state {
	case stConnected:
	case stRefused:
		in.Disconnected = true
	default:
		in.Disconnected = c.clock.Now().Sub(c.lostAt) >= DisconnectedAfter
	}
	if st := c.status; st != nil && c.state == stConnected {
		in.Attention = len(st.Attention)
		for _, e := range st.Envs {
			if e.Sandbox == daemonproto.SandboxRunning {
				in.Running++
			}
		}
	}
	icon := Icon(in)
	if c.sentIcon == nil || *c.sentIcon != icon {
		c.sentIcon = &icon
		c.opts.UI.SetIcon(icon)
	}
	if c.menuOpen {
		c.menuDirty = true
		return
	}
	c.menuDirty = false
	menu, acts := build(m)
	if c.sentMenu != nil && reflect.DeepEqual(*c.sentMenu, menu) {
		return
	}
	c.sentMenu = &menu
	if c.actions != nil {
		c.past = append(c.past, c.actions)
		if len(c.past) > keptMenus {
			c.past = c.past[len(c.past)-keptMenus:]
		}
	}
	c.actions = acts
	c.opts.UI.SetMenu(menu)
}
