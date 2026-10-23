package launcher

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bfreis/caboose/internal/broker"
	"github.com/bfreis/caboose/internal/checkpoint"
	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/daemon"
	"github.com/bfreis/caboose/internal/daemonproto"
	"github.com/bfreis/caboose/internal/docker"
	"github.com/bfreis/caboose/internal/selfupdate"
)

func attnByKind(fs []daemon.Finding) map[string]daemon.Finding {
	m := map[string]daemon.Finding{}
	for _, f := range fs {
		m[f.Kind] = f
	}
	return m
}

// attnWorld is brokerWorld's alpha with a running link, and sources over
// it whose clock, image check and update record are the test's.
type attnWorld struct {
	*brokerWorld
	e      *envLink
	now    time.Time
	image  string
	imgErr error
	update selfupdate.State
	src    *daemonAttention
	ver    string
}

func newAttnWorld(t *testing.T) *attnWorld {
	w := &attnWorld{brokerWorld: newBrokerWorld(t, brokerConfig), now: time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC), ver: "v1.0.0"}
	w.l.brokerHook = nil
	w.e = w.l.env("alpha")
	w.e.mu.Lock()
	w.e.cur = &linkInstance{runner: &linkRunner{broker: w.b}}
	w.e.state = daemonproto.LinkRunning
	w.e.mu.Unlock()
	t.Cleanup(func() { w.e.mu.Lock(); w.e.cur = nil; w.e.mu.Unlock() })
	w.src = newDaemonAttention(w.l, attentionSeams{now: func() time.Time { return w.now }, version: w.ver,
		imageNote:   func(context.Context, *App) (string, error) { return w.image, w.imgErr },
		updateState: func(string) selfupdate.State { return w.update }})
	return w
}

func TestAttentionSourceRequestsAndLinkFailed(t *testing.T) {
	w := newAttnWorld(t)
	if got := w.src.Cheap(t.Context()); len(got) != 0 {
		t.Fatalf("nothing pending: %+v", got)
	}
	w.file(broker.Ask{Request: egressReq("api.example.com", "10.0.0.5")})
	w.decide(urlReq("https://example.com/a", "https://example.com"))
	f := attnByKind(w.src.Cheap(t.Context()))[daemonproto.AttentionRequests]
	if f.Env != "alpha" || f.Text != "2 pending requests" || f.Route != "requests/alpha" {
		t.Errorf("%+v", f)
	}
	// The link fails, with an error that quotes the sandbox.
	w.e.mu.Lock()
	w.e.cur = nil
	w.e.state, w.e.lastErr = daemonproto.LinkFailed, "boom \x1b[2J \u202e "+strings.Repeat("z", 1000)
	w.e.mu.Unlock()
	f = attnByKind(w.src.Cheap(t.Context()))[daemonproto.AttentionLinkFailed]
	item := daemonproto.AttentionItem{Env: f.Env, Kind: f.Kind, Text: f.Text, Route: f.Route}
	if err := item.Validate(); err != nil || !strings.HasPrefix(f.Text, "The link failed: boom") || len(f.Text) > 260 {
		t.Errorf("%q: %v", f.Text, err)
	}
	w.e.mu.Lock()
	w.e.state, w.e.lastErr = daemonproto.LinkStopped, ""
	w.e.mu.Unlock()
	if _, ok := attnByKind(w.src.Cheap(t.Context()))[daemonproto.AttentionLinkFailed]; ok {
		t.Error("a stopped link still needs the user")
	}
}

func TestAttentionSourceVnodes(t *testing.T) {
	w := newAttnWorld(t)
	w.e.mu.Lock()
	w.e.cur = &linkInstance{runner: &linkRunner{broker: w.b}}
	w.e.mu.Unlock()
	if _, ok := attnByKind(w.src.Cheap(t.Context()))[daemonproto.AttentionVnodes]; ok {
		t.Fatal("no warning yet")
	}
	// The watch's state, through the real check.
	wt := &vnodeWatch{sampler: &fakeVnodes{files: []int{900, 100}, errs: []error{nil, nil}}, env: "alpha"}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	wt.check(now)
	w.e.cur.runner.vnodes = wt.warning
	f := attnByKind(w.src.Cheap(t.Context()))[daemonproto.AttentionVnodes]
	if f.Text != "the VM holds 900 of the Mac's 1,000 vnodes" || f.Env != "alpha" {
		t.Errorf("%+v", f)
	}
	wt.check(now.Add(time.Minute))
	if wt.warning != "" {
		t.Errorf("warning stays under the re-arm level: %q", wt.warning)
	}
}

func TestAttentionSourceProposals(t *testing.T) {
	w := newAttnWorld(t)
	dir := filepath.Join(w.envDir, "data", "proposals")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, ok := attnByKind(w.src.Cheap(t.Context()))[daemonproto.AttentionProposals]; ok {
		t.Fatal("none pending")
	}
	for _, n := range []string{"a.toml", "b.toml", ".hidden.toml", "c.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f := attnByKind(w.src.Cheap(t.Context()))[daemonproto.AttentionProposals]
	if f.Env != "alpha" || f.Text != "2 pending proposals" || f.Route != daemonproto.RouteProposals {
		t.Errorf("%+v", f)
	}
	if err := os.Remove(filepath.Join(dir, "a.toml")); err != nil {
		t.Fatal(err)
	}
	if f := attnByKind(w.src.Cheap(t.Context()))[daemonproto.AttentionProposals]; f.Text != "1 pending proposal" {
		t.Errorf("%+v", f)
	}
}

// A proposals dir that is a link is not followed.
func TestAttentionSourceProposalsLink(t *testing.T) {
	w := newAttnWorld(t)
	dir := filepath.Join(w.envDir, "data", "proposals")
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "z.toml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}
	if _, ok := attnByKind(w.src.Cheap(t.Context()))[daemonproto.AttentionProposals]; ok {
		t.Error("followed a link")
	}
}

func TestCheckpointAttention(t *testing.T) {
	if checkpointAttention(nil) != "" || checkpointAttention(&checkpoint.Status{}) != "" {
		t.Error("a healthy schedule needs no one")
	}
	for name, tc := range map[string]struct {
		st   checkpoint.Status
		want string
	}{
		"error":   {checkpoint.Status{LastError: "disk\x1b[2J"}, "The last checkpoint failed: disk"},
		"space":   {checkpoint.Status{LowSpace: true}, "Low disk space for checkpoints"},
		"shrink":  {checkpoint.Status{Paths: []checkpoint.PathStatus{{Shrunk: "20260101"}}}, "A checkpointed path shrank sharply"},
		"exposed": {checkpoint.Status{Exposed: "root /x holds it"}, "The checkpoint store can be reached by the sandbox: root /x holds it"},
	} {
		got := checkpointAttention(&tc.st)
		if !strings.HasPrefix(got, tc.want) || strings.ContainsAny(got, "\x1b") {
			t.Errorf("%s: %q", name, got)
		}
	}
	both := checkpointAttention(&checkpoint.Status{LowSpace: true, LastError: "x"})
	if !strings.Contains(both, "; low disk space") {
		t.Errorf("%q", both)
	}
}

func TestAttentionSourceImageStale(t *testing.T) {
	w := newAttnWorld(t)
	got, err := w.src.Slow(t.Context(), "alpha")
	if err != nil || len(got) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
	w.image = "The image is out of date"
	got, err = w.src.Slow(t.Context(), "alpha")
	if err != nil || len(got) != 1 || got[0].Kind != daemonproto.AttentionImageStale || got[0].Route != "env/alpha" {
		t.Fatalf("%+v %v", got, err)
	}
	w.imgErr = errors.New("engine down")
	if _, err := w.src.Slow(t.Context(), "alpha"); err == nil {
		t.Error("an engine error was swallowed")
	}
	if got := w.src.SlowEnvs(); len(got) != 1 || got[0] != "alpha" {
		t.Errorf("envs %v", got)
	}
}

func TestAttentionSourceUpdate(t *testing.T) {
	w := newAttnWorld(t)
	for name, tc := range map[string]struct {
		latest, cur string
		want        bool
	}{
		"newer":  {"v1.1.0", "v1.0.0", true},
		"same":   {"v1.0.0", "v1.0.0", false},
		"older":  {"v0.9.0", "v1.0.0", false},
		"junk":   {"latest\x1b", "v1.0.0", false},
		"none":   {"", "v1.0.0", false},
		"dev":    {"v9.0.0", "dev", false},
		"nocurr": {"v9.0.0", "", false},
	} {
		w.l.dev = false
		w.update = selfupdate.State{Latest: tc.latest}
		w.src.s.version = tc.cur
		got, err := w.src.Update(t.Context())
		if err != nil || (len(got) == 1) != tc.want {
			t.Errorf("%s: %+v %v", name, got, err)
		}
		if tc.want {
			it := daemonproto.AttentionItem{Kind: got[0].Kind, Text: got[0].Text, Route: got[0].Route}
			if err := it.Validate(); err != nil || got[0].Env != "" {
				t.Errorf("%s: %v", name, err)
			}
		}
	}
}

func TestAttentionSourceUpdateNotForACheckout(t *testing.T) {
	w := newAttnWorld(t)
	w.update = selfupdate.State{Latest: "v9.0.0"}
	w.l.dev = true
	if got, err := w.src.Update(t.Context()); err != nil || len(got) != 0 {
		t.Errorf("%+v %v", got, err)
	}
}

// The runner's warning is what the source reports, and a stopped link's
// is gone.
func TestAttentionSourceReadsTheRunnersVnodeWarning(t *testing.T) {
	w := newAttnWorld(t)
	w.e.cur.runner.mu.Lock()
	w.e.cur.runner.vnodes = "the VM holds 900 of the Mac's 1,000 vnodes"
	w.e.cur.runner.mu.Unlock()
	if w.e.cur.runner.vnodeWarning() == "" {
		t.Fatal("the runner does not report it")
	}
	if f, ok := attnByKind(w.src.Cheap(t.Context()))[daemonproto.AttentionVnodes]; !ok || f.Env != "alpha" {
		t.Errorf("%+v", f)
	}
}

// An engine that hangs: the ask ends with its context.
func TestImageNoteEndsWithItsContext(t *testing.T) {
	script := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	app := &App{Cfg: &config.Config{Image: "x"}, Docker: &docker.CLI{Path: script, Stderr: io.Discard}}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := imageNote(ctx, app); err == nil || time.Since(start) > 2*time.Second {
		t.Errorf("returned %v after %v", err, time.Since(start))
	}
}
