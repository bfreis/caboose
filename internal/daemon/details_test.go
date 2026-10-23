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

// fakeDetails answers link_log, env_details and doctor with what it holds,
// and records the calls.
type fakeDetails struct {
	mu    sync.Mutex
	calls []string
	lines []int
	log   *daemonproto.LinkLogResult
	det   *daemonproto.EnvDetailsResult
	doc   *daemonproto.DoctorResult
	err   error
}

func (f *fakeDetails) note(what, env string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, what+" "+env)
}

func (f *fakeDetails) LinkLog(_ context.Context, env string, lines int) (*daemonproto.LinkLogResult, error) {
	f.note("link_log", env)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines = append(f.lines, lines)
	if f.err != nil {
		return nil, f.err
	}
	if f.log == nil {
		return &daemonproto.LinkLogResult{Lines: []string{}}, nil
	}
	return f.log, nil
}

func (f *fakeDetails) EnvDetails(_ context.Context, env string) (*daemonproto.EnvDetailsResult, error) {
	f.note("env_details", env)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if f.det == nil {
		return &daemonproto.EnvDetailsResult{Env: env}, nil
	}
	return f.det, nil
}

func (f *fakeDetails) Doctor(_ context.Context, env string) (*daemonproto.DoctorResult, error) {
	f.note("doctor", env)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if f.doc == nil {
		return &daemonproto.DoctorResult{As: daemonproto.DoctorAsDaemon}, nil
	}
	return f.doc, nil
}

// The three methods are served to every client kind, the web page's
// included; a daemon given no Details does not know them; bad params and
// the service's errors come back as errors, and a result the daemon cannot
// vouch for is never sent.
func TestDetailsMethods(t *testing.T) {
	none := startCheckpointDaemon(t, nil)
	cn := dialAs(t, none, daemonproto.ClientCLI, 0)
	if _, err := cn.EnvDetails(t.Context(), "alpha"); daemonproto.Kind(err) != daemonproto.KindUnknownMethod {
		t.Fatalf("without Details: %v", err)
	}

	f := &fakeDetails{
		log: &daemonproto.LinkLogResult{Lines: []string{"a", "b"}, Truncated: true},
		det: &daemonproto.EnvDetailsResult{Env: "alpha", Isolation: "vm", Image: &daemonproto.ImageDetail{State: daemonproto.ImageCurrent},
			Sessions: []string{"s1"}, Ports: []daemonproto.PortDetail{{Port: 3000, URL: "http://localhost:3000"}},
			Keep: []daemonproto.KeepDetail{{Path: "~/.ssh"}}, Errors: []daemonproto.DetailError{{Part: "resources", Message: "x"}}},
		doc: &daemonproto.DoctorResult{As: daemonproto.DoctorAsDaemon, RanAt: time.Now(),
			Findings: []daemonproto.DoctorFinding{{Level: daemonproto.LevelProblem, Label: "git", Message: "m", Fix: "f"}}},
	}
	dir, s := startServer(t, Options{Version: "v1.0.0", Idle: time.Minute, Log: testLog{t}, Details: f})
	for _, kind := range []string{daemonproto.ClientCLI, daemonproto.ClientApp} {
		c := dialAs(t, dir, kind, 0)
		if r, err := c.LinkLog(t.Context(), "alpha", 5); err != nil || len(r.Lines) != 2 || !r.Truncated {
			t.Fatalf("%s link_log: %+v, %v", kind, r, err)
		}
		if r, err := c.EnvDetails(t.Context(), "alpha"); err != nil || r.Isolation != "vm" || len(r.Errors) != 1 || r.Ports[0].Port != 3000 {
			t.Fatalf("%s env_details: %+v, %v", kind, r, err)
		}
		if r, err := c.Doctor(t.Context(), "alpha"); err != nil || r.As != "daemon" || len(r.Findings) != 1 {
			t.Fatalf("%s doctor: %+v, %v", kind, r, err)
		}
	}
	// The page calls them too, and a missing lines is the default.
	for name, params := range map[string]string{daemonproto.MethodLinkLog: `{"env":"alpha"}`, daemonproto.MethodEnvDetails: `{"env":"alpha"}`, daemonproto.MethodDoctor: `{"env":"alpha"}`} {
		raw, e := s.callWeb(t.Context(), name, json.RawMessage(params))
		if e != nil || !json.Valid(raw) {
			t.Fatalf("web %s: %s, %v", name, raw, e)
		}
	}
	f.mu.Lock()
	last := f.lines[len(f.lines)-1]
	f.mu.Unlock()
	if last != daemonproto.DefaultLinkLogLines {
		t.Errorf("the default lines = %d", last)
	}

	// Bad params never reach the service.
	f.mu.Lock()
	before := len(f.calls)
	f.mu.Unlock()
	c := dialAs(t, dir, daemonproto.ClientCLI, 0)
	for _, bad := range []struct {
		method string
		params any
	}{
		{daemonproto.MethodLinkLog, map[string]any{"env": "alpha", "lines": 501}},
		{daemonproto.MethodLinkLog, map[string]any{"env": "alpha", "lines": -1}},
		{daemonproto.MethodLinkLog, map[string]any{"env": "../x"}},
		{daemonproto.MethodEnvDetails, map[string]any{"env": "../x"}},
		{daemonproto.MethodDoctor, map[string]any{"env": ""}},
	} {
		if err := c.Call(t.Context(), bad.method, bad.params, nil); daemonproto.Kind(err) != daemonproto.KindBadRequest {
			t.Errorf("%s %v: %v", bad.method, bad.params, err)
		}
		if _, e := s.callWeb(t.Context(), bad.method, mustJSON(t, bad.params)); e == nil || e.Kind != daemonproto.KindBadRequest {
			t.Errorf("web %s %v: %v", bad.method, bad.params, e)
		}
	}
	f.mu.Lock()
	if len(f.calls) != before {
		t.Errorf("bad params reached the service: %v", f.calls[before:])
	}
	f.err = daemonproto.NewError(daemonproto.KindUnknownEnv, "no such environment")
	f.mu.Unlock()
	if _, err := c.EnvDetails(t.Context(), "beta"); daemonproto.Kind(err) != daemonproto.KindUnknownEnv {
		t.Fatalf("a service error: %v", err)
	}
	f.mu.Lock()
	f.err = errors.New("disk on fire")
	f.mu.Unlock()
	if _, err := c.Doctor(t.Context(), "beta"); daemonproto.Kind(err) != daemonproto.KindInternal || !strings.Contains(err.Error(), "disk on fire") {
		t.Fatalf("another error: %v", err)
	}
	// Results with control characters or too many entries are not sent,
	// not to the socket and not to the page.
	f.mu.Lock()
	f.err = nil
	f.log = &daemonproto.LinkLogResult{Lines: []string{"\x1b[31mred"}}
	f.det = &daemonproto.EnvDetailsResult{Env: "alpha", Sessions: []string{"a\nb"}}
	f.doc = &daemonproto.DoctorResult{As: "daemon", Findings: make([]daemonproto.DoctorFinding, daemonproto.MaxDoctorFindings+1)}
	f.mu.Unlock()
	if _, err := c.LinkLog(t.Context(), "alpha", 1); daemonproto.Kind(err) != daemonproto.KindInternal {
		t.Errorf("a link log with an escape: %v", err)
	}
	if _, err := c.EnvDetails(t.Context(), "alpha"); daemonproto.Kind(err) != daemonproto.KindInternal {
		t.Errorf("details with a newline: %v", err)
	}
	if _, e := s.callWeb(t.Context(), daemonproto.MethodDoctor, json.RawMessage(`{"env":"alpha"}`)); e == nil || e.Kind != daemonproto.KindInternal {
		t.Errorf("too many findings to the page: %v", e)
	}
	if err := c.Ping(t.Context()); err != nil {
		t.Fatalf("the connection did not survive: %v", err)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
