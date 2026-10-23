package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// caller is who a call comes from: the kind of client its hello names,
// which is who an answer is recorded as, never anything in a request.
type caller struct {
	Kind string // daemonproto.ClientCLI

	// hello is the client's hello.
	hello *daemonproto.Frame
	// cmp is how the client's protocol compares to the daemon's.
	cmp daemonproto.Compat
}

// method is one daemon method.
type method struct {
	name string
	// newer serves it to a client of a newer protocol too, which is
	// otherwise refused: such a client is to replace the daemon.
	newer bool
	// run serves it. then, when not nil, runs once the answer is sent.
	run func(ctx context.Context, c caller, params json.RawMessage) (res any, then func(), e *daemonproto.Error)
}

// methods is the daemon's method table, the one source of what a client
// may call and how each is served.
func (s *server) methods() []method {
	return []method{
		{name: daemonproto.MethodPing, newer: true, run: func(context.Context, caller, json.RawMessage) (any, func(), *daemonproto.Error) {
			return nil, nil, nil
		}},
		{name: daemonproto.MethodStatus, newer: true, run: func(context.Context, caller, json.RawMessage) (any, func(), *daemonproto.Error) {
			return s.status(), nil, nil
		}},
		{name: daemonproto.MethodStop, newer: true, run: func(context.Context, caller, json.RawMessage) (any, func(), *daemonproto.Error) {
			return nil, func() { s.stop("asked to stop") }, nil
		}},
		{name: daemonproto.MethodReplace, newer: true, run: s.runReplace},
		{name: daemonproto.MethodEnsureLink, run: s.runLink(daemonproto.MethodEnsureLink)},
		{name: daemonproto.MethodRestartLink, run: s.runLink(daemonproto.MethodRestartLink)},
		{name: daemonproto.MethodStopLink, run: s.runLink(daemonproto.MethodStopLink)},
		{name: daemonproto.MethodRequests, run: s.runBroker(daemonproto.MethodRequests)},
		{name: daemonproto.MethodRequest, run: s.runBroker(daemonproto.MethodRequest)},
		{name: daemonproto.MethodAnswer, run: s.runBroker(daemonproto.MethodAnswer)},
		{name: daemonproto.MethodGrants, run: s.runBroker(daemonproto.MethodGrants)},
		{name: daemonproto.MethodRevoke, run: s.runBroker(daemonproto.MethodRevoke)},
		{name: daemonproto.MethodPermissions, run: s.runBroker(daemonproto.MethodPermissions)},
		{name: daemonproto.MethodCheckpoints, run: s.runCheckpoints},
		// The host paths the sandboxes write, for the command line's
		// restore.
		{name: daemonproto.MethodWritable, run: s.runWritable},
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

// call serves one call of the method name from c. Params are checked as a
// request's are; the result is checked before it leaves, so no answer
// carries a string with control characters.
func (s *server) call(ctx context.Context, c caller, name string, params json.RawMessage) (any, func(), *daemonproto.Error) {
	m := s.lookup(name)
	if m == nil {
		return nil, nil, daemonproto.NewError(daemonproto.KindUnknownMethod, "this daemon (caboose %s) does not serve %q", s.opts.Version, daemonproto.Text(name, 64))
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
