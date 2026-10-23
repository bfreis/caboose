package daemon

import (
	"io"
	"log"
	"sync"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// Source is where a host notification comes from, which decides its
// title (from a fixed table, daemonproto.HostTitles) and the web page it
// is about: never anything in its text.
type Source string

// The sources of host notifications.
const (
	// SourceLink is a link's own notices (new proposals, a VM's open
	// files): "caboose: ENV", about the environment's page.
	SourceLink Source = "link"
	// SourceCheckpoints is the checkpoint schedule's warnings:
	// "caboose: ENV checkpoints", about the environment's page.
	SourceCheckpoints Source = "checkpoints"
	// SourceDaemon is the daemon's own: "caboose: ENV", or "caboose".
	SourceDaemon Source = "daemon"
)

// The attached app's queue.
const (
	// notifyRing is the most items queued for the app.
	notifyRing = 64
	// notifyMaxAge is how long an item the app never fetched is kept.
	notifyMaxAge = 10 * time.Minute
	// notifyStale is how long an app that holds notifications may go
	// without asking for them before new ones go to the fallback again.
	notifyStale = 30 * time.Second
)

// Notifier is where the daemon's notifications go: to the attached app's
// queue while an app that can post them is attached and asks for them,
// else to the fallback (the host's own: osascript or notify-send), with
// the title and text the component gave, as they always went. Every
// item keeps its origin (host or sandbox), its environment, and a route
// and title the Notifier picks from its source.
type Notifier struct {
	fallback func(title, text string) error
	now      func() time.Time

	mu       sync.Mutex
	log      *log.Logger
	on       bool      // an attached app holds CapNotifications
	lastPoll time.Time // when it last asked, or attached
	seq      uint64    // the last item's
	ring     []queued  // oldest first
}

type queued struct {
	n    daemonproto.Notification
	at   time.Time // when queued, with the monotonic reading n.At drops
	sent bool      // returned by a notifications call
}

// NewNotifier is a Notifier whose fallback shows a notification on this
// machine; nil shows none.
func NewNotifier(fallback func(title, text string) error) *Notifier {
	if fallback == nil {
		fallback = func(string, string) error { return nil }
	}
	return &Notifier{fallback: fallback, now: time.Now, log: log.New(io.Discard, "", 0)}
}

// setLog has the Notifier log to l: the daemon's log.
func (n *Notifier) setLog(l *log.Logger) {
	n.mu.Lock()
	n.log = l
	n.mu.Unlock()
}

// For is the Notifier as a component of env (empty for none) sees it,
// whose host notifications come from src.
func (n *Notifier) For(env string, src Source) EnvNotifier {
	if !daemonproto.ValidEnv(env) {
		env = ""
	}
	return EnvNotifier{n: n, env: env, src: src}
}

// EnvNotifier is a Notifier for one environment and source: what a link,
// or a checkpoint schedule is handed.
type EnvNotifier struct {
	n   *Notifier
	env string
	src Source
}

// Notify is a host component's notification. The app gets it under the
// source's title; the fallback, under title.
func (e EnvNotifier) Notify(title, text string) error {
	item := daemonproto.Notification{Env: e.env, Origin: daemonproto.OriginHost,
		Title: hostTitle(e.env, e.src), Text: daemonproto.Text(text, daemonproto.MaxNotificationText), Route: hostRoute(e.env)}
	return e.n.deliver(item, title, text)
}

// SandboxNotify is a sandbox's notification (hostlink.SandboxNotifier):
// title is the sandbox's own, empty when it gave none. The app gets it
// under daemonproto.SandboxTitle, the sandbox's title only in the text;
// the fallback, as a sandbox's notification always went ("caboose" for
// no title).
func (e EnvNotifier) SandboxNotify(title, text string) error {
	fbTitle := title
	if fbTitle == "" {
		fbTitle = "caboose"
	}
	if e.env == "" {
		return e.n.fallback(fbTitle, text)
	}
	body := text
	if title != "" {
		body = title + ": " + text
	}
	item := daemonproto.Notification{Env: e.env, Origin: daemonproto.OriginSandbox,
		Title: daemonproto.SandboxTitle(e.env), Text: daemonproto.Text(body, daemonproto.MaxNotificationText), Route: daemonproto.RouteEnv(e.env)}
	return e.n.deliver(item, fbTitle, text)
}

// hostTitle is the title of src's notifications about env: one of
// daemonproto.HostTitles(env).
func hostTitle(env string, src Source) string {
	t := daemonproto.HostTitles(env)
	switch {
	case env == "":
		return t[0] // caboose
	case src == SourceCheckpoints:
		return t[1]
	}
	return t[0] // caboose: ENV
}

// hostRoute is the page a host notification about env opens: the
// environment's, or the overview.
func hostRoute(env string) string {
	if env == "" {
		return ""
	}
	return daemonproto.RouteEnv(env)
}

// deliver queues item for the app, or shows title and text through the
// fallback.
func (n *Notifier) deliver(item daemonproto.Notification, title, text string) error {
	now := n.now()
	n.mu.Lock()
	if !n.on || now.Sub(n.lastPoll) >= notifyStale {
		n.mu.Unlock()
		return n.fallback(title, text)
	}
	n.prune(now)
	if len(n.ring) >= notifyRing {
		if !n.ring[0].sent {
			n.log.Printf("notifications: the menu-bar app's queue is full: dropped one it never fetched")
		}
		n.ring = n.ring[1:]
	}
	n.seq++
	item.Seq, item.At = n.seq, now.UTC()
	n.ring = append(n.ring, queued{n: item, at: now})
	n.mu.Unlock()
	return nil
}

// prune drops the items older than notifyMaxAge, counting those the app
// never fetched. Under mu.
func (n *Notifier) prune(now time.Time) {
	i, lost := 0, 0
	for ; i < len(n.ring) && now.Sub(n.ring[i].at) >= notifyMaxAge; i++ {
		if !n.ring[i].sent {
			lost++
		}
	}
	n.ring = n.ring[i:]
	if lost > 0 {
		n.log.Printf("notifications: dropped %d the menu-bar app never fetched in %v", lost, notifyMaxAge)
	}
}

// attach starts an attachment (fresh) or changes one: on is whether the
// app holds CapNotifications. A fresh one, or one that gives up
// notifications, starts with an empty queue.
func (n *Notifier) attach(on, fresh bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if fresh || !on {
		n.clear("the menu-bar app's notifications changed")
	}
	n.on, n.lastPoll = on, n.now()
}

// detach ends the attachment: notifications go to the fallback again.
func (n *Notifier) detach() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.clear("the menu-bar app went away")
	n.on = false
}

// clear empties the queue, logging what the app never fetched. Under mu.
func (n *Notifier) clear(why string) {
	lost := 0
	for _, q := range n.ring {
		if !q.sent {
			lost++
		}
	}
	if lost > 0 {
		n.log.Printf("notifications: %s with %d it never fetched", why, lost)
	}
	n.ring = nil
}

// poll is the notifications method: the items after after, at most
// daemonproto.MaxNotifications, oldest first. Those up to after the app
// has had, and are dropped.
func (n *Notifier) poll(after uint64) *daemonproto.NotificationsResult {
	now := n.now()
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lastPoll = now
	n.prune(now)
	i := 0
	for i < len(n.ring) && n.ring[i].n.Seq <= after {
		i++
	}
	n.ring = n.ring[i:]
	r := &daemonproto.NotificationsResult{Items: []daemonproto.Notification{}, Next: min(after, n.seq)}
	for j := 0; j < len(n.ring) && len(r.Items) < daemonproto.MaxNotifications; j++ {
		n.ring[j].sent = true
		r.Items = append(r.Items, n.ring[j].n)
		r.Next = n.ring[j].n.Seq
	}
	return r
}
