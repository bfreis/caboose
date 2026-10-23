package daemon

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// fakeJobs starts a job per environment and keeps it running; a second
// for the same environment is busy.
type fakeJobs struct {
	mu      sync.Mutex
	running map[string]string // env -> kind
	at      time.Time
	bad     bool // answer a job that does not validate
}

func (f *fakeJobs) StartJob(_ context.Context, kind, env string) (*daemonproto.JobInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running == nil {
		f.running = map[string]string{}
	}
	if k, ok := f.running[env]; ok {
		return nil, daemonproto.NewError(daemonproto.KindBusy, "a %s job already runs for %s", k, env)
	}
	f.running[env] = kind
	j := &daemonproto.JobInfo{Kind: kind, State: daemonproto.JobRunning, Since: f.at}
	if f.bad {
		j.Error = "\x1b[31m"
	}
	return j, nil
}

// sandbox_stop and sandbox_restart start a job and answer with it at
// once, for the command line and the app; a second while one runs is
// busy; the page may call neither; a daemon given no Jobs does not know
// them; bad params never reach the jobs, and a job that does not
// validate is not sent.
func TestJobMethods(t *testing.T) {
	none := startCheckpointDaemon(t, nil)
	if _, err := dialAs(t, none, daemonproto.ClientCLI, 0).SandboxRestart(t.Context(), "alpha"); daemonproto.Kind(err) != daemonproto.KindUnknownMethod {
		t.Fatalf("without Jobs: %v", err)
	}

	f := &fakeJobs{at: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	dir, s := startServer(t, Options{Version: "v1.0.0", Idle: time.Minute, Log: testLog{t}, Jobs: f})
	cli, app := dialAs(t, dir, daemonproto.ClientCLI, 0), dialAs(t, dir, daemonproto.ClientApp, 0)
	r, err := app.SandboxRestart(t.Context(), "alpha")
	if err != nil || r.Job.Kind != daemonproto.JobRestart || r.Job.State != daemonproto.JobRunning || !r.Job.Since.Equal(f.at) {
		t.Fatalf("app restart: %+v, %v", r, err)
	}
	if _, err := cli.SandboxStop(t.Context(), "alpha"); daemonproto.Kind(err) != daemonproto.KindBusy {
		t.Fatalf("a second job for alpha: %v", err)
	}
	if r, err := cli.SandboxStop(t.Context(), "beta"); err != nil || r.Job.Kind != daemonproto.JobStop {
		t.Fatalf("cli stop of beta: %+v, %v", r, err)
	}
	for _, m := range []string{daemonproto.MethodSandboxStop, daemonproto.MethodSandboxRestart} {
		if _, e := s.callWeb(t.Context(), m, json.RawMessage(`{"env":"gamma"}`)); e == nil || e.Kind != daemonproto.KindNotAllowed {
			t.Errorf("web %s: %v", m, e)
		}
		if err := cli.Call(t.Context(), m, map[string]any{"env": "../x"}, nil); daemonproto.Kind(err) != daemonproto.KindBadRequest {
			t.Errorf("%s with a bad env: %v", m, err)
		}
	}
	f.mu.Lock()
	if _, ok := f.running["gamma"]; ok || len(f.running) != 2 {
		t.Errorf("jobs started: %v", f.running)
	}
	f.bad = true
	f.mu.Unlock()
	if _, err := cli.SandboxStop(t.Context(), "delta"); daemonproto.Kind(err) != daemonproto.KindInternal {
		t.Errorf("an invalid job was answered: %v", err)
	}
}
