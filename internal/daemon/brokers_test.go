package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// call is one request a fake Brokers served.
type call struct {
	who, method, env string
	params           string
}

// fakeBrokers records what the daemon hands its Brokers, and answers with
// result or err.
type fakeBrokers struct {
	mu     sync.Mutex
	calls  []call
	result any
	err    error
}

func (f *fakeBrokers) Call(_ context.Context, who, method, env string, params json.RawMessage) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{who, method, env, string(params)})
	return f.result, f.err
}

func (f *fakeBrokers) served() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

func startBrokerDaemon(t *testing.T, b Brokers) string {
	t.Helper()
	return startServer(t, Options{Version: "v1.0.0", Idle: time.Minute, Log: testLog{t}, Brokers: b})
}

// startServer runs a daemon in this process with opts (in a directory of
// its own) and returns the directory.
func startServer(t *testing.T, opts Options) string {
	t.Helper()
	h := newHarness(t)
	opts.Dir = h.dir
	runInProcess(t, opts)
	return h.dir
}

func dialAs(t *testing.T, dir, kind string, protocol int) *daemonproto.Client {
	t.Helper()
	c, err := daemonproto.Dial(context.Background(), Socket(dir), daemonproto.Options{Client: kind, Version: "v9.9.9", Protocol: protocol})
	if err != nil {
		t.Fatalf("dial as %s: %v", kind, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

var brokerMethods = []string{daemonproto.MethodRequests, daemonproto.MethodRequest, daemonproto.MethodAnswer,
	daemonproto.MethodGrants, daemonproto.MethodRevoke, daemonproto.MethodPermissions}

// brokerParams are valid params for each method.
func brokerParams(method string) any {
	switch method {
	case daemonproto.MethodRequests:
		return &daemonproto.RequestsParams{Env: "alpha"}
	case daemonproto.MethodRequest:
		return &daemonproto.RequestParams{Env: "alpha", ID: 1}
	case daemonproto.MethodAnswer:
		return &daemonproto.AnswerParams{Env: "alpha", ID: 1, Verdict: "deny"}
	case daemonproto.MethodRevoke:
		return &daemonproto.RevokeParams{Env: "alpha", Grant: "g1"}
	}
	return &daemonproto.EnvParams{Env: "alpha"}
}

// A daemon given no Brokers does not know the methods.
func TestNoBrokersIsUnknownMethod(t *testing.T) {
	dir := startBrokerDaemon(t, nil)
	c := dialAs(t, dir, daemonproto.ClientCLI, 0)
	for _, m := range brokerMethods {
		if err := c.Call(context.Background(), m, brokerParams(m), nil); daemonproto.Kind(err) != daemonproto.KindUnknownMethod {
			t.Errorf("%s: %v", m, err)
		}
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("the connection did not survive: %v", err)
	}
}

// The command line reads and answers, and reaches the brokers as its own
// kind.
func TestWhoMayAnswer(t *testing.T) {
	fb := &fakeBrokers{result: &daemonproto.RevokeResult{Revoked: "g1"}}
	dir := startBrokerDaemon(t, fb)
	c := dialAs(t, dir, daemonproto.ClientCLI, 0)
	for _, m := range brokerMethods {
		before := len(fb.served())
		if err := c.Call(context.Background(), m, brokerParams(m), nil); err != nil || len(fb.served()) != before+1 {
			t.Errorf("%s: %v", m, err)
			continue
		}
		if last := fb.served()[len(fb.served())-1]; last.who != daemonproto.ClientCLI {
			t.Errorf("%s reached the broker as %q", m, last.who)
		}
	}
}

// Who answers is the caller's kind and nothing the client writes: a field
// naming another is not read.
func TestWhoAnswersComesFromTheConnection(t *testing.T) {
	fb := &fakeBrokers{result: &daemonproto.RevokeResult{Revoked: "g1"}}
	dir := startBrokerDaemon(t, fb)
	liar := map[string]any{"env": "alpha", "id": 1, "verdict": "deny", "by": "dialog", "decided_by": "dialog", "client": "daemon", "who": "daemon", "kind": "daemon"}
	cli := dialAs(t, dir, daemonproto.ClientCLI, 0)
	if err := cli.Call(context.Background(), daemonproto.MethodAnswer, liar, nil); err != nil {
		t.Fatal(err)
	}
	got := fb.served()
	if len(got) != 1 || got[0].who != daemonproto.ClientCLI || got[0].env != "alpha" {
		t.Errorf("served %+v", got)
	}
}

// The daemon decodes the environment first; a bad name or a non-object
// never reaches the brokers.
func TestBrokerCallsAreChecked(t *testing.T) {
	fb := &fakeBrokers{result: &daemonproto.RevokeResult{Revoked: "g1"}}
	dir := startBrokerDaemon(t, fb)
	c := dialAs(t, dir, daemonproto.ClientCLI, 0)
	for _, env := range []string{"", "Bad", "../x", strings.Repeat("a", 40), "a b"} {
		err := c.Call(context.Background(), daemonproto.MethodPermissions, map[string]any{"env": env}, nil)
		if daemonproto.Kind(err) != daemonproto.KindBadRequest {
			t.Errorf("env %q: %v", env, err)
		}
	}
	if err := c.Call(context.Background(), daemonproto.MethodGrants, nil, nil); daemonproto.Kind(err) != daemonproto.KindBadRequest {
		t.Errorf("no params: %v", err)
	}
	if n := len(fb.served()); n != 0 {
		t.Errorf("%d bad calls reached the brokers", n)
	}
}

// A client of a newer protocol is told to replace this daemon, as for the
// link methods; nothing is served.
func TestBrokerCallsRefuseANewerClient(t *testing.T) {
	fb := &fakeBrokers{result: &daemonproto.RevokeResult{Revoked: "g1"}}
	dir := startBrokerDaemon(t, fb)
	c := dialAs(t, dir, daemonproto.ClientCLI, daemonproto.ProtocolVersion+1)
	for _, m := range brokerMethods {
		if err := c.Call(context.Background(), m, brokerParams(m), nil); daemonproto.Kind(err) != daemonproto.KindProtocolMismatch {
			t.Errorf("%s: %v", m, err)
		}
	}
	if n := len(fb.served()); n != 0 {
		t.Errorf("%d calls served", n)
	}
}

// What the brokers return is what the client gets: a protocol error as it
// is, any other as internal, and a result too big for a frame as an error
// that leaves the connection usable.
func TestBrokerResultsAndErrors(t *testing.T) {
	fb := &fakeBrokers{}
	dir := startBrokerDaemon(t, fb)
	c := dialAs(t, dir, daemonproto.ClientCLI, 0)
	ctx := context.Background()

	fb.result = &daemonproto.RevokeResult{Revoked: "g7"}
	if r, err := c.Revoke(ctx, "alpha", "g7"); err != nil || r.Revoked != "g7" {
		t.Errorf("%v %v", r, err)
	}
	fb.err = daemonproto.NewError(daemonproto.KindNotOwner, "no link here")
	if _, err := c.Revoke(ctx, "alpha", "g7"); daemonproto.Kind(err) != daemonproto.KindNotOwner || !strings.Contains(err.Error(), "no link here") {
		t.Errorf("not_owner: %v", err)
	}
	fb.err = errors.New("disk on fire")
	if _, err := c.Revoke(ctx, "alpha", "g7"); daemonproto.Kind(err) != daemonproto.KindInternal {
		t.Errorf("internal: %v", err)
	}
	fb.err = nil
	fb.result = map[string]string{"big": strings.Repeat("x", daemonproto.MaxLine)}
	if err := c.Call(ctx, daemonproto.MethodGrants, &daemonproto.EnvParams{Env: "alpha"}, nil); daemonproto.Kind(err) != daemonproto.KindInternal {
		t.Errorf("oversize: %v", err)
	}
	if err := c.Ping(ctx); err != nil {
		t.Errorf("after an oversize result: %v", err)
	}
}
