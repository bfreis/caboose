package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/agentproto"
	"github.com/bfreis/caboose/internal/backend"
	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
)

// brokerHandle is a running environment's broker, and what its link knows
// besides.
type brokerHandle struct {
	b *broker.Broker
	// mods is the agent's last count of Claude Code sessions, and whether
	// it sent one.
	mods func() (agentproto.ModsInfo, bool)
}

var _ daemon.Brokers = (*DaemonLinks)(nil)

// brokerOf is env's broker: the one its link in this daemon opened. Only
// the process running an environment's link holds its link.lock, and
// only that one serves its broker, so an environment this daemon runs no
// link for has none here.
func (l *DaemonLinks) brokerOf(env string) (*brokerHandle, error) {
	if l.brokerHook != nil {
		if h := l.brokerHook(env); h != nil {
			return h, nil
		}
	} else {
		l.mu.Lock()
		e := l.envs[env]
		l.mu.Unlock()
		if e != nil {
			e.mu.Lock()
			in := e.cur
			e.mu.Unlock()
			if in != nil && in.runner.broker != nil {
				return &brokerHandle{b: in.runner.broker, mods: in.runner.lastMods}, nil
			}
		}
	}
	return nil, daemonproto.NewError(daemonproto.KindNotOwner,
		"no link of this daemon serves environment %s, and the broker answers only while its link runs: "+
			"start the sandbox (or run: %s). With CABOOSE_DATA_DIR set a detached helper owns the link, and only this daemon answers requests",
		env, envCommand(env, "link"))
}

// byOf is who answered, as the broker records it, for the kind of client
// the daemon says the call came from (never anything the call says). The
// daemon's method table decides which kinds may answer; this refuses any
// kind that is not a person's.
func byOf(who string) (string, error) {
	switch who {
	case daemonproto.ClientCLI:
		return broker.ByCLI, nil
	case daemonproto.ClientWeb:
		return broker.ByWeb, nil
	case daemonproto.ClientApp:
		return broker.ByApp, nil
	}
	return "", daemonproto.NewError(daemonproto.KindNotAllowed, "a %s client may not answer", daemonproto.Text(who, 16))
}

// Call serves the broker's methods (daemon.Brokers).
func (l *DaemonLinks) Call(_ context.Context, who, method, env string, params json.RawMessage) (any, error) {
	if !config.ValidEnv(env) {
		return nil, daemonproto.NewError(daemonproto.KindBadRequest, "bad environment name")
	}
	h, err := l.brokerOf(env)
	if err != nil {
		return nil, err
	}
	frame := &daemonproto.Frame{Type: daemonproto.TypeRequest, Method: method, Params: params}
	var res any
	switch method {
	case daemonproto.MethodRequests:
		var p daemonproto.RequestsParams
		if e := daemonproto.DecodeParams(frame, &p); e != nil {
			return nil, e
		}
		res, err = brokerRequests(h.b, p)
	case daemonproto.MethodRequest:
		var p daemonproto.RequestParams
		if e := daemonproto.DecodeParams(frame, &p); e != nil {
			return nil, e
		}
		var (
			q        broker.RequestInfo
			offers   []broker.Offer
			withheld []broker.Withheld
		)
		if q, offers, withheld, err = h.b.RequestOffers(p.ID); err == nil {
			res = requestResult(q, offers, withheld)
		}
	case daemonproto.MethodAnswer:
		var p daemonproto.AnswerParams
		if e := daemonproto.DecodeParams(frame, &p); e != nil {
			return nil, e
		}
		by, berr := byOf(who)
		if berr != nil {
			return nil, berr
		}
		res, err = brokerAnswer(h.b, p, by)
	case daemonproto.MethodGrants:
		var gs []broker.GrantInfo
		if gs, err = h.b.Grants(); err == nil {
			v, truncated := grantViews(gs)
			res = &daemonproto.GrantsResult{Grants: v, Truncated: truncated}
		}
	case daemonproto.MethodRevoke:
		var p daemonproto.RevokeParams
		if e := daemonproto.DecodeParams(frame, &p); e != nil {
			return nil, e
		}
		if _, berr := byOf(who); berr != nil {
			return nil, berr
		}
		if err = h.b.Revoke(p.Grant); err == nil {
			res = &daemonproto.RevokeResult{Revoked: p.Grant}
		}
	case daemonproto.MethodPermissions:
		res, err = l.permissions(env, h)
	case daemonproto.MethodEditEgress:
		var p daemonproto.EditEgressParams
		if e := daemonproto.DecodeParams(frame, &p); e != nil {
			return nil, e
		}
		by, berr := byOf(who)
		if berr != nil {
			return nil, berr
		}
		res, err = brokerEditEgress(h.b, p, by)
	default:
		return nil, daemonproto.NewError(daemonproto.KindUnknownMethod, "this daemon does not serve %q", method)
	}
	if err != nil {
		return nil, brokerError(err)
	}
	return res, nil
}

// brokerError is err as the protocol's error, with the kind the broker's
// error has.
func brokerError(err error) error {
	var de *daemonproto.Error
	if errors.As(err, &de) {
		return de
	}
	var be *broker.Error
	if errors.As(err, &be) {
		kind := daemonproto.KindInternal
		switch be.Kind {
		case broker.KindNotOwner:
			kind = daemonproto.KindNotOwner
		case broker.KindNotFound:
			kind = daemonproto.KindNotFound
		case broker.KindAlreadyAnswered:
			kind = daemonproto.KindAlreadyAnswered
		case broker.KindStaleDiff:
			kind = daemonproto.KindStaleDiff
		case broker.KindBadRequest:
			kind = daemonproto.KindBadRequest
		case broker.KindUnavailable:
			kind = daemonproto.KindBrokerUnavailable
		case broker.KindTooManyWaits:
			kind = daemonproto.KindBusy
		}
		return daemonproto.NewError(kind, "%s", be.Msg)
	}
	return daemonproto.NewError(daemonproto.KindInternal, "%v", err)
}

func brokerRequests(b *broker.Broker, p daemonproto.RequestsParams) (*daemonproto.RequestsResult, error) {
	limit := p.Limit
	if limit == 0 {
		limit = daemonproto.DefaultRequestsLimit
	}
	list, _, err := b.List(broker.Query{All: p.All, Limit: limit})
	if err != nil {
		return nil, err
	}
	views := make([]daemonproto.RequestView, 0, len(list))
	for _, q := range list {
		views = append(views, requestView(q))
	}
	views, truncated := daemonproto.FitRequests(views)
	return &daemonproto.RequestsResult{Requests: views, Truncated: truncated}, nil
}

func brokerAnswer(b *broker.Broker, p daemonproto.AnswerParams, by string) (*daemonproto.AnswerResult, error) {
	res, err := b.Answer(p.ID, broker.Reply{Verdict: p.Verdict, Scope: p.Scope, Hours: p.Hours, By: by, Confirm: p.ConfirmDiff})
	if err != nil {
		return nil, err
	}
	out := &daemonproto.AnswerResult{
		Request: requestView(res.Request),
		Effect:  daemonproto.Text(res.Effect, 4096),
		Diff:    daemonproto.Lines(res.Diff, daemonproto.MaxDiff), DiffHash: res.DiffHash,
		Recorded: res.Recorded, Grant: res.Grant, Also: res.Also,
		OpenError: daemonproto.Text(res.OpenError, 4096),
	}
	if len(out.Also) > daemonproto.MaxRequestsLimit*4 {
		out.Also = out.Also[:daemonproto.MaxRequestsLimit*4]
	}
	return out, nil
}

// brokerEditEgress is an egress edit, its diff as the protocol carries
// it; the hash is of the diff as it is written. As for an "always" offer
// (requestResult), an edit whose diff the protocol would show otherwise
// than it is written is not made: the broker refuses it, shown or
// confirmed, under the same lock as the write.
func brokerEditEgress(b *broker.Broker, p daemonproto.EditEgressParams, by string) (*daemonproto.EditEgressResult, error) {
	res, err := b.EditEgress(broker.EgressEdit{AddAllow: p.AddAllow, RemoveAllow: p.RemoveAllow, AddDeny: p.AddDeny, RemoveDeny: p.RemoveDeny,
		By: by, Confirm: p.ConfirmDiff, Shown: func(diff string) bool { return shownAsWritten(diff, daemonproto.DiffLines(diff)) }})
	if err != nil {
		return nil, err
	}
	return &daemonproto.EditEgressResult{Diff: daemonproto.DiffLines(res.Diff), Hash: res.DiffHash,
		Effect: daemonproto.Text(res.Effect, 4096), Written: res.Written}, nil
}

// requestResult is q with its offers, as the protocol has them. The
// diff's lines are config.toml's, made printable for the page and the
// terminal; the hash is of the diff as it would be written.
func requestResult(q broker.RequestInfo, offers []broker.Offer, withheld []broker.Withheld) *daemonproto.RequestResult {
	r := &daemonproto.RequestResult{Request: requestView(q)}
	for _, o := range offers {
		po := daemonproto.Offer{Scope: o.Scope, Effect: daemonproto.Text(o.Effect, 4096), HoursMin: o.HoursMin, HoursMax: o.HoursMax}
		if o.Scope == broker.ScopeAlways {
			po.Hash, po.Diff = o.DiffHash, daemonproto.DiffLines(o.Diff)
		}
		if po.Scope == broker.ScopeAlways {
			// A diff the protocol cannot carry whole, or whose lines it
			// shows otherwise than they would be written (a character
			// made printable, a line cut), is not offered: the hash
			// confirms the diff as written, so what is shown must be
			// that. A tab shown as a space changes nothing a reader
			// decides on, and is let through. The rest of the request
			// still is offered.
			one := daemonproto.RequestResult{Request: r.Request, Offers: []daemonproto.Offer{po}}
			if err := one.Validate(); err != nil || !shownAsWritten(o.Diff, po.Diff) {
				r.Withheld = append(r.Withheld, daemonproto.Withheld{Scope: o.Scope, Why: "the change to config.toml cannot be shown whole, so it is not offered"})
				continue
			}
		}
		r.Offers = append(r.Offers, po)
	}
	for _, w := range withheld {
		r.Withheld = append(r.Withheld, daemonproto.Withheld{Scope: w.Scope, Why: daemonproto.Text(w.Why, 4096)})
	}
	return r
}

// shownAsWritten reports whether shown, a diff's lines as the protocol
// carries them (daemonproto.DiffLines), is diff line for line, unchanged
// but for each tab shown as a space.
func shownAsWritten(diff string, shown []string) bool {
	d := strings.TrimSuffix(diff, "\n")
	if d == "" {
		return len(shown) == 0
	}
	return slices.Equal(strings.Split(strings.ReplaceAll(d, "\t", " "), "\n"), shown)
}

// requestView is q as the protocol has it. The broker already made the
// sandbox's words printable and clipped them; the protocol's bounds are
// kept here as well, since the client refuses a frame that breaks them.
func requestView(q broker.RequestInfo) daemonproto.RequestView {
	v := daemonproto.RequestView{
		ID: q.ID, Kind: string(q.Kind), Key: daemonproto.Text(q.Key, 640), Effect: daemonproto.Text(q.Effect, 4096),
		State: q.State, Scope: q.Scope, Hours: q.Hours, Created: q.Created, Expires: q.Expires,
		DecidedAt: q.DecidedAt, DecidedBy: q.DecidedBy, Reason: daemonproto.Text(q.Reason, 4096),
		Seq: q.Seq, Attempts: q.Attempts, Session: daemonproto.Text(q.Session, 128),
	}
	if a := q.Ask; a != nil {
		v.Ask = &daemonproto.Ask{Context: daemonproto.Text(a.Context, 2048), Recommendation: daemonproto.Text(a.Recommendation, 2048),
			Meanwhile: daemonproto.Text(a.Meanwhile, 2048)}
		for i, o := range a.Options {
			if i == 5 {
				break
			}
			v.Ask.Options = append(v.Ask.Options, daemonproto.Text(o, 256))
		}
	}
	return v
}

func grantViews(gs []broker.GrantInfo) ([]daemonproto.GrantView, bool) {
	var out []daemonproto.GrantView
	for _, g := range gs {
		if len(out) == daemonproto.MaxGrants {
			return out, true
		}
		gv := daemonproto.GrantView{ID: g.ID, Kind: string(g.Kind), Key: daemonproto.Text(g.Key, 640), Once: g.Once,
			Created: g.Created, Expires: g.Expires, LeftSeconds: int64(g.Left / time.Second), Request: g.Request}
		for _, a := range g.Addrs {
			if len(gv.Addrs) < 64 {
				gv.Addrs = append(gv.Addrs, daemonproto.Text(a, 64))
			}
		}
		out = append(out, gv)
	}
	return out, false
}

// permissions is what env's sandbox may do: config.toml as the daemon reads
// it now, and the grants in force.
func (l *DaemonLinks) permissions(env string, h *brokerHandle) (*daemonproto.PermissionsResult, error) {
	app, err := l.newApp(env)
	if err != nil {
		return nil, err
	}
	gs, err := h.b.Grants()
	if err != nil {
		return nil, err
	}
	r := permissionsOf(app.Cfg)
	r.Drift = app.restartDrift()
	r.Grants, _ = grantViews(gs)
	if h.mods != nil {
		if m, ok := h.mods(); ok {
			r.Mods = &daemonproto.ModsPerm{Sessions: m.Sessions, WithMod: m.WithMod}
		}
	}
	return r, nil
}

// restartDrift says how the sandbox as it was created differs from
// config.toml in what the report shows and only a restart changes: the
// roots and their writable parts (as warnIfRootsDrifted and writableDrift
// say), and the trust and the VM's network (trustDrift, networkDrift).
// Nothing when they agree or there is no sandbox to ask. Each sentence is
// made printable and bounded as the protocol carries it.
//
// The checks each ask the sandbox for its labels and mounts; they are
// read once here, for every check (onceBox), since the page asks every
// few seconds.
func (a *App) restartDrift() []string {
	prev := a.Backend
	a.Backend = &onceBox{Backend: a.box()}
	defer func() { a.Backend = prev }()
	var out []string
	if mounted := a.mountedRoots(); len(mounted) > 0 && !config.SameRoots(mounted, a.Cfg.Roots) {
		out = append(out, fmt.Sprintf("the %s mounts %s; the configuration says %s", a.noun(), mountList(mounted), mountList(a.Cfg.Roots)))
	}
	for _, d := range []string{a.writableDrift(), a.trustDrift(), a.networkDrift()} {
		if d != "" {
			out = append(out, d)
		}
	}
	for i, d := range out {
		out[i] = daemonproto.Text(d, 4096)
	}
	if len(out) > daemonproto.MaxPermDrift {
		out = out[:daemonproto.MaxPermDrift]
	}
	return out
}

// onceBox is a Backend whose labels and mounts are read once, the first
// time they are asked for, and then answered from what that read gave.
// It is for one caller, for the length of a report.
type onceBox struct {
	backend.Backend
	labelsRead, mountsRead bool
	labels                 map[string]string
	labelsErr              error
	mounts                 []backend.Mount
	mountsErr              error
}

func (o *onceBox) Labels() (map[string]string, error) {
	if !o.labelsRead {
		o.labels, o.labelsErr = o.Backend.Labels()
		o.labelsRead = true
	}
	return maps.Clone(o.labels), o.labelsErr
}

func (o *onceBox) Mounts() ([]backend.Mount, error) {
	if !o.mountsRead {
		o.mounts, o.mountsErr = o.Backend.Mounts()
		o.mountsRead = true
	}
	return slices.Clone(o.mounts), o.mountsErr
}

// permissionsOf is c's settings as a permissions report. The roots are
// config.toml's: what the running sandbox mounts is what it was created
// with, which a changed config.toml reaches at the next restart
// (restartDrift says where the two differ). The
// writable set the broker decides aliases against is not drawn from them
// yet -- it counts everything as writable, so no alias runs -- and the
// report says nothing of it.
func permissionsOf(c *config.Config) *daemonproto.PermissionsResult {
	t := c.Trust
	if t == "" {
		t = config.TrustFallible
	}
	r := &daemonproto.PermissionsResult{Env: c.Env, Trust: t, Isolation: isolationOf(c), Profile: c.Profile}
	for _, ro := range c.Roots {
		r.Roots = append(r.Roots, daemonproto.RootPerm{Name: ro.Name, Host: ro.Host, Container: ro.Container,
			ReadOnly: len(ro.Writable) == 0, Writable: ro.Writable})
	}
	for _, a := range c.Aliases {
		r.Aliases = append(r.Aliases, daemonproto.AliasPerm{Name: a.Name, Description: a.Description, Command: a.Command, Cwd: a.Cwd, Policy: a.Policy})
	}
	fallible := t == config.TrustFallible
	r.HostExec = daemonproto.HostExecPerm{Configured: c.HostExec, Effective: c.HostExec && fallible}
	switch {
	case !c.HostExec:
		r.HostExec.Why = "host_exec is off in config.toml"
	case !fallible:
		r.HostExec.Why = "trust = " + t + ": the link does not offer host exec, whatever host_exec says"
	}
	r.OpenURLs = daemonproto.OpenURLsPerm{Mode: c.OpenURLs, Allow: c.OpenURLsAllow}
	if !fallible && c.OpenURLs != "off" {
		r.OpenURLs.Why = "trust = " + t + ": every URL asks, whatever open_urls and open_urls_allow say"
	}
	if isolationOf(c) == config.KindVM {
		r.Egress = daemonproto.EgressPerm{On: c.Egress, Ports: or(c.EgressPorts, config.DefaultEgressPorts), Allow: c.EgressAllow, Deny: c.EgressDeny}
		switch {
		case !fallible:
			r.Egress.On = true
			r.Egress.Network = "trust = " + t + ": the VM has no network of its own; only the outbound proxy leaves it, through this machine"
			r.Egress.Default = fmt.Sprintf("trust = %s: every host that egress_allow or a grant does not name asks", t)
		case !c.Egress:
			r.Egress.Network = "the VM's own NAT, which reaches none of this machine's VPN routes (egress off)"
		default:
			r.Egress.Network = "through this machine, so its VPN routes and DNS apply (egress on)"
			r.Egress.Default = "a host that resolves only to public addresses connects; a private or local address asks"
		}
	} else {
		r.Egress.Network = "the engine's own network; egress applies under isolation vm only"
	}
	r.ForwardPorts = c.ForwardPorts
	return r
}
