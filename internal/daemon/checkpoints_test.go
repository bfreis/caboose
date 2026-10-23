package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/daemonproto"
)

// fakeCheckpoints answers the checkpoints method with result or err.
type fakeCheckpoints struct {
	mu     sync.Mutex
	envs   []string
	result *daemonproto.CheckpointsResult
	err    error
}

func (f *fakeCheckpoints) Checkpoints(_ context.Context, env string) (*daemonproto.CheckpointsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.envs = append(f.envs, env)
	if f.err != nil {
		return nil, f.err
	}
	r := *f.result
	return &r, nil
}

func startCheckpointDaemon(t *testing.T, c Checkpoints) string {
	t.Helper()
	h := newHarness(t)
	runInProcess(t, Options{Dir: h.dir, Version: "v1.0.0", Idle: time.Minute, Log: testLog{t}, Checkpoints: c})
	return h.dir
}

// checkpoints lists an environment's checkpoints for any client that may
// read; a daemon given none does not know the method.
func TestCheckpointsMethod(t *testing.T) {
	none := startCheckpointDaemon(t, nil)
	if _, err := dialAs(t, none, daemonproto.ClientCLI, 0).Checkpoints(t.Context(), "alpha"); daemonproto.Kind(err) != daemonproto.KindUnknownMethod {
		t.Fatalf("without Checkpoints: %v", err)
	}

	f := &fakeCheckpoints{result: &daemonproto.CheckpointsResult{Env: "alpha", Paths: []daemonproto.CheckpointListed{{
		Path: "/work/p", Key: "p-00000000", Checkpoints: []daemonproto.CheckpointView{{ID: "p-00000000@20261018T120000Z", Reason: "interval"}}}}}}
	dir := startServer(t, Options{Version: "v1.0.0", Idle: time.Minute, Log: testLog{t}, Checkpoints: f})
	for _, kind := range []string{daemonproto.ClientCLI} {
		r, err := dialAs(t, dir, kind, 0).Checkpoints(t.Context(), "alpha")
		if err != nil || len(r.Paths) != 1 || r.Paths[0].Checkpoints[0].Reason != "interval" {
			t.Fatalf("%s: %+v, %v", kind, r, err)
		}
	}
	c := dialAs(t, dir, daemonproto.ClientCLI, 0)
	if err := c.Call(t.Context(), daemonproto.MethodCheckpoints, &daemonproto.EnvParams{Env: "../x"}, nil); daemonproto.Kind(err) != daemonproto.KindBadRequest {
		t.Fatalf("a bad environment: %v", err)
	}
	f.mu.Lock()
	f.err = daemonproto.NewError(daemonproto.KindUnknownEnv, "no such environment")
	f.mu.Unlock()
	if _, err := c.Checkpoints(t.Context(), "beta"); daemonproto.Kind(err) != daemonproto.KindUnknownEnv {
		t.Fatalf("an error of the service: %v", err)
	}
	f.mu.Lock()
	f.err = errors.New("disk on fire")
	f.mu.Unlock()
	if _, err := c.Checkpoints(t.Context(), "beta"); daemonproto.Kind(err) != daemonproto.KindInternal || !strings.Contains(err.Error(), "disk on fire") {
		t.Fatalf("another error: %v", err)
	}
	// A result the daemon cannot vouch for is never sent.
	f.mu.Lock()
	f.err = nil
	f.result = &daemonproto.CheckpointsResult{Env: "alpha", Paths: []daemonproto.CheckpointListed{{Path: "/work/\x1b[31m", Key: "k"}}}
	f.mu.Unlock()
	if _, err := c.Checkpoints(t.Context(), "alpha"); daemonproto.Kind(err) != daemonproto.KindInternal {
		t.Fatalf("an invalid listing: %v", err)
	}
	if err := c.Ping(t.Context()); err != nil {
		t.Fatalf("the connection did not survive: %v", err)
	}
}

// A listing over a frame is cut to fit, oldest checkpoints first, and
// says so.
func TestCheckpointsFitAFrame(t *testing.T) {
	long := strings.Repeat("x", 1000)
	var paths []daemonproto.CheckpointListed
	for range 8 {
		l := daemonproto.CheckpointListed{Path: "/work/" + long[:900], Key: "k"}
		for range daemonproto.MaxCheckpointList {
			l.Checkpoints = append(l.Checkpoints, daemonproto.CheckpointView{ID: "k@20261018T120000Z", Error: long})
		}
		paths = append(paths, l)
	}
	f := &fakeCheckpoints{result: &daemonproto.CheckpointsResult{Env: "alpha", Paths: paths}}
	dir := startCheckpointDaemon(t, f)
	r, err := dialAs(t, dir, daemonproto.ClientCLI, 0).Checkpoints(t.Context(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Truncated || len(r.Paths) != 8 || len(r.Paths[0].Checkpoints) == 0 || len(r.Paths[0].Checkpoints) == daemonproto.MaxCheckpointList {
		t.Fatalf("truncated %v, %d paths, %d checkpoints of the first", r.Truncated, len(r.Paths), len(r.Paths[0].Checkpoints))
	}
}
