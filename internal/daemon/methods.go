package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// caller is who a call comes from: the kind of client, which decides what
// it may call and is who an answer is recorded as. A socket client's is
// its hello's (never cli's claim of web or daemon: socketCaller); the web
// page's is set by the daemon's own HTTP handler, never by a request.
type caller struct {
	Kind string // daemonproto.ClientCLI, ClientWeb or ClientApp

	// hello is a socket client's hello, nil for the web page.
	hello *daemonproto.Frame
	// cmp is how a socket client's protocol compares to the daemon's;
	// the web page speaks the daemon's own.
	cmp daemonproto.Compat
	// conn is a socket client's connection, as its peer was read when it
	// was taken; nil for the web page.
	conn *connPeer
}

// socketCaller is the caller a socket connection's hello makes, and false
// for a kind a socket client may not be.
func socketCaller(hello *daemonproto.Frame, cmp daemonproto.Compat) (caller, bool) {
	switch hello.Client {
	case daemonproto.ClientCLI, daemonproto.ClientApp:
		return caller{Kind: hello.Client, hello: hello, cmp: cmp}, true
	}
	return caller{}, false
}

// webCaller is the web page's caller.
func webCaller() caller { return caller{Kind: daemonproto.ClientWeb} }

// Who may call a method. Every method names its kinds: one that does not
// name a kind refuses it, so a method added later reaches the web page
// only once it is listed for it.
var (
	allKinds    = []string{daemonproto.ClientCLI, daemonproto.ClientApp, daemonproto.ClientWeb}
	clientKinds = []string{daemonproto.ClientCLI, daemonproto.ClientApp} // not the web page
	appKind     = []string{daemonproto.ClientApp}                        // the menu-bar app alone
)

// method is one daemon method.
type method struct {
	name    string
	clients []string // the kinds of client that may call it
	// newer serves it to a client of a newer protocol too, which is
	// otherwise refused: such a client is to replace the daemon.
	newer bool
	// run serves it. then, when not nil, runs once the answer is sent,
	// and only on the socket.
	run func(ctx context.Context, c caller, params json.RawMessage) (res any, then func(), e *daemonproto.Error)
}

// methods is the daemon's method table, the one source of what the socket
// and the web page may call and how each is served.
func (s *server) methods() []method {
	return []method{
		{name: daemonproto.MethodPing, clients: allKinds, newer: true, run: func(context.Context, caller, json.RawMessage) (any, func(), *daemonproto.Error) {
			return nil, nil, nil
		}},
		{name: daemonproto.MethodStatus, clients: allKinds, newer: true, run: func(_ context.Context, c caller, _ json.RawMessage) (any, func(), *daemonproto.Error) {
			return s.status(c), nil, nil
		}},
		{name: daemonproto.MethodStop, clients: clientKinds, newer: true, run: func(context.Context, caller, json.RawMessage) (any, func(), *daemonproto.Error) {
			return nil, func() { s.stop("asked to stop") }, nil
		}},
		{name: daemonproto.MethodReplace, clients: clientKinds, newer: true, run: s.runReplace},
		{name: daemonproto.MethodEnsureLink, clients: clientKinds, run: s.runLink(daemonproto.MethodEnsureLink)},
		{name: daemonproto.MethodRestartLink, clients: clientKinds, run: s.runLink(daemonproto.MethodRestartLink)},
		{name: daemonproto.MethodStopLink, clients: clientKinds, run: s.runLink(daemonproto.MethodStopLink)},
		{name: daemonproto.MethodRequests, clients: allKinds, run: s.runBroker(daemonproto.MethodRequests)},
		{name: daemonproto.MethodRequest, clients: allKinds, run: s.runBroker(daemonproto.MethodRequest)},
		{name: daemonproto.MethodAnswer, clients: allKinds, run: s.runBroker(daemonproto.MethodAnswer)},
		{name: daemonproto.MethodGrants, clients: allKinds, run: s.runBroker(daemonproto.MethodGrants)},
		{name: daemonproto.MethodRevoke, clients: allKinds, run: s.runBroker(daemonproto.MethodRevoke)},
		{name: daemonproto.MethodPermissions, clients: allKinds, run: s.runBroker(daemonproto.MethodPermissions)},
		{name: daemonproto.MethodEditEgress, clients: allKinds, run: s.runBroker(daemonproto.MethodEditEgress)},
		{name: daemonproto.MethodCheckpoints, clients: allKinds, run: s.runCheckpoints},
		{name: daemonproto.MethodLinkLog, clients: allKinds, run: s.runLinkLog},
		{name: daemonproto.MethodEnvDetails, clients: allKinds, run: s.runEnvDetails},
		{name: daemonproto.MethodDoctor, clients: allKinds, run: s.runDoctor},
		{name: daemonproto.MethodProposals, clients: allKinds, run: s.runProposals},
		{name: daemonproto.MethodProposalDelete, clients: allKinds, run: s.runProposalDelete},
		{name: daemonproto.MethodProposalApply, clients: allKinds, run: s.runProposalApply},
		// A job, but one that ends no session: what caboose apply does.
		{name: daemonproto.MethodProposalBuild, clients: allKinds, run: s.runProposalBuild},
		// What ends every session in a sandbox: the command line's and
		// the app's, which ask the user first, never the page's.
		{name: daemonproto.MethodSandboxStop, clients: clientKinds, run: s.runJob(daemonproto.MethodSandboxStop, daemonproto.JobStop)},
		{name: daemonproto.MethodSandboxRestart, clients: clientKinds, run: s.runJob(daemonproto.MethodSandboxRestart, daemonproto.JobRestart)},
		// The host paths the sandboxes write, for the command line's
		// restore: the page has no use for them.
		{name: daemonproto.MethodWritable, clients: clientKinds, run: s.runWritable},
		// The page's token: the socket's alone, never the page's.
		{name: daemonproto.MethodWeb, clients: clientKinds, run: s.runWeb},
		// The menu-bar app's: its connection alone, and only the
		// attached one for notifications and dialogs.
		{name: daemonproto.MethodAppAttach, clients: appKind, run: s.runAppAttach},
		{name: daemonproto.MethodNotifications, clients: appKind, run: s.runNotifications},
		{name: daemonproto.MethodDialogs, clients: appKind, run: s.runDialogs},
	}
}

// lookup is the method of that name, or nil.
func (s *server) lookup(name string) *method {
	s.tableOnce.Do(func() {
		s.table = map[string]*method{}
		for _, m := range s.methods() {
			s.table[m.name] = &m
		}
	})
	return s.table[name]
}

// call serves one call of the method name from c: the socket's and the web
// page's alike. Who may call it is the table's; params are checked as a
// request's are; the result is checked before it leaves, so neither path
// sends a string with control characters.
func (s *server) call(ctx context.Context, c caller, name string, params json.RawMessage) (any, func(), *daemonproto.Error) {
	m := s.lookup(name)
	if m == nil {
		return nil, nil, daemonproto.NewError(daemonproto.KindUnknownMethod, "this daemon (caboose %s) does not serve %q", s.opts.Version, daemonproto.Text(name, 64))
	}
	if !slices.Contains(m.clients, c.Kind) {
		s.log.Printf("%s: refused a client of kind %q", m.name, daemonproto.Text(c.Kind, 16))
		return nil, nil, daemonproto.NewError(daemonproto.KindNotAllowed, "a %s client may not call %s", daemonproto.Text(c.Kind, 16), m.name)
	}
	if !m.newer && c.cmp == daemonproto.CompatClientNewer {
		return nil, nil, daemonproto.NewError(daemonproto.KindProtocolMismatch, "this daemon (caboose %s) is older than the client: replace it first", s.opts.Version)
	}
	if p := bytes.TrimSpace(params); len(p) > 0 && string(p) != "null" && p[0] != '{' {
		return nil, nil, daemonproto.NewError(daemonproto.KindBadRequest, "params are not an object")
	}
	res, then, e := m.run(ctx, c, params)
	if e != nil {
		return nil, nil, e
	}
	if isNil(res) {
		return nil, then, nil
	}
	if v, ok := res.(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			s.log.Printf("%s: refused its own answer: %v", m.name, err)
			return nil, nil, daemonproto.NewError(daemonproto.KindInternal, "the answer to %s: %v", m.name, err)
		}
	}
	return res, then, nil
}

// isNil reports whether v is nil, or a nil pointer in an interface.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	return r.Kind() == reflect.Pointer && r.IsNil()
}

// handle answers one request from a socket client, and returns what to do
// once the answer is sent.
func (s *server) handle(f *daemonproto.Frame, c caller) (*daemonproto.Frame, func()) {
	res, then, e := s.call(s.ctx, c, f.Method, f.Params)
	if e != nil {
		return daemonproto.Failure(f.ID, e), nil
	}
	r, err := daemonproto.Result(f.ID, res)
	if err == nil {
		_, err = daemonproto.Encode(r)
	}
	if err != nil {
		// A result over a frame: never sent, and the connection stays
		// usable. A stop or replace answers {}, which always fits.
		return daemonproto.Failure(f.ID, daemonproto.NewError(daemonproto.KindInternal, "the answer to %s does not fit a frame", f.Method)), nil
	}
	return r, then
}

// callWeb serves one call from the daemon's web page: the caller kind is
// web, whatever the request says, and the result comes back encoded, at
// most a frame's worth. What runs after an answer (stop, replace) is the
// socket's alone, and no such method is the page's.
func (s *server) callWeb(ctx context.Context, name string, params json.RawMessage) (json.RawMessage, *daemonproto.Error) {
	// The call ends with the request's ctx or the daemon, whichever ends
	// first.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(s.ctx, cancel)()
	res, _, e := s.call(ctx, webCaller(), name, params)
	if e != nil {
		return nil, e
	}
	if res == nil {
		res = struct{}{}
	} else if _, ok := res.(interface{ Validate() error }); !ok {
		// The page gets only what the daemon can vouch for.
		s.log.Printf("%s: an answer of type %T, which has no Validate, is not sent to the web page", name, res)
		return nil, daemonproto.NewError(daemonproto.KindInternal, "the answer to %s cannot be checked", name)
	}
	b, err := json.Marshal(res)
	if err == nil && len(b) > daemonproto.MaxLine {
		err = errors.New("over a frame")
	}
	if err != nil {
		return nil, daemonproto.NewError(daemonproto.KindInternal, "the answer to %s does not fit a frame", name)
	}
	return b, nil
}

// decode reads params into v as a request's, Validate included.
func decode(name string, params json.RawMessage, v any) *daemonproto.Error {
	return daemonproto.DecodeParams(&daemonproto.Frame{Type: daemonproto.TypeRequest, Method: name, Params: params}, v)
}

// protoError is err as the protocol's: a *daemonproto.Error as it is, any
// other as KindInternal.
func protoError(err error) *daemonproto.Error {
	var e *daemonproto.Error
	if errors.As(err, &e) {
		return e
	}
	return daemonproto.NewError(daemonproto.KindInternal, "%v", err)
}

func (s *server) unserved(name string) *daemonproto.Error {
	return daemonproto.NewError(daemonproto.KindUnknownMethod, "this daemon (caboose %s) does not serve %q", s.opts.Version, name)
}

// runReplace is replace: see daemonproto.ReplaceParams.
func (s *server) runReplace(_ context.Context, c caller, params json.RawMessage) (any, func(), *daemonproto.Error) {
	if c.hello == nil {
		return nil, nil, daemonproto.NewError(daemonproto.KindNotAllowed, "replace is for a socket client")
	}
	var p daemonproto.ReplaceParams
	if e := decode(daemonproto.MethodReplace, params, &p); e != nil {
		return nil, nil, e
	}
	res, e, then := s.replace(p, c.hello, c.cmp)
	if e != nil {
		return nil, nil, e
	}
	return res, then, nil
}

// runLink is a link method, served by Links.
func (s *server) runLink(name string) func(context.Context, caller, json.RawMessage) (any, func(), *daemonproto.Error) {
	return func(ctx context.Context, _ caller, params json.RawMessage) (any, func(), *daemonproto.Error) {
		var p daemonproto.EnvParams
		if e := decode(name, params, &p); e != nil {
			return nil, nil, e
		}
		r, err := s.links.Serve(ctx, name, p.Env)
		if err != nil {
			return nil, nil, protoError(err)
		}
		return r, nil, nil
	}
}

// brokerParamsOf are the params of each broker method, checked here before
// Brokers decodes them again: a bad request never reaches it.
func brokerParamsOf(name string) (p interface{ Validate() error }, env func() string) {
	switch name {
	case daemonproto.MethodRequests:
		v := &daemonproto.RequestsParams{}
		return v, func() string { return v.Env }
	case daemonproto.MethodRequest:
		v := &daemonproto.RequestParams{}
		return v, func() string { return v.Env }
	case daemonproto.MethodAnswer:
		v := &daemonproto.AnswerParams{}
		return v, func() string { return v.Env }
	case daemonproto.MethodRevoke:
		v := &daemonproto.RevokeParams{}
		return v, func() string { return v.Env }
	case daemonproto.MethodEditEgress:
		v := &daemonproto.EditEgressParams{}
		return v, func() string { return v.Env }
	}
	v := &daemonproto.EnvParams{}
	return v, func() string { return v.Env }
}

// runBroker is one of the request broker's methods. Brokers gets the
// caller's kind, which is who an answer is recorded as: never anything in
// params.
func (s *server) runBroker(name string) func(context.Context, caller, json.RawMessage) (any, func(), *daemonproto.Error) {
	return func(ctx context.Context, c caller, params json.RawMessage) (any, func(), *daemonproto.Error) {
		if s.opts.Brokers == nil {
			return nil, nil, s.unserved(name)
		}
		p, env := brokerParamsOf(name)
		if e := decode(name, params, p); e != nil {
			return nil, nil, e
		}
		res, err := s.opts.Brokers.Call(ctx, c.Kind, name, env(), params)
		if err != nil {
			return nil, nil, protoError(err)
		}
		return res, nil, nil
	}
}

// runCheckpoints is checkpoints: an environment's checkpoints, fitted to
// a frame.
func (s *server) runCheckpoints(ctx context.Context, _ caller, params json.RawMessage) (any, func(), *daemonproto.Error) {
	if s.opts.Checkpoints == nil {
		return nil, nil, s.unserved(daemonproto.MethodCheckpoints)
	}
	var p daemonproto.EnvParams
	if e := decode(daemonproto.MethodCheckpoints, params, &p); e != nil {
		return nil, nil, e
	}
	res, err := s.opts.Checkpoints.Checkpoints(ctx, p.Env)
	if err != nil {
		return nil, nil, protoError(err)
	}
	if res != nil {
		daemonproto.FitCheckpoints(res)
	}
	return res, nil, nil
}

// runWritable is writable: what the sandboxes write, read afresh.
func (s *server) runWritable(ctx context.Context, _ caller, params json.RawMessage) (any, func(), *daemonproto.Error) {
	if s.opts.Writable == nil {
		return nil, nil, s.unserved(daemonproto.MethodWritable)
	}
	var p struct{}
	if e := decode(daemonproto.MethodWritable, params, &p); e != nil {
		return nil, nil, e
	}
	res, err := s.opts.Writable.Writable(ctx)
	if err != nil {
		return nil, nil, protoError(err)
	}
	return res, nil, nil
}

// runLinkLog is link_log: the last lines of an environment's link log.
func (s *server) runLinkLog(ctx context.Context, _ caller, params json.RawMessage) (any, func(), *daemonproto.Error) {
	if s.opts.Details == nil {
		return nil, nil, s.unserved(daemonproto.MethodLinkLog)
	}
	var p daemonproto.LinkLogParams
	if e := decode(daemonproto.MethodLinkLog, params, &p); e != nil {
		return nil, nil, e
	}
	if p.Lines == 0 {
		p.Lines = daemonproto.DefaultLinkLogLines
	}
	res, err := s.opts.Details.LinkLog(ctx, p.Env, p.Lines)
	if err != nil {
		return nil, nil, protoError(err)
	}
	return res, nil, nil
}

// runEnvDetails is env_details.
func (s *server) runEnvDetails(ctx context.Context, _ caller, params json.RawMessage) (any, func(), *daemonproto.Error) {
	if s.opts.Details == nil {
		return nil, nil, s.unserved(daemonproto.MethodEnvDetails)
	}
	var p daemonproto.EnvParams
	if e := decode(daemonproto.MethodEnvDetails, params, &p); e != nil {
		return nil, nil, e
	}
	res, err := s.opts.Details.EnvDetails(ctx, p.Env)
	if err != nil {
		return nil, nil, protoError(err)
	}
	return res, nil, nil
}

// runJob is sandbox_stop or sandbox_restart: the job started, and
// answered with at once.
func (s *server) runJob(name, kind string) func(context.Context, caller, json.RawMessage) (any, func(), *daemonproto.Error) {
	return func(ctx context.Context, c caller, params json.RawMessage) (any, func(), *daemonproto.Error) {
		if s.opts.Jobs == nil {
			return nil, nil, s.unserved(name)
		}
		var p daemonproto.EnvParams
		if e := decode(name, params, &p); e != nil {
			return nil, nil, e
		}
		j, err := s.opts.Jobs.StartJob(ctx, kind, p.Env)
		if err != nil {
			return nil, nil, protoError(err)
		}
		s.log.Printf("%s %s: started for a %s client", name, p.Env, c.Kind)
		return &daemonproto.JobResult{Job: *j}, nil, nil
	}
}

// runDoctor is doctor: the checks, run in the daemon.
func (s *server) runDoctor(ctx context.Context, _ caller, params json.RawMessage) (any, func(), *daemonproto.Error) {
	if s.opts.Details == nil {
		return nil, nil, s.unserved(daemonproto.MethodDoctor)
	}
	var p daemonproto.EnvParams
	if e := decode(daemonproto.MethodDoctor, params, &p); e != nil {
		return nil, nil, e
	}
	res, err := s.opts.Details.Doctor(ctx, p.Env)
	if err != nil {
		return nil, nil, protoError(err)
	}
	return res, nil, nil
}

// runProposals is proposals.
func (s *server) runProposals(ctx context.Context, _ caller, params json.RawMessage) (any, func(), *daemonproto.Error) {
	if s.opts.Proposals == nil {
		return nil, nil, s.unserved(daemonproto.MethodProposals)
	}
	var p daemonproto.EnvParams
	if e := decode(daemonproto.MethodProposals, params, &p); e != nil {
		return nil, nil, e
	}
	res, err := s.opts.Proposals.Proposals(ctx, p.Env)
	if err != nil {
		return nil, nil, protoError(err)
	}
	return res, nil, nil
}

// runProposalDelete is proposal_delete.
func (s *server) runProposalDelete(ctx context.Context, _ caller, params json.RawMessage) (any, func(), *daemonproto.Error) {
	if s.opts.Proposals == nil {
		return nil, nil, s.unserved(daemonproto.MethodProposalDelete)
	}
	var p daemonproto.ProposalDeleteParams
	if e := decode(daemonproto.MethodProposalDelete, params, &p); e != nil {
		return nil, nil, e
	}
	if err := s.opts.Proposals.ProposalDelete(ctx, p.Env, p.File, p.Hash); err != nil {
		return nil, nil, protoError(err)
	}
	return nil, nil, nil
}

// runProposalApply is proposal_apply.
func (s *server) runProposalApply(ctx context.Context, _ caller, params json.RawMessage) (any, func(), *daemonproto.Error) {
	if s.opts.Proposals == nil {
		return nil, nil, s.unserved(daemonproto.MethodProposalApply)
	}
	var p daemonproto.ProposalApplyParams
	if e := decode(daemonproto.MethodProposalApply, params, &p); e != nil {
		return nil, nil, e
	}
	res, err := s.opts.Proposals.ProposalApply(ctx, p)
	if err != nil {
		return nil, nil, protoError(err)
	}
	return res, nil, nil
}

// runProposalBuild is proposal_build: the job started, and answered with
// at once.
func (s *server) runProposalBuild(ctx context.Context, c caller, params json.RawMessage) (any, func(), *daemonproto.Error) {
	if s.opts.Proposals == nil {
		return nil, nil, s.unserved(daemonproto.MethodProposalBuild)
	}
	var p daemonproto.ProposalApplyParams
	if e := decode(daemonproto.MethodProposalBuild, params, &p); e != nil {
		return nil, nil, e
	}
	j, err := s.opts.Proposals.ProposalBuild(ctx, p)
	if err != nil {
		return nil, nil, protoError(err)
	}
	s.log.Printf("%s %s: started for a %s client", daemonproto.MethodProposalBuild, p.Env, c.Kind)
	return &daemonproto.JobResult{Job: *j}, nil, nil
}
