package menubar

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// Caps on what the menu lists.
const (
	MaxAttentionLines = 8
	MaxRequestItems   = 20
)

// HourChoices are the durations an "Allow for" item may offer, those
// within the offer's bounds.
var HourChoices = []int{1, 4, 8, 24, 72, 168}

// actKind is what selecting an item does.
type actKind int

const (
	actAnswer actKind = iota + 1
	actAlways
	actRoute
	actPort
	actStop
	actRestart
	actLog
	actLogin
	actQuit
)

// action is what an item ID stands for. An ID is made from everything
// its action holds (the request's environment, number and answer, the
// environment, the port, the login item's state), so one ID stands for the same action in every
// menu ever built: a selection that arrives after a newer menu was built
// still resolves to what the user saw (TestIDsStandForOneAction).
type action struct {
	kind  actKind
	env   string
	id    uint64
	scope string // an answer's: once, hours, or "" for a deny
	hours int
	route string
	port  int
	login LoginState // the login item's state as the item showed it
}

// reqEntry is one pending request the menu lists, with its offers once
// fetched.
type reqEntry struct {
	env    string
	view   daemonproto.RequestView
	offers *daemonproto.RequestResult
}

// model is everything the menu is built from.
type model struct {
	now  time.Time
	dev  bool
	conn string // "" when connected; else the line saying what is wrong

	status   *daemonproto.StatusResult
	details  map[string]*daemonproto.EnvDetailsResult
	requests []reqEntry
	// noRequests: the daemon cannot list requests; noAttach: it has no
	// app attachment.
	noRequests, noAttach bool

	info []string // transient lines, newest last

	login    LoginState
	loginErr string
}

type builder struct {
	acts map[string]action
}

func (b *builder) item(id, title string, a action) Item {
	b.acts[id] = a
	return Item{ID: id, Title: Clip(title, MaxTitle), Enabled: true}
}

func info(title string) Item { return Item{Title: Clip(title, MaxTitle), Info: true} }

var separator = Item{Separator: true}

// build is the menu for m, and what its item IDs stand for.
func build(m model) (Menu, map[string]action) {
	b := &builder{acts: map[string]action{}}
	var items []Item
	title := "caboose"
	if m.dev || (m.status != nil && m.status.Daemon.Dev) {
		title = "caboose (checkout)"
	}
	items = append(items, info(title))
	if m.conn != "" {
		items = append(items, info(m.conn))
	}
	for _, s := range m.info {
		items = append(items, info(s))
	}
	if m.noAttach {
		items = append(items, info("this daemon is older than the app: its alerts stay the system's"))
	}
	st := m.status
	if m.conn == "" && st != nil {
		items = append(items, b.attention(st.Attention)...)
		items = append(items, b.requests(m, st)...)
		if n := proposals(st.Attention); n > 0 {
			items = append(items, separator, b.item("proposals", plural(n, "proposal", "proposals")+"…", action{kind: actRoute, route: daemonproto.RouteProposals}))
		}
		for _, e := range st.Envs {
			items = append(items, separator)
			items = append(items, b.env(m, e)...)
		}
	}
	items = append(items, separator)
	connected := m.conn == "" && st != nil
	web := b.item("web", "Open web UI", action{kind: actRoute})
	web.Enabled = connected
	items = append(items, web)
	logItem := b.item("log", "Open log", action{kind: actLog})
	logItem.Enabled = connected && st.Daemon.Dir != ""
	items = append(items, logItem)
	items = append(items, b.loginItem(m))
	if m.loginErr != "" {
		items = append(items, info(m.loginErr))
	}
	items = append(items, b.item("quit", "Quit", action{kind: actQuit}))
	return Menu{Items: items}, b.acts
}

func (b *builder) attention(att []daemonproto.AttentionItem) []Item {
	if len(att) == 0 {
		return nil
	}
	items := []Item{separator}
	for i, a := range att {
		if i == MaxAttentionLines {
			items = append(items, b.item("attention-more", fmt.Sprintf("%d more…", len(att)-i), action{kind: actRoute}))
			break
		}
		line := a.Text
		if a.Env != "" {
			line = a.Env + ": " + a.Text
		}
		items = append(items, info(line))
	}
	return items
}

// proposals is how many proposals the attention list says are pending:
// each environment's item opens with its count.
func proposals(att []daemonproto.AttentionItem) int {
	n := 0
	for _, a := range att {
		if a.Kind != daemonproto.AttentionProposals {
			continue
		}
		f, _, _ := strings.Cut(a.Text, " ")
		k, err := strconv.Atoi(f)
		if err != nil || k < 1 {
			k = 1
		}
		n += k
	}
	return n
}

func (b *builder) requests(m model, st *daemonproto.StatusResult) []Item {
	total := 0
	for _, e := range st.Envs {
		if e.Owner == "" {
			total += e.Pending
		}
	}
	if total == 0 {
		return nil
	}
	if m.noRequests {
		return []Item{separator, info(fmt.Sprintf("Requests (%d)", total)),
			info("this daemon is older than the app: requests are shown in `caboose requests`")}
	}
	reqs := slices.Clone(m.requests)
	slices.SortStableFunc(reqs, func(a, b reqEntry) int {
		if c := b.view.Created.Compare(a.view.Created); c != 0 {
			return c
		}
		if c := cmp.Compare(a.env, b.env); c != 0 {
			return c
		}
		return cmp.Compare(b.view.ID, a.view.ID)
	})
	if len(reqs) > MaxRequestItems {
		reqs = reqs[:MaxRequestItems]
	}
	listening := st.Daemon.Web != nil && st.Daemon.Web.Listening
	var kids []Item
	for _, r := range reqs {
		kids = append(kids, b.request(r, listening))
	}
	if more := total - len(reqs); more > 0 {
		kids = append(kids, b.item("requests-more", fmt.Sprintf("%d more… (opens the web page)", more), action{kind: actRoute, route: daemonproto.RouteRequests}))
	}
	top := Item{Title: fmt.Sprintf("Requests (%d)", total), Enabled: true, Children: kids}
	return []Item{separator, top}
}

func reqID(env string, id uint64, what string) string {
	return fmt.Sprintf("req:%s:%d:%s", env, id, what)
}

// answers are the answers a request's submenu offers: exactly the host's
// offers, hours within their bounds, and deny.
func answers(o *daemonproto.RequestResult) []action {
	var acts []action
	if o == nil {
		return []action{{kind: actAnswer}}
	}
	if o.Offered(daemonproto.ScopeOnce) != nil {
		acts = append(acts, action{kind: actAnswer, scope: daemonproto.ScopeOnce})
	}
	if h := o.Offered(daemonproto.ScopeHours); h != nil {
		for _, n := range HourChoices {
			if n >= h.HoursMin && n <= h.HoursMax {
				acts = append(acts, action{kind: actAnswer, scope: daemonproto.ScopeHours, hours: n})
			}
		}
	}
	if o.Offered(daemonproto.ScopeAlways) != nil {
		acts = append(acts, action{kind: actAlways})
	}
	return append(acts, action{kind: actAnswer})
}

func answerTitle(a action) string {
	switch {
	case a.kind == actAlways:
		return "Allow always…"
	case a.scope == daemonproto.ScopeOnce:
		return "Allow once"
	case a.scope == daemonproto.ScopeHours:
		return "Allow for " + plural(a.hours, "hour", "hours")
	}
	return "Deny"
}

func answerWhat(a action) string {
	switch {
	case a.kind == actAlways:
		return "always"
	case a.scope == daemonproto.ScopeHours:
		return fmt.Sprintf("hours:%d", a.hours)
	case a.scope == daemonproto.ScopeOnce:
		return "once"
	}
	return "deny"
}

func (b *builder) request(r reqEntry, listening bool) Item {
	var kids []Item
	for _, a := range answers(r.offers) {
		a.env, a.id = r.env, r.view.ID
		it := b.item(reqID(r.env, r.view.ID, answerWhat(a)), answerTitle(a), a)
		if a.kind == actAlways && !listening {
			it.Title, it.Enabled = "Allow always… (web UI not running)", false
		}
		kids = append(kids, it)
	}
	kids = append(kids, separator,
		b.item(reqID(r.env, r.view.ID, "details"), "Details… (web page)", action{kind: actRoute, route: daemonproto.RouteEnvRequests(r.env)}))
	return Item{Title: RequestTitle(r.env, r.view), Enabled: true, Children: kids}
}

func (b *builder) env(m model, e daemonproto.EnvStatus) []Item {
	name := Clean(e.Name)
	if e.Owner != "" {
		who := "the release's"
		if e.Owner == daemonproto.OwnerDev {
			who = "the checkout's"
		}
		return []Item{info(name), info("served by " + who + " daemon")}
	}
	d := m.details[e.Name]
	running := e.Sandbox == daemonproto.SandboxRunning
	state := map[string]string{daemonproto.SandboxRunning: "running", daemonproto.SandboxStopped: "stopped",
		daemonproto.SandboxAbsent: "not created"}[e.Sandbox]
	if state == "" {
		state = "state unknown"
	}
	if e.Link == daemonproto.LinkFailed {
		state += ", link failed"
	}
	var paren []string
	if running && d != nil {
		if d.Isolation != "" {
			paren = append(paren, Clean(d.Isolation))
		}
		if !d.Started.IsZero() && !m.now.Before(d.Started) {
			paren = append(paren, uptime(m.now.Sub(d.Started)))
		}
	}
	header := name + ": " + state
	if len(paren) > 0 {
		header += " (" + strings.Join(paren, ", ") + ")"
	}
	items := []Item{info(header)}
	if e.Link == daemonproto.LinkFailed && e.LastError != "" {
		items = append(items, info(e.LastError))
	}
	if j := e.Job; j != nil {
		items = append(items, info(jobLine(j)))
	}
	if running && d != nil {
		if n := len(d.Sessions); n > 0 {
			items = append(items, info(plural(n, "session", "sessions")+" attached"))
		}
		if r := d.Resources; r != nil {
			if r.MemoryBytes > 0 && r.MemoryLimitBytes > 0 {
				items = append(items, info("memory "+gb(r.MemoryBytes)+" of "+gb(r.MemoryLimitBytes)+" GB"))
			} else if r.MemoryBytes > 0 {
				items = append(items, info("memory "+gb(r.MemoryBytes)+" GB"))
			}
			if r.VnodesMax > 0 {
				items = append(items, info("Mac files "+group(r.Vnodes)+" of "+group(r.VnodesMax)))
			}
		}
		for _, p := range d.Ports {
			if p.Port > 0 && p.Port <= 65535 {
				items = append(items, b.item(fmt.Sprintf("port:%s:%d", e.Name, p.Port), fmt.Sprintf("localhost:%d", p.Port), action{kind: actPort, env: e.Name, port: p.Port}))
			}
		}
	}
	items = append(items, b.item("env:"+e.Name, "Open in web page", action{kind: actRoute, route: daemonproto.RouteEnv(e.Name)}))
	if running && (e.Job == nil || e.Job.State != daemonproto.JobRunning) {
		items = append(items,
			b.item("stop:"+e.Name, "Stop…", action{kind: actStop, env: e.Name}),
			b.item("restart:"+e.Name, "Restart…", action{kind: actRestart, env: e.Name}))
	}
	return items
}

func jobLine(j *daemonproto.JobInfo) string {
	verb, past := "stopping", "stopped"
	switch j.Kind {
	case daemonproto.JobRestart:
		verb, past = "restarting", "restarted"
	case daemonproto.JobApply:
		// The proposal's file name is the sandbox's: never in a title.
		verb, past = "applying a proposal", "applied a proposal: its image is used from the next restart"
	}
	switch j.State {
	case daemonproto.JobRunning:
		return verb + "…"
	case daemonproto.JobFailed:
		if j.Error != "" {
			return verb + " failed: " + j.Error
		}
		return verb + " failed"
	}
	return past
}

func (b *builder) loginItem(m model) Item {
	switch m.login {
	case LoginOn:
		it := b.item("login:off", "Open at login", action{kind: actLogin, login: LoginOn})
		it.Checked = true
		return it
	case LoginOff:
		return b.item("login:on", "Open at login", action{kind: actLogin, login: LoginOff})
	case LoginNeedsApproval:
		it := b.item("login:approve", "Open at login (needs approval in System Settings)", action{kind: actLogin, login: LoginNeedsApproval})
		it.Checked = true
		return it
	}
	return Item{Title: "Open at login (not available)"}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// uptime is how long a sandbox has run, coarsely.
func uptime(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("up %d min", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("up %d h", int(d/time.Hour))
	}
	return fmt.Sprintf("up %d d", int(d/(24*time.Hour)))
}

// gb is bytes in GB (2^30), one decimal, none when whole.
func gb(n int64) string {
	s := strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64)
	return strings.TrimSuffix(s, ".0")
}

// group writes n with a comma between thousands, whatever the locale.
func group(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
