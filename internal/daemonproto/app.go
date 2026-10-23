package daemonproto

import (
	"context"
	"strings"
	"time"
)

// The menu-bar app's methods. Only a socket connection of kind app may
// call them, and only the attached one (see AppAttachParams) may call
// notifications and dialogs.
const (
	// MethodAppAttach makes the calling connection the daemon's one
	// attached app, or changes what the attached app can do.
	MethodAppAttach = "app_attach"
	// MethodNotifications is the attached app's notifications: see
	// NotificationsParams.
	MethodNotifications = "notifications"
	// MethodDialogs is the attached app's URL dialogs: see DialogsParams.
	MethodDialogs = "dialogs"
)

// What an attached app can do now, as app_attach names it.
const (
	// CapNotifications is set when the app can post notifications of its
	// own: the daemon then queues them for it instead of showing them
	// itself.
	CapNotifications = "notifications"
	// CapDialogs is set when the app can ask the user's questions in
	// dialogs of its own.
	CapDialogs = "dialogs"
)

// Bounds of the app's messages.
const (
	// MaxCapabilities bounds an attach's capabilities.
	MaxCapabilities = 8
	// MaxExecutable bounds an attached app's executable path.
	MaxExecutable = 1024
	// MaxNotifications is the most one notifications call returns.
	MaxNotifications = 32
	// MaxNotificationTitle and MaxNotificationText bound an item.
	MaxNotificationTitle = 128
	MaxNotificationText  = 1024
)

// Where a notification came from.
const (
	// OriginHost is a notification a host component wrote: a proposals
	// notice, a checkpoint warning, the daemon's own.
	OriginHost = "host"
	// OriginSandbox is one a sandbox asked for (caboose-agent notify).
	OriginSandbox = "sandbox"
)

// The fixed routes of the web page a notification may open, besides
// RouteEnv's.
const (
	RouteRequests  = "requests"
	RouteProposals = "proposals"
)

// RouteEnv is the route of an environment's page.
func RouteEnv(env string) string { return "env/" + env }

// RouteEnvRequests is the route of an environment's requests.
func RouteEnvRequests(env string) string { return "requests/" + env }

// ValidRoute reports whether r is a route a notification may carry: the
// overview (""), requests, proposals, requests/ENV or env/ENV, as the web
// page's to= takes them.
func ValidRoute(r string) bool {
	switch r {
	case "", RouteRequests, RouteProposals:
		return true
	}
	if env, ok := strings.CutPrefix(r, "env/"); ok {
		return ValidEnv(env)
	}
	if env, ok := strings.CutPrefix(r, "requests/"); ok {
		return ValidEnv(env)
	}
	return false
}

// HostTitles are the titles a host notification may have, for env (empty
// when it is about no one environment): a fixed table, never anything
// interpolated but the environment's name.
func HostTitles(env string) []string {
	if env == "" {
		return []string{"caboose"}
	}
	return []string{"caboose: " + env, "caboose: " + env + " checkpoints"}
}

// SandboxTitle is the title of every notification env's sandbox asked
// for: host-written, so that a sandbox never posts one that looks like
// caboose's own.
func SandboxTitle(env string) string { return "From the " + env + " sandbox" }

// AppAttachParams are the params of app_attach. The first call on a
// connection, while no app is attached, attaches it (another
// connection's then answers KindBusy); a later one on the same connection
// replaces its capabilities. The attachment ends when the connection
// closes. The app's process ID and executable are what the daemon reads
// off the socket, never anything sent.
type AppAttachParams struct {
	// Capabilities are what the app can do now (CapNotifications,
	// CapDialogs); names the daemon does not know are left out.
	Capabilities []string `json:"capabilities,omitempty"`
	// Version is the app's caboose version.
	Version string `json:"version,omitempty"`
}

// Validate checks the capabilities and the version.
func (p *AppAttachParams) Validate() error {
	if err := checkCapabilities(p.Capabilities); err != nil {
		return err
	}
	return checkText("app version", p.Version, maxName)
}

func checkCapabilities(caps []string) error {
	if len(caps) > MaxCapabilities {
		return bad("over %d capabilities", MaxCapabilities)
	}
	for _, c := range caps {
		if len(c) > maxName || !nameRe.MatchString(c) {
			return bad("bad capability")
		}
	}
	return nil
}

// AppAttachResult is the result of app_attach.
type AppAttachResult struct {
	Attached bool `json:"attached"`
	// Capabilities are those the daemon took: the known ones of the
	// params', each once.
	Capabilities []string `json:"capabilities"`
}

// Validate checks the capabilities.
func (r *AppAttachResult) Validate() error { return checkCapabilities(r.Capabilities) }

// AppInfo is the attached app, as status says it.
type AppInfo struct {
	Attached bool `json:"attached"`
	// PID is the app's process, as the socket said it (0 when it could
	// not tell).
	PID int `json:"pid,omitempty"`
	// Executable is the app's executable path, read for its PID; empty
	// when it could not be read, and always for the web page.
	Executable   string    `json:"executable,omitempty"`
	Version      string    `json:"version,omitempty"`
	Since        time.Time `json:"since,omitzero"`
	Capabilities []string  `json:"capabilities,omitempty"`
}

func (a *AppInfo) validate() error {
	if a.PID < 0 {
		return bad("negative app pid")
	}
	if err := checkText("app executable", a.Executable, MaxExecutable); err != nil {
		return err
	}
	if err := checkText("app version", a.Version, maxName); err != nil {
		return err
	}
	return checkCapabilities(a.Capabilities)
}

// NotificationsParams are the params of notifications: the items queued
// for the attached app with a Seq over After. An attachment's queue starts
// empty, so a newly attached app asks with After 0.
type NotificationsParams struct {
	After uint64 `json:"after,omitempty"`
}

// Notification is one item of the attached app's queue.
type Notification struct {
	Seq uint64 `json:"seq"`
	// Env is the environment it is about; empty for none.
	Env string `json:"env,omitempty"`
	// Origin is OriginHost or OriginSandbox.
	Origin string `json:"origin"`
	// Title is host-written whatever the origin: one of HostTitles(Env),
	// or SandboxTitle(Env) for a sandbox's.
	Title string `json:"title"`
	// Text is the body: a sandbox's title and text joined, for a
	// sandbox's.
	Text string `json:"text"`
	// Route is the web page the item is about (ValidRoute), picked by
	// the daemon from where the item came from, never from its text.
	Route string    `json:"route,omitempty"`
	At    time.Time `json:"at"`
}

func (n *Notification) validate() error {
	if n.Seq == 0 {
		return bad("a notification without a sequence number")
	}
	if n.Env != "" && !ValidEnv(n.Env) {
		return bad("bad environment name in a notification")
	}
	if !ValidRoute(n.Route) {
		return bad("bad notification route")
	}
	if err := checkText("notification title", n.Title, MaxNotificationTitle); err != nil {
		return err
	}
	if err := checkText("notification text", n.Text, MaxNotificationText); err != nil {
		return err
	}
	switch n.Origin {
	case OriginSandbox:
		if n.Env == "" || n.Title != SandboxTitle(n.Env) {
			return bad("a sandbox's notification without the sandbox's title")
		}
	case OriginHost:
		ok := false
		for _, t := range HostTitles(n.Env) {
			ok = ok || n.Title == t
		}
		if !ok {
			return bad("a host notification whose title is not one of the table's")
		}
	default:
		return bad("bad notification origin")
	}
	return nil
}

// NotificationsResult is the result of notifications: at most
// MaxNotifications items, oldest first, and the After to ask with next.
type NotificationsResult struct {
	Items []Notification `json:"items"`
	Next  uint64         `json:"next"`
}

// Validate checks the items: bounded, each valid, in order, none past
// Next.
func (r *NotificationsResult) Validate() error {
	if len(r.Items) > MaxNotifications {
		return bad("over %d notifications", MaxNotifications)
	}
	var last uint64
	for i := range r.Items {
		if err := r.Items[i].validate(); err != nil {
			return err
		}
		if r.Items[i].Seq <= last {
			return bad("notifications out of order")
		}
		last = r.Items[i].Seq
	}
	if r.Next < last {
		return bad("next before the last notification")
	}
	return nil
}

// DialogsParams are the params of dialogs, which only the attached app
// holding CapDialogs may call: the URL request whose dialog it is to
// show. While such an app is attached, the daemon shows no URL dialog of
// its own: it hands the requests over here one at a time, in the order
// it would have shown them. A request once handed over is in flight with
// the app, and dialogs returns it again until it is answered (by the
// app's answer, scope once, or by anyone else's, which the app then
// dismisses). An attachment that ends with one in flight denies it,
// "the app went away; ask again", and so does a daemon that starts to
// find one its predecessor handed over.
type DialogsParams struct{}

// Validate checks nothing: there is nothing to check.
func (p *DialogsParams) Validate() error { return nil }

// DialogRef is a request a dialog is to ask about: the app reads its URL
// and the sandbox's text with request.
type DialogRef struct {
	Env string `json:"env"`
	ID  uint64 `json:"id"`
}

// DialogsResult is the result of dialogs: the request in flight with the
// app, nil when there is none to show.
type DialogsResult struct {
	Dialog *DialogRef `json:"dialog,omitempty"`
}

// Validate checks the request's environment and number.
func (r *DialogsResult) Validate() error {
	if r.Dialog == nil {
		return nil
	}
	if !ValidEnv(r.Dialog.Env) {
		return bad("bad environment name in a dialog")
	}
	if r.Dialog.ID == 0 {
		return bad("a dialog without a request number")
	}
	return nil
}

// Dialogs asks for the request whose URL dialog the attached app is to
// show; nil when there is none.
func (c *Client) Dialogs(ctx context.Context) (*DialogRef, error) {
	var r DialogsResult
	if err := c.Call(ctx, MethodDialogs, &DialogsParams{}, &r); err != nil {
		return nil, err
	}
	return r.Dialog, nil
}

// AppAttach attaches the connection as the menu-bar app.
func (c *Client) AppAttach(ctx context.Context, p AppAttachParams) (*AppAttachResult, error) {
	var r AppAttachResult
	if err := c.Call(ctx, MethodAppAttach, &p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Notifications asks for the attached app's notifications after after.
func (c *Client) Notifications(ctx context.Context, after uint64) (*NotificationsResult, error) {
	var r NotificationsResult
	if err := c.Call(ctx, MethodNotifications, &NotificationsParams{After: after}, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
